package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/joho/godotenv"
)

// Against the real bucket in R2_* (the development one in .env.local). Skipped with -short or without
// R2 configured, so CI never needs credentials.
func TestPresignedUploadAgainstR2(t *testing.T) {
	if testing.Short() {
		t.Skip("needs R2")
	}

	_ = godotenv.Load("../.env.local")

	endpoint, key, secret, bucket := os.Getenv("R2_ENDPOINT"), os.Getenv("R2_ACCESS_KEY_ID"), os.Getenv("R2_SECRET_ACCESS_KEY"), os.Getenv("R2_BUCKET")
	if endpoint == "" || key == "" || secret == "" || bucket == "" {
		t.Skip("R2_* not set")
	}

	ctx := context.Background()
	client := New(endpoint, key, secret, bucket)
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	objectKey := "incoming/test-" + hex.EncodeToString(b)
	body := bytes.Repeat([]byte("a"), 1000)

	t.Cleanup(func() { _ = client.Delete(context.Background(), objectKey) })

	url, headers, err := client.PresignPut(ctx, objectKey, "image/jpeg", int64(len(body)), sha(body), time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	put := func(body []byte) int {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
		for k, v := range headers {
			req.Header.Set(k, v)
		}

		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}

		_ = res.Body.Close()

		return res.StatusCode
	}

	// A body of another size than the one signed is refused.
	if status := put(body[:999]); status != http.StatusForbidden {
		t.Fatalf("PUT of the wrong size: %d", status)
	}

	if _, err := client.Head(ctx, objectKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("HEAD before upload: %v", err)
	}

	if status := put(body); status != http.StatusOK {
		t.Fatalf("PUT: %d", status)
	}

	object, err := client.Head(ctx, objectKey)
	if err != nil || object.Size != 1000 || object.ContentType != "image/jpeg" {
		t.Fatalf("HEAD: %+v %v", object, err)
	}

	if err := client.Delete(ctx, objectKey); err != nil {
		t.Fatal(err)
	}

	if _, err := client.Head(ctx, objectKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("HEAD after delete: %v", err)
	}
}
