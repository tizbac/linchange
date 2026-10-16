// WBXML (Wireless Binary XML) 1.3 codec for Exchange ActiveSync, per
// [MS-ASWBXML] and [WBXML1.2].
//
// ActiveSync speaks a fixed, small vocabulary of elements. Rather than carrying
// an XML document and then serialising it, a client encodes the document as a
// stream of single-byte tag tokens, one per element, in a code page that maps
// those tokens to element names. The result is several times smaller than the
// same document as text, which is the whole reason the protocol is binary: a
// phone synchronising a folder of mail should not be shipping kilobytes of
// angle brackets.
//
// This file is the codec both directions share. The writer turns an element
// tree the handlers build into a token stream; the decoder turns a client's
// token stream back into a tree the handlers read. Three details of that stream
// are the contract with a real client and are easy to get wrong:
//
//   - The header is four bytes: version (0x03), public identifier, charset
//     (0x6A = UTF-8) and string table length, the last three as multi-byte
//     integers. Microsoft's own client writes public identifier 0x01.
//   - Bit 6 of a tag token (0x40) says the element has content. An element
//     without it is empty and is finished on the spot; an element with it is
//     closed by an END token (0x01). A decoder that ignores the bit cannot
//     place children, and a writer that never sets it sends documents no
//     client can read.
//   - Code pages are switched in the stream with SWITCH_PAGE (0x00), and the
//     tokens are the published assignments in [MS-ASWBXML], not a private
//     dialect.
//
// The tag tables below follow the published code pages, and they are the
// contract with a real client, so they are not to be edited for convenience:
// if a handler needs an element the specification does not have, the handler
// is what has to change.

package eas

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// --- Token values ([MS-ASWBXML] §2.2) -------------------------------------

const (
	wbxmlVersion  = 0x03 // WBXML 1.3
	wbxmlPublicID = 0x01 // the public identifier Microsoft's client writes
	wbxmlCharset  = 0x6A // 106 = UTF-8
	wbxmlStrTable = 0x00 // no string table

	tokenSwitchPage = 0x00 // followed by a code page number
	tokenEnd        = 0x01 // end of an element that has content
	tokenEntity     = 0x02 // entity reference, followed by an MBUInt
	tokenStrI       = 0x03 // inline string, followed by UTF-8 and a 0x00
	tokenLiteral    = 0x04 // inline literal (unused by ActiveSync)
	tokenExtI0      = 0x40 // extended inline string, one of three
	tokenExtI1      = 0x41
	tokenExtI2      = 0x42
	tokenPI         = 0x43 // processing instruction (unused by ActiveSync)
	tokenLiteralC   = 0x44 // literal with content (unused by ActiveSync)
	tokenStrT       = 0x83 // string table reference (unused by ActiveSync)
	tokenOpaque     = 0xC3 // 1.3 opaque data, followed by an MBUInt32 length

	// The two attribute bits of a tag token. Bit 6 says the element has
	// content; bit 7 says it has attributes, which ActiveSync never uses.
	tokenHasContent    = 0x40
	tokenHasAttributes = 0x80
	// The element name itself is the low six bits.
	tokenNameMask = 0x3F
)

// --- Code pages ----------------------------------------------------------
//
// A code page is a namespace: a set of element names bound to token values. A
// client names an element by first switching to the code page the element lives
// in and then writing the element's token. The numbers are the published
// ActiveSync assignments; a client that sends a different number for the same
// element would be speaking a different protocol.

const (
	cpAirSync         = 0
	cpContacts        = 1
	cpEmail           = 2
	cpCalendar        = 4
	cpMove            = 5
	cpGetItemEstimate = 6
	cpFolderHierarchy = 7
	cpMeetingResponse = 8
	cpResolveRecip    = 10
	cpPing            = 13
	cpProvision       = 14
	cpSearch          = 15
	cpAirSyncBase     = 17
	cpSettings        = 18
	cpItemOperations  = 20
	cpComposeMail     = 21
)

// nsByCP names each code page, so the writer can decide when to switch.
var nsByCP = map[byte]string{
	cpAirSync:         "AirSync",
	cpContacts:        "Contacts",
	cpEmail:           "Email",
	cpCalendar:        "Calendar",
	cpMove:            "Move",
	cpGetItemEstimate: "GetItemEstimate",
	cpFolderHierarchy: "FolderHierarchy",
	cpMeetingResponse: "MeetingResponse",
	cpResolveRecip:    "ResolveRecipients",
	cpPing:            "Ping",
	cpProvision:       "Provision",
	cpSearch:          "Search",
	cpAirSyncBase:     "AirSyncBase",
	cpSettings:        "Settings",
	cpItemOperations:  "ItemOperations",
	cpComposeMail:     "ComposeMail",
}

