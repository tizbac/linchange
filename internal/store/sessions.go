package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/auth/secret"
)

// Session lifetime policy.
//
// A session outlives every access token issued under it, so these bounds decide
// how long a stolen refresh token is actually useful. Absolute is a hard
// ceiling that refreshing cannot extend, which is what stops a long-lived
// session from becoming permanent by accident. Idle is checked on each use, so
// a session nobody touches ends on its own without a sweeper having to find it.
const (
	// DefaultSessionAbsoluteTTL is how long a session may live in total.
	DefaultSessionAbsoluteTTL = 720 * time.Hour
	// DefaultSessionIdleTTL is how long a session may go unused before it
	// stops being honoured.
	DefaultSessionIdleTTL = 336 * time.Hour
	// refreshTokenBytes is the entropy of a refresh token. 32 bytes is well
	// past the point where guessing is a strategy, and the refresh token is
	// the only thing standing between a database dump and every live session.
	refreshTokenBytes = 32
	// sessionIDBytes names a session in a token claim and in the logs. It is
	// not a credential: knowing it does not let anyone authenticate, because
	// the refresh token is what the row checks.
	sessionIDBytes = 12
)

// Errors specific to sessions.
//
// These are kept apart from ErrNotFound because the caller's response differs.
// A missing session means "start over", while a revoked or expired one means
// "this sign-in is over", and a stolen one means "end this session and warn
// somebody". Collapsing them into one error would make all three look like a
// routine expiry.
var (
	// ErrSessionRevoked means the session existed and has been ended, by a
	// logout, a password change, or an administrator.
	ErrSessionRevoked = errors.New("store: session revoked")
	// ErrSessionExpired means the session passed its absolute or idle deadline.
	ErrSessionExpired = errors.New("store: session expired")
	// ErrSessionTokenMismatch means the presented refresh token is not the one
	// on file.
	ErrSessionTokenMismatch = errors.New("store: refresh token does not match")
)

// Session is a live row in the sessions table.
type Session struct {
	// ID is what a token's sid claim carries. Not a secret.
	ID string
	// Address is the principal this session authenticates.
	Address string
	// Principal is the identity as the database currently has it, not as the
	// token claims. This is the reason Load exists: a token is a cache of an
	// answer, and this is where the answer is checked.
	Principal policy.Principal
	// IP and UserAgent are for the "where am I signed in" view and for a
	// support conversation about a session nobody recognises.
	IP        netip.Addr
	UserAgent string
	// Impersonating names the principal a domain admin is acting as, if any.
	Impersonating string
	CreatedAt     time.Time
	LastSeenAt    time.Time
	// ExpiresAt is the absolute deadline, fixed at creation.
	ExpiresAt time.Time
	// RevokedAt is nil while the session is live.
	RevokedAt *time.Time
}

// Live reports whether the session may still be used, ignoring the idle
// deadline, which needs the idle TTL and so is checked by the repository.
func (s Session) Live(now time.Time) bool {
	return s.RevokedAt == nil && now.Before(s.ExpiresAt)
}

// Sessions reads and writes the sessions table.
type Sessions struct{}

// NewSessions returns a repository. It is stateless, so this is free and the
// value can be constructed anywhere.
func NewSessions() *Sessions { return &Sessions{} }

// CreateSessionRequest is what a completed login supplies.
type CreateSessionRequest struct {
	// Address is the principal that just authenticated.
	Address string
	// IP and UserAgent are optional, but recording them is what makes an
	// unfamiliar session recognisable later.
	IP        netip.Addr
	UserAgent string
	// Impersonating is set only when a domain admin signs in to act as another
	// user. The session records it so the audit trail has both parties.
	Impersonating string
	// AbsoluteTTL and IdleTTL override the defaults when positive.
	AbsoluteTTL time.Duration
	IdleTTL     time.Duration
}

// CreatedSession is what the login path gets back.
type CreatedSession struct {
	Session Session
	// RefreshToken is returned once and never stored. Only its digest reaches
	// the database, so a dump of the sessions table yields no usable credential.
	RefreshToken string
}

// sessionRow is the sessions table as scanned. Nullable columns are read
// through sql.Null* so a row without them is not a scan error.
type sessionRow struct {
	ID                 string         `db:"id"`
	Address            string         `db:"address"`
	Impersonating      sql.NullString `db:"impersonating"`
	IPHost             sql.NullString `db:"ip_host"`
	UserAgent          string         `db:"user_agent"`
	CreatedAt          time.Time      `db:"created_at"`
	LastSeenAt         time.Time      `db:"last_seen_at"`
	AbsoluteExpiresAt  time.Time      `db:"absolute_expires_at"`
	RevokedAt          sql.NullTime   `db:"revoked_at"`
	Source             string         `db:"source"`
	Enabled            bool           `db:"enabled"`
	MailboxEnabled     bool           `db:"mailbox_enabled"`
	MustChangePassword bool           `db:"must_change_password"`
	IsDomainAdmin      bool           `db:"is_domain_admin"`
}

