package photos

import (
	"encoding/binary"
	"io"
	"os"
)

type riffChunk struct {
	kind         string
	offset, size int64 // payload
}

// webpChunks lists the chunks of a WebP file, refusing one whose sizes don't add up.
func webpChunks(r io.ReaderAt, fileSize int64) ([]riffChunk, error) {
	head := make([]byte, 12)
	if _, err := r.ReadAt(head, 0); err != nil || string(head[:4]) != "RIFF" || string(head[8:12]) != "WEBP" {
		return nil, ErrUnreadable
	}

	// Anything past the declared RIFF size isn't part of the picture and is dropped.
	end := min(8+int64(binary.LittleEndian.Uint32(head[4:])), fileSize)

	var chunks []riffChunk

	for at := int64(12); at < end; {
		h := make([]byte, 8)
		if _, err := r.ReadAt(h, at); err != nil {
			return nil, ErrUnreadable
		}

		size := int64(binary.LittleEndian.Uint32(h[4:]))
		if at+8+size > end {
			return nil, ErrUnreadable
		}

		chunks = append(chunks, riffChunk{kind: string(h[:4]), offset: at + 8, size: size})
		at += 8 + size + size%2
	}

	if len(chunks) == 0 {
		return nil, ErrUnreadable
	}

	return chunks, nil
}

// stripWebP copies a WebP without its EXIF and XMP chunks, keeping the orientation as an EXIF chunk
// with that tag alone and the picture chunks byte for byte. VP8X's flags and the RIFF size are rewritten.
func stripWebP(in *os.File, out io.Writer) error {
	info, err := in.Stat()
	if err != nil {
		return err
	}

	chunks, err := webpChunks(in, info.Size())
	if err != nil {
		return err
	}

	var (
		orient uint16 = 1
		kept   []riffChunk
	)

	for _, c := range chunks {
		switch c.kind {
		case "EXIF":
			data := make([]byte, c.size)
			if _, err := in.ReadAt(data, c.offset); err != nil {
				return ErrUnreadable
			}

			// Some writers keep the "Exif\0\0" prefix of JPEG.
			if len(data) > 6 && string(data[:6]) == "Exif\x00\x00" {
				data = data[6:]
			}

			orient = orientation(data)
		case "XMP ":
		default:
			kept = append(kept, c)
		}
	}

	var exif []byte
	if orient != 1 {
		exif = orientationTIFF(orient)
	}

	total := int64(4) // "WEBP"
	for _, c := range kept {
		total += 8 + c.size + c.size%2
	}

	if exif != nil {
		total += 8 + int64(len(exif)) + int64(len(exif)%2)
	}

	if total > 1<<32-1 {
		return ErrUnreadable
	}

	header := []byte("RIFF\x00\x00\x00\x00WEBP")
	binary.LittleEndian.PutUint32(header[4:], uint32(total))

	if _, err := out.Write(header); err != nil {
		return err
	}

	for _, c := range kept {
		payload := make([]byte, c.size+c.size%2)
		if _, err := in.ReadAt(payload, c.offset); err != nil && c.size%2 == 0 {
			return ErrUnreadable
		}

		// VP8X: EXIF is bit 3, XMP bit 2 of the first byte.
		if c.kind == "VP8X" && len(payload) > 0 {
			payload[0] &^= 0x04 | 0x08
			if exif != nil {
				payload[0] |= 0x08
			}
		}

		writeRIFF(out, c.kind, payload[:c.size])
	}

	// EXIF goes after the picture data, as the container spec orders it.
	if exif != nil {
		writeRIFF(out, "EXIF", exif)
	}

	return nil
}

func writeRIFF(w io.Writer, kind string, payload []byte) {
	h := make([]byte, 8)
	copy(h, kind)
	binary.LittleEndian.PutUint32(h[4:], uint32(len(payload)))
	_, _ = w.Write(h)
	_, _ = w.Write(payload)

	if len(payload)%2 == 1 {
		_, _ = w.Write([]byte{0})
	}
}

// webpBounds reads the canvas size from the first chunk: VP8X (extended), VP8 (lossy) or VP8L (lossless).
func webpBounds(path string) (int, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}

	chunks, err := webpChunks(f, info.Size())
	if err != nil {
		return 0, 0, err
	}

	first := chunks[0]
	b := make([]byte, 10)

	if first.size < 10 {
		return 0, 0, ErrUnreadable
	}

	if _, err := f.ReadAt(b, first.offset); err != nil {
		return 0, 0, ErrUnreadable
	}

	u24 := func(p []byte) int { return int(p[0]) | int(p[1])<<8 | int(p[2])<<16 }

	switch first.kind {
	case "VP8X":
		return u24(b[4:]) + 1, u24(b[7:]) + 1, nil
	case "VP8 ":
		if b[3] != 0x9d || b[4] != 0x01 || b[5] != 0x2a {
			return 0, 0, ErrUnreadable
		}

		return int(binary.LittleEndian.Uint16(b[6:]) & 0x3fff), int(binary.LittleEndian.Uint16(b[8:]) & 0x3fff), nil
	case "VP8L":
		if b[0] != 0x2f {
			return 0, 0, ErrUnreadable
		}

		bits := binary.LittleEndian.Uint32(b[1:])

		return int(bits&0x3fff) + 1, int(bits>>14&0x3fff) + 1, nil
	}

	return 0, 0, ErrUnreadable
}
