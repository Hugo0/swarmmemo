package find

// Ranking. Relevance is BM25 over the best candidate's, so the best match
// is 1; a query word a doc has only as another form of it in its
// description ("priced" for "price") first takes LooseCut of that word's
// share off the doc's BM25 (looseWords). Quality q, from -1 to 1, adds q × QualityShift: it can only
// reorder results within 2 × QualityShift of each other. Down (our calls
// fail, or the lab says down) and the lab's avoid set q to -1: they sink
// within their band, and are never hidden.

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"
)

// quality is d's q with its parts. Under a door filter (door), the door's
// provenance adds a bounded term (doorTerm): measured +1, outside +0.5,
// inferred 0, self -0.5, stale -0.5 more; without one the door never moves
// rank.
func quality(d *Doc, door bool, now time.Time) (float64, map[string]any) {
	qd := d.Quality
	parts := map[string]any{}
	var sum float64
	var n int
	add := func(name string, v float64) {
		v = math.Max(-1, math.Min(1, v))
		parts[name] = round4(v)
		sum += v
		n++
	}
	sink := ""
	if qd.HasLab() {
		parts["lab_verdict"] = qd.LabVerdict
		switch qd.LabVerdict {
		case "avoid", "down":
			sink = "lab " + qd.LabVerdict
		}
		if qd.LabLower >= 0 {
			parts["lab_band_lower"] = qd.LabLower
			add("lab", float64(qd.LabLower-50)/50)
		}
	}
	if qd.Reviews > 0 {
		parts["reviews"] = qd.Reviews
		parts["reviews_verdict_mean"] = round4(qd.ReviewMean)
		parts["reviews_weight"] = round4(qd.ReviewWeight)
		if qd.ReviewWeight > 0 {
			add("community", (qd.ReviewMean-0.5)*2)
		}
	}
	if qd.Standing != "" {
		// v(standing), the one weight (RFC 0021 §12).
		parts["standing_band"] = qd.Standing
		add("standing", qd.StandingV)
	}
	if qd.AcceptedTasks > 0 {
		parts["accepted_tasks"] = qd.AcceptedTasks // shown, never weighed
		parts["paid_tasks"] = qd.PaidTasks
	}
	switch qd.State {
	case "up":
		parts["state"] = "up"
		add("up", 0.3)
	case "down":
		parts["state"] = "down"
		sink = "down"
	}
	if sink != "" {
		parts["sinks"] = sink
		return -1, parts
	}
	q := 0.0
	if n > 0 {
		q = sum / float64(n)
	}
	if v, ok := doorTerm(d, now); ok && door {
		parts["door_source"] = round4(v)
		q = math.Max(-1, math.Min(1, q+v))
	}
	return q, parts
}

// scored is one ranked candidate.
type scored struct {
	doc     *Doc
	bm25    float64
	rel     float64
	q       float64
	score   float64
	after   bool // ranked after the rest: a method's door on a sign-up question
	parts   map[string]any
	matched []string
	fields  []string
	loose   []string // words found only as another form in the description
	factor  float64  // what loose leaves of the BM25: 1 - LooseCut × loose/words
	now     time.Time
}

// rank orders the candidates. words are the query's words (for the why and
// the explain); with none, relevance is 1 for every candidate. door says a
// door filter applies; signup that the query asks where to sign up, so
// entries whose door is only a SwarmMemo method's come after the places to
// sign up, whatever their score (signupQuestion).
func (ix *Index) rank(cands []match, words []string, door, signup bool) []scored {
	out := make([]scored, 0, len(cands))
	best := 0.0
	for _, m := range cands {
		d := &ix.docs[m.doc]
		s := scored{doc: d, bm25: m.bm25, rel: 1, factor: 1, now: ix.BuiltAt}
		if s.loose = looseWords(d, words); len(s.loose) > 0 {
			s.factor = 1 - LooseCut*float64(len(s.loose))/float64(len(words))
		}
		best = math.Max(best, -m.bm25*s.factor)
		out = append(out, s)
	}
	for i := range out {
		s := &out[i]
		d := s.doc
		if best > 0 {
			s.rel = -s.bm25 * s.factor / best
		}
		s.q, s.parts = quality(d, door, ix.BuiltAt)
		if signup && methodDoor(d) {
			s.after = true
			s.parts["door_after"] = "a SwarmMemo method's door, after the places to sign up"
		}
		s.score = s.rel + QualityShift*s.q
		s.matched, s.fields = matchedWords(d, words, ix.vocab)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.after != b.after {
			return b.after
		}
		if a.score != b.score {
			return a.score > b.score
		}
		if a.rel != b.rel {
			return a.rel > b.rel
		}
		if a.doc.Name != b.doc.Name {
			return a.doc.Name < b.doc.Name
		}
		return a.doc.ID < b.doc.ID
	})
	return out
}

