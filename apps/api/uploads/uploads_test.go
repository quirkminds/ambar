package uploads

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"ambar/api/migrations"
	"ambar/api/storage"
	"ambar/api/vaults"
)

// fakeStorage holds "uploaded" objects in memory; put stands in for the client's PUT and, like R2, stores
// the checksum that was signed (R2 refuses other bytes). afterHead runs once
// Head has read an object, so tests can make something happen between Complete's check and its write;
// headErr and deleteErr make the bucket fail.
type fakeStorage struct {
	mu        sync.Mutex
	objects   map[string]storage.Object
	signed    map[string]string
	deleted   []string
	afterHead func()
	headErr   error
	deleteErr error
	// data is the bytes behind objects, for processing; written is every key Put stored.
	data    map[string][]byte
	written []string
	getErr  error
	putErr  error
	// beforePut runs on every Put, outside the lock.
	beforePut func(key string)
}

func (f *fakeStorage) Get(_ context.Context, key string, max int64) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.getErr != nil {
		return nil, f.getErr
	}

	b, ok := f.data[key]
	if !ok {
		return nil, storage.ErrNotFound
	}

	return io.NopCloser(io.LimitReader(bytes.NewReader(b), max+1)), nil
}

func (f *fakeStorage) Put(_ context.Context, key, contentType, path string) error {
	f.mu.Lock()
	beforePut := f.beforePut
	f.mu.Unlock()

	if beforePut != nil {
		beforePut(key)
	}

	f.mu.Lock()
	putErr := f.putErr
	f.mu.Unlock()

	if putErr != nil {
		return putErr
	}

	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	sum := sha256.Sum256(b)

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.data == nil {
		f.data = map[string][]byte{}
	}

	f.data[key] = b
	f.written = append(f.written, key)

	if f.objects == nil {
		f.objects = map[string]storage.Object{}
	}

	f.objects[key] = storage.Object{Size: int64(len(b)), ContentType: contentType, SHA256: hex.EncodeToString(sum[:])}

	return nil
}

// upload plays the client's PUT of real bytes, with the checksum that was signed.
func (f *fakeStorage) upload(key, contentType string, b []byte) {
	f.mu.Lock()

	if f.data == nil {
		f.data = map[string][]byte{}
	}

	f.data[key] = b
	sum := f.signed[key]
	f.mu.Unlock()

	f.putObject(key, storage.Object{Size: int64(len(b)), ContentType: contentType, SHA256: sum})
}

// keys is every object in the bucket now.
func (f *fakeStorage) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	var keys []string
	for k := range f.objects {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	return keys
}

func (f *fakeStorage) PresignPut(_ context.Context, key, contentType string, _ int64, sha256Hex string, _ time.Duration) (string, map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.signed == nil {
		f.signed = map[string]string{}
	}

	f.signed[key] = sha256Hex

	return "https://bucket.example/" + key, map[string]string{"Content-Type": contentType, "x-amz-checksum-sha256": sha256Hex}, nil
}

func (f *fakeStorage) Head(_ context.Context, key string) (storage.Object, error) {
	f.mu.Lock()
	object, ok := f.objects[key]
	afterHead, headErr := f.afterHead, f.headErr
	f.mu.Unlock()

	if headErr != nil {
		return storage.Object{}, headErr
	}

	if !ok {
		return storage.Object{}, storage.ErrNotFound
	}

	if afterHead != nil {
		afterHead()
	}

	return object, nil
}

func (f *fakeStorage) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.deleteErr != nil {
		return f.deleteErr
	}

	delete(f.objects, key)
	delete(f.data, key)
	f.deleted = append(f.deleted, key)

	return nil
}

func (f *fakeStorage) put(key string, size int64, contentType string) {
	f.mu.Lock()
	sum := f.signed[key]
	f.mu.Unlock()

	f.putObject(key, storage.Object{Size: size, ContentType: contentType, SHA256: sum})
}

func (f *fakeStorage) putObject(key string, object storage.Object) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.objects == nil {
		f.objects = map[string]storage.Object{}
	}

	f.objects[key] = object
}

