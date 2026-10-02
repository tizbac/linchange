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

	return meta
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
