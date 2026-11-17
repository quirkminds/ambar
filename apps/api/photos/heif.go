package photos

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"os"
	"strconv"
	"strings"
)

// HEIC keeps metadata as items (Exif, XMP) whose bytes iloc locates, usually inside mdat. Removing them
// would mean rewriting every offset in the file; instead their bytes are overwritten in place with an
// empty block of the same length, so nothing moves and the pictures are untouched. Orientation lives in
// irot and imir properties, which are kept.

type box struct {
	kind         string
	offset, size int64 // payload
}

// boxes lists the boxes between from and to.
func boxes(r io.ReaderAt, from, to int64) ([]box, error) {
	var list []box

	for at := from; at < to; {
		h := make([]byte, 16)
		if _, err := r.ReadAt(h[:8], at); err != nil {
			return nil, ErrUnreadable
		}

		size, header := int64(binary.BigEndian.Uint32(h)), int64(8)

		switch size {
		case 1: // 64-bit size
			if _, err := r.ReadAt(h[8:16], at+8); err != nil {
				return nil, ErrUnreadable
			}

			size, header = int64(binary.BigEndian.Uint64(h[8:])), 16
		case 0: // to the end
			size = to - at
		}

		if size < header || at+size > to {
			return nil, ErrUnreadable
		}

		list = append(list, box{kind: string(h[4:8]), offset: at + header, size: size - header})
		at += size
	}

	return list, nil
}

func find(list []box, kind string) (box, bool) {
	for _, b := range list {
		if b.kind == kind {
			return b, true
		}
	}

	return box{}, false
}

func read(r io.ReaderAt, b box) ([]byte, error) {
	if b.size > 16<<20 { // a meta box this large isn't a photo's
		return nil, ErrUnreadable
	}

	data := make([]byte, b.size)
	if _, err := r.ReadAt(data, b.offset); err != nil {
		return nil, ErrUnreadable
	}

	return data, nil
}

type extent struct{ offset, length int64 }

// stripHEIF overwrites the Exif and XMP items of the HEIF file at path, in place.
func stripHEIF(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return err
	}

	top, err := boxes(f, 0, info.Size())
	if err != nil {
		return err
	}

	meta, ok := find(top, "meta")
	if !ok || meta.size < 4 {
		return ErrUnreadable
	}

	// meta is a full box: version and flags first.
	children, err := boxes(f, meta.offset+4, meta.offset+meta.size)
	if err != nil {
		return err
	}

	targets, err := metadataItems(f, children)
	if err != nil {
		return err
	}

	if len(targets) == 0 {
		return nil
	}

	locations, err := itemLocations(f, children)
	if err != nil {
		return err
	}

	for id, kind := range targets {
		extents, ok := locations[id]
		if !ok {
			return ErrUnreadable
		}

		var total int64
		for _, e := range extents {
			if e.offset < 0 || e.length < 0 || e.offset+e.length > info.Size() {
				return ErrUnreadable
			}

			total += e.length
		}

		replacement := emptyItem(kind, total)

		for _, e := range extents {
			if _, err := f.WriteAt(replacement[:e.length], e.offset); err != nil {
				return err
			}

			replacement = replacement[e.length:]
		}
	}

	return f.Sync()
}

// metadataItems finds the ids of Exif and XMP items in iinf.
func metadataItems(r io.ReaderAt, children []box) (map[uint32]string, error) {
	iinf, ok := find(children, "iinf")
	if !ok {
		return nil, nil
	}

	data, err := read(r, iinf)
	if err != nil {
		return nil, err
	}

	if len(data) < 6 {
		return nil, ErrUnreadable
	}

	at := 6 // version, flags, entry_count (16 bits)
	if data[0] != 0 {
		at = 8 // entry_count (32 bits)
	}

	entries, err := boxes(bytes.NewReader(data), int64(at), int64(len(data)))
	if err != nil {
		return nil, err
	}

	items := map[uint32]string{}

	for _, e := range entries {
		if e.kind != "infe" {
			continue
		}

		infe := data[e.offset : e.offset+e.size]
		if len(infe) < 4 {
			return nil, ErrUnreadable
		}

		version := infe[0]

		var (
			id   uint32
			rest []byte
		)

		switch version {
		case 2:
			if len(infe) < 12 {
				return nil, ErrUnreadable
			}

			id, rest = uint32(binary.BigEndian.Uint16(infe[4:])), infe[8:]
		case 3:
			if len(infe) < 14 {
				return nil, ErrUnreadable
			}

			id, rest = binary.BigEndian.Uint32(infe[4:]), infe[10:]
		default:
			// Versions 0 and 1 predate item types: refuse rather than miss metadata.
			return nil, ErrUnreadable
		}

		kind := string(rest[:4])
		switch kind {
		case "Exif":
			items[id] = "Exif"
		case "mime":
			// item_name, then content_type, both NUL-terminated.
			fields := bytes.SplitN(rest[4:], []byte{0}, 3)
			if len(fields) >= 2 && strings.Contains(string(fields[1]), "rdf+xml") {
				items[id] = "XMP"
			}
		}
	}

	return items, nil
}

