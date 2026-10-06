package ews

import (
	"fmt"
	"strings"
	"time"

	"github.com/linchange/linchange/internal/imapd"
)

// AQS.
//
// FindItem carries a query string in the syntax Exchange calls AQS
// (MS-OXCSEARCH §2.6): terms separated by AND, OR and NOT, grouped by
// parentheses, each term either a bare word or a `field:value`.
//
// The parser is written out rather than regular-expressed because the grammar is
// recursive and because the value of a term can contain anything: a search for
// `subject:"(budget) 2024"` is a word, a parenthesis and a year, and matching it
// against a pattern that also has to know about the grouping would mean the two
// had to agree about where one ended. A parser cannot lose track of that, and the
// reason to write one is that the string being parsed arrives from whoever is
// looking at the mailbox, which is not a reason to trust it.
//
// What it produces is a tree of imapd filters. Every combination the grammar
// expresses is one the store can answer, so nothing is dropped: a query that
// cannot be compiled fails the request with a message naming it, rather than
// silently answering a different, narrower query than the one that was asked for.

// aqsQuery is the compiled form of a query string.
type aqsQuery struct {
	// filter is the search tree.
	filter *imapd.MailFilter
	// window is the date range the `received:` and `sent:` terms named. It is
	// kept apart from the filter because a range is the one term that has to be
	// intersected with the view FindItem was given rather than conjoined with
	// it: two ranges mean both, where two filters would mean neither.
	window aqsWindow
}

// aqsWindow is an inclusive range of internal dates. A zero bound means open.
type aqsWindow struct {
	from time.Time
	to   time.Time
}

// ---------------------------------------------------------------------------
// Lexer
// ---------------------------------------------------------------------------

type aqsKind int

const (
	// aqsEOF is the end of the query.
	aqsEOF aqsKind = iota
	// aqsWord is a bare word or a quoted string.
	aqsWord
	// aqsOpen and aqsClose are the grouping parentheses.
	aqsOpen
	aqsClose
	// aqsAnd, aqsOr and aqsNot are the operators.
	aqsAnd
	aqsOr
	aqsNot
	// aqsCmp is one of the comparison operators: `:`, `=`, `>=`, `<=`, `>`, `<`.
	// They are one token because they all separate a field from a value and the
	// parser's job is the same for each; only the date fields care which one it
	// was, and they read it back from the token's text.
	aqsCmp
)

// aqsToken is one lexical unit of a query string.
type aqsToken struct {
	kind aqsKind
	// text is the token's value: the word, the operator, or the empty string.
	text string
}

// aqsLexer walks a query string.
//
// The reader is an index rather than a state machine over runes, because every
// decision here is about what the next character is and an index answers that
// without allocating.
type aqsLexer struct {
	src string
	pos int
}

// next returns the next token.
//
// A quoted string is a word like any other, because that is how the grammar
// treats it: `subject:"a b"` is two tokens' worth of text carrying one value, and
// a lexer that returned the quotes would make the parser split a phrase it was
// told not to split.
func (l *aqsLexer) next() (aqsToken, error) {
	for l.pos < len(l.src) && isSpace(l.src[l.pos]) {
		l.pos++
	}
	if l.pos >= len(l.src) {
		return aqsToken{kind: aqsEOF}, nil
	}
	switch c := l.src[l.pos]; {
	case c == '(':
		l.pos++
		return aqsToken{kind: aqsOpen, text: "("}, nil
	case c == ')':
		l.pos++
		return aqsToken{kind: aqsClose, text: ")"}, nil
	case c == '"' || c == '\'':
		return l.quoted(c)
	case c == ':' || c == '=' || c == '>' || c == '<':
		// A bare comparison with no field in front of it, which arrives as
		// `:value` when a client strips a namespace it thinks it does not need,
		// and as `>=2024-01-01` for a bare date. Returning it as a comparison
		// rather than a word is what makes the parser report the missing field
		// instead of reading the operator as part of a search for nothing.
		return l.comparison(), nil
	case c == '-' && l.standsAloneMinus():
		// A minus followed by nothing is the NOT operator. A minus inside a
		// word -- `2024-01-01`, `-urgent` -- is part of the word, which is why
		// this is a separate question rather than a rule about hyphens.
		l.pos++
		return aqsToken{kind: aqsNot, text: "-"}, nil
	}
	return l.word()
}

