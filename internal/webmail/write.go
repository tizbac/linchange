package webmail

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/delivery"
	"github.com/linchange/linchange/internal/mail"
	"github.com/linchange/linchange/internal/store"
)

// clientFlags are the IMAP system flags a web client may set and clear.
//
// The list is closed rather than a pass-through because the IMAP namespace is
// shared with the protocol: letting a request set \Recent would let a web client
// forge a flag that only the server is entitled to set, and letting it set an
// unknown flag stores garbage that every IMAP client then has to display.
var clientFlags = map[string]bool{
	`\Seen`:     true,
	`\Answered`: true,
	`\Flagged`:  true,
	`\Deleted`:  true,
	`\Draft`:    true,
}

// IsClientFlag reports whether a flag may be set from the web client.
func IsClientFlag(flag string) bool { return clientFlags[flag] }

// MarkRead sets or clears \Seen.
//
// Reading a message is the one state change a mail client performs on its own,
// so it is a first-class call rather than a flag the UI has to remember to set: a
// client that opens a message in a preview pane has read it either way.
func (s *Service) MarkRead(ctx context.Context, owner string, id int64, read bool) error {
	// The owner check happens either way. Without it, an unscoped UPDATE on a
	// bigserial is an enumeration of other people's mail, and the "clear the flag"
	// call is the one that never reads the row first.
	if _, err := s.messages.GetByID(ctx, s.db, owner, id); err != nil {
		return translate(err)
	}
	if read {
		return s.messages.AddFlag(ctx, s.db, id, `\Seen`)
	}
	return s.messages.RemoveFlag(ctx, s.db, id, `\Seen`)
}

// SetFlag sets or clears one system flag on a message the caller owns.
func (s *Service) SetFlag(ctx context.Context, owner string, id int64, flag string, set bool) error {
	flag = strings.TrimSpace(flag)
	if !IsClientFlag(flag) {
		return fmt.Errorf("%w: %q is not a flag a client may set", ErrInvalid, flag)
	}
	if _, err := s.messages.GetByID(ctx, s.db, owner, id); err != nil {
		return translate(err)
	}
	if set {
		return s.messages.AddFlag(ctx, s.db, id, flag)
	}
	return s.messages.RemoveFlag(ctx, s.db, id, flag)
}

// Move puts messages into a folder and returns how many moved.
//
// It is the action behind dragging, "move to", and the Junk and Archive buttons.
// Messages already in the destination are skipped rather than moved onto
// themselves, which would otherwise spend a UID in the destination for nothing.
func (s *Service) Move(ctx context.Context, owner string, ids []int64, destination int64) (int, error) {
	if len(ids) == 0 {
		return 0, fmt.Errorf("%w: no messages were named", ErrInvalid)
	}
	dest, err := s.mailboxes.GetFolder(ctx, s.db, owner, destination)
	if err != nil {
		return 0, translate(err)
	}

	type plan struct {
		mailboxID int64
		uid       uint32
	}
	var moves []plan
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		row, err := s.messages.GetByID(ctx, s.db, owner, id)
		if err != nil {
			return 0, translate(err)
		}
		if row.MailboxID == dest.ID || seen[id] {
			continue
		}
		seen[id] = true
		moves = append(moves, plan{mailboxID: row.MailboxID, uid: row.UID})
	}
	if len(moves) == 0 {
		return 0, nil
	}

	// UIDs are only unique within a mailbox, so a batch spanning several source
	// folders is moved one folder at a time rather than as one UID set that
	// would address whichever message happened to hold that UID elsewhere.
	byFolder := map[int64][]uint32{}
	for _, m := range moves {
		byFolder[m.mailboxID] = append(byFolder[m.mailboxID], m.uid)
	}
	moved := 0
	for mailboxID, uids := range byFolder {
		result, err := s.messages.MoveTo(ctx, s.db, mailboxID, uids, dest.ID)
		if err != nil {
			return moved, err
		}
		moved += len(result)
	}
	s.recordAudit(ctx, owner, "message.move", fmt.Sprintf("%d message(s) to %s", moved, dest.Name))
	return moved, nil
}

