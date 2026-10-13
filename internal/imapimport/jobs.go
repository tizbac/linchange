package imapimport

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/linchange/linchange/internal/store"
)

const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusCancelled = "cancelled"
	StatusFailed    = "failed"
)

type Job struct {
	ID               int64      `db:"id" json:"id"`
	Owner            string     `db:"owner" json:"owner"`
	StartedBy        string     `db:"started_by" json:"started_by"`
	RemoteHost       string     `db:"remote_host" json:"remote_host"`
	RemotePort       int        `db:"remote_port" json:"remote_port"`
	RemoteTLS        string     `db:"remote_tls" json:"remote_tls"`
	RemoteUsername   string     `db:"remote_username" json:"remote_username"`
	AcceptSelfSigned bool       `db:"accept_self_signed" json:"accept_self_signed"`
	SourceRoot       string     `db:"source_root" json:"source_root"`
	TargetRoot       string     `db:"target_root" json:"target_root"`
	Status           string     `db:"status" json:"status"`
	CancelRequested  bool       `db:"cancel_requested" json:"cancel_requested"`
	LastError        *string    `db:"last_error" json:"last_error,omitempty"`
	FoldersTotal     int        `db:"folders_total" json:"folders_total"`
	FoldersDone      int        `db:"folders_done" json:"folders_done"`
	MessagesSeen     int64      `db:"messages_seen" json:"messages_seen"`
	MessagesImported int64      `db:"messages_imported" json:"messages_imported"`
	MessagesSkipped  int64      `db:"messages_skipped" json:"messages_skipped"`
	BytesImported    int64      `db:"bytes_imported" json:"bytes_imported"`
	CreatedAt        time.Time  `db:"created_at" json:"created_at"`
	StartedAt        *time.Time `db:"started_at" json:"started_at,omitempty"`
	FinishedAt       *time.Time `db:"finished_at" json:"finished_at,omitempty"`
}

type CreateJobParams struct {
	Owner            string
	StartedBy        string
	RemoteHost       string
	RemotePort       int
	RemoteTLS        string
	RemoteUsername   string
	RemotePassword   string
	AcceptSelfSigned bool
	SourceRoot       string
	TargetRoot       string
}

const defaultTLSPort = 993
const defaultPort    = 143

func (s *Service) CreateJob(ctx context.Context, params CreateJobParams) (Job, error) {
	params.Owner = strings.TrimSpace(params.Owner)
	params.RemoteHost = strings.TrimSpace(params.RemoteHost)
	params.RemoteUsername = strings.TrimSpace(params.RemoteUsername)

	if params.Owner == "" {
		return Job{}, fmt.Errorf("%w: target address is required", ErrInvalid)
	}
	if params.RemoteHost == "" {
		return Job{}, fmt.Errorf("%w: remote host is required", ErrInvalid)
	}
	if params.RemoteUsername == "" {
		return Job{}, fmt.Errorf("%w: remote username is required", ErrInvalid)
	}
	if params.RemotePassword == "" {
		return Job{}, fmt.Errorf("%w: remote password is required", ErrInvalid)
	}
	switch params.RemoteTLS {
	case "tls", "starttls", "plain":
	case "":
		params.RemoteTLS = "tls"
	default:
		return Job{}, fmt.Errorf("%w: remote_tls must be tls, starttls or plain", ErrInvalid)
	}
	if params.RemotePort <= 0 {
		if params.RemoteTLS == "plain" {
			params.RemotePort = defaultPort
		} else {
			params.RemotePort = defaultTLSPort
		}
	}

	var mailboxEnabled bool
	err := s.db.GetContext(ctx, &mailboxEnabled,
		`SELECT mailbox_enabled FROM users WHERE address = $1 AND deleted_at IS NULL`, params.Owner)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Job{}, fmt.Errorf("%w: no such account", store.ErrNotFound)
		}
		return Job{}, fmt.Errorf("imapimport: looking up target account: %w", err)
	}
	if !mailboxEnabled {
		return Job{}, fmt.Errorf("%w: that account cannot hold mail", ErrInvalid)
	}

	sealed, err := encryptPassword(s.key, params.RemotePassword)
	if err != nil {
		return Job{}, err
	}

	var job Job
	const insert = `
		INSERT INTO imap_import_jobs (owner, started_by, remote_host, remote_port, remote_tls,
		                              remote_username, remote_password, accept_self_signed,
		                              source_root, target_root)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id, owner, started_by, remote_host, remote_port, remote_tls, remote_username,
		          accept_self_signed, source_root, target_root, status, cancel_requested,
		          last_error, folders_total, folders_done, messages_seen, messages_imported,
		          messages_skipped, bytes_imported, created_at, started_at, finished_at`
	err = s.db.GetContext(ctx, &job, insert,
		params.Owner, params.StartedBy, params.RemoteHost, params.RemotePort, params.RemoteTLS,
		params.RemoteUsername, sealed, params.AcceptSelfSigned,
		params.SourceRoot, params.TargetRoot)
	if err != nil {
		return Job{}, fmt.Errorf("imapimport: creating job: %w", err)
	}
	return job, nil
}