// standsAloneMinus reports whether the character at pos is a minus the parser
// should treat as an operator.
func (l *aqsLexer) standsAloneMinus() bool {
	// The next character is still l.src[l.pos] == '-', so the one after decides:
	// a minus followed by a letter or a digit is part of a word.
	if l.pos+1 >= len(l.src) {
		return true
	}
	next := l.src[l.pos+1]
	return isSpace(next) || next == '(' || next == ')'
}

// comparison reads a comparison operator as a single token, longest first:
// `>=` before `>` and `<=` before `<`, or the meaning of the two-sided form
// would be lost to the prefix. The colon and the equals are the other two
// comparison spellings and are single-character tokens already.
func (l *aqsLexer) comparison() aqsToken {
	if l.pos+1 < len(l.src) {
		switch l.src[l.pos : l.pos+2] {
		case ">=", "<=":
			op := l.src[l.pos : l.pos+2]
			l.pos += 2
			return aqsToken{kind: aqsCmp, text: op}
		}
	}
	op := string(l.src[l.pos])
	l.pos++
	return aqsToken{kind: aqsCmp, text: op}
}

// quoted reads a quoted string; the quotes are not part of the value.
func (l *aqsLexer) quoted(quote byte) (aqsToken, error) {
	l.pos++
	start := l.pos
	for l.pos < len(l.src) && l.src[l.pos] != quote {
		l.pos++
	}
	if l.pos >= len(l.src) {
		return aqsToken{}, fmt.Errorf("a quoted value is missing its closing %c", quote)
	}
	text := l.src[start:l.pos]
	l.pos++
	return aqsToken{kind: aqsWord, text: text}, nil
}

// word reads a run of characters up to the next separator.
//
// The special characters are exactly the ones that have meaning elsewhere in the
// grammar, so a word stops at a colon it is the field of, at a parenthesis it is
// not inside, and at a quote it did not start. What it does not stop at is a
// hyphen inside it, because a date is the most common thing typed with hyphens.
func (l *aqsLexer) word() (aqsToken, error) {
	start := l.pos
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if isSpace(c) || c == '(' || c == ')' || c == ':' || c == '=' ||
			c == '"' || c == '\'' {
			break
		}
		if c == '>' || c == '<' {
			break
		}
		l.pos++
	}
	if l.pos == start {
		return aqsToken{}, fmt.Errorf("unexpected character %q in the query", l.src[l.pos])
	}
	word := l.src[start:l.pos]
	// An operator is an operator only when it stands alone: `orange` is a word,
	// `OR` is a word, and only a token between two terms is the operator. The
	// parser is what knows whether the token is between two terms, so the
	// distinction is made there rather than here.
	return aqsToken{kind: aqsWord, text: word}, nil
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

// ---------------------------------------------------------------------------
// Parser
// ---------------------------------------------------------------------------

// aqsParser is a recursive-descent parser over the token stream.
//
// Precedence is the one the grammar gives: NOT binds tighter than AND, which
// binds tighter than OR. Writing the three levels out is what makes
// `a OR b AND c` mean `a OR (b AND c)` and not `(a OR b) AND c`, which a single
// left-to-right loop would get wrong for every query with more than two terms.
type aqsParser struct {
	lex aqsLexer
	tok aqsToken
	// bare collects the words that arrived with no field, which are one text
	// search rather than one search per word.
	bare []string
	// win is the accumulated date window.
	win aqsWindow
	// prev is the previous token's kind, which is how a bare AND/OR/NOT is
	// distinguished from one standing between two terms.
	prev aqsKind
	// first records that no token has been read yet.
	first bool
}

