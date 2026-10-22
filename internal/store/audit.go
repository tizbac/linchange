package store

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"time"
)

// AuditEntry is one recorded action. The log is append-only and is the record
// of who did what to whose data, which is the only way an administrator's
// access to other people's mail is ever accountable.
type AuditEntry struct {
	ID            int64           `db:"id"`
	ActorAddress  *string         `db:"actor_address"`
	TargetAddress *string         `db:"target_address"`
	Action        string          `db:"action"`
	TargetType    string          `db:"target_type"`
	TargetID      string          `db:"target_id"`
	IP            *netip.Addr     `db:"ip"`
	Detail        json.RawMessage `db:"detail"`
	CreatedAt     time.Time       `db:"created_at"`
}

// AuditFilter narrows a log query. The zero value matches everything, so a
// caller only has to name the columns it cares about.
type AuditFilter struct {
	Actor  string
	Target string
	Action string
	// Search is a free-text match across every field an entry carries: actor,
	// target, action, target type and id, IP, the detail JSON and the
	// timestamp. It is what the administrator's single search box means.
	Search string
	Since  *time.Time
	Until  *time.Time
	Limit  int
	Offset int
}

// AuditLog is append-only. There is deliberately no Update or Delete on the
// repository: a record that can be edited is not a record.
type AuditLog struct{}

// NewAuditLog returns the repository. It is stateless.
func NewAuditLog() *AuditLog { return &AuditLog{} }

const defaultAuditLimit = 100
const maxAuditLimit = 1000

// Record appends an entry. An empty detail is stored as an empty object rather
// than SQL NULL, so a reader never has to handle both.
func (a *AuditLog) Record(ctx context.Context, qr Querier, entry AuditEntry) (int64, error) {
	detail := entry.Detail
	if len(detail) == 0 {
		detail = json.RawMessage(`{}`)
	}
	var ip any
	if entry.IP != nil {
		ip = entry.IP.String()
	}

	var id int64
	err := qr.GetContext(ctx, &id, `
		INSERT INTO audit_log (actor_address, target_address, action,
		                       target_type, target_id, ip, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id`,
		entry.ActorAddress, entry.TargetAddress, entry.Action,
		entry.TargetType, entry.TargetID, ip, detail)
	if err != nil {
		return 0, fmt.Errorf("store: writing audit entry: %w", err)
	}
	return id, nil
}

const auditSelect = `
	SELECT id, actor_address, target_address, action, target_type, target_id,
	       ip, detail, created_at
	FROM audit_log`

// auditWhere builds the shared WHERE clause. Unmatched filters are left out of
// the query entirely rather than compared against NULL, so the indexes on each
// column are actually usable. The free-text search is deliberately a single
// ILIKE over every field: an administrator reading the log does not know which
// column the value is in, and correctness is worth more here than a scan.
func auditWhere(f AuditFilter) (string, []any) {
	var (
		where []string
		args  []any
	)
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if f.Search != "" {
		args = append(args, "%"+escapeLike(f.Search)+"%")
		n := len(args)
		where = append(where, fmt.Sprintf(
			`(actor_address ILIKE $%d ESCAPE '\'
			  OR target_address ILIKE $%d ESCAPE '\'
			  OR action ILIKE $%d ESCAPE '\'
			  OR target_type ILIKE $%d ESCAPE '\'
			  OR target_id ILIKE $%d ESCAPE '\'
			  OR COALESCE(ip::text, '') ILIKE $%d ESCAPE '\'
			  OR detail::text ILIKE $%d ESCAPE '\'
			  OR id::text ILIKE $%d ESCAPE '\'
			  OR created_at::text ILIKE $%d ESCAPE '\')`,
			n, n, n, n, n, n, n, n, n))
	}
	if f.Actor != "" {
		add("actor_address = $%d", f.Actor)
	}
	if f.Target != "" {
		add("target_address = $%d", f.Target)
	}
	if f.Action != "" {
		add("action = $%d", f.Action)
	}
	if f.Since != nil {
		add("created_at >= $%d", *f.Since)
	}
	if f.Until != nil {
		add("created_at < $%d", *f.Until)
	}
	if len(where) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(where, " AND "), args
}

