package reviews

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"swarmmemo/internal/judge"
)

// The universal verdicts (RFC 0019 §3), in order from best. A lab's
// proceed, caution, avoid and down are good, mixed, bad and unusable.
var Verdicts = []string{"good", "mixed", "bad", "unusable", "unverified"}

// LabVerdicts maps each verdict to the lab word x402 Trust and the design
// use for it, shown beside the verdict on a lab card.
var LabVerdicts = map[string]string{"good": "proceed", "mixed": "caution", "bad": "avoid", "unusable": "down", "unverified": "unverified"}

// ScoreVerdict is the verdict a 0-100 score (50 neutral, RFC 0020) reads
// as: the verdict whose judge.VerdictX is nearest, a tie going to the better
// verdict (ScoreBands). A rated work.accept's review carries both (the score
// as its good dimension), so the score and its verdict never disagree.
func ScoreVerdict(score int) string {
	for _, b := range ScoreBands {
		if score >= b.From {
			return b.Verdict
		}
	}
	return ScoreBands[len(ScoreBands)-1].Verdict
}

// ScoreBand is the lowest score (From) that reads as Verdict.
type ScoreBand struct {
	From    int    `json:"from"`
	Verdict string `json:"verdict"`
}

// ScoreBands are ScoreVerdict's bands, best first, derived from
// judge.VerdictX (the one verdict-to-x map): each band starts at the
// midpoint (rounded up) between its verdict's x and the next better one's.
// clients/contract.json carries them for the clients that build a rating.
var ScoreBands = scoreBands()

func scoreBands() []ScoreBand {
	var out []ScoreBand
	for _, v := range Verdicts { // best first; unverified has no x
		x, ok := judge.VerdictX[v]
		if !ok {
			continue
		}
		out = append(out, ScoreBand{From: int(x), Verdict: v})
	}
	for i := 0; i+1 < len(out); i++ {
		out[i].From = (out[i].From + out[i+1].From + 1) / 2
	}
	out[len(out)-1].From = 0
	return out
}

// UniversalVerdict is the verdict for a lab word (internal/rubric's
// proceed, caution, avoid, down, unverified); "" for anything else.
func UniversalVerdict(lab string) string {
	for v, w := range LabVerdicts {
		if w == lab {
			return v
		}
	}
	return ""
}

// Dimensions are the four universal dimensions, each 0-100.
var Dimensions = []string{"works", "as_described", "safe", "good"}

// NoGrade is what shows in place of a lab assessment's letter grade while a
// dimension is unmeasured (GLOSSARY "no grade").
const NoGrade = "no grade"

// GradeShown is the grade every surface shows for a lab assessment, derived
// from the grade and dimensions it stores (C752). The stored grade stays the
// record (the recompute compares it); a page or summary shows a letter only
// when every universal dimension was measured. A dimension the scorer left na
// (good needs a paid call) is absent from the stored dimensions; the headline
// then rests on unmeasured checks held at the prior, and the band's low edge,
// which the letter comes from, says little. Then Grade is "", Unmeasured
// names those dimensions and Note says "no grade (good not measured)".
type GradeShown struct {
	Grade      string
	Note       string
	Unmeasured []string
}

// ShowGrade derives GradeShown from a lab assessment's stored grade and
// dimensions; no stored grade shows nothing.
func ShowGrade(grade string, dims map[string]int) GradeShown {
	if grade == "" {
		return GradeShown{}
	}
	var missing []string
	for _, d := range Dimensions {
		if _, ok := dims[d]; !ok {
			missing = append(missing, d)
		}
	}
	if len(missing) == 0 {
		return GradeShown{Grade: grade}
	}
	return GradeShown{Note: NoGrade + " (" + strings.Join(missing, ", ") + " not measured)", Unmeasured: missing}
}

// Relationships a reviewer declares. The service may raise one (owner,
// affiliate, paid), never lower it. lab is a run of a rubric; outside_source
// republishes another rater's public figure and is open only to the
// operator's importer keys.
var Relationships = []string{"customer", "tester", "lab", "outside_source", "paid", "affiliate", "owner", "competitor", "none"}