var cpByNS = func() map[string]byte {
	m := make(map[string]byte, len(nsByCP))
	for cp, ns := range nsByCP {
		m[ns] = cp
	}
	return m
}()

// --- Tag tables ----------------------------------------------------------
//
// Each table is one code page: element name to token. The names are the local
// element names a client uses; the tokens are the published values from
// [MS-ASWBXML]. A name that appears in several code pages (Status, ServerId,
// Folders, ...) is listed in each one it is used in, because the code page a
// client is in decides which token means which name.

// AirSync (code page 0): the Sync command, the collection list, and the
// elements every command shares.
var airSyncTags = tagTable{
	"Sync":              0x05,
	"Responses":         0x06,
	"Add":               0x07,
	"Change":            0x08,
	"Delete":            0x09,
	"Fetch":             0x0A,
	"SyncKey":           0x0B,
	"ClientId":          0x0C,
	"ServerId":          0x0D,
	"Status":            0x0E,
	"Collection":        0x0F,
	"Class":             0x10,
	"CollectionId":      0x12,
	"GetChanges":        0x13,
	"MoreAvailable":     0x14,
	"WindowSize":        0x15,
	"Commands":          0x16,
	"Options":           0x17,
	"FilterType":        0x18,
	"Truncation":        0x19,
	"Conflict":          0x1B,
	"Collections":       0x1C,
	"ApplicationData":   0x1D,
	"DeletesAsMoves":    0x1E,
	"Supported":         0x20,
	"SoftDelete":        0x21,
	"MIMESupport":       0x22,
	"MIMETruncation":    0x23,
	"Wait":              0x24,
	"Limit":             0x25,
	"Partial":           0x26,
	"ConversationMode":  0x27,
	"MaxItems":          0x28,
	"HeartbeatInterval": 0x29,
}

// Email (code page 2): the properties of a mail item, which is what a Sync
// response carries for each message inside its ApplicationData.
var emailTags = tagTable{
	"Attachment":              0x05,
	"Attachments":             0x06,
	"AttName":                 0x07,
	"AttSize":                 0x08,
	"Att0id":                  0x09,
	"AttMethod":               0x0A,
	"Body":                    0x0C,
	"BodySize":                0x0D,
	"BodyTruncated":           0x0E,
	"DateReceived":            0x0F,
	"DisplayName":             0x10,
	"DisplayTo":               0x11,
	"Importance":              0x12,
	"MessageClass":            0x13,
	"Subject":                 0x14,
	"Read":                    0x15,
	"To":                      0x16,
	"Cc":                      0x17,
	"From":                    0x18,
	"ReplyTo":                 0x19,
	"AllDayEvent":             0x1A,
	"Categories":              0x1B,
	"Category":                0x1C,
	"DtStamp":                 0x1D,
	"EndTime":                 0x1E,
	"InstanceType":            0x1F,
	"BusyStatus":              0x20,
	"Location":                0x21,
	"MeetingRequest":          0x22,
	"Organizer":               0x23,
	"RecurrenceId":            0x24,
	"Reminder":                0x25,
	"ResponseRequested":       0x26,
	"Recurrences":             0x27,
	"Recurrence":              0x28,
	"Type":                    0x29,
	"Until":                   0x2A,
	"Occurrences":             0x2B,
	"Interval":                0x2C,
	"DayOfWeek":               0x2D,
	"DayOfMonth":              0x2E,
	"WeekOfMonth":             0x2F,
	"MonthOfYear":             0x30,
	"StartTime":               0x31,
	"Sensitivity":             0x32,
	"TimeZone":                0x33,
	"GlobalObjId":             0x34,
	"ThreadTopic":             0x35,
	"MIMEData":                0x36,
	"MIMETruncated":           0x37,
	"MIMESize":                0x38,
	"InternetCPID":            0x39,
	"Flag":                    0x3A,
	"Status":                  0x3B,
	"ContentClass":            0x3C,
	"FlagType":                0x3D,
	"CompleteTime":            0x3E,
	"DisallowNewTimeProposal": 0x3F,
}

