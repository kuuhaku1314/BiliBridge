package stream

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"bilibridge/internal/mp4"
)

func TestWriteRangeLimitsAndReusesUpstreamRead(t *testing.T) {
	var requests atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if got := r.Header.Get("Range"); got != "bytes=1-4" {
			t.Errorf("upstream Range = %q, want bytes=1-4", got)
		}
		http.ServeContent(w, r, "source.mp4", time.Time{}, bytes.NewReader([]byte("abcdef")))
	}))
	defer origin.Close()

	f := &File{
		Layout: &mp4.Layout{
			Size: 6,
			Pieces: []mp4.Piece{
				{Track: mp4.TrackVideo, Off: 0, Src: 0, Size: 3},
				{Track: mp4.TrackVideo, Off: 3, Src: 3, Size: 3},
			},
		},
		tracks: [2]*Upstream{NewUpstream("video", []string{origin.URL}, make(http.Header), nil)},
	}
	var out bytes.Buffer
	if err := f.WriteRange(t.Context(), &out, 1, 5); err != nil {
		t.Fatal(err)
	}
	if out.String() != "bcde" || requests.Load() != 1 {
		t.Fatalf("wrote %q using %d requests, want bcde in one request", out.String(), requests.Load())
	}
}

func TestPublishedAudioSupportsByteRanges(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "audio.m4a", time.Time{}, bytes.NewReader([]byte("abcdef")))
	}))
	defer origin.Close()
	s := &Server{base: "http://127.0.0.1:1", files: make(map[string]publication)}
	f := &File{
		Layout: &mp4.Layout{Size: 99},
		audio: &File{
			Layout: &mp4.Layout{Size: 6, Pieces: []mp4.Piece{{Track: mp4.TrackAudio, Size: 6}}},
			tracks: [2]*Upstream{mp4.TrackAudio: NewUpstream("audio", []string{origin.URL}, make(http.Header), nil)},
		},
	}
	media := s.AddMP4(t.Context(), f, "Two tracks", 0)
	audio := s.AudioURL(media)
	req := httptest.NewRequest(http.MethodGet, audio, nil)
	req.Header.Set("Range", "bytes=2-4")
	response := httptest.NewRecorder()
	s.ServeHTTP(response, req)
	result := response.Result()
	defer result.Body.Close()
	data, err := io.ReadAll(result.Body)
	if err != nil || result.StatusCode != http.StatusPartialContent || result.Header.Get("Content-Range") != "bytes 2-4/6" || string(data) != "cde" {
		t.Fatalf("audio response %d %q %q, err %v", result.StatusCode, result.Header.Get("Content-Range"), data, err)
	}
}
