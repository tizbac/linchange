package imapd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

// Searching.
//
// A mail protocol expresses a search as syntax: IMAP gives an unevaluated token
// tree, Exchange gives AQS, and both arrive as a string from a client. Neither is
// ever handed to the database, because a syntax that reaches a query builder as
// text can reach it as anything -- the first thing it would do is turn a user's
// apostrophe into a query.
//
// What arrives at the query is a tree: a conjunction of whole queries, whose
// leaves are the conditions a client can name, combined with AND, OR and NOT to
// any depth. A tree rather than one flat conjunctive struct because that is what
// the syntaxes mean. Collapsing "from:alice OR from:bob" into two conjunctive
// fields would answer for messages from somebody called "alice bob", which is
// neither what the client asked for nor a thing a user notices until it matters.

// MailTerms is one leaf of a search: the conditions, conjoined.
//
// Every field is optional and every field is conjunctive. A leaf with nothing set
// matches every message, which is the correct answer for a search that names no
// conditions and the one a caller reaches for when it wants "just the folder".
type MailTerms struct {
	// Text is full text over the indexed columns: the subject, the sender's
	// name and address, and the snippet. It goes through plainto_tsquery, so a
	// user typing "invoice (2)" means those words rather than a group.
	Text string

	// Header matches. Each entry is compared case-insensitively as a substring
	// against the corresponding header, which is what every mail client means by
	// "from:ali".
	From    []string
	To      []string
	Cc      []string
	Bcc     []string
	Subject []string

	// MessageIDs match the Message-ID header exactly. A conversation lookup is
	// exact or it is nothing: a partial Message-ID matches unrelated messages,
	// because they are all prefixed with the same domain.
	MessageIDs []string

	// ReceivedFrom and ReceivedTo bound the internal date, which is when this
	// server received the message rather than when it was sent. A client
	// filtering on "received" is asking about arrival, which is the only arrival
	// this server can vouch for.
	ReceivedFrom time.Time
	ReceivedTo   time.Time

	// Flag filters.
	UnreadOnly bool
	Flagged    bool
	DraftsOnly bool
	// ExcludeFlagged removes flagged messages, which is what a client asking
	// for "everything important" negates.
	ExcludeFlagged bool

	// HasAttachments is a tri-state: nil does not filter.
	HasAttachments *bool

	// ItemClass is the MS-OXPROPS item class filter. It is a prefix match, since
	// a client filters on "IPM.Schedule" expecting both requests and responses.
	ItemClass string
}

// empty reports whether the leaf constrains nothing, which lets a caller drop it
// rather than emit a condition that costs the planner a scan to prove true.
func (t *MailTerms) empty() bool {
	if t == nil {
		return true
	}
	return t.Text == "" &&
		len(nonEmpty(t.From)) == 0 && len(nonEmpty(t.To)) == 0 &&
		len(nonEmpty(t.Cc)) == 0 && len(nonEmpty(t.Bcc)) == 0 &&
		len(nonEmpty(t.Subject)) == 0 && len(nonEmpty(t.MessageIDs)) == 0 &&
		t.ReceivedFrom.IsZero() && t.ReceivedTo.IsZero() &&
		!t.UnreadOnly && !t.Flagged && !t.DraftsOnly && !t.ExcludeFlagged &&
		t.HasAttachments == nil && strings.TrimSpace(t.ItemClass) == ""
}

// FilterKind is what a node of a search tree does to its children.
type FilterKind int

const (
	// FilterAll is the empty search: every message.
	FilterAll FilterKind = iota
	// FilterTerms is one leaf.
	FilterTerms
	// FilterAnd, FilterOr and FilterNot combine children.
	FilterAnd
	FilterOr
	FilterNot
)

// MailFilter is a node of a search tree.
//
// A nil filter is FilterAll, because the caller that has nothing to say should
// not have to build a tree that says it.
type MailFilter struct {
	Kind FilterKind
	// Terms is the leaf's conditions. It is set only for FilterTerms.
	Terms *MailTerms
	// Children are the sub-filters. A FilterNot has exactly one.
	Children []*MailFilter
}

// All is the filter that matches everything.
func All() *MailFilter { return &MailFilter{Kind: FilterAll} }

