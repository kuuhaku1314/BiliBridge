package mp4

import (
	"errors"
	"fmt"
)

// ErrEncrypted reports a DRM protected stream, which BiliBridge cannot play.
var ErrEncrypted = errors.New("mp4: stream is DRM encrypted")

// Track is one single-track fragmented MP4 (a DASH SegmentBase stream),
// described by its head: the bytes from the start of the file to the end
// of its sidx box.
type Track struct {
	Head           []byte
	Ftyp, Moov     Box
	TrackID        uint32
	MovieTimescale uint32
	Sidx           *Sidx
	SidxOffset     int
	FragStart      int64 // upstream offset of the first fragment
}

// Frag is one fragment (moof + mdat) of a track.
type Frag struct {
	Src   int64  // upstream byte offset
	Size  int64  // bytes
	Start uint64 // presentation time, in the sidx timescale
	Dur   uint64
}

// ParseTrack parses the head of a single-track fragmented MP4.
func ParseTrack(head []byte) (*Track, error) {
	top, err := Boxes(head, 0, len(head))
	if err != nil {
		return nil, err
	}
	t := &Track{Head: head}
	var haveFtyp, haveMoov bool
	for _, b := range top {
		switch b.Type {
		case "ftyp":
			t.Ftyp, haveFtyp = b, true
		case "moov":
			t.Moov, haveMoov = b, true
		case "sidx":
			if t.Sidx, err = ParseSidx(b.Payload(head)); err != nil {
				return nil, err
			}
			t.FragStart = int64(b.End) + int64(t.Sidx.FirstOffset)
			t.SidxOffset = b.Start
		}
	}
	if !haveFtyp || !haveMoov || t.Sidx == nil {
		return nil, errors.New("mp4: stream head lacks ftyp, moov or sidx")
	}

	kids, err := Children(head, t.Moov)
	if err != nil {
		return nil, err
	}
	traks := 0
	for _, k := range kids {
		switch k.Type {
		case "trak":
			traks++
		case "pssh":
			return nil, ErrEncrypted
		}
	}
	if traks != 1 {
		return nil, fmt.Errorf("mp4: expected 1 track, found %d", traks)
	}
	trak, _ := Find(head, t.Moov, "trak")
	if entry, ok := sampleEntry(head, trak); !ok {
		return nil, errors.New("mp4: track has no sample entry")
	} else if entry == "encv" || entry == "enca" {
		return nil, ErrEncrypted
	}

	tkhd, ok := Find(head, trak, "tkhd")
	if !ok {
		return nil, errors.New("mp4: track lacks tkhd")
	}
	p := tkhd.Payload(head)
	t.TrackID = be.Uint32(p[fullBoxField(p, 8, 16):])

	mvhd, ok := Find(head, t.Moov, "mvhd")
	if !ok {
		return nil, errors.New("mp4: moov lacks mvhd")
	}
	p = mvhd.Payload(head)
	t.MovieTimescale = be.Uint32(p[fullBoxField(p, 8, 16):])
	return t, nil
}

// sampleEntry returns the type of the first sample entry in trak's stsd.
func sampleEntry(buf []byte, trak Box) (string, bool) {
	stsd, ok := Find(buf, trak, "mdia", "minf", "stbl", "stsd")
	if !ok {
		return "", false
	}
	p := stsd.Payload(buf)
	if len(p) < 16 {
		return "", false
	}
	return string(p[12:16]), true
}

// Frags lists the fragments of the track from its sidx.
func (t *Track) Frags() []Frag {
	out := make([]Frag, len(t.Sidx.Refs))
	off, pts := t.FragStart, t.Sidx.EarliestPTS
	for i, r := range t.Sidx.Refs {
		out[i] = Frag{Src: off, Size: int64(r.Size), Start: pts, Dur: uint64(r.Duration)}
		off += int64(r.Size)
		pts += uint64(r.Duration)
	}
	return out
}

// Size is the byte size of the whole upstream file.
func (t *Track) Size() int64 {
	n := t.FragStart
	for _, r := range t.Sidx.Refs {
		n += int64(r.Size)
	}
	return n
}

// Duration is the track duration in seconds.
func (t *Track) Duration() float64 {
	var d uint64
	for _, r := range t.Sidx.Refs {
		d += uint64(r.Duration)
	}
	return float64(d) / float64(t.Sidx.Timescale)
}

// Layout preserves the original bytes and index of a single DASH track.
func (t *Track) Layout(track int) *Layout {
	off := int64(len(t.Head))
	l := &Layout{
		Header: t.Head, InitSize: t.SidxOffset, Size: t.Size(), Duration: t.Duration(),
		Pieces: []Piece{{Track: track, Off: off, Src: off, Size: t.Size() - off}},
	}
	for _, frag := range t.Frags() {
		end := frag.Src + frag.Size
		l.Segments = append(l.Segments, Segment{Off: off, Size: end - off, Duration: float64(frag.Dur) / float64(t.Sidx.Timescale)})
		off = end
	}
	return l
}
