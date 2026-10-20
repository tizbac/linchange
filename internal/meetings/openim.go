// Package meetings turns a calendar event into a meeting: a chat room, an
// invitation that anybody can answer, and a link back to this server.
//
// The design keeps the calendar where the calendar belongs. A meeting is
// still a row in events with its guest list in attendees, so every existing
// client - CalDAV, EWS, EAS, the web calendar - sees it and answers it with
// the protocol it already speaks. What this package adds is everything those
// protocols do not carry: the OpenIM room the conversation happens in, the
// invitation email with a join link, and the per-guest token that lets
// somebody without an account here accept from a browser.
//
// OpenIM users are linchange users. Nothing creates an OpenIM account that is
// not one of ours: the administrator token lives only in this process, and the
// SDK tokens the browser uses are minted here after the session is already
// authenticated. An external invitee gets the invitation and the RSVP link,
// but no chat account, because they have no account to have one with.
package meetings

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// OpenIMClient talks to an OpenIM server's Platform API.
//
// It holds the administrator token in memory and renews it shortly before it
// expires, so the common path is one HTTP round trip rather than two. Every
// method degrades the same way: an unreachable OpenIM server fails that call
// and nothing else, because a meeting whose chat room could not be created is
// still a meeting whose invitation should go out.
type OpenIMClient struct {
	apiAddr     string
	wsAddr      string
	adminUserID string
	secret      string
	http        *http.Client

	mu        sync.Mutex
	token     string
	expires   time.Time
	ensured   map[string]bool // userIDs known to be registered
	ensuredMu sync.Mutex
}

// OpenIMConfig configures an OpenIMClient.
type OpenIMConfig struct {
	APIAddr     string
	WSAddr      string
	AdminUserID string
	Secret      string
	// HTTPClient overrides the client used for Platform API calls. Tests
	// inject one pointed at a stub server; a nil value means a client with a
	// ten-second timeout.
	HTTPClient *http.Client
}

// NewOpenIMClient builds a client. It does not contact the server: a
// deployment whose OpenIM is down still starts, and the first call reports
// the problem.
func NewOpenIMClient(cfg OpenIMConfig) *OpenIMClient {
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	return &OpenIMClient{
		apiAddr:     strings.TrimRight(cfg.APIAddr, "/"),
		wsAddr:      cfg.WSAddr,
		adminUserID: cfg.AdminUserID,
		secret:      cfg.Secret,
		http:        hc,
		ensured:     map[string]bool{},
	}
}

// WSAddr is the WebSocket gateway the browser SDK logs in through.
func (c *OpenIMClient) WSAddr() string { return c.wsAddr }

// APIAddr is the Platform API base, which the browser SDK also needs.
func (c *OpenIMClient) APIAddr() string { return c.apiAddr }

// openIMResponse is the envelope every Platform API endpoint answers with.
// errCode 0 is success; anything else is a business failure described by
// errMsg, even under HTTP 200.
type openIMResponse struct {
	ErrCode int             `json:"errCode"`
	ErrMsg  string          `json:"errMsg"`
	Data    json.RawMessage `json:"data"`
}

// APIError is a business failure from the OpenIM server: the request reached
// it and was refused. A transport problem is a different error entirely, and
// the distinction matters at the call sites - "this user already exists" is
// an APIError worth recognising, while a refused connection is not.
type APIError struct {
	Code int
	Msg  string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("openim: errCode %d: %s", e.Code, e.Msg)
}

func (r openIMResponse) err(op string) error {
	if r.ErrCode == 0 {
		return nil
	}
	return fmt.Errorf("openim: %s failed: %w", op, &APIError{Code: r.ErrCode, Msg: r.ErrMsg})
}

// errRegisteredAlready is OpenIM's answer when a user exists. It is not a
// failure here: provisioning a user is meant to be repeatable.
const errRegisteredAlready = 1102

// call performs one Platform API request with the administrator token.
func (c *OpenIMClient) call(ctx context.Context, op, path string, body any, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("openim: encoding %s request: %w", op, err)
	}

	token, err := c.adminToken(ctx)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiAddr+path, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("openim: building %s request: %w", op, err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("token", token)
	// operationID is how OpenIM correlates a request with its logs. Any unique
	// value satisfies it; the operation name plus a counter is enough.
	req.Header.Set("operationID", operationID())

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("openim: %s: %w", op, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("openim: reading %s response: %w", op, err)
	}
	var envelope openIMResponse
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("openim: %s answered %d with no JSON envelope: %w", op, resp.StatusCode, err)
	}
	if err := envelope.err(op); err != nil {
		return err
	}
	if out != nil && len(envelope.Data) > 0 {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return fmt.Errorf("openim: decoding %s response: %w", op, err)
		}
	}
	return nil
}