// sessionSelect joins the user row on, so a session cannot report attributes
// that the user table no longer agrees with.
const sessionSelect = `
	SELECT s.id, s.address, s.impersonating, host(s.ip) AS ip_host,
	       s.user_agent, s.created_at, s.last_seen_at, s.absolute_expires_at,
	       s.revoked_at,
	       u.source, u.enabled, u.mailbox_enabled, u.must_change_password,
	       u.is_domain_admin
	FROM sessions s
	JOIN users u ON u.address = s.address`

// toSession converts a scanned row.
func (r sessionRow) toSession() Session {
	s := Session{
		ID:            r.ID,
		Address:       r.Address,
		UserAgent:     r.UserAgent,
		CreatedAt:     r.CreatedAt,
		LastSeenAt:    r.LastSeenAt,
		ExpiresAt:     r.AbsoluteExpiresAt,
		Impersonating: r.Impersonating.String,
		Principal: policy.Principal{
			Address:            r.Address,
			Source:             policy.Source(r.Source),
			Enabled:            r.Enabled,
			MailboxEnabled:     r.MailboxEnabled,
			MustChangePassword: r.MustChangePassword,
			IsDomainAdmin:      r.IsDomainAdmin,
		},
	}
	if r.RevokedAt.Valid {
		revoked := r.RevokedAt.Time
		s.RevokedAt = &revoked
	}
	if r.IPHost.Valid {
		if addr, ok := parseIP(r.IPHost.String); ok {
			s.IP = addr
		}
	}
	return s
}

// Create starts a session.
//
// The principal is re-read inside the call rather than taken from the argument.
// A caller can hold a principal it authenticated a moment ago that has since
// been disabled, and the entire value of a session is that it keeps working
// after the request that created it has gone.
func (s *Sessions) Create(ctx context.Context, q Querier, req CreateSessionRequest) (CreatedSession, error) {
	var out CreatedSession

	if req.Address == "" {
		return out, errors.New("store: a session needs an address")
	}
	absolute := req.AbsoluteTTL
	if absolute <= 0 {
		absolute = DefaultSessionAbsoluteTTL
	}

	principal, err := LoadPrincipal(ctx, q, req.Address)
	if err != nil {
		return out, err
	}

	// An impersonation session is only meaningful for a domain admin, and it
	// has to be pointed at somebody who can actually hold mail, or there is
	// nothing to impersonate into.
	if req.Impersonating != "" {
		if !principal.DomainAdmin() {
			return out, fmt.Errorf("store: %s is not a domain admin", req.Address)
		}
		target, terr := LoadPrincipal(ctx, q, req.Impersonating)
		if terr != nil {
			return out, terr
		}
		if !target.MailboxEnabled {
			return out, ErrNoMailbox
		}
	}

	id, err := secret.Generate(sessionIDBytes)
	if err != nil {
		return out, fmt.Errorf("store: session id: %w", err)
	}
	refresh, err := secret.Generate(refreshTokenBytes)
	if err != nil {
		return out, fmt.Errorf("store: refresh token: %w", err)
	}

	now := time.Now()
	row := struct {
		ID                string    `db:"id"`
		Address           string    `db:"address"`
		RefreshTokenHash  []byte    `db:"refresh_token_hash"`
		Impersonating     *string   `db:"impersonating"`
		IP                *string   `db:"ip"`
		UserAgent         string    `db:"user_agent"`
		CreatedAt         time.Time `db:"created_at"`
		LastSeenAt        time.Time `db:"last_seen_at"`
		AbsoluteExpiresAt time.Time `db:"absolute_expires_at"`
	}{
		ID:                id,
		Address:           req.Address,
		RefreshTokenHash:  secret.Hash(refresh),
		UserAgent:         req.UserAgent,
		CreatedAt:         now,
		LastSeenAt:        now,
		AbsoluteExpiresAt: now.Add(absolute),
	}
	if req.Impersonating != "" {
		row.Impersonating = &req.Impersonating
	}
	if req.IP.IsValid() {
		host := req.IP.String()
		row.IP = &host
	}

	const insert = `
		INSERT INTO sessions
			(id, address, refresh_token_hash, impersonating, ip, user_agent,
			 created_at, last_seen_at, absolute_expires_at)
		VALUES (:id, :address, :refresh_token_hash, :impersonating, :ip,
		        :user_agent, :created_at, :last_seen_at, :absolute_expires_at)`

	if _, err := q.NamedExecContext(ctx, insert, row); err != nil {
		return out, fmt.Errorf("store: creating session: %w", err)
	}

	created := sessionRow{
		ID:                 id,
		Address:            req.Address,
		UserAgent:          req.UserAgent,
		CreatedAt:          now,
		LastSeenAt:         now,
		AbsoluteExpiresAt:  now.Add(absolute),
		Source:             string(principal.Source),
		Enabled:            principal.Enabled,
		MailboxEnabled:     principal.MailboxEnabled,
		MustChangePassword: principal.MustChangePassword,
	}
	if req.Impersonating != "" {
		created.Impersonating = sql.NullString{String: req.Impersonating, Valid: true}
	}
	if req.IP.IsValid() {
		created.IPHost = sql.NullString{String: req.IP.String(), Valid: true}
	}

	out.Session = created.toSession()
	out.RefreshToken = refresh
	return out, nil
}

