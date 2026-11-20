package stream

import (
	"bytes"
	"context"
	"io"
	"sort"
	"sync"
	"time"
)

const (
	mp4PrefetchLimit = 16
	mp4PrefetchBytes = 32 << 20
	mp4PrefetchPiece = 8 << 20
)

type mp4Stream struct {
	ctx      context.Context
	file     *File
	prefetch time.Duration

	mu      sync.Mutex
	cached  map[int][]byte
	cancel  context.CancelFunc
	anchor  int
	next    int
	reading map[int]int
	loading *mp4Load
}

type mp4Load struct {
	index  int
	cancel context.CancelFunc
}

func (m *mp4Stream) writeRange(ctx context.Context, w io.Writer, start, end int64) error {
	segments := m.file.Layout.Segments
	index := sort.Search(len(segments), func(i int) bool { return segments[i].Off+segments[i].Size > start })
	if index == len(segments) {
		return m.file.WriteRange(ctx, w, start, end)
	}
	readAhead := end-start >= 64<<10
	if headerEnd := min(segments[index].Off, end); start < headerEnd {
		if err := m.file.WriteRange(ctx, w, start, headerEnd); err != nil {
			return err
		}
		start = headerEnd
	}
	for ; start < end; index++ {
		segment := segments[index]
		stop := min(end, segment.Off+segment.Size)
		data := m.beginRead(index)
		if readAhead {
			m.warm(index)
		}
		if data != nil {
			if _, err := w.Write(data[start-segment.Off : stop-segment.Off]); err != nil {
				return err
			}
		} else {
			err := m.file.WriteRange(ctx, w, start, stop)
			m.endRead(index)
			if err != nil {
				return err
			}
		}
		start = stop
	}
	return nil
}

func (m *mp4Stream) beginRead(index int) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	if data := m.cached[index]; data != nil {
		return data
	}
	if m.loading != nil && m.loading.index == index {
		// Play immediately from the upstream instead of waiting for the
		// speculative read to finish buffering the whole fragment.
		m.loading.cancel()
	}
	if m.reading == nil {
		m.reading = make(map[int]int)
	}
	m.reading[index]++
	return nil
}

func (m *mp4Stream) endRead(index int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.reading[index] == 1 {
		delete(m.reading, index)
	} else {
		m.reading[index]--
	}
}

func (m *mp4Stream) warm(index int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil || m.cancel != nil && index >= m.anchor-3 && index < m.next {
		return
	}
	if m.cancel != nil {
		m.cancel()
	}
	segments := m.file.Layout.Segments
	end := index + 1
	var seconds float64
	var size int64
	for end < min(len(segments), index+1+mp4PrefetchLimit) && seconds < m.prefetch.Seconds() {
		segment := segments[end]
		if segment.Size > mp4PrefetchPiece || size+segment.Size > mp4PrefetchBytes {
			break
		}
		size += segment.Size
		seconds += segment.Duration
		end++
	}
	for cached := range m.cached {
		if cached <= index || cached >= end {
			delete(m.cached, cached)
		}
	}
	m.anchor, m.next = index, index+max(1, (end-index)/2)
	ctx, cancel := context.WithCancel(m.ctx)
	m.cancel = cancel
	go m.fill(ctx, index+1, end)
}

func (m *mp4Stream) fill(ctx context.Context, first, end int) {
	for index := first; index < end && ctx.Err() == nil; index++ {
		m.mu.Lock()
		if ctx.Err() != nil || m.cached[index] != nil || m.reading[index] > 0 {
			m.mu.Unlock()
			continue
		}
		loadCtx, cancel := context.WithCancel(ctx)
		load := &mp4Load{index: index, cancel: cancel}
		m.loading = load
		m.mu.Unlock()

		segment := m.file.Layout.Segments[index]
		buf := bytes.NewBuffer(make([]byte, 0, int(segment.Size)))
		err := m.file.WriteRange(loadCtx, buf, segment.Off, segment.Off+segment.Size)
		interrupted := loadCtx.Err() != nil
		cancel()

		m.mu.Lock()
		if m.loading == load {
			m.loading = nil
		}
		if err == nil && !interrupted && ctx.Err() == nil && m.reading[index] == 0 {
			if m.cached == nil {
				m.cached = make(map[int][]byte)
			}
			m.cached[index] = buf.Bytes()
		}
		m.mu.Unlock()
		if err != nil && !interrupted {
			return
		}
	}
}
