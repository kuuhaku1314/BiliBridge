package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"bilibridge/internal/bili"
	"bilibridge/internal/danmaku"
	"bilibridge/internal/potplayer"
	"bilibridge/internal/stream"
	"golang.org/x/sys/windows"
)

var supportedPage = regexp.MustCompile(`^/(video/BV[0-9A-Za-z]{10}|bangumi/play/ep[0-9]+)/?$`)

type desktop struct {
	ctx    context.Context
	stream *stream.Server

	mu       sync.RWMutex
	cfg      desktopConfig
	client   *bili.Client
	liveOpen *liveOpening

	webMu     sync.Mutex
	webCancel context.CancelFunc

	albumMu sync.Mutex
	media   map[string]albumMedia
}

type albumMedia struct {
	preparedMedia
	cancel context.CancelFunc
	used   time.Time
}

func runDesktop() error {
	instance, err := windows.CreateMutex(nil, false, winText(`Local\BiliBridgeDesktop`))
	if instance != 0 {
		defer windows.CloseHandle(instance)
	}
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("无法创建 BiliBridge 单实例锁: %w", err)
	}
	trayUser.NewProc("SetProcessDpiAwarenessContext").Call(^uintptr(3))
	cfg, err := loadConfig(true)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp4", desktopAddr)
	if err != nil {
		return fmt.Errorf("BiliBridge 托盘端口无法打开: %w", err)
	}
	defer listener.Close()
	if err := registerNativeHost(); err != nil {
		return err
	}
	s, err := stream.Listen("127.0.0.1:0")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &desktop{ctx: ctx, stream: s, cfg: cfg, client: bili.NewClient(""), media: make(map[string]albumMedia)}
	server := &http.Server{Handler: d, ReadHeaderTimeout: 3 * time.Second}
	defer server.Close()
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Print(err)
		}
	}()
	return runTray(d)
}

func (d *desktop) settings() desktopConfig {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.cfg
}

func (d *desktop) setSettings(cfg desktopConfig) error {
	if err := saveConfig(cfg); err != nil {
		return err
	}
	d.mu.Lock()
	d.cfg = cfg
	d.mu.Unlock()
	return nil
}

func (d *desktop) account(cookie string) *bili.Client {
	d.mu.RLock()
	client := d.client
	d.mu.RUnlock()
	if keepAccount(client.Cookie, cookie) {
		return client
	}

	// Update the client and invalidate the album cache together, in the
	// same lock order as an album item resolving its playback URL.
	d.albumMu.Lock()
	defer d.albumMu.Unlock()
	d.mu.Lock()
	defer d.mu.Unlock()
	if keepAccount(d.client.Cookie, cookie) {
		return d.client
	}
	d.client = bili.NewClient(cookie)
	for key, item := range d.media {
		item.cancel()
		d.stream.Forget(item.URL)
		delete(d.media, key)
	}
	return d.client
}

func keepAccount(current, incoming string) bool {
	return current == incoming || (hasBiliSession(current) && !hasBiliSession(incoming))
}

func hasBiliSession(cookie string) bool {
	request := http.Request{Header: http.Header{"Cookie": {cookie}}}
	session, err := request.Cookie("SESSDATA")
	return err == nil && session.Value != ""
}

func (d *desktop) hasSession() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return hasBiliSession(d.client.Cookie)
}

func (d *desktop) verifyLogin(ctx context.Context, cookie string) bridgeReply {
	if !hasBiliSession(cookie) {
		return bridgeReply{Error: "请先在浏览器中登录 B 站"}
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if _, err := bili.NewClient(cookie).AccountID(ctx); err != nil {
		return bridgeReply{Error: "暂未确认 B 站登录状态，请在网页确认已登录后重试"}
	}
	d.account(cookie)
	return bridgeReply{OK: true}
}

func localhostProof(key, challenge string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(challenge))
	return hex.EncodeToString(mac.Sum(nil))
}

