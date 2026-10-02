package mail

import (
	"bytes"
	"fmt"
	"html"
	"io"
	"mime"
	"regexp"
	"strings"

	gomessage "github.com/emersion/go-message"
	gomail "github.com/emersion/go-message/mail"
)

// charsetReader converts a legacy charset into UTF-8. It is shared with
// go-message so that a subject and a body from the same non-UTF-8 message are
// decoded the same way.
func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	return gomessage.CharsetReader(charset, input)
}

// Part is one leaf of the MIME tree, with its content already decoded from any
// transfer encoding and charset conversion applied to text parts.
type Part struct {
	ContentType string
	Charset     string
	Filename    string
	ContentID   string
	Disposition string
	Content     []byte
}

// Size is the decoded length of the part.
func (p Part) Size() int64 { return int64(len(p.Content)) }

// IsInline reports whether the part is referenced from the HTML body by
// Content-ID rather than offered as a download.
func (p Part) IsInline() bool {
	return strings.EqualFold(p.Disposition, "inline") && p.ContentID != ""
}

// IsAttachment reports whether the part should be offered as a download.
//
// An inline image that the body refers to by Content-ID is not an attachment,
// otherwise every HTML mail with a logo would report one. Everything else with
// a filename, or anything explicitly marked as an attachment, is.
func (p Part) IsAttachment() bool {
	if p.IsInline() {
		return false
	}
	if p.Filename != "" {
		return true
	}
	return strings.EqualFold(p.Disposition, "attachment")
}

// Parsed is a message read into memory.
//
// Attachments are held as bytes rather than streamed because a webmail reader
// shows their names and sizes and downloads one at a time; the whole message
// is already resident, since it was just read out of the blob store.
type Parsed struct {
	raw    []byte
	Header gomail.Header

	// Parts is every leaf of the MIME tree in document order, including
	// inline images and the body parts.
	Parts []Part

	// Text and HTML are the body as a client would show it: the first
	// text/plain and text/html parts. A message may hold either, both or
	// neither, and a reader falls back accordingly.
	Text string
	HTML string

	// Attachments are the downloadable parts, in the order they appear.
	Attachments []Part

	// Inline are parts the body refers to by Content-ID.
	Inline []Part
}

