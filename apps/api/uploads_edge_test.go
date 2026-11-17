package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"ambar/api/auth"
	"ambar/api/moments"
	"ambar/api/storage"
	"ambar/api/uploads"
)

// Attacks on the upload endpoints over HTTP. Each test states what the contract and the rules promise.

type uploadsHarness struct {
	t      *testing.T
	pool   *pgxpool.Pool
	mailer *captureMailer
	bucket *memoryBucket
	router http.Handler
	h      http.Handler // router checked against openapi.yaml
}

func newUploadsHarness(t *testing.T) *uploadsHarness {
	pool := testPool(t)
	mailer := &captureMailer{}
	bucket := &memoryBucket{objects: map[string]storage.Object{}}
	router := newRouter(&api{
		logger:         slog.New(slog.DiscardHandler),
		db:             pool,
		auth:           auth.NewService(pool, mailer, []byte("test-secret-test-secret-test-secret"), app),
		uploads:        uploads.NewService(pool, bucket),
		allowedOrigins: []string{app},
	})

	return &uploadsHarness{t: t, pool: pool, mailer: mailer, bucket: bucket, router: router, h: withContract(router)}
}

type person struct {
	id, vault string
	headers   map[string]string
	ip        string
}

// signUp creates an account over HTTP, from its own IP so rate limits don't mix.
func (u *uploadsHarness) signUp(ip string) person {
	u.t.Helper()

	email := "edge-" + randomSuffix() + "@example.com"
	headers := map[string]string{"Content-Type": "application/json", "Origin": app}
	request(u.h, "POST", "/v1/auth/email", ip, headers, `{"email":"`+email+`"}`)

	rec := request(u.h, "POST", "/v1/auth/email/verify", ip, headers, `{"email":"`+email+`","code":"`+u.mailer.code+`"}`)
	if rec.Code != http.StatusOK {
		u.t.Fatalf("sign up: %d %s", rec.Code, rec.Body.String())
	}

	u.t.Cleanup(func() { _, _ = u.pool.Exec(context.Background(), `DELETE FROM users WHERE email = $1`, email) })

	p := person{ip: ip}
	if err := u.pool.QueryRow(context.Background(), `SELECT u.id::text, v.id::text FROM users u JOIN vaults v ON v.personal_of = u.id WHERE u.email = $1`, email).Scan(&p.id, &p.vault); err != nil {
		u.t.Fatal(err)
	}

	cookie := rec.Result().Cookies()[0]
	p.headers = map[string]string{"Content-Type": "application/json", "Origin": app, "Cookie": cookie.Name + "=" + cookie.Value}

	return p
}

func (u *uploadsHarness) join(p person, vault, role string) {
	u.t.Helper()

	if _, err := u.pool.Exec(context.Background(), `INSERT INTO vault_members (vault_id, user_id, role) VALUES ($1, $2, $3)`, vault, p.id, role); err != nil {
		u.t.Fatal(err)
	}
}

var sum = strings.Repeat("ab", 32)

func fileJSON(name string, size int) string {
	return fmt.Sprintf(`{"name":%q,"size":%d,"contentType":"image/jpeg","sha256":%q}`, name, size, sum)
}

type createAnswer struct {
	Results []struct {
		Status string
		Code   string
		Upload struct{ ID string }
	}
	Quota struct{ ReservedBytes, LimitBytes int64 }
}

func (u *uploadsHarness) create(p person, body string) (int, createAnswer, string) {
	u.t.Helper()

	rec := request(u.h, "POST", "/v1/uploads", p.ip, p.headers, body)

	var answer createAnswer
	if rec.Code == http.StatusCreated {
		if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
			u.t.Fatal(err)
		}
	}

	return rec.Code, answer, rec.Body.String()
}