// Trash moves messages to the Trash.
func (s *Service) Trash(ctx context.Context, owner string, ids []int64) (int, error) {
	return s.moveToFolderNamed(ctx, owner, ids, "Trash")
}

// Archive moves messages to the Archive.
func (s *Service) Archive(ctx context.Context, owner string, ids []int64) (int, error) {
	return s.moveToFolderNamed(ctx, owner, ids, "Archive")
}

func (s *Service) moveToFolderNamed(ctx context.Context, owner string, ids []int64, name string) (int, error) {
	dest, err := s.mailboxes.GetByName(ctx, s.db, owner, name)
	if err != nil {
		if !store.IsNotFound(err) {
			return 0, err
		}
		// The standard folders exist for any account the delivery path has
		// touched, but a new account may have none, and trashing mail from one
		// should not fail because of that.
		if err := s.mailboxes.EnsureFolders(ctx, s.db, owner); err != nil {
			return 0, err
		}
		dest, err = s.mailboxes.GetByName(ctx, s.db, owner, name)
		if err != nil {
			return 0, err
		}
	}
	return s.Move(ctx, owner, ids, dest.ID)
}

// Delete removes messages for good and returns how many went, and the owner's
// remaining usage.
//
// This is the destructive one, and it is a separate call from Trash on purpose:
// the client asks for it explicitly and the UI is expected to confirm. The usage
// comes back because a user who deletes mail to free space needs to see that it
// worked.
func (s *Service) Delete(ctx context.Context, owner string, ids []int64) (int, store.QuotaUsage, error) {
	if len(ids) == 0 {
		return 0, store.QuotaUsage{}, fmt.Errorf("%w: no messages were named", ErrInvalid)
	}

	deleted := 0
	for _, id := range ids {
		row, err := s.messages.GetByID(ctx, s.db, owner, id)
		if err != nil {
			return deleted, store.QuotaUsage{}, translate(err)
		}
		if err := s.messages.DeleteByUID(ctx, s.db, row.MailboxID, row.UID); err != nil {
			if store.IsNotFound(err) {
				continue
			}
			return deleted, store.QuotaUsage{}, err
		}
		deleted++
	}
	if deleted > 0 {
		s.recordAudit(ctx, owner, "message.delete", fmt.Sprintf("%d message(s)", deleted))
	}

	usage, err := store.NewQuotas().Get(ctx, s.db, owner)
	if err != nil {
		return deleted, store.QuotaUsage{}, err
	}
	return deleted, usage, nil
}

// ---------------------------------------------------------------------------
// Composing.
// ---------------------------------------------------------------------------

// AttachmentInput is one file the composer is attaching.
//
// The content arrives base64-encoded because the composer posts JSON, and the
// size limit is checked against the decoded bytes rather than the encoded ones: a
// caller must not be able to send a 40MB attachment inside 54MB of base64.
type AttachmentInput struct {
	Filename string `json:"filename"`
	MIMEType string `json:"mime_type"`
	// Content is standard base64. A data URL is also accepted, because that is
	// what a browser produces when a file is read with FileReader.
	Content string `json:"content"`
	// ContentID ties an inline image to the cid: URL in the HTML body.
	ContentID string `json:"content_id,omitempty"`
}

// ComposeRequest is what the compose form posts.
type ComposeRequest struct {
	From    string    `json:"from"`
	To      []Address `json:"to"`
	Cc      []Address `json:"cc"`
	Bcc     []Address `json:"bcc"`
	Subject string    `json:"subject"`
	Text    string    `json:"text_body,omitempty"`
	HTML    string    `json:"html_body,omitempty"`
	ReplyTo []Address `json:"reply_to,omitempty"`
	// InReplyTo and References thread a reply with its conversation. A client
	// that does not thread can leave them empty and the message still sends.
	InReplyTo  string            `json:"in_reply_to,omitempty"`
	References []string          `json:"references,omitempty"`
	DraftID    int64             `json:"draft_id,omitempty"`
	Attachment []AttachmentInput `json:"attachments,omitempty"`
}