// Parse reads an RFC 5322 message.
//
// A message with an unknown transfer encoding or charset is still parsed: the
// part keeps its bytes as sent rather than being dropped, which is what a
// client that cannot decode the encoding would see too.
func Parse(raw []byte) (*Parsed, error) {
	entity, err := gomessage.Read(bytes.NewReader(raw))
	if err != nil && !gomessage.IsUnknownEncoding(err) && !gomessage.IsUnknownCharset(err) {
		return nil, fmt.Errorf("mail: parse message: %w", err)
	}

	p := &Parsed{
		raw:    raw,
		Header: gomail.Header{Header: entity.Header},
	}

	walkErr := entity.Walk(func(_ []int, e *gomessage.Entity, partErr error) error {
		if partErr != nil {
			if gomessage.IsUnknownEncoding(partErr) || gomessage.IsUnknownCharset(partErr) {
				return nil
			}
			return partErr
		}
		// Walk visits the multipart containers as well as the leaves; only the
		// leaves hold content.
		if e == nil || e.MultipartReader() != nil {
			return nil
		}

		body, err := io.ReadAll(e.Body)
		if err != nil {
			return fmt.Errorf("mail: read part body: %w", err)
		}

		part := partFrom(e, body)
		p.Parts = append(p.Parts, part)

		switch {
		case part.IsAttachment():
			p.Attachments = append(p.Attachments, part)
		case part.IsInline():
			p.Inline = append(p.Inline, part)
		case strings.EqualFold(part.ContentType, "text/html"):
			if p.HTML == "" {
				p.HTML = string(part.Content)
			}
		case strings.EqualFold(part.ContentType, "text/plain"):
			if p.Text == "" {
				p.Text = string(part.Content)
			}
		}
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("mail: walk parts: %w", walkErr)
	}

	return p, nil
}

// partFrom describes one entity, reading its media type from the raw header so
// that RFC 2231 continuations (`filename*0*=`) are folded into a single name.
func partFrom(e *gomessage.Entity, body []byte) Part {
	part := Part{Content: body}

	mediaType, params := parseMediaType(e.Header.Get("Content-Type"))
	part.ContentType = mediaType
	part.Charset = params["charset"]
	part.Filename = params["name"]

	disposition, dispParams := parseMediaType(e.Header.Get("Content-Disposition"))
	part.Disposition = strings.ToLower(disposition)
	if name := dispParams["filename"]; name != "" {
		part.Filename = name
	}

	// The Content-ID used by the body is written without the angle brackets
	// that the header carries, so it is stored that way.
	part.ContentID = strings.Trim(strings.TrimSpace(e.Header.Get("Content-ID")), "<>")

	if part.ContentType == "" {
		part.ContentType = "text/plain"
	}
	return part
}

// parseMediaType splits a media type header into its value and parameters,
// tolerating the malformed parameter lists that appear in real mail.
func parseMediaType(header string) (string, map[string]string) {
	if strings.TrimSpace(header) == "" {
		return "", map[string]string{}
	}
	mediaType, params, err := mime.ParseMediaType(header)
	if err != nil {
		// Keep the value before the first semicolon: it is the part that
		// decides how the content is handled, and losing it would turn a
		// malformed multipart into an opaque blob.
		mediaType, _, _ = strings.Cut(header, ";")
	}
	return strings.ToLower(strings.TrimSpace(mediaType)), params
}

// Body returns the best body for a reader: HTML when the message has it,
// otherwise the text.
func (p *Parsed) Body() (html, text string) {
	if p.HTML != "" {
		return p.HTML, ""
	}
	return "", p.Text
}

// Snippet renders the first maxLen characters of the body, for a folder
// listing and for the full-text index.
//
// The snippet comes from the body rather than from the headers: an index built
// from the headers can only match subjects and names, so searching for a word
// someone wrote in their mail would find nothing.
func (p *Parsed) Snippet(maxLen int) string {
	source := collapseWhitespace(p.Text)
	if source == "" {
		source = collapseWhitespace(stripHTML(p.HTML))
	}
	if maxLen <= 0 {
		return source
	}
	runes := []rune(source)
	if len(runes) <= maxLen {
		return source
	}
	// Cut on a rune boundary, and trim the partial word the cut leaves behind.
	cut := strings.TrimSpace(string(runes[:maxLen]))
	if idx := strings.LastIndexAny(cut, " \t\n"); idx > 0 {
		cut = cut[:idx]
	}
	return cut + "…"
}

var (
	whitespaceRun = regexp.MustCompile(`\s+`)
	htmlTag       = regexp.MustCompile(`(?s)<(script|style)[^>]*>.*?</(script|style)>|<[^>]*>`)
	htmlBreak     = regexp.MustCompile(`(?i)<br\s*/?>|</(p|div|li|tr|h[1-6])>`)
)

// stripHTML turns an HTML body into readable text. It is only used for
// snippets, so it drops tags and resolves entities without trying to reproduce
// the document's structure.
func stripHTML(in string) string {
	if in == "" {
		return ""
	}
	text := htmlBreak.ReplaceAllString(in, " ")
	text = htmlTag.ReplaceAllString(text, " ")
	return html.UnescapeString(text)
}

// collapseWhitespace reduces every run of whitespace to a single space and
// trims the ends.
func collapseWhitespace(in string) string {
	return strings.TrimSpace(whitespaceRun.ReplaceAllString(in, " "))
}

// InlineByID finds a part the body refers to by Content-ID, which is how a
// cid: URL in an HTML body is resolved.
func (p *Parsed) InlineByID(contentID string) (Part, bool) {
	key := strings.Trim(strings.TrimSpace(contentID), "<>")
	if key == "" {
		return Part{}, false
	}
	for _, part := range p.Inline {
		if part.ContentID == key {
			return part, true
		}
	}
	return Part{}, false
}

// AttachmentByIndex finds an attachment by its position in the attachment
// list, which is what a download URL carries.
func (p *Parsed) AttachmentByIndex(i int) (Part, bool) {
	if i < 0 || i >= len(p.Attachments) {
		return Part{}, false
	}
	return p.Attachments[i], true
}
