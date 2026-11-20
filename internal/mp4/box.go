// Package mp4 parses the ISO BMFF boxes BiliBridge needs and merges a DASH
// video track and audio track into the layout of one fragmented MP4 file.
package mp4

import (
	"encoding/binary"
	"fmt"
)

// Box is a box located inside a byte slice.
type Box struct {
	Type   string
	Start  int // offset of the box header
	Header int // header length: 8, or 16 for 64-bit sizes
	End    int // offset just past the box
}

// Data returns the whole box, header included.
func (b Box) Data(buf []byte) []byte { return buf[b.Start:b.End] }

// Payload returns the box contents after the header.
func (b Box) Payload(buf []byte) []byte { return buf[b.Start+b.Header : b.End] }

// Boxes parses the sibling boxes in buf[start:end].
func Boxes(buf []byte, start, end int) ([]Box, error) {
	var out []Box
	for off := start; off < end; {
		if end-off < 8 {
			return nil, fmt.Errorf("mp4: truncated box header at %d", off)
		}
		size := int64(binary.BigEndian.Uint32(buf[off:]))
		typ := string(buf[off+4 : off+8])
		hdr := 8
		switch size {
		case 0:
			size = int64(end - off)
		case 1:
			if end-off < 16 {
				return nil, fmt.Errorf("mp4: truncated large box header at %d", off)
			}
			size = int64(binary.BigEndian.Uint64(buf[off+8:]))
			hdr = 16
		}
		if size < int64(hdr) || int64(off)+size > int64(end) {
			return nil, fmt.Errorf("mp4: box %q at %d has bad size %d", typ, off, size)
		}
		out = append(out, Box{Type: typ, Start: off, Header: hdr, End: off + int(size)})
		off += int(size)
	}
	return out, nil
}

// Children parses the boxes nested directly inside parent.
func Children(buf []byte, parent Box) ([]Box, error) {
	return Boxes(buf, parent.Start+parent.Header, parent.End)
}

// Find descends from parent through the first box of each type in path,
// e.g. Find(buf, trak, "mdia", "mdhd").
func Find(buf []byte, parent Box, path ...string) (Box, bool) {
	cur := parent
	for _, typ := range path {
		kids, err := Children(buf, cur)
		if err != nil {
			return Box{}, false
		}
		found := false
		for _, k := range kids {
			if k.Type == typ {
				cur, found = k, true
				break
			}
		}
		if !found {
			return Box{}, false
		}
	}
	return cur, true
}

// makeBox builds a box of the given type around the concatenated payloads.
func makeBox(typ string, payloads ...[]byte) []byte {
	n := 8
	for _, p := range payloads {
		n += len(p)
	}
	out := make([]byte, 8, n)
	binary.BigEndian.PutUint32(out, uint32(n))
	copy(out[4:], typ)
	for _, p := range payloads {
		out = append(out, p...)
	}
	return out
}

// fullBoxField returns the offset, within a full box's payload, of a field
// whose position depends on the box version: v0 is used for version 0 and
// v1 otherwise. The 4-byte version/flags header is accounted for.
func fullBoxField(payload []byte, v0, v1 int) int {
	if payload[0] == 0 {
		return 4 + v0
	}
	return 4 + v1
}
