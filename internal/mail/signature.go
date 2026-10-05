package mail

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Signature composition: the HTML a user keeps as their signature, and the two
// things that have to happen to it before it can go into a message.
//
// The first is the plain text. A message is multipart/alternative and both parts
// are read by somebody, so the signature has to appear in each of them. Deriving
// the text from the HTML here rather than storing a second copy means the two
// parts cannot disagree, which is the failure nobody notices until a recipient
// reads the plain-text version and finds a logo's worth of nothing.
//
// The second is the images. A signature is written with a logo in it, and the
// browser the user composed it in will have produced a data: URI for that logo.
// A data: URI is the wrong thing to send, though: Outlook refuses to render it,
// several webmail clients strip it outright, and it makes the message several
// times larger than the image. The way mail actually carries an image is as a
// MIME part with a Content-ID that the body refers to as cid:. So every data:
// URI in the signature is decoded into an inline part and the reference is
// rewritten to point at it.

// MaxSignatureBytes bounds a signature.
//
// It is the same number as the user_signatures_html_size constraint, and it is
// stated in both places because the column lives in SQL and this lives in Go and
// neither can call the other. A signature this size is far beyond a logo, and
// below what a pasted screenshot reaches.
const MaxSignatureBytes = 262144

// InlineImage is an image lifted out of an HTML body and turned into a MIME
// part.
type InlineImage struct {
	// ContentID is the part's Content-ID, without angle brackets, which is what
	// a cid: reference in the body carries.
	ContentID string
	Filename  string
	// ContentType is the media type of the decoded bytes.
	ContentType string
	Data        []byte
}

// inlineImageTypes are the image media types that may be inlined, with the
// extension each is given as a part filename.
//
// The list is explicit because the default answer to "is this a thing to put in
// an email body" has to be no. SVG is excluded, and that exclusion is most of the
// reason for the list: an SVG is a document that can carry script, and several
// clients will render one straight out of a message. Everything here is a raster
// format that a client either draws or ignores, with no code of its own.
var inlineImageTypes = map[string]string{
	"image/png":      ".png",
	"image/jpeg":     ".jpg",
	"image/gif":      ".gif",
	"image/bmp":      ".bmp",
	"image/x-ms-bmp": ".bmp",
}

// PrepareSignature returns the HTML body with its images turned into MIME parts.
//
// The returned HTML has had its data: URIs replaced by cid: references, and the
// images are returned for the caller to attach inline. A data: URI that is not a
// permitted image is left exactly as it is: rewriting it is not possible without
// the bytes, and dropping the image would be worse than leaving a reference the
// recipient's client may or may not honour.
//
// The domain is used for the Content-IDs, which are minted rather than supplied
// so that two images in one signature cannot collide.
func PrepareSignature(body, domain string) (string, []InlineImage, error) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return "", nil, nil
	}

	// Checked here as well as by the column constraint, so that a signature
	// assembled from parts which each fit can still be refused before it is
	// spent on a message.
	if len(trimmed) > MaxSignatureBytes {
		return "", nil, fmt.Errorf("mail: the signature is %d bytes, over the %d limit",
			len(trimmed), MaxSignatureBytes)
	}

	nodes, err := parseFragment(trimmed)
	if err != nil {
		return "", nil, fmt.Errorf("mail: read the signature: %w", err)
	}

	var images []InlineImage
	for _, n := range nodes {
		if err := collectInlineImages(n, domain, &images); err != nil {
			return "", nil, err
		}
	}

	var buf bytes.Buffer
	for _, n := range nodes {
		if err := html.Render(&buf, n); err != nil {
			return "", nil, fmt.Errorf("mail: write the signature: %w", err)
		}
	}
	return buf.String(), images, nil
}

// collectInlineImages walks the tree and turns every permitted data: URI into an
// image part, rewriting the reference to point at it.
func collectInlineImages(n *html.Node, domain string, images *[]InlineImage) error {
	if n.Type == html.ElementNode && n.DataAtom == atom.Img {
		contentType, data, ok := decodeImageURI(attr(n, "src"))
		if ok {
			id, err := newContentID(domain)
			if err != nil {
				return err
			}
			*images = append(*images, InlineImage{
				ContentID:   id,
				Filename:    id + inlineImageTypes[contentType],
				ContentType: contentType,
				Data:        data,
			})
			// Without angle brackets, because cid: carries a bare Content-ID
			// rather than an addr-spec and clients that accept one form reject
			// the other.
			setAttr(n, "src", "cid:"+id)
		}
	}
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if err := collectInlineImages(child, domain, images); err != nil {
			return err
		}
	}
	return nil
}

