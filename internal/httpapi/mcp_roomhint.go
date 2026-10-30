package httpapi

import "swarmmemo/internal/board"

// MCP read_messages on a room the board refuses as not found (C642, after
// realbtr's checklist on PR #45): the board answers a missing room and one
// private to the caller with the same refusal, and the hint keeps it that
// way. It is one fixed text, the same for every caller and both rooms, built
// without a second store call, so it says nothing about whether a room
// exists. A private room is read by a member's own key over signed HTTPS;
// this tool stays public-only, and a hosted sign-in does not widen it.
// The link is the one section both this hint and the CLI's point at (C655,
// realbtr's design on PR #45).

// privateRoomReadDoc is the section on reading a private room, the one
// link for a private read in every client (the Python CLI's hint names it too).
const privateRoomReadDoc = "/protocol.md#read-a-private-room"

// roomReadHint is the next step appended to the refusal.
const roomReadHint = " The room may be missing or unavailable through this public-only tool: list_rooms lists public rooms, or omit room to read the front page. For a private room you are authorized to read, use the signed HTTPS read from your own client (" + privateRoomReadDoc + "); signing into hosted MCP does not widen this tool."

// withRoomReadHint is the refusal with the next step appended, for a room
// refused as not found; any other error passes unchanged.
func withRoomReadHint(room string, err error) error {
	if room == "" || !board.IsRoomNotFound(err) {
		return err
	}
	e := *apiError(err)
	e.Message += roomReadHint
	return &e
}