// Validate checks a request without touching the database.
//
// It is separate from Send because the composer wants to say what is wrong while
// the user is still typing, and because the rules engine applies the same checks
// to a message that did not come from a form.
//
// This is the check for a message that is going to be sent, so it demands a
// recipient and a body. A draft is held to validateDraft instead.
func (r ComposeRequest) Validate() error {
	if len(r.To) == 0 {
		return fmt.Errorf("%w: a message needs at least one recipient", ErrInvalid)
	}
	if r.Text == "" && r.HTML == "" && len(r.Attachment) == 0 {
		return fmt.Errorf("%w: the message has no body", ErrInvalid)
	}
	return r.validateHeaders()
}

// validateDraft checks a request that is only being stored.
//
// A draft is a partly written message, and the two things a sent message must
// have are exactly the two things a draft is most often missing: a draft saved
// on the way out of a long meeting has a subject and no recipient, and one
// saved on the way into a long meeting has a recipient and no body. Refusing
// either of those means the user's own work is thrown away for not being a
// message yet, which is the one thing a draft is allowed not to be.
//
// What is still checked is everything that is present. A malformed address or an
// over-long subject is not "not finished yet", it is wrong, and the user wants
// to hear about it while the draft is still open rather than when they send it.
func (r ComposeRequest) validateDraft() error {
	if r.Text == "" && r.HTML == "" && r.Subject == "" && len(r.Attachment) == 0 &&
		len(r.To) == 0 && len(r.Cc) == 0 && len(r.Bcc) == 0 {
		return fmt.Errorf("%w: an empty draft has nothing to save", ErrInvalid)
	}
	return r.validateHeaders()
}

// validateHeaders holds the rules that apply whether the message is being sent
// or only stored. They are about the fields being well-formed rather than
// complete, which is exactly why both callers want them.
func (r ComposeRequest) validateHeaders() error {
	// RFC 5322 caps a header line at 998 octets, and folding does not help a
	// subject that is one word 1200 characters long.
	if len(r.Subject) > 998 {
		return fmt.Errorf("%w: the subject is %d characters, over the 998 limit", ErrInvalid, len(r.Subject))
	}
	for _, group := range []struct {
		kind  string
		addrs []Address
	}{{"To", r.To}, {"Cc", r.Cc}, {"Bcc", r.Bcc}, {"Reply-To", r.ReplyTo}} {
		for _, a := range group.addrs {
			if _, err := mail.ParseAddressList(a.String()); err != nil {
				return fmt.Errorf("%w: %s has %q, which is not an address", ErrInvalid, group.kind, a.Address)
			}
		}
	}
	return nil
}

// recipients returns the envelope recipients, deduplicated, in header order.
//
// The envelope is built from To, Cc and Bcc together, because a recipient in Bcc
// must receive the message even though no header they see names them. Duplicates
// are dropped because a client that lists somebody in both To and Cc would
// otherwise get two copies of the same message.
func (r ComposeRequest) recipients() []delivery.OutboxRecipient {
	seen := make(map[string]bool)
	var out []delivery.OutboxRecipient
	for _, group := range []struct {
		kind  delivery.OutboxRecipientKind
		addrs []Address
	}{
		{delivery.To, r.To},
		{delivery.CC, r.Cc},
		{delivery.BCC, r.Bcc},
	} {
		for _, a := range group.addrs {
			key := strings.ToLower(strings.TrimSpace(a.Address))
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, delivery.OutboxRecipient{
				Position: len(out),
				Address:  key,
				Kind:     group.kind,
			})
		}
	}
	return out
}

// SendResult reports what a send did.
type SendResult struct {
	// OutboxID is the queued message, which is what the UI shows as "queued".
	// The SMTP conversation happens in the delivery worker afterwards, so a
	// successful send is not yet a delivered one.
	OutboxID  int64  `json:"outbox_id"`
	MessageID string `json:"message_id"`
	// SentMessageID is the copy filed in the Sent folder, when there is one.
	SentMessageID int64 `json:"sent_message_id,omitempty"`
}