func TestUploadsRejectMalformedBodies(t *testing.T) {
	u := newUploadsHarness(t)
	p := u.signUp("192.0.2.60:1")

	tooMany := make([]string, uploads.MaxFiles+1)
	for i := range tooMany {
		tooMany[i] = fileJSON("a.jpg", 1)
	}

	for name, body := range map[string]string{
		"empty body":         ``,
		"not an object":      `[]`,
		"files null":         `{"files":null}`,
		"unknown field":      `{"files":[` + fileJSON("a.jpg", 1) + `],"admin":true}`,
		"unknown file key":   `{"files":[{"name":"a.jpg","size":1,"contentType":"image/jpeg","sha256":"` + sum + `","path":"/etc"}]}`,
		"trailing data":      `{"files":[` + fileJSON("a.jpg", 1) + `]} {"files":[]}`,
		"size as a string":   `{"files":[{"name":"a.jpg","size":"1","contentType":"image/jpeg","sha256":"` + sum + `"}]}`,
		"size as a float":    `{"files":[{"name":"a.jpg","size":1.5,"contentType":"image/jpeg","sha256":"` + sum + `"}]}`,
		"size of 1e30":       `{"files":[{"name":"a.jpg","size":1e30,"contentType":"image/jpeg","sha256":"` + sum + `"}]}`,
		"width over int32":   `{"files":[{"name":"a.jpg","size":1,"contentType":"image/jpeg","sha256":"` + sum + `","width":4294967297}]}`,
		"NUL in the name":    `{"files":[{"name":"a\u0000.jpg","size":1,"contentType":"image/jpeg","sha256":"` + sum + `"}]}`,
		"escape in the name": `{"files":[{"name":"\u001b[31ma.jpg","size":1,"contentType":"image/jpeg","sha256":"` + sum + `"}]}`,
		"bad capturedAt":     `{"files":[{"name":"a.jpg","size":1,"contentType":"image/jpeg","sha256":"` + sum + `","capturedAt":"yesterday"}]}`,
		"21 files":           `{"files":[` + strings.Join(tooMany, ",") + `]}`,
		"vaultId not uuid":   `{"vaultId":"'; DROP TABLE uploads; --","files":[` + fileJSON("a.jpg", 1) + `]}`,
		"over 64 KB":         `{"files":[` + fileJSON(strings.Repeat("a", 70<<10), 1) + `]}`,
		"vaultId as number":  `{"vaultId":1,"files":[` + fileJSON("a.jpg", 1) + `]}`,
	} {
		code, _, body := u.create(p, body)
		if code >= 500 || code == http.StatusCreated {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}

	var pending int
	if err := u.pool.QueryRow(context.Background(), `SELECT count(*) FROM uploads WHERE user_id = $1`, p.id).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("malformed requests reserved %d uploads (%v)", pending, err)
	}
}

// The largest request a correct client can send (20 files, 255-character names, every character
// escaped in JSON) must fit the body limit.
func TestUploadsTakeTheLargestValidRequest(t *testing.T) {
	u := newUploadsHarness(t)
	p := u.signUp("192.0.2.61:1")

	name := strings.Repeat(`\ud83d\ude00`, 255)
	files := make([]string, uploads.MaxFiles)

	for i := range files {
		files[i] = `{"name":"` + name + `","size":104857600,"contentType":"video/quicktime","sha256":"` + sum + `","capturedAt":"2026-10-18T17:42:10.123456789+14:00","width":2147483647,"height":2147483647,"durationMs":30000}`
	}

	body := `{"vaultId":"` + p.vault + `","files":[` + strings.Join(files, ",") + `]}`
	code, answer, raw := u.create(p, body)

	if code != http.StatusCreated {
		t.Fatalf("%d bytes: %d %s", len(body), code, raw)
	}

	t.Logf("largest valid request: %d bytes", len(body))

	if len(answer.Results) != uploads.MaxFiles {
		t.Fatalf("%d results", len(answer.Results))
	}
}

