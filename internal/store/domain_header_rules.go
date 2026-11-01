package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// A header rule is one test on one header of an inbound message, and one
// outcome when the test matches. The vocabulary is deliberately small: the
// delivery path runs these on every message, and a rule that needed a
// condition tree would be a mail_rules rule, not a domain rule.
const (
	HeaderMatchContains   = "contains"
	HeaderMatchIs         = "is"
	HeaderMatchStartsWith = "starts_with"
	HeaderMatchEndsWith   = "ends_with"
	HeaderMatchExists     = "exists"

	HeaderActionJunk    = "junk"
	HeaderActionReject  = "reject"
	HeaderActionDiscard = "discard"
)

// ValidHeaderMatch reports whether match is a comparison the engine implements.
// Checking it here rather than at the schema keeps the error message about the
// value the administrator typed instead of a constraint violation.
func ValidHeaderMatch(match string) bool {
	switch match {
	case HeaderMatchContains, HeaderMatchIs, HeaderMatchStartsWith,
		HeaderMatchEndsWith, HeaderMatchExists:
		return true
	}
	return false
}

// ValidHeaderAction reports whether action is an outcome the delivery path
// knows how to carry out:
//   - junk   file the message in Junk
//   - reject refuse it at the SMTP level, so the sender is bounced
//   - discard accept it and store nothing
func ValidHeaderAction(action string) bool {
	switch action {
	case HeaderActionJunk, HeaderActionReject, HeaderActionDiscard:
		return true
	}
	return false
}

// DomainHeaderRule is one per-domain header test as stored.
type DomainHeaderRule struct {
	ID            int64     `db:"id"`
	Domain        string    `db:"domain"`
	Header        string    `db:"header"`
	Value         string    `db:"value"`
	Match         string    `db:"match"`
	Action        string    `db:"action"`
	CaseSensitive bool      `db:"case_sensitive"`
	Enabled       bool      `db:"enabled"`
	CreatedAt     time.Time `db:"created_at"`
	UpdatedAt     time.Time `db:"updated_at"`
}

// DomainHeaderRuleRequest is what an administrator supplies. Empty Match and
// Action fall back to the defaults, which is the common case: a rule written as
// "X-Spam contains YES means junk" needs no other words. Enabled is a pointer
// because a rule is enabled unless the caller says otherwise, and a false zero
// value would make the most common request - "add this rule" - mean "add it
// switched off".
type DomainHeaderRuleRequest struct {
	Header        string
	Value         string
	Match         string
	Action        string
	CaseSensitive bool
	Enabled       *bool
}

// Normalize returns the request with the defaults applied and the text fields
// trimmed, so that what is validated is what is stored.
func (r DomainHeaderRuleRequest) Normalize() DomainHeaderRuleRequest {
	out := r
	out.Header = strings.TrimSpace(r.Header)
	out.Value = strings.TrimSpace(r.Value)
	out.Match = strings.ToLower(strings.TrimSpace(r.Match))
	if out.Match == "" {
		out.Match = HeaderMatchContains
	}
	out.Action = strings.ToLower(strings.TrimSpace(r.Action))
	if out.Action == "" {
		out.Action = HeaderActionJunk
	}
	return out
}

// DomainHeaderRules reads and writes the domain_header_rules table.
type DomainHeaderRules struct{}

// NewDomainHeaderRules returns the repository. It is stateless, so this is free.
func NewDomainHeaderRules() *DomainHeaderRules { return &DomainHeaderRules{} }

const domainHeaderRuleColumns = `id, domain, header, value, match, action,
	case_sensitive, enabled, created_at, updated_at`

