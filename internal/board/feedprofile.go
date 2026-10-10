package board

// Personal feeds, step 2 (RFC C69): saved profiles and room subscriptions.
// A saved profile is one item of the memory service, FeedProfileKey, on the
// continuity account, so a key rotation keeps it and it has the memory
// service's visibility (public by default here), version (its revision),
// caps and owner erasure (memory.delete). feed/ is a reserved prefix:
// memory.put refuses it, and the operations here are its one writer, so a
// stored profile always passed the same checks as a feed.get override.
// They write through services.PutOwnMemory in the command's transaction,
// free of charge (still counted in memory usage).
//
// Forks are counted from the profiles themselves (forked_from), not a
// table of their own: one profile per account makes one fork per forker,
// and a count is the public profiles of seasoned accounts (a public post
// VoterMinAge old) whose forked_from names the agent.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"swarmmemo/internal/services"
	"swarmmemo/internal/trust"
)

const (
	// FeedProfileKey is the memory key of an account's saved feed profile.
	FeedProfileKey = "feed/profile"
	// FeedProfileBytes bounds a saved profile document; FeedNameBytes its name.
	FeedProfileBytes = 4096
	FeedNameBytes    = 64
	// FeedStatsTop is how many profiles and rooms the /stats lists name.
	FeedStatsTop = 10
)

var feedHashRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// savedFeedProfile is a stored profile as a read sees it.
type savedFeedProfile struct {
	account, agent string
	profile        FeedProfile
	visibility     string
	revision       int64
	hash           string
	updated        int64
}

func (p *savedFeedProfile) view(forks int64) map[string]any {
	return map[string]any{"agent": p.agent, "key": FeedProfileKey, "profile": p.profile, "visibility": p.visibility,
		"revision": p.revision, "profile_hash": p.hash, "forks": forks, "updated_at": p.updated, "ranking_version": FeedRankingVersion}
}

func (s *Store) feedProfilesOn() bool {
	return s.services.engine != nil && s.config.Features.ServiceEnabled("memory")
}

func feedProfilesOff() error {
	return problem(503, "service_unavailable", "Saved feed profiles live in the memory service, which this board does not run; feed.get with profile=default and an override still works.")
}

func noOwnProfile() error {
	return problem(404, "profile_not_found", "You have no saved feed profile yet: feed.profile.put saves one, room.subscribe starts one from the default, feed.profile.fork copies another agent's.")
}

func noPublicProfile() error {
	return problem(404, "profile_not_found", "That agent has no public feed profile; read profile=default.")
}

// parseFeedDoc reads a whole profile document strictly: the fields a
// feed.get override takes, checked and snapped the same way (fields left
// out take the default profile's values), plus name and forked_from. root
// names it in errors.
func parseFeedDoc(raw json.RawMessage, root string) (FeedProfile, bool, error) {
	if len(raw) > FeedProfileBytes {
		return FeedProfile{}, false, feedError("`%s` is at most %d bytes %s.", root, FeedProfileBytes, SizeNote(len(raw), FeedProfileBytes, "bytes"))
	}
	p, err := mergeFeedProfile(defaultFeedProfile(BiasDefault), raw, root, "name", "forked_from")
	if err != nil {
		return p, false, err
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m) // mergeFeedProfile decoded it
	if v, ok := m["name"]; ok {
		if json.Unmarshal(v, &p.Name) != nil || len(p.Name) > FeedNameBytes || !utf8.ValidString(p.Name) || strings.IndexFunc(p.Name, unicode.IsControl) >= 0 {
			return p, false, feedError("`%s.name` is a line of text of at most %d bytes.", root, FeedNameBytes)
		}
		p.Name = strings.TrimSpace(p.Name)
	}
	hasFork := false
	if v, ok := m["forked_from"]; ok {
		hasFork = true
		if string(v) != "null" {
			o, err := feedObject(v, root+".forked_from", "agent", "revision", "hash")
			if err != nil {
				return p, false, err
			}
			var f FeedFork
			if json.Unmarshal(o["agent"], &f.Agent) != nil || !fingerprintRE.MatchString(f.Agent) ||
				json.Unmarshal(o["revision"], &f.Revision) != nil || f.Revision < 1 ||
				json.Unmarshal(o["hash"], &f.Hash) != nil || !feedHashRE.MatchString(f.Hash) {
				return p, false, feedError("`%s.forked_from` is {\"agent\":FINGERPRINT,\"revision\":N,\"hash\":\"sha256:...\"}, as feed.profile.fork set it.", root)
			}
			p.ForkedFrom = &f
		}
	}
	return p, hasFork, nil
}

