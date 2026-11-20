package stream

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"bilibridge/internal/mp4"
)

func TestParseRangeClampsEndWithoutOverflow(t *testing.T) {
	const size = 180566708
	start, end, ok := parseRange("bytes=10-9223372036854775807", size)
	if !ok || start != 10 || end != size {
		t.Fatalf("parseRange returned (%d, %d, %t), want (10, %d, true)", start, end, ok, size)
	}
}

func TestWaitForVideoReadStartsOnlyAfterGet(t *testing.T) {
	s := &Server{base: "http://127.0.0.1:1", files: make(map[string]publication)}
	f := &File{Layout: &mp4.Layout{Header: []byte("video"), Size: 5}, audio: &File{Layout: &mp4.Layout{Header: []byte("audio"), Size: 5}}}
	u := s.AddMP4(t.Context(), f, "movie", 0)
	s.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodHead, u, nil))
	s.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, s.AudioURL(u), nil))
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.WaitForVideoRead(canceled, u); !errors.Is(err, context.Canceled) {
		t.Fatalf("a probe or the audio track was counted as video playback: %v", err)
	}
	s.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, u, nil))
	ready, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	if err := s.WaitForVideoRead(ready, u); err != nil {
		t.Fatalf("video read did not signal readiness: %v", err)
	}
}
