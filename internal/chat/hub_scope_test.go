package chat

import (
	"context"
	"testing"

	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/testdb"
)

// A guest who followed a meeting invitation may use that room and no other.
// The socket otherwise authenticates as the invited address, which for a
// local user would expose every conversation they belong to.
func TestScopedSocketCannotReachAnotherConversation(t *testing.T) {
	db := testdb.Migrated(t)
	ctx := context.Background()
	for _, address := range []string{"alice@example.com", "bob@example.com", "carol@example.com"} {
		if err := store.NewUsers().Create(ctx, db, store.CreateUserRequest{
			Address: address, Source: policy.SourceLocal, DisplayName: address,
		}); err != nil {
			t.Fatalf("seeding %s: %v", address, err)
		}
	}

	svc := New(db, nil, nil)
	create := func(members ...string) Conversation {
		t.Helper()
		conv, err := svc.Create(ctx, db, CreateRequest{
			Kind:      KindDirect,
			Members:   members,
			CreatedBy: members[0],
		})
		if err != nil {
			t.Fatalf("creating direct conversation: %v", err)
		}
		return conv
	}
	meeting := create("alice@example.com", "bob@example.com")
	other := create("bob@example.com", "carol@example.com")

	hub := NewHub(svc, nil)
	guest := &client{hub: hub, member: "bob@example.com", scope: meeting.UID}

	if _, err := hub.conversation(ctx, guest, meeting.UID); err != nil {
		t.Fatalf("the scoped room was refused: %v", err)
	}
	if _, err := hub.conversation(ctx, guest, other.UID); err == nil {
		t.Fatal("a meeting-scoped socket reached another conversation")
	}

	signedIn := &client{hub: hub, member: "bob@example.com"}
	if _, err := hub.conversation(ctx, signedIn, other.UID); err != nil {
		t.Fatalf("an unscoped socket was refused its own room: %v", err)
	}
}