// Calendar (code page 4): the properties of an appointment, which a Sync
// response carries for each event.
var calendarTags = tagTable{
	"Timezone":                  0x05,
	"AllDayEvent":               0x06,
	"Attendees":                 0x07,
	"Attendee":                  0x08,
	"Email":                     0x09,
	"Name":                      0x0A,
	"Body":                      0x0B,
	"BodyTruncated":             0x0C,
	"BusyStatus":                0x0D,
	"Categories":                0x0E,
	"Category":                  0x0F,
	"DtStamp":                   0x11,
	"EndTime":                   0x12,
	"Exception":                 0x13,
	"Exceptions":                0x14,
	"Deleted":                   0x15,
	"ExceptionStartTime":        0x16,
	"Location":                  0x17,
	"MeetingStatus":             0x18,
	"OrganizerEmail":            0x19,
	"OrganizerName":             0x1A,
	"Recurrence":                0x1B,
	"Type":                      0x1C,
	"Until":                     0x1D,
	"Occurrences":               0x1E,
	"Interval":                  0x1F,
	"DayOfWeek":                 0x20,
	"DayOfMonth":                0x21,
	"WeekOfMonth":               0x22,
	"MonthOfYear":               0x23,
	"Reminder":                  0x24,
	"Sensitivity":               0x25,
	"Subject":                   0x26,
	"StartTime":                 0x27,
	"UID":                       0x28,
	"AttendeeStatus":            0x29,
	"AttendeeType":              0x2A,
	"DisallowNewTimeProposal":   0x33,
	"ResponseRequested":         0x34,
	"AppointmentReplyTime":      0x35,
	"ResponseType":              0x36,
	"CalendarType":              0x37,
	"IsLeapMonth":               0x38,
	"FirstDayOfWeek":            0x39,
	"OnlineMeetingConfLink":     0x3A,
	"OnlineMeetingExternalLink": 0x3B,
	"ClientUid":                 0x3C,
}

// Move (code page 5): the MoveItems command.
var moveTags = tagTable{
	"MoveItems": 0x05,
	"Move":      0x06,
	"SrcMsgId":  0x07,
	"SrcFldId":  0x08,
	"DstFldId":  0x09,
	"Response":  0x0A,
	"Status":    0x0B,
	"DstMsgId":  0x0C,
}

// GetItemEstimate (code page 6): the GetItemEstimate command and its count.
var getItemEstimateTags = tagTable{
	"GetItemEstimate": 0x05,
	"Collections":     0x07,
	"Collection":      0x08,
	"Class":           0x09,
	"CollectionId":    0x0A,
	"Estimate":        0x0C,
	"Response":        0x0D,
	"Status":          0x0E,
}

// FolderHierarchy (code page 7): the FolderSync, FolderCreate, FolderDelete and
// FolderUpdate commands, and the tree a folder sync returns.
var folderHierarchyTags = tagTable{
	"Folders":      0x05,
	"Folder":       0x06,
	"DisplayName":  0x07,
	"ServerId":     0x08,
	"ParentId":     0x09,
	"Type":         0x0A,
	"Status":       0x0C,
	"Changes":      0x0E,
	"Add":          0x0F,
	"Delete":       0x10,
	"Update":       0x11,
	"SyncKey":      0x12,
	"FolderCreate": 0x13,
	"FolderDelete": 0x14,
	"FolderUpdate": 0x15,
	"FolderSync":   0x16,
	"Count":        0x17,
}

// MeetingResponse (code page 8): the command and the answer it carries.
var meetingResponseTags = tagTable{
	"CalendarId":        0x05,
	"CollectionId":      0x06,
	"MeetingResponse":   0x07,
	"RequestId":         0x08,
	"Request":           0x09,
	"Result":            0x0A,
	"Status":            0x0B,
	"UserResponse":      0x0C,
	"InstanceId":        0x0E,
	"ProposedStartTime": 0x10,
	"ProposedEndTime":   0x11,
	"SendResponse":      0x12,
}

// Ping (code page 13): the Ping command and the folders it reports.
var pingTags = tagTable{
	"Ping":              0x05,
	"Status":            0x07,
	"HeartbeatInterval": 0x08,
	"Folders":           0x09,
	"Folder":            0x0A,
	"Id":                0x0B,
	"Class":             0x0C,
	"MaxFolders":        0x0D,
}