// compileQuery parses a query string into a store filter.
//
// An empty string is not an error: it is a caller that named no conditions, which
// is the same answer as "everything". A query that parses but names nothing --
// `AND`, or `()` -- is also everything rather than an error, because a client
// sends it when the user is halfway through typing.
func compileQuery(raw string) (*aqsQuery, *soapError) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return &aqsQuery{filter: imapd.All()}, nil
	}

	p := &aqsParser{lex: aqsLexer{src: raw}, first: true}
	if err := p.advance(); err != nil {
		return nil, aqsFault(err)
	}
	filter, err := p.parseOr()
	if err != nil {
		return nil, aqsFault(err)
	}
	if p.tok.kind != aqsEOF {
		return nil, aqsFault(fmt.Errorf("the query has an unexpected %q after it", p.tok.text))
	}

	// Bare words are one text search over the indexed columns. They are combined
	// rather than sent one at a time so that "invoice 2024" means both words,
	// which is what a phrase the user did not quote means.
	if len(p.bare) > 0 {
		leaf := imapd.Leaf(&imapd.MailTerms{Text: strings.Join(p.bare, " ")})
		switch {
		case filter == nil, filter.Kind == imapd.FilterAll:
			filter = leaf
		default:
			filter = imapd.And(filter, leaf)
		}
	}
	if filter == nil {
		filter = imapd.All()
	}
	return &aqsQuery{filter: filter, window: p.win}, nil
}

// aqsFault reports a query that could not be parsed.
//
// The status is the same one every other request failure uses. A client that sent
// a query this server cannot read is told which query, so the user can retype it;
// being told "no results" instead would be a search box that quietly ignores part
// of what was typed, which is worse than an error because it looks like an answer.
func aqsFault(err error) *soapError {
	return errorf("FindItemResponse", codeErrorInvalidRequest, 200,
		"the query could not be read: %v", err)
}

// advance reads the next token, remembering the one it replaces.
func (p *aqsParser) advance() error {
	if p.first {
		p.first = false
	} else {
		p.prev = p.tok.kind
	}
	tok, err := p.lex.next()
	if err != nil {
		return err
	}
	// AND, OR and NOT are operators only between terms. `orange` and `notable`
	// are words; only a token with a term on either side of it is the operator,
	// and that is a question about the neighbours rather than about the token.
	if tok.kind == aqsWord {
		switch strings.ToLower(tok.text) {
		case "and":
			if p.termOnTheLeft() && p.termComing() {
				tok.kind = aqsAnd
			}
		case "or":
			if p.termOnTheLeft() && p.termComing() {
				tok.kind = aqsOr
			}
		case "not":
			if p.termComing() {
				tok.kind = aqsNot
			}
		}
	}
	p.tok = tok
	return nil
}

// termOnTheLeft reports whether the token before the current one was a value.
func (p *aqsParser) termOnTheLeft() bool {
	return p.prev == aqsWord || p.prev == aqsClose
}

// termComing peeks at the next token to decide whether an operator has a right
// side. The lexer is copied so that a look-ahead that decides against reading the
// token does not consume it.
func (p *aqsParser) termComing() bool {
	peek := p.lex
	for {
		tok, err := peek.next()
		if err != nil {
			return false
		}
		switch tok.kind {
		case aqsEOF:
			return false
		case aqsWord, aqsOpen, aqsNot:
			return true
		}
		return false
	}
}

// parseOr parses a disjunction, or a single term when there is no OR.
func (p *aqsParser) parseOr() (*imapd.MailFilter, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	var terms []*imapd.MailFilter
	if left != nil {
		terms = append(terms, left)
	}
	for p.tok.kind == aqsOr {
		if err := p.advance(); err != nil {
			return nil, err
		}
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		terms = append(terms, right)
	}
	switch len(terms) {
	case 0:
		return nil, nil
	case 1:
		return terms[0], nil
	}
	return imapd.Or(terms...), nil
}

