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
}

type LiveStream struct {
	io.ReadCloser
	Quality      int
	QualityLabel string
}

func (c *Client) liveSources(ctx context.Context, room int64, quality int) ([]liveSource, error) {
	if quality <= 0 || quality > 30000 {
		quality = 10000
	}
	params := url.Values{
		"room_id": {strconv.FormatInt(room, 10)}, "platform": {"web"}, "protocol": {"0"},
		"format": {"0"}, "codec": {"0,1"}, "qn": {strconv.Itoa(quality)}, "no_playurl": {"0"},
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
					Formats []struct {
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
			if format.Name != "flv" {
				continue
			}
			for _, codec := range format.Codecs {
				for _, mirror := range codec.URLs {
					u, err := url.Parse(mirror.Host + codec.Path + mirror.Extra)
					if err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil {
						sources = append(sources, liveSource{u.String(), codec.Quality, labels[codec.Quality]})
					}
				}
			}
		}
	}
	if len(sources) == 0 {
		return nil, errors.New("直播间没有返回可用的 FLV 播放源")
	}
	// Prefer the requested quality over fallbacks returned for other codecs.
	slices.SortStableFunc(sources, func(a, b liveSource) int {
		switch {
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

func (c *Client) OpenLive(ctx context.Context, room int64, quality int) (*LiveStream, error) {
	sources, err := c.liveSources(ctx, room, quality)
	if err != nil {
		return nil, err
	}
	for _, source := range sources {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, source.url, nil)
		if err != nil {
			return nil, err
		}
		req.Header = MediaHeader()
		req.Header.Set("Referer", fmt.Sprintf("https://live.bilibili.com/%d", room))
		resp, err := liveHTTP.Do(req)
		if err != nil {
			continue
		}
		if resp.StatusCode == http.StatusOK {
			return &LiveStream{ReadCloser: resp.Body, Quality: source.quality, QualityLabel: source.label}, nil
		}
		resp.Body.Close()
	}
	return nil, errors.New("直播线路暂时无法连接")
}
