package caldav

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"
)

// The namespaces this server speaks.
//
// A WebDAV document is namespaced per element rather than per document, and a
// client decides what a property means by the namespace URI, never by the
// prefix it happens to have been written with. Apple calls its calendar colour
// "A:calendar-color" and RFC 7529 calls the same idea something else entirely,
// so matching on a prefix would make this server understand whichever
// convention it happened to invent for itself.
const (
	nsDAV    = "DAV:"
	nsCalDAV = "urn:ietf:params:xml:ns:caldav"
	nsApple  = "http://apple.com/ns/ical/"
	// nsXMLNS is the pseudo-namespace a namespace declaration carries. It is not
	// spoken: no element is ever in it, and a declaration never survives parsing
	// because the decoder has already applied it by then.
	nsXMLNS = "xmlns"
)

// nsPrefix maps a namespace URI to the prefix this server writes it under.
//
// Clients resolve URIs, so these are a readability choice and nothing more, but
// the declarations still have to be on the document root or the document is not
// well formed. The prefixes are the conventional ones because a human reading a
// packet capture of a sync should not have to look them up.
var nsPrefix = map[string]string{
	nsDAV:    "D",
	nsCalDAV: "C",
	nsApple:  "A",
}

// hrefProp is the href-valued DAV property every WebDAV implementation has, and
// it is the one that appears inside collection-valued properties such as
// calendar-home-set, so it is worth a name.
func hrefProp(local string) string { return nsDAV + " " + local }

// propName is a qualified property name, kept as a single string so a set of them
// can be compared and used as a map key.
type propName string

// knownProp builds a qualified property name.
func knownProp(space, local string) propName { return propName(space + " " + local) }

// splitProp separates a qualified property name back into its two halves.
func splitProp(p propName) (space, local string) {
	space, local, _ = strings.Cut(string(p), " ")
	return space, local
}

// node is one element of a DAV document.
//
// The XML is built as a tree rather than marshalled from structs because
// encoding/xml cannot emit an arbitrary set of namespace prefixes under chosen
// names: given a tag it writes a default namespace and nests another one on the
// child, which is valid XML and is read correctly by a namespace-aware client but
// is unreadable to a human and has been the source of DAV interoperability bugs
// before. Handing this server's own tests and its own log output a document in
// the prefixes the RFCs use is worth the small amount of code.
//
// A node with no children and no text is written as a self-closing element,
// which is what an empty collection-valued property looks like.
type node struct {
	space string
	local string
	// attrs are written before any children, in order, and their values are
	// escaped. Namespace declarations are attrs with space "xmlns".
	attrs []attr
	kids  []*node
	// text is the character data of this element, used by leaf elements. A node
	// with both text and children would be mixed content, which DAV does not
	// use, so render refuses it rather than emitting something ambiguous.
	text string
}

type attr struct {
	space string
	local string
	value string
}

// el builds an element with children.
func el(space, local string, kids ...*node) *node {
	return &node{space: space, local: local, kids: kids}
}

// leaf builds an element holding character data.
func leaf(space, local, value string) *node {
	return &node{space: space, local: local, text: value}
}

// href builds a DAV:href, the URL-valued leaf that collection properties are
// made of.
func href(value string) *node { return leaf(nsDAV, "href", value) }

// withAttrs returns n with the given attributes, so a property element can be
// written with its own namespace attributes in one expression.
func withAttrs(n *node, attrs ...attr) *node {
	n.attrs = append(n.attrs, attrs...)
	return n
}

// xmlAttr builds a prefixed attribute, for the cases where a property's value
// depends on an attribute rather than on the namespace of a child element.
func xmlAttr(space, local, value string) attr { return attr{space: space, local: local, value: value} }

// unprefixedAttr builds an attribute with no namespace, which is what every DAV
// attribute is: the namespaced "content-type" and "valid" on supported-calendar-
// component-set are the exception and say so.
func unprefixedAttr(local, value string) attr { return attr{local: local, value: value} }

// document is a rendered tree's namespace plan.
//
// A namespace has to be declared before it is used, and only the root can carry
// the declaration without the document becoming a mess of scattered prefixes.
// So the tree is walked first to find out what it uses, and the root then
// declares all of it in one place -- which is what the RFCs' own examples do.
type document struct {
	// prefix maps a namespace URI to the prefix the document declares it under.
	prefix map[string]string
	// declared is the order the declarations are written in.
	declared []string
}

