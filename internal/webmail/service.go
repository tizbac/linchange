// Package webmail is the mail reading and writing service behind the REST API
// and, through it, the web client.
//
// It exists so that the same operations are reachable by every surface: a
// compose button in the web client, a rule applied during delivery, and the
// tests, all go through this one place rather than each growing its own SQL.
// The HTTP layer translates requests into these calls and nothing else, in the
// same spirit as the rest of the server.
package webmail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/blobstore"
	"github.com/linchange/linchange/internal/delivery"
	"github.com/linchange/linchange/internal/imapd"
	"github.com/linchange/linchange/internal/store"
)

// Errors a caller can distinguish. The HTTP layer maps these onto statuses, so
// a handler never has to interpret a database error to decide what to tell a
// user.
var (
	// ErrNotFound means the object does not exist or belongs to somebody else.
	// The two are deliberately the same error: telling a caller that an id
	// exists but is not theirs is a way of enumerating other people's mail.
	ErrNotFound = errors.New("webmail: not found")
	ErrDenied   = errors.New("webmail: not permitted")
	ErrQuota    = errors.New("webmail: mailbox quota exceeded")
	ErrInvalid  = errors.New("webmail: invalid request")
)

// Options configures a Service.
type Options struct {
	DB     *sqlx.DB
	Blobs  *blobstore.Store
	Logger *slog.Logger
	// Hostname is the domain this server claims to send mail from, and the
	// domain half of any Message-ID it mints.
	Hostname string
	// Outbound is the configuration for the delivery worker: where a sent
	// message goes and which identity it uses. It is read from the same
	// configuration the worker reads, so a send cannot disagree with the retry
	// behaviour of the message it queued.
	Outbound delivery.Config
	// MaxAttachmentBytes bounds one attachment and the whole message. A request
	// body is only bounded so far; this is the limit that has to hold.
	MaxAttachmentBytes int64
	MaxMessageBytes    int64
}

// Service is the mail reading and writing surface.
type Service struct {
	db                 *sqlx.DB
	blobs              *blobstore.Store
	log                *slog.Logger
	hostname           string
	outbound           delivery.Config
	maxAttachmentBytes int64
	maxMessageBytes    int64

	mailboxes  *imapd.MailboxRepo
	messages   *imapd.MessageRepo
	aliases    *store.Aliases
	shared     *store.SharedMailboxes
	grants     *store.SharedMailboxGrants
	outbox     *delivery.OutboxRepository
	rules      *store.MailRules
	signatures *store.Signatures
	audit      *store.AuditLog
}

// defaults keep a caller from having to think about limits.
const (
	defaultMaxAttachmentBytes = 25 << 20
	defaultMaxMessageBytes    = 50 << 20
)

// StoreSignatureImage stores a signature image and returns a CID for referencing it.
func (s *Service) StoreSignatureImage(ctx context.Context, owner string, data []byte, mimeType string) (string, error) {
	// Store in blobstore; the SHA256 becomes the CID
	sum, _, err := s.blobs.Put(data)
	if err != nil {
		return "", fmt.Errorf("store signature image: %w", err)
	}
	// Use the blob hash as the CID
	cid := fmt.Sprintf("sig-%x", sum)
	return cid, nil
}

// GetBlob retrieves a blob by digest. Used for serving signature/compose images.
func (s *Service) GetBlob(ctx context.Context, d blobstore.Digest) ([]byte, error) {
	return s.blobs.Get(d)
}