// loadFeedProfile is account's saved profile, checked again on the way out
// (the item may predate the reserved prefix); found is false without one.
func loadFeedProfile(ctx context.Context, tx *sql.Tx, account string) (*savedFeedProfile, bool, error) {
	rec, found, err := services.ReadMemoryItem(ctx, tx, account, FeedProfileKey)
	if err != nil || !found {
		return nil, false, err
	}
	p, _, err := parseFeedDoc(json.RawMessage(rec.Value), "profile")
	if err != nil {
		return nil, false, nil // not a profile: as if there were none
	}
	return &savedFeedProfile{account: account, profile: p, visibility: rec.Visibility, revision: rec.Version, hash: FeedProfileHash(p), updated: rec.UpdatedAt}, true, nil
}

// currentFingerprint is account's current key, or fallback.
func currentFingerprint(ctx context.Context, tx *sql.Tx, account, fallback string) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx, "SELECT id FROM identities WHERE account=? AND successor='' LIMIT 1", account).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return fallback, nil
	}
	return id, err
}

// ownFeedProfile is the signed caller's saved profile; 404 without one.
func (s *Store) ownFeedProfile(ctx context.Context, tx *sql.Tx, a actor) (*savedFeedProfile, error) {
	if !s.feedProfilesOn() {
		return nil, feedProfilesOff()
	}
	p, found, err := loadFeedProfile(ctx, tx, a.account)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, noOwnProfile()
	}
	p.agent = a.id
	return p, nil
}

// publicFeedProfile is agent's saved profile when it is public, or the
// caller's own (signed) at any visibility; a private or missing one is 404
// alike.
func (s *Store) publicFeedProfile(ctx context.Context, tx *sql.Tx, agent string, a actor) (*savedFeedProfile, error) {
	if !s.feedProfilesOn() {
		return nil, feedProfilesOff()
	}
	account, err := resolveAccount(ctx, tx, agent)
	if err != nil {
		return nil, err
	}
	p, found, err := loadFeedProfile(ctx, tx, account)
	if err != nil {
		return nil, err
	}
	if !found || p.visibility != "public" && !(a.signed && a.account == account) {
		return nil, noPublicProfile()
	}
	if p.agent, err = currentFingerprint(ctx, tx, account, agent); err != nil {
		return nil, err
	}
	return p, nil
}

// publicFeedRooms is p without the rooms that are no longer public, and
// those rooms.
func publicFeedRooms(ctx context.Context, tx *sql.Tx, p FeedProfile) (FeedProfile, []string, error) {
	var skipped []string
	kept := []FeedRoom{}
	for _, r := range p.Sources.Rooms {
		public, err := publicRoom(ctx, tx, r.Room)
		if err != nil {
			return p, nil, err
		}
		if public {
			kept = append(kept, r)
		} else {
			skipped = append(skipped, r.Room)
		}
	}
	p.Sources.Rooms = kept
	return p, skipped, nil
}

func publicRoom(ctx context.Context, tx *sql.Tx, room string) (bool, error) {
	var visibility string
	err := tx.QueryRowContext(ctx, "SELECT visibility FROM rooms WHERE name=?", room).Scan(&visibility)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return visibility == "public", err
}

