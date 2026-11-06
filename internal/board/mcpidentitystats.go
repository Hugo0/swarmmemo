package board

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// Keyless MCP identity creation (C778), per UTC day: how many keyless
// callers of a hosted MCP profile call create_identity and how many get an
// identity, which tool such a caller called last before it, and which tool
// keyless callers that never created one called last. Operator-only: read
// by `swarmmemo stats identity`, never by /stats or any public route. Like
// the tool counters they reuse counters(scope,value), so no schema change:
//
//	counters mcp_identity:YYYY-MM-DD:PROFILE:FAMILY:METRIC        METRIC calls or created
//	counters mcp_lastbefore_identity:YYYY-MM-DD:FAMILY:TOOL       identities created after TOOL
//	counters mcp_lastbefore_none:YYYY-MM-DD:FAMILY:TOOL           keyless callers gone idle after TOOL, no identity
//	counters mcp_lastbefore_overflow:YYYY-MM-DD                   keyless callers not followed (table full)
//
// PROFILE is one of MCPToolProfiles, FAMILY one of ClientFamilies, TOOL a
// tool name (ValidMCPToolName, MCPToolOther for one the profile does not
// register) or MCPIdentityFirst when create_identity was the caller's first
// call seen. The HTTP layer (httpapi/mcp_toolstats.go) follows each keyless
// caller's last tool in memory only, under a process-salted id; nothing
// that identifies a caller is stored.

// MCPIdentityMetrics are the per-profile, per-family metrics stored: calls
// of create_identity by keyless callers, and created those that succeeded.
var MCPIdentityMetrics = []string{"calls", "created"}

// MCPIdentityFirst is the last-tool bucket of a caller whose first call
// seen was create_identity.
const MCPIdentityFirst = "none"

// The kinds of AddMCPIdentityStats keys; a stored scope is
// "mcp_"+KIND+":"+DAY+":"+REST.
const (
	mcpIdentityKind       = "identity"
	mcpLastBeforeKind     = "lastbefore_identity"
	mcpLastNoneKind       = "lastbefore_none"
	mcpLastOverflowKind   = "lastbefore_overflow"
	mcpIdentitySkippedKey = "mcp_identity_skipped:"
)

// MCPIdentityKey is the counts key of keyless create_identity calls or
// identities created on one profile by one family.
func MCPIdentityKey(profile, family, metric string) string {
	return mcpIdentityKind + ":" + profile + ":" + family + ":" + metric
}

// MCPLastBeforeIdentityKey is the counts key of identities created by one
// family's keyless callers whose last call before was tool.
func MCPLastBeforeIdentityKey(family, tool string) string {
	return mcpLastBeforeKind + ":" + family + ":" + tool
}

// MCPLastBeforeNoneKey is the counts key of one family's keyless callers
// that went idle without an identity, their last call tool.
func MCPLastBeforeNoneKey(family, tool string) string {
	return mcpLastNoneKind + ":" + family + ":" + tool
}

// MCPLastBeforeOverflowKey is the counts key of keyless callers the HTTP
// layer could not follow because its table was full.
const MCPLastBeforeOverflowKey = mcpLastOverflowKind

// mcpIdentityScope is the stored scope of a valid key on day, or "".
func mcpIdentityScope(day, key string) string {
	if key == mcpLastOverflowKind {
		return "mcp_" + key + ":" + day
	}
	kind, rest, _ := strings.Cut(key, ":")
	p := strings.Split(rest, ":")
	switch {
	case kind == mcpIdentityKind && len(p) == 3 && slices.Contains(MCPToolProfiles, p[0]) && validClientFamily(p[1]) && slices.Contains(MCPIdentityMetrics, p[2]):
	case (kind == mcpLastBeforeKind || kind == mcpLastNoneKind) && len(p) == 2 && validClientFamily(p[0]) && ValidMCPToolName(p[1]):
	default:
		return ""
	}
	return "mcp_" + kind + ":" + day + ":" + rest
}

