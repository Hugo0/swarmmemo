package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/trust"
)

func TestOperatorTrust(t *testing.T) {
	ctx := context.Background()
	off, err := board.Open(filepath.Join(t.TempDir(), "off.db"), board.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer off.Close()
	var out bytes.Buffer
	if err = operatorTrust(ctx, off, []string{"run"}, &out); err == nil || !strings.Contains(err.Error(), "TRUST=shadow") {
		t.Fatalf("run with TRUST off: %v", err)
	}

	store, err := board.Open(filepath.Join(t.TempDir(), "on.db"), board.Config{Features: board.Features{Trust: board.TrustShadow}})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, args := range [][]string{nil, {"run", "x"}, {"show"}, {"lift", "id"}, {"lift", "id", "--why", "x"}, {"bogus"}} {
		if err = operatorTrust(ctx, store, args, &out); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("%q: %v", args, err)
		}
	}
	out.Reset()
	if err = operatorTrust(ctx, store, []string{"run"}, &out); err != nil || !strings.Contains(out.String(), "state=done") {
		t.Fatalf("run: %v %q", err, out.String())
	}
	out.Reset()
	if err = operatorTrust(ctx, store, []string{"inputs"}, &out); err != nil {
		t.Fatal(err)
	}
	if _, err = trust.ReadJSONL(&out); err != nil {
		t.Fatalf("inputs are not a snapshot: %v", err)
	}
	if err = operatorTrust(ctx, store, []string{"show", strings.Repeat("a", 64)}, &out); err == nil {
		t.Fatal("show of an unknown agent")
	}
	if err = operatorTrust(ctx, store, []string{"lift", "funnel-none", "--reason", "no such evidence"}, &out); err == nil {
		t.Fatal("lifted missing evidence")
	}
}
