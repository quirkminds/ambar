package uploads

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"ambar/api/moments"
	"ambar/api/photos"
	"ambar/api/storage"
	"ambar/api/vaults"
)

// Processing runs for real here: Postgres, libvips and the photos package. Only the bucket is in memory.

// processing is a service whose workers only take uploads from one user's personal vault.
func processing(t *testing.T) (*Service, *fakeStorage, *pgxpool.Pool, string) {
	t.Helper()

	if err := photos.CheckTools(context.Background()); err != nil {
		t.Skipf("libvips: %v", err)
	}

	s, bucket, db := newTestService(t)
	user := newUser(t, db)

	vault, err := vaults.Personal(context.Background(), db, user)
	if err != nil {
		t.Fatal(err)
	}

	s.store.onlyVault = vault

	return s, bucket, db, user
}

var (
	photoOnce sync.Once
	photoJPEG []byte
	photoErr  error
)

// testPhoto is a real 640 × 480 JPEG with a comment that must not survive processing. Each call with a
// different tag is a different file (the tag goes after the end of the picture), so fingerprints differ.
func testPhoto(t *testing.T, tag string) []byte {
	t.Helper()

	photoOnce.Do(func() {
		dir, err := os.MkdirTemp("", "ambar-photo-")
		if err != nil {
			photoErr = err

			return
		}
		defer os.RemoveAll(dir)

		noise, out := filepath.Join(dir, "noise.v"), filepath.Join(dir, "photo.jpg")
		for _, args := range [][]string{{"gaussnoise", noise, "640", "480"}, {"copy", noise, out}} {
			if b, err := exec.Command("vips", args...).CombinedOutput(); err != nil {
				photoErr = err
				t.Logf("%s", b)

				return
			}
		}

		raw, err := os.ReadFile(out)
		if err != nil {
			photoErr = err

			return
		}

		note := []byte("SECRET-NOTE")
		comment := slices.Concat([]byte{0xFF, 0xFE, 0, byte(len(note) + 2)}, note)
		photoJPEG = slices.Concat(raw[:2], comment, raw[2:])
	})

	if photoErr != nil {
		t.Fatal(photoErr)
	}

	return slices.Concat(photoJPEG, []byte(tag))
}

func fileOf(name, contentType string, data []byte) File {
	sum := sha256.Sum256(data)

	return File{Name: name, Size: int64(len(data)), ContentType: contentType, SHA256: hex.EncodeToString(sum[:])}
}

// sendFile plays a client: create, PUT the bytes, complete. It returns the upload, now uploaded.
func sendFile(t *testing.T, s *Service, bucket *fakeStorage, user, vaultID string, f File, data []byte) Upload {
	t.Helper()

	ctx := context.Background()

	results, _, err := s.Create(ctx, user, vaultID, []File{f})
	if err != nil || results[0].Upload == nil {
		t.Fatalf("create: %+v %v", results, err)
	}

	id := results[0].Upload.ID
	bucket.upload(Key(id), f.ContentType, data)

	upload, err := s.Complete(ctx, user, id)
	if err != nil || upload.Status != Uploaded {
		t.Fatalf("complete: %+v %v", upload, err)
	}

	return upload
}

func processAll(t *testing.T, s *Service) {
	t.Helper()

	for range 20 {
		worked, err := s.ProcessNext(context.Background())
		if err != nil {
			t.Logf("process: %v", err)
		}

		if !worked {
			return
		}
	}

	t.Fatal("processing never ran out of work")
}

func quotaOf(t *testing.T, s *Service, user string) Quota {
	t.Helper()

	results, quota, err := s.Create(context.Background(), user, "", []File{file("probe.jpg", 1<<62, "image/jpeg")})
	if err != nil || results[0].Refused == "" {
		t.Fatalf("probe: %+v %v", results, err)
	}

	return quota
}