func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()

	if testing.Short() {
		t.Skip("needs Postgres")
	}

	// The same .env.local as the API (never overriding a real variable); no URL lives in the code.
	_ = godotenv.Load("../.env.local")

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Fatal("DATABASE_URL is not set: copy apps/api/.env.example to apps/api/.env.local, or run with -short")
	}

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(pool.Close)

	if err := migrations.Up(context.Background(), pool); err != nil {
		t.Fatalf("Postgres not reachable or migrations failed (bun run --filter @ambar/api db:up): %v", err)
	}

	return pool
}

func newTestService(t *testing.T) (*Service, *fakeStorage, *pgxpool.Pool) {
	db := testDB(t)
	bucket := &fakeStorage{}

	return NewService(db, bucket), bucket, db
}

// newUser creates an account with its personal vault, as sign-up does; the user is deleted afterwards.
func newUser(t *testing.T, db *pgxpool.Pool) string {
	t.Helper()

	ctx := context.Background()
	b := make([]byte, 6)
	_, _ = rand.Read(b)

	var id string
	if err := db.QueryRow(ctx, `INSERT INTO users (email) VALUES ($1) RETURNING id::text`, "uploads-"+hex.EncodeToString(b)+"@example.com").Scan(&id); err != nil {
		t.Fatal(err)
	}

	if err := vaults.CreatePersonal(ctx, db, id); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _, _ = db.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id) })

	return id
}

func setLimit(t *testing.T, db *pgxpool.Pool, userID string, limit int64) {
	t.Helper()

	if _, err := db.Exec(context.Background(), `UPDATE vaults SET storage_limit_bytes = $2 WHERE personal_of = $1`, userID, limit); err != nil {
		t.Fatal(err)
	}
}

func file(name string, size int64, contentType string) File {
	sum := sha256.Sum256([]byte(name))

	return File{Name: name, Size: size, ContentType: contentType, SHA256: hex.EncodeToString(sum[:])}
}

