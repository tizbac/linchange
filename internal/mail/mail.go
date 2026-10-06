// Package mail reads and writes RFC 5322 messages.
//
// The inbound listener, the IMAP server and the webmail reader all need the
// same answers about a message: who sent it, what its body says, which parts
// are attachments, and which conversation it belongs to. Deriving them once
// here is what keeps those surfaces agreeing about the same blob, instead of
// each one carrying its own header parser that drifts.
//
// Everything is derived from the raw bytes, so a message is only ever stored
// as written by its sender and every surface can re-derive what it needs.
package mail

import (
	"strings"
	"time"

	gomail "github.com/emersion/go-message/mail"
)

// Metadata is the denormalised summary of a message that the store keeps in
// columns. It exists so that a folder listing, an IMAP SEARCH and a thread
// grouping never have to read a message body.
//
// It is written once, on delivery, and read many times. Anything that can be
// derived later is left out on purpose.
type Metadata struct {
	Subject        string
	FromAddress    string
	FromName       string
	To             []Address
	Cc             []Address
	Bcc            []Address
	ReplyTo        []Address
	Date           time.Time
	MessageID      string
	References     []string
	ThreadKey      string
	ThreadSubject  string
	Snippet        string
	HasAttachments bool
	Size           int64
	// ItemClass is the MS-OXPROPS class an Exchange client filters and draws
	// this message by. See ItemClass.
	ItemClass string
}

// Describe reads raw and returns the metadata worth storing.
func Describe(raw []byte) (Metadata, error) {
	parsed, err := Parse(raw)
	if err != nil {
		return Metadata{}, err
	}
	return parsed.Metadata(), nil
}

// Metadata summarises an already-parsed message.
func (p *Parsed) Metadata() Metadata {
	h := &p.Header

	meta := Metadata{
		Size: int64(len(p.raw)),
	}

	if subj, err := h.Subject(); err == nil {
		meta.Subject = strings.TrimSpace(subj)
	}
	meta.FromAddress, meta.FromName = p.primarySender()

	if to, err := h.AddressList("To"); err == nil {
		meta.To = convertAddresses(to)
	}
	if cc, err := h.AddressList("Cc"); err == nil {
		meta.Cc = convertAddresses(cc)
	}
	if bcc, err := h.AddressList("Bcc"); err == nil {
		meta.Bcc = convertAddresses(bcc)
	}
	if rt, err := h.AddressList("Reply-To"); err == nil {
		meta.ReplyTo = convertAddresses(rt)
	}

	if d, err := h.Date(); err == nil && !d.IsZero() {
		meta.Date = d
	}
	if id, err := h.MessageID(); err == nil {
		meta.MessageID = id
	}
	meta.References = p.referenceIDs()

	meta.ThreadSubject = NormaliseSubject(meta.Subject)
	meta.ThreadKey = ThreadKey(meta.MessageID, meta.References, meta.Subject)
	meta.Snippet = p.Snippet(SnippetLength)
	meta.HasAttachments = len(p.Attachments) > 0
	meta.ItemClass = ItemClass(p)

	return meta
}

// Item class names (MS-OXPROPS §2.10.1). A message's class is what an Exchange
// client filters by and what it draws a message as: a meeting request gets a
// Respond button, a plain note does not.
const (
	ItemClassNote      = "IPM.Note"
	ItemClassMeeting   = "IPM.Schedule.Meeting.Request"
	ItemClassMeetingRe = "IPM.Schedule.Meeting.Response"
	ItemClassCancel    = "IPM.Schedule.Meeting.Cancellation"
	ItemClassContact   = "IPM.Contact"
	ItemClassSticky    = "IPM.Note.StickyNote"
	ItemClassDistList  = "IPM.DistList"
)

// ItemClass derives the MS-OXPROPS item class of a parsed message.
//
// The class cannot be derived from the headers alone, because the thing that
// distinguishes a meeting request from a note is the body: the same Subject and
// From with an iCalendar part inside is a meeting request. So it is read from the
// Content-Type of the part that carries the scheduling data, which is where iTIP
// puts it, and the Message-ID convention is used as the fallback for the messages
// a sender stripped it from.
func ItemClass(p *Parsed) string {
	switch strings.ToLower(strings.TrimSpace(p.Header.Get("Content-Type"))) {
	case `application/ics`, `text/calendar`:
		return ItemClassNote
	}
	// A multipart message carries the calendar part as one of its parts, which
	// is how a meeting request travels with a body alongside it.
	for _, part := range p.Parts {
		if method := calendarMethod(part.ContentType, string(part.Content)); method != "" {
			return itemClassForMethod(method)
		}
	}
	// The iTIP convention that predates the MIME registration: a meeting request
	// has "-REQUEST" appended to the Message-ID. A sender that filed the message
	// with a conforming client leaves a part behind; one that did not may leave
	// only this.
	if id := strings.ToUpper(p.Header.Get("Message-ID")); strings.HasSuffix(id, "-REQUEST") {
		return ItemClassMeeting
	}
	if strings.HasSuffix(strings.ToUpper(p.Header.Get("Message-ID")), "-REPLY") {
		return ItemClassMeetingRe
	}
	if strings.HasSuffix(strings.ToUpper(p.Header.Get("Message-ID")), "-CANCEL") {
		return ItemClassCancel
	}
	return ItemClassNote
}

