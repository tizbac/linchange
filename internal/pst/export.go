package pst

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"sort"
	"strings"
	"time"

	"github.com/linchange/linchange/internal/ical"
)

// Text returns a text property, or the empty string.
func (it Item) Text(identifier uint32) string {
	return it.Properties[identifier].Text
}

// Time returns a time property, or the zero time.
func (it Item) Time(identifier uint32) time.Time {
	return it.Properties[identifier].When
}

// Int returns an integer property.
func (it Item) Int(identifier uint32) int64 {
	return it.Properties[identifier].Int
}

// Unread reports whether the MAPI message flags leave the item unread. Bit 0
// of PR_MESSAGE_FLAGS is MSGFLAG_READ.
func (it Item) Unread() bool {
	return it.Int(propMessageFlags)&1 == 0
}

// named returns the property whose named LID matches, within the property set
// GUID. Outlook moves these between file-local identifiers, so the LID is the
// only stable handle.
func (p *File) named(item Item, guid string, lid uint32) (Property, bool) {
	for identifier, prop := range item.Properties {
		named, ok := p.namedProperty(identifier)
		if !ok {
			continue
		}
		if named.LID == lid && named.Name == "" && guidString(named.GUID) == guid {
			return prop, true
		}
	}
	return Property{}, false
}

func (p *File) namedText(item Item, guid string, lid uint32) string {
	if prop, ok := p.named(item, guid, lid); ok {
		return prop.Text
	}
	return ""
}

func (p *File) namedTime(item Item, guid string, lid uint32) time.Time {
	if prop, ok := p.named(item, guid, lid); ok {
		return prop.When
	}
	return time.Time{}
}

func (p *File) namedInt(item Item, guid string, lid uint32) (int64, bool) {
	if prop, ok := p.named(item, guid, lid); ok {
		return prop.Int, true
	}
	return 0, false
}

// subjectOf strips the two control characters Outlook puts in front of a
// subject to carry the message importance and flags. They are not part of the
// subject and look like mojibake in every other client.
func subjectOf(subject string) string {
	return strings.TrimLeft(subject, "\x01\x02\x03\x04")
}

// senderOf picks the best available From address. Outlook stores an Exchange
// DN in PR_SENDER_EMAIL_ADDRESS for internal mail; PR_SENDER_SMTP_ADDRESS and
// the transport headers carry the routable address, and a DN in a From header
// is not something another mail system can reply to.
func (p *File) senderOf(item Item) mail.Address {
	name := item.Text(propSenderName)
	if name == "" {
		name = item.Text(0x0042)
	}
	for _, candidate := range []string{
		item.Text(propSenderAddress),
		item.Text(0x5d01), // PidTagSenderSmtpAddress
		item.Text(0x5d02), // PidTagSentRepresentingSmtpAddress
		item.Text(0x0065), // PidTagSentRepresentingEmailAddress
	} {
		candidate = strings.TrimSpace(candidate)
		if strings.Contains(candidate, "@") {
			return mail.Address{Name: name, Address: candidate}
		}
	}
	if value := headerValue(item.Text(propTransportHeader), "From"); value != "" {
		if addr, err := mail.ParseAddress(value); err == nil {
			if name == "" {
				name = addr.Name
			}
			return mail.Address{Name: name, Address: addr.Address}
		}
	}
	return mail.Address{Name: name, Address: item.Text(propSenderAddress)}
}

// headerValue reads one header out of a message's transport header block. The
// block is a plain "Name: value" list with CRLF line endings.
func headerValue(headers, name string) string {
	for _, line := range strings.Split(strings.ReplaceAll(headers, "\r\n", "\n"), "\n") {
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(line[:colon]), name) {
			return strings.TrimSpace(line[colon+1:])
		}
	}
	return ""
}

// cleanAddressList drops the single quotes Outlook sometimes wraps a display
// address list in. They are not part of an address and break RFC 5322 parsers.
func cleanAddressList(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
		value = value[1 : len(value)-1]
	}
	return value
}