// New builds a Service.
func New(opts Options) (*Service, error) {
	if opts.DB == nil {
		return nil, errors.New("webmail: a database handle is required")
	}
	if opts.Blobs == nil {
		return nil, errors.New("webmail: a blob store is required")
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	maxAttachment := opts.MaxAttachmentBytes
	if maxAttachment <= 0 {
		maxAttachment = defaultMaxAttachmentBytes
	}
	maxMessage := opts.MaxMessageBytes
	if maxMessage <= 0 {
		maxMessage = defaultMaxMessageBytes
	}

	return &Service{
		db:                 opts.DB,
		blobs:              opts.Blobs,
		log:                log,
		hostname:           strings.TrimSpace(opts.Hostname),
		outbound:           opts.Outbound,
		maxAttachmentBytes: maxAttachment,
		maxMessageBytes:    maxMessage,
		mailboxes:          imapd.NewMailboxRepo(),
		messages:           imapd.NewMessageRepo(),
		aliases:            store.NewAliases(),
		shared:             store.NewSharedMailboxes(),
		grants:             store.NewSharedMailboxGrants(),
		outbox:             delivery.NewOutboxRepository(opts.DB),
		rules:              store.NewMailRules(),
		signatures:         store.NewSignatures(),
		audit:              store.NewAuditLog(),
	}, nil
}

// Folder is a mailbox as the web client sees it.
type Folder struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name"`
	ParentID   *int64  `json:"parent_id,omitempty"`
	SpecialUse *string `json:"special_use,omitempty"`
	Subscribed bool    `json:"subscribed"`
	Total      int     `json:"total"`
	Unread     int     `json:"unread"`
	Flagged    int     `json:"flagged"`
	// Deletable is false for INBOX and for the folders a client relies on by
	// name. The UI uses it to hide the delete affordance rather than to decide
	// whether a request is allowed.
	Deletable bool `json:"deletable"`
	// Owner is the mailbox address this folder belongs to. It is empty for the
	// signed-in user's own mailbox, and set when the folder is in a shared
	// mailbox. The client uses it to know which mailbox a folder operation
	// should target.
	Owner string `json:"owner,omitempty"`
}

// standardFolders are the names a client addresses by role rather than by
// choice, so they cannot be deleted or renamed out from under it.
var standardFolders = map[string]bool{
	"INBOX": true, "Sent": true, "Drafts": true, "Junk": true, "Trash": true, "Archive": true,
}

// Folders returns an owner's folders with their counts, in display order.
func (s *Service) Folders(ctx context.Context, owner string) ([]Folder, error) {
	folders, err := s.mailboxes.ListForUser(ctx, s.db, owner)
	if err != nil {
		return nil, err
	}
	// The counts are read by mailbox ID rather than by position. They used to be
	// taken from an index-aligned slice, which broke the moment the folders were
	// sorted for display: ListForUser orders by name and the display order puts
	// INBOX first, so every folder after INBOX showed its neighbour's unread
	// count. Keying on the ID means a sort cannot detach the two.
	stats, err := s.mailboxes.FolderStats(ctx, s.db, owner)
	if err != nil {
		return nil, err
	}
	imapd.SortFolders(folders)

	out := make([]Folder, len(folders))
	for i, mb := range folders {
		counts := stats[mb.ID]
		out[i] = Folder{
			ID:         mb.ID,
			Name:       mb.Name,
			ParentID:   mb.ParentID,
			SpecialUse: mb.SpecialUse,
			Subscribed: mb.Subscribed,
			Total:      counts.Total,
			Unread:     counts.Unread,
			Flagged:    counts.Flagged,
			Deletable:  !standardFolders[mb.Name],
			Owner:      owner,
		}
	}
	return out, nil
}

// EnsureFolders creates the standard folder set for an owner when it does not
// exist yet.
//
// Delivery does this for every message it files, but a shared mailbox can be
// opened by a member before its first message arrives, and a member who is
// shown a mailbox with no folders cannot file anything in it. The call is
// idempotent, so asking on every read costs a no-op rather than a duplicate.
func (s *Service) EnsureFolders(ctx context.Context, owner string) error {
	return s.mailboxes.EnsureFolders(ctx, s.db, owner)
}

// CreateFolder makes a new top-level folder for an owner.
func (s *Service) CreateFolder(ctx context.Context, owner, name string) (Folder, error) {
	return s.CreateFolderIn(ctx, owner, name, nil)
}