// itemLocations parses iloc: where each item's bytes are in the file.
func itemLocations(r io.ReaderAt, children []box) (map[uint32][]extent, error) {
	iloc, ok := find(children, "iloc")
	if !ok {
		return nil, ErrUnreadable
	}

	data, err := read(r, iloc)
	if err != nil {
		return nil, err
	}

	p := parser{data: data}
	version := p.u(1)
	p.u(3) // flags

	sizes := p.u(2)
	offsetSize, lengthSize := int(sizes>>12&0xf), int(sizes>>8&0xf)
	baseSize, indexSize := int(sizes>>4&0xf), 0

	if version == 1 || version == 2 {
		indexSize = int(sizes & 0xf)
	}

	count := p.u(2)
	if version == 2 {
		count = p.u(4)
	}

	locations := map[uint32][]extent{}

	for range count {
		var id uint64
		if version < 2 {
			id = p.u(2)
		} else {
			id = p.u(4)
		}

		method := uint64(0)
		if version == 1 || version == 2 {
			method = p.u(2) & 0xf
		}

		p.u(2) // data_reference_index
		base := p.u(baseSize)
		extents := p.u(2)

		for range extents {
			if indexSize > 0 {
				p.u(indexSize)
			}

			offset, length := p.u(offsetSize), p.u(lengthSize)

			// Only file offsets (method 0) are rewritten here; items stored elsewhere are left to the
			// caller to refuse if they are metadata.
			if method == 0 {
				locations[uint32(id)] = append(locations[uint32(id)], extent{int64(base + offset), int64(length)})
			}
		}

		if p.bad {
			return nil, ErrUnreadable
		}
	}

	return locations, nil
}

// emptyItem is an Exif or XMP item that says nothing, padded to length bytes.
func emptyItem(kind string, length int64) []byte {
	out := make([]byte, length)

	var body []byte
	if kind == "Exif" {
		// exif_tiff_header_offset (0), then a TIFF block with no tags.
		body = append([]byte{0, 0, 0, 0}, 'I', 'I', 42, 0, 8, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	} else {
		body = []byte(`<?xpacket begin="" id="W5M0MpCehiHzreSzNTczkc9d"?><x:xmpmeta xmlns:x="adobe:ns:meta/"/><?xpacket end="w"?>`)
		for i := range out {
			out[i] = ' '
		}
	}

	if int64(len(body)) <= length {
		copy(out, body)
	}

	return out
}

type parser struct {
	data []byte
	at   int
	bad  bool
}

func (p *parser) u(n int) uint64 {
	if n == 0 {
		return 0
	}

	if n > 8 || p.at+n > len(p.data) {
		p.bad = true

		return 0
	}

	var v uint64
	for _, b := range p.data[p.at : p.at+n] {
		v = v<<8 | uint64(b)
	}

	p.at += n

	return v
}

// heifBounds asks libvips for the size of the main picture: decoding HEVC in Go isn't an option.
func heifBounds(path string) (int, int, error) {
	out, err := run(context.Background(), "vipsheader", "-f", "width", path)
	if err != nil {
		return 0, 0, ErrUnreadable
	}

	width, _ := strconv.Atoi(strings.TrimSpace(out))

	out, err = run(context.Background(), "vipsheader", "-f", "height", path)
	if err != nil {
		return 0, 0, ErrUnreadable
	}

	height, _ := strconv.Atoi(strings.TrimSpace(out))

	return width, height, nil
}
