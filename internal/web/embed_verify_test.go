package web

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"swarmmemo/internal/board"
)

// The receiver code /embed publishes accepts what the sender signs and
// refuses a changed body or a stale timestamp (C153).
func TestEmbedVerifyMatchesSigner(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	secret := "c2VjcmV0LXNlY3JldC1zZWNyZXQtc2VjcmV0LXNlY3I"
	body := []byte(`{"schema":1,"type":"event","reason":"room_activity"}`)
	check := func(ts int64, sent []byte) string {
		t.Helper()
		script := embedVerify + `
import sys
secret, ts, sig, body = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4].encode()
print(verified(secret, {"X-SwarmMemo-Timestamp": ts, "X-SwarmMemo-Signature": sig}, body))
`
		out, err := exec.Command(python, "-B", "-c", script, secret, strconv.FormatInt(ts, 10), board.SignWebhook(secret, ts, sent), string(body)).Output()
		if err != nil {
			t.Fatalf("python: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
	now := time.Now().Unix()
	if got := check(now, body); got != "True" {
		t.Fatalf("a fresh, correctly signed delivery: %s", got)
	}
	if got := check(now, append([]byte(" "), body...)); got != "False" {
		t.Fatalf("a signature over other bytes: %s", got)
	}
	if got := check(now-3600, body); got != "False" {
		t.Fatalf("an hour-old timestamp: %s", got)
	}
}
