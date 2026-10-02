package mail

import (
	"mime"
	"strings"

	gomail "github.com/emersion/go-message/mail"
)

// Address is one mailbox taken from a header, with its display name decoded.
//
// RFC 2047 encoded words in the display name are decoded on the way in, so
// "=?utf-8?q?Andr=C3=A9?=" arrives as "André". Anything stored or rendered
// downstream should assume names are already readable.
type Address struct {
	Name    string `json:"name,omitempty"`
	Address string `json:"address"`
}

// String renders the address back into a header value, quoting the display
// name when it contains anything that would otherwise be ambiguous.
func (a Address) String() string {
	addr := &gomail.Address{Name: a.Name, Address: a.Address}
	return addr.String()
}

// ParseAddressList parses an RFC 5322 address list.
//
// A header that is absent, empty or whitespace yields no addresses and no
// error, because "To: " is legal and a message is still deliverable. A header
// that is present but unparseable is an error: silently dropping recipients
// would deliver a message to the wrong set of people.
func ParseAddressList(s string) ([]Address, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	parsed, err := gomail.ParseAddressList(s)
	if err != nil {
		return nil, err
	}
	return convertAddresses(parsed), nil
}

// FormatAddressList renders addresses back into a header value.
func FormatAddressList(addrs []Address) string {
	parts := make([]string, 0, len(addrs))
	for _, a := range addrs {
		parts = append(parts, a.String())
	}
	return strings.Join(parts, ", ")
}

// AddressesOf reduces a list to bare addresses, which is what envelope
// recipients and policy checks compare against.
func AddressesOf(addrs []Address) []string {
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if a.Address != "" {
			out = append(out, strings.ToLower(a.Address))
		}
	}
	return out
}

// DecodeText decodes RFC 2047 encoded words in arbitrary header text.
//
// It is used for values that go-message does not type for us, such as a subject
// read off the raw header, and for display names that arrived double-encoded.
// Text with no encoded words is returned unchanged, and text that cannot be
// decoded is returned unchanged rather than dropped, because a mojibake name is
// better than an empty one.
func DecodeText(s string) string {
	if s == "" || !strings.Contains(s, "=?") {
		return s
	}
	decoder := mime.WordDecoder{CharsetReader: charsetReader}
	out, err := decoder.DecodeHeader(s)
	if err != nil {
		return s
	}
	return out
}

// DecodeAddress decodes a display name in an address spec of the form
// `Name <addr@host>`, tolerating a bare address.
func DecodeAddress(s string) string {
	if s == "" {
		return ""
	}
	if idx := strings.LastIndex(s, "<"); idx > 0 {
		name := strings.TrimSpace(s[:idx])
		name = strings.Trim(name, `"`)
		return DecodeText(name)
	}
	return DecodeText(s)
}
