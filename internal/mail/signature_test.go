package mail_test

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/linchange/linchange/internal/mail"
)

// A one pixel PNG, which is the smallest thing that is genuinely an image and so
// the smallest thing worth pretending is a logo.
var (
	pngBytes = []byte{
		0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 'I', 'H', 'D', 'R',
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
		0x89, 0x00, 0x00, 0x00, 0x0a, 'I', 'D', 'A', 'T',
		0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00, 0x05,
		0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00,
		0x00, 0x00, 'I', 'E', 'N', 'D', 0xae, 0x42,
		0x60, 0x82,
	}
	svgBytes = []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
)

func dataURI(contentType string, data []byte) string {
	return "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// The whole point of PrepareSignature: a data: URI is turned into a real MIME
// part and a cid: reference, because that is the only form of an embedded image
// mail clients agree to render.
func TestPrepareSignatureTurnsDataURIsIntoInlineParts(t *testing.T) {
	body := `<p>Alice Example</p><p><img src="` + dataURI("image/png", pngBytes) + `" alt="logo"></p>`

	out, images, err := mail.PrepareSignature(body, "example.com")
	if err != nil {
		t.Fatalf("PrepareSignature: %v", err)
	}

	if len(images) != 1 {
		t.Fatalf("images = %d, want 1", len(images))
	}
	img := images[0]
	if !bytes.Equal(img.Data, pngBytes) {
		t.Errorf("image bytes = %d, want the %d that were encoded", len(img.Data), len(pngBytes))
	}
	if img.ContentType != "image/png" {
		t.Errorf("image type = %q, want image/png", img.ContentType)
	}
	if !strings.HasSuffix(img.Filename, ".png") {
		t.Errorf("image filename = %q, want a .png", img.Filename)
	}
	if img.ContentID == "" || strings.ContainsAny(img.ContentID, "<>") {
		t.Errorf("content id = %q, want a bare id with no angle brackets", img.ContentID)
	}

	if !strings.Contains(out, `src="cid:`+img.ContentID+`"`) {
		t.Errorf("body does not refer to the part it declares:\n%s", out)
	}
	if strings.Contains(out, "data:image") {
		t.Errorf("body still carries a data URI, which most clients will not render:\n%s", out)
	}
	// The content id belongs to the sending host, so a client that checks it has
	// something that looks like it came from there.
	if !strings.HasSuffix(img.ContentID, "@example.com") {
		t.Errorf("content id = %q, want it to end @example.com", img.ContentID)
	}
}

// Two images in one signature must not share a content id, because a cid: is a
// reference and a duplicated one resolves to whichever part the client finds
// first.
func TestPrepareSignatureGivesEveryImageItsOwnID(t *testing.T) {
	body := `<img src="` + dataURI("image/png", pngBytes) + `"><img src="` + dataURI("image/gif", []byte("GIF89a")) + `">`

	_, images, err := mail.PrepareSignature(body, "example.com")
	if err != nil {
		t.Fatalf("PrepareSignature: %v", err)
	}
	if len(images) != 2 {
		t.Fatalf("images = %d, want 2", len(images))
	}
	if images[0].ContentID == images[1].ContentID {
		t.Errorf("both images were given the id %q", images[0].ContentID)
	}
}

// An SVG is a document that can carry script, and clients render ones that arrive
// in a message. It is left as it was found rather than inlined, and in particular
// it is not inlined and then sanitised, because the difference between the two is
// exactly the part that runs.
func TestPrepareSignatureRefusesToInlineSVG(t *testing.T) {
	src := dataURI("image/svg+xml", svgBytes)
	body := `<img src="` + src + `">`

	out, images, err := mail.PrepareSignature(body, "example.com")
	if err != nil {
		t.Fatalf("PrepareSignature: %v", err)
	}
	if len(images) != 0 {
		t.Errorf("images = %d, want 0 for an SVG", len(images))
	}
	if !strings.Contains(out, src) {
		t.Errorf("the SVG reference was rewritten, so it now points at nothing:\n%s", out)
	}
}

// A reference the user cannot inline is left exactly as found. Rewriting it would
// point at nothing, and removing it would silently drop a picture the user put
// there on purpose.
func TestPrepareSignatureLeavesWhatItCannotInlineAlone(t *testing.T) {
	cases := map[string]string{
		"a remote image":            `<img src="https://example.com/logo.png">`,
		"a broken data URI":         `<img src="data:image/png;base64,not base64 at all">`,
		"an unpadded one":           `<img src="data:image/png;base64,` + base64.RawStdEncoding.EncodeToString(pngBytes) + `">`,
		"a data URI, no media type": `<img src="data:;base64,` + base64.StdEncoding.EncodeToString(pngBytes) + `">`,
		"a data URI, not base64":    `<img src="data:image/png,%89PNG">`,
		"no src at all":             `<img alt="nothing">`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			out, images, err := mail.PrepareSignature(body, "example.com")
			if err != nil {
				t.Fatalf("PrepareSignature: %v", err)
			}
			switch name {
			case "an unpadded one":
				// This one is legitimate, and the raw decoder is what rescues
				// it, so it is the exception rather than the rule.
				if len(images) != 1 {
					t.Fatalf("images = %d, want 1 for base64 without padding", len(images))
				}
				return
			}
			if len(images) != 0 {
				t.Errorf("images = %d, want 0", len(images))
			}
			if strings.Contains(out, "cid:") {
				t.Errorf("a reference was rewritten to cid: for a body with no image part:\n%s", out)
			}
		})
	}
}

