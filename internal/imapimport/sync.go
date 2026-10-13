package imapimport

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/linchange/linchange/internal/mail"
)

const fetchBatchSize = 50

var (
	ErrInvalid = errors.New("imapimport: invalid request")
	ErrDenied  = errors.New("imapimport: not permitted")
)

func (s *Service) sync(ctx context.Context, job Job) error {
	var sealed []byte
	if err := s.db.GetContext(ctx, &sealed,
		`SELECT remote_password FROM imap_import_jobs WHERE id = $1`, job.ID); err != nil {
		return fmt.Errorf("imapimport: reading stored password: %w", err)
	}
	password, err := decryptPassword(s.key, sealed)
	if err != nil {
		return err
	}

	client, err := s.dial(ctx, RemoteParams{
		Host:             job.RemoteHost,
		Port:             job.RemotePort,
		TLS:              job.RemoteTLS,
		Username:         job.RemoteUsername,
		Password:         password,
		AcceptSelfSigned: job.AcceptSelfSigned,
	})
	if err != nil {
		return err
	}
	defer client.Close()

	if err := client.Login(job.RemoteUsername, password).Wait(); err != nil {
		return fmt.Errorf("imapimport: remote login: %w", err)
	}

	cmd := client.List("", "*", nil)
	entries, err := cmd.Collect()
	if err != nil {
		return fmt.Errorf("imapimport: listing remote folders: %w", err)
	}

	var delim rune = '/'
	var folders []string
	for _, entry := range entries {
		if hasAttr(entry.Attrs, imap.MailboxAttrNoSelect) {
			continue
		}
		if !underRoot(entry.Mailbox, job.SourceRoot, entry.Delim) {
			continue
		}
		if entry.Delim != 0 {
			delim = entry.Delim
		}
		folders = append(folders, entry.Mailbox)
	}

	p := progress{folders: len(folders)}
	s.saveProgress(ctx, job.ID, p)

	quotaLimit, err := s.quotaLimit(ctx, job.Owner)
	if err != nil {
		return err
	}

	for _, folder := range folders {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.syncFolder(ctx, client, job, folder, delim, quotaLimit, &p); err != nil {
			return err
		}
		p.foldersDone++
		s.saveProgress(ctx, job.ID, p)
	}
	return nil
}

