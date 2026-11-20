package stream

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"bilibridge/internal/mp4"
)

func TestMP4RangeSpansHeaderColdAndPrefetchedSegments(t *testing.T) {
	const segmentSize = 64 << 10
	header := []byte("header")
	first := bytes.Repeat([]byte{1}, segmentSize)
	second := bytes.Repeat([]byte{2}, segmentSize)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=0-65535" {
			t.Errorf("unexpected upstream range %q", r.Header.Get("Range"))
		}
		http.ServeContent(w, r, "source.mp4", time.Time{}, bytes.NewReader(first))
	}))
	defer origin.Close()
	a, b := int64(len(header)), int64(len(header)+segmentSize)
	m := &mp4Stream{
		ctx: t.Context(), prefetch: time.Minute, cached: map[int][]byte{1: second},
		file: &File{
			Layout: &mp4.Layout{
				Header: header,
				Size:   b + segmentSize,
				Segments: []mp4.Segment{
					{Off: a, Size: segmentSize, Duration: 5}, {Off: b, Size: segmentSize, Duration: 5},
				},
				Pieces: []mp4.Piece{
					{Off: a, Size: segmentSize}, {Off: b, Size: segmentSize, Src: segmentSize},
				},
			},
			tracks: [2]*Upstream{NewUpstream("video", []string{origin.URL}, make(http.Header), nil)},
		},
	}
	var out bytes.Buffer
	if err := m.writeRange(t.Context(), &out, 2, b+9); err != nil {
		t.Fatal(err)
	}
	want := bytes.Join([][]byte{header[2:], first, second[:9]}, nil)
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatal("range differs from the original merged file")
	}
}

func TestMP4PlaybackTakesPriorityOverPendingPrefetch(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	var requests atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			close(started)
			<-r.Context().Done()
			close(stopped)
			return
		}
		http.ServeContent(w, r, "source.mp4", time.Time{}, bytes.NewReader([]byte("abcdefgh")))
	}))
	defer origin.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m := &mp4Stream{
		ctx: ctx, prefetch: time.Minute,
		file: &File{
			Layout: &mp4.Layout{
				Size: 8, Pieces: []mp4.Piece{{Size: 8}},
				Segments: []mp4.Segment{{Size: 4, Duration: 5}, {Off: 4, Size: 4, Duration: 5}},
			},
			tracks: [2]*Upstream{NewUpstream("video", []string{origin.URL}, make(http.Header), nil)},
		},
	}
	m.warm(0)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("prefetch did not start")
	}
	var out bytes.Buffer
	if err := m.writeRange(t.Context(), &out, 4, 8); err != nil || out.String() != "efgh" {
		t.Fatalf("playback read %q, err %v", out.String(), err)
	}
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("competing prefetch was left running")
	}
}
