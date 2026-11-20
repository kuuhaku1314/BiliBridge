package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bilibridge/internal/bili"
)

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
