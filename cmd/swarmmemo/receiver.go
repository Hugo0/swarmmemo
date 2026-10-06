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