func (s *Service) syncFolder(ctx context.Context, client *imapclient.Client, job Job, folder string, delim rune, quotaLimit int64, p *progress) error {
	sel, err := client.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return fmt.Errorf("imapimport: selecting %q: %w", folder, err)
	}
	if sel.NumMessages == 0 {
		return nil
	}
	uidValidity := int64(sel.UIDValidity)

	imported, err := s.importedUIDs(ctx, job.ID, folder, uidValidity)
	if err != nil {
		return err
	}

	target := targetName(job.SourceRoot, job.TargetRoot, folder, delim)
	mailboxID, err := s.mailbox.EnsureNamed(ctx, s.db, job.Owner, target)
	if err != nil {
		return fmt.Errorf("imapimport: preparing target folder %q: %w", target, err)
	}

	maxUID := uint32(sel.UIDNext) - 1
	for start := uint32(1); start <= maxUID; start += fetchBatchSize {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := start + fetchBatchSize - 1
		if end > maxUID {
			end = maxUID
		}
		var set imap.UIDSet
		set.AddRange(imap.UID(start), imap.UID(end))

		cmd := client.Fetch(set, &imap.FetchOptions{
			UID:          true,
			Flags:        true,
			InternalDate: true,
			RFC822Size:   true,
			BodySection:  []*imap.FetchItemBodySection{{Peek: true}},
		})
		messages, err := cmd.Collect()
		if err != nil {
			return fmt.Errorf("imapimport: fetching from %q: %w", folder, err)
		}

		for _, msg := range messages {
			if err := ctx.Err(); err != nil {
				return err
			}
			uid := int64(msg.UID)
			p.seen++
			if imported[uid] {
				p.skipped++
				continue
			}
			if msg.InternalDate.IsZero() {
				msg.InternalDate = time.Now()
			}

			var raw []byte
			for _, section := range msg.BodySection {
				raw = section.Bytes
				break
			}
			if len(raw) == 0 {
				p.skipped++
				continue
			}

			digest, size, err := s.blobs.Put(raw)
			if err != nil {
				return fmt.Errorf("imapimport: storing blob: %w", err)
			}
			sha := digest[:]

			exists, err := s.mailboxHasBlob(ctx, mailboxID, sha)
			if err != nil {
				return err
			}
			if exists {
				if err := s.recordItem(ctx, job.ID, folder, uid, uidValidity, mailboxID, sha); err != nil {
					return err
				}
				imported[uid] = true
				p.skipped++
				continue
			}

			exceeded, err := s.quotas.Exceeded(ctx, s.db, job.Owner, quotaLimit, size)
			if err != nil {
				return err
			}
			if exceeded {
				return fmt.Errorf("imapimport: quota exceeded for %s while importing %q", job.Owner, folder)
			}

			meta, err := mail.Describe(raw)
			if err != nil {
				p.skipped++
				continue
			}

			flags := importFlags(msg.Flags)
			if _, err := s.messages.Insert(ctx, s.db, mailboxID, sha, size, msg.InternalDate, flags, meta); err != nil {
				return fmt.Errorf("imapimport: inserting message: %w", err)
			}
			if _, err := s.quotas.Adjust(ctx, s.db, job.Owner, size, 1); err != nil {
				return err
			}
			if err := s.recordItem(ctx, job.ID, folder, uid, uidValidity, mailboxID, sha); err != nil {
				return err
			}
			imported[uid] = true
			p.imported++
			p.bytes += size
		}

		s.saveProgress(ctx, job.ID, *p)
	}
	return nil
}

func (s *Service) quotaLimit(ctx context.Context, owner string) (int64, error) {
	usr, err := s.users.Get(ctx, s.db, owner)
	if err != nil {
		return 0, fmt.Errorf("imapimport: reading quota for %s: %w", owner, err)
	}
	if usr.QuotaBytes != nil {
		if *usr.QuotaBytes == 0 {
			return 0, nil
		}
		return *usr.QuotaBytes, nil
	}
	return 10_737_418_240, nil
}

func importFlags(flags []imap.Flag) []string {
	out := make([]string, 0, len(flags))
	for _, flag := range flags {
		if string(flag) == `\Recent` {
			continue
		}
		out = append(out, string(flag))
	}
	return out
}

func underRoot(name, root string, delim rune) bool {
	if root == "" {
		return true
	}
	if delim == 0 {
		delim = '/'
	}
	d := string(delim)
	root = trimTrailing(root, d)
	return name == root || len(name) > len(root) && name[:len(root)] == root && name[len(root)] == byte(delim)
}

func trimTrailing(s, suffix string) string {
	for len(s) > 0 && s[len(s)-1] == suffix[0] {
		s = s[:len(s)-1]
	}
	return s
}

func targetName(sourceRoot, targetRoot, folder string, delim rune) string {
	if delim == 0 {
		delim = '/'
	}
	name := folder
	if sourceRoot != "" {
		root := trimTrailing(sourceRoot, string(delim))
		if name == root {
			name = ""
		} else if len(name) > len(root) && name[:len(root)] == root && name[len(root)] == byte(delim) {
			name = name[len(root)+1:]
		}
	}
	if delim != '/' {
		name = replaceAll(name, string(delim), "/")
	}
	if targetRoot != "" {
		if name == "" {
			name = targetRoot
		} else {
			name = targetRoot + "/" + name
		}
	}
	if name == "" {
		name = "INBOX"
	}
	return name
}

func replaceAll(s, old, new string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		if i+len(old) <= len(s) && s[i:i+len(old)] == old {
			out = append(out, new...)
			i += len(old)
			continue
		}
		out = append(out, s[i])
		i++
	}
	return string(out)
}
