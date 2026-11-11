package web

import (
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

// A reply its task's requester marked off-task (C903) is drawn collapsed in
// the thread: one line saying who marked it, the text a click away, never
// removed; other replies are drawn as before.
func TestOffTaskReplyIsCollapsed(t *testing.T) {
	now := time.Now().Unix()
	pitch := treeMessage("pitch", "root", 2, now-50, 0)
	pitch.Text = "5 USDC off-board, DM me"
	pitch.OffTask = &board.MessageOffTask{By: "requester", At: now}
	events := []board.Message{treeMessage("root", "", 1, now-100, 0), pitch, treeMessage("answer", "root", 3, now-10, 0)}
	s := &testService{execute: func(c board.Command) (board.Result, error) {
		return board.Result{OK: true, Messages: events, Data: map[string]any{"root_id": "root"}}, nil
	}}
	body := render(s, "/e/root").Body.String()
	at := strings.Index(body, `id="e-pitch"`)
	if at < 0 {
		t.Fatal("the off-task reply is missing")
	}
	reply := body[at:]
	reply = reply[:strings.Index(reply, "</article>")]
	if !strings.Contains(reply, `<details class="memo-off-task"><summary`) || !strings.Contains(reply, "Marked off-task by the requester · show</summary>") ||
		!strings.Contains(reply, "5 USDC off-board, DM me") || strings.Contains(reply, `class="removed"`) {
		t.Fatalf("off-task reply not collapsed:\n%s", reply)
	}
	if strings.Count(body, "memo-off-task") != 1 {
		t.Fatal("only the marked reply is collapsed")
	}
}