func (d *desktop) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Host != desktopAddr {
		http.NotFound(w, r)
		return
	}
	cfg := d.settings()
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet && r.URL.Path == "/ready" {
		challenge := r.Header.Get("X-BiliBridge-Challenge")
		if len(challenge) != 26 {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, localhostProof(cfg.Key, challenge))
		return
	}
	route, ok := strings.CutPrefix(r.URL.Path, "/"+cfg.Key+"/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch {
	case r.Method == http.MethodPost && route == "bridge":
		d.serveBridge(w, r)
	case r.Method == http.MethodGet && (route == "home.bbalbum" && cfg.Home ||
		route == "favorites.bbalbum" && cfg.Favorite || route == "live.bbalbum" && cfg.Live):
		d.serveCatalog(w, r, route, cfg.baseURL())
	case r.Method == http.MethodGet && strings.HasPrefix(route, "item/"):
		d.serveAlbumMedia(w, r, strings.TrimPrefix(route, "item/"), cfg)
	case cfg.Live && (r.Method == http.MethodGet || r.Method == http.MethodHead) && strings.HasPrefix(route, "live-stream/"):
		d.serveLive(w, r, strings.TrimPrefix(route, "live-stream/"))
	default:
		http.NotFound(w, r)
	}
}

func (d *desktop) serveBridge(w http.ResponseWriter, r *http.Request) {
	var request bridgeRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	reply := bridgeReply{Error: "无效的浏览器请求"}
	if r.Header.Get("Origin") == "" && decoder.Decode(&request) == nil && len(request.Cookie) <= 16000 &&
		!strings.ContainsAny(request.Cookie, "\r\n") {
		switch request.Action {
		case "sync":
			d.account(request.Cookie)
			reply = bridgeReply{OK: true}
		case "login":
			reply = d.verifyLogin(r.Context(), request.Cookie)
		case "play":
			reply = d.play(r.Context(), request)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(reply)
}

func desktopPrefs(quality, codec int) bili.Prefs {
	prefs := bili.Prefs{
		Codecs:   []int{bili.CodecAVC, bili.CodecHEVC, bili.CodecAV1},
		AllowHDR: quality == 0 || quality == 125 || quality == 126 || quality == 127,
	}
	if _, known := bili.QualityNames[quality]; known {
		prefs.MaxQuality = quality
	}
	switch codec {
	case bili.CodecHEVC:
		prefs.Codecs = []int{bili.CodecHEVC, bili.CodecAVC, bili.CodecAV1}
	case bili.CodecAV1:
		prefs.Codecs = []int{bili.CodecAV1, bili.CodecAVC, bili.CodecHEVC}
	}
	return prefs
}

func (d *desktop) play(requestCtx context.Context, request bridgeRequest) bridgeReply {
	u, err := url.Parse(request.Ref)
	if err == nil && u.Scheme == "https" && u.Host == "live.bilibili.com" && u.User == nil && liveRoomPage.MatchString(u.Path) {
		if roomID, err := strconv.ParseInt(strings.Trim(u.Path, "/"), 10, 64); err == nil {
			return d.playLive(requestCtx, request.Cookie, roomID, request.Quality)
		}
	}
	if err != nil || u.Scheme != "https" || u.Host != "www.bilibili.com" || u.User != nil || !supportedPage.MatchString(u.Path) {
		return bridgeReply{Error: "当前页面不是支持的 B 站视频或剧集"}
	}
	d.webMu.Lock()
	defer d.webMu.Unlock()
	if err := requestCtx.Err(); err != nil {
		return bridgeReply{Error: err.Error()}
	}
	client := d.account(request.Cookie)
	ctx, cancel := context.WithCancel(d.ctx)
	stop := context.AfterFunc(requestCtx, cancel)
	m, err := prepare(ctx, d.stream, client, u.String(), "", time.Minute, desktopPrefs(request.Quality, request.Codec), true)
	if err != nil || !stop() {
		cancel()
		if err == nil {
			d.stream.Forget(m.URL)
			err = context.Canceled
		}
		return bridgeReply{Error: err.Error()}
	}
	var requested, adopted bool
	defer func() {
		if !adopted {
			cancel()
			if requested {
				go d.releaseWebMedia(m.URL)
			} else {
				d.stream.Forget(m.URL)
			}
		}
	}()
	exe, err := potplayer.FindExe()
	if err != nil {
		return bridgeReply{Error: "找不到 PotPlayer 64 位"}
	}
	at := time.Duration(max(0, min(request.At, max(0, m.Duration.Seconds()-1))) * float64(time.Second))
	mediaURL, _ := url.Parse(m.URL)
	name := path.Base(mediaURL.Path)
	old, err := pauseCurrentPlayer(requestCtx, ctx)
	defer func() {
		if !adopted {
			old.resume()
		}
	}()
	if err != nil {
		return bridgeReply{Error: err.Error()}
	}
	if err := potplayer.OpenTracks(exe, m.URL, m.AudioURL); err != nil {
		return bridgeReply{Error: err.Error()}
	}
	requested = true
	ready, stopReady := context.WithTimeout(ctx, 16*time.Second)
	defer stopReady()
	if err := d.stream.WaitForVideoRead(ready, m.URL); err != nil {
		return bridgeReply{Error: "PotPlayer 未读取新视频"}
	}
	window, err := potplayer.FindPlaybackWindow(ready, 0, name)
	if err != nil {
		return bridgeReply{Error: "PotPlayer 窗口尚未就绪"}
	}
	if d.webCancel != nil {
		d.webCancel()
	}
	d.webCancel, adopted = cancel, true
	go d.watchWebMedia(ctx, window, name, m.URL, cancel)
	// Let PotPlayer finish opening both inputs before seeking.
	waitForPlayback := old.paused && old.window == window && strings.Contains(old.title, name)
	var initial time.Duration
	for {
		status, statusErr := window.Status()
		pos, positionErr := window.Position()
		playing := status == potplayer.Playing || (status == potplayer.Paused && !waitForPlayback)
		if statusErr == nil && positionErr == nil && playing &&
			(at == 0 || pos >= 250*time.Millisecond || status == potplayer.Paused) {
			initial = pos
			break
		}
		select {
		case <-ready.Done():
			return bridgeReply{Error: "PotPlayer 未能及时起播"}
		case <-time.After(120 * time.Millisecond):
		}
	}
	go d.overlay(ctx, client, m, window, name)
	if at > initial+200*time.Millisecond || at < initial-2*time.Second {
		// A seek can be processed even if the window message times out while
		// the player buffers. Check the resulting position before reporting it.
		_ = window.Seek(at)
		for {
			if pos, err := window.Position(); err == nil && pos >= at-time.Second && pos <= at+2*time.Second {
				break
			}
			select {
			case <-ready.Done():
				return bridgeReply{Error: "PotPlayer 已起播，但未确认网页续播位置"}
			case <-time.After(120 * time.Millisecond):
			}
		}
	}
	return bridgeReply{
		OK: true, Height: m.Video.Height, Quality: m.Video.ID,
		QualityLabel: bili.QualityNames[m.Video.ID], Codec: m.Video.CodecID,
	}
}

func (d *desktop) watchWebMedia(ctx context.Context, window potplayer.Window, name, mediaURL string, cancel context.CancelFunc) {
	defer d.releaseWebMedia(mediaURL)
	defer cancel()
	pid := window.PID()
	lastMatch := time.Now()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			if pid == 0 || window.PID() != pid {
				return
			}
			if strings.Contains(window.Title(), name) {
				lastMatch = now
			} else if now.Sub(lastMatch) >= 2*time.Second {
				return
			}
		}
	}
}

