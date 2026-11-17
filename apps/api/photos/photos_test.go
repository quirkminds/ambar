package photos

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Fixtures are made at test time by libvips (the same encoder family a phone or editor uses), then given
// real metadata: GPS, author, XMP, comments, and data hidden after the end of the picture. Each test first
// checks that libvips reads that metadata in the fixture, so a strip that "passes" can't be a fixture that
// never had any.

func needVips(t *testing.T) {
	t.Helper()

	if err := CheckTools(context.Background()); err != nil {
		t.Skipf("libvips: %v", err)
	}
}

// secretTIFF is EXIF with the orientation, an author and a GPS position.
func secretTIFF(orient uint16) []byte {
	le := binary.LittleEndian
	artist := []byte("Jane Secret\x00")

	// IFD0 at 8 (3 entries, ends at 50): Orientation, Artist (-> 50), GPS IFD (-> 62).
	// GPS IFD at 62 (2 entries, ends at 92): LatitudeRef, Latitude (-> 92).
	b := []byte{'I', 'I', 42, 0, 8, 0, 0, 0}
	entry := func(tag, kind uint16, count, value uint32) {
		b = le.AppendUint16(b, tag)
		b = le.AppendUint16(b, kind)
		b = le.AppendUint32(b, count)
		b = le.AppendUint32(b, value)
	}

	b = le.AppendUint16(b, 3)
	entry(0x0112, 3, 1, uint32(orient))
	entry(0x013B, 2, uint32(len(artist)), 50)
	entry(0x8825, 4, 1, 62)
	b = le.AppendUint32(b, 0)
	b = append(b, artist...)

	b = le.AppendUint16(b, 2)
	entry(0x0001, 2, 2, uint32('N'))
	entry(0x0002, 5, 3, 92)
	b = le.AppendUint32(b, 0)

	for _, v := range []uint32{40, 1, 25, 1, 1234, 100} { // 40° 25' 12.34"
		b = le.AppendUint32(b, v)
	}

	return b
}

const secretXMP = `<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"><rdf:Description xmlns:dc="http://purl.org/dc/elements/1.1/" dc:creator="Jane Secret"/></rdf:RDF></x:xmpmeta>`

// vips runs a libvips tool for the tests themselves: making fixtures and reading results. Unlike run, it
// allows libvips' own .v format, which VIPS_BLOCK_UNTRUSTED blocks in processing.
func vips(t *testing.T, args ...string) string {
	t.Helper()

	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}

	return string(out)
}

// base is a noisy 640×480 picture in the given format.
func base(t *testing.T, dir, ext string) string {
	noise := filepath.Join(dir, "noise.v")
	if _, err := os.Stat(noise); err != nil {
		vips(t, "vips", "gaussnoise", noise, "640", "480")
		vips(t, "vips", "cast", noise, filepath.Join(dir, "noise8.v"), "uchar")
		vips(t, "vips", "bandjoin", filepath.Join(dir, "noise8.v")+" "+filepath.Join(dir, "noise8.v")+" "+filepath.Join(dir, "noise8.v"), noise+"rgb.v")
	}

	out := filepath.Join(dir, "base"+ext)
	vips(t, "vips", "copy", noise+"rgb.v", out)

	return out
}