func TestPrepareSignatureOfNothingIsNothing(t *testing.T) {
	for _, in := range []string{"", "   \n\t "} {
		out, images, err := mail.PrepareSignature(in, "example.com")
		if err != nil {
			t.Fatalf("PrepareSignature(%q): %v", in, err)
		}
		if out != "" || len(images) != 0 {
			t.Errorf("PrepareSignature(%q) = %q, %d images; want empty", in, out, len(images))
		}
	}
}

// The size limit is in the column constraint as well, and the check here is what
// stops a signature assembled from parts that each fit from reaching the sender.
func TestPrepareSignatureRefusesAnOversizedSignature(t *testing.T) {
	huge := "<p>" + strings.Repeat("x", mail.MaxSignatureBytes) + "</p>"

	_, _, err := mail.PrepareSignature(huge, "example.com")
	if err == nil {
		t.Fatal("PrepareSignature accepted a signature over the limit")
	}
	if !strings.Contains(err.Error(), "over the") {
		t.Errorf("PrepareSignature error = %v, want it to say what was wrong", err)
	}
}

// A signature is pasted markup that goes out inside somebody else's message. The
// HTML that is sent is where the script would run, so it is the HTML that is
// cleaned, not only the text derived from it.
func TestPrepareSignatureStripsBehaviourFromTheHTML(t *testing.T) {
	cases := []struct {
		name string
		in   string
		gone string
		kept string
	}{
		{"a script element", `<p>Alice</p><script>fetch('x')</script>`, "script", "Alice"},
		{"an event handler", `<p onclick="steal()">Alice</p>`, "onclick", "Alice"},
		{"a javascript link", `<a href="javascript:alert(1)">Alice</a>`, "javascript", "Alice"},
		{"a scheme split over lines", "<a href=\"java\nscript:alert(1)\">Alice</a>", "script:", "Alice"},
		{"an iframe", `<p>Alice</p><iframe src="https://example.com"></iframe>`, "iframe", "Alice"},
		{"a remote stylesheet", `<p>Alice</p><link rel="stylesheet" href="x.css">`, "link", "Alice"},
		{"an svg", `<p>Alice</p><svg><circle/></svg>`, "svg", "Alice"},
		{"a form wrapper keeps its text", `<form><p>Alice</p></form>`, "<form", "Alice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, _, err := mail.PrepareSignature(tc.in, "example.com")
			if err != nil {
				t.Fatalf("PrepareSignature: %v", err)
			}
			lower := strings.ToLower(out)
			if strings.Contains(lower, strings.ToLower(tc.gone)) {
				t.Errorf("the sent HTML still carries %q:\n%s", tc.gone, out)
			}
			if !strings.Contains(out, tc.kept) {
				t.Errorf("cleaning took the text with it, %q is gone:\n%s", tc.kept, out)
			}
		})
	}
}

// Colour and type are most of what a signature is, so the style attribute has to
// survive even though everything else about presentation is markup.
func TestPrepareSignatureKeepsStyling(t *testing.T) {
	out, _, err := mail.PrepareSignature(`<p style="color:#b00;font-weight:bold">Alice</p>`, "example.com")
	if err != nil {
		t.Fatalf("PrepareSignature: %v", err)
	}
	if !strings.Contains(out, "color:#b00") {
		t.Errorf("the style attribute was dropped:\n%s", out)
	}
}

// The plain-text alternative is derived, not stored, so it cannot drift from the
// HTML. This is what a recipient who cannot render HTML reads.
func TestSignatureText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"nothing", "", ""},
		{"a name and a title", "<p>Alice Example</p><p>Director</p>",
			"Alice Example\n\nDirector"},
		{"one line with a break", "Alice<br>Director", "Alice\n\nDirector"},
		{"inline markup is not markup", "<p>Call me on <b>07700 900123</b></p>",
			"Call me on 07700 900123"},
		{"an image with alternative text", `<p><img src="x.png" alt="our logo"></p>`,
			"[our logo]"},
		{"an image without it", `<p><img src="x.png"></p>`, ""},
		{"markup run together does not add space", "<p>a<b>b</b></p>", "ab"},
		{"a list", "<ul><li>one</li><li>two</li></ul>", "one\n\ntwo"},
		{"runs of newlines are folded", "<p>a</p><p></p><p></p><p>b</p>", "a\n\nb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mail.SignatureText(tc.in); got != tc.want {
				t.Errorf("SignatureText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A signature is HTML the user wrote, and it goes out inside somebody else's
// message. The elements that carry behaviour have to be gone by the time it is
// sent, because a signature is not a document the recipient chose to open.
func TestSignatureTextDropsScriptAndEventHandlers(t *testing.T) {
	got := mail.SignatureText(`<p onclick="steal()">Alice</p><script>fetch('x')</script>`)

	if strings.Contains(got, "steal") || strings.Contains(got, "fetch") {
		t.Errorf("SignatureText kept script from the signature: %q", got)
	}
	if !strings.Contains(got, "Alice") {
		t.Errorf("SignatureText dropped the text along with the script: %q", got)
	}
}
