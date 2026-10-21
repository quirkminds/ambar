// Package uploads takes files from clients to storage without their bytes passing through the API.
// Create checks the vault role, each file and the quota, reserves the bytes and signs one PUT per file;
// the client sends the file straight to the bucket and calls Complete, which checks what arrived. The
// rules (limits, reservation, expiry) live here; HTTP lives in the handlers, SQL in store.go.
package uploads

import (
	"context"
	"encoding/hex"
	"errors"
	"math"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"

	"ambar/api/storage"
)

const (
	// MaxFileBytes must match MAX_FILE_BYTES in packages/core/src/moment.ts (TestPlanLimitsMatchCore).
	MaxFileBytes int64 = 100 << 20
	// MaxClipMs must match MAX_CLIP_DURATION_MS in packages/core/src/moment.ts.
	MaxClipMs = 30_000
	// MaxFiles per request.
	MaxFiles = 20
	// maxPending uploads per vault, so nobody can fill the table with slots they never use.
	maxPending = 200
	// putTTL is how long a signed PUT works.
	putTTL = 15 * time.Minute
	// completeWithin: how long the client has to call Complete; past it the reservation is released.
	// Longer than putTTL, so a file sent at the last moment can still be completed.
	completeWithin = time.Hour
	// keepFinished: expired and failed uploads stay readable this long, then cleanup deletes them.
	keepFinished  = 24 * time.Hour
	maxNameLength = 255
)

// contentTypes Ambar keeps: photos, clips and PDFs. Never SVG or HTML, which could run as a page.
var contentTypes = []string{
	"image/jpeg", "image/png", "image/webp", "image/heic", "image/heif",
	"video/mp4", "video/quicktime", "video/webm",
	"application/pdf",
}

var (
	// ErrInvalid: the request is malformed (a client bug, not a file Ambar refuses).
	ErrInvalid = errors.New("invalid upload request")
	// ErrReadOnly: the caller can see the vault but not add to it.
	ErrReadOnly = errors.New("vault is read-only")
	// ErrTooManyPending: the vault has maxPending uploads in flight.
	ErrTooManyPending = errors.New("too many uploads in progress")
	ErrNotFound       = errors.New("upload not found")
	// ErrNotPending: the upload is past pending and can't be cancelled.
	ErrNotPending = errors.New("upload is not pending")
	// ErrMissing: Complete was called before the file reached storage.
	ErrMissing = errors.New("file not in storage")
	// ErrExpired: the upload wasn't completed in time; its reservation is released.
	ErrExpired = errors.New("upload expired")
)

// Status of an upload, as in the API contract.
const (
	Pending    = "pending"
	Uploaded   = "uploaded"
	Processing = "processing"
	Ready      = "ready"
	Failed     = "failed"
	Expired    = "expired"
)

// Per-file refusals inside a Create answer (not errors: the other files go on).
const (
	UnsupportedType = "unsupported-type"
	FileTooLarge    = "file-too-large"
	QuotaExceeded   = "quota-exceeded"
	// Mismatch: what arrived isn't what was declared (size or type).
	Mismatch = "upload-mismatch"
)

// Storage is what uploads need from the bucket (storage.Client, or a fake in tests).
type Storage interface {
	PresignPut(ctx context.Context, key, contentType string, size int64, sha256Hex string, ttl time.Duration) (string, map[string]string, error)
	Head(ctx context.Context, key string) (storage.Object, error)
	Delete(ctx context.Context, key string) error
}

// File is one file a client wants to keep, as declared. Size, ContentType and SHA256 describe the bytes it
// will send; the rest is what it read before stripping metadata, kept as declared values.
type File struct {
	Name        string
	Size        int64
	ContentType string
	SHA256      string
	CapturedAt  *time.Time
	Width       *int
	Height      *int
	DurationMs  *int
}

