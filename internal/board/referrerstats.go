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
//	referrer:YYYY-MM-DD:refvisitor:VENUE  distinct visiting networks among VENUE's visits (C729)
//	referrer:YYYY-MM-DD:refvisitor-other  the same for venues past the day's named ones
//	referrer:YYYY-MM-DD:fetch:FAMILY:PATH  landing-page GET/HEADs by a user-initiated assistant fetcher (C451)
//	referrer:YYYY-MM-DD:fetch-other:FAMILY that family's landing fetches past its named paths
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
//
// A refvisitor row is a number only: the caller counts a network once per
// venue per day with a salted hash it holds in memory for that day and never
// stores (no IP, prefix or hash reaches the database). A venue's refvisitor
// row is kept only while its ref row is: when a venue folds into ref-other
// its visitors fold into refvisitor-other, an over-count there since one
// network may have visited several folded venues.
//
// The fetch rows answer "which pages do assistants fetch for their users?":
// FAMILY is one of AssistantFetchFamilies, the fetchers an assistant sends
// when its user asks it to read a page (never a crawler), and PATH is the
// landing page folded by web.LandingKey (no query, every ID a placeholder,
// ValidAssistantPath). At most AssistantPathsPerDay paths are kept per family
// per day, the rest folded into fetch-other:FAMILY. Unlike the referrer
// rows these name only public pages, so ReadAssistantFetches publishes them
// (/api/stats/daily assistant_fetches and /stats), each path from
// ClientCountMinimum; swarmmemo stats assistants shows them all.

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

// AssistantFetchFamilies are the UserAgentFamilies an assistant sends to
// fetch a page its user asked about, as opposed to a crawler indexing ahead.
var AssistantFetchFamilies = []string{"ChatGPT-User", "Claude-User", "Perplexity-User", "MistralAI-User", "meta-externalfetcher"}

// IsAssistantFetchFamily reports whether family is one of AssistantFetchFamilies.
func IsAssistantFetchFamily(family string) bool {
	for _, f := range AssistantFetchFamilies {
		if f == family {
			return true
		}
	}
	return false
}

// AssistantPathsPerDay bounds the named paths stored per assistant family per day.
const AssistantPathsPerDay = 50

// AssistantPathBytes is the longest folded path stored.
const AssistantPathBytes = 128

// assistantPath is a folded landing path: "/" or slash-separated segments of
// [a-z0-9._-] (starting with a letter or digit), {id} or {room}.
var assistantPath = regexp.MustCompile(`^/(?:(?:[a-z0-9][a-z0-9._-]*|\{id\}|\{room\})(?:/(?:[a-z0-9][a-z0-9._-]*|\{id\}|\{room\}))*)?$`)

// ValidAssistantPath reports whether path may be stored as a folded landing
// path: never a query, an upper-case ID or anything but the page's shape.
func ValidAssistantPath(path string) bool {
	return len(path) <= AssistantPathBytes && assistantPath.MatchString(path)
}

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
	case key == "other", key == "ref-other", key == "asset-other", key == "refvisitor-other":
		return true
	case strings.HasPrefix(key, "refvisitor:"):
		return ValidReferrerRef(key[len("refvisitor:"):])
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
	case strings.HasPrefix(key, "fetch:"):
		family, path, ok := strings.Cut(key[len("fetch:"):], ":")
		return ok && IsAssistantFetchFamily(family) && ValidAssistantPath(path)
	case strings.HasPrefix(key, "fetch-other:"):
		return IsAssistantFetchFamily(key[len("fetch-other:"):])
	}
	return false
}

