package mp4

import (
	"errors"
	"fmt"
	"math/bits"
	"slices"
)

// Track indexes of a merged layout.
const (
	TrackVideo = 0
	TrackAudio = 1
)

// Layout describes a virtual fragmented MP4 holding one video and one audio
// track. Its bytes are Header followed by Pieces, each of which is a whole
// fragment copied from one of the upstream tracks.
type Layout struct {
	Header   []byte
	InitSize int // ftyp and moov, before the sidx in Header
	Pieces   []Piece
	Segments []Segment
	Size     int64
	Duration float64 // seconds
}

// Segment is a group of pieces containing one video fragment and its audio.
type Segment struct {
	Off, Size int64
	Duration  float64 // seconds
}

// Piece is a fragment placed in the merged file.
type Piece struct {
	Track        int   // TrackVideo or TrackAudio
	Off          int64 // offset in the merged file
	Src          int64 // offset in the upstream track
	Size         int64
	PatchTrackID uint32 // if nonzero, the fragment's tfhd track_IDs are rewritten to this
}

// Merge lays out video and audio as one file: a merged moov, one sidx
// indexing the video fragments, then the fragments of both tracks
// interleaved by time. No media data is touched, so the file can be served
// straight from byte ranges of the two upstream files.
func Merge(video, audio *Track) (*Layout, error) {
	audioID := audio.TrackID
	var patch uint32
	if audioID == video.TrackID {
		audioID = video.TrackID + 1
		patch = audioID
	}
	moov, err := mergeMoov(video, audio, audioID)
	if err != nil {
		return nil, err
	}
	ftyp := video.Ftyp.Data(video.Head)

	vf, af := video.Frags(), audio.Frags()
	if len(vf) == 0 || len(af) == 0 {
		return nil, errors.New("mp4: track has no fragments")
	}

	// Each group starts at one video fragment and is one sidx entry. An audio
	// fragment goes into the last group that starts before the audio ends, so
	// seeking to any group start finds the audio covering that moment at or
	// after the seek point, never before it.
	vts, ats := float64(video.Sidx.Timescale), float64(audio.Sidx.Timescale)
	groups := make([][]Frag, len(vf))
	g := 0
	for _, a := range af {
		end := float64(a.Start+a.Dur) / ats
		for g+1 < len(vf) && float64(vf[g+1].Start)/vts < end {
			g++
		}
		groups[g] = append(groups[g], a)
	}

	sidx := &Sidx{
		ReferenceID: video.TrackID,
		Timescale:   video.Sidx.Timescale,
		EarliestPTS: video.Sidx.EarliestPTS,
		Refs:        make([]SidxRef, len(vf)),
	}
	l := &Layout{
		Duration: max(video.Duration(), audio.Duration()),
		InitSize: len(ftyp) + len(moov),
		Segments: make([]Segment, 0, len(vf)),
	}
	off := int64(l.InitSize + sidxBoxSize(len(vf)))
	for i, v := range vf {
		start := off
		for _, a := range groups[i] {
			l.Pieces = append(l.Pieces, Piece{Track: TrackAudio, Off: off, Src: a.Src, Size: a.Size, PatchTrackID: patch})
			off += a.Size
		}
		l.Pieces = append(l.Pieces, Piece{Track: TrackVideo, Off: off, Src: v.Src, Size: v.Size})
		off += v.Size
		if off-start >= 1<<31 {
			return nil, fmt.Errorf("mp4: fragment group %d is too large", i)
		}
		ref := video.Sidx.Refs[i]
		sidx.Refs[i] = SidxRef{Size: uint32(off - start), Duration: ref.Duration, SAP: ref.SAP}
		l.Segments = append(l.Segments, Segment{Off: start, Size: off - start, Duration: float64(ref.Duration) / vts})
	}
	l.Header = slices.Concat(ftyp, moov, sidx.Marshal())
	l.Size = off
	return l, nil
}

