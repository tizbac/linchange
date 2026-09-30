package migrations

import (
	"strings"
	"testing"
)

func TestAllParsesEmbeddedMigrations(t *testing.T) {
	all, err := All()
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("no migrations are embedded")
	}

	seenVersion := make(map[int64]bool)
	var previous int64

	for _, m := range all {
		if strings.TrimSpace(m.SQL) == "" {
			t.Errorf("migration %04d_%s is empty", m.Version, m.Name)
		}
		if seenVersion[m.Version] {
			t.Errorf("duplicate version %d", m.Version)
		}
		seenVersion[m.Version] = true

		// Versions must be strictly increasing, because Migrate applies them
		// in this order and a gap-free monotonic sequence is what makes the
		// applied set predictable.
		if m.Version <= previous {
			t.Errorf("migration %04d_%s is not after version %d", m.Version, m.Name, previous)
		}
		previous = m.Version
	}
}

func TestLatestVersion(t *testing.T) {
	all, err := All()
	if err != nil {
		t.Fatal(err)
	}
	latest, err := LatestVersion()
	if err != nil {
		t.Fatal(err)
	}
	if latest != all[len(all)-1].Version {
		t.Errorf("LatestVersion = %d, want %d", latest, all[len(all)-1].Version)
	}
}

func TestParseName(t *testing.T) {
	cases := []struct {
		in          string
		wantVersion int64
		wantName    string
		wantErr     bool
	}{
		{in: "0001_initial.sql", wantVersion: 1, wantName: "initial"},
		{in: "0042_add_quotas.sql", wantVersion: 42, wantName: "add_quotas"},
		{in: "12_no_desc.sql", wantVersion: 12, wantName: "no_desc"},
		{in: "no_version.sql", wantErr: true},
		{in: "abc_name.sql", wantErr: true},
		{in: "0_zero.sql", wantErr: true},
		{in: "0001_.sql", wantErr: true},
		{in: "0001missing.sql", wantErr: true},
	}

	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			version, name, err := parseName(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected an error for %q", c.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if version != c.wantVersion {
				t.Errorf("version = %d, want %d", version, c.wantVersion)
			}
			if name != c.wantName {
				t.Errorf("name = %q, want %q", name, c.wantName)
			}
		})
	}
}

// The initial migration is the foundation of every table, so a regression there
// would be discovered far from its cause.
func TestInitialSchemaCoversCoreTables(t *testing.T) {
	all, err := All()
	if err != nil {
		t.Fatal(err)
	}
	first := all[0].SQL

	for _, table := range []string{
		"domains", "users", "credentials", "ldap_links",
		"user_secondary_emails", "user_totp",
		"workgroups", "workgroup_members",
		"aliases", "shared_mailbox_grants",
		"mailboxes", "messages", "message_addresses", "message_references",
		"quota_usage", "send_counters",
		"outbox", "greylist",
		"calendars", "events", "attendees",
		"sessions", "reset_tokens", "impersonation_grants", "audit_log",
		"dkim_keys", "ldap_sync_state",
	} {
		if !strings.Contains(first, "CREATE TABLE "+table+" ") {
			t.Errorf("initial migration does not create table %q", table)
		}
	}
}