func momentFiles(t *testing.T, db *pgxpool.Pool, momentID string) map[string]int64 {
	t.Helper()

	rows, err := db.Query(context.Background(), `SELECT variant, size FROM moment_files WHERE moment_id = $1`, momentID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	files := map[string]int64{}

	for rows.Next() {
		var (
			variant string
			size    int64
		)

		if err := rows.Scan(&variant, &size); err != nil {
			t.Fatal(err)
		}

		files[variant] = size
	}

	return files
}

func TestProcessMakesAMoment(t *testing.T) {
	ctx := context.Background()
	s, bucket, db, user := processing(t)

	data := testPhoto(t, "make")
	f := fileOf("beach.jpg", photos.JPEG, data)
	upload := sendFile(t, s, bucket, user, "", f, data)

	if q := quotaOf(t, s, user); q.Reserved != f.Size || q.Used != 0 {
		t.Fatalf("before processing: %+v", q)
	}

	processAll(t, s)

	got, err := s.Get(ctx, user, upload.ID)
	if err != nil || got.Status != Ready || got.MomentID == "" || got.Attempts != 1 {
		t.Fatalf("%+v %v", got, err)
	}

	files := momentFiles(t, db, got.MomentID)
	if len(files) != 3 || files["original"] == 0 || files["small"] == 0 || files["large"] == 0 {
		t.Fatalf("files: %v", files)
	}

	// The reservation became use, with the clean original's size (never more than declared).
	q := quotaOf(t, s, user)
	if q.Reserved != 0 || q.Used != files["original"] || q.Used > f.Size {
		t.Fatalf("after: %+v, original %d", q, files["original"])
	}

	var (
		vaultID, name, kind string
		width, height       int
	)

	if err := db.QueryRow(ctx, `SELECT vault_id::text, name, kind, width, height FROM moments WHERE id = $1`, got.MomentID).Scan(&vaultID, &name, &kind, &width, &height); err != nil {
		t.Fatal(err)
	}

	if vaultID != upload.VaultID || name != "beach.jpg" || kind != "photo" || width != 640 || height != 480 {
		t.Fatalf("moment: %s %s %s %d×%d", vaultID, name, kind, width, height)
	}

	// The bucket holds the moment's files and nothing else: the incoming file is gone.
	prefix := "vaults/" + vaultID + "/moments/" + got.MomentID + "/"
	if keys := bucket.keys(); !slices.Equal(keys, []string{prefix + "large", prefix + "original", prefix + "small"}) {
		t.Fatalf("bucket: %v", keys)
	}

	original := bucket.data[prefix+"original"]
	if bytes.Contains(original, []byte("SECRET-NOTE")) || bytes.Contains(original, []byte("make")) {
		t.Fatal("the stored original kept the comment or the trailer")
	}

	if photos.Sniff(original[:32]) != photos.JPEG || photos.Sniff(bucket.data[prefix+"small"][:32]) != photos.WebP {
		t.Fatal("stored types")
	}
}

// While a photo is being processed its bytes stay reserved; once ready they are used. At no moment can
// another file take them.
func TestProcessKeepsTheQuotaWithoutAGap(t *testing.T) {
	ctx := context.Background()
	s, bucket, db, user := processing(t)

	data := testPhoto(t, "gap")
	f := fileOf("a.jpg", photos.JPEG, data)
	setLimit(t, db, user, f.Size+10)
	upload := sendFile(t, s, bucket, user, "", f, data)

	other := file("other.jpg", 11, photos.JPEG)

	var during []Result

	bucket.beforePut = func(string) {
		if during == nil {
			during, _, _ = s.Create(ctx, user, "", []File{other})
		}
	}

	processAll(t, s)

	if during == nil || during[0].Refused != QuotaExceeded {
		t.Fatalf("while processing: %+v", during)
	}

	if got, _ := s.Get(ctx, user, upload.ID); got.Status != Ready {
		t.Fatalf("%+v", got)
	}

	// The clean original is smaller (the comment and trailer went), which frees exactly that much.
	q := quotaOf(t, s, user)
	free := q.Limit - q.Used

	after, _, err := s.Create(ctx, user, "", []File{file("fits.jpg", free, photos.JPEG), file("over.jpg", 1, photos.JPEG)})
	if err != nil || after[0].Upload == nil || after[1].Refused != QuotaExceeded {
		t.Fatalf("after (free %d): %+v %v", free, after, err)
	}
}

func TestDuplicates(t *testing.T) {
	ctx := context.Background()
	s, bucket, db, user := processing(t)

	data := testPhoto(t, "dup")
	f := fileOf("a.jpg", photos.JPEG, data)

	// Two uploads of the same file, both sent before either is processed.
	first := sendFile(t, s, bucket, user, "", f, data)
	second := sendFile(t, s, bucket, user, "", fileOf("copy of a.jpg", photos.JPEG, data), data)

	processAll(t, s)

	a, _ := s.Get(ctx, user, first.ID)
	b, _ := s.Get(ctx, user, second.ID)

	if a.Status != Ready || b.Status != Ready || a.MomentID == "" || a.MomentID != b.MomentID {
		t.Fatalf("%+v\n%+v", a, b)
	}

	var count int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM moments WHERE vault_id = $1`, a.VaultID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("%d moments %v", count, err)
	}

	if keys := bucket.keys(); len(keys) != 3 {
		t.Fatalf("bucket: %v", keys)
	}

	// Asking again finds it at once: nothing to upload, nothing reserved.
	results, quota, err := s.Create(ctx, user, "", []File{f})
	if err != nil || results[0].Duplicate != a.MomentID || results[0].Upload != nil || quota.Reserved != 0 {
		t.Fatalf("%+v %+v %v", results, quota, err)
	}

	// In the Trash it still counts against the quota, but it isn't a duplicate: keeping it again is new.
	used := quotaOf(t, s, user).Used
	if _, err := db.Exec(ctx, `UPDATE moments SET deleted_at = now() WHERE id = $1`, a.MomentID); err != nil {
		t.Fatal(err)
	}

	if q := quotaOf(t, s, user); q.Used != used {
		t.Fatalf("trash should still count: %d, was %d", q.Used, used)
	}

	again := sendFile(t, s, bucket, user, "", f, data)
	processAll(t, s)

	if got, _ := s.Get(ctx, user, again.ID); got.Status != Ready || got.MomentID == a.MomentID {
		t.Fatalf("kept again: %+v", got)
	}

	if q := quotaOf(t, s, user); q.Used != 2*used {
		t.Fatalf("both count: %d", q.Used)
	}
}

// A duplicate across vaults is a different moment: deduplication never crosses a vault.
func TestDuplicatesStayInTheirVault(t *testing.T) {
	ctx := context.Background()
	s, bucket, db, user := processing(t)
	data := testPhoto(t, "vaults")
	f := fileOf("a.jpg", photos.JPEG, data)

	sendFile(t, s, bucket, user, "", f, data)
	processAll(t, s)

	other := newUser(t, db)
	if results, _, err := s.Create(ctx, other, "", []File{f}); err != nil || results[0].Upload == nil || results[0].Duplicate != "" {
		t.Fatalf("%+v %v", results, err)
	}
}

// editorOf adds a second user as editor of the owner's vault.
func editorOf(t *testing.T, db *pgxpool.Pool, vaultID string) string {
	t.Helper()

	editor := newUser(t, db)
	if _, err := db.Exec(context.Background(), `INSERT INTO vault_members (vault_id, user_id, role) VALUES ($1, $2, 'editor')`, vaultID, editor); err != nil {
		t.Fatal(err)
	}

	return editor
}

func revoke(t *testing.T, db *pgxpool.Pool, vaultID, userID string) {
	t.Helper()

	if _, err := db.Exec(context.Background(), `DELETE FROM vault_members WHERE vault_id = $1 AND user_id = $2`, vaultID, userID); err != nil {
		t.Fatal(err)
	}
}

func TestRemovedMembersCantFinishAdding(t *testing.T) {
	ctx := context.Background()

	t.Run("removed before complete", func(t *testing.T) {
		s, bucket, db, owner := processing(t)
		vault := s.store.onlyVault
		editor := editorOf(t, db, vault)

		data := testPhoto(t, "before-complete")
		f := fileOf("a.jpg", photos.JPEG, data)

		results, _, err := s.Create(ctx, editor, vault, []File{f})
		if err != nil || results[0].Upload == nil {
			t.Fatalf("%+v %v", results, err)
		}

		id := results[0].Upload.ID
		bucket.upload(Key(id), f.ContentType, data)
		revoke(t, db, vault, editor)

		got, err := s.Complete(ctx, editor, id)
		if err != nil || got.Status != Failed || got.FailureCode != Revoked {
			t.Fatalf("%+v %v", got, err)
		}

		if len(bucket.keys()) != 0 {
			t.Fatalf("bucket: %v", bucket.keys())
		}

		if q := quotaOf(t, s, owner); q.Reserved != 0 {
			t.Fatalf("still reserved: %+v", q)
		}
	})

	t.Run("removed while processing", func(t *testing.T) {
		s, bucket, db, owner := processing(t)
		vault := s.store.onlyVault
		editor := editorOf(t, db, vault)

		data := testPhoto(t, "while-processing")
		upload := sendFile(t, s, bucket, editor, vault, fileOf("a.jpg", photos.JPEG, data), data)

		// Removed after the files were made and stored, just before the moment is recorded.
		bucket.beforePut = func(key string) {
			if strings.HasSuffix(key, "/small") {
				revoke(t, db, vault, editor)
			}
		}

		processAll(t, s)

		got, err := s.Get(ctx, editor, upload.ID)
		if err != nil || got.Status != Failed || got.FailureCode != Revoked {
			t.Fatalf("%+v %v", got, err)
		}

		var count int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM moments WHERE vault_id = $1`, vault).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%d moments %v", count, err)
		}

		if keys := bucket.keys(); len(keys) != 0 {
			t.Fatalf("left in the bucket: %v", keys)
		}

		if q := quotaOf(t, s, owner); q.Reserved != 0 || q.Used != 0 {
			t.Fatalf("quota: %+v", q)
		}
	})
}

