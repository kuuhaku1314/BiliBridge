package stream

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"sort"

	"bilibridge/internal/mp4"
)

// File is a virtual MP4 whose fragments live upstream.
type File struct {
	Layout *mp4.Layout
	tracks [2]*Upstream
	audio  *File
}

// NewFile reads the stream indexes and constructs a merged or video-only MP4.
// indexEnd is the offset of the last byte of each stream's sidx.
func NewFile(ctx context.Context, video, audio *Upstream, videoIndexEnd, audioIndexEnd int64, separateAudio bool) (*File, error) {
	type result struct {
		t   *mp4.Track
		err error
	}
	read := func(u *Upstream, indexEnd int64, out chan<- result) {
		rc, err := u.Open(ctx, 0, indexEnd+1)
		if err != nil {
			out <- result{err: err}
			return
		}
		defer rc.Close()
		head := make([]byte, indexEnd+1)
		if _, err := io.ReadFull(rc, head); err != nil {
			out <- result{err: fmt.Errorf("%s: reading head: %w", u.name, err)}
			return
		}
		t, err := mp4.ParseTrack(head)
		if err != nil {
			err = fmt.Errorf("%s: %w", u.name, err)
		}
		out <- result{t, err}
	}
	vc, ac := make(chan result, 1), make(chan result, 1)
	go read(video, videoIndexEnd, vc)
	go read(audio, audioIndexEnd, ac)
	v, a := <-vc, <-ac
	if err := errors.Join(v.err, a.err); err != nil {
		return nil, err
	}
	tracks := [2]*Upstream{video, audio}
	if len(v.t.Sidx.Refs) == 0 || len(a.t.Sidx.Refs) == 0 {
		return nil, errors.New("mp4: track has no fragments")
	}
	if separateAudio {
		return &File{
			Layout: v.t.Layout(mp4.TrackVideo), tracks: tracks,
			audio: &File{Layout: a.t.Layout(mp4.TrackAudio), tracks: tracks},
		}, nil
	}
	l, err := mp4.Merge(v.t, a.t)
	if err != nil {
		return nil, err
	}
	return &File{Layout: l, tracks: tracks}, nil
}

// WriteRange writes bytes [start, end) of the file to w.
func (f *File) WriteRange(ctx context.Context, w io.Writer, start, end int64) error {
	l := f.Layout
	if start < int64(len(l.Header)) {
		e := min(end, int64(len(l.Header)))
		if _, err := w.Write(l.Header[start:e]); err != nil {
			return err
		}
		start = e
	}
	if start == end {
		return nil
	}
	first := sort.Search(len(l.Pieces), func(i int) bool { return l.Pieces[i].Off+l.Pieces[i].Size > start })
	last := sort.Search(len(l.Pieces), func(i int) bool { return l.Pieces[i].Off >= end })
	pieces := l.Pieces[first:last]
	c := &copier{ctx: ctx, f: f, buf: make([]byte, 64<<10)}
	defer c.close()
	for _, p := range pieces {
		length := min(end-p.Off, p.Size)
		if p.PatchTrackID != 0 {
			length = p.Size // rewriting the track ID may need bytes outside the requested range
		}
		c.end[p.Track] = p.Src + length
	}
	write := func(b []byte) error { _, err := w.Write(b); return err }
	for _, p := range pieces {
		from, to := start-p.Off, min(end, p.Off+p.Size)-p.Off
		if err := c.piece(p, from, to, write); err != nil {
			return err
		}
		start = p.Off + to
	}
	return nil
}

// copier copies pieces for one response. It keeps one upstream connection
// per track open, so consecutive fragments of a track, which are adjacent
// upstream, are read without new requests.
type copier struct {
	ctx  context.Context
	f    *File
	buf  []byte
	end  [2]int64 // exclusive upper bound of this response's read from each track
	conn [2]struct {
		rc  io.ReadCloser
		pos int64 // upstream offset of the next byte rc returns
	}
}

func (c *copier) close() {
	for i := range c.conn {
		if c.conn[i].rc != nil {
			c.conn[i].rc.Close()
			c.conn[i].rc = nil
		}
	}
}

// piece writes bytes [from, to) of piece p.
func (c *copier) piece(p mp4.Piece, from, to int64, write func([]byte) error) error {
	if p.PatchTrackID == 0 {
		return c.read(p.Track, p.Src+from, to-from, write)
	}
	// The fragment's track id must be rewritten, which needs its whole moof.
	var moof []byte
	collect := func(b []byte) error { moof = append(moof, b...); return nil }
	if err := c.read(p.Track, p.Src, 8, collect); err != nil {
		return err
	}
	size := int64(binary.BigEndian.Uint32(moof))
	if string(moof[4:8]) != "moof" || size < 8 || size > p.Size {
		return errors.New("fragment does not start with a moof box")
	}
	if err := c.read(p.Track, p.Src+8, size-8, collect); err != nil {
		return err
	}
	if err := mp4.PatchMoofTrackID(moof, p.PatchTrackID); err != nil {
		return err
	}
	if from < size {
		if err := write(moof[from:min(to, size)]); err != nil {
			return err
		}
		from = min(to, size)
	}
	if from < to {
		return c.read(p.Track, p.Src+from, to-from, write)
	}
	return nil
}

// read streams n bytes of a track, starting at upstream offset off, into
// sink, reconnecting when the upstream connection fails.
func (c *copier) read(track int, off, n int64, sink func([]byte) error) error {
	conn := &c.conn[track]
	u := c.f.tracks[track]
	failures := 0
	for n > 0 {
		if conn.rc == nil || conn.pos != off {
			if conn.rc != nil {
				conn.rc.Close()
			}
			conn.rc = nil
			rc, err := u.Open(c.ctx, off, c.end[track])
			if err != nil {
				return err
			}
			conn.rc, conn.pos = rc, off
		}
		m, err := conn.rc.Read(c.buf[:min(n, int64(len(c.buf)))])
		if m > 0 {
			if werr := sink(c.buf[:m]); werr != nil {
				return werr
			}
			conn.pos += int64(m)
			off += int64(m)
			n -= int64(m)
			failures = 0
		}
		if err != nil {
			conn.rc.Close()
			conn.rc = nil
			if n == 0 {
				return nil
			}
			if c.ctx.Err() != nil {
				return c.ctx.Err()
			}
			if failures++; failures > 3 {
				return fmt.Errorf("%s: read at %d: %w", u.name, off, err)
			}
			log.Printf("%s: read at %d failed, reconnecting: %v", u.name, off, err)
		}
	}
	return nil
}
