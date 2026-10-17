package webmail

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/linchange/linchange/internal/blobstore"
	"github.com/linchange/linchange/internal/imapd"
	"github.com/linchange/linchange/internal/mail"
)

// Attachment is one file carried by a message.
type Attachment struct {
	// Index is the position in the message's attachment list, and is what the
	// download endpoint is addressed by. A name is not an identifier: two
	// attachments in one message can share it.
	Index    int    `json:"index"`
	Filename string `json:"filename"`
	// MIMEType is what the sender declared. It is reported rather than trusted:
	// the client is told what the bytes claim to be and what they were sniffed
	// as, and decides.
	MIMEType  string `json:"mime_type"`
	Size      int64  `json:"size"`
	Inline    bool   `json:"inline"`
	ContentID string `json:"content_id,omitempty"`
}

// Address is a header address as the composer and the reader show it.
type Address struct {
	Address string `json:"address"`
	Name    string `json:"name,omitempty"`
}

// String renders the address in header form.
//
// It exists because the composer posts addresses and the composer has to be
// able to send them straight back out: a form that returns "Alice <a@example.com>"
// for display must not send that string as the address.
//
// The display name is quoted when it contains specials. An EWS client puts the
// address itself in the name field, and "a@example.com <a@example.com>" is not a
// legal RFC 5322 phrase: the unquoted atom breaks at the first special, and the
// validator then rejects a perfectly good address. Quoting is the form the
// standard provides, and net/mail parses it back exactly.
func (a Address) String() string {
	if a.Name == "" {
		return a.Address
	}
	return fmt.Sprintf("%s <%s>", headerDisplayName(a.Name), a.Address)
}

// headerDisplayName makes a display name safe for a header.
func headerDisplayName(name string) string {
	if strings.ContainsAny(name, "()[]<>.,;:@\"\\") {
		return `"` + strings.ReplaceAll(name, "\"", "") + `"`
	}
	return name
}

// MessageDetail is a message as the reader needs it: the stored summary, the
// parsed addresses, the bodies, and the attachment list.
//
// The bodies are HTML and plain text, not one "body" string. A web client
// renders the HTML in a sandboxed frame and falls back to the text, and a
// client that only got a concatenation of both would show a user the HTML
// source or the text of a message that has both.
type MessageDetail struct {
	Message
	// ReplyTo and the recipient lists come from the parsed headers rather than
	// from the summary row, because the summary only denormalises the sender.
	From        []Address    `json:"from"`
	To          []Address    `json:"to"`
	Cc          []Address    `json:"cc"`
	Bcc         []Address    `json:"bcc,omitempty"`
	ReplyTo     []Address    `json:"reply_to,omitempty"`
	TextBody    string       `json:"text_body,omitempty"`
	HTMLBody    string       `json:"html_body,omitempty"`
	Attachments []Attachment `json:"attachments"`
	// InlineImages are the parts the body refers to by Content-ID. They are
	// listed separately from attachments because a client serves them out of the
	// message rather than offering them for download.
	InlineImages []InlineImage `json:"inline_images,omitempty"`
	// Headers is the rest of the message's headers, for the details panel. It
	// is bounded: a message can carry any number of headers and a client should
	// not render an unbounded list.
	Headers []Header `json:"headers,omitempty"`
}

// InlineImage is an image part the HTML body refers to.
type InlineImage struct {
	ContentID string `json:"content_id"`
	Filename  string `json:"filename,omitempty"`
	MIMEType  string `json:"mime_type"`
	Size      int64  `json:"size"`
}

// Header is one header row for the details panel.
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// maxHeaders bounds the header list.
const maxHeaders = 40

// Message returns a message with its parsed content.
func (s *Service) Message(ctx context.Context, owner string, id int64) (MessageDetail, error) {
	row, err := s.messages.GetByID(ctx, s.db, owner, id)
	if err != nil {
		return MessageDetail{}, translate(err)
	}

	raw, err := s.readBlob(ctx, row.BlobSHA256)
	if err != nil {
		return MessageDetail{}, err
	}

	parsed, err := mail.Parse(raw)
	if err != nil {
		// A blob that will not parse is a real possibility: the server wrote
		// it, but the bytes may have come from a client. The listing data is
		// still worth showing, so the summary is returned with the bodies
		// empty rather than failing the whole read.
		s.log.Warn("stored message will not parse",
			"message_id", row.ID, "error", err)
		return s.detailWithoutBody(toMessage(row)), nil
	}

	return s.detail(row, parsed), nil
}