func TestCreateAnswersEachFile(t *testing.T) {
	ctx := context.Background()
	s, _, db := newTestService(t)
	user := newUser(t, db)
	setLimit(t, db, user, 1000)

	results, quota, err := s.Create(ctx, user, "", []File{
		file("a.jpg", 600, "image/jpeg"),
		file("page.html", 10, "text/html"),
		file("big.heic", MaxFileBytes+1, "image/heic"),
		file("b.png", 500, "image/png"),
		file("c.webp", 400, "image/webp"),
		file("d.pdf", 10, "application/pdf"),
		file("e.mov", 10, "video/quicktime"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if results[0].Upload == nil || results[0].Upload.Status != Pending || results[0].Put.Headers["Content-Type"] != "image/jpeg" {
		t.Fatalf("a.jpg: %+v", results[0])
	}

	if results[1].Refused != UnsupportedType {
		t.Fatalf("page.html: %+v", results[1])
	}

	if results[2].Refused != FileTooLarge || results[2].MaxBytes != MaxFileBytes {
		t.Fatalf("big.heic: %+v", results[2])
	}

	// 600 reserved of 1000: 500 doesn't fit, 400 does.
	if results[3].Refused != QuotaExceeded || results[3].BytesLeft != 400 {
		t.Fatalf("b.png: %+v", results[3])
	}

	if results[4].Upload == nil {
		t.Fatalf("c.webp: %+v", results[4])
	}

	// Not processed yet (phase 2b): refused before upload, never accepted and then failed.
	if results[5].Refused != UnsupportedType || results[6].Refused != UnsupportedType {
		t.Fatalf("pdf, mov: %+v %+v", results[5], results[6])
	}

	if quota.Limit != 1000 || quota.Reserved != 1000 || quota.Used != 0 {
		t.Fatalf("quota: %+v", quota)
	}

	if !results[0].Put.ExpiresAt.Before(results[0].Upload.ExpiresAt) {
		t.Fatal("the PUT should expire before the time to complete")
	}
}

func TestCreateRefusesMalformedRequests(t *testing.T) {
	ctx := context.Background()
	s, _, db := newTestService(t)
	user := newUser(t, db)

	tooMany := make([]File, MaxFiles+1)
	for i := range tooMany {
		tooMany[i] = file("x.jpg", 1, "image/jpeg")
	}

	long := MaxClipMs + 1
	badSum := file("a.jpg", 1, "image/jpeg")
	badSum.SHA256 = "ABC"
	longClip := file("a.mp4", 1, "video/mp4")
	longClip.DurationMs = &long

	for name, files := range map[string][]File{
		"none":      nil,
		"too many":  tooMany,
		"no name":   {file("", 1, "image/jpeg")},
		"empty":     {file("a.jpg", 0, "image/jpeg")},
		"bad sum":   {badSum},
		"long clip": {longClip},
	} {
		if _, _, err := s.Create(ctx, user, "", files); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestCreateChecksTheVault(t *testing.T) {
	ctx := context.Background()
	s, _, db := newTestService(t)
	owner, viewer, stranger := newUser(t, db), newUser(t, db), newUser(t, db)

	var vaultID string
	if err := db.QueryRow(ctx, `SELECT id::text FROM vaults WHERE personal_of = $1`, owner).Scan(&vaultID); err != nil {
		t.Fatal(err)
	}

	if _, err := db.Exec(ctx, `INSERT INTO vault_members (vault_id, user_id, role) VALUES ($1, $2, 'viewer')`, vaultID, viewer); err != nil {
		t.Fatal(err)
	}

	files := []File{file("a.jpg", 1, "image/jpeg")}

	if _, _, err := s.Create(ctx, owner, vaultID, files); err != nil {
		t.Fatalf("owner: %v", err)
	}

	if _, _, err := s.Create(ctx, viewer, vaultID, files); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("viewer: %v", err)
	}

	if _, _, err := s.Create(ctx, stranger, vaultID, files); !errors.Is(err, vaults.ErrNotFound) {
		t.Fatalf("stranger: %v", err)
	}
}

// Parallel requests are serialized on the vault, so together they never reserve more than its limit.
func TestCreateNeverOverbooksTheQuota(t *testing.T) {
	ctx := context.Background()
	s, _, db := newTestService(t)
	user := newUser(t, db)
	setLimit(t, db, user, 10_000)

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		accepted int64
	)

	for range 12 {
		wg.Go(func() {
			results, _, err := s.Create(ctx, user, "", []File{file("a.jpg", 1000, "image/jpeg"), file("b.jpg", 1000, "image/jpeg")})
			if err != nil {
				t.Error(err)

				return
			}

			mu.Lock()
			defer mu.Unlock()

			for _, r := range results {
				if r.Upload != nil {
					accepted += r.Upload.Size
				}
			}
		})
	}

	wg.Wait()

	if accepted != 10_000 {
		t.Fatalf("accepted %d bytes for a 10000-byte vault", accepted)
	}
}

func TestCreateCapsPendingUploads(t *testing.T) {
	ctx := context.Background()
	s, _, db := newTestService(t)
	user := newUser(t, db)

	batch := make([]File, MaxFiles)
	for i := range batch {
		batch[i] = file("a.jpg", 1, "image/jpeg")
	}

	for range maxPending / MaxFiles {
		if _, _, err := s.Create(ctx, user, "", batch); err != nil {
			t.Fatal(err)
		}
	}

	if _, _, err := s.Create(ctx, user, "", batch[:1]); !errors.Is(err, ErrTooManyPending) {
		t.Fatalf("201st pending upload: %v", err)
	}
}

func TestComplete(t *testing.T) {
	ctx := context.Background()
	s, bucket, db := newTestService(t)
	user := newUser(t, db)

	results, _, err := s.Create(ctx, user, "", []File{file("a.jpg", 100, "image/jpeg"), file("b.jpg", 100, "image/jpeg")})
	if err != nil {
		t.Fatal(err)
	}

	good, bad := results[0].Upload.ID, results[1].Upload.ID

	if _, err := s.Complete(ctx, user, good); !errors.Is(err, ErrMissing) {
		t.Fatalf("before the PUT: %v", err)
	}

	bucket.put(Key(good), 100, "image/jpeg")

	upload, err := s.Complete(ctx, user, good)
	if err != nil || upload.Status != Uploaded {
		t.Fatalf("complete: %+v %v", upload, err)
	}

	// Repeating it changes nothing.
	if again, err := s.Complete(ctx, user, good); err != nil || again.Status != Uploaded {
		t.Fatalf("again: %+v %v", again, err)
	}

	// Only whoever sent it can complete it.
	if _, err := s.Complete(ctx, newUser(t, db), good); !errors.Is(err, ErrNotFound) {
		t.Fatalf("someone else: %v", err)
	}

	// What arrived isn't what was declared: failed, and the object is gone.
	bucket.put(Key(bad), 99, "image/jpeg")

	failed, err := s.Complete(ctx, user, bad)
	if err != nil || failed.Status != Failed || failed.FailureCode != Mismatch {
		t.Fatalf("mismatch: %+v %v", failed, err)
	}

	if _, err := bucket.Head(ctx, Key(bad)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("the mismatched object should be deleted")
	}
}

func TestCompleteTooLateExpires(t *testing.T) {
	ctx := context.Background()
	s, bucket, db := newTestService(t)
	user := newUser(t, db)

	results, _, err := s.Create(ctx, user, "", []File{file("a.jpg", 100, "image/jpeg")})
	if err != nil {
		t.Fatal(err)
	}

	id := results[0].Upload.ID
	bucket.put(Key(id), 100, "image/jpeg")

	if _, err := db.Exec(ctx, `UPDATE uploads SET expires_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Complete(ctx, user, id); !errors.Is(err, ErrExpired) {
		t.Fatalf("late: %v", err)
	}

	if _, err := s.Complete(ctx, user, id); !errors.Is(err, ErrExpired) {
		t.Fatalf("late again: %v", err)
	}

	// The reservation is released.
	_, quota, err := s.Create(ctx, user, "", []File{file("b.jpg", 1, "image/jpeg")})
	if err != nil || quota.Reserved != 1 {
		t.Fatalf("after expiry: %+v %v", quota, err)
	}
}

func TestCancel(t *testing.T) {
	ctx := context.Background()
	s, bucket, db := newTestService(t)
	user := newUser(t, db)

	results, _, err := s.Create(ctx, user, "", []File{file("a.jpg", 100, "image/jpeg"), file("b.jpg", 100, "image/jpeg")})
	if err != nil {
		t.Fatal(err)
	}

	pending, done := results[0].Upload.ID, results[1].Upload.ID

	if err := s.Cancel(ctx, user, pending); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Get(ctx, user, pending); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancelled upload still there: %v", err)
	}

	bucket.put(Key(done), 100, "image/jpeg")

	if _, err := s.Complete(ctx, user, done); err != nil {
		t.Fatal(err)
	}

	if err := s.Cancel(ctx, user, done); !errors.Is(err, ErrNotPending) {
		t.Fatalf("cancel uploaded: %v", err)
	}
}

func TestCleanup(t *testing.T) {
	ctx := context.Background()
	s, bucket, db := newTestService(t)
	user := newUser(t, db)

	results, _, err := s.Create(ctx, user, "", []File{file("a.jpg", 100, "image/jpeg")})
	if err != nil {
		t.Fatal(err)
	}

	id := results[0].Upload.ID
	bucket.put(Key(id), 100, "image/jpeg")

	if _, err := db.Exec(ctx, `UPDATE uploads SET expires_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}

	if upload, err := s.Get(ctx, user, id); err != nil || upload.Status != Expired {
		t.Fatalf("after cleanup: %+v %v", upload, err)
	}

	if _, err := bucket.Head(ctx, Key(id)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("cleanup should delete the expired upload's object")
	}

	// A day later the row goes too.
	if _, err := db.Exec(ctx, `UPDATE uploads SET completed_at = now() - interval '25 hours' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Get(ctx, user, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("finished upload kept: %v", err)
	}
}