// Send queues a message for delivery and files a copy in the Sent folder.
//
// Both halves happen here rather than in the worker, because a user who asks to
// send a message and then finds nothing in their Sent folder is the most
// confusing outcome a mail client can produce. The copy is written from the same
// bytes that were built, so what the user reads back is what was sent.
func (s *Service) Send(ctx context.Context, owner string, req ComposeRequest) (SendResult, error) {
	if err := req.Validate(); err != nil {
		return SendResult{}, err
	}

	from := strings.ToLower(strings.TrimSpace(req.From))
	if from == "" {
		from = strings.ToLower(owner)
	}
	// Otherwise a client could send as anybody by putting their address in From.
	if from != strings.ToLower(owner) && !s.sendsAs(ctx, owner, from) {
		return SendResult{}, fmt.Errorf("%w: you cannot send as %s", ErrDenied, from)
	}

	// The signature goes on after validation and before the message is built,
	// because it is part of what is sent rather than part of what the user
	// wrote: it belongs in the bytes that are built, and it must not be able to
	// make an otherwise invalid message valid.
	images, err := s.applySignature(ctx, owner, &req)
	if err != nil {
		return SendResult{}, err
	}

	raw, err := s.buildWithImages(&req, from, mail.NewMessageID(s.hostname), images)
	if err != nil {
		return SendResult{}, err
	}

	// Bcc must reach its recipients without appearing in the headers those
	// recipients see. The queued blob is what every recipient receives, so the
	// header is stripped from the copy that goes out and kept only in the Sent
	// folder copy the sender reads.
	outgoing := stripBcc(raw)

	digest, size, err := s.blobs.Put(outgoing)
	if err != nil {
		return SendResult{}, fmt.Errorf("webmail: store the message: %w", err)
	}

	var outboxID int64
	err = store.InTx(ctx, s.db, func(tx *sqlx.Tx) error {
		id, err := s.outbox.InsertMessage(ctx, tx, owner, from, digest[:], size, req.recipients())
		if err != nil {
			return err
		}
		outboxID = id
		return nil
	})
	if err != nil {
		return SendResult{}, err
	}
	s.recordAudit(ctx, owner, "message.send", fmt.Sprintf("%d recipient(s)", len(req.recipients())))

	result := SendResult{OutboxID: outboxID, MessageID: messageIDOf(raw)}
	sentID, err := s.file(ctx, owner, "Sent", raw, []string{`\Seen`})
	if err != nil {
		// The message is queued and will go out. Failing the request now would
		// have the client offer to send it again, which is a duplicate.
		s.log.Warn("could not file the sent copy", "outbox_id", outboxID, "error", err)
		return result, nil
	}
	result.SentMessageID = sentID
	return result, nil
}

