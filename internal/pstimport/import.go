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
	"errors"
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

var (
	// ErrInvalid marks a request the caller can fix.
	ErrInvalid = errors.New("pstimport: invalid request")
	// ErrDenied marks an import the caller may not start.
	ErrDenied = errors.New("pstimport: not permitted")
)

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
	StartedBy  string
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
	Messages int   `json:"messages"`
	Events   int   `json:"events"`
	Contacts int   `json:"contacts"`
	Skipped  int   `json:"skipped"`
	Bytes    int64 `json:"bytes"`
}

// Add accumulates another batch's numbers.
func (s *Stats) Add(other Stats) {
	s.Messages += other.Messages
	s.Events += other.Events
	s.Contacts += other.Contacts
	s.Skipped += other.Skipped
	s.Bytes += other.Bytes
}

// Source is one imported archive: the durable record the list endpoint shows
// and the batch endpoint writes its progress into.
type Source struct {
	ID               int64      `db:"id" json:"id"`
	Owner            string     `db:"owner" json:"owner"`
	StartedBy        string     `db:"started_by" json:"started_by"`
	FileName         string     `db:"file_name" json:"file_name"`
	Status           string     `db:"status" json:"status"`
	CalendarID       *int64     `db:"calendar_id" json:"calendar_id,omitempty"`
	AddressBookID    *int64     `db:"address_book_id" json:"address_book_id,omitempty"`
	MessagesImported int64      `db:"messages_imported" json:"messages_imported"`
	EventsImported   int64      `db:"events_imported" json:"events_imported"`
	ContactsImported int64      `db:"contacts_imported" json:"contacts_imported"`
	ItemsSkipped     int64      `db:"items_skipped" json:"items_skipped"`
	BytesImported    int64      `db:"bytes_imported" json:"bytes_imported"`
	LastError        *string    `db:"last_error" json:"last_error,omitempty"`
	CreatedAt        time.Time  `db:"created_at" json:"created_at"`
	FinishedAt       *time.Time `db:"finished_at" json:"finished_at,omitempty"`
}

// WorkItem is one already-converted item ready to be stored. The command line
// builds these from a PST read on disk; the browser builds the same shape from
// the items it parsed and uploads them in batches.
type WorkItem struct {
	// Key is the stable item identifier within the archive, such as "m:123".
	Key    string
	Kind   string // "message", "event" or "contact"
	Target string // mailbox name for messages, empty otherwise

	// Message.
	MIME []byte
	Date time.Time
	Seen bool

	// Event.
	Event ical.Event

	// Contact.
	Card pst.Contact
	// VCard is the already rendered card; empty means render from Card.
	VCard string
}

// Importer holds the repositories an import writes through.
type Importer struct {
	DB        *sqlx.DB
	Blobs     *blobstore.Store
	Mailbox   *imapd.MailboxRepo
	Messages  *imapd.MessageRepo
	Events    *store.Events
	Books     *store.AddressBooks
	Calendars *store.Calendars
	Quotas    *store.Quotas
	Users     *store.Users
}