// Load reads a session and reports whether it may still be used.
//
// This is the check a token cannot do for itself, and the reason access tokens
// can be self-contained. The token says who it was minted for; this says
// whether that is still true. A revoked session, an expired one, a disabled
// user, or a user who has lost the right to hold mail all come back as an
// error, so a caller acting on the token's claims alone would be wrong in every
// one of those cases.
//
// It also advances last_seen_at, which is what makes the idle deadline work.
// Callers that check on every request should expect one small write per
// request; a session being polled is a session in use, and ending those is the
// idle rule's job rather than a sweeper's.
func (s *Sessions) Load(ctx context.Context, q Querier, id string) (Session, error) {
	session, err := s.fetch(ctx, q, id)
	if err != nil {
		return Session{}, err
	}
	return session, nil
}

// LoadForRefresh is Load for the refresh path: it additionally requires the
// presented refresh token to be the one on file.
//
// A mismatch is treated as theft rather than as a bad request, and the session
// is ended. At that point the server cannot tell which of the two parties
// holding a copy of the session id is the impostor, so continuing to serve
// whichever one asks first would mean the attacker keeps the session and the
// legitimate user is logged out, which is exactly backwards.
func (s *Sessions) LoadForRefresh(ctx context.Context, q Querier, id, refreshToken string) (Session, error) {
	// An absent token is a malformed request, not evidence of theft, so it must
	// not reach the comparison below. That comparison ends the session, and if
	// a blank were treated as a mismatch then anyone who knew a session id
	// could log a user out by sending an empty refresh.
	if id == "" || refreshToken == "" {
		return Session{}, ErrNotFound
	}

	const query = sessionSelect + ` WHERE s.id = $1 FOR UPDATE`
	var row sessionRow
	if err := q.GetContext(ctx, &row, query, id); err != nil {
		if IsNotFound(err) {
			return Session{}, ErrNotFound
		}
		return Session{}, fmt.Errorf("store: loading session: %w", err)
	}

	var stored []byte
	const hashQuery = `SELECT refresh_token_hash FROM sessions WHERE id = $1`
	if err := q.GetContext(ctx, &stored, hashQuery, id); err != nil {
		if IsNotFound(err) {
			return Session{}, ErrNotFound
		}
		return Session{}, fmt.Errorf("store: loading refresh token: %w", err)
	}
	if !secret.Equal(refreshToken, stored) {
		_ = s.Revoke(ctx, q, id)
		return Session{}, ErrSessionTokenMismatch
	}

	session := row.toSession()
	if err := s.checkLive(session, DefaultSessionIdleTTL); err != nil {
		return Session{}, err
	}
	if !session.Principal.Enabled {
		// The user was disabled after the session started. LoadPrincipal hides
		// disabled users, so this is checked against the row directly.
		_ = s.Revoke(ctx, q, id)
		return Session{}, ErrSessionRevoked
	}
	return session, nil
}