// detail assembles the reader view from a parsed message.
func (s *Service) detail(row imapd.Message, parsed *mail.Parsed) MessageDetail {
	out := s.detailWithoutBody(toMessage(row))
	out.From = addressesOf(parsed.AddressList("From"))
	out.To = addressesOf(parsed.AddressList("To"))
	out.Cc = addressesOf(parsed.AddressList("Cc"))
	out.Bcc = addressesOf(parsed.AddressList("Bcc"))
	out.ReplyTo = addressesOf(parsed.AddressList("ReplyTo"))
	out.TextBody = parsed.Text
	out.HTMLBody = parsed.HTML
	out.Attachments = []Attachment{}

	for _, part := range parsed.Attachments {
		a := Attachment{
			Index:    len(out.Attachments),
			Filename: part.Filename,
			MIMEType: part.ContentType,
			Size:     int64(len(part.Content)),
			Inline:   part.Disposition == "inline",
		}
		if a.Filename == "" {
			// A download with no name is saved as "part" by every browser,
			// which is not a useful name for the user to see in a list.
			a.Filename = fmt.Sprintf("attachment-%d", a.Index+1)
		}
		out.Attachments = append(out.Attachments, a)
	}
	for _, part := range parsed.Inline {
		if part.ContentID == "" {
			continue
		}
		out.InlineImages = append(out.InlineImages, InlineImage{
			ContentID: part.ContentID,
			Filename:  part.Filename,
			MIMEType:  part.ContentType,
			Size:      int64(len(part.Content)),
		})
	}

	for _, h := range parsed.HeaderList() {
		if len(out.Headers) >= maxHeaders {
			break
		}
		out.Headers = append(out.Headers, Header{Name: h.Name, Value: h.Value})
	}
	return out
}

// detailWithoutBody is the listing view, which is all that can be said about a
// message whose bytes are unreadable.
func (s *Service) detailWithoutBody(m Message) MessageDetail {
	return MessageDetail{Message: m, Attachments: []Attachment{}}
}

func addressesOf(in []mail.Address) []Address {
	out := make([]Address, 0, len(in))
	for _, a := range in {
		out = append(out, Address{Address: a.Address, Name: a.Name})
	}
	return out
}

// Raw returns the stored bytes of a message, exactly as they were received.
//
// It is what "view source" shows and what a client downloads. It is the one
// endpoint that serves arbitrary content, so it is authenticated and
// owner-scoped like every other, and it is not rendered by the server.
func (s *Service) Raw(ctx context.Context, owner string, id int64) ([]byte, error) {
	row, err := s.messages.GetByID(ctx, s.db, owner, id)
	if err != nil {
		return nil, translate(err)
	}
	return s.readBlob(ctx, row.BlobSHA256)
}

// AttachmentContent returns one attachment of a message.
//
// The message is addressed by id and the attachment by index rather than by
// name, because names repeat within a message and a path built from a
// user-supplied name is a path traversal waiting to happen.
func (s *Service) AttachmentContent(ctx context.Context, owner string, id int64, index int) (Attachment, []byte, error) {
	row, err := s.messages.GetByID(ctx, s.db, owner, id)
	if err != nil {
		return Attachment{}, nil, translate(err)
	}
	if index < 0 {
		return Attachment{}, nil, fmt.Errorf("%w: attachment index is negative", ErrInvalid)
	}

	raw, err := s.readBlob(ctx, row.BlobSHA256)
	if err != nil {
		return Attachment{}, nil, err
	}
	parsed, err := mail.Parse(raw)
	if err != nil {
		return Attachment{}, nil, fmt.Errorf("webmail: message %d will not parse: %w", id, err)
	}
	if index >= len(parsed.Attachments) {
		return Attachment{}, nil, fmt.Errorf("%w: message %d has %d attachment(s)", ErrNotFound, id, len(parsed.Attachments))
	}

	part, ok := parsed.AttachmentByIndex(index)
	if !ok {
		return Attachment{}, nil, fmt.Errorf("%w: message %d has %d attachment(s)", ErrNotFound, id, len(parsed.Attachments))
	}
	a := Attachment{
		Index:    index,
		Filename: part.Filename,
		MIMEType: part.ContentType,
		Size:     int64(len(part.Content)),
		Inline:   part.Disposition == "inline",
	}
	if a.Filename == "" {
		a.Filename = fmt.Sprintf("attachment-%d", index+1)
	}
	return a, part.Content, nil
}

// InlineImageContent returns an inline part by its Content-ID.
//
// It is addressed by Content-ID because that is how the HTML body refers to it,
// so the client can serve it with the exact URL the body contains without
// having to rewrite the body.
func (s *Service) InlineImageContent(ctx context.Context, owner string, id int64, contentID string) (InlineImage, []byte, error) {
	row, err := s.messages.GetByID(ctx, s.db, owner, id)
	if err != nil {
		return InlineImage{}, nil, translate(err)
	}

	raw, err := s.readBlob(ctx, row.BlobSHA256)
	if err != nil {
		return InlineImage{}, nil, err
	}
	parsed, err := mail.Parse(raw)
	if err != nil {
		return InlineImage{}, nil, fmt.Errorf("webmail: message %d will not parse: %w", id, err)
	}

	// Content-IDs appear in the body as cid: URLs, usually without the angle
	// brackets the header has, so both spellings are accepted.
	wanted := strings.Trim(strings.TrimSpace(contentID), "<>")
	for _, part := range parsed.Inline {
		if strings.Trim(strings.TrimSpace(part.ContentID), "<>") != wanted {
			continue
		}
		return InlineImage{
			ContentID: part.ContentID,
			Filename:  part.Filename,
			MIMEType:  part.ContentType,
			Size:      int64(len(part.Content)),
		}, part.Content, nil
	}
	return InlineImage{}, nil, fmt.Errorf("%w: inline image %q in message %d", ErrNotFound, contentID, id)
}

