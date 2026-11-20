package bili

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var episodePath = regexp.MustCompile(`^/bangumi/play/ep([0-9]+)/?$`)

type Media struct {
	Title string
	BVID  string
	CID   int64
	AID   int64
	EPID  int64
}

func (c *Client) Resolve(ctx context.Context, ref string) (Media, error) {
	if u, err := url.Parse(ref); err == nil && u.Scheme == "https" && u.Host == "www.bilibili.com" {
		if match := episodePath.FindStringSubmatch(u.Path); match != nil {
			return c.resolveEpisode(ctx, match[1])
		}
	}
	bvid, page, err := ParseVideoRef(ref)
	if err != nil {
		return Media{}, err
	}
	view, err := c.View(ctx, bvid)
	if err != nil {
		return Media{}, err
	}
	cid := view.CID
	if page >= 1 && page <= len(view.Pages) {
		cid = view.Pages[page-1].CID
	}
	if cid <= 0 {
		return Media{}, errors.New("B站没有返回可用的分 P")
	}
	return Media{Title: view.PageTitle(page), BVID: bvid, CID: cid, AID: view.AID}, nil
}

type episode struct {
	ID        int64  `json:"id"`
	AID       int64  `json:"aid"`
	CID       int64  `json:"cid"`
	BVID      string `json:"bvid"`
	Title     string `json:"title"`
	LongTitle string `json:"long_title"`
}

func (c *Client) resolveEpisode(ctx context.Context, id string) (Media, error) {
	epID, err := strconv.ParseInt(id, 10, 64)
	if err != nil || epID <= 0 {
		return Media{}, errors.New("无效的 B 站剧集编号")
	}
	var season struct {
		Title    string    `json:"title"`
		Episodes []episode `json:"episodes"`
		Section  []struct {
			Episodes []episode `json:"episodes"`
		} `json:"section"`
	}
	if err := c.get(ctx, "https://api.bilibili.com/pgc/view/web/season?ep_id="+id, &season); err != nil {
		return Media{}, err
	}
	for _, section := range season.Section {
		season.Episodes = append(season.Episodes, section.Episodes...)
	}
	for _, ep := range season.Episodes {
		if ep.ID == epID && ep.CID > 0 {
			title := strings.TrimSpace(fmt.Sprintf("%s %s %s", season.Title, ep.Title, ep.LongTitle))
			return Media{Title: title, BVID: ep.BVID, CID: ep.CID, AID: ep.AID, EPID: epID}, nil
		}
	}
	return Media{}, errors.New("B站没有返回这一集的播放信息")
}

func (c *Client) MediaPlayURL(ctx context.Context, m Media) (*PlayInfo, error) {
	if m.EPID == 0 {
		return c.PlayURL(ctx, m.BVID, m.CID)
	}
	query := url.Values{
		"ep_id": {strconv.FormatInt(m.EPID, 10)}, "avid": {strconv.FormatInt(m.AID, 10)},
		"cid": {strconv.FormatInt(m.CID, 10)}, "qn": {"127"}, "fnval": {"4048"}, "fourk": {"1"},
	}
	var result struct {
		PlayInfo
		Preview int `json:"is_preview"`
	}
	if err := c.get(ctx, "https://api.bilibili.com/pgc/player/web/playurl?"+query.Encode(), &result); err != nil {
		return nil, err
	}
	if result.Preview != 0 || result.Dash == nil || len(result.Dash.Video) == 0 {
		return nil, errors.New("B站未向当前账号返回本集的完整 DASH 流")
	}
	return &result.PlayInfo, nil
}
