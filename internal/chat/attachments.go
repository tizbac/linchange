package chat

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/linchange/linchange/internal/blobstore"
)

// MaxAttachmentBytes bounds one chat file. It matches the webmail attachment
// limit and keeps a room from becoming a general-purpose file host.
const MaxAttachmentBytes = 25 << 20

// Attachment is one file sent into a conversation. The bytes are addressed by
// BlobSHA256 in the blob store; the rest is what a client needs to show and
// download it.
type Attachment struct {
	ID             int64     `db:"id" json:"-"`
	UID            string    `db:"uid" json:"uid"`
	ConversationID int64     `db:"conversation_id" json:"-"`
	Uploader       string    `db:"uploader" json:"uploader"`
	BlobSHA256     []byte    `db:"blob_sha256" json:"-"`
	Name           string    `db:"name" json:"name"`
	MIME           string    `db:"mime" json:"mime"`
	Size           int64     `db:"size" json:"size"`
	CreatedAt      time.Time `db:"created_at" json:"createdAt"`
}

// Attach stores a file for a conversation and returns its metadata. The file
// is not visible to anyone until a message references it; membership is checked
// here so a stranger cannot use the room as storage.
func (s *Service) Attach(ctx context.Context, conversationID int64, uploader, name, mime string, data []byte) (Attachment, error) {
	uploader = normalise(uploader)
	if uploader == "" {
		return Attachment{}, fmt.Errorf("%w: an uploader is required", ErrInvalid)
	}
	if s.blobs == nil {
		return Attachment{}, errors.New("chat: no blob store is configured")
	}
	if len(data) == 0 {
		return Attachment{}, fmt.Errorf("%w: an empty file cannot be attached", ErrInvalid)
	}
	if len(data) > MaxAttachmentBytes {
		return Attachment{}, fmt.Errorf("%w: a file is too large", ErrInvalid)
	}
	member, err := s.IsMember(ctx, conversationID, uploader)
	if err != nil {
		return Attachment{}, err
	}
	if !member {
		return Attachment{}, ErrNotMember
	}

	name = safeFilename(name)
	if name == "" {
		name = "attachment"
	}
	mime = strings.TrimSpace(mime)
	if mime == "" {
		mime = "application/octet-stream"
	}

	digest, size, err := s.blobs.Put(data)
	if err != nil {
		return Attachment{}, fmt.Errorf("chat: storing attachment: %w", err)
	}
	uid, err := newAttachmentUID()
	if err != nil {
		return Attachment{}, err
	}

	var att Attachment
	err = s.db.QueryRowxContext(ctx, `
		INSERT INTO chat_attachments
			(uid, conversation_id, uploader, blob_sha256, name, mime, size)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, uid, conversation_id, uploader, blob_sha256, name, mime, size, created_at`,
		uid, conversationID, uploader, digest[:], name, mime, size,
	).StructScan(&att)
	if err != nil {
		return Attachment{}, fmt.Errorf("chat: recording attachment: %w", err)
	}
	return att, nil
}

// AttachmentFor returns an attachment and its bytes to a member of the room it
// belongs to. A non-member gets ErrNotMember, and an unknown identifier gets
// ErrNotFound; the HTTP layer presents both as "not found".
func (s *Service) AttachmentFor(ctx context.Context, uid, requester string) (Attachment, []byte, error) {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return Attachment{}, nil, ErrNotFound
	}
	var att Attachment
	err := s.db.GetContext(ctx, &att, `
		SELECT id, uid, conversation_id, uploader, blob_sha256, name, mime, size, created_at
		FROM chat_attachments
		WHERE uid = $1`, uid)
	if errors.Is(err, sql.ErrNoRows) {
		return Attachment{}, nil, ErrNotFound
	}
	if err != nil {
		return Attachment{}, nil, fmt.Errorf("chat: loading attachment: %w", err)
	}
	member, err := s.IsMember(ctx, att.ConversationID, normalise(requester))
	if err != nil {
		return Attachment{}, nil, err
	}
	if !member {
		return Attachment{}, nil, ErrNotMember
	}
	if s.blobs == nil {
		return Attachment{}, nil, errors.New("chat: no blob store is configured")
	}
	digest, err := digestFromBytes(att.BlobSHA256)
	if err != nil {
		return Attachment{}, nil, err
	}
	data, err := s.blobs.Get(digest)
	if err != nil {
		if errors.Is(err, blobstore.ErrNotFound) {
			return Attachment{}, nil, ErrNotFound
		}
		return Attachment{}, nil, fmt.Errorf("chat: reading attachment: %w", err)
	}
	return att, data, nil
}

// linkAttachment checks that an attachment can be sent as a message: it must
// belong to this conversation, be uploaded by this sender, and not already be
// attached to another message. It returns the row id to store on the message.
func (s *Service) linkAttachment(ctx context.Context, conversationID int64, attachmentUID, sender string) (int64, error) {
	attachmentUID = strings.TrimSpace(attachmentUID)
	if attachmentUID == "" {
		return 0, nil
	}
	var row struct {
		ID             int64         `db:"id"`
		ConversationID int64         `db:"conversation_id"`
		Uploader       string        `db:"uploader"`
		MessageID      sql.NullInt64 `db:"message_id"`
	}
	err := s.db.GetContext(ctx, &row, `
		SELECT a.id, a.conversation_id, a.uploader, m.id AS message_id
		FROM chat_attachments a
		LEFT JOIN chat_messages m ON m.attachment_id = a.id
		WHERE a.uid = $1`, attachmentUID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: no such attachment", ErrInvalid)
	}
	if err != nil {
		return 0, fmt.Errorf("chat: checking attachment: %w", err)
	}
	if row.ConversationID != conversationID {
		return 0, fmt.Errorf("%w: the attachment belongs to another room", ErrInvalid)
	}
	if normalise(row.Uploader) != normalise(sender) {
		return 0, fmt.Errorf("%w: the attachment was uploaded by somebody else", ErrInvalid)
	}
	if row.MessageID.Valid {
		return 0, fmt.Errorf("%w: the attachment has already been sent", ErrInvalid)
	}
	return row.ID, nil
}

func digestFromBytes(b []byte) (blobstore.Digest, error) {
	var d blobstore.Digest
	if len(b) != len(d) {
		return d, errors.New("chat: attachment digest has the wrong length")
	}
	copy(d[:], b)
	return d, nil
}

func newAttachmentUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("chat: generating an attachment id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// safeFilename keeps the display name from carrying a path. Browsers already
// strip one, but a client is not the only way an upload can arrive.
func safeFilename(name string) string {
	name = strings.ReplaceAll(strings.TrimSpace(name), "\\", "/")
	name = path.Base(name)
	if name == "." || name == "/" {
		return ""
	}
	if len(name) > 255 {
		name = name[:255]
	}
	return name
}
