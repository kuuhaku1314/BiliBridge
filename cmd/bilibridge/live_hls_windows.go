package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"bilibridge/internal/bili"
)

const livePlaylistLimit = 1 << 20

var liveHLSURI = regexp.MustCompile(`\bURI="([^"]+)"`)

func (d *desktop) liveHLSProof(stem, name, token string) string {
	// Keep the signing key separate from the key exposed in localhost URLs.
	d.hlsOnce.Do(func() { d.hlsKey = rand.Text() })
	return localhostProof(d.hlsKey, stem+"/"+name+"/"+token)
}

func (d *desktop) liveHLSURL(base, stem, name, upstream string) string {
	token := base64.RawURLEncoding.EncodeToString([]byte(upstream))
	proof := d.liveHLSProof(stem, name, token)
	return base + "/live-hls/" + stem + "/" + proof + "/" + token + "/" + name
}

func (d *desktop) serveLiveHLS(w http.ResponseWriter, r *http.Request, route, base string) {
	parts := strings.SplitN(route, "/", 4)
	if len(parts) != 4 || !hmac.Equal([]byte(parts[1]), []byte(d.liveHLSProof(parts[0], parts[3], parts[2]))) {
		http.NotFound(w, r)
		return
	}
	stem, name := parts[0], parts[3]
	room, _, ok := liveRoomRef(stem)
	upstream, err := base64.RawURLEncoding.DecodeString(parts[2])
	if !ok || err != nil {
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	playlist := path.Ext(name) == ".m3u8"
	method, byteRange := r.Method, r.Header.Get("Range")
	if playlist {
		method, byteRange = http.MethodGet, ""
	}
	resp, err := bili.FetchLiveMedia(ctx, room, method, string(upstream), byteRange)
	if err != nil {
		http.Error(w, "直播 HLS 线路暂时无法连接", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	playlist = playlist || strings.Contains(resp.Header.Get("Content-Type"), "mpegurl")
	if playlist && resp.StatusCode == http.StatusOK && method == http.MethodGet {
		body, err := d.rewriteLivePlaylist(resp, base, stem)
		if err != nil {
			http.Error(w, "直播 HLS 播放列表无效", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if r.Method != http.MethodHead {
			if _, err := io.WriteString(w, body); err == nil && name == "index.m3u8" {
				if _, opening := d.livePlayback(stem); opening != nil {
					opening.start(opening.playback.Quality, opening.playback.QualityLabel)
				}
			}
		}
		return
	}

	for _, name := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges"} {
		if value := resp.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if r.Method != http.MethodHead {
		io.Copy(w, resp.Body)
	}
}

func (d *desktop) rewriteLivePlaylist(resp *http.Response, base, stem string) (string, error) {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, livePlaylistLimit+1))
	if err != nil || len(raw) > livePlaylistLimit || !strings.HasPrefix(string(raw), "#EXTM3U") {
		return "", errors.New("invalid HLS playlist")
	}
	var referenceErr error
	rewrite := func(reference string) string {
		u, err := resp.Request.URL.Parse(reference)
		if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
			referenceErr = errors.New("invalid HLS resource URL")
			return ""
		}
		ext := strings.ToLower(path.Ext(u.Path))
		switch ext {
		case ".m3u8", ".m4s", ".mp4", ".ts", ".aac", ".vtt":
		default:
			ext = ".bin"
		}
		return d.liveHLSURL(base, stem, "media"+ext, u.String())
	}
	lines := strings.Split(string(raw), "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			lines[i] = liveHLSURI.ReplaceAllStringFunc(line, func(attribute string) string {
				return `URI="` + rewrite(attribute[5:len(attribute)-1]) + `"`
			})
		} else if line != "" {
			lines[i] = rewrite(line)
		}
	}
	return strings.Join(lines, "\n"), referenceErr
}