// Provision (code page 14): the Provision command, the policy it returns, and
// the policy fields the policy document carries.
var provisionTags = tagTable{
	"Provision":                                0x05,
	"Policies":                                 0x06,
	"Policy":                                   0x07,
	"PolicyType":                               0x08,
	"PolicyKey":                                0x09,
	"Data":                                     0x0A,
	"Status":                                   0x0B,
	"RemoteWipe":                               0x0C,
	"EASProvisionDoc":                          0x0D,
	"DevicePasswordEnabled":                    0x0E,
	"AlphanumericDevicePasswordRequired":       0x0F,
	"RequireStorageCardEncryption":             0x10,
	"PasswordRecoveryEnabled":                  0x11,
	"AttachmentsEnabled":                       0x13,
	"MinDevicePasswordLength":                  0x14,
	"MaxInactivityTimeDeviceLock":              0x15,
	"MaxDevicePasswordFailedAttempts":          0x16,
	"MaxAttachmentSize":                        0x17,
	"AllowSimpleDevicePassword":                0x18,
	"DevicePasswordExpiration":                 0x19,
	"DevicePasswordHistory":                    0x1A,
	"AllowStorageCard":                         0x1B,
	"AllowCamera":                              0x1C,
	"RequireDeviceEncryption":                  0x1D,
	"AllowUnsignedApplications":                0x1E,
	"AllowUnsignedInstallationPackages":        0x1F,
	"MinDevicePasswordComplexCharacters":       0x20,
	"AllowWiFi":                                0x21,
	"AllowTextMessaging":                       0x22,
	"AllowPOPIMAPEmail":                        0x23,
	"AllowBluetooth":                           0x24,
	"AllowIrDA":                                0x25,
	"RequireManualSyncWhenRoaming":             0x26,
	"AllowDesktopSync":                         0x27,
	"MaxCalendarAgeFilter":                     0x28,
	"AllowHTMLEmail":                           0x29,
	"MaxEmailAgeFilter":                        0x2A,
	"MaxEmailBodyTruncationSize":               0x2B,
	"MaxEmailHTMLBodyTruncationSize":           0x2C,
	"RequireSignedSMIMEMessages":               0x2D,
	"RequireEncryptedSMIMEMessages":            0x2E,
	"RequireSignedSMIMEAlgorithm":              0x2F,
	"RequireEncryptionSMIMEAlgorithm":          0x30,
	"AllowSMIMEEncryptionAlgorithmNegotiation": 0x31,
	"AllowSMIMESoftCerts":                      0x32,
	"AllowBrowser":                             0x33,
	"AllowConsumerEmail":                       0x34,
	"AllowRemoteDesktop":                       0x35,
	"AllowInternetSharing":                     0x36,
	"UnapprovedInROMApplicationList":           0x37,
	"ApplicationName":                          0x38,
	"ApprovedApplicationList":                  0x39,
	"Hash":                                     0x3A,
	"AccountOnlyRemoteWipe":                    0x3B,
}

// Search (code page 15): the Search command and the stores it names.
var searchTags = tagTable{
	"Search":         0x05,
	"Store":          0x07,
	"Name":           0x08,
	"Query":          0x09,
	"Options":        0x0A,
	"Range":          0x0B,
	"Status":         0x0C,
	"Response":       0x0D,
	"Result":         0x0E,
	"Properties":     0x0F,
	"Total":          0x10,
	"EqualTo":        0x11,
	"Value":          0x12,
	"And":            0x13,
	"Or":             0x14,
	"FreeText":       0x15,
	"DeepTraversal":  0x17,
	"LongId":         0x18,
	"RebuildResults": 0x19,
	"LessThan":       0x1A,
	"GreaterThan":    0x1B,
	"UserName":       0x1E,
	"Password":       0x1F,
	"ConversationId": 0x20,
	"Picture":        0x21,
	"MaxSize":        0x22,
	"MaxPictures":    0x23,
}

// AirSyncBase (code page 17): the modern spelling of a body and its
// attachments, which a client negotiating 12.0 or newer uses in place of the
// older Email body elements.
var airSyncBaseTags = tagTable{
	"BodyPreference":     0x05,
	"Type":               0x06,
	"TruncationSize":     0x07,
	"AllOrNone":          0x08,
	"Body":               0x0A,
	"Data":               0x0B,
	"EstimatedDataSize":  0x0C,
	"Truncated":          0x0D,
	"Attachments":        0x0E,
	"Attachment":         0x0F,
	"DisplayName":        0x10,
	"FileReference":      0x11,
	"Method":             0x12,
	"ContentId":          0x13,
	"ContentLocation":    0x14,
	"IsInline":           0x15,
	"NativeBodyType":     0x16,
	"ContentType":        0x17,
	"Preview":            0x18,
	"BodyPartPreference": 0x19,
	"BodyPart":           0x1A,
	"Status":             0x1B,
	"Add":                0x1C,
	"Delete":             0x1D,
	"ClientId":           0x1E,
	"Content":            0x1F,
	"Location":           0x20,
}