// CreateFolderIn makes a folder inside parentID, or at the top level when
// parentID is nil. The stored name is the full path ("INBOX/Projects"), which
// is also the IMAP name; parent_id carries the tree for the web client.
func (s *Service) CreateFolderIn(ctx context.Context, owner, name string, parentID *int64) (Folder, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Folder{}, fmt.Errorf("%w: a folder needs a name", ErrInvalid)
	}
	if strings.ContainsAny(name, "/\r\n\x00") {
		return Folder{}, fmt.Errorf("%w: %q is not a usable folder name", ErrInvalid, name)
	}
	fullName := name
	if parentID != nil {
		parent, err := s.mailboxes.GetFolder(ctx, s.db, owner, *parentID)
		if err != nil {
			return Folder{}, fmt.Errorf("%w: the parent folder does not exist", ErrInvalid)
		}
		fullName = parent.Name + "/" + name
	}
	if strings.EqualFold(fullName, "INBOX") {
		return Folder{}, fmt.Errorf("%w: INBOX always exists", ErrInvalid)
	}

	if _, err := s.mailboxes.GetByName(ctx, s.db, owner, fullName); err == nil {
		return Folder{}, fmt.Errorf("%w: a folder called %q already exists", ErrInvalid, fullName)
	} else if !store.IsNotFound(err) {
		return Folder{}, err
	}

	const insert = `
		INSERT INTO mailboxes (owner, name, parent_id, uidvalidity, uidnext, created_by)
		VALUES ($1, $2, $3, 1, 1, $1)
		RETURNING id`
	var id int64
	if err := s.db.GetContext(ctx, &id, insert, owner, fullName, parentID); err != nil {
		if store.IsConflict(err) {
			return Folder{}, fmt.Errorf("%w: a folder called %q already exists", ErrInvalid, fullName)
		}
		return Folder{}, fmt.Errorf("webmail: create folder: %w", err)
	}
	s.recordAudit(ctx, owner, "folder.create", fullName)

	folders, err := s.Folders(ctx, owner)
	if err != nil {
		return Folder{}, err
	}
	for _, f := range folders {
		if f.ID == id {
			return f, nil
		}
	}
	return Folder{}, fmt.Errorf("webmail: folder %d vanished after creation", id)
}

// RenameFolder renames a folder an owner owns.
//
// The check runs against the name in the database rather than a spelling the
// caller supplied, so "inbox" cannot be used to slip past the INBOX check by
// case.
func (s *Service) RenameFolder(ctx context.Context, owner string, id int64, name string) (Folder, error) {
	mb, err := s.mailboxes.GetFolder(ctx, s.db, owner, id)
	if err != nil {
		return Folder{}, translate(err)
	}
	if standardFolders[mb.Name] {
		return Folder{}, fmt.Errorf("%w: %s is a standard folder", ErrInvalid, mb.Name)
	}

	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, "/\r\n\x00") {
		return Folder{}, fmt.Errorf("%w: that is not a usable folder name", ErrInvalid)
	}
	// The caller types the leaf. The stored name is the full IMAP path, so a
	// child of INBOX keeps its parent even when the reader renames "Eventi" to
	// "Events".
	fullName := name
	if mb.ParentID != nil {
		parent, err := s.mailboxes.GetFolder(ctx, s.db, owner, *mb.ParentID)
		if err != nil {
			return Folder{}, translate(err)
		}
		fullName = parent.Name + "/" + name
	}

	if err := s.mailboxes.RenameSubtree(ctx, s.db, id, fullName); err != nil {
		return Folder{}, translate(err)
	}
	s.recordAudit(ctx, owner, "folder.rename", fmt.Sprintf("%s -> %s", mb.Name, fullName))
	return s.folderByID(ctx, owner, id)
}

// DeleteFolder empties a folder into the Trash and then removes it.
//
// Removing a folder that still holds mail without asking would be the one
// destructive action in the client with no confirmation, so the contents go to
// the Trash first: the delete is undoable by dragging them back.
func (s *Service) DeleteFolder(ctx context.Context, owner string, id int64) error {
	mb, err := s.mailboxes.GetFolder(ctx, s.db, owner, id)
	if err != nil {
		return translate(err)
	}
	if mb.Name == "INBOX" {
		return fmt.Errorf("%w: INBOX cannot be deleted", ErrInvalid)
	}

	// Anything still in the folder goes to the Trash first.
	if err := s.moveAll(ctx, owner, id, "Trash"); err != nil {
		return err
	}

	// A folder with children cannot be deleted, or the children would be
	// deleted with it by the cascade. Refusing is the honest answer.
	const children = `SELECT count(*) FROM mailboxes WHERE parent_id = $1`
	var n int
	if err := s.db.GetContext(ctx, &n, children, id); err != nil {
		return fmt.Errorf("webmail: count subfolders: %w", err)
	}
	if n > 0 {
		return fmt.Errorf("%w: empty or remove the %d folder(s) inside it first", ErrInvalid, n)
	}

	const remove = `DELETE FROM mailboxes WHERE id = $1 AND owner = $2`
	if _, err := s.db.ExecContext(ctx, remove, id, owner); err != nil {
		return fmt.Errorf("webmail: delete folder: %w", err)
	}
	s.recordAudit(ctx, owner, "folder.delete", mb.Name)
	return nil
}

