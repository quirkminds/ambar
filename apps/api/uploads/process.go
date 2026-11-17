package uploads

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"ambar/api/moments"
	"ambar/api/photos"
	"ambar/api/storage"
)

const (
	// leaseFor is how long a worker holds an upload. Past it, another worker takes it over: the first one
	// died, or got stuck. Longer than a 100 MB photo takes to move and process.
	leaseFor = 5 * time.Minute
	// maxAttempts: leases an upload gets before it fails with internal. A file that keeps crashing or
	// stalling workers ends there instead of looping.
	maxAttempts = 3
	// idlePoll: how often an idle worker looks for work it wasn't told about (another instance's uploads,
	// leases that ran out).
	idlePoll = 5 * time.Second
)

// errFinal marks a processing outcome that retrying can't change; Code is the upload's failure code.
type errFinal struct{ Code string }

func (e errFinal) Error() string { return "uploads: " + e.Code }

// nudge wakes one idle worker, without ever blocking.
func (s *Service) nudge() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Work runs n workers until ctx ends, then waits for them. Interrupted processing isn't lost: its lease
// runs out and a worker takes it again.
func (s *Service) Work(ctx context.Context, n int) {
	var wg sync.WaitGroup

	for range n {
		wg.Go(func() {
			for ctx.Err() == nil {
				if worked, _ := s.ProcessNext(ctx); worked {
					continue
				}

				select {
				case <-ctx.Done():
				case <-s.wake:
				case <-time.After(idlePoll):
				}
			}
		})
	}

	wg.Wait()
}

// ProcessNext claims one upload and processes it. worked is false when there was nothing to do.
func (s *Service) ProcessNext(ctx context.Context) (worked bool, err error) {
	upload, ok, err := s.store.claim(ctx, leaseFor)
	if err != nil {
		slog.ErrorContext(ctx, "upload claim failed", "event", "upload.error", "err", err)

		return false, err
	}

	if !ok {
		return false, nil
	}

	log := slog.With("upload_id", upload.ID, "attempt", upload.Attempts)

	if upload.Attempts > maxAttempts {
		log.ErrorContext(ctx, "upload gave up", "event", "upload.failed", "code", Internal)

		return true, s.discard(ctx, upload, Internal, nil)
	}

	log.InfoContext(ctx, "upload processing", "event", "upload.processing")

	momentID, duplicate, err := s.process(ctx, upload)

	var final errFinal

	switch {
	case err == nil && duplicate:
		log.InfoContext(ctx, "upload was a duplicate", "event", "upload.duplicate", "moment_id", momentID)
	case err == nil:
		log.InfoContext(ctx, "upload ready", "event", "upload.ready", "moment_id", momentID)
	case errors.As(err, &final):
		log.WarnContext(ctx, "upload failed", "event", "upload.failed", "code", final.Code)
	case errors.Is(err, errLeaseLost):
		log.WarnContext(ctx, "upload taken over by another worker", "event", "upload.retry")
	case ctx.Err() != nil:
		// Shutting down: the lease runs out and the upload is taken again.
	case upload.Attempts >= maxAttempts:
		log.ErrorContext(ctx, "upload gave up", "event", "upload.failed", "code", Internal, "err", err)
		err = s.discard(ctx, upload, Internal, nil)
	default:
		// Transient (storage, a tool timing out): leave it processing; once the lease runs out it is
		// taken again.
		log.WarnContext(ctx, "upload will be retried", "event", "upload.retry", "err", err)
	}

	return true, err
}

// discard fails an upload and deletes what it left in the bucket: its incoming file and anything written
// for it. Deletes are best effort; the lifecycle rule removes incoming/ after a day anyway.
func (s *Service) discard(ctx context.Context, upload Upload, code string, written []string) error {
	err := s.store.fail(ctx, upload, code)

	for _, key := range append(written, Key(upload.ID)) {
		_ = s.storage.Delete(ctx, key)
	}

	return err
}

