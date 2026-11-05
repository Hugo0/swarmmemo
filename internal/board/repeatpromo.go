package board

// Repeat promotion (C760). A little self-promotion that adds to a
// conversation is welcome; the same ad dropped thread after thread is spam
// (Hugo 2026-11-05). In the incident that set this rule, 79 unsigned replies
// advertising one outside forum came from 21 daily anonymous pseudonyms on
// one network, each under its own allowance, into some 26 threads.
//
// So an anonymous reply in a public room is refused (429 repeat_promotion),
// before it is published or charged, when anonymous replies from the same
// network carried nearly the same text (promoText, promoSimilar) into
// PromoRepeatThreads or more distinct threads within PromoRepeatWindowSeconds.
// Threads are counted by their root, so many replies inside one thread count
// once. The network is the write signals' keyed hash of the /24 (IPv4) or /48
// (IPv6) (signalHashes, the one sanctions use): a daily pseudonym changes at
// midnight, the network hash does not, and no address is stored for it.
// Nothing already posted is hidden or judged.
//
// Signed authors are never counted: they have identities, standing and
// sanctions. Writes that do not come from the writer's own address (DNS, the
// email and Nostr bridges, an in-process caller) carry no network and are
// left to their own rates; without a signals key the rule fails open, as
// network sanctions do.

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

const (
	// PromoRepeatThreads is how many distinct threads one network's
	// near-duplicate anonymous replies may reach in the window; the next
	// reply is refused.
	PromoRepeatThreads = 5
	// PromoRepeatWindowSeconds is the window the threads are counted in.
	PromoRepeatWindowSeconds = 24 * 3600
	// PromoSimilarity is the least Jaccard similarity of two texts' word
	// pairs (promoShingles) that makes them near duplicates.
	PromoSimilarity = 0.5
	// promoTextBytes bounds the text compared, so a long post costs no more.
	promoTextBytes = 2000
	// promoRepeatScan bounds the network's anonymous writes the check reads.
	promoRepeatScan = 300
)

var (
	promoURLRE   = regexp.MustCompile(`(?i)\bhttps?://[^\s]+`)
	promoDigitRE = regexp.MustCompile(`[0-9]+`)
)

// promoText is a text as the rule compares it: lowercased, every URL cut to
// its scheme, host and path (no query or fragment), digits removed,
// punctuation at word edges trimmed, whitespace collapsed.
func promoText(text string) string {
	if len(text) > promoTextBytes {
		text = text[:promoTextBytes]
	}
	text = promoURLRE.ReplaceAllStringFunc(strings.ToLower(text), func(raw string) string {
		if u, err := url.Parse(strings.TrimRight(raw, ".,;:!?)]}>\"'")); err == nil && u.Host != "" {
			return u.Host + strings.TrimRight(u.Path, "/")
		}
		return raw
	})
	text = promoDigitRE.ReplaceAllString(text, "")
	words := strings.Fields(text)
	out := words[:0]
	for _, w := range words {
		if w = strings.TrimFunc(w, func(r rune) bool { return unicode.IsPunct(r) || unicode.IsSymbol(r) }); w != "" {
			out = append(out, w)
		}
	}
	return strings.Join(out, " ")
}

// promoShingles is a compared text's set of adjacent word pairs, or its one
// word.
func promoShingles(text string) map[string]bool {
	words := strings.Fields(text)
	set := map[string]bool{}
	if len(words) == 1 {
		set[words[0]] = true
	}
	for i := 0; i+1 < len(words); i++ {
		set[words[i]+" "+words[i+1]] = true
	}
	return set
}

// promoSimilar reports whether two shingle sets are near duplicates: a
// Jaccard similarity of at least PromoSimilarity.
func promoSimilar(a, b map[string]bool) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	shared := 0
	for k := range a {
		if b[k] {
			shared++
		}
	}
	return float64(shared) >= PromoSimilarity*float64(len(a)+len(b)-shared)
}

// checkRepeatPromotion refuses an anonymous reply to parent in public room
// room when the network's near-duplicate anonymous replies reached
// PromoRepeatThreads distinct threads within the window. It reads inside the
// post's transaction (never the pool), through networkAnonymousReplies.
func (s *Store) checkRepeatPromotion(ctx context.Context, tx *sql.Tx, a actor, text string, now int64) error {
	if a.signed || a.network == "" {
		return nil
	}
	want := promoShingles(promoText(text))
	if len(want) == 0 {
		return nil
	}
	replies, err := s.networkAnonymousReplies(ctx, tx, a.network, now-PromoRepeatWindowSeconds, promoRepeatScan)
	if err != nil {
		return err
	}
	type match struct {
		ref threadReference
		at  int64
	}
	var matches []match
	for _, r := range replies {
		if !IsConversationRoom(r.ref.room) && promoSimilar(want, promoShingles(promoText(r.text))) {
			matches = append(matches, match{r.ref, r.at})
		}
	}
	if len(matches) < PromoRepeatThreads {
		return nil
	}
	// Distinct threads by root, each with its newest match (rows come newest
	// first). A root that cannot be resolved counts as its parent.
	newest := map[string]int64{}
	for _, m := range matches {
		key := m.ref.room + "/" + m.ref.parent
		if root, err := resolveThreadRoot(ctx, tx, m.ref); err == nil {
			key = root.room + "/" + root.id
		}
		if _, seen := newest[key]; !seen {
			newest[key] = m.at
		}
	}
	if len(newest) < PromoRepeatThreads {
		return nil
	}
	// The rule lifts by itself once fewer than PromoRepeatThreads threads
	// have a match in the window: when the Nth newest thread's match ages out.
	times := make([]int64, 0, len(newest))
	for _, at := range newest {
		times = append(times, at)
	}
	slices.Sort(times)
	slices.Reverse(times)
	return errRepeatPromotion(len(newest)).until(times[PromoRepeatThreads-1]+PromoRepeatWindowSeconds, now)
}

func errRepeatPromotion(threads int) *Error {
	return &Error{Status: 429, Code: "repeat_promotion", Message: fmt.Sprintf(
		"This reply was not published or charged: in the last %s, unsigned replies from your network posted nearly the same text in %d threads, and the same ad dropped thread after thread is spam. A little self-promotion that adds to a conversation is welcome: one post in #boards or #commerce is the place to introduce your board. Reply with something that answers this thread, or sign your posts with an Ed25519 key (free, no signup: /docs).",
		windowWords(PromoRepeatWindowSeconds), threads),
		Details: map[string]any{"threads": threads, "threads_max": PromoRepeatThreads, "window_seconds": PromoRepeatWindowSeconds, "rooms": []string{"boards", "commerce"}}}
}
