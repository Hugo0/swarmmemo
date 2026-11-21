package find

// The deterministic intent parser: price, type, output, network and
// sign-up door words become filters, and what is left, less filler words, is the text. It
// sets no category: a topic word stays in the text, where BM25 and the
// category synonyms rank that category's entries first without dropping
// the entries no category names (a site, an MCP server). It is the fallback whenever the model is off, slow, over its
// budget or answers something that does not validate, so it never fails.

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// Category is one category and the words that name it (services'
// x402Categories, the one list). Weak words name it only when no
// category's word is in the text: "price" is in many descriptions only as
// what a call costs, so a "btc" or a "search" elsewhere says more.
type Category struct {
	Name  string
	Words []string
	Weak  []string
}

// Vocab is what parsing and matching know about categories.
type Vocab struct {
	Categories []Category
}

func (v Vocab) has(name string) bool {
	return slices.ContainsFunc(v.Categories, func(c Category) bool { return c.Name == name })
}

// Categorize is the first category one of whose words is a word of text,
// then the first one of whose weak words is; "other" when none is. This is
// the catalogue's categorizer (services.Categorize), the one
// implementation. A word matches whole words only, or its plural ("coin"
// is in "coins", not in "coingecko"; "price" in "prices", not in
// "priced"): another form of a word ("pricing", "scraping") says something
// else often enough ("priced per row" is what a call costs) that it names
// a category only when listed itself. A phrase ("adult content") matches
// its words in a row. Text splits at anything not a letter or digit and at
// camelCase ("getBTCPrice" is get, btc, price).
func (v Vocab) Categorize(text string) string {
	toks := CategoryTokens(text)
	for _, weak := range []bool{false, true} {
		for _, c := range v.Categories {
			words := c.Words
			if weak {
				words = c.Weak
			}
			for _, w := range words {
				if hasPhrase(toks, w) {
					return c.Name
				}
			}
		}
	}
	return "other"
}

// contradicts reports whether q has a word of a category other than cat
// and none of cat's own (weak words aside): the model's "finance" for
// "price of BTC" is its guess from "price", and btc says crypto. A
// contradicted category is dropped, never swapped: the text and the
// category synonyms still rank the query's category first.
func (v Vocab) contradicts(cat, q string) bool {
	if cat == "" {
		return false
	}
	toks := CategoryTokens(q)
	own, other := false, false
	for _, c := range v.Categories {
		if c.Name == "data" {
			continue
		}
		for _, w := range c.Words {
			if hasPhrase(toks, w) {
				if c.Name == cat {
					own = true
				} else {
					other = true
				}
				break
			}
		}
	}
	return other && !own
}

// CategoryTokens are text's words as Categorize matches them: lower case,
// split at anything not a letter or digit and at camelCase boundaries.
func CategoryTokens(text string) []string {
	var out []string
	var cur []rune
	rs := []rune(text)
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	for i, r := range rs {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
			continue
		}
		if len(cur) > 0 && unicode.IsUpper(r) {
			// "getBtc" splits before B; "BTCPrice" before P.
			if prev := rs[i-1]; unicode.IsLower(prev) || unicode.IsUpper(prev) && i+1 < len(rs) && unicode.IsLower(rs[i+1]) {
				flush()
			}
		}
		cur = append(cur, r)
	}
	flush()
	return out
}

