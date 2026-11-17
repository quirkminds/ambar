package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// Processing's own transfers, against the real bucket: Put stores a file with its checksum, Get reads it
// back and never yields more than asked for.
func TestPutAndGetAgainstR2(t *testing.T) {
	ctx := context.Background()
	c := testClient(t)
	key := testKey(t, c)

	body := randomBytes(300_000)
	path := filepath.Join(t.TempDir(), "file")

	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := c.Put(ctx, key, "image/webp", path); err != nil {
		t.Fatal(err)
	}

	object, err := c.Head(ctx, key)
	if err != nil || object.Size != int64(len(body)) || object.ContentType != "image/webp" || object.SHA256 != sha(body) {
		t.Fatalf("head: %+v %v", object, err)
	}

	read := func(max int64) []byte {
		r, err := c.Get(ctx, key, max)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()

		b, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}

		return b
	}

	if got := read(int64(len(body))); !bytes.Equal(got, body) {
		t.Fatalf("read %d bytes back", len(got))
	}

	// Asked for less than is there: one byte more than the limit, so the caller can tell.
	if got := read(1000); len(got) != 1001 || !bytes.Equal(got, body[:1001]) {
		t.Fatalf("limited read: %d bytes", len(got))
	}

	if _, err := c.Get(ctx, key+"-missing", 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}
