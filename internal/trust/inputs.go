package trust

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

// Inputs are the paged, read-only sources of one run (§4.3 schedule). The
// board implements it (trustwire.go): Read calls emit once per record, reading
// pages of at most PageRows rows, each in its own short read transaction, so
// nothing holds the store's one connection while the run computes.
type Inputs interface {
	Read(ctx context.Context, asOf int64, p Params, emit func(Record) error) error
}

// PageRows is the largest page an input reader may read in one transaction.
const PageRows = 2000

// Snapshot is everything one run reads, as typed records. Its JSONL form (one
// Record per line, any order) is the golden fixture's input format and what a
// third party rebuilds from /v1/export, /v1/export?stream=endorsements,
// /api/ledger, /api/trust/runs and /api/params/trust to recompute a run.
type Snapshot struct {
	Meta         Meta
	Params       Params
	Accounts     []Record // type "account"
	Posts        []Record // type "post"
	Endorsements []Record // type "endorsement"
	Proofs       []Record // type "proof"
	Breakers     []Record // type "breaker"
	Transfers    []Record // type "transfer"
	Claims       []Record // type "claim"
	Priors       []Record // type "prior"
	Penalties    []Record // type "penalty"
	Sponsorships []Record // type "sponsorship"
	// Standing inputs (RFC0015, parameter version 2): edges that are not
	// endorsement records, and credit spent.
	Acts   []Record // type "edge"
	Spends []Record // type "spend"
}

// Meta is the run's frame: its as-of time and the input positions it read.
type Meta struct {
	Schema            int64 `json:"schema"`
	AsOf              int64 `json:"as_of"`
	PriorRuns         int64 `json:"prior_runs"`
	PauseNewKeysSince int64 `json:"pause_new_keys_since"`
	EventsSeq         int64 `json:"events_seq"`
	EndorsementsSeq   int64 `json:"endorsements_seq"`
	LedgerSeq         int64 `json:"ledger_seq"`
}

