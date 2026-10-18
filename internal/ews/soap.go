package ews

import (
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// Namespaces. Struct tags have to spell them out in full: "m:Foo" in a tag names a
// namespace "m", which no envelope declares, so nothing ever matched.
const (
	soapEnvelopeNS = "http://schemas.xmlsoap.org/soap/envelope/"
	messagesNS     = "http://schemas.microsoft.com/exchange/services/2006/messages"
	typesNS        = "http://schemas.microsoft.com/exchange/services/2006/types"
	errorsNS       = "http://schemas.microsoft.com/exchange/services/2006/errors"
)

// The ServerVersion this service reports.
//
// A client that sees no version assumes the oldest one it knows, which is the
// one whose rules are least likely to suit this server: it would then refuse to
// use a folder property that is perfectly ordinary here. The version is reported
// in the SOAP header (MS-OXECWS §2.2.2.2) and in the envelope of every response.
const (
	serverVersionMajor = "15"
	serverVersionMinor = "1"
)

// Response codes from MS-OXERRORS, echoed as m:ResponseCode inside the
// ResponseMessage that carries the real answer.
//
// The distinction between these is what a client acts on. "ErrorItemNotFound"
// makes it drop the item from its cache; "ErrorAccessDenied" makes it ask for
// credentials; and anything else makes it report the mailbox as broken. So the
// code has to name the situation rather than the operation that hit it.
const (
	codeNoError                             = "NoError"
	codeErrorInvalidRequest                 = "ErrorInvalidRequest"
	codeErrorInvalidServerVersion           = "ErrorInvalidServerVersion"
	codeErrorSchemaValidation               = "ErrorSchemaValidation"
	codeInvalidIdMalformed                  = "ErrorInvalidIdMalformed"
	codeErrorItemNotFound                   = "ErrorItemNotFound"
	codeErrorFolderNotFound                 = "ErrorFolderNotFound"
	codeErrorAccessDenied                   = "ErrorAccessDenied"
	codeErrorAccessIsDenied                 = "ErrorAccessIsDenied"
	codeErrorQuotaExceeded                  = "ErrorQuotaExceeded"
	codeErrorNotAllowedInFolder             = "ErrorNotAllowedInFolder"
	codeErrorCannotDeleteFolder             = "ErrorCannotDeleteFolder"
	codeErrorFolderIsEmpty                  = "ErrorFolderIsEmpty"
	codeErrorIrresolvableConflict           = "ErrorIrresolvableConflict"
	codeErrorChangeKeyRequiredForSend       = "ErrorChangeKeyRequiredForSend"
	codeErrorInvalidIdMalformedOrUnreadable = "ErrorInvalidIdMalformedOrUnreadable"
	codeErrorNameResolutionMultipleMatches  = "ErrorNameResolutionMultipleMatches"
	codeErrorNameResolutionNoResults        = "ErrorNameResolutionNoResults"
	codeErrorNonExistentMailbox             = "ErrorNonExistentMailbox"
	codeErrorInternalServerError            = "ErrorInternalServerError"
	codeErrorInvalidRequestMessage          = "ErrorInvalidRequestMessage"
	codeErrorMessageNotFound                = "ErrorMessageNotFound"
)

// SOAPEnvelope is the parsed request envelope.
//
// Only the two fields this service acts on are modelled: the header, which
// carries the version the client believes it is talking to, and the body, whose
// first element names the operation. Everything else is preserved as raw XML so
// that each operation can decode the shape it needs without this type having to
// know every argument of every operation.
//
// Header and Body are matched by local name rather than by namespace because
// MS-OXECWS §2.2.1 permits both SOAP 1.1 and SOAP 1.2 envelopes: Outlook 2021
// sends the 1.2 namespace for some operations, and a parser keyed to the 1.1
// namespace alone reads that envelope as having no body at all.
type SOAPEnvelope struct {
	XMLName   xml.Name    `xml:"Envelope"`
	XmlnsSoap string      `xml:"xmlns:soap,attr"`
	XmlnsT    string      `xml:"xmlns:t,attr"`
	XmlnsM    string      `xml:"xmlns:m,attr"`
	Header    *SOAPHeader `xml:"Header,omitempty"`
	Body      *SOAPBody   `xml:"Body"`
}

type SOAPHeader struct {
	RequestServerVersion      *RequestServerVersion `xml:"http://schemas.microsoft.com/exchange/services/2006/types RequestServerVersion,omitempty"`
	SerializedSecurityContext *string               `xml:"http://schemas.microsoft.com/exchange/services/2006/types SerializedSecurityContext,omitempty"`
}

type RequestServerVersion struct {
	Version string `xml:"Version,attr"`
}

type SOAPBody struct {
	// Inner is the raw XML of the body's children, which is where the operation
	// and its arguments live.
	Inner string `xml:",innerxml"`
}

// Operation returns the name of the requested operation: the local name of the
// first element inside s:Body. That element is the operation in MS-OXECWS -
// FindItem, GetItem, CreateItem and the rest - and it is what dispatch keys on
// when the SOAPAction header is absent.
func (b *SOAPBody) Operation() string {
	if b == nil {
		return ""
	}
	return firstElementName(b.Inner)
}

// firstElementName returns the local name of the first element in a fragment.
func firstElementName(fragment string) string {
	dec := xml.NewDecoder(strings.NewReader(fragment))
	for {
		tok, err := dec.Token()
		if err != nil {
			// io.EOF here just means the fragment held nothing but whitespace.
			return ""
		}
		if start, ok := tok.(xml.StartElement); ok {
			return start.Name.Local
		}
	}
}

// findAttribute reads an attribute of an element, whichever prefix it carries.
//
// A client is free to declare m: or t: or neither for an attribute that has no
// namespace of its own, and MS-OXECWS clients do both, so an attribute is found
// by local name and the prefix is not part of the answer.
func findAttribute(attrs []xml.Attr, name string) string {
	for _, a := range attrs {
		if a.Name.Local == name && a.Name.Space != xmlnsPrefix {
			return a.Value
		}
	}
	return ""
}

// xmlnsPrefix is the namespace encoding/xml gives to an xmlns declaration.
const xmlnsPrefix = "xmlns"

// responseCode is a successful response: the name of the response element and
// the XML for its contents.
//
// The name is part of the transport rather than of the operation. A client looks
// for <m:FindItemResponse> and nothing else, so an operation that got it right in
// the body and wrong in the element name has failed.
type responseCode struct {
	element string
	body    string
}

// ok is the empty successful response, for an operation with nothing to say.
func ok(element string) responseCode {
	return responseCode{element: element, body: `<m:ResponseCode>` + codeNoError + `</m:ResponseCode>`}
}

// soapError is a failure that belongs inside a SOAP envelope.
//
// MS-OXECWS puts almost every failure in the body rather than in an HTTP status:
// a 200 carrying ErrorItemNotFound is how a client learns one item of a batch is
// missing while the rest succeeded. The status is still carried, because a client
// that never parses the body needs something to go on, and because an
// unauthenticated request must not be answered 200.
type soapError struct {
	// code is the m:ResponseCode the client acts on.
	code string
	// text is the human-readable explanation, in the m:MessageText that
	// MS-OXERRORS defines for exactly this purpose.
	text string
	// status is the HTTP status to answer with.
	status int
	// element is the response element, which for a failure at the transport is
	// still the operation's own.
	element string
}

func (e *soapError) Error() string { return e.code + ": " + e.text }

// errorf builds a soapError.
func errorf(element, code string, status int, format string, args ...any) *soapError {
	return &soapError{
		code:    code,
		text:    fmt.Sprintf(format, args...),
		status:  status,
		element: element,
	}
}

// The shapes a request can fail in before an operation has read a single
// argument. They are collected here rather than written out at each use so that
// every refusal for the same reason carries the same code, which is what lets a
// client tell "you sent me nonsense" from "I could not do that".
var (
	errMissingOperation = errorf("", codeErrorInvalidRequest, 400, "the request carried no operation")
	errUnreadableBody   = errorf("", codeErrorInvalidRequest, 400, "the request body could not be read: %v", errRead)
	errRead             = fmt.Errorf("the SOAP envelope is malformed")
)

// readBody reads a request body with a limit.
//
// An EWS request carries a message being composed, so the bound is generous, but
// it is a bound: without one, a client that announces a gigabyte gets to send a
// gigabyte at a server that has already allocated the response.
func readBody(r io.Reader, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, limit))
}