// MIME renders a message item as an RFC 5322 message, so the import path can
// reuse the same storage, threading and search as mail that arrived over SMTP.
func (p *File) MIME(item Item) ([]byte, error) {
	subject := subjectOf(item.Text(propSubject))
	from := p.senderOf(item)

	to := item.Text(propDisplayTo)
	cc := item.Text(propDisplayCc)
	bcc := item.Text(propDisplayBcc)
	transportHeaders := item.Text(propTransportHeader)
	if to != "" && !strings.Contains(to, "@") {
		if value := headerValue(transportHeaders, "To"); value != "" {
			to = value
		}
	}
	if cc != "" && !strings.Contains(cc, "@") {
		if value := headerValue(transportHeaders, "Cc"); value != "" {
			cc = value
		}
	}
	to = cleanAddressList(to)
	cc = cleanAddressList(cc)
	bcc = cleanAddressList(bcc)
	date := item.Time(propSubmitTime)
	if date.IsZero() {
		date = item.Time(propDeliveryTime)
	}
	if date.IsZero() {
		date = time.Now()
	}

	messageID := strings.TrimSpace(item.Text(propInternetMessageID))
	if messageID == "" {
		messageID = fmt.Sprintf("<pst-%d@linchange.import>", item.ID)
	}
	messageID = strings.Trim(messageID, "<>")

	body := item.Text(propBody)
	html := item.Text(propBodyHTML)

	// Build the body part: HTML with a plain text alternative when both are
	// present, a single text part otherwise.
	var bodyBytes []byte
	var bodyType string
	switch {
	case html != "" && body != "":
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		if err := writeTextPart(mw, "text/plain; charset=utf-8", []byte(body)); err != nil {
			return nil, err
		}
		if err := writeTextPart(mw, "text/html; charset=utf-8", []byte(html)); err != nil {
			return nil, err
		}
		if err := mw.Close(); err != nil {
			return nil, err
		}
		bodyBytes = buf.Bytes()
		bodyType = "multipart/alternative; boundary=\"" + mw.Boundary() + "\""
	case html != "":
		bodyBytes = quotedPrintable(html)
		bodyType = "text/html; charset=utf-8"
	default:
		bodyBytes = quotedPrintable(body)
		bodyType = "text/plain; charset=utf-8"
	}

	// Attachments turn the message into multipart/mixed with the body as the
	// first part.
	var payload []byte
	contentType := bodyType
	transferEncoding := ""
	if len(item.Attachments) > 0 {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		header := make(map[string][]string)
		header["Content-Type"] = []string{bodyType}
		if strings.HasPrefix(bodyType, "text/") {
			header["Content-Transfer-Encoding"] = []string{"quoted-printable"}
		}
		part, err := mw.CreatePart(header)
		if err != nil {
			return nil, err
		}
		if _, err := part.Write(bodyBytes); err != nil {
			return nil, err
		}
		for _, attachment := range item.Attachments {
			name := attachment.Name
			if name == "" {
				name = "attachment"
			}
			header := make(map[string][]string)
			header["Content-Type"] = []string{mime.FormatMediaType("application/octet-stream",
				map[string]string{"name": name})}
			header["Content-Disposition"] = []string{mime.FormatMediaType("attachment",
				map[string]string{"filename": name})}
			header["Content-Transfer-Encoding"] = []string{"base64"}
			part, err := mw.CreatePart(header)
			if err != nil {
				return nil, err
			}
			encoder := base64.NewEncoder(base64.StdEncoding, &breakWriter{w: part})
			if _, err := encoder.Write(attachment.Data); err != nil {
				return nil, err
			}
			if err := encoder.Close(); err != nil {
				return nil, err
			}
		}
		if err := mw.Close(); err != nil {
			return nil, err
		}
		payload = buf.Bytes()
		contentType = "multipart/mixed; boundary=\"" + mw.Boundary() + "\""
	} else {
		payload = bodyBytes
		if strings.HasPrefix(bodyType, "text/") {
			transferEncoding = "quoted-printable"
		}
	}

	var out bytes.Buffer
	writeHeader := func(key, value string) {
		value = strings.ReplaceAll(value, "\r", " ")
		value = strings.ReplaceAll(value, "\n", " ")
		if value != "" {
			fmt.Fprintf(&out, "%s: %s\r\n", key, value)
		}
	}
	writeHeader("MIME-Version", "1.0")
	writeHeader("Date", date.Format(time.RFC1123Z))
	writeHeader("From", from.String())
	writeHeader("To", to)
	writeHeader("Cc", cc)
	writeHeader("Bcc", bcc)
	writeHeader("Subject", encodeHeader(subject))
	writeHeader("Message-ID", "<"+messageID+">")
	if refs := strings.TrimSpace(item.Text(propInternetReferences)); refs != "" {
		writeHeader("References", refs)
	}
	writeHeader("Content-Type", contentType)
	writeHeader("Content-Transfer-Encoding", transferEncoding)
	out.WriteString("\r\n")
	out.Write(payload)
	return out.Bytes(), nil
}

func writeTextPart(mw *multipart.Writer, contentType string, text []byte) error {
	header := make(map[string][]string)
	header["Content-Type"] = []string{contentType}
	header["Content-Transfer-Encoding"] = []string{"quoted-printable"}
	part, err := mw.CreatePart(header)
	if err != nil {
		return err
	}
	encoder := quotedprintable.NewWriter(part)
	if _, err := encoder.Write(text); err != nil {
		return err
	}
	return encoder.Close()
}

