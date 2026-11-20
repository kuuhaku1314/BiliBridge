package stream

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLiveFLVStartup(t *testing.T) {
	// The clock extends past 24 bits and the audio is ahead of video.
	first := uint32(1 << 24)
	prefix := []byte("FLV\x01\x05\x00\x00\x00\x09\x00\x00\x00\x00")
	prefix = appendFLVTestTag(prefix, 9, 0)
	prefix = appendFLVTestTag(prefix, 9, first)
	prefix = appendFLVTestTag(prefix, 8, first+6000)
	prefix = appendFLVTestTag(prefix, 9, first+5999)
	data := appendFLVTestTag(prefix, 9, first+6000)
	var scan flvStartup
	for n := 0; n < len(data); n++ {
		if scan.ready(data[:n]) {
			t.Fatalf("startup buffer released with only %d of %d bytes", n, len(data))
		}
	}
	if !scan.ready(data) {
		t.Fatal("startup buffer did not release after six seconds of video")
	}
}

func TestProxyLiveFLVReleasesPartialBufferOnTimeout(t *testing.T) {
	buffered := []byte("FLV\x01\x05\x00\x00\x00\x09\x00\x00\x00\x00")
	rest := appendFLVTestTag(nil, 9, 7000)
	reader, writer := io.Pipe()
	defer writer.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	response := &liveFlushRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		flushed:          make(chan struct{}),
	}
	done := make(chan struct{})
	go func() {
		proxyLiveFLV(ctx, response, reader, 80*time.Millisecond)
		close(done)
	}()
	if _, err := writer.Write(buffered); err != nil {
		t.Fatal(err)
	}
	select {
	case <-response.flushed:
		if got := response.Body.Bytes(); !bytes.Equal(got, buffered) {
			t.Fatalf("startup buffer = %x, want %x", got, buffered)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("startup buffer did not flush when upstream stopped sending data")
	}
	if _, err := writer.Write(rest); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	<-done
	if got, want := response.Body.Bytes(), append(buffered, rest...); !bytes.Equal(got, want) {
		t.Fatalf("proxied bytes = %x, want %x", got, want)
	}
}

func appendFLVTestTag(data []byte, kind byte, timestamp uint32) []byte {
	tag := []byte{kind, 0, 0, 1, byte(timestamp >> 16), byte(timestamp >> 8), byte(timestamp), byte(timestamp >> 24),
		0, 0, 0, 1, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(tag[12:], 12)
	return append(data, tag...)
}

type liveFlushRecorder struct {
	*httptest.ResponseRecorder
	flushed chan struct{}
}

func (r *liveFlushRecorder) Flush() {
	r.ResponseRecorder.Flush()
	close(r.flushed)
}
