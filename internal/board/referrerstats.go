package board

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Referrer counters tell the operator where visitors come from without access
// logs. Like the reader counters they reuse counters(scope,value), so they need
// no schema change, and each row is one (UTC day, name, integer):
//
//	referrer:YYYY-MM-DD:host:DOMAIN   landing-page visits (GET/HEAD) whose Referer named DOMAIN
//	referrer:YYYY-MM-DD:other         unnamed, invalid, IP-literal or overflow referrers
//	referrer:YYYY-MM-DD:asset:DOMAIN  every other Referer-bearing request naming DOMAIN (embeds, hotlinks, API calls)
//	referrer:YYYY-MM-DD:asset-other   asset referrers past the day's named ones
//	referrer:YYYY-MM-DD:agent:FAMILY  requests from a known crawler or agent family
//	referrer:YYYY-MM-DD:ref:VENUE     landing-page visits that carried ?ref=VENUE
//	referrer:YYYY-MM-DD:ref-other     ?ref= venues past the day's named ones
//
// DOMAIN is a lowercase ASCII domain reduced to its registrable part by the
// caller (never a path, query, port, user name or full URL), and FAMILY is one
// of UserAgentFamilies (never the User-Agent itself); AddReferrerCounts refuses
// anything else. VENUE is the short label a link to SwarmMemo chose for
// itself (ValidReferrerRef), never anything about the visitor. At most
// ReferrerHostsPerDay "host:" domains, ReferrerHostsPerDay "asset:" domains
// and ReferrerRefsPerDay venues are kept per day: after each write the
// smallest are folded into other, asset-other and ref-other. These counts are
// for the operator only (swarmmemo stats referrers) and are never served over
// HTTP.

// ReferrerHostsPerDay bounds the named domains stored for one day.
const ReferrerHostsPerDay = 200

// ReferrerRefsPerDay bounds the named ?ref= venues stored for one day.
const ReferrerRefsPerDay = 100

// ReferrerRefBytes is the longest ?ref= venue counted.
const ReferrerRefBytes = 32

// UserAgentFamilies are the crawler and agent families counted by name.
var UserAgentFamilies = []string{
	"Googlebot", "GoogleOther", "Google-InspectionTool", "Bingbot", "DuckDuckBot", "YandexBot", "Baiduspider", "Applebot",
	"GPTBot", "OAI-SearchBot", "ChatGPT-User", "ClaudeBot", "Claude-User", "Claude-SearchBot", "anthropic-ai",
	"PerplexityBot", "Perplexity-User", "Amazonbot", "Bytespider", "CCBot", "meta-externalagent", "meta-externalfetcher",
	"FacebookBot", "cohere-ai", "MistralAI-User", "DuckAssistBot", "Diffbot", "PetalBot",
	"Twitterbot", "LinkedInBot", "Slackbot", "Discordbot", "TelegramBot",
	"curl", "Wget", "python-requests", "python-httpx", "Python-urllib", "aiohttp", "Go-http-client", "node", "axios",
	"other-bot",
}

const referrerScopePrefix = "referrer:"

