package bili

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFollowedLiveListsOnlyOnAirRooms(t *testing.T) {
	client := NewClient("SESSDATA=fixture")
	client.HTTP.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.live.bilibili.com" || r.URL.Query().Get("hit_ab") != "true" ||
			r.Header.Get("Cookie") != "SESSDATA=fixture" {
			t.Error("unexpected followed live request")
		}
		body := `{"code":0,"data":{"rooms":[{"room_id":8,"live_status":0},
			{"room_id":9,"live_status":1,"uname":"主播","cover_from_user":0,"face":"https://i0.hdslb.com/avatar.jpg"},
			{"room_id":10,"live_status":2}],"list":[{"room_id":9,"live_status":1},
			{"room_id":11,"live_status":1,"cover_from_user":1,"user_cover":"//i0.hdslb.com/room.jpg",
			"keyframe":"https://i0.hdslb.com/keyframe.jpg","face":"https://i0.hdslb.com/avatar.jpg"}]}}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	rooms, err := client.FollowedLive(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rooms) != 2 || rooms[0].ID != 9 || rooms[0].Author != "主播" ||
		rooms[0].Thumbnail() != "https://i0.hdslb.com/avatar.jpg" ||
		rooms[1].ID != 11 || rooms[1].Thumbnail() != "https://i0.hdslb.com/room.jpg" {
		t.Fatalf("unexpected followed rooms: %+v", rooms)
	}
}

func TestLiveSourcesPreferRequestedQualityAndReportFallback(t *testing.T) {
	for _, tc := range []struct {
		name      string
		requested int
		query     string
		want      int
		label     string
	}{
		{name: "web quality", requested: 250, query: "250", want: 250, label: "超清"},
		{name: "album defaults to source", query: "10000", want: 400, label: "蓝光"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := NewClient("")
			client.HTTP.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
				if got := r.URL.Query().Get("qn"); got != tc.query {
					t.Errorf("requested qn=%s, want %s", got, tc.query)
				}
				body := `{"code":0,"data":{"live_status":1,"playurl_info":{"playurl":{
					"g_qn_desc":[{"qn":250,"desc":"超清"},{"qn":400,"desc":"蓝光"}],
					"stream":[{"format":[{"format_name":"flv","codec":[
						{"current_qn":400,"base_url":"/blue.flv","url_info":[{"host":"https://cdn.example"}]},
						{"current_qn":250,"base_url":"/super.flv","url_info":[{"host":"https://cdn.example"}]}
					]}]}]
				}}}}`
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			sources, err := client.liveSources(t.Context(), 42, tc.requested)
			if err != nil {
				t.Fatal(err)
			}
			if len(sources) != 2 || sources[0].quality != tc.want || sources[0].label != tc.label {
				t.Fatalf("unexpected live sources: %+v", sources)
			}
		})
	}
}

func TestResolveLiveHLSOnlyRoom(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Referer") != "https://live.bilibili.com/308543" {
			t.Error("missing live room referer")
		}
		if r.URL.Path == "/available/index.m3u8" {
			io.WriteString(w, "#EXTM3U\n#EXT-X-VERSION:7\n")
			return
		}
		io.WriteString(w, "<html>line unavailable</html>")
	}))
	defer cdn.Close()

	client := NewClient("")
	client.HTTP.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		query := r.URL.Query()
		if query.Get("protocol") != "0,1" || query.Get("format") != "0,1,2" || query.Get("qn") != "10000" {
			t.Errorf("unexpected format query: %s", r.URL.RawQuery)
		}
		body := `{"code":0,"data":{"live_status":1,"playurl_info":{"playurl":{
			"g_qn_desc":[{"qn":250,"desc":"超清"}],
			"stream":[{"protocol_name":"http_hls","format":[{"format_name":"fmp4","codec":[
				{"base_url":"/index.m3u8","current_qn":250,
				"url_info":[{"host":"` + cdn.URL + `/unavailable"},{"host":"` + cdn.URL + `/available"}]}
			]}]}]
		}}}}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	})

	playback, err := client.ResolveLive(t.Context(), 308543, 10000)
	if err != nil {
		t.Fatal(err)
	}
	if playback.HLSURL != cdn.URL+"/available/index.m3u8" || playback.Quality != 250 || playback.QualityLabel != "超清" {
		t.Fatalf("unexpected HLS playback: %+v", playback)
	}
}
