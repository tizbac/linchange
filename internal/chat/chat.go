// Package chat is linchange's own chat backend: conversations, messages, read
// watermarks and the live socket that carries them.
//
// It exists so that chat and meetings work by installing linchange alone. There
// is no external chat server to run, no second account system to keep in step,
// and no second set of identities that could disagree with the mailbox about
// who somebody is.
//
// A conversation is the unit a message belongs to. A direct message is a
// conversation with two members, a group has more, and a meeting's room is tied
// to the meeting's UID. One table serves all three, so membership, unread
// counts and read receipts are defined once rather than three times.
package chat

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/blobstore"
	"github.com/linchange/linchange/internal/store"
)

// Kinds of conversation.
const (
	KindDirect  = "direct"
	KindGroup   = "group"
	KindMeeting = "meeting"
)

// maxBody bounds one message. It is generous for prose and small enough that a
// client cannot use the chat as a file store; attachments are a separate
// feature with their own limits.
const maxBody = 16 * 1024

// ErrNotFound means the conversation does not exist.
var ErrNotFound = errors.New("chat: conversation not found")

// ErrNotMember means the caller is not on the conversation's member list. It is
// deliberately the same error whether the conversation does not exist or the
// caller may not see it, so a probe cannot enumerate rooms.
var ErrNotMember = errors.New("chat: not a member of this conversation")

// ErrInvalid means the request cannot be carried out: empty message, unknown
// kind, missing members.
var ErrInvalid = errors.New("chat: invalid request")

// Conversation is one room: a direct message, a group, or a meeting's chat.
type Conversation struct {
	ID         int64  `db:"id" json:"-"`
	UID        string `db:"uid" json:"uid"`
	Kind       string `db:"kind" json:"kind"`
	Title      string `db:"title" json:"title"`
	MeetingUID string `db:"meeting_uid" json:"meetingUid,omitempty"`
	GroupID    int64  `db:"group_id" json:"groupId,omitempty"`
	// OneWay marks a webinar: only Presenter speaks until the presenter grants
	// somebody the floor. Presenter is empty for every other kind of room.
	OneWay    bool      `db:"one_way" json:"oneWay,omitempty"`
	Presenter string    `db:"presenter" json:"presenter,omitempty"`
	CreatedBy string    `db:"created_by" json:"createdBy"`
	CreatedAt time.Time `db:"created_at" json:"createdAt"`
}

// Message is one chat message. A message may carry one file; the body may be
// empty when it does.
type Message struct {
	ID         int64       `db:"id" json:"id"`
	Sender     string      `db:"sender" json:"sender"`
	Body       string      `db:"body" json:"body"`
	CreatedAt  time.Time   `db:"created_at" json:"createdAt"`
	Attachment *Attachment `json:"attachment,omitempty"`
}

// Summary is a conversation as the conversation list draws it: the room, the
// last thing said in it, and how much of it this member has not read.
type Summary struct {
	Conversation
	Members     []string `json:"members"`
	LastMessage *Message `json:"lastMessage,omitempty"`
	Unread      int      `json:"unread"`
}

// Member is one person on a conversation, with the highest message they have
// read. The watermark is what a read receipt is made of.
type Member struct {
	Address    string    `db:"address" json:"member"`
	LastReadID int64     `db:"last_read_id" json:"lastReadId"`
	JoinedAt   time.Time `db:"joined_at" json:"joinedAt"`
}

// CreateRequest describes a conversation to open.
type CreateRequest struct {
	Kind       string
	Title      string
	Members    []string
	CreatedBy  string
	MeetingUID string
	// GroupID opens an organization group's room. Its members are read from
	// the group, so Members is not needed and is ignored when it is set.
	GroupID int64
	// OneWay and Presenter apply to a meeting room: a webinar has one speaker
	// and everybody else asks for the floor.
	OneWay    bool
	Presenter string
}

// Service is the chat store and the rules over it.
type Service struct {
	db     *sqlx.DB
	groups *store.Groups
	blobs  *blobstore.Store
	log    *slog.Logger
}

