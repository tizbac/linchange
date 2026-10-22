package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// OutOfOffice is an account's automatic reply setting.
//
// Start and End are optional: an administrator who wants a reply now can leave
// the schedule open, and a client that only knows "enabled until Friday" can
// send the end alone. When both are set, the window is inclusive of Start and
// exclusive of End, which is how a calendar range is normally read.
type OutOfOffice struct {
	Enabled bool       `json:"enabled"`
	Start   *time.Time `json:"start,omitempty"`
	End     *time.Time `json:"end,omitempty"`
	Subject string     `json:"subject"`
	Message string     `json:"message"`
}

// Active reports whether the setting should answer a message at instant now.
func (o OutOfOffice) Active(now time.Time) bool {
	if !o.Enabled {
		return false
	}
	if o.Start != nil && now.Before(*o.Start) {
		return false
	}
	if o.End != nil && !now.Before(*o.End) {
		return false
	}
	return true
}

// OutOfOfficeSettings reads and writes the per-account automatic reply.
type OutOfOfficeSettings struct{}

// NewOutOfOffice returns the repository. It is stateless.
func NewOutOfOffice() *OutOfOfficeSettings { return &OutOfOfficeSettings{} }

// Get returns an account's setting, or an empty disabled one when the account
// exists but has never filled the fields in.
func (o *OutOfOfficeSettings) Get(ctx context.Context, q Querier, address string) (OutOfOffice, error) {
	var row struct {
		Enabled bool         `db:"out_of_office_enabled"`
		Start   sql.NullTime `db:"out_of_office_start"`
		End     sql.NullTime `db:"out_of_office_end"`
		Subject string       `db:"out_of_office_subject"`
		Message string       `db:"out_of_office_message"`
	}
	if err := q.GetContext(ctx, &row, `
		SELECT out_of_office_enabled, out_of_office_start, out_of_office_end,
		       out_of_office_subject, out_of_office_message
		FROM users WHERE address = $1 AND deleted_at IS NULL`,
		strings.ToLower(strings.TrimSpace(address))); err != nil {
		if IsNotFound(err) {
			return OutOfOffice{}, ErrNotFound
		}
		return OutOfOffice{}, fmt.Errorf("store: reading out-of-office setting: %w", err)
	}
	out := OutOfOffice{
		Enabled: row.Enabled,
		Subject: row.Subject,
		Message: row.Message,
	}
	if row.Start.Valid {
		start := row.Start.Time
		out.Start = &start
	}
	if row.End.Valid {
		end := row.End.Time
		out.End = &end
	}
	return out, nil
}

// Set replaces one account's automatic reply setting.
func (o *OutOfOfficeSettings) Set(ctx context.Context, q Querier, address string, setting OutOfOffice) (OutOfOffice, error) {
	address = strings.ToLower(strings.TrimSpace(address))
	if setting.Start != nil && setting.End != nil && !setting.End.After(*setting.Start) {
		return OutOfOffice{}, fmt.Errorf("store: the end of an out-of-office period must be after its start")
	}
	res, err := q.ExecContext(ctx, `
		UPDATE users
		SET out_of_office_enabled = $2,
		    out_of_office_start = $3,
		    out_of_office_end = $4,
		    out_of_office_subject = $5,
		    out_of_office_message = $6,
		    updated_at = now()
		WHERE address = $1 AND deleted_at IS NULL`,
		address, setting.Enabled, setting.Start, setting.End,
		strings.TrimSpace(setting.Subject), strings.TrimSpace(setting.Message))
	if err != nil {
		return OutOfOffice{}, fmt.Errorf("store: saving out-of-office setting: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return OutOfOffice{}, ErrNotFound
	}
	// A new setting is a new out-of-office period: the people who were already
	// answered may be answered again the next time it is switched on. Editing
	// the text resets the list too, which is the least surprising reading of
	// "save this automatic reply".
	if _, err := q.ExecContext(ctx,
		`DELETE FROM out_of_office_replies WHERE owner = $1`, address); err != nil {
		return OutOfOffice{}, fmt.Errorf("store: resetting out-of-office replies: %w", err)
	}
	return o.Get(ctx, q, address)
}

// ClaimReply records that this correspondent has been answered and reports
// whether this call was the one that won the claim.
//
// One reply per sender per activation: a sender who writes five times gets one
// answer, not five. The row stays until the setting is saved again, which is
// when Set clears the list. The insert and the conflict test are one statement,
// so two messages arriving together cannot both decide they are the first.
func (o *OutOfOfficeSettings) ClaimReply(ctx context.Context, q Querier, owner, recipient string) (bool, error) {
	owner = strings.ToLower(strings.TrimSpace(owner))
	recipient = strings.ToLower(strings.TrimSpace(recipient))
	if owner == "" || recipient == "" {
		return false, nil
	}
	var sentAt time.Time
	err := q.GetContext(ctx, &sentAt, `
		INSERT INTO out_of_office_replies (owner, recipient, last_sent_at)
		VALUES ($1, $2, now())
		ON CONFLICT (owner, recipient) DO NOTHING
		RETURNING last_sent_at`, owner, recipient)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: claiming an out-of-office reply: %w", err)
	}
	return !sentAt.IsZero(), nil
}