// readBlob loads a stored message by its digest.
func (s *Service) readBlob(ctx context.Context, sum []byte) ([]byte, error) {
	if len(sum) != 32 {
		return nil, fmt.Errorf("webmail: stored digest is %d bytes, want 32", len(sum))
	}
	var digest blobstore.Digest
	copy(digest[:], sum)

	b, err := s.blobs.Get(digest)
	if err != nil {
		if errors.Is(err, blobstore.ErrNotFound) {
			return nil, fmt.Errorf("webmail: message body %s is missing from the blob store", digest)
		}
		return nil, fmt.Errorf("webmail: read message body: %w", err)
	}
	return b, nil
}

// Thread returns a conversation, oldest first.
//
// A conversation is worth showing as one list with the messages in order,
// including the ones in other folders, because a reply that landed in Junk is
// still part of the conversation the user is reading.
func (s *Service) Thread(ctx context.Context, owner, threadKey string) ([]MessageDetail, error) {
	if strings.TrimSpace(threadKey) == "" {
		return nil, fmt.Errorf("%w: a thread key is required", ErrInvalid)
	}
	rows, err := s.messages.ThreadMessages(ctx, s.db, owner, threadKey)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%w: thread %q", ErrNotFound, threadKey)
	}

	out := make([]MessageDetail, 0, len(rows))
	for _, row := range rows {
		detail, err := s.Message(ctx, owner, row.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, detail)
	}
	return out, nil
}

// Threads lists the conversations of a folder.
//
// It is the default view of a folder in a mail client, because a folder of two
// hundred replies to twenty conversations is twenty things to read, not two
// hundred.
func (s *Service) Threads(ctx context.Context, owner string, folderID int64, limit int) ([]Thread, error) {
	mb, err := s.mailboxes.GetFolder(ctx, s.db, owner, folderID)
	if err != nil {
		return nil, translate(err)
	}
	rows, err := s.messages.ListThreads(ctx, s.db, mb.ID, limit)
	if err != nil {
		return nil, err
	}

	out := make([]Thread, 0, len(rows))
	for _, row := range rows {
		t := Thread{
			ThreadKey:    row.ThreadKey,
			Subject:      row.Subject,
			MessageCount: row.MessageCount,
			UnreadCount:  row.UnreadCount,
			Latest:       toMessage(row.Latest),
		}
		if t.Subject == "" {
			// A conversation whose root had no Subject still needs something to
			// show. "(no subject)" is what every client says, and matching it
			// makes a list of such threads recognisable rather than blank rows.
			t.Subject = "(no subject)"
		}
		out = append(out, t)
	}
	return out, nil
}

// Thread is a conversation as a list row.
type Thread struct {
	ThreadKey    string  `json:"thread_key"`
	Subject      string  `json:"subject"`
	MessageCount int     `json:"message_count"`
	UnreadCount  int     `json:"unread_count"`
	Latest       Message `json:"latest"`
}

// Search returns messages matching text across an owner's mail, or one folder.
func (s *Service) Search(ctx context.Context, owner, text string, folderID int64, limit int) ([]Message, error) {
	rows, err := s.messages.SearchMessages(ctx, s.db, imapd.SearchOptions{
		Owner:    owner,
		FolderID: folderID,
		Text:     text,
		Limit:    limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]Message, 0, len(rows))
	for _, row := range rows {
		out = append(out, toMessage(row))
	}
	return out, nil
}

// contentDisposition builds the header that makes a download save under a given
// name rather than under whatever the browser guesses from the URL.
//
// Both the filename and the RFC 5987 form are sent: the first is what old
// clients read, and the second is what handles a name that is not ASCII, which
// every attachment filename eventually is.
func contentDisposition(kind, filename string) string {
	ascii := asciiFilename(filename)
	return fmt.Sprintf("%s; filename=\"%s\"; filename*=UTF-8''%s",
		kind, ascii, url.PathEscape(filename))
}

// asciiFilename reduces a filename to characters a header may carry unencoded.
func asciiFilename(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.' || r == '-' || r == '_' || r == ' ':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" || out == "." || out == ".." {
		return "attachment"
	}
	return out
}

// nowUTC is the clock, so tests can reason about stored dates.
func nowUTC() time.Time { return time.Now().UTC() }