// hasPhrase reports whether toks has phrase's words in a row, its last
// word whole or as a plural.
func hasPhrase(toks []string, phrase string) bool {
	ws := CategoryTokens(phrase)
	if len(ws) == 0 {
		return false
	}
	for i := 0; i+len(ws) <= len(toks); i++ {
		ok := true
		for j, w := range ws {
			if t := toks[i+j]; t != w && (j < len(ws)-1 || !isPlural(t, w)) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// isPlural reports whether t is w's plural: w and "s" or "es".
func isPlural(t, w string) bool {
	return len(t) > len(w) && (t == w+"s" || t == w+"es")
}

// synonyms are the category names a query word stands for: the word, or
// its plural, is one of the category's words, weak ones too (or its name).
func (v Vocab) synonyms(word string) []string {
	var out []string
	for _, c := range v.Categories {
		if c.Name == word || c.Name == "data" || c.Name == "other" {
			continue
		}
		for _, w := range slices.Concat(c.Words, c.Weak) {
			if w = strings.TrimSpace(w); w == word || isPlural(word, w) {
				out = append(out, c.Name)
				break
			}
		}
	}
	return out
}

var (
	// "under a cent", "less than one penny", "below a dollar"
	pricePhraseRE = regexp.MustCompile(`\b(?:under|below|less than|at most|up to|max(?:imum)?|cheaper than|no more than|within)\s+(?:a|one)\s+(cent|penny|dollar|buck)\b`)
	// "under $0.05", "under 5 cents", "max 0.01 usd", "<= $1". A number
	// counts only with a currency cue ("up to 10 languages" is no price),
	// and the trail group catches "$5,000" so it is not read as $5.
	priceAmountRE = regexp.MustCompile(`(?:\b(?:under|below|less than|at most|up to|max(?:imum)?|cheaper than|no more than|within)\s*|<=?\s*)(\$?)\s*(\d{1,7}(?:\.\d{1,6})?|\.\d{1,6})([.,]?\d)?\s*(cents?\b|¢|c\b|usd\b|usdc\b|dollars?\b|\$)?`)
	// "$0.01 or less", "0.01 usd max"
	priceTrailRE = regexp.MustCompile(`(\$)\s*(\d{1,7}(?:\.\d{1,6})?|\.\d{1,6})([.,]?\d)?\s*()(?:or less|max(?:imum)?|tops)?`)
	// A hyphen is no word boundary here: "royalty-free" or "gluten-free" is
	// a property of what is sought, not a price (bug report dc6d3fb5, 11-21).
	// RE2 has no lookbehind, so the edges are matched; cut turns the match
	// into a space either way.
	freeRE       = regexp.MustCompile(`(?:^|[^\w-])(?:free|no cost|costs nothing)(?:$|[^\w-])`)
	outputRE     = regexp.MustCompile(`\b(?:that\s+)?(?:returns?|returning|outputs?|gives?(?:\s+(?:me|back))?|responds? (?:with|in)|answers? (?:with|in)|in|as)\s+(?:a\s+|an\s+)?(json|text|plain text|markdown|csv|html|images?|png|jpe?g|audio|mp3|wav|video|mp4)\b`)
	outputBareRE = regexp.MustCompile(`\b(json|html|csv)\b`)
	networkRE    = regexp.MustCompile(`\b(?:on|via|over|paid on|pay on|settled on|using)\s+(base|polygon|solana|ethereum|arbitrum|optimism)\b`)
)

var outputWords = map[string]string{
	"json": "json", "text": "text", "plain text": "text", "markdown": "text", "csv": "text", "html": "html",
	"image": "image", "images": "image", "png": "image", "jpg": "image", "jpeg": "image",
	"audio": "audio", "mp3": "audio", "wav": "audio", "video": "video", "mp4": "video",
}

// typeWords are words that name a result type; keep says the word stays
// in the text too (it is also how such entries are named).
var typeWords = map[string]struct {
	types []string
	keep  bool
}{
	"api": {[]string{TypeService}, false}, "apis": {[]string{TypeService}, false}, "endpoint": {[]string{TypeService}, false}, "endpoints": {[]string{TypeService}, false},
	"x402": {[]string{TypeService}, false}, "tool": {[]string{TypeService}, false}, "tools": {[]string{TypeService}, false},
	"service": {[]string{TypeService}, false}, "services": {[]string{TypeService}, false},
	"offering": {[]string{TypeOffering}, false}, "offerings": {[]string{TypeOffering}, false}, "gig": {[]string{TypeOffering}, false},
	"agent": {[]string{TypeAgent}, false}, "agents": {[]string{TypeAgent}, false}, "bot": {[]string{TypeAgent}, false}, "bots": {[]string{TypeAgent}, false},
	"site": {[]string{TypeSite, TypeDomain}, false}, "sites": {[]string{TypeSite, TypeDomain}, false},
	"website": {[]string{TypeSite, TypeDomain}, false}, "websites": {[]string{TypeSite, TypeDomain}, false},
	"domain": {[]string{TypeDomain}, false}, "domains": {[]string{TypeDomain}, false},
	"mcp":     {[]string{TypeMCP}, false},
	"package": {[]string{TypePkg}, false}, "packages": {[]string{TypePkg}, false}, "library": {[]string{TypePkg}, false}, "libraries": {[]string{TypePkg}, false},
	"npm": {[]string{TypePkg}, true}, "pypi": {[]string{TypePkg}, true}, "crate": {[]string{TypePkg}, true},
}

// stopWords say nothing about what is wanted.
var stopWords = map[string]bool{
	"a": true, "an": true, "the": true, "i": true, "me": true, "my": true, "we": true, "us": true, "need": true, "needs": true, "want": true, "wants": true,
	"find": true, "looking": true, "look": true, "for": true, "to": true, "of": true, "with": true, "that": true, "which": true,
	"who": true, "can": true, "could": true, "please": true, "some": true, "any": true, "something": true, "someone": true, "anything": true,
	"get": true, "gets": true, "give": true, "show": true, "is": true, "are": true, "be": true, "it": true, "its": true, "and": true, "or": true,
	"on": true, "in": true, "at": true, "by": true, "from": true, "per": true, "call": true, "calls": true, "request": true, "server": true, "servers": true,
	"cheap": true, "cheapest": true, "inexpensive": true, "affordable": true, "good": true, "best": true, "reliable": true, "fast": true,
	"under": true, "below": true, "less": true, "than": true, "max": true, "usd": true, "dollar": true, "dollars": true, "cent": true, "cents": true,
	"returns": true, "return": true, "returning": true, "does": true, "do": true, "will": true, "should": true, "how": true, "what": true, "where": true,
	"this": true, "these": true, "there": true, "your": true, "you": true, "one": true, "about": true, "like": true, "into": true,
}

// priceMatch reads one price match: dollar is "$" or empty, amount the
// number, trail what follows the number directly (a further digit or a
// thousands separator, which refuses the whole match), unit the currency
// word after it. A bare number is no price: it needs "$" or a unit. Cents
// round up to a whole micro-USD, so a tiny cap never becomes free.
func priceMatch(dollar, amount, trail, unit string) (int64, bool) {
	unit = strings.TrimSpace(unit)
	if trail != "" || (dollar == "" && unit == "") {
		return 0, false
	}
	micro, ok := ParseUSD(amount)
	if !ok {
		return 0, false
	}
	if strings.HasPrefix(unit, "cent") || unit == "¢" || unit == "c" {
		micro = (micro + 99) / 100
	}
	return micro, true
}

// ParseRules is the deterministic parser: the filters q names and the text
// left. It never fails; an unreadable price is left in the text.
func ParseRules(q string, v Vocab) (Filters, string) {
	var f Filters
	s := " " + strings.ToLower(normalizeSpace(q)) + " "
	cut := func(re *regexp.Regexp, fn func(m []string) bool) {
		s = re.ReplaceAllStringFunc(s, func(match string) string {
			if fn(re.FindStringSubmatch(match)) {
				return " "
			}
			return match
		})
	}
	setPrice := func(micro int64) {
		if p := f.maxMicro(); p < 0 || micro < p {
			f.MaxPrice = FormatUSD(micro)
		}
	}
	cut(pricePhraseRE, func(m []string) bool {
		switch m[1] {
		case "cent", "penny":
			setPrice(10_000)
		default:
			setPrice(1_000_000)
		}
		return true
	})
	for _, re := range []*regexp.Regexp{priceAmountRE, priceTrailRE} {
		cut(re, func(m []string) bool {
			micro, ok := priceMatch(m[1], m[2], m[3], m[4])
			if ok {
				setPrice(micro)
			}
			return ok
		})
	}
	cut(freeRE, func([]string) bool { f.MaxPrice = "0"; return true })
	cut(outputRE, func(m []string) bool { f.Output = outputWords[m[1]]; return f.Output != "" })
	if f.Output == "" {
		cut(outputBareRE, func(m []string) bool { f.Output = outputWords[m[1]]; return true })
	}
	cut(networkRE, func(m []string) bool { f.Network = m[1]; return true })
	// A sign-up door: its words count only beside a sign-up word, except
	// "no CAPTCHA"; read, they and the sign-up words leave the text, so the
	// query can browse every entry with that door.
	setDoor := func(door string) func([]string) bool {
		return func([]string) bool {
			if f.Door == "" {
				f.Door = door
			}
			return true
		}
	}
	if signupRE.MatchString(s) {
		cut(doorWalletRE, setDoor(DoorWallet))
		cut(doorKeyRE, setDoor(DoorKey))
		cut(doorAgentRE, setDoor(DoorGroupAgent))
	}
	cut(noCaptchaRE, setDoor(DoorGroupAgent))
	if f.Door != "" {
		cut(signupRE, func([]string) bool { return true })
	}
	var text []string
	// A type word inside a hyphen- or underscore-joined token is part of a
	// name ("agent-exec-attest"), never the type asked for.
	type word struct {
		w      string
		joined bool
	}
	var words []word
	for _, tok := range strings.Fields(s) {
		ws := Words(tok)
		j := len(ws) > 1 && strings.ContainsAny(tok, "-_")
		for _, w := range ws {
			words = append(words, word{w, j})
		}
	}
	for _, ww := range words {
		w := ww.w
		if t, ok := typeWords[w]; ok && f.Door != "" && slices.Equal(t.types, []string{TypeAgent}) {
			continue // "my agent" signs up: who, not what is sought
		}
		if t, ok := typeWords[w]; ok && !ww.joined {
			// "mcp" in "mcp server", "agent" and the rest name a type.
			for _, typ := range t.types {
				if !slices.Contains(f.Types, typ) {
					f.Types = append(f.Types, typ)
				}
			}
			if !t.keep {
				continue
			}
		}
		if stopWords[w] {
			continue
		}
		if !slices.Contains(text, w) {
			text = append(text, w)
		}
	}
	text = applyLanguage(&f, text, q)
	slices.SortStableFunc(f.Types, func(a, b string) int { return slices.Index(Types, a) - slices.Index(Types, b) })
	return f, strings.Join(text, " ")
}

// Words are a text's lower-cased words: runs of letters, digits and
// the marks that complete letters (Hindi, Thai vowel signs).
func Words(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !unicode.IsMark(r) })
}

func normalizeSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// CheckFilters validates filters a caller or the model set: known types,
// a known category, a USD amount, a known output and network. The error
// names the field.
func CheckFilters(f Filters, v Vocab) (Filters, string) {
	var out Filters
	for _, t := range f.Types {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		if !slices.Contains(Types, t) {
			return out, "type is one or more of " + strings.Join(Types, ", ") + "."
		}
		if !slices.Contains(out.Types, t) {
			out.Types = append(out.Types, t)
		}
	}
	slices.SortStableFunc(out.Types, func(a, b string) int { return slices.Index(Types, a) - slices.Index(Types, b) })
	if c := strings.ToLower(strings.TrimSpace(f.Category)); c != "" {
		if !v.has(c) && c != "other" {
			names := make([]string, 0, len(v.Categories))
			for _, c := range v.Categories {
				names = append(names, c.Name)
			}
			return out, "category is one of " + strings.Join(names, ", ") + "."
		}
		out.Category = c
	}
	if p := strings.TrimSpace(f.MaxPrice); p != "" {
		n, ok := ParseUSD(p)
		if !ok {
			return out, "max_price is a USD amount, such as 0.01."
		}
		out.MaxPrice = FormatUSD(n)
	}
	if o := strings.ToLower(strings.TrimSpace(f.Output)); o != "" {
		if !slices.Contains(Outputs, o) {
			return out, "output is one of " + strings.Join(Outputs, ", ") + "."
		}
		out.Output = o
	}
	if n := strings.TrimSpace(f.Network); n != "" {
		if out.Network = NetworkName(n); out.Network == "" {
			return out, "network is base, polygon, ethereum, optimism, arbitrum, solana or tempo (or its CAIP-2 id)."
		}
	}
	if p := strings.ToLower(strings.TrimSpace(f.Protocol)); p != "" {
		if !slices.Contains(Protocols, p) {
			return out, "protocol is " + strings.Join(Protocols, " or ") + "."
		}
		out.Protocol = p
	}
	if d := strings.ToLower(strings.TrimSpace(f.Door)); d != "" {
		if !slices.Contains(DoorFilters, d) {
			return out, "door is agent (no human step), human, or one of " + strings.Join(Doors, ", ") + "."
		}
		out.Door = d
	}
	return out, ""
}

// merge applies the caller's explicit filters over parsed ones, field by
// field: a set field wins, even empty (cleared).
func merge(parsed, set Filters, keys map[string]bool) Filters {
	if keys["type"] {
		parsed.Types = set.Types
	}
	if keys["category"] {
		parsed.Category = set.Category
	}
	if keys["max_price"] {
		parsed.MaxPrice = set.MaxPrice
	}
	if keys["output"] {
		parsed.Output = set.Output
	}
	if keys["network"] {
		parsed.Network = set.Network
	}
	if keys["protocol"] {
		parsed.Protocol = set.Protocol
	}
	if keys["door"] {
		parsed.Door = set.Door
	}
	if parsed.Types == nil {
		parsed.Types = []string{}
	}
	return parsed
}

// sentence reports whether q reads as a request worth the model: three or
// more words, or a word the rules read as a filter cue.
func sentence(q string) bool {
	return len(Words(q)) >= 3
}
