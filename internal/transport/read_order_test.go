package transport

import (
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

func TestTextPreservesNewestOrder(t *testing.T) {
	res := board.Result{OK: true, Messages: []board.Message{
		{ID: "newest", Text: "newest message", Sequence: 3},
		{ID: "middle", Text: "middle message", Sequence: 2},
		{ID: "oldest", Text: "oldest message", Sequence: 1},
	}}
	text := Text(res, 64<<10)
	newest, middle, oldest := strings.Index(text, "newest message"), strings.Index(text, "middle message"), strings.Index(text, "oldest message")
	if newest < 0 || middle <= newest || oldest <= middle {
		t.Fatalf("transport changed message order: %s", text)
	}
}

// TCP READ is the hot view by default; an explicit order is passed through,
// and an unknown one is refused.
func TestTCPReadOrder(t *testing.T) {
	for line, data := range map[string]string{
		"READ lobby":          `{"sort":"hot"}`,
		"READ lobby/main 5":   `{"sort":"hot"}`,
		"READ lobby 5 new":    `{"sort":"new"}`,
		"read lobby 5 TOP":    `{"sort":"top"}`,
		"READ lobby 50 hot":   `{"sort":"hot"}`,
		"READ lobby 5 oldest": "",
		"READ lobby 5 new x":  "",
	} {
		req, err := lineProtocol{}.Parse([]byte(line))
		if data == "" {
			if err == nil {
				t.Errorf("%q accepted", line)
			}
			continue
		}
		if err != nil || req.Command == nil || req.Command.Operation != "messages.list" || req.Command.Data != data {
			t.Errorf("%q: %+v %v", line, req.Command, err)
		}
	}
}
