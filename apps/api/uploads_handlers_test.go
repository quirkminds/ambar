package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ambar/api/auth"
	"ambar/api/storage"
	"ambar/api/uploads"
	"ambar/api/vaults"
)

// memoryBucket stands in for R2: put plays the client's PUT, with the checksum that was signed.
type memoryBucket struct {
	mu      sync.Mutex
	objects map[string]storage.Object
	signed  map[string]string
}

func (b *memoryBucket) PresignPut(_ context.Context, key, contentType string, _ int64, sha256Hex string, _ time.Duration) (string, map[string]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.signed == nil {
		b.signed = map[string]string{}
	}

	b.signed[key] = sha256Hex

	return "https://bucket.example/" + key + "?X-Amz-Signature=test", map[string]string{"Content-Type": contentType, "x-amz-checksum-sha256": sha256Hex}, nil
}

func (b *memoryBucket) Head(_ context.Context, key string) (storage.Object, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	object, ok := b.objects[key]
	if !ok {
		return storage.Object{}, storage.ErrNotFound
	}

	return object, nil
}

func (b *memoryBucket) Delete(_ context.Context, key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	delete(b.objects, key)

	return nil
}

func (b *memoryBucket) put(key string, size int64, contentType string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.objects[key] = storage.Object{Size: size, ContentType: contentType, SHA256: b.signed[key]}
}

// The upload flow over HTTP, against Postgres: every response is checked against openapi.yaml.
func TestUploadsOverHTTP(t *testing.T) {
	pool := testPool(t)
	mailer := &captureMailer{}
	bucket := &memoryBucket{objects: map[string]storage.Object{}}
	h := withContract(newRouter(&api{
		logger:         slog.New(slog.DiscardHandler),
		db:             pool,
		auth:           auth.NewService(pool, mailer, []byte("test-secret-test-secret-test-secret"), app),
		uploads:        uploads.NewService(pool, bucket),
		allowedOrigins: []string{app},
	}))

	// Sign up over HTTP: the account comes with its personal vault.
	email := "uploads-" + randomSuffix() + "@example.com"
	headers := map[string]string{"Content-Type": "application/json", "Origin": app}
	request(h, "POST", "/v1/auth/email", "192.0.2.50:1", headers, `{"email":"`+email+`"}`)

	rec := request(h, "POST", "/v1/auth/email/verify", "192.0.2.50:1", headers, `{"email":"`+email+`","code":"`+mailer.code+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("sign in: %d %s", rec.Code, rec.Body.String())
	}

	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE email = $1`, email) })

	cookie := rec.Result().Cookies()[0]
	signedIn := map[string]string{"Content-Type": "application/json", "Origin": app, "Cookie": cookie.Name + "=" + cookie.Value}
	sum := strings.Repeat("ab", 32)

	rec = request(h, "POST", "/v1/uploads", "192.0.2.50:1", signedIn, `{"files":[
		{"name":"IMG_2041.jpg","size":2048,"contentType":"image/jpeg","sha256":"`+sum+`","capturedAt":"2026-10-18T17:42:10Z","width":4032,"height":3024},
		{"name":"page.svg","size":10,"contentType":"image/svg+xml","sha256":"`+sum+`"}]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}

	var created struct {
		Results []struct {
			Status string
			Code   string
			Upload struct{ ID string }
			Put    struct {
				URL     string
				Headers map[string]string
			}
		}
		Quota struct{ ReservedBytes, LimitBytes int64 }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	if created.Results[0].Status != "accepted" || created.Results[0].Put.Headers["Content-Type"] != "image/jpeg" || created.Results[1].Code != uploads.UnsupportedType {
		t.Fatalf("results: %s", rec.Body.String())
	}

	if created.Quota.ReservedBytes != 2048 || created.Quota.LimitBytes != vaults.SeedStorageBytes {
		t.Fatalf("quota: %+v", created.Quota)
	}

	id := created.Results[0].Upload.ID
	path := "/v1/uploads/" + id

	if rec := request(h, "POST", path+":complete", "192.0.2.50:1", signedIn, ""); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"upload-missing"`) {
		t.Fatalf("complete before the PUT: %d %s", rec.Code, rec.Body.String())
	}

	bucket.put(uploads.Key(id), 2048, "image/jpeg")

	if rec := request(h, "POST", path+":complete", "192.0.2.50:1", signedIn, ""); rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"status":"uploaded"`) {
		t.Fatalf("complete: %d %s", rec.Code, rec.Body.String())
	}

	if rec := request(h, "GET", path, "192.0.2.50:1", signedIn, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"uploaded"`) {
		t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
	}

	if rec := request(h, "DELETE", path, "192.0.2.50:1", signedIn, ""); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"upload-not-pending"`) {
		t.Fatalf("cancel uploaded: %d %s", rec.Code, rec.Body.String())
	}

	// Ids that can't exist, and vaults the caller isn't in, look the same as missing ones.
	if rec := request(h, "GET", "/v1/uploads/not-an-id", "192.0.2.50:1", signedIn, ""); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"upload-not-found"`) {
		t.Fatalf("bad id: %d %s", rec.Code, rec.Body.String())
	}

	if rec := request(h, "POST", "/v1/uploads", "192.0.2.50:1", signedIn, `{"vaultId":"00000000-0000-4000-8000-000000000000","files":[{"name":"a.jpg","size":1,"contentType":"image/jpeg","sha256":"`+sum+`"}]}`); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"vault-not-found"`) {
		t.Fatalf("other vault: %d %s", rec.Code, rec.Body.String())
	}

	if rec := request(h, "POST", "/v1/uploads", "192.0.2.50:1", signedIn, `{"files":[]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("no files: %d %s", rec.Code, rec.Body.String())
	}
}

// Without R2 configured (development), upload routes answer 503 instead of failing later.
func TestUploadsWithoutStorage(t *testing.T) {
	pool := testPool(t)
	mailer := &captureMailer{}
	h := withContract(newRouter(&api{
		logger:         slog.New(slog.DiscardHandler),
		db:             pool,
		auth:           auth.NewService(pool, mailer, []byte("test-secret-test-secret-test-secret"), app),
		allowedOrigins: []string{app},
	}))

	email := "nostorage-" + randomSuffix() + "@example.com"
	headers := map[string]string{"Content-Type": "application/json", "Origin": app}
	request(h, "POST", "/v1/auth/email", "192.0.2.51:1", headers, `{"email":"`+email+`"}`)
	rec := request(h, "POST", "/v1/auth/email/verify", "192.0.2.51:1", headers, `{"email":"`+email+`","code":"`+mailer.code+`"}`)

	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE email = $1`, email) })

	cookie := rec.Result().Cookies()[0]
	signedIn := map[string]string{"Content-Type": "application/json", "Origin": app, "Cookie": cookie.Name + "=" + cookie.Value}

	if rec := request(h, "POST", "/v1/uploads", "192.0.2.51:1", signedIn, `{"files":[]}`); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"storage-unavailable"`) {
		t.Fatalf("create without storage: %d %s", rec.Code, rec.Body.String())
	}
}

// Every account has exactly one personal vault: existing ones from the migration, new ones from sign-up.
func TestEveryUserHasAPersonalVault(t *testing.T) {
	pool := testPool(t)

	var orphans int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM users u
		WHERE NOT EXISTS (
			SELECT 1 FROM vaults v JOIN vault_members m ON m.vault_id = v.id AND m.user_id = u.id AND m.role = 'owner'
			WHERE v.personal_of = u.id)`).Scan(&orphans); err != nil {
		t.Fatal(err)
	}

	if orphans != 0 {
		t.Fatalf("%d accounts without a personal vault", orphans)
	}
}

