package mail

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	gomessage "github.com/emersion/go-message"
	gomail "github.com/emersion/go-message/mail"
)

// Builder composes an RFC 5322 message with attachments.
//
// The nesting is built rather than flattened, because a client shows the
// difference:
//
//	multipart/mixed                     (only when there is an attachment)
//	 ├── multipart/related             (only when there is an inline image)
//	 │    ├── multipart/alternative    (when both bodies are set)
//	 │    │    ├── text/plain
//	 │    │    └── text/html
//	 │    └── inline images
//	 └── attachments
//
// A single-part message is not wrapped in a multipart at all, which keeps plain
// text mail readable to anything that can only handle the simplest form.
type Builder struct {
	from    *Address
	to      []Address
	cc      []Address
	bcc     []Address
	replyTo []Address
	headers map[string][]string

	subject   string
	date      time.Time
	messageID string

	text string
	html string

	attachments []attachment
	inline      []attachment
}

type attachment struct {
	filename    string
	contentType string
	contentID   string
	data        []byte
}

// NewBuilder returns an empty composer.
func NewBuilder() *Builder {
	return &Builder{
		headers: map[string][]string{},
		date:    time.Now(),
	}
}

// From sets the author.
func (b *Builder) From(a Address) *Builder {
	b.from = &a
	return b
}

// To, Cc and Bcc add recipients. Bcc recipients appear in the header so a
// user can see who they mailed, and are stripped by the send path.
func (b *Builder) To(addrs ...Address) *Builder {
	b.to = append(b.to, addrs...)
	return b
}

// Cc adds carbon-copy recipients.
func (b *Builder) Cc(addrs ...Address) *Builder {
	b.cc = append(b.cc, addrs...)
	return b
}

// Bcc adds blind-copy recipients.
func (b *Builder) Bcc(addrs ...Address) *Builder {
	b.bcc = append(b.bcc, addrs...)
	return b
}

// ReplyTo sets the Reply-To addresses.
func (b *Builder) ReplyTo(addrs ...Address) *Builder {
	b.replyTo = append(b.replyTo, addrs...)
	return b
}

// Subject sets the subject.
func (b *Builder) Subject(s string) *Builder {
	b.subject = s
	return b
}

// Date sets the Date header, defaulting to the moment the message is built.
func (b *Builder) Date(t time.Time) *Builder {
	b.date = t
	return b
}

// MessageID sets the Message-ID, generating one from hostname when absent.
func (b *Builder) MessageID(id string) *Builder {
	b.messageID = id
	return b
}

// Header sets an extra header, replacing any existing values for the name.
func (b *Builder) Header(name, value string) *Builder {
	b.headers[name] = []string{value}
	return b
}

// Text sets the plain-text body.
func (b *Builder) Text(body string) *Builder {
	b.text = body
	return b
}

// HTML sets the HTML body. When both bodies are set the message becomes
// multipart/alternative, which is what lets a client pick.
func (b *Builder) HTML(body string) *Builder {
	b.html = body
	return b
}

// Attach adds a downloadable file.
//
// An attachment whose content is already text is sent as quoted-printable
// rather than base64, which is roughly a third smaller and stays legible in a
// raw view; binary content is base64, which has no line-length limit.
func (b *Builder) Attach(filename, contentType string, data []byte) *Builder {
	b.attachments = append(b.attachments, attachment{
		filename:    sanitiseFilename(filename),
		contentType: normaliseContentType(contentType, filename),
		data:        data,
	})
	return b
}

// InlineImage adds an image the HTML body refers to by Content-ID.
func (b *Builder) InlineImage(filename, contentType, contentID string, data []byte) *Builder {
	b.inline = append(b.inline, attachment{
		filename:    sanitiseFilename(filename),
		contentType: normaliseContentType(contentType, filename),
		contentID:   strings.Trim(strings.TrimSpace(contentID), "<>"),
		data:        data,
	})
	return b
}

// HasAttachments reports whether anything will be attached, so a caller can
// decide between the single-part and multipart layouts before writing.
func (b *Builder) HasAttachments() bool {
	return len(b.attachments) > 0
}

