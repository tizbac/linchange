package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// MailRule is one filtering rule.
//
// Conditions and Actions are kept as JSON on both sides of this boundary on
// purpose. They are always read and written as a unit by the delivery path,
// and never queried field by field, so turning them into columns would add
// twenty columns that no query filters on while making every rule a schema
// migration. What *is* queried is the owner, the enabled flag and the order,
// and those are columns.
type MailRule struct {
	ID             int64           `db:"id"`
	Owner          string          `db:"owner"`
	Name           string          `db:"name"`
	Enabled        bool            `db:"enabled"`
	Priority       int             `db:"priority"`
	Conditions     json.RawMessage `db:"conditions"`
	Actions        json.RawMessage `db:"actions"`
	StopProcessing bool            `db:"stop_processing"`
	CreatedAt      time.Time       `db:"created_at"`
	UpdatedAt      time.Time       `db:"updated_at"`
}

// MailRules reads and writes the mail_rules table.
type MailRules struct{}

// NewMailRules returns the repository. It is stateless, so this is free.
func NewMailRules() *MailRules { return &MailRules{} }

const mailRuleColumns = `id, owner, name, enabled, priority, conditions, actions,
	stop_processing, created_at, updated_at`

// Create inserts a rule for an owner.
func (m *MailRules) Create(ctx context.Context, q Querier, rule MailRule) (int64, error) {
	// The owner is a parameter rather than a column the caller sets, because
	// the column is also the foreign key: a rule that named somebody else's
	// address would belong to their mailbox and run on their mail.
	const query = `
		INSERT INTO mail_rules (owner, name, enabled, priority, conditions, actions, stop_processing)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id`

	var id int64
	err := q.GetContext(ctx, &id, query, rule.Owner, rule.Name, rule.Enabled,
		rule.Priority, rule.Conditions, rule.Actions, rule.StopProcessing)
	if err != nil {
		if IsConflict(err) {
			return 0, fmt.Errorf("%w: rule %q could not be created", ErrConflict, rule.Name)
		}
		return 0, fmt.Errorf("store: create mail rule: %w", err)
	}
	return id, nil
}

// List returns every rule of an owner, in the order the delivery path runs them.
//
// The order is total: priority, then id. Ties on priority are common in a UI
// where the user drags rules around, and an unstable order would make a filter
// behave differently on two deliveries of the same message.
func (m *MailRules) List(ctx context.Context, q Querier, owner string) ([]MailRule, error) {
	const query = `SELECT ` + mailRuleColumns + `
		FROM mail_rules
		WHERE owner = $1
		ORDER BY priority, id`

	return scanMailRules(ctx, q, query, owner, "store: list mail rules")
}

// ListEnabled returns the enabled rules of an owner.
//
// This is the delivery-path query, so it selects only what applying a rule
// needs and nothing that only a list view would show.
func (m *MailRules) ListEnabled(ctx context.Context, q Querier, owner string) ([]MailRule, error) {
	const query = `
		SELECT id, owner, name, enabled, priority, conditions, actions, stop_processing
		FROM mail_rules
		WHERE owner = $1 AND enabled
		ORDER BY priority, id`

	return scanMailRules(ctx, q, query, owner, "store: list enabled mail rules")
}

// scanMailRules runs a rule query and collects the rows.
//
// The Querier interface deliberately has no SelectContext, because a
// *sqlx.Tx is passed around as a Querier and every extra method on the
// interface is a method both types must have. Collecting rows here keeps that
// interface small.
func scanMailRules(ctx context.Context, q Querier, query, owner, context string) ([]MailRule, error) {
	rows, err := q.QueryxContext(ctx, query, owner)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", context, err)
	}
	defer rows.Close()

	out := make([]MailRule, 0, 8)
	for rows.Next() {
		var rule MailRule
		if err := rows.StructScan(&rule); err != nil {
			return nil, fmt.Errorf("%s: %w", context, err)
		}
		out = append(out, rule)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", context, err)
	}
	return out, nil
}

// Get returns one rule, scoped to its owner.
func (m *MailRules) Get(ctx context.Context, q Querier, owner string, id int64) (MailRule, error) {
	const query = `SELECT ` + mailRuleColumns + `
		FROM mail_rules
		WHERE id = $1 AND owner = $2`

	var rule MailRule
	if err := q.GetContext(ctx, &rule, query, id, owner); err != nil {
		if IsNotFound(err) {
			return MailRule{}, fmt.Errorf("%w: mail rule %d", ErrNotFound, id)
		}
		return MailRule{}, fmt.Errorf("store: get mail rule: %w", err)
	}
	return rule, nil
}

// MailRuleUpdate is the editable part of a rule.
//
// Pointer fields distinguish "leave alone" from "set to false", because a
// request that omits Enabled should not switch the rule off.
type MailRuleUpdate struct {
	Name           *string
	Enabled        *bool
	Priority       *int
	Conditions     json.RawMessage
	Actions        json.RawMessage
	StopProcessing *bool
}

// Update writes the editable fields of a rule.
func (m *MailRules) Update(ctx context.Context, q Querier, owner string, id int64, up MailRuleUpdate) error {
	// COALESCE keeps this one statement instead of a read-modify-write, so a
	// concurrent edit of the same rule cannot interleave and drop a field.
	const query = `
		UPDATE mail_rules SET
			name           = COALESCE($3, name),
			enabled        = COALESCE($4, enabled),
			priority       = COALESCE($5, priority),
			conditions     = COALESCE($6, conditions),
			actions        = COALESCE($7, actions),
			stop_processing = COALESCE($8, stop_processing),
			updated_at     = now()
		WHERE id = $1 AND owner = $2`

	res, err := q.ExecContext(ctx, query, id, owner, up.Name, up.Enabled, up.Priority,
		up.Conditions, up.Actions, up.StopProcessing)
	if err != nil {
		return fmt.Errorf("store: update mail rule: %w", err)
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return fmt.Errorf("%w: mail rule %d", ErrNotFound, id)
	}
	return nil
}

// Delete removes a rule.
func (m *MailRules) Delete(ctx context.Context, q Querier, owner string, id int64) error {
	const query = `DELETE FROM mail_rules WHERE id = $1 AND owner = $2`

	res, err := q.ExecContext(ctx, query, id, owner)
	if err != nil {
		return fmt.Errorf("store: delete mail rule: %w", err)
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return fmt.Errorf("%w: mail rule %d", ErrNotFound, id)
	}
	return nil
}

// CountFor returns how many rules an owner has, enabled or not.
func (m *MailRules) CountFor(ctx context.Context, q Querier, owner string) (int, error) {
	var n int
	if err := q.GetContext(ctx, &n,
		`SELECT count(*) FROM mail_rules WHERE owner = $1`, owner); err != nil {
		return 0, fmt.Errorf("store: count mail rules: %w", err)
	}
	return n, nil
}