// Create adds a rule to a domain. The domain is a parameter rather than a field
// of the request because it is also the foreign key: a rule that named a domain
// the caller did not ask about would apply to somebody else's mail.
func (h *DomainHeaderRules) Create(ctx context.Context, q Querier, domain string, req DomainHeaderRuleRequest) (DomainHeaderRule, error) {
	req = req.Normalize()
	if req.Header == "" {
		return DomainHeaderRule{}, fmt.Errorf("header rule: the header name is empty")
	}
	if !ValidHeaderMatch(req.Match) {
		return DomainHeaderRule{}, fmt.Errorf("header rule: unknown match %q", req.Match)
	}
	if !ValidHeaderAction(req.Action) {
		return DomainHeaderRule{}, fmt.Errorf("header rule: unknown action %q", req.Action)
	}
	if req.Match != HeaderMatchExists && req.Value == "" {
		return DomainHeaderRule{}, fmt.Errorf("header rule: the match %q needs a value", req.Match)
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	const query = `
		INSERT INTO domain_header_rules (domain, header, value, match, action, case_sensitive, enabled)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING ` + domainHeaderRuleColumns

	var rule DomainHeaderRule
	if err := q.GetContext(ctx, &rule, query, domain, req.Header, req.Value, req.Match,
		req.Action, req.CaseSensitive, enabled); err != nil {
		if IsConflict(err) {
			return DomainHeaderRule{}, fmt.Errorf("%w: header rule for %s", ErrConflict, domain)
		}
		return DomainHeaderRule{}, fmt.Errorf("store: create header rule: %w", err)
	}
	return rule, nil
}

// List returns a domain's rules, enabled and disabled, in the order the
// delivery path reads them.
func (h *DomainHeaderRules) List(ctx context.Context, q Querier, domain string) ([]DomainHeaderRule, error) {
	const query = `SELECT ` + domainHeaderRuleColumns + `
		FROM domain_header_rules
		WHERE domain = $1
		ORDER BY id`
	return scanHeaderRules(ctx, q, query, domain, "store: list header rules")
}

// ListEnabled returns the rules the delivery path runs: enabled ones only, in
// the same order as List so that a rule added earlier is evaluated first.
func (h *DomainHeaderRules) ListEnabled(ctx context.Context, q Querier, domain string) ([]DomainHeaderRule, error) {
	const query = `SELECT ` + domainHeaderRuleColumns + `
		FROM domain_header_rules
		WHERE domain = $1 AND enabled
		ORDER BY id`
	return scanHeaderRules(ctx, q, query, domain, "store: list enabled header rules")
}

// scanHeaderRules runs a rule query and collects the rows. Querier has no
// SelectContext on purpose, so the rows are read here rather than in the
// caller.
func scanHeaderRules(ctx context.Context, q Querier, query, domain, context string) ([]DomainHeaderRule, error) {
	rows, err := q.QueryxContext(ctx, query, domain)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", context, err)
	}
	defer rows.Close()

	out := make([]DomainHeaderRule, 0, 8)
	for rows.Next() {
		var rule DomainHeaderRule
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

// Get returns one rule by id.
func (h *DomainHeaderRules) Get(ctx context.Context, q Querier, id int64) (DomainHeaderRule, error) {
	const query = `SELECT ` + domainHeaderRuleColumns + ` FROM domain_header_rules WHERE id = $1`

	var rule DomainHeaderRule
	if err := q.GetContext(ctx, &rule, query, id); err != nil {
		if IsNotFound(err) {
			return DomainHeaderRule{}, fmt.Errorf("%w: header rule %d", ErrNotFound, id)
		}
		return DomainHeaderRule{}, fmt.Errorf("store: get header rule: %w", err)
	}
	return rule, nil
}

// GetForDomain returns one rule scoped to a domain. The scope is not cosmetic:
// without it an id from another domain's rule list would edit that rule.
func (h *DomainHeaderRules) GetForDomain(ctx context.Context, q Querier, domain string, id int64) (DomainHeaderRule, error) {
	const query = `SELECT ` + domainHeaderRuleColumns + `
		FROM domain_header_rules WHERE id = $1 AND domain = $2`

	var rule DomainHeaderRule
	if err := q.GetContext(ctx, &rule, query, id, domain); err != nil {
		if IsNotFound(err) {
			return DomainHeaderRule{}, fmt.Errorf("%w: header rule %d", ErrNotFound, id)
		}
		return DomainHeaderRule{}, fmt.Errorf("store: get header rule: %w", err)
	}
	return rule, nil
}

// DomainHeaderRuleUpdate is the editable part of a rule. Pointer fields
// distinguish "leave alone" from "set to false", because a request that omits
// Enabled must not switch the rule off.
type DomainHeaderRuleUpdate struct {
	Header        *string
	Value         *string
	Match         *string
	Action        *string
	CaseSensitive *bool
	Enabled       *bool
}

// Update writes the editable fields of a rule scoped to a domain.
func (h *DomainHeaderRules) Update(ctx context.Context, q Querier, domain string, id int64, up DomainHeaderRuleUpdate) error {
	// COALESCE keeps this one statement rather than a read-modify-write, so a
	// concurrent edit of the same rule cannot interleave and drop a field.
	const query = `
		UPDATE domain_header_rules SET
			header         = COALESCE($3, header),
			value          = COALESCE($4, value),
			match          = COALESCE($5, match),
			action         = COALESCE($6, action),
			case_sensitive = COALESCE($7, case_sensitive),
			enabled        = COALESCE($8, enabled),
			updated_at     = now()
		WHERE id = $1 AND domain = $2`

	res, err := q.ExecContext(ctx, query, id, domain, up.Header, up.Value, up.Match,
		up.Action, up.CaseSensitive, up.Enabled)
	if err != nil {
		return fmt.Errorf("store: update header rule: %w", err)
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return fmt.Errorf("%w: header rule %d", ErrNotFound, id)
	}
	return nil
}

// Delete removes a rule scoped to a domain.
func (h *DomainHeaderRules) Delete(ctx context.Context, q Querier, domain string, id int64) error {
	const query = `DELETE FROM domain_header_rules WHERE id = $1 AND domain = $2`

	res, err := q.ExecContext(ctx, query, id, domain)
	if err != nil {
		return fmt.Errorf("store: delete header rule: %w", err)
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return fmt.Errorf("%w: header rule %d", ErrNotFound, id)
	}
	return nil
}
