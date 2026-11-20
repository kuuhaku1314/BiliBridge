package stream

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bilibridge/internal/mp4"
)

func TestDASHManifestUsesSeparateTrackIndexes(t *testing.T) {
	s := &Server{base: "http://127.0.0.1:1", files: make(map[string]publication)}
	f := &File{
		Layout: &mp4.Layout{Size: 12000, Duration: 10, InitSize: 800, Header: make([]byte, 1012)},
		audio:  &File{Layout: &mp4.Layout{Size: 3000, Duration: 10.1, InitSize: 640, Header: make([]byte, 740)}},
	}
	video := s.AddMP4(t.Context(), f, "Title & author", 0)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, s.DASHURL(video), nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/dash+xml" {
		t.Fatalf("unexpected manifest response %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	for _, entry := range []string{
		`contentType="video"`, `contentType="audio"`,
		`indexRange="800-1011"`, `range="0-799"`, `indexRange="640-739"`, `range="0-639"`,
		strings.ReplaceAll(video, "&", "&amp;"), strings.ReplaceAll(s.AudioURL(video), "&", "&amp;"),
	} {
		if !strings.Contains(rec.Body.String(), entry) {
			t.Errorf("manifest is missing %q", entry)
		}
	}
}