// render writes the tree as a standalone document, declaring every namespace it
// used on the root.
func render(root *node) []byte {
	doc := newDocument(root)

	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	doc.writeNode(&buf, root, true)
	buf.WriteByte('\n')
	return buf.Bytes()
}

// newDocument plans the prefixes for a tree.
//
// The three namespaces this server speaks come first, under their conventional
// prefixes, and only the ones the tree actually uses are declared at all: a
// declaration with no use is legal XML but is noise in every response, and a use
// with no declaration is a document a strict parser rejects outright.
//
// The order is fixed rather than taken from map iteration because two answers to
// the same question have to be byte-identical. A client that diffs two sync
// responses to decide what changed is looking at the whole body, so a map's
// iteration order would make every response look different from the last. A
// namespace reached some other way -- a property name a client sent that this
// registry has no entry for -- is therefore given the next free xN in sorted URI
// order, so even that case is reproducible.
func newDocument(root *node) *document {
	used := map[string]bool{}
	collectNamespaces(root, used)

	doc := &document{prefix: map[string]string{}}
	for _, space := range []string{nsDAV, nsCalDAV, nsApple} {
		if !used[space] {
			continue
		}
		doc.prefix[space] = nsPrefix[space]
		doc.declared = append(doc.declared, space)
	}

	other := make([]string, 0, len(used))
	for space := range used {
		if _, known := nsPrefix[space]; !known {
			other = append(other, space)
		}
	}
	sort.Strings(other)
	for _, space := range other {
		doc.prefix[space] = "x" + strconvItoa(int64(len(doc.prefix)+1))
		doc.declared = append(doc.declared, space)
	}
	return doc
}

// collectNamespaces notes every namespace an element and its children use.
func collectNamespaces(n *node, used map[string]bool) {
	if n == nil {
		return
	}
	used[n.space] = true
	for _, kid := range n.kids {
		collectNamespaces(kid, used)
	}
}

func (d *document) writeNode(buf *bytes.Buffer, n *node, isRoot bool) {
	prefix := d.prefix[n.space]

	buf.WriteByte('<')
	if prefix != "" {
		buf.WriteString(prefix)
		buf.WriteByte(':')
	}
	buf.WriteString(n.local)

	if isRoot {
		for _, space := range d.declared {
			buf.WriteString(` xmlns:`)
			buf.WriteString(d.prefix[space])
			buf.WriteString(`="`)
			escapeTo(buf, space)
			buf.WriteByte('"')
		}
	}
	for _, a := range n.attrs {
		// A namespace declaration is dropped here: a property never carries
		// one, because the document has already declared every namespace it
		// uses. An ordinary attribute is written with its own prefix, which
		// was resolved when the request was parsed.
		if a.space == nsXMLNS {
			continue
		}
		buf.WriteByte(' ')
		if a.space != "" {
			buf.WriteString(d.prefix[a.space])
			buf.WriteByte(':')
		}
		buf.WriteString(a.local)
		buf.WriteString(`="`)
		escapeTo(buf, a.value)
		buf.WriteByte('"')
	}
	if len(n.kids) == 0 && n.text == "" {
		buf.WriteString("/>")
		return
	}
	buf.WriteByte('>')

	if n.text != "" {
		escapeTo(buf, n.text)
	}
	for _, kid := range n.kids {
		d.writeNode(buf, kid, false)
	}

	buf.WriteString("</")
	if prefix != "" {
		buf.WriteString(prefix)
		buf.WriteByte(':')
	}
	buf.WriteString(n.local)
	buf.WriteByte('>')
}

// escapeTo writes s as XML character data.
//
// Control characters are dropped rather than escaped. XML 1.0 has no way to
// represent them at all -- not even as a numeric reference -- so a SUMMARY
// carrying a vertical tab cannot be put in a calendar-data property in any
// conformant encoding. A client that sends one will send it raw, so the only
// options are to strip it here or to emit a document the client cannot parse,
// and there is no third option where it survives.
func escapeTo(buf *bytes.Buffer, s string) {
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch {
			case c == '&':
				buf.WriteString("&amp;")
			case c == '<':
				buf.WriteString("&lt;")
			case c == '>':
				buf.WriteString("&gt;")
			case c == '"':
				buf.WriteString("&quot;")
			case c == '\t' || c == '\n' || c == '\r':
				buf.WriteByte(c)
			case c < 0x20:
				// Stripped. See above.
			default:
				buf.WriteByte(c)
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			// An invalid byte. There is no character to write, and writing a
			// replacement would silently change the text.
			i++
			continue
		}
		buf.WriteString(s[i : i+size])
		i += size
	}
}