// decodeImageURI reads a data: URI holding a permitted image.
//
// The boolean is false for anything else: not a data: URI, not base64, not an
// image type that may be inlined, or not decodable. Each of those means the
// caller leaves the reference alone, which is the safe outcome.
func decodeImageURI(src string) (contentType string, data []byte, ok bool) {
	if !strings.HasPrefix(src, "data:") {
		return "", nil, false
	}
	rest := src[len("data:"):]

	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		return "", nil, false
	}
	meta, payload := rest[:comma], rest[comma+1:]

	// RFC 2397 makes base64 optional, but a signature's images arrive as base64
	// from every browser that can produce one, and a URL-encoded payload here
	// would be text that was never meant to be an image.
	fields := strings.Split(meta, ";")
	if len(fields) < 2 || !strings.EqualFold(strings.TrimSpace(fields[len(fields)-1]), "base64") {
		return "", nil, false
	}

	mediaType := strings.ToLower(strings.TrimSpace(fields[0]))
	extension, permitted := inlineImageTypes[mediaType]
	if !permitted {
		return "", nil, false
	}

	payload = strings.TrimSpace(payload)
	// Standard first, then raw, because some producers emit base64 without the
	// padding and rescuing a logo that would otherwise arrive as a broken
	// reference costs one wasted attempt.
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		if data, err = base64.RawStdEncoding.DecodeString(payload); err != nil {
			return "", nil, false
		}
	}
	if len(data) == 0 {
		return "", nil, false
	}
	_ = extension
	return mediaType, data, true
}

// newContentID mints an identifier for an inline part.
//
// It uses the same construction as a Message-ID: random bytes in front so two
// images cannot share one, and a hostname at the end so the value is a plausible
// globally unique string rather than a bare counter. The domain is the sending
// host, so a client that is suspicious of an inline part has one that at least
// appears to have come from the sender.
func newContentID(domain string) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		// The same reasoning as NewMessageID: the uniqueness that matters is the
		// random component, and a failing system RNG is not a reason to send a
		// message whose logo cannot be resolved. The counter keeps two calls in
		// the same failing process apart.
		binary.BigEndian.PutUint64(random[:8], uint64(time.Now().UnixNano()))
		binary.BigEndian.PutUint64(random[8:], idCounter.Add(1))
	}
	d := strings.TrimPrefix(strings.ToLower(domain), "@")
	if d == "" || strings.ContainsAny(d, " \t<>@") {
		d = "localhost"
	}
	return hex.EncodeToString(random[:]) + "@" + d, nil
}

// SignatureText renders a signature as the plain-text alternative.
//
// It is the HTML converted rather than a second stored copy, so a change to the
// HTML is a change to both parts at once. An empty signature comes back empty,
// which is what lets a caller append unconditionally.
func SignatureText(sigHTML string) string {
	if strings.TrimSpace(sigHTML) == "" {
		return ""
	}
	nodes, err := parseFragment(sigHTML)
	if err != nil {
		// The HTML was accepted once, when it was stored, so a parse failure
		// here means something rewrote it since. Sending a message with a
		// signature that is literal markup is the worst of the three outcomes,
		// so it is the one returned.
		return strings.TrimSpace(sigHTML)
	}
	var sb strings.Builder
	for _, n := range nodes {
		writeText(&sb, n)
	}
	return strings.TrimSpace(collapseBlankLines(sb.String()))
}

// writeText walks the tree and writes the plain text of what it finds.
func writeText(sb *strings.Builder, n *html.Node) {
	switch {
	case n.Type == html.TextNode:
		sb.WriteString(n.Data)
		return
	case n.Type != html.ElementNode:
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			writeText(sb, child)
		}
		return
	}

	// An image has no text equivalent, and an empty gap where a logo was reads
	// as a mistake. One with alternative text says what it was; one without says
	// nothing, which is the same as what the reader would have got anyway.
	if n.DataAtom == atom.Img {
		if alt := strings.TrimSpace(attr(n, "alt")); alt != "" {
			sb.WriteString("[" + alt + "]")
		}
		return
	}

	// Every block ends a line and <br> is one. Both are emitted for a block,
	// because a <p> ending in a <br> has two line endings to lose and only one
	// of them is the break the user typed; collapseBlankLines folds the result.
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		writeText(sb, child)
	}
	if n.DataAtom == atom.Br || blockElements[n.DataAtom] {
		sb.WriteString("\n\n")
	}
}

