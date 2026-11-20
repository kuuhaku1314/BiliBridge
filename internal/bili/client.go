// Package bili is a small client for the Bilibili web APIs BiliBridge uses.
package bili

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

const (
	UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/157.0.0.0 Safari/537.36"
	Referer   = "https://www.bilibili.com/"
)

// Client calls the Bilibili APIs, optionally as a logged-in user.
type Client struct {
	HTTP   *http.Client
	Cookie string // raw Cookie header from the browser; empty for anonymous

	wbiMu   sync.Mutex
	wbiKey  string
	wbiTime time.Time
}

// NewClient returns a client that sends cookie with API requests.
func NewClient(cookie string) *Client {
	return &Client{HTTP: &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" || req.URL.Host != via[0].URL.Host || len(via) >= 5 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}, Cookie: cookie}
}

// MediaHeader returns the headers the video CDN requires.
func MediaHeader() http.Header {
	return http.Header{"User-Agent": {UserAgent}, "Referer": {Referer}}
}

// APIError is a response whose code is not 0.
type APIError struct {
	Code    int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("bilibili API error %d: %s", e.Code, e.Message) }

type envelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
	Result  json.RawMessage `json:"result"` // used instead of data by the pgc APIs
}

func (c *Client) fetch(ctx context.Context, rawURL string) (*envelope, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header = MediaHeader()
	if c.Cookie != "" {
		req.Header.Set("Cookie", c.Cookie)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bilibili API: HTTP %s for %s", resp.Status, req.URL.Path)
	}
	var env envelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("bilibili API: decoding %s: %w", req.URL.Path, err)
	}
	return &env, nil
}

// get fetches rawURL and decodes its data (or result) into out.
func (c *Client) get(ctx context.Context, rawURL string, out any) error {
	env, err := c.fetch(ctx, rawURL)
	if err != nil {
		return err
	}
	if env.Code != 0 {
		return &APIError{Code: env.Code, Message: env.Message}
	}
	data := env.Data
	if len(data) == 0 || string(data) == "null" {
		data = env.Result
	}
	return json.Unmarshal(data, out)
}