// parseXML reads a document into a tree with every element's namespace resolved
// to its URI.
//
// The decoder does the resolving, which is the whole reason for going through a
// tree rather than unmarshalling into tagged structs: a struct tag can name a
// namespace URI and match elements in it regardless of prefix, but it cannot say
// which of several sibling elements in the same namespace a value came from
// without one type per element, and CalDAV reports are distinguished by their
// root element alone.
func parseXML(data []byte) (*node, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false

	var root *node
	var stack []*node

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("caldav: reading the request: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			n := &node{space: t.Name.Space, local: t.Name.Local}
			for _, a := range t.Attr {
				if a.Name.Space == nsXMLNS || a.Name.Space == "" && a.Name.Local == nsXMLNS {
					// A declaration. The decoder has already applied it, so
					// keeping it would only re-emit it somewhere it does not
					// belong.
					continue
				}
				n.attrs = append(n.attrs, attr{space: a.Name.Space, local: a.Name.Local, value: a.Value})
			}
			if root == nil {
				root = n
			}
			if len(stack) > 0 {
				parent := stack[len(stack)-1]
				parent.kids = append(parent.kids, n)
			}
			stack = append(stack, n)

		case xml.EndElement:
			if len(stack) == 0 {
				return nil, fmt.Errorf("caldav: %q closed but nothing was open", t.Name.Local)
			}
			stack = stack[:len(stack)-1]

		case xml.CharData:
			if len(stack) > 0 {
				// Accumulated rather than assigned, so a document that splits a
				// run of character data around a comment still reads as one
				// string. This matters for calendar-data: an iCalendar body with
				// a folded line and a comment in it is still one body.
				stack[len(stack)-1].text += string(t)
			}
		}
	}
	if root == nil {
		return nil, fmt.Errorf("caldav: the request body is empty")
	}
	if len(stack) != 0 {
		return nil, fmt.Errorf("caldav: %q was never closed", stack[len(stack)-1].local)
	}
	return root, nil
}

// child returns the first child in the given namespace with the given local name.
func (n *node) child(space, local string) *node {
	if n == nil {
		return nil
	}
	for _, kid := range n.kids {
		if kid.space == space && kid.local == local {
			return kid
		}
	}
	return nil
}

// children returns every child in the given namespace with the given local name.
func (n *node) childrenNamed(space, local string) []*node {
	if n == nil {
		return nil
	}
	var out []*node
	for _, kid := range n.kids {
		if kid.space == space && kid.local == local {
			out = append(out, kid)
		}
	}
	return out
}

// descendant returns the first element at any depth with the given qualified
// name. Filters nest to a depth the callers do not track, so a filter written
// with a wrapper element in between is still found.
func (n *node) descendant(space, local string) *node {
	if n == nil {
		return nil
	}
	for _, kid := range n.kids {
		if kid.space == space && kid.local == local {
			return kid
		}
		if found := kid.descendant(space, local); found != nil {
			return found
		}
	}
	return nil
}

// attrValue returns an attribute by local name, or "" when it is absent. DAV
// attributes are unprefixed, so the namespace is not part of the lookup.
func (n *node) attrValue(local string) string {
	if n == nil {
		return ""
	}
	for _, a := range n.attrs {
		if a.local == local {
			return a.value
		}
	}
	return ""
}

// hasAttr reports whether an attribute is present, which for DAV is not the same
// as having a value: <C:calendar-data/> and <C:calendar-data></C:calendar-data>
// mean different things, and a prop-filter with no test is a presence test.
func (n *node) hasAttr(local string) bool {
	if n == nil {
		return false
	}
	for _, a := range n.attrs {
		if a.local == local {
			return true
		}
	}
	return false
}

// propElements lists the DAV:prop children of a request, returning the qualified
// names asked for. An empty prop with no children means allprop, which the
// caller signals by getting a nil Prop.
func (n *node) propElements() []propName {
	prop := n.child(nsDAV, "prop")
	if prop == nil {
		return nil
	}
	out := make([]propName, 0, len(prop.kids))
	for _, kid := range prop.kids {
		out = append(out, knownProp(kid.space, kid.local))
	}
	return out
}

// is reports whether the element has the given qualified name.
func (n *node) is(space, local string) bool {
	return n != nil && n.space == space && n.local == local
}

// localOf returns the element's local name, for error messages that name the
// report the client sent.
func (n *node) localOf() string {
	if n == nil {
		return ""
	}
	return n.local
}
