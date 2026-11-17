package photos

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// toolBudget bounds every libvips call: a photo that takes longer is treated as unreadable.
const toolBudget = 60 * time.Second

// ErrTools: libvips' command-line tools are missing, or can't read HEIC or write WebP.
var ErrTools = errors.New("photos: libvips tools unavailable")

// Variant is one copy made to be shown. Name is the moment file variant ("small", "large", "display").
type Variant struct {
	Name          string
	Path          string
	Width, Height int
	Size          int64
}

// Sizes of the copies: the longest side, in pixels.
var sizes = []struct {
	name string
	px   int
}{{"small", 480}, {"large", 1200}}

// displayPx: HEIC also gets a large copy browsers can show, since most can't decode HEIC.
const displayPx = 2560

// CheckTools confirms libvips is installed with what processing needs.
func CheckTools(ctx context.Context) error {
	for _, tool := range []string{"vips", "vipsthumbnail", "vipsheader"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("%w: %s not found", ErrTools, tool)
		}
	}

	out, err := run(ctx, "vips", "-l")
	if err != nil {
		return fmt.Errorf("%w: vips -l: %v", ErrTools, err)
	}

	for _, op := range []string{"heifload", "webpsave", "jpegload", "pngload", "webpload"} {
		if !strings.Contains(out, op) {
			return fmt.Errorf("%w: no %s", ErrTools, op)
		}
	}

	return nil
}

// Strip writes the photo at in to out without its metadata, never re-encoding the picture.
func Strip(in, out, contentType string) error {
	switch contentType {
	case HEIC, HEIF:
		if err := copyFile(in, out); err != nil {
			return err
		}

		return stripHEIF(out)
	}

	src, err := os.Open(in)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.Create(out)
	if err != nil {
		return err
	}
	defer dst.Close()

	switch contentType {
	case JPEG:
		err = stripJPEG(src, dst)
	case PNG:
		err = stripPNG(src, dst)
	case WebP:
		err = stripWebP(src, dst)
	default:
		err = ErrUnreadable
	}

	if err != nil {
		return err
	}

	return dst.Close()
}

// Thumbnails makes the copies of the photo at path that are shown, as WebP files in dir. libvips turns
// the picture the right way up and writes no metadata.
func Thumbnails(ctx context.Context, path, contentType, dir string) ([]Variant, error) {
	targets := sizes
	if contentType == HEIC || contentType == HEIF {
		targets = append(targets, struct {
			name string
			px   int
		}{"display", displayPx})
	}

	variants := make([]Variant, 0, len(targets))

	for _, t := range targets {
		out := filepath.Join(dir, t.name+".webp")

		// --size WxH> fits inside the box and never enlarges a smaller photo.
		if _, err := run(ctx, "vipsthumbnail", path, "--size", fmt.Sprintf("%dx%d>", t.px, t.px), "-o", out+"[Q=82,strip]"); err != nil {
			return nil, err
		}

		width, height, err := webpBounds(out)
		if err != nil {
			return nil, err
		}

		info, err := os.Stat(out)
		if err != nil {
			return nil, err
		}

		variants = append(variants, Variant{Name: t.name, Path: out, Width: width, Height: height, Size: info.Size()})
	}

	return variants, nil
}

// run executes a libvips tool without a shell. VIPS_BLOCK_UNTRUSTED keeps libvips to its audited
// loaders, whatever the bytes claim to be. A tool that fails or runs out of time makes the photo
// unreadable; its output is never shown to users.
func run(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, toolBudget)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.TempDir(), "VIPS_BLOCK_UNTRUSTED=1", "VIPS_CONCURRENCY=1"}

	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("%s: %w", name, ctx.Err())
		}

		return "", fmt.Errorf("%w: %s failed", ErrUnreadable, name)
	}

	return stdout.String(), nil
}

func copyFile(from, to string) error {
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.Create(to)
	if err != nil {
		return err
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		return err
	}

	return dst.Close()
}
