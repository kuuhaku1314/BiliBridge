package stream

import (
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"time"
)

const (
	liveBufferDuration = 6 * time.Second
	liveBufferWait     = 10 * time.Second
	liveBufferLimit    = 12 << 20
)

// ProxyLiveFLV lets the player start behind the live edge. Assemble the
// initial buffer from media timestamps, then forward the original bytes.
// It takes ownership of the upstream response body.
func ProxyLiveFLV(ctx context.Context, w http.ResponseWriter, body io.ReadCloser) {
	proxyLiveFLV(ctx, w, body, liveBufferWait)
}

func proxyLiveFLV(ctx context.Context, w http.ResponseWriter, body io.ReadCloser, wait time.Duration) {
	defer body.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	chunks := make(chan []byte)
	go func() {
		defer close(chunks)
		// The unbuffered channel hands off one block while the other is filled.
		var blocks [2][32 << 10]byte
		for i := 0; ; i = 1 - i {
			n, err := body.Read(blocks[i][:])
			if n != 0 {
				select {
				case chunks <- blocks[i][:n]:
				case <-ctx.Done():
					return
				}
			}
			if err != nil || ctx.Err() != nil {
				return
			}
		}
	}()

	timer := time.NewTimer(wait)
	defer timer.Stop()
	deadline := timer.C
	var prefix []byte
	var scan flvStartup
	release := func() error {
		timer.Stop()
		deadline = nil
		if len(prefix) == 0 {
			return nil
		}
		_, err := w.Write(prefix)
		prefix = nil
		if flusher, ok := w.(http.Flusher); err == nil && ok {
			flusher.Flush()
		}
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			if release() != nil {
				return
			}
		case chunk, ok := <-chunks:
			if !ok {
				release()
				return
			}
			if deadline != nil {
				if len(prefix)+len(chunk) <= liveBufferLimit {
					prefix = append(prefix, chunk...)
					if !scan.ready(prefix) {
						continue
					}
					chunk = nil
				}
				if release() != nil {
					return
				}
			}
			if len(chunk) != 0 {
				if _, err := w.Write(chunk); err != nil {
					return
				}
			}
		}
	}
}

type flvStartup struct {
	next      int
	track     byte
	firstTime uint32
	started   bool
}

// ready examines complete tags only. Malformed headers fall back to ordinary
// pass-through, as do streams that exceed the startup time or byte budget.
func (s *flvStartup) ready(data []byte) bool {
	if s.next == 0 {
		if len(data) < 9 {
			return false
		}
		offset := binary.BigEndian.Uint32(data[5:9])
		if string(data[:3]) != "FLV" || offset < 9 || offset > liveBufferLimit-4 {
			return true
		}
		s.next = int(offset) + 4 // Skip PreviousTagSize0.
		s.track = 8              // Audio-only FLV.
		if data[4]&1 != 0 {
			s.track = 9
		}
	}
	for len(data)-s.next >= 11 {
		tag := data[s.next:]
		size := int(flvUint24(tag[1:4]))
		end := s.next + 11 + size + 4
		if end > len(data) {
			return false
		}
		s.next = end
		if tag[0]&31 != s.track || size == 0 {
			continue
		}
		timestamp := uint32(tag[7])<<24 | flvUint24(tag[4:7])
		if !s.started && timestamp != 0 {
			s.firstTime, s.started = timestamp, true
		}
		if s.started && int32(timestamp-s.firstTime) >= int32(liveBufferDuration.Milliseconds()) {
			return true
		}
	}
	return false
}

func flvUint24(b []byte) uint32 {
	return uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])
}