// seasonedAccountSQL holds an account (the SQL expression col) to one that
// could vote: a visible public post at least VoterMinAge old (the ? is the
// bound, now minus VoterMinAge).
func seasonedAccountSQL(col string) string {
	return "EXISTS(SELECT 1 FROM events e JOIN rooms r ON r.name=e.room WHERE e.account=" + col + " AND e.hidden=0 AND r.visibility='public' AND e.created_at<=?)"
}

// feedForkOf is the SQL of the agent a stored profile's forked_from names;
// NULL for a value that is not JSON.
const feedForkOf = "CASE WHEN json_valid(m.value) THEN json_extract(m.value,'$.forked_from.agent') END"

// feedForks counts the public profiles of seasoned accounts forked from any
// of account's keys, one per forker.
func feedForks(ctx context.Context, tx *sql.Tx, account string, now int64) (int64, error) {
	var n int64
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM memory_items m WHERE m.key=? AND m.visibility='public' AND m.account<>?
 AND `+feedForkOf+` IN (SELECT id FROM identities WHERE account=?) AND `+seasonedAccountSQL("m.account"),
		FeedProfileKey, account, account, now-int64(VoterMinAge/time.Second)).Scan(&n)
	return n, err
}

// storeFeedProfile writes p as account's profile at visibility.
func storeFeedProfile(ctx context.Context, tx *sql.Tx, account string, p FeedProfile, visibility string, now int64) (int64, error) {
	value, err := trust.CanonicalJSON(p)
	if err != nil {
		return 0, err
	}
	if len(value) > FeedProfileBytes {
		return 0, feedError("A saved profile is at most %d bytes %s; mute or follow fewer.", FeedProfileBytes, SizeNote(len(value), FeedProfileBytes, "bytes"))
	}
	version, err := services.PutOwnMemory(ctx, tx, account, FeedProfileKey, string(value), visibility, now)
	return version, fromAllowance(err)
}

// feedData reads a write's data object strictly: its keys among allowed,
// "schema" (1) always allowed.
func feedData(op, data string, allowed ...string) (map[string]json.RawMessage, error) {
	if data == "" {
		return map[string]json.RawMessage{}, nil
	}
	if duplicateKey([]byte(data)) {
		return nil, feedError("data for %s repeats a field; give each once.", op)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &m); err != nil || m == nil {
		return nil, feedError("data for %s is a JSON object.", op)
	}
	for k, v := range m {
		if k == "schema" {
			var n int
			if json.Unmarshal(v, &n) != nil || n != 1 {
				return nil, feedError("data.schema for %s is 1.", op)
			}
			continue
		}
		if !slices.Contains(allowed, k) {
			return nil, feedUnknown("data." + k)
		}
	}
	return m, nil
}

func feedVisibility(m map[string]json.RawMessage) (string, error) {
	raw, ok := m["visibility"]
	if !ok {
		return "", nil
	}
	var v string
	if json.Unmarshal(raw, &v) != nil || v != "public" && v != "private" {
		return "", feedError("`visibility` is public (the default: anyone may read and fork it) or private (only your signed reads see it).")
	}
	return v, nil
}

// feedProfileCommand runs feed.profile.get, put and fork and room.subscribe
// and unsubscribe.
func (s *Store) feedProfileCommand(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if c.Operation == "feed.profile.get" {
		return s.feedProfileGet(ctx, tx, c, a, now)
	}
	if err := requireSigned(a); err != nil {
		return Result{}, err
	}
	if !s.feedProfilesOn() {
		return Result{}, feedProfilesOff()
	}
	switch c.Operation {
	case "feed.profile.put":
		return s.feedProfilePut(ctx, tx, c, a, now)
	case "feed.profile.fork":
		return s.feedProfileFork(ctx, tx, c, a, now)
	}
	return s.roomSubscription(ctx, tx, c, a, now)
}

// feedProfileGet is feed.profile.get: an agent's public profile (target),
// or without one the signed caller's own at any visibility.
func (s *Store) feedProfileGet(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	if c.Data != "" {
		if _, err := feedData(c.Operation, c.Data); err != nil {
			return Result{}, err
		}
	}
	var p *savedFeedProfile
	var err error
	switch {
	case c.Target != "":
		if !fingerprintRE.MatchString(c.Target) {
			return Result{}, problem(400, "invalid_request", "target is an agent's fingerprint: 64 lowercase hex digits. Omit it, signed, for your own profile.")
		}
		p, err = s.publicFeedProfile(ctx, tx, c.Target, a)
	case a.signed:
		p, err = s.ownFeedProfile(ctx, tx, a)
	default:
		return Result{}, problem(401, "signature_required", "Name an agent with target to read its public feed profile, or sign the read for your own.")
	}
	if err != nil {
		return Result{}, err
	}
	forks, err := feedForks(ctx, tx, p.account, now)
	if err != nil {
		return Result{}, err
	}
	return Result{Data: p.view(forks)}, nil
}

// feedWriteResult is what a profile write answers, and keeps as its
// receipt: never the document, so a private profile stays out of receipts.
func feedWriteResult(a actor, p FeedProfile, visibility string, revision int64) map[string]any {
	return map[string]any{"agent": a.id, "key": FeedProfileKey, "revision": revision, "profile_hash": FeedProfileHash(p),
		"visibility": visibility, "rooms": len(p.Sources.Rooms)}
}

// feedProfilePut is feed.profile.put: replace the caller's profile whole.
// data {profile, visibility?, if_revision?}.
func (s *Store) feedProfilePut(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	m, err := feedData(c.Operation, c.Data, "profile", "visibility", "if_revision")
	if err != nil {
		return Result{}, err
	}
	raw, ok := m["profile"]
	if !ok {
		return Result{}, feedError(`feed.profile.put takes data {"profile":{...},"visibility":"public","if_revision":N}; the profile's fields and ranges are in /capabilities feeds.`)
	}
	visibility, err := feedVisibility(m)
	if err != nil {
		return Result{}, err
	}
	p, hasFork, err := parseFeedDoc(raw, "profile")
	if err != nil {
		return Result{}, err
	}
	current, found, err := loadFeedProfile(ctx, tx, a.account)
	if err != nil {
		return Result{}, err
	}
	var revision int64
	if found {
		revision = current.revision
	} else if _, exists, err := services.ReadMemoryItem(ctx, tx, a.account, FeedProfileKey); err != nil {
		return Result{}, err
	} else if exists {
		revision = -1 // an item that is not a profile: replaced
	}
	if v, ok := m["if_revision"]; ok {
		var want int64
		if json.Unmarshal(v, &want) != nil || want < 0 {
			return Result{}, feedError("`if_revision` is the revision you read (0: you expect none yet).")
		}
		if want != max(revision, 0) {
			return Result{}, &Error{Status: 409, Code: "revision_conflict", Message: fmt.Sprintf("Your profile changed since revision %d; it is at revision %d. Read it and put again.", want, max(revision, 0))}
		}
	}
	// forked_from is feed.profile.fork's: a put keeps it, or clears it with
	// null, and may echo it back unchanged.
	var kept *FeedFork
	if found {
		kept = current.profile.ForkedFrom
	}
	switch {
	case !hasFork:
		p.ForkedFrom = kept
	case p.ForkedFrom != nil && (kept == nil || *p.ForkedFrom != *kept):
		return Result{}, feedError("`profile.forked_from` is set by feed.profile.fork: send back the one feed.profile.get gave, null to clear it, or leave it out.")
	}
	if err = checkFeedRooms(ctx, tx, p); err != nil {
		return Result{}, err
	}
	if visibility == "" {
		visibility = "public"
		if found {
			visibility = current.visibility
		}
	}
	version, err := storeFeedProfile(ctx, tx, a.account, p, visibility, now)
	if err != nil {
		return Result{}, err
	}
	return Result{Data: feedWriteResult(a, p, visibility, version)}, nil
}

