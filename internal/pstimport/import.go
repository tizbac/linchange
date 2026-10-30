// Package pstimport writes the items read from a PST or OST into a user's
// mailboxes, calendar and address book.
//
// Two properties matter more than speed here. First, a re-run over the same
// file must not duplicate anything: every item is recorded in
// pst_import_items keyed by the file's digest and the item's stable node
// identifier, and messages are additionally deduplicated by blob digest, so a
// message already present in the target folder is recognised even if the file
// was re-exported and its node identifiers changed. Second, a batch commits as
// one transaction: a connection lost halfway through a batch leaves the
// previous batches in place and nothing half-written in the current one.
package pstimport

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/blobstore"
	"github.com/linchange/linchange/internal/ical"
	"github.com/linchange/linchange/internal/imapd"
	"github.com/linchange/linchange/internal/mail"
	"github.com/linchange/linchange/internal/pst"
	"github.com/linchange/linchange/internal/store"
)

// DefaultBatchBytes is the upload chunk the browser uses and the default here.
// A batch is accumulated until it reaches this size, then written in one
// transaction.
const DefaultBatchBytes = 64 << 20

// FolderMapping says where the messages of one source folder go.
type FolderMapping struct {
	// SourceID is the PST folder node identifier.
	SourceID uint32
	// Target is the destination mailbox name, with "/" separators.
	Target string
}

// Plan describes an import.
type Plan struct {
	Owner      string
	SourcePath string
	// SourceSHA is the digest of the file. It is computed when unset.
	SourceSHA [32]byte

	Folders []FolderMapping

	IncludeCalendar bool
	CalendarID      int64
	IncludeContacts bool
	AddressBookID   int64

	BatchBytes int64
	// DryRun reads and converts everything but writes nothing.
	DryRun bool
}

// Stats reports what an import did.
type Stats struct {
	Messages int
	Events   int
	Contacts int
	Skipped  int
	Bytes    int64
}

// Importer holds the repositories an import writes through.
type Importer struct {
	DB       *sqlx.DB
	Blobs    *blobstore.Store
	Mailbox  *imapd.MailboxRepo
	Messages *imapd.MessageRepo
	Events   *store.Events
	Books    *store.AddressBooks
	Quotas   *store.Quotas
	Users    *store.Users
}

// New builds an importer.
func New(db *sqlx.DB, blobs *blobstore.Store) *Importer {
	return &Importer{
		DB:       db,
		Blobs:    blobs,
		Mailbox:  imapd.NewMailboxRepo(),
		Messages: imapd.NewMessageRepo(),
		Events:   store.NewEvents(),
		Books:    store.NewAddressBooks(),
		Quotas:   store.NewQuotas(),
		Users:    store.NewUsers(),
	}
}