func jpegWithSecrets(t *testing.T, dir string) string {
	src, err := os.ReadFile(base(t, dir, ".jpg"))
	if err != nil {
		t.Fatal(err)
	}

	xmp := append([]byte("http://ns.adobe.com/xap/1.0/\x00"), secretXMP...)
	seg := func(marker byte, body []byte) []byte {
		s := []byte{0xFF, marker, 0, 0}
		binary.BigEndian.PutUint16(s[2:], uint16(len(body)+2))

		return append(s, body...)
	}

	var b bytes.Buffer
	b.Write(src[:2])
	b.Write(app1(secretTIFF(6)))
	b.Write(seg(0xE1, xmp))
	b.Write(seg(0xED, []byte("Photoshop 3.0\x00Jane Secret")))
	b.Write(seg(0xFE, []byte("shot at Jane Secret's house")))
	b.Write(src[2:])
	b.WriteString("TRAILER Jane Secret GPS 40.42") // after EOI, like a motion photo

	path := filepath.Join(dir, "secrets.jpg")
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

func chunk(kind string, data []byte) []byte {
	var b bytes.Buffer
	writeChunk(&b, kind, data)

	return b.Bytes()
}

func pngWithSecrets(t *testing.T, dir string) string {
	src, err := os.ReadFile(base(t, dir, ".png"))
	if err != nil {
		t.Fatal(err)
	}

	ihdrEnd := 8 + 8 + 13 + 4

	var b bytes.Buffer
	b.Write(src[:ihdrEnd])
	b.Write(chunk("eXIf", secretTIFF(6)))
	b.Write(chunk("tEXt", []byte("Author\x00Jane Secret")))
	b.Write(chunk("iTXt", append([]byte("XML:com.adobe.xmp\x00\x00\x00\x00\x00"), secretXMP...)))
	b.Write(chunk("tIME", []byte{0x07, 0xEA, 10, 21, 12, 0, 0}))
	b.Write(src[ihdrEnd:])
	b.WriteString("TRAILER Jane Secret")

	path := filepath.Join(dir, "secrets.png")
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

func webpWithSecrets(t *testing.T, dir string) string {
	path := base(t, dir, ".webp")
	src := mustRead(t, path)

	// Keep libvips' VP8 picture chunk; rebuild the file around it with our EXIF and XMP.
	f, _ := os.Open(path)
	chunks, err := webpChunks(f, int64(len(src)))
	_ = f.Close()

	if err != nil {
		t.Fatal(err)
	}

	var vp8 []byte

	for _, c := range chunks {
		if c.kind == "VP8 " {
			vp8 = src[c.offset-8 : c.offset+c.size+c.size%2]
		}
	}

	if vp8 == nil {
		t.Fatal("no VP8 chunk")
	}

	vp8x := make([]byte, 10)
	vp8x[0] = 0x08 | 0x04
	vp8x[4], vp8x[5] = 0x7f, 0x02 // 640 - 1
	vp8x[7], vp8x[8] = 0xdf, 0x01 // 480 - 1

	var body bytes.Buffer
	body.WriteString("WEBP")
	writeRIFF(&body, "VP8X", vp8x)
	body.Write(vp8)
	writeRIFF(&body, "EXIF", secretTIFF(6))
	writeRIFF(&body, "XMP ", []byte(secretXMP))

	var b bytes.Buffer
	b.WriteString("RIFF")
	b.Write(binary.LittleEndian.AppendUint32(nil, uint32(body.Len())))
	b.Write(body.Bytes())
	b.WriteString("TRAILER Jane Secret")

	path = filepath.Join(dir, "secrets.webp")
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

func heicWithSecrets(t *testing.T, dir string) string {
	path := filepath.Join(dir, "secrets.heic")

	// libvips carries the JPEG's EXIF and XMP into the HEIC it writes, as a phone does.
	if out, err := exec.Command("vips", "copy", jpegWithSecrets(t, dir), path).CombinedOutput(); err != nil {
		t.Skipf("this libvips can't write HEIC: %v %s", err, out)
	}

	return path
}

// header is everything libvips reads about a file: its fields and metadata.
func header(t *testing.T, path string) string {
	return vips(t, "vipsheader", "-a", path)
}

// pixels decodes a file to raw pixels (no header, no metadata), to compare pictures exactly.
func pixels(t *testing.T, path string) []byte {
	out := path + ".raw"
	vips(t, "vips", "rawsave", path, out)

	return mustRead(t, out)
}

func TestStripRemovesMetadataAndKeepsThePicture(t *testing.T) {
	needVips(t)

	for name, make := range map[string]func(*testing.T, string) string{
		JPEG: jpegWithSecrets,
		PNG:  pngWithSecrets,
		WebP: webpWithSecrets,
		HEIC: heicWithSecrets,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			in := make(t, dir)
			out := filepath.Join(dir, "clean")

			before, err := os.ReadFile(in)
			if err != nil {
				t.Fatal(err)
			}

			if !strings.Contains(header(t, in), "Jane Secret") {
				t.Fatalf("the fixture has no metadata to strip:\n%s", header(t, in))
			}

			head := before[:32]
			if got := Sniff(head); got != name {
				t.Fatalf("sniffed %q", got)
			}

			if err := Strip(in, out, name); err != nil {
				t.Fatal(err)
			}

			after, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}

			fields := header(t, out)
			for _, secret := range []string{"Jane Secret", "GPS", "40.42", "Artist", "iptc-data"} {
				if strings.Contains(fields, secret) {
					t.Errorf("%q is still there:\n%s", secret, fields)
				}
			}

			// HEIC keeps its metadata items, emptied in place: what they say is what matters.
			for _, field := range []string{"xmp-data", "exif-data"} {
				if blob := metadataBlob(t, out, field); bytes.Contains(blob, []byte("Jane")) || bytes.Contains(blob, []byte("creator")) {
					t.Errorf("%s still says: %q", field, blob)
				}
			}

			// No trace in the raw bytes either: a reader libvips doesn't know about mustn't find it.
			for _, secret := range []string{"Jane Secret", "TRAILER"} {
				if bytes.Contains(after, []byte(secret)) {
					t.Errorf("raw bytes still contain %q", secret)
				}
			}

			if !bytes.Equal(pixels(t, in), pixels(t, out)) {
				t.Error("the picture changed")
			}

			if name != HEIC {
				if got := vips(t, "vipsheader", "-f", "orientation", out); strings.TrimSpace(got) != "6" {
					t.Errorf("orientation %q, want 6", got)
				}
			}

			if Sniff(after[:32]) != name {
				t.Error("the clean file isn't the same type any more")
			}
		})
	}
}