// EmptyFolder moves everything in a folder to the Trash and returns the count.
func (s *Service) EmptyFolder(ctx context.Context, owner string, id int64) (int, error) {
	mb, err := s.mailboxes.GetFolder(ctx, s.db, owner, id)
	if err != nil {
		return 0, translate(err)
	}
	if mb.Name == "INBOX" {
		return 0, fmt.Errorf("%w: emptying INBOX is not a thing; delete messages instead", ErrInvalid)
	}
	return s.moveAllCounted(ctx, owner, id, "Trash")
}

// moveAll moves every message of a folder to a named folder.
func (s *Service) moveAll(ctx context.Context, owner string, fromID int64, toName string) error {
	_, err := s.moveAllCounted(ctx, owner, fromID, toName)
	return err
}

func (s *Service) moveAllCounted(ctx context.Context, owner string, fromID int64, toName string) (int, error) {
	uids, err := s.uidsIn(ctx, fromID)
	if err != nil {
		return 0, err
	}
	if len(uids) == 0 {
		return 0, nil
	}
	moved, err := s.mailboxes.MoveToNamed(ctx, s.db, owner, fromID, uids, toName)
	if err != nil {
		return 0, translate(err)
	}
	return moved, nil
}

// uidsIn returns every UID of a folder.
func (s *Service) uidsIn(ctx context.Context, mailboxID int64) ([]uint32, error) {
	rows, err := s.db.QueryxContext(ctx,
		`SELECT uid FROM messages WHERE mailbox_id = $1 ORDER BY uid`, mailboxID)
	if err != nil {
		return nil, fmt.Errorf("webmail: list uids: %w", err)
	}
	defer rows.Close()

	var uids []uint32
	for rows.Next() {
		var uid uint32
		if err := rows.Scan(&uid); err != nil {
			return nil, fmt.Errorf("webmail: scan uid: %w", err)
		}
		uids = append(uids, uid)
	}
	return uids, rows.Err()
}

func (s *Service) folderByID(ctx context.Context, owner string, id int64) (Folder, error) {
	folders, err := s.Folders(ctx, owner)
	if err != nil {
		return Folder{}, err
	}
	for _, f := range folders {
		if f.ID == id {
			return f, nil
		}
	}
	return Folder{}, fmt.Errorf("%w: folder %d", ErrNotFound, id)
}

// ListQuery selects a page of a folder.
type ListQuery struct {
	FolderID   int64
	Limit      int
	CursorDate time.Time
	CursorID   int64
	UnreadOnly bool
	Flagged    bool
}

// Message is one row of a listing, as the client needs it to draw a line.
type Message struct {
	ID          int64     `json:"id"`
	FolderID    int64     `json:"folder_id"`
	UID         uint32    `json:"uid"`
	Subject     string    `json:"subject"`
	From        string    `json:"from"`
	FromName    string    `json:"from_name"`
	Snippet     string    `json:"snippet"`
	Date        time.Time `json:"date"`
	Size        int64     `json:"size"`
	Flags       []string  `json:"flags"`
	Attachments bool      `json:"has_attachments"`
	ThreadKey   string    `json:"thread_key"`
	// ItemClass is the MS-OXPROPS class the message arrived with. It is
	// derived from the message when it is filed and is what an Exchange client
	// filters a folder by. See mail.ItemClass.
	ItemClass string `json:"item_class,omitempty"`
	// MessageID is the RFC 5322 Message-ID header, denormalised from the
	// message itself. It is what a client shows as the Internet Message-ID and
	// what S/MIME and threading paths use to refer to the message without
	// owning an identifier of this server's.
	MessageID string `json:"message_id,omitempty"`
}