type Upload struct {
	ID          string
	Status      string
	Name        string
	Size        int64
	ContentType string
	// SHA256 (hex) of the bytes the client declared; signed into the PUT, so R2 takes no others.
	SHA256    string
	CreatedAt time.Time
	ExpiresAt time.Time
	// FailureCode is set when Status is Failed.
	FailureCode string
}

// Target is where the client sends the bytes.
type Target struct {
	URL       string
	Headers   map[string]string
	ExpiresAt time.Time
}

// Result is the outcome for one file: Accepted with Upload and Put, or Refused with a per-file code.
type Result struct {
	Upload *Upload
	Put    *Target
	// Refused, with MaxBytes (FileTooLarge) or BytesLeft (QuotaExceeded).
	Refused   string
	MaxBytes  int64
	BytesLeft int64
}

type Quota struct {
	Used, Reserved, Limit int64
}

type Service struct {
	store   store
	storage Storage
}

func NewService(db *pgxpool.Pool, s Storage) *Service {
	return &Service{store: store{db: db}, storage: s}
}

// Key is where an upload's bytes wait in the bucket until processing moves them.
func Key(uploadID string) string { return "incoming/" + uploadID }

// Create reserves and signs uploads for files in vaultID (empty: the caller's personal vault). One result
// per file, in order. Reservations are counted under a lock on the vault, so parallel requests can't
// add up to more than its limit.
func (s *Service) Create(ctx context.Context, userID, vaultID string, files []File) ([]Result, Quota, error) {
	if len(files) == 0 || len(files) > MaxFiles {
		return nil, Quota{}, ErrInvalid
	}

	for _, f := range files {
		if !wellFormed(f) {
			return nil, Quota{}, ErrInvalid
		}
	}

	return s.store.reserve(ctx, userID, vaultID, completeWithin, func(quota *Quota, pending int, insert insertFunc) ([]Result, error) {
		if pending+len(files) > maxPending {
			return nil, ErrTooManyPending
		}

		results := make([]Result, len(files))

		for i, f := range files {
			switch {
			case !slices.Contains(contentTypes, f.ContentType):
				results[i] = Result{Refused: UnsupportedType}
			case f.Size > MaxFileBytes:
				results[i] = Result{Refused: FileTooLarge, MaxBytes: MaxFileBytes}
			case quota.Used+quota.Reserved+f.Size > quota.Limit:
				results[i] = Result{Refused: QuotaExceeded, BytesLeft: max(quota.Limit-quota.Used-quota.Reserved, 0)}
			default:
				upload, err := insert(f)
				if err != nil {
					return nil, err
				}

				// Presigning is local (no request leaves), so it can happen inside the transaction: a slot
				// is only reserved if it was also signed.
				url, headers, err := s.storage.PresignPut(ctx, Key(upload.ID), f.ContentType, f.Size, f.SHA256, putTTL)
				if err != nil {
					return nil, err
				}

				quota.Reserved += f.Size
				results[i] = Result{Upload: &upload, Put: &Target{URL: url, Headers: headers, ExpiresAt: upload.CreatedAt.Add(putTTL)}}
			}
		}

		return results, nil
	})
}

// wellFormed checks what a correct client always sends. Files Ambar refuses (type, size) are answered per
// file instead, so one of them never blocks the others.
func wellFormed(f File) bool {
	if !nameOK(f.Name) || f.Size < 1 {
		return false
	}

	if sum, err := hex.DecodeString(f.SHA256); err != nil || len(sum) != 32 || f.SHA256 != hex.EncodeToString(sum) {
		return false
	}

	// Postgres integers: anything larger can't be a real picture and would fail to store.
	for _, n := range []*int{f.Width, f.Height, f.DurationMs} {
		if n != nil && (*n < 1 || *n > math.MaxInt32) {
			return false
		}
	}

	return f.DurationMs == nil || *f.DurationMs <= MaxClipMs
}

