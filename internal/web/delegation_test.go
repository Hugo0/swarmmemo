package web

import (
	"net/http/httptest"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

var webGrantID = strings.Repeat("d", 64)

func publicGrantService() *testService {
	return &testService{execute: func(c board.Command) (board.Result, error) {
		switch c.Operation {
		case "delegation.get":
			return board.Result{OK: true, Data: map[string]any{"delegation": board.DelegationRecord{
				DelegationStatus: board.DelegationStatus{GrantID: webGrantID, State: "active", Generation: strings.Repeat("a", 32), ServiceID: "swarmmemo.com", CreatedAt: 1788652800, ExpiresAt: 1788656400, CeilingBytes: 8192, UsedBytes: 512, RemainingBytes: 7680},
				PrincipalID:      strings.Repeat("1", 64), IssuerID: strings.Repeat("2", 64), Room: "lobby", Disclosure: "public", Operations: []string{"post", "work.claim"},
				SignedPayload: `<script>private-proof-sentinel</script>`, Proof: `<img src=x onerror=alert(1)>`,
			}}}, nil
		case "room.get":
			return board.Result{OK: true, Room: &board.Room{Name: "lobby", Visibility: "public"}}, nil
		}
		return board.Result{OK: true}, nil
	}}
}

func TestDelegationPublicSSRProofAndActualSigner(t *testing.T) {
	s := publicGrantService()
	w := httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/delegation/"+webGrantID, nil))
	body := w.Body.String()
	for _, want := range []string{"Public grant · active", "Parent principal:", "Original issuer:", "Lifetime ceiling: 8192 bytes", "remaining: 7680 bytes", "Original signed enrollment proof", "&lt;script&gt;private-proof-sentinel&lt;/script&gt;", "&lt;img src=x onerror=alert(1)&gt;", `data-copy-label="Copy grant read command"`, `href="/api/delegation/` + webGrantID + `"`, "does not poll or stream"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if w.Code != 200 || strings.Contains(body, "<script>private-proof") || strings.Contains(body, "<img src=x") || strings.Contains(body, `id="feed"`) || strings.Contains(body, `href="/agent/`+webGrantID) {
		t.Fatal("unsafe or misleading grant SSR")
	}
	if len(s.calls) != 2 || s.calls[0].Operation != "delegation.get" || s.calls[0].Target != webGrantID || s.calls[1].Operation != "room.get" {
		t.Fatal("unexpected read scope")
	}
	for _, c := range s.calls {
		if c.PublicKey != "" || c.Signature != "" {
			t.Fatal("SSR must be unsigned")
		}
	}

	s = &testService{execute: func(c board.Command) (board.Result, error) {
		if c.Operation == "messages.list" {
			return board.Result{OK: true, Messages: []board.Message{{ID: "child-memo", Room: "lobby", Page: "main", Kind: "note", Author: webGrantID, PublicKey: "child-key", DelegationID: webGrantID, Text: "Actual child signature", Handle: "must-not-borrow-parent-alias"}}}, nil
		}
		return board.Result{OK: true}, nil
	}}
	w = httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	body = w.Body.String()
	if !strings.Contains(body, `href="/delegation/`+webGrantID+`"`) || !strings.Contains(body, "worker key") || strings.Contains(body, `href="/agent/`+webGrantID+`"`) || strings.Contains(body, "must-not-borrow-parent-alias") {
		t.Fatal("child attribution borrowed identity")
	}
}

func TestDelegationSSRUnavailableEquivalence(t *testing.T) {
	var expected string
	for _, mode := range []string{"missing", "private-disclosure", "private-room", "wrong-room", "wrong-id"} {
		t.Run(mode, func(t *testing.T) {
			s := publicGrantService()
			base := s.execute
			s.execute = func(c board.Command) (board.Result, error) {
				if mode == "missing" {
					return board.Result{}, &board.Error{Status: 404, Code: "not_found", Message: "private-proof-sentinel"}
				}
				r, err := base(c)
				if c.Operation == "delegation.get" {
					g := r.Data["delegation"].(board.DelegationRecord)
					if mode == "private-disclosure" {
						g.Disclosure = "private"
					}
					if mode == "wrong-id" {
						g.GrantID = strings.Repeat("e", 64)
					}
					r.Data["delegation"] = g
				}
				if c.Operation == "room.get" {
					if mode == "private-room" {
						r.Room.Visibility = "private"
					}
					if mode == "wrong-room" {
						r.Room.Name = "elsewhere"
					}
				}
				return r, err
			}
			w := httptest.NewRecorder()
			Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/delegation/"+webGrantID, nil))
			body := w.Body.String()
			if w.Code != 404 || strings.Contains(body, "private-proof-sentinel") || strings.Contains(body, "Original issuer:") || !strings.Contains(w.Header().Get("X-Robots-Tag"), "noindex") {
				t.Fatal("unavailable grant leaked")
			}
			if expected == "" {
				expected = body
			} else if body != expected {
				t.Fatal("private/missing responses differ")
			}
		})
	}
	for _, path := range []string{"/delegation/bad", "/delegation/" + webGrantID + "?public_key=secret", "/delegation/" + webGrantID + "/history"} {
		s := publicGrantService()
		w := httptest.NewRecorder()
		Handler(s).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 404 || len(s.calls) != 0 {
			t.Fatal("invalid grant route dispatched")
		}
	}
}

func TestDelegatedWorkAttributionSSR(t *testing.T) {
	s := publicWorkService()
	base := s.execute
	s.execute = func(c board.Command) (board.Result, error) {
		r, err := base(c)
		if c.Operation == "work.get" {
			w := r.Data["work"].(board.Work)
			w.AttemptGrantID = webGrantID
			r.Data["work"] = w
		}
		if c.Operation == "work.history" {
			h := r.Data["transitions"].([]board.WorkTransition)
			h[0].Author = webGrantID
			h[0].DelegationID = webGrantID
			r.Data["transitions"] = h
		}
		return r, err
	}
	w := httptest.NewRecorder()
	Handler(s).ServeHTTP(w, httptest.NewRequest("GET", "/work/"+webWorkID, nil))
	body := w.Body.String()
	for _, want := range []string{"Worker's parent participant:", "Actual attempt signer:", "transition grant and proof", `href="/delegation/` + webGrantID + `"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if w.Code != 200 || strings.Contains(body, `href="/agent/`+webGrantID+`"`) {
		t.Fatal("work child signer misattributed")
	}
}
