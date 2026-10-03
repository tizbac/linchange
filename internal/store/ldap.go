package store

import (
	"context"
	"fmt"
	"time"
)

// LDAPLink is the binding between a local principal and the directory entry
// that backs it. The principal is the primary key: one address is linked from
// at most one provider, which is what lets the login path find the directory
// that checks a user's password by looking the address up once.
type LDAPLink struct {
	Address     string    `db:"address"`
	Provider    string    `db:"provider"`
	DN          string    `db:"dn"`
	LastSeenAt  time.Time `db:"last_seen_at"`
	DisplayName string    `db:"display_name"`
	MemberOf    []string  `db:"member_of"`
	SyncedAt    time.Time `db:"synced_at"`
}

// LDAPLinks reads and writes the ldap_links table.
type LDAPLinks struct{}

// NewLDAPLinks returns the repository. It is stateless, so this is free.
func NewLDAPLinks() *LDAPLinks { return &LDAPLinks{} }

// Get returns the link for an address, or ErrNotFound when the principal is
// not directory-backed.
func (l *LDAPLinks) Get(ctx context.Context, q Querier, address string) (LDAPLink, error) {
	var row LDAPLink
	if err := q.GetContext(ctx, &row, `
		SELECT address, provider, dn, last_seen_at, display_name, member_of, synced_at
		FROM ldap_links WHERE address = $1`, address); err != nil {
		if IsNotFound(err) {
			return LDAPLink{}, ErrNotFound
		}
		return LDAPLink{}, err
	}
	return row, nil
}

// Upsert records that the directory reported this entry at now. It is
// idempotent: a second sync that sees the same entry refreshes the observed
// attributes and moves the timestamp, and never duplicates.
func (l *LDAPLinks) Upsert(ctx context.Context, q Querier, link LDAPLink, now time.Time) error {
	link.LastSeenAt = now
	link.SyncedAt = now
	if link.MemberOf == nil {
		link.MemberOf = []string{}
	}
	const query = `
		INSERT INTO ldap_links (address, provider, dn, last_seen_at, display_name, member_of, synced_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (address)
		DO UPDATE SET provider = EXCLUDED.provider, dn = EXCLUDED.dn,
		              last_seen_at = EXCLUDED.last_seen_at,
		              display_name = EXCLUDED.display_name,
		              member_of = EXCLUDED.member_of,
		              synced_at = EXCLUDED.synced_at`
	if _, err := q.ExecContext(ctx, query,
		link.Address, link.Provider, link.DN, link.LastSeenAt,
		link.DisplayName, link.MemberOf, link.SyncedAt); err != nil {
		return fmt.Errorf("store: upserting ldap link: %w", err)
	}
	return nil
}

// ListForProvider returns every link the provider owns, for the reconcile that
// decides which of its users have gone missing from the directory.
func (l *LDAPLinks) ListForProvider(ctx context.Context, q Querier, provider string) ([]LDAPLink, error) {
	rows, err := q.QueryxContext(ctx, `
		SELECT address, provider, dn, last_seen_at, display_name, member_of, synced_at
		FROM ldap_links WHERE provider = $1 ORDER BY address`, provider)
	if err != nil {
		return nil, fmt.Errorf("store: listing ldap links: %w", err)
	}
	defer rows.Close()

	out := make([]LDAPLink, 0, 16)
	for rows.Next() {
		var row LDAPLink
		if err := rows.StructScan(&row); err != nil {
			return nil, fmt.Errorf("store: scanning ldap link: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterating ldap links: %w", err)
	}
	return out, nil
}

// Delete removes the binding for an address. The syncer uses it when an entry
// has moved providers; deleting the principal removes the link on its own.
func (l *LDAPLinks) Delete(ctx context.Context, q Querier, address string) error {
	res, err := q.ExecContext(ctx, `DELETE FROM ldap_links WHERE address = $1`, address)
	if err != nil {
		return fmt.Errorf("store: deleting ldap link: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SyncState is the bookkeeping for one provider's reconcile runs.
type SyncState struct {
	Provider        string     `db:"provider"`
	LastRunAt       *time.Time `db:"last_run_at"`
	LastSuccessAt   *time.Time `db:"last_success_at"`
	LastError       string     `db:"last_error"`
	EntriesSeen     int64      `db:"entries_seen"`
	EntriesAdded    int64      `db:"entries_added"`
	EntriesUpdated  int64      `db:"entries_updated"`
	EntriesDisabled int64      `db:"entries_disabled"`
}

// LDAPSyncState reads and writes the ldap_sync_state table.
type LDAPSyncState struct{}

// NewLDAPSyncState returns the repository. It is stateless, so this is free.
func NewLDAPSyncState() *LDAPSyncState { return &LDAPSyncState{} }

// Get returns the recorded state for a provider. A provider that has never run
// is not an error: it is a zero state, because the admin UI shows "never" for
// it rather than an error.
func (s *LDAPSyncState) Get(ctx context.Context, q Querier, provider string) (SyncState, error) {
	var row SyncState
	if err := q.GetContext(ctx, &row, `
		SELECT provider, last_run_at, last_success_at, last_error,
		       entries_seen, entries_added, entries_updated, entries_disabled
		FROM ldap_sync_state WHERE provider = $1`, provider); err != nil {
		if IsNotFound(err) {
			return SyncState{Provider: provider}, nil
		}
		return SyncState{}, err
	}
	return row, nil
}

// RecordRun records the outcome of one reconcile. ranAt is when the run
// finished; successAt is non-nil only when it succeeded, and a failed run must
// not erase the memory of the last good one, so the update coalesces it. A
// directory that was reachable an hour ago is then still tellable from one
// that has been silent for a day. failure is the stringified cause, empty on
// success; the counters describe what the run did to the local principals.
func (s *LDAPSyncState) RecordRun(
	ctx context.Context, q Querier, provider string, ranAt time.Time,
	successAt *time.Time, failure string,
	seen, added, updated, disabled int64,
) error {
	const query = `
		INSERT INTO ldap_sync_state (provider, last_run_at, last_success_at, last_error,
		                             entries_seen, entries_added, entries_updated, entries_disabled)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (provider)
		DO UPDATE SET last_run_at = EXCLUDED.last_run_at,
		              last_success_at = COALESCE(EXCLUDED.last_success_at, ldap_sync_state.last_success_at),
		              last_error = EXCLUDED.last_error,
		              entries_seen = EXCLUDED.entries_seen,
		              entries_added = EXCLUDED.entries_added,
		              entries_updated = EXCLUDED.entries_updated,
		              entries_disabled = EXCLUDED.entries_disabled`
	if _, err := q.ExecContext(ctx, query, provider, ranAt, successAt, failure,
		seen, added, updated, disabled); err != nil {
		return fmt.Errorf("store: recording ldap sync state: %w", err)
	}
	return nil
}