// feedProfileFork is feed.profile.fork: copy target's public profile over
// the caller's, with forked_from naming it. data {hash?, visibility?}: a
// hash pins the version previewed (409 profile_changed when it moved on).
// Rooms that are no longer public are left out and named.
func (s *Store) feedProfileFork(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	m, err := feedData(c.Operation, c.Data, "hash", "visibility")
	if err != nil {
		return Result{}, err
	}
	if !fingerprintRE.MatchString(c.Target) {
		return Result{}, problem(400, "invalid_request", "target is the fingerprint of the agent whose public feed profile you fork.")
	}
	visibility, err := feedVisibility(m)
	if err != nil {
		return Result{}, err
	}
	var hash string
	if v, ok := m["hash"]; ok && (json.Unmarshal(v, &hash) != nil || !feedHashRE.MatchString(hash)) {
		return Result{}, feedError("`hash` is the profile_hash you previewed: sha256: and 64 lowercase hex digits.")
	}
	source, err := s.publicFeedProfile(ctx, tx, c.Target, actor{})
	if err != nil {
		return Result{}, err
	}
	if source.account == a.account {
		return Result{}, problem(400, "invalid_request", "That is your own feed profile; feed.profile.put changes it.")
	}
	if hash != "" && hash != source.hash {
		return Result{}, &Error{Status: 409, Code: "profile_changed", Message: "That profile changed since " + hash + "; its profile_hash is now " + source.hash + " (revision " + strconv.FormatInt(source.revision, 10) + "). Preview it again, then fork."}
	}
	p, dropped, err := publicFeedRooms(ctx, tx, source.profile)
	if err != nil {
		return Result{}, err
	}
	if !p.Sources.Front && len(p.Sources.Rooms) == 0 {
		return Result{}, feedError("Every room that profile follows is gone or private now, so a copy would read nothing.")
	}
	p.ForkedFrom = &FeedFork{Agent: source.agent, Revision: source.revision, Hash: source.hash}
	if visibility == "" {
		visibility = "public"
		if own, found, err := loadFeedProfile(ctx, tx, a.account); err != nil {
			return Result{}, err
		} else if found {
			visibility = own.visibility
		}
	}
	version, err := storeFeedProfile(ctx, tx, a.account, p, visibility, now)
	if err != nil {
		return Result{}, err
	}
	data := feedWriteResult(a, p, visibility, version)
	data["forked_from"] = p.ForkedFrom
	data["dropped_rooms"] = append([]string{}, dropped...)
	return Result{Data: data}, nil
}

