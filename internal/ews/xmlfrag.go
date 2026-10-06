package ews

import (
	"encoding/xml"
	"strings"
)

// Reading a fragment of XML.
//
// An item in a request arrives as an element -- <t:Message>, <t:CalendarItem>,
// <t:Updates> -- whose contents are a bag of properties the schema defines and
// whose exact composition varies with which client sent it. Decoding the whole
// request into one flat Go struct cannot work: the same element carries
// <t:Body>Body</t:Body> from one client and
// <t:Body BodyType="HTML">...</t:Body> from another, and the difference is in an
// attribute the schema does not name for the first.
//
// So a fragment is read as a fragment: elements are looked up by name, their text
// is taken, and the parts of the fragment nobody asked about are skipped. That is
// what this file does, and it is deliberately narrow: everything here takes a
// string that is already known to be one item's inner XML.

// newFragmentDecoder starts a decoder over a fragment.
func newFragmentDecoder(fragment string) *xml.Decoder {
	return xml.NewDecoder(strings.NewReader(fragment))
}

// innerElement returns the inner XML of the first <name> element in a fragment.
//
// It returns "" when there is none, which is also the correct answer for an
// element with nothing in it: a caller that wanted to distinguish the two passes
// the name to innerTextPresent instead, which does.
func innerElement(fragment, name string) string {
	return innerXMLAt(fragment, name)
}

// innerXMLAt returns everything between the first <name> and its matching
// </name>.
//
// The nesting is counted rather than assumed. A client sends
// <t:Updates><t:SetItemField><t:Message>...</t:Message></t:SetItemField></t:Updates>
// and looking for the first closing tag would return a prefix of a document that
// does not parse, which every downstream call would then treat as a partial value
// and use anyway.
func innerXMLAt(fragment, name string) string {
	depth := 0
	seen := false
	start := 0
	i := 0
	for i < len(fragment) {
		if fragment[i] != '<' {
			i++
			continue
		}
		end := strings.IndexByte(fragment[i:], '>')
		if end < 0 {
			return ""
		}
		tag := fragment[i : i+end+1]
		local := tagName(tag)
		if local == name && !seen && !strings.HasPrefix(tag, "</") {
			seen = true
			depth = 1
			start = i + len(tag)
			i += len(tag)
			continue
		}
		if seen {
			switch {
			case strings.HasPrefix(tag, "</"):
				depth--
				if depth == 0 {
					return fragment[start:i]
				}
			case isSelfClosing(tag):
				// Nothing to balance: a self-closing element opens and closes.
			default:
				depth++
			}
		}
		i += len(tag)
	}
	// The element was opened and never closed, which is malformed XML that the
	// outer decode would have caught. Returning nothing is the answer that keeps
	// a partial value from being used.
	return ""
}

// innerElements returns the inner XML of every <name> element in a fragment.
//
// A list of attachments is exactly this: many elements of one name inside a
// wrapper of another, read without the caller having to know that the wrapper is
// named <t:Attachments> and the entries <t:FileAttachment>.
func innerElements(fragment, name string) []string {
	var out []string
	rest := fragment
	for {
		idx := indexOfStartTag(rest, name)
		if idx < 0 {
			return out
		}
		open := strings.IndexByte(rest[idx:], '>')
		if open < 0 {
			return out
		}
		if isSelfClosing(rest[idx : idx+open+1]) {
			// An element with nothing in it is still an element: a client
			// sending an empty attachment slot has sent one.
			out = append(out, "")
			rest = rest[idx+open+1:]
			continue
		}
		out = append(out, innerXMLAt(rest[idx:], name))
		closing := strings.Index(rest[idx:], "</"+name+">")
		if closing < 0 {
			return out
		}
		rest = rest[idx+closing+len(name)+3:]
	}
}

// nextStart returns the next start element, skipping anything else.
func nextStart(dec *xml.Decoder) (xml.StartElement, bool) {
	for {
		tok, err := dec.Token()
		if err != nil {
			return xml.StartElement{}, false
		}
		if start, ok := tok.(xml.StartElement); ok {
			return start, true
		}
	}
}

// tagName returns the local name of a tag written as text.
func tagName(tag string) string {
	name := tag
	if strings.HasPrefix(name, "</") {
		name = name[2:]
	} else {
		name = name[1:]
	}
	if i := strings.IndexAny(name, " \t\r\n/>"); i >= 0 {
		name = name[:i]
	}
	if i := strings.IndexByte(name, ':'); i >= 0 {
		// The prefix is not part of the name. A client is free to declare m: or
		// t: or neither, and searching for "m:Message" would find only the ones
		// that chose that prefix.
		name = name[i+1:]
	}
	return name
}

// isSelfClosing reports whether a start tag closes itself.
func isSelfClosing(tag string) bool {
	return len(tag) >= 2 && strings.HasSuffix(tag, "/>")
}

// indexOfStartTag finds the first <name> element in a fragment, ignoring closing
// tags and tags whose name continues with a dash or a letter.
func indexOfStartTag(fragment, name string) int {
	for i := 0; i < len(fragment); {
		idx := strings.IndexByte(fragment[i:], '<')
		if idx < 0 {
			return -1
		}
		i += idx
		end := strings.IndexByte(fragment[i:], '>')
		if end < 0 {
			return -1
		}
		tag := fragment[i : i+end+1]
		if !strings.HasPrefix(tag, "</") && !strings.HasPrefix(tag, "<?") && !strings.HasPrefix(tag, "<!") {
			if tagName(tag) == name {
				return i
			}
		}
		i += len(tag)
	}
	return -1
}
