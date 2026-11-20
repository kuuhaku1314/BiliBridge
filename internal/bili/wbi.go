package bili

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

var mixinKeyEncTab = [64]int{
	46, 47, 18, 2, 53, 8, 23, 32, 15, 50, 10, 31, 58, 3, 45, 35, 27, 43, 5, 49,
	33, 9, 42, 19, 29, 28, 14, 39, 12, 38, 41, 13, 37, 48, 7, 16, 24, 55, 40,
	61, 26, 17, 0, 1, 60, 51, 30, 4, 22, 25, 54, 21, 56, 59, 6, 63, 57, 62, 11,
	36, 20, 34, 44, 52,
}

// wbiMixinKey returns the WBI signing key, refreshing it hourly.
func (c *Client) wbiMixinKey(ctx context.Context) (string, error) {
	c.wbiMu.Lock()
	defer c.wbiMu.Unlock()
	if c.wbiKey != "" && time.Since(c.wbiTime) < time.Hour {
		return c.wbiKey, nil
	}
	// nav answers -101 when logged out but still carries the keys.
	env, err := c.fetch(ctx, "https://api.bilibili.com/x/web-interface/nav")
	if err != nil {
		return "", err
	}
	var nav struct {
		WbiImg struct {
			ImgURL string `json:"img_url"`
			SubURL string `json:"sub_url"`
		} `json:"wbi_img"`
	}
	if err := json.Unmarshal(env.Data, &nav); err != nil {
		return "", err
	}
	stem := func(u string) string { b := path.Base(u); return strings.TrimSuffix(b, path.Ext(b)) }
	raw := stem(nav.WbiImg.ImgURL) + stem(nav.WbiImg.SubURL)
	if len(raw) < 64 {
		return "", errors.New("bilibili API: unexpected WBI keys")
	}
	var key [32]byte
	for i := range key {
		key[i] = raw[mixinKeyEncTab[i]]
	}
	c.wbiKey, c.wbiTime = string(key[:]), time.Now()
	return c.wbiKey, nil
}

// signWBI returns the query string for params with the WBI signature added.
func (c *Client) signWBI(ctx context.Context, params url.Values) (string, error) {
	key, err := c.wbiMixinKey(ctx)
	if err != nil {
		return "", err
	}
	params.Set("wts", strconv.FormatInt(time.Now().Unix(), 10))
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		v := strings.Map(func(r rune) rune {
			if strings.ContainsRune("!'()*", r) {
				return -1
			}
			return r
		}, params.Get(k))
		b.WriteString(escape(k) + "=" + escape(v))
	}
	q := b.String()
	sum := md5.Sum([]byte(q + key))
	return q + "&w_rid=" + hex.EncodeToString(sum[:]), nil
}

// escape matches JavaScript's encodeURIComponent for the characters WBI
// leaves in values.
func escape(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }
