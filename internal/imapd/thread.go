package imapd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/mail"
	"github.com/linchange/linchange/internal/store"
)

// threadAdoptionWindow bounds how far back a message may join an existing
// conversation.
//
// Without a bound, a "Re: Lunch" arriving three years after the last one would
// reopen a conversation with a hundred old messages in it. Thirty days is wider
// than any real reply chain and narrow enough that a stale subject cannot drag
// new mail into an old thread.
const threadAdoptionWindow = 30 * 24 * time.Hour

// adoptThread decides which conversation a message belongs to.
//
// A message that carries a References chain is unambiguous: the chain names its
// conversation, and ThreadKey has already used it. The interesting case is the
// message with no References at all, which every client is entitled to send and
// several do: a reply composed on a phone, one relayed through a mailing list,
// one whose headers were rewritten on the way. Such a message would otherwise
// open a conversation of its own for every reply, which is how a mailbox ends
// up with four threads called "Re: invoice".
//
// For those, the conversation is looked up rather than declared. Three
// conditions have to hold at once, and all three are needed:
//
//   - the normalised subject matches, which is the client's own claim about
//     which conversation this belongs to;
//   - the sender already appears in that conversation, so two unrelated
//     threads that happen to share a subject do not merge;
//   - the conversation is recent, so a stale subject cannot collect new mail.
//
// This is a heuristic and it is deliberately a narrow one. When a References
// chain exists it always wins, and a subject match alone never groups anything.
func adoptThread(ctx context.Context, tx *sqlx.Tx, mailboxID int64, meta mail.Metadata, internalDate time.Time) (string, error) {
	// References present means the sender told us where the message belongs.
	// Re-deciding it here could only contradict the sender.
	if len(meta.References) > 0 {
		return meta.ThreadKey, nil
	}
	// Without a subject there is nothing to match, and without a sender address
	// the conversation cannot be shared with anyone.
	subject := strings.TrimSpace(meta.ThreadSubject)
	sender := strings.TrimSpace(meta.FromAddress)
	if subject == "" || sender == "" {
		return meta.ThreadKey, nil
	}

	const find = `
		SELECT thread_key
		FROM messages
		WHERE mailbox_id = $1
		  AND thread_subject = $2
		  AND from_address = $3
		  AND internal_date > $4
		ORDER BY internal_date DESC, id DESC
		LIMIT 1`

	var adopted string
	err := tx.GetContext(ctx, &adopted, find, mailboxID, subject, sender,
		internalDate.Add(-threadAdoptionWindow))
	if err != nil {
		if store.IsNotFound(err) {
			return meta.ThreadKey, nil
		}
		return "", fmt.Errorf("store: look up conversation for thread subject: %w", err)
	}

	// An empty match would be indistinguishable from a NULL, and would put the
	// message in a conversation whose key is the empty string.
	if adopted == "" {
		return meta.ThreadKey, nil
	}
	return adopted, nil
}