func TestUploadsGuardHeadersAndSessions(t *testing.T) {
	u := newUploadsHarness(t)
	p := u.signUp("192.0.2.62:1")

	_, answer, _ := u.create(p, `{"files":[`+fileJSON("a.jpg", 1)+`]}`)
	id := answer.Results[0].Upload.ID

	routes := []struct{ method, path, body string }{
		{"POST", "/v1/uploads", `{"files":[` + fileJSON("a.jpg", 1) + `]}`},
		{"GET", "/v1/uploads/" + id, ""},
		{"POST", "/v1/uploads/" + id + ":complete", ""},
		{"DELETE", "/v1/uploads/" + id, ""},
	}

	for _, r := range routes {
		anonymous := map[string]string{"Content-Type": "application/json", "Origin": app}
		if rec := request(u.h, r.method, r.path, p.ip, anonymous, r.body); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a session: %d", r.method, r.path, rec.Code)
		}

		forged := map[string]string{"Content-Type": "application/json", "Origin": "https://evil.example", "Cookie": p.headers["Cookie"]}
		if r.method != "GET" {
			if rec := request(u.h, r.method, r.path, p.ip, forged, r.body); rec.Code != http.StatusForbidden {
				t.Errorf("%s %s from another origin: %d", r.method, r.path, rec.Code)
			}
		}
	}

	noType := map[string]string{"Origin": app, "Cookie": p.headers["Cookie"]}
	if rec := request(u.h, "POST", "/v1/uploads", p.ip, noType, `{"files":[`+fileJSON("a.jpg", 1)+`]}`); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("no Content-Type: %d", rec.Code)
	}

	form := map[string]string{"Content-Type": "text/plain", "Origin": app, "Cookie": p.headers["Cookie"]}
	if rec := request(u.h, "POST", "/v1/uploads", p.ip, form, `{"files":[`+fileJSON("a.jpg", 1)+`]}`); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain (a cross-site form can send it): %d", rec.Code)
	}

	// Nothing above touched the upload.
	if rec := request(u.h, "GET", "/v1/uploads/"+id, p.ip, p.headers, ""); !strings.Contains(rec.Body.String(), `"status":"pending"`) {
		t.Fatalf("upload changed: %s", rec.Body.String())
	}
}

// Odd paths never reach another upload and never fail with a 500. The plain router is used: these paths
// aren't in the contract.
func TestUploadsOddPaths(t *testing.T) {
	u := newUploadsHarness(t)
	p := u.signUp("192.0.2.63:1")

	_, answer, _ := u.create(p, `{"files":[`+fileJSON("a.jpg", 1)+`]}`)
	id := answer.Results[0].Upload.ID

	for _, r := range []struct{ method, path string }{
		{"POST", "/v1/uploads/" + id + ":completeX"},
		{"POST", "/v1/uploads/" + id + ":cancel"},
		{"POST", "/v1/uploads/" + id + ":complete:complete"},
		{"GET", "/v1/uploads/" + id + ":complete"},
		{"DELETE", "/v1/uploads/" + id + ":complete"},
		{"POST", "/v1/uploads/" + id + "%3Acomplete"},
		{"GET", "/v1/uploads/" + id + "%00"},
		{"GET", "/v1/uploads/..%2F" + id},
		{"GET", "/v1/uploads/" + id + "/../" + id},
		{"GET", "/v1/uploads/" + strings.Repeat("a", 10_000)},
		{"POST", "/v1/uploads/:complete"},
		{"PUT", "/v1/uploads/" + id},
		{"PATCH", "/v1/uploads/" + id},
	} {
		rec := request(u.router, r.method, r.path, p.ip, p.headers, "")
		if rec.Code >= 500 || rec.Code < 400 {
			t.Errorf("%s %s: %d %s", r.method, r.path, rec.Code, rec.Body.String())
		}
	}

	if rec := request(u.h, "GET", "/v1/uploads/"+id, p.ip, p.headers, ""); !strings.Contains(rec.Body.String(), `"status":"pending"`) {
		t.Fatalf("upload changed: %s", rec.Body.String())
	}

	// The same id in capitals is the same upload.
	if rec := request(u.h, "GET", "/v1/uploads/"+strings.ToUpper(id), p.ip, p.headers, ""); rec.Code != http.StatusOK {
		t.Errorf("uppercase id: %d", rec.Code)
	}
}