// referrerHost is a stored domain: two or more LDH labels, ASCII lowercase.
var referrerHost = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+(?:[a-z]{2,63}|xn--[a-z0-9-]{1,59})$`)

// ValidReferrerHost reports whether host may be stored as a referrer domain.
func ValidReferrerHost(host string) bool { return len(host) <= 253 && referrerHost.MatchString(host) }

// referrerRef is a stored venue: lowercase letters, digits and inner hyphens.
var referrerRef = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)

// ValidReferrerRef reports whether ref may be stored as a ?ref= venue: 1 to
// ReferrerRefBytes of [a-z0-9-], starting and ending with a letter or digit.
func ValidReferrerRef(ref string) bool {
	return len(ref) <= ReferrerRefBytes && referrerRef.MatchString(ref)
}

func validReferrerKey(key string) bool {
	switch {
	case key == "other", key == "ref-other", key == "asset-other":
		return true
	case strings.HasPrefix(key, "host:"):
		return ValidReferrerHost(key[len("host:"):])
	case strings.HasPrefix(key, "asset:"):
		return ValidReferrerHost(key[len("asset:"):])
	case strings.HasPrefix(key, "ref:"):
		return ValidReferrerRef(key[len("ref:"):])
	case strings.HasPrefix(key, "agent:"):
		for _, family := range UserAgentFamilies {
			if key[len("agent:"):] == family {
				return true
			}
		}
	}
	return false
}

// AddReferrerCounts adds counts for one UTC day in one transaction, then keeps
// only the ReferrerHostsPerDay largest domains and ReferrerRefsPerDay largest
// venues of that day, folding the rest into other and ref-other. Keys are
// "host:DOMAIN", "agent:FAMILY", "ref:VENUE", "other" or "ref-other".
func (s *Store) AddReferrerCounts(ctx context.Context, day string, counts map[string]int64) error {
	if _, err := time.Parse("2006-01-02", day); err != nil || !readerDay.MatchString(day) {
		return fmt.Errorf("referrer counts: invalid day")
	}
	for key, n := range counts {
		if !validReferrerKey(key) || n < 0 {
			return fmt.Errorf("referrer counts: invalid key")
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	prefix := referrerScopePrefix + day + ":"
	for key, n := range counts {
		if n == 0 {
			continue
		}
		if err = addCounter(ctx, tx, prefix+key, n); err != nil {
			return err
		}
	}
	// Fold every "host:" domain past the largest ReferrerHostsPerDay into
	// other, every "asset:" domain past the largest ReferrerHostsPerDay into
	// asset-other, and every venue past the largest ReferrerRefsPerDay into
	// ref-other. A name folded away starts again from zero if it comes back;
	// the day's total is unchanged.
	for _, fold := range []struct {
		kind, into string
		keep       int
	}{{"host", "other", ReferrerHostsPerDay}, {"asset", "asset-other", ReferrerHostsPerDay}, {"ref", "ref-other", ReferrerRefsPerDay}} {
		if err = foldReferrers(ctx, tx, prefix, fold.kind, fold.into, fold.keep); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// foldReferrers keeps the keep largest kind:NAME counters of the day at
// prefix and adds the rest to the into counter.
func foldReferrers(ctx context.Context, tx *sql.Tx, prefix, kind, into string, keep int) error {
	var folded int64
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(sum(value),0) FROM (SELECT value FROM counters WHERE scope>? AND scope<?
 ORDER BY value DESC, scope LIMIT -1 OFFSET ?)`, prefix+kind+":", prefix+kind+";", keep).Scan(&folded); err != nil || folded == 0 {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM counters WHERE scope IN (SELECT scope FROM counters WHERE scope>? AND scope<?
 ORDER BY value DESC, scope LIMIT -1 OFFSET ?)`, prefix+kind+":", prefix+kind+";", keep); err != nil {
		return err
	}
	return addCounter(ctx, tx, prefix+into, folded)
}

// ReferrerCount is one name and its count.
type ReferrerCount struct {
	Name  string
	Count int64
}

// ReferrerDay is one UTC day of referrer, asset-referrer, agent and ?ref=
// venue counts, largest first. Hosts are landing-page visits arriving from a
// domain; Assets are every other Referer-bearing request naming a domain
// (embeds, hotlinks, API calls) and are not visits.
type ReferrerDay struct {
	Day        string
	Hosts      []ReferrerCount
	Other      int64
	Assets     []ReferrerCount
	AssetOther int64
	Agents     []ReferrerCount
	Refs       []ReferrerCount
	RefOther   int64
}

// ReadReferrerStats returns `days` consecutive UTC days ending with the day
// containing end, newest first.
func (s *Store) ReadReferrerStats(ctx context.Context, end time.Time, days int) ([]ReferrerDay, error) {
	if days < 1 || days > 366 {
		return nil, fmt.Errorf("referrer stats: days out of range")
	}
	end = end.UTC().Truncate(24 * time.Hour)
	out := make([]ReferrerDay, days)
	index := map[string]int{}
	for i := range out {
		out[i].Day = end.AddDate(0, 0, -i).Format("2006-01-02")
		index[out[i].Day] = i
	}
	rows, err := s.db.QueryContext(ctx, "SELECT scope,value FROM counters WHERE scope>=? AND scope<?", referrerScopePrefix+out[days-1].Day, referrerScopePrefix+out[0].Day+";")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var scope string
		var value int64
		if err = rows.Scan(&scope, &value); err != nil {
			return nil, err
		}
		day, key, ok := strings.Cut(strings.TrimPrefix(scope, referrerScopePrefix), ":")
		i, known := index[day]
		if !ok || !known || !validReferrerKey(key) {
			continue
		}
		switch {
		case key == "other":
			out[i].Other += value
		case key == "asset-other":
			out[i].AssetOther += value
		case key == "ref-other":
			out[i].RefOther += value
		case strings.HasPrefix(key, "host:"):
			out[i].Hosts = append(out[i].Hosts, ReferrerCount{key[len("host:"):], value})
		case strings.HasPrefix(key, "asset:"):
			out[i].Assets = append(out[i].Assets, ReferrerCount{key[len("asset:"):], value})
		case strings.HasPrefix(key, "ref:"):
			out[i].Refs = append(out[i].Refs, ReferrerCount{key[len("ref:"):], value})
		default:
			out[i].Agents = append(out[i].Agents, ReferrerCount{key[len("agent:"):], value})
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		for _, list := range [][]ReferrerCount{out[i].Hosts, out[i].Assets, out[i].Agents, out[i].Refs} {
			sort.Slice(list, func(a, b int) bool {
				return list[a].Count > list[b].Count || list[a].Count == list[b].Count && list[a].Name < list[b].Name
			})
		}
	}
	return out, nil
}