// AddReferrerCounts adds counts for one UTC day in one transaction, then keeps
// only the ReferrerHostsPerDay largest domains and ReferrerRefsPerDay largest
// venues of that day, folding the rest into other and ref-other, and the
// AssistantPathsPerDay largest paths of each assistant family it touched,
// folding the rest into fetch-other:FAMILY. Keys are "host:DOMAIN",
// "asset:DOMAIN", "agent:FAMILY", "ref:VENUE", "fetch:FAMILY:PATH",
// "other", "asset-other", "ref-other" or "fetch-other:FAMILY". It runs in
// its own transaction on the store's one connection, from the background
// flush only: nothing else may hold a transaction around it.
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
	touched := map[string]bool{}
	for key, n := range counts {
		if n == 0 {
			continue
		}
		if err = addCounter(ctx, tx, prefix+key, n); err != nil {
			return err
		}
		if rest, ok := strings.CutPrefix(key, "fetch:"); ok {
			family, _, _ := strings.Cut(rest, ":")
			touched[family] = true
		}
	}
	for _, family := range AssistantFetchFamilies {
		if !touched[family] {
			continue
		}
		if err = foldReferrers(ctx, tx, prefix, "fetch:"+family, "fetch-other:"+family, AssistantPathsPerDay); err != nil {
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
	if err = foldRefVisitors(ctx, tx, prefix); err != nil {
		return err
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

// foldRefVisitors folds every refvisitor:VENUE counter of the day at prefix
// whose ref:VENUE counter is gone (folded into ref-other) into
// refvisitor-other, so the kept visitor rows never outnumber the kept venues.
func foldRefVisitors(ctx context.Context, tx *sql.Tx, prefix string) error {
	const orphan = `scope>? AND scope<? AND NOT EXISTS (SELECT 1 FROM counters r WHERE r.scope=?||substr(counters.scope,?))`
	args := []any{prefix + "refvisitor:", prefix + "refvisitor;", prefix + "ref:", len(prefix+"refvisitor:") + 1}
	var folded int64
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(sum(value),0) FROM counters WHERE `+orphan, args...).Scan(&folded); err != nil || folded == 0 {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM counters WHERE `+orphan, args...); err != nil {
		return err
	}
	return addCounter(ctx, tx, prefix+"refvisitor-other", folded)
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
	// RefVisitors is the distinct visiting networks per venue in Refs (by
	// name; a venue with none is absent), RefVisitorOther those of RefOther.
	RefVisitors     map[string]int64
	RefVisitorOther int64
	// Assistants are the landing pages each assistant fetch family read
	// that day, in AssistantFetchFamilies order, families with none left out.
	Assistants []AssistantFetches
}

// AssistantFetches is one assistant fetch family's landing pages for one
// day: folded paths largest first, and the fetches past the named paths.
type AssistantFetches struct {
	Family string
	Paths  []ReferrerCount
	Other  int64
}

// Total is every landing-page fetch the family made that day.
func (a AssistantFetches) Total() int64 {
	n := a.Other
	for _, p := range a.Paths {
		n += p.Count
	}
	return n
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
	fetches := make([]map[string]*AssistantFetches, days)
	fetch := func(i int, family string) *AssistantFetches {
		if fetches[i] == nil {
			fetches[i] = map[string]*AssistantFetches{}
		}
		a := fetches[i][family]
		if a == nil {
			a = &AssistantFetches{Family: family}
			fetches[i][family] = a
		}
		return a
	}
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
		case strings.HasPrefix(key, "fetch:"):
			family, path, _ := strings.Cut(key[len("fetch:"):], ":")
			a := fetch(i, family)
			a.Paths = append(a.Paths, ReferrerCount{path, value})
		case strings.HasPrefix(key, "fetch-other:"):
			fetch(i, key[len("fetch-other:"):]).Other += value
		case key == "other":
			out[i].Other += value
		case key == "asset-other":
			out[i].AssetOther += value
		case key == "ref-other":
			out[i].RefOther += value
		case key == "refvisitor-other":
			out[i].RefVisitorOther += value
		case strings.HasPrefix(key, "refvisitor:"):
			if out[i].RefVisitors == nil {
				out[i].RefVisitors = map[string]int64{}
			}
			out[i].RefVisitors[key[len("refvisitor:"):]] = value
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
		for _, family := range AssistantFetchFamilies {
			if a := fetches[i][family]; a != nil {
				sortReferrerCounts(a.Paths)
				out[i].Assistants = append(out[i].Assistants, *a)
			}
		}
		for _, list := range [][]ReferrerCount{out[i].Hosts, out[i].Assets, out[i].Agents, out[i].Refs} {
			sortReferrerCounts(list)
		}
	}
	return out, nil
}

func sortReferrerCounts(list []ReferrerCount) {
	sort.Slice(list, func(a, b int) bool {
		return list[a].Count > list[b].Count || list[a].Count == list[b].Count && list[a].Name < list[b].Name
	})
}

// AssistantDay is one UTC day of published assistant fetches.
type AssistantDay struct {
	Day      string
	Families []AssistantFetches
}

// ReadAssistantFetches is the public part of ReadReferrerStats: for each
// day, newest first, each assistant fetch family's folded landing paths
// with a count of at least ClientCountMinimum, the smaller ones added to
// Other. It never reads or returns a referrer domain, venue or crawler.
func (s *Store) ReadAssistantFetches(ctx context.Context, end time.Time, days int) ([]AssistantDay, error) {
	stats, err := s.ReadReferrerStats(ctx, end, days)
	if err != nil {
		return nil, err
	}
	out := make([]AssistantDay, len(stats))
	for i, d := range stats {
		out[i].Day = d.Day
		for _, a := range d.Assistants {
			pub := AssistantFetches{Family: a.Family, Other: a.Other}
			for _, p := range a.Paths {
				if p.Count >= ClientCountMinimum {
					pub.Paths = append(pub.Paths, p)
				} else {
					pub.Other += p.Count
				}
			}
			out[i].Families = append(out[i].Families, pub)
		}
	}
	return out, nil
}
