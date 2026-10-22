package admin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/delivery"
	"github.com/linchange/linchange/internal/store"
)

// The outbound queue is what an administrator opens when mail has not
// arrived: what was submitted, who it was going to, and why the last attempt
// failed. It reads through the same repository the delivery worker writes to,
// so the states on screen are the ones the worker is acting on rather than a
// second account of them that could disagree.

// OutboxQueue lists the outbound queue, newest first.
func (s *Service) OutboxQueue(ctx context.Context, actor policy.Principal, filter delivery.QueueFilter) ([]delivery.OutboxQueueItem, error) {
	if err := requireAdmin(actor); err != nil {
		return nil, err
	}
	return delivery.NewOutboxRepository(s.db).ListQueue(ctx, nil, filter)
}

// OutboxQueueCount returns the size of the filtered queue, so the page can say
// how many messages there are without loading the others.
func (s *Service) OutboxQueueCount(ctx context.Context, actor policy.Principal, filter delivery.QueueFilter) (int, error) {
	if err := requireAdmin(actor); err != nil {
		return 0, err
	}
	return delivery.NewOutboxRepository(s.db).CountQueue(ctx, nil, filter)
}

// OutboxMessage returns one queued message with its recipients.
func (s *Service) OutboxMessage(ctx context.Context, actor policy.Principal, id int64) (delivery.OutboxMessage, error) {
	if err := requireAdmin(actor); err != nil {
		return delivery.OutboxMessage{}, err
	}
	msg, err := delivery.NewOutboxRepository(s.db).Get(ctx, nil, id)
	if errors.Is(err, sql.ErrNoRows) {
		// The routes speak in this package's own not-found, so an id nobody
		// has gets the same 404 as anything else that is not there.
		return delivery.OutboxMessage{}, store.ErrNotFound
	}
	return msg, err
}

// RequeueOutbox tries a stuck message again now rather than when its backoff
// runs out, and puts its refused recipients back in the retry set.
//
// The message is looked up first so that a wrong id and a message that is not
// waiting are told apart. The update itself reports both the same way — no
// rows — and answering a typo with "it is in flight or already sent" sends
// the reader off to find a problem they do not have.
//
// It refuses a message in flight or already sent instead of pretending the
// click did something: a retry button that changes nothing is how an
// administrator spends an afternoon believing they fixed it.
func (s *Service) RequeueOutbox(ctx context.Context, actor policy.Principal, id int64) error {
	if err := requireAdmin(actor); err != nil {
		return err
	}
	repo := delivery.NewOutboxRepository(s.db)

	msg, err := repo.Get(ctx, nil, id)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}
	if err != nil {
		return err
	}
	switch msg.State {
	case delivery.OutboxQueued, delivery.OutboxDeferred, delivery.OutboxFailed, delivery.OutboxBounced:
	default:
		return errors.Join(ErrInvalid, fmt.Errorf(
			"message %d is %s; only a waiting or given-up message can be tried again", id, msg.State))
	}

	if err := repo.Requeue(ctx, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// The worker claimed it between the two reads. Saying so is the
			// useful half of the message: the queue is alive, and the mail is
			// already being dealt with.
			return errors.Join(ErrInvalid, fmt.Errorf(
				"message %d changed state while it was being looked at — it may already be in flight", id))
		}
		return err
	}
	return s.record(ctx, s.db, actor, change{
		action:     ActionOutboxRequeue,
		targetType: "outbox",
		targetID:   fmt.Sprintf("%d", id),
		target:     fmt.Sprintf("message %d", id),
	})
}
