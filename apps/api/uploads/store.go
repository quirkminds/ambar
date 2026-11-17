package uploads

import (
	"context"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ambar/api/moments"
	"ambar/api/vaults"
)

// store is the SQL for uploads (migrations/00007_uploads.sql, 00008_moments.sql). Time comes from the database (now()), so
// every rule uses one clock.
type store struct {
	db *pgxpool.Pool
	// onlyVault, when set, keeps claim to one vault. Only tests set it: packages tested in parallel share
	// one database and must not process each other's uploads.
	onlyVault string
}

// holding is the uploads that hold bytes against the vault's quota: uploaded or processing, or pending and
// still in time (one past expires_at holds nothing, whether or not the cleanup has marked it yet).
const holding = `(status IN ('uploaded', 'processing') OR (status = 'pending' AND expires_at > now()))`

const columns = `id::text, vault_id::text, user_id::text, status, name, size, content_type, encode(sha256, 'hex'),
	created_at, expires_at, coalesce(failure_code, ''), coalesce(moment_id::text, ''), captured_at, attempts`

func scan(row pgx.Row, u *Upload, extra ...any) error {
	return row.Scan(append([]any{&u.ID, &u.VaultID, &u.UserID, &u.Status, &u.Name, &u.Size, &u.ContentType, &u.SHA256,
		&u.CreatedAt, &u.ExpiresAt, &u.FailureCode, &u.MomentID, &u.CapturedAt, &u.Attempts}, extra...)...)
}

// reservation is what decide can do inside the locked vault: record an accepted file, or find the moment
// a file duplicates.
type reservation struct {
	insert    func(File) (Upload, error)
	duplicate func(sha256Hex string) (string, bool, error)
}

// reserve runs decide in one transaction with the vault locked, after checking the caller may add to it.
// quota arrives with what is already used and reserved; decide adds to it as it accepts files.
func (s store) reserve(ctx context.Context, userID, vaultID string, within time.Duration, decide func(quota *Quota, pending int, tx reservation) ([]Result, error)) ([]Result, Quota, error) {
	var (
		results []Result
		quota   Quota
	)

	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if vaultID == "" {
			personal, err := vaults.Personal(ctx, tx, userID)
			if err != nil {
				return err
			}

			vaultID = personal
		} else {
			role, err := vaults.RoleOf(ctx, tx, userID, vaultID)
			if err != nil {
				return err
			}

			if !role.CanAdd() {
				return ErrReadOnly
			}
		}

		// The lock serializes every reservation in this vault until commit.
		if err := tx.QueryRow(ctx, `SELECT storage_limit_bytes FROM vaults WHERE id = $1 FOR UPDATE`, vaultID).Scan(&quota.Limit); err != nil {
			return err
		}

		var (
			pending int
			err     error
		)

		if quota.Used, err = moments.Used(ctx, tx, vaultID); err != nil {
			return err
		}

		if err := tx.QueryRow(ctx, `
			SELECT coalesce(sum(size), 0), count(*) FILTER (WHERE status = 'pending')
			FROM uploads WHERE vault_id = $1 AND status IN ('pending', 'uploaded', 'processing') AND `+holding, vaultID).Scan(&quota.Reserved, &pending); err != nil {
			return err
		}

		insert := func(f File) (Upload, error) {
			sum, _ := hex.DecodeString(f.SHA256)

			var u Upload

			err := scan(tx.QueryRow(ctx, `
				INSERT INTO uploads (vault_id, user_id, name, content_type, size, sha256, captured_at, width, height, duration_ms, expires_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, now() + make_interval(secs => $11))
				RETURNING `+columns,
				vaultID, userID, f.Name, f.ContentType, f.Size, sum, f.CapturedAt, f.Width, f.Height, f.DurationMs, within.Seconds()), &u)

			return u, err
		}

		duplicate := func(sha256Hex string) (string, bool, error) { return moments.Duplicate(ctx, tx, vaultID, sha256Hex) }

		results, err = decide(&quota, pending, reservation{insert: insert, duplicate: duplicate})

		return err
	})

	return results, quota, err
}

// get is one upload of the user's, and whether its time to complete ran out.
func (s store) get(ctx context.Context, userID, uploadID string) (Upload, bool, error) {
	var (
		u       Upload
		expired bool
	)

	err := scan(s.db.QueryRow(ctx, `
		SELECT `+columns+`, expires_at <= now() FROM uploads WHERE id = $1 AND user_id = $2`, uploadID, userID), &u, &expired)
	if errors.Is(err, pgx.ErrNoRows) {
		return Upload{}, false, ErrNotFound
	}

	return u, expired, err
}

// finish moves a pending upload to status. When another call got there first, it returns the upload as
// that call left it.
func (s store) finish(ctx context.Context, uploadID, status, failureCode string) (Upload, error) {
	var u Upload

	err := scan(s.db.QueryRow(ctx, `
		UPDATE uploads SET status = $2, failure_code = nullif($3, ''), completed_at = now()
		WHERE id = $1 AND status = 'pending'
		RETURNING `+columns, uploadID, status, failureCode), &u)
	if errors.Is(err, pgx.ErrNoRows) {
		err = scan(s.db.QueryRow(ctx, `SELECT `+columns+` FROM uploads WHERE id = $1`, uploadID), &u)
	}

	if errors.Is(err, pgx.ErrNoRows) {
		// Cancelled meanwhile.
		return Upload{}, ErrNotFound
	}

	return u, err
}

