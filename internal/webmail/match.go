package webmail

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/linchange/linchange/internal/mail"
	"github.com/linchange/linchange/internal/store"
)

// MatchContext is what a rule is tested against.
//
// It is a struct of already-parsed facts rather than a message, because the
// delivery path has parsed the message by the time a rule runs, and a rule that
// re-parsed the bytes would do the expensive part twice for every message.
type MatchContext struct {
	Subject   string
	Body      string
	From      []Address
	To        []Address
	Cc        []Address
	Headers   map[string][]string
	Size      int64
	HasAttachments bool
	// Attachments are the filenames, so a rule can act on what is attached
	// without the engine holding the bytes.
	Attachments []string
}

// NewMatchContext builds the facts a rule tests from a parsed message.
func NewMatchContext(parsed *mail.Parsed, size int64) MatchContext {
	ctx := MatchContext{
		Subject:        parsed.Subject(),
		Body:           parsed.Text,
		Size:           size,
		HasAttachments: len(parsed.Attachments) > 0,
		Headers:        map[string][]string{},
	}
	// A message with only an HTML body still has searchable text, or a rule on
	// the body would never match half the mail that arrives.
	if ctx.Body == "" && parsed.HTML != "" {
		ctx.Body = stripMarkup(parsed.HTML)
	}

	ctx.From = toAddresses(parsed.AddressList("From"))
	ctx.To = toAddresses(parsed.AddressList("To"))
	ctx.Cc = toAddresses(parsed.AddressList("Cc"))

	for _, h := range parsed.HeaderList() {
		ctx.Headers[strings.ToLower(h.Name)] = append(ctx.Headers[strings.ToLower(h.Name)], h.Value)
	}
	for _, a := range parsed.Attachments {
		ctx.Attachments = append(ctx.Attachments, a.Filename)
	}
	return ctx
}

func toAddresses(in []mail.Address) []Address {
	out := make([]Address, 0, len(in))
	for _, a := range in {
		out = append(out, Address{Address: a.Address, Name: a.Name})
	}
	return out
}

// RuleOutcome is what applying an owner's rules decided.
type RuleOutcome struct {
	// Destination is the folder the message should be filed in, empty when no
	// rule chose one.
	Destination string
	// Flags are the IMAP flags the rules asked for.
	Flags []string
	// Forward is an address a rule asked the message to be forwarded to.
	Forward string
	// Discard is set when a rule asked for the message to be dropped.
	Discard bool
	// Matched names the rules that fired, for the log line and for the UI's
	// "why did this land in Junk" answer.
	Matched []string
}

// ApplyRules runs an owner's enabled rules against a message.
//
// It returns a decision rather than performing it. The caller is the delivery
// path, which knows how to file a message and how to account for its quota, and
// a rule engine that moved messages itself would have to duplicate both. It also
// means the decision can be tested without a database write.
//
// Rules run in the stored order and stop at the first one that says to. A later
// rule that would move the message out of Junk is not what a user means by
// "put newsletters in a folder and stop there".
func (s *Service) ApplyRules(ctx context.Context, owner string, facts MatchContext) (RuleOutcome, error) {
	rows, err := s.rules.ListEnabled(ctx, s.db, owner)
	if err != nil {
		return RuleOutcome{}, err
	}
	return ApplyRules(rows, facts)
}

// ApplyRules evaluates rules against a message. It is separated from the method
// so that the delivery path and the tests can run it without a database.
func ApplyRules(rows []store.MailRule, facts MatchContext) (RuleOutcome, error) {
	var out RuleOutcome

	for _, row := range rows {
		rule, err := ruleFromRow(row)
		if err != nil {
			// A rule that cannot be read is skipped rather than fatal. Delivery
			// that refuses mail because one filter is malformed would turn a
			// settings mistake into an outage.
			continue
		}
		if !matches(rule, facts) {
			continue
		}
		out.Matched = append(out.Matched, rule.Name)

		stop := rule.Stop
		for _, action := range rule.Actions {
			switch action.Type {
			case "move":
				out.Destination = action.Target
				if action.Stop {
					stop = true
				}
			case "mark_read":
				out.Flags = addFlag(out.Flags, `\Seen`)
			case "mark_flagged":
				out.Flags = addFlag(out.Flags, `\Flagged`)
			case "mark_unflagged":
				out.Flags = removeFlag(out.Flags, `\Flagged`)
			case "forward":
				out.Forward = action.Target
			case "discard":
				out.Discard = true
				// Discarding is final: continuing would let a later rule file a
				// message the user asked to have dropped.
				stop = true
			}
		}
		if stop {
			break
		}
	}
	return out, nil
}

// matches reports whether every condition of a rule holds.
func matches(rule Rule, facts MatchContext) bool {
	if len(rule.Conditions) == 0 {
		return false
	}
	for _, c := range rule.Conditions {
		if !matchesOne(c, facts) {
			return false
		}
	}
	return true
}

// matchesOne evaluates a single condition.
func matchesOne(c RuleCondition, facts MatchContext) bool {
	result := evaluate(c, facts)
	if c.Negate {
		return !result
	}
	return result
}

