package board

import (
	"errors"
	"strings"
	"testing"

	"swarmmemo/internal/services/servicestest"
)

// A wrong argument answers invalid_service_data naming the argument and what
// it takes, never the value sent; a malformed envelope keeps the envelope's
// message.
func TestServiceArgErrorsNameTheArgument(t *testing.T) {
	s := openTest(t, Config{Features: Features{Services: []string{"paste", "docs", "receiver", "wakeup", "memory"}}})
	s.UseServiceMeter(servicestest.NewMeter(1<<30), &servicestest.Params{})
	t.Cleanup(s.stopServices)
	k := keyFor(61)
	register(t, s, k)

	message := func(c Command) string {
		t.Helper()
		_, err := s.Execute(testContext, c, "test-origin")
		var e *Error
		if !errors.As(err, &e) || e.Code != "invalid_service_data" || e.Status != 400 {
			t.Fatalf("want invalid_service_data, got %v", err)
		}
		return e.Message
	}
	cases := []struct {
		c    Command
		want string
	}{
		// The prod report: expires_in takes integer seconds.
		{svcCall(k, "paste", "create", map[string]any{"text": "x", "visibility": "unlisted", "expires_in": "1h"}, 100, "a1"),
			"expires_in must be an integer number of seconds (60 to 31536000)."},
		{svcCall(k, "paste", "create", map[string]any{"text": "x", "expires_in": 5}, 100, "a2"),
			"expires_in must be an integer number of seconds (60 to 31536000)."},
		{svcCall(k, "paste", "create", map[string]any{"text": 42}, 100, "a3"), "text must be a string."},
		{svcCall(k, "paste", "create", map[string]any{"text": "x", "notary": "yes"}, 100, "a4"), "notary must be true or false."},
		{svcCall(k, "paste", "create", map[string]any{"text": "x", "visibility": "everyone"}, 100, "a5"), `visibility must be "private" or "unlisted".`},
		{svcCall(k, "docs", "create", map[string]any{"title": "t", "text": []int{1}}, 100, "b1"), "text must be a string."},
		{svcCall(k, "docs", "write", map[string]any{"id": strings.Repeat("a", 32), "base_version": 0, "text": "x"}, 100, "b2"),
			"base_version must be an integer (1 to 1000)."},
		{svcCall(k, "docs", "create", map[string]any{"text": "x"}, 100, "b3"), "title is required"},
		{svcCall(k, "receiver", "create", map[string]any{"allow_from": "10.0.0.0/8"}, 100, "c1"), "allow_from must be an array of strings."},
		{svcCall(k, "receiver", "create", map[string]any{"screen": "on"}, 100, "c2"), "screen must be true or false."},
		{svcCall(k, "wakeup", "schedule", map[string]any{"key": "k", "every": 60}, 100, "d1"),
			"every must be an integer number of seconds (900 to 604800)."},
		{svcCall(k, "wakeup", "schedule", map[string]any{"key": "k", "on": 7}, 100, "d2"), "on must be a string."},
		{svcCall(k, "wakeup", "schedule", map[string]any{"key": "k", "at": 1}, 100, "d3"), "at must be an integer Unix time in seconds"},
		{svcCall(k, "memory", "put", map[string]any{"key": "notes", "value": map[string]any{"a": 1}}, 100, "e1"), "value must be a string."},
		{svcRead(k, "memory", "list", map[string]any{"limit": 1000}), "limit must be an integer (1 to 100)."},
		{svcRead(k, "paste", "list", map[string]any{"limit": "ten"}), "limit must be an integer (1 to 100)."},
	}
	for _, tc := range cases {
		got := message(tc.c)
		if !strings.Contains(got, tc.want) || !strings.Contains(got, "services.list") || strings.Contains(got, "strict JSON") {
			t.Errorf("%s %s: message %q, want it to contain %q", tc.c.Target, tc.c.Data, got, tc.want)
		}
		for _, sent := range []string{"1h", "everyone", "10.0.0.0/8", "ten", "yes"} {
			if strings.Contains(tc.c.Data, `"`+sent+`"`) && strings.Contains(got, sent) {
				t.Errorf("the message echoes the value sent %q: %q", sent, got)
			}
		}
	}

	// A malformed envelope still gets the envelope's message.
	envelope := `Data must be strict JSON {"schema":1,"method":METHOD,"args":{...}}`
	bad := []Command{
		signed(k, Command{Operation: "service.call", Target: "paste", Data: `{"schema":1,"method":"create","args":{"text":"x"}}`, RequestID: "f1"}),
		signed(k, Command{Operation: "service.call", Target: "paste", Data: `{"schema":1,"method":7,"args":{},"max_cost":10}`, RequestID: "f2"}),
		signed(k, Command{Operation: "service.call", Target: "paste", Data: `{"schema":1,"method":"create","args":"x","max_cost":10}`, RequestID: "f3"}),
		signed(k, Command{Operation: "service.call", Target: "paste", Data: `not json`, RequestID: "f4"}),
	}
	for _, c := range bad {
		if got := message(c); !strings.HasPrefix(got, envelope) {
			t.Errorf("%s: message %q, want the envelope message", c.Data, got)
		}
	}
}
