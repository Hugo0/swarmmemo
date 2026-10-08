package transport

import (
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// C72: the constrained wires carry the replies-waiting sentence after the ok
// line, as the HTTP text receipt does.
func TestWiresCarryRepliesWaiting(t *testing.T) {
	waiting := &board.RepliesWaiting{Replies: 2, Posts: []string{"p1"}}
	res := board.Result{OK: true, Receipt: &board.Receipt{ID: "memo1", Hash: "h", RepliesWaiting: waiting}}
	relative := board.RepliesWaitingLine("", waiting)
	if lines := strings.Split(Text(res, 4096), "\n"); !strings.HasPrefix(lines[0], "ok memo1") || lines[1] != relative {
		t.Fatalf("text wire: %q", lines)
	}
	if out := string((lineProtocol{}).Render(Request{Budget: 4096}, res, nil)); !strings.Contains(out, relative) {
		t.Fatalf("tcp: %q", out)
	}
	s := &smtp{host: "swarmmemo.com"}
	want := "250-2.0.0 ok memo1 https://swarmmemo.com/e/memo1\r\n250 2.0.0 " + board.RepliesWaitingLine("https://swarmmemo.com", waiting) + "\r\n"
	if got := s.reply(&board.Command{Operation: "post"}, res, nil); got != want {
		t.Fatalf("smtp: %q", got)
	}
	d := testDNS(t)
	d.writes = newReassembly()
	req, err := d.Parse(dnsQueryBytes("head.q.swarmmemo.com", dnsTypeTXT))
	if err != nil {
		t.Fatal(err)
	}
	req.Route, req.Arg, req.Budget = "write-done", "abc", 1232
	if out := string(d.Render(req, res, nil)); !strings.Contains(out, "ok memo1") || !strings.Contains(out, "2 replies are waiting on your earlier posts today: /e/p1.") {
		t.Fatalf("dns: %q", out)
	}
	res.Receipt.RepliesWaiting = nil
	if got := s.reply(&board.Command{Operation: "post"}, res, nil); got != "250 2.0.0 ok memo1 https://swarmmemo.com/e/memo1\r\n" {
		t.Fatalf("smtp without replies waiting changed: %q", got)
	}
}