// ---------------------------------------------------------------------------
// Writing XML
// ---------------------------------------------------------------------------

// attr is one attribute of an element being written.
type attr struct {
	name  string
	value string
}

// attrs builds an attribute list from alternating names and values.
//
// It is a function rather than a variadic of pairs because a call site reads
// better with it, and because a stray odd argument is a compile error here rather
// than a panic: pairs are checked at build time, not at render time.
func attrs(pairs ...string) []attr {
	if len(pairs)%2 != 0 {
		// A malformed attribute list would otherwise render as a shift, which
		// is very hard to see in the output.
		panic("ews: an odd number of attribute names and values")
	}
	out := make([]attr, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, attr{name: pairs[i], value: pairs[i+1]})
	}
	return out
}

// builder writes the XML of a response.
//
// The EWS response vocabulary is wide and shallow: an element, some attributes
// and text, or a list of children. Spelling each shape out as a Go struct would
// mean one type per element and no way to answer a subset -- and answering
// exactly the properties the client asked for is most of what an operation does,
// since a client sends a shape rather than asking for "a message".
//
// Nothing here escapes on its own account: text goes through escape() on the way
// in, because a subject line is caller data and a response that reflected it
// unescaped would be a reflected-XSS bug in every client that renders it.
type builder struct {
	buf   strings.Builder
	depth int
	// indent is off by default: whitespace inside an element is character data
	// to a parser reading mixed content, and EWS has none, but indenting a leaf
	// with text would change that text.
	indent bool
}

