package pstimport

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/linchange/linchange/internal/blobstore"
	"github.com/linchange/linchange/internal/pst"
	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/testdb"
)

// TestImportIsIdempotent is the end-to-end check for the property that matters
// most: importing the same archive twice must not duplicate a single item.
// It needs a real PST and a database, so it is skipped unless the operator
// points PST_SAMPLE at a file.
func TestImportIsIdempotent(t *testing.T) {
	path := os.Getenv("PST_SAMPLE")
	if path == "" {
		t.Skip("set PST_SAMPLE to a .pst file to run this")
	}
	ctx := context.Background()
	db := testdb.Migrated(t)

	const owner = "pst-test@example.test"
	if _, err := db.ExecContext(ctx,
		`INSERT INTO domains (name) VALUES ('example.test') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (address, source, address_local)
		VALUES ($1, 'local', 'pst-test')`, owner); err != nil {
		t.Fatal(err)
	}

	calendar, err := store.NewCalendars().EnsureDefault(ctx, db, owner, "UTC")
	if err != nil {
		t.Fatal(err)
	}
	book, err := store.NewAddressBooks().EnsurePersonal(ctx, db, owner)
	if err != nil {
		t.Fatal(err)
	}

	file, err := pst.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	blobs, err := blobstore.Open(blobstore.Options{Root: filepath.Join(t.TempDir(), "blobs")})
	if err != nil {
		t.Fatal(err)
	}
	importer := New(db, blobs)

	folders := map[uint32]bool{}
	for _, item := range file.Items() {
		if item.Kind == "message" {
			folders[item.ParentID] = true
		}
	}
	plan := Plan{
		Owner:           owner,
		SourcePath:      path,
		IncludeCalendar: true,
		CalendarID:      calendar.ID,
		IncludeContacts: true,
		AddressBookID:   book.ID,
		BatchBytes:      1 << 20,
	}
	for id := range folders {
		plan.Folders = append(plan.Folders, FolderMapping{SourceID: id, Target: "PST Import"})
	}

	first, err := importer.Import(ctx, file, plan, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Messages == 0 && first.Events == 0 && first.Contacts == 0 {
		t.Fatal("the file held nothing to import")
	}

	second, err := importer.Import(ctx, file, plan, nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.Messages != 0 || second.Events != 0 || second.Contacts != 0 {
		t.Errorf("second import wrote %+v, want nothing", second)
	}

	var messages, events, contacts int
	if err := db.QueryRowContext(ctx, `
		SELECT (SELECT count(*) FROM messages),
		       (SELECT count(*) FROM events),
		       (SELECT count(*) FROM contacts)`).Scan(&messages, &events, &contacts); err != nil {
		t.Fatal(err)
	}
	if messages != first.Messages {
		t.Errorf("messages = %d after the second import, first imported %d", messages, first.Messages)
	}
	if events != first.Events {
		t.Errorf("events = %d after the second import, first imported %d", events, first.Events)
	}
	if contacts != first.Contacts {
		t.Errorf("contacts = %d after the second import, first imported %d", contacts, first.Contacts)
	}
	t.Logf("imported %+v", first)
}