// The limits the landing and the app promise (packages/core) are the ones the API enforces.
func TestPlanLimitsMatchCore(t *testing.T) {
	read := func(path string) string {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}

		return string(b)
	}

	number := func(source, pattern string) int64 {
		m := regexp.MustCompile(pattern).FindStringSubmatch(source)
		if m == nil {
			t.Fatalf("%s not found", pattern)
		}

		n, _ := strconv.ParseInt(strings.ReplaceAll(m[1], "_", ""), 10, 64)

		return n
	}

	plans := read("../../packages/core/src/plans.ts")
	moment := read("../../packages/core/src/moment.ts")

	if !strings.Contains(plans, "const GB = 1024 ** 3;") {
		t.Fatal("plans.ts: GB is no longer 1024 ** 3")
	}

	if seed := number(plans, `id: "seed",[\s\S]*?storageBytes: (\d+) \* GB`) << 30; seed != vaults.SeedStorageBytes {
		t.Errorf("seed storage: core %d, API %d", seed, vaults.SeedStorageBytes)
	}

	if maxFile := number(moment, `MAX_FILE_BYTES = (\d+) \* 1024 \* 1024;`) << 20; maxFile != uploads.MaxFileBytes {
		t.Errorf("max file: core %d, API %d", maxFile, uploads.MaxFileBytes)
	}

	if maxClip := number(moment, `MAX_CLIP_DURATION_MS = ([\d_]+);`); maxClip != uploads.MaxClipMs {
		t.Errorf("max clip: core %d, API %d", maxClip, uploads.MaxClipMs)
	}
}
