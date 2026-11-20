package bili

import (
	"context"
	"errors"
	"maps"
	"net/url"
	"slices"
	"strconv"
)

type CatalogItem struct {
	BVID     string `json:"bvid"`
	Title    string `json:"title"`
	Duration int    `json:"duration"`
	Cover    string `json:"cover"`
	Picture  string `json:"pic"`
	Added    int64  `json:"fav_time"`
	Owner    struct {
		Name string `json:"name"`
	} `json:"owner"`
	Upper struct {
		Name string `json:"name"`
	} `json:"upper"`
}

func (c *Client) Home(ctx context.Context) ([]CatalogItem, error) {
	var response struct {
		Items []CatalogItem `json:"item"`
	}
	params := url.Values{
		"fresh_type": {"3"}, "feed_version": {"V8"}, "fresh_idx": {"1"},
		"homepage_ver": {"1"}, "ps": {"30"}, "y_num": {"5"},
	}
	query, err := c.signWBI(ctx, params)
	if err != nil {
		return nil, err
	}
	if err := c.get(ctx, "https://api.bilibili.com/x/web-interface/wbi/index/top/feed/rcmd?"+query, &response); err != nil {
		return nil, err
	}
	return uniqueVideos(response.Items), nil
}

// AccountID returns the ID of the signed-in Bilibili account.
func (c *Client) AccountID(ctx context.Context) (int64, error) {
	var account struct {
		LoggedIn bool  `json:"isLogin"`
		ID       int64 `json:"mid"`
	}
	if err := c.get(ctx, "https://api.bilibili.com/x/web-interface/nav", &account); err != nil {
		return 0, err
	}
	if !account.LoggedIn || account.ID <= 0 {
		return 0, errors.New("请先登录 B 站，再从浏览器连接 BiliBridge")
	}
	return account.ID, nil
}

func (c *Client) Favorites(ctx context.Context) ([]CatalogItem, error) {
	accountID, err := c.AccountID(ctx)
	if err != nil {
		return nil, err
	}
	var folders struct {
		List []struct {
			ID int64 `json:"id"`
		} `json:"list"`
	}
	if err := c.get(ctx, "https://api.bilibili.com/x/v3/fav/folder/created/list-all?up_mid="+strconv.FormatInt(accountID, 10), &folders); err != nil {
		return nil, err
	}
	const pageLimit = 20
	const itemLimit = 200
	byID := make(map[string]CatalogItem)
	for page := 1; page <= pageLimit && len(byID) < itemLimit; page++ {
		more := false
		for _, folder := range folders.List {
			if folder.ID <= 0 {
				continue
			}
			var data struct {
				Items []CatalogItem `json:"medias"`
				More  bool          `json:"has_more"`
			}
			query := url.Values{
				"media_id": {strconv.FormatInt(folder.ID, 10)}, "pn": {strconv.Itoa(page)}, "ps": {"20"}, "order": {"mtime"},
			}
			if err := c.get(ctx, "https://api.bilibili.com/x/v3/fav/resource/list?"+query.Encode(), &data); err != nil {
				return nil, err
			}
			more = more || data.More
			for _, item := range uniqueVideos(data.Items) {
				if old, ok := byID[item.BVID]; !ok || item.Added > old.Added {
					byID[item.BVID] = item
				}
			}
		}
		if !more {
			break
		}
	}
	out := slices.Collect(maps.Values(byID))
	slices.SortFunc(out, func(a, b CatalogItem) int {
		if a.Added > b.Added {
			return -1
		}
		if a.Added < b.Added {
			return 1
		}
		return 0
	})
	return out[:min(len(out), itemLimit)], nil
}

func uniqueVideos(items []CatalogItem) []CatalogItem {
	seen := make(map[string]bool)
	var out []CatalogItem
	for _, item := range items {
		if len(item.BVID) != 12 || bvidRe.FindString(item.BVID) != item.BVID || item.Title == "" || seen[item.BVID] {
			continue
		}
		seen[item.BVID] = true
		out = append(out, item)
	}
	return out
}