// SaveDraft stores or replaces a draft.
//
// A draft is an ordinary message in the Drafts folder carrying \Draft, not a
// separate kind of object. That is what makes it openable in any IMAP client and
// deletable there too, which is the only way a user can be sure their drafts are
// gone.
func (s *Service) SaveDraft(ctx context.Context, owner string, req ComposeRequest) (int64, error) {
	if err := req.validateDraft(); err != nil {
		return 0, err
	}

	// A draft that replies is threaded like any other reply, so that opening it
	// shows the conversation it belongs to.
	if req.InReplyTo != "" && len(req.References) == 0 {
		req.References = []string{req.InReplyTo}
	}

	messageID := mail.NewMessageID(s.hostname)
	raw, err := s.buildDraft(&req, owner, messageID)
	if err != nil {
		return 0, err
	}

	// Replacing a draft deletes the old one, so saving the same draft ten times
	// leaves one message rather than ten.
	if req.DraftID != 0 {
		if _, err := s.messages.GetByID(ctx, s.db, owner, req.DraftID); err != nil {
			return 0, translate(err)
		}
		if err := s.discardDraft(ctx, owner, req.DraftID); err != nil {
			return 0, err
		}
	}

	id, err := s.file(ctx, owner, "Drafts", raw, []string{`\Draft`})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// discardDraft removes a draft without sending it.
func (s *Service) discardDraft(ctx context.Context, owner string, id int64) error {
	row, err := s.messages.GetByID(ctx, s.db, owner, id)
	if err != nil {
		return translate(err)
	}
	for _, flag := range row.Flags {
		if flag == `\Draft` {
			_, _, err := s.Delete(ctx, owner, []int64{id})
			return err
		}
	}
	return fmt.Errorf("%w: message %d is not a draft", ErrInvalid, id)
}

// build turns a compose request into RFC 5322 bytes.
func (s *Service) build(req *ComposeRequest, from, messageID string) ([]byte, error) {
	return s.buildAs(req, from, messageID, nil, false)
}

// buildWithImages is build for a message that carries images the signature
// lifted out of its own HTML. They are passed rather than looked up here because
// the signature is read once, by the caller that already knows what the user
// sent, and reading it again for the build would be a second chance to disagree
// with the copy that was appended to the body.
func (s *Service) buildWithImages(req *ComposeRequest, from, messageID string, images []mail.InlineImage) ([]byte, error) {
	return s.buildAs(req, from, messageID, images, false)
}

// buildDraft renders a request that is only being stored.
//
// It differs from build in exactly one way: the message is allowed to be
// incomplete. A draft goes to the same folder and carries the same flag as any
// other message, so it is built by the same code, but it must not be held to
// the rules that make a message sendable.
//
// It never carries the signature either, for the same reason it is not required
// to be complete: the signature is added when the message goes out, so a draft
// saved ten times has one signature when it is sent rather than ten in the folder.
func (s *Service) buildDraft(req *ComposeRequest, from, messageID string) ([]byte, error) {
	return s.buildAs(req, from, messageID, nil, true)
}

func (s *Service) buildAs(req *ComposeRequest, from, messageID string, signatureImages []mail.InlineImage, asDraft bool) ([]byte, error) {
	attachments, err := s.decodeAttachments(req.Attachment)
	if err != nil {
		return nil, err
	}

	builder := mail.NewBuilder()
	builder.From(mail.Address{Address: from})
	if n := senderName(req.From); n != "" {
		builder.From(mail.Address{Address: from, Name: n})
	}
	if asDraft {
		builder.Draft()
	}
	builder.To(toMailAddresses(req.To)...)
	builder.Cc(toMailAddresses(req.Cc)...)
	builder.Bcc(toMailAddresses(req.Bcc)...)
	builder.ReplyTo(toMailAddresses(req.ReplyTo)...)
	builder.Subject(req.Subject)
	builder.Date(nowUTC())
	builder.MessageID(messageID)
	if req.InReplyTo != "" {
		builder.Header("In-Reply-To", "<"+strings.Trim(req.InReplyTo, "<>")+">")
	}
	if len(req.References) > 0 {
		builder.Header("References", referenceHeader(req.References))
	}
	if req.Text != "" {
		builder.Text(req.Text)
	}
	if req.HTML != "" {
		builder.HTML(req.HTML)
	}
	for _, a := range attachments {
		if a.ContentID != "" {
			builder.InlineImage(a.Filename, a.MIMEType, a.ContentID, a.Data)
			continue
		}
		builder.Attach(a.Filename, a.MIMEType, a.Data)
	}
	// After the user's own attachments, so that a signature image referenced
	// from the body resolves against the first part that claims its id rather
	// than against whatever happens to be declared first.
	for _, img := range signatureImages {
		builder.InlineImage(img.Filename, img.ContentType, img.ContentID, img.Data)
	}

	raw, err := builder.Bytes()
	if err != nil {
		return nil, fmt.Errorf("webmail: build the message: %w", err)
	}
	if int64(len(raw)) > s.maxMessageBytes {
		return nil, fmt.Errorf("%w: the message is %d bytes, over the %d limit",
			ErrInvalid, len(raw), s.maxMessageBytes)
	}
	return raw, nil
}

// file stores a message in one of the owner's standard folders.
func (s *Service) file(ctx context.Context, owner, folder string, raw []byte, flags []string) (int64, error) {
	if err := s.mailboxes.EnsureFolders(ctx, s.db, owner); err != nil {
		return 0, err
	}
	box, err := s.mailboxes.GetByName(ctx, s.db, owner, folder)
	if err != nil {
		return 0, err
	}

	// The blob is keyed by its own bytes rather than reusing the outgoing
	// digest: the summary row has to describe the bytes it points at, and a
	// stripped Bcc header makes those different messages.
	digest, size, err := s.blobs.Put(raw)
	if err != nil {
		return 0, fmt.Errorf("webmail: store the message: %w", err)
	}
	meta, err := mail.Describe(raw)
	if err != nil {
		return 0, fmt.Errorf("webmail: describe the message: %w", err)
	}

	res, err := s.messages.Insert(ctx, s.db, box.ID, digest[:], size, meta.Date, flags, meta)
	if err != nil {
		return 0, err
	}
	return res.ID, nil
}

// decodedAttachment is one attachment with its bytes in hand.
type decodedAttachment struct {
	Filename  string
	MIMEType  string
	ContentID string
	Data      []byte
}

// decodeAttachments validates and decodes the attached files.
//
// The limits are enforced on decoded bytes. A limit checked against the encoded
// text would be a limit on base64 rather than on the attachment.
func (s *Service) decodeAttachments(in []AttachmentInput) ([]decodedAttachment, error) {
	out := make([]decodedAttachment, 0, len(in))
	var total int64
	for i, a := range in {
		name := strings.TrimSpace(a.Filename)
		if name == "" {
			return nil, fmt.Errorf("%w: attachment %d has no filename", ErrInvalid, i+1)
		}
		// A name that walks out of a directory is never legitimate, and this
		// name ends up in a Content-Disposition header as well as in a client's
		// download box.
		if strings.ContainsAny(name, "/\\\r\n\x00") || name == "." || name == ".." {
			return nil, fmt.Errorf("%w: %q is not a usable attachment name", ErrInvalid, name)
		}

		data, err := decodeBase64(a.Content)
		if err != nil {
			return nil, fmt.Errorf("%w: attachment %q is not base64: %s", ErrInvalid, name, err)
		}
		if int64(len(data)) > s.maxAttachmentBytes {
			return nil, fmt.Errorf("%w: %q is %d bytes, over the %d limit",
				ErrInvalid, name, len(data), s.maxAttachmentBytes)
		}
		total += int64(len(data))
		if total > s.maxMessageBytes {
			return nil, fmt.Errorf("%w: the attachments total %d bytes, over the %d limit",
				ErrInvalid, total, s.maxMessageBytes)
		}

		mime := strings.TrimSpace(a.MIMEType)
		if mime == "" {
			mime = "application/octet-stream"
		}
		out = append(out, decodedAttachment{
			Filename:  name,
			MIMEType:  mime,
			ContentID: strings.Trim(strings.TrimSpace(a.ContentID), "<>"),
			Data:      data,
		})
	}
	return out, nil
}

// decodeBase64 accepts raw base64 and the data URL form a browser produces.
func decodeBase64(s string) ([]byte, error) {
	if _, after, ok := strings.Cut(s, ","); ok && strings.HasPrefix(s, "data:") {
		s = after
	}
	return base64.StdEncoding.DecodeString(strings.TrimSpace(s))
}

// stripBcc removes the Bcc header field from a message.
//
// It works on the header block and leaves the body byte for byte, because a
// re-encode could change the transfer encodings and therefore the bytes a
// recipient's checksum was computed over.
func stripBcc(raw []byte) []byte {
	header, body, ok := splitHeader(raw)
	if !ok {
		return raw
	}

	var out []byte
	dropping := false
	for _, line := range splitLines(header) {
		if isFieldStart(line) {
			dropping = isBccField(line)
		} else if !isContinuation(line) {
			dropping = false
		}
		// A folded continuation of a dropped Bcc goes with it: leaving the
		// continuation behind would splice an address fragment into whatever
		// header follows.
		if dropping {
			continue
		}
		out = append(out, line...)
	}
	return append(out, body...)
}

// splitHeader cuts the header block off the body. The separator belongs to the
// body half: stripBcc reassembles header+body, and handing the separator back
// with the header instead would make every reassembled message a document with
// no empty line between headers and body -- unparseable to every recipient,
// and to this server's own inbound listener.
func splitHeader(raw []byte) (header, body []byte, ok bool) {
	if i := indexBytes(raw, "\r\n\r\n"); i >= 0 {
		return raw[:i+2], raw[i+2:], true
	}
	if i := indexBytes(raw, "\n\n"); i >= 0 {
		return raw[:i+1], raw[i+1:], true
	}
	return nil, raw, false
}

// isFieldStart reports whether a raw header line begins a new field rather than
// continuing the previous one.
func isFieldStart(line []byte) bool {
	if len(line) == 0 {
		return false
	}
	if line[0] == ' ' || line[0] == '\t' {
		return false
	}
	i := 0
	for i < len(line) && line[i] != ':' {
		if line[i] == '\n' {
			return false
		}
		i++
	}
	return i < len(line) && i > 0
}

func isBccField(line []byte) bool {
	name, _, _ := cutByte(line, ':')
	return strings.EqualFold(strings.TrimSpace(string(name)), "Bcc")
}

// isContinuation reports whether a line continues the field above it.
func isContinuation(line []byte) bool {
	return len(line) > 0 && (line[0] == ' ' || line[0] == '\t')
}

func splitLines(b []byte) [][]byte {
	var out [][]byte
	for len(b) > 0 {
		i := indexByte(b, '\n')
		if i < 0 {
			return append(out, b)
		}
		out = append(out, b[:i+1])
		b = b[i+1:]
	}
	return out
}

// indexBytes finds a substring. The messages involved are bounded by the
// message size limit, so a byte-wise search over them is not a concern.
func indexBytes(b []byte, sub string) int { return strings.Index(string(b), sub) }

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

func cutByte(b []byte, sep byte) (before, after []byte, found bool) {
	for i := range b {
		if b[i] == sep {
			return b[:i], b[i+1:], true
		}
	}
	return b, nil, false
}

// senderName returns the display name a composer put on the From address, which
// is only usable if it is the caller's own address.
func senderName(from string) string {
	_, name, _ := strings.Cut(strings.TrimSpace(from), "<")
	return strings.Trim(strings.TrimSpace(name), " <>")
}

func toMailAddresses(in []Address) []mail.Address {
	if len(in) == 0 {
		return nil
	}
	out := make([]mail.Address, 0, len(in))
	for _, a := range in {
		out = append(out, mail.Address{Address: strings.ToLower(strings.TrimSpace(a.Address)), Name: a.Name})
	}
	return out
}

// referenceHeader builds a References header value.
func referenceHeader(refs []string) string {
	parts := make([]string, 0, len(refs))
	for _, r := range refs {
		trimmed := strings.TrimSpace(r)
		if trimmed == "" {
			continue
		}
		parts = append(parts, "<"+strings.Trim(trimmed, "<>")+">")
	}
	return strings.Join(parts, " ")
}

// messageIDOf reads the Message-ID back out of a built message.
func messageIDOf(raw []byte) string {
	header, _, ok := splitHeader(raw)
	if !ok {
		return ""
	}
	for _, line := range splitLines(header) {
		name, value, found := cutByte(line, ':')
		if !found || !strings.EqualFold(strings.TrimSpace(string(name)), "Message-ID") {
			continue
		}
		return strings.Trim(strings.TrimSpace(string(value)), "<>\r\n")
	}
	return ""
}

// sendsAs reports whether an owner may send as from.
//
// The only case is a shared mailbox the owner may contribute to: a team address
// is not the sender's own, and refusing that would make shared mailboxes
// unusable for the reason they exist.
func (s *Service) sendsAs(ctx context.Context, owner, from string) bool {
	access, err := s.grants.Access(ctx, s.db, from, owner)
	if err != nil {
		return false
	}
	return access.AtLeast(policy.GrantContribute)
}
