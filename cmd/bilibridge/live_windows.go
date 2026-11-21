package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"bilibridge/internal/bili"
	"bilibridge/internal/potplayer"
	"bilibridge/internal/stream"
)

var liveRoomPage = regexp.MustCompile(`^/[1-9][0-9]{0,18}/?$`)
var liveStreamName = regexp.MustCompile(`^([1-9][0-9]{0,18})(?:-(?:q([1-9][0-9]{0,4})-)?[A-Z2-7]{26,64})?$`)

type liveOpening struct {
	name     string
	room     bili.LiveRoom
	playback bili.LivePlayback
	started  chan struct{}
	once     sync.Once
	source   bridgeReply
}

func (o *liveOpening) start(quality int, label string) {
	o.once.Do(func() {
		o.source = bridgeReply{OK: true, Quality: quality, QualityLabel: label}
		close(o.started)
	})
}

func liveTitle(room bili.LiveRoom) string {
	for _, title := range []string{room.Title, room.Author} {
		if title = strings.TrimSpace(title); title != "" {
			return "[直播] " + title
		}
	}
	return "[直播] " + strconv.FormatInt(room.ID, 10)
}

func liveRoomRef(stem string) (int64, int, bool) {
	match := liveStreamName.FindStringSubmatch(stem)
	if match == nil {
		return 0, 0, false
	}
	id, err := strconv.ParseInt(match[1], 10, 64)
	quality, _ := strconv.Atoi(match[2])
	return id, quality, err == nil
}

func (d *desktop) playLive(requestCtx context.Context, cookie string, roomID int64, quality int) bridgeReply {
	d.webMu.Lock()
	defer d.webMu.Unlock()
	cfg := d.settings()
	if !cfg.Live {
		return bridgeReply{Error: "请先在 BiliBridge 托盘中启用「关注的直播」"}
	}
	ctx, cancel := context.WithTimeout(requestCtx, 24*time.Second)
	defer cancel()
	client := d.account(cookie)
	room, err := client.LiveRoomInfo(ctx, roomID)
	if err != nil {
		return bridgeReply{Error: "无法获取直播间信息"}
	}
	if room.Status != 1 {
		return bridgeReply{Error: "直播间当前未开播"}
	}
	exe, err := potplayer.FindExe()
	if err != nil {
		return bridgeReply{Error: "找不到 PotPlayer 64 位"}
	}
	playback, err := client.ResolveLive(ctx, room.ID, quality)
	if err != nil {
		return bridgeReply{Error: err.Error()}
	}
	name := strconv.FormatInt(room.ID, 10)
	if quality > 0 && quality <= 30000 {
		name += "-q" + strconv.Itoa(quality)
	}
	opening := &liveOpening{name: name + "-" + rand.Text(), room: room, playback: playback, started: make(chan struct{})}
	d.mu.Lock()
	d.liveOpen = opening
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.liveOpen = nil
		d.mu.Unlock()
	}()
	old, err := pauseCurrentPlayer(ctx, d.ctx)
	opened := false
	defer func() {
		if !opened {
			old.resume()
		}
	}()
	if err != nil {
		return bridgeReply{Error: err.Error()}
	}
	if err := potplayer.OpenMedia(exe, cfg.baseURL()+"/item/"+opening.name+".bblive"); err != nil {
		return bridgeReply{Error: err.Error()}
	}
	select {
	case <-opening.started:
	case <-ctx.Done():
		return bridgeReply{Error: "PotPlayer 未能读取直播源"}
	}
	window, err := potplayer.FindPlaybackWindow(ctx, 0, liveTitle(room))
	if err != nil {
		return bridgeReply{Error: "PotPlayer 直播窗口尚未就绪"}
	}
	tick := time.NewTicker(120 * time.Millisecond)
	defer tick.Stop()
	for {
		if status, err := window.Status(); err == nil && status == potplayer.Playing {
			if d.webCancel != nil {
				d.webCancel()
				d.webCancel = nil
			}
			opened = true
			return opening.source
		}
		select {
		case <-ctx.Done():
			return bridgeReply{Error: "PotPlayer 未能及时起播"}
		case <-tick.C:
		}
	}
}

func (d *desktop) livePlayback(stem string) (*bili.Client, *liveOpening) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if opening := d.liveOpen; opening != nil && opening.name == stem {
		return d.client, opening
	}
	return d.client, nil
}

func (d *desktop) serveLiveItem(w http.ResponseWriter, r *http.Request, stem, base string) {
	id, quality, ok := liveRoomRef(stem)
	if !ok {
		http.NotFound(w, r)
		return
	}
	client, opening := d.livePlayback(stem)
	var room bili.LiveRoom
	var playback bili.LivePlayback
	if opening != nil {
		room = opening.room
		playback = opening.playback
	} else {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		room, _ = client.LiveRoomInfo(ctx, id)
		cancel()
		ctx, cancel = context.WithTimeout(r.Context(), 12*time.Second)
		var err error
		playback, err = client.ResolveLive(ctx, id, quality)
		cancel()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
	}
	info := map[string]string{"url": base + "/live-stream/" + stem + ".flv"}
	if playback.HLSURL != "" {
		info["url"] = d.liveHLSURL(base, stem, "index.m3u8", playback.HLSURL)
	}
	if room.ID != 0 {
		info["title"] = liveTitle(room)
		info["thumbnail"] = room.Thumbnail()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}

func (d *desktop) serveLive(w http.ResponseWriter, r *http.Request, name string) {
	stem, flv := strings.CutSuffix(name, ".flv")
	room, quality, ok := liveRoomRef(stem)
	if !flv || !ok {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Type", "video/x-flv")
		return
	}
	client, opening := d.livePlayback(stem)
	body, err := client.OpenLive(r.Context(), room, quality)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "video/x-flv")
	if opening != nil {
		w = liveStartWriter{ResponseWriter: w, opening: opening, source: body}
	}
	stream.ProxyLiveFLV(r.Context(), w, body)
}

type liveStartWriter struct {
	http.ResponseWriter
	opening *liveOpening
	source  *bili.LiveStream
}

func (w liveStartWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if n > 0 && err == nil {
		w.opening.start(w.source.Quality, w.source.QualityLabel)
	}
	return n, err
}

func (w liveStartWriter) Flush() {
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

type pausedPlayer struct {
	window potplayer.Window
	title  string
	paused bool
}

func pauseCurrentPlayer(requestCtx, appCtx context.Context) (pausedPlayer, error) {
	old := pausedPlayer{window: potplayer.FindSingleWindow()}
	if old.window == 0 {
		return old, nil
	}
	old.title = old.window.Title()
	if status, err := old.window.Status(); err != nil || status != potplayer.Playing || old.window.SetStatus(potplayer.Paused) != nil {
		return old, nil
	}
	old.paused = true
	// Allow the video filter to finish its in-flight frame requests.
	select {
	case <-requestCtx.Done():
		return old, requestCtx.Err()
	case <-appCtx.Done():
		return old, appCtx.Err()
	case <-time.After(500 * time.Millisecond):
		return old, nil
	}
}

func (old pausedPlayer) resume() {
	if old.paused && old.window.Title() == old.title {
		_ = old.window.SetStatus(potplayer.Playing)
	}
}
