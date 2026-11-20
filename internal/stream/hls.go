package stream

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"math"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxHLSSegmentSize = 32 << 20
	hlsCacheSlots     = 24
	hlsCacheSlotSize  = 8 << 20
	hlsPrefetchLimit  = 16
	hlsPrefetchBytes  = 32 << 20
)

type hlsStream struct {
	file     *File
	ffmpeg   string
	playlist []byte
	workers  chan struct{}
	cache    [hlsCacheSlots]hlsCacheSlot

	ctx            context.Context
	prefetch       time.Duration
	warmMu         sync.Mutex
	warmCancel     context.CancelFunc
	warmAnchor     int
	warmNextUpdate int
}

type hlsCacheSlot struct {
	busy  chan struct{}
	index int
	data  []byte
}

func newHLS(ctx context.Context, f *File, ffmpeg string, prefetch time.Duration) (*hlsStream, error) {
	var entries strings.Builder
	var target int
	for i, segment := range f.Layout.Segments {
		if segment.Size <= 0 || segment.Size > maxHLSSegmentSize || segment.Duration <= 0 {
			return nil, fmt.Errorf("hls: unsupported size or duration for segment %d", i)
		}
		target = max(target, int(math.Ceil(segment.Duration)))
		fmt.Fprintf(&entries, "#EXTINF:%.6f,\npart-%d.ts\n", segment.Duration, i)
	}
	if target == 0 {
		return nil, fmt.Errorf("hls: no media segments")
	}
	playlist := fmt.Sprintf("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n%s#EXT-X-ENDLIST\n", target, entries.String())
	h := &hlsStream{
		file: f, ffmpeg: ffmpeg, playlist: []byte(playlist), workers: make(chan struct{}, 4),
		ctx: ctx, prefetch: prefetch,
	}
	for i := range h.cache {
		h.cache[i].busy = make(chan struct{}, 1)
	}
	return h, nil
}

func (h *hlsStream) serve(w http.ResponseWriter, r *http.Request, name string) {
	if strings.HasSuffix(name, ".m3u8") {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(h.playlist))
		return
	}
	indexText, prefixOK := strings.CutPrefix(name, "part-")
	indexText, suffixOK := strings.CutSuffix(indexText, ".ts")
	index, err := strconv.Atoi(indexText)
	if !prefixOK || !suffixOK || err != nil || index < 0 || index >= len(h.file.Layout.Segments) {
		http.NotFound(w, r)
		return
	}
	data, err := h.segment(r.Context(), index)
	if err != nil {
		if r.Context().Err() == nil {
			log.Printf("hls: segment %d: %v", index, err)
			http.Error(w, "segment unavailable", http.StatusBadGateway)
		}
		return
	}
	w.Header().Set("Content-Type", "video/mp2t")
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
	if r.Method == http.MethodGet && r.Context().Err() == nil {
		h.warm(index)
	}
}

// prefetchEnd chooses a bounded window after the requested segment.
func (h *hlsStream) prefetchEnd(index int) int {
	segments := h.file.Layout.Segments
	end := index + 1
	var seconds float64
	var size int64
	for end < min(len(segments), index+1+hlsPrefetchLimit) && seconds < h.prefetch.Seconds() {
		seg := segments[end]
		if seg.Size > hlsCacheSlotSize || size+seg.Size > hlsPrefetchBytes {
			break
		}
		seconds += seg.Duration
		size += seg.Size
		end++
	}
	return end
}

func (h *hlsStream) warm(index int) {
	if h.prefetch <= 0 || h.ctx.Err() != nil {
		return
	}
	h.warmMu.Lock()
	defer h.warmMu.Unlock()
	if h.warmCancel != nil {
		// Allow duplicate and slightly out-of-order probes. A larger jump,
		// or a request halfway through the window, moves the prefetch target.
		if index >= h.warmAnchor-3 && index < h.warmNextUpdate {
			return
		}
		h.warmCancel()
	}
	end := h.prefetchEnd(index)
	ctx, cancel := context.WithCancel(h.ctx)
	h.warmCancel = cancel
	h.warmAnchor = index
	h.warmNextUpdate = index + max(1, (end-index)/2)
	go func() {
		defer cancel()
		for next := index + 1; next < end && ctx.Err() == nil; next++ {
			// Leave the other workers free to serve player requests.
			if _, err := h.segment(ctx, next); err != nil {
				return
			}
		}
	}()
}

func (h *hlsStream) segment(ctx context.Context, index int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// PotPlayer can probe and then play the same segment concurrently.
	slot := &h.cache[index%hlsCacheSlots]
	select {
	case slot.busy <- struct{}{}:
		defer func() { <-slot.busy }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if slot.index == index && slot.data != nil {
		return slot.data, nil
	}
	select {
	case h.workers <- struct{}{}:
		defer func() { <-h.workers }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	l := h.file.Layout
	seg := l.Segments[index]
	input := bytes.NewBuffer(make([]byte, 0, l.InitSize+int(seg.Size)))
	input.Write(l.Header[:l.InitSize])
	if err := h.file.WriteRange(ctx, input, seg.Off, seg.Off+seg.Size); err != nil {
		return nil, err
	}

	// Preserve timestamps across independently generated segments. PotPlayer
	// uses the playlist timeline to seek into the resulting transport stream.
	cmd := exec.CommandContext(ctx, h.ffmpeg,
		"-nostdin", "-hide_banner", "-loglevel", "error", "-copyts", "-i", "pipe:0",
		"-map", "0:v:0", "-map", "0:a:0", "-c", "copy",
		"-muxdelay", "0", "-mpegts_copyts", "1", "-f", "mpegts", "pipe:1")
	hideFFmpegWindow(cmd)
	var stderr bytes.Buffer
	cmd.Stdin = input
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("ffmpeg returned an empty segment")
	}
	if len(data) <= hlsCacheSlotSize {
		slot.index, slot.data = index, data
	}
	log.Printf("hls: segment %d read %d bytes, remuxed to %d bytes", index, seg.Size, len(data))
	return data, nil
}