func TestProcessRefusesBadFiles(t *testing.T) {
	ctx := context.Background()
	s, bucket, db, user := processing(t)
	jpeg := testPhoto(t, "bad")

	dir := t.TempDir()
	bomb := filepath.Join(dir, "bomb.png")

	if b, err := exec.Command("vips", "black", bomb, "30000", "30000").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, b)
	}

	bombData, err := os.ReadFile(bomb)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name        string
		contentType string
		data        []byte
		code        string
	}{
		{"a 900 MP PNG", photos.PNG, bombData, Unreadable},
		{"a JPEG cut in half", photos.JPEG, jpeg[:len(jpeg)/2], Unreadable},
		{"a JPEG declared as PNG", photos.PNG, jpeg, Mismatch},
		{"HTML declared as JPEG", photos.JPEG, append([]byte("<!doctype html><script>alert(1)</script>"), make([]byte, 64)...), Mismatch},
		{"JPEG magic and garbage", photos.JPEG, append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, bytes.Repeat([]byte{0x41}, 4096)...), Unreadable},
	}

	for _, c := range cases {
		upload := sendFile(t, s, bucket, user, "", fileOf(c.name, c.contentType, c.data), c.data)
		processAll(t, s)

		got, err := s.Get(ctx, user, upload.ID)
		if err != nil || got.Status != Failed || got.FailureCode != c.code {
			t.Errorf("%s: %+v %v", c.name, got, err)
		}
	}

	if keys := bucket.keys(); len(keys) != 0 {
		t.Errorf("left in the bucket: %v", keys)
	}

	if q := quotaOf(t, s, user); q.Reserved != 0 || q.Used != 0 {
		t.Errorf("quota: %+v", q)
	}

	var count int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM moments WHERE vault_id = $1`, s.store.onlyVault).Scan(&count); err != nil || count != 0 {
		t.Errorf("%d moments %v", count, err)
	}
}

// What the bucket holds by the time processing reads it is checked again: :complete only saw a HEAD.
func TestProcessChecksTheBytesAgain(t *testing.T) {
	ctx := context.Background()
	s, bucket, _, user := processing(t)

	for name, swap := range map[string]func([]byte) []byte{
		"other bytes, same size": func(b []byte) []byte { c := slices.Clone(b); c[len(c)-1] ^= 1; return c },
		"one byte more":          func(b []byte) []byte { return append(slices.Clone(b), 0) },
		"one byte less":          func(b []byte) []byte { return b[:len(b)-1] },
		"gone":                   func([]byte) []byte { return nil },
	} {
		data := testPhoto(t, name)
		upload := sendFile(t, s, bucket, user, "", fileOf(name, photos.JPEG, data), data)

		bucket.mu.Lock()
		if swapped := swap(data); swapped == nil {
			delete(bucket.data, Key(upload.ID))
		} else {
			bucket.data[Key(upload.ID)] = swapped
		}
		bucket.mu.Unlock()

		processAll(t, s)

		if got, _ := s.Get(ctx, user, upload.ID); got.Status != Failed || got.FailureCode != Mismatch {
			t.Errorf("%s: %+v", name, got)
		}
	}
}

// A worker that dies mid-way leaves its lease; once it runs out another worker finishes the upload.
func TestAnInterruptedUploadIsTakenAgain(t *testing.T) {
	ctx := context.Background()
	s, bucket, db, user := processing(t)

	data := testPhoto(t, "crash")
	upload := sendFile(t, s, bucket, user, "", fileOf("a.jpg", photos.JPEG, data), data)

	claimed, ok, err := s.store.claim(ctx, leaseFor)
	if err != nil || !ok || claimed.ID != upload.ID {
		t.Fatalf("%+v %v %v", claimed, ok, err)
	}

	// Its worker is gone. While the lease holds, nobody else takes it.
	if worked, _ := s.ProcessNext(ctx); worked {
		t.Fatal("taken while the lease held")
	}

	if _, err := db.Exec(ctx, `UPDATE uploads SET lease_until = now() - interval '1 second' WHERE id = $1`, upload.ID); err != nil {
		t.Fatal(err)
	}

	processAll(t, s)

	got, _ := s.Get(ctx, user, upload.ID)
	if got.Status != Ready || got.Attempts != 2 {
		t.Fatalf("%+v", got)
	}
}

// A worker that comes back after its lease ran out, while another worker holds the upload, can neither
// keep a moment nor fail the upload: only the lease holder decides.
func TestAStaleWorkerCantDecide(t *testing.T) {
	ctx := context.Background()
	s, bucket, db, user := processing(t)

	data := testPhoto(t, "stale")
	upload := sendFile(t, s, bucket, user, "", fileOf("a.jpg", photos.JPEG, data), data)

	stale, _, _ := s.store.claim(ctx, leaseFor)
	expireLease(t, db, upload.ID)

	holder, ok, err := s.store.claim(ctx, leaseFor)
	if err != nil || !ok || holder.ID != upload.ID || holder.Attempts != 2 {
		t.Fatalf("%+v %v %v", holder, ok, err)
	}

	id, err := s.store.newMomentID(ctx)
	if err != nil {
		t.Fatal(err)
	}

	moment := moments.Moment{ID: id, VaultID: stale.VaultID, CreatedBy: stale.UserID, Kind: "photo", ContentType: photos.JPEG, SHA256: stale.SHA256}
	if _, _, err := s.store.ready(ctx, stale, moment, []moments.File{{Variant: "original", Key: "k-" + id, ContentType: photos.JPEG, Size: 1}}); !errors.Is(err, errLeaseLost) {
		t.Fatalf("stale ready: %v", err)
	}

	if err := s.store.fail(ctx, stale, Internal); err != nil {
		t.Fatal(err)
	}

	got, _ := s.Get(ctx, user, upload.ID)
	if got.Status != Processing || got.Attempts != 2 {
		t.Fatalf("the stale worker changed it: %+v", got)
	}

	var count int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM moments WHERE vault_id = $1`, stale.VaultID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("%d moments %v", count, err)
	}
}