// Settings (code page 18): the Settings command, its device information, and
// the account list a client reads on setup.
var settingsTags = tagTable{
	"Settings":                    0x05,
	"Status":                      0x06,
	"Get":                         0x07,
	"Set":                         0x08,
	"Oof":                         0x09,
	"OofState":                    0x0A,
	"StartTime":                   0x0B,
	"EndTime":                     0x0C,
	"OofMessage":                  0x0D,
	"AppliesToInternal":           0x0E,
	"AppliesToExternalKnown":      0x0F,
	"AppliesToExternalUnknown":    0x10,
	"Enabled":                     0x11,
	"ReplyMessage":                0x12,
	"BodyType":                    0x13,
	"DevicePassword":              0x14,
	"Password":                    0x15,
	"DeviceInformation":           0x16,
	"Model":                       0x17,
	"IMEI":                        0x18,
	"FriendlyName":                0x19,
	"OS":                          0x1A,
	"OSLanguage":                  0x1B,
	"PhoneNumber":                 0x1C,
	"UserInformation":             0x1D,
	"EmailAddresses":              0x1E,
	"SMTPAddress":                 0x1F,
	"UserAgent":                   0x20,
	"EnableOutboundSMS":           0x21,
	"MobileOperator":              0x22,
	"PrimarySmtpAddress":          0x23,
	"Accounts":                    0x24,
	"Account":                     0x25,
	"AccountId":                   0x26,
	"AccountName":                 0x27,
	"UserDisplayName":             0x28,
	"SendDisabled":                0x29,
	"RightsManagementInformation": 0x2B,
}

// ItemOperations (code page 20): the ItemOperations command, its fetch, and
// the folder purge it can carry.
var itemOperationsTags = tagTable{
	"ItemOperations":      0x05,
	"Fetch":               0x06,
	"Store":               0x07,
	"Options":             0x08,
	"Range":               0x09,
	"Total":               0x0A,
	"Properties":          0x0B,
	"Data":                0x0C,
	"Status":              0x0D,
	"Response":            0x0E,
	"Version":             0x0F,
	"Schema":              0x10,
	"Part":                0x11,
	"EmptyFolderContents": 0x12,
	"DeleteSubFolders":    0x13,
	"UserName":            0x14,
	"Password":            0x15,
	"Move":                0x16,
	"DstFldId":            0x17,
	"ConversationId":      0x18,
	"MoveAlways":          0x19,
}

// ComposeMail (code page 21): the SendMail command, which carries the message
// it sends as one MIME document.
var composeMailTags = tagTable{
	"SendMail":        0x05,
	"SmartForward":    0x06,
	"SmartReply":      0x07,
	"SaveInSentItems": 0x08,
	"ReplaceMime":     0x09,
	"Source":          0x0B,
	"FolderId":        0x0C,
	"ItemId":          0x0D,
	"LongId":          0x0E,
	"InstanceId":      0x0F,
	"Mime":            0x10,
	"ClientId":        0x11,
	"Status":          0x12,
	"AccountId":       0x13,
	"Forwardees":      0x15,
	"Forwardee":       0x16,
	"Name":            0x17,
	"Email":           0x18,
}

// tagTable is one code page: element name to token.
type tagTable map[string]byte

// tagsByCP is the full set of code pages.
var tagsByCP = map[byte]tagTable{
	cpAirSync:         airSyncTags,
	cpEmail:           emailTags,
	cpCalendar:        calendarTags,
	cpMove:            moveTags,
	cpGetItemEstimate: getItemEstimateTags,
	cpFolderHierarchy: folderHierarchyTags,
	cpMeetingResponse: meetingResponseTags,
	cpPing:            pingTags,
	cpProvision:       provisionTags,
	cpSearch:          searchTags,
	cpAirSyncBase:     airSyncBaseTags,
	cpSettings:        settingsTags,
	cpItemOperations:  itemOperationsTags,
	cpComposeMail:     composeMailTags,
}