// New builds the service. A nil blob store disables attachments; every other
// chat operation still works.
func New(db *sqlx.DB, blobs *blobstore.Store, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{db: db, groups: store.NewGroups(), blobs: blobs, log: log}
}

// Create opens a conversation, or returns the existing one when it is a direct
// room, a meeting room, or an organization group's room that already has one.
//
// Direct rooms are keyed on the sorted pair of members, meeting rooms on the
// meeting UID and group rooms on the group id, so opening any of them twice is
// idempotent: a caller does not have to know whether the room exists before
// asking for it.
func (s *Service) Create(ctx context.Context, q store.Querier, req CreateRequest) (Conversation, error) {
	kind := strings.ToLower(strings.TrimSpace(req.Kind))
	createdBy := normalise(req.CreatedBy)
	members := normaliseMembers(req.Members)

	var group *store.WorkgroupGroup
	switch kind {
	case KindDirect:
		if len(members) != 2 {
			return Conversation{}, fmt.Errorf("%w: a direct message needs exactly two members", ErrInvalid)
		}
	case KindGroup:
		if req.GroupID != 0 {
			g, err := s.groups.Get(ctx, q, req.GroupID)
			if err != nil {
				return Conversation{}, fmt.Errorf("%w: no such group", ErrInvalid)
			}
			group = &g
		} else if len(members) < 2 {
			return Conversation{}, fmt.Errorf("%w: a group needs at least two members", ErrInvalid)
		}
	case KindMeeting:
		if strings.TrimSpace(req.MeetingUID) == "" {
			return Conversation{}, fmt.Errorf("%w: a meeting conversation needs a meeting", ErrInvalid)
		}
		if len(members) == 0 {
			return Conversation{}, fmt.Errorf("%w: a meeting conversation needs members", ErrInvalid)
		}
	default:
		return Conversation{}, fmt.Errorf("%w: unknown kind %q", ErrInvalid, kind)
	}
	if createdBy == "" {
		return Conversation{}, fmt.Errorf("%w: a creator is required", ErrInvalid)
	}

	// The creator is a member whether or not the caller remembered to list
	// themselves; a room you cannot open is not a room you created. A group
	// room ignores the list: its members are the group's.
	if group == nil {
		members = withMember(members, createdBy)
	}

	conv := Conversation{
		UID:       newUID(),
		Kind:      kind,
		Title:     strings.TrimSpace(req.Title),
		CreatedBy: createdBy,
		OneWay:    req.OneWay,
		Presenter: normalise(req.Presenter),
	}
	if group != nil && conv.Title == "" {
		conv.Title = group.Name
	}

	var dmKey, meetingUID, groupID any
	switch kind {
	case KindDirect:
		sorted := append([]string(nil), members...)
		if sorted[0] > sorted[1] {
			sorted[0], sorted[1] = sorted[1], sorted[0]
		}
		dmKey = strings.Join(sorted, "|")
	case KindMeeting:
		meetingUID = req.MeetingUID
	case KindGroup:
		if group != nil {
			// Creating the room for a group you are not in would hand you the
			// group's conversation; membership is the group's own answer.
			member, err := s.groups.IsMember(ctx, q, group.ID, createdBy)
			if err != nil || !member {
				return Conversation{}, fmt.Errorf("%w: you are not a member of this group", ErrInvalid)
			}
			groupID = group.ID
			members = nil
		}
	}

	// DO NOTHING rather than an upsert: when the room already exists the row's
	// uid must be the one the members already know, and RETURNING would have
	// handed back the new uid that was discarded.
	err := q.QueryRowxContext(ctx, `
		INSERT INTO chat_conversations
		    (uid, kind, title, dm_key, meeting_uid, group_id, one_way, presenter, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''), $9)
		ON CONFLICT DO NOTHING
		RETURNING id, uid, kind, title, COALESCE(meeting_uid, '') AS meeting_uid,
		          COALESCE(group_id, 0) AS group_id, one_way,
		          COALESCE(presenter, '') AS presenter, created_by, created_at`,
		conv.UID, conv.Kind, conv.Title, dmKey, meetingUID, groupID,
		conv.OneWay, conv.Presenter, conv.CreatedBy,
	).Scan(&conv.ID, &conv.UID, &conv.Kind, &conv.Title, &conv.MeetingUID, &conv.GroupID,
		&conv.OneWay, &conv.Presenter, &conv.CreatedBy, &conv.CreatedAt)

	if errors.Is(err, sql.ErrNoRows) {
		// The room exists. Find it by the key that made it unique.
		switch kind {
		case KindDirect:
			conv, err = s.byDMKey(ctx, q, dmKey.(string))
		case KindMeeting:
			conv, err = s.ByMeeting(ctx, q, req.MeetingUID)
		case KindGroup:
			if groupID != nil {
				conv, err = s.byGroupID(ctx, q, groupID.(int64))
			}
		}
		if err != nil {
			return Conversation{}, err
		}
	} else if err != nil {
		return Conversation{}, fmt.Errorf("chat: creating conversation: %w", err)
	}

	// A group room's members are the group's, read at use time, so there is
	// nothing to copy here.
	if len(members) > 0 {
		// Only a meeting room may contain an external guest. Direct and
		// ad-hoc group rooms are still a conversation between accounts on
		// this server, which is what the old foreign key used to enforce.
		if kind != KindMeeting {
			if err := s.requireUsers(ctx, q, members); err != nil {
				return Conversation{}, err
			}
		}
		if err := s.addMembers(ctx, q, conv.ID, members); err != nil {
			return Conversation{}, err
		}
	}
	return conv, nil
}

