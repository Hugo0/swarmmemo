package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"swarmmemo/internal/board"
)

const receiverUsage = "usage: swarmmemo receiver revoke RECEIVER_ID REASON"

// operatorReceiver is swarmmemo receiver revoke ID REASON: the operator's
// revocation of an abused receiver. Its URL stops at once, its owner sees
// the reason, and its items stay. It prints no URL, secret or body.
func operatorReceiver(ctx context.Context, store *board.Store, args []string, out io.Writer) error {
	if len(args) != 3 || args[0] != "revoke" {
		return errors.New(receiverUsage)
	}
	v, err := store.ReceiverRevoke(ctx, args[1], args[2])
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "receiver %s is %s (%d deliveries); its items stay readable by its owner\n", v.ID, v.State, v.Deliveries)
	return nil
}

const pasteUsage = "usage: swarmmemo paste hide PASTE_ID REASON"

// operatorPaste is swarmmemo paste hide ID REASON: the operator's hide of an
// abused paste. It stops opening at once, its owner sees the reason, and its
// text stays (moderation hides, never deletes). It prints no text.
func operatorPaste(ctx context.Context, store *board.Store, args []string, out io.Writer) error {
	if len(args) != 3 || args[0] != "hide" {
		return errors.New(pasteUsage)
	}
	v, err := store.PasteHide(ctx, args[1], args[2])
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "paste %s is %s (%d bytes, sha256 %s); its owner still reads it\n", v.ID, v.State, v.Bytes, v.Hash)
	return nil
}

const docUsage = "usage: swarmmemo doc hide DOC_ID REASON"

// operatorDoc is swarmmemo doc hide ID REASON: the operator's hide of an
// abused doc or paste (a paste is a doc). It stops opening and taking
// versions at once, its owner sees the reason, and its text stays
// (moderation hides, never deletes). It prints no text.
func operatorDoc(ctx context.Context, store *board.Store, args []string, out io.Writer) error {
	if len(args) != 3 || args[0] != "hide" {
		return errors.New(docUsage)
	}
	v, err := store.DocHide(ctx, args[1], args[2])
	if err != nil {
		return err
	}
	state := v.State
	if state == "" {
		state = "active"
	}
	fmt.Fprintf(out, "doc %s is %s (version %d, %d bytes, sha256 %s); its owner still reads it\n", v.ID, state, v.Version, v.Bytes, v.Hash)
	return nil
}
