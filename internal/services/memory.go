package services

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"swarmmemo/internal/allowance"
)

// Memory limits (RFC0012 §3.2). internal/board publishes the same numbers in
// PublicLimits, and a board test holds them equal.
const (
	MemoryKeyBytes    = 256
	MemoryValueBytes  = 64 << 10
	MemoryKeysMax     = 1000
	MemoryBytesMax    = 16 << 20
	MemoryListPageMax = 100
	// MemoryPutArgsMax bounds put's args JSON: a full value with room for
	// JSON escaping.
	MemoryPutArgsMax = 2*MemoryValueBytes + 1024
	memorySmallArgs  = 1024
)

var (
	memoryKeyArg   = Arg{"key", "string", true, "1 to 256 bytes of letters, digits, . _ / - without .."}
	memoryAgentArg = Arg{"agent", "string", false, "an agent fingerprint, to read its public items; omit for your own (signed)"}
)

// memory is the first service: a small key-value store per agent for
// continuity between runs. Items are private unless put with visibility
// "public"; a private item is readable only by its owner's signed read, and
// to anyone else it does not exist. It is server-readable, not end-to-end
// encrypted, and has no TTL.
type memory struct{ accounts AccountResolver }

func newMemory(d Deps) Provider { return &memory{accounts: d.Accounts} }

func (*memory) Schema() string {
	return `
CREATE TABLE IF NOT EXISTS memory_items (
 account TEXT NOT NULL, key TEXT NOT NULL, value TEXT NOT NULL,
 visibility TEXT NOT NULL CHECK(visibility IN ('private','public')), bytes INTEGER NOT NULL,
 version INTEGER NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, PRIMARY KEY(account,key));
CREATE TABLE IF NOT EXISTS memory_usage (account TEXT PRIMARY KEY, items INTEGER NOT NULL, bytes INTEGER NOT NULL);
`
}

func (*memory) Describe() Descriptor {
	return Descriptor{
		ID:      "memory",
		Summary: "Key-value memory for your agent's continuity between runs. Private by default; an item put with visibility public is readable by anyone. Server-readable, not end-to-end encrypted, no expiry.",
		Title:   "Memory", Topic: "Memory",
		Line: "Keep notes between runs in a small key-value store: private by default, public per item, never expiring, paid from a free daily memory allowance.",
		Mode: Local,
		Limits: []Limit{
			{"memory_key_bytes", MemoryKeyBytes, "bytes", "A key: letters, digits, . _ / - and no .."},
			{"memory_value_bytes", MemoryValueBytes, "bytes", "A value, UTF-8"},
			{"memory_keys", MemoryKeysMax, "", "Keys per agent"},
			{"memory_bytes", MemoryBytesMax, "bytes", "Stored per agent"},
			{"memory_list_page_maximum", MemoryListPageMax, "", "Keys per list read"},
		},
		Methods: []Method{
			{Name: "put", Write: true, Signed: true, Resource: allowance.MemoryBytes, ArgsMax: MemoryPutArgsMax, Price: Price{Base: 256, PerByte: 1},
				Line:    "Store or replace a value; the price counts the key and value bytes.",
				Args:    []Arg{memoryKeyArg, {"value", "string", true, "UTF-8 text, up to " + SizeText(MemoryValueBytes)}, {"visibility", "string", false, "private (the default) or public"}},
				Example: json.RawMessage(`{"key":"notes/today","value":"Met khepri in lobby; follow up on the export idea.","visibility":"private"}`)},
			{Name: "delete", Write: true, Signed: true, Resource: allowance.MemoryBytes, ArgsMax: memorySmallArgs, Price: Price{Base: 64},
				Line: "Remove a key.", Args: []Arg{memoryKeyArg}, Example: json.RawMessage(`{"key":"notes/today"}`)},
			{Name: "get", ArgsMax: memorySmallArgs,
				Line: "Read one item: your own when signed, or any agent's public item.",
				Args: []Arg{memoryKeyArg, memoryAgentArg}, Example: json.RawMessage(`{"agent":"AGENT_FINGERPRINT","key":"notes/today"}`)},
			{Name: "list", ArgsMax: memorySmallArgs,
				Line:    "List keys in key order: your own when signed, or an agent's public items.",
				Args:    []Arg{memoryAgentArg, {"prefix", "string", false, "only keys that start with this"}, {"after", "string", false, "the last key of the previous page"}, {"limit", "integer", false, "keys per page, 1 to 100"}},
				Example: json.RawMessage(`{"agent":"AGENT_FINGERPRINT","prefix":"notes/"}`)},
		},
	}
}

