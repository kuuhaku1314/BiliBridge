package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bilibridge/internal/bili"
)

type liveTestTransport func(*http.Request) (*http.Response, error)

func (f liveTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDesktopPlaybackQualityByEntryPoint(t *testing.T) {
	info := bili.PlayInfo{Dash: &bili.Dash{
		Video: []bili.DashStream{
			{ID: 32, CodecID: bili.CodecAVC},
			{ID: 64, CodecID: bili.CodecAVC},
			{ID: 64, CodecID: bili.CodecHEVC},
			{ID: 80, CodecID: bili.CodecAVC},
			{ID: 125, CodecID: bili.CodecHEVC},
		},
		Audio: []bili.DashStream{{ID: 30280}},
	}}
	for _, test := range []struct {
		name                   string
		quality, codec         int
		wantQuality, wantCodec int
	}{
		{"web selection", 64, bili.CodecHEVC, 64, bili.CodecHEVC},
		{"web codec unavailable", 64, bili.CodecAV1, 64, bili.CodecAVC},
		{"web quality unavailable", 74, bili.CodecHEVC, 64, bili.CodecHEVC},
		{"menu selects best", 0, 0, 125, bili.CodecHEVC},
	} {
		t.Run(test.name, func(t *testing.T) {
			video, _, err := info.Pick(desktopPrefs(test.quality, test.codec))
			if err != nil {
				t.Fatal(err)
			}
			if video.ID != test.wantQuality || video.CodecID != test.wantCodec {
				t.Fatalf("selected quality %d and codec %d, want %d and %d", video.ID, video.CodecID, test.wantQuality, test.wantCodec)
			}
		})
	}
}

func TestBrowserSyncRetainsAlbumSession(t *testing.T) {
	d := desktop{cfg: desktopConfig{Key: "test-key"}, client: bili.NewClient("")}
	send := func(body string) bridgeReply {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, d.cfg.baseURL()+"/bridge", strings.NewReader(body))
		rec := httptest.NewRecorder()
		d.ServeHTTP(rec, req)
		var reply bridgeReply
		if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		return reply
	}
	if reply := send(`{"action":"login","cookie":"buvid3=guest"}`); reply.OK {
		t.Fatal("guest cookies were accepted as a login")
	}
	if reply := send(`{"action":"sync","cookie":"SESSDATA=fixture; buvid3=account"}`); !reply.OK || !d.hasSession() {
		t.Fatal("browser session did not reach the desktop")
	}
	client := d.client
	if reply := send(`{"action":"sync","cookie":"buvid3=guest"}`); !reply.OK || d.client != client {
		t.Fatal("a guest tab replaced the signed-in album client")
	}
}

func TestLiveItemReturnsCoverAndPlaybackURL(t *testing.T) {
	const stem = "21452505-q250-ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	d := desktop{
		cfg: desktopConfig{Key: "test-key", Live: true},
		liveOpen: &liveOpening{
			name: stem,
			room: bili.LiveRoom{ID: 21452505, Title: "GTA6", UserCover: json.RawMessage(`"//i0.hdslb.com/room.jpg"`)},
		},
	}
	request := httptest.NewRequest(http.MethodGet, d.cfg.baseURL()+"/item/"+stem+".bblive?tracks=1", nil)
	response := httptest.NewRecorder()
	d.ServeHTTP(response, request)
	var item map[string]string
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &item) != nil {
		t.Fatalf("unexpected live item response: %d %s", response.Code, response.Body.String())
	}
	if item["url"] != d.cfg.baseURL()+"/live-stream/"+stem+".flv" ||
		item["title"] != "[直播] GTA6" || item["thumbnail"] != "https://i0.hdslb.com/room.jpg" {
		t.Fatalf("unexpected live item: %v", item)
	}
	if id, quality, ok := liveRoomRef(stem); !ok || id != 21452505 || quality != 250 {
		t.Fatalf("live URL lost requested quality: %d %d %t", id, quality, ok)
	}
}

