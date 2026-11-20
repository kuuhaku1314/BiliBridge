package bili

import (
	"io"
	"net/http"
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