// Rotate replaces a session's refresh token and returns the new one.
//
// Rotation on every use means a stolen token is good for at most one refresh
// before the legitimate user's next refresh invalidates it, which the
// legitimate user then sees as a stolen-token event rather than discovering
// months later that someone else has been reading their mail.
func (s *Sessions) Rotate(ctx context.Context, q Querier, id string) (string, error) {
	refresh, err := secret.Generate(refreshTokenBytes)
	if err != nil {
		return "", fmt.Errorf("store: refresh token: %w", err)
	}

	const update = `UPDATE sessions SET refresh_token_hash = $2 WHERE id = $1 AND revoked_at IS NULL`
	res, err := q.ExecContext(ctx, update, id, secret.Hash(refresh))
	if err != nil {
		return "", fmt.Errorf("store: rotating refresh token: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// The WHERE clause did not match, so the session was revoked between
		// the check and here. Reporting that beats handing back a token for a
		// session that no longer exists.
		return "", ErrSessionRevoked
	}
	return refresh, nil
}

// Revoke ends a single session. Revoking an already-revoked session succeeds,
// since logging out twice is not an error worth surfacing to a user.
func (s *Sessions) Revoke(ctx context.Context, q Querier, id string) error {
	const update = `
		UPDATE sessions SET revoked_at = now()
		WHERE id = $1 AND revoked_at IS NULL`
	if _, err := q.ExecContext(ctx, update, id); err != nil {
		return fmt.Errorf("store: revoking session: %w", err)
	}
	return nil
}

// RevokeAllForAddress ends every live session belonging to a principal and
// reports how many were ended.
//
// This is the call a password change and an account disable have to make, since
// both are meaningless as security measures while a session opened with the old
// credential is still live.
func (s *Sessions) RevokeAllForAddress(ctx context.Context, q Querier, address string) (int64, error) {
	const update = `
		UPDATE sessions SET revoked_at = now()
		WHERE address = $1 AND revoked_at IS NULL`
	res, err := q.ExecContext(ctx, update, address)
	if err != nil {
		return 0, fmt.Errorf("store: revoking sessions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: revoking sessions: %w", err)
	}
	return n, nil
}

// ListForAddress returns a principal's live sessions, newest first, for the
// "where am I signed in" view. Each one is shown by id and address so a user can
// recognise and end a session that is not theirs.
func (s *Sessions) ListForAddress(ctx context.Context, q Querier, address string) ([]Session, error) {
	const query = sessionSelect + `
		WHERE s.address = $1 AND s.revoked_at IS NULL
		ORDER BY s.last_seen_at DESC`

	rows, err := q.QueryxContext(ctx, query, address)
	if err != nil {
		return nil, fmt.Errorf("store: listing sessions: %w", err)
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		var row sessionRow
		if err := rows.StructScan(&row); err != nil {
			return nil, fmt.Errorf("store: scanning session: %w", err)
		}
		out = append(out, row.toSession())
	}
	return out, rows.Err()
}

// PurgeExpired deletes sessions that are revoked or past their absolute
// deadline, and reports how many went.
//
// This is housekeeping, not security: an expired session is already refused by
// Load, so nothing depends on this running on time. It exists so the table does
// not grow without bound, which for a per-login table means it eventually does.
func (s *Sessions) PurgeExpired(ctx context.Context, q Querier) (int64, error) {
	const del = `
		DELETE FROM sessions
		WHERE absolute_expires_at < now()
		   OR (revoked_at IS NOT NULL AND revoked_at < now() - interval '7 days')`
	res, err := q.ExecContext(ctx, del)
	if err != nil {
		return 0, fmt.Errorf("store: purging sessions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: purging sessions: %w", err)
	}
	return n, nil
}

// fetch reads a session by id without touching it.
func (s *Sessions) fetch(ctx context.Context, q Querier, id string) (Session, error) {
	if id == "" {
		return Session{}, ErrNotFound
	}

	var row sessionRow
	if err := q.GetContext(ctx, &row, sessionSelect+` WHERE s.id = $1`, id); err != nil {
		if IsNotFound(err) {
			return Session{}, ErrNotFound
		}
		return Session{}, fmt.Errorf("store: loading session: %w", err)
	}

	session := row.toSession()
	if err := s.checkLive(session, DefaultSessionIdleTTL); err != nil {
		return Session{}, err
	}
	if !session.Principal.Enabled {
		return Session{}, ErrSessionRevoked
	}

	// Advancing last_seen_at is what makes the idle deadline meaningful.
	const touch = `UPDATE sessions SET last_seen_at = now() WHERE id = $1`
	if _, err := q.ExecContext(ctx, touch, id); err != nil {
		return Session{}, fmt.Errorf("store: touching session: %w", err)
	}
	session.LastSeenAt = time.Now()
	return session, nil
}

// checkLive applies the deadlines to a fetched session.
func (s *Sessions) checkLive(session Session, idleTTL time.Duration) error {
	now := time.Now()
	if session.RevokedAt != nil {
		return ErrSessionRevoked
	}
	if !now.Before(session.ExpiresAt) {
		return ErrSessionExpired
	}
	if idleTTL > 0 && now.Sub(session.LastSeenAt) > idleTTL {
		return ErrSessionExpired
	}
	return nil
}
