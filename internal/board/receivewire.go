package board

// Receivers (ROADMAP §3.10, services/receiver.go): the agent's own drop box.
// The receiver service's create, rotate, delete, list and items travel as
// ordinary service.call and service.read; a delivery is a POST to the
// receive URL, which the HTTP layer hands to Receive. Receive makes no
// outbound request and never logs a URL, a secret or a body.

import (
	"context"
	"errors"
	"fmt"

	"swarmmemo/internal/allowance"
	"swarmmemo/internal/services"
)

// Receive stores one delivery to a receive URL for its owner: the source's
// attempt is counted, then one short transaction finds the receiver, checks
// it, charges the owner and stores the item; after commit the item is
// queued for screening. With receivers off every URL is not found.
func (s *Store) Receive(ctx context.Context, d services.Delivery) (services.DeliveryReceipt, error) {
	e := s.services.engine
	if e == nil {
		return services.DeliveryReceipt{}, receiverError(&allowance.Err{Code: "receiver_not_found"})
	}
	now := s.now().Unix()
	if err := e.AdmitDelivery(d.Source, now); err != nil {
		return services.DeliveryReceipt{}, receiverError(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return services.DeliveryReceipt{}, err
	}
	defer tx.Rollback()
	receipt, after, err := e.Deliver(ctx, tx, d, now)
	if err != nil {
		return services.DeliveryReceipt{}, receiverError(err)
	}
	if err = tx.Commit(); err != nil {
		return services.DeliveryReceipt{}, err
	}
	after()
	return receipt, nil
}

// ReceiverRevoke is the operator's revocation of a receiver (swarmmemo
// receiver revoke ID REASON): its URL stops at once, its owner sees the
// reason, and its items stay.
func (s *Store) ReceiverRevoke(ctx context.Context, id, reason string) (services.ReceiverView, error) {
	return services.RevokeReceiver(ctx, s.db, id, reason, s.now().Unix())
}

// receiverError is what a sender is told: every refusal in words a sender's
// operator can act on, and nothing about the receiver's owner.
func receiverError(err error) error {
	var e *allowance.Err
	if !errors.As(err, &e) {
		return err
	}
	switch e.Code {
	case "receiver_not_found":
		return problem(404, "receiver_not_found", "No receiver answers at this URL: it is wrong, rotated or stopped. Ask its owner for the current one.")
	case "receiver_source_refused":
		return problem(403, "receiver_source_refused", "This receiver takes deliveries only from the addresses its owner allowed.")
	case "receiver_too_large":
		return problem(413, "receiver_too_large", fmt.Sprintf("The body is larger than a receiver takes %s; send at most %d KiB.", SizeNote(e.Sent, e.Limit, "bytes"), services.ReceiverBodyBytes>>10))
	case "receiver_unsupported_type":
		return problem(415, "receiver_unsupported_type", "A receiver takes application/json, application/x-www-form-urlencoded or text/* in UTF-8.")
	case "receiver_invalid_body":
		return problem(400, "receiver_invalid_body", "The body must be UTF-8 text without NUL bytes, and valid JSON when sent as JSON.")
	case "receiver_signature_invalid":
		return problem(401, "receiver_signature_invalid", "This receiver checks X-Hub-Signature-256: send sha256= and the hex HMAC-SHA256 of the exact body with the secret its owner set.")
	case "receiver_quota_exhausted":
		return &Error{Status: 429, Code: "receiver_quota_exhausted", Message: "This receiver's owner has no credit left for deliveries today; retry after it resets (retry_after).", RetryAfter: e.RetryAfter}
	case "request_rate":
		return &Error{Status: 429, Code: "request_rate", Message: fmt.Sprintf("Too many deliveries: a receiver takes %d a minute and %d a day, and one network may try %d a minute. Wait retry_after seconds.", services.ReceiverPerMinute, services.ReceiverPerDay, services.ReceiverSourcePerMinute), RetryAfter: e.RetryAfter}
	}
	return fromAllowance(err)
}

// receiverCallError is the receiver service's refusals to its owner, over
// service.call and service.read.
func receiverCallError(code string) error {
	switch code {
	case "receiver_limit":
		return problem(409, "receiver_limit", fmt.Sprintf("You have %d active receivers, the most allowed (or the service is full); delete one first.", services.ReceiversPerAccount))
	case "receiver_not_found":
		return problem(404, "receiver_not_found", "None of your receivers has that id; service.read receiver list shows yours.")
	case "receiver_not_active":
		return problem(409, "receiver_not_active", "That receiver was deleted or revoked, so it has no URL to rotate; create a new one.")
	}
	return nil
}