// tokenToName is the reverse of tagsByCP: code page and token to name.
var tokenToName = func() map[byte]map[byte]string {
	out := make(map[byte]map[byte]string, len(tagsByCP))
	for cp, table := range tagsByCP {
		reverse := make(map[byte]string, len(table))
		for name, token := range table {
			reverse[token] = name
		}
		out[cp] = reverse
	}
	return out
}()

// resolveTag finds the token for an element in a code page.
func resolveTag(ns, name string) (byte, byte, bool) {
	cp, ok := cpByNS[ns]
	if !ok {
		return 0, 0, false
	}
	token, ok := tagsByCP[cp][name]
	if !ok {
		return 0, 0, false
	}
	return cp, token, true
}

// ---------------------------------------------------------------------------
// Writing
// ---------------------------------------------------------------------------

// openElement is an element whose opening token has been written but whose
// content bit has not: the writer only learns an element has content once
// something has been written inside it.
type openElement struct {
	offset     int // where its token byte sits in the buffer
	hasContent bool
}

// WBXMLWriter encodes an element tree into a WBXML 1.3 token stream.
//
// It starts the stream with the four byte header, tracks the code page it is
// in and switches, in the stream, whenever an element comes from a different
// code page, and sets the content bit on every element that got content plus
// an END token after each of them. An element with no content is exactly one
// token and gets no END, which is what the format says and what a client reads.
type WBXMLWriter struct {
	buf      *bytes.Buffer
	codePage byte
	stack    []openElement
}

// NewWBXMLWriter starts a stream with the WBXML header already written.
func NewWBXMLWriter() *WBXMLWriter {
	w := &WBXMLWriter{buf: &bytes.Buffer{}, codePage: cpAirSync}
	w.buf.Write([]byte{wbxmlVersion, wbxmlPublicID, wbxmlCharset, wbxmlStrTable})
	return w
}

// Start opens an element. It switches code page in the stream when the element
// lives in a different code page than the one currently open.
func (w *WBXMLWriter) Start(ns, name string) error {
	cp, token, ok := resolveTag(ns, name)
	if !ok {
		return fmt.Errorf("eas: no tag for %s:%s", ns, name)
	}
	// Opening a child is itself content: the parent now has some.
	if len(w.stack) > 0 {
		w.stack[len(w.stack)-1].hasContent = true
	}
	if cp != w.codePage {
		w.buf.WriteByte(tokenSwitchPage)
		w.buf.WriteByte(cp)
		w.codePage = cp
	}
	w.stack = append(w.stack, openElement{offset: w.buf.Len()})
	w.buf.WriteByte(token)
	return nil
}

// End closes the innermost open element, setting its content bit if anything
// was written inside it and writing the END token that says where it stops.
func (w *WBXMLWriter) End() {
	if len(w.stack) == 0 {
		return
	}
	open := w.stack[len(w.stack)-1]
	w.stack = w.stack[:len(w.stack)-1]
	if !open.hasContent {
		// The element stayed empty: one token, no content bit, no END.
		return
	}
	w.buf.Bytes()[open.offset] |= tokenHasContent
	w.buf.WriteByte(tokenEnd)
}

// Text writes an inline string as the content of the open element.
func (w *WBXMLWriter) Text(s string) {
	if s == "" || len(w.stack) == 0 {
		return
	}
	w.stack[len(w.stack)-1].hasContent = true
	w.buf.WriteByte(tokenStrI)
	w.buf.WriteString(s)
	w.buf.WriteByte(0x00)
}

// Opaque writes a binary payload as the content of the open element.
func (w *WBXMLWriter) Opaque(data []byte) {
	if len(data) == 0 || len(w.stack) == 0 {
		return
	}
	w.stack[len(w.stack)-1].hasContent = true
	w.buf.WriteByte(tokenOpaque)
	w.writeMBUInt32(uint32(len(data)))
	w.buf.Write(data)
}

// Leaf writes an element that holds one string and nothing else.
func (w *WBXMLWriter) Leaf(ns, name, value string) error {
	if err := w.Start(ns, name); err != nil {
		return err
	}
	w.Text(value)
	w.End()
	return nil
}

// Bytes returns the document written so far.
func (w *WBXMLWriter) Bytes() []byte {
	return w.buf.Bytes()
}

