package mail_test

import (
	"strings"
	"testing"

	"github.com/linchange/linchange/internal/mail"
)

// simpleMessage is a plain text mail with one attachment part.
const simpleMessage = "From: Alice Example <alice@example.com>\r\n" +
	"To: bob@example.com, Carol <carol@example.com>\r\n" +
	"Subject: Quarterly numbers\r\n" +
	"Message-ID: <root-1@example.com>\r\n" +
	"Date: Mon, 02 Jan 2006 15:04:05 +0000\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"The revenue is up.\r\n"

func TestParsePlainText(t *testing.T) {
	parsed, err := mail.Parse([]byte(simpleMessage))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if parsed.Text != "The revenue is up.\r\n" {
		t.Errorf("Text = %q", parsed.Text)
	}
	if parsed.HTML != "" {
		t.Errorf("HTML = %q, want empty", parsed.HTML)
	}
	if len(parsed.Attachments) != 0 {
		t.Errorf("Attachments = %d, want 0", len(parsed.Attachments))
	}
}

func TestMetadataFromPlainText(t *testing.T) {
	meta, err := mail.Describe([]byte(simpleMessage))
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}

	if meta.Subject != "Quarterly numbers" {
		t.Errorf("Subject = %q", meta.Subject)
	}
	if meta.FromAddress != "alice@example.com" {
		t.Errorf("FromAddress = %q", meta.FromAddress)
	}
	if meta.FromName != "Alice Example" {
		t.Errorf("FromName = %q", meta.FromName)
	}
	if len(meta.To) != 2 {
		t.Fatalf("To = %d addresses, want 2", len(meta.To))
	}
	if meta.To[1].Address != "carol@example.com" || meta.To[1].Name != "Carol" {
		t.Errorf("To[1] = %+v", meta.To[1])
	}
	if meta.MessageID != "root-1@example.com" {
		t.Errorf("MessageID = %q, want it without angle brackets", meta.MessageID)
	}
	if meta.ThreadKey != "root-1@example.com" {
		t.Errorf("ThreadKey = %q, want the Message-ID", meta.ThreadKey)
	}
	if meta.Date.Year() != 2006 {
		t.Errorf("Date = %v", meta.Date)
	}
	if meta.HasAttachments {
		t.Error("HasAttachments = true, want false")
	}
	// The snippet has to come from the body, or full-text search can only ever
	// match subjects and names.
	if !strings.Contains(meta.Snippet, "revenue") {
		t.Errorf("Snippet = %q, want body text", meta.Snippet)
	}
	if meta.Size != int64(len(simpleMessage)) {
		t.Errorf("Size = %d, want %d", meta.Size, len(simpleMessage))
	}
}

const multipartMessage = "From: Alice <alice@example.com>\r\n" +
	"To: bob@example.com\r\n" +
	"Subject: With parts\r\n" +
	"Message-ID: <parts-1@example.com>\r\n" +
	"Content-Type: multipart/mixed; boundary=\"outer\"\r\n" +
	"\r\n" +
	"--outer\r\n" +
	"Content-Type: multipart/alternative; boundary=\"inner\"\r\n" +
	"\r\n" +
	"--inner\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"plain body\r\n" +
	"--inner\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n" +
	"\r\n" +
	"<p>html body</p>\r\n" +
	"--inner--\r\n" +
	"--outer\r\n" +
	"Content-Type: application/pdf; name=\"report.pdf\"\r\n" +
	"Content-Disposition: attachment; filename=\"report.pdf\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"JVBERi0xLjQK\r\n" +
	"--outer--\r\n"

func TestParseMultipart(t *testing.T) {
	parsed, err := mail.Parse([]byte(multipartMessage))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if strings.TrimSpace(parsed.Text) != "plain body" {
		t.Errorf("Text = %q", parsed.Text)
	}
	if strings.TrimSpace(parsed.HTML) != "<p>html body</p>" {
		t.Errorf("HTML = %q", parsed.HTML)
	}

	if len(parsed.Attachments) != 1 {
		t.Fatalf("Attachments = %d, want 1", len(parsed.Attachments))
	}
	att := parsed.Attachments[0]
	if att.Filename != "report.pdf" {
		t.Errorf("Filename = %q", att.Filename)
	}
	if att.ContentType != "application/pdf" {
		t.Errorf("ContentType = %q", att.ContentType)
	}
	// base64 has to be decoded, or a download would hand back the encoded text.
	if !strings.HasPrefix(string(att.Content), "%PDF-1.4") {
		t.Errorf("Content = %q, want the decoded PDF", att.Content)
	}

	if got := parsed.Snippet(mail.SnippetLength); !strings.Contains(got, "plain body") {
		t.Errorf("Snippet = %q, want the text body", got)
	}
}