// itemClassForMethod maps an iTIP METHOD onto its item class.
func itemClassForMethod(method string) string {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case "REQUEST":
		return ItemClassMeeting
	case "REPLY":
		return ItemClassMeetingRe
	case "CANCEL":
		return ItemClassCancel
	case "ADD", "REFRESH", "COUNTER", "DECLINECOUNTER", "PUBLISH":
		return ItemClassMeeting
	}
	return ItemClassNote
}

// calendarMethod reads the iTIP method out of a calendar part.
//
// The method is a property of the part's content, not a header, so it is read
// from the text: a part that declares itself text/calendar but carries no method
// is a forwardee's pasted appointment rather than a request, and treating it as a
// request would put a Respond button on mail that has nobody to respond to.
func calendarMethod(contentType, body string) string {
	if !strings.Contains(strings.ToLower(contentType), "calendar") {
		return ""
	}
	const marker = "METHOD:"
	upper := strings.ToUpper(body)
	idx := strings.Index(upper, marker)
	if idx < 0 {
		return ""
	}
	rest := body[idx+len(marker):]
	if nl := strings.IndexAny(rest, "\r\n"); nl >= 0 {
		rest = rest[:nl]
	}
	return strings.TrimSpace(rest)
}

// primarySender returns the From address and display name.
//
// A message can carry several From headers or an empty one; the first
// parsable mailbox wins, and a message with no From at all reports the empty
// string rather than failing, because delivery must not depend on it.
func (p *Parsed) primarySender() (address, name string) {
	if from, err := p.Header.AddressList("From"); err == nil {
		for _, a := range from {
			if a.Address != "" {
				return strings.ToLower(a.Address), DecodeText(a.Name)
			}
		}
	}
	return "", ""
}

// referenceIDs returns the message IDs this message references, in order,
// most recent last. In-Reply-To is folded in as the last reference so a
// message with only that header still joins its conversation.
func (p *Parsed) referenceIDs() []string {
	refs, err := p.Header.MsgIDList("References")
	if err != nil {
		refs = nil
	}
	if replyTo, err := p.Header.MsgIDList("In-Reply-To"); err == nil {
		refs = append(refs, replyTo...)
	}
	return normaliseIDs(refs)
}

// SnippetLength is how many characters of body text a snippet holds. It is
// long enough to identify a message in a list without storing a paragraph of
// every message in the database.
const SnippetLength = 200

// Recipients returns every address the message was addressed to, across To and
// Cc, without duplicates and in a stable order. A reply-all starts from this,
// minus the sender, which the caller decides.
func (p *Parsed) Recipients() []Address {
	var out []Address
	seen := make(map[string]bool)
	for _, key := range []string{"To", "Cc"} {
		list, err := p.Header.AddressList(key)
		if err != nil {
			continue
		}
		for _, a := range convertAddresses(list) {
			if a.Address == "" || seen[a.Address] {
				continue
			}
			seen[a.Address] = true
			out = append(out, a)
		}
	}
	return out
}

// References returns the message IDs this message references, oldest first.
func (p *Parsed) References() []string {
	return p.referenceIDs()
}

// Sender returns the From address and display name of the message.
func (p *Parsed) Sender() (address, name string) {
	return p.primarySender()
}

// Subject returns the decoded subject of the message.
func (p *Parsed) Subject() string {
	subject, err := p.Header.Subject()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(subject)
}

// convertAddresses lowers and decodes an address list into this package's type,
// so that callers never hold a go-message type they could compare loosely.
func convertAddresses(in []*gomail.Address) []Address {
	if len(in) == 0 {
		return nil
	}
	out := make([]Address, 0, len(in))
	for _, a := range in {
		if a == nil || a.Address == "" {
			continue
		}
		out = append(out, Address{
			Name:    DecodeText(a.Name),
			Address: strings.ToLower(a.Address),
		})
	}
	return out
}

// AddressList returns the decoded addresses of one header, or nil when the
// header is absent or unusable.
//
// It is exported because the reader and the composer both need the individual
// lists, not only the merged Recipients: a reply-all has to know who was in To
// and who in Cc to put them back where the sender put them, and a reader shows
// the lists under their own names.
func (p *Parsed) AddressList(header string) []Address {
	list, err := p.Header.AddressList(header)
	if err != nil {
		return nil
	}
	return convertAddresses(list)
}

// HeaderPair is one header field, for a details panel.
type HeaderPair struct {
	Name  string
	Value string
}

// HeaderList returns the message's headers in the order they appeared.
//
// It is not decoded: a details panel shows what was sent, and a header whose
// encoded words are unreadable is still evidence of what arrived. The values
// are bounded by the caller.
func (p *Parsed) HeaderList() []HeaderPair {
	var out []HeaderPair
	fields := p.Header.Fields()
	for more := fields.Next(); more; more = fields.Next() {
		out = append(out, HeaderPair{Name: fields.Key(), Value: fields.Value()})
	}
	return out
}