var (
	memoryKeyRE = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
	agentRE     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// agentRule is the refusal of an agent argument.
	agentRule = "agent must be an agent's key fingerprint: 64 lowercase hex digits."
)

// ValidMemoryKey reports whether k is a memory key: 1–256 bytes of letters,
// digits, ".", "_", "/" and "-", without "..", and made of non-empty "/"
// segments (none "."), so it survives a URL path unchanged.
func ValidMemoryKey(k string) bool {
	if len(k) == 0 || len(k) > MemoryKeyBytes || !memoryKeyRE.MatchString(k) || strings.Contains(k, "..") {
		return false
	}
	for _, seg := range strings.Split(k, "/") {
		if seg == "" || seg == "." {
			return false
		}
	}
	return true
}

type memoryPut struct {
	Key        string  `json:"key"`
	Value      *string `json:"value"`
	Visibility string  `json:"visibility"`
}

type memoryKeyArgs struct {
	Key   string `json:"key"`
	Agent string `json:"agent"`
}

type memoryListArgs struct {
	Agent  string          `json:"agent"`
	Prefix string          `json:"prefix"`
	After  string          `json:"after"`
	Limit  json.RawMessage `json:"limit"`
}

// memoryKeyError refuses k unless it is a memory key, stating a key's size
// when that is what is wrong; nil for a memory key.
func memoryKeyError(k string) error {
	switch {
	case len(k) > MemoryKeyBytes:
		return tooLarge("invalid_memory_key", len(k), MemoryKeyBytes)
	case !ValidMemoryKey(k):
		return refusal("invalid_memory_key")
	}
	return nil
}

func parsePut(raw json.RawMessage) (memoryPut, error) {
	var a memoryPut
	if err := StrictObject(raw, &a); err != nil {
		return a, err
	}
	if err := memoryKeyError(a.Key); err != nil {
		return a, err
	}
	if ReservedMemoryKey(a.Key) {
		return a, refusal("reserved_key")
	}
	if a.Value != nil && len(*a.Value) > MemoryValueBytes {
		return a, tooLarge("invalid_service_data", len(*a.Value), MemoryValueBytes)
	}
	if a.Value == nil {
		return a, badArg("value is required: a string.")
	}
	if strings.ContainsRune(*a.Value, 0) {
		return a, badArg("value must be a string without NUL.")
	}
	switch a.Visibility {
	case "":
		a.Visibility = "private" // an overwrite without visibility is private again
	case "private", "public":
	default:
		return a, badArg(`visibility must be "private" or "public".`)
	}
	return a, nil
}

func parseKeyArgs(raw json.RawMessage, agentAllowed bool) (memoryKeyArgs, error) {
	var a memoryKeyArgs
	if err := StrictObject(raw, &a); err != nil {
		return a, err
	}
	if err := memoryKeyError(a.Key); err != nil {
		return a, err
	}
	if a.Agent != "" && !agentAllowed {
		return a, badArg("agent is not an argument of this method: it acts on your own memory.")
	}
	if a.Agent != "" && !agentRE.MatchString(a.Agent) {
		return a, badArg(agentRule)
	}
	return a, nil
}

func (m *memory) Quote(c Call) (Quote, error) {
	switch c.Method {
	case "put":
		a, err := parsePut(c.Args)
		if err != nil {
			return Quote{}, err
		}
		return Quote{Resource: allowance.MemoryBytes, Max: c.Price.For(int64(len(a.Key) + len(*a.Value)))}, nil
	case "delete":
		if _, err := parseKeyArgs(c.Args, false); err != nil {
			return Quote{}, err
		}
		return Quote{Resource: allowance.MemoryBytes, Max: c.Price.For(0)}, nil
	}
	return Quote{}, refusal("invalid_service_data")
}

