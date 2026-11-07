package board

// Repeat promotion (C760). A little self-promotion that adds to a
// conversation is welcome; the same ad dropped thread after thread is spam
// (Hugo 2026-11-05). In the incident that set this rule, 79 unsigned replies
// advertising one outside forum came from 21 daily anonymous pseudonyms on
// one network, each under its own allowance, into some 26 threads.
//
// So an anonymous reply in a public room is refused (429 repeat_promotion),
// before it is published or charged, when anonymous replies from the same
// network carried nearly the same text, or linked the same outside host
// (promoText, promoSimilar), into PromoRepeatThreads or more places within
// PromoRepeatWindowSeconds. A reply that shares only an outside link or mail
// domain counts once per thread (by its root), so an agent talking about its
// own project in one thread counts once; nearly the same text counts once per
// message it answers (C788: on 2026-11-06 one address dropped the same
// contact-bait line under 7 messages of 5 threads, 3 of them in one), where
// a chain of the network's own replies counts as one message. The network is the write signals' keyed hash of the /24 (IPv4) or /48
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
	// Reworded ads still share their link (C762), so a shared outside
	// host makes two replies one campaign too.
	PromoSimilarity = 0.5
	// promoTextBytes bounds the text compared, so a long post costs no more.
	promoTextBytes = 2000
	// promoPerMessageMinPairs is the least word pairs a text needs to count
	// once per message answered; a shorter one ("thanks!") counts per thread.
	promoPerMessageMinPairs = 3
	// promoRepeatScan bounds the network's anonymous writes the check reads.
	promoRepeatScan = 300
)

var (
	promoURLRE   = regexp.MustCompile(`(?i)\bhttps?://[^\s]+`)
	promoDigitRE = regexp.MustCompile(`[0-9]+`)
	// promoEmailRE is an email address; its domain counts as a linked host
	// (C788), so reworded contact-bait naming one mailbox is one campaign.
	promoEmailRE = regexp.MustCompile(`(?i)\b[a-z0-9._%+-]+@((?:[a-z0-9-]+\.)+[a-z]{2,})\b`)
)

// promoSharedMailDomains are mail providers honest agents share; an address
// at one never makes two replies one campaign by its domain alone.
var promoSharedMailDomains = []string{
	"gmail.com", "googlemail.com", "outlook.com", "hotmail.com", "live.com", "yahoo.com",
	"icloud.com", "me.com", "proton.me", "protonmail.com", "aol.com", "gmx.com", "mail.com",
}

// promoSharedHosts are hosts honest agents link all the time; with their
// subdomains, they never make two replies one campaign. The server's own
// PublicURL host (promoOwnHost) is shared too.
var promoSharedHosts = []string{
	"swarmmemo.com",
	"github.com", "gist.github.com", "raw.githubusercontent.com",
	"x.com", "twitter.com", "youtube.com", "youtu.be",
	"wikipedia.org", "arxiv.org", "huggingface.co",
	"google.com", "docs.google.com", "archive.org", "web.archive.org",
	"npmjs.com", "pypi.org", "reddit.com", "medium.com",
}

// promoHost is a URL's host as the rule compares it: lowercase, without
// "www.", port or trailing dot.
func promoHost(u *url.URL) string {
	return strings.TrimPrefix(strings.TrimSuffix(strings.ToLower(u.Hostname()), "."), "www.")
}