// parseAnd parses a conjunction.
//
// AND may be omitted: `from:alice to:bob` is a conjunction in every search syntax
// a user has ever typed, and refusing it would be the strictest reading of a
// grammar no client follows literally.
func (p *aqsParser) parseAnd() (*imapd.MailFilter, error) {
	left, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	var terms []*imapd.MailFilter
	if left != nil {
		terms = append(terms, left)
	}
	for {
		if p.tok.kind == aqsAnd {
			if err := p.advance(); err != nil {
				return nil, err
			}
			if p.tok.kind == aqsEOF || p.tok.kind == aqsClose || p.tok.kind == aqsOr {
				break
			}
		} else if p.tok.kind != aqsWord && p.tok.kind != aqsNot && p.tok.kind != aqsOpen {
			break
		}
		right, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		if right == nil {
			break
		}
		terms = append(terms, right)
	}
	switch len(terms) {
	case 0:
		return nil, nil
	case 1:
		return terms[0], nil
	}
	return imapd.And(terms...), nil
}

// parseNot parses a negation, a parenthesised expression, or a term.
func (p *aqsParser) parseNot() (*imapd.MailFilter, error) {
	if p.tok.kind == aqsNot {
		if err := p.advance(); err != nil {
			return nil, err
		}
		inner, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		if inner == nil {
			return nil, nil
		}
		return imapd.Not(inner), nil
	}
	if p.tok.kind == aqsOpen {
		if err := p.advance(); err != nil {
			return nil, err
		}
		inner, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if p.tok.kind != aqsClose {
			return nil, fmt.Errorf("a group is missing its closing parenthesis")
		}
		if err := p.advance(); err != nil {
			return nil, err
		}
		return inner, nil
	}
	if p.tok.kind != aqsWord {
		return nil, nil
	}
	return p.parseTerm()
}

// parseTerm reads `field:value`, `field=value`, `field>=value` or a bare word.
func (p *aqsParser) parseTerm() (*imapd.MailFilter, error) {
	field := p.tok.text
	if err := p.advance(); err != nil {
		return nil, err
	}
	if p.tok.kind != aqsCmp {
		// A bare word. It is collected rather than compiled here so that a query
		// of only bare words becomes one text search rather than one search per
		// word, which would require every word to match rather than any of them.
		p.bare = append(p.bare, field)
		return nil, nil
	}
	op := p.tok
	if err := p.advance(); err != nil {
		return nil, err
	}
	if p.tok.kind != aqsWord {
		return nil, fmt.Errorf("%q is not followed by a value", field)
	}
	value := p.tok.text
	if err := p.advance(); err != nil {
		return nil, err
	}
	return p.buildTerm(field, op.text, value)
}