// requireUsers rejects an address that has no account here. It is the
// membership rule for rooms other than a meeting's.
func (s *Service) requireUsers(ctx context.Context, q store.Querier, members []string) error {
	for _, member := range members {
		var exists bool
		if err := q.GetContext(ctx, &exists,
			`SELECT EXISTS (SELECT 1 FROM users WHERE address = $1)`, member); err != nil {
			return fmt.Errorf("chat: checking member %s: %w", member, err)
		}
		if !exists {
			return fmt.Errorf("%w: %s is not a user of this server", ErrInvalid, member)
		}
	}
	return nil
}

// EnsureMember adds one address to a room if it is not there yet. It is what a
// meeting guest's token calls before opening the room, so an invitation issued
// before guest membership existed still works.
func (s *Service) EnsureMember(ctx context.Context, conversationID int64, member string) error {
	member = normalise(member)
	if member == "" {
		return fmt.Errorf("%w: a member is required", ErrInvalid)
	}
	return s.addMembers(ctx, s.db, conversationID, []string{member})
}

// addMembers adds the given addresses, leaving existing rows (and their read
// watermark) untouched.
func (s *Service) addMembers(ctx context.Context, q store.Querier, conversationID int64, members []string) error {
	for _, member := range members {
		if _, err := q.ExecContext(ctx, `
			INSERT INTO chat_members (conversation_id, member, user_address)
			VALUES ($1, $2, (SELECT address FROM users WHERE address = $2))
			ON CONFLICT (conversation_id, member) DO NOTHING`,
			conversationID, member); err != nil {
			return fmt.Errorf("chat: adding a member: %w", err)
		}
	}
	return nil
}

// ByUID loads a conversation by its public identifier.
func (s *Service) ByUID(ctx context.Context, q store.Querier, uid string) (Conversation, error) {
	var conv Conversation
	err := q.GetContext(ctx, &conv, `
		SELECT id, uid, kind, title, COALESCE(meeting_uid, '') AS meeting_uid,
		       COALESCE(group_id, 0) AS group_id, one_way,
		       COALESCE(presenter, '') AS presenter, created_by, created_at
		FROM chat_conversations WHERE uid = $1`, strings.TrimSpace(uid))
	if store.IsNotFound(err) {
		return Conversation{}, ErrNotFound
	}
	if err != nil {
		return Conversation{}, fmt.Errorf("chat: loading conversation: %w", err)
	}
	return conv, nil
}