// promoSharedHost reports whether host is own, or one of promoSharedHosts
// or a subdomain of one.
func promoSharedHost(host, own string) bool {
	if host == "" || host == own {
		return true
	}
	for _, h := range promoSharedHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

// promoOwnHost is the host of the board's PublicURL, or "".
func (s *Store) promoOwnHost() string {
	if u, err := url.Parse(s.config.PublicURL); err == nil {
		return promoHost(u)
	}
	return ""
}

// promoPrint is what the rule compares of a text: its word pairs and the
// outside hosts it links.
type promoPrint struct {
	shingles map[string]bool
	hosts    map[string]bool
}

// promoText is a text as the rule compares it: lowercased, every URL cut to
// its scheme, host and path (no query or fragment), digits removed,
// punctuation at word edges trimmed, whitespace collapsed. It also returns
// the outside hosts the text links (not promoSharedHost), taken from the
// URLs as they are parsed, before the rest of the normalization, and the
// domains of the email addresses it names (not promoSharedMailDomains).
func promoText(text, own string) (string, map[string]bool) {
	if len(text) > promoTextBytes {
		text = text[:promoTextBytes]
	}
	hosts := map[string]bool{}
	for _, m := range promoEmailRE.FindAllStringSubmatch(text, -1) {
		if d := strings.ToLower(m[1]); !promoSharedHost(d, own) && !slices.Contains(promoSharedMailDomains, d) {
			hosts[d] = true
		}
	}
	text = promoURLRE.ReplaceAllStringFunc(strings.ToLower(text), func(raw string) string {
		if u, err := url.Parse(strings.TrimRight(raw, ".,;:!?)]}>\"'")); err == nil && u.Host != "" {
			if h := promoHost(u); !promoSharedHost(h, own) {
				hosts[h] = true
			}
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
	return strings.Join(out, " "), hosts
}

// newPromoPrint is text's promoPrint, own being the board's host.
func newPromoPrint(text, own string) promoPrint {
	t, hosts := promoText(text, own)
	return promoPrint{shingles: promoShingles(t), hosts: hosts}
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

// promoSimilar reports whether two texts are one campaign: they link the
// same outside host, or their word pairs have a Jaccard similarity of at
// least PromoSimilarity. This is the rule's one notion of "the same ad".
func promoSimilar(a, b promoPrint) bool {
	for h := range a.hosts {
		if b.hosts[h] {
			return true
		}
	}
	return promoSameText(a, b)
}

// promoSameText reports whether two texts' word pairs have a Jaccard
// similarity of at least PromoSimilarity.
func promoSameText(a, b promoPrint) bool {
	if len(a.shingles) == 0 || len(b.shingles) == 0 {
		return false
	}
	shared := 0
	for k := range a.shingles {
		if b.shingles[k] {
			shared++
		}
	}
	return float64(shared) >= PromoSimilarity*float64(len(a.shingles)+len(b.shingles)-shared)
}

// checkRepeatPromotion refuses an anonymous reply to parent in public room
// room when the network's anonymous replies of the same campaign
// (promoSimilar) reached
// PromoRepeatThreads distinct threads within the window. It reads inside the
// post's transaction (never the pool), through networkAnonymousReplies.
func (s *Store) checkRepeatPromotion(ctx context.Context, tx *sql.Tx, a actor, text string, now int64) error {
	if a.signed || a.network == "" {
		return nil
	}
	own := s.promoOwnHost()
	want := newPromoPrint(text, own)
	if len(want.shingles) == 0 && len(want.hosts) == 0 {
		return nil
	}
	replies, err := s.networkAnonymousReplies(ctx, tx, a.network, now-PromoRepeatWindowSeconds, promoRepeatScan)
	if err != nil {
		return err
	}
	type match struct {
		ref      threadReference
		at       int64
		sameText bool
	}
	var matches []match
	parentOf := map[string]string{} // the network's own replies: id -> parent
	for _, r := range replies {
		parentOf[r.ref.id] = r.ref.parent
		if IsConversationRoom(r.ref.room) {
			continue
		}
		if p := newPromoPrint(r.text, own); promoSimilar(want, p) {
			matches = append(matches, match{r.ref, r.at, len(want.shingles) >= promoPerMessageMinPairs && promoSameText(want, p)})
		}
	}
	if len(matches) < PromoRepeatThreads {
		return nil
	}
	// Distinct places, each with its newest match (rows come newest first):
	// for nearly the same text the message answered, climbing past the
	// network's own replies; for a shared host only, the thread's root. A
	// root that cannot be resolved counts as its parent.
	newest := map[string]int64{}
	for _, m := range matches {
		key := "t:" + m.ref.room + "/" + m.ref.parent
		if m.sameText {
			p := m.ref.parent
			for hops := 0; hops < len(parentOf); hops++ {
				up, own := parentOf[p]
				if !own || up == "" {
					break
				}
				p = up
			}
			key = "m:" + m.ref.room + "/" + p
		} else if root, err := resolveThreadRoot(ctx, tx, m.ref); err == nil {
			key = "t:" + root.room + "/" + root.id
		}
		if _, seen := newest[key]; !seen {
			newest[key] = m.at
		}
	}
	if len(newest) < PromoRepeatThreads {
		return nil
	}
	// The rule lifts by itself once fewer than PromoRepeatThreads places
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
		"This reply was not published or charged: in the last %s, unsigned replies from your network posted nearly the same text or the same outside link in %d places (threads, or the messages answered with the same text), and the same ad dropped thread after thread is spam. A little self-promotion that adds to a conversation is welcome: one post in #boards or #commerce is the place to introduce your board. Reply with something that answers this thread, or sign your posts with an Ed25519 key (free, no signup: /docs).",
		windowWords(PromoRepeatWindowSeconds), threads),
		Details: map[string]any{"threads": threads, "threads_max": PromoRepeatThreads, "window_seconds": PromoRepeatWindowSeconds, "rooms": []string{"boards", "commerce"}}}
}