// List returns entries newest first, filtered and paged.
func (a *AuditLog) List(ctx context.Context, qr Querier, f AuditFilter) ([]AuditEntry, error) {
	where, args := auditWhere(f)
	query := auditSelect + where + " ORDER BY created_at DESC, id DESC"

	limit := f.Limit
	if limit <= 0 {
		limit = defaultAuditLimit
	}
	if limit > maxAuditLimit {
		limit = maxAuditLimit
	}
	args = append(args, limit)
	query += fmt.Sprintf(" LIMIT $%d", len(args))
	if f.Offset > 0 {
		args = append(args, f.Offset)
		query += fmt.Sprintf(" OFFSET $%d", len(args))
	}

	rows, err := qr.QueryxContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing audit entries: %w", err)
	}
	defer rows.Close()

	out := make([]AuditEntry, 0, 16)
	for rows.Next() {
		var row auditRow
		if err := rows.StructScan(&row); err != nil {
			return nil, fmt.Errorf("store: scanning audit entry: %w", err)
		}
		out = append(out, row.entry())
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterating audit entries: %w", err)
	}
	return out, nil
}

// Count returns how many entries match, ignoring Limit and Offset. The audit
// page needs it to say "1-50 of 173" without loading the other 123 rows.
func (a *AuditLog) Count(ctx context.Context, qr Querier, f AuditFilter) (int64, error) {
	where, args := auditWhere(f)
	var n int64
	if err := qr.GetContext(ctx, &n, `SELECT count(*) FROM audit_log`+where, args...); err != nil {
		return 0, fmt.Errorf("store: counting audit entries: %w", err)
	}
	return n, nil
}

// Walk visits every matching entry newest first and never applies a LIMIT.
// It is the export path: a log can be larger than the server's memory, so the
// caller writes each row as it arrives.
func (a *AuditLog) Walk(ctx context.Context, qr Querier, f AuditFilter, fn func(AuditEntry) error) error {
	where, args := auditWhere(f)
	query := auditSelect + where + " ORDER BY created_at DESC, id DESC"
	rows, err := qr.QueryxContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("store: listing audit entries: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var row auditRow
		if err := rows.StructScan(&row); err != nil {
			return fmt.Errorf("store: scanning audit entry: %w", err)
		}
		if err := fn(row.entry()); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: iterating audit entries: %w", err)
	}
	return nil
}

// CountFor reports how many entries an action has produced, which is how the
// security view answers "how often has this been used" without paging the
// whole log.
func (a *AuditLog) CountFor(ctx context.Context, qr Querier, action string) (int64, error) {
	var n int64
	if err := qr.GetContext(ctx, &n,
		`SELECT count(*) FROM audit_log WHERE action = $1`, action); err != nil {
		return 0, fmt.Errorf("store: counting audit entries: %w", err)
	}
	return n, nil
}

// auditRow carries the inet column as a string, which lib/pq returns for an
// inet, and the jsonb as raw bytes.
type auditRow struct {
	ID            int64     `db:"id"`
	ActorAddress  *string   `db:"actor_address"`
	TargetAddress *string   `db:"target_address"`
	Action        string    `db:"action"`
	TargetType    string    `db:"target_type"`
	TargetID      string    `db:"target_id"`
	IP            *string   `db:"ip"`
	Detail        []byte    `db:"detail"`
	CreatedAt     time.Time `db:"created_at"`
}

func (r auditRow) entry() AuditEntry {
	entry := AuditEntry{
		ID:            r.ID,
		ActorAddress:  r.ActorAddress,
		TargetAddress: r.TargetAddress,
		Action:        r.Action,
		TargetType:    r.TargetType,
		TargetID:      r.TargetID,
		Detail:        json.RawMessage(r.Detail),
		CreatedAt:     r.CreatedAt,
	}
	if entry.Detail == nil {
		entry.Detail = json.RawMessage(`{}`)
	}
	if r.IP != nil {
		if addr, err := netip.ParseAddr(*r.IP); err == nil {
			entry.IP = &addr
		}
	}
	return entry
}
