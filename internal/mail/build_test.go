package mail_test

import (
	"strings"
	"testing"

	"github.com/linchange/linchange/internal/mail"
)

func TestBuilderRequiresTheBasics(t *testing.T) {
	tests := []struct {
		name  string
		build func() *mail.Builder
	}{
		{"no sender", func() *mail.Builder {
			return mail.NewBuilder().To(mail.Address{Address: "b@example.com"}).Text("hi")
		}},
		{"no recipient", func() *mail.Builder {
			return mail.NewBuilder().From(mail.Address{Address: "a@example.com"}).Text("hi")
		}},
		{"no body", func() *mail.Builder {
			return mail.NewBuilder().
				From(mail.Address{Address: "a@example.com"}).
				To(mail.Address{Address: "b@example.com"})
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tt.build().Bytes(); err == nil {
				t.Error("Bytes succeeded, want an error naming what is missing")
			}
		})
	}
}

func TestBuilderSinglePartRoundTrips(t *testing.T) {
	raw, err := mail.NewBuilder().
		From(mail.Address{Name: "Alice Example", Address: "alice@example.com"}).
		To(mail.Address{Address: "bob@example.com"}).
		Subject("Lunch?").
		Text("Are you free at one?").
		Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}

	// A plain message must not be wrapped in a multipart: the simplest form is
	// the one every client can read.
	if strings.Contains(string(raw), "multipart/") {
		t.Errorf("single-part message grew a multipart:\n%s", raw)
	}

	meta, err := mail.Describe(raw)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if meta.Subject != "Lunch?" {
		t.Errorf("Subject = %q", meta.Subject)
	}
	if meta.FromAddress != "alice@example.com" {
		t.Errorf("FromAddress = %q", meta.FromAddress)
	}
	if !strings.Contains(meta.Snippet, "free at one") {
		t.Errorf("Snippet = %q, want the body", meta.Snippet)
	}
	if meta.MessageID == "" {
		t.Error("MessageID is empty, want one generated")
	}
}

func TestBuilderAlternativeBodies(t *testing.T) {
	raw, err := mail.NewBuilder().
		From(mail.Address{Address: "a@example.com"}).
		To(mail.Address{Address: "b@example.com"}).
		Subject("Both").
		Text("plain version").
		HTML("<p>html version</p>").
		Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	if !strings.Contains(string(raw), "multipart/alternative") {
		t.Errorf("two bodies should be an alternative:\n%s", raw)
	}

	parsed, err := mail.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if strings.TrimSpace(parsed.Text) != "plain version" {
		t.Errorf("Text = %q", parsed.Text)
	}
	if strings.TrimSpace(parsed.HTML) != "<p>html version</p>" {
		t.Errorf("HTML = %q", parsed.HTML)
	}
}

func TestBuilderAttachmentRoundTrips(t *testing.T) {
	pdf := []byte("%PDF-1.4\nbinary-ish\x00\x01\xff content\n")

	raw, err := mail.NewBuilder().
		From(mail.Address{Address: "a@example.com"}).
		To(mail.Address{Address: "b@example.com"}).
		Subject("Report").
		Text("see attached").
		Attach("report.pdf", "application/pdf", pdf).
		Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}

	meta, err := mail.Describe(raw)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if !meta.HasAttachments {
		t.Error("HasAttachments = false, want true")
	}

	parsed, err := mail.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(parsed.Attachments) != 1 {
		t.Fatalf("Attachments = %d, want 1", len(parsed.Attachments))
	}
	got := parsed.Attachments[0]
	if got.Filename != "report.pdf" {
		t.Errorf("Filename = %q", got.Filename)
	}
	if string(got.Content) != string(pdf) {
		t.Errorf("attachment content = %q, want the original bytes back", got.Content)
	}
}

func TestBuilderTextAttachmentStaysReadable(t *testing.T) {
	// Text is sent as quoted-printable, so a raw view stays legible instead of
	// arriving as a wall of base64.
	raw, err := mail.NewBuilder().
		From(mail.Address{Address: "a@example.com"}).
		To(mail.Address{Address: "b@example.com"}).
		Subject("Notes").
		Text("see attached").
		Attach("notes.txt", "text/plain", []byte("line one\nline two\n")).
		Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	if !strings.Contains(string(raw), "quoted-printable") {
		t.Errorf("text attachment should use quoted-printable:\n%s", raw)
	}

	parsed, err := mail.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// Quoted-printable represents a line break as a hard CRLF, so the decoded
	// attachment carries CRLF endings even though it was handed over with LF.
	// That is what a mail client sees too, and the text is otherwise intact.
	if got := string(parsed.Attachments[0].Content); got != "line one\r\nline two\r\n" {
		t.Errorf("content = %q, want the text with CRLF endings", got)
	}
}