// Leaf builds a terms filter.
func Leaf(t *MailTerms) *MailFilter {
	if t == nil || t.empty() {
		return All()
	}
	return &MailFilter{Kind: FilterTerms, Terms: t}
}

// And, Or and Not build the combinations, dropping children that constrain
// nothing. "subject:X AND <nothing>" is "subject:X", and keeping the unconstrained
// child would make the planner prove a tautology before it could start.
func And(kids ...*MailFilter) *MailFilter {
	var keep []*MailFilter
	for _, k := range kids {
		if k == nil || k.Kind == FilterAll {
			continue
		}
		keep = append(keep, k)
	}
	switch len(keep) {
	case 0:
		return All()
	case 1:
		return keep[0]
	}
	return &MailFilter{Kind: FilterAnd, Children: keep}
}

// Or builds a disjunction. Unlike And, an empty side is not dropped: nothing OR
// something is something, but something OR nothing would be wrongly narrowed to
// nothing if the empty side were removed.
func Or(kids ...*MailFilter) *MailFilter {
	var keep []*MailFilter
	for _, k := range kids {
		if k == nil {
			continue
		}
		keep = append(keep, k)
	}
	if len(keep) == 0 {
		return All()
	}
	for _, k := range keep {
		if k.Kind != FilterAll {
			if len(keep) == 1 {
				return keep[0]
			}
			return &MailFilter{Kind: FilterOr, Children: keep}
		}
	}
	// Every side matches everything, so the disjunction matches everything.
	return All()
}

// Not builds a negation. An unconstrained child negated is nothing, which is the
// one case where dropping the child is wrong -- so it is kept and the caller
// still gets an empty answer rather than everything.
func Not(kid *MailFilter) *MailFilter {
	if kid == nil {
		return All()
	}
	return &MailFilter{Kind: FilterNot, Children: []*MailFilter{kid}}
}

// MailQuery is a search: whose, where, what, and how much of it.
//
// Owner is the only field that is not optional. A query without an owner would be
// a way to read somebody's mail by leaving out a field.
type MailQuery struct {
	// Owner is whose mail this is.
	Owner string

	// FolderIDs restricts the query to these folders. Empty means every folder
	// the owner has.
	FolderIDs []int64

	// Filter is the search tree. Nil matches everything.
	Filter *MailFilter

	// Limit bounds the result. Zero means the default, and a value above
	// MaxMailQueryLimit is clamped to it.
	Limit int

	// CursorDate and CursorID are the (internal_date, id) of the last row of the
	// previous page, newest-first keyset paging. Both are required together,
	// because the date alone is not unique.
	CursorDate time.Time
	CursorID   int64

	// Skip discards rows before the first one returned, as an offset.
	//
	// It exists for the callers that are given an offset by a protocol rather
	// than a cursor: an Exchange client's FindItem carries an Offset, and there
	// is no keyset to derive from it because the client does not have one. Keyset
	// paging is preferred where the caller can use it -- offset paging over a
	// folder that is being delivered into skips or repeats rows -- and is what
	// the cursor above does.
	Skip int

	// Ascending lists oldest first. A mail client lists newest first; the IMAP
	// sequences list oldest first and say so.
	Ascending bool

	// Count asks for the total number of matching rows as well as the page.
	Count bool
}

// MailQueryResult is one page of a MailQuery plus, when asked for, the total.
type MailQueryResult struct {
	Messages []Message
	Total    int
	// CursorDate and CursorID are the position of the last row of this page, to
	// be sent back as the next page's cursor. Zero when the page is empty.
	CursorDate time.Time
	CursorID   int64
	HasMore    bool
}

// MaxMailQueryLimit bounds one page of a MailQuery.
const MaxMailQueryLimit = MaxListLimit

// condBuilder accumulates a condition and the placeholder arguments behind it.
//
// One builder for the whole statement is what keeps the placeholders in the same
// order as the arguments: a second construction of the condition would be a second
// chance to number them differently, and the mistake would only show up as "there
// is no parameter $7" on a query with seven parameters.
type condBuilder struct {
	args  []any
	where []string
}

// arg appends a value and returns its placeholder.
func (b *condBuilder) arg(v any) string {
	b.args = append(b.args, v)
	return fmt.Sprintf("$%d", len(b.args))
}

