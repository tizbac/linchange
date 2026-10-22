package caldav_test

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/auth/mailauth"
	pwd "github.com/linchange/linchange/internal/auth/password"
	"github.com/linchange/linchange/internal/caldav"
	"github.com/linchange/linchange/internal/calendar"
	"github.com/linchange/linchange/internal/ical"
	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/testdb"
)

const (
	aliceAddr = "alice@example.com"
	alicePass = "correct-horse-battery"
	bobAddr   = "bob@example.com"
	bobPass   = "another-good-password"
)

// harness is a running CalDAV service plus the database behind it, so a test can
// set up state the way the rest of the system would and then read it back.
type harness struct {
	t   *testing.T
	srv *httptest.Server
	db  *sqlx.DB
	cal *store.Calendar
}

// newHarness starts a CalDAV service with alice's account and one calendar.
func newHarness(t *testing.T) *harness {
	t.Helper()

	db := testdb.Migrated(t)
	ctx := context.Background()

	hash, err := pwd.Hash(alicePass, config.New().Auth.Local.Argon2)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	bobHash, err := pwd.Hash(bobPass, config.New().Auth.Local.Argon2)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}

	for _, u := range []struct {
		addr, display string
		h             string
	}{
		{aliceAddr, "Alice Example", hash},
		{bobAddr, "Bob Example", bobHash},
	} {
		if err := store.NewUsers().Create(ctx, db, store.CreateUserRequest{
			Address: u.addr, Source: "local", DisplayName: u.display,
		}); err != nil {
			t.Fatalf("create %s: %v", u.addr, err)
		}
		if err := store.NewCredentials().Set(ctx, db, u.addr, u.h); err != nil {
			t.Fatalf("set credential for %s: %v", u.addr, err)
		}
	}

	owner := aliceAddr
	cal, err := store.NewCalendars().Create(ctx, db, store.CreateCalendarRequest{
		Owner: &owner, Name: "Work", Timezone: "Europe/Lisbon",
	})
	if err != nil {
		t.Fatalf("create calendar: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := calendar.NewService(store.NewCalendars(), store.NewEvents(), store.NewWorkgroups())
	server := caldav.NewServer("/caldav", svc, db, mailauth.New(db, logger, nil))

	srv := httptest.NewServer(server.Handler())
	t.Cleanup(srv.Close)

	return &harness{t: t, srv: srv, db: db, cal: &cal}
}

// response is one decoded DAV:response, which is what most of these tests
// actually assert on.
type response struct {
	href  string
	found map[string]string
	// statuses maps a property's qualified name to the status line reported for
	// it, so a test can tell 200 from 404 per property.
	statuses map[string]string
	// statusesOrder keeps the properties in the order the document listed them,
	// because propname must be deterministic for a client to rely on and a map
	// cannot say.
	order []string
	// spaces records the namespace each property came back in, so a test can
	// check that a CalDAV property was not written into the DAV: namespace.
	spaces map[string]string
	// wholeStatus is the short-form status for the whole resource, used when there
	// was no propstat to carry one.
	wholeStatus string
}

// document is a decoded multistatus: the responses, and whatever the document
// itself carries.
//
// The synchronisation token is a child of the multistatus and not of a response,
// which RFC 6578 §3.2 fixes as (response*, responsedescription?, sync-token?) and
// §6.4 describes element by element. A server that put it inside whichever response
// happened to come first would be answering a question no client asks, and one
// client parses -- the token is read off the document -- would see none at all.
type document struct {
	responses []response
	syncToken string
}

// status is the code from a status line.
func (r response) status(prop string) string { return r.statuses[prop] }

// ok reports whether a property came back 200.
func (r response) ok(prop string) bool { return r.status(prop) == "200" }

type davProp struct {
	XMLName xml.Name
	// Inner is the element's character data. An element with only child
	// elements has none, which is what a collection-valued property looks like.
	Inner string `xml:",chardata"`
	// Attrs carries the attributes, for the properties whose value is an
	// attribute rather than text: Apple's calendar-color is the one in the
	// wild, and it is stored that way because that is what the clients send.
	Attrs []xml.Attr `xml:",any,attr"`
	// Href is the value of a property that wraps a DAV:href rather than holding
	// its value as text. current-user-principal is the one that matters here:
	// RFC 3744 makes it a wrapper precisely so that it can carry several
	// alternatives, and reading it as text finds only the whitespace between
	// the tags.
	Href string `xml:"DAV: href"`
}

// attr returns an attribute value by local name.
func (p davProp) attr(local string) string {
	for _, a := range p.Attrs {
		if a.Name.Local == local {
			return a.Value
		}
	}
	return ""
}

// davPropGroup is a DAV:prop, and davPropstat is what carries its status.
//
// The properties are matched with ,any rather than by name because they arrive in
// whichever namespace they live in -- DAV:, CalDAV:, Apple's -- and the element's
// own name says which, which is also what a test asserts on.
type davPropGroup struct {
	Props []davProp `xml:",any"`
}

type davPropstat struct {
	Props  davPropGroup `xml:"DAV: prop"`
	Status string       `xml:"DAV: status"`
}

type davResponse struct {
	Href     string        `xml:"DAV: href"`
	Propstat []davPropstat `xml:"DAV: propstat"`
	// Status is the short form, used when the whole resource shares one status
	// and there is therefore no propstat to carry it. A sync reports a removed
	// member this way, with nothing else.
	Status string `xml:"DAV: status"`
}

type davBody struct {
	XMLName xml.Name `xml:"DAV: multistatus"`
	// Responses carries the DAV: namespace explicitly, so a document whose
	// multistatus is in some other namespace does not decode. That is the
	// check worth making: the root element's namespace is what says this is a
	// WebDAV response at all, and a client that cannot rely on it is a client
	// that will read a body this server never meant to send.
	Responses []davResponse `xml:"DAV: response"`
	// SyncToken is at document level, where RFC 6578 §3.2 puts it.
	SyncToken string `xml:"DAV: sync-token"`
}

// decode parses a multistatus document.
//
// The struct tags carry namespace URIs, so a property this server declared in the
// wrong namespace fails to decode rather than being matched on its local name
// alone. Local names are not enough: DAV: and the CalDAV: namespace both have a
// displayname-like element, and a response that used the wrong one would satisfy
// a looser check.
func decode(t *testing.T, body []byte) document {
	t.Helper()

	var doc davBody
	if err := xml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("the response is not a multistatus document: %v\n%s", err, body)
	}

	out := make([]response, 0, len(doc.Responses))
	for _, r := range doc.Responses {
		parsed := response{
			href:     r.Href,
			found:    map[string]string{},
			statuses: map[string]string{},
			spaces:   map[string]string{},
		}
		for _, ps := range r.Propstat {
			code := statusCode(ps.Status)
			for _, p := range ps.Props.Props {
				name := p.XMLName.Local
				parsed.order = append(parsed.order, name)
				parsed.statuses[name] = code
				parsed.spaces[name] = p.XMLName.Space
				if code != "200" {
					continue
				}
				value := strings.TrimSpace(p.Inner)
				if value == "" {
					value = strings.TrimSpace(p.Href)
				}
				if value == "" {
					value = p.attr("color")
				}
				parsed.found[name] = value
			}
		}
		if r.Status != "" {
			parsed.wholeStatus = statusCode(r.Status)
		}
		out = append(out, parsed)
	}
	return document{responses: out, syncToken: strings.TrimSpace(doc.SyncToken)}
}

func statusCode(line string) string {
	parts := strings.Split(strings.TrimSpace(line), " ")
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}

// do issues a DAV request as alice.
func (h *harness) do(method, path, body string, headers map[string]string) (*http.Response, string) {
	h.t.Helper()
	return h.send(aliceAddr, alicePass, method, path, body, headers)
}

// doAs issues a DAV request as a named principal.
func (h *harness) doAs(addr, pass, method, path, body string, headers map[string]string) (*http.Response, string) {
	h.t.Helper()
	return h.send(addr, pass, method, path, body, headers)
}