func TestDescribeReportsAttachments(t *testing.T) {
	meta, err := mail.Describe([]byte(multipartMessage))
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if !meta.HasAttachments {
		t.Error("HasAttachments = false, want true for a message with a file part")
	}
}

// An inline image is referenced from the HTML body, so it is not an attachment.
const inlineImageMessage = "From: Alice <alice@example.com>\r\n" +
	"To: bob@example.com\r\n" +
	"Subject: Logo\r\n" +
	"Content-Type: multipart/related; boundary=\"rel\"\r\n" +
	"\r\n" +
	"--rel\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n" +
	"\r\n" +
	"<p>see <img src=\"cid:logo1\"></p>\r\n" +
	"--rel\r\n" +
	"Content-Type: image/png\r\n" +
	"Content-ID: <logo1>\r\n" +
	"Content-Disposition: inline\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"iVBORw0KGgo=\r\n" +
	"--rel--\r\n"

func TestParseInlineImage(t *testing.T) {
	parsed, err := mail.Parse([]byte(inlineImageMessage))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(parsed.Attachments) != 0 {
		t.Errorf("Attachments = %d, want 0 for a referenced inline image", len(parsed.Attachments))
	}
	if len(parsed.Inline) != 1 {
		t.Fatalf("Inline = %d, want 1", len(parsed.Inline))
	}
	if parsed.Inline[0].ContentID != "logo1" {
		t.Errorf("ContentID = %q, want it without the angle brackets", parsed.Inline[0].ContentID)
	}

	part, ok := parsed.InlineByID("logo1")
	if !ok {
		t.Fatal("InlineByID(logo1) not found, but a cid: URL in the body points at it")
	}
	if part.Size() == 0 {
		t.Error("inline part is empty after decoding")
	}
}

func TestSnippetFromHTMLWhenNoText(t *testing.T) {
	const htmlOnly = "From: a@example.com\r\n" +
		"To: b@example.com\r\n" +
		"Subject: s\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"\r\n" +
		"<html><body><p>Hello   there</p><p>second</p></body></html>"

	parsed, err := mail.Parse([]byte(htmlOnly))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := parsed.Snippet(mail.SnippetLength)
	if !strings.Contains(got, "Hello there second") {
		t.Errorf("Snippet = %q, want the tags stripped and whitespace collapsed", got)
	}
}

