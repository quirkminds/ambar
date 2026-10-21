// Package vaults is who can do what with a library. Everything a person keeps lives in a vault; each
// account has a personal one, created with the account, and access is a vault_members row with a role.
// SQL lives here so every feature (uploads, moments, sharing) asks the same questions the same way.
package vaults

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// SeedStorageBytes is the Seed plan's storage, the limit of every new vault. It must match storageBytes
// for "seed" in packages/core/src/plans.ts (TestPlanLimitsMatchCore checks it).
const SeedStorageBytes int64 = 2 << 30

type Role string

const (
	Owner  Role = "owner"
	Editor Role = "editor"
	Viewer Role = "viewer"
)

// CanAdd: owners and editors keep things in a vault; viewers only look.
func (r Role) CanAdd() bool { return r == Owner || r == Editor }

// ErrNotFound: no such vault, or the user isn't a member. The two are the same answer on purpose, so a
// vault id never confirms that a vault exists.
var ErrNotFound = errors.New("vault not found")

// DB is what these queries need: the pool or a transaction.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// CreatePersonal gives a new account its vault, with the account as owner. Called in the transaction
// that creates the user, so an account never exists without one.
func CreatePersonal(ctx context.Context, db DB, userID string) error {
	_, err := db.Exec(ctx, `
		WITH vault AS (
			INSERT INTO vaults (personal_of, storage_limit_bytes) VALUES ($1, $2) RETURNING id
		)
		INSERT INTO vault_members (vault_id, user_id, role) SELECT id, $1, 'owner' FROM vault`, userID, SeedStorageBytes)
	if err != nil {
		return fmt.Errorf("create personal vault: %w", err)
	}

	return nil
}

// Personal is the user's own vault.
func Personal(ctx context.Context, db DB, userID string) (string, error) {
	var id string

	err := db.QueryRow(ctx, `SELECT id::text FROM vaults WHERE personal_of = $1`, userID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}

	return id, err
}

// RoleOf is the user's role in the vault. vaultID must be a valid UUID (the handlers check).
func RoleOf(ctx context.Context, db DB, userID, vaultID string) (Role, error) {
	var role Role

	err := db.QueryRow(ctx, `SELECT role FROM vault_members WHERE vault_id = $1 AND user_id = $2`, vaultID, userID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}

	return role, err
}