// blockElements are the elements that end a line in plain text.
var blockElements = map[atom.Atom]bool{
	atom.P: true, atom.Div: true, atom.Li: true, atom.H1: true, atom.H2: true,
	atom.H3: true, atom.H4: true, atom.H5: true, atom.H6: true,
	atom.Blockquote: true, atom.Pre: true, atom.Ul: true, atom.Ol: true,
	atom.Hr: true, atom.Table: true,
}

// collapseBlankLines folds the runs of newlines the walker emits into single
// blank lines, so a signature does not arrive padded with space that came from
// the markup rather than from the user.
func collapseBlankLines(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	pending := 0
	for _, r := range s {
		if r == '\n' {
			pending++
			continue
		}
		if pending > 0 {
			out.WriteString("\n\n")
			pending = 0
		}
		out.WriteRune(r)
	}
	if pending > 0 {
		out.WriteString("\n\n")
	}
	return out.String()
}

// parseFragment reads an HTML fragment the way it appears inside a body, and
// returns its top-level nodes, with anything that can carry behaviour removed.
//
// ParseFragment rather than Parse, because Parse wraps its input in html, head
// and body, and rendering those back out would put a document skeleton into the
// middle of somebody's message.
//
// The sanitising is here rather than in either caller because a signature is
// pasted markup: it comes out of whatever the user copied from, and it is sent
// inside a message that somebody else chose to open. There is no way to review
// it between the two, so the element list below is what stands between a pasted
// signature and a recipient's mail client.
func parseFragment(s string) ([]*html.Node, error) {
	context := &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body}
	parsed, err := html.ParseFragment(strings.NewReader(s), context)
	if err != nil {
		return nil, err
	}
	// A node that was dropped is detached, and a detached node still renders, so
	// the ones that were removed, and the children that replaced an unwrapped
	// one, are gathered here rather than left for the renderer to trip over.
	var nodes []*html.Node
	for _, n := range parsed {
		nodes = append(nodes, sanitise(n)...)
	}
	return nodes, nil
}

// droppedElements are removed with their contents.
//
// Each is a thing that either runs, or brings in something that does. Their
// contents go with them because the contents are the code: keeping the text of a
// <script> would put "fetch('x')" in the plain-text part as though the user had
// written it, and keeping the children of an <object> is keeping the payload.
var droppedElements = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Iframe: true, atom.Object: true,
	atom.Embed: true, atom.Applet: true, atom.Svg: true, atom.Math: true,
	atom.Frame: true, atom.Frameset: true, atom.Template: true,
	atom.Canvas: true, atom.Video: true, atom.Audio: true, atom.Source: true,
	atom.Track: true, atom.Map: true, atom.Area: true, atom.Input: true,
	atom.Button: true, atom.Select: true, atom.Option: true,
	atom.Textarea: true, atom.Meta: true, atom.Link: true, atom.Base: true,
}

// unwrappedElements keep their contents but lose the element.
//
// These are containers whose purpose does not survive being sent, and a signature
// wrapped in one of them should still say what it says. A <form> around a name
// and an address is a layout habit from a word processor, and the address is
// worth keeping even though submitting it is not.
var unwrappedElements = map[atom.Atom]bool{
	atom.Label: true, atom.Fieldset: true, atom.Legend: true, atom.Noscript: true,
	atom.Center: true, atom.Font: true, atom.Big: true, atom.Form: true,
}

// urlAttributes are the attributes whose value is fetched or navigated to, and so
// the ones where a scheme matters.
var urlAttributes = map[string]bool{
	"href": true, "src": true, "action": true, "background": true,
	"poster": true, "formaction": true, "cite": true, "data": true,
	"xlink:href": true,
}

// dangerousSchemes are refused in a url attribute.
//
// data: is absent on purpose: an image data URI is exactly what PrepareSignature
// is here to replace with a cid: reference, and it rewrites those before this
// list is consulted. Blocking every data: URI would leave the images of every
// signature that carries one as broken references.
var dangerousSchemes = []string{"javascript:", "vbscript:", "data:text/html", "data:application/"}