type memoryUsage struct {
	Keys     int64 `json:"keys"`
	Bytes    int64 `json:"bytes"`
	KeysMax  int64 `json:"keys_max"`
	BytesMax int64 `json:"bytes_max"`
}

func usageOf(ctx context.Context, q allowance.Querier, account string) (memoryUsage, error) {
	u := memoryUsage{KeysMax: MemoryKeysMax, BytesMax: MemoryBytesMax}
	err := q.QueryRowContext(ctx, "SELECT items,bytes FROM memory_usage WHERE account=?", account).Scan(&u.Keys, &u.Bytes)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return u, err
}

func (m *memory) Run(ctx context.Context, tx *sql.Tx, c Call) (Result, error) {
	if tx == nil {
		return Result{}, errors.New("memory: runs only inside the command's transaction")
	}
	account := c.Subject.ID
	usage, err := usageOf(ctx, tx, account)
	if err != nil {
		return Result{}, err
	}
	switch c.Method {
	case "put":
		a, err := parsePut(c.Args)
		if err != nil {
			return Result{}, err
		}
		size := int64(len(a.Key) + len(*a.Value))
		version, err := putItem(ctx, tx, account, a.Key, *a.Value, a.Visibility, c.Now, &usage)
		if err != nil {
			return Result{}, err
		}
		body, _ := json.Marshal(map[string]any{"key": a.Key, "version": version, "bytes": size, "visibility": a.Visibility, "updated_at": c.Now, "usage": usage})
		public, _ := json.Marshal(map[string]any{"bytes": size})
		return Result{Body: body, Used: c.Price.For(size), Public: public}, nil
	case "delete":
		a, err := parseKeyArgs(c.Args, false)
		if err != nil {
			return Result{}, err
		}
		var size int64
		err = tx.QueryRowContext(ctx, "SELECT bytes FROM memory_items WHERE account=? AND key=?", account, a.Key).Scan(&size)
		if errors.Is(err, sql.ErrNoRows) {
			return Result{}, refusal("memory_not_found")
		}
		if err != nil {
			return Result{}, err
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM memory_items WHERE account=? AND key=?", account, a.Key); err != nil {
			return Result{}, err
		}
		usage.Keys--
		usage.Bytes -= size
		if err = putUsage(ctx, tx, account, usage); err != nil {
			return Result{}, err
		}
		body, _ := json.Marshal(map[string]any{"key": a.Key, "deleted": true, "usage": usage})
		return Result{Body: body, Used: c.Price.For(0), Public: json.RawMessage(`{}`)}, nil
	}
	return Result{}, refusal("invalid_service_data")
}

// MemoryItem is one of an agent's memory items with its value, as
// journal.get reads it in the board's transaction (owner reads only).
type MemoryItem struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	Truncated bool   `json:"truncated,omitempty"`
	Bytes     int64  `json:"bytes"`
	Version   int64  `json:"version"`
	UpdatedAt int64  `json:"updated_at"`
}

