package bili

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

type LiveRoom struct {
	ID        int64           `json:"room_id"`
	Title     string          `json:"title"`
	Author    string          `json:"uname"`
	Face      string          `json:"face"`
	Status    int             `json:"live_status"`
	Cover     json.RawMessage `json:"cover_from_user"`
	UserCover json.RawMessage `json:"user_cover"`
	RoomCover json.RawMessage `json:"cover"`
	Frame     json.RawMessage `json:"keyframe"`
}

func (r LiveRoom) Thumbnail() string {
	for _, raw := range []json.RawMessage{r.UserCover, r.Cover, r.RoomCover, r.Frame} {
		var cover string
		if json.Unmarshal(raw, &cover) == nil {
			if cover = liveCoverURL(cover); cover != "" {
				return cover
			}
		}
	}
	return liveCoverURL(r.Face)
}

func liveCoverURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return ""
	}
	return u.String()
}

func (c *Client) LiveRoomInfo(ctx context.Context, roomID int64) (LiveRoom, error) {
	var room LiveRoom
	api := "https://api.live.bilibili.com/room/v1/Room/get_info?id=" + strconv.FormatInt(roomID, 10)
	if err := c.get(ctx, api, &room); err != nil {
		return LiveRoom{}, err
	}
	if room.ID <= 0 {
		return LiveRoom{}, errors.New("直播房间不存在")
	}
	return room, nil
}

func (c *Client) FollowedLive(ctx context.Context) ([]LiveRoom, error) {
	if c.Cookie == "" {
		return nil, errors.New("请先登录 B 站，再从浏览器连接 BiliBridge")
	}
	var data struct {
		Rooms []LiveRoom `json:"rooms"`
		List  []LiveRoom `json:"list"`
	}
	if err := c.get(ctx, "https://api.live.bilibili.com/xlive/web-ucenter/v1/xfetter/GetWebList?hit_ab=true", &data); err != nil {
		return nil, err
	}
	var live []LiveRoom
	seen := make(map[int64]bool)
	for _, room := range append(data.Rooms, data.List...) {
		if room.ID <= 0 || room.Status != 1 || seen[room.ID] {
			continue
		}
		seen[room.ID] = true
		live = append(live, room)
	}
	return live, nil
}

type liveSource struct {
	url     string
	quality int
	label   string
	hls     bool
}

type LiveStream struct {
	io.ReadCloser
	Quality      int
	QualityLabel string
}

// LivePlayback carries the upstream playlist for rooms that use HLS. An empty
// HLSURL means the room has an FLV source that the local proxy can open.
type LivePlayback struct {
	HLSURL       string
	Quality      int
	QualityLabel string
}

