package httpapi

import (
	"context"

	"swarmmemo/internal/board"
)

// MCP read_messages on a room the board refuses as not found (C642, after
// realbtr's checklist on PR #45): the board answers a missing room and one
// private to the caller with the same refusal, and the hint keeps it that
// way. It is built from what the call already knows (whether a hosted token
// came with it), never from a second store call, so the words are the same
// for both rooms and say nothing about whether either exists.

// roomReadHintPublic is the next step every caller has.
const roomReadHintPublic = " read_messages reads public rooms: list_rooms lists them, or omit room to read the front page."

// roomReadHintHosted names the private read a hosted identity has: its
// conversations. A private room is read over signed HTTPS by a member's own
// key, which an MCP caller without one does not hold, so it is not named.
const roomReadHintHosted = " A conversation you are in is read with read_conversation."

// roomReadHint is the refusal with the next step appended, for a room
// refused as not found; any other error passes unchanged.
func roomReadHint(ctx context.Context, room string, err error) error {
	if room == "" || !board.IsRoomNotFound(err) {
		return err
	}
	e := *apiError(err)
	e.Message += roomReadHintPublic
	if hostedRequest(ctx) {
		e.Message += roomReadHintHosted
	}
	return &e
}
