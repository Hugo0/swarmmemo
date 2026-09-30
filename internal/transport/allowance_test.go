package transport

import (
	"errors"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/web"
)

// RFC0012: on every constrained wire a write's result carries the "free
// today" line after its ok, and the help texts explain the waterfall only
// while the allowance ledger is on. Without a note nothing changes.

var testNote = &board.AllowanceNote{Line: "Free today: 4 MiB of posting (signed tier), 3.9 MiB left, resets 00:00 UTC.", Resource: "post_bytes", Tier: 3}

func TestTextWiresPrintTheAllowanceLineAfterOk(t *testing.T) {
	res := board.Result{OK: true, Receipt: &board.Receipt{ID: "memo1", Hash: "h"}}
	if strings.Contains(Text(res, 4096), "Free today") {
		t.Fatal("line without a note")
	}
	res.Allowance = testNote
	lines := strings.Split(Text(res, 4096), "\n")
	if !strings.HasPrefix(lines[0], "ok memo1") || lines[1] != testNote.Line {
		t.Fatalf("text wire: %q", lines)
	}
	if out := string((lineProtocol{}).Render(Request{Budget: 4096}, res, nil)); !strings.Contains(out, "ok memo1") || !strings.Contains(out, testNote.Line) {
		t.Fatalf("tcp: %q", out)
	}
}

func TestSMTPReplyCarriesTheAllowanceLine(t *testing.T) {
	s := &smtp{host: "swarmmemo.com"}
	res := board.Result{OK: true, Receipt: &board.Receipt{ID: "memo1"}}
	if got := s.reply(&board.Command{Operation: "post"}, res, nil); got != "250 2.0.0 ok memo1 https://swarmmemo.com/e/memo1\r\n" {
		t.Fatalf("reply without a note changed: %q", got)
	}
	res.Allowance = testNote
	if got := s.reply(&board.Command{Operation: "post"}, res, nil); got != "250-2.0.0 ok memo1 https://swarmmemo.com/e/memo1\r\n250 2.0.0 "+testNote.Line+"\r\n" {
		t.Fatalf("multiline reply: %q", got)
	}
}

func TestDNSWriteAnswerAndUsageExplainTheAllowance(t *testing.T) {
	d := testDNS(t)
	d.writes = newReassembly()
	req, err := d.Parse(dnsQueryBytes("head.q.swarmmemo.com", dnsTypeTXT))
	if err != nil {
		t.Fatal(err)
	}
	req.Route, req.Arg, req.Budget = "write-done", "abc", 1232
	res := board.Result{OK: true, Receipt: &board.Receipt{ID: "memo1"}, Allowance: testNote}
	if out := string(d.Render(req, res, nil)); !strings.Contains(out, "ok memo1") || !strings.Contains(out, testNote.Line) {
		t.Fatalf("write answer lacks the line: %q", out)
	}
	if out := string(d.Render(req, board.Result{}, errors.New("x"))); strings.Contains(out, "Free today") {
		t.Fatal("line on a failed write")
	}
	usage := func() string {
		return string(answerOnce(t, d, dnsQueryBytes("q.swarmmemo.com", dnsTypeTXT), board.Result{}, nil, 1232))
	}
	if strings.Contains(usage(), web.WaterfallSentence) {
		t.Fatal("usage explains the allowance while the ledger is off")
	}
	d.allowance = true
	if !strings.Contains(usage(), web.WaterfallSentence) {
		t.Fatal("usage lacks the waterfall sentence")
	}
}

func TestLineHelpExplainsTheAllowanceOnlyWhenOn(t *testing.T) {
	help := Request{Route: "help"}
	if string((lineProtocol{}).Render(help, board.Result{}, nil)) != lineHelp {
		t.Fatal("HELP changed while the ledger is off")
	}
	out := string((lineProtocol{allowance: true}).Render(help, board.Result{}, nil))
	if !strings.HasPrefix(out, lineHelp) || !strings.Contains(out, web.WaterfallSentence) || !strings.Contains(out, `the line after "ok"`) {
		t.Fatalf("HELP with the ledger on: %q", out)
	}
}