func expireLease(t *testing.T, db *pgxpool.Pool, uploadID string) {
	t.Helper()

	if _, err := db.Exec(context.Background(), `UPDATE uploads SET lease_until = now() - interval '1 second' WHERE id = $1`, uploadID); err != nil {
		t.Fatal(err)
	}
}

// A storage outage is retried, and after maxAttempts the upload fails with internal and frees its bytes.
func TestTransientFailuresGiveUp(t *testing.T) {
	ctx := context.Background()
	s, bucket, db, user := processing(t)

	data := testPhoto(t, "outage")
	upload := sendFile(t, s, bucket, user, "", fileOf("a.jpg", photos.JPEG, data), data)
	bucket.getErr = storage.ErrUnavailable

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if worked, _ := s.ProcessNext(ctx); !worked {
			t.Fatalf("attempt %d: nothing to do", attempt)
		}

		got, _ := s.Get(ctx, user, upload.ID)

		if attempt < maxAttempts && (got.Status != Processing || got.Attempts != attempt) {
			t.Fatalf("attempt %d: %+v", attempt, got)
		}

		expireLease(t, db, upload.ID)
	}

	got, _ := s.Get(ctx, user, upload.ID)
	if got.Status != Failed || got.FailureCode != Internal {
		t.Fatalf("%+v", got)
	}

	if q := quotaOf(t, s, user); q.Reserved != 0 {
		t.Fatalf("still reserved: %+v", q)
	}

	if worked, _ := s.ProcessNext(ctx); worked {
		t.Fatal("a failed upload was taken again")
	}
}

