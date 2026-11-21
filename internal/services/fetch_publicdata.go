package services

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"

	"swarmmemo/internal/allowance"
)

// A fetch the site itself refuses (robots.txt, a bot challenge, 401/403,
// 429) on a site a public_data dataset reads names that dataset and the
// call that reads it (C1147): the refusal stays a refusal, and the caller
// learns the data has another, sanctioned road.

// siteRefusalCode is the refusals where the site itself keeps
// fetch out: they carry the public_data hint (public_data reads the API)
// and a public refused-call record (C1157, refused.go).
func siteRefusalCode(code string) bool {
	switch code {
	case "fetch_robots", "fetch_captcha", "fetch_blocked", "fetch_site_rate_limited":
		return true
	}
	return false
}

// pdSite is the site a dataset host belongs to: the host less its first
// label when it has three or more (api.congress.gov is on congress.gov,
// api.coingecko.com on coingecko.com), else the host itself.
func pdSite(host string) string {
	if strings.Count(host, ".") >= 2 {
		return host[strings.IndexByte(host, '.')+1:]
	}
	return host
}

// pdSiteDatasets is the one map from a host to the public_data datasets
// that read it, derived from each dataset's Hosts: every declared host, then
// every declared host's site, each to its datasets in catalogue order.
var pdSiteDatasets = sync.OnceValue(func() map[string][]string {
	m := map[string][]string{}
	add := func(key, id string) {
		for _, have := range m[key] {
			if have == id {
				return
			}
		}
		m[key] = append(m[key], id)
	}
	for _, ds := range pdCatalogue {
		for _, h := range ds.Hosts {
			add(strings.ToLower(h), ds.ID)
		}
	}
	exact := map[string]bool{}
	for k := range m {
		exact[k] = true
	}
	for _, ds := range pdCatalogue {
		for _, h := range ds.Hosts {
			if site := pdSite(strings.ToLower(h)); !exact[site] {
				add(site, ds.ID)
			}
		}
	}
	return m
})

// PublicDataDatasetsForHost is the ids of the public_data datasets that read
// host or the site it is on (case-insensitive; the most specific match):
// www.congress.gov names congress_bills, which reads api.congress.gov.
func PublicDataDatasetsForHost(host string) []string {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	m := pdSiteDatasets()
	for strings.Contains(h, ".") {
		if ids, ok := m[h]; ok {
			return ids
		}
		h = h[strings.IndexByte(h, '.')+1:]
	}
	return nil
}

// publicDataCall is the call that reads ds: public_data.fetch with the
// parameters that have an example set to it, which validates as sent
// (TestPublicDataCallsValidate).
func publicDataCall(ds *pdDataset) string {
	params := map[string]string{}
	for _, p := range ds.Params {
		if p.Example != "" {
			params[p.Name] = p.Example
		}
	}
	b, _ := json.Marshal(map[string]any{"dataset": ds.ID, "params": params})
	return "public_data.fetch " + string(b)
}

// withPublicDataHint adds details.public_data {dataset, call} to a site
// refusal (siteRefusalCode) when a dataset reads host's site: the first
// in catalogue order. Anything else passes through unchanged.
func withPublicDataHint(err error, host string) error {
	var ae *allowance.Err
	if !errors.As(err, &ae) || !siteRefusalCode(ae.Code) {
		return err
	}
	ids := PublicDataDatasetsForHost(host)
	if len(ids) == 0 {
		return err
	}
	d, ok := ae.Details.(map[string]any)
	switch {
	case ae.Details == nil:
		d = map[string]any{}
	case !ok:
		return err
	}
	out := *ae
	nd := make(map[string]any, len(d)+1)
	for k, v := range d {
		nd[k] = v
	}
	nd["public_data"] = map[string]any{"dataset": ids[0], "call": publicDataCall(pdByID[ids[0]])}
	out.Details = nd
	return &out
}