// newBuilder returns a builder writing indented XML.
func newBuilder() *builder { return &builder{indent: true} }

func (b *builder) pad() {
	if !b.indent {
		return
	}
	b.buf.WriteString(strings.Repeat("  ", b.depth))
}

func (b *builder) writeAttrs(a []attr) {
	for _, at := range a {
		b.buf.WriteByte(' ')
		b.buf.WriteString(at.name)
		b.buf.WriteString(`="`)
		b.buf.WriteString(escape(at.value))
		b.buf.WriteByte('"')
	}
}

// open writes a start tag.
func (b *builder) open(name string, a ...attr) {
	b.pad()
	b.buf.WriteByte('<')
	b.buf.WriteString(name)
	b.writeAttrs(a)
	b.buf.WriteString(">\n")
	b.depth++
}

// close writes an end tag. A mismatched name is rendered as written rather than
// corrected: the alternative is a document that parses and is wrong, which is
// harder to notice than one that does not parse.
func (b *builder) close(name string) {
	if b.depth > 0 {
		b.depth--
	}
	b.pad()
	b.buf.WriteString("</")
	b.buf.WriteString(name)
	b.buf.WriteString(">\n")
}

// leaf writes an element holding text.
func (b *builder) leaf(name, text string, a ...attr) {
	b.pad()
	b.buf.WriteByte('<')
	b.buf.WriteString(name)
	b.writeAttrs(a)
	if text == "" {
		// An element that is written with a start and an end tag rather than
		// self-closed, because a self-closing element is not what the schema
		// calls an empty string and a strict client treats the difference as
		// an absent value.
		b.buf.WriteString("></")
		b.buf.WriteString(name)
		b.buf.WriteString(">\n")
		return
	}
	b.buf.WriteByte('>')
	b.buf.WriteString(escape(text))
	b.buf.WriteString("</")
	b.buf.WriteString(name)
	b.buf.WriteString(">\n")
}

// empty writes an element with no content and no end tag.
func (b *builder) empty(name string, a ...attr) {
	b.pad()
	b.buf.WriteByte('<')
	b.buf.WriteString(name)
	b.writeAttrs(a)
	b.buf.WriteString("/>\n")
}

// String returns the XML written so far.
func (b *builder) String() string { return b.buf.String() }

// escape makes a string safe for element content and for an attribute value.
//
// Both contexts need the same treatment here, so this is one function: an
// attribute value quoted with " needs the double quote escaped and a < escaped for
// the same reason it is in content, and the ampersand has to go first or the
// escapes written afterwards would themselves be escaped.
func escape(s string) string {
	var out strings.Builder
	if err := xml.EscapeText(&out, []byte(s)); err != nil {
		// EscapeText only fails if the writer fails, and strings.Builder does not.
		return ""
	}
	return out.String()
}

