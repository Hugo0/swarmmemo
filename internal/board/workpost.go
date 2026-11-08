package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// A request opened as work in one call (C821). Opening paid work took two
// calls, a kind=request post and then work.create on its id; when the second
// was refused the request stood as a plain note that looked like a task and
// paid nothing. A signed request post may carry work.create's data as
// data.work (and its ttl beside it): the post and the create run in one
// transaction, so a refusal of any work rule refuses the post with that same
// problem, and nothing is published, held or charged. The create's signed
// record is the post itself: its work_transitions row keeps the post's
// payload, which workCommandData reads the work object back from.

// openPostedWork runs work.create on the request just posted as id, in the
// post's transaction, and returns its acknowledgement. The work.create
// command is checked as one sent on its own would be: an app's token, a
// scoped worker key or a device may open work only where work.create may.
func (s *Store) openPostedWork(ctx context.Context, tx *sql.Tx, c Command, a actor, id string, work json.RawMessage, now int64) (*WorkAck, error) {
	wc := Command{Operation: "work.create", MessageID: id, Data: string(work), TTL: c.TTL, PublicKey: c.PublicKey, Signature: c.Signature, Timestamp: c.Timestamp, Nonce: c.Nonce, RequestID: c.RequestID, Delegation: c.Delegation}
	if err := s.authorizeScope(ctx, tx, wc, a); err != nil {
		return nil, err
	}
	if err := authorizeDevice(wc, a, 0, now); err != nil {
		return nil, err
	}
	if err := s.authorizeDelegation(ctx, tx, wc, a, now); err != nil {
		return nil, err
	}
	wa := a
	wa.operation = wc.Operation
	res, err := s.workStep(ctx, tx, wc, wa, now, false)
	if err != nil {
		return nil, err
	}
	ack, ok := res.Data["ack"].(WorkAck)
	if !ok {
		return nil, errors.New("board: work.create answered without an ack")
	}
	return &ack, nil
}