// adminToken returns a valid administrator token, renewing the cached one
// when it is within a minute of expiring.
func (c *OpenIMClient) adminToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.expires) {
		return c.token, nil
	}

	body := map[string]string{
		"secret": c.secret,
		"userID": c.adminUserID,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("openim: encoding admin token request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.apiAddr+"/auth/get_admin_token", bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("openim: building admin token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("operationID", operationID())

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("openim: requesting the administrator token: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("openim: reading the administrator token response: %w", err)
	}
	var envelope struct {
		openIMResponse
		Data struct {
			Token             string `json:"token"`
			ExpireTimeSeconds int64  `json:"expireTimeSeconds"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return "", fmt.Errorf("openim: the administrator token response has no JSON envelope: %w", err)
	}
	if err := envelope.err("get_admin_token"); err != nil {
		return "", err
	}
	if envelope.Data.Token == "" {
		return "", fmt.Errorf("openim: get_admin_token returned no token")
	}

	// Renew a minute early: a token that expires mid-request is a failure the
	// caller cannot distinguish from a network fault, and this one can be
	// avoided by asking again.
	c.token = envelope.Data.Token
	c.expires = time.Now().Add(time.Duration(envelope.Data.ExpireTimeSeconds)*time.Second - time.Minute)
	return c.token, nil
}

// EnsureUser registers a linchange account with OpenIM, once.
//
// Repeated calls for a user that exists succeed without touching the server
// again: the meeting path calls this for every guest on every invitation
// resend, and a resend is not a reason to re-register the world.
func (c *OpenIMClient) EnsureUser(ctx context.Context, userID, nickname string) error {
	c.ensuredMu.Lock()
	if c.ensured[userID] {
		c.ensuredMu.Unlock()
		return nil
	}
	c.ensuredMu.Unlock()

	body := map[string]any{
		"users": []map[string]string{{
			"userID":   userID,
			"nickname": nickname,
		}},
	}
	var raw json.RawMessage
	if err := c.call(ctx, "user_register", "/user/user_register", body, &raw); err != nil {
		// OpenIM answers an existing user with errCode 1102 rather than an
		// idempotent success. That is the same fact this cache exists to
		// state, so it is recorded as success rather than surfaced.
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Code != errRegisteredAlready {
			return err
		}
	}

	c.ensuredMu.Lock()
	c.ensured[userID] = true
	c.ensuredMu.Unlock()
	return nil
}

// UserToken mints an SDK login token for a user that exists in OpenIM.
func (c *OpenIMClient) UserToken(ctx context.Context, userID string) (string, error) {
	body := map[string]any{
		// 5 is the Web platform. The SDK refuses a token minted for another
		// platform, so this value is part of the contract with the browser.
		"platformID": 5,
		"userID":     userID,
	}
	var data struct {
		Token string `json:"token"`
	}
	if err := c.call(ctx, "get_user_token", "/auth/get_user_token", body, &data); err != nil {
		return "", err
	}
	if data.Token == "" {
		return "", fmt.Errorf("openim: get_user_token returned no token")
	}
	return data.Token, nil
}

// CreateGroup makes the chat room for one meeting.
//
// The group id is supplied by the caller and is the meeting's identity in
// OpenIM; OpenIM would happily generate one, but then there would be no way
// to find the room again from the meeting. The owner is the organiser, and
// must not also appear in the member list - OpenIM calls that a repeated
// member and refuses the whole request.
func (c *OpenIMClient) CreateGroup(ctx context.Context, groupID, name, ownerUserID string, memberUserIDs []string) error {
	body := map[string]any{
		"ownerUserID":   ownerUserID,
		"memberUserIDs": memberUserIDs,
		"adminUserIDs":  []string{},
		"sendMessage":   false,
		"groupInfo": map[string]any{
			"groupID":   groupID,
			"groupName": name,
			// 2 is the ordinary group type. OpenIM refuses anything else on
			// this endpoint.
			"groupType": 2,
		},
	}
	return c.call(ctx, "create_group", "/group/create_group", body, nil)
}

// operationID returns a value unique enough for OpenIM's request correlation.
func operationID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("linchange-%d", time.Now().UnixNano())
	}
	return "linchange-" + hex.EncodeToString(buf[:])
}

// ImUserID maps a mail address to the OpenIM user id it is provisioned as.
//
// OpenIM accepts only letters, digits and underscore, and caps the id at 64
// characters; a mail address contains separators of neither kind. The mapping
// keeps the readable shape where it is safe to - "alice@example.com" becomes
// "alice_at_example_com" - and then appends six hex digits of a hash of the
// full address. The suffix is what makes collisions unlikely rather than
// merely improbable by eye: a.b@c and a@b.c flatten to the same text and would
// otherwise be the same user. The readable prefix is what makes the OpenIM
// admin pages bearable, and is truncated before the suffix when an address is
// long enough to run into the cap.
func ImUserID(address string) string {
	lower := strings.ToLower(strings.TrimSpace(address))
	var b strings.Builder
	for _, r := range lower {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		case r == '@':
			b.WriteString("_at_")
		default:
			b.WriteRune('_')
		}
	}
	// The suffix is always seven characters: an underscore and six hex
	// digits. Whatever is left after it is room for the readable part.
	const maxLen, suffixLen = 64, 7
	readable := b.String()
	if len(readable) > maxLen-suffixLen {
		readable = readable[:maxLen-suffixLen]
	}
	return readable + "_" + hashSuffix(lower)
}

// hashSuffix is six hex digits of a SHA-256 over the address. Twenty-four
// bits is far more than the collision margin a mailbox directory needs, and
// taking it from a full digest means the suffix depends on every byte of the
// address rather than on a pattern somebody could stumble into.
func hashSuffix(address string) string {
	sum := sha256.Sum256([]byte(address))
	return hex.EncodeToString(sum[:3])
}
