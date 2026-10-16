// Reading the message a SendMail carries.
//
// ActiveSync sends mail the way the Internet does: as one RFC 5322 document
// with MIME parts, in the Mime element of the command. That is a different
// shape from the field-by-field request the rest of the webmail service takes,
// so the document is read here and turned into one.
//
// The From header is deliberately not read. The account that authenticated is
// what the message is sent as, which is the rule every other submission path
// follows and the one that stops a client claiming to be somebody else.

package eas

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"

	"github.com/linchange/linchange/internal/webmail"
)

// errNoRecipient says the message was read but names nobody to send it to. It
// is kept apart from a parse failure because the two are told to a client
// differently: 119 (MessageHasNoRecipient) rather than 107 (InvalidMIME).
var errNoRecipient = errors.New("the message names no recipient")

// composeFromMIME turns a raw RFC 5322 message into a compose request.
func composeFromMIME(owner string, raw []byte) (webmail.ComposeRequest, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return webmail.ComposeRequest{}, fmt.Errorf("read message: %w", err)
	}

	req := webmail.ComposeRequest{From: owner}
	req.To = parseAddressList(msg.Header.Get("To"))
	req.Cc = parseAddressList(msg.Header.Get("Cc"))
	req.Bcc = parseAddressList(msg.Header.Get("Bcc"))
	req.Subject = msg.Header.Get("Subject")
	if req.To == nil && req.Cc == nil && req.Bcc == nil {
		return webmail.ComposeRequest{}, errNoRecipient
	}

	ctype := msg.Header.Get("Content-Type")
	if ctype == "" {
		ctype = "text/plain"
	}
	mediatype, params, err := mime.ParseMediaType(ctype)
	if err != nil {
		return webmail.ComposeRequest{}, fmt.Errorf("content type: %w", err)
	}
	if strings.EqualFold(mediatype, "multipart/signed") {
		// The signature part is not a body and not an attachment the composer
		// would re-send; the text parts before it are the message.
		mediatype, params, err = mime.ParseMediaType(
			"multipart/mixed; boundary=" + params["boundary"])
		if err != nil {
			return webmail.ComposeRequest{}, err
		}
	}
	if strings.HasPrefix(mediatype, "multipart/") {
		if params["boundary"] == "" {
			return webmail.ComposeRequest{}, fmt.Errorf("a multipart message with no boundary")
		}
		reader := multipart.NewReader(msg.Body, params["boundary"])
		if err := walkParts(reader, &req, 0); err != nil {
			return webmail.ComposeRequest{}, err
		}
	} else {
		data, err := decodeTransfer(msg.Body, msg.Header.Get("Content-Transfer-Encoding"))
		if err != nil {
			return webmail.ComposeRequest{}, fmt.Errorf("read body: %w", err)
		}
		if strings.EqualFold(mediatype, "text/html") {
			req.HTML = string(data)
		} else {
			req.Text = string(data)
		}
	}
	return req, nil
}

// walkParts reads one multipart level into the request: the text parts are the
// body and everything else is an attachment. The depth is bounded, because a
// message nesting multipart inside multipart forever is a message that would
// otherwise never finish.
func walkParts(reader *multipart.Reader, req *webmail.ComposeRequest, depth int) error {
	if depth > 8 {
		return nil
	}
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read part: %w", err)
		}
		ctype := part.Header.Get("Content-Type")
		if ctype == "" {
			ctype = "text/plain"
		}
		mediatype, params, err := mime.ParseMediaType(ctype)
		if err != nil {
			continue
		}
		if strings.HasPrefix(mediatype, "multipart/") && params["boundary"] != "" {
			if err := walkParts(multipart.NewReader(part, params["boundary"]), req, depth+1); err != nil {
				return err
			}
			continue
		}
		data, err := decodeTransfer(part, part.Header.Get("Content-Transfer-Encoding"))
		if err != nil {
			continue
		}
		if strings.EqualFold(mediatype, "text/html") {
			if req.HTML == "" {
				req.HTML = string(data)
			}
			continue
		}
		if strings.HasPrefix(mediatype, "text/") {
			if req.Text == "" {
				req.Text = string(data)
			}
			continue
		}
		// Not text: an attachment, or a part such as message/rfc822 that is
		// best carried along as one. The store takes base64 either way.
		name := part.FileName()
		if name == "" {
			name = "attachment"
		}
		req.Attachment = append(req.Attachment, webmail.AttachmentInput{
			Filename: name,
			MIMEType: mediatype,
			Content:  base64.StdEncoding.EncodeToString(data),
		})
	}
}

// decodeTransfer reads a part's bytes, undoing the transfer encoding named in
// its headers. A part that names an encoding this server does not know is read
// as if it named none, which is what it almost always means.
func decodeTransfer(r io.Reader, encoding string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		decoder := base64.NewDecoder(base64.StdEncoding, newMIMEBaseReader(r))
		return io.ReadAll(decoder)
	case "quoted-printable":
		return io.ReadAll(quotedprintable.NewReader(r))
	default:
		return io.ReadAll(r)
	}
}

// newMIMEBaseReader drops the whitespace base64 in mail is allowed to carry.
func newMIMEBaseReader(r io.Reader) io.Reader {
	return &whitespaceSkippingReader{r: r}
}

type whitespaceSkippingReader struct {
	r io.Reader
}

func (w *whitespaceSkippingReader) Read(p []byte) (int, error) {
	buf := make([]byte, len(p))
	n, err := w.r.Read(buf)
	kept := buf[:n][:0]
	for _, b := range buf[:n] {
		if b == '\r' || b == '\n' || b == ' ' || b == '\t' {
			continue
		}
		kept = append(kept, b)
	}
	copied := copy(p, kept)
	if copied > 0 && err != nil {
		// Bytes were delivered, so the end of the stream is the next call's
		// answer rather than this one's.
		return copied, nil
	}
	return copied, err
}

// parseAddressList reads a header address list ("A <a@b>, c@d") into the shape
// the composer takes. A list that cannot be read is dropped rather than passed
// on half-parsed: a malformed recipient is a bounce, and a bounce is worse
// than a refusal.
func parseAddressList(value string) []webmail.Address {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	list, err := mail.ParseAddressList(value)
	if err != nil {
		return nil
	}
	out := make([]webmail.Address, 0, len(list))
	for _, a := range list {
		out = append(out, webmail.Address{Address: a.Address, Name: a.Name})
	}
	return out
}