// Page is one page of a folder listing.
type Page struct {
	Messages   []Message `json:"messages"`
	Total      int       `json:"total"`
	HasMore    bool      `json:"has_more"`
	NextCursor *Cursor   `json:"next_cursor,omitempty"`
}

// Cursor is the position of the last message on a page, echoed back as the next
// page's starting point.
type Cursor struct {
	Date time.Time `json:"date"`
	ID   int64     `json:"id"`
}

// Messages returns a page of a folder.
func (s *Service) Messages(ctx context.Context, owner string, q ListQuery) (Page, error) {
	mb, err := s.mailboxes.GetFolder(ctx, s.db, owner, q.FolderID)
	if err != nil {
		return Page{}, translate(err)
	}

	page, err := s.messages.List(ctx, s.db, imapd.ListOptions{
		MailboxID:  mb.ID,
		Limit:      q.Limit,
		CursorDate: q.CursorDate,
		CursorID:   q.CursorID,
		UnreadOnly: q.UnreadOnly,
		Flagged:    q.Flagged,
	})
	if err != nil {
		return Page{}, err
	}

	out := Page{Messages: make([]Message, 0, len(page.Messages)), Total: page.Total, HasMore: page.HasMore}
	for _, m := range page.Messages {
		out.Messages = append(out.Messages, toMessage(m))
	}
	if page.HasMore && len(page.Messages) > 0 {
		out.NextCursor = &Cursor{Date: page.CursorDate.UTC(), ID: page.CursorID}
	}
	return out, nil
}

func toMessage(m imapd.Message) Message {
	flags := m.Flags
	if flags == nil {
		flags = []string{}
	}
	return Message{
		ID:          m.ID,
		FolderID:    m.MailboxID,
		UID:         m.UID,
		Subject:     m.Subject,
		From:        m.FromAddress,
		FromName:    m.FromName,
		Snippet:     m.Snippet,
		Date:        m.InternalDate,
		Size:        m.Size,
		Flags:       flags,
		Attachments: m.HasAttachments,
		ThreadKey:   m.ThreadKey,
		ItemClass:   m.ItemClass,
		MessageID:   m.MessageID,
	}
}

// MessageFromRow turns a store row into the summary a protocol draws.
//
// It exists because two protocols -- this one and EWS -- list messages from the
// structured query rather than through a page of this service, and each would
// otherwise spell the row-to-summary mapping itself. Two spellings of the same
// mapping are two places for one of them to forget a column, and the column it
// forgot would be one that appears in one protocol's listings and not the other's.
func MessageFromRow(m imapd.Message) Message { return toMessage(m) }

// recordAudit writes an audit row and logs a warning if it cannot.
//
// The action has already happened by the time this is called, so a failed audit
// write must not fail the request: refusing to report having moved a folder
// would leave the client showing an error for something that did happen.
func (s *Service) recordAudit(ctx context.Context, owner, action, target string) {
	if err := s.writeAudit(ctx, owner, action, target); err != nil {
		s.log.Warn("audit entry not written", "action", action, "target", target, "error", err)
	}
}

// writeAudit records an action against the owner's mail.
//
// Failures are the caller's problem to ignore, not to fail on: the action
// already happened, and refusing to have moved a folder because the audit row
// could not be written would leave the two out of step in the other direction.
func (s *Service) writeAudit(ctx context.Context, owner, action, target string) error {
	actor := owner
	detail, err := json.Marshal(map[string]string{"target": target})
	if err != nil {
		return err
	}
	_, err = s.audit.Record(ctx, s.db, store.AuditEntry{
		ActorAddress:  &actor,
		TargetAddress: &actor,
		Action:        action,
		TargetType:    "mail",
		TargetID:      target,
		Detail:        detail,
	})
	return err
}

// translate converts a store error into one the HTTP layer can act on.
func translate(err error) error {
	switch {
	case err == nil:
		return nil
	case store.IsNotFound(err):
		return fmt.Errorf("%w: %s", ErrNotFound, err)
	case store.IsConflict(err):
		return fmt.Errorf("%w: %s", ErrInvalid, err)
	default:
		return err
	}
}
