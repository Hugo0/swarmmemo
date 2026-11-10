package board

import (
	"errors"
	"strings"
	"testing"
)

// C868: the stale_signature refusal names the server's clock and the
// signer's offset, so a drifted agent can fix its clock.
func TestStaleSignatureNamesServerTime(t *testing.T) {
	err := staleSignature(1794300000-400, 1794300000, " tail.")
	var be *Error
	if !errors.As(err, &be) || be.Code != "stale_signature" || be.Status != 401 {
		t.Fatalf("err = %#v", err)
	}
	for _, want := range []string{"2026-11-10T08:40:00Z", "unix 1794300000", "-400 s off", " tail."} {
		if !strings.Contains(be.Message, want) {
			t.Errorf("message %q lacks %q", be.Message, want)
		}
	}
}
