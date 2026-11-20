package stream

import (
	"testing"
	"time"

	"bilibridge/internal/mp4"
)

func TestHLSPrefetchWindowLimits(t *testing.T) {
	segments := make([]mp4.Segment, 30)
	for i := range segments {
		segments[i] = mp4.Segment{Size: 1 << 20, Duration: 5}
	}
	h := &hlsStream{file: &File{Layout: &mp4.Layout{Segments: segments}}, prefetch: time.Minute}
	if end := h.prefetchEnd(3); end != 16 {
		t.Errorf("one-minute window ends at %d, want 16", end)
	}
	h.prefetch = time.Hour
	if end := h.prefetchEnd(3); end != 20 {
		t.Errorf("segment-limited window ends at %d, want 20", end)
	}
	for i := range segments {
		segments[i].Size = 5 << 20
	}
	if end := h.prefetchEnd(3); end != 10 {
		t.Errorf("byte-limited window ends at %d, want 10", end)
	}
	if end := h.prefetchEnd(28); end != len(segments) {
		t.Errorf("last window ends at %d, want %d", end, len(segments))
	}
}