// cancel deletes a pending upload of the user's. cancelled is false for an expired one, which is already
// released.
func (s store) cancel(ctx context.Context, userID, uploadID string) (cancelled bool, err error) {
	// The status is checked by the DELETE itself, so a Complete that commits first wins: a snapshot read
	// before it could delete an upload that had just become uploaded.
	tag, err := s.db.Exec(ctx, `DELETE FROM uploads WHERE id = $1 AND user_id = $2 AND status = 'pending' AND expires_at > now()`, uploadID, userID)
	if err != nil {
		return false, err
	}

	if tag.RowsAffected() == 1 {
		return true, nil
	}

	var status string

	err = s.db.QueryRow(ctx, `
		SELECT CASE WHEN status = 'pending' AND expires_at <= now() THEN 'expired' ELSE status END
		FROM uploads WHERE id = $1 AND user_id = $2`, uploadID, userID).Scan(&status)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, ErrNotFound
	case err != nil:
		return false, err
	case status == Pending:
		return true, nil
	case status == Expired:
		return false, nil
	default:
		return false, ErrNotPending
	}
}

// expire releases pending uploads whose time ran out.
func (s store) expire(ctx context.Context) ([]string, error) {
	rows, err := s.db.Query(ctx, `
		UPDATE uploads SET status = 'expired', completed_at = now()
		WHERE status = 'pending' AND expires_at <= now()
		RETURNING id::text`)
	if err != nil {
		return nil, err
	}

	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (s store) deleteFinished(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := s.db.Exec(ctx, `
		DELETE FROM uploads
		WHERE status IN ('ready', 'expired', 'failed') AND completed_at < now() - make_interval(secs => $1)`, olderThan.Seconds())

	return tag.RowsAffected(), err
}

// canAdd: the user is still an owner or editor of the vault.
func (s store) canAdd(ctx context.Context, userID, vaultID string) (bool, error) {
	role, err := vaults.RoleOf(ctx, s.db, userID, vaultID)
	if errors.Is(err, vaults.ErrNotFound) {
		return false, nil
	}

	return role.CanAdd(), err
}

// claim leases the oldest upload waiting to be processed: uploaded, or processing with a lease that ran
// out (its worker died). attempts counts every lease, so a file that keeps crashing workers gives up.
func (s store) claim(ctx context.Context, lease time.Duration) (Upload, bool, error) {
	var u Upload

	err := scan(s.db.QueryRow(ctx, `
		UPDATE uploads SET status = 'processing', lease_until = now() + make_interval(secs => $1), attempts = attempts + 1
		WHERE id = (
			SELECT id FROM uploads
			WHERE (status = 'uploaded' OR (status = 'processing' AND lease_until < now()))
				AND ($2 = '' OR vault_id::text = $2)
			ORDER BY completed_at
			FOR UPDATE SKIP LOCKED
			LIMIT 1)
		RETURNING `+columns, lease.Seconds(), s.onlyVault), &u)
	if errors.Is(err, pgx.ErrNoRows) {
		return Upload{}, false, nil
	}

	return u, err == nil, err
}

// errLeaseLost: another worker took the upload over (this one was too slow); its result is discarded.
var errLeaseLost = errors.New("uploads: lease lost")

// errRevoked: whoever sent it can no longer add to the vault.
var errRevoked = errors.New("uploads: access revoked")

// ready keeps the processed upload: in one transaction with the vault locked, it creates the moment and
// marks the upload ready, so its bytes move from reserved to used at once. When the vault already keeps
// the same fingerprint, the upload points at that moment instead and nothing is created (duplicate).
func (s store) ready(ctx context.Context, u Upload, m moments.Moment, files []moments.File) (momentID string, duplicate bool, err error) {
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM vaults WHERE id = $1 FOR UPDATE`, u.VaultID); err != nil {
			return err
		}

		role, err := vaults.RoleOf(ctx, tx, u.UserID, u.VaultID)
		if errors.Is(err, vaults.ErrNotFound) || (err == nil && !role.CanAdd()) {
			return errRevoked
		} else if err != nil {
			return err
		}

		momentID, duplicate, err = moments.Duplicate(ctx, tx, u.VaultID, u.SHA256)
		if err != nil {
			return err
		}

		if !duplicate {
			if err := moments.Create(ctx, tx, m, files); err != nil {
				return err
			}

			momentID = m.ID
		}

		tag, err := tx.Exec(ctx, `
			UPDATE uploads SET status = 'ready', moment_id = $2, completed_at = now(), lease_until = NULL
			WHERE id = $1 AND status = 'processing' AND attempts = $3`, u.ID, momentID, u.Attempts)
		if err != nil {
			return err
		}

		if tag.RowsAffected() != 1 {
			return errLeaseLost
		}

		return nil
	})

	return momentID, duplicate, err
}

// fail ends a processing upload that can't be kept, releasing its reservation. Like ready, it only applies
// while this worker still holds the lease.
func (s store) fail(ctx context.Context, u Upload, code string) error {
	_, err := s.db.Exec(ctx, `
		UPDATE uploads SET status = 'failed', failure_code = $2, completed_at = now(), lease_until = NULL
		WHERE id = $1 AND status = 'processing' AND attempts = $3`, u.ID, code, u.Attempts)

	return err
}

// newMomentID is a time-ordered id from Postgres, so moment ids sort like the moments.
func (s store) newMomentID(ctx context.Context) (string, error) { return moments.NewID(ctx, s.db) }
