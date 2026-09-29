package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"swarmmemo/internal/board"
	"swarmmemo/internal/ledger"
)

func TestOperatorParamsAndAllowance(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := board.Open(filepath.Join(dir, "board.db"), board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var out bytes.Buffer
	if err = operatorParams(ctx, store, []string{"show", "allowance"}, &out); err != nil {
		t.Fatal(err)
	}
	var shown ledger.ParamsVersion
	if err = json.Unmarshal(out.Bytes(), &shown); err != nil || shown.Version != 0 || !shown.CompiledIn {
		t.Fatalf("show %s %v", out.String(), err)
	}
	p := ledger.DefaultAllowanceParams()
	p.Resources["post_bytes"].TransferFee = 512
	file := filepath.Join(dir, "allowance.json")
	if err = os.WriteFile(file, p.Marshal(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = operatorParams(ctx, store, []string{"set", "allowance", file}, &out); err == nil {
		t.Fatal("set without a reason")
	}
	out.Reset()
	if err = operatorParams(ctx, store, []string{"set", "allowance", file, "--reason", "raise the transfer fee"}, &out); err != nil || !strings.Contains(out.String(), "version 1") {
		t.Fatalf("set: %q %v", out.String(), err)
	}
	if err = operatorParams(ctx, store, []string{"set", "allowance", file, "--reason", "x", "--reason", "y"}, &out); err == nil {
		t.Fatal("a repeated flag")
	}
	if err = operatorParams(ctx, store, []string{"bogus"}, &out); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("usage: %v", err)
	}
	out.Reset()
	if err = operatorAllowance(ctx, store, []string{"sweep"}, &out); err != nil || !strings.Contains(out.String(), "swept") {
		t.Fatalf("sweep: %q %v", out.String(), err)
	}
	if err = operatorAllowance(ctx, store, []string{"grant", strings.Repeat("a", 64), "post_bytes", "10", "--reason", "x", "--bucket", "paid"}, &out); err == nil {
		t.Fatal("a paid grant from the operator")
	}
	if err = operatorAllowance(ctx, store, []string{"grant", strings.Repeat("a", 64), "post_bytes", "10", "--reason", "x"}, &out); err == nil {
		t.Fatal("a grant to an unregistered agent")
	}
}
