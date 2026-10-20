package meetings

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestNormaliseTitleStripsHeaderBreaks pins the reason the strip exists: the
// title becomes the Subject header, and a title carrying a line break is a
// header the organiser did not write.
func TestNormaliseTitleStripsHeaderBreaks(t *testing.T) {
	got, err := normaliseTitle("  Review\r\nBcc: victim@evil.example\t\tnow  ")
	if err != nil {
		t.Fatalf("normaliseTitle: %v", err)
	}
	want := "Review Bcc: victim@evil.example now"
	if got != want {
		t.Errorf("normaliseTitle = %q, want %q", got, want)
	}

	// A title that is only breaks and spaces has no title left in it.
	if _, err := normaliseTitle(" \r\n\t "); !errors.Is(err, ErrInvalid) {
		t.Errorf("blank title error = %v, want ErrInvalid", err)
	}
	if _, err := normaliseTitle(""); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty title error = %v, want ErrInvalid", err)
	}
	if _, err := normaliseTitle(strings.Repeat("x", maxTitle+1)); !errors.Is(err, ErrInvalid) {
		t.Errorf("over-long title error = %v, want ErrInvalid", err)
	}
	// The cap counts characters: a title in another script is not four times
	// the meeting it reads as.
	if _, err := normaliseTitle(strings.Repeat("会", maxTitle)); err != nil {
		t.Errorf("%d-character non-ASCII title rejected: %v", maxTitle, err)
	}
}

func TestNormaliseAttendees(t *testing.T) {
	organizer := "alice@example.com"
	in := []Attendee{
		{Address: "Bob <bob@example.com>"},
		{Address: "BOB@example.com"},   // same mailbox, different spelling
		{Address: "Alice@example.com"}, // the organiser does not invite themselves
		{Address: "Carol <carol@outside.example>", Name: "Carol Outside"},
	}
	got, err := normaliseAttendees(in, organizer)
	if err != nil {
		t.Fatalf("normaliseAttendees: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("guests = %d (%v), want 2", len(got), got)
	}
	if got[0].Address != "bob@example.com" || got[0].Name != "Bob" {
		t.Errorf("first guest = %+v, want bob@example.com named Bob", got[0])
	}
	if got[1].Address != "carol@outside.example" || got[1].Name != "Carol Outside" {
		t.Errorf("second guest = %+v, want carol outside with the name given", got[1])
	}

	if _, err := normaliseAttendees(nil, organizer); !errors.Is(err, ErrInvalid) {
		t.Errorf("no guests: %v, want ErrInvalid", err)
	}
	if _, err := normaliseAttendees([]Attendee{{Address: "not an address"}}, organizer); !errors.Is(err, ErrInvalid) {
		t.Errorf("unparsable address: %v, want ErrInvalid", err)
	}
	if _, err := normaliseAttendees([]Attendee{{Address: organizer}}, organizer); !errors.Is(err, ErrInvalid) {
		t.Errorf("organiser only: %v, want ErrInvalid", err)
	}
}

func TestValidAnswer(t *testing.T) {
	for _, ok := range []string{"ACCEPTED", "DECLINED", "TENTATIVE", "NEEDS-ACTION"} {
		if !validAnswer(ok) {
			t.Errorf("validAnswer(%q) = false", ok)
		}
	}
	for _, bad := range []string{"accepted", "MAYBE", "", "DELEGATED", "ACCEPTED;x"} {
		if validAnswer(bad) {
			t.Errorf("validAnswer(%q) = true", bad)
		}
	}
}

// TestFormatWhen keeps the zone in the rendering. "14:00" with no zone is a
// promise no attendee can keep, so every form of this line ends with one.
func TestFormatWhen(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("no time zone database: %v", err)
	}

	// 21 October 2026 is a Wednesday.
	start := time.Date(2026, time.October, 21, 14, 0, 0, 0, berlin)
	end := start.Add(time.Hour)

	sameDay := formatWhen(start, end, "Europe/Berlin")
	wantSame := "Wed, 21 Oct 2026 14:00 - 15:00 Europe/Berlin"
	if sameDay != wantSame {
		t.Errorf("same day = %q, want %q", sameDay, wantSame)
	}

	// A meeting that crosses midnight shows both dates: one date would send
	// somebody to the wrong day of the week.
	overnightEnd := time.Date(2026, time.October, 22, 1, 0, 0, 0, berlin)
	overnight := formatWhen(start, overnightEnd, "Europe/Berlin")
	wantOvernight := "Wed, 21 Oct 2026 14:00 - Thu, 22 Oct 2026 01:00 Europe/Berlin"
	if overnight != wantOvernight {
		t.Errorf("overnight = %q, want %q", overnight, wantOvernight)
	}

	// A zone that cannot be loaded falls back to UTC and says so, rather than
	// printing times in a zone it is not rendering in.
	unknown := formatWhen(start, end, "Mars/Phobos")
	if !strings.HasSuffix(unknown, "UTC") {
		t.Errorf("unknown zone = %q, want a UTC rendering", unknown)
	}
}

// TestIdentifiers checks the three identifiers this package mints against the
// properties they exist for: unguessability, uniqueness, and OpenIM's rules.
func TestIdentifiers(t *testing.T) {
	uid, err := newUID()
	if err != nil {
		t.Fatalf("newUID: %v", err)
	}
	if len(uid) != len("0123456789abcdef0123456789abcdef@linchange") {
		t.Errorf("uid = %q, want 32 hex digits and the @linchange suffix", uid)
	}
	if uid2, err := newUID(); err != nil || uid2 == uid {
		t.Errorf("newUID twice = %q, %q (err %v), want different values", uid, uid2, err)
	}

	token, err := randomToken()
	if err != nil {
		t.Fatalf("randomToken: %v", err)
	}
	if len(token) != 32 {
		t.Errorf("token = %d characters, want 32 hex digits (128 bits)", len(token))
	}

	// The room id must be derivable from the UID alone - that is what makes
	// it stable across a resend - and must satisfy OpenIM's alphabet.
	group := imGroupIDFor(uid)
	if !strings.HasPrefix(group, "mtg_") {
		t.Errorf("room id = %q, want an mtg_ prefix", group)
	}
	if group != imGroupIDFor(uid) {
		t.Error("room id is not stable for one meeting")
	}
	if group == imGroupIDFor(uid+"x") {
		t.Error("two meetings share a room id")
	}
}