// ByMeeting loads the conversation attached to a meeting.
func (s *Service) ByMeeting(ctx context.Context, q store.Querier, meetingUID string) (Conversation, error) {
	var conv Conversation
	err := q.GetContext(ctx, &conv, `
		SELECT id, uid, kind, title, COALESCE(meeting_uid, '') AS meeting_uid,
		       COALESCE(group_id, 0) AS group_id, one_way,
		       COALESCE(presenter, '') AS presenter, created_by, created_at
		FROM chat_conversations WHERE meeting_uid = $1`, strings.TrimSpace(meetingUID))
	if store.IsNotFound(err) {
		return Conversation{}, ErrNotFound
	}
	if err != nil {
		return Conversation{}, fmt.Errorf("chat: loading meeting conversation: %w", err)
	}
	return conv, nil
}

func (s *Service) byDMKey(ctx context.Context, q store.Querier, key string) (Conversation, error) {
	var conv Conversation
	err := q.GetContext(ctx, &conv, `
		SELECT id, uid, kind, title, COALESCE(meeting_uid, '') AS meeting_uid,
		       COALESCE(group_id, 0) AS group_id, one_way,
		       COALESCE(presenter, '') AS presenter, created_by, created_at
		FROM chat_conversations WHERE dm_key = $1`, key)
	if store.IsNotFound(err) {
		return Conversation{}, ErrNotFound
	}
	if err != nil {
		return Conversation{}, fmt.Errorf("chat: loading direct conversation: %w", err)
	}
	return conv, nil
}

func (s *Service) byGroupID(ctx context.Context, q store.Querier, groupID int64) (Conversation, error) {
	var conv Conversation
	err := q.GetContext(ctx, &conv, `
		SELECT id, uid, kind, title, COALESCE(meeting_uid, '') AS meeting_uid,
		       COALESCE(group_id, 0) AS group_id, one_way,
		       COALESCE(presenter, '') AS presenter, created_by, created_at
		FROM chat_conversations WHERE group_id = $1`, groupID)
	if store.IsNotFound(err) {
		return Conversation{}, ErrNotFound
	}
	if err != nil {
		return Conversation{}, fmt.Errorf("chat: loading group conversation: %w", err)
	}
	return conv, nil
}

// byID loads a conversation by its internal id, which callers that already hold
// a message or member row have.
func (s *Service) byID(ctx context.Context, id int64) (Conversation, error) {
	var conv Conversation
	err := s.db.GetContext(ctx, &conv, `
		SELECT id, uid, kind, title, COALESCE(meeting_uid, '') AS meeting_uid,
		       COALESCE(group_id, 0) AS group_id, one_way,
		       COALESCE(presenter, '') AS presenter, created_by, created_at
		FROM chat_conversations WHERE id = $1`, id)
	if store.IsNotFound(err) {
		return Conversation{}, ErrNotFound
	}
	if err != nil {
		return Conversation{}, fmt.Errorf("chat: loading conversation: %w", err)
	}
	return conv, nil
}

