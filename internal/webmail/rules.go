package webmail

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/linchange/linchange/internal/mail"
	"github.com/linchange/linchange/internal/store"
)

// RuleCondition is one test a message must pass.
//
// The set is closed. A rule engine whose conditions are an open expression
// evaluated by the server is an evaluation engine reachable from a mailbox
// settings page, so these are named tests with typed values.
type RuleCondition struct {
	// Field is what is tested: from, to, cc, subject, body, attachment,
	// header, size, spam_score or recipient_domain.
	Field string `json:"field"`
	// Operator is how it is tested: contains, is, matches, is_not, starts_with,
	// ends_with, greater_than, less_than, exists or is_true.
	Operator string `json:"operator"`
	// Value is the operand. It is a string because the form posts one, and the
	// numeric operators parse it; making it a union would move the type error
	// from the rule into the message that matches it.
	Value string `json:"value,omitempty"`
	// Header names the header for the field "header".
	Header string `json:"header,omitempty"`
	// CaseSensitive is off by default, because "Invoice" and "invoice" are the
	// same word to the person writing the rule.
	CaseSensitive bool `json:"case_sensitive,omitempty"`
	// Negate inverts the test, so "does not contain" needs no second operator.
	Negate bool `json:"negate,omitempty"`
}

// RuleAction is something to do with a message that matched.
type RuleAction struct {
	// Type is move, mark_read, mark_flagged, mark_unflagged, add_label,
	// forward or discard.
	Type string `json:"type"`
	// Target is the destination folder for move, the flag for the mark actions,
	// and the address for forward.
	Target string `json:"target,omitempty"`
	// Stop is set on an action to mean "stop here" as well as doing this thing,
	// which is what a user means by "move it to Junk and leave the rest alone".
	Stop bool `json:"stop,omitempty"`
}

// Rule is a stored rule as the client sees it.
type Rule struct {
	ID        int64           `json:"id"`
	Name      string          `json:"name"`
	Enabled   bool            `json:"enabled"`
	Priority  int             `json:"priority"`
	Conditions []RuleCondition `json:"conditions"`
	Actions    []RuleAction    `json:"actions"`
	Stop       bool            `json:"stop"`
}

// knownConditionFields are the tests the engine implements.
var knownConditionFields = map[string]bool{
	"from": true, "to": true, "cc": true, "subject": true, "body": true,
	"attachment": true, "header": true, "size": true, "sender_domain": true,
}

// knownConditionOperators are the comparisons the engine implements.
var knownConditionOperators = map[string]bool{
	"contains": true, "is": true, "starts_with": true, "ends_with": true,
	"matches": true, "exists": true, "greater_than": true, "less_than": true,
	"is_true": true,
}

// knownActionTypes are the things a rule may do.
var knownActionTypes = map[string]bool{
	"move": true, "mark_read": true, "mark_flagged": true, "mark_unflagged": true,
	"forward": true, "discard": true,
}

// ValidateRule checks a rule before it is stored.
//
// A rule is rejected rather than stored-and-ignored: a rule that silently does
// nothing is worse than no rule, because the user believes their mail is being
// filed and it is going to Junk unfiled.
func ValidateRule(r Rule) error {
	if strings.TrimSpace(r.Name) == "" {
		return fmt.Errorf("%w: a rule needs a name", ErrInvalid)
	}
	if len(r.Conditions) == 0 {
		// A rule with no conditions matches everything, which is a filter that
		// catches mail and a foot-gun that does not. A rule that should apply to
		// everything is expressed by saying so.
		return fmt.Errorf("%w: a rule needs at least one condition", ErrInvalid)
	}
	if len(r.Actions) == 0 {
		return fmt.Errorf("%w: a rule needs at least one action", ErrInvalid)
	}
	if r.Priority < 0 {
		return fmt.Errorf("%w: a rule priority cannot be negative", ErrInvalid)
	}

	for i, c := range r.Conditions {
		if !knownConditionFields[c.Field] {
			return fmt.Errorf("%w: condition %d tests %q, which is not something a rule can test",
				ErrInvalid, i+1, c.Field)
		}
		if !knownConditionOperators[c.Operator] {
			return fmt.Errorf("%w: condition %d uses %q, which is not an operator a rule has",
				ErrInvalid, i+1, c.Operator)
		}
		if c.Field == "header" && strings.TrimSpace(c.Header) == "" {
			return fmt.Errorf("%w: condition %d tests a header without naming one", ErrInvalid, i+1)
		}
		if c.Operator != "exists" && c.Operator != "is_true" && strings.TrimSpace(c.Value) == "" {
			return fmt.Errorf("%w: condition %d needs a value to compare against", ErrInvalid, i+1)
		}
		if c.Operator == "matches" {
			if _, err := compileMatch(c); err != nil {
				return fmt.Errorf("%w: condition %d: %s", ErrInvalid, i+1, err)
			}
		}
	}

	for i, a := range r.Actions {
		if !knownActionTypes[a.Type] {
			return fmt.Errorf("%w: action %d is %q, which is not something a rule can do",
				ErrInvalid, i+1, a.Type)
		}
		switch a.Type {
		case "move":
			if strings.TrimSpace(a.Target) == "" {
				return fmt.Errorf("%w: action %d moves the message nowhere", ErrInvalid, i+1)
			}
		case "forward":
			if _, err := mail.ParseAddressList(a.Target); err != nil {
				return fmt.Errorf("%w: action %d forwards to %q, which is not an address",
					ErrInvalid, i+1, a.Target)
			}
		}
	}
	return nil
}

