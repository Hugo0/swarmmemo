package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOperatorRoomCommand(t *testing.T) {
	agent := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		args          []string
		op, target, d string
	}{
		{[]string{"lobby", "policy", `{"write":"owner"}`}, "room.policy.set", "", `{"write":"owner"}`},
		{[]string{"lobby", "moderator", "add", agent}, "room.moderator.add", agent, ""},
		{[]string{"lobby", "moderator", "remove", agent}, "room.moderator.remove", agent, ""},
		{[]string{"lobby", "owner", agent}, "room.owner.transfer", agent, ""},
		{[]string{"lobby", "style", "clear"}, "room.style.clear", "", ""},
	} {
		c, err := operatorRoomCommand(tc.args)
		if err != nil || c.Operation != tc.op || c.Room != "lobby" || c.Target != tc.target || c.Data != tc.d {
			t.Fatalf("%v: %+v %v", tc.args, c, err)
		}
	}
	file := filepath.Join(t.TempDir(), "room.css")
	if err := os.WriteFile(file, []byte(":scope{color:#000}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if c, err := operatorRoomCommand([]string{"lobby", "style", "set", file}); err != nil || c.Operation != "room.style.set" || c.Data != `{"css":":scope{color:#000}\n"}` {
		t.Fatalf("style set: %+v %v", c, err)
	}
	for _, args := range [][]string{nil, {"lobby"}, {"lobby", "policy"}, {"lobby", "moderator", "promote", agent}, {"lobby", "hide", "x"}, {"lobby", "owner"},
		{"lobby", "style"}, {"lobby", "style", "set"}, {"lobby", "style", "set", filepath.Join(t.TempDir(), "missing.css")}} {
		if _, err := operatorRoomCommand(args); err == nil {
			t.Fatalf("%v accepted", args)
		}
	}
}
