# PST and OST import

linchange imports Outlook data files without a conversion tool in between. The
reader is written from scratch, twice: `internal/pst` in Go and
`web/src/app/core/pst` in TypeScript. Neither wraps libpff or another library;
they implement the MS-PST layout directly.

## What is imported

- Mail: messages become ordinary RFC 5322 messages in the chosen mailbox
  folders, so they thread, search and display like mail that arrived over SMTP.
  Attachments are carried in the MIME body.
- Appointments: imported into a calendar as iCalendar events.
- Contacts: imported into an address book as vCard 3.0 cards.

Items Outlook uses internally (`IPM.Configuration.*`, search-folder records and
similar) are recognised and skipped rather than imported as mail.

## From the command line

```sh
linchange pst-import --config /etc/linchange/linchange.yaml \
  --file archive.pst --owner alice@example.com
```

The command opens the archive on the server, lists the folders it contains,
and asks:

1. which source folders to import (space toggles, enter accepts);
2. where the mail should go — a new `PST Import` tree, `INBOX`, or any existing
   folder; each source folder keeps its name underneath;
3. which calendar receives the appointments, or that none should be imported;
4. which address book receives the contacts, or that none should be imported.

It finishes with a summary and a confirmation. `--dry-run` reads and converts
everything but writes nothing; `--yes` accepts the recommended choices without
asking; `--batch-mb` sets the largest transaction batch (64 MiB by default).

## Re-running is safe

Every item is recorded in `pst_import_items`, keyed by the archive's SHA-256 and
the item's stable PST node identifier. A second run over the same file imports
nothing. Messages are additionally deduplicated by their blob digest in the
target folder, so a message that already exists there is not inserted again
even if the archive was re-exported and its node identifiers changed. Contacts
are deduplicated by vCard UID and events by UID.

A batch is written in one database transaction. An interrupted import leaves
the completed batches in place and the current batch untouched; running the
command again continues where it stopped.

## Limits

- 32-bit (ANSI, Outlook 97-2002) and 64-bit (Unicode, Outlook 2007+) files are
  supported, including the per-block XOR obfuscation and multi-block data
  arrays.
- Compressed 4k-page OST files are rejected with a clear message. Export them
  to a `.pst` from Outlook first.
- Recurring appointments are imported as their first occurrence. The recurrence
  blob is not expanded into individual events.
- Encrypted PST files need their password removed in Outlook before import.

## Browser import

The TypeScript reader exposes the same model (`PstReader`, `buildMessage`,
`buildVCard`, `buildICalendar`) and reads the file through `File.slice`, so the
archive never leaves the machine in one piece. Upload batches are capped at
64 MiB, matching the server-side transaction batch.
