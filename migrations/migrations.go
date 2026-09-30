// Package migrations embeds the ordered SQL schema migrations.
//
// The files are embedded rather than read from disk so that a compiled binary
// carries its own schema and cannot be pointed at the wrong directory. A
// migration file is named NNNN_description.sql; NNNN is the version and must
// be unique and increasing.
package migrations

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

//go:embed *.sql
var files embed.FS

// Migration is one versioned schema change.
type Migration struct {
	Version int64
	Name    string
	SQL     string
}

// All returns every embedded migration ordered by version.
//
// It returns an error rather than a partial list if the embed contains a file
// that is not a well-formed migration name, because a silently skipped
// migration is far worse than a startup failure.
func All() ([]Migration, error) {
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		return nil, fmt.Errorf("migrations: reading embedded files: %w", err)
	}

	var out []Migration
	seen := make(map[int64]string, len(entries))

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".sql") {
			// Testdata and similar are not migrations.
			continue
		}

		version, desc, err := parseName(name)
		if err != nil {
			return nil, fmt.Errorf("migrations: %w", err)
		}

		if other, dup := seen[version]; dup {
			return nil, fmt.Errorf("migrations: version %d is claimed by both %q and %q",
				version, other, name)
		}
		seen[version] = name

		body, err := files.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("migrations: reading %s: %w", name, err)
		}
		out = append(out, Migration{Version: version, Name: desc, SQL: string(body)})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// LatestVersion returns the highest embedded migration version, or 0 when
// there are none.
func LatestVersion() (int64, error) {
	all, err := All()
	if err != nil {
		return 0, err
	}
	if len(all) == 0 {
		return 0, nil
	}
	return all[len(all)-1].Version, nil
}

// parseName splits "0003_add_quota.sql" into (3, "add_quota"). The version is
// fixed-width so that lexical and numeric ordering agree.
func parseName(name string) (int64, string, error) {
	base := strings.TrimSuffix(name, ".sql")

	underscore := strings.Index(base, "_")
	if underscore < 0 {
		return 0, "", fmt.Errorf("%q is not named NNNN_description.sql", name)
	}

	version, err := strconv.ParseInt(base[:underscore], 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("%q has a non-numeric version: %w", name, err)
	}
	if version < 1 {
		return 0, "", fmt.Errorf("%q has a version below 1", name)
	}

	desc := base[underscore+1:]
	if desc == "" {
		return 0, "", fmt.Errorf("%q has an empty description", name)
	}
	return version, desc, nil
}