func TestSnippetTruncatesOnWordBoundary(t *testing.T) {
	// A long body must be cut without leaving a half word, and must cut on a
	// rune boundary rather than splitting a multi-byte character.
	long := strings.Repeat("café ", 100)
	body := "From: a@example.com\r\nTo: b@example.com\r\nSubject: s\r\n\r\n" + long
	parsed, err := mail.Parse([]byte(body))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := parsed.Snippet(20)
	if len([]rune(got)) > 21 {
		t.Errorf("Snippet length = %d runes, want at most 20 plus the ellipsis", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("Snippet = %q, want a truncation marker", got)
	}
	if strings.Contains(got, "\uFFFD") {
		t.Errorf("Snippet = %q, want no replacement character from a split rune", got)
	}
}

func TestUnknownCharsetKeepsTheMessage(t *testing.T) {
	const weird = "From: a@example.com\r\n" +
		"To: b@example.com\r\n" +
		"Subject: s\r\n" +
		"Content-Type: text/plain; charset=x-nonexistent-charset\r\n" +
		"\r\n" +
		"body survives\r\n"

	parsed, err := mail.Parse([]byte(weird))
	if err != nil {
		t.Fatalf("Parse: %v, want a message with an unknown charset to survive", err)
	}
	if len(parsed.Parts) != 1 {
		t.Fatalf("Parts = %d, want 1", len(parsed.Parts))
	}
	if !strings.Contains(string(parsed.Parts[0].Content), "body survives") {
		t.Errorf("Content = %q, want the raw bytes kept", parsed.Parts[0].Content)
	}
}

func TestMalformedContentTypeStillYieldsAPart(t *testing.T) {
	const bad = "From: a@example.com\r\nTo: b@example.com\r\nSubject: s\r\n" +
		"Content-Type: text/plain; charset=\"unclosed\r\n\r\nbody\r\n"

	parsed, err := mail.Parse([]byte(bad))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(parsed.Parts) != 1 {
		t.Fatalf("Parts = %d, want 1", len(parsed.Parts))
	}
	if parsed.Parts[0].ContentType != "text/plain" {
		t.Errorf("ContentType = %q, want the type read despite the bad parameters", parsed.Parts[0].ContentType)
	}
}

func TestRecipientsMergeToAndCc(t *testing.T) {
	parsed, err := mail.Parse([]byte(simpleMessage))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := parsed.Recipients()
	if len(got) != 2 {
		t.Fatalf("Recipients = %v, want 2", got)
	}
	if got[0].Address != "bob@example.com" || got[1].Address != "carol@example.com" {
		t.Errorf("Recipients = %v", got)
	}
}

// The submission path reads To and Cc off a message the client sent. These are
// the shapes it has to survive, including the line endings real clients emit.
func TestRecipientsAcrossLineEndings(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "crlf",
			body: "From: s@example.com\r\nTo: a@example.com\r\nCc: b@example.com\r\nSubject: s\r\n\r\nbody\r\n",
			want: []string{"a@example.com", "b@example.com"},
		},
		{
			name: "bare lf",
			body: "From: s@example.com\nTo: a@example.com\nSubject: s\n\nbody\n",
			want: []string{"a@example.com"},
		},
		{
			name: "mixed",
			body: "From: s@example.com\r\nTo: a@example.com\nCc: b@example.com\r\n\r\nbody\n",
			want: []string{"a@example.com", "b@example.com"},
		},
		{
			name: "folded continuation",
			body: "From: s@example.com\r\nTo: a@example.com,\r\n\tb@example.com\r\nSubject: s\r\n\r\nbody\r\n",
			want: []string{"a@example.com", "b@example.com"},
		},
		{
			name: "display names",
			body: "From: s@example.com\r\nTo: Alice <a@example.com>, \"Doe, Bob\" <b@example.com>\r\nSubject: s\r\n\r\nbody\r\n",
			want: []string{"a@example.com", "b@example.com"},
		},
		{
			// A header that names no valid address yields nobody rather than an
			// error: the envelope is known regardless of what the header says.
			name: "unusable address",
			body: "From: s@example.com\r\nTo: not-an-address\r\nSubject: s\r\n\r\nbody\r\n",
			want: nil,
		},
		{
			name: "repeated across headers",
			body: "From: s@example.com\r\nTo: a@example.com\r\nCc: a@example.com, b@example.com\r\nSubject: s\r\n\r\nbody\r\n",
			want: []string{"a@example.com", "b@example.com"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := mail.Parse([]byte(tt.body))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			got := mail.AddressesOf(parsed.Recipients())
			if len(got) != len(tt.want) {
				t.Fatalf("Recipients = %v, want %v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("Recipients[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestScheduleDocumentFindsTheITIPPayload pins what makes a message a
// scheduling message: a calendar part that declares a METHOD in its body.
// A calendar file without one - the forward of somebody's appointment - asks
// nothing, and a message with no calendar part at all is ordinary mail.
func TestScheduleDocumentFindsTheITIPPayload(t *testing.T) {
	multipart := func(contentType, body string) string {
		return "From: organizer@example.com\r\n" +
			"To: alice@example.com\r\n" +
			"Subject: Invitation: Design review\r\n" +
			"MIME-Version: 1.0\r\n" +
			"Content-Type: multipart/mixed; boundary=\"cal\"\r\n\r\n" +
			"--cal\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" +
			"You are invited.\r\n" +
			"--cal\r\nContent-Type: " + contentType + "\r\n\r\n" +
			body +
			"\r\n--cal--\r\n"
	}

	request := "BEGIN:VCALENDAR\r\nMETHOD:REQUEST\r\nEND:VCALENDAR\r\n"
	p, err := mail.Parse([]byte(multipart("text/calendar; method=REQUEST; charset=utf-8", request)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	doc := p.ScheduleDocument()
	if string(doc) != request {
		t.Errorf("ScheduleDocument = %q, want the calendar part's bytes", doc)
	}

	// The method parameter is decoration: the body decides. A part that says
	// method=REQUEST while its body carries none is a pasted file.
	pasted := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:x\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	p, err = mail.Parse([]byte(multipart("text/calendar; method=REQUEST; charset=utf-8", pasted)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if doc := p.ScheduleDocument(); doc != nil {
		t.Errorf("ScheduleDocument = %q, want nil for a calendar file with no METHOD", doc)
	}

	// A top-level calendar message with no multipart wrapper is still one:
	// the leaf of a single-part message is the message itself.
	topLevel := "From: organizer@example.com\r\nTo: alice@example.com\r\n" +
		"Content-Type: text/calendar; method=CANCEL\r\n\r\n" +
		"BEGIN:VCALENDAR\r\nMETHOD:CANCEL\r\nEND:VCALENDAR\r\n"
	p, err = mail.Parse([]byte(topLevel))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if doc := p.ScheduleDocument(); !strings.Contains(string(doc), "METHOD:CANCEL") {
		t.Errorf("ScheduleDocument = %q, want the top-level calendar document", doc)
	}

	p, err = mail.Parse([]byte(simpleMessage))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if doc := p.ScheduleDocument(); doc != nil {
		t.Errorf("ScheduleDocument = %q, want nil for ordinary mail", doc)
	}
}