func TestBuilderInlineImageNestsUnderRelated(t *testing.T) {
	raw, err := mail.NewBuilder().
		From(mail.Address{Address: "a@example.com"}).
		To(mail.Address{Address: "b@example.com"}).
		Subject("Logo").
		HTML(`<img src="cid:logo1">`).
		Attach("report.pdf", "application/pdf", []byte("%PDF-1.4\n")).
		InlineImage("logo.png", "image/png", "logo1", []byte{0x89, 'P', 'N', 'G'}).
		Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}

	// Body plus referenced image go in a related part; the download sits beside
	// it in the mixed envelope.
	if !strings.Contains(string(raw), "multipart/related") {
		t.Errorf("inline image should sit in a related part:\n%s", raw)
	}

	parsed, err := mail.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(parsed.Inline) != 1 {
		t.Fatalf("Inline = %d, want 1", len(parsed.Inline))
	}
	if parsed.Inline[0].ContentID != "logo1" {
		t.Errorf("ContentID = %q", parsed.Inline[0].ContentID)
	}
	if len(parsed.Attachments) != 1 || parsed.Attachments[0].Filename != "report.pdf" {
		t.Errorf("Attachments = %+v, want the report only", parsed.Attachments)
	}
	if _, ok := parsed.InlineByID("logo1"); !ok {
		t.Error("InlineByID could not resolve the cid: the HTML body refers to it")
	}
}

func TestBuilderReplyHeaders(t *testing.T) {
	raw, err := mail.NewBuilder().
		From(mail.Address{Address: "bob@example.com"}).
		To(mail.Address{Address: "alice@example.com"}).
		Subject("Re: Plan").
		Text("agreed").
		Header("In-Reply-To", "<root@example.com>").
		Header("References", "<root@example.com>").
		Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}

	meta, err := mail.Describe(raw)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if meta.ThreadKey != "root@example.com" {
		t.Errorf("ThreadKey = %q, want the replied-to message", meta.ThreadKey)
	}
	if meta.ThreadSubject != "plan" {
		t.Errorf("ThreadSubject = %q, want the prefix stripped", meta.ThreadSubject)
	}
}

func TestBuilderReplyToAndBcc(t *testing.T) {
	raw, err := mail.NewBuilder().
		From(mail.Address{Address: "a@example.com"}).
		To(mail.Address{Address: "b@example.com"}).
		Bcc(mail.Address{Address: "archive@example.com"}).
		ReplyTo(mail.Address{Address: "list@example.com"}).
		Subject("s").
		Text("body").
		Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}

	meta, err := mail.Describe(raw)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if len(meta.Bcc) != 1 || meta.Bcc[0].Address != "archive@example.com" {
		t.Errorf("Bcc = %+v", meta.Bcc)
	}
	if len(meta.ReplyTo) != 1 || meta.ReplyTo[0].Address != "list@example.com" {
		t.Errorf("ReplyTo = %+v", meta.ReplyTo)
	}
}

func TestBuilderSanitisesFilenames(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"report.pdf", "report.pdf"},
		{"/home/alice/notes.txt", "notes.txt"},
		{`..\windows\evil.exe`, "evil.exe"},
		{"in\r\nBcc: attacker@example.com", "inBcc: attacker@example.com"},
		{"", "attachment"},
		{"..", "attachment"},
	}

	for _, tt := range tests {
		raw, err := mail.NewBuilder().
			From(mail.Address{Address: "a@example.com"}).
			To(mail.Address{Address: "b@example.com"}).
			Subject("s").
			Text("body").
			Attach(tt.in, "", []byte("x")).
			Bytes()
		if err != nil {
			t.Fatalf("Bytes(%q): %v", tt.in, err)
		}
		parsed, err := mail.Parse(raw)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if got := parsed.Attachments[0].Filename; got != tt.want {
			t.Errorf("filename %q became %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestBuilderGuesssesContentTypeFromFilename(t *testing.T) {
	// A browser sends an empty or generic type for a file read from disk.
	raw, err := mail.NewBuilder().
		From(mail.Address{Address: "a@example.com"}).
		To(mail.Address{Address: "b@example.com"}).
		Subject("s").
		Text("body").
		Attach("photo.png", "", []byte{0x89, 'P'}).
		Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}

	parsed, err := mail.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := parsed.Attachments[0].ContentType; got != "image/png" {
		t.Errorf("ContentType = %q, want it guessed from the filename", got)
	}
}

func TestBuilderBodyRoundTripsNonASCII(t *testing.T) {
	body := "Grüße aus München — 東京 — ok"
	raw, err := mail.NewBuilder().
		From(mail.Address{Name: "André", Address: "andre@example.com"}).
		To(mail.Address{Address: "b@example.com"}).
		Subject("Grüße").
		Text(body).
		Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}

	meta, err := mail.Describe(raw)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if meta.Subject != "Grüße" {
		t.Errorf("Subject = %q", meta.Subject)
	}
	if meta.FromName != "André" {
		t.Errorf("FromName = %q", meta.FromName)
	}
	if !strings.Contains(meta.Snippet, "München") {
		t.Errorf("Snippet = %q, want the body decoded to UTF-8", meta.Snippet)
	}
}