// sql renders a filter as a boolean SQL expression.
//
// Every leaf is wrapped in COALESCE(..., false) so that a NULL on one side of a
// comparison behaves as "did not match" rather than as "unknown". Without it, a
// NOT over a nullable column silently drops rows instead of keeping them, and an
// OR whose other side was true would too. The difference is invisible on a
// populated table and wrong on exactly the rows nobody is looking at.
func (b *condBuilder) sql(f *MailFilter) string {
	switch {
	case f == nil:
		return "true"
	case f.Kind == FilterAll:
		return "true"
	case f.Kind == FilterTerms:
		conds := b.terms(f.Terms)
		if len(conds) == 0 {
			return "true"
		}
		return "COALESCE((" + strings.Join(conds, " AND ") + "), false)"
	case f.Kind == FilterAnd:
		parts := make([]string, 0, len(f.Children))
		for _, k := range f.Children {
			parts = append(parts, b.sql(k))
		}
		return "(" + strings.Join(parts, " AND ") + ")"
	case f.Kind == FilterOr:
		parts := make([]string, 0, len(f.Children))
		for _, k := range f.Children {
			parts = append(parts, b.sql(k))
		}
		return "(" + strings.Join(parts, " OR ") + ")"
	case f.Kind == FilterNot:
		if len(f.Children) == 0 {
			return "true"
		}
		return "NOT " + b.sql(f.Children[0])
	}
	return "true"
}

// terms renders one leaf as its list of conditions.
func (b *condBuilder) terms(t *MailTerms) []string {
	if t.empty() {
		return nil
	}
	var where []string

	if text := strings.TrimSpace(t.Text); text != "" {
		where = append(where, fmt.Sprintf(
			"m.search_vector @@ plainto_tsquery('english', %s)", b.arg(text)))
	}

	// A header filter is a join against the denormalised addresses rather than a
	// read of the message: a search over a folder must not read every body in it.
	// The EXISTS form keeps the row set one-per-message even when several
	// recipients match.
	headerFilter := func(column string, values []string) {
		cleaned := nonEmpty(values)
		if len(cleaned) == 0 {
			return
		}
		clauses := make([]string, 0, len(cleaned))
		for _, v := range cleaned {
			clauses = append(clauses, fmt.Sprintf(
				`(a.address ILIKE %s OR a.display_name ILIKE %s)`,
				b.arg("%"+escapeLike(v)+"%"), b.arg("%"+escapeLike(v)+"%")))
		}
		where = append(where, fmt.Sprintf(
			`EXISTS (SELECT 1 FROM message_addresses a WHERE a.message_id = m.id AND a.header_name = %s AND (%s))`,
			b.arg(column), strings.Join(clauses, " OR ")))
	}
	headerFilter("From", t.From)
	headerFilter("To", t.To)
	headerFilter("Cc", t.Cc)
	headerFilter("Bcc", t.Bcc)

	for _, v := range nonEmpty(t.Subject) {
		where = append(where, fmt.Sprintf("m.subject ILIKE %s",
			b.arg("%"+escapeLike(v)+"%")))
	}
	for _, v := range nonEmpty(t.MessageIDs) {
		where = append(where, fmt.Sprintf("m.message_id = %s",
			b.arg(strings.Trim(v, "<>"))))
	}
	if !t.ReceivedFrom.IsZero() {
		where = append(where, fmt.Sprintf("m.internal_date >= %s", b.arg(t.ReceivedFrom)))
	}
	if !t.ReceivedTo.IsZero() {
		where = append(where, fmt.Sprintf("m.internal_date <= %s", b.arg(t.ReceivedTo)))
	}
	if t.UnreadOnly {
		where = append(where, `'\Seen' <> ALL (m.flags)`)
	}
	if t.Flagged {
		where = append(where, `'\Flagged' = ANY (m.flags)`)
	}
	if t.ExcludeFlagged {
		where = append(where, `'\Flagged' <> ALL (m.flags)`)
	}
	if t.DraftsOnly {
		where = append(where, `'\Draft' = ANY (m.flags)`)
	}
	if t.HasAttachments != nil {
		where = append(where, fmt.Sprintf("m.has_attachments = %s", b.arg(*t.HasAttachments)))
	}
	if class := strings.TrimSpace(t.ItemClass); class != "" {
		// A client filters on a prefix, because it filters on "IPM.Schedule"
		// expecting both requests and responses to match.
		where = append(where, "m.item_class ILIKE "+b.arg(escapeLike(class)+"%"))
	}
	return where
}