// DigestFile returns the SHA-256 of a file, streamed so a large archive is not
// held in memory twice.
func DigestFile(path string) ([32]byte, error) {
	var out [32]byte
	f, err := os.Open(path)
	if err != nil {
		return out, fmt.Errorf("pstimport: opening %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return out, fmt.Errorf("pstimport: hashing %s: %w", path, err)
	}
	copy(out[:], h.Sum(nil))
	return out, nil
}

// work is one item queued for the current batch.
type work struct {
	item   pst.Item
	kind   string
	key    string
	target string
	mime   []byte
	event  ical.Event
	card   pst.Contact
}

// Import runs the plan over an opened PST. progress, when set, is called after
// every committed batch.
func (im *Importer) Import(ctx context.Context, file *pst.File, plan Plan, progress func(Stats)) (Stats, error) {
	stats := Stats{}
	if plan.Owner == "" {
		return stats, fmt.Errorf("pstimport: an owner is required")
	}
	if plan.BatchBytes <= 0 {
		plan.BatchBytes = DefaultBatchBytes
	}
	if plan.SourceSHA == ([32]byte{}) {
		digest, err := DigestFile(plan.SourcePath)
		if err != nil {
			return stats, err
		}
		plan.SourceSHA = digest
	}

	targetByFolder := make(map[uint32]string, len(plan.Folders))
	for _, mapping := range plan.Folders {
		if strings.TrimSpace(mapping.Target) == "" {
			continue
		}
		targetByFolder[mapping.SourceID] = strings.Trim(mapping.Target, "/")
		if !plan.DryRun {
			if _, err := im.Mailbox.EnsureNamed(ctx, im.DB, plan.Owner, mapping.Target); err != nil {
				return stats, fmt.Errorf("pstimport: preparing folder %q: %w", mapping.Target, err)
			}
		}
	}

	sourceID := int64(0)
	if !plan.DryRun {
		if err := im.DB.GetContext(ctx, &sourceID, `
			INSERT INTO pst_import_sources (owner, source_sha256, file_name)
			VALUES ($1, $2, $3)
			ON CONFLICT (owner, source_sha256)
			DO UPDATE SET file_name = EXCLUDED.file_name
			RETURNING id`,
			plan.Owner, plan.SourceSHA[:], filepath.Base(plan.SourcePath)); err != nil {
			return stats, fmt.Errorf("pstimport: recording the source: %w", err)
		}
	}

	quotaLimit, err := im.quotaLimit(ctx, plan.Owner)
	if err != nil {
		return stats, err
	}

	var batch []work
	var batchBytes int64
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if plan.DryRun {
			for _, w := range batch {
				stats.add(w)
			}
			batch = nil
			batchBytes = 0
			if progress != nil {
				progress(stats)
			}
			return nil
		}

		err := store.InTx(ctx, im.DB, func(tx *sqlx.Tx) error {
			for _, w := range batch {
				done, err := im.writeItem(ctx, tx, sourceID, plan, w, quotaLimit)
				if err != nil {
					return err
				}
				if done {
					stats.add(w)
				} else {
					stats.Skipped++
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		batch = nil
		batchBytes = 0
		if progress != nil {
			progress(stats)
		}
		return nil
	}

	items := file.Items()
	for _, item := range items {
		switch item.Kind {
		case "message":
			target, ok := targetByFolder[item.ParentID]
			if !ok {
				continue
			}
			raw, err := file.MIME(item)
			if err != nil {
				stats.Skipped++
				continue
			}
			w := work{
				item:   item,
				kind:   "message",
				key:    fmt.Sprintf("m:%d", item.ID),
				target: target,
				mime:   raw,
			}
			batch = append(batch, w)
			batchBytes += int64(len(raw))
		case "appointment":
			if !plan.IncludeCalendar {
				continue
			}
			event := file.Appointment(item)
			if event.Start.IsZero() {
				stats.Skipped++
				continue
			}
			w := work{
				item:  item,
				kind:  "event",
				key:   fmt.Sprintf("e:%d", item.ID),
				event: event,
			}
			batch = append(batch, w)
			batchBytes += int64(len(event.Marshal()))
		case "contact":
			if !plan.IncludeContacts {
				continue
			}
			card := file.ContactCard(item)
			if card.DisplayName == "" && len(card.Emails) == 0 {
				stats.Skipped++
				continue
			}
			w := work{
				item: item,
				kind: "contact",
				key:  fmt.Sprintf("c:%d", item.ID),
				card: card,
			}
			batch = append(batch, w)
			batchBytes += int64(len(card.VCard()))
		default:
			continue
		}
		if batchBytes >= plan.BatchBytes {
			if err := flush(); err != nil {
				return stats, err
			}
		}
	}
	if err := flush(); err != nil {
		return stats, err
	}
	return stats, nil
}

func (s *Stats) add(w work) {
	switch w.kind {
	case "message":
		s.Messages++
		s.Bytes += int64(len(w.mime))
	case "event":
		s.Events++
	case "contact":
		s.Contacts++
	}
}

// writeItem writes one item and records it in the dedupe ledger. It reports
// false when the item was already imported.
func (im *Importer) writeItem(ctx context.Context, tx *sqlx.Tx, sourceID int64, plan Plan, w work, quotaLimit int64) (bool, error) {
	var seen bool
	if err := tx.GetContext(ctx, &seen, `
		SELECT EXISTS (SELECT 1 FROM pst_import_items WHERE source_id = $1 AND item_key = $2)`,
		sourceID, w.key); err != nil {
		return false, fmt.Errorf("pstimport: checking %s: %w", w.key, err)
	}
	if seen {
		return false, nil
	}

	switch w.kind {
	case "message":
		mailboxID, err := im.Mailbox.EnsureNamed(ctx, im.DB, plan.Owner, w.target)
		if err != nil {
			return false, fmt.Errorf("pstimport: preparing folder %q: %w", w.target, err)
		}
		digest, size, err := im.Blobs.Put(w.mime)
		if err != nil {
			return false, fmt.Errorf("pstimport: storing message blob: %w", err)
		}
		var exists bool
		if err := tx.GetContext(ctx, &exists, `
			SELECT EXISTS (SELECT 1 FROM messages WHERE mailbox_id = $1 AND blob_sha256 = $2)`,
			mailboxID, digest[:]); err != nil {
			return false, fmt.Errorf("pstimport: checking for an existing message: %w", err)
		}
		if exists {
			if _, err := im.record(ctx, tx, sourceID, w, "duplicate"); err != nil {
				return false, err
			}
			return false, nil
		}

		exceeded, err := im.Quotas.Exceeded(ctx, tx, plan.Owner, quotaLimit, size)
		if err != nil {
			return false, fmt.Errorf("pstimport: checking the quota: %w", err)
		}
		if exceeded {
			return false, fmt.Errorf("pstimport: quota exceeded for %s", plan.Owner)
		}

		meta, err := mail.Describe(w.mime)
		if err != nil {
			return false, nil
		}
		date := w.item.Time(0x0039)
		if date.IsZero() {
			date = w.item.Time(0x0019)
		}
		if date.IsZero() {
			date = time.Now()
		}
		flags := []string{}
		if !w.item.Unread() {
			flags = append(flags, `\Seen`)
		}
		if _, err := im.Messages.InsertTx(ctx, tx, mailboxID, digest[:], size, date, flags, meta); err != nil {
			return false, fmt.Errorf("pstimport: inserting message: %w", err)
		}
		if _, err := im.Quotas.Adjust(ctx, tx, plan.Owner, size, 1); err != nil {
			return false, err
		}
		return im.record(ctx, tx, sourceID, w, w.target)

	case "event":
		event := w.event
		blob := event.Marshal()
		createdBy := plan.Owner
		_, err := im.Events.Upsert(ctx, tx, store.WriteEventRequest{
			CalendarID: plan.CalendarID,
			UID:        event.UID,
			ICalBlob:   blob,
			DTStart:    event.Start,
			DTEnd:      event.End,
			AllDay:     event.AllDay,
			Summary:    event.Summary,
			Location:   event.Location,
			Status:     ical.StatusConfirmed,
			ShowAs:     ical.ShowAsBusy,
			DTStamp:    event.DTStamp,
			CreatedBy:  &createdBy,
		})
		if err != nil {
			return false, fmt.Errorf("pstimport: inserting event %q: %w", event.Summary, err)
		}
		return im.record(ctx, tx, sourceID, w, "calendar")

	case "contact":
		card := w.card
		if _, err := im.Books.GetContactByUID(ctx, tx, plan.AddressBookID, card.UID); err == nil {
			if _, err := im.record(ctx, tx, sourceID, w, "addressbook"); err != nil {
				return false, err
			}
			return false, nil
		}
		email := ""
		if len(card.Emails) > 0 {
			email = card.Emails[0]
		}
		search := strings.ToLower(strings.Join([]string{
			card.DisplayName, strings.Join(card.Emails, " "),
			strings.Join(card.Phones, " "), card.Organization, card.Title,
		}, " "))
		_, err := im.Books.CreateContact(ctx, tx, store.AddressContact{
			BookID:       plan.AddressBookID,
			UID:          card.UID,
			VCard:        string(card.VCard()),
			DisplayName:  card.DisplayName,
			Email:        email,
			SearchText:   search,
			Organization: card.Organization,
			Title:        card.Title,
			Notes:        card.Notes,
		})
		if err != nil {
			if store.IsConflict(err) {
				return im.record(ctx, tx, sourceID, w, "addressbook")
			}
			return false, fmt.Errorf("pstimport: inserting contact %q: %w", card.DisplayName, err)
		}
		return im.record(ctx, tx, sourceID, w, "addressbook")
	}
	return false, nil
}

func (im *Importer) record(ctx context.Context, tx *sqlx.Tx, sourceID int64, w work, target string) (bool, error) {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO pst_import_items (source_id, item_key, kind, target)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (source_id, item_key) DO NOTHING`,
		sourceID, w.key, w.kind, target)
	if err != nil {
		return false, fmt.Errorf("pstimport: recording %s: %w", w.key, err)
	}
	return true, nil
}

// quotaLimit returns the owner's byte quota, or zero when unlimited. It mirrors
// the rule the IMAP importer uses so both paths stop at the same point rather
// than one of them writing past the limit.
func (im *Importer) quotaLimit(ctx context.Context, owner string) (int64, error) {
	user, err := im.Users.Get(ctx, im.DB, owner)
	if err != nil {
		return 0, fmt.Errorf("pstimport: reading the quota for %s: %w", owner, err)
	}
	if user.QuotaBytes != nil {
		if *user.QuotaBytes == 0 {
			return 0, nil
		}
		return *user.QuotaBytes, nil
	}
	return 10_737_418_240, nil
}