func (d *desktop) releaseWebMedia(mediaURL string) {
	// Keep serving the previous URL briefly while the old graph is being torn down.
	select {
	case <-time.After(2 * time.Second):
	case <-d.ctx.Done():
	}
	d.stream.Forget(mediaURL)
}

func (d *desktop) serveCatalog(w http.ResponseWriter, r *http.Request, route, prefix string) {
	d.mu.RLock()
	client := d.client
	d.mu.RUnlock()
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	var rows []catalogRow
	var err error
	if route == "live.bbalbum" {
		var rooms []bili.LiveRoom
		rooms, err = client.FollowedLive(ctx)
		rows = make([]catalogRow, 0, len(rooms))
		for _, room := range rooms {
			rows = append(rows, catalogRow{
				URL:   prefix + "/item/" + strconv.FormatInt(room.ID, 10) + ".bblive",
				Title: liveTitle(room), Author: room.Author, Thumbnail: room.Thumbnail(),
			})
		}
	} else {
		var items []bili.CatalogItem
		if route == "favorites.bbalbum" {
			items, err = client.Favorites(ctx)
		} else {
			items, err = client.Home(ctx)
		}
		rows = catalogRows(prefix, items)
	}
	if err != nil {
		log.Printf("catalog: %v", err)
		http.Error(w, "B站列表暂不可用", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rows)
}

type catalogRow struct {
	URL       string `json:"url"`
	Title     string `json:"title"`
	Author    string `json:"author"`
	Thumbnail string `json:"thumbnail"`
	Duration  int64  `json:"duration"`
}

func catalogRows(prefix string, items []bili.CatalogItem) []catalogRow {
	rows := make([]catalogRow, 0, len(items))
	for _, item := range items {
		author := item.Owner.Name
		if author == "" {
			author = item.Upper.Name
		}
		cover := item.Picture
		if cover == "" {
			cover = item.Cover
		}
		if strings.HasPrefix(cover, "//") {
			cover = "https:" + cover
		}
		rows = append(rows, catalogRow{URL: prefix + "/item/" + item.BVID + ".bbvideo",
			Title: item.Title, Author: author, Thumbnail: cover, Duration: int64(item.Duration) * 1000})
	}
	return rows
}

func (d *desktop) serveAlbumMedia(w http.ResponseWriter, r *http.Request, name string, cfg desktopConfig) {
	if stem, live := strings.CutSuffix(name, ".bblive"); live && cfg.Live {
		d.serveLiveItem(w, r, stem, cfg.baseURL())
		return
	}
	id, ok := strings.CutSuffix(name, ".bbvideo")
	if !ok || len(id) != 12 || !supportedPage.MatchString("/video/"+id) {
		http.Error(w, "无效的视频入口", http.StatusNotFound)
		return
	}
	separateAudio := r.URL.Query().Get("tracks") == "1"
	m, err := d.albumItem(id, separateAudio)
	if err != nil {
		log.Printf("album media: %v", err)
		http.Error(w, "B站流暂不可用", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	u := m.URL
	if separateAudio {
		u = d.stream.DASHURL(u)
	}
	json.NewEncoder(w).Encode(map[string]string{"url": u, "title": m.Title})
}

func (d *desktop) albumItem(bvid string, separateAudio bool) (preparedMedia, error) {
	d.albumMu.Lock()
	defer d.albumMu.Unlock()
	now := time.Now()
	key := bvid
	if separateAudio {
		key += ":tracks"
	}
	if old, ok := d.media[key]; ok {
		old.used = now
		d.media[key] = old
		return old.preparedMedia, nil
	}
	d.mu.RLock()
	client := d.client
	d.mu.RUnlock()
	ctx, cancel := context.WithCancel(d.ctx)
	m, err := prepare(ctx, d.stream, client, bvid, "", time.Minute, desktopPrefs(0, 0), separateAudio)
	if err != nil {
		cancel()
		return preparedMedia{}, err
	}
	if len(d.media) >= 8 {
		var oldest string
		for k, item := range d.media {
			if oldest == "" || item.used.Before(d.media[oldest].used) {
				oldest = k
			}
		}
		item := d.media[oldest]
		item.cancel()
		d.stream.Forget(item.URL)
		delete(d.media, oldest)
	}
	d.media[key] = albumMedia{preparedMedia: m, cancel: cancel, used: now}
	go func() {
		wait, stop := context.WithTimeout(ctx, 20*time.Second)
		defer stop()
		window, err := potplayer.FindPlaybackWindow(wait, 0, m.Title)
		if err == nil {
			d.overlay(ctx, client, m, window, m.Title)
		}
	}()
	return m, nil
}

func (d *desktop) overlay(ctx context.Context, client *bili.Client, m preparedMedia, w potplayer.Window, title string) {
	danmaku.Show(ctx, w, title, m.Video.Width, m.Video.Height,
		func() bool { return d.settings().Danmaku },
		func(ctx context.Context, segment int) ([]bili.Danmaku, error) {
			if time.Duration(segment-1)*6*time.Minute > m.Duration {
				return nil, nil
			}
			return client.DanmakuSegment(ctx, m.CID, m.AID, segment)
		})
}
