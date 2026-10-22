package chat_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/chat"
	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/testdb"
)

func seedUser(t *testing.T, db *sqlx.DB, address string) {
	t.Helper()
	if err := store.NewUsers().Create(context.Background(), db, store.CreateUserRequest{
		Address: address, Source: policy.SourceLocal, DisplayName: address,
	}); err != nil {
		t.Fatalf("seeding %s: %v", address, err)
	}
}

func TestDirectConversation(t *testing.T) {
	db := testdb.Migrated(t)
	ctx := context.Background()
	seedUser(t, db, "alice@example.com")
	seedUser(t, db, "bob@example.com")
	seedUser(t, db, "carol@example.com")
	svc := chat.New(db, nil)

	conv, err := svc.Create(ctx, db, chat.CreateRequest{
		Kind:      chat.KindDirect,
		Members:   []string{"alice@example.com", "bob@example.com"},
		CreatedBy: "alice@example.com",
	})
	if err != nil {
		t.Fatalf("creating a direct conversation: %v", err)
	}
	if conv.UID == "" || conv.Kind != chat.KindDirect {
		t.Fatalf("conversation = %+v, want a direct room with a uid", conv)
	}

	// Opening the same pair again must find the same room rather than making a
	// second one that never sees the first.
	again, err := svc.Create(ctx, db, chat.CreateRequest{
		Kind:      chat.KindDirect,
		Members:   []string{"bob@example.com", "alice@example.com"},
		CreatedBy: "bob@example.com",
	})
	if err != nil {
		t.Fatalf("re-opening the direct conversation: %v", err)
	}
	if again.UID != conv.UID {
		t.Errorf("second uid = %q, want %q", again.UID, conv.UID)
	}

	if ok, err := svc.IsMember(ctx, conv.ID, "alice@example.com"); err != nil || !ok {
		t.Fatalf("alice membership = %v, %v", ok, err)
	}
	if ok, _ := svc.IsMember(ctx, conv.ID, "carol@example.com"); ok {
		t.Error("carol is not on the conversation but reads as a member")
	}

	first, err := svc.Append(ctx, conv.ID, "alice@example.com", "hello bob")
	if err != nil {
		t.Fatalf("sending: %v", err)
	}
	if first.ID == 0 || first.Body != "hello bob" {
		t.Fatalf("stored message = %+v", first)
	}
	if _, err := svc.Append(ctx, conv.ID, "carol@example.com", "hi"); !errors.Is(err, chat.ErrNotMember) {
		t.Errorf("outsider sending = %v, want ErrNotMember", err)
	}

	second, err := svc.Append(ctx, conv.ID, "bob@example.com", "hello alice")
	if err != nil {
		t.Fatalf("reply: %v", err)
	}
	history, err := svc.History(ctx, conv.ID, 0, 50)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) != 2 || history[0].ID != first.ID || history[1].ID != second.ID {
		t.Errorf("history = %+v, want the two messages in reading order", history)
	}

	// Alice has read her own message but not bob's, so the room shows one
	// unread: her own words are not something to notify her about.
	if err := svc.MarkRead(ctx, conv.ID, "alice@example.com", first.ID); err != nil {
		t.Fatalf("marking read: %v", err)
	}
	summaries, err := svc.ListFor(ctx, "alice@example.com")
	if err != nil {
		t.Fatalf("listing conversations: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("alice's conversations = %d, want 1", len(summaries))
	}
	if summaries[0].Unread != 1 {
		t.Errorf("unread = %d, want 1", summaries[0].Unread)
	}
	if summaries[0].LastMessage == nil || summaries[0].LastMessage.ID != second.ID {
		t.Errorf("last message = %+v, want bob's reply", summaries[0].LastMessage)
	}
}

func TestGroupConversationFollowsTheGroup(t *testing.T) {
	db := testdb.Migrated(t)
	ctx := context.Background()
	seedUser(t, db, "alice@example.com")
	seedUser(t, db, "bob@example.com")
	seedUser(t, db, "carol@example.com")
	seedUser(t, db, "dave@other.example")

	workgroup, err := store.NewWorkgroups().Create(ctx, db, store.CreateWorkgroupRequest{
		Name: "Acme", Domain: "example.com", DisplayName: "Acme",
	})
	if err != nil {
		t.Fatalf("creating the workgroup: %v", err)
	}
	groups := store.NewGroups()
	project, err := groups.Create(ctx, db, workgroup.ID, "Project", "the project group")
	if err != nil {
		t.Fatalf("creating the group: %v", err)
	}
	for _, address := range []string{"alice@example.com", "bob@example.com"} {
		if err := groups.AddMember(ctx, db, project.ID, address); err != nil {
			t.Fatalf("adding %s: %v", address, err)
		}
	}

	svc := chat.New(db, nil)
	conv, err := svc.Create(ctx, db, chat.CreateRequest{
		Kind:      chat.KindGroup,
		GroupID:   project.ID,
		CreatedBy: "alice@example.com",
	})
	if err != nil {
		t.Fatalf("opening the group room: %v", err)
	}
	if conv.Kind != chat.KindGroup || conv.GroupID != project.ID {
		t.Fatalf("conversation = %+v, want a group room for %d", conv, project.ID)
	}

	// Membership is the group's, not a copy taken when the room was made.
	if ok, _ := svc.IsMember(ctx, conv.ID, "bob@example.com"); !ok {
		t.Error("bob is in the group but not in its room")
	}
	if ok, _ := svc.IsMember(ctx, conv.ID, "carol@example.com"); ok {
		t.Error("carol is not in the group but reads as a member of its room")
	}

	if _, err := svc.Append(ctx, conv.ID, "alice@example.com", "hello group"); err != nil {
		t.Fatalf("a member sending: %v", err)
	}
	if _, err := svc.Append(ctx, conv.ID, "carol@example.com", "hi"); !errors.Is(err, chat.ErrNotMember) {
		t.Errorf("an outsider sending = %v, want ErrNotMember", err)
	}
	if err := svc.MarkRead(ctx, conv.ID, "bob@example.com", 1); err != nil {
		t.Fatalf("a group member marking read: %v", err)
	}

	// Adding somebody to the group lets them in without touching the room.
	if err := groups.AddMember(ctx, db, project.ID, "carol@example.com"); err != nil {
		t.Fatalf("adding carol: %v", err)
	}
	if ok, _ := svc.IsMember(ctx, conv.ID, "carol@example.com"); !ok {
		t.Error("carol joined the group but cannot open its room")
	}

	// Removing somebody takes them out just as directly.
	if _, err := groups.RemoveMember(ctx, db, project.ID, "bob@example.com"); err != nil {
		t.Fatalf("removing bob: %v", err)
	}
	if ok, _ := svc.IsMember(ctx, conv.ID, "bob@example.com"); ok {
		t.Error("bob left the group but still reads as a member of its room")
	}

	// A member's listing includes the group room plus the organization's own
	// Everyone room; somebody outside the organization sees neither.
	carolRooms, err := svc.ListFor(ctx, "carol@example.com")
	if err != nil {
		t.Fatalf("listing carol's rooms: %v", err)
	}
	if len(carolRooms) != 2 {
		t.Errorf("carol's rooms = %d, want the group and Everyone", len(carolRooms))
	}
	daveRooms, err := svc.ListFor(ctx, "dave@other.example")
	if err != nil {
		t.Fatalf("listing dave's rooms: %v", err)
	}
	if len(daveRooms) != 0 {
		t.Errorf("dave's rooms = %d, want none: %+v", len(daveRooms), daveRooms)
	}
}