// ListFor returns the member's conversations, most recently active first.
//
// Organization-group rooms are included: every group the member belongs to has
// one, created the first time it is needed. Their membership is read from the
// group at this point rather than copied, so a room follows the group as people
// join and leave.
func (s *Service) ListFor(ctx context.Context, member string) ([]Summary, error) {
	member = normalise(member)

	groupIDs, err := s.ensureGroupConversations(ctx, member)
	if err != nil {
		return nil, err
	}

	rows := []struct {
		Conversation
		LastReadID int64        `db:"last_read_id"`
		Unread     int          `db:"unread"`
		LastID     int64        `db:"last_id"`
		LastSender string       `db:"last_sender"`
		LastBody   string       `db:"last_body"`
		LastAt     sql.NullTime `db:"last_at"`
	}{}

	err = s.db.SelectContext(ctx, &rows, `
		SELECT c.id, c.uid, c.kind, c.title, COALESCE(c.meeting_uid, '') AS meeting_uid,
		       COALESCE(c.group_id, 0) AS group_id, c.one_way,
		       COALESCE(c.presenter, '') AS presenter, c.created_by, c.created_at,
		       COALESCE(m.last_read_id, 0) AS last_read_id,
		       (SELECT count(*) FROM chat_messages cm
		           WHERE cm.conversation_id = c.id
		             AND cm.id > COALESCE(m.last_read_id, 0)
		             AND cm.sender <> $1) AS unread,
		       COALESCE(lm.id, 0) AS last_id,
		       COALESCE(lm.sender, '') AS last_sender,
		       COALESCE(lm.body, '') AS last_body,
		       lm.created_at AS last_at
		FROM chat_conversations c
		LEFT JOIN chat_members m ON m.conversation_id = c.id AND m.member = $1
		LEFT JOIN LATERAL (
		    SELECT id, sender, body, created_at
		    FROM chat_messages
		    WHERE conversation_id = c.id
		    ORDER BY id DESC
		    LIMIT 1
		) lm ON true
		WHERE m.member IS NOT NULL
		   OR (c.kind = 'group' AND c.group_id = ANY($2))
		ORDER BY COALESCE(lm.created_at, c.created_at) DESC`, member, groupIDs)
	if err != nil {
		return nil, fmt.Errorf("chat: listing conversations: %w", err)
	}

	out := make([]Summary, 0, len(rows))
	materialized := make([]int64, 0, len(rows))
	for _, row := range rows {
		summary := Summary{
			Conversation: row.Conversation,
			Unread:       row.Unread,
		}
		if row.LastID != 0 {
			last := Message{ID: row.LastID, Sender: row.LastSender, Body: row.LastBody}
			if row.LastAt.Valid {
				last.CreatedAt = row.LastAt.Time
			}
			summary.LastMessage = &last
		}
		out = append(out, summary)
		if row.Conversation.Kind != KindGroup || row.Conversation.GroupID == 0 {
			materialized = append(materialized, row.Conversation.ID)
		}
	}

	members, err := s.membersByConversation(ctx, materialized)
	if err != nil {
		return nil, err
	}
	for i := range out {
		conv := out[i].Conversation
		if conv.Kind == KindGroup && conv.GroupID != 0 {
			groupMembers, err := s.groups.Members(ctx, s.db, conv.GroupID)
			if err != nil {
				return nil, err
			}
			out[i].Members = groupMembers
			continue
		}
		out[i].Members = members[conv.ID]
	}
	return out, nil
}