// doAnon issues a DAV request carrying no credentials at all.
//
// Distinct from sending a blank password, which is a wrong password: a client that
// has been told nothing wants the challenge again, while a client that guessed gets
// the same answer as a client whose account does not exist.
func (h *harness) doAnon(method, path, body string, headers map[string]string) (*http.Response, string) {
	h.t.Helper()
	return h.send("", "", method, path, body, headers)
}

// send issues the request. An empty address means no Authorization header.
func (h *harness) send(addr, pass, method, path, body string, headers map[string]string) (*http.Response, string) {
	h.t.Helper()

	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, rdr)
	if err != nil {
		h.t.Fatalf("build request: %v", err)
	}
	if addr != "" {
		req.SetBasicAuth(addr, pass)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("read body: %v", err)
	}
	return resp, string(raw)
}

// home is alice's calendar home set, escaped as a path.
func home() string {
	return "/caldav/alice%40example.com/"
}

func calPath(name string) string { return home() + name + "/" }

// ---------------------------------------------------------------------------
// Authentication
// ---------------------------------------------------------------------------

// A client with no password cannot be told to send one unless the server says so
// in a way a browser and a phone's calendar app both understand. A bare 401 is
// read as "this endpoint is not here".
func TestNoCredentialsAreAnsweredWithAChallenge(t *testing.T) {
	h := newHarness(t)

	resp, _ := h.doAnon("PROPFIND", "/caldav/", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", resp.StatusCode)
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	if !strings.HasPrefix(challenge, "Basic") {
		t.Errorf("the challenge is %q, which does not ask for Basic credentials", challenge)
	}
	if !strings.Contains(challenge, `realm=`) {
		t.Errorf("the challenge has no realm: %q", challenge)
	}
}

// A wrong password and an account that does not exist must be the same answer.
// Anything else lets a caller enumerate accounts by timing or by status code.
func TestAWrongPasswordIsIndistinguishableFromAnAccountThatDoesNotExist(t *testing.T) {
	h := newHarness(t)

	wrongPass, bodyWrong := h.doAs(aliceAddr, "not-the-password", "PROPFIND", "/caldav/", "", nil)
	noSuchUser, bodyNoSuch := h.doAs("nobody@example.com", alicePass, "PROPFIND", "/caldav/", "", nil)

	if wrongPass.StatusCode != noSuchUser.StatusCode {
		t.Errorf("a wrong password gave %d and an unknown account gave %d",
			wrongPass.StatusCode, noSuchUser.StatusCode)
	}
	if bodyWrong != bodyNoSuch {
		t.Errorf("the bodies differ:\n wrong password: %q\n unknown account: %q", bodyWrong, bodyNoSuch)
	}
	if !strings.Contains(wrongPass.Header.Get("WWW-Authenticate"), "Basic") {
		t.Error("the refusal does not carry a challenge, so a client has nothing to prompt with")
	}
}

// An account that exists and has the wrong password must not be logged in as,
// which is the whole purpose of asking a verifier rather than reading the user
// table.
func TestThePrincipalInTheContextIsTheOneThatAuthenticated(t *testing.T) {
	h := newHarness(t)

	_, body := h.do("PROPFIND", "/caldav/", propfindBody("current-user-principal"), nil)
	doc := decode(t, []byte(body))
	got := doc.responses
	if len(got) != 1 {
		t.Fatalf("got %d responses, want 1", len(got))
	}
	want := "/caldav/principals/alice%40example.com/"
	if got[0].found["current-user-principal"] != want {
		t.Errorf("current-user-principal is %q, want %q", got[0].found["current-user-principal"], want)
	}
}

// ---------------------------------------------------------------------------
// Discovery and principals
// ---------------------------------------------------------------------------

// RFC 6764 puts the discovery document at the root of the origin. A client
// configured with nothing but a domain name asks /.well-known/caldav, and
// answering it at /caldav/.well-known/caldav is a redirect from nothing.
func TestTheDiscoveryDocumentIsAtTheRootOfTheOrigin(t *testing.T) {
	h := newHarness(t)

	client := *h.srv.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	req, _ := http.NewRequest("GET", h.srv.URL+"/.well-known/caldav", nil)
	req.SetBasicAuth(aliceAddr, alicePass)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /.well-known/caldav: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("got %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/caldav/" {
		t.Errorf("the redirect points at %q, want %q", loc, "/caldav/")
	}
}

// A client that cannot read the DAV header concludes the server does not speak
// WebDAV and offers no way to add an account at all.
func TestOptionsAdvertisesCalendarAccess(t *testing.T) {
	h := newHarness(t)

	resp, _ := h.do("OPTIONS", "/caldav/", "", nil)
	dav := resp.Header.Get("DAV")
	if !strings.Contains(dav, "calendar-access") {
		t.Errorf("DAV: %q does not mention calendar-access", dav)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("got %d, want 200", resp.StatusCode)
	}
	for _, verb := range []string{"PROPFIND", "REPORT", "MKCALENDAR", "PROPPATCH"} {
		if !strings.Contains(resp.Header.Get("Allow"), verb) {
			t.Errorf("Allow does not list %s: %q", verb, resp.Header.Get("Allow"))
		}
	}
}

// The principal collection exists so a client can find its own principal. Listing
// every account in the domain there would publish the address list to anyone who
// could authenticate as anybody.
func TestThePrincipalCollectionHoldsOnlyTheCaller(t *testing.T) {
	h := newHarness(t)

	_, body := h.do("PROPFIND", "/caldav/principals/",
		propfindBody("displayname", "calendar-home-set", "principal-URL"),
		map[string]string{"Depth": "1"})

	doc := decode(t, []byte(body))
	got := doc.responses
	// The collection and the caller's own principal, in that order. A client
	// asking who exists here gets an answer that says somebody does, rather than
	// one that reads as an empty directory.
	if len(got) != 2 {
		t.Fatalf("got %d responses at depth 1, want the collection and one principal: %s", len(got), body)
	}
	if got[0].href != "/caldav/principals/" {
		t.Errorf("the first response is %q, want the collection", got[0].href)
	}
	if got[1].href != "/caldav/principals/alice%40example.com/" {
		t.Errorf("the principal is %q, want the caller's own", got[1].href)
	}
	if got[1].found["displayname"] != "Alice Example" {
		t.Errorf("displayname is %q, want the user's", got[1].found["displayname"])
	}
	// The other account on this server exists and must not be listed: a principal
	// collection that enumerated its users would be an account list anyone could
	// read.
	if strings.Contains(body, bobAddr) {
		t.Errorf("the collection listed another principal: %s", body)
	}
}

// A principal may read its own properties and nobody else's. There is no
// delegation model here, so answering for another address would be reporting
// another user's account with nothing behind the answer.
func TestAnotherUsersPrincipalIsRefused(t *testing.T) {
	h := newHarness(t)

	resp, body := h.do("PROPFIND", "/caldav/principals/bob%40example.com/",
		propfindBody("displayname"), nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("got %d, want 403: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "need-privileges") {
		t.Errorf("the refusal does not name a condition element: %s", body)
	}
}

// ---------------------------------------------------------------------------
// Collections
// ---------------------------------------------------------------------------

// A home set at depth 0 tells a client where its calendars are; at depth 1 it
// tells it which they are. A client that receives only the first has nothing to
// show the user, which is the complaint this server was built against.
func TestAHomeSetListsItsCalendarsAtDepthOne(t *testing.T) {
	h := newHarness(t)

	_, body := h.do("PROPFIND", home(),
		propfindBody("displayname", "resourcetype", "getetag"),
		map[string]string{"Depth": "1"})

	doc := decode(t, []byte(body))
	got := doc.responses
	if len(got) != 2 {
		t.Fatalf("got %d responses, want the home set and one calendar: %s", len(got), body)
	}
	if got[0].href != home() {
		t.Errorf("the first response is %q, want the home set %q", got[0].href, home())
	}
	if got[1].href != calPath("Work") {
		t.Errorf("the calendar is at %q, want %q", got[1].href, calPath("Work"))
	}
	if !got[1].ok("resourcetype") {
		t.Errorf("resourcetype is %s, want 200", got[1].status("resourcetype"))
	}
	// getetag does not exist on a calendar collection. Reporting it 200 as an
	// empty element would make a client cache a calendar it can never revalidate.
	if got[1].status("getetag") != "404" {
		t.Errorf("getetag on a collection is %s, want 404", got[1].status("getetag"))
	}
}

// Depth: infinity on a calendar means walking every event the user has ever kept.
// RFC 4918 §9.1 forbids it and the answer has to be the one it specifies.
func TestDepthInfinityIsRefused(t *testing.T) {
	h := newHarness(t)

	resp, body := h.do("PROPFIND", home(), "", map[string]string{"Depth": "infinity"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("got %d, want 403: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "propfind-finite-depth") {
		t.Errorf("the refusal does not name DAV:propfind-finite-depth: %s", body)
	}
}

// MKCALENDAR is the only way RFC 4791 lets a client create a calendar, and its
// success is 201 with a Location. A 200 tells the client nothing happened and it
// will create the same calendar again on its next sync.
func TestCreatingACalendar(t *testing.T) {
	h := newHarness(t)

	resp, body := h.do("MKCALENDAR", home()+"Holidays", mkCalendarBody("Holidays"), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("got %d, want 201: %s", resp.StatusCode, body)
	}
	if loc := resp.Header.Get("Location"); !strings.HasSuffix(loc, "Holidays/") {
		t.Errorf("Location is %q, want the new calendar's URL", loc)
	}

	_, body = h.do("PROPFIND", home(), propfindBody("displayname"), map[string]string{"Depth": "1"})
	if !strings.Contains(body, "Holidays") {
		t.Errorf("the new calendar is not in the home set: %s", body)
	}
}

// A name that is taken is 405 rather than a conflict, because that is what tells
// a client "there is already one here, use it" as opposed to "you may not".
func TestCreatingACalendarTwiceIsRefusedAsAMethod(t *testing.T) {
	h := newHarness(t)

	resp, body := h.do("MKCALENDAR", home()+"Work", mkCalendarBody("Work"), nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("got %d, want 405: %s", resp.StatusCode, body)
	}
}

// The display name and the name in the path are the same string, because the
// name is a path segment and there is nowhere to record a second one. A request
// that asks for two different names is refused rather than silently losing one.
func TestACalendarNameMustMatchThePath(t *testing.T) {
	h := newHarness(t)

	resp, body := h.do("MKCALENDAR", home()+"Personal", mkCalendarBody("Something Else"), nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("got %d, want 409: %s", resp.StatusCode, body)
	}
}

// Claiming VTODO support and being given a server that stores tasks and never
// shows them is worse than being refused.
func TestACalendarAskingForTasksIsRefused(t *testing.T) {
	h := newHarness(t)

	body := `<?xml version="1.0" encoding="utf-8"?>
<C:mkcalendar xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:set><D:prop>
    <D:resourcetype><D:collection/><C:calendar/></D:resourcetype>
    <D:displayname>Tasks</D:displayname>
    <C:supported-calendar-component-set>
      <C:comp name="VEVENT"/><C:comp name="VTODO"/>
    </C:supported-calendar-component-set>
  </D:prop></D:set>
</C:mkcalendar>`

	resp, out := h.do("MKCALENDAR", home()+"Tasks", body, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("got %d, want 403: %s", resp.StatusCode, out)
	}
	if !strings.Contains(out, "VTODO") {
		t.Errorf("the refusal does not name what is not supported: %s", out)
	}
}

// PROPPATCH is a per-property operation. Reporting one status for the request
// would leave a client that changed three properties unable to tell which half of
// its change it has to retry.
func TestProppatchChangesWhatItMayAndReportsWhatItMayNot(t *testing.T) {
	h := newHarness(t)

	body := `<?xml version="1.0" encoding="utf-8"?>
<D:propertyupdate xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:set><D:prop>
    <C:calendar-description>Team calendar</C:calendar-description>
    <A:calendar-color xmlns:A="http://apple.com/ns/ical/" A:color="#ff0000"/>
    <D:displayname>Renamed</D:displayname>
    <D:owner/>
  </D:prop></D:set>
</D:propertyupdate>`

	resp, out := h.do("PROPPATCH", calPath("Work"), body, nil)
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("got %d, want 207: %s", resp.StatusCode, out)
	}

	// A live property is refused. The response is still 207 with 200 for the rest,
	// so the client learns which is which.
	if !strings.Contains(out, "cannot-modify-protected-property") {
		t.Errorf("the live property was not refused: %s", out)
	}

	_, list := h.do("PROPFIND", calPath("Work"),
		propfindBody("displayname", "calendar-description", "calendar-color"), nil)
	doc := decode(t, []byte(list))
	got := doc.responses
	if len(got) != 1 {
		t.Fatalf("got %d responses: %s", len(got), list)
	}
	if got[0].found["displayname"] != "Renamed" {
		t.Errorf("displayname is %q, want Renamed", got[0].found["displayname"])
	}
	if got[0].found["calendar-description"] != "Team calendar" {
		t.Errorf("calendar-description is %q, want %q", got[0].found["calendar-description"], "Team calendar")
	}
	if got[0].spaces["calendar-description"] != "urn:ietf:params:xml:ns:caldav" {
		t.Errorf("calendar-description is in namespace %q, want the CalDAV one",
			got[0].spaces["calendar-description"])
	}
	if got[0].found["calendar-color"] != "#ff0000" {
		t.Errorf("calendar-color is %q, want it to carry #ff0000", got[0].found["calendar-color"])
	}
	// The Apple namespace is not the same as DAV: or CalDAV:, and a client that
	// reads the colour out of the wrong one gets nothing.
	if got[0].spaces["calendar-color"] != "http://apple.com/ns/ical/" {
		t.Errorf("calendar-color is in namespace %q, want the Apple one",
			got[0].spaces["calendar-color"])
	}
}

// A colour is stored one way and reported that way, whatever spelling the client
// sent, or a calendar loses its colour the first time it is saved from a Mac.
func TestColoursAreNormalisedOnTheWayIn(t *testing.T) {
	h := newHarness(t)

	body := `<?xml version="1.0" encoding="utf-8"?>
<D:propertyupdate xmlns:D="DAV:" xmlns:A="http://apple.com/ns/ical/">
  <D:set><D:prop><A:calendar-color A:color="F0A"/></D:prop></D:set>
</D:propertyupdate>`
	if resp, out := h.do("PROPPATCH", calPath("Work"), body, nil); resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("PROPPATCH: %d %s", resp.StatusCode, out)
	}

	_, list := h.do("PROPFIND", calPath("Work"), propfindBody("calendar-color"), nil)
	doc := decode(t, []byte(list))
	got := doc.responses
	if got[0].found["calendar-color"] != "#ff00aa" {
		t.Errorf("calendar-color is %q, want #ff00aa", got[0].found["calendar-color"])
	}
}

// A colour that is not one is refused rather than stored, because a client that
// cannot read it back loses the setting silently.
func TestAnImpossibleColourIsRefused(t *testing.T) {
	h := newHarness(t)

	body := `<?xml version="1.0" encoding="utf-8"?>
<D:propertyupdate xmlns:D="DAV:" xmlns:A="http://apple.com/ns/ical/">
  <D:set><D:prop><A:calendar-color A:color="chartreuse"/></D:prop></D:set>
</D:propertyupdate>`

	resp, out := h.do("PROPPATCH", calPath("Work"), body, nil)
	if !strings.Contains(out, "409") && resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("got %d: %s", resp.StatusCode, out)
	}
	_, list := h.do("PROPFIND", calPath("Work"), propfindBody("calendar-color"), nil)
	if strings.Contains(list, "chartreuse") {
		t.Errorf("the bad colour was stored: %s", list)
	}
}

// ---------------------------------------------------------------------------
// Calendar object resources
// ---------------------------------------------------------------------------

// eventDoc is an event with no SEQUENCE, which is what most clients send and is
// allowed to mean nothing at all about which revision is being written.
func eventDoc(uid, summary string, start time.Time) string {
	return eventDocSequence(uid, summary, start, -1)
}

// eventDocSequence is an event carrying a SEQUENCE. A negative one omits the
// property rather than writing a negative number.
func eventDocSequence(uid, summary string, start time.Time, sequence int) string {
	seq := ""
	if sequence >= 0 {
		seq = fmt.Sprintf("SEQUENCE:%d\n", sequence)
	}
	return fmt.Sprintf(`BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//linchange//test//EN
BEGIN:VEVENT
UID:%s
DTSTAMP:20260101T090000Z
DTSTART:%s
DTEND:%s
%sSUMMARY:%s
END:VEVENT
END:VCALENDAR
`, uid, start.UTC().Format("20060102T150405Z"),
		start.Add(time.Hour).UTC().Format("20060102T150405Z"), seq, summary)
}

// A PUT stores the document and a GET returns it. The bytes come back as they
// went in: re-serialising through this server's parser would drop properties the
// schema does not model, and a client would see the loss as the server having
// changed the event.
func TestPutThenGetAnEvent(t *testing.T) {
	h := newHarness(t)

	doc := eventDoc("ev-1", "Standup", time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC))

	resp, body := h.do("PUT", calPath("Work")+"ev-1.ics", doc, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT: %d %s", resp.StatusCode, body)
	}
	if resp.Header.Get("ETag") == "" {
		t.Error("the PUT returned no ETag, so the client cannot write the event again conditionally")
	}

	resp, got := h.do("GET", calPath("Work")+"ev-1.ics", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET: %d %s", resp.StatusCode, got)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/calendar") {
		t.Errorf("Content-Type is %q", resp.Header.Get("Content-Type"))
	}
	if got != doc {
		t.Errorf("the document came back different:\n got: %q\nwant: %q", got, doc)
	}
	if resp.Header.Get("ETag") == "" {
		t.Error("the GET returned no ETag")
	}
}

// If-Match is what makes two clients editing one event safe. Without it there is
// one surviving edit and neither party told.
func TestAStaleWriteIsRefused(t *testing.T) {
	h := newHarness(t)

	start := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	if resp, body := h.do("PUT", calPath("Work")+"ev-1.ics", eventDoc("ev-1", "Standup", start), nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT: %d %s", resp.StatusCode, body)
	}

	// Somebody else writes first.
	revised := eventDoc("ev-1", "Standup (moved)", start)
	if resp, body := h.do("PUT", calPath("Work")+"ev-1.ics", revised, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("the revision: %d %s", resp.StatusCode, body)
	}

	// Now a client writing from the copy it read before that change.
	stale := eventDoc("ev-1", "Standup (my edit)", start)
	resp, body := h.do("PUT", calPath("Work")+"ev-1.ics", stale, map[string]string{"If-Match": `"seq-0"`})
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("the stale write got %d, want 412: %s", resp.StatusCode, body)
	}
	// The refusal names the tag that is actually stored so the client can retry
	// without guessing what it should have sent.
	if resp.Header.Get("ETag") == `"seq-0"` {
		t.Error("the refusal reported the client's stale tag as the current one")
	}

	_, got := h.do("GET", calPath("Work")+"ev-1.ics", "", nil)
	if strings.Contains(got, "my edit") {
		t.Errorf("the stale write took effect: %s", got)
	}
}

