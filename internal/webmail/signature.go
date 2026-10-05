package webmail

import (
	"context"
	"fmt"
	"strings"

	"github.com/linchange/linchange/internal/mail"
	"github.com/linchange/linchange/internal/store"
)

// The signature a user appends to the mail they send.
//
// Three decisions here, each of which a different client makes differently:
//
// It is appended when the message is sent and not when the draft is saved. A
// draft is the message the user is still writing, and a client that put the
// signature in at save time would put a second one in every time the draft was
// saved again, so the copy in Drafts would grow a signature per keystroke-batch
// and the sent copy would carry all of them.
//
// It goes into both parts of the message. A message is multipart/alternative and
// both parts are read by somebody, so the text is derived from the same HTML
// rather than stored beside it, and the two cannot disagree.
//
// It is appended after the body is validated rather than before. An empty message
// is refused whether or not a signature is set, because a signature is not
// something to send instead of writing.

// signatureSeparator is the conventional line between a message and the signature
// that follows it.
//
// RFC 3676 defines "-- " (two hyphens and a space) for this in plain text, and
// clients have understood it for thirty years: it is what lets a reader tell a
// signature from the last line the sender actually wrote. It is emitted whether
// or not the reader will do anything with it, because the reader is not the only
// thing reading the message.
const signatureSeparator = "-- "

// htmlSignatureSeparator is the same boundary in HTML.
//
// A blank line and a horizontal rule, because the plain-text separator is not
// something an HTML renderer can represent and a signature that runs straight on
// from the last line of the message reads as part of it. The rule is inline
// rather than a <hr> so that it cannot be styled away by a client that has its
// own opinion about rules in mail.
const htmlSignatureSeparator = `<br><div style="border-top:1px solid #d1d5db;margin-top:8px;padding-top:8px">`

// Signature returns an owner's stored signature.
func (s *Service) Signature(ctx context.Context, owner string) (store.Signature, error) {
	sig, err := s.signatures.Get(ctx, s.db, owner)
	if err != nil {
		return store.Signature{}, translate(err)
	}
	return sig, nil
}

// SaveSignature stores an owner's signature, replacing whatever was there.
//
// The HTML is size-checked here so that the refusal names the limit, rather than
// letting the column constraint turn it into a generic conflict.
func (s *Service) SaveSignature(ctx context.Context, owner, html string) (store.Signature, error) {
	if len(html) > mail.MaxSignatureBytes {
		return store.Signature{}, fmt.Errorf("%w: the signature is %d bytes, over the %d limit",
			ErrInvalid, len(html), mail.MaxSignatureBytes)
	}
	if err := s.signatures.Set(ctx, s.db, owner, html); err != nil {
		return store.Signature{}, translate(err)
	}
	return store.Signature{Owner: owner, HTML: html}, nil
}

// ClearSignature removes an owner's signature, so they send without one.
func (s *Service) ClearSignature(ctx context.Context, owner string) error {
	if err := s.signatures.Clear(ctx, s.db, owner); err != nil {
		return translate(err)
	}
	return nil
}

// applySignature appends the owner's signature to a message being sent, and
// returns the images the signature carries so they can be attached inline.
//
// A signature that is empty, or an owner who has none, leaves the request
// untouched and returns nothing: this runs on every send, and the common case by
// far is that there is no signature at all.
func (s *Service) applySignature(ctx context.Context, owner string, req *ComposeRequest) ([]mail.InlineImage, error) {
	sig, err := s.signatures.Get(ctx, s.db, owner)
	if err != nil {
		return nil, translate(err)
	}
	if strings.TrimSpace(sig.HTML) == "" {
		return nil, nil
	}

	// The signature is prepared here rather than when it is stored, so that a
	// copy pasted into the message body is handled the same way as one pasted
	// into the signature box, and so that the images are only decoded on a send
	// that is actually going out.
	prepared, images, err := mail.PrepareSignature(sig.HTML, s.hostname)
	if err != nil {
		// A signature that cannot be prepared is not a reason to lose the
		// message. It is dropped and the mail goes out without it, which is
		// what the sender would get from every client that failed to render it.
		return nil, nil
	}
	if prepared == "" {
		return nil, nil
	}

	text := mail.SignatureText(sig.HTML)

	switch {
	case req.HTML != "" && req.Text != "":
		// Both parts exist. Each gets the signature in its own form.
		req.HTML = appendHTMLSignature(req.HTML, prepared)
		req.Text = appendTextSignature(req.Text, text)
	case req.HTML != "":
		// HTML only. The plain-text alternative is not there for the signature
		// to be appended to, but it is still not an excuse for the reader who
		// cannot render HTML to be left with an empty alternative and no idea
		// who wrote to them.
		req.HTML = appendHTMLSignature(req.HTML, prepared)
		if text != "" {
			req.Text = signatureSeparator + text
		}
	default:
		// Plain text only, which is the whole message.
		req.Text = appendTextSignature(req.Text, text)
	}

	return images, nil
}

// appendHTMLSignature puts the signature below the body in an HTML part.
func appendHTMLSignature(body, signature string) string {
	if body == "" {
		return htmlSignatureSeparator + signature + "</div>"
	}
	return body + htmlSignatureSeparator + signature + "</div>"
}

// appendTextSignature puts the signature below the body in a plain-text part.
func appendTextSignature(body, signature string) string {
	if signature == "" {
		return body
	}
	if strings.TrimSpace(body) == "" {
		return signatureSeparator + signature
	}
	return strings.TrimRight(body, "\n") + "\n\n" + signatureSeparator + signature
}
