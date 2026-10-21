package uploads

import (
	"context"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ambar/api/vaults"
)

// store is the SQL for uploads (migrations/00007_uploads.sql). Time comes from the database (now()), so
// every rule uses one clock.
type store struct {
	db *pgxpool.Pool
}

// holding is the uploads that hold bytes against the vault's quota: uploaded or processing, or pending and
// still in time (one past expires_at holds nothing, whether or not the cleanup has marked it yet).
const holding = `(status IN ('uploaded', 'processing') OR (status = 'pending' AND expires_at > now()))`

const columns = `id::text, status, name, size, content_type, encode(sha256, 'hex'), created_at, expires_at, coalesce(failure_code, '')`

func scan(row pgx.Row, u *Upload, extra ...any) error {
	return row.Scan(append([]any{&u.ID, &u.Status, &u.Name, &u.Size, &u.ContentType, &u.SHA256, &u.CreatedAt, &u.ExpiresAt, &u.FailureCode}, extra...)...)
}

// insertFunc records one accepted file in the locked vault.
type insertFunc func(File) (Upload, error)

// reserve runs decide in one transaction with the vault locked, after checking the caller may add to it.
// quota arrives with what is already used and reserved; decide adds to it as it accepts files, inserting
// each one with insert.
func (s store) reserve(ctx context.Context, userID, vaultID string, within time.Duration, decide func(quota *Quota, pending int, insert insertFunc) ([]Result, error)) ([]Result, Quota, error) {
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

		var pending int

		// Kept moments will count in Used once they exist; today only reservations hold bytes.
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

		var err error

		results, err = decide(&quota, pending, insert)

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
		WHERE status IN ('expired', 'failed') AND completed_at < now() - make_interval(secs => $1)`, olderThan.Seconds())

	return tag.RowsAffected(), err
}