// A matching If-Match is the other half: the client says what it read and the
// write goes through.
func TestAFreshWriteIsAccepted(t *testing.T) {
	h := newHarness(t)

	start := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	resp, _ := h.do("PUT", calPath("Work")+"ev-1.ics", eventDoc("ev-1", "Standup", start), nil)
	tag := resp.Header.Get("ETag")
	if tag == "" {
		t.Fatal("no ETag on the create")
	}

	resp, body := h.do("PUT", calPath("Work")+"ev-1.ics",
		eventDoc("ev-1", "Standup (revised)", start), map[string]string{"If-Match": tag})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("got %d, want 204 for a replacement: %s", resp.StatusCode, body)
	}
	if resp.Header.Get("ETag") == tag {
		t.Error("the ETag did not change after the event was revised, so the two revisions are indistinguishable")
	}
}

// If-None-Match: * is how two clients can each create the same event without one
// overwriting the other.
func TestCreatingOverAnExistingEventIsRefused(t *testing.T) {
	h := newHarness(t)

	start := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	if resp, body := h.do("PUT", calPath("Work")+"ev-1.ics", eventDoc("ev-1", "Standup", start), nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("the first PUT: %d %s", resp.StatusCode, body)
	}

	resp, body := h.do("PUT", calPath("Work")+"ev-1.ics",
		eventDoc("ev-1", "Something else", start), map[string]string{"If-None-Match": "*"})
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("got %d, want 412: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "resource-must-be-null") {
		t.Errorf("the refusal does not name DAV:resource-must-be-null: %s", body)
	}
}