// A photo with no metadata comes out unchanged in what matters.
func TestStripPlainPhotos(t *testing.T) {
	needVips(t)

	for _, ext := range []string{".jpg", ".png", ".webp"} {
		dir := t.TempDir()
		in := base(t, dir, ext)

		head, _ := os.ReadFile(in)
		kind := Sniff(head[:32])
		out := filepath.Join(dir, "clean")

		if err := Strip(in, out, kind); err != nil {
			t.Fatalf("%s: %v", ext, err)
		}

		if !bytes.Equal(pixels(t, in), pixels(t, out)) {
			t.Errorf("%s: picture changed", ext)
		}

		if fields := header(t, out); strings.Contains(fields, "orientation: ") && !strings.Contains(fields, "orientation: 1") {
			t.Errorf("%s: an orientation appeared:\n%s", ext, fields)
		}
	}
}

func TestStripRefusesBrokenFiles(t *testing.T) {
	needVips(t)
	dir := t.TempDir()

	for name, make := range map[string]func(*testing.T, string) string{JPEG: jpegWithSecrets, PNG: pngWithSecrets, WebP: webpWithSecrets} {
		in := make(t, dir)
		data, _ := os.ReadFile(in)

		// Cut in the middle of the picture data.
		truncated := filepath.Join(dir, "truncated")
		if err := os.WriteFile(truncated, data[:len(data)/2], 0o600); err != nil {
			t.Fatal(err)
		}

		if err := Strip(truncated, filepath.Join(dir, "out"), name); !errors.Is(err, ErrUnreadable) {
			t.Errorf("%s truncated: %v", name, err)
		}

		// Garbage with the right magic bytes.
		garbage := filepath.Join(dir, "garbage")
		if err := os.WriteFile(garbage, append(data[:16:16], bytes.Repeat([]byte{0xFF}, 5000)...), 0o600); err != nil {
			t.Fatal(err)
		}

		if err := Strip(garbage, filepath.Join(dir, "out"), name); !errors.Is(err, ErrUnreadable) {
			t.Errorf("%s garbage: %v", name, err)
		}
	}
}