// buildTerm turns one comparison into a filter.
//
// The vocabulary is the one MS-OXCSEARCH documents for AQS. A field this server
// does not have is an error naming it rather than a term that matches nothing: a
// client that asked for `category:Urgent` and was quietly given no results would
// have no way to learn that the field does not exist here.
func (p *aqsParser) buildTerm(field, op, value string) (*imapd.MailFilter, error) {
	name := strings.ToLower(strings.TrimSpace(field))
	// A field may carry its own negation, which is how a client that has
	// rewritten a query writes one.
	negated := strings.HasPrefix(name, "-") || strings.HasPrefix(name, "!")
	name = strings.TrimLeft(name, "-!")
	flip := func(f *imapd.MailFilter) *imapd.MailFilter {
		if negated {
			return imapd.Not(f)
		}
		return f
	}
	// A term whose value the client negated with the operator, as in `!~`.
	strict := func() error {
		if negated {
			return fmt.Errorf("%s cannot be negated", field)
		}
		return nil
	}

	switch name {
	case "subject":
		return flip(imapd.Leaf(&imapd.MailTerms{Subject: []string{value}})), nil
	case "from", "sender":
		return flip(imapd.Leaf(&imapd.MailTerms{From: []string{value}})), nil
	case "to", "recipients":
		return flip(imapd.Leaf(&imapd.MailTerms{To: []string{value}})), nil
	case "cc":
		return flip(imapd.Leaf(&imapd.MailTerms{Cc: []string{value}})), nil
	case "bcc":
		return flip(imapd.Leaf(&imapd.MailTerms{Bcc: []string{value}})), nil
	case "body", "content", "preview", "freetext":
		return flip(imapd.Leaf(&imapd.MailTerms{Text: value})), nil
	case "messageid":
		return flip(imapd.Leaf(&imapd.MailTerms{MessageIDs: []string{value}})), nil
	case "itemclass":
		return flip(imapd.Leaf(&imapd.MailTerms{ItemClass: value})), nil
	case "attachment", "hasattachment", "hasattachments":
		wants, ok := aqsBool(value)
		if !ok {
			return nil, fmt.Errorf("%s:%s is not a yes or a no", field, value)
		}
		return flip(imapd.Leaf(&imapd.MailTerms{HasAttachments: &wants})), nil
	case "isread":
		if err := strict(); err != nil {
			return nil, err
		}
		wants, ok := aqsBool(value)
		if !ok {
			return nil, fmt.Errorf("isread:%s is not a yes or a no", value)
		}
		// isread is the negation of unread. Building it that way rather than
		// adding a second flag to the store is what keeps one flag and one
		// answer, instead of two columns that can disagree after a flag change
		// over a protocol that does not know about the second one.
		unread := imapd.Leaf(&imapd.MailTerms{UnreadOnly: true})
		if wants {
			return imapd.Not(unread), nil
		}
		return unread, nil
	case "isflagged", "importance":
		if err := strict(); err != nil {
			return nil, err
		}
		wants, ok := aqsBool(value)
		if !ok {
			wants = strings.EqualFold(value, "high")
		}
		flagged := imapd.Leaf(&imapd.MailTerms{Flagged: true})
		if wants {
			return flagged, nil
		}
		return imapd.Not(flagged), nil
	case "isdraft":
		if err := strict(); err != nil {
			return nil, err
		}
		wants, ok := aqsBool(value)
		if !ok {
			return nil, fmt.Errorf("isdraft:%s is not a yes or a no", value)
		}
		drafts := imapd.Leaf(&imapd.MailTerms{DraftsOnly: true})
		if wants {
			return drafts, nil
		}
		return imapd.Not(drafts), nil
	case "received", "receivedrange", "sent", "date":
		// A message's sent date is its internal date here: this server has no
		// other record of when a sender wrote it, and the two differ by the
		// delay between writing and delivery, which is not a difference a search
		// over a mailbox can act on.
		if err := strict(); err != nil {
			return nil, err
		}
		from, to, err := aqsRange(op, value)
		if err != nil {
			return nil, fmt.Errorf("%s%s%s is not a date or a date range: %w",
				field, op, value, err)
		}
		p.mergeWindow(from, to)
		return flip(imapd.All()), nil
	case "folder", "in":
		return nil, fmt.Errorf(
			"this server searches the folder FindItem named, so %s cannot be combined with it", field)
	}
	return nil, fmt.Errorf("%s is not a query field this server understands", field)
}

// mergeWindow intersects the window with another one.
//
// Intersect rather than union: a search over one range that is then narrowed by a
// second is asking for both. Two disjoint ranges are an empty window, which is
// the honest answer and the one that stops a widening merge from returning
// messages the user asked to exclude.
func (p *aqsParser) mergeWindow(from, to time.Time) {
	if !from.IsZero() && (p.win.from.IsZero() || from.After(p.win.from)) {
		p.win.from = from
	}
	if !to.IsZero() && (p.win.to.IsZero() || to.Before(p.win.to)) {
		p.win.to = to
	}
}

// aqsBool reads the yes/no spellings clients use.
func aqsBool(v string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "yes", "1", "on", "y":
		return true, true
	case "false", "no", "0", "off", "n":
		return false, true
	}
	return false, false
}