// A URL naming one UID and a body naming another would leave a resource the sync
// reports under one name and holds the document for another.
func TestTheNameAndTheUIDMustAgree(t *testing.T) {
	h := newHarness(t)

	start := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	resp, body := h.do("PUT", calPath("Work")+"wrong.ics", eventDoc("ev-9", "Standup", start), nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("got %d, want 409: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "no-uid-conflict") {
		t.Errorf("the refusal does not name DAV:no-uid-conflict: %s", body)
	}
}

// A document older than the stored one is a client writing from a copy it made
// before somebody else's change, and it is refused even without a header.
func TestADocumentBehindTheStoredOneIsRefused(t *testing.T) {
	h := newHarness(t)

	start := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	if resp, body := h.do("PUT", calPath("Work")+"ev-1.ics",
		eventDocSequence("ev-1", "Standup", start, 3), nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}

	// A lower SEQUENCE than the stored document carries is a client writing from a
	// copy it took before somebody else's change, and storing it would undo that
	// change without either party finding out. RFC 5545 §3.8.7.4 gives SEQUENCE
	// for exactly this.
	resp, body := h.do("PUT", calPath("Work")+"ev-1.ics", eventDocSequence("ev-1", "Old", start, 1), nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("got %d, want 409: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "no-uid-conflict") {
		t.Errorf("the refusal does not name DAV:no-uid-conflict: %s", body)
	}

	// The same SEQUENCE is allowed: a client that changed nothing sends back the
	// sequence it read, and that is the ordinary case rather than a conflict.
	if resp, body := h.do("PUT", calPath("Work")+"ev-1.ics",
		eventDocSequence("ev-1", "Same", start, 3), nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("the same sequence: got %d, want 204: %s", resp.StatusCode, body)
	}
}

// A document with no SEQUENCE expresses no opinion about which revision it is
// writing, so it is stored. Treating the absence as SEQUENCE:0 would refuse the
// second write from every client that omits the property -- which is most of them,
// and would surface as a calendar that accepts an event once and then refuses to
// update it.
func TestADocumentWithNoSequenceIsNotBehind(t *testing.T) {
	h := newHarness(t)

	start := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	if resp, body := h.do("PUT", calPath("Work")+"ev-1.ics",
		eventDocSequence("ev-1", "Standup", start, 3), nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}

	resp, body := h.do("PUT", calPath("Work")+"ev-1.ics", eventDoc("ev-1", "Moved", start), nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("got %d, want 204: %s", resp.StatusCode, body)
	}

	_, listing := h.do("GET", calPath("Work")+"ev-1.ics", "", nil)
	if !strings.Contains(listing, "Moved") {
		t.Errorf("the change was not stored: %s", listing)
	}
}

// RFC 4918 §9.7.4: an empty body is a DELETE. Parsing it would fail and leave the
// resource in place while the client believed it was gone.
func TestAnEmptyBodyIsADelete(t *testing.T) {
	h := newHarness(t)

	start := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	if resp, body := h.do("PUT", calPath("Work")+"ev-1.ics", eventDoc("ev-1", "Standup", start), nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}

	resp, body := h.do("PUT", calPath("Work")+"ev-1.ics", "", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("got %d, want 204: %s", resp.StatusCode, body)
	}
	resp, _ = h.do("GET", calPath("Work")+"ev-1.ics", "", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("the event is still there: %d", resp.StatusCode)
	}
}

// A delete of something that is not there succeeds, so a client retrying after a
// dropped connection stops retrying.
func TestDeletingSomethingAbsentSucceeds(t *testing.T) {
	h := newHarness(t)

	resp, _ := h.do("DELETE", calPath("Work")+"never-existed.ics", "", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("got %d, want 204", resp.StatusCode)
	}
}

// A malformed PROPFIND is a bad request. Treating it as allprop would answer a
// question the client did not ask, with the most expensive answer available.
func TestAnUnparsablePropfindIsRefusedRatherThanTreatedAsAllprop(t *testing.T) {
	h := newHarness(t)

	resp, body := h.do("PROPFIND", home(), "<D:propfind><D:prop><D:displayname/>", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", resp.StatusCode, body)
	}
	if strings.Contains(body, "quota-available-bytes") {
		t.Errorf("the body looks like an allprop answer: %s", body)
	}
}

// ---------------------------------------------------------------------------
// REPORT
// ---------------------------------------------------------------------------

// calendar-query over a window is how a client shows a month.
func TestCalendarQueryOverATimeRange(t *testing.T) {
	h := newHarness(t)
	seedEvents(t, h,
		seedEvent{uid: "mar", summary: "March review", start: time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)},
		seedEvent{uid: "apr", summary: "April review", start: time.Date(2026, 4, 10, 9, 0, 0, 0, time.UTC)},
		seedEvent{uid: "long", summary: "Conference",
			start: time.Date(2026, 3, 28, 9, 0, 0, 0, time.UTC), end: time.Date(2026, 4, 2, 17, 0, 0, 0, time.UTC)},
	)

	_, body := h.do("REPORT", calPath("Work"), calendarQuery(
		"20260301T000000Z", "20260401T000000Z", "getetag", "calendar-data"), nil)

	doc := decode(t, []byte(body))
	got := doc.responses
	var hrefs []string
	for _, r := range got {
		if strings.HasSuffix(r.href, ".ics") {
			hrefs = append(hrefs, r.href)
		}
	}
	if len(hrefs) != 2 {
		t.Fatalf("got %d events in March, want 2 (the March one and the conference that starts in it): %s",
			len(hrefs), body)
	}
	for _, want := range []string{"mar", "long"} {
		found := false
		for _, href := range hrefs {
			if strings.Contains(href, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is missing from the answer: %v", want, hrefs)
		}
	}
	if strings.Contains(body, "April review") {
		t.Error("an event outside the window was returned")
	}
}

// A meeting that began before the window and ends inside it is in the window. It
// is the case that matching on DTSTART alone drops, and it is every multi-day
// event.
func TestAnEventOverlappingTheWindowIsIncluded(t *testing.T) {
	h := newHarness(t)
	seedEvents(t, h, seedEvent{
		uid: "overlap", summary: "Overlapping",
		start: time.Date(2026, 3, 30, 9, 0, 0, 0, time.UTC),
		end:   time.Date(2026, 4, 3, 17, 0, 0, 0, time.UTC),
	})

	_, body := h.do("REPORT", calPath("Work"), calendarQuery(
		"20260401T000000Z", "20260430T000000Z", "getetag"), nil)
	if !strings.Contains(body, "overlap") {
		t.Errorf("an event overlapping the window was left out: %s", body)
	}
}

// calendar-multiget fetches the handful of resources a change touched, which is
// far cheaper than re-querying a date range.
func TestCalendarMultiget(t *testing.T) {
	h := newHarness(t)
	seedEvents(t, h,
		seedEvent{uid: "one", summary: "One", start: time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)},
		seedEvent{uid: "two", summary: "Two", start: time.Date(2026, 3, 11, 9, 0, 0, 0, time.UTC)},
	)

	body := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<C:calendar-multiget xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:prop><D:getetag/><C:calendar-data/></D:prop>
  <D:href>%s</D:href>
</C:calendar-multiget>`, calPath("Work")+"one.ics")

	_, out := h.do("REPORT", calPath("Work"), body, nil)
	if !strings.Contains(out, "SUMMARY:One") {
		t.Errorf("the named event is missing: %s", out)
	}
	if strings.Contains(out, "SUMMARY:Two") {
		t.Errorf("an event that was not asked for was returned: %s", out)
	}
}

// A multiget is scoped to the collection it was sent to. Following an href into
// another collection turns a scoped request into a way to read whatever else the
// same principal can reach.
func TestMultigetWillNotFollowAnHrefIntoAnotherCollection(t *testing.T) {
	h := newHarness(t)
	seedEvents(t, h, seedEvent{uid: "mine", summary: "Mine",
		start: time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)})

	body := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<C:calendar-multiget xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:prop><C:calendar-data/></D:prop>
  <D:href>%s</D:href>
</C:calendar-multiget>`, calPath("Work")+"../elsewhere/x.ics")

	_, out := h.do("REPORT", calPath("Work"), body, nil)
	if strings.Contains(out, "SUMMARY:Mine") {
		t.Errorf("a traversal-shaped href reached an event: %s", out)
	}
}

