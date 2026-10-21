package uploads

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"ambar/api/storage"
)

// Edge cases and attacks on the upload rules. Each test states what the rules promise, not what the code
// happens to do.

func limitOf(n int) *int { return &n }

func TestCreateQuotaBoundaries(t *testing.T) {
	ctx := context.Background()
	s, _, db := newTestService(t)

	t.Run("exact fit is accepted, one byte more is not", func(t *testing.T) {
		user := newUser(t, db)
		setLimit(t, db, user, 1000)

		results, quota, err := s.Create(ctx, user, "", []File{file("a.jpg", 400, "image/jpeg"), file("b.jpg", 600, "image/jpeg"), file("c.jpg", 1, "image/jpeg")})
		if err != nil {
			t.Fatal(err)
		}

		if results[0].Upload == nil || results[1].Upload == nil {
			t.Fatalf("the files that fit exactly were refused: %+v", results)
		}

		if results[2].Refused != QuotaExceeded || results[2].BytesLeft != 0 {
			t.Fatalf("one byte over: %+v", results[2])
		}

		if quota.Reserved != 1000 {
			t.Fatalf("reserved %d", quota.Reserved)
		}
	})

	t.Run("a limit of zero refuses everything", func(t *testing.T) {
		user := newUser(t, db)
		setLimit(t, db, user, 0)

		results, quota, err := s.Create(ctx, user, "", []File{file("a.jpg", 1, "image/jpeg")})
		if err != nil || results[0].Refused != QuotaExceeded || results[0].BytesLeft != 0 || quota.Reserved != 0 {
			t.Fatalf("%+v %+v %v", results, quota, err)
		}
	})

	t.Run("refused files reserve nothing", func(t *testing.T) {
		user := newUser(t, db)
		setLimit(t, db, user, 100)

		_, quota, err := s.Create(ctx, user, "", []File{
			file("a.svg", 10, "image/svg+xml"),
			file("b.jpg", MaxFileBytes+1, "image/jpeg"),
			file("c.jpg", 101, "image/jpeg"),
		})
		if err != nil || quota.Reserved != 0 {
			t.Fatalf("%+v %v", quota, err)
		}

		results, _, err := s.Create(ctx, user, "", []File{file("d.jpg", 100, "image/jpeg")})
		if err != nil || results[0].Upload == nil {
			t.Fatalf("the whole limit should still be free: %+v %v", results, err)
		}
	})

	t.Run("a file of exactly MaxFileBytes is accepted", func(t *testing.T) {
		user := newUser(t, db)

		results, _, err := s.Create(ctx, user, "", []File{file("a.mp4", MaxFileBytes, "video/mp4")})
		if err != nil || results[0].Upload == nil {
			t.Fatalf("%+v %v", results, err)
		}
	})
}