// An upload belongs to whoever sent it: nobody else can read, complete or cancel it, not even a member of
// the same vault, and the answer is the same as for an id that doesn't exist.
func TestUploadsAreIsolated(t *testing.T) {
	u := newUploadsHarness(t)
	owner, editor, stranger := u.signUp("192.0.2.64:1"), u.signUp("192.0.2.65:1"), u.signUp("192.0.2.66:1")
	u.join(editor, owner.vault, "editor")

	_, answer, _ := u.create(owner, `{"files":[`+fileJSON("a.jpg", 1)+`]}`)
	id := answer.Results[0].Upload.ID
	u.bucket.put(uploads.Key(id), 1, "image/jpeg")

	for _, other := range []person{editor, stranger} {
		for _, r := range []struct{ method, path string }{
			{"GET", "/v1/uploads/" + id},
			{"POST", "/v1/uploads/" + id + ":complete"},
			{"DELETE", "/v1/uploads/" + id},
		} {
			rec := request(u.h, r.method, r.path, other.ip, other.headers, "")
			if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"upload-not-found"`) {
				t.Errorf("%s %s by someone else: %d %s", r.method, r.path, rec.Code, rec.Body.String())
			}
		}
	}

	if rec := request(u.h, "GET", "/v1/uploads/"+id, owner.ip, owner.headers, ""); !strings.Contains(rec.Body.String(), `"status":"pending"`) {
		t.Fatalf("upload changed: %s", rec.Body.String())
	}
}

func TestUploadsFollowVaultRoles(t *testing.T) {
	u := newUploadsHarness(t)
	owner, editor, viewer, former := u.signUp("192.0.2.67:1"), u.signUp("192.0.2.68:1"), u.signUp("192.0.2.69:1"), u.signUp("192.0.2.70:1")
	u.join(editor, owner.vault, "editor")
	u.join(viewer, owner.vault, "viewer")
	u.join(former, owner.vault, "editor")

	if _, err := u.pool.Exec(context.Background(), `UPDATE vaults SET storage_limit_bytes = 1000 WHERE id = $1`, owner.vault); err != nil {
		t.Fatal(err)
	}

	in := func(vault string, size int) string {
		return `{"vaultId":"` + vault + `","files":[` + fileJSON("a.jpg", size) + `]}`
	}

	// The editor's upload is charged to the vault it goes into, not to the editor's own.
	code, answer, raw := u.create(editor, in(owner.vault, 600))
	if code != http.StatusCreated || answer.Results[0].Status != "accepted" || answer.Quota.LimitBytes != 1000 || answer.Quota.ReservedBytes != 600 {
		t.Fatalf("editor: %d %s", code, raw)
	}

	if _, own, _ := u.create(editor, `{"files":[`+fileJSON("b.jpg", 1)+`]}`); own.Quota.ReservedBytes != 1 {
		t.Fatalf("the editor's own vault was charged: %+v", own.Quota)
	}

	// The owner sees the editor's reservation in the shared limit.
	if _, answer, raw := u.create(owner, in(strings.ToUpper(owner.vault), 500)); answer.Results[0].Code != uploads.QuotaExceeded {
		t.Fatalf("owner after the editor: %s", raw)
	}

	if code, _, raw := u.create(viewer, in(owner.vault, 1)); code != http.StatusForbidden || !strings.Contains(raw, `"vault-read-only"`) {
		t.Fatalf("viewer: %d %s", code, raw)
	}

	if _, err := u.pool.Exec(context.Background(), `DELETE FROM vault_members WHERE vault_id = $1 AND user_id = $2`, owner.vault, former.id); err != nil {
		t.Fatal(err)
	}

	if code, _, raw := u.create(former, in(owner.vault, 1)); code != http.StatusNotFound || !strings.Contains(raw, `"vault-not-found"`) {
		t.Fatalf("removed member: %d %s", code, raw)
	}

	if code, _, raw := u.create(owner, in("00000000-0000-0000-0000-000000000000", 1)); code != http.StatusNotFound {
		t.Fatalf("nil vault: %d %s", code, raw)
	}
}

func TestUploadsAreRateLimited(t *testing.T) {
	u := newUploadsHarness(t)
	p := u.signUp("192.0.2.71:1")

	for i := range 30 {
		if code, _, raw := u.create(p, `{"files":[`+fileJSON("a.jpg", 1)+`]}`); code != http.StatusCreated {
			t.Fatalf("request %d: %d %s", i+1, code, raw)
		}
	}

	code, _, raw := u.create(p, `{"files":[`+fileJSON("a.jpg", 1)+`]}`)
	if code != http.StatusTooManyRequests || !strings.Contains(raw, `"rate-limited"`) || !strings.Contains(raw, `"retryAfterMs"`) {
		t.Fatalf("31st request: %d %s", code, raw)
	}

	// The limit is the account's: another network doesn't reset it.
	elsewhere := p
	elsewhere.ip = "198.51.100.71:1"

	if code, _, raw := u.create(elsewhere, `{"files":[`+fileJSON("a.jpg", 1)+`]}`); code != http.StatusTooManyRequests {
		t.Fatalf("same account, other IP: %d %s", code, raw)
	}

	// And it is only that account's: someone else on the same network still keeps.
	other := u.signUp("192.0.2.72:1")
	other.ip = p.ip

	if code, _, raw := u.create(other, `{"files":[`+fileJSON("a.jpg", 1)+`]}`); code != http.StatusCreated {
		t.Fatalf("other account, same IP: %d %s", code, raw)
	}
}

// What processing leaves is served as the contract says: a ready upload names its moment, and asking to
// keep the same file again answers duplicate. (Processing itself is tested in the uploads package.)
func TestUploadsServeReadyAndDuplicate(t *testing.T) {
	ctx := context.Background()
	u := newUploadsHarness(t)
	p := u.signUp("192.0.2.73:1")

	_, created, raw := u.create(p, `{"files":[`+fileJSON("a.jpg", 2048)+`]}`)
	if len(created.Results) != 1 || created.Results[0].Status != "accepted" {
		t.Fatalf("create: %s", raw)
	}

	id := created.Results[0].Upload.ID

	momentID, err := moments.NewID(ctx, u.pool)
	if err != nil {
		t.Fatal(err)
	}

	if err := moments.Create(ctx, u.pool, moments.Moment{ID: momentID, VaultID: p.vault, CreatedBy: p.id, Kind: "photo", Name: "a.jpg", ContentType: "image/jpeg", SHA256: sum},
		[]moments.File{{Variant: "original", Key: moments.Key(p.vault, momentID, "original"), ContentType: "image/jpeg", Size: 2000}}); err != nil {
		t.Fatal(err)
	}

	if _, err := u.pool.Exec(ctx, `UPDATE uploads SET status = 'ready', moment_id = $2, completed_at = now() WHERE id = $1`, id, momentID); err != nil {
		t.Fatal(err)
	}

	rec := request(u.h, "GET", "/v1/uploads/"+id, p.ip, p.headers, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"ready"`) || !strings.Contains(rec.Body.String(), `"momentId":"`+momentID+`"`) {
		t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
	}

	code, again, raw := u.create(p, `{"files":[`+fileJSON("copy.jpg", 2048)+`,{"name":"b.jpg","size":10,"contentType":"image/jpeg","sha256":"`+strings.Repeat("cd", 32)+`"}]}`)
	if code != http.StatusCreated || again.Results[0].Status != "duplicate" || !strings.Contains(raw, `"momentId":"`+momentID+`"`) || again.Results[1].Status != "accepted" {
		t.Fatalf("again: %d %s", code, raw)
	}

	// Used is the moment's original; only the new file is reserved.
	if again.Quota.ReservedBytes != 10 || !strings.Contains(raw, `"usedBytes":2000`) {
		t.Fatalf("quota: %s", raw)
	}
}

// A failure is served with its code and a sentence for the person.
func TestUploadsServeFailures(t *testing.T) {
	ctx := context.Background()
	u := newUploadsHarness(t)
	p := u.signUp("192.0.2.74:1")

	for _, code := range []string{uploads.Mismatch, uploads.Unreadable, uploads.Revoked, uploads.Internal} {
		_, created, raw := u.create(p, `{"files":[`+fileJSON("a.jpg", 10)+`]}`)
		if len(created.Results) != 1 {
			t.Fatalf("create: %s", raw)
		}

		id := created.Results[0].Upload.ID
		if _, err := u.pool.Exec(ctx, `UPDATE uploads SET status = 'failed', failure_code = $2, completed_at = now() WHERE id = $1`, id, code); err != nil {
			t.Fatal(err)
		}

		rec := request(u.h, "GET", "/v1/uploads/"+id, p.ip, p.headers, "")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"code":"`+code+`"`) || strings.Contains(rec.Body.String(), `"detail":""`) {
			t.Fatalf("%s: %d %s", code, rec.Code, rec.Body.String())
		}
	}
}
