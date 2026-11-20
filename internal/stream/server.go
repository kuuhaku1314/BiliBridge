package stream

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Server serves merged files and HLS playlists over local HTTP.
type Server struct {
	base string

	mu    sync.Mutex
	files map[string]publication
}

type publication struct {
	file  *File
	hls   *hlsStream
	mp4   *mp4Stream
	audio *mp4Stream
	use   *videoUse
}

type videoUse struct {
	once sync.Once
	read chan struct{}
}

// Listen starts a server on addr, e.g. "127.0.0.1:0".
func Listen(addr string) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{base: "http://" + ln.Addr().String(), files: map[string]publication{}}
	go http.Serve(ln, s)
	return s, nil
}

// AddMP4 publishes f with the given read-ahead window, or without read-ahead
// when prefetch is zero. name is shown as the file name.
func (s *Server) AddMP4(ctx context.Context, f *File, name string, prefetch time.Duration) string {
	p := publication{file: f}
	if prefetch > 0 {
		p.mp4 = &mp4Stream{ctx: ctx, file: f, prefetch: prefetch}
		if f.audio != nil {
			p.audio = &mp4Stream{ctx: ctx, file: f.audio, prefetch: prefetch}
		}
	}
	id := s.publish(p)
	return fmt.Sprintf("%s/v/%s/%s.mp4", s.base, id, url.PathEscape(cleanName(name)))
}

// AudioURL returns the original audio track for a published MP4.
func (s *Server) AudioURL(mediaURL string) string {
	return s.relatedURL(mediaURL, "audio", ".m4a")
}

// DASHURL returns a manifest linking the original video and audio tracks.
func (s *Server) DASHURL(mediaURL string) string {
	return s.relatedURL(mediaURL, "dash", ".mpd")
}

func (s *Server) relatedURL(mediaURL, route, extension string) string {
	mediaPath, local := strings.CutPrefix(mediaURL, s.base+"/v/")
	id, name, ok := strings.Cut(mediaPath, "/")
	if !local || !ok {
		return ""
	}
	return s.base + "/" + route + "/" + id + "/" + strings.TrimSuffix(name, path.Ext(name)) + extension
}

// AddHLS publishes a VOD playlist whose segments are remuxed by FFmpeg.
func (s *Server) AddHLS(ctx context.Context, f *File, name, ffmpeg string, prefetch time.Duration) (string, error) {
	hls, err := newHLS(ctx, f, ffmpeg, prefetch)
	if err != nil {
		return "", err
	}
	id := s.publish(publication{file: f, hls: hls})
	return fmt.Sprintf("%s/hls/%s/%s.m3u8", s.base, id, url.PathEscape(cleanName(name))), nil
}

func (s *Server) publish(p publication) string {
	b := make([]byte, 8)
	rand.Read(b)
	id := hex.EncodeToString(b)
	p.use = &videoUse{read: make(chan struct{})}
	s.mu.Lock()
	s.files[id] = p
	s.mu.Unlock()
	return id
}

// WaitForVideoRead waits for the new URL to be read, even when the player
// already shows the same title from a previous playback session.
func (s *Server) WaitForVideoRead(ctx context.Context, mediaURL string) error {
	mediaPath, local := strings.CutPrefix(mediaURL, s.base+"/v/")
	id, _, ok := strings.Cut(mediaPath, "/")
	if !local || !ok {
		return errors.New("not a published video URL")
	}
	s.mu.Lock()
	use := s.files[id].use
	s.mu.Unlock()
	if use == nil {
		return errors.New("video is no longer published")
	}
	select {
	case <-use.read:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) Forget(mediaURL string) {
	id := strings.SplitN(strings.TrimPrefix(mediaURL, s.base+"/"), "/", 3)
	if len(id) != 3 {
		return
	}
	s.mu.Lock()
	delete(s.files, id[1])
	s.mu.Unlock()
}

// cleanName makes a title safe to use as a file name.
func cleanName(s string) string {
	s = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`\/:*?"<>|`, r) || r < 32 {
			return '_'
		}
		return r
	}, s)
	if s == "" {
		s = "video"
	}
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) != 4 || (parts[1] != "v" && parts[1] != "hls" && parts[1] != "audio" && parts[1] != "dash") {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	p := s.files[parts[2]]
	s.mu.Unlock()
	if p.file == nil || (parts[1] == "hls" && p.hls == nil) || ((parts[1] == "audio" || parts[1] == "dash") && p.file.audio == nil) {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if parts[1] == "hls" {
		p.hls.serve(w, r, parts[3])
		return
	}
	if parts[1] == "dash" {
		name := strings.TrimSuffix(parts[3], ".mpd")
		s.serveDASH(w, r, p.file, parts[2], name)
		return
	}

	f, buffered := p.file, p.mp4
	contentType := "video/mp4"
	if parts[1] == "audio" {
		f, buffered, contentType = f.audio, p.audio, "audio/mp4"
	}
	size := f.Layout.Size
	writeRange := f.WriteRange
	if buffered != nil {
		writeRange = buffered.writeRange
	}
	h := w.Header()
	h.Set("Accept-Ranges", "bytes")
	h.Set("Content-Type", contentType)
	start, end, status := int64(0), size, http.StatusOK
	if rh := r.Header.Get("Range"); rh != "" {
		var ok bool
		if start, end, ok = parseRange(rh, size); !ok {
			h.Set("Content-Range", fmt.Sprintf("bytes */%d", size))
			http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		status = http.StatusPartialContent
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, size))
	}
	h.Set("Content-Length", strconv.FormatInt(end-start, 10))
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	if parts[1] == "v" && p.use != nil {
		p.use.once.Do(func() { close(p.use.read) })
	}

	t0 := time.Now()
	cw := &countWriter{w: w}
	err := writeRange(r.Context(), cw, start, end)
	if err != nil && !errors.Is(err, context.Canceled) && r.Context().Err() == nil {
		log.Printf("serve %d-%d: %v", start, end-1, err)
	}
	log.Printf("%s %s range=%q sent %d bytes in %v", r.Method, parts[2], r.Header.Get("Range"), cw.n, time.Since(t0).Round(time.Millisecond))
}

// parseRange parses a single-range Range header into [start, end).
func parseRange(h string, size int64) (start, end int64, ok bool) {
	spec, found := strings.CutPrefix(h, "bytes=")
	if !found || strings.Contains(spec, ",") {
		return 0, 0, false
	}
	a, b, found := strings.Cut(strings.TrimSpace(spec), "-")
	if !found {
		return 0, 0, false
	}
	if a == "" { // suffix range: the last b bytes
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		return max(0, size-n), size, true
	}
	start, err := strconv.ParseInt(a, 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	end = size
	if b != "" {
		e, err := strconv.ParseInt(b, 10, 64)
		if err != nil || e < start {
			return 0, 0, false
		}
		end = min(e, size-1) + 1
	}
	return start, end, true
}

type countWriter struct {
	w interface{ Write([]byte) (int, error) }
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