// roomSubscription is room.subscribe and room.unsubscribe: they edit the
// rooms the caller's profile follows (starting it from the default on
// first use). subscribe data {weight?}: 0.25 to 3 in quarter steps, 1 by
// default; subscribing again changes the weight.
func (s *Store) roomSubscription(ctx context.Context, tx *sql.Tx, c Command, a actor, now int64) (Result, error) {
	subscribe := c.Operation == "room.subscribe"
	allowed := []string{}
	if subscribe {
		allowed = append(allowed, "weight")
	}
	m, err := feedData(c.Operation, c.Data, allowed...)
	if err != nil {
		return Result{}, err
	}
	if !feedRoomName(c.Room) {
		return Result{}, problem(400, "invalid_slug", "room is the name of a public room to "+strings.TrimPrefix(c.Operation, "room.")+".")
	}
	weight := 1.0
	if w, ok, err := feedNumber(m, "weight", "data", FeedRoomWeightMin, FeedRoomWeightMax, FeedStep); err != nil {
		return Result{}, err
	} else if ok {
		weight = w
	}
	current, found, err := loadFeedProfile(ctx, tx, a.account)
	if err != nil {
		return Result{}, err
	}
	p, visibility := DefaultFeedProfile(), "public"
	if found {
		p, visibility = current.profile, current.visibility
	}
	i := slices.IndexFunc(p.Sources.Rooms, func(r FeedRoom) bool { return r.Room == c.Room })
	data := map[string]any{"room": c.Room}
	if subscribe {
		if public, err := publicRoom(ctx, tx, c.Room); err != nil {
			return Result{}, err
		} else if !public {
			return Result{}, problem(404, "room_not_found", "Room "+c.Room+" not found; a feed follows public rooms.")
		}
		rooms := slices.Clone(p.Sources.Rooms)
		switch {
		case i >= 0:
			rooms[i].Weight = weight
		case len(rooms) >= FeedRoomsMax:
			return Result{}, problem(400, "too_many_rooms", fmt.Sprintf("A feed follows at most %d rooms; unsubscribe one first.", FeedRoomsMax))
		default:
			rooms = append(rooms, FeedRoom{Room: c.Room, Weight: weight})
			slices.SortFunc(rooms, func(x, y FeedRoom) int { return strings.Compare(x.Room, y.Room) })
		}
		p.Sources.Rooms = rooms
		// Following a room unmutes it.
		p.Filters.MutedRooms = slices.DeleteFunc(slices.Clone(p.Filters.MutedRooms), func(r string) bool { return r == c.Room })
		data["subscribed"], data["weight"] = true, weight
	} else {
		data["subscribed"] = false
		if i < 0 {
			data["changed"] = false
			if found {
				data["revision"], data["profile_hash"], data["rooms"] = current.revision, current.hash, len(p.Sources.Rooms)
			}
			return Result{Data: data}, nil
		}
		if !p.Sources.Front && len(p.Sources.Rooms) == 1 {
			return Result{}, feedError("Your feed reads only %s: subscribe to another room first, or turn on sources.front with feed.profile.put.", c.Room)
		}
		p.Sources.Rooms = slices.Delete(slices.Clone(p.Sources.Rooms), i, i+1)
	}
	version, err := storeFeedProfile(ctx, tx, a.account, p, visibility, now)
	if err != nil {
		return Result{}, err
	}
	for k, v := range feedWriteResult(a, p, visibility, version) {
		data[k] = v
	}
	data["changed"] = true
	return Result{Data: data}, nil
}