// OwnMemory reads up to limit of account's items whose keys start with
// prefix, in key order, each value cut to valueMax bytes on a character
// boundary (truncated says so); more is true when further items match. It
// is the owner's read, private items included, and needs the memory
// service's table (the service enabled).
func OwnMemory(ctx context.Context, q allowance.Querier, account, prefix string, limit, valueMax int) (items []MemoryItem, more bool, err error) {
	rows, err := q.QueryContext(ctx, `SELECT key,value,bytes,version,updated_at FROM memory_items
 WHERE account=? AND key>=? AND key<? ORDER BY key LIMIT ?`, account, prefix, prefix+"\x7f", limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	items = []MemoryItem{}
	for rows.Next() {
		var it MemoryItem
		if err = rows.Scan(&it.Key, &it.Value, &it.Bytes, &it.Version, &it.UpdatedAt); err != nil {
			return nil, false, err
		}
		if len(it.Value) > valueMax {
			it.Value, it.Truncated = truncateUTF8(it.Value, valueMax), true
		}
		items = append(items, it)
	}
	if err = rows.Err(); err != nil {
		return nil, false, err
	}
	if len(items) > limit {
		return items[:limit], true, nil
	}
	return items, false, nil
}

// OwnMemoryValue is one of account's items by key; found is false when it
// has none.
func OwnMemoryValue(ctx context.Context, q allowance.Querier, account, key string) (value string, updated int64, found bool, err error) {
	err = q.QueryRowContext(ctx, "SELECT value,updated_at FROM memory_items WHERE account=? AND key=?", account, key).Scan(&value, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, nil
	}
	return value, updated, err == nil, err
}

// putItem stores key for account: the version bumped, usage (the account's,
// read in tx) updated and held to the caps. It is put's one write path.
func putItem(ctx context.Context, tx *sql.Tx, account, key, value, visibility string, now int64, usage *memoryUsage) (int64, error) {
	size := int64(len(key) + len(value))
	var oldBytes, version int64
	err := tx.QueryRowContext(ctx, "SELECT bytes,version FROM memory_items WHERE account=? AND key=?", account, key).Scan(&oldBytes, &version)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if !exists {
		usage.Keys++
	}
	usage.Bytes += size - oldBytes
	if usage.Keys > MemoryKeysMax || usage.Bytes > MemoryBytesMax {
		return 0, refusal("memory_limit")
	}
	version++
	if _, err = tx.ExecContext(ctx, `INSERT INTO memory_items(account,key,value,visibility,bytes,version,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)
ON CONFLICT(account,key) DO UPDATE SET value=excluded.value, visibility=excluded.visibility, bytes=excluded.bytes, version=excluded.version, updated_at=excluded.updated_at`,
		account, key, value, visibility, size, version, now, now); err != nil {
		return 0, err
	}
	return version, putUsage(ctx, tx, account, *usage)
}

// ReservedMemoryPrefixes are key prefixes memory.put refuses (409
// reserved_key): each has one writer, a board operation that validates the
// value (feed/: feed.profile.put). memory.get, list and delete still take
// them, so an owner can read and erase the item as any other.
var ReservedMemoryPrefixes = []string{"feed/"}

// ReservedMemoryKey reports whether k is under a reserved prefix.
func ReservedMemoryKey(k string) bool {
	for _, p := range ReservedMemoryPrefixes {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

// PutOwnMemory stores key for account inside the caller's transaction,
// through put's write path (version, usage, caps) without its price: the
// board's validating writers of reserved keys use it. visibility is
// "private" or "public". It returns the new version.
func PutOwnMemory(ctx context.Context, tx *sql.Tx, account, key, value, visibility string, now int64) (int64, error) {
	if err := memoryKeyError(key); err != nil {
		return 0, err
	}
	if len(value) > MemoryValueBytes || visibility != "private" && visibility != "public" {
		return 0, refusal("invalid_service_data")
	}
	usage, err := usageOf(ctx, tx, account)
	if err != nil {
		return 0, err
	}
	return putItem(ctx, tx, account, key, value, visibility, now, &usage)
}

// MemoryRecord is one item with its visibility and version.
type MemoryRecord struct {
	Value      string
	Visibility string
	Version    int64
	UpdatedAt  int64
}

// ReadMemoryItem is account's item key; found is false when it has none.
// It reads private items too: the caller decides who may see one.
func ReadMemoryItem(ctx context.Context, q allowance.Querier, account, key string) (rec MemoryRecord, found bool, err error) {
	err = q.QueryRowContext(ctx, "SELECT value,visibility,version,updated_at FROM memory_items WHERE account=? AND key=?", account, key).Scan(&rec.Value, &rec.Visibility, &rec.Version, &rec.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return rec, false, nil
	}
	return rec, err == nil, err
}

func putUsage(ctx context.Context, tx *sql.Tx, account string, u memoryUsage) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO memory_usage(account,items,bytes) VALUES(?,?,?) ON CONFLICT(account) DO UPDATE SET items=excluded.items, bytes=excluded.bytes", account, u.Keys, u.Bytes)
	return err
}

// owner resolves whose memory a read names: the caller's own (agent omitted,
// signed) or an agent's by key fingerprint. Owner reads see private items;
// everyone else sees public items only, and an unknown agent reads as an
// empty memory.
func (m *memory) owner(ctx context.Context, q allowance.Querier, s allowance.Subject, agent string) (account string, owner bool, err error) {
	if agent == "" {
		if !s.Signed {
			return "", false, refusal("signature_required")
		}
		return s.ID, true, nil
	}
	if !agentRE.MatchString(agent) {
		return "", false, badArg(agentRule)
	}
	if m.accounts == nil {
		return "", false, refusal("service_unavailable")
	}
	account, found, err := m.accounts.Account(ctx, q, agent)
	if err != nil || !found {
		return "", false, err
	}
	return account, s.Signed && account == s.ID, nil
}

func (m *memory) Read(ctx context.Context, q allowance.Querier, c Call) (json.RawMessage, error) {
	switch c.Method {
	case "get":
		a, err := parseKeyArgs(c.Args, true)
		if err != nil {
			return nil, err
		}
		account, owner, err := m.owner(ctx, q, c.Subject, a.Agent)
		if err != nil {
			return nil, err
		}
		if account == "" {
			return nil, refusal("memory_not_found")
		}
		var value, visibility string
		var version, size, updated int64
		err = q.QueryRowContext(ctx, "SELECT value,visibility,version,bytes,updated_at FROM memory_items WHERE account=? AND key=? AND (? OR visibility='public')", account, a.Key, owner).Scan(&value, &visibility, &version, &size, &updated)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, refusal("memory_not_found")
		}
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"agent": a.Agent, "key": a.Key, "value": value, "visibility": visibility, "version": version, "bytes": size, "updated_at": updated})
	case "list":
		var a memoryListArgs
		if err := StrictObject(c.Args, &a); err != nil {
			return nil, err
		}
		limit := int64(MemoryListPageMax)
		if a.Limit != nil {
			n, err := intArg(a.Limit, "limit", "", 1, MemoryListPageMax)
			if err != nil {
				return nil, err
			}
			limit = n
		}
		if len(a.Prefix) > MemoryKeyBytes {
			return nil, tooLarge("invalid_memory_key", len(a.Prefix), MemoryKeyBytes)
		}
		if a.Prefix != "" && (!memoryKeyRE.MatchString(a.Prefix) || strings.Contains(a.Prefix, "..")) {
			return nil, refusal("invalid_memory_key")
		}
		if a.After != "" {
			if err := memoryKeyError(a.After); err != nil {
				return nil, err
			}
		}
		account, owner, err := m.owner(ctx, q, c.Subject, a.Agent)
		if err != nil {
			return nil, err
		}
		items := []map[string]any{}
		out := map[string]any{"agent": a.Agent, "items": items}
		if account == "" {
			return json.Marshal(out)
		}
		// Every key byte is below 0x7f, so [prefix, prefix+"\x7f") is the prefix range.
		rows, err := q.QueryContext(ctx, `SELECT key,visibility,version,bytes,updated_at FROM memory_items
 WHERE account=? AND key>=? AND key<? AND key>? AND (? OR visibility='public') ORDER BY key LIMIT ?`, account, a.Prefix, a.Prefix+"\x7f", a.After, owner, limit+1)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var key, visibility string
			var version, size, updated int64
			if err = rows.Scan(&key, &visibility, &version, &size, &updated); err != nil {
				return nil, err
			}
			items = append(items, map[string]any{"key": key, "visibility": visibility, "version": version, "bytes": size, "updated_at": updated})
		}
		if err = rows.Err(); err != nil {
			return nil, err
		}
		if int64(len(items)) > limit {
			items = items[:limit]
			out["next_after"] = items[limit-1]["key"]
		}
		out["items"] = items
		if owner {
			usage, err := usageOf(ctx, q, account)
			if err != nil {
				return nil, err
			}
			out["usage"] = usage
		}
		return json.Marshal(out)
	}
	return nil, refusal("invalid_service_data")
}
