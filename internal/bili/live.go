package bili

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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

func (c *Client) liveURLs(ctx context.Context, room int64) ([]string, error) {
	params := url.Values{
		"room_id": {strconv.FormatInt(room, 10)}, "platform": {"web"}, "protocol": {"0"},
		"format": {"0"}, "codec": {"0,1"}, "qn": {"10000"}, "no_playurl": {"0"},
	}
	var data struct {
		Status int `json:"live_status"`
		Info   struct {
			PlayURL struct {
				Streams []struct {
					Formats []struct {
						Name   string `json:"format_name"`
						Codecs []struct {
							Path string `json:"base_url"`
							URLs []struct {
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
	var urls []string
	for _, stream := range data.Info.PlayURL.Streams {
		for _, format := range stream.Formats {
			if format.Name != "flv" {
				continue
			}
			for _, codec := range format.Codecs {
				for _, mirror := range codec.URLs {
					u, err := url.Parse(mirror.Host + codec.Path + mirror.Extra)
					if err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil {
						urls = append(urls, u.String())
					}
				}
			}
		}
	}
	if len(urls) == 0 {
		return nil, errors.New("直播间没有返回可用的 FLV 播放源")
	}
	return urls, nil
}

var liveHTTP = &http.Client{Transport: &http.Transport{
	ResponseHeaderTimeout: 8 * time.Second,
	IdleConnTimeout:       30 * time.Second,
	ForceAttemptHTTP2:     true,
}}

func (c *Client) OpenLive(ctx context.Context, room int64) (io.ReadCloser, error) {
	sources, err := c.liveURLs(ctx, room)
	if err != nil {
		return nil, err
	}
	for _, source := range sources {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
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
			return resp.Body, nil
		}
		resp.Body.Close()
	}
	return nil, errors.New("直播线路暂时无法连接")
}