// process turns one claimed upload into a moment. A returned errFinal has already been recorded as the
// upload's failure; any other error leaves it to be retried.
func (s *Service) process(ctx context.Context, upload Upload) (momentID string, duplicate bool, err error) {
	dir, err := os.MkdirTemp("", "ambar-upload-")
	if err != nil {
		return "", false, err
	}
	defer os.RemoveAll(dir)

	final := func(code string, written []string) (string, bool, error) {
		if err := s.discard(ctx, upload, code, written); err != nil {
			return "", false, err
		}

		return "", false, errFinal{code}
	}

	in := filepath.Join(dir, "incoming")

	switch err := s.download(ctx, upload, in); {
	case errors.Is(err, errMismatch):
		return final(Mismatch, nil)
	case errors.Is(err, storage.ErrNotFound):
		// :complete saw it; without it there is nothing to keep.
		return final(Mismatch, nil)
	case err != nil:
		return "", false, err
	}

	width, height, err := photos.Bounds(in, upload.ContentType)
	if errors.Is(err, photos.ErrUnreadable) {
		return final(Unreadable, nil)
	} else if err != nil {
		return "", false, err
	}

	// Already kept (by another upload of the same file): no need to process or store anything. ready
	// checks again under the vault's lock.
	if existing, found, err := moments.Duplicate(ctx, s.store.db, upload.VaultID, upload.SHA256); err != nil {
		return "", false, err
	} else if found {
		return s.keep(ctx, upload, moments.Moment{ID: existing}, nil, nil)
	}

	clean := filepath.Join(dir, "original")
	if err := photos.Strip(in, clean, upload.ContentType); errors.Is(err, photos.ErrUnreadable) {
		return final(Unreadable, nil)
	} else if err != nil {
		return "", false, err
	}

	variants, err := photos.Thumbnails(ctx, clean, upload.ContentType, dir)
	if errors.Is(err, photos.ErrUnreadable) {
		return final(Unreadable, nil)
	} else if err != nil {
		return "", false, err
	}

	id, err := s.store.newMomentID(ctx)
	if err != nil {
		return "", false, err
	}

	info, err := os.Stat(clean)
	if err != nil {
		return "", false, err
	}

	// The copies are already turned the right way up; when that turned the photo, so do its dimensions.
	if large := variants[len(variants)-1]; (large.Width > large.Height) != (width > height) && width != height {
		width, height = height, width
	}

	files := []moments.File{{Variant: "original", Key: moments.Key(upload.VaultID, id, "original"), ContentType: upload.ContentType, Size: info.Size(), Width: width, Height: height}}
	paths := []string{clean}

	for _, v := range variants {
		files = append(files, moments.File{Variant: v.Name, Key: moments.Key(upload.VaultID, id, v.Name), ContentType: photos.WebP, Size: v.Size, Width: v.Width, Height: v.Height})
		paths = append(paths, v.Path)
	}

	var written []string

	for i, f := range files {
		if err := s.storage.Put(ctx, f.Key, f.ContentType, paths[i]); err != nil {
			s.deleteAll(ctx, written)

			return "", false, err
		}

		written = append(written, f.Key)
	}

	moment := moments.Moment{
		ID: id, VaultID: upload.VaultID, CreatedBy: upload.UserID, Kind: "photo", Name: upload.Name,
		ContentType: upload.ContentType, SHA256: upload.SHA256, CapturedAt: upload.CapturedAt, Width: width, Height: height,
	}

	return s.keep(ctx, upload, moment, files, written)
}

// keep records the moment (or the duplicate) and cleans up after it: the incoming file always goes, and
// what was written goes too unless it became the moment's.
func (s *Service) keep(ctx context.Context, upload Upload, moment moments.Moment, files []moments.File, written []string) (string, bool, error) {
	momentID, duplicate, err := s.store.ready(ctx, upload, moment, files)

	switch {
	case errors.Is(err, errRevoked):
		if err := s.discard(ctx, upload, Revoked, written); err != nil {
			return "", false, err
		}

		return "", false, errFinal{Revoked}
	case err != nil:
		s.deleteAll(ctx, written)

		return "", false, err
	}

	if duplicate {
		s.deleteAll(ctx, written)
	}

	_ = s.storage.Delete(ctx, Key(upload.ID))

	return momentID, duplicate, nil
}

func (s *Service) deleteAll(ctx context.Context, keys []string) {
	for _, key := range keys {
		_ = s.storage.Delete(ctx, key)
	}
}

// errMismatch: the stored bytes aren't the ones declared (size, fingerprint or type).
var errMismatch = errors.New("uploads: stored file doesn't match")

// download copies the upload's file to path, checking its size, fingerprint and real type on the way.
func (s *Service) download(ctx context.Context, upload Upload, path string) error {
	body, err := s.storage.Get(ctx, Key(upload.ID), upload.Size)
	if err != nil {
		return err
	}
	defer body.Close()

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	hash := sha256.New()

	n, err := io.Copy(io.MultiWriter(f, hash), body)
	if err != nil {
		return err
	}

	if n != upload.Size || hex.EncodeToString(hash.Sum(nil)) != upload.SHA256 {
		return errMismatch
	}

	head := make([]byte, 32)
	if _, err := f.ReadAt(head, 0); err != nil && !errors.Is(err, io.EOF) {
		return err
	}

	if !photos.SameType(upload.ContentType, photos.Sniff(head)) {
		return errMismatch
	}

	return f.Close()
}