// A filter on a property the event does not have does not match it. Getting this
// wrong fills a filtered view with exactly what the filter excluded.
func TestAPropFilterDoesNotMatchAbsentProperties(t *testing.T) {
	h := newHarness(t)
	seedEvents(t, h,
		seedEvent{uid: "titled", summary: "Budget review",
			start: time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)},
		seedEvent{uid: "untitled", summary: "",
			start: time.Date(2026, 3, 11, 9, 0, 0, 0, time.UTC)},
	)

	query := `<?xml version="1.0" encoding="utf-8"?>
<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:prop><D:getetag/></D:prop>
  <C:filter>
    <C:comp-filter name="VCALENDAR"><C:comp-filter name="VEVENT">
      <C:prop-filter name="SUMMARY">
        <C:text-match>Budget</C:text-match>
      </C:prop-filter>
    </C:comp-filter></C:comp-filter>
  </C:filter>
</C:calendar-query>`

	_, body := h.do("REPORT", calPath("Work"), query, nil)
	if !strings.Contains(body, "titled") {
		t.Errorf("the matching event is missing: %s", body)
	}
	if strings.Contains(body, "untitled") {
		t.Errorf("the untitled event was returned by a summary filter: %s", body)
	}
}

// Text matching folds case by default, because RFC 4791 §9.7.5 says so and a
// client that wants exactness asks for a collation.
func TestTextMatchingFoldsCaseByDefault(t *testing.T) {
	h := newHarness(t)
	seedEvents(t, h, seedEvent{uid: "upper", summary: "BUDGET REVIEW",
		start: time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)})

	query := `<?xml version="1.0" encoding="utf-8"?>
<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:prop><D:getetag/></D:prop>
  <C:filter>
    <C:comp-filter name="VCALENDAR"><C:comp-filter name="VEVENT">
      <C:prop-filter name="SUMMARY"><C:text-match>budget</C:text-match></C:prop-filter>
    </C:comp-filter></C:comp-filter>
  </C:filter>
</C:calendar-query>`

	_, body := h.do("REPORT", calPath("Work"), query, nil)
	if !strings.Contains(body, "upper") {
		t.Errorf("a case-folded match did not find the event: %s", body)
	}
}

