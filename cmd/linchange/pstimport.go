package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/blobstore"
	"github.com/linchange/linchange/internal/imapd"
	"github.com/linchange/linchange/internal/pst"
	"github.com/linchange/linchange/internal/pstimport"
	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/tui"
)

// runPSTImport is the `linchange pst-import` subcommand: an interactive import
// of a PST or OST file straight into a user's mailboxes, calendar and address
// book. It reads the file on this machine, so unlike the browser import it is
// not limited by an upload and it can ask about every folder before writing
// anything.
func runPSTImport(args []string) error {
	fs := flag.NewFlagSet("pst-import", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `linchange pst-import - import a PST or OST file

Usage:
  linchange pst-import --config <path.yaml> --file <archive.pst> --owner <address>

Flags:
`)
		fs.PrintDefaults()
	}

	var (
		configPath = fs.String("config", "", "path to the YAML configuration file (required)")
		pstPath    = fs.String("file", "", "PST or OST file to import (required)")
		owner      = fs.String("owner", "", "mailbox that receives the items (required)")
		batchMB    = fs.Int64("batch-mb", pstimport.DefaultBatchBytes>>20, "largest transaction batch, in MiB")
		dryRun     = fs.Bool("dry-run", false, "read and convert the file without writing anything")
		yes        = fs.Bool("yes", false, "accept the recommended choices without asking")
	)
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if *configPath == "" {
		fs.Usage()
		return fmt.Errorf("--config is required")
	}
	if *pstPath == "" {
		fs.Usage()
		return fmt.Errorf("--file is required")
	}
	if *batchMB <= 0 {
		return fmt.Errorf("--batch-mb must be positive")
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.OpenFromConfig(ctx, cfg, "linchange")
	if err != nil {
		return err
	}
	defer db.Close()

	file, err := pst.Open(*pstPath)
	if err != nil {
		return err
	}
	defer file.Close()

	items := file.Items()
	folders := collectImportFolders(func(id uint32) string { return file.FolderPath(items, id) }, items)
	if len(folders) == 0 {
		return fmt.Errorf("%s holds no importable items", *pstPath)
	}

	term := tui.New(os.Stdin, os.Stdout)
	term.Section(0, 0, "PST import", "Import the mail, appointments and contacts of an Outlook data file into a linchange mailbox.")

	mailboxRepo := imapd.NewMailboxRepo()
	if *owner == "" {
		*owner, err = term.Input("Mailbox that receives the import", tui.InputOpts{
			Validate: func(value string) error {
				if _, err := store.NewUsers().Get(ctx, db, value); err != nil {
					return fmt.Errorf("no such account: %s", value)
				}
				return nil
			},
		})
		if err != nil {
			return err
		}
	}
	*owner = strings.ToLower(strings.TrimSpace(*owner))
	if _, err := store.NewUsers().Get(ctx, db, *owner); err != nil {
		return fmt.Errorf("no such account %q: %w", *owner, err)
	}

	interactive := isTerminal(os.Stdin) && isTerminal(os.Stdout) && !*yes
	if interactive {
		term.KeyValue("File", filepath.Base(*pstPath))
		term.KeyValue("Format", fmt.Sprintf("%s, %s", file.ContentType(), bitness(file)))
		term.KeyValue("Account", *owner)
	}

	// --- source folders ---------------------------------------------------
	selected := make([]bool, len(folders))
	for i := range selected {
		selected[i] = folders[i].Messages > 0
	}
	if interactive {
		choices := make([]tui.Choice, len(folders))
		for i, folder := range folders {
			choices[i] = tui.Choice{
				Label: fmt.Sprintf("%-45s %5d mail", truncateLeft(folder.Path, 45), folder.Messages),
				Hint:  fmt.Sprintf("%d appointment(s), %d contact(s)", folder.Calendars, folder.Contacts),
			}
		}
		selected, err = term.Multi(
			"Which folders should be imported?",
			choices, selected,
			"Space toggles a folder. Mail is imported from every selected folder; appointments and contacts are collected across the whole file below.")
		if err != nil {
			return err
		}
	}

	var chosen []importFolder
	for i, folder := range folders {
		if selected[i] && folder.Messages > 0 {
			chosen = append(chosen, folder)
		}
	}
	if len(chosen) == 0 && !interactive {
		return fmt.Errorf("no folders contain mail")
	}

	// --- destination mail folders ----------------------------------------
	targetRoot := "PST Import"
	if interactive && len(chosen) > 0 {
		mailboxes, err := mailboxRepo.ListForUser(ctx, db, *owner)
		if err != nil {
			return fmt.Errorf("listing folders: %w", err)
		}
		const newFolder = "\x00new"
		options := []tui.Choice{
			{Label: "New folder: PST Import", Hint: "Import into a fresh folder tree."},
			{Label: "INBOX", Hint: "Import into the existing inbox."},
		}
		values := []string{newFolder, "INBOX"}
		for _, mailbox := range mailboxes {
			if mailbox.Name == "INBOX" {
				continue
			}
			options = append(options, tui.Choice{Label: mailbox.Name, Hint: "Existing folder."})
			values = append(values, mailbox.Name)
		}
		index, err := term.Select("Where should imported mail go?", options, 0,
			"Each selected source folder keeps its name under this destination.")
		if err != nil {
			return err
		}
		targetRoot = values[index]
		if targetRoot == newFolder {
			targetRoot, err = term.Input("Name of the new destination folder", tui.InputOpts{
				Default:  "PST Import",
				Validate: func(value string) error { return validateMailboxName(value) },
			})
			if err != nil {
				return err
			}
		}
	}

	common := commonFolderPrefix(chosen)
	mappings := make([]pstimport.FolderMapping, 0, len(chosen))
	for _, folder := range chosen {
		relative := strings.Trim(strings.TrimPrefix(folder.Path, common), "/")
		target := strings.Trim(targetRoot, "/")
		if relative != "" {
			target = strings.Trim(target+"/"+relative, "/")
		}
		mappings = append(mappings, pstimport.FolderMapping{SourceID: folder.ID, Target: target})
	}

	// --- calendar and contacts -------------------------------------------
	includeCalendar := true
	calendarID := int64(0)
	if interactive {
		calendars, err := store.NewCalendars().ByOwner(ctx, db, *owner)
		if err != nil {
			return fmt.Errorf("listing calendars: %w", err)
		}
		const newCalendar = "\x00new"
		options := []tui.Choice{{Label: "Do not import appointments"}}
		values := []string{"0"}
		for _, calendar := range calendars {
			options = append(options, tui.Choice{Label: calendar.Label()})
			values = append(values, fmt.Sprint(calendar.ID))
		}
		options = append(options, tui.Choice{Label: "New calendar: PST Import"})
		values = append(values, newCalendar)
		index, err := term.Select("Import appointments into which calendar?", options, 0, "")
		if err != nil {
			return err
		}
		switch values[index] {
		case "0":
			includeCalendar = false
		case newCalendar:
			calendar, err := store.NewCalendars().Create(ctx, db, store.CreateCalendarRequest{
				Owner: ownerRef(*owner), Name: "PST Import",
			})
			if err != nil {
				return fmt.Errorf("creating a calendar: %w", err)
			}
			calendarID = calendar.ID
		default:
			fmt.Sscan(values[index], &calendarID)
		}
	} else {
		calendar, err := store.NewCalendars().EnsureDefault(ctx, db, *owner, "UTC")
		if err != nil {
			return fmt.Errorf("preparing the default calendar: %w", err)
		}
		calendarID = calendar.ID
	}

	includeContacts := true
	addressBookID := int64(0)
	if interactive {
		books, err := store.NewAddressBooks().ListOwned(ctx, db, *owner)
		if err != nil {
			return fmt.Errorf("listing address books: %w", err)
		}
		personal, err := store.NewAddressBooks().EnsurePersonal(ctx, db, *owner)
		if err != nil {
			return fmt.Errorf("preparing the personal address book: %w", err)
		}
		const newBook = "\x00new"
		options := []tui.Choice{{Label: "Do not import contacts"}}
		values := []string{"0"}
		options = append(options, tui.Choice{Label: "My contacts (personal)"})
		values = append(values, fmt.Sprint(personal.ID))
		for _, book := range books {
			if book.ID == personal.ID {
				continue
			}
			options = append(options, tui.Choice{Label: book.Name})
			values = append(values, fmt.Sprint(book.ID))
		}
		options = append(options, tui.Choice{Label: "New address book: PST Import"})
		values = append(values, newBook)
		index, err := term.Select("Import contacts into which address book?", options, 1, "")
		if err != nil {
			return err
		}
		switch values[index] {
		case "0":
			includeContacts = false
		case newBook:
			book, err := store.NewAddressBooks().Create(ctx, db, store.AddressBook{
				Owner: *owner, Scope: "shared", Name: "PST Import",
				Slug: strings.ToLower(strings.ReplaceAll(*owner, "@", "-")) + "-pst-import",
			})
			if err != nil {
				return fmt.Errorf("creating an address book: %w", err)
			}
			addressBookID = book.ID
		default:
			fmt.Sscan(values[index], &addressBookID)
		}
	} else {
		personal, err := store.NewAddressBooks().EnsurePersonal(ctx, db, *owner)
		if err != nil {
			return fmt.Errorf("preparing the personal address book: %w", err)
		}
		addressBookID = personal.ID
	}

	// --- confirm ---------------------------------------------------------
	totalMail, totalEvents, totalContacts := 0, 0, 0
	for _, folder := range chosen {
		totalMail += folder.Messages
	}
	for _, folder := range folders {
		totalEvents += folder.Calendars
		totalContacts += folder.Contacts
	}
	if interactive {
		term.Section(0, 0, "Ready to import", "")
		term.KeyValue("Mail", fmt.Sprintf("%d message(s) into %d folder(s)", totalMail, len(mappings)))
		if includeCalendar {
			term.KeyValue("Appointments", fmt.Sprintf("%d", totalEvents))
		} else {
			term.KeyValue("Appointments", "not imported")
		}
		if includeContacts {
			term.KeyValue("Contacts", fmt.Sprintf("%d", totalContacts))
		} else {
			term.KeyValue("Contacts", "not imported")
		}
		if *dryRun {
			term.Warn("Dry run: nothing will be written.")
		} else {
			term.Warn("Leave this running until it finishes. Interrupting is safe, but partial batches are rolled back and the rest stays unimported.")
		}
		proceed, err := term.Confirm("Start the import?", true, "")
		if err != nil {
			return err
		}
		if !proceed {
			term.Info("nothing was imported")
			return nil
		}
	}

	blobs, err := blobstore.Open(blobstore.Options{Root: cfg.Dir("blobs")})
	if err != nil {
		return fmt.Errorf("opening the blob store: %w", err)
	}
	importer := pstimport.New(db, blobs)
	plan := pstimport.Plan{
		Owner:           *owner,
		SourcePath:      *pstPath,
		Folders:         mappings,
		IncludeCalendar: includeCalendar,
		CalendarID:      calendarID,
		IncludeContacts: includeContacts,
		AddressBookID:   addressBookID,
		BatchBytes:      *batchMB << 20,
		DryRun:          *dryRun,
	}
	last := time.Now()
	stats, err := importer.Import(ctx, file, plan, func(stats pstimport.Stats) {
		if time.Since(last) < 500*time.Millisecond {
			return
		}
		last = time.Now()
		term.Info("imported %d mail, %d appointment(s), %d contact(s), skipped %d",
			stats.Messages, stats.Events, stats.Contacts, stats.Skipped)
	})
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("import interrupted: %w", ctx.Err())
		}
		return err
	}
	term.Section(0, 0, "Import finished", "")
	term.Success("Mail imported: %d", stats.Messages)
	term.Success("Appointments imported: %d", stats.Events)
	term.Success("Contacts imported: %d", stats.Contacts)
	if stats.Skipped > 0 {
		term.Info("Skipped: %d (already imported, or empty)", stats.Skipped)
	}
	if !*dryRun {
		term.Info("Re-running the same file is safe: imported items are remembered.")
	}
	return nil
}

