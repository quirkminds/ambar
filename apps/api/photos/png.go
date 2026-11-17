package photos

import (
	"bufio"
	"encoding/binary"
	"hash/crc32"
	"io"
)

// stripPNG copies a PNG without its text and EXIF chunks (tEXt, zTXt, iTXt, eXIf, tIME), keeping every
// chunk that affects the picture (iCCP, gAMA, …) byte for byte, the orientation as an eXIf chunk with that
// tag alone, and nothing after IEND.
func stripPNG(in io.Reader, out io.Writer) error {
	r := bufio.NewReader(in)
	w := bufio.NewWriter(out)

	signature := make([]byte, 8)
	if _, err := io.ReadFull(r, signature); err != nil || string(signature) != "\x89PNG\r\n\x1a\n" {
		return ErrUnreadable
	}

	_, _ = w.Write(signature)

	for {
		head := make([]byte, 8)
		if _, err := io.ReadFull(r, head); err != nil {
			return ErrUnreadable // no IEND: truncated
		}

		size := binary.BigEndian.Uint32(head)
		kind := string(head[4:8])

		if size > 1<<31-1 {
			return ErrUnreadable
		}

		switch kind {
		case "tEXt", "zTXt", "iTXt", "tIME":
			if _, err := io.CopyN(io.Discard, r, int64(size)+4); err != nil {
				return ErrUnreadable
			}

			continue
		case "eXIf":
			data := make([]byte, size)
			if _, err := io.ReadFull(r, data); err != nil {
				return ErrUnreadable
			}

			if _, err := io.CopyN(io.Discard, r, 4); err != nil {
				return ErrUnreadable
			}

			if o := orientation(data); o != 1 {
				writeChunk(w, "eXIf", orientationTIFF(o))
			}

			continue
		}

		_, _ = w.Write(head)

		if _, err := io.CopyN(w, r, int64(size)+4); err != nil {
			return ErrUnreadable
		}

		if kind == "IEND" {
			return w.Flush()
		}
	}
}

func writeChunk(w io.Writer, kind string, data []byte) {
	head := make([]byte, 8)
	binary.BigEndian.PutUint32(head, uint32(len(data)))
	copy(head[4:], kind)

	crc := crc32.NewIEEE()
	_, _ = crc.Write(head[4:])
	_, _ = crc.Write(data)

	_, _ = w.Write(head)
	_, _ = w.Write(data)
	_, _ = w.Write(binary.BigEndian.AppendUint32(nil, crc.Sum32()))
}
