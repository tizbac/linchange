package webmail

import (
	"context"
	"fmt"
	"strings"
)

// Folder returns one of an owner's folders by id, with its counts.
//
// It is the single-folder form of Folders. A client that already knows which
// folder it wants -- because the protocol it speaks names folders by identifier,
// not by position -- should not have to read every folder to find it, and the
// ownership check is the reason it cannot simply be given the id.
func (s *Service) Folder(ctx context.Context, owner string, id int64) (Folder, error) {
	return s.folderByID(ctx, owner, id)
}

// Recipients returns the header addresses of the named messages, keyed by message
// id.
//
// It exists because a listing needs the recipients of every message it shows and
// parsing each message body to get them would mean reading every blob on a
// request that is otherwise a table scan. The addresses were written when the
// message was filed, precisely so that they could be read here.
//
// The map is keyed by id rather than returned as a slice so that a caller can ask
// about a page of messages in one query and look the answers up in any order; the
// two orderings have no reason to agree.
func (s *Service) Recipients(ctx context.Context, owner string, ids []int64) (map[int64]HeaderAddresses, error) {
	if len(ids) == 0 {
		return map[int64]HeaderAddresses{}, nil
	}

	// The ownership check is a join against the owner's mailboxes rather than a
	// loop of single lookups: the whole point of the call is to answer about
	// many messages at once, and a loop would make it O(n) round trips.
	placeholders := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids)+1)
	args = append(args, strings.ToLower(owner))
	for i, id := range ids {
		placeholders = append(placeholders, fmt.Sprintf("$%d", i+2))
		args = append(args, id)
	}
	query := `
		SELECT a.message_id, a.header_name, a.address, a.display_name, a.position
		FROM message_addresses a
		JOIN messages m ON m.id = a.message_id
		JOIN mailboxes b ON b.id = m.mailbox_id
		WHERE lower(b.owner) = $1 AND a.message_id IN (` + strings.Join(placeholders, ",") + `)
		ORDER BY a.message_id, a.header_name, a.position`

	var rows []struct {
		ID      int64  `db:"message_id"`
		Header  string `db:"header_name"`
		Address string `db:"address"`
		Name    string `db:"display_name"`
		Order   int    `db:"position"`
	}
	if err := s.db.SelectContext(ctx, &rows, query, args...); err != nil {
		return nil, fmt.Errorf("webmail: read addresses: %w", err)
	}

	out := make(map[int64]HeaderAddresses, len(ids))
	for _, r := range rows {
		addr := Address{Address: r.Address, Name: r.Name}
		got := out[r.ID]
		switch strings.ToLower(r.Header) {
		case "from":
			got.From = append(got.From, addr)
		case "to":
			got.To = append(got.To, addr)
		case "cc":
			got.Cc = append(got.Cc, addr)
		case "bcc":
			got.Bcc = append(got.Bcc, addr)
		case "reply-to":
			got.ReplyTo = append(got.ReplyTo, addr)
		}
		out[r.ID] = got
	}
	return out, nil
}

// HeaderAddresses is one message's header addresses.
type HeaderAddresses struct {
	From    []Address `json:"from,omitempty"`
	To      []Address `json:"to,omitempty"`
	Cc      []Address `json:"cc,omitempty"`
	Bcc     []Address `json:"bcc,omitempty"`
	ReplyTo []Address `json:"reply_to,omitempty"`
}
