package mail_test

import (
	"testing"

	"github.com/linchange/linchange/internal/mail"
)

func TestNormaliseSubject(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"Quarterly numbers", "quarterly numbers"},
		{"Re: Quarterly numbers", "quarterly numbers"},
		{"RE: Quarterly numbers", "quarterly numbers"},
		{"Re: Re: Quarterly numbers", "quarterly numbers"},
		{"Re:Fwd:Re: Quarterly numbers", "quarterly numbers"},
		{"FW: Quarterly numbers", "quarterly numbers"},
		{"AW: Quarterly numbers", "quarterly numbers"},
		{"[list] Re: Quarterly numbers", "[list] re: quarterly numbers"},
		{"  Re:   Quarterly   numbers  ", "quarterly numbers"},
		{"", ""},
		{"Re:", ""},
		// A word that merely starts with "re" is not a prefix.
		{"Report", "report"},
		{"Reschedule the report", "reschedule the report"},
	}

	for _, tt := range tests {
		if got := mail.NormaliseSubject(tt.in); got != tt.want {
			t.Errorf("NormaliseSubject(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestThreadKeyPrefersTheRootReference(t *testing.T) {
	refs := []string{"<root@example.com>", "<parent@example.com>"}
	got := mail.ThreadKey("self@example.com", refs, "Re: Plan")
	if got != "root@example.com" {
		t.Errorf("ThreadKey = %q, want the first reference", got)
	}
}

func TestThreadKeyFallsBackToOwnMessageID(t *testing.T) {
	got := mail.ThreadKey("self@example.com", nil, "Plan")
	if got != "self@example.com" {
		t.Errorf("ThreadKey = %q, want the message's own ID", got)
	}
}

func TestThreadKeyFallsBackToSubject(t *testing.T) {
	a := mail.ThreadKey("", nil, "Re: Q3 plan")
	b := mail.ThreadKey("", nil, "Fwd: Q3 plan")
	if a != b {
		t.Errorf("ThreadKey without IDs differ by subject: %q vs %q", a, b)
	}
	if a == "" {
		t.Error("ThreadKey is empty; a message with no ID must still group")
	}
}

// The whole point of threading: a reply has to land on the same key as the
// message it answers.
func TestReplyJoinsTheParentThread(t *testing.T) {
	root := "From: alice@example.com\r\nTo: bob@example.com\r\n" +
		"Subject: Plan\r\nMessage-ID: <root@example.com>\r\n\r\nbody\r\n"
	reply := "From: bob@example.com\r\nTo: alice@example.com\r\n" +
		"Subject: Re: Plan\r\nMessage-ID: <reply@example.com>\r\n" +
		"In-Reply-To: <root@example.com>\r\nReferences: <root@example.com>\r\n\r\nbody\r\n"

	rootMeta, err := mail.Describe([]byte(root))
	if err != nil {
		t.Fatalf("Describe root: %v", err)
	}
	replyMeta, err := mail.Describe([]byte(reply))
	if err != nil {
		t.Fatalf("Describe reply: %v", err)
	}

	if replyMeta.ThreadKey != rootMeta.ThreadKey {
		t.Errorf("reply ThreadKey = %q, want the root's %q", replyMeta.ThreadKey, rootMeta.ThreadKey)
	}
	if replyMeta.ThreadSubject != rootMeta.ThreadSubject {
		t.Errorf("reply ThreadSubject = %q, want %q", replyMeta.ThreadSubject, rootMeta.ThreadSubject)
	}
	if len(replyMeta.References) != 1 || replyMeta.References[0] != "root@example.com" {
		t.Errorf("References = %v, want [root@example.com]", replyMeta.References)
	}
}

func TestInReplyToAloneStillThreads(t *testing.T) {
	const reply = "From: bob@example.com\r\nTo: alice@example.com\r\n" +
		"Subject: Plan\r\nMessage-ID: <reply@example.com>\r\n" +
		"In-Reply-To: <root@example.com>\r\n\r\nbody\r\n"

	meta, err := mail.Describe([]byte(reply))
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if meta.ThreadKey != "root@example.com" {
		t.Errorf("ThreadKey = %q, want the In-Reply-To target", meta.ThreadKey)
	}
}

func TestNewMessageIDIsUniqueAndHosted(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 500; i++ {
		id := mail.NewMessageID("Example.com")
		if seen[id] {
			t.Fatalf("NewMessageID returned a duplicate: %s", id)
		}
		seen[id] = true
		if got := id[len(id)-len("@example.com"):]; got != "@example.com" {
			t.Fatalf("NewMessageID = %s, want it hosted on the sender's domain", id)
		}
	}
}

func TestNewMessageIDRejectsAnUnusableDomain(t *testing.T) {
	// A host that would break the header cannot go into a Message-ID.
	id := mail.NewMessageID("bad domain@with spaces")
	if got := id[len(id)-len("@localhost"):]; got != "@localhost" {
		t.Errorf("NewMessageID = %s, want a safe fallback host", id)
	}
}

func TestParseAddressListDecodesEncodedWords(t *testing.T) {
	got, err := mail.ParseAddressList("=?utf-8?q?Andr=C3=A9?= <andre@example.com>, plain@example.com")
	if err != nil {
		t.Fatalf("ParseAddressList: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d addresses, want 2: %+v", len(got), got)
	}
	if got[0].Name != "André" {
		t.Errorf("Name = %q, want the RFC 2047 word decoded", got[0].Name)
	}
	if got[1].Address != "plain@example.com" {
		t.Errorf("Address = %q", got[1].Address)
	}
}

func TestParseAddressListEmptyIsNotAnError(t *testing.T) {
	for _, in := range []string{"", "   ", "\t"} {
		got, err := mail.ParseAddressList(in)
		if err != nil {
			t.Errorf("ParseAddressList(%q) error = %v, want none", in, err)
		}
		if len(got) != 0 {
			t.Errorf("ParseAddressList(%q) = %+v, want none", in, got)
		}
	}
}

func TestParseAddressListRejectsGarbage(t *testing.T) {
	// A present but broken header must be reported, not silently emptied: the
	// caller would otherwise deliver to the wrong set of recipients.
	if _, err := mail.ParseAddressList("not an address <<"); err == nil {
		t.Error("ParseAddressList accepted a malformed list, want an error")
	}
}

func TestFormatAddressListRoundTrips(t *testing.T) {
	in := []mail.Address{
		{Name: "Alice Example", Address: "alice@example.com"},
		{Name: `Doe, Jane "JJ"`, Address: "jane@example.com"},
	}
	out, err := mail.ParseAddressList(mail.FormatAddressList(in))
	if err != nil {
		t.Fatalf("ParseAddressList: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("round trip gave %d addresses: %+v", len(out), out)
	}
	if out[0].Address != in[0].Address || out[1].Address != in[1].Address {
		t.Errorf("round trip lost addresses: %+v", out)
	}
}

func TestDecodeTextLeavesPlainTextAlone(t *testing.T) {
	if got := mail.DecodeText("no encoded words here"); got != "no encoded words here" {
		t.Errorf("DecodeText = %q, want it unchanged", got)
	}
}

func TestAddressesOfLowercases(t *testing.T) {
	got := mail.AddressesOf([]mail.Address{{Address: "Alice@Example.COM"}, {Name: "no address"}})
	if len(got) != 1 || got[0] != "alice@example.com" {
		t.Errorf("AddressesOf = %v, want one lowercased address", got)
	}
}