// matchedWords are the words d matches (a word, its stem-like prefix or a
// category it stands for) and the fields they are in.
func matchedWords(d *Doc, words []string, v Vocab) ([]string, []string) {
	fields := []struct{ name, text string }{{"name", d.Name}, {"category", d.Category}, {"description", d.Description}, {"url", d.Terms}}
	var matched, in []string
	for _, w := range words {
		probe := w
		if r := []rune(w); len(r) > 5 {
			probe = string(r[:5]) // a crude stem: "forecasts" finds "forecast"
		}
		hit := false
		for _, f := range fields {
			t := strings.ToLower(f.text)
			ok := strings.Contains(t, probe)
			if !ok && f.name == "category" {
				ok = slices.Contains(v.synonyms(w), d.Category)
			}
			if ok {
				hit = true
				if !slices.Contains(in, f.name) {
					in = append(in, f.name)
				}
			}
		}
		if hit {
			matched = append(matched, w)
		}
	}
	order := []string{"name", "category", "description", "url"}
	slices.SortFunc(in, func(a, b string) int { return slices.Index(order, a) - slices.Index(order, b) })
	return matched, in
}

// LooseCut is how much of its share of the match a query word loses when a
// doc has it only as another form of it in its description: "priced per
// row" stems to "price", yet says nothing of what the endpoint returns, and
// the "finance" category the catalogue read from that same word stands for
// "price" too. The word's exact form (or its plural) anywhere, or any form
// in the name, category or URL words, counts in full. A doc's BM25 is
// multiplied by 1 - LooseCut × loose words / query words before relevance
// is taken: a loose word counts half. It reorders, never hides.
const LooseCut = 0.5

// looseWords are the query words d has only as another form of them in its
// description: no exact form (the word, its plural or singular) in any
// field, no form at all in the name, category or URL words, and a form
// sharing its stem in the description. A word d has nowhere in its text (a
// category synonym, say) is not loose: BM25 weighs it as it is.
func looseWords(d *Doc, words []string) []string {
	if len(words) == 0 {
		return nil
	}
	strong := slices.Concat(Words(d.Name), Words(d.Category), Words(d.Terms))
	desc := Words(d.Description)
	var out []string
	for _, w := range words {
		if nonLatin(w) || len([]rune(w)) < 4 {
			continue
		}
		if slices.ContainsFunc(strong, func(t string) bool { return sameForm(t, w) || sameStem(t, w) }) {
			continue
		}
		exact, other := false, false
		for _, t := range desc {
			switch {
			case sameForm(t, w):
				exact = true
			case sameStem(t, w):
				other = true
			}
		}
		if other && !exact {
			out = append(out, w)
		}
	}
	return out
}

// sameForm reports whether t is w, or one is the other's plural (price,
// prices; query, queries).
func sameForm(t, w string) bool {
	plural := func(a, b string) bool {
		return a == b+"s" || a == b+"es" || strings.HasSuffix(b, "y") && a == b[:len(b)-1]+"ies"
	}
	return t == w || plural(t, w) || plural(w, t)
}

// sameStem reports whether t and w share a stem of four letters or more,
// as the index's prefix and porter matches find one for the other
// ("priced", "pricing" for "price"; "funded" for "funding"): one, less a
// common ending, begins the other.
func sameStem(t, w string) bool {
	a, b := crudeStem(t), crudeStem(w)
	if len(a) > len(b) {
		a, b = b, a
	}
	return len([]rune(a)) >= 4 && strings.HasPrefix(b, a)
}

// crudeStem is s less one common English ending, when four letters stay.
func crudeStem(s string) string {
	for _, end := range []string{"ings", "ing", "ers", "ed", "es", "er", "s", "e"} {
		if r := strings.TrimSuffix(s, end); r != s && len([]rune(r)) >= 4 {
			return r
		}
	}
	return s
}

