package services

import (
	"slices"
	"strings"

	"swarmmemo/internal/allowance"
)

// RefusalDatasetUnavailable is a public_data refusal that retrying cannot
// lift (C1035): the dataset needs an upstream API key this server does not
// have. It is not upstream_unavailable (503, Retry-After), which is for a
// source that may answer later; callers retried a missing key forever.
const RefusalDatasetUnavailable = "dataset_unavailable"

// pdKeyMissingReason is the item error reason of a missing upstream key.
const pdKeyMissingReason = "key_missing"

// PublicDataUnavailableReason is the catalogue's unavailable_reason of a
// dataset whose key file is absent: the same check (available) that refuses
// its fetches with dataset_unavailable.
const PublicDataUnavailableReason = "no upstream API key on this server for this source, so it cannot be fetched here; not a temporary outage, retrying will not help"

// pdSimilarMax is how many working datasets a dataset_unavailable names.
const pdSimilarMax = 3

func pdKeyMissing() *pdErr { return pdFail(RefusalDatasetUnavailable, pdKeyMissingReason) }

// similarAvailable is up to n datasets other than ds that can be fetched
// now, most similar first: a shared upstream host counts most, then shared
// words of the titles and descriptions; ties keep catalogue order.
func (p *publicData) similarAvailable(ds *pdDataset, n int) []string {
	words := func(d *pdDataset) map[string]bool {
		out := map[string]bool{}
		for _, w := range strings.FieldsFunc(strings.ToLower(d.Title+" "+d.Description+" "+d.Source), func(r rune) bool { return r < 'a' || r > 'z' }) {
			if len(w) >= 5 {
				out[w] = true
			}
		}
		return out
	}
	mine := words(ds)
	type cand struct {
		id    string
		score int
	}
	var list []cand
	for _, o := range pdCatalogue {
		if o == ds || !p.available(o) {
			continue
		}
		score := 0
		for _, h := range o.Hosts {
			if slices.Contains(ds.Hosts, h) {
				score += 100
			}
		}
		for w := range words(o) {
			if mine[w] {
				score++
			}
		}
		list = append(list, cand{o.ID, score})
	}
	slices.SortStableFunc(list, func(a, b cand) int { return b.score - a.score })
	out := []string{}
	for _, c := range list[:min(n, len(list))] {
		out = append(out, c.id)
	}
	return out
}

// datasetUnavailable is the refusal of a fetch that needs ds's upstream key
// when its key file is absent: it names the dataset and the working
// datasets most like it, and carries no retry hint.
func (p *publicData) datasetUnavailable(ds *pdDataset) error {
	alt := p.similarAvailable(ds, pdSimilarMax)
	what := "Dataset " + ds.ID + " is unavailable on this server: it has no upstream API key for " + ds.Source + "."
	if p.available(ds) { // the key is optional: only these params need it
		what = "Dataset " + ds.ID + " answers these params only with an upstream API key for " + ds.Source + ", which this server does not have."
	}
	msg := what + " This is not a temporary outage, so retrying will not help; nothing was charged."
	if len(alt) > 0 {
		msg += " Similar datasets that work: " + strings.Join(alt, ", ") + "."
	}
	return &allowance.Err{Code: RefusalDatasetUnavailable, Message: msg,
		Details: map[string]any{"dataset": ds.ID, "reason": pdKeyMissingReason, "similar_available": alt}}
}