func evaluate(c RuleCondition, facts MatchContext) bool {
	switch c.Field {
	case "from":
		return matchAddresses(c, facts.From)
	case "to":
		return matchAddresses(c, facts.To)
	case "cc":
		return matchAddresses(c, facts.Cc)
	case "subject":
		return matchString(c, facts.Subject)
	case "body":
		return matchString(c, facts.Body)
	case "attachment":
		// An attachment rule is a question about presence or about a filename.
		// "contains pdf" has to match a filename, not the whole list.
		if c.Operator == "exists" {
			return facts.HasAttachments != c.Negate
		}
		for _, name := range facts.Attachments {
			if compare(c, name) {
				return true
			}
		}
		return false
	case "header":
		values := facts.Headers[strings.ToLower(strings.TrimSpace(c.Header))]
		if c.Operator == "exists" {
			return len(values) > 0 != c.Negate
		}
		for _, v := range values {
			if compare(c, v) {
				return true
			}
		}
		return false
	case "sender_domain":
		for _, a := range facts.From {
			if _, domain, ok := strings.Cut(a.Address, "@"); ok && compare(c, domain) {
				return true
			}
		}
		return false
	case "size":
		return matchSize(c, facts.Size)
	case "is_true":
		return facts.HasAttachments
	}
	return false
}

// matchAddresses tests an address list.
//
// An address list is matched on both the address and the display name, because a
// rule written as "contains support" should catch "Support <support@example.com>"
// and a rule written as "contains example.com" should not need the domain typed
// out twice.
func matchAddresses(c RuleCondition, addrs []Address) bool {
	if c.Operator == "exists" {
		return len(addrs) > 0 != c.Negate
	}
	for _, a := range addrs {
		if compare(c, a.Address) || compare(c, a.Name) {
			return true
		}
	}
	return false
}

func matchString(c RuleCondition, value string) bool {
	if c.Operator == "exists" {
		return strings.TrimSpace(value) != "" != c.Negate
	}
	return compare(c, value)
}

func matchSize(c RuleCondition, size int64) bool {
	want, err := strconv.ParseInt(strings.TrimSpace(c.Value), 10, 64)
	if err != nil {
		// An unparseable size never matches rather than matching everything.
		return false
	}
	switch c.Operator {
	case "greater_than":
		return size > want
	case "less_than":
		return size < want
	default:
		return size == want
	}
}

// compare applies one operator to one string.
func compare(c RuleCondition, value string) bool {
	needle, haystack := c.Value, value
	if !c.CaseSensitive {
		needle, haystack = strings.ToLower(needle), strings.ToLower(haystack)
	}

	switch c.Operator {
	case "contains":
		return strings.Contains(haystack, needle)
	case "is":
		return strings.TrimSpace(haystack) == strings.TrimSpace(needle)
	case "starts_with":
		return strings.HasPrefix(haystack, needle)
	case "ends_with":
		return strings.HasSuffix(haystack, needle)
	case "matches":
		re, err := compileMatch(c)
		if err != nil {
			return false
		}
		return re.MatchString(value)
	case "exists":
		return strings.TrimSpace(value) != ""
	}
	return false
}

// matchCache memoises compiled patterns by their rule text.
//
// A rule set is evaluated on every message, and the regex compile is the most
// expensive thing in the engine. The cache is bounded by the number of distinct
// patterns a deployment actually uses, which is small.
var matchCache = struct {
	patterns map[string]*regexp.Regexp
}{patterns: map[string]*regexp.Regexp{}}

// maxMatchPatternLength bounds a user-supplied pattern.
const maxMatchPatternLength = 200

// compileMatch compiles a regular expression from a rule condition.
func compileMatch(c RuleCondition) (*regexp.Regexp, error) {
	pattern := c.Value
	if len(pattern) > maxMatchPatternLength {
		return nil, fmt.Errorf("the pattern is %d characters, over the %d limit", len(pattern), maxMatchPatternLength)
	}
	if !c.CaseSensitive {
		pattern = "(?i)" + pattern
	}

	key := pattern
	if re, ok := matchCache.patterns[key]; ok {
		return re, nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("%q is not a usable pattern: %w", c.Value, err)
	}
	if len(matchCache.patterns) < 512 {
		matchCache.patterns[key] = re
	}
	return re, nil
}

// stripMarkup reduces an HTML body to its text.
//
// It is a rough reduction on purpose: the alternative is keeping the HTML and
// matching a rule against markup, which means a rule on "invoice" misses a
// message whose subject only says <b>invoice</b>.
func stripMarkup(in string) string {
	var b strings.Builder
	depth := 0
	for _, r := range in {
		switch {
		case r == '<':
			depth++
		case r == '>':
			if depth > 0 {
				depth--
			}
			b.WriteRune(' ')
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func addFlag(flags []string, flag string) []string {
	for _, f := range flags {
		if f == flag {
			return flags
		}
	}
	return append(flags, flag)
}

func removeFlag(flags []string, flag string) []string {
	out := flags[:0]
	for _, f := range flags {
		if f != flag {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return []string{}
	}
	return out
}