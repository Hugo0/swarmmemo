package transport

import "testing"

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