func (s *Service) ListJobs(ctx context.Context, owner *string) ([]Job, error) {
	var (
		jobs []Job
		err  error
	)
	if owner != nil {
		err = s.db.SelectContext(ctx, &jobs, `
			SELECT id, owner, started_by, remote_host, remote_port, remote_tls, remote_username,
			       accept_self_signed, source_root, target_root, status, cancel_requested,
			       last_error, folders_total, folders_done, messages_seen, messages_imported,
			       messages_skipped, bytes_imported, created_at, started_at, finished_at
			FROM imap_import_jobs
			WHERE owner = $1
			ORDER BY created_at DESC
			LIMIT 200`, *owner)
	} else {
		err = s.db.SelectContext(ctx, &jobs, `
			SELECT id, owner, started_by, remote_host, remote_port, remote_tls, remote_username,
			       accept_self_signed, source_root, target_root, status, cancel_requested,
			       last_error, folders_total, folders_done, messages_seen, messages_imported,
			       messages_skipped, bytes_imported, created_at, started_at, finished_at
			FROM imap_import_jobs
			ORDER BY created_at DESC
			LIMIT 200`)
	}
	if err != nil {
		return nil, fmt.Errorf("imapimport: listing jobs: %w", err)
	}
	return jobs, nil
}

func (s *Service) GetJob(ctx context.Context, id int64) (Job, error) {
	job, err := s.getJob(ctx, id)
	if err != nil {
		return Job{}, err
	}
	return job, nil
}

func (s *Service) getJob(ctx context.Context, id int64) (Job, error) {
	var job Job
	const query = `
		SELECT id, owner, started_by, remote_host, remote_port, remote_tls, remote_username,
		       accept_self_signed, source_root, target_root, status, cancel_requested,
		       last_error, folders_total, folders_done, messages_seen, messages_imported,
		       messages_skipped, bytes_imported, created_at, started_at, finished_at
		FROM imap_import_jobs
		WHERE id = $1`
	err := s.db.GetContext(ctx, &job, query, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Job{}, store.ErrNotFound
		}
		return Job{}, fmt.Errorf("imapimport: reading job %d: %w", id, err)
	}
	return job, nil
}

func (s *Service) CancelJob(ctx context.Context, id int64) (Job, error) {
	const update = `
		UPDATE imap_import_jobs
		SET cancel_requested = true,
		    status = CASE WHEN status = 'queued' THEN 'cancelled' ELSE status END,
		    finished_at = CASE WHEN status = 'queued' THEN now() ELSE finished_at END
		WHERE id = $1
		RETURNING id, owner, started_by, remote_host, remote_port, remote_tls, remote_username,
		          accept_self_signed, source_root, target_root, status, cancel_requested,
		          last_error, folders_total, folders_done, messages_seen, messages_imported,
		          messages_skipped, bytes_imported, created_at, started_at, finished_at`
	var job Job
	if err := s.db.GetContext(ctx, &job, update, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Job{}, store.ErrNotFound
		}
		return Job{}, fmt.Errorf("imapimport: cancelling job %d: %w", id, err)
	}
	s.cancelInFlight(id)
	return job, nil
}

func (s *Service) cancelInFlight(id int64) {
	s.mu.Lock()
	cancel, ok := s.cancels[id]
	s.mu.Unlock()
	if ok {
		cancel()
	}
}

