package photos

import "encoding/binary"

// orientation reads the Orientation tag (0x0112) from IFD0 of a TIFF block (the body of EXIF), or 1 when
// it is missing or unreadable. Nothing else in the block is read: everything else is dropped.
func orientation(tiff []byte) uint16 {
	if len(tiff) < 8 {
		return 1
	}

	var order binary.ByteOrder

	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 1
	}

	ifd := int(order.Uint32(tiff[4:8]))
	if ifd < 8 || ifd+2 > len(tiff) {
		return 1
	}

	entries := int(order.Uint16(tiff[ifd:]))

	for i := range entries {
		at := ifd + 2 + i*12
		if at+12 > len(tiff) {
			return 1
		}

		// SHORT (3), one value, stored in the first two bytes of the value field.
		if order.Uint16(tiff[at:]) == 0x0112 && order.Uint16(tiff[at+2:]) == 3 {
			if o := order.Uint16(tiff[at+8:]); o >= 1 && o <= 8 {
				return o
			}
		}
	}

	return 1
}

// orientationTIFF is a TIFF block whose only tag is Orientation: the one piece of metadata a photo needs
// to be shown the right way up.
func orientationTIFF(o uint16) []byte {
	b := make([]byte, 0, 26)
	b = append(b, 'I', 'I', 42, 0, 8, 0, 0, 0) // little endian, IFD0 at 8
	b = binary.LittleEndian.AppendUint16(b, 1) // one entry
	b = binary.LittleEndian.AppendUint16(b, 0x0112)
	b = binary.LittleEndian.AppendUint16(b, 3) // SHORT
	b = binary.LittleEndian.AppendUint32(b, 1) // count
	b = binary.LittleEndian.AppendUint16(b, o)
	b = append(b, 0, 0)                        // value padding
	b = binary.LittleEndian.AppendUint32(b, 0) // no next IFD

	return b
}
