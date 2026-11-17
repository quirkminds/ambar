package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/joho/godotenv"
)

// Attacks on presigned uploads against the real bucket in R2_* (skipped with -short or without R2).

func testClient(t *testing.T) *Client {
	t.Helper()

	if testing.Short() {
		t.Skip("needs R2")
	}

	_ = godotenv.Load("../.env.local")

	endpoint, key, secret, bucket := os.Getenv("R2_ENDPOINT"), os.Getenv("R2_ACCESS_KEY_ID"), os.Getenv("R2_SECRET_ACCESS_KEY"), os.Getenv("R2_BUCKET")
	if endpoint == "" || key == "" || secret == "" || bucket == "" {
		t.Skip("R2_* not set")
	}

	return New(endpoint, key, secret, bucket)
}

func testKey(t *testing.T, c *Client) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	key := "incoming/test-" + hex.EncodeToString(b)

	t.Cleanup(func() { _ = c.Delete(context.Background(), key) })

	return key
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)

	return b
}

func sha(b []byte) string {
	s := sha256.Sum256(b)

	return hex.EncodeToString(s[:])
}

func put(t *testing.T, url string, headers map[string]string, body []byte) int {
	t.Helper()

	req, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()

	return res.StatusCode
}

// The declared bytes are the only ones a signed PUT takes: other bytes of the same size and type are
// refused, before and after a good upload (no swapping the file once Complete has checked it).
func TestSignedPutTakesOnlyTheDeclaredBytes(t *testing.T) {
	ctx := context.Background()
	c := testClient(t)
	key := testKey(t, c)
	good, evil := randomBytes(2000), randomBytes(2000)

	url, headers, err := c.PresignPut(ctx, key, "image/jpeg", int64(len(good)), sha(good), time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	if status := put(t, url, headers, evil); status < 400 {
		t.Fatalf("other bytes of the same size were taken: %d", status)
	}

	if status := put(t, url, headers, good); status != http.StatusOK {
		t.Fatalf("the declared bytes: %d", status)
	}

	if status := put(t, url, headers, evil); status < 400 {
		t.Fatalf("the file was swapped after upload: %d", status)
	}

	object, err := c.Head(ctx, key)
	if err != nil || object.Size != int64(len(good)) || object.SHA256 != sha(good) {
		t.Fatalf("HEAD: %+v %v", object, err)
	}
}

func TestSignedPutRefusesTampering(t *testing.T) {
	ctx := context.Background()
	c := testClient(t)
	body := randomBytes(500)

	t.Run("expired", func(t *testing.T) {
		key := testKey(t, c)

		url, headers, err := c.PresignPut(ctx, key, "image/jpeg", int64(len(body)), sha(body), time.Second)
		if err != nil {
			t.Fatal(err)
		}

		time.Sleep(2 * time.Second)

		if status := put(t, url, headers, body); status != http.StatusForbidden {
			t.Fatalf("expired PUT: %d", status)
		}
	})

	t.Run("another key", func(t *testing.T) {
		key := testKey(t, c)

		url, headers, err := c.PresignPut(ctx, key, "image/jpeg", int64(len(body)), sha(body), time.Minute)
		if err != nil {
			t.Fatal(err)
		}

		other := url[:len("https://")] + url[len("https://"):]
		other = replaceOnce(other, "incoming/test-", "incoming/evil-")

		if status := put(t, other, headers, body); status != http.StatusForbidden {
			t.Fatalf("PUT to another key: %d", status)
		}
	})

	t.Run("unsigned headers", func(t *testing.T) {
		for name, value := range map[string]string{
			"x-amz-meta-owner":    "someone-else",
			"x-amz-acl":           "public-read",
			"Content-Disposition": "inline; filename=page.html",
			"Cache-Control":       "public, max-age=31536000",
			"Content-Encoding":    "gzip",
			"Content-Language":    "en",
			"Expires":             "Wed, 21 Oct 2099 07:28:00 GMT",
		} {
			key := testKey(t, c)

			url, headers, err := c.PresignPut(ctx, key, "image/jpeg", int64(len(body)), sha(body), time.Minute)
			if err != nil {
				t.Fatal(err)
			}

			extra := map[string]string{name: value}
			for k, v := range headers {
				extra[k] = v
			}

			// R2 may take the PUT; then Head must say the object carries something we never signed.
			if status := put(t, url, extra, body); status == http.StatusOK {
				object, err := c.Head(ctx, key)
				if err != nil {
					t.Fatal(err)
				}

				if len(object.Unsigned) == 0 {
					t.Errorf("%s was taken and Head didn't report it", name)
				}

				t.Logf("%s: taken, reported as %v", name, object.Unsigned)
			} else {
				t.Logf("%s: refused (%d)", name, status)
			}
		}
	})

	t.Run("deleting a missing object succeeds", func(t *testing.T) {
		if err := c.Delete(ctx, testKey(t, c)); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("head of a missing object is not found", func(t *testing.T) {
		if _, err := c.Head(ctx, testKey(t, c)); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
	})
}

func replaceOnce(s, old, replacement string) string { return strings.Replace(s, old, replacement, 1) }
