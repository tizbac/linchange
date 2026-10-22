package webmail

import (
	"context"
	"fmt"
	"strings"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/delivery"
	"github.com/linchange/linchange/internal/mail"
	"github.com/linchange/linchange/internal/store"
)

// SendInternal files a message into the INBOX of each recipient on this
// server, without any of it going out over SMTP.
//
// It exists for the notices this server writes to its own people — that a
// colleague has offered them a calendar, that something was shared with their
// team — because routing one of those through the outbound queue would hand
// it to the relay or to this server's own inbound listener, where it is
// scored as incoming mail, delayed for greylisting, and occasionally bounced
// to nobody. The recipient lives on this server and the message comes from
// this server: filing it is the delivery, and the SMTP conversation in
// between would only be a place for it to be refused.
//
// One message is built per recipient rather than one addressed to all of
// them. Each is a personal notice, and a single copy in every mailbox would
// show its reader the whole list of people who were told, which is not the
// reader's to see.
//
// A failure to file is logged rather than returned: the thing the notice is
// about — the share, the invitation — has already happened, and reporting an
// error about the message that mentions it would either fail work that is not
// this message's to undo, or fail it twice.
func (s *Service) SendInternal(ctx context.Context, from string, recipients []string, subject, body string) error {
	for _, to := range recipients {
		builder := mail.NewBuilder()
		builder.From(mail.Address{Address: from})
		builder.To(mail.Address{Address: to})
		builder.Subject(subject)
		builder.Date(nowUTC())
		builder.MessageID(mail.NewMessageID(s.hostname))
		builder.Text(body)

		raw, err := builder.Bytes()
		if err != nil {
			return fmt.Errorf("webmail: build the internal message: %w", err)
		}
		if _, err := s.file(ctx, to, "INBOX", raw, nil); err != nil {
			s.log.Warn("could not file an internal message",
				"to", to, "subject", subject, "error", err)
		}
	}
	return nil
}

// SendExternal queues one plain-text notice for delivery outside this server.
//
// SendInternal files a message into a local INBOX and cannot reach a personal
// address on another domain. An account invitation has to get there, so it goes
// through the ordinary outbox and the delivery worker. It is deliberately not
// filed in anybody's Sent folder: it is a service notice, not a message the
// administrator composed.
func (s *Service) SendExternal(ctx context.Context, from, to, subject, body string) (int64, error) {
	from = strings.ToLower(strings.TrimSpace(from))
	to = strings.ToLower(strings.TrimSpace(to))
	if from == "" || to == "" {
		return 0, fmt.Errorf("webmail: an external notice needs a sender and a recipient")
	}

	builder := mail.NewBuilder()
	builder.From(mail.Address{Address: from})
	builder.To(mail.Address{Address: to})
	builder.Subject(subject)
	builder.Date(nowUTC())
	builder.MessageID(mail.NewMessageID(s.hostname))
	builder.Text(body)

	raw, err := builder.Bytes()
	if err != nil {
		return 0, fmt.Errorf("webmail: build the notice: %w", err)
	}
	digest, size, err := s.blobs.Put(raw)
	if err != nil {
		return 0, fmt.Errorf("webmail: store the notice: %w", err)
	}

	var outboxID int64
	err = store.InTx(ctx, s.db, func(tx *sqlx.Tx) error {
		id, err := s.outbox.InsertMessage(ctx, tx, from, from, digest[:], size, []delivery.OutboxRecipient{
			{Position: 0, Address: to, Kind: delivery.To},
		})
		if err != nil {
			return err
		}
		outboxID = id
		return nil
	})
	if err != nil {
		return 0, err
	}
	return outboxID, nil
}
