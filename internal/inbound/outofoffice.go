package inbound

import (
	"context"
	"strings"
	"time"

	"github.com/linchange/linchange/internal/delivery"
	"github.com/linchange/linchange/internal/mail"
	"github.com/linchange/linchange/internal/store"
)

// applyOutOfOffice sends the owner's automatic reply when it is active.
//
// It is deliberately conservative. An automatic reply is a message the
// recipient did not ask for, so it is skipped for junk, for a message that is
// itself automatic, for a mailing list and for the owner writing to themselves.
// The store claims one reply per correspondent per day before anything is
// queued, so two accounts that are both away cannot answer each other in a loop.
//
// It is best effort: the incoming message is already stored, and a failure to
// queue a courtesy reply must never make the sender retry the delivery.
func (b *Backend) applyOutOfOffice(ctx context.Context, owner string, meta mail.Metadata, parsed *mail.Parsed, flags []string) {
	if parsed == nil || containsFlag(flags, `\Junk`) {
		return
	}
	from := strings.ToLower(strings.TrimSpace(meta.FromAddress))
	if from == "" || strings.EqualFold(from, owner) {
		return
	}
	// RFC 3834: an automatic message must not provoke another automatic one.
	if auto := strings.TrimSpace(parsed.Header.Get("Auto-Submitted")); auto != "" && !strings.EqualFold(auto, "no") {
		return
	}
	if parsed.Header.Get("List-Id") != "" || strings.EqualFold(parsed.Header.Get("Precedence"), "bulk") ||
		strings.EqualFold(parsed.Header.Get("Precedence"), "list") {
		return
	}
	if strings.HasPrefix(from, "mailer-daemon@") || strings.HasPrefix(from, "postmaster@") {
		return
	}

	settings := store.NewOutOfOffice()
	setting, err := settings.Get(ctx, b.db, owner)
	if err != nil || !setting.Active(time.Now()) {
		return
	}
	claimed, err := settings.ClaimReply(ctx, b.db, owner, from)
	if err != nil {
		b.log.Warn("out-of-office claim failed", "owner", owner, "to", from, "error", err)
		return
	}
	if !claimed {
		return
	}

	subject := strings.TrimSpace(setting.Subject)
	if subject == "" {
		subject = "Out of office"
	}
	body := strings.TrimSpace(setting.Message)
	if body == "" {
		body = "I am currently out of the office and will reply when I return."
	}
	builder := mail.NewBuilder()
	builder.From(mail.Address{Address: owner})
	builder.To(mail.Address{Address: from, Name: meta.FromName})
	builder.Subject(subject)
	builder.Date(time.Now().UTC())
	builder.MessageID(mail.NewMessageID(b.cfg.Server.Hostname))
	builder.Header("Auto-Submitted", "auto-replied")
	builder.Header("X-Linchange-Out-Of-Office", "yes")
	if meta.MessageID != "" {
		builder.Header("In-Reply-To", meta.MessageID)
		builder.Header("References", meta.MessageID)
	}
	builder.Text(body)
	raw, err := builder.Bytes()
	if err != nil {
		b.log.Warn("could not build the out-of-office reply", "owner", owner, "error", err)
		return
	}
	digest, size, err := b.blobs.Put(raw)
	if err != nil {
		b.log.Warn("could not store the out-of-office reply", "owner", owner, "error", err)
		return
	}
	if _, err := b.outbox.InsertMessage(ctx, nil, owner, owner, digest[:], size,
		[]delivery.OutboxRecipient{{Position: 0, Address: from, Kind: delivery.To}}); err != nil {
		b.log.Warn("could not queue the out-of-office reply", "owner", owner, "to", from, "error", err)
		return
	}
	b.log.Info("out-of-office reply queued", "owner", owner, "to", from)
}