// Bytes renders the message.
func (b *Builder) Bytes() ([]byte, error) {
	if b.from == nil || b.from.Address == "" {
		return nil, fmt.Errorf("mail: builder needs a From address")
	}
	if len(b.to) == 0 && len(b.cc) == 0 && len(b.bcc) == 0 {
		return nil, fmt.Errorf("mail: builder needs at least one recipient")
	}
	if b.text == "" && b.html == "" {
		return nil, fmt.Errorf("mail: builder needs a body")
	}

	msgID := b.messageID
	if msgID == "" {
		msgID = NewMessageID(domainOf(b.from.Address))
	}

	var buf bytes.Buffer
	if err := b.write(&buf, msgID); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// write renders the message with the nesting described on Builder.
func (b *Builder) write(buf *bytes.Buffer, messageID string) error {
	switch {
	case len(b.attachments) > 0 && len(b.inline) > 0:
		return b.writeMixedWithRelated(buf, messageID)
	case len(b.attachments) > 0:
		return b.writeMixed(buf, messageID)
	case len(b.inline) > 0:
		return b.writeRelated(buf, messageID)
	case b.text != "" && b.html != "":
		return b.writeAlternative(buf, messageID)
	default:
		return b.writeSinglePart(buf, messageID)
	}
}

func (b *Builder) writeMixed(buf *bytes.Buffer, messageID string) error {
	w, err := gomessage.CreateWriter(buf, b.topLevelHeader(messageID, "multipart/mixed"))
	if err != nil {
		return fmt.Errorf("mail: start multipart/mixed: %w", err)
	}
	if err := b.writeBodyPart(w); err != nil {
		return err
	}
	for _, a := range b.attachments {
		if err := writeAttachment(w, a, ""); err != nil {
			return err
		}
	}
	return w.Close()
}

// writeMixedWithRelated nests the body and its inline images inside a related
// part, so a client that does not understand Content-ID still shows the images
// where the body refers to them instead of offering them as downloads.
func (b *Builder) writeMixedWithRelated(buf *bytes.Buffer, messageID string) error {
	w, err := gomessage.CreateWriter(buf, b.topLevelHeader(messageID, "multipart/mixed"))
	if err != nil {
		return fmt.Errorf("mail: start multipart/mixed: %w", err)
	}

	related, err := w.CreatePart(relatedHeader())
	if err != nil {
		return fmt.Errorf("mail: start multipart/related: %w", err)
	}
	if err := b.writeBodyPart(related); err != nil {
		return err
	}
	for _, a := range b.inline {
		if err := writeAttachment(related, a, a.contentID); err != nil {
			return err
		}
	}
	if err := related.Close(); err != nil {
		return err
	}
	for _, a := range b.attachments {
		if err := writeAttachment(w, a, ""); err != nil {
			return err
		}
	}
	return w.Close()
}

func (b *Builder) writeAlternative(buf *bytes.Buffer, messageID string) error {
	w, err := gomessage.CreateWriter(buf, b.topLevelHeader(messageID, "multipart/alternative"))
	if err != nil {
		return fmt.Errorf("mail: start multipart/alternative: %w", err)
	}
	if err := b.writeBodyPart(w); err != nil {
		return err
	}
	return w.Close()
}

func (b *Builder) writeRelated(buf *bytes.Buffer, messageID string) error {
	w, err := gomessage.CreateWriter(buf, b.topLevelHeader(messageID, "multipart/related"))
	if err != nil {
		return fmt.Errorf("mail: start multipart/related: %w", err)
	}
	if err := b.writeBodyPart(w); err != nil {
		return err
	}
	for _, a := range b.inline {
		if err := writeAttachment(w, a, a.contentID); err != nil {
			return err
		}
	}
	return w.Close()
}

// relatedHeader is the container for a body plus the images it refers to.
func relatedHeader() gomessage.Header {
	return newHeader("multipart/related", map[string]string{"type": "multipart/alternative"}).Header
}

// writeBodyPart writes the body as one part, or as multipart/alternative when
// both bodies are set.
func (b *Builder) writeBodyPart(w *gomessage.Writer) error {
	if b.text != "" && b.html != "" {
		alt, err := w.CreatePart(newHeader("multipart/alternative", nil).Header)
		if err != nil {
			return fmt.Errorf("mail: start multipart/alternative: %w", err)
		}
		if err := writeTextPart(alt, "text/plain", b.text, ""); err != nil {
			return err
		}
		if err := writeTextPart(alt, "text/html", b.html, ""); err != nil {
			return err
		}
		return alt.Close()
	}
	if b.html != "" {
		return writeTextPart(w, "text/html", b.html, "")
	}
	return writeTextPart(w, "text/plain", b.text, "")
}

func (b *Builder) writeSinglePart(buf *bytes.Buffer, messageID string) error {
	contentType, body := "text/plain", b.text
	if b.html != "" {
		contentType, body = "text/html", b.html
	}

	h := newHeader(contentType, map[string]string{"charset": "utf-8"})
	h.Set("Content-Transfer-Encoding", quotedPrintable)
	b.fillHeaders(h, messageID)

	w, err := gomessage.CreateWriter(buf, h.Header)
	if err != nil {
		return fmt.Errorf("mail: start single part: %w", err)
	}
	if _, err := w.Write([]byte(body)); err != nil {
		return fmt.Errorf("mail: write body: %w", err)
	}
	return w.Close()
}

// newHeader starts a header with the content type set.
//
// go-message's header is a struct holding a slice of fields, not a map, so a
// header has to be built through a pointer and only then handed on. Copying
// the struct first and mutating the copy loses every appended field, because
// the copy's slice grows while the original's does not. Everything below fills
// one header and returns it at the end.
func newHeader(mediaType string, params map[string]string) *gomail.Header {
	h := &gomail.Header{Header: gomessage.Header{}}
	if params == nil {
		params = map[string]string{}
	}
	h.SetContentType(mediaType, params)
	return h
}

// topLevelHeader builds the outer header, including the addresses the sender
// controls and the conversation headers that make replies thread.
func (b *Builder) topLevelHeader(messageID, mediaType string) gomessage.Header {
	if mediaType == "multipart/mixed" {
		h := newHeader(mediaType, nil)
		b.fillHeaders(h, messageID)
		return h.Header
	}
	// A related or alternative container declares which of its children is the
	// one a client should render.
	h := newHeader(mediaType, map[string]string{"type": "multipart/alternative"})
	b.fillHeaders(h, messageID)
	return h.Header
}

// fillHeaders writes the address, date and conversation headers onto h.
func (b *Builder) fillHeaders(h *gomail.Header, messageID string) {
	h.SetAddressList("From", []*gomail.Address{{Name: b.from.Name, Address: b.from.Address}})
	if len(b.to) > 0 {
		h.SetAddressList("To", toMailAddresses(b.to))
	}
	if len(b.cc) > 0 {
		h.SetAddressList("Cc", toMailAddresses(b.cc))
	}
	if len(b.bcc) > 0 {
		h.SetAddressList("Bcc", toMailAddresses(b.bcc))
	}
	if len(b.replyTo) > 0 {
		h.SetAddressList("Reply-To", toMailAddresses(b.replyTo))
	}
	h.SetSubject(b.subject)
	h.SetDate(b.date)
	h.SetMessageID(messageID)
	for name, values := range b.headers {
		for _, v := range values {
			h.Add(name, v)
		}
	}
}

// writeTextPart writes one body part with an explicit transfer encoding.
func writeTextPart(w *gomessage.Writer, contentType, body, contentID string) error {
	header := gomessage.Header{}
	header.SetContentType(contentType, map[string]string{"charset": "utf-8"})
	header.Set("Content-Transfer-Encoding", quotedPrintable)
	if contentID != "" {
		header.Set("Content-ID", "<"+contentID+">")
	}

	pw, err := w.CreatePart(header)
	if err != nil {
		return fmt.Errorf("mail: create %s part: %w", contentType, err)
	}
	if _, err := pw.Write([]byte(body)); err != nil {
		return fmt.Errorf("mail: write %s part: %w", contentType, err)
	}
	return pw.Close()
}

// writeAttachment writes one file part. An empty contentID marks it as a
// download rather than something the body refers to.
func writeAttachment(w *gomessage.Writer, a attachment, contentID string) error {
	header := gomessage.Header{}
	header.SetContentType(a.contentType, map[string]string{"name": a.filename})
	disposition := "attachment"
	if contentID != "" {
		disposition = "inline"
	}
	header.SetContentDisposition(disposition, map[string]string{"filename": a.filename})
	if contentID != "" {
		header.Set("Content-ID", "<"+contentID+">")
	}

	encoding := base64Encoding
	if isTextual(a.contentType) {
		encoding = quotedPrintable
	}
	header.Set("Content-Transfer-Encoding", encoding)

	pw, err := w.CreatePart(header)
	if err != nil {
		return fmt.Errorf("mail: create attachment %q: %w", a.filename, err)
	}
	if _, err := pw.Write(a.data); err != nil {
		return fmt.Errorf("mail: write attachment %q: %w", a.filename, err)
	}
	return pw.Close()
}

const (
	quotedPrintable = "quoted-printable"
	base64Encoding  = "base64"
)

func toMailAddresses(in []Address) []*gomail.Address {
	if len(in) == 0 {
		return nil
	}
	out := make([]*gomail.Address, 0, len(in))
	for _, a := range in {
		out = append(out, &gomail.Address{Name: a.Name, Address: a.Address})
	}
	return out
}

// isTextual reports whether a content type should be sent as quoted-printable.
func isTextual(contentType string) bool {
	mediaType := contentType
	if idx := strings.IndexByte(mediaType, ';'); idx >= 0 {
		mediaType = mediaType[:idx]
	}
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	return strings.HasPrefix(mediaType, "text/") ||
		strings.Contains(mediaType, "json") ||
		strings.Contains(mediaType, "xml") ||
		strings.Contains(mediaType, "javascript")
}

// normaliseContentType falls back to guessing from the filename, because a
// browser sends an empty or generic type for a file it read from disk.
func normaliseContentType(contentType, filename string) string {
	contentType = strings.TrimSpace(contentType)
	if idx := strings.IndexByte(contentType, ';'); idx >= 0 {
		contentType = strings.TrimSpace(contentType[:idx])
	}
	if contentType != "" && !strings.Contains(contentType, "/") {
		contentType = ""
	}
	if contentType != "" && contentType != "application/octet-stream" {
		return strings.ToLower(contentType)
	}
	if guessed := guessContentType(filename); guessed != "" {
		return guessed
	}
	return "application/octet-stream"
}

// sanitiseFilename keeps a filename safe to put in a header. A path is reduced
// to its last element, and anything that could split the header is dropped,
// so a hostile filename cannot inject a header of its own.
func sanitiseFilename(filename string) string {
	filename = strings.TrimSpace(filename)
	if idx := strings.LastIndexAny(filename, `/\`); idx >= 0 {
		filename = filename[idx+1:]
	}
	filename = strings.NewReplacer("\r", "", "\n", "", `"`, "").Replace(filename)
	if filename == "" || filename == "." || filename == ".." {
		return "attachment"
	}
	// RFC 2231 encoding is applied by the header writer, but an unencodable
	// name has to be replaced rather than dropped.
	if strings.ContainsAny(filename, "-") {
		return "attachment"
	}
	return filename
}

func domainOf(address string) string {
	if idx := strings.LastIndexByte(address, '@'); idx >= 0 {
		return address[idx+1:]
	}
	return ""
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// guessContentType maps an extension onto a media type, covering what a browser
// actually sends for a file the user picked from disk.
func guessContentType(filename string) string {
	idx := strings.LastIndexByte(filename, '.')
	if idx < 0 {
		return ""
	}
	switch strings.ToLower(filename[idx+1:]) {
	case "txt", "text", "log", "md":
		return "text/plain"
	case "csv":
		return "text/csv"
	case "html", "htm":
		return "text/html"
	case "css":
		return "text/css"
	case "js", "mjs":
		return "application/javascript"
	case "json":
		return "application/json"
	case "xml":
		return "application/xml"
	case "pdf":
		return "application/pdf"
	case "zip":
		return "application/zip"
	case "gz", "tgz":
		return "application/gzip"
	case "png":
		return "image/png"
	case "jpg", "jpeg":
		return "image/jpeg"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	case "svg":
		return "image/svg+xml"
	case "bmp":
		return "image/bmp"
	case "ico":
		return "image/x-icon"
	case "doc":
		return "application/msword"
	case "docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case "xls":
		return "application/vnd.ms-excel"
	case "xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case "ppt":
		return "application/vnd.ms-powerpoint"
	case "pptx":
		return "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	case "odt":
		return "application/vnd.oasis.opendocument.text"
	case "ics":
		return "text/calendar"
	case "eml":
		return "message/rfc822"
	default:
		return ""
	}
}