func (c *Client) liveSources(ctx context.Context, room int64, quality int) ([]liveSource, error) {
	if quality <= 0 || quality > 30000 {
		quality = 10000
	}
	params := url.Values{
		"room_id": {strconv.FormatInt(room, 10)}, "platform": {"web"}, "protocol": {"0,1"},
		"format": {"0,1,2"}, "codec": {"0,1"}, "qn": {strconv.Itoa(quality)}, "no_playurl": {"0"},
	}
	var data struct {
		Status int `json:"live_status"`
		Info   struct {
			PlayURL struct {
				Qualities []struct {
					ID   int    `json:"qn"`
					Name string `json:"desc"`
				} `json:"g_qn_desc"`
				Streams []struct {
					Protocol string `json:"protocol_name"`
					Formats  []struct {
						Name   string `json:"format_name"`
						Codecs []struct {
							Path    string `json:"base_url"`
							Quality int    `json:"current_qn"`
							URLs    []struct {
								Host  string `json:"host"`
								Extra string `json:"extra"`
							} `json:"url_info"`
						} `json:"codec"`
					} `json:"format"`
				} `json:"stream"`
			} `json:"playurl"`
		} `json:"playurl_info"`
	}
	if err := c.get(ctx, "https://api.live.bilibili.com/xlive/web-room/v2/index/getRoomPlayInfo?"+params.Encode(), &data); err != nil {
		return nil, err
	}
	if data.Status != 1 {
		return nil, errors.New("直播间当前未开播")
	}
	labels := make(map[int]string)
	for _, option := range data.Info.PlayURL.Qualities {
		labels[option.ID] = option.Name
	}
	var sources []liveSource
	for _, stream := range data.Info.PlayURL.Streams {
		for _, format := range stream.Formats {
			hls := stream.Protocol == "http_hls" && (format.Name == "fmp4" || format.Name == "ts")
			if format.Name != "flv" && !hls {
				continue
			}
			for _, codec := range format.Codecs {
				for _, mirror := range codec.URLs {
					u, err := url.Parse(mirror.Host + codec.Path + mirror.Extra)
					if err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil {
						sources = append(sources, liveSource{url: u.String(), quality: codec.Quality, label: labels[codec.Quality], hls: hls})
					}
				}
			}
		}
	}
	if len(sources) == 0 {
		return nil, errors.New("直播间没有返回可用的 FLV 或 HLS 播放源")
	}
	// Keep FLV as the first choice, then prefer the requested quality.
	slices.SortStableFunc(sources, func(a, b liveSource) int {
		switch {
		case !a.hls && b.hls:
			return -1
		case a.hls && !b.hls:
			return 1
		case a.quality == quality && b.quality != quality:
			return -1
		case a.quality != quality && b.quality == quality:
			return 1
		default:
			return 0
		}
	})
	return sources, nil
}

var liveHTTP = &http.Client{Transport: &http.Transport{
	ResponseHeaderTimeout: 8 * time.Second,
	IdleConnTimeout:       30 * time.Second,
	ForceAttemptHTTP2:     true,
}}

// FetchLiveMedia opens a playlist or segment using the live room's CDN headers.
func FetchLiveMedia(ctx context.Context, room int64, method, rawURL, byteRange string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header = MediaHeader()
	req.Header.Set("Referer", fmt.Sprintf("https://live.bilibili.com/%d", room))
	if byteRange != "" {
		req.Header.Set("Range", byteRange)
	}
	return liveHTTP.Do(req)
}

func openLiveSource(ctx context.Context, room int64, source liveSource) (*http.Response, error) {
	resp, err := FetchLiveMedia(ctx, room, http.MethodGet, source.url, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("live source: HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

// ResolveLive selects the local FLV proxy or a reachable HLS playlist.
func (c *Client) ResolveLive(ctx context.Context, room int64, quality int) (LivePlayback, error) {
	sources, err := c.liveSources(ctx, room, quality)
	if err != nil {
		return LivePlayback{}, err
	}
	if !sources[0].hls {
		return LivePlayback{}, nil
	}
	for _, source := range sources {
		// Bound both the connection and the read of the playlist signature.
		probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		resp, err := openLiveSource(probeCtx, room, source)
		var prefix [7]byte
		if err == nil {
			_, err = io.ReadFull(resp.Body, prefix[:])
			resp.Body.Close()
		}
		cancel()
		if err == nil && string(prefix[:]) == "#EXTM3U" {
			return LivePlayback{HLSURL: resp.Request.URL.String(), Quality: source.quality, QualityLabel: source.label}, nil
		}
	}
	return LivePlayback{}, errors.New("直播 HLS 线路暂时无法连接")
}

func (c *Client) OpenLive(ctx context.Context, room int64, quality int) (*LiveStream, error) {
	sources, err := c.liveSources(ctx, room, quality)
	if err != nil {
		return nil, err
	}
	for _, source := range sources {
		if source.hls {
			break
		}
		resp, err := openLiveSource(ctx, room, source)
		if err == nil {
			return &LiveStream{ReadCloser: resp.Body, Quality: source.quality, QualityLabel: source.label}, nil
		}
	}
	return nil, errors.New("直播线路暂时无法连接")
}