// aqsRange reads a date under the comparison the client chose, or the two forms
// of range it sends: `2024-01-01..2024-01-31`, `2024-01-01..`, `..2024-01-31`.
//
// The bounds are the start of the first day and the end of the last, because a
// search for "messages from January" is a search over whole days and a half-open
// range taken literally would stop at midnight on the first.
func aqsRange(op, v string) (time.Time, time.Time, error) {
	v = strings.TrimSpace(v)
	switch op {
	case ">=":
		d, err := aqsDate(v)
		return d, time.Time{}, err
	case "<=":
		d, err := aqsDate(v)
		return time.Time{}, d, err
	case ">":
		d, err := aqsDate(v)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		if d.IsZero() {
			return d, time.Time{}, nil
		}
		return d.AddDate(0, 0, 1), time.Time{}, nil
	case "<":
		d, err := aqsDate(v)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		if d.IsZero() {
			return time.Time{}, d, nil
		}
		return time.Time{}, d.AddDate(0, 0, -1), nil
	}
	if i := strings.Index(v, ".."); i >= 0 {
		from, err := aqsDate(v[:i])
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		to, err := aqsDate(v[i+2:])
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		return from, to, nil
	}
	d, err := aqsDate(v)
	return d, d, err
}

// aqsDate reads a date in the formats the syntax allows, expanding a partially
// specified one: a year bounds the whole year, a month the whole month.
//
// A partial date is expanded rather than refused because every client sends one --
// `received:2024` is how a user narrows a search to a year -- and refusing it
// would leave a search box that fails on the most common thing typed into it.
func aqsDate(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, nil
	}
	now := time.Now().UTC()
	switch {
	case len(v) == 4 && allDigits(v):
		return time.Date(aqsAtoi(v), 1, 1, 0, 0, 0, 0, time.UTC), nil
	case len(v) == 7 && v[4] == '-' && allDigits(v[:4]) && allDigits(v[5:]):
		return time.Date(aqsAtoi(v[:4]), time.Month(aqsAtoi(v[5:])), 1,
			0, 0, 0, 0, time.UTC), nil
	case len(v) == 10 && v[4] == '-' && v[7] == '-' &&
		allDigits(v[:4]) && allDigits(v[5:7]) && allDigits(v[8:]):
		return time.Date(aqsAtoi(v[:4]), time.Month(aqsAtoi(v[5:7])), aqsAtoi(v[8:]),
			0, 0, 0, 0, time.UTC), nil
	}
	// The words the syntaxes allow for "now".
	switch strings.ToLower(v) {
	case "now":
		return now, nil
	case "today":
		return aqsStartOfDay(now), nil
	case "yesterday":
		return aqsStartOfDay(now).AddDate(0, 0, -1), nil
	}
	return aqsRelative(v, now)
}

// aqsRelative reads an offset like `-3d`, `+2w`, `-6m`, `-1y`.
//
// Only day and week offsets are exact; month and year are approximated by days
// for the ones that are not years, and years are calendar years, because a search
// for "last year" is a search for a year and not for 365 days that happen to end
// in December.
func aqsRelative(v string, now time.Time) (time.Time, error) {
	if len(v) < 2 {
		return time.Time{}, fmt.Errorf("%q is not a date", v)
	}
	direction := 1
	body := v
	switch v[0] {
	case '-':
		direction = -1
		body = v[1:]
	case '+':
		body = v[1:]
	}
	if len(body) < 2 {
		return time.Time{}, fmt.Errorf("%q is not a date", v)
	}
	unit := body[len(body)-1]
	magnitude, err := aqsInt(body[:len(body)-1])
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not a date", v)
	}
	magnitude *= direction
	switch unit {
	case 'd', 'D':
		return now.AddDate(0, 0, magnitude), nil
	case 'w', 'W':
		return now.AddDate(0, 0, 7*magnitude), nil
	case 'm', 'M':
		return now.AddDate(0, magnitude, 0), nil
	case 'y', 'Y':
		if body == string(unit) {
			magnitude = direction
		}
		return time.Date(now.Year()+magnitude, 1, 1, 0, 0, 0, 0, time.UTC), nil
	}
	return time.Time{}, fmt.Errorf("%q is not a date", v)
}

// aqsStartOfDay is midnight UTC of the day t falls in.
func aqsStartOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func aqsAtoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func aqsInt(s string) (int, error) {
	if !allDigits(s) {
		return 0, fmt.Errorf("%q is not a number", s)
	}
	return aqsAtoi(s), nil
}
