// Package photos is what the API does to a photo before keeping it: tell its real type from its bytes,
// read its size without decoding it, strip its metadata without re-encoding it, and make the copies that
// are only shown (with libvips' command-line tools). It never touches the network; uploads moves the
// bytes.
package photos

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	_ "image/jpeg" // DecodeConfig
	_ "image/png"  // DecodeConfig
	"os"
)

// MaxPixels is the largest photo kept (100 MP, more than any phone or camera). A larger header is a
// decompression bomb: a few kilobytes that would need gigabytes to decode.
const MaxPixels = 100_000_000

// ErrUnreadable: the bytes aren't a photo we can read (truncated, corrupt, a bomb, or a layout we refuse
// to guess at). The upload fails with unreadable-file.
var ErrUnreadable = errors.New("photos: unreadable")

const (
	JPEG = "image/jpeg"
	PNG  = "image/png"
	WebP = "image/webp"
	HEIC = "image/heic"
	HEIF = "image/heif"
)

// Sniff is the type of a photo from its first bytes (at least 32), or "" when it isn't one we keep.
func Sniff(head []byte) string {
	switch {
	case bytes.HasPrefix(head, []byte{0xFF, 0xD8, 0xFF}):
		return JPEG
	case bytes.HasPrefix(head, []byte("\x89PNG\r\n\x1a\n")):
		return PNG
	case len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "WEBP":
		return WebP
	case len(head) >= 16 && string(head[4:8]) == "ftyp":
		return heifBrand(head)
	}

	return ""
}

// heifBrand: HEIC is HEIF with HEVC pictures. AVIF and other HEIF flavours aren't kept.
func heifBrand(head []byte) string {
	size := int(binary.BigEndian.Uint32(head))
	if size < 16 || size > len(head) {
		size = len(head)
	}

	brands := []string{string(head[8:12])}
	for i := 16; i+4 <= size; i += 4 {
		brands = append(brands, string(head[i:i+4]))
	}

	for _, b := range brands {
		switch b {
		case "heic", "heix", "heim", "heis", "hevc", "hevx":
			return HEIC
		}
	}

	return ""
}

// SameType: what the client declared is what the bytes are. HEIC and HEIF name the same files.
func SameType(declared, sniffed string) bool {
	if sniffed == HEIC {
		return declared == HEIC || declared == HEIF
	}

	return sniffed != "" && declared == sniffed
}

// Bounds reads a photo's width and height from its header, without decoding it, and refuses bombs.
func Bounds(path, contentType string) (width, height int, err error) {
	switch contentType {
	case JPEG, PNG:
		f, err := os.Open(path)
		if err != nil {
			return 0, 0, err
		}
		defer f.Close()

		config, _, err := image.DecodeConfig(f)
		if err != nil {
			return 0, 0, ErrUnreadable
		}

		width, height = config.Width, config.Height
	case WebP:
		width, height, err = webpBounds(path)
	case HEIC, HEIF:
		width, height, err = heifBounds(path)
	default:
		return 0, 0, ErrUnreadable
	}

	if err != nil {
		return 0, 0, err
	}

	if width < 1 || height < 1 || int64(width)*int64(height) > MaxPixels {
		return 0, 0, ErrUnreadable
	}

	return width, height, nil
}
