package main

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"swarmmemo/internal/board"
)

// swarmmemo x402 vet ID and unvet ID: the operator's way to make an open
// catalogue resource callable, and to withdraw it.
func TestX402VetCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.db")
	store, err := board.Open(path, board.Config{ServiceID: "swarmmemo.com"})
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO x402_catalogue(id,bundler,url,method,pay_to,amount,summary,first_seen,last_seen)
VALUES('api-example-com-search-0123456789abcdef','x402','https://api.example.com/search','GET','0x209693Bc6afc0C5328bA36FaF03C514EF312287C',1500,'Web search',1,1)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if store, err = board.Open(path, board.Config{ServiceID: "swarmmemo.com"}); err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		err := operatorX402(context.Background(), store, args, &out)
		return out.String(), err
	}
	out, err := run("vet", "api-example-com-search-0123456789abcdef")
	if err != nil || !strings.Contains(out, "vetted api-example-com-search-0123456789abcdef: GET https://api.example.com/search") || !strings.Contains(out, `"Web search"`) {
		t.Fatalf("vet: %v %s", err, out)
	}
	if out, err = run("unvet", "api-example-com-search-0123456789abcdef"); err != nil || !strings.Contains(out, "unvetted") {
		t.Fatalf("unvet: %v %s", err, out)
	}
	if _, err = run("vet", "no-such-id"); err == nil {
		t.Fatal("an unknown id was vetted")
	}
	if _, err = run("vet"); err == nil || !strings.Contains(err.Error(), "x402 vet ID") {
		t.Fatalf("usage: %v", err)
	}
	if !x402VetCommand([]string{"vet", "x"}) || x402VetCommand([]string{"check"}) {
		t.Fatal("x402VetCommand")
	}
}
