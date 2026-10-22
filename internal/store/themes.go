package store

import (
	"context"
	"fmt"
	"time"
)

// Theme is a custom theme uploaded by an administrator. The built-in light,
// dark and system themes have no row: they are part of the client.
type Theme struct {
	ID         int64     `db:"id"`
	UID        string    `db:"uid"`
	Name       string    `db:"name"`
	Prefix     string    `db:"prefix"`
	BlobSHA256 []byte    `db:"blob_sha256"`
	CreatedBy  string    `db:"created_by"`
	CreatedAt  time.Time `db:"created_at"`
}

// Themes reads and writes the custom theme catalogue.
type Themes struct{}

// NewThemes returns the repository. It is stateless, so this is free.
func NewThemes() *Themes { return &Themes{} }

const themeColumns = `
	id, uid, name, prefix, blob_sha256, created_by, created_at`

// Create records an uploaded theme. The ZIP itself is already in the blob
// store; prefix is the directory inside it that holds theme.css.
func (t *Themes) Create(ctx context.Context, q Querier, uid, name, prefix, createdBy string, sha []byte) (Theme, error) {
	var out Theme
	err := q.GetContext(ctx, &out, `
		INSERT INTO themes (uid, name, prefix, blob_sha256, created_by)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+themeColumns, uid, name, prefix, sha, createdBy)
	if err != nil {
		if IsConflict(err) {
			return Theme{}, ErrConflict
		}
		return Theme{}, fmt.Errorf("store: creating theme: %w", err)
	}
	return out, nil
}

// List returns every custom theme, newest last so a selector is stable.
func (t *Themes) List(ctx context.Context, q Querier) ([]Theme, error) {
	rows := make([]Theme, 0, 8)
	if err := q.SelectContext(ctx, &rows,
		`SELECT `+themeColumns+` FROM themes ORDER BY created_at, id`); err != nil {
		return nil, fmt.Errorf("store: listing themes: %w", err)
	}
	return rows, nil
}

// Get returns a theme by its public uid, or ErrNotFound.
func (t *Themes) Get(ctx context.Context, q Querier, uid string) (Theme, error) {
	var out Theme
	if err := q.GetContext(ctx, &out,
		`SELECT `+themeColumns+` FROM themes WHERE uid = $1`, uid); err != nil {
		if IsNotFound(err) {
			return Theme{}, ErrNotFound
		}
		return Theme{}, fmt.Errorf("store: loading theme: %w", err)
	}
	return out, nil
}

// Delete removes the catalogue row. The ZIP is left for the blob store's
// collector, as every other unreferenced blob is.
func (t *Themes) Delete(ctx context.Context, q Querier, uid string) error {
	res, err := q.ExecContext(ctx, `DELETE FROM themes WHERE uid = $1`, uid)
	if err != nil {
		return fmt.Errorf("store: deleting theme: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