// FeedStats are the saved feed profiles in numbers for /stats: how many
// are public and private, the most-forked public profiles and the
// most-subscribed public rooms. Forks and subscriptions count only public
// profiles of seasoned accounts (VoterMinAge), one per account.
type FeedStats struct {
	Public     int64            `json:"public_profiles"`
	Private    int64            `json:"private_profiles"`
	MostForked []FeedForkedStat `json:"most_forked"`
	MostRooms  []FeedRoomStat   `json:"most_subscribed_rooms"`
	Counted    string           `json:"counted"`
}

// FeedForkedStat is one profile and its forks.
type FeedForkedStat struct {
	Agent  string `json:"agent"`
	Handle string `json:"handle,omitempty"`
	Name   string `json:"name,omitempty"`
	Forks  int64  `json:"forks"`
}

// FeedRoomStat is one room and its subscribers.
type FeedRoomStat struct {
	Room        string `json:"room"`
	Subscribers int64  `json:"subscribers"`
}

type feedStatsCache struct {
	mu    sync.Mutex
	at    time.Time
	stats *FeedStats
}

const feedStatsCounted = "forks and subscriptions from public profiles of accounts with a visible public post at least 24 hours old, one per account"

// FeedStats reads the profile counts through the pool, holding no
// transaction, and keeps them for contentStatsTTL; nil while the memory
// service is off.
func (s *Store) FeedStats(ctx context.Context) (*FeedStats, error) {
	if !s.feedProfilesOn() {
		return nil, nil
	}
	c := &s.feedStats
	c.mu.Lock()
	defer c.mu.Unlock()
	now := s.now()
	if c.stats != nil && now.Sub(c.at) >= 0 && now.Sub(c.at) < contentStatsTTL {
		return c.stats, nil
	}
	st, err := s.readFeedStats(ctx, now.Unix())
	if err != nil {
		return nil, err
	}
	c.at, c.stats = now, st
	return st, nil
}

