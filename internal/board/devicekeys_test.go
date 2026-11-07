package board

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func surfaceCtx(family, via, ua string) context.Context {
	return WithRequestSignals(WithVia(WithClientFamily(testContext, family), via), RequestSignals{UserAgent: ua})
}

func TestDeviceKeySurfaceOf(t *testing.T) {
	for _, c := range []struct{ family, via, ua, want string }{
		{"browsers", "ui", "Mozilla/5.0 (X11; Linux x86_64) Chrome/140", "web"},
		{"scripts", "command", "swarmmemo.py (+https://swarmmemo.com)", "python"},
		{"scripts", "get", "swarmmemo.py (+https://swarmmemo.com) stamp-fetch", "python"},
		{"scripts", "command", "swarmmemo.mjs (+https://swarmmemo.com)", "javascript"},
		{"browsers", "command", "Mozilla/5.0 (Macintosh) Safari/605", "other_site"},
		{"scripts", "command", "python-urllib/3.12", ""},
		{"scripts", "command", "notswarmmemo.python", ""},
		{"claude", "mcp", "claude-code/2.1", ""},
		{"browsers", "get", "Mozilla/5.0", ""},
	} {
		if got := deviceKeySurface(surfaceCtx(c.family, c.via, c.ua)); got != c.want {
			t.Errorf("%s %s %q: %q, want %q", c.family, c.via, c.ua, got, c.want)
		}
	}
}

// C546: new keys and returning keys by the surface that sent the write, in
// the arrivals counters: signed writes only, published for closed days from
// ClientCountMinimum, and no other key shape accepted.
func TestDeviceKeysCountedBySurface(t *testing.T) {
	s := openTest(t, Config{})
	web := surfaceCtx("browsers", "ui", "Mozilla/5.0 Chrome/140")
	py := surfaceCtx("scripts", "command", "swarmmemo.py (+https://swarmmemo.com)")
	exec := func(ctx context.Context, c Command, source string) {
		t.Helper()
		if _, err := s.Execute(ctx, c, source); err != nil {
			t.Fatalf("%s: %v", c.Operation, err)
		}
	}
	surface := func(name string) map[string]int64 {
		t.Helper()
		s.FlushClientCounts()
		_, raw, err := s.readClientCounts(testContext, time.Unix(testTime, 0), 1)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]int64{}
		for key, n := range raw[0] {
			if m, ok := strings.CutPrefix(key, "surface:"+name+":"); ok {
				out[m] = n
			}
		}
		return out
	}
	// A key that last wrote from the Python client a day ago comes back.
	back := keyFor(91)
	s.now = func() time.Time { return time.Unix(testTime-86400+60, 0) }
	exec(py, signed(back, Command{Operation: "post", Room: "lobby", Text: "yesterday", Timestamp: testTime - 86400 + 60}), "203.0.113.20")
	s.now = func() time.Time { return time.Unix(testTime, 0) }
	exec(py, signed(back, Command{Operation: "post", Room: "lobby", Text: "back today"}), "203.0.113.20")
	for i := range ClientCountMinimum {
		exec(web, signed(keyFor(byte(92+i)), Command{Operation: "post", Room: "lobby", Text: fmt.Sprintf("site %c", 'a'+i)}), "203.0.113.21")
	}
	// Anonymous posts are no key's.
	exec(web, Command{Operation: "post", Room: "lobby", Text: "anonymous from the site"}, "198.51.100.30")
	if got := surface("web"); got["new_keys"] != int64(ClientCountMinimum) || len(got) != 1 {
		t.Fatalf("web: %v", got)
	}
	if got := surface("python"); got["returning_1d"] != 1 || got["new_keys"] != 0 {
		t.Fatalf("python: %v", got)
	}
	// Today is not published; once the day is closed, counts from the
	// minimum are, and smaller ones are left out.
	days, err := s.ReadClientStats(testContext, time.Unix(testTime, 0), 1)
	if err != nil || len(days[0].Surfaces) != 0 {
		t.Fatalf("today's surfaces published: %+v %v", days, err)
	}
	s.now = func() time.Time { return time.Unix(testTime+86400, 0) }
	days, err = s.ReadClientStats(testContext, time.Unix(testTime, 0), 1)
	if err != nil || days[0].Surfaces["web"]["new_keys"] != int64(ClientCountMinimum) || days[0].Surfaces["python"] != nil {
		t.Fatalf("closed day: %+v %v", days[0].Surfaces, err)
	}
	for _, bad := range []map[string]int64{{"surface:martian:new_keys": 1}, {"surface:web:first_posts": 1}, {"surface:web": 1}, {"surface:web:new_keys:x": 1}} {
		if s.AddClientCounts(testContext, testDay, bad) == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}
