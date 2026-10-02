package mail

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// replyPrefixes are the subject prefixes a mail client prepends when replying.
// They are matched case-insensitively and may be stacked, which is what a user
// replying to "Re: Fwd: Re: plan" actually produces.
var replyPrefixes = regexp.MustCompile(`(?i)^\s*(?:(?:re|aw|fw|fwd|wg|tr|rv|sv|vs|odp|antw|res)\s*(?:\[\d+\])?\s*:\s*)+`)

// whitespaceRun folds the space a client may or may not add after a prefix.
var subjectSpace = regexp.MustCompile(`[\s\p{Zs}]+`)

// NormaliseSubject reduces a subject to the part that identifies the
// conversation, so that "Re: plan", "RE: plan" and "Plan" land in one thread.
//
// The comparison is on a folded, lower-cased form. Folding matters because two
// clients disagree about whether to put a space after the prefix, and case
// matters not at all. What is deliberately *not* removed is the rest of the
// subject: a thread key built from the whole normalised subject still merges
// "plan" and "plan v2" only when they really are the same words.
func NormaliseSubject(subject string) string {
	stripped := replyPrefixes.ReplaceAllString(subject, "")
	folded := subjectSpace.ReplaceAllString(stripped, " ")
	folded = strings.TrimSpace(folded)
	return strings.ToLower(folded)
}

// ThreadKey returns the key that groups a message with its conversation.
//
// The root Message-ID of the References chain is the key when the message
// carries one, because that is the only identifier the whole conversation
// agrees on. Failing that, the message's own ID is used, and failing that, a
// key derived from the normalised subject, which is weaker but still groups the
// replies that arrived with their References stripped.
func ThreadKey(messageID string, references []string, subject string) string {
	if refs := normaliseIDs(references); len(refs) > 0 {
		return refs[0]
	}
	if ids := normaliseIDs([]string{messageID}); len(ids) > 0 {
		return ids[0]
	}
	return "subject:" + NormaliseSubject(subject)
}

// normaliseIDs trims the angle brackets from message IDs, drops empty ones and
// removes repeats while preserving order.
//
// The de-duplication is not cosmetic. A conforming reply repeats the last ID
// of its References chain in In-Reply-To, so the reference list is assembled
// from both headers; without this the same ID would be stored twice for every
// reply in every conversation.
func normaliseIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		trimmed := strings.Trim(strings.TrimSpace(id), "<>")
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		out = append(out, trimmed)
	}
	return out
}

// NewMessageID returns a globally unique Message-ID for a message this server
// originates. The domain part is the host the message claims to be from, so
// that conformant receivers attribute it correctly.
//
// The random part is what makes it unique: a timestamp plus a counter would
// collide between two replicas of this server started at the same moment.
func NewMessageID(domain string) string {
	domain = strings.TrimPrefix(strings.ToLower(domain), "@")
	if domain == "" || strings.ContainsAny(domain, " \t<>@") {
		domain = "localhost"
	}

	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		// A weak ID is still better than an empty one: the uniqueness that
		// matters for threading is the random component, and a failing system
		// RNG is not a reason to send no ID at all.
		binary.BigEndian.PutUint64(random[:8], uint64(time.Now().UnixNano()))
		binary.BigEndian.PutUint64(random[8:], uint64(idCounter.Add(1)))
	}
	return fmt.Sprintf("%x.%x@%s", random[:8], random[8:], domain)
}

// idCounter separates IDs minted while the system RNG is unavailable.
var idCounter atomic.Uint64