// sanitise removes behaviour from a parsed signature and returns the nodes that
// take the place of the one it was given: the node itself, its children if it
// was a container that only survives as text, or nothing at all if it was
// dropped.
func sanitise(n *html.Node) []*html.Node {
	if n.Type == html.ElementNode {
		if droppedElements[n.DataAtom] {
			// Taken out with its contents, because the contents are the code:
			// keeping the text of a <script> would put its source in the
			// plain-text part as though the user had written it.
			detach(n)
			return nil
		}
		if unwrappedElements[n.DataAtom] {
			// The children take this node's place, so what the user wrote
			// survives and the tag does not. They are put back before this node
			// rather than appended, because a form around a name is a layout
			// habit from a word processor and the name still has to come first.
			promoted := promoteChildren(n)
			if parent := n.Parent; parent != nil {
				for _, p := range promoted {
					parent.InsertBefore(p, n)
				}
			}
			detach(n)
			return promoted
		}
		sanitiseAttributes(n)
	}

	// The next sibling is read before the walk, because a child that was
	// detached has no NextSibling to step to afterwards.
	for child := n.FirstChild; child != nil; {
		next := child.NextSibling
		sanitise(child)
		child = next
	}
	return []*html.Node{n}
}

// promoteChildren detaches a node's children, sanitised and in order, and
// returns them.
func promoteChildren(n *html.Node) []*html.Node {
	var out []*html.Node
	for child := n.FirstChild; child != nil; {
		next := child.NextSibling
		detach(child)
		out = append(out, sanitise(child)...)
		child = next
	}
	n.FirstChild, n.LastChild = nil, nil
	return out
}

// sanitiseAttributes drops event handlers and references to a dangerous scheme.
//
// The style attribute is kept, because colour and font are most of what a
// signature is, and because a style attribute cannot execute in a client that
// matters.
func sanitiseAttributes(n *html.Node) {
	kept := n.Attr[:0]
	for _, a := range n.Attr {
		name := strings.ToLower(a.Key)
		if strings.HasPrefix(name, "on") {
			continue
		}
		// srcset describes several candidate images and would need the same
		// treatment as src, which it cannot get, so a signature does without it.
		if name == "srcset" || name == "formaction" {
			continue
		}
		if urlAttributes[name] && hasDangerousScheme(a.Val) {
			continue
		}
		kept = append(kept, a)
	}
	n.Attr = kept
}

// hasDangerousScheme reports whether a url begins with a scheme that executes.
//
// The value is read with its whitespace and control characters removed first,
// because "java\nscript:" and " javascript:" both navigate in a browser and a
// check that does not know that is a check that can be walked past.
func hasDangerousScheme(value string) bool {
	lowered := strings.ToLower(value)
	var cleaned strings.Builder
	for _, r := range lowered {
		if r <= ' ' || r == 0x7f {
			continue
		}
		cleaned.WriteRune(r)
	}
	flat := cleaned.String()
	for _, scheme := range dangerousSchemes {
		if strings.HasPrefix(flat, scheme) {
			return true
		}
	}
	return false
}

// detach removes a node from its parent without touching its children.
func detach(n *html.Node) {
	parent := n.Parent
	if parent == nil {
		return
	}
	if parent.FirstChild == n {
		parent.FirstChild = n.NextSibling
	}
	if parent.LastChild == n {
		parent.LastChild = n.PrevSibling
	}
	if n.PrevSibling != nil {
		n.PrevSibling.NextSibling = n.NextSibling
	}
	if n.NextSibling != nil {
		n.NextSibling.PrevSibling = n.PrevSibling
	}
	n.Parent, n.PrevSibling, n.NextSibling = nil, nil, nil
}

// attr returns an attribute's value, or "" when it is absent.
func attr(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val
		}
	}
	return ""
}

// setAttr replaces an attribute's value, adding the attribute if it is not there.
func setAttr(n *html.Node, name, value string) {
	for i := range n.Attr {
		if n.Attr[i].Key == name {
			n.Attr[i].Val = value
			return
		}
	}
	n.Attr = append(n.Attr, html.Attribute{Key: name, Val: value})
}
