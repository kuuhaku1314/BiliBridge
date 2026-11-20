package bili

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
)

// View is the metadata of a video (稿件).
type View struct {
	BVID     string `json:"bvid"`
	AID      int64  `json:"aid"`
	Title    string `json:"title"`
	Duration int    `json:"duration"`
	CID      int64  `json:"cid"`
	Owner    struct {
		Name string `json:"name"`
	} `json:"owner"`
	Pages []Page `json:"pages"`
}

// Page is one part (分P) of a video.
type Page struct {
	CID      int64  `json:"cid"`
	Page     int    `json:"page"`
	Part     string `json:"part"`
	Duration int    `json:"duration"`
}

// View fetches the metadata of the video with the given BV id.
func (c *Client) View(ctx context.Context, bvid string) (*View, error) {
	var v View
	if err := c.get(ctx, "https://api.bilibili.com/x/web-interface/view?bvid="+url.QueryEscape(bvid), &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// PageTitle returns the title to show for part p (1-based).
func (v *View) PageTitle(p int) string {
	if len(v.Pages) > 1 && p >= 1 && p <= len(v.Pages) {
		return fmt.Sprintf("%s - P%d %s", v.Title, p, v.Pages[p-1].Part)
	}
	return v.Title
}

var (
	bvidRe = regexp.MustCompile(`BV[0-9A-Za-z]{10}`)
	pageRe = regexp.MustCompile(`[?&]p=(\d+)`)
)

// ParseVideoRef extracts the BV id and part number (1 if absent) from a
// video URL or bare BV id.
func ParseVideoRef(s string) (bvid string, page int, err error) {
	bvid = bvidRe.FindString(s)
	if bvid == "" {
		return "", 0, errors.New("no BV id found in " + strconv.Quote(s))
	}
	page = 1
	if m := pageRe.FindStringSubmatch(s); m != nil {
		page, _ = strconv.Atoi(m[1])
	}
	return bvid, page, nil
}