// EvidenceKinds are the evidence id prefixes (RFC 0015 §5.2).
var EvidenceKinds = []string{"call", "work", "fetch", "stamp", "tx"}

// EvidenceForms is how each EvidenceKinds entry is written, in that order:
// the refusal of a malformed id and the write_review tool say it from here.
var EvidenceForms = []string{"call:CALL_ID (an x402 call, a SwarmMemo tool's call or its receipt's hold_id, or an offering call's oc_ id)", "work:WORK_ID", "fetch:CALL_ID (32 hex)", "stamp:SHA256 (64 hex)", "tx:CHAIN:0xHASH"}

// EvidenceFormsText is EvidenceForms as prose: "call:CALL_ID (...), ... or tx:CHAIN:0xHASH".
func EvidenceFormsText() string { return orList(EvidenceForms) }

// TypesText is Types as prose: "x402, mpp, ... or pkg".
func TypesText() string { return orList(Types) }

// orList joins items as prose: "a", "a or b", "a, b or c".
func orList(items []string) string {
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " or " + items[len(items)-1]
}

// Caps on one review (RFC 0019 §4). The post data's 1,024 bytes bound
// the whole.
const (
	MaxEvidence           = 8
	MaxObservations       = 32
	MaxObservationCites   = 4
	MaxFlags              = 8
	MaxCount              = 1000000
	MaxSourceNameBytes    = 64
	MaxSourceURLBytes     = 256
	MaxSourceTermsBytes   = 128
	MaxSourceFigureBytes  = 32
	MaxObservationValueAb = 1e12
)

// Review is one parsed data.review.
type Review struct {
	Subject      Subject
	Verdict      string
	Dimensions   map[string]int
	Relationship string
	Evidence     []string
	Observations []Observation
	Rubric       *RubricRef
	Score        *int
	Band         *[2]int
	Grade        string
	Flags        []string
	Record       *Record
	Source       *Source
}

// Observation is one check result: counts (pass, fail), a measured value in
// the rubric's unit, or not applicable. Cites index Evidence.
type Observation struct {
	Check string   `json:"check"`
	Pass  *int     `json:"pass,omitempty"`
	Fail  *int     `json:"fail,omitempty"`
	Value *float64 `json:"value,omitempty"`
	NA    bool     `json:"na,omitempty"`
	Cites []int    `json:"evidence,omitempty"`
}

// RubricRef pins the rubric file a review applied, by its bytes.
type RubricRef struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
}

// Record is the full record behind a review, too big for post data: its
// SHA-256 and, optionally, the doc (paste) that holds it.
type Record struct {
	SHA256 string `json:"sha256"`
	Doc    string `json:"doc,omitempty"`
}

// Source is the rater an outside_source review republishes, with its own
// figure kept as given.
type Source struct {
	Name    string   `json:"name"`
	URL     string   `json:"url"`
	Terms   string   `json:"terms,omitempty"`
	Score   *float64 `json:"score,omitempty"`
	Grade   string   `json:"grade,omitempty"`
	Verdict string   `json:"verdict,omitempty"`
}

// ParseError is a refused data.review; Code is invalid_review.
type ParseError struct{ Message string }

func (e *ParseError) Error() string { return e.Message }

func bad(format string, args ...any) error {
	return &ParseError{Message: fmt.Sprintf(format, args...)}
}

const reviewUsage = `data.review is {"subject":"TYPE:KEY","verdict":"good|mixed|bad|unusable|unverified"} with optional dimensions {works,as_described,safe,good} (0-100), relationship, evidence, observations, rubric, record and, for relationship lab, score, band, grade and flags (RFC 0019).`

