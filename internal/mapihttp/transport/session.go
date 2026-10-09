package transport

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/linchange/linchange/internal/mapihttp/nspi"
	"github.com/linchange/linchange/internal/mapihttp/rop"
)

const (
	// sessionIdle is how long a session survives without a request: the
	// X-ExpirationInfo the server advertises (900 s) plus the grace the
	// reference allows before it reaps an idle session (60 s). A client
	// that keeps the session alive is told the same number it is held to,
	// so it can PING comfortably before it rather than exactly when it.
	sessionIdle = 900*time.Second + 60*time.Second

	// sidLength is a session id's length in characters.
	sidLength = 32
)

// session is one Session Context (MS-OXCMAPIHTTP 3.1.5).
//
// username is the authenticated account it was created for: the context and
// the credentials that established it have to stay the same pair, because
// the context is what carries the right to read a mailbox and a cookie is
// only as private as the header it travels in.
//
// sequence is the request-sequence cookie, rotated after most requests so
// that a request the client never sent cannot be replayed into this session
// by anyone who saw it.
//
// rop and nspi are the per-endpoint state: an EMSMDB session holds the handle
// table a ROP list is applied to, an NSPI session the address book's caches.
// Only the endpoint that created a session ever reads its own field.
type session struct {
	username string
	sequence string
	expires  time.Time
	rop      *rop.Session
	nspi     *nspi.Session
}

// sessionsFor returns the endpoint's session table. The two endpoints have
// one each because they are two Session Contexts, not one: a client holds a
// context with the mailbox endpoint and a separate one with the address book
// endpoint, and a sid that arrives at the wrong one is a sid for a context
// that does not exist there.
func (t *Transport) sessionsFor(endpoint string) map[string]*session {
	if endpoint == endpointEMSMDB {
		return t.emsmdbSessions
	}
	return t.nspiSessions
}

// lookupSession finds a live session by its sid, reaping the ones whose time
// has passed on the way.
//
// There is no background sweeper: a session is only ever looked at when a
// request names it, so dropping the expired ones as they are passed is enough
// to keep the table the size of the number of clients actually talking to it.
func (t *Transport) lookupSession(endpoint, sid string) (*session, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	table := t.sessionsFor(endpoint)
	now := time.Now()
	for k, s := range table {
		if s.expires.Before(now) {
			delete(table, k)
		}
	}
	s, ok := table[sid]
	if !ok {
		return nil, false
	}
	return s, true
}

// putSession records a newly established session.
func (t *Transport) putSession(endpoint, sid string, s *session) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sessionsFor(endpoint)[sid] = s
}

// dropSession forgets a session, which is what Disconnect and Unbind do.
func (t *Transport) dropSession(endpoint, sid string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.sessionsFor(endpoint), sid)
}

// newSID mints a session id: 32 characters of lowercase hex, a form the
// reference's own ids share. It is random rather than derived from the
// account name and the clock the way the reference derives it, because the
// id is the credential that names the session and an id anybody could
// recompute would not protect anything.
func newSID() (string, error) {
	var b [sidLength / 2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// newSequence mints a request-sequence value.
//
// The reference makes it a GUID and validates the value a client sends back
// as one, so this is a GUID too: same shape on the wire, same rejection of a
// mangled or invented value.
func newSequence() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// isSequence reports whether a sequence cookie is in the shape this server
// issues. A value that is not is the client handing back something other
// than what it was given — a forged cookie or a mangled one, both of which
// are an invalid context cookie rather than an absent sequence.
func isSequence(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch i {
		case 8, 13, 18, 23:
			if s[i] != '-' {
				return false
			}
		default:
			c := s[i]
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
				return false
			}
		}
	}
	return true
}

// parseCookies reads a Cookie header into its names and values. Only the
// two cookies this protocol defines are ever asked for, and a header with no
// semicolons at all is still one cookie, which is how the oldest clients
// send it.
func parseCookies(header string) map[string]string {
	out := make(map[string]string)
	for _, part := range strings.Split(header, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(name)] = strings.TrimSpace(value)
	}
	return out
}
