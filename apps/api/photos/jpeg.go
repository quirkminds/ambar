package photos

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
)

// stripJPEG copies a JPEG without its metadata and without touching the compressed picture:
//   - drops APP1 (EXIF, XMP), APP13 (IPTC), COM and every other APPn but APP0 (JFIF), APP2 ICC profiles
//     and APP14 (Adobe colour transform), which change how the picture looks;
//   - keeps the orientation as an EXIF block with that tag alone;
//   - stops at the end-of-image marker: anything appended after it (multi-picture thumbnails, motion
//     photos, vendor trailers) can carry its own metadata.
func stripJPEG(in io.Reader, out io.Writer) error {
	r := bufio.NewReader(in)
	w := bufio.NewWriter(out)

	soi := make([]byte, 2)
	if _, err := io.ReadFull(r, soi); err != nil || soi[0] != 0xFF || soi[1] != 0xD8 {
		return ErrUnreadable
	}

	// Header segments up to the first scan are small; keep the ones we want, then write them after the
	// orientation is known.
	var (
		kept   [][]byte
		app0   []byte
		orient uint16 = 1
	)

	for {
		marker, err := nextMarker(r)
		if err != nil {
			return err
		}

		if marker == 0xD9 { // EOI before any scan
			return ErrUnreadable
		}

		segment, err := readSegment(r, marker)
		if err != nil {
			return err
		}

		body := segment[4:]

		switch {
		case marker == 0xE0 && app0 == nil:
			app0 = segment
		case marker == 0xE1:
			if bytes.HasPrefix(body, []byte("Exif\x00\x00")) && orient == 1 {
				orient = orientation(body[6:])
			}
		case marker == 0xE2 && bytes.HasPrefix(body, []byte("ICC_PROFILE\x00")),
			marker == 0xEE && bytes.HasPrefix(body, []byte("Adobe")):
			kept = append(kept, segment)
		case marker >= 0xE0 && marker <= 0xEF, marker == 0xFE:
			// Other application data and comments: dropped.
		default:
			kept = append(kept, segment)
		}

		if marker == 0xDA { // start of scan: the header is complete
			break
		}
	}

	_, _ = w.Write([]byte{0xFF, 0xD8})

	if app0 != nil {
		_, _ = w.Write(app0)
	}

	if orient != 1 {
		_, _ = w.Write(app1(orientationTIFF(orient)))
	}

	for _, s := range kept {
		_, _ = w.Write(s)
	}

	if err := copyScans(r, w); err != nil {
		return err
	}

	return w.Flush()
}

// copyScans copies entropy-coded data and the segments between scans (progressive JPEGs have several)
// up to and including EOI. The SOS segment itself was already written.
func copyScans(r *bufio.Reader, w *bufio.Writer) error {
	for {
		b, err := r.ReadByte()
		if err != nil {
			return ErrUnreadable // no EOI: truncated
		}

		if b != 0xFF {
			_ = w.WriteByte(b)

			continue
		}

		next, err := r.ReadByte()
		if err != nil {
			return ErrUnreadable
		}

		for next == 0xFF { // fill bytes
			next, err = r.ReadByte()
			if err != nil {
				return ErrUnreadable
			}
		}

		switch {
		case next == 0x00, next >= 0xD0 && next <= 0xD7: // stuffed byte, restart marker
			_, _ = w.Write([]byte{0xFF, next})
		case next == 0xD9:
			_, _ = w.Write([]byte{0xFF, 0xD9})

			return nil
		case next >= 0xE0 && next <= 0xEF, next == 0xFE:
			// Metadata between scans is as private as before them.
			if _, err := readSegment(r, next); err != nil {
				return err
			}
		default:
			segment, err := readSegment(r, next)
			if err != nil {
				return err
			}

			_, _ = w.Write(segment)
		}
	}
}

func nextMarker(r *bufio.Reader) (byte, error) {
	b, err := r.ReadByte()
	if err != nil || b != 0xFF {
		return 0, ErrUnreadable
	}

	for {
		m, err := r.ReadByte()
		if err != nil {
			return 0, ErrUnreadable
		}

		if m != 0xFF {
			return m, nil
		}
	}
}

// readSegment reads a marker segment's length and body, returning it whole (FF, marker, length, body).
func readSegment(r *bufio.Reader, marker byte) ([]byte, error) {
	size := make([]byte, 2)
	if _, err := io.ReadFull(r, size); err != nil {
		return nil, ErrUnreadable
	}

	n := int(binary.BigEndian.Uint16(size))
	if n < 2 {
		return nil, ErrUnreadable
	}

	segment := make([]byte, 4+n-2)
	segment[0], segment[1], segment[2], segment[3] = 0xFF, marker, size[0], size[1]

	if _, err := io.ReadFull(r, segment[4:]); err != nil {
		return nil, ErrUnreadable
	}

	return segment, nil
}

func app1(tiff []byte) []byte {
	body := append([]byte("Exif\x00\x00"), tiff...)
	segment := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(segment[2:], uint16(len(body)+2))

	return append(segment, body...)
}