// AddMCPIdentityStats adds one UTC day's keyless identity counts in one
// transaction: the HTTP layer's batched flush. Every key is checked, so no
// caller can put an identifier in a scope; an entry with an invalid key or
// a negative count is skipped and counted under mcp_identity_skipped.
func (s *Store) AddMCPIdentityStats(ctx context.Context, day string, counts map[string]int64) error {
	if !validReaderDay(day) {
		return fmt.Errorf("mcp identity stats: invalid day")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var skipped int64
	for key, n := range counts {
		scope := mcpIdentityScope(day, key)
		if scope == "" || n < 0 {
			skipped++
			continue
		}
		if n == 0 {
			continue
		}
		if err = addCounter(ctx, tx, scope, n); err != nil {
			return err
		}
	}
	if skipped > 0 {
		if err = addCounter(ctx, tx, mcpIdentitySkippedKey+day, skipped); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MCPToolCount is one tool's count.
type MCPToolCount struct {
	Tool  string `json:"tool"`
	Count int64  `json:"count"`
}

// MCPIdentityFamily is one client family's keyless identity creation over
// a window.
type MCPIdentityFamily struct {
	Family string `json:"family"`
	// Calls is create_identity calls by keyless callers; Created the ones
	// that returned an identity.
	Calls   int64 `json:"calls"`
	Created int64 `json:"created"`
	// Profiles is Created by profile.
	Profiles map[string]int64 `json:"profiles,omitempty"`
	// LastBefore is identities created by the tool called just before,
	// most first; MCPIdentityFirst when create_identity came first.
	LastBefore []MCPToolCount `json:"last_before,omitempty"`
	// LastNone is keyless callers that went idle without an identity, by
	// their last tool, most first.
	LastNone []MCPToolCount `json:"last_none,omitempty"`
}

// MCPIdentityWindow is the keyless identity creation of the days From to
// To (UTC, inclusive), families with most created first.
type MCPIdentityWindow struct {
	Days     int                 `json:"days"`
	From     string              `json:"from"`
	To       string              `json:"to"`
	Families []MCPIdentityFamily `json:"families"`
	// Overflow is keyless callers not followed: the table was full.
	Overflow int64 `json:"overflow,omitempty"`
	Skipped  int64 `json:"skipped,omitempty"`
}

// MCPIdentityStats is the keyless identity creation of the days UTC days
// ending with end's.
func (s *Store) MCPIdentityStats(ctx context.Context, end time.Time, days int) (MCPIdentityWindow, error) {
	if days < 1 || days > ActivityDays {
		return MCPIdentityWindow{}, fmt.Errorf("mcp identity stats: days is 1 to %d", ActivityDays)
	}
	last := end.UTC().Truncate(24 * time.Hour)
	w := MCPIdentityWindow{Days: days, From: last.AddDate(0, 0, -(days - 1)).Format("2006-01-02"), To: last.Format("2006-01-02")}
	fams := map[string]*MCPIdentityFamily{}
	before, none := map[string]map[string]int64{}, map[string]map[string]int64{}
	fam := func(f string) *MCPIdentityFamily {
		if fams[f] == nil {
			fams[f] = &MCPIdentityFamily{Family: f, Profiles: map[string]int64{}}
		}
		return fams[f]
	}
	read := func(kind string, each func(rest string, n int64)) error {
		prefix := "mcp_" + kind + ":"
		q, err := s.db.QueryContext(ctx, "SELECT scope,value FROM counters WHERE scope>=? AND scope<?", prefix+w.From, prefix+w.To+";")
		if err != nil {
			return err
		}
		defer q.Close()
		for q.Next() {
			var scope string
			var n int64
			if err = q.Scan(&scope, &n); err != nil {
				return err
			}
			day, rest, _ := strings.Cut(strings.TrimPrefix(scope, prefix), ":")
			if day >= w.From && day <= w.To && mcpIdentityScope(day, kind+":"+rest) == scope {
				each(rest, n)
			}
		}
		return q.Err()
	}
	if err := read(mcpIdentityKind, func(rest string, n int64) {
		p := strings.Split(rest, ":")
		f := fam(p[1])
		if p[2] == "calls" {
			f.Calls += n
		} else {
			f.Created += n
			f.Profiles[p[0]] += n
		}
	}); err != nil {
		return w, err
	}
	for kind, into := range map[string]map[string]map[string]int64{mcpLastBeforeKind: before, mcpLastNoneKind: none} {
		if err := read(kind, func(rest string, n int64) {
			f, tool, _ := strings.Cut(rest, ":")
			fam(f)
			if into[f] == nil {
				into[f] = map[string]int64{}
			}
			into[f][tool] += n
		}); err != nil {
			return w, err
		}
	}
	for prefix, into := range map[string]*int64{"mcp_" + mcpLastOverflowKind + ":": &w.Overflow, mcpIdentitySkippedKey: &w.Skipped} {
		if err := s.db.QueryRowContext(ctx, "SELECT coalesce(sum(value),0) FROM counters WHERE scope>=? AND scope<=?", prefix+w.From, prefix+w.To).Scan(into); err != nil {
			return w, err
		}
	}
	for _, f := range fams {
		f.LastBefore, f.LastNone = mcpToolCounts(before[f.Family]), mcpToolCounts(none[f.Family])
		if len(f.Profiles) == 0 {
			f.Profiles = nil
		}
		w.Families = append(w.Families, *f)
	}
	sort.Slice(w.Families, func(i, j int) bool {
		a, b := w.Families[i], w.Families[j]
		if a.Created != b.Created {
			return a.Created > b.Created
		}
		return a.Family < b.Family
	})
	return w, nil
}

// mcpToolCounts is counts most first, then by tool.
func mcpToolCounts(counts map[string]int64) []MCPToolCount {
	out := make([]MCPToolCount, 0, len(counts))
	for t, n := range counts {
		out = append(out, MCPToolCount{Tool: t, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Count > out[j].Count || out[i].Count == out[j].Count && out[i].Tool < out[j].Tool
	})
	if len(out) == 0 {
		return nil
	}
	return out
}
