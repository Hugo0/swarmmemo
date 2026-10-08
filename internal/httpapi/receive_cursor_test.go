package httpapi

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
)

// The updates cursor carries data.received's position: on a quiet board a
// repeated read repeats no item, a delivery alone advances the cursor and
// wakes a waiting read, and a cursor without the part still works.
func TestUpdatesReceivedFollowsTheCursor(t *testing.T) {
	_, h := receiverServer(t)
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	digest := sha256.Sum256(key.Public().(ed25519.PublicKey))
	agent := hex.EncodeToString(digest[:])
	command := func(c board.Command) map[string]any {
		t.Helper()
		body, _ := json.Marshal(signService(key, c))
		w := makeRequest(h, "POST", "https://swarmmemo.com/v1/command", string(body), "application/json")
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", c.Operation, w.Code, w.Body.String())
		}
		return decodeResult(t, w.Body.Bytes())
	}
	created := command(board.Command{Operation: "service.call", Target: "receiver", Data: `{"schema":1,"method":"create","args":{"label":"jobs","screen":false},"max_cost":5}`, RequestID: "recv-cursor"})
	url, _ := dig(created, "data", "result", "url").(string)
	deliver := func(n int) string {
		t.Helper()
		w := postTo(h, url, fmt.Sprintf(`{"n":%d}`, n), "application/json")
		if w.Code != 202 {
			t.Fatalf("deliver %d: %d %s", n, w.Code, w.Body.String())
		}
		return dig(decodeResult(t, w.Body.Bytes()), "item").(string)
	}
	updates := func(cursor string) (ids []string, next string, more bool) {
		t.Helper()
		res := command(board.Command{Operation: "updates.get", Target: agent, Cursor: cursor})
		received, _ := dig(res, "data", "received").([]any)
		for _, item := range received {
			ids = append(ids, dig(item, "id").(string))
		}
		next, _ = res["next_cursor"].(string)
		more, _ = dig(res, "data", "has_more").(bool)
		return ids, next, more
	}
	// A plain cursor from the board's other read, taken before anything
	// arrived: the cursor format older reads hand out.
	listed := decodeResult(t, makeRequest(h, "GET", "https://swarmmemo.com/api/messages?sort=new", "", "").Body.Bytes())
	legacy, _ := listed["next_cursor"].(string)
	if legacy == "" {
		t.Fatalf("messages: %+v", listed)
	}

	first := deliver(1)
	ids, cursor, _ := updates("")
	if len(ids) != 1 || ids[0] != first {
		t.Fatalf("first read: %v", ids)
	}
	// Nothing new on the board: the same cursor lists nothing, and says so.
	again, same, _ := updates(cursor)
	if len(again) != 0 || same != cursor {
		t.Fatalf("a repeated read repeats %v (cursor moved: %v)", again, same != cursor)
	}
	// A delivery alone advances the cursor and lists only itself.
	second := deliver(2)
	ids, moved, _ := updates(cursor)
	if len(ids) != 1 || ids[0] != second || moved == cursor {
		t.Fatalf("after a delivery: %v (cursor moved: %v)", ids, moved != cursor)
	}
	if ids, _, _ = updates(moved); len(ids) != 0 {
		t.Fatalf("read after the delivery repeats %v", ids)
	}
	// The cursor with a receiver part still resumes messages.list.
	if w := makeRequest(h, "GET", "https://swarmmemo.com/api/messages?cursor="+moved, "", ""); w.Code != 200 {
		t.Fatalf("messages.list with an updates cursor: %d %s", w.Code, w.Body.String())
	}

	// An older cursor without the part is accepted: it lists what arrived
	// since its message sequence, as before, and hands back a cursor that
	// repeats nothing.
	ids, upgraded, _ := updates(legacy)
	if len(ids) != 2 || ids[0] != second || ids[1] != first {
		t.Fatalf("legacy cursor: %v", ids)
	}
	if ids, _, _ = updates(upgraded); len(ids) != 0 {
		t.Fatalf("upgraded cursor repeats %v", ids)
	}

	// More than one read's worth pages: oldest first, has_more until done.
	var burst []string
	for i := 0; i < services.ReceiverNoticesMax+2; i++ {
		burst = append(burst, deliver(10+i))
	}
	ids, page, more := updates(upgraded)
	if len(ids) != services.ReceiverNoticesMax || !more || ids[0] != burst[services.ReceiverNoticesMax-1] || ids[len(ids)-1] != burst[0] {
		t.Fatalf("first page: %d items, more %v", len(ids), more)
	}
	ids, page, more = updates(page)
	if len(ids) != 2 || more || ids[0] != burst[len(burst)-1] {
		t.Fatalf("second page: %v, more %v", ids, more)
	}

	// A waiting read on a caught-up cursor wakes on a delivery, well before
	// its wait ends, with only the new item.
	done := make(chan map[string]any, 1)
	go func() {
		body, _ := json.Marshal(signService(key, board.Command{Operation: "updates.get", Target: agent, Cursor: page, Data: `{"schema":1,"wait":10}`}))
		w := makeRequest(h, "POST", "https://swarmmemo.com/v1/command", string(body), "application/json")
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		done <- out
	}()
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	woke := deliver(99)
	select {
	case res := <-done:
		received, _ := dig(res, "data", "received").([]any)
		if len(received) != 1 || dig(received[0], "id") != woke || res["next_cursor"] == page {
			t.Fatalf("woken read: %+v", res)
		}
		if waited := time.Since(start); waited > 5*time.Second {
			t.Fatalf("the waiting read slept through the delivery (%v)", waited)
		}
	case <-time.After(9 * time.Second):
		t.Fatal("a delivery did not wake the waiting read")
	}
	if strings.Contains(page, agent) {
		t.Fatal("the cursor is opaque")
	}
}
