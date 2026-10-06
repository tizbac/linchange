package ews

import (
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/linchange/linchange/internal/webmail"
)

// owner is the account the request acts as.
//
// It comes from the authenticated principal rather than from anywhere in the
// request. A handler that took an owner from a query parameter or a folder name
// would be one missing check away from serving one person's mail to another, and
// there is no field in an EWS request that could legitimately name one: the
// protocol has exactly one account per connection.
func (c *call) owner() string { return c.who.Address }

// responseElement is the response element an operation answers with. It is the
// operation's own name with "Response" appended, which is what every EWS response
// element is, and it is a function rather than a string so that no operation can
// answer with the wrong element by accident.
func (c *call) responseElement() string { return c.name + "Response" }

// okOne renders the empty successful response, for an operation with nothing to
// return beyond the fact that it worked.
func (c *call) okOne(operation string) responseCode {
	return responseCode{
		element: operation + "Response",
		body:    responseMessageOne(operation, ""),
	}
}

// okList renders the empty successful response for an operation that returns a
// collection.
//
// The list is present and empty rather than absent, because a client that finds
// the <t:Items> element missing concludes the request failed and retries it for
// ever, while an empty one is the ordinary answer to "nothing there".
func (c *call) okList(operation string) responseCode {
	return responseCode{
		element: operation + "Response",
		body: responseMessageOne(operation,
			`<m:RootFolder TotalItemsInView="0" IncludesLastItemInRange="true"><t:Items></t:Items></m:RootFolder>`),
	}
}

// faultResponse turns a failure into a response.
//
// The element is the operation's own response element when the operation named
// one, and the HTTP status comes from the failure. An operation that fails on
// purpose reports it inside the envelope with a 200: a batch that is half
// successful is not a failure of the request, and a client told 500 for it retries
// the whole batch, which is how one missing item becomes a loop of duplicate
// operations.
func (c *call) faultResponse(err *soapError) responseCode {
	if err.element == "" {
		err.element = c.responseElement()
	}
	return responseCode{element: err.element, body: faultResponseMessage(err)}
}

