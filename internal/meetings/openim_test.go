package meetings

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// openIMStub answers the Platform API the way the real server does: HTTP 200
// with the business answer in errCode, and a refusal recorded for every
// request missing the headers every real endpoint demands. The recorded
// problems are asserted after the calls rather than failing inside the
// handler, because the handler runs on a goroutine the test does not own.
type openIMStub struct {
	mu sync.Mutex

	adminTokenCalls int
	registerBodies  []map[string]json.RawMessage
	userTokenBodies []map[string]any
	groupBodies     []map[string]json.RawMessage
	headerProblems  []string

	// registerErrCode is what user_register answers with. 1102 is
	// "already registered", which the client must treat as success.
	registerErrCode int
}

func (s *openIMStub) problemf(format string, args ...any) {
	s.headerProblems = append(s.headerProblems, strings.TrimSpace(fmt.Sprintf(format, args...)))
}

func (s *openIMStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Every Platform API request carries these. Content-Type in particular:
	// the server reads the body as JSON only when the type says so.
	if ct := r.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		s.problemf("%s: Content-Type = %q", r.URL.Path, ct)
	}
	if r.Header.Get("operationID") == "" {
		s.problemf("%s: no operationID", r.URL.Path)
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	reply := func(v string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(v))
	}

	switch r.URL.Path {
	case "/auth/get_admin_token":
		s.adminTokenCalls++
		// The admin token endpoint is the one request that carries no
		// token: the secret is the credential.
		if tok := r.Header.Get("token"); tok != "" {
			s.problemf("get_admin_token carried token %q", tok)
		}
		var req struct {
			Secret string `json:"secret"`
			UserID string `json:"userID"`
		}
		if err := json.Unmarshal(body, &req); err != nil || req.Secret == "" || req.UserID == "" {
			s.problemf("get_admin_token body = %s", body)
		}
		reply(`{"errCode":0,"errMsg":"","data":{"token":"admin-token","expireTimeSeconds":3600}}`)

	case "/user/user_register":
		if tok := r.Header.Get("token"); tok != "admin-token" {
			s.problemf("user_register token = %q", tok)
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(body, &payload); err != nil {
			s.problemf("user_register body = %s", body)
			reply(`{"errCode":1,"errMsg":"bad json"}`)
			return
		}
		if _, ok := payload["users"]; !ok {
			// The flat {"userID":...} shape is what a client that has
			// not read the API sends; the server ignores it silently
			// and the user is never provisioned.
			s.problemf("user_register body has no users array: %s", body)
		}
		s.registerBodies = append(s.registerBodies, payload)
		if s.registerErrCode != 0 {
			reply(`{"errCode":1102,"errMsg":"RegisteredAlreadyError"}`)
			return
		}
		reply(`{"errCode":0,"errMsg":""}`)

	case "/auth/get_user_token":
		var req struct {
			PlatformID int    `json:"platformID"`
			UserID     string `json:"userID"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			s.problemf("get_user_token body = %s", body)
		}
		s.userTokenBodies = append(s.userTokenBodies, map[string]any{
			"platformID": req.PlatformID, "userID": req.UserID,
		})
		reply(`{"errCode":0,"errMsg":"","data":{"token":"sdk-token"}}`)

	case "/group/create_group":
		if tok := r.Header.Get("token"); tok != "admin-token" {
			s.problemf("create_group token = %q", tok)
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(body, &payload); err != nil {
			s.problemf("create_group body = %s", body)
			reply(`{"errCode":1,"errMsg":"bad json"}`)
			return
		}
		s.groupBodies = append(s.groupBodies, payload)
		reply(`{"errCode":0,"errMsg":""}`)

	default:
		s.problemf("unknown path %s", r.URL.Path)
		reply(`{"errCode":404,"errMsg":"unknown"}`)
	}
}

// newOpenIMStub starts a stub Platform API and a client pointed at it.
func newOpenIMStub(t *testing.T, stub *openIMStub) *OpenIMClient {
	t.Helper()
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	return NewOpenIMClient(OpenIMConfig{
		APIAddr:     srv.URL,
		WSAddr:      "ws://openim.invalid:10001",
		AdminUserID: "imAdmin",
		Secret:      "openIM123",
		HTTPClient:  srv.Client(),
	})
}

// TestOpenIMSendsTheShapesTheServerDemands walks one provisioning round - a
// token, two users, a token for one of them, a room - and asserts the payload
// shapes against what the live OpenIM server answered when each of these was
// discovered the hard way. Every one of these fields was a silent failure
// before it was asserted: the server answers HTTP 200 to a body it ignored.
func TestOpenIMSendsTheShapesTheServerDemands(t *testing.T) {
	stub := &openIMStub{}
	c := newOpenIMStub(t, stub)
	ctx := context.Background()

	alice := ImUserID("alice@example.com")
	bob := ImUserID("bob@example.com")
	if alice == bob {
		t.Fatalf("ImUserID collapsed two addresses into %q", alice)
	}
	if err := c.EnsureUser(ctx, alice, "Alice Example"); err != nil {
		t.Fatalf("EnsureUser(alice): %v", err)
	}
	if err := c.EnsureUser(ctx, bob, "Bob Example"); err != nil {
		t.Fatalf("EnsureUser(bob): %v", err)
	}

	// The administrator token is cached: two registrations cost one token
	// request, or every guest list would re-auth against the server.
	if stub.adminTokenCalls != 1 {
		t.Errorf("admin token requested %d times for two users, want 1", stub.adminTokenCalls)
	}
	if len(stub.registerBodies) != 2 {
		t.Fatalf("register calls = %d, want 2", len(stub.registerBodies))
	}
	// The payload is {"users":[{...}]}, not a flat user: the flat shape is
	// accepted by the endpoint and silently provisions nobody.
	for _, body := range stub.registerBodies {
		raw, ok := body["users"]
		if !ok {
			t.Errorf("register payload has no users array: %v", body)
			continue
		}
		var users []struct {
			UserID   string `json:"userID"`
			Nickname string `json:"nickname"`
		}
		if err := json.Unmarshal(raw, &users); err != nil {
			t.Errorf("users array: %v (%s)", err, raw)
			continue
		}
		if len(users) != 1 || users[0].UserID == "" || users[0].Nickname == "" {
			t.Errorf("users = %+v, want one entry with userID and nickname", users)
		}
	}

	// A user known to be registered is not registered again: a resend walks
	// the whole guest list, and that is not a reason to re-register the world.
	if err := c.EnsureUser(ctx, alice, "Alice Example"); err != nil {
		t.Fatalf("EnsureUser(alice) again: %v", err)
	}
	if len(stub.registerBodies) != 2 {
		t.Errorf("register calls after a repeat = %d, want 2", len(stub.registerBodies))
	}

	// The SDK token is minted for the Web platform (5). Any other platform id
	// produces a token the browser SDK refuses, and the refusal looks like a
	// broken login rather than a wrong request.
	token, err := c.UserToken(ctx, alice)
	if err != nil {
		t.Fatalf("UserToken: %v", err)
	}
	if token != "sdk-token" {
		t.Errorf("token = %q, want sdk-token", token)
	}
	if len(stub.userTokenBodies) != 1 {
		t.Fatalf("user token calls = %d, want 1", len(stub.userTokenBodies))
	}
	if got := stub.userTokenBodies[0]["platformID"]; got != 5 {
		t.Errorf("platformID = %v, want 5 (Web)", got)
	}
	if stub.userTokenBodies[0]["userID"] != alice {
		t.Errorf("userID = %v, want %s", stub.userTokenBodies[0]["userID"], alice)
	}

	// The group payload's fields live at the top level, with the group's own
	// fields nested under groupInfo. This shape was found by watching the
	// server answer 200 to a body it had ignored entirely.
	if err := c.CreateGroup(ctx, "mtg_abc", "Design review", alice, []string{bob}); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	if len(stub.groupBodies) != 1 {
		t.Fatalf("create_group calls = %d, want 1", len(stub.groupBodies))
	}
	group := stub.groupBodies[0]
	var owner string
	if err := json.Unmarshal(group["ownerUserID"], &owner); err != nil || owner != alice {
		t.Errorf("ownerUserID = %s, want %q (top level)", group["ownerUserID"], alice)
	}
	var members []string
	if err := json.Unmarshal(group["memberUserIDs"], &members); err != nil || len(members) != 1 || members[0] != bob {
		t.Errorf("memberUserIDs = %s, want [%q]", group["memberUserIDs"], bob)
	}
	var info struct {
		GroupID   string `json:"groupID"`
		GroupName string `json:"groupName"`
		GroupType int    `json:"groupType"`
	}
	if err := json.Unmarshal(group["groupInfo"], &info); err != nil {
		t.Fatalf("groupInfo: %v", err)
	}
	if info.GroupID != "mtg_abc" || info.GroupName != "Design review" || info.GroupType != 2 {
		t.Errorf("groupInfo = %+v", info)
	}
	// The organiser is the owner. Repeating them as a member is refused by
	// the server as a repeated member - the whole request, not just the line.
	for _, m := range members {
		if m == owner {
			t.Errorf("the owner %q is also a member", owner)
		}
	}
	if _, ok := group["sendMessage"]; !ok {
		t.Error("group payload has no sendMessage field")
	}

	if len(stub.headerProblems) > 0 {
		t.Errorf("requests the server would refuse:\n  %s",
			strings.Join(stub.headerProblems, "\n  "))
	}
}

// TestOpenIMTreatsAlreadyRegisteredAsSuccess pins the one business error the
// client translates: OpenIM answers a user it already has with errCode 1102
// rather than an idempotent success, and a meeting resend must not surface
// that as a failure.
func TestOpenIMTreatsAlreadyRegisteredAsSuccess(t *testing.T) {
	stub := &openIMStub{registerErrCode: errRegisteredAlready}
	c := newOpenIMStub(t, stub)
	ctx := context.Background()

	if err := c.EnsureUser(ctx, "alice_at_example_com_dead00", "Alice"); err != nil {
		t.Fatalf("EnsureUser on an existing user: %v", err)
	}
	// The success is remembered: the next call does not ask the server again.
	if err := c.EnsureUser(ctx, "alice_at_example_com_dead00", "Alice"); err != nil {
		t.Fatalf("EnsureUser again: %v", err)
	}
	if len(stub.registerBodies) != 1 {
		t.Errorf("register calls = %d, want 1", len(stub.registerBodies))
	}
	if len(stub.headerProblems) > 0 {
		t.Errorf("header problems: %v", stub.headerProblems)
	}
}

// TestImUserIDFitsTheOpenIMContract checks the mapping against the two rules
// OpenIM enforces on an id - the alphabet and the 64-character cap - and
// against the property the mapping exists for: different addresses stay
// different users even when their readable text flattens together.
func TestImUserIDFitsTheOpenIMContract(t *testing.T) {
	alphabet := regexp.MustCompile(`^[A-Za-z0-9_]+$`)

	addresses := []string{
		"alice@example.com",
		"Bob+tag@sub.example.co.uk",
		"a.b@c.example",
		"a@b.c.example",
		"very.long.local.part.for.a.serious.corporate.address@example.com",
		`weird"chars@exam\u{308}ple.com`,
	}
	seen := map[string]string{}
	for _, addr := range addresses {
		id := ImUserID(addr)
		if !alphabet.MatchString(id) {
			t.Errorf("ImUserID(%q) = %q, outside the [A-Za-z0-9_] alphabet", addr, id)
		}
		if len(id) > 64 {
			t.Errorf("ImUserID(%q) = %d characters, over the 64 cap", addr, len(id))
		}
		if id != ImUserID(addr) {
			t.Errorf("ImUserID(%q) is not stable: %q then %q", addr, id, ImUserID(addr))
		}
		if prev, dup := seen[id]; dup {
			t.Errorf("ImUserID(%q) and ImUserID(%q) both map to %q", prev, addr, id)
		}
		seen[id] = addr
	}

	// The readable part is the point of reading an OpenIM admin page at all.
	if got := ImUserID("alice@example.com"); !strings.HasPrefix(got, "alice_at_example_com_") {
		t.Errorf("ImUserID(alice@example.com) = %q, want a readable prefix", got)
	}
	// The hash suffix is over the full address, which is what keeps the two
	// flattening addresses above apart.
	if ImUserID("a.b@c.example") == ImUserID("a@b.c.example") {
		t.Error("two addresses that flatten alike became the same OpenIM user")
	}
	// Case does not create two accounts for one mailbox.
	if ImUserID("Alice@Example.COM") != ImUserID("alice@example.com") {
		t.Error("ImUserID differs by case")
	}
}