// ensureGroupConversations creates a room for every organization group the
// member belongs to and returns the group ids, so the listing can include them.
//
// It is idempotent: the room is created the first time a member opens chat and
// found again afterwards. A rename of the group is carried into the room's
// title, because the room is named after the group.
func (s *Service) ensureGroupConversations(ctx context.Context, member string) ([]int64, error) {
	groups, err := s.groups.ForMember(ctx, s.db, member)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(groups))
	for _, group := range groups {
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO chat_conversations (uid, kind, title, group_id, created_by)
			VALUES ($1, 'group', $2, $3, $4)
			ON CONFLICT (group_id) WHERE group_id IS NOT NULL
			DO UPDATE SET title = EXCLUDED.title`,
			newUID(), group.Name, group.ID, member); err != nil {
			return nil, fmt.Errorf("chat: opening a group room: %w", err)
		}
		ids = append(ids, group.ID)
	}
	return ids, nil
}

// membersByConversation loads the member lists for several rooms in one query.
func (s *Service) membersByConversation(ctx context.Context, ids []int64) (map[int64][]string, error) {
	out := make(map[int64][]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	query, args, err := sqlx.In(`
		SELECT conversation_id, member FROM chat_members
		WHERE conversation_id IN (?)
		ORDER BY conversation_id, member`, ids)
	if err != nil {
		return nil, fmt.Errorf("chat: building member query: %w", err)
	}
	query = s.db.Rebind(query)

	rows, err := s.db.QueryxContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("chat: listing members: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var conversationID int64
		var member string
		if err := rows.Scan(&conversationID, &member); err != nil {
			return nil, fmt.Errorf("chat: scanning member: %w", err)
		}
		out[conversationID] = append(out[conversationID], member)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chat: iterating members: %w", err)
	}
	return out, nil
}

// Members returns the member list of one conversation, with each member's read
// watermark.
//
// A group room's membership is read from the group, so it follows the group as
// people join and leave; the watermark is whatever read state has been recorded
// here, defaulting to zero for somebody who has never opened the room.
func (s *Service) Members(ctx context.Context, conversationID int64) ([]Member, error) {
	conv, err := s.byID(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	if conv.Kind == KindGroup && conv.GroupID != 0 {
		addresses, err := s.groups.Members(ctx, s.db, conv.GroupID)
		if err != nil {
			return nil, err
		}
		marks, err := s.watermarks(ctx, conversationID, addresses)
		if err != nil {
			return nil, err
		}
		out := make([]Member, 0, len(addresses))
		for _, address := range addresses {
			address = normalise(address)
			if mark, ok := marks[address]; ok {
				out = append(out, mark)
				continue
			}
			out = append(out, Member{Address: address})
		}
		return out, nil
	}

	var out []Member
	err = s.db.SelectContext(ctx, &out, `
		SELECT member AS address, last_read_id, joined_at
		FROM chat_members WHERE conversation_id = $1
		ORDER BY member`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("chat: listing members: %w", err)
	}
	return out, nil
}

// watermarks loads read state for a set of addresses in one room.
func (s *Service) watermarks(ctx context.Context, conversationID int64, addresses []string) (map[string]Member, error) {
	out := make(map[string]Member, len(addresses))
	if len(addresses) == 0 {
		return out, nil
	}
	query, args, err := sqlx.In(`
		SELECT member AS address, last_read_id, joined_at
		FROM chat_members
		WHERE conversation_id = ? AND member IN (?)`, conversationID, addresses)
	if err != nil {
		return nil, fmt.Errorf("chat: building watermark query: %w", err)
	}
	query = s.db.Rebind(query)

	rows, err := s.db.QueryxContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("chat: loading read state: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var mark Member
		if err := rows.StructScan(&mark); err != nil {
			return nil, fmt.Errorf("chat: scanning read state: %w", err)
		}
		out[normalise(mark.Address)] = mark
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chat: iterating read state: %w", err)
	}
	return out, nil
}

// MemberAddresses returns just the addresses, which is what a broadcast needs.
func (s *Service) MemberAddresses(ctx context.Context, conversationID int64) ([]string, error) {
	conv, err := s.byID(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	if conv.Kind == KindGroup && conv.GroupID != 0 {
		members, err := s.groups.Members(ctx, s.db, conv.GroupID)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(members))
		for _, address := range members {
			out = append(out, normalise(address))
		}
		return out, nil
	}

	var out []string
	err = s.db.SelectContext(ctx, &out, `
		SELECT member FROM chat_members WHERE conversation_id = $1
		ORDER BY member`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("chat: listing member addresses: %w", err)
	}
	return out, nil
}

// IsMember reports whether the address belongs to the conversation. A group
// room asks the group, so membership cannot be granted by a stale copy of it.
func (s *Service) IsMember(ctx context.Context, conversationID int64, member string) (bool, error) {
	conv, err := s.byID(ctx, conversationID)
	if err != nil {
		return false, err
	}
	if conv.Kind == KindGroup && conv.GroupID != 0 {
		ok, err := s.groups.IsMember(ctx, s.db, conv.GroupID, normalise(member))
		if store.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return ok, nil
	}

	var exists bool
	err = s.db.GetContext(ctx, &exists, `
		SELECT EXISTS (
		    SELECT 1 FROM chat_members WHERE conversation_id = $1 AND member = $2
		)`, conversationID, normalise(member))
	if err != nil {
		return false, fmt.Errorf("chat: checking membership: %w", err)
	}
	return exists, nil
}

// messageColumns is the one projection shared by history and by the copy of a
// message a sender gets back, so a message with a file and one without are
// read the same way.
const messageColumns = `
	m.id, m.sender, m.body, m.created_at,
	a.id AS attachment_id, a.uid AS attachment_uid,
	a.conversation_id AS attachment_conversation_id,
	a.uploader AS attachment_uploader,
	a.blob_sha256 AS attachment_blob_sha256,
	a.name AS attachment_name, a.mime AS attachment_mime,
	a.size AS attachment_size, a.created_at AS attachment_created_at`

// messageRow is the flat shape the join produces before it is folded into the
// Message/Attachment pair a client sees.
type messageRow struct {
	ID                       int64          `db:"id"`
	Sender                   string         `db:"sender"`
	Body                     string         `db:"body"`
	CreatedAt                time.Time      `db:"created_at"`
	AttachmentID             sql.NullInt64  `db:"attachment_id"`
	AttachmentUID            sql.NullString `db:"attachment_uid"`
	AttachmentConversationID sql.NullInt64  `db:"attachment_conversation_id"`
	AttachmentUploader       sql.NullString `db:"attachment_uploader"`
	AttachmentBlob           []byte         `db:"attachment_blob_sha256"`
	AttachmentName           sql.NullString `db:"attachment_name"`
	AttachmentMIME           sql.NullString `db:"attachment_mime"`
	AttachmentSize           sql.NullInt64  `db:"attachment_size"`
	AttachmentCreatedAt      sql.NullTime   `db:"attachment_created_at"`
}

func (r messageRow) message() Message {
	m := Message{ID: r.ID, Sender: r.Sender, Body: r.Body, CreatedAt: r.CreatedAt}
	if r.AttachmentID.Valid {
		m.Attachment = &Attachment{
			ID:             r.AttachmentID.Int64,
			UID:            r.AttachmentUID.String,
			ConversationID: r.AttachmentConversationID.Int64,
			Uploader:       r.AttachmentUploader.String,
			BlobSHA256:     r.AttachmentBlob,
			Name:           r.AttachmentName.String,
			MIME:           r.AttachmentMIME.String,
			Size:           r.AttachmentSize.Int64,
			CreatedAt:      r.AttachmentCreatedAt.Time,
		}
	}
	return m
}

// History returns messages in a conversation, newest page first but returned in
// ascending order so a client can append them to the transcript directly.
//
// beforeID pages backwards: pass the oldest id already shown, and the page
// before it comes back. Zero means the newest page.
func (s *Service) History(ctx context.Context, conversationID int64, beforeID int64, limit int) ([]Message, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if beforeID < 0 {
		beforeID = 0
	}
	var rows []messageRow
	err := s.db.SelectContext(ctx, &rows, `
		SELECT `+messageColumns+`
		FROM chat_messages m
		LEFT JOIN chat_attachments a ON a.id = m.attachment_id
		WHERE m.conversation_id = $1 AND ($2 = 0 OR m.id < $2)
		ORDER BY m.id DESC
		LIMIT $3`, conversationID, beforeID, limit)
	if err != nil {
		return nil, fmt.Errorf("chat: loading history: %w", err)
	}
	out := make([]Message, len(rows))
	for i, row := range rows {
		out[i] = row.message()
	}
	// The query walks backwards so the page is the newest one; reverse it into
	// reading order.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// Append stores a text message. It is the common case in front of
// AppendWithAttachment.
func (s *Service) Append(ctx context.Context, conversationID int64, sender, body string) (Message, error) {
	return s.AppendWithAttachment(ctx, conversationID, sender, body, "")
}

// AppendWithAttachment stores a message and returns it as written, so every
// recipient sees the same id, timestamp and attachment. A message may carry a
// file, text, or both; only an empty message with no file is refused.
func (s *Service) AppendWithAttachment(ctx context.Context, conversationID int64, sender, body, attachmentUID string) (Message, error) {
	sender = normalise(sender)
	body = strings.TrimSpace(body)
	attachmentUID = strings.TrimSpace(attachmentUID)
	if sender == "" {
		return Message{}, fmt.Errorf("%w: a sender is required", ErrInvalid)
	}
	if body == "" && attachmentUID == "" {
		return Message{}, fmt.Errorf("%w: a message cannot be empty", ErrInvalid)
	}
	if len(body) > maxBody {
		return Message{}, fmt.Errorf("%w: a message is too long", ErrInvalid)
	}

	member, err := s.IsMember(ctx, conversationID, sender)
	if err != nil {
		return Message{}, err
	}
	if !member {
		return Message{}, ErrNotMember
	}
	attachmentID, err := s.linkAttachment(ctx, conversationID, attachmentUID, sender)
	if err != nil {
		return Message{}, err
	}

	var attachment any
	if attachmentID != 0 {
		attachment = attachmentID
	}
	var id int64
	err = s.db.QueryRowxContext(ctx, `
		INSERT INTO chat_messages (conversation_id, sender, body, attachment_id)
		VALUES ($1, $2, $3, $4)
		RETURNING id`,
		conversationID, sender, body, attachment,
	).Scan(&id)
	if err != nil {
		return Message{}, fmt.Errorf("chat: storing message: %w", err)
	}

	var row messageRow
	if err := s.db.GetContext(ctx, &row, `
		SELECT `+messageColumns+`
		FROM chat_messages m
		LEFT JOIN chat_attachments a ON a.id = m.attachment_id
		WHERE m.id = $1`, id); err != nil {
		return Message{}, fmt.Errorf("chat: reading stored message: %w", err)
	}
	return row.message(), nil
}

// MarkRead advances the member's read watermark. It never moves backwards, so a
// late duplicate cannot un-read messages that were already seen.
func (s *Service) MarkRead(ctx context.Context, conversationID int64, member string, messageID int64) error {
	member = normalise(member)
	conv, err := s.byID(ctx, conversationID)
	if err != nil {
		return err
	}

	if conv.Kind == KindGroup && conv.GroupID != 0 {
		// A group's membership is the group's, so a member may have no row here
		// yet: the first read creates one. It holds read state only; membership
		// is still answered by the group.
		ok, err := s.groups.IsMember(ctx, s.db, conv.GroupID, member)
		if err != nil {
			return err
		}
		if !ok {
			return ErrNotMember
		}
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO chat_members (conversation_id, member, user_address, last_read_id)
			VALUES ($1, $2, (SELECT address FROM users WHERE address = $2), $3)
			ON CONFLICT (conversation_id, member)
			DO UPDATE SET last_read_id = GREATEST(chat_members.last_read_id, EXCLUDED.last_read_id)`,
			conversationID, member, messageID); err != nil {
			return fmt.Errorf("chat: marking read: %w", err)
		}
		return nil
	}

	res, err := s.db.ExecContext(ctx, `
		UPDATE chat_members
		SET last_read_id = GREATEST(last_read_id, $3)
		WHERE conversation_id = $1 AND member = $2`,
		conversationID, member, messageID)
	if err != nil {
		return fmt.Errorf("chat: marking read: %w", err)
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return ErrNotMember
	}
	return nil
}

