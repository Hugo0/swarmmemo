package transport

import (
	"context"
	"encoding/base64"
	"regexp"
	"strings"
	"testing"
)

// C160: mail keeps its envelope sender's domain, never the address, with the
// write's operator-only signals; the peer's address is hashed by the board.
func TestSMTPKeepsTheSenderDomainOnly(t *testing.T) {
	store := openStore(t)
	seed(t, store, "room exists")
	_, addrs := startedWith(t, store, Config{Host: "swarmmemo.com", SMTPAddr: "127.0.0.1:0", SMTPDomain: "post.swarmmemo.com"})
	cmd := base64.RawURLEncoding.EncodeToString(signedPost(t, "lobby", "signed by mail", "mail-signals"))
	lines := mailTo("post@post.swarmmemo.com", "swarmmemo-command: "+cmd)
	lines[1] = "MAIL FROM:<Someone@Mail.Example.COM> SIZE=100"
	out := smtpSession(t, addrs["smtp/tcp"], lines...)
	m := regexp.MustCompile(`2\.0\.0 ok (\S+) `).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("signed mail: %s", out)
	}
	sig, ok, err := store.SignalMessage(context.Background(), m[1])
	if err != nil || !ok {
		t.Fatalf("no signal: %v", err)
	}
	if sig.Origin != "email:mail.example.com" || sig.Via != "email" || sig.IPHash24 == "" || strings.Contains(sig.Origin, "someone") {
		t.Fatalf("signal %+v", sig)
	}
}

func TestEnvelopeDomain(t *testing.T) {
	for in, want := range map[string]string{
		"<a@B.example>":                        "b.example",
		"<a@b.example> SIZE=10":                "b.example",
		"a@b.example":                          "b.example",
		"<>":                                   "",
		"<a@[192.0.2.1]>":                      "",
		"<a@b.example\x1b[31m>":                "",
		"<no-at>":                              "",
		"<a@" + strings.Repeat("x", 300) + ">": "",
	} {
		if got := envelopeDomain(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
