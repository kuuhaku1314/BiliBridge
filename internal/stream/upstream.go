// Package stream serves a merged DASH video+audio file over local HTTP,
// reading each byte range on demand from the upstream CDN.
package stream

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// stallTimeout aborts an upstream read that makes no progress, so a stuck
// CDN connection is replaced instead of freezing playback.
const stallTimeout = 15 * time.Second

var httpClient = &http.Client{Transport: &http.Transport{
	Proxy:                 nil, // the CDN must be reached directly, not through a system proxy
	ResponseHeaderTimeout: 10 * time.Second,
	IdleConnTimeout:       60 * time.Second,
	MaxIdleConnsPerHost:   4,
	ForceAttemptHTTP2:     true,
}}

// Upstream reads byte ranges of one remote file from a list of mirror URLs.
type Upstream struct {
	name    string
	header  http.Header
	refresh func(context.Context) ([]string, error)

	mu   sync.Mutex
	urls []string
}

// NewUpstream returns an upstream for the file at urls, best first. When
// every URL fails, refresh (if set) is called once for new ones, which
// covers URLs that have expired.
func NewUpstream(name string, urls []string, header http.Header, refresh func(context.Context) ([]string, error)) *Upstream {
	return &Upstream{name: name, urls: urls, header: header, refresh: refresh}
}

// Open returns the file's bytes from off up to end (exclusive), or to the
// end of the file if end is negative.
func (u *Upstream) Open(ctx context.Context, off, end int64) (io.ReadCloser, error) {
	var lastErr error
	for pass := 0; pass < 2; pass++ {
		u.mu.Lock()
		urls := u.urls
		u.mu.Unlock()
		for _, rawURL := range urls {
			rc, err := u.open(ctx, rawURL, off, end)
			if err == nil {
				u.prefer(rawURL)
				return rc, nil
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			log.Printf("%s: %v", u.name, err)
			lastErr = err
		}
		if pass > 0 || u.refresh == nil {
			break
		}
		log.Printf("%s: all URLs failed, fetching new ones", u.name)
		fresh, err := u.refresh(ctx)
		if err != nil {
			return nil, fmt.Errorf("%s: refreshing URLs: %w", u.name, err)
		}
		u.mu.Lock()
		u.urls = fresh
		u.mu.Unlock()
	}
	return nil, fmt.Errorf("%s: no URL works: %w", u.name, lastErr)
}

// prefer moves a working URL to the front of the list.
func (u *Upstream) prefer(rawURL string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for i, x := range u.urls {
		if x == rawURL {
			if i > 0 {
				u.urls = append([]string{x}, append(u.urls[:i:i], u.urls[i+1:]...)...)
			}
			return
		}
	}
}

func (u *Upstream) open(ctx context.Context, rawURL string, off, end int64) (io.ReadCloser, error) {
	host := rawURL
	if pu, err := url.Parse(rawURL); err == nil {
		host = pu.Host
	}
	rctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, rawURL, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header = u.header.Clone()
	if end < 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", off))
	} else {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, end-1))
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%s: %w", host, err)
	}
	if resp.StatusCode != http.StatusPartialContent ||
		!strings.HasPrefix(resp.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-", off)) {
		resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("%s: HTTP %s for range %d-", host, resp.Status, off)
	}
	t := time.AfterFunc(stallTimeout, cancel)
	t.Stop()
	return &stallReader{body: resp.Body, timer: t, cancel: cancel}, nil
}

// stallReader cancels its request when a Read blocks for stallTimeout.
type stallReader struct {
	body   io.ReadCloser
	timer  *time.Timer
	cancel context.CancelFunc
}

func (r *stallReader) Read(p []byte) (int, error) {
	r.timer.Reset(stallTimeout)
	n, err := r.body.Read(p)
	r.timer.Stop()
	return n, err
}

func (r *stallReader) Close() error {
	r.timer.Stop()
	r.cancel()
	return r.body.Close()
}