// MarkAllRead advances the watermark to the newest message in the room.
func (s *Service) MarkAllRead(ctx context.Context, conversationID int64, member string) (int64, error) {
	var lastID int64
	if err := s.db.GetContext(ctx, &lastID, `
		SELECT COALESCE(max(id), 0) FROM chat_messages WHERE conversation_id = $1`,
		conversationID); err != nil {
		return 0, fmt.Errorf("chat: finding newest message: %w", err)
	}
	if lastID == 0 {
		return 0, nil
	}
	return lastID, s.MarkRead(ctx, conversationID, member, lastID)
}

// normalise lowercases and trims an address. Addresses are citext in the
// database, but a socket frame is compared in Go too, and one spelling keeps
// membership checks predictable.
func normalise(address string) string {
	return strings.ToLower(strings.TrimSpace(address))
}

// normaliseMembers normalises and deduplicates, preserving order.
func normaliseMembers(addresses []string) []string {
	out := make([]string, 0, len(addresses))
	seen := make(map[string]bool, len(addresses))
	for _, address := range addresses {
		address = normalise(address)
		if address == "" || seen[address] {
			continue
		}
		seen[address] = true
		out = append(out, address)
	}
	return out
}

func withMember(members []string, address string) []string {
	for _, member := range members {
		if member == address {
			return members
		}
	}
	return append(members, address)
}

// newUID is 128 bits of randomness, the same shape as a meeting UID, so a
// conversation identifier is a capability rather than a guessable sequence.
func newUID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand failing is not a state to paper over with a weak id.
		panic("chat: cannot read randomness: " + err.Error())
	}
	return hex.EncodeToString(buf[:])
}