// Rules returns an owner's rules in the order they run.
func (s *Service) Rules(ctx context.Context, owner string) ([]Rule, error) {
	rows, err := s.rules.List(ctx, s.db, owner)
	if err != nil {
		return nil, err
	}
	out := make([]Rule, 0, len(rows))
	for _, row := range rows {
		rule, err := ruleFromRow(row)
		if err != nil {
			// One unreadable rule must not hide the rest of them: the user can
			// still read, reorder and delete the rules that do parse.
			s.log.Warn("skipping unreadable rule", "rule_id", row.ID, "error", err)
			continue
		}
		out = append(out, rule)
	}
	return out, nil
}

// SaveRule creates or replaces a rule.
//
// It is one call rather than a create and an update because the client's rule
// editor sends the whole rule, and a PUT that replaced fields one at a time
// would leave a renamed rule with its old conditions.
func (s *Service) SaveRule(ctx context.Context, owner string, id int64, r Rule) (Rule, error) {
	if err := ValidateRule(r); err != nil {
		return Rule{}, err
	}
	conditions, err := json.Marshal(r.Conditions)
	if err != nil {
		return Rule{}, fmt.Errorf("webmail: encode conditions: %w", err)
	}
	actions, err := json.Marshal(r.Actions)
	if err != nil {
		return Rule{}, fmt.Errorf("webmail: encode actions: %w", err)
	}
	enabled := r.Enabled
	// A rule that was never toggled arrives with no value to distinguish "off"
	// from "unspecified", and a new rule that is silently disabled is invisible.
	if id == 0 {
		enabled = true
	}

	if id == 0 {
		newID, err := s.rules.Create(ctx, s.db, store.MailRule{
			Owner:          owner,
			Name:           strings.TrimSpace(r.Name),
			Enabled:        enabled,
			Priority:       r.Priority,
			Conditions:     conditions,
			Actions:        actions,
			StopProcessing: r.Stop,
		})
		if err != nil {
			return Rule{}, translate(err)
		}
		id = newID
	} else {
		up := store.MailRuleUpdate{
			Name:           ptr(strings.TrimSpace(r.Name)),
			Priority:       ptr(r.Priority),
			Conditions:     conditions,
			Actions:        actions,
			StopProcessing: ptr(r.Stop),
		}
		if err := s.rules.Update(ctx, s.db, owner, id, up); err != nil {
			return Rule{}, translate(err)
		}
		_ = enabled
	}

	stored, err := s.rules.Get(ctx, s.db, owner, id)
	if err != nil {
		return Rule{}, translate(err)
	}
	return ruleFromRow(stored)
}

// SetRuleEnabled switches one rule on or off.
//
// It is separate from SaveRule because toggling is the one thing a user does to
// a rule while suspecting it, and making them resend the whole rule to flip a
// switch means a rule that cannot be parsed cannot be turned off.
func (s *Service) SetRuleEnabled(ctx context.Context, owner string, id int64, enabled bool) error {
	err := s.rules.Update(ctx, s.db, owner, id, store.MailRuleUpdate{Enabled: &enabled})
	if err != nil {
		return translate(err)
	}
	s.recordAudit(ctx, owner, "rule.enabled", fmt.Sprintf("rule %d = %v", id, enabled))
	return nil
}

// DeleteRule removes a rule.
func (s *Service) DeleteRule(ctx context.Context, owner string, id int64) error {
	err := s.rules.Delete(ctx, s.db, owner, id)
	if err != nil {
		return translate(err)
	}
	s.recordAudit(ctx, owner, "rule.delete", fmt.Sprintf("rule %d", id))
	return nil
}

func ruleFromRow(row store.MailRule) (Rule, error) {
	var conditions []RuleCondition
	if len(row.Conditions) > 0 {
		if err := json.Unmarshal(row.Conditions, &conditions); err != nil {
			return Rule{}, fmt.Errorf("webmail: rule %d has unreadable conditions: %w", row.ID, err)
		}
	}
	var actions []RuleAction
	if len(row.Actions) > 0 {
		if err := json.Unmarshal(row.Actions, &actions); err != nil {
			return Rule{}, fmt.Errorf("webmail: rule %d has unreadable actions: %w", row.ID, err)
		}
	}
	return Rule{
		ID:         row.ID,
		Name:       row.Name,
		Enabled:    row.Enabled,
		Priority:   row.Priority,
		Conditions: conditions,
		Actions:    actions,
		Stop:       row.StopProcessing,
	}, nil
}

func ptr[T any](v T) *T { return &v }