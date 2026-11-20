package mp4

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Sidx is a segment index box: the list of fragments of a track with their
// byte sizes and durations.
type Sidx struct {
	ReferenceID uint32
	Timescale   uint32
	EarliestPTS uint64
	FirstOffset uint64
	Refs        []SidxRef
}

// SidxRef is one sidx reference. Only media references are supported.
type SidxRef struct {
	Size     uint32
	Duration uint32
	SAP      uint32 // raw starts_with_SAP | SAP_type | SAP_delta_time word
}

var be = binary.BigEndian

// ParseSidx parses the payload of a sidx box.
func ParseSidx(p []byte) (*Sidx, error) {
	short := errors.New("mp4: truncated sidx")
	if len(p) < 12 {
		return nil, short
	}
	s := &Sidx{ReferenceID: be.Uint32(p[4:]), Timescale: be.Uint32(p[8:])}
	r := p[12:]
	if p[0] == 0 {
		if len(r) < 12 {
			return nil, short
		}
		s.EarliestPTS, s.FirstOffset = uint64(be.Uint32(r)), uint64(be.Uint32(r[4:]))
		r = r[8:]
	} else {
		if len(r) < 20 {
			return nil, short
		}
		s.EarliestPTS, s.FirstOffset = be.Uint64(r), be.Uint64(r[8:])
		r = r[16:]
	}
	n := int(be.Uint16(r[2:]))
	r = r[4:]
	if len(r) < 12*n {
		return nil, short
	}
	if s.Timescale == 0 {
		return nil, errors.New("mp4: sidx timescale is 0")
	}
	s.Refs = make([]SidxRef, n)
	for i := range s.Refs {
		e := r[12*i:]
		a := be.Uint32(e)
		if a>>31 != 0 {
			return nil, fmt.Errorf("mp4: hierarchical sidx is not supported")
		}
		s.Refs[i] = SidxRef{Size: a, Duration: be.Uint32(e[4:]), SAP: be.Uint32(e[8:])}
	}
	return s, nil
}

// sidxBoxSize is the size of the version 1 sidx box Marshal writes.
func sidxBoxSize(refs int) int { return 8 + 4 + 8 + 16 + 4 + 12*refs }

// Marshal encodes s as a version 1 sidx box.
func (s *Sidx) Marshal() []byte {
	p := make([]byte, sidxBoxSize(len(s.Refs))-8)
	p[0] = 1
	be.PutUint32(p[4:], s.ReferenceID)
	be.PutUint32(p[8:], s.Timescale)
	be.PutUint64(p[12:], s.EarliestPTS)
	be.PutUint64(p[20:], s.FirstOffset)
	be.PutUint16(p[30:], uint16(len(s.Refs)))
	for i, ref := range s.Refs {
		e := p[32+12*i:]
		be.PutUint32(e, ref.Size)
		be.PutUint32(e[4:], ref.Duration)
		be.PutUint32(e[8:], ref.SAP)
	}
	return makeBox("sidx", p)
}
