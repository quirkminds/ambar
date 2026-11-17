// Package moments is the SQL for what a vault keeps. Processing creates moments; later the library
// endpoints read them. Every query takes the pool or a transaction, so callers decide what is atomic.
package moments

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"ambar/api/vaults"
)

// Moment is one thing kept. SHA256 is hex.
type Moment struct {
	ID          string
	VaultID     string
	CreatedBy   string
	Kind        string
	Name        string
	ContentType string
	SHA256      string
	CapturedAt  *time.Time
	Width       int
	Height      int
}

// File is one of a moment's files in R2.
type File struct {
	Variant     string
	Key         string
	ContentType string
	Size        int64
	Width       int
	Height      int
}

// Key is where a moment's file lives in R2: ids only, never a name.
func Key(vaultID, momentID, variant string) string {
	return "vaults/" + vaultID + "/moments/" + momentID + "/" + variant
}

// NewID is a time-ordered id for a moment, from Postgres (the one clock every rule uses).
func NewID(ctx context.Context, db vaults.DB) (string, error) {
	var id string
	err := db.QueryRow(ctx, `SELECT uuidv7()::text`).Scan(&id)

	return id, err
}

// Used is the bytes a vault's moments take against its quota: their originals, Trash included (it only
// stops counting when a moment is erased for good, so deleting and keeping again can't get around it).
func Used(ctx context.Context, db vaults.DB, vaultID string) (int64, error) {
	var used int64

	err := db.QueryRow(ctx, `
		SELECT coalesce(sum(f.size), 0)
		FROM moments m JOIN moment_files f ON f.moment_id = m.id AND f.variant = 'original'
		WHERE m.vault_id = $1`, vaultID).Scan(&used)

	return used, err
}

// Duplicate is the moment in the vault, outside the Trash, with this fingerprint.
func Duplicate(ctx context.Context, db vaults.DB, vaultID, sha256Hex string) (string, bool, error) {
	sum, err := hex.DecodeString(sha256Hex)
	if err != nil {
		return "", false, fmt.Errorf("moments: bad fingerprint: %w", err)
	}

	var id string

	err = db.QueryRow(ctx, `
		SELECT id::text FROM moments WHERE vault_id = $1 AND sha256 = $2 AND deleted_at IS NULL
		ORDER BY created_at LIMIT 1`, vaultID, sum).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}

	return id, err == nil, err
}

// Create records a moment and its files.
func Create(ctx context.Context, db vaults.DB, m Moment, files []File) error {
	sum, err := hex.DecodeString(m.SHA256)
	if err != nil {
		return fmt.Errorf("moments: bad fingerprint: %w", err)
	}

	if _, err := db.Exec(ctx, `
		INSERT INTO moments (id, vault_id, created_by, kind, name, content_type, sha256, captured_at, width, height)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, nullif($9, 0), nullif($10, 0))`,
		m.ID, m.VaultID, m.CreatedBy, m.Kind, m.Name, m.ContentType, sum, m.CapturedAt, m.Width, m.Height); err != nil {
		return fmt.Errorf("create moment: %w", err)
	}

	for _, f := range files {
		if _, err := db.Exec(ctx, `
			INSERT INTO moment_files (moment_id, variant, key, content_type, size, width, height)
			VALUES ($1, $2, $3, $4, $5, nullif($6, 0), nullif($7, 0))`,
			m.ID, f.Variant, f.Key, f.ContentType, f.Size, f.Width, f.Height); err != nil {
			return fmt.Errorf("create moment file: %w", err)
		}
	}

	return nil
}