func TestSniff(t *testing.T) {
	cases := map[string]string{
		"\xFF\xD8\xFF\xE0\x00\x10JFIF\x00":                               JPEG,
		"\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR":                          PNG,
		"RIFF\x10\x00\x00\x00WEBPVP8 ":                                   WebP,
		"\x00\x00\x00\x18ftypheic\x00\x00\x00\x00mif1heic":               HEIC,
		"\x00\x00\x00\x18ftypmif1\x00\x00\x00\x00mif1heic":               HEIC,
		"\x00\x00\x00\x1cftypavif\x00\x00\x00\x00avifmif1miaf":           "",
		"\x00\x00\x00\x18ftypisom\x00\x00\x00\x00isommp41":               "", // an MP4
		"<svg xmlns=\"http://www.w3.org/2000/svg\">":                     "",
		"<!doctype html><html><body>":                                    "",
		"GIF89a\x01\x00\x01\x00\x80\x00\x00\x00\x00\x00\xff\xff\xff\x00": "",
		"%PDF-1.7\n%\xe2\xe3\xcf\xd3\n1 0 obj":                           "",
	}

	for head, want := range cases {
		padded := append([]byte(head), make([]byte, 32)...)
		if got := Sniff(padded); got != want {
			t.Errorf("%q: %q, want %q", head[:12], got, want)
		}
	}

	if !SameType(HEIF, HEIC) || SameType(PNG, JPEG) || SameType(JPEG, "") {
		t.Error("SameType")
	}
}

func TestBoundsRefusesBombs(t *testing.T) {
	needVips(t)
	dir := t.TempDir()

	// A 30 000 × 30 000 PNG of one colour is a few hundred kilobytes and 900 MP to decode.
	bomb := filepath.Join(dir, "bomb.png")
	vips(t, "vips", "black", bomb, "30000", "30000")

	if _, _, err := Bounds(bomb, PNG); !errors.Is(err, ErrUnreadable) {
		t.Fatalf("PNG bomb: %v", err)
	}

	// The same lie in a WebP header: VP8X declares a 16384 × 16384 canvas.
	src, _ := os.ReadFile(webpWithSecrets(t, dir))
	src[12+8+4], src[12+8+5], src[12+8+6] = 0xff, 0x3f, 0
	src[12+8+7], src[12+8+8], src[12+8+9] = 0xff, 0x3f, 0
	lie := filepath.Join(dir, "lie.webp")
	_ = os.WriteFile(lie, src, 0o600)

	if _, _, err := Bounds(lie, WebP); !errors.Is(err, ErrUnreadable) {
		t.Fatalf("WebP bomb: %v", err)
	}

	for _, ext := range []string{".jpg", ".png", ".webp"} {
		path := base(t, t.TempDir(), ext)
		head, _ := os.ReadFile(path)

		if w, h, err := Bounds(path, Sniff(head[:32])); err != nil || w != 640 || h != 480 {
			t.Errorf("%s: %d×%d %v", ext, w, h, err)
		}
	}
}

func TestThumbnails(t *testing.T) {
	needVips(t)
	dir := t.TempDir()
	in := jpegWithSecrets(t, dir)
	clean := filepath.Join(dir, "clean.jpg")

	if err := Strip(in, clean, JPEG); err != nil {
		t.Fatal(err)
	}

	variants, err := Thumbnails(context.Background(), clean, JPEG, dir)
	if err != nil {
		t.Fatal(err)
	}

	// 640 × 480 with orientation 6 is shown 480 × 640: the copies are already turned.
	want := map[string][2]int{"small": {360, 480}, "large": {480, 640}}

	for _, v := range variants {
		if got := [2]int{v.Width, v.Height}; got != want[v.Name] {
			t.Errorf("%s: %v, want %v", v.Name, got, want[v.Name])
		}

		if fields := header(t, v.Path); strings.Contains(fields, "exif") || strings.Contains(fields, "Jane") {
			t.Errorf("%s carries metadata:\n%s", v.Name, fields)
		}
	}

	if len(variants) != 2 {
		t.Fatalf("%d variants", len(variants))
	}
}

func TestThumbnailsOfHEICIncludeADisplayCopy(t *testing.T) {
	needVips(t)
	dir := t.TempDir()
	in := heicWithSecrets(t, dir)

	variants, err := Thumbnails(context.Background(), in, HEIC, dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(variants) != 3 || variants[2].Name != "display" || Sniff(mustRead(t, variants[2].Path)[:32]) != WebP {
		t.Fatalf("%+v", variants)
	}
}

// metadataBlob is a metadata field's raw bytes as libvips reads them, or nil when the file has none.
func metadataBlob(t *testing.T, path, field string) []byte {
	out, err := exec.Command("vipsheader", "-f", field, path).Output()
	if err != nil {
		return nil
	}

	blob, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("%s: %v", field, err)
	}

	return blob
}

func mustRead(t *testing.T, path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	return b
}