// Record is one input line. Type selects the fields that apply:
//
//	meta         schema as_of prior_runs pause_new_keys_since events_seq endorsements_seq ledger_seq
//	params       version body
//	account      account first_seen
//	post         id account created_at reply_to reply_to_account
//	endorsement  seq kind voter target message_id value sponsor created_at
//	             (kind: vote, vouch, legacy_vote, unsigned; target is the
//	             account the edge points at: the author of the voted post's
//	             edit-chain root, or the vouched account)
//	proof        account kind link_value state created_at checked_at link_account
//	             registered_at (a domain's registry creation time, when known)
//	             root assessed (from version 5, an assessed root's key and
//	             assessment: wallet, github, pow)
//	breaker      account started_at trust_until
//	transfer     from to amount created_at
//	claim        account day claimed spent
//	prior        account flow_sum standing (standing: nonzero in the latest run)
//	penalty      account evidence fraction_ppm ends_at
//	sponsorship  invitee sponsor_account high_water
//	edge         kind id from to created_at (kind: work_accept, the accepting
//	             key's account → the worker's, id the work item; witness, the
//	             witness's account → the witnessed agent's, verdict verified)
//	spend        account day amount (paid and earned credit spent that day)
//	             to link_value (from version 3: the payee, an account or a
//	             host, when the payment went to one: a self-dealt spend is
//	             not seed)
type Record struct {
	Type string `json:"type"`
	// meta
	Schema            int64 `json:"schema,omitempty"`
	AsOf              int64 `json:"as_of,omitempty"`
	PriorRuns         int64 `json:"prior_runs,omitempty"`
	PauseNewKeysSince int64 `json:"pause_new_keys_since,omitempty"`
	EventsSeq         int64 `json:"events_seq,omitempty"`
	EndorsementsSeq   int64 `json:"endorsements_seq,omitempty"`
	LedgerSeq         int64 `json:"ledger_seq,omitempty"`
	// params
	Version int64           `json:"version,omitempty"`
	Body    json.RawMessage `json:"body,omitempty"`
	// shared
	Account   string `json:"account,omitempty"`
	CreatedAt int64  `json:"created_at,omitempty"`
	// account
	FirstSeen int64 `json:"first_seen,omitempty"`
	// post
	ID             string `json:"id,omitempty"`
	ReplyTo        string `json:"reply_to,omitempty"`
	ReplyToAccount string `json:"reply_to_account,omitempty"`
	// endorsement
	Seq       int64  `json:"seq,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Voter     string `json:"voter,omitempty"`
	Target    string `json:"target,omitempty"`
	MessageID string `json:"message_id,omitempty"`
	Value     int64  `json:"value,omitempty"`
	Sponsor   bool   `json:"sponsor,omitempty"`
	// Weight is a vouch's author-chosen weight (1..50; 0 is the default),
	// read from parameter version 4.
	Weight int64 `json:"weight,omitempty"`
	// proof (Kind shared)
	LinkValue   string `json:"link_value,omitempty"`
	State       string `json:"state,omitempty"`
	CheckedAt   int64  `json:"checked_at,omitempty"`
	LinkAccount string `json:"link_account,omitempty"`
	// RegisteredAt is a domain's registration time (the registry's RDAP
	// creation date), when known; read from parameter version 3.
	RegisteredAt int64 `json:"registered_at,omitempty"`
	// Root and Assessed are an assessed proof's root key and assessment
	// (from version 5: wallet, github and pow; roots.go).
	Root     string `json:"root,omitempty"`
	Assessed int64  `json:"assessed,omitempty"`
	// breaker
	StartedAt  int64 `json:"started_at,omitempty"`
	TrustUntil int64 `json:"trust_until,omitempty"`
	// transfer
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
	Amount int64  `json:"amount,omitempty"`
	// claim
	Day     int64 `json:"day,omitempty"`
	Claimed int64 `json:"claimed,omitempty"`
	Spent   int64 `json:"spent,omitempty"`
	// prior
	FlowSum  int64 `json:"flow_sum,omitempty"`
	Standing bool  `json:"standing,omitempty"`
	// penalty
	Evidence    string `json:"evidence,omitempty"`
	FractionPPM int64  `json:"fraction_ppm,omitempty"`
	EndsAt      int64  `json:"ends_at,omitempty"`
	// sponsorship
	Invitee   string `json:"invitee,omitempty"`
	SponsorOf string `json:"sponsor_account,omitempty"`
	HighWater int64  `json:"high_water,omitempty"`
}

// Add files one record under its type.
func (s *Snapshot) Add(r Record) error {
	switch r.Type {
	case "meta":
		s.Meta = Meta{Schema: r.Schema, AsOf: r.AsOf, PriorRuns: r.PriorRuns, PauseNewKeysSince: r.PauseNewKeysSince, EventsSeq: r.EventsSeq, EndorsementsSeq: r.EndorsementsSeq, LedgerSeq: r.LedgerSeq}
	case "params":
		p, err := ParseParams(r.Version, r.Body)
		if err != nil {
			return err
		}
		s.Params = p
	case "account":
		s.Accounts = append(s.Accounts, r)
	case "post":
		s.Posts = append(s.Posts, r)
	case "endorsement":
		s.Endorsements = append(s.Endorsements, r)
	case "proof":
		s.Proofs = append(s.Proofs, r)
	case "breaker":
		s.Breakers = append(s.Breakers, r)
	case "transfer":
		s.Transfers = append(s.Transfers, r)
	case "claim":
		s.Claims = append(s.Claims, r)
	case "prior":
		s.Priors = append(s.Priors, r)
	case "penalty":
		s.Penalties = append(s.Penalties, r)
	case "sponsorship":
		s.Sponsorships = append(s.Sponsorships, r)
	case "edge":
		s.Acts = append(s.Acts, r)
	case "spend":
		s.Spends = append(s.Spends, r)
	default:
		return fmt.Errorf("trust input: unknown record type %q", r.Type)
	}
	return nil
}

// Records lists the snapshot as records: meta, params, then each type in
// canonical order, which is also the order WriteJSONL uses.
func (s *Snapshot) Records() []Record {
	m := s.Meta
	out := []Record{
		{Type: "meta", Schema: m.Schema, AsOf: m.AsOf, PriorRuns: m.PriorRuns, PauseNewKeysSince: m.PauseNewKeysSince, EventsSeq: m.EventsSeq, EndorsementsSeq: m.EndorsementsSeq, LedgerSeq: m.LedgerSeq},
		{Type: "params", Version: s.Params.Version, Body: s.Params.Body()},
	}
	for _, list := range [][]Record{s.Accounts, s.Posts, s.Endorsements, s.Proofs, s.Breakers, s.Transfers, s.Claims, s.Priors, s.Penalties, s.Sponsorships, s.Acts, s.Spends} {
		out = append(out, sortRecords(list)...)
	}
	return out
}

// recordKey orders records of one type canonically.
func recordKey(r Record) string {
	b, _ := json.Marshal(r)
	return string(b)
}

// sortRecords returns a copy of list in canonical order (by recordKey).
func sortRecords(list []Record) []Record {
	type keyed struct {
		key string
		rec Record
	}
	tmp := make([]keyed, len(list))
	for i, r := range list {
		tmp[i] = keyed{recordKey(r), r}
	}
	sort.SliceStable(tmp, func(i, j int) bool { return tmp[i].key < tmp[j].key })
	out := make([]Record, len(list))
	for i, k := range tmp {
		out[i] = k.rec
	}
	return out
}

// ReadJSONL parses a snapshot, strictly: unknown fields and types are errors.
func ReadJSONL(r io.Reader) (Snapshot, error) {
	var s Snapshot
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	line := 0
	for sc.Scan() {
		line++
		text := bytes.TrimSpace(sc.Bytes())
		if len(text) == 0 {
			continue
		}
		var rec Record
		dec := json.NewDecoder(bytes.NewReader(text))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&rec); err != nil {
			return Snapshot{}, fmt.Errorf("trust input line %d: %w", line, err)
		}
		if err := s.Add(rec); err != nil {
			return Snapshot{}, fmt.Errorf("trust input line %d: %w", line, err)
		}
	}
	if err := sc.Err(); err != nil {
		return Snapshot{}, err
	}
	return s, nil
}

// WriteJSONL writes the snapshot, one canonical JSON record per line.
func (s *Snapshot) WriteJSONL(w io.Writer) error {
	bw := bufio.NewWriter(w)
	for _, r := range s.Records() {
		raw, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if _, err = bw.Write(canonicalize(raw)); err != nil {
			return err
		}
		if err = bw.WriteByte('\n'); err != nil {
			return err
		}
	}
	return bw.Flush()
}