func quotedPrintable(text string) []byte {
	var buf bytes.Buffer
	w := quotedprintable.NewWriter(&buf)
	_, _ = w.Write([]byte(text))
	_ = w.Close()
	return buf.Bytes()
}

// encodeHeader RFC 2047-encodes a header value when it is not plain ASCII.
func encodeHeader(value string) string {
	ascii := true
	for i := 0; i < len(value); i++ {
		if value[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return value
	}
	return mime.QEncoding.Encode("utf-8", value)
}

// breakWriter wraps base64 at 76 columns, which is what a MIME reader expects
// and what keeps a large attachment readable with line tools.
type breakWriter struct {
	w   interface{ Write([]byte) (int, error) }
	col int
}

func (b *breakWriter) Write(data []byte) (int, error) {
	written := 0
	for len(data) > 0 {
		n := 76 - b.col
		if n > len(data) {
			n = len(data)
		}
		if _, err := b.w.Write(data[:n]); err != nil {
			return written, err
		}
		written += n
		data = data[n:]
		b.col += n
		if b.col == 76 {
			if _, err := b.w.Write([]byte("\r\n")); err != nil {
				return written, err
			}
			b.col = 0
		}
	}
	return written, nil
}

// Appointment renders a calendar item as an iCalendar event. Recurrence is not
// expanded: the first occurrence is imported and the series is imported as a
// single event, which is what a reader that does not understand the recurrence
// blob can honestly represent.
func (p *File) Appointment(item Item) ical.Event {
	start := p.namedTime(item, guidAppointment, lidAppointmentStartWhole)
	end := p.namedTime(item, guidAppointment, lidAppointmentEndWhole)
	if start.IsZero() {
		start = item.Time(propStartDate)
	}
	if end.IsZero() {
		end = item.Time(propEndDate)
	}
	allDayValue, _ := p.namedInt(item, guidAppointment, lidAppointmentSubType)
	allDay := allDayValue != 0

	location := p.namedText(item, guidAppointment, lidLocation)
	if location == "" {
		location = item.Text(propLocation)
	}

	uid := ""
	if prop, ok := p.named(item, guidMeeting, lidGlobalObjectID); ok && len(prop.Data) >= 16 {
		uid = fmt.Sprintf("pst-%d-%x", item.ID, prop.Data)
	} else {
		uid = fmt.Sprintf("pst-%d@linchange.import", item.ID)
	}

	event := ical.Event{
		UID:         uid,
		Summary:     subjectOf(item.Text(propSubject)),
		Description: item.Text(propBody),
		Location:    location,
		Start:       start,
		End:         end,
		AllDay:      allDay,
		Status:      ical.StatusConfirmed,
		ShowAs:      ical.ShowAsBusy,
		DTStamp:     time.Now().UTC(),
	}
	if event.End.IsZero() && !event.Start.IsZero() {
		event.End = event.Start.Add(time.Hour)
	}
	return event
}

// Contact is the subset of a MAPI contact a vCard can carry.
type Contact struct {
	UID          string
	DisplayName  string
	GivenName    string
	FamilyName   string
	MiddleName   string
	Organization string
	Title        string
	Notes        string
	Emails       []string
	Phones       []string
	WorkAddress  Address
	HomeAddress  Address
}

// Address is a postal address.
type Address struct {
	Street     string
	City       string
	Region     string
	PostalCode string
	Country    string
}

// ContactCard reads a contact item into a Contact.
func (p *File) ContactCard(item Item) Contact {
	card := Contact{
		UID:          fmt.Sprintf("pst-%d@linchange.import", item.ID),
		DisplayName:  item.Text(propDisplayName),
		GivenName:    item.Text(propGivenName),
		FamilyName:   item.Text(propSurname),
		MiddleName:   item.Text(propMiddleName),
		Organization: item.Text(propOrganization),
		Title:        item.Text(propJobTitle),
		Notes:        item.Text(propNotes),
	}
	if card.Notes == "" {
		card.Notes = item.Text(propBody)
	}
	card.WorkAddress = Address{
		Street:     firstNonEmpty(p.namedText(item, guidAddress, lidWorkAddressStreet), item.Text(propStreetAddress)),
		City:       firstNonEmpty(p.namedText(item, guidAddress, lidWorkAddressCity), item.Text(propLocality)),
		Region:     firstNonEmpty(p.namedText(item, guidAddress, lidWorkAddressState), item.Text(propRegion)),
		PostalCode: firstNonEmpty(p.namedText(item, guidAddress, lidWorkAddressPostalCode), item.Text(propPostalCode)),
		Country:    firstNonEmpty(p.namedText(item, guidAddress, lidWorkAddressCountry), item.Text(propCountry)),
	}
	card.HomeAddress = Address{
		Street:     item.Text(propHomeStreet),
		City:       item.Text(propHomeCity),
		Region:     item.Text(propHomeRegion),
		PostalCode: item.Text(propHomePostalCode),
		Country:    item.Text(propHomeCountry),
	}
	if card.DisplayName == "" {
		card.DisplayName = strings.TrimSpace(card.GivenName + " " + card.FamilyName)
	}

	addEmail := func(address string) {
		address = strings.TrimSpace(address)
		if address == "" || !strings.Contains(address, "@") {
			return
		}
		for _, existing := range card.Emails {
			if strings.EqualFold(existing, address) {
				return
			}
		}
		card.Emails = append(card.Emails, address)
	}
	addEmail(item.Text(propEmailAddress))
	for _, lid := range []uint32{lidEmail1Address, lidEmail2Address, lidEmail3Address} {
		addEmail(p.namedText(item, guidAddress, lid))
	}
	// Named property identifiers are file-local; when the map did not resolve
	// them, an address that looks like an address is still worth keeping.
	if len(card.Emails) == 0 {
		for identifier, prop := range item.Properties {
			if identifier >= 0x8000 && strings.Contains(prop.Text, "@") {
				addEmail(prop.Text)
			}
		}
	}

	addPhone := func(number string) {
		number = strings.TrimSpace(number)
		digits := 0
		for _, r := range number {
			if r >= '0' && r <= '9' {
				digits++
			}
		}
		if number != "" && digits >= 3 {
			card.Phones = append(card.Phones, number)
		}
	}
	addPhone(item.Text(propBusinessPhone))
	addPhone(item.Text(propHomePhone))
	addPhone(item.Text(propMobilePhone))
	addPhone(item.Text(0x3a1a)) // primary telephone
	return card
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// VCard renders a contact as a version 3.0 vCard, the format the rest of the
// server already speaks.
func (c Contact) VCard() []byte {
	var out bytes.Buffer
	line := func(value string) {
		out.WriteString(foldVCard(value))
		out.WriteString("\r\n")
	}
	line("BEGIN:VCARD")
	line("VERSION:3.0")
	line("UID:" + escapeVCard(c.UID))
	line("FN:" + escapeVCard(c.DisplayName))
	line("N:" + escapeVCard(c.FamilyName) + ";" + escapeVCard(c.GivenName) + ";" +
		escapeVCard(c.MiddleName) + ";;")
	if c.Organization != "" {
		line("ORG:" + escapeVCard(c.Organization))
	}
	if c.Title != "" {
		line("TITLE:" + escapeVCard(c.Title))
	}
	for _, address := range c.Emails {
		line("EMAIL;TYPE=INTERNET:" + escapeVCard(address))
	}
	for _, phone := range c.Phones {
		line("TEL;TYPE=VOICE:" + escapeVCard(phone))
	}
	if c.WorkAddress != (Address{}) {
		line("ADR;TYPE=WORK:" + vcardAddress(c.WorkAddress))
	}
	if c.HomeAddress != (Address{}) {
		line("ADR;TYPE=HOME:" + vcardAddress(c.HomeAddress))
	}
	if c.Notes != "" {
		line("NOTE:" + escapeVCard(c.Notes))
	}
	line("END:VCARD")
	return out.Bytes()
}

func vcardAddress(a Address) string {
	return ";;" + escapeVCard(a.Street) + ";" + escapeVCard(a.City) + ";" +
		escapeVCard(a.Region) + ";" + escapeVCard(a.PostalCode) + ";" +
		escapeVCard(a.Country)
}

func escapeVCard(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, ";", "\\;")
	s = strings.ReplaceAll(s, ",", "\\,")
	s = strings.ReplaceAll(s, "\r\n", "\\n")
	s = strings.ReplaceAll(s, "\n", "\\n")
	return s
}

// foldVCard folds a line at 75 octets, continuing with a space.
func foldVCard(line string) string {
	if len(line) <= 75 {
		return line
	}
	var out strings.Builder
	for len(line) > 75 {
		out.WriteString(line[:75])
		out.WriteString("\r\n ")
		line = line[75:]
	}
	out.WriteString(line)
	return out.String()
}

// FolderPath returns the display path of a folder item, ending with its name.
func (p *File) FolderPath(items []Item, id uint32) string {
	byID := make(map[uint32]Item, len(items))
	for _, item := range items {
		byID[item.ID] = item
	}
	var parts []string
	seen := map[uint32]bool{}
	current := id
	for current != 0 && !seen[current] {
		seen[current] = true
		item, ok := byID[current]
		if !ok {
			break
		}
		if item.Name != "" {
			parts = append(parts, item.Name)
		}
		current = item.ParentID
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return strings.Join(parts, "/")
}

// SortItems orders items the way a directory listing reads: folders first,
// then by name.
func SortItems(items []Item) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Kind != items[j].Kind {
			return items[i].Kind == "folder" || items[i].Kind == "search folder"
		}
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})
}