// A param-filter on ATTENDEE is how a client asks for the meetings it is in.
func TestAParamFilterOnAttendee(t *testing.T) {
	h := newHarness(t)
	seedEvents(t, h,
		seedEvent{uid: "mine", summary: "Ours",
			start:     time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC),
			attendees: []string{"alice@example.com", "bob@example.com"}},
		seedEvent{uid: "theirs", summary: "Theirs",
			start:     time.Date(2026, 3, 11, 9, 0, 0, 0, time.UTC),
			attendees: []string{"bob@example.com"}},
	)

	query := `<?xml version="1.0" encoding="utf-8"?>
<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:prop><D:getetag/></D:prop>
  <C:filter>
    <C:comp-filter name="VCALENDAR"><C:comp-filter name="VEVENT">
      <C:param-filter name="ATTENDEE">
        <C:text-match>alice@example.com</C:text-match>
      </C:param-filter>
    </C:comp-filter></C:comp-filter>
  </C:filter>
</C:calendar-query>`

	_, body := h.do("REPORT", calPath("Work"), query, nil)
	if !strings.Contains(body, "mine") {
		t.Errorf("the event alice attends is missing: %s", body)
	}
	if strings.Contains(body, "theirs") {
		t.Errorf("an event alice does not attend was returned: %s", body)
	}
}

// A query with no filter element is a request the server cannot answer. Accepting
// it as "everything ever" would make a malformed request able to ask for the whole
// collection.
func TestACalendarQueryNeedsAFilter(t *testing.T) {
	h := newHarness(t)

	body := `<?xml version="1.0" encoding="utf-8"?>
<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:prop><D:getetag/></D:prop>
</C:calendar-query>`

	resp, out := h.do("REPORT", calPath("Work"), body, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", resp.StatusCode, out)
	}
	if !strings.Contains(out, "valid-filter") {
		t.Errorf("the refusal does not name DAV:valid-filter: %s", out)
	}
}

// A report this server does not implement must say so. Advertising a report set
// and then refusing one of it makes a client pick the one that fails.
func TestAnUnknownReportIsRefused(t *testing.T) {
	h := newHarness(t)

	body := `<?xml version="1.0" encoding="utf-8"?>
<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:prop><D:getetag/></D:prop>
</C:calendar-query>`
	// Both occurrences: renaming only the opening element leaves a closing tag for
	// something the document no longer contains, and the server would rightly
	// refuse it as malformed XML -- which tests the wrong thing.
	body = strings.ReplaceAll(body, "calendar-query", "addressbook-query")

	resp, out := h.do("REPORT", calPath("Work"), body, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", resp.StatusCode, out)
	}
	if !strings.Contains(out, "supported-report") {
		t.Errorf("the refusal does not name DAV:supported-report: %s", out)
	}
}

// Every report the collection advertises must be one it answers.
func TestTheAdvertisedReportsAreTheOnesImplemented(t *testing.T) {
	h := newHarness(t)

	_, body := h.do("PROPFIND", calPath("Work"), propfindBody("supported-report-set"), nil)
	for _, report := range []string{"calendar-query", "calendar-multiget", "sync-collection"} {
		if !strings.Contains(body, report) {
			t.Errorf("supported-report-set does not advertise %s: %s", report, body)
		}
	}
	if strings.Contains(body, "addressbook-query") {
		t.Errorf("supported-report-set advertises something this service does not host: %s", body)
	}
}

// ---------------------------------------------------------------------------
// Synchronisation
// ---------------------------------------------------------------------------

// An empty sync token means everything, and the response carries one to use next
// time.
func TestSyncCollectionFromTheBeginning(t *testing.T) {
	h := newHarness(t)
	seedEvents(t, h,
		seedEvent{uid: "one", summary: "One", start: time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)},
		seedEvent{uid: "two", summary: "Two", start: time.Date(2026, 3, 11, 9, 0, 0, 0, time.UTC)},
	)

	_, body := h.do("REPORT", calPath("Work"), syncReport(""), nil)
	doc := decode(t, []byte(body))

	var events int
	for _, r := range doc.responses {
		if strings.HasSuffix(r.href, ".ics") {
			events++
		}
	}
	if events != 2 {
		t.Errorf("got %d events, want 2: %s", events, body)
	}
	if doc.syncToken == "" {
		t.Errorf("the response carries no token to use next time: %s", body)
	}
}

// A token that is current means nothing has changed. Reporting the collection
// again would make a client that polls every minute re-download the calendar
// sixty times an hour for nothing.
func TestSyncWithACurrentTokenReportsNothing(t *testing.T) {
	h := newHarness(t)
	seedEvents(t, h, seedEvent{uid: "one", summary: "One",
		start: time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)})

	_, first := h.do("REPORT", calPath("Work"), syncReport(""), nil)
	token := decode(t, []byte(first)).syncToken
	if token == "" {
		t.Fatalf("no token in the first sync: %s", first)
	}

	_, second := h.do("REPORT", calPath("Work"), syncReport(token), nil)
	doc := decode(t, []byte(second))
	for _, r := range doc.responses {
		if strings.HasSuffix(r.href, ".ics") {
			t.Errorf("an event was returned for an unchanged collection: %s", second)
		}
	}
	if doc.syncToken != token {
		t.Errorf("the token changed without the collection changing: %q then %q", token, doc.syncToken)
	}
}

// After a change the token moves, so the next sync returns the event.
func TestSyncAfterAChange(t *testing.T) {
	h := newHarness(t)
	seedEvents(t, h, seedEvent{uid: "one", summary: "One",
		start: time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)})

	_, first := h.do("REPORT", calPath("Work"), syncReport(""), nil)
	token := decode(t, []byte(first)).syncToken

	if resp, body := h.do("PUT", calPath("Work")+"two.ics",
		eventDoc("two", "Two", time.Date(2026, 3, 11, 9, 0, 0, 0, time.UTC)), nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("the change was not stored: %d %s", resp.StatusCode, body)
	}

	_, second := h.do("REPORT", calPath("Work"), syncReport(token), nil)
	doc := decode(t, []byte(second))
	if len(doc.responses) != 1 || !strings.HasSuffix(doc.responses[0].href, "two.ics") {
		t.Fatalf("the changed event was not reported: %s", second)
	}
	if doc.syncToken == token {
		t.Error("the token did not move after a change, so the client would never see the next one")
	}
}

// A deleted event has to be reported as gone, and this is the case that a sync
// built out of what still exists cannot answer: the client applies what it is
// told, so an unmentioned member is one it keeps. Without this the event stays on
// every subscribed device until the user deletes it there by hand.
//
// It is reported as a 404 with no properties, because that is what RFC 6578 §3.6
// specifies and because properties for a resource the server has just said it does
// not have would be a lie the client might believe.
func TestSyncReportsADeletedEvent(t *testing.T) {
	h := newHarness(t)
	seedEvents(t, h,
		seedEvent{uid: "one", summary: "One", start: time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)},
		seedEvent{uid: "two", summary: "Two", start: time.Date(2026, 3, 11, 9, 0, 0, 0, time.UTC)},
	)

	_, first := h.do("REPORT", calPath("Work"), syncReport(""), nil)
	token := decode(t, []byte(first)).syncToken

	if resp, body := h.do("DELETE", calPath("Work")+"two.ics", "", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: got %d, want 204: %s", resp.StatusCode, body)
	}

	_, second := h.do("REPORT", calPath("Work"), syncReport(token), nil)
	doc := decode(t, []byte(second))
	if len(doc.responses) != 1 {
		t.Fatalf("got %d responses, want just the deletion: %s", len(doc.responses), second)
	}
	got := doc.responses[0]
	if !strings.HasSuffix(got.href, "two.ics") {
		t.Errorf("the wrong resource was reported: %s", second)
	}
	if got.wholeStatus != "404" {
		t.Errorf("got status %q for the deletion, want 404: %s", got.wholeStatus, second)
	}
	if len(got.statuses) != 0 {
		t.Errorf("a deletion was reported with properties: %s", second)
	}
}