var (
	hex64RE     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hex32RE     = regexp.MustCompile(`^[0-9a-f]{32}$`)
	checkIDRE   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,15}$`)
	flagRE      = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
	gradeRE     = regexp.MustCompile(`^[A-F]$`)
	txRE        = regexp.MustCompile(`^[a-z0-9-]{3,8}:[A-Za-z0-9-]{1,32}:0x[0-9a-f]{64}$`)
	smallIntRE  = regexp.MustCompile(`^(0|[1-9][0-9]?|100)$`)
	countRE     = regexp.MustCompile(`^(0|[1-9][0-9]{0,6})$`)
	cleanTextRE = regexp.MustCompile(`^[^\x00-\x1f\x7f]*$`)
)

// strictObject decodes one JSON object whose keys are unique and whose values
// are never null.
func strictObject(raw []byte) (map[string]json.RawMessage, []string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, nil, errors.New("not an object")
	}
	out := map[string]json.RawMessage{}
	var order []string
	for dec.More() {
		t, err := dec.Token()
		name, ok := t.(string)
		if err != nil || !ok {
			return nil, nil, errors.New("bad key")
		}
		if _, dup := out[name]; dup {
			return nil, nil, fmt.Errorf("duplicate field %q", name)
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil || bytes.Equal(v, []byte("null")) {
			return nil, nil, fmt.Errorf("field %q is null or invalid", name)
		}
		out[name] = v
		order = append(order, name)
	}
	if _, err := dec.Token(); err != nil {
		return nil, nil, errors.New("unterminated object")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, nil, errors.New("trailing data")
	}
	return out, order, nil
}

func str(raw json.RawMessage) (string, bool) {
	var s string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

func strList(raw json.RawMessage, max int) ([]string, bool) {
	var items []json.RawMessage
	if len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &items) != nil || len(items) > max {
		return nil, false
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		s, ok := str(it)
		if !ok || slices.Contains(out, s) {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

func intIn(raw json.RawMessage, re *regexp.Regexp) (int, bool) {
	if !re.Match(raw) {
		return 0, false
	}
	n, err := strconv.Atoi(string(raw))
	return n, err == nil
}

func number(raw json.RawMessage, limit float64) (float64, bool) {
	if len(raw) == 0 || len(raw) > 32 || raw[0] == '"' || raw[0] == '{' || raw[0] == '[' || raw[0] == 't' || raw[0] == 'f' {
		return 0, false
	}
	f, err := strconv.ParseFloat(string(raw), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || math.Abs(f) > limit {
		return 0, false
	}
	return f, true
}

func cleanText(s string, max int) bool {
	return s != "" && len(s) <= max && utf8.ValidString(s) && cleanTextRE.MatchString(s) && strings.TrimSpace(s) == s
}

// ValidEvidenceID reports whether id is one evidence id: call:, work: or
// fetch: and a 32-hex id (call: also an offering call's oc_ id), stamp: and a 64-hex SHA-256, or tx: and a CAIP-2
// chain and a 0x transaction hash.
func ValidEvidenceID(id string) bool {
	kind, v, ok := strings.Cut(id, ":")
	if !ok {
		return false
	}
	switch kind {
	case "call":
		// An x402 call through the relay, a call of one of SwarmMemo's
		// own tools (its call id, or its receipt's 32-hex hold_id), or a
		// call of an agent offering (oc_ and 32 hex, RFC 0017).
		return hex32RE.MatchString(v) || strings.HasPrefix(v, "oc_") && hex32RE.MatchString(v[3:])
	case "work", "fetch":
		return hex32RE.MatchString(v)
	case "stamp":
		return hex64RE.MatchString(v)
	case "tx":
		return txRE.MatchString(v)
	}
	return false
}

// Parse validates data.review strictly: known fields only, no duplicates or
// nulls, a canonical subject, a known rubric pinned by its bytes, check ids
// and flags the rubric names, and the lab-only and outside_source-only
// fields only with that relationship. opt is the board's own hosts.
func Parse(raw []byte, opt Options) (Review, error) {
	var r Review
	fields, order, err := strictObject(raw)
	if err != nil {
		return r, bad("data.review: %v. %s", err, reviewUsage)
	}
	for _, name := range order {
		switch name {
		case "subject", "verdict", "dimensions", "relationship", "evidence", "observations", "rubric", "score", "band", "grade", "flags", "record", "source":
		default:
			return r, bad("data.review: unknown field %q. %s", name, reviewUsage)
		}
	}
	subject, ok := str(fields["subject"])
	if fields["subject"] == nil || !ok {
		return r, bad("data.review.subject is required: the canonical TYPE:KEY (subject.resolve gives it).")
	}
	s, err := Resolve(subject, opt)
	if err != nil {
		return r, err
	}
	if s.URI() != subject {
		return r, bad("data.review.subject must be canonical: %q (subject.resolve gives it).", s.URI())
	}
	r.Subject = s
	if fields["verdict"] == nil {
		return r, bad("data.review.verdict is required: %s.", strings.Join(Verdicts, ", "))
	}
	if r.Verdict, ok = str(fields["verdict"]); !ok || !slices.Contains(Verdicts, r.Verdict) {
		return r, bad("data.review.verdict is one of %s.", strings.Join(Verdicts, ", "))
	}
	r.Relationship = "none"
	if raw := fields["relationship"]; raw != nil {
		if r.Relationship, ok = str(raw); !ok || !slices.Contains(Relationships, r.Relationship) {
			return r, bad("data.review.relationship is one of %s.", strings.Join(Relationships, ", "))
		}
	}
	if raw := fields["dimensions"]; raw != nil {
		dims, _, err := strictObject(raw)
		if err != nil || len(dims) == 0 {
			return r, bad("data.review.dimensions is an object of works, as_described, safe and good, each an integer 0-100.")
		}
		r.Dimensions = map[string]int{}
		for k, v := range dims {
			n, ok := intIn(v, smallIntRE)
			if !slices.Contains(Dimensions, k) || !ok {
				return r, bad("data.review.dimensions is an object of works, as_described, safe and good, each an integer 0-100.")
			}
			r.Dimensions[k] = n
		}
	}
	if raw := fields["evidence"]; raw != nil {
		if r.Evidence, ok = strList(raw, MaxEvidence); !ok {
			return r, bad("data.review.evidence is at most %d distinct evidence ids.", MaxEvidence)
		}
		for _, id := range r.Evidence {
			if !ValidEvidenceID(id) {
				return r, bad("Evidence %q is not an evidence id: "+EvidenceFormsText()+".", id)
			}
		}
	}
	var rubric *Rubric
	if raw := fields["rubric"]; raw != nil {
		ref, _, err := strictObject(raw)
		if err != nil || len(ref) != 3 {
			return r, bad("data.review.rubric is {\"id\",\"version\",\"sha256\"} of a rubric file /capabilities reviews.rubrics lists.")
		}
		id, ok1 := str(ref["id"])
		version, ok2 := str(ref["version"])
		sum, ok3 := str(ref["sha256"])
		if !ok1 || !ok2 || !ok3 {
			return r, bad("data.review.rubric is {\"id\",\"version\",\"sha256\"} of a rubric file /capabilities reviews.rubrics lists.")
		}
		rubric = LookupRubric(id, version)
		if rubric == nil {
			return r, bad("Unknown rubric %s %s; /capabilities reviews.rubrics lists the rubric files.", id, version)
		}
		if sum != rubric.SHA256 {
			return r, bad("data.review.rubric.sha256 is not the SHA-256 of rubric %s %s (%s).", id, version, rubric.SHA256)
		}
		if !slices.Contains(rubric.SubjectTypes, s.Type) {
			return r, bad("Rubric %s applies to %s subjects, not %s.", id, strings.Join(rubric.SubjectTypes, ", "), s.Type)
		}
		r.Rubric = &RubricRef{ID: id, Version: version, SHA256: sum}
	}
	if raw := fields["observations"]; raw != nil {
		if rubric == nil {
			return r, bad("data.review.observations need a rubric: each names one of its checks.")
		}
		var items []json.RawMessage
		if raw[0] != '[' || json.Unmarshal(raw, &items) != nil || len(items) == 0 || len(items) > MaxObservations {
			return r, bad("data.review.observations is 1 to %d check results.", MaxObservations)
		}
		seen := map[string]bool{}
		for _, it := range items {
			o, err := parseObservation(it, rubric, len(r.Evidence))
			if err != nil {
				return r, err
			}
			if seen[o.Check] {
				return r, bad("Check %s is observed twice; one result per check.", o.Check)
			}
			seen[o.Check] = true
			r.Observations = append(r.Observations, o)
		}
	}
	lab := r.Relationship == "lab"
	for _, name := range []string{"score", "band", "grade", "flags"} {
		if fields[name] != nil && !lab {
			return r, bad("data.review.%s is a lab assessment's: it needs relationship lab and a rubric.", name)
		}
	}
	if lab && rubric == nil {
		return r, bad("A lab assessment (relationship lab) names the rubric it ran: data.review.rubric.")
	}
	if raw := fields["score"]; raw != nil {
		n, ok := intIn(raw, smallIntRE)
		if !ok {
			return r, bad("data.review.score is an integer 0-100.")
		}
		r.Score = &n
	}
	if raw := fields["band"]; raw != nil {
		var band []json.RawMessage
		if raw[0] != '[' || json.Unmarshal(raw, &band) != nil || len(band) != 2 {
			return r, bad("data.review.band is [low, high], integers 0-100 around score.")
		}
		lo, ok1 := intIn(band[0], smallIntRE)
		hi, ok2 := intIn(band[1], smallIntRE)
		if !ok1 || !ok2 || lo > hi || r.Score == nil || *r.Score < lo || *r.Score > hi {
			return r, bad("data.review.band is [low, high], integers 0-100 around score.")
		}
		r.Band = &[2]int{lo, hi}
	}
	if raw := fields["grade"]; raw != nil {
		if r.Grade, ok = str(raw); !ok || !gradeRE.MatchString(r.Grade) {
			return r, bad("data.review.grade is one letter, A to F.")
		}
	}
	if raw := fields["flags"]; raw != nil {
		if r.Flags, ok = strList(raw, MaxFlags); !ok {
			return r, bad("data.review.flags is at most %d distinct flags of the rubric's gates.", MaxFlags)
		}
		for _, f := range r.Flags {
			if !flagRE.MatchString(f) || !rubric.HasFlag(f) {
				return r, bad("Flag %q is not one of rubric %s's gates.", f, rubric.ID)
			}
		}
	}
	if raw := fields["record"]; raw != nil {
		rec, _, err := strictObject(raw)
		sum, ok := str(rec["sha256"])
		if err != nil || !ok || !hex64RE.MatchString(sum) || len(rec) > 2 || (len(rec) == 2 && rec["doc"] == nil) {
			return r, bad("data.review.record is {\"sha256\":HEX} and optionally \"doc\", the 32-hex doc id that holds the full record.")
		}
		r.Record = &Record{SHA256: sum}
		if d := rec["doc"]; d != nil {
			if r.Record.Doc, ok = str(d); !ok || !hex32RE.MatchString(r.Record.Doc) {
				return r, bad("data.review.record.doc is a 32-hex doc id.")
			}
		}
	}
	outside := r.Relationship == "outside_source"
	if (fields["source"] != nil) != outside {
		return r, bad("data.review.source goes with relationship outside_source, and only with it: {name, url, terms} and the source's own score, grade or verdict.")
	}
	if outside {
		src, err := parseSource(fields["source"])
		if err != nil {
			return r, err
		}
		r.Source = src
		if len(r.Evidence) == 0 || !slices.ContainsFunc(r.Evidence, func(id string) bool {
			return strings.HasPrefix(id, "fetch:") || strings.HasPrefix(id, "stamp:")
		}) {
			return r, bad("An outside_source review cites a fetch: or stamp: of the source's page or answer as evidence.")
		}
	}
	return r, nil
}

func parseObservation(raw json.RawMessage, rubric *Rubric, evidence int) (Observation, error) {
	var o Observation
	usage := bad(`An observation is {"check":ID} with "pass" and "fail" counts, a measured "value", or "na":true; "evidence" lists up to %d indexes into data.review.evidence.`, MaxObservationCites)
	f, _, err := strictObject(raw)
	if err != nil {
		return o, usage
	}
	for k := range f {
		if !slices.Contains([]string{"check", "pass", "fail", "value", "na", "evidence"}, k) {
			return o, usage
		}
	}
	var ok bool
	if o.Check, ok = str(f["check"]); f["check"] == nil || !ok || !checkIDRE.MatchString(o.Check) {
		return o, usage
	}
	c := rubric.Check(o.Check)
	if c == nil {
		return o, bad("Check %q is not in rubric %s.", o.Check, rubric.ID)
	}
	counts := f["pass"] != nil || f["fail"] != nil
	forms := 0
	if counts {
		forms++
		p, okp := 0, true
		q, okq := 0, true
		if f["pass"] != nil {
			p, okp = intIn(f["pass"], countRE)
		}
		if f["fail"] != nil {
			q, okq = intIn(f["fail"], countRE)
		}
		if !okp || !okq || p+q == 0 || p > MaxCount || q > MaxCount {
			return o, bad("Observation %s: pass and fail are counts, 0 to %d, not both 0.", o.Check, MaxCount)
		}
		o.Pass, o.Fail = &p, &q
	}
	if f["value"] != nil {
		forms++
		v, ok := number(f["value"], MaxObservationValueAb)
		if !ok {
			return o, bad("Observation %s: value is a finite number in the rubric's unit.", o.Check)
		}
		o.Value = &v
	}
	if f["na"] != nil {
		forms++
		if string(f["na"]) != "true" {
			return o, usage
		}
		o.NA = true
	}
	if forms != 1 {
		return o, usage
	}
	if counts && c.Metric != "rate" || o.Value != nil && c.Metric != "curve" {
		return o, bad("Observation %s: a %s check takes %s.", o.Check, c.Metric, map[string]string{"rate": "pass and fail counts", "curve": "a value"}[c.Metric])
	}
	if raw := f["evidence"]; raw != nil {
		var idx []json.RawMessage
		if raw[0] != '[' || json.Unmarshal(raw, &idx) != nil || len(idx) == 0 || len(idx) > MaxObservationCites {
			return o, usage
		}
		for _, x := range idx {
			n, ok := intIn(x, countRE)
			if !ok || n >= evidence || slices.Contains(o.Cites, n) {
				return o, bad("Observation %s: evidence indexes data.review.evidence (0 to %d), each once.", o.Check, evidence-1)
			}
			o.Cites = append(o.Cites, n)
		}
	}
	return o, nil
}

func parseSource(raw json.RawMessage) (*Source, error) {
	usage := bad(`data.review.source is {"name","url"} with optional "terms" (licence or terms note) and the source's own "score" (number), "grade" and "verdict", kept as given.`)
	f, _, err := strictObject(raw)
	if err != nil {
		return nil, usage
	}
	var src Source
	for k, v := range f {
		var ok bool
		switch k {
		case "name":
			src.Name, ok = str(v)
			ok = ok && cleanText(src.Name, MaxSourceNameBytes)
		case "url":
			src.URL, ok = str(v)
			u, perr := url.Parse(src.URL)
			ok = ok && perr == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && len(src.URL) <= MaxSourceURLBytes && printableASCIIOrSpace(src.URL) && !strings.Contains(src.URL, " ")
		case "terms":
			src.Terms, ok = str(v)
			ok = ok && cleanText(src.Terms, MaxSourceTermsBytes)
		case "score":
			var n float64
			n, ok = number(v, 1e9)
			src.Score = &n
		case "grade":
			src.Grade, ok = str(v)
			ok = ok && cleanText(src.Grade, MaxSourceFigureBytes)
		case "verdict":
			src.Verdict, ok = str(v)
			ok = ok && cleanText(src.Verdict, MaxSourceFigureBytes)
		}
		if !ok {
			return nil, usage
		}
	}
	if src.Name == "" || src.URL == "" || (src.Score == nil && src.Grade == "" && src.Verdict == "") {
		return nil, usage
	}
	return &src, nil
}

// VerdictValue is a verdict's place in the community aggregate: good 1,
// mixed 0.5, bad and unusable 0; unverified is not counted (ok false).
func VerdictValue(v string) (float64, bool) {
	switch v {
	case "good":
		return 1, true
	case "mixed":
		return 0.5, true
	case "bad", "unusable":
		return 0, true
	}
	return 0, false
}