// hit is s as the wire shows it.
func (s scored) hit(explain bool) Hit {
	d := s.doc
	h := Hit{Type: d.Type, Kind: d.Kind, ID: d.ID, Name: d.Name, Description: d.Description, Category: d.Category, URL: d.URL,
		PriceText: d.PriceText, Network: d.Network, Output: d.Output, Protocols: d.Protocols, SubjectID: d.SubjectID, ReviewURL: d.Review, Also: d.Also, State: d.Quality.State,
		Standing: d.Quality.Standing, AcceptedTasks: d.Quality.AcceptedTasks, PaidTasks: d.Quality.PaidTasks, Reputation: d.Quality.Reputation, Untrusted: d.Untrusted, ListedBy: d.ListedBy, Score: round4(s.score)}
	if h.Category == "other" {
		h.Category = ""
	}
	if d.Price >= 0 {
		h.Price = FormatUSD(d.Price)
		if h.PriceText == "" {
			h.PriceText = usdText(d.Price) + " a call"
			if d.Price == 0 {
				h.PriceText = "free"
			}
		}
	}
	if q := d.Quality; q.HasLab() {
		h.Lab = &LabMark{Verdict: q.LabVerdict, Grade: q.LabGrade, GradeNote: q.LabGradeNote}
		if q.LabLower >= 0 {
			lower := q.LabLower
			h.Lab.Lower = &lower
		}
	}
	if q := d.Quality; q.Reviews > 0 {
		h.Reviews = &Reviews{Count: q.Reviews, Mean: round4(q.ReviewMean), Word: reviewWord(q)}
	}
	h.Door = doorMark(d, s.now)
	h.Why = s.why()
	if explain {
		h.Explain = &Explain{BM25: s.bm25, Relevance: round4(s.rel), Matched: s.matched, Fields: s.fields, Loose: s.loose, Quality: s.parts,
			Q: round4(s.q), Shift: round4(QualityShift * s.q), Score: round4(s.score), Rule: "score = relevance + 0.1 × q"}
		if len(s.loose) > 0 {
			h.Explain.LooseFactor = round4(s.factor)
		}
	}
	return h
}

// usdText is a price as people read it: at least cents ("$0.50", "$0.002").
func usdText(micro int64) string {
	s := FormatUSD(micro)
	whole, frac, _ := strings.Cut(s, ".")
	for len(frac) < 2 {
		frac += "0"
	}
	return "$" + whole + "." + frac
}

// reviewWord sums the community's verdict mean in a word.
func reviewWord(q Quality) string {
	switch mean := q.ReviewMean; {
	case q.ReviewWeight <= 0:
		return "not weighed yet"
	case mean >= 0.65:
		return "mostly good"
	case mean <= 0.35:
		return "mostly bad"
	}
	return "mixed"
}

// why is the one line that says why s is here and where it stands.
func (s scored) why() string {
	d := s.doc
	var parts []string
	switch {
	case len(s.matched) > 0:
		quoted := make([]string, len(s.matched))
		for i, w := range s.matched {
			quoted[i] = "“" + w + "”"
		}
		where := ""
		if len(s.fields) > 0 {
			where = " in its " + strings.Join(s.fields, ", ")
		}
		parts = append(parts, "Matches "+strings.Join(quoted, ", ")+where)
	case s.bm25 != 0:
		parts = append(parts, "Matches your words")
	default:
		parts = append(parts, "Fits your filters")
	}
	q := d.Quality
	switch {
	case q.HasLab() && q.LabGrade != "":
		parts = append(parts, fmt.Sprintf("lab grade %s (%s)", q.LabGrade, q.LabVerdict))
	case q.HasLab() && q.LabGradeNote != "":
		parts = append(parts, fmt.Sprintf("lab: %s, %s", q.LabVerdict, q.LabGradeNote))
	case q.HasLab():
		parts = append(parts, "lab: "+q.LabVerdict)
	}
	if q.Reviews > 0 {
		noun := "reviews"
		if q.Reviews == 1 {
			noun = "review"
		}
		parts = append(parts, fmt.Sprintf("%d %s, %s", q.Reviews, noun, reviewWord(q)))
	}
	if q.Standing != "" {
		parts = append(parts, q.Standing+" standing")
	}
	if q.AcceptedTasks > 0 {
		parts = append(parts, Delivered(q.AcceptedTasks, q.PaidTasks))
	}
	if w := doorWhy(d, s.now); w != "" {
		parts = append(parts, w)
	}
	switch {
	case q.State == "down" && q.StateNote != "":
		parts = append(parts, "down: "+q.StateNote)
	case q.State == "down":
		parts = append(parts, "down")
	case q.StateNote != "":
		parts = append(parts, q.StateNote)
	}
	if s.q < 0 && q.State != "down" && q.HasLab() && (q.LabVerdict == "avoid" || q.LabVerdict == "down") {
		parts = append(parts, "ranked lower")
	}
	return strings.Join(parts, " · ")
}

// Delivered says an agent's work record as worker in one phrase,
// "Delivered 2 accepted · 1 paid": every accept counted, and those whose
// reward was paid. The reputation header, a hit's why and the board's
// pages say it from here.
func Delivered(accepted, paid int) string {
	return fmt.Sprintf("Delivered %d accepted · %d paid", accepted, paid)
}

// ReviewWord is the word for a community aggregate, given its verdict mean
// and the weight behind it: what a result's why says, and what a "Rated on
// SwarmMemo" badge shows.
func ReviewWord(mean, weight float64) string {
	return reviewWord(Quality{ReviewMean: mean, ReviewWeight: weight})
}