// Query runs a MailQuery.
//
// The statement is built from placeholders and arguments, never from the caller's
// strings, and the one place a value is interpolated -- the item class prefix --
// escapes its quotes first. That is not a stylistic point: every field here comes
// from a mail client, which is exactly the kind of caller that will eventually
// type an apostrophe into a search box.
func (r *MessageRepo) Query(ctx context.Context, db *sqlx.DB, q MailQuery) (MailQueryResult, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit > MaxMailQueryLimit {
		limit = MaxMailQueryLimit
	}

	b := &condBuilder{args: []any{strings.ToLower(q.Owner)}}
	b.where = append(b.where, "b.owner = $1")

	if len(q.FolderIDs) > 0 {
		placeholders := make([]string, 0, len(q.FolderIDs))
		for _, id := range q.FolderIDs {
			placeholders = append(placeholders, b.arg(id))
		}
		b.where = append(b.where, "m.mailbox_id IN ("+strings.Join(placeholders, ",")+")")
	}
	// The filter is rendered before the cursor and the limit so that its
	// placeholders precede theirs, and the argument count is captured alongside it
	// so that the count statement -- which has neither -- is handed exactly the
	// arguments its own conditions used.
	b.where = append(b.where, b.sql(q.Filter))
	// Both marks are taken here: the number of arguments the count statement may
	// use, and the number of conditions it may have. Anything appended after this
	// point -- the cursor, the limit, the offset -- belongs to a statement the
	// count does not run.
	countArgs := len(b.args)
	countWhere := len(b.where)

	if !q.CursorDate.IsZero() {
		// Row-value comparison is one index scan and cannot straddle a tie,
		// where two clauses joined by OR can.
		b.where = append(b.where, fmt.Sprintf("(m.internal_date, m.id) < (%s, %s)",
			b.arg(q.CursorDate), b.arg(q.CursorID)))
	}

	condition := strings.Join(b.where, " AND ")
	order := "DESC"
	if q.Ascending {
		order = "ASC"
	}

	// One row beyond the page, dropped afterwards: that is how HasMore is known
	// without a second query that could disagree with this one.
	const selectColumns = `
		m.id, m.mailbox_id, m.uid, m.blob_sha256, m.size, m.internal_date, m.flags,
		m.subject, m.from_address, m.from_name, m.snippet, m.thread_key,
		COALESCE(m.message_id, '') AS message_id, m.has_attachments, m.item_class`
	query := `
		SELECT` + selectColumns + `
		FROM messages m
		JOIN mailboxes b ON b.id = m.mailbox_id
		WHERE ` + condition + `
		ORDER BY m.internal_date ` + order + `, m.id ` + order + `
		LIMIT ` + b.arg(limit+1)
	if q.Skip > 0 {
		query += ` OFFSET ` + b.arg(q.Skip)
	}

	var rows []Message
	if err := db.SelectContext(ctx, &rows, query, b.args...); err != nil {
		return MailQueryResult{}, fmt.Errorf("store: query messages: %w", err)
	}

	out := MailQueryResult{Messages: rows}
	if len(rows) > limit {
		out.HasMore = true
		out.Messages = rows[:limit]
	}
	if n := len(out.Messages); n > 0 {
		last := out.Messages[n-1]
		out.CursorDate, out.CursorID = last.InternalDate, last.ID
	}

	if q.Count {
		countQuery := `
			SELECT count(*)
			FROM messages m
			JOIN mailboxes b ON b.id = m.mailbox_id
			WHERE ` + strings.Join(b.where[:countWhere], " AND ")
		if err := db.GetContext(ctx, &out.Total, countQuery, b.args[:countArgs]...); err != nil {
			return MailQueryResult{}, fmt.Errorf("store: count messages: %w", err)
		}
	}
	return out, nil
}

// nonEmpty drops the blank entries from a filter list.
//
// A filter with a blank value matches everything, because every string contains
// the empty string. Dropping them rather than passing them down is what keeps
// "from: " from turning into "every message has this sender".
func nonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// escapeLike escapes the wildcards of a LIKE pattern.
//
// The search goes through ILIKE so that a search is case-insensitive, and ILIKE
// treats % and _ as wildcards. A user searching for "100%" is searching for the
// characters, not for anything at all.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