// A change made outside CalDAV has to reach the client too. An event moved in the
// web interface or cancelled by the reply to an invitation is a change to the
// collection, and a server that only noticed the ones made over CalDAV would leave
// the phones showing a meeting that no longer exists.
func TestSyncReportsAChangeMadeElsewhere(t *testing.T) {
	h := newHarness(t)
	seedEvents(t, h, seedEvent{uid: "one", summary: "One",
		start: time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)})

	_, first := h.do("REPORT", calPath("Work"), syncReport(""), nil)
	token := decode(t, []byte(first)).syncToken

	// The store's own write, which is what the web interface and an iTIP reply
	// both go through -- not the CalDAV handler.
	ev, err := store.NewEvents().ByUID(context.Background(), h.db, h.cal.ID, "one", nil)
	if err != nil {
		t.Fatalf("load the event: %v", err)
	}
	moved := fmt.Sprintf(`BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//linchange//test//EN
BEGIN:VEVENT
UID:one
DTSTAMP:20260101T090000Z
DTSTART:20260310T090000Z
DTEND:20260310T100000Z
SUMMARY:Moved
END:VEVENT
END:VCALENDAR
`)
	if _, err := store.NewEvents().Upsert(context.Background(), h.db, store.WriteEventRequest{
		CalendarID: ev.CalendarID, UID: ev.UID, ICalBlob: []byte(moved),
		DTStart: ev.DTStart, DTEnd: ev.DTEnd, Timezone: ev.Timezone, AllDay: ev.AllDay,
		RRULE: ev.RRULE, Summary: "Moved", Sequence: ev.Sequence + 1,
	}); err != nil {
		t.Fatalf("edit the event: %v", err)
	}

	_, second := h.do("REPORT", calPath("Work"), syncReport(token), nil)
	doc := decode(t, []byte(second))
	if len(doc.responses) != 1 || !strings.HasSuffix(doc.responses[0].href, "one.ics") {
		t.Fatalf("a change made outside CalDAV was not reported: %s", second)
	}
}

// A token this server never issued must be refused with the precondition RFC 6578
// §3.6 names, so the client starts over rather than applying a partial delta on
// top of a state it no longer has.
func TestAnInvalidSyncTokenIsRefused(t *testing.T) {
	h := newHarness(t)

	resp, body := h.do("REPORT", calPath("Work"), syncReport("http://example.com/made-up"), nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("got %d, want 403: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "valid-sync-token") {
		t.Errorf("the refusal does not name DAV:valid-sync-token: %s", body)
	}
}

// ---------------------------------------------------------------------------
// Recurrence
// ---------------------------------------------------------------------------

// A master and an override share a UID and are two resources. Giving them one
// name would make a client keep the second and silently lose an occurrence.
func TestAMasterAndAnOverrideGetDistinctURLs(t *testing.T) {
	h := newHarness(t)
	seedEvents(t, h,
		seedEvent{uid: "series", summary: "Weekly",
			start: time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC),
			rrule: "FREQ=WEEKLY;COUNT=4"},
		seedEvent{uid: "series", summary: "Weekly (moved)",
			start:      time.Date(2026, 3, 9, 11, 0, 0, 0, time.UTC),
			recurrence: time.Date(2026, 3, 9, 9, 0, 0, 0, time.UTC),
			overrideOf: "series"},
	)

	_, body := h.do("REPORT", calPath("Work"), calendarQuery(
		"20260101T000000Z", "20270101T000000Z", "getetag"), nil)

	doc := decode(t, []byte(body))
	got := doc.responses
	var hrefs []string
	for _, r := range got {
		if strings.HasSuffix(r.href, ".ics") {
			hrefs = append(hrefs, r.href)
		}
	}
	if len(hrefs) != 2 {
		t.Fatalf("got %d resources, want the master and the override: %s", len(hrefs), body)
	}
	if hrefs[0] == hrefs[1] {
		t.Fatalf("both resources are at %q", hrefs[0])
	}
}

// A REPORT returns masters and overrides, not expanded occurrences. A client does
// its own expansion, and giving it occurrences would mean a series of forty comes
// back as forty resources that look like forty separate events.
func TestAReportReturnsObjectsNotOccurrences(t *testing.T) {
	h := newHarness(t)
	seedEvents(t, h, seedEvent{uid: "series", summary: "Weekly",
		start: time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC),
		rrule: "FREQ=WEEKLY;COUNT=40"})

	_, body := h.do("REPORT", calPath("Work"), calendarQuery(
		"20260101T000000Z", "20270101T000000Z", "getetag"), nil)

	doc := decode(t, []byte(body))
	got := doc.responses
	resources := 0
	for _, r := range got {
		if strings.HasSuffix(r.href, ".ics") {
			resources++
		}
	}
	if resources != 1 {
		t.Errorf("got %d resources for one 40-occurrence series, want 1", resources)
	}
}

// Deleting a repeating meeting means the whole series, which is what a client
// means by deleting it.
func TestDeletingTheMasterRemovesTheSeries(t *testing.T) {
	h := newHarness(t)
	seedEvents(t, h,
		seedEvent{uid: "series", summary: "Weekly",
			start: time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC),
			rrule: "FREQ=WEEKLY;COUNT=4"},
		seedEvent{uid: "series", summary: "Weekly (moved)",
			start:      time.Date(2026, 3, 9, 11, 0, 0, 0, time.UTC),
			recurrence: time.Date(2026, 3, 9, 9, 0, 0, 0, time.UTC),
			overrideOf: "series"},
	)

	resp, body := h.do("DELETE", calPath("Work")+"series.ics", "", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("got %d, want 204: %s", resp.StatusCode, body)
	}

	var left int
	if err := h.db.Get(&left, `SELECT count(*) FROM events WHERE uid = 'series'`); err != nil {
		t.Fatalf("count: %v", err)
	}
	if left != 0 {
		t.Errorf("%d rows of the series are still stored", left)
	}
}

// ---------------------------------------------------------------------------
// Authorisation
// ---------------------------------------------------------------------------

// Another user's calendar is not addressable. Without this check a client could
// read a colleague's calendar by guessing a path.
func TestAnotherUsersCalendarIsNotFound(t *testing.T) {
	h := newHarness(t)

	resp, body := h.do("PROPFIND", "/caldav/bob%40example.com/", propfindBody("displayname"),
		map[string]string{"Depth": "1"})
	// The path names another principal, which is refused before the calendar is
	// looked up at all.
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("got %d, want 403: %s", resp.StatusCode, body)
	}
}

// A principal may not address its own home under a name that is not its own, so
// bob cannot reach alice's calendar by writing her address with a different case
// or with whitespace around it.
func TestAHomeSetBelongsToTheCallerOnly(t *testing.T) {
	h := newHarness(t)

	for _, path := range []string{
		"/caldav/ALICE%40EXAMPLE.COM/",
		"/caldav/%20alice@example.com%20/",
		"/caldav/bob%40example.com/Work/",
	} {
		resp, body := h.doAs(aliceAddr, alicePass, "PROPFIND", path,
			propfindBody("displayname"), nil)
		if resp.StatusCode == http.StatusOK {
			t.Errorf("%s was readable by alice: %s", path, body)
		}
	}
}

// ---------------------------------------------------------------------------
// Interoperability details
// ---------------------------------------------------------------------------

// XML 1.0 has no way to represent a control character at all, so a SUMMARY
// carrying one cannot go into a calendar-data property in any conformant
// encoding. A client sends it raw, and the only options are to strip it or to emit
// a document the client cannot parse.
func TestAControlCharacterInAnEventDoesNotBreakTheResponse(t *testing.T) {
	h := newHarness(t)
	seedEvents(t, h, seedEvent{uid: "odd", summary: "Before\vAfter",
		start: time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)})

	_, body := h.do("REPORT", calPath("Work"), calendarQuery(
		"20260101T000000Z", "20270101T000000Z", "calendar-data"), nil)

	if strings.ContainsRune(body, '\v') {
		t.Error("the response carries the vertical tab, so no XML parser can read it")
	}
	// The document must still parse, which is the property that matters.
	var doc davBody
	if err := xml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("the response does not parse: %v\n%s", err, body)
	}
	if !strings.Contains(body, "Before") || !strings.Contains(body, "After") {
		t.Errorf("the text around the control character was lost: %s", body)
	}
}