// nameOK: a file name shown to people later. No control characters (Postgres can't store NUL, terminals
// and logs read escapes) and no bidirectional overrides, which make "gpj.exe" look like "exe.jpg".
func nameOK(name string) bool {
	if strings.TrimSpace(name) == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > maxNameLength {
		return false
	}

	for _, r := range name {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
			return false
		}
	}

	return true
}

// Complete checks that the file reached storage as declared and marks it uploaded, ready for processing.
// Safe to repeat: an upload past pending comes back as it is.
func (s *Service) Complete(ctx context.Context, userID, uploadID string) (Upload, error) {
	upload, expired, err := s.store.get(ctx, userID, uploadID)

	switch {
	case err != nil:
		return Upload{}, err
	case upload.Status == Expired:
		return Upload{}, ErrExpired
	case upload.Status != Pending:
		return upload, nil
	}

	if expired {
		if _, err := s.store.finish(ctx, uploadID, Expired, ""); err != nil {
			return Upload{}, err
		}

		return Upload{}, ErrExpired
	}

	object, err := s.storage.Head(ctx, Key(uploadID))
	if errors.Is(err, storage.ErrNotFound) {
		return Upload{}, ErrMissing
	}

	if err != nil {
		return Upload{}, err
	}

	status, failure := Uploaded, ""

	// R2 already refused other bytes; this also catches headers a client added to the PUT, which R2
	// stores (Content-Encoding: gzip would change how the file is read).
	if object.Size != upload.Size || object.ContentType != upload.ContentType || object.SHA256 != upload.SHA256 || tampered(object.Unsigned) {
		// Best effort: the bucket's lifecycle rule removes incoming/ a day later anyway.
		_ = s.storage.Delete(ctx, Key(uploadID))

		status, failure = Failed, Mismatch
	}

	finished, err := s.store.finish(ctx, uploadID, status, failure)

	// Something else got there between the HEAD and the write: a cancel (the upload is gone) or the
	// cleanup (it expired). Answer as for those, never with a stale "pending" or a success.
	switch {
	case err != nil:
		return Upload{}, err
	case finished.Status == Expired:
		return Upload{}, ErrExpired
	}

	return finished, nil
}

// tampered: the object carries a header the client added and no browser adds by itself. Browsers send
// Cache-Control on their own (no-cache, with the HTTP cache off) and it only affects reading the object
// straight from R2, which every read overrides; anything else is refused.
func tampered(unsigned []string) bool {
	for _, name := range unsigned {
		if name != "Cache-Control" {
			return true
		}
	}

	return false
}

// Get is one of the caller's uploads. Uploads are only visible to whoever sent them.
func (s *Service) Get(ctx context.Context, userID, uploadID string) (Upload, error) {
	upload, expired, err := s.store.get(ctx, userID, uploadID)

	// Its time ran out even if the cleanup hasn't marked it yet: it no longer holds its bytes either.
	if expired && upload.Status == Pending {
		upload.Status = Expired
	}

	return upload, err
}

// Cancel gives up a pending upload and releases its bytes. Cancelling an expired one succeeds.
func (s *Service) Cancel(ctx context.Context, userID, uploadID string) error {
	cancelled, err := s.store.cancel(ctx, userID, uploadID)
	if err != nil || !cancelled {
		return err
	}

	_ = s.storage.Delete(ctx, Key(uploadID))

	return nil
}

// Cleanup expires pending uploads whose time ran out (releasing their reservation) and deletes finished
// ones older than keepFinished. Their files are deleted best effort; the bucket's lifecycle rule removes
// whatever is left in incoming/ after a day.
func (s *Service) Cleanup(ctx context.Context) (expired, deleted int64, err error) {
	ids, err := s.store.expire(ctx)
	if err != nil {
		return 0, 0, err
	}

	for _, id := range ids {
		_ = s.storage.Delete(ctx, Key(id))
	}

	deleted, err = s.store.deleteFinished(ctx, keepFinished)

	return int64(len(ids)), deleted, err
}