// mergeMoov builds a moov holding the video and audio tracks, giving the
// audio track audioID.
func mergeMoov(video, audio *Track, audioID uint32) ([]byte, error) {
	var mvhd, vtrak, atrak []byte
	var mvex [][]byte

	vh := video.Head
	kids, err := Children(vh, video.Moov)
	if err != nil {
		return nil, err
	}
	for _, k := range kids {
		switch k.Type {
		case "mvhd":
			mvhd = slices.Clone(k.Data(vh))
		case "trak":
			vtrak = k.Data(vh)
		case "mvex":
			ek, err := Children(vh, k)
			if err != nil {
				return nil, err
			}
			for _, e := range ek {
				mvex = append(mvex, e.Data(vh))
			}
		}
	}

	ah := audio.Head
	if kids, err = Children(ah, audio.Moov); err != nil {
		return nil, err
	}
	renumber := audioID != audio.TrackID
	for _, k := range kids {
		switch k.Type {
		case "trak":
			atrak = slices.Clone(k.Data(ah))
		case "mvex":
			ek, err := Children(ah, k)
			if err != nil {
				return nil, err
			}
			for _, e := range ek {
				if e.Type == "mehd" { // the video's mehd covers the movie
					continue
				}
				b := slices.Clone(e.Data(ah))
				if renumber && (e.Type == "trex" || e.Type == "trep") {
					be.PutUint32(b[e.Header+4:], audioID)
				}
				mvex = append(mvex, b)
			}
		}
	}
	if mvhd == nil || vtrak == nil || atrak == nil {
		return nil, errors.New("mp4: moov lacks mvhd or trak")
	}

	be.PutUint32(mvhd[len(mvhd)-4:], max(video.TrackID, audioID)+1) // next_track_ID
	if err := fixAudioTrak(atrak, audioID, renumber, uint64(video.MovieTimescale), uint64(audio.MovieTimescale)); err != nil {
		return nil, err
	}
	return makeBox("moov", mvhd, vtrak, atrak, makeBox("mvex", mvex...)), nil
}

// fixAudioTrak renumbers the audio trak and rescales its movie-timescale
// durations from the audio file's movie timescale to the video file's.
func fixAudioTrak(trak []byte, id uint32, renumber bool, to, from uint64) error {
	top, err := Boxes(trak, 0, len(trak))
	if err != nil {
		return err
	}
	tb := top[0]
	tkhd, ok := Find(trak, tb, "tkhd")
	if !ok {
		return errors.New("mp4: audio trak lacks tkhd")
	}
	p := tkhd.Payload(trak)
	if renumber {
		be.PutUint32(p[fullBoxField(p, 8, 16):], id)
	}
	if to == from || from == 0 {
		return nil
	}
	if p[0] == 0 {
		o := fullBoxField(p, 16, 0)
		be.PutUint32(p[o:], uint32(scale(uint64(be.Uint32(p[o:])), to, from)))
	} else {
		o := fullBoxField(p, 0, 24)
		be.PutUint64(p[o:], scale(be.Uint64(p[o:]), to, from))
	}
	if elst, ok := Find(trak, tb, "edts", "elst"); ok {
		p := elst.Payload(trak)
		n := int(be.Uint32(p[4:]))
		for i := range n {
			if p[0] == 0 {
				o := 8 + 12*i
				be.PutUint32(p[o:], uint32(scale(uint64(be.Uint32(p[o:])), to, from)))
			} else {
				o := 8 + 20*i
				be.PutUint64(p[o:], scale(be.Uint64(p[o:]), to, from))
			}
		}
	}
	return nil
}

// scale returns x*to/from without intermediate overflow.
func scale(x, to, from uint64) uint64 {
	hi, lo := bits.Mul64(x, to)
	if hi >= from {
		return ^uint64(0)
	}
	q, _ := bits.Div64(hi, lo, from)
	return q
}

// PatchMoofTrackID rewrites the track_ID of every tfhd inside a moof box.
func PatchMoofTrackID(moof []byte, id uint32) error {
	top, err := Boxes(moof, 0, len(moof))
	if err != nil {
		return err
	}
	if len(top) != 1 || top[0].Type != "moof" {
		return errors.New("mp4: fragment does not start with moof")
	}
	trafs, err := Children(moof, top[0])
	if err != nil {
		return err
	}
	for _, t := range trafs {
		if t.Type != "traf" {
			continue
		}
		tfhd, ok := Find(moof, t, "tfhd")
		if !ok {
			return errors.New("mp4: traf lacks tfhd")
		}
		be.PutUint32(tfhd.Payload(moof)[4:], id)
	}
	return nil
}
