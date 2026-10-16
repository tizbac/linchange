package webmail

import (
	"bytes"
	"testing"
)

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
