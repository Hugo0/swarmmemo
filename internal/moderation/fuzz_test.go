package moderation

import (
	"strings"
	"testing"
)

// FuzzParsePolicy: the policy parser never panics, and whatever it accepts
// is within bounds, uses only actions its surfaces have, and survives a
// round trip through its canonical form.
func FuzzParsePolicy(f *testing.F) {
	f.Add([]byte(`{"schema":1,"version":1}`))
	f.Add(DefaultPolicy().Canonical())
	f.Add([]byte(`{"schema":1,"version":2,"surfaces":{"post":{"classifiers":["jev"],"on_unavailable":"flag","burst":{"window_seconds":3600,"max":5,"mode":"downgrade"},"categories":{"phishing":{"thresholds":[{"at":0.9,"action":"hide"},{"at":0.6,"action":"flag"}]}}}}}`))
	f.Add([]byte(`{"schema":1,"version":3,"egress":{"deny_cidrs":["203.0.113.0/24","2001:db8::/32"],"mining_ports":[3333]},"surfaces":{"run.egress":{"classifiers":["egress","rate"],"on_unavailable":"block","rate":{"window_seconds":60,"max":10},"categories":{}}}}`))
	f.Add([]byte(`{"schema":1,"version":4,"surfaces":{"run.code":{"classifiers":["rules"],"on_unavailable":"hold","rules":[{"id":"a","category":"mining","regex":"(?i)xmrig"}],"categories":{"mining":{"thresholds":[{"at":1,"action":"block"}]}}}}}`))
	f.Add([]byte(`{"schema":1,"version":1,"version":2}`))
	f.Add([]byte(`{"schema":1,"version":1,"surfaces":{"post":{"rules":[{"id":"x","category":"y","regex":"(a{1000}){1000}"}]}}}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		p, err := ParsePolicy(body)
		if err != nil {
			return
		}
		if p.Version < 1 || p.Schema != 1 {
			t.Fatalf("accepted version %d schema %d", p.Version, p.Schema)
		}
		for s, sp := range p.Surfaces {
			spec, ok := surfaceSpec(s)
			if !ok {
				t.Fatalf("unknown surface %q accepted", s)
			}
			if !spec.allows(sp.OnUnavailable) || sp.OnUnavailable == Allow {
				t.Fatalf("%s: on_unavailable %q", s, sp.OnUnavailable)
			}
			for name, cp := range sp.Categories {
				for _, th := range cp.Thresholds {
					if !spec.allows(th.Action) || th.At <= 0 || th.At > 1 {
						t.Fatalf("%s/%s: %+v", s, name, th)
					}
				}
			}
			for _, r := range sp.Rules {
				if r.re == nil {
					t.Fatalf("%s: rule %s not compiled", s, r.ID)
				}
				r.re.MatchString(strings.Repeat("a", 64))
			}
		}
		again, err := ParsePolicy(p.Canonical())
		if err != nil {
			t.Fatalf("canonical form refused: %v", err)
		}
		if string(again.Canonical()) != string(p.Canonical()) {
			t.Fatal("canonical form is not stable")
		}
	})
}
