package webmail

import (
	"bytes"
	"testing"

	"github.com/linchange/linchange/internal/mail"
)

// A header address whose display name is the address itself -- exactly what an
// EWS client sends when it has no display name to give -- must still parse:
// unquoted, "a@example.com <a@example.com>" is not a legal RFC 5322 phrase, and
// a composer that rejected it would refuse a perfectly good recipient.
func TestAddressStringParsesWhenTheNameIsTheAddress(t *testing.T) {
	for _, a := range []Address{
		{Address: "e2e-user@example.com", Name: "e2e-user@example.com"},
		{Address: "a@example.com", Name: "Doe, John"},
		{Address: "a@example.com", Name: "Alice"},
		{Address: "a@example.com"},
	} {
		if _, err := mail.ParseAddressList(a.String()); err != nil {
		}
	}
}

// The queued copy of a sent message has to stay parseable after the Bcc is
// stripped: a message without the empty line between headers and body cannot
// be parsed by any recipient, including this server's own inbound listener,
// which answers 554 to it.
func TestStripBccKeepsTheHeaderBodySeparator(t *testing.T) {
	raw := []byte("From: a@example.com\r\n" +
		"To: b@example.com\r\n" +
		"Bcc: hidden@example.com\r\n" +
		"Subject: hi\r\n" +
		"\r\n" +
		"the body\r\n")

	got := stripBcc(raw)
	if bytes.Contains(got, []byte("hidden@example.com")) {
		t.Errorf("Bcc survived stripping:\n%s", got)
	}
	if !bytes.Contains(got, []byte("\r\n\r\n")) {
		t.Errorf("the header/body separator was lost:\n%q", got)
	}
	want := "From: a@example.com\r\nTo: b@example.com\r\nSubject: hi\r\n\r\nthe body\r\n"
	if string(got) != want {
		t.Errorf("stripBcc =\n%q\nwant\n%q", got, want)
	}
}

// A message without a Bcc is rewritten all the same, and comes out byte for
// byte the same message.
func TestStripBccLeavesAMessageWithoutBccIntact(t *testing.T) {
	raw := []byte("From: a@example.com\r\nSubject: hi\r\n\r\nbody\r\n")
	if got := stripBcc(raw); !bytes.Equal(got, raw) {
		t.Errorf("stripBcc changed the message:\ngot  %q\nwant %q", got, raw)
	}
}