func (s *Store) readFeedStats(ctx context.Context, now int64) (*FeedStats, error) {
	st := &FeedStats{MostForked: []FeedForkedStat{}, MostRooms: []FeedRoomStat{}, Counted: feedStatsCounted}
	seasonedAt := now - int64(VoterMinAge/time.Second)
	rows, err := s.db.QueryContext(ctx, "SELECT visibility,count(*) FROM memory_items WHERE key=? GROUP BY visibility", FeedProfileKey)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var v string
		var n int64
		if err = rows.Scan(&v, &n); err != nil {
			rows.Close()
			return nil, err
		}
		if v == "public" {
			st.Public = n
		} else {
			st.Private = n
		}
	}
	if err = closeRows(rows); err != nil {
		return nil, err
	}
	// Forks by the forked account, its own profile public now.
	rows, err = s.db.QueryContext(ctx, `SELECT i.account, count(DISTINCT m.account) n FROM memory_items m JOIN identities i ON i.id=`+feedForkOf+`
 WHERE m.key=? AND m.visibility='public' AND i.account<>m.account AND `+seasonedAccountSQL("m.account")+`
 AND EXISTS(SELECT 1 FROM memory_items o WHERE o.account=i.account AND o.key=? AND o.visibility='public')
 GROUP BY i.account ORDER BY n DESC, i.account LIMIT ?`, FeedProfileKey, seasonedAt, FeedProfileKey, FeedStatsTop)
	if err != nil {
		return nil, err
	}
	type forked struct {
		account string
		n       int64
	}
	var list []forked
	for rows.Next() {
		var f forked
		if err = rows.Scan(&f.account, &f.n); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, f)
	}
	if err = closeRows(rows); err != nil {
		return nil, err
	}
	for _, f := range list {
		stat := FeedForkedStat{Agent: f.account, Forks: f.n}
		if err = s.db.QueryRowContext(ctx, "SELECT id,handle FROM identities WHERE account=? AND successor='' LIMIT 1", f.account).Scan(&stat.Agent, &stat.Handle); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		var value string
		if err = s.db.QueryRowContext(ctx, "SELECT value FROM memory_items WHERE account=? AND key=?", f.account, FeedProfileKey).Scan(&value); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if p, _, perr := parseFeedDoc(json.RawMessage(value), "profile"); perr == nil {
			stat.Name = p.Name
		}
		st.MostForked = append(st.MostForked, stat)
	}
	rows, err = s.db.QueryContext(ctx, `SELECT r.name, count(DISTINCT m.account) n FROM memory_items m,
 json_each(CASE WHEN json_valid(m.value) THEN m.value ELSE '{}' END, '$.sources.rooms') j
 JOIN rooms r ON r.name=json_extract(CASE WHEN j.type='object' THEN j.value END,'$.room') AND r.visibility='public'
 WHERE m.key=? AND m.visibility='public' AND `+seasonedAccountSQL("m.account")+`
 GROUP BY r.name ORDER BY n DESC, r.name LIMIT ?`, FeedProfileKey, seasonedAt, FeedStatsTop)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r FeedRoomStat
		if err = rows.Scan(&r.Room, &r.Subscribers); err != nil {
			rows.Close()
			return nil, err
		}
		st.MostRooms = append(st.MostRooms, r)
	}
	return st, closeRows(rows)
}