func TestLiveHLSItemHandsPlaylistToPotPlayer(t *testing.T) {
	const stem = "308543-q10000-ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/live/video.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Write([]byte("#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:1.0,\nfragment.m4s\n"))
		case "/live/fragment.m4s":
			if r.Header.Get("Range") != "bytes=0-2" {
				t.Errorf("upstream Range = %q", r.Header.Get("Range"))
			}
			w.Header().Set("Content-Range", "bytes 0-2/20")
			w.WriteHeader(http.StatusPartialContent)
			w.Write([]byte("abc"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer cdn.Close()
	opening := &liveOpening{
		name: stem, room: bili.LiveRoom{ID: 308543, Title: "GTA6"},
		playback: bili.LivePlayback{HLSURL: cdn.URL + "/live/video.m3u8", Quality: 250, QualityLabel: "超清"},
		started:  make(chan struct{}),
	}
	d := desktop{cfg: desktopConfig{Key: "test-key", Live: true}, liveOpen: opening}
	request := httptest.NewRequest(http.MethodGet, d.cfg.baseURL()+"/item/"+stem+".bblive?tracks=1", nil)
	response := httptest.NewRecorder()
	d.ServeHTTP(response, request)
	var item map[string]string
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &item) != nil ||
		!strings.HasPrefix(item["url"], d.cfg.baseURL()+"/live-hls/"+stem+"/") ||
		!strings.HasSuffix(item["url"], "/index.m3u8") || item["title"] != "[直播] GTA6" {
		t.Fatalf("unexpected HLS item: %d %s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	d.ServeHTTP(response, httptest.NewRequest(http.MethodGet, item["url"], nil))
	playlist := response.Body.String()
	lines := strings.Split(strings.TrimSpace(playlist), "\n")
	if response.Code != http.StatusOK || len(lines) != 4 ||
		!strings.Contains(lines[1], `URI="`+d.cfg.baseURL()+"/live-hls/") ||
		!strings.HasPrefix(lines[3], d.cfg.baseURL()+"/live-hls/") {
		t.Fatalf("expected local playlist URLs: %d %s", response.Code, playlist)
	}
	segment := httptest.NewRequest(http.MethodGet, lines[3], nil)
	segment.Header.Set("Range", "bytes=0-2")
	response = httptest.NewRecorder()
	d.ServeHTTP(response, segment)
	if response.Code != http.StatusPartialContent || response.Body.String() != "abc" || response.Header().Get("Content-Range") != "bytes 0-2/20" {
		t.Fatalf("unexpected HLS segment: %d %s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	unauthorized := strings.TrimSuffix(lines[3], "/media.m4s") + "/other.m4s"
	d.ServeHTTP(response, httptest.NewRequest(http.MethodGet, unauthorized, nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("altered HLS link returned HTTP %d", response.Code)
	}
	select {
	case <-opening.started:
		if !opening.source.OK || opening.source.Quality != 250 || opening.source.QualityLabel != "超清" {
			t.Fatalf("unexpected opening reply: %+v", opening.source)
		}
	default:
		t.Fatal("web playback was left waiting for the FLV proxy")
	}

	client := bili.NewClient("SESSDATA=fixture")
	client.HTTP.Transport = liveTestTransport(func(r *http.Request) (*http.Response, error) {
		var body string
		switch r.URL.Path {
		case "/room/v1/Room/get_info":
			body = `{"code":0,"data":{"room_id":308543,"title":"GTA6","live_status":1}}`
		case "/xlive/web-room/v2/index/getRoomPlayInfo":
			body = `{"code":0,"data":{"live_status":1,"playurl_info":{"playurl":{"stream":[
				{"protocol_name":"http_hls","format":[{"format_name":"fmp4","codec":[
					{"base_url":"/live/video.m3u8","current_qn":250,"url_info":[{"host":"` + cdn.URL + `"}]}
				]}]}
			]}}}}`
		default:
			t.Errorf("unexpected album request: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	album := desktop{cfg: d.cfg, client: client}
	response = httptest.NewRecorder()
	album.ServeHTTP(response, httptest.NewRequest(http.MethodGet, album.cfg.baseURL()+"/item/308543.bblive?tracks=1", nil))
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &item) != nil ||
		!strings.HasPrefix(item["url"], album.cfg.baseURL()+"/live-hls/308543/") || item["title"] != "[直播] GTA6" {
		t.Fatalf("followed-live album did not select HLS: %d %s", response.Code, response.Body.String())
	}
}