// importFolder is one source folder with the item counts shown in the picker.
type importFolder struct {
	ID        uint32
	Path      string
	Messages  int
	Calendars int
	Contacts  int
}

// collectImportFolders builds the folder list with per-folder counts. Message
// items belong to their parent folder; appointments and contacts are counted
// against their folder too, so the picker's hint is honest about where they
// live even though the import gathers them globally.
func collectImportFolders(pathOf func(id uint32) string, items []pst.Item) []importFolder {
	counts := make(map[uint32]*importFolder)
	get := func(id uint32) *importFolder {
		if folder, ok := counts[id]; ok {
			return folder
		}
		folder := &importFolder{ID: id, Path: pathOf(id)}
		if folder.Path == "" {
			folder.Path = fmt.Sprintf("folder-%d", id)
		}
		counts[id] = folder
		return folder
	}

	for _, item := range items {
		switch item.Kind {
		case "message":
			get(item.ParentID).Messages++
		case "appointment":
			get(item.ParentID).Calendars++
		case "contact":
			get(item.ParentID).Contacts++
		}
	}

	out := make([]importFolder, 0, len(counts))
	for _, folder := range counts {
		out = append(out, *folder)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Path) < strings.ToLower(out[j].Path)
	})
	return out
}

// commonFolderPrefix returns the deepest path shared by every chosen folder,
// so that importing one folder into "PST Import" does not repeat the folder's
// own name beneath it.
func commonFolderPrefix(folders []importFolder) string {
	if len(folders) == 0 {
		return ""
	}
	parts := strings.Split(folders[0].Path, "/")
	for _, folder := range folders[1:] {
		other := strings.Split(folder.Path, "/")
		n := 0
		for n < len(parts) && n < len(other) && parts[n] == other[n] {
			n++
		}
		parts = parts[:n]
	}
	return strings.Join(parts, "/")
}

func validateMailboxName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("a folder name is required")
	}
	if strings.Contains(name, "//") || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") {
		return fmt.Errorf("a folder name must not start, end or contain an empty path segment")
	}
	return nil
}

func bitness(file *pst.File) string {
	if file.Is64Bit() {
		return "64-bit Unicode"
	}
	return "32-bit ANSI"
}

func truncateLeft(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n+1:]
}

func ownerRef(owner string) *string { return &owner }