// writeMBUInt32 writes a multi-byte integer: seven bits at a time, high bit
// set on every byte but the last.
func (w *WBXMLWriter) writeMBUInt32(v uint32) {
	for v >= 0x80 {
		w.buf.WriteByte(byte(v&0x7F) | 0x80)
		v >>= 7
	}
	w.buf.WriteByte(byte(v))
}

// ---------------------------------------------------------------------------
// Reading
// ---------------------------------------------------------------------------

// WBXMLNode is one decoded element: its code page's namespace, its name, its
// string content, its opaque payload and its children. The code page it came
// from was resolved as it was read, so the pair identifies it.
type WBXMLNode struct {
	NS       string
	Name     string
	Text     string
	Opaque   []byte
	Children []*WBXMLNode
}

// FindChild returns the first child element with the given name.
func (n *WBXMLNode) FindChild(name string) *WBXMLNode {
	for _, c := range n.Children {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// FindChildren returns every child element with the given name.
func (n *WBXMLNode) FindChildren(name string) []*WBXMLNode {
	var out []*WBXMLNode
	for _, c := range n.Children {
		if c.Name == name {
			out = append(out, c)
		}
	}
	return out
}

// TextOf returns the text of the first child element with the given name.
func (n *WBXMLNode) TextOf(name string) string {
	if c := n.FindChild(name); c != nil {
		return c.Text
	}
	return ""
}

// FindDescendant returns the first element with the given name anywhere below.
func (n *WBXMLNode) FindDescendant(name string) *WBXMLNode {
	for _, c := range n.Children {
		if c.Name == name {
			return c
		}
		if found := c.FindDescendant(name); found != nil {
			return found
		}
	}
	return nil
}

// TextDescendant returns the text of the first element with the given name
// anywhere below.
func (n *WBXMLNode) TextDescendant(name string) string {
	if c := n.FindDescendant(name); c != nil {
		return c.Text
	}
	return ""
}

// WBXMLDecoder reads a WBXML 1.3 token stream into an element tree.
type WBXMLDecoder struct {
	data     []byte
	pos      int
	codePage byte
}

// NewWBXMLDecoder starts a decoder over a document.
func NewWBXMLDecoder(data []byte) *WBXMLDecoder {
	return &WBXMLDecoder{data: data}
}

func (d *WBXMLDecoder) readByte() (byte, error) {
	if d.pos >= len(d.data) {
		return 0, io.ErrUnexpectedEOF
	}
	b := d.data[d.pos]
	d.pos++
	return b, nil
}

func (d *WBXMLDecoder) readMBUInt32() (uint32, error) {
	var v uint32
	var shift uint
	for {
		b, err := d.readByte()
		if err != nil {
			return 0, err
		}
		v |= uint32(b&0x7F) << shift
		if b&0x80 == 0 {
			return v, nil
		}
		shift += 7
		if shift > 28 {
			return 0, fmt.Errorf("eas: WBXML integer too long")
		}
	}
}

func (d *WBXMLDecoder) readStrI() (string, error) {
	start := d.pos
	for d.pos < len(d.data) && d.data[d.pos] != 0x00 {
		d.pos++
	}
	if d.pos >= len(d.data) {
		return "", fmt.Errorf("eas: WBXML string runs past the document")
	}
	s := string(d.data[start:d.pos])
	d.pos++ // the terminator
	return s, nil
}

func (d *WBXMLDecoder) readOpaque() ([]byte, error) {
	n, err := d.readMBUInt32()
	if err != nil {
		return nil, err
	}
	if uint64(d.pos)+uint64(n) > uint64(len(d.data)) {
		return nil, fmt.Errorf("eas: WBXML opaque payload runs past the document")
	}
	raw := make([]byte, n)
	copy(raw, d.data[d.pos:d.pos+int(n)])
	d.pos += int(n)
	return raw, nil
}

// DecodeHeader reads the WBXML header: version, public identifier, charset and
// string table, the last three as multi-byte integers. ActiveSync writes no
// string table, so a document that carries one is read past and noted.
func (d *WBXMLDecoder) DecodeHeader() error {
	version, err := d.readByte()
	if err != nil {
		return fmt.Errorf("eas: WBXML header: %w", err)
	}
	if version != wbxmlVersion {
		return fmt.Errorf("eas: unexpected WBXML version 0x%02x", version)
	}
	if _, err := d.readMBUInt32(); err != nil {
		return fmt.Errorf("eas: WBXML public id: %w", err)
	}
	if _, err := d.readMBUInt32(); err != nil {
		return fmt.Errorf("eas: WBXML charset: %w", err)
	}
	tableLen, err := d.readMBUInt32()
	if err != nil {
		return fmt.Errorf("eas: WBXML string table length: %w", err)
	}
	if tableLen > 0 {
		if uint64(d.pos)+uint64(tableLen) > uint64(len(d.data)) {
			return fmt.Errorf("eas: WBXML string table runs past the document")
		}
		d.pos += int(tableLen)
	}
	return nil
}

// Decode reads one document: a single root element with its content.
//
// The content bit decides the shape. An element that has it is open until an
// END token closes it; an element without it is finished the moment its token
// is read, and its siblings attach to its parent. That is the format as a real
// client writes it: <MoreAvailable/> is one token with no END after it, and
// misreading that would hang the rest of the document off the wrong parent.
//
// The one tolerance: a writer that forgot the bit but still sends its string
// is read as if it had set it, because the string only exists where content
// exists, and the END behind it closes that element rather than its parent.
func (d *WBXMLDecoder) Decode() (*WBXMLNode, error) {
	if err := d.DecodeHeader(); err != nil {
		return nil, err
	}

	root := &WBXMLNode{Name: "#root"}
	stack := []*WBXMLNode{root}

	for d.pos < len(d.data) {
		b, err := d.readByte()
		if err != nil {
			return nil, err
		}
		switch b {
		case tokenSwitchPage:
			cp, err := d.readByte()
			if err != nil {
				return nil, fmt.Errorf("eas: code page switch past the document")
			}
			d.codePage = cp
			continue
		case tokenEnd:
			if len(stack) <= 1 {
				return nil, fmt.Errorf("eas: WBXML end token with no open element")
			}
			stack = stack[:len(stack)-1]
			continue
		case tokenEntity:
			if _, err := d.readMBUInt32(); err != nil {
				return nil, err
			}
			continue
		case tokenStrI:
			s, err := d.readStrI()
			if err != nil {
				return nil, err
			}
			stack[len(stack)-1].Text += s
			continue
		case tokenOpaque:
			raw, err := d.readOpaque()
			if err != nil {
				return nil, err
			}
			stack[len(stack)-1].Opaque = raw
			continue
		case tokenExtI0, tokenExtI1, tokenExtI2, tokenPI, tokenLiteralC:
			return nil, fmt.Errorf("eas: unsupported WBXML global token 0x%02x", b)
		}
		if b&tokenHasAttributes != 0 {
			// Attributes are not part of ActiveSync; a token carrying one is
			// not a token this server can place.
			return nil, fmt.Errorf("eas: WBXML token 0x%02x carries attributes", b)
		}
		token := b & tokenNameMask
		name, known := tokenToName[d.codePage][token]
		if !known {
			// An element this table does not have: read it as an unnamed
			// element so the rest of the document still lines up. A handler
			// looks its own names up, so an unnamed element is ignored.
			name = fmt.Sprintf("?0x%02X", token)
		}
		node := &WBXMLNode{NS: nsByCP[d.codePage], Name: name}
		stack[len(stack)-1].Children = append(stack[len(stack)-1].Children, node)
		if b&tokenHasContent != 0 {
			stack = append(stack, node)
			continue
		}
		if d.pos < len(d.data) {
			switch d.data[d.pos] {
			case tokenStrI, tokenOpaque, tokenStrT:
				// Content without the bit that says there is any: read it,
				// and the END that follows closes this element.
				stack = append(stack, node)
			}
		}
	}

	if len(root.Children) != 1 {
		return nil, fmt.Errorf("eas: expected one root element, found %d", len(root.Children))
	}
	if len(stack) != 1 {
		return nil, fmt.Errorf("eas: %d element(s) left open", len(stack)-1)
	}
	return root.Children[0], nil
}

// CommandName returns the decoded command: the root element's name.
func (n *WBXMLNode) CommandName() string {
	return n.Name
}

// asInt reads a child element as a number, with a default when it is absent
// or is not a number at all.
func asInt(n *WBXMLNode, def int) int {
	if n == nil {
		return def
	}
	v, err := strconv.Atoi(strings.TrimSpace(n.Text))
	if err != nil {
		return def
	}
	return v
}

// asBool reads a child element as the 0/1 a client writes for a flag.
func asBool(n *WBXMLNode, def bool) bool {
	switch asInt(n, -1) {
	case 0:
		return false
	case 1:
		return true
	default:
		return def
	}
}