// New builds an importer.
func New(db *sqlx.DB, blobs *blobstore.Store) *Importer {
	return &Importer{
		DB:        db,
		Blobs:     blobs,
		Mailbox:   imapd.NewMailboxRepo(),
		Messages:  imapd.NewMessageRepo(),
		Events:    store.NewEvents(),
		Books:     store.NewAddressBooks(),
		Calendars: store.NewCalendars(),
		Quotas:    store.NewQuotas(),
		Users:     store.NewUsers(),
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

const sourceColumns = `
	id, owner, started_by, file_name, status, calendar_id, address_book_id,
	messages_imported, events_imported, contacts_imported, items_skipped,
	bytes_imported, last_error, created_at, finished_at`

// CreateSource records an archive that is about to be imported. An existing
// session for the same owner and digest is reset rather than duplicated: the
// ledger inside it keeps the re-run from importing anything twice, and the
// counters describe this run.
func (im *Importer) CreateSource(ctx context.Context, owner, startedBy string, digest [32]byte, fileName string, calendarID, addressBookID int64) (Source, error) {
	owner = strings.ToLower(strings.TrimSpace(owner))
	if owner == "" {
		return Source{}, fmt.Errorf("%w: an owner is required", ErrInvalid)
	}
	if startedBy == "" {
		startedBy = owner
	}
	if strings.TrimSpace(fileName) == "" {
		fileName = "archive"
	}

	var mailboxEnabled bool
	if err := im.DB.GetContext(ctx, &mailboxEnabled, `
		SELECT mailbox_enabled FROM users WHERE address = $1 AND deleted_at IS NULL`, owner); err != nil {
		if store.IsNotFound(err) {
			return Source{}, store.ErrNotFound
		}
		return Source{}, fmt.Errorf("pstimport: looking up %s: %w", owner, err)
	}
	if !mailboxEnabled {
		return Source{}, fmt.Errorf("%w: that account cannot hold mail", ErrInvalid)
	}

	var source Source
	err := im.DB.GetContext(ctx, &source, `
		INSERT INTO pst_import_sources (owner, started_by, source_sha256, file_name,
		                                calendar_id, address_book_id, status)
		VALUES ($1, $2, $3, $4, NULLIF($5, 0), NULLIF($6, 0), 'running')
		ON CONFLICT (owner, source_sha256) DO UPDATE SET
			started_by = EXCLUDED.started_by,
			file_name = EXCLUDED.file_name,
			calendar_id = EXCLUDED.calendar_id,
			address_book_id = EXCLUDED.address_book_id,
			status = 'running',
			messages_imported = 0,
			events_imported = 0,
			contacts_imported = 0,
			items_skipped = 0,
			bytes_imported = 0,
			last_error = NULL,
			finished_at = NULL
		RETURNING `+sourceColumns,
		owner, startedBy, digest[:], fileName, calendarID, addressBookID)
	if err != nil {
		return Source{}, fmt.Errorf("pstimport: recording the source: %w", err)
	}
	return source, nil
}

// GetSource returns one session by id.
func (im *Importer) GetSource(ctx context.Context, id int64) (Source, error) {
	var source Source
	if err := im.DB.GetContext(ctx, &source, `
		SELECT `+sourceColumns+` FROM pst_import_sources WHERE id = $1`, id); err != nil {
		if store.IsNotFound(err) {
			return Source{}, store.ErrNotFound
		}
		return Source{}, fmt.Errorf("pstimport: reading source %d: %w", id, err)
	}
	return source, nil
}

// ListSources returns sessions, newest first. A nil owner lists every owner,
// which only the admin routes ask for.
func (im *Importer) ListSources(ctx context.Context, owner *string, limit, offset int) ([]Source, int, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	query := `SELECT ` + sourceColumns + ` FROM pst_import_sources`
	countQuery := `SELECT count(*) FROM pst_import_sources`
	var args []any
	if owner != nil && *owner != "" {
		query += ` WHERE owner = $1`
		countQuery += ` WHERE owner = $1`
		args = append(args, *owner)
	}
	query += ` ORDER BY created_at DESC`
	query += fmt.Sprintf(` LIMIT %d OFFSET %d`, limit, offset)

	var sources []Source
	if err := im.DB.SelectContext(ctx, &sources, query, args...); err != nil {
		return nil, 0, fmt.Errorf("pstimport: listing sources: %w", err)
	}
	var total int
	if err := im.DB.GetContext(ctx, &total, countQuery, args...); err != nil {
		return nil, 0, fmt.Errorf("pstimport: counting sources: %w", err)
	}
	return sources, total, nil
}

// FinishSource closes a session. A failure message is truncated so a runaway
// driver error cannot fill the column.
func (im *Importer) FinishSource(ctx context.Context, id int64, status string, jobErr error) error {
	if status != "completed" && status != "failed" {
		return fmt.Errorf("%w: status must be completed or failed", ErrInvalid)
	}
	var message *string
	if jobErr != nil {
		text := jobErr.Error()
		if len(text) > 2000 {
			text = text[:2000]
		}
		message = &text
	}
	_, err := im.DB.ExecContext(ctx, `
		UPDATE pst_import_sources
		SET status = $2, last_error = $3, finished_at = now()
		WHERE id = $1`, id, status, message)
	if err != nil {
		return fmt.Errorf("pstimport: finishing source %d: %w", id, err)
	}
	return nil
}

// Import runs the plan over an opened PST. progress, when set, is called after
// every committed batch.
func (im *Importer) Import(ctx context.Context, file *pst.File, plan Plan, progress func(Stats)) (Stats, error) {
	stats := Stats{}
	if plan.Owner == "" {
		return stats, fmt.Errorf("%w: an owner is required", ErrInvalid)
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
		calendarID := int64(0)
		if plan.IncludeCalendar {
			calendarID = plan.CalendarID
		}
		addressBookID := int64(0)
		if plan.IncludeContacts {
			addressBookID = plan.AddressBookID
		}
		source, err := im.CreateSource(ctx, plan.Owner, plan.StartedBy, plan.SourceSHA,
			filepath.Base(plan.SourcePath), calendarID, addressBookID)
		if err != nil {
			return stats, err
		}
		sourceID = source.ID
	}

	var batch []WorkItem
	var batchBytes int64
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		written, err := im.ImportBatch(ctx, plan, sourceID, batch)
		if err != nil {
			return err
		}
		stats.Add(written)
		batch = nil
		batchBytes = 0
		if progress != nil {
			progress(stats)
		}
		return nil
	}

	items := file.Items()
	for _, item := range items {
		var w WorkItem
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
			date := item.Time(0x0039)
			if date.IsZero() {
				date = item.Time(0x0019)
			}
			w = WorkItem{
				Key:    fmt.Sprintf("m:%d", item.ID),
				Kind:   "message",
				Target: target,
				MIME:   raw,
				Date:   date,
				Seen:   !item.Unread(),
			}
		case "appointment":
			if !plan.IncludeCalendar {
				continue
			}
			event := file.Appointment(item)
			if event.Start.IsZero() {
				stats.Skipped++
				continue
			}
			w = WorkItem{
				Key:   fmt.Sprintf("e:%d", item.ID),
				Kind:  "event",
				Event: event,
			}
		case "contact":
			if !plan.IncludeContacts {
				continue
			}
			card := file.ContactCard(item)
			if card.DisplayName == "" && len(card.Emails) == 0 {
				stats.Skipped++
				continue
			}
			w = WorkItem{
				Key:   fmt.Sprintf("c:%d", item.ID),
				Kind:  "contact",
				Card:  card,
				VCard: string(card.VCard()),
			}
		default:
			continue
		}
		batch = append(batch, w)
		batchBytes += w.size()
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

// ImportBatch writes one batch of already-converted items in a single
// transaction. It is the entry point the browser uses.
func (im *Importer) ImportBatch(ctx context.Context, plan Plan, sourceID int64, items []WorkItem) (Stats, error) {
	if plan.Owner == "" {
		return Stats{}, fmt.Errorf("%w: an owner is required", ErrInvalid)
	}
	if len(items) == 0 {
		return Stats{}, nil
	}
	if len(items) > 5000 {
		return Stats{}, fmt.Errorf("%w: at most 5000 items fit in a batch", ErrInvalid)
	}
	var err error
	plan, err = im.resolveTargets(ctx, plan, items)
	if err != nil {
		return Stats{}, err
	}
	quotaLimit, err := im.quotaLimit(ctx, plan.Owner)
	if err != nil {
		return Stats{}, err
	}
	return im.writeBatch(ctx, plan, sourceID, items, quotaLimit)
}

// resolveTargets fills in the owner's default calendar and address book when a
// batch contains appointments or contacts and the caller did not name one. The
// admin page cannot browse another user's calendars, so this is what makes its
// import land somewhere sensible rather than failing on an empty id.
func (im *Importer) resolveTargets(ctx context.Context, plan Plan, items []WorkItem) (Plan, error) {
	needCalendar, needAddressBook := false, false
	for _, item := range items {
		switch item.Kind {
		case "event":
			needCalendar = true
		case "contact":
			needAddressBook = true
		}
	}
	if needCalendar && plan.CalendarID == 0 {
		calendar, err := im.Calendars.EnsureDefault(ctx, im.DB, plan.Owner, "UTC")
		if err != nil {
			return plan, fmt.Errorf("pstimport: preparing the default calendar: %w", err)
		}
		plan.CalendarID = calendar.ID
	}
	if needAddressBook && plan.AddressBookID == 0 {
		book, err := im.Books.EnsurePersonal(ctx, im.DB, plan.Owner)
		if err != nil {
			return plan, fmt.Errorf("pstimport: preparing the personal address book: %w", err)
		}
		plan.AddressBookID = book.ID
	}
	return plan, nil
}

func (im *Importer) writeBatch(ctx context.Context, plan Plan, sourceID int64, items []WorkItem, quotaLimit int64) (Stats, error) {
	stats := Stats{}
	if plan.DryRun {
		for _, w := range items {
			stats.add(w)
		}
		return stats, nil
	}

	err := store.InTx(ctx, im.DB, func(tx *sqlx.Tx) error {
		for _, w := range items {
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
		if sourceID != 0 {
			if _, err := tx.ExecContext(ctx, `
				UPDATE pst_import_sources
				SET messages_imported = messages_imported + $2,
				    events_imported = events_imported + $3,
				    contacts_imported = contacts_imported + $4,
				    items_skipped = items_skipped + $5,
				    bytes_imported = bytes_imported + $6
				WHERE id = $1`,
				sourceID, stats.Messages, stats.Events, stats.Contacts, stats.Skipped, stats.Bytes); err != nil {
				return fmt.Errorf("pstimport: saving progress: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return Stats{}, err
	}
	return stats, nil
}

func (w WorkItem) size() int64 {
	switch w.Kind {
	case "message":
		return int64(len(w.MIME))
	case "event":
		return int64(len(w.Event.Marshal()))
	case "contact":
		return int64(len(w.VCard))
	}
	return 0
}

func (s *Stats) add(w WorkItem) {
	switch w.Kind {
	case "message":
		s.Messages++
		s.Bytes += int64(len(w.MIME))
	case "event":
		s.Events++
	case "contact":
		s.Contacts++
	}
}

// writeItem writes one item and records it in the dedupe ledger. It reports
// false when the item was already imported.
func (im *Importer) writeItem(ctx context.Context, tx *sqlx.Tx, sourceID int64, plan Plan, w WorkItem, quotaLimit int64) (bool, error) {
	var seen bool
	if err := tx.GetContext(ctx, &seen, `
		SELECT EXISTS (SELECT 1 FROM pst_import_items WHERE source_id = $1 AND item_key = $2)`,
		sourceID, w.Key); err != nil {
		return false, fmt.Errorf("pstimport: checking %s: %w", w.Key, err)
	}
	if seen {
		return false, nil
	}

	switch w.Kind {
	case "message":
		if len(w.MIME) == 0 {
			return false, nil
		}
		mailboxID, err := im.Mailbox.EnsureNamed(ctx, im.DB, plan.Owner, w.Target)
		if err != nil {
			return false, fmt.Errorf("pstimport: preparing folder %q: %w", w.Target, err)
		}
		digest, size, err := im.Blobs.Put(w.MIME)
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

		meta, err := mail.Describe(w.MIME)
		if err != nil {
			return false, nil
		}
		date := w.Date
		if date.IsZero() {
			date = time.Now()
		}
		flags := []string{}
		if w.Seen {
			flags = append(flags, `\Seen`)
		}
		if _, err := im.Messages.InsertTx(ctx, tx, mailboxID, digest[:], size, date, flags, meta); err != nil {
			return false, fmt.Errorf("pstimport: inserting message: %w", err)
		}
		if _, err := im.Quotas.Adjust(ctx, tx, plan.Owner, size, 1); err != nil {
			return false, err
		}
		return im.record(ctx, tx, sourceID, w, w.Target)

	case "event":
		event := w.Event
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
		card := w.Card
		if _, err := im.Books.GetContactByUID(ctx, tx, plan.AddressBookID, card.UID); err == nil {
			if _, err := im.record(ctx, tx, sourceID, w, "addressbook"); err != nil {
				return false, err
			}
			return false, nil
		}
		vcard := w.VCard
		if vcard == "" {
			vcard = string(card.VCard())
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
			VCard:        vcard,
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

func (im *Importer) record(ctx context.Context, tx *sqlx.Tx, sourceID int64, w WorkItem, target string) (bool, error) {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO pst_import_items (source_id, item_key, kind, target)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (source_id, item_key) DO NOTHING`,
		sourceID, w.Key, w.Kind, target)
	if err != nil {
		return false, fmt.Errorf("pstimport: recording %s: %w", w.Key, err)
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