// Bytes come back when an upload is cancelled, fails or expires; an uploaded file keeps holding them.
func TestReleasedBytesCanBeReservedAgain(t *testing.T) {
	ctx := context.Background()
	s, bucket, db := newTestService(t)

	reserveAll := func(t *testing.T, user string) string {
		t.Helper()

		results, _, err := s.Create(ctx, user, "", []File{file("a.jpg", 100, "image/jpeg")})
		if err != nil || results[0].Upload == nil {
			t.Fatalf("reserve: %+v %v", results, err)
		}

		return results[0].Upload.ID
	}

	fitsAgain := func(t *testing.T, user string) bool {
		t.Helper()

		results, _, err := s.Create(ctx, user, "", []File{file("b.jpg", 100, "image/jpeg")})
		if err != nil {
			t.Fatal(err)
		}

		return results[0].Upload != nil
	}

	t.Run("cancelled", func(t *testing.T) {
		user := newUser(t, db)
		setLimit(t, db, user, 100)
		id := reserveAll(t, user)

		if err := s.Cancel(ctx, user, id); err != nil {
			t.Fatal(err)
		}

		if !fitsAgain(t, user) {
			t.Fatal("cancelled bytes still reserved")
		}
	})

	t.Run("failed", func(t *testing.T) {
		user := newUser(t, db)
		setLimit(t, db, user, 100)
		id := reserveAll(t, user)
		bucket.put(Key(id), 100, "image/png")

		if upload, err := s.Complete(ctx, user, id); err != nil || upload.Status != Failed {
			t.Fatalf("%+v %v", upload, err)
		}

		if !fitsAgain(t, user) {
			t.Fatal("failed bytes still reserved")
		}
	})

	t.Run("expired", func(t *testing.T) {
		user := newUser(t, db)
		setLimit(t, db, user, 100)
		id := reserveAll(t, user)

		if _, err := db.Exec(ctx, `UPDATE uploads SET expires_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}

		// Not even waiting for the cleanup: a slot that ran out holds nothing.
		if !fitsAgain(t, user) {
			t.Fatal("expired bytes still reserved before the cleanup ran")
		}
	})

	t.Run("uploaded keeps them", func(t *testing.T) {
		user := newUser(t, db)
		setLimit(t, db, user, 100)
		id := reserveAll(t, user)
		bucket.put(Key(id), 100, "image/jpeg")

		if _, err := s.Complete(ctx, user, id); err != nil {
			t.Fatal(err)
		}

		if fitsAgain(t, user) {
			t.Fatal("an uploaded file stopped counting against the quota")
		}
	})
}

func TestCreateEdgeInput(t *testing.T) {
	ctx := context.Background()
	s, _, db := newTestService(t)
	user := newUser(t, db)

	with := func(change func(*File)) File {
		f := file("a.jpg", 1, "image/jpeg")
		change(&f)

		return f
	}

	year := func(y int) *time.Time {
		v := time.Date(y, 6, 1, 12, 0, 0, 0, time.UTC)

		return &v
	}

	valid := map[string]File{
		"255 multibyte runes":     with(func(f *File) { f.Name = strings.Repeat("é", 255) }),
		"255 emoji":               with(func(f *File) { f.Name = strings.Repeat("😀", 255) }),
		"path-like name":          with(func(f *File) { f.Name = "../../etc/passwd%2F..\\x.jpg" }),
		"width at int32 max":      with(func(f *File) { f.Width = limitOf(math.MaxInt32) }),
		"clip at the limit":       with(func(f *File) { f.ContentType = "video/mp4"; f.DurationMs = limitOf(MaxClipMs) }),
		"captured in year 1":      with(func(f *File) { f.CapturedAt = year(1) }),
		"captured in year 9999":   with(func(f *File) { f.CapturedAt = year(9999) }),
		"captured in a far zone":  with(func(f *File) { v := time.Date(2026, 1, 1, 0, 0, 0, 0, time.FixedZone("", 14*3600)); f.CapturedAt = &v }),
		"a single-character name": with(func(f *File) { f.Name = "x" }),
	}

	for name, f := range valid {
		results, _, err := s.Create(ctx, user, "", []File{f})
		if err != nil || results[0].Upload == nil {
			t.Errorf("%s should be accepted: %+v %v", name, results, err)

			continue
		}

		if got, _ := s.Get(ctx, user, results[0].Upload.ID); got.Name != f.Name {
			t.Errorf("%s: name stored as %q", name, got.Name)
		}
	}

	invalid := map[string]File{
		"256 runes":             with(func(f *File) { f.Name = strings.Repeat("é", 256) }),
		"NUL in the name":       with(func(f *File) { f.Name = "a\x00.jpg" }),
		"newline in the name":   with(func(f *File) { f.Name = "a\n.jpg" }),
		"escape in the name":    with(func(f *File) { f.Name = "\x1b[31ma.jpg" }),
		"right-to-left spoof":   with(func(f *File) { f.Name = "photo\u202egpj.exe" }),
		"only spaces":           with(func(f *File) { f.Name = "   " }),
		"uppercase sha256":      with(func(f *File) { f.SHA256 = strings.ToUpper(f.SHA256) }),
		"sha256 of 63":          with(func(f *File) { f.SHA256 = f.SHA256[:63] }),
		"sha256 of 65":          with(func(f *File) { f.SHA256 += "0" }),
		"non-hex sha256":        with(func(f *File) { f.SHA256 = strings.Repeat("zz", 32) }),
		"zero width":            with(func(f *File) { f.Width = limitOf(0) }),
		"negative height":       with(func(f *File) { f.Height = limitOf(-1) }),
		"width over int32":      with(func(f *File) { f.Width = limitOf(math.MaxInt32 + 1) }),
		"height of 2^32+1":      with(func(f *File) { f.Height = limitOf(1<<32 + 1) }),
		"clip one ms too long":  with(func(f *File) { f.ContentType = "video/mp4"; f.DurationMs = limitOf(MaxClipMs + 1) }),
		"negative size":         with(func(f *File) { f.Size = -1 }),
		"zero duration":         with(func(f *File) { f.DurationMs = limitOf(0) }),
		"invalid utf-8 in name": with(func(f *File) { f.Name = "a\xff.jpg" }),
	}

	for name, f := range invalid {
		if _, _, err := s.Create(ctx, user, "", []File{f}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
}

// A huge declared size must never overflow the quota sum into "fits".
func TestCreateHugeSizeIsRefusedNotWrapped(t *testing.T) {
	ctx := context.Background()
	s, _, db := newTestService(t)
	user := newUser(t, db)
	setLimit(t, db, user, math.MaxInt64)

	results, _, err := s.Create(ctx, user, "", []File{file("a.jpg", math.MaxInt64, "image/jpeg")})
	if errors.Is(err, ErrInvalid) {
		return
	}

	if err != nil || results[0].Upload != nil {
		t.Fatalf("%+v %v", results, err)
	}
}

func TestCreateSameFileTwiceInABatch(t *testing.T) {
	ctx := context.Background()
	s, _, db := newTestService(t)
	user := newUser(t, db)

	results, quota, err := s.Create(ctx, user, "", []File{file("a.jpg", 10, "image/jpeg"), file("a.jpg", 10, "image/jpeg")})
	if err != nil || results[0].Upload == nil || results[1].Upload == nil || results[0].Upload.ID == results[1].Upload.ID || quota.Reserved != 20 {
		t.Fatalf("%+v %+v %v", results, quota, err)
	}
}

func TestPendingCapBoundaries(t *testing.T) {
	ctx := context.Background()
	s, bucket, db := newTestService(t)
	user := newUser(t, db)

	batch := func(n int) []File {
		files := make([]File, n)
		for i := range files {
			files[i] = file("a.jpg", 1, "image/jpeg")
		}

		return files
	}

	var ids []string

	for range 9 {
		results, _, err := s.Create(ctx, user, "", batch(MaxFiles))
		if err != nil {
			t.Fatal(err)
		}

		for _, r := range results {
			ids = append(ids, r.Upload.ID)
		}
	}

	if _, _, err := s.Create(ctx, user, "", batch(19)); err != nil {
		t.Fatalf("199 pending: %v", err)
	}

	if _, _, err := s.Create(ctx, user, "", batch(2)); !errors.Is(err, ErrTooManyPending) {
		t.Fatalf("199 + 2: %v", err)
	}

	if _, _, err := s.Create(ctx, user, "", batch(1)); err != nil {
		t.Fatalf("199 + 1: %v", err)
	}

	// Completed uploads are no longer pending, so they make room.
	for _, id := range ids[:5] {
		bucket.put(Key(id), 1, "image/jpeg")

		if _, err := s.Complete(ctx, user, id); err != nil {
			t.Fatal(err)
		}
	}

	if _, _, err := s.Create(ctx, user, "", batch(5)); err != nil {
		t.Fatalf("after completing five: %v", err)
	}
}

func TestCompleteEdges(t *testing.T) {
	ctx := context.Background()
	s, bucket, db := newTestService(t)
	user := newUser(t, db)

	reserve := func(t *testing.T) string {
		t.Helper()

		results, _, err := s.Create(ctx, user, "", []File{file("a.jpg", 100, "image/jpeg")})
		if err != nil {
			t.Fatal(err)
		}

		return results[0].Upload.ID
	}

	t.Run("right size, wrong type fails", func(t *testing.T) {
		id := reserve(t)
		bucket.put(Key(id), 100, "text/html")

		upload, err := s.Complete(ctx, user, id)
		if err != nil || upload.Status != Failed || upload.FailureCode != Mismatch {
			t.Fatalf("%+v %v", upload, err)
		}

		again, err := s.Complete(ctx, user, id)
		if err != nil || again.Status != Failed || again.FailureCode != Mismatch {
			t.Fatalf("again: %+v %v", again, err)
		}
	})

	t.Run("a bigger object fails", func(t *testing.T) {
		id := reserve(t)
		bucket.put(Key(id), 101, "image/jpeg")

		if upload, err := s.Complete(ctx, user, id); err != nil || upload.Status != Failed {
			t.Fatalf("%+v %v", upload, err)
		}
	})

	t.Run("after cancel it is gone", func(t *testing.T) {
		id := reserve(t)

		if err := s.Cancel(ctx, user, id); err != nil {
			t.Fatal(err)
		}

		bucket.put(Key(id), 100, "image/jpeg")

		if _, err := s.Complete(ctx, user, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%v", err)
		}
	})

	t.Run("storage down keeps it pending", func(t *testing.T) {
		id := reserve(t)
		bucket.put(Key(id), 100, "image/jpeg")
		bucket.mu.Lock()
		bucket.headErr = storage.ErrUnavailable
		bucket.mu.Unlock()

		_, err := s.Complete(ctx, user, id)

		bucket.mu.Lock()
		bucket.headErr = nil
		bucket.mu.Unlock()

		if !errors.Is(err, storage.ErrUnavailable) {
			t.Fatalf("want ErrUnavailable, got %v", err)
		}

		if upload, _ := s.Get(ctx, user, id); upload.Status != Pending {
			t.Fatalf("status %s", upload.Status)
		}

		if upload, err := s.Complete(ctx, user, id); err != nil || upload.Status != Uploaded {
			t.Fatalf("retry: %+v %v", upload, err)
		}
	})

	t.Run("cancelled between the check and the write", func(t *testing.T) {
		id := reserve(t)
		bucket.put(Key(id), 100, "image/jpeg")
		bucket.mu.Lock()
		bucket.afterHead = func() { _ = s.Cancel(ctx, user, id) }
		bucket.mu.Unlock()

		_, err := s.Complete(ctx, user, id)

		bucket.mu.Lock()
		bucket.afterHead = nil
		bucket.mu.Unlock()

		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("expired between the check and the write", func(t *testing.T) {
		id := reserve(t)
		bucket.put(Key(id), 100, "image/jpeg")
		bucket.mu.Lock()
		bucket.afterHead = func() {
			_, _ = db.Exec(ctx, `UPDATE uploads SET expires_at = now() - interval '1 second' WHERE id = $1`, id)
			_, _, _ = s.Cleanup(ctx)
		}
		bucket.mu.Unlock()

		upload, err := s.Complete(ctx, user, id)

		bucket.mu.Lock()
		bucket.afterHead = nil
		bucket.mu.Unlock()

		if !errors.Is(err, ErrExpired) {
			t.Fatalf("want ErrExpired, got %+v %v", upload, err)
		}
	})
}

// Many completes at once: every caller gets the same answer.
func TestCompleteInParallel(t *testing.T) {
	ctx := context.Background()
	s, bucket, db := newTestService(t)
	user := newUser(t, db)

	results, _, err := s.Create(ctx, user, "", []File{file("a.jpg", 100, "image/jpeg")})
	if err != nil {
		t.Fatal(err)
	}

	id := results[0].Upload.ID
	bucket.put(Key(id), 100, "image/jpeg")

	var wg sync.WaitGroup

	for range 20 {
		wg.Go(func() {
			if upload, err := s.Complete(ctx, user, id); err != nil || upload.Status != Uploaded {
				t.Errorf("%+v %v", upload, err)
			}
		})
	}

	wg.Wait()
}

// Cancel and complete racing: one wins, and the loser's answer agrees with what is left.
func TestCancelRacingComplete(t *testing.T) {
	ctx := context.Background()
	s, bucket, db := newTestService(t)
	user := newUser(t, db)

	for range 40 {
		results, _, err := s.Create(ctx, user, "", []File{file("a.jpg", 100, "image/jpeg")})
		if err != nil {
			t.Fatal(err)
		}

		id := results[0].Upload.ID
		bucket.put(Key(id), 100, "image/jpeg")

		var (
			wg                     sync.WaitGroup
			completeErr, cancelErr error
			completed              Upload
		)

		wg.Go(func() { completed, completeErr = s.Complete(ctx, user, id) })
		wg.Go(func() { cancelErr = s.Cancel(ctx, user, id) })
		wg.Wait()

		stored, getErr := s.Get(ctx, user, id)

		switch {
		case cancelErr == nil:
			// Cancel won: the upload is gone, and complete didn't claim otherwise.
			if !errors.Is(getErr, ErrNotFound) {
				t.Fatalf("cancelled but still there: %+v %v", stored, getErr)
			}

			if completeErr == nil {
				t.Fatalf("complete answered %s for an upload that was cancelled", completed.Status)
			}

			if !errors.Is(completeErr, ErrNotFound) && !errors.Is(completeErr, ErrMissing) {
				t.Fatalf("complete: %v", completeErr)
			}
		case errors.Is(cancelErr, ErrNotPending):
			// Complete won: uploaded, and its file still in the bucket.
			if completeErr != nil || stored.Status != Uploaded {
				t.Fatalf("complete %+v %v, stored %+v %v", completed, completeErr, stored, getErr)
			}

			if _, err := bucket.Head(ctx, Key(id)); err != nil {
				t.Fatal("an uploaded file lost its object")
			}
		default:
			t.Fatalf("cancel: %v", cancelErr)
		}
	}
}

// Reservations in two vaults never wait on, or count against, each other.
func TestQuotasAreSeparatePerVault(t *testing.T) {
	ctx := context.Background()
	s, _, db := newTestService(t)
	a, b := newUser(t, db), newUser(t, db)
	setLimit(t, db, a, 5000)
	setLimit(t, db, b, 5000)

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		took = map[string]int64{}
	)

	for i := range 20 {
		user := a
		if i%2 == 1 {
			user = b
		}

		wg.Go(func() {
			results, _, err := s.Create(ctx, user, "", []File{file("a.jpg", 1000, "image/jpeg")})
			if err != nil {
				t.Error(err)

				return
			}

			if results[0].Upload != nil {
				mu.Lock()
				took[user] += 1000
				mu.Unlock()
			}
		})
	}

	wg.Wait()

	if took[a] != 5000 || took[b] != 5000 {
		t.Fatalf("a %d, b %d", took[a], took[b])
	}
}

func TestCleanupEdges(t *testing.T) {
	ctx := context.Background()
	s, bucket, db := newTestService(t)
	user := newUser(t, db)

	results, _, err := s.Create(ctx, user, "", []File{file("on-time.jpg", 1, "image/jpeg"), file("done.jpg", 1, "image/jpeg"), file("late.jpg", 1, "image/jpeg")})
	if err != nil {
		t.Fatal(err)
	}

	onTime, done, late := results[0].Upload.ID, results[1].Upload.ID, results[2].Upload.ID
	bucket.put(Key(done), 1, "image/jpeg")
	bucket.put(Key(late), 1, "image/jpeg")

	if _, err := s.Complete(ctx, user, done); err != nil {
		t.Fatal(err)
	}

	if _, err := db.Exec(ctx, `UPDATE uploads SET expires_at = now() - interval '1 second' WHERE id = ANY($1)`, []string{done, late}); err != nil {
		t.Fatal(err)
	}

	// The bucket refuses deletes: cleanup still releases the reservation and doesn't fail.
	bucket.mu.Lock()
	bucket.deleteErr = storage.ErrUnavailable
	bucket.mu.Unlock()

	if _, _, err := s.Cleanup(ctx); err != nil {
		t.Fatalf("cleanup with storage down: %v", err)
	}

	bucket.mu.Lock()
	bucket.deleteErr = nil
	bucket.mu.Unlock()

	if _, _, err := s.Cleanup(ctx); err != nil {
		t.Fatalf("second cleanup: %v", err)
	}

	for id, want := range map[string]string{onTime: Pending, done: Uploaded, late: Expired} {
		if upload, err := s.Get(ctx, user, id); err != nil || upload.Status != want {
			t.Errorf("%s: %+v %v, want %s", id, upload, err, want)
		}
	}

	if _, err := bucket.Head(ctx, Key(done)); err != nil {
		t.Error("cleanup deleted an uploaded file")
	}
}

// What arrived must be exactly what was signed: another checksum, or a header R2 stored that we never
// signed, fails the upload.
func TestCompleteRefusesWhatWasNotSigned(t *testing.T) {
	ctx := context.Background()
	s, bucket, db := newTestService(t)
	user := newUser(t, db)

	for name, object := range map[string]func(sum string) storage.Object{
		"other checksum": func(string) storage.Object {
			return storage.Object{Size: 100, ContentType: "image/jpeg", SHA256: strings.Repeat("0", 64)}
		},
		"no checksum": func(string) storage.Object { return storage.Object{Size: 100, ContentType: "image/jpeg"} },
		"content-encoding": func(sum string) storage.Object {
			return storage.Object{Size: 100, ContentType: "image/jpeg", SHA256: sum, Unsigned: []string{"Content-Encoding"}}
		},
		"content-disposition next to cache-control": func(sum string) storage.Object {
			return storage.Object{Size: 100, ContentType: "image/jpeg", SHA256: sum, Unsigned: []string{"Cache-Control", "Content-Disposition"}}
		},
		"expires": func(sum string) storage.Object {
			return storage.Object{Size: 100, ContentType: "image/jpeg", SHA256: sum, Unsigned: []string{"Expires"}}
		},
	} {
		results, _, err := s.Create(ctx, user, "", []File{file(name+".jpg", 100, "image/jpeg")})
		if err != nil {
			t.Fatal(err)
		}

		upload := results[0].Upload
		bucket.putObject(Key(upload.ID), object(upload.SHA256))

		done, err := s.Complete(ctx, user, upload.ID)
		if err != nil || done.Status != Failed || done.FailureCode != Mismatch {
			t.Errorf("%s: %+v %v", name, done, err)
		}
	}
}

// Past its time an upload reads as expired, and can't be cancelled back into the quota twice.
func TestAnExpiredUploadReadsAsExpired(t *testing.T) {
	ctx := context.Background()
	s, _, db := newTestService(t)
	user := newUser(t, db)

	results, _, err := s.Create(ctx, user, "", []File{file("a.jpg", 100, "image/jpeg")})
	if err != nil {
		t.Fatal(err)
	}

	id := results[0].Upload.ID

	if _, err := db.Exec(ctx, `UPDATE uploads SET expires_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}

	if upload, err := s.Get(ctx, user, id); err != nil || upload.Status != Expired {
		t.Fatalf("get: %+v %v", upload, err)
	}

	// Cancelling an expired upload succeeds and changes nothing.
	if err := s.Cancel(ctx, user, id); err != nil {
		t.Fatalf("cancel: %v", err)
	}
}

// Chromium sends Cache-Control: no-cache on its own when its HTTP cache is off, and R2 stores it: that is
// a real browser upload, and it must not fail.
func TestCompleteTakesTheBrowsersCacheControl(t *testing.T) {
	ctx := context.Background()
	s, bucket, db := newTestService(t)
	user := newUser(t, db)

	results, _, err := s.Create(ctx, user, "", []File{file("a.jpg", 100, "image/jpeg")})
	if err != nil {
		t.Fatal(err)
	}

	upload := results[0].Upload
	bucket.putObject(Key(upload.ID), storage.Object{Size: 100, ContentType: "image/jpeg", SHA256: upload.SHA256, Unsigned: []string{"Cache-Control"}})

	if done, err := s.Complete(ctx, user, upload.ID); err != nil || done.Status != Uploaded {
		t.Fatalf("%+v %v", done, err)
	}
}