// Two identical responses must be byte-identical, because a client that diffs
// them to decide what changed is looking at the whole body, and a map's iteration
// order would make every one of them look different from the last.
func TestPropnameIsOrderedTheSameWayEveryTime(t *testing.T) {
	h := newHarness(t)

	_, first := h.do("PROPFIND", calPath("Work"), "<?xml version=\"1.0\"?><D:propfind xmlns:D=\"DAV:\"><D:propname/></D:propfind>", nil)
	for i := 0; i < 5; i++ {
		_, again := h.do("PROPFIND", calPath("Work"), "<?xml version=\"1.0\"?><D:propfind xmlns:D=\"DAV:\"><D:propname/></D:propfind>", nil)
		if again != first {
			t.Fatalf("two propname responses differ:\n%s\n%s", first, again)
		}
	}
	if !strings.Contains(first, "calendar-description") {
		t.Errorf("propname did not list what the collection has: %s", first)
	}
	if strings.Contains(first, "<") && strings.Contains(first, "getetag") {
		t.Errorf("propname listed a property that does not exist on a collection: %s", first)
	}
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

type seedEvent struct {
	uid        string
	summary    string
	start, end time.Time
	recurrence time.Time
	rrule      string
	attendees  []string
	overrideOf string
}

func seedEvents(t *testing.T, h *harness, events ...seedEvent) {
	t.Helper()

	ctx := context.Background()
	for _, e := range events {
		if e.end.IsZero() {
			e.end = e.start.Add(time.Hour)
		}
		doc := ical.Event{
			UID: e.uid, Summary: e.summary,
			Start: e.start, End: e.end,
			RRULE:     e.rrule,
			Organizer: ical.Organizer{Address: aliceAddr},
		}
		if !e.recurrence.IsZero() {
			id := e.recurrence
			doc.RecurrenceID = &id
		}
		for _, a := range e.attendees {
			doc.Attendees = append(doc.Attendees, ical.Attendee{
				Address: a, PartStat: "ACCEPTED",
			})
		}
		blob := doc.Marshal()
		var recurrence *time.Time
		if !e.recurrence.IsZero() {
			recurrence = &e.recurrence
		}
		if _, err := store.NewEvents().Create(ctx, h.db, store.WriteEventRequest{
			CalendarID:       h.cal.ID,
			UID:              e.uid,
			RecurrenceID:     recurrence,
			ICalBlob:         blob,
			DTStart:          e.start,
			DTEnd:            e.end,
			Timezone:         "UTC",
			Summary:          e.summary,
			Organizer:        "Alice Example",
			OrganizerAddress: aliceAddr,
			RRULE:            emptyToNil(e.rrule),
			Attendees:        attendeeRows(e.attendees),
		}); err != nil {
			t.Fatalf("seed %s: %v", e.uid, err)
		}
	}
}

func attendeeRows(addresses []string) []store.Attendee {
	var out []store.Attendee
	for _, a := range addresses {
		out = append(out, store.Attendee{Address: a, PartStat: "ACCEPTED"})
	}
	return out
}

func emptyToNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func propfindBody(props ...string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	b.WriteString(`<D:propfind xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav" xmlns:A="http://apple.com/ns/ical/">` + "\n")
	b.WriteString("  <D:prop>")
	for _, p := range props {
		switch p {
		case "calendar-description", "supported-calendar-component-set", "calendar-data":
			b.WriteString("<C:" + p + "/>")
		case "calendar-color":
			b.WriteString(`<A:calendar-color/>`)
		default:
			b.WriteString("<D:" + p + "/>")
		}
	}
	b.WriteString("</D:prop>\n</D:propfind>\n")
	return b.String()
}

func mkCalendarBody(displayName string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<C:mkcalendar xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:set><D:prop>
    <D:resourcetype><D:collection/><C:calendar/></D:resourcetype>
    <D:displayname>%s</D:displayname>
    <C:supported-calendar-component-set><C:comp name="VEVENT"/></C:supported-calendar-component-set>
  </D:prop></D:set>
</C:mkcalendar>`, displayName)
}

func calendarQuery(start, end string, props ...string) string {
	return `<?xml version="1.0" encoding="utf-8"?>
<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:prop>` + propElements(props) + `</D:prop>
  <C:filter>
    <C:comp-filter name="VCALENDAR">
      <C:comp-filter name="VEVENT">
        <C:time-range start="` + start + `" end="` + end + `"/>
      </C:comp-filter>
    </C:comp-filter>
  </C:filter>
</C:calendar-query>`
}

func propElements(props []string) string {
	var b strings.Builder
	for _, p := range props {
		switch p {
		case "calendar-data":
			b.WriteString("<C:calendar-data/>")
		default:
			b.WriteString("<D:" + p + "/>")
		}
	}
	return b.String()
}

func syncReport(token string) string {
	return `<?xml version="1.0" encoding="utf-8"?>
<D:sync-collection xmlns:D="DAV:">
  <D:sync-token>` + token + `</D:sync-token>
  <D:sync-level>1</D:sync-level>
  <D:prop><D:getetag/></D:prop>
</D:sync-collection>`
}

// A calendar shared with somebody is listed in their home set and reachable
// under its owner's URL. The URL names the calendar's home, not the viewer; the
// viewer's own credentials and the share decide whether they may read or write.
func TestSharedCalendarIsListedAndReachableByTheSubject(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	address := bobAddr
	share := store.CalendarShare{
		CalendarID:     h.cal.ID,
		SubjectKind:    store.ShareSubjectUser,
		SubjectAddress: &address,
		Level:          store.CalendarShareRead,
		Status:         store.CalendarShareActive,
	}
	if _, err := store.NewCalendarShares().Put(ctx, h.db, share); err != nil {
		t.Fatalf("sharing the calendar: %v", err)
	}

	bobHome := "/caldav/bob%40example.com/"
	aliceCal := "/caldav/alice%40example.com/" + h.cal.Name + "/"

	resp, body := h.doAs(bobAddr, bobPass, "PROPFIND", bobHome,
		propfindBody("displayname", "resourcetype"), map[string]string{"Depth": "1"})
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("bob's home = %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, aliceCal) {
		t.Fatalf("the shared calendar is not listed in bob's home: %s", body)
	}

	resp, body = h.doAs(bobAddr, bobPass, "PROPFIND", aliceCal,
		propfindBody("displayname", "resourcetype"), map[string]string{"Depth": "0"})
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("bob reading the shared calendar = %d %s", resp.StatusCode, body)
	}

	// Read means read: a write is refused until the share is raised.
	resp, body = h.doAs(bobAddr, bobPass, "PUT", aliceCal+"shared-1.ics", sharedEvent,
		map[string]string{"Content-Type": "text/calendar"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bob writing to a read-only share = %d %s, want 403", resp.StatusCode, body)
	}

	share.Level = store.CalendarShareWrite
	if _, err := store.NewCalendarShares().Put(ctx, h.db, share); err != nil {
		t.Fatalf("raising the share: %v", err)
	}
	resp, body = h.doAs(bobAddr, bobPass, "PUT", aliceCal+"shared-1.ics", sharedEvent,
		map[string]string{"Content-Type": "text/calendar"})
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("bob writing to a write share = %d %s", resp.StatusCode, body)
	}
}

const sharedEvent = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\nBEGIN:VEVENT\r\nUID:shared-1\r\nDTSTAMP:20260101T000000Z\r\nDTSTART:20260102T100000Z\r\nDTEND:20260102T110000Z\r\nSUMMARY:Shared\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