// Storage recovering in time: the retry keeps the photo.
func TestATransientFailureRecovers(t *testing.T) {
	ctx := context.Background()
	s, bucket, db, user := processing(t)

	data := testPhoto(t, "recover")
	upload := sendFile(t, s, bucket, user, "", fileOf("a.jpg", photos.JPEG, data), data)

	failed := false
	bucket.beforePut = func(key string) {
		if !failed && strings.HasSuffix(key, "/large") {
			failed = true

			bucket.mu.Lock()
			bucket.putErr = storage.ErrUnavailable
			bucket.mu.Unlock()
		}
	}

	_, _ = s.ProcessNext(ctx)

	// The failed attempt cleaned up what it had written.
	if keys := bucket.keys(); !slices.Equal(keys, []string{Key(upload.ID)}) {
		t.Fatalf("after the failure: %v", keys)
	}

	bucket.mu.Lock()
	bucket.putErr = nil
	bucket.mu.Unlock()

	expireLease(t, db, upload.ID)
	processAll(t, s)

	if got, _ := s.Get(ctx, user, upload.ID); got.Status != Ready || got.Attempts != 2 {
		t.Fatalf("%+v", got)
	}
}

// Workers in parallel never take the same upload twice.
func TestWorkersShareTheWork(t *testing.T) {
	ctx := context.Background()
	s, bucket, db, user := processing(t)

	const n = 24

	ids := make([]string, n)
	for i := range ids {
		data := testPhoto(t, "many-"+string(rune('a'+i)))
		ids[i] = sendFile(t, s, bucket, user, "", fileOf("a.jpg", photos.JPEG, data), data).ID
	}

	workCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		s.Work(workCtx, 3)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Minute)

	for {
		var waiting int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM uploads WHERE id = ANY($1) AND status IN ('uploaded', 'processing')`, ids).Scan(&waiting); err != nil {
			t.Fatal(err)
		}

		if waiting == 0 {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("%d still waiting", waiting)
		}

		time.Sleep(50 * time.Millisecond)
	}

	stop()
	<-done

	var ready, once, kept int
	if err := db.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status = 'ready'), count(*) FILTER (WHERE attempts = 1), count(DISTINCT moment_id)
		FROM uploads WHERE id = ANY($1)`, ids).Scan(&ready, &once, &kept); err != nil {
		t.Fatal(err)
	}

	if ready != n || once != n || kept != n {
		t.Fatalf("ready %d, processed once %d, moments %d, want %d", ready, once, kept, n)
	}

	bucket.mu.Lock()
	puts := len(bucket.written)
	bucket.mu.Unlock()

	if puts != 3*n {
		t.Fatalf("%d puts, want %d", puts, 3*n)
	}
}

func TestCleanupDeletesReadyUploads(t *testing.T) {
	ctx := context.Background()
	s, bucket, db, user := processing(t)

	data := testPhoto(t, "cleanup")
	upload := sendFile(t, s, bucket, user, "", fileOf("a.jpg", photos.JPEG, data), data)
	processAll(t, s)

	if _, err := db.Exec(ctx, `UPDATE uploads SET completed_at = now() - interval '2 days' WHERE id = $1`, upload.ID); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Get(ctx, user, upload.ID); err == nil {
		t.Fatal("the ready upload is still there")
	}

	// The moment it became stays.
	if q := quotaOf(t, s, user); q.Used == 0 {
		t.Fatal("the moment went with the upload")
	}
}