func (s *Service) claimNext(ctx context.Context) (*Job, error) {
	const claim = `
		UPDATE imap_import_jobs
		SET status = 'running', started_at = COALESCE(started_at, now())
		WHERE id = (
			SELECT id FROM imap_import_jobs
			WHERE status = 'queued' AND cancel_requested = false
			ORDER BY id
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, owner, started_by, remote_host, remote_port, remote_tls, remote_username,
		          accept_self_signed, source_root, target_root, status, cancel_requested,
		          last_error, folders_total, folders_done, messages_seen, messages_imported,
		          messages_skipped, bytes_imported, created_at, started_at, finished_at`
	var job Job
	err := s.db.GetContext(ctx, &job, claim)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("imapimport: claiming job: %w", err)
	}
	return &job, nil
}

func (s *Service) requeueOrphans(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE imap_import_jobs
		SET status = 'queued'
		WHERE status = 'running' AND cancel_requested = false`)
	if err != nil {
		return fmt.Errorf("imapimport: requeueing interrupted jobs: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE imap_import_jobs
		SET status = 'cancelled', finished_at = now()
		WHERE status = 'running' AND cancel_requested = true`)
	if err != nil {
		return fmt.Errorf("imapimport: finishing cancelled jobs: %w", err)
	}
	return nil
}

func (s *Service) finishJob(ctx context.Context, id int64, status string, jobErr error) {
	var msg *string
	if jobErr != nil {
		text := jobErr.Error()
		if len(text) > 2000 {
			text = text[:2000]
		}
		msg = &text
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE imap_import_jobs
		SET status = $2, last_error = $3, finished_at = now()
		WHERE id = $1`, id, status, msg)
	if err != nil {
		s.log.Error("imapimport: finishing job", "job", id, "status", status, "err", err)
	}
}

func (s *Service) requeueJob(ctx context.Context, id int64) {
	_, err := s.db.ExecContext(ctx, `
		UPDATE imap_import_jobs SET status = 'queued' WHERE id = $1`, id)
	if err != nil {
		s.log.Error("imapimport: requeueing job", "job", id, "err", err)
	}
}

type progress struct {
	seen     int64
	imported int64
	skipped  int64
	bytes    int64
	folders  int
	foldersDone int
}

func (s *Service) saveProgress(ctx context.Context, id int64, p progress) {
	_, err := s.db.ExecContext(ctx, `
		UPDATE imap_import_jobs
		SET messages_seen = $2, messages_imported = $3, messages_skipped = $4,
		    bytes_imported = $5, folders_total = $6, folders_done = $7
		WHERE id = $1`,
		id, p.seen, p.imported, p.skipped, p.bytes, p.folders, p.foldersDone)
	if err != nil {
		s.log.Warn("imapimport: saving progress", "job", id, "err", err)
	}
}

func (s *Service) recordItem(ctx context.Context, jobID int64, folder string, uid, uidValidity, mailboxID int64, sha []byte) error {
	const insert = `
		INSERT INTO imap_import_items (job_id, remote_folder, remote_uid, remote_uidvalidity,
		                               target_mailbox_id, message_sha256)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (job_id, remote_folder, remote_uid) DO NOTHING`
	_, err := s.db.ExecContext(ctx, insert, jobID, folder, uid, uidValidity, mailboxID, sha)
	if err != nil {
		return fmt.Errorf("imapimport: recording item: %w", err)
	}
	return nil
}

func (s *Service) importedUIDs(ctx context.Context, jobID int64, folder string, uidValidity int64) (map[int64]bool, error) {
	var uids []int64
	err := s.db.SelectContext(ctx, &uids, `
		SELECT remote_uid FROM imap_import_items
		WHERE job_id = $1 AND remote_folder = $2 AND remote_uidvalidity = $3`,
		jobID, folder, uidValidity)
	if err != nil {
		return nil, fmt.Errorf("imapimport: reading imported uids: %w", err)
	}
	set := make(map[int64]bool, len(uids))
	for _, uid := range uids {
		set[uid] = true
	}
	return set, nil
}

func (s *Service) mailboxHasBlob(ctx context.Context, mailboxID int64, sha []byte) (bool, error) {
	var exists bool
	err := s.db.GetContext(ctx, &exists, `
		SELECT EXISTS (SELECT 1 FROM messages WHERE mailbox_id = $1 AND blob_sha256 = $2)`,
		mailboxID, sha)
	if err != nil {
		return false, fmt.Errorf("imapimport: checking existing message: %w", err)
	}
	return exists, nil
}