// envelope wraps a response body in the SOAP envelope MS-OXECWS §2.2.2 requires.
//
// The namespace declarations live on the Envelope element and the response
// element is in the messages namespace with the prefix m, which is the spelling
// every Exchange client expects: the client parses the response by looking for
// the element by namespace, but any code or log that reads these documents as
// text reads them with these prefixes.
func envelope(element, body string) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	b.WriteString(`<soap:Envelope xmlns:soap="` + soapEnvelopeNS + `"` +
		` xmlns:m="` + messagesNS + `"` +
		` xmlns:t="` + typesNS + `"` +
		` xmlns:a="` + errorsNS + `">` + "\n")
	b.WriteString("  <soap:Header>\n")
	b.WriteString(`    <t:ServerVersionInfo MajorVersion="` + serverVersionMajor +
		`" MinorVersion="` + serverVersionMinor +
		`" Version="V2_23" BuildNumber="2300.4" />` + "\n")
	b.WriteString("  </soap:Header>\n")
	b.WriteString("  <soap:Body>\n")
	b.WriteString(`    <m:` + element + ">" + body + `</m:` + element + ">" + "\n")
	b.WriteString("  </soap:Body>\n")
	b.WriteString("</soap:Envelope>\n")
	return []byte(b.String())
}

// responseMessages wraps one operation's answer in the ResponseMessages element
// every EWS response carries.
//
// Each operation has its own response message element - FindItemResponseMessage
// for FindItem - because a client dispatches on it, and a generic one would be a
// response to no operation in particular.
func responseMessages(messageElement, body string) string {
	return `<m:ResponseMessages><m:` + messageElement + ` ResponseClass="Success">` +
		`<m:ResponseCode>` + codeNoError + `</m:ResponseCode>` + body +
		`</m:` + messageElement + `></m:ResponseMessages>`
}

// responseMessageOne wraps an answer that is not a list, in the response message
// element the operation's own name gives.
func responseMessageOne(operation, body string) string {
	return responseMessages(operation+"ResponseMessage", body)
}

// responseMessagesErrors wraps a batch answer whose failures are reported as
// additional response messages beside the successful one.
//
// A batch that is partly successful is answered in full: the items that resolved
// ride in the success message and each failure gets its own error message, which
// is how the schema carries "three of the four you asked for, and here is why the
// fourth is not there".
func responseMessagesErrors(messageElement, body, failures string) string {
	return `<m:ResponseMessages><m:` + messageElement + ` ResponseClass="Success">` +
		`<m:ResponseCode>` + codeNoError + `</m:ResponseCode>` + body +
		`</m:` + messageElement + `>` + failures + `</m:ResponseMessages>`
}

// fault renders a failure.
//
// The status is 500 for a fault the caller caused and 200 for one the protocol
// expects to be reported in the body: an unknown item in a batch is a normal
// answer to a FindItem, and a client that is told 500 for it retries the whole
// batch forever.
func fault(e *soapError) []byte {
	status := e.status
	if status == 0 {
		status = 500
	}
	return faultWithStatus(e, status)
}

func faultWithStatus(e *soapError, status int) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	b.WriteString(`<soap:Envelope xmlns:soap="` + soapEnvelopeNS +
		`" xmlns:a="` + errorsNS + `">` + "\n")
	b.WriteString("  <soap:Body>\n")
	b.WriteString("    <soap:Fault>\n")
	b.WriteString("      <faultcode>soap:Server</faultcode>\n")
	b.WriteString(`      <faultstring>` + escape(e.text) + "</faultstring>\n")
	b.WriteString("      <detail>\n")
	b.WriteString(`        <a:ResponseCode>` + escape(e.code) + "</a:ResponseCode>\n")
	b.WriteString(`        <a:Message>` + escape(e.text) + "</a:Message>\n")
	b.WriteString("      </detail>\n")
	b.WriteString("    </soap:Fault>\n")
	b.WriteString("  </soap:Body>\n")
	b.WriteString("</soap:Envelope>\n")
	return []byte(b.String())
}