// faultResponseMessage renders a single failure inside a response message.
//
// This is the per-item form of a fault: <m:ResponseMessage ResponseClass="Error">
// with the response code inside it. It is what makes a batch usable, because one
// item's failure is reported next to the others rather than instead of them.
func faultResponseMessage(err *soapError) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<m:ResponseMessage ResponseClass="Error"><m:MessageText>`)
	b.WriteString(escape(err.text))
	b.WriteString(`</m:MessageText><m:ResponseCode>`)
	b.WriteString(escape(err.code))
	b.WriteString(`</m:ResponseCode><m:Items></m:Items></m:ResponseMessage>`)
	return b.String()
}

// responseMessageList renders a batch of per-item answers.
//
// Each item is answered separately and each carries its own response code, which
// is what lets a client retry the ones that failed and keep the ones that did not.
func responseMessageList(operation, items string) string {
	return responseMessages(operation+"ResponseMessage", items)
}

// internal turns a storage error into a fault, without leaking the error text.
//
// The message a client is shown is fixed. A database error string can name a
// table, a constraint or a file path, and MS-OXERRORS has a separate field for
// the diagnostic text that a client logs rather than displays -- but there is no
// reason to fill it in either, because a client that logs the server's schema is
// a client that publishes it.
func (e *EWS) internal(operation string, err error) *soapError {
	e.log.Error("ews: "+operation+" failed", "error", err)
	return &soapError{
		code:    codeErrorInternalServerError,
		text:    "the request could not be completed",
		status:  http.StatusInternalServerError,
		element: operation + "Response",
	}
}

// folderIDFault turns a malformed identifier into the fault a client acts on.
//
// The distinction matters more here than anywhere else. A client whose identifier
// is malformed has a bug or has cached an identifier from a server that no longer
// exists, and the correct response is to discard the cache and re-sync.
// ErrorInvalidIdMalformed says so. ErrorItemNotFound would make it delete one
// item and keep going, which leaves the client permanently convinced that a
// folder it can see is empty.
func folderIDFault(element string, err error) *soapError {
	return errorf(element, codeInvalidIdMalformed, http.StatusOK,
		"the identifier could not be read: %v", err)
}

// folderWriteFault maps a mail-service error onto the code for what went wrong.
//
// The distinction a client acts on is between "it does not exist" and "you may
// not touch that": the first makes it re-sync, the second makes it ask for
// different credentials. Both arrive here as the same Go error, and mapping them
// apart is the only reason the client can tell which happened.
func folderWriteFault(element string, err error) *soapError {
	switch {
	case errors.Is(err, webmail.ErrNotFound), isNotFoundText(err):
		return errorf(element, codeErrorFolderNotFound, http.StatusOK,
			"the folder does not exist")
	case errors.Is(err, webmail.ErrDenied):
		return errorf(element, codeErrorAccessDenied, http.StatusOK,
			"you do not have access to that folder")
	case errors.Is(err, webmail.ErrInvalid):
		return errorf(element, codeErrorInvalidRequest, http.StatusOK, "%v", err)
	case errors.Is(err, webmail.ErrQuota):
		return errorf(element, codeErrorQuotaExceeded, http.StatusOK,
			"the mailbox quota has been reached")
	}
	return &soapError{
		code:    codeErrorInternalServerError,
		text:    "the folder could not be read",
		status:  http.StatusInternalServerError,
		element: element,
	}
}

// itemFault maps an error from the mail service onto the code a client acts on
// for an item.
func itemFault(element string, err error) *soapError {
	switch {
	case errors.Is(err, webmail.ErrNotFound), isNotFoundText(err):
		return errorf(element, codeErrorItemNotFound, http.StatusOK,
			"the item does not exist")
	case errors.Is(err, webmail.ErrDenied):
		return errorf(element, codeErrorAccessDenied, http.StatusOK,
			"you do not have access to that item")
	case errors.Is(err, webmail.ErrQuota):
		return errorf(element, codeErrorQuotaExceeded, http.StatusOK,
			"the mailbox quota has been reached")
	case errors.Is(err, webmail.ErrInvalid):
		return errorf(element, codeErrorInvalidRequest, http.StatusOK, "%v", err)
	}
	return &soapError{
		code:    codeErrorInternalServerError,
		text:    "the item could not be read",
		status:  http.StatusInternalServerError,
		element: element,
	}
}

// isNotFoundText recognises a not-found answer that arrived as text.
//
// The mail service returns a wrapped sentinel, and every path through it does so
// today. This exists so that a path which builds its message by hand -- which is
// what translate() used to do -- is recognised anyway rather than reported as an
// internal error to the one client that can do something about it.
func isNotFoundText(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "not found")
}

// innerText returns the text of the first <name> element in a fragment.
//
// The property elements of a CreateItem or an UpdateItem are a bag whose keys the
// schema defines and whose shape varies with the client, so reading one is a
// search for an element by name rather than a decode into a struct.
func innerText(fragment, name string) string {
	text, _ := innerTextPresent(fragment, name)
	return text
}

// innerTextPresent returns the text of the first <name> element and whether one
// was there at all.
//
// The two are separate because an empty value is not the same as an absent
// element: a client that sends <t:DisplayName></t:DisplayName> is renaming a
// folder to the empty string, which is a rename this server refuses, and treating
// it as absent would silently keep the old name instead.
func innerTextPresent(fragment, name string) (string, bool) {
	// The fragment is walked recursively rather than sibling by sibling, because
	// the value is allowed to sit under a wrapper: a client sets a message's read
	// flag as <t:Message><t:IsRead>true</t:IsRead></t:Message>, and skipping the
	// wrapper -- as a flat scan would -- would make every such update a no-op.
	// The name is matched by local name at any depth, so the wrapper is entered
	// and the first element of the target name in document order is the answer.
	dec := xml.NewDecoder(strings.NewReader(fragment))
	return innerTextFrom(dec, name)
}

// innerTextFrom reads tokens until the target element or the end of the input.
//
// A mismatched element is entered rather than skipped: the value being sought
// may be nested inside it. An end element means the current container is over,
// and the search at this depth in the container found nothing.
func innerTextFrom(dec *xml.Decoder, name string) (string, bool) {
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", false
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == name {
				var text string
				if err := dec.DecodeElement(&text, &t); err != nil {
					// A malformed value is an absent value: the element is there
					// but cannot be read, and using the partial text would be a
					// guess.
					return "", false
				}
				return text, true
			}
			if text, ok := innerTextFrom(dec, name); ok {
				return text, true
			}
		case xml.EndElement:
			return "", false
		}
	}
}

// innerBool reads a boolean-valued property element.
//
// The comparison is on the parsed value rather than on the text, because the two
// spellings a client uses -- "true" and "1" -- have to mean the same thing and a
// string comparison would make "1" false.
func innerBool(fragment, name string) (bool, bool) {
	text, ok := innerTextPresent(fragment, name)
	if !ok {
		return false, false
	}
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "true", "1":
		return true, true
	case "false", "0":
		return false, true
	}
	return false, false
}

// addressList reads the mailboxes out of a recipient field.
//
// The recipient fields all have the same shape -- a run of <t:Mailbox> elements,
// each with a display name and an address -- but they are addressed by name
// (t:From, t:ToRecipients, t:CcRecipients, ...) and the caller has already read
// the one it wants. Scanning for the mailbox elements rather than decoding the
// field is what keeps one reader for all of them, and what makes the nesting
// harmless: a <t:Mailbox> inside a <t:Mailbox> produces two addresses, which is
// what the schema means by it.
//
// A mailbox with no address is dropped. Clients send an empty one when a recipient
// row has been left half-filled, and it cannot be filed anywhere.
func addressList(fragment string) []ewsAddress {
	var out []ewsAddress
	dec := xml.NewDecoder(strings.NewReader(fragment))
	for {
		tok, err := dec.Token()
		if err != nil {
			return out
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if start.Name.Local != "Mailbox" {
			continue
		}
		var mb mailboxXML
		if err := dec.DecodeElement(&mb, &start); err != nil {
			// An unreadable mailbox is skipped rather than returned empty: an
			// empty address would become a recipient of "", which the delivery
			// path would report as a failure for the whole message.
			continue
		}
		addr := mb.address()
		if addr == "" {
			continue
		}
		out = append(out, ewsAddress{name: mb.Name, address: addr})
	}
}

// mailboxXML is a t:Mailbox as a request carries it.
type mailboxXML struct {
	Name           string       `xml:"Name"`
	EmailAddress   emailAddress `xml:"EmailAddress"`
	MailingAddress emailAddress `xml:"MailingAddress"`
}

// address returns the mailbox's address, whichever field it arrived in.
func (m mailboxXML) address() string {
	if a := m.EmailAddress.value; a != "" {
		return a
	}
	return m.MailingAddress.value
}

// emailAddress reads a t:EmailAddress in either of the shapes clients use: a
// nested <t:Address>, or the address as the element's own text.
//
// Both spellings appear in the wild and the difference is not one a client
// negotiates: some versions put an element inside and some put the address in the
// text, and both are legal in their respective schemas. A decoder that handled
// only one would drop recipients from a real client, and it would drop them
// silently -- the message would send to whoever it could read and not to the
// person whose address it could not.
type emailAddress struct {
	// value is the address read out of the element.
	value string
}

// UnmarshalXML implements xml.Unmarshaler.
func (e *emailAddress) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	var text []byte
	for _, a := range start.Attr {
		if a.Name.Local == "Address" {
			e.value = a.Value
		}
	}
	for {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.CharData:
			text = append(text, t...)
		case xml.StartElement:
			switch t.Name.Local {
			case "Address", "SmtpAddress", "EmailAddress":
				var addr string
				if err := d.DecodeElement(&addr, &t); err != nil {
					return err
				}
				if e.value == "" {
					e.value = addr
				}
			default:
				if err := d.Skip(); err != nil {
					return err
				}
			}
		case xml.EndElement:
			if e.value == "" {
				e.value = strings.TrimSpace(string(text))
			}
			return nil
		}
	}
}

// ewsAddress is one mailbox from a request, in the shape a header needs.
type ewsAddress struct {
	name    string
	address string
}

// display renders the address as "Name <address>", which is what a client puts in
// a display field and what a header shows.
func (a ewsAddress) display() string {
	if a.name == "" {
		return a.address
	}
	return fmt.Sprintf("%s <%s>", a.name, a.address)
}

// toWebmail converts to the composer shape.
func (a ewsAddress) toWebmail() webmail.Address {
	return webmail.Address{Address: a.address, Name: a.name}
}

// toWebmailAll converts a list, dropping any entry with no address.
func toWebmailAll(in []ewsAddress) []webmail.Address {
	out := make([]webmail.Address, 0, len(in))
	for _, a := range in {
		if a.address == "" {
			continue
		}
		out = append(out, a.toWebmail())
	}
	return out
}

// displayList renders a list of addresses for a display field.
//
// The separator matters: clients split a display field on it to decide how many
// recipients to show before they collapse it to "and 3 more". A semicolon is what
// they expect from this protocol, and a name containing one does not confuse them
// because they split on the outside of the angle brackets.
func displayList(addrs []webmail.Address) string {
	parts := make([]string, 0, len(addrs))
	for _, a := range addrs {
		parts = append(parts, a.String())
	}
	return strings.Join(parts, "; ")
}
