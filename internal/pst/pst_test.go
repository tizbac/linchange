package pst

import (
	"bytes"
	"encoding/binary"
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseBlockIndex(t *testing.T) {
	data := make([]byte, 64)
	binary.LittleEndian.PutUint16(data[0:2], 20) // index at 20
	binary.LittleEndian.PutUint16(data[20:22], 2)
	binary.LittleEndian.PutUint16(data[22:24], 0)
	binary.LittleEndian.PutUint16(data[24:26], 4)
	binary.LittleEndian.PutUint16(data[26:28], 12)
	binary.LittleEndian.PutUint16(data[28:30], 28)

	values := parseBlockIndex(data)
	if len(values) != 2 {
		t.Fatalf("got %d values, want 2", len(values))
	}
	if values[0] != (indexValue{start: 4, end: 12}) {
		t.Errorf("value 0 = %+v", values[0])
	}
	if values[1] != (indexValue{start: 12, end: 28}) {
		t.Errorf("value 1 = %+v", values[1])
	}
}

func TestTableValueAndRecordEntries(t *testing.T) {
	// A tiny bc table:
	//   0..4   index offset, signature, type, value reference
	//   12..20 the b5 header
	//   20..40 two record entries
	//   40..   the block index
	data := make([]byte, 52)
	binary.LittleEndian.PutUint16(data[0:2], 40)
	data[2] = 0xec
	data[3] = 0xbc
	binary.LittleEndian.PutUint32(data[4:8], 0x20) // index item 1: the b5 header

	data[12] = 0xb5
	data[13] = 2
	data[14] = 6
	data[15] = 0
	binary.LittleEndian.PutUint32(data[16:20], 0x40) // index item 2: record entries

	binary.LittleEndian.PutUint16(data[20:22], 0x0037) // subject
	binary.LittleEndian.PutUint16(data[22:24], 0x0003) // integer
	binary.LittleEndian.PutUint32(data[24:28], 7)
	binary.LittleEndian.PutUint16(data[28:30], 0x001a) // message class
	binary.LittleEndian.PutUint16(data[30:32], 0x000b) // boolean
	binary.LittleEndian.PutUint32(data[32:36], 1)

	binary.LittleEndian.PutUint16(data[40:42], 2)
	binary.LittleEndian.PutUint16(data[42:44], 0)
	binary.LittleEndian.PutUint16(data[44:46], 12)
	binary.LittleEndian.PutUint16(data[46:48], 20)
	binary.LittleEndian.PutUint16(data[48:50], 40)

	blocks := []block{{data: data, index: parseBlockIndex(data)}}
	if got, ok := tableValue(blocks, 0x20); !ok || len(got) != 8 {
		t.Fatalf("b5 value = %v, %v", got, ok)
	}
	entries, err := recordEntries(blocks, 0x40, 0, 2, 6, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[0].identifier != 0x0037 || entries[0].valueType != 0x0003 || entries[0].raw != 7 {
		t.Errorf("entry 0 = %+v", entries[0])
	}
	if entries[1].identifier != 0x001a || entries[1].valueType != 0x000b || entries[1].raw != 1 {
		t.Errorf("entry 1 = %+v", entries[1])
	}
}

func TestSubjectKeepsTheTextAndDropsTheFlags(t *testing.T) {
	if got := subjectOf("\x01\x02Review"); got != "Review" {
		t.Errorf("subjectOf = %q", got)
	}
}

func TestMessageMIME(t *testing.T) {
	item := Item{
		ID:   42,
		Kind: "message",
		Properties: map[uint32]Property{
			propSubject:       {Text: "\x01Hello"},
			propBody:          {Text: "Line one\nLine two\n"},
			propSenderName:    {Text: "Alice"},
			propSenderAddress: {Text: "alice@example.com"},
			propDisplayTo:     {Text: "bob@example.com"},
			propSubmitTime:    {When: time.Date(2024, 3, 1, 9, 30, 0, 0, time.UTC)},
		},
	}
	raw, err := (&File{}).MIME(item)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{
		"From: \"Alice\" <alice@example.com>",
		"To: bob@example.com",
		"Subject: Hello",
		"Date: Fri, 01 Mar 2024 09:30:00 +0000",
		"Content-Type: text/plain; charset=utf-8",
		"Line one",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("message does not contain %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "\x01") {
		t.Error("the subject flag control character survived")
	}
}

func TestVCard(t *testing.T) {
	card := Contact{
		UID:         "pst-1@linchange.import",
		DisplayName: "Doe, Jane",
		GivenName:   "Jane",
		FamilyName:  "Doe",
		Emails:      []string{"jane@example.com"},
		Phones:      []string{"+1 555 0100"},
		Notes:       "line one\nline two",
	}
	raw := string(card.VCard())
	for _, want := range []string{
		"BEGIN:VCARD",
		"FN:Doe\\, Jane",
		"N:Doe;Jane;;;",
		"EMAIL;TYPE=INTERNET:jane@example.com",
		"TEL;TYPE=VOICE:+1 555 0100",
		"NOTE:line one\\nline two",
		"END:VCARD",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("vCard does not contain %q:\n%s", want, raw)
		}
	}
	if !bytes.HasSuffix([]byte(raw), []byte("END:VCARD\r\n")) {
		t.Error("vCard does not end with CRLF")
	}
}

func TestAppointmentFallsBackToFixedProperties(t *testing.T) {
	start := time.Date(2024, 3, 1, 9, 0, 0, 0, time.UTC)
	item := Item{
		ID:   7,
		Kind: "appointment",
		Properties: map[uint32]Property{
			propSubject:   {Text: "Standup"},
			propStartDate: {When: start},
			propEndDate:   {When: start.Add(30 * time.Minute)},
			propLocation:  {Text: "Room 1"},
		},
	}
	event := (&File{}).Appointment(item)
	if event.Summary != "Standup" || event.Location != "Room 1" {
		t.Errorf("event = %+v", event)
	}
	if !event.Start.Equal(start) || !event.End.Equal(start.Add(30*time.Minute)) {
		t.Errorf("times = %v - %v", event.Start, event.End)
	}
	doc := string(event.Marshal())
	if !strings.Contains(doc, "DTSTART:20240301T090000Z") {
		t.Errorf("iCalendar = %s", doc)
	}
}

// TestRealFile reads whatever PST is named in PST_SAMPLE. It is the harness the
// reader was validated with against Outlook-generated archives; the fixtures
// are not committed because they are third-party mailboxes.
func TestRealFile(t *testing.T) {
	path := os.Getenv("PST_SAMPLE")
	if path == "" {
		t.Skip("set PST_SAMPLE to a .pst or .ost file to run this")
	}
	f, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	items := f.Items()
	if len(items) == 0 {
		t.Fatal("no items")
	}
	counts := map[string]int{}
	for _, item := range items {
		counts[item.Kind]++
		if item.Kind == "message" {
			raw, err := f.MIME(item)
			if err != nil {
				t.Fatalf("message %d: %v", item.ID, err)
			}
			if len(raw) == 0 {
				t.Fatalf("message %d produced no MIME", item.ID)
			}
		}
		if item.Kind == "appointment" {
			event := f.Appointment(item)
			if event.Start.IsZero() {
				t.Errorf("appointment %d has no start", item.ID)
			}
			if len(event.Marshal()) == 0 {
				t.Errorf("appointment %d produced no iCalendar", item.ID)
			}
		}
		if item.Kind == "contact" {
			if len(f.ContactCard(item).VCard()) == 0 {
				t.Errorf("contact %d produced no vCard", item.ID)
			}
		}
	}
	if counts["folder"] == 0 {
		t.Error("no folders were found")
	}
	t.Logf("%s: %s, %d items, %v", path, f.ContentType(), len(items), counts)
}
