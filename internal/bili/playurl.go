package bili

import (
	"cmp"
	"context"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// Codec ids used by the playurl API.
const (
	CodecAVC  = 7
	CodecHEVC = 12
	CodecAV1  = 13
)

// QualityNames maps quality ids to the labels the web player shows.
var QualityNames = map[int]string{
	127: "8K 超高清", 126: "杜比视界", 125: "HDR 真彩", 120: "4K 超清",
	116: "1080P 60帧", 112: "1080P 高码率", 80: "1080P 高清", 74: "720P 60帧",
	64: "720P 高清", 32: "480P 清晰", 16: "360P 流畅", 6: "240P 极速",
}

// PlayInfo is the playurl response.
type PlayInfo struct {
	Quality       int   `json:"quality"`
	AcceptQuality []int `json:"accept_quality"`
	Dash          *Dash `json:"dash"`
}

// Dash lists the DASH streams of a video.
type Dash struct {
	Duration int          `json:"duration"`
	Video    []DashStream `json:"video"`
	Audio    []DashStream `json:"audio"`
	Flac     *struct {
		Audio *DashStream `json:"audio"`
	} `json:"flac"`
}

// DashStream is one video or audio stream: a single-track fragmented MP4.
type DashStream struct {
	ID          int      `json:"id"`
	BaseURL     string   `json:"baseUrl"`
	BackupURL   []string `json:"backupUrl"`
	Bandwidth   int      `json:"bandwidth"`
	Codecs      string   `json:"codecs"`
	CodecID     int      `json:"codecid"`
	Width       int      `json:"width"`
	Height      int      `json:"height"`
	FrameRate   string   `json:"frameRate"`
	SegmentBase struct {
		Initialization string `json:"Initialization"`
		IndexRange     string `json:"indexRange"`
	} `json:"SegmentBase"`
}

// IndexEnd returns the offset of the last byte of the stream's sidx, so
// bytes [0, IndexEnd] hold everything needed to describe the stream.
func (s *DashStream) IndexEnd() (int64, error) {
	_, end, ok := strings.Cut(s.SegmentBase.IndexRange, "-")
	if !ok {
		return 0, errors.New("stream has no index range")
	}
	return strconv.ParseInt(end, 10, 64)
}

// URLs returns the stream's URLs, best first. PCDN nodes (mcdn, szbdyd) are
// often slow or flaky, so they go last.
func (s *DashStream) URLs() []string {
	var out []string
	for _, u := range append([]string{s.BaseURL}, s.BackupURL...) {
		if u != "" && !slices.Contains(out, u) {
			out = append(out, u)
		}
	}
	slices.SortStableFunc(out, func(a, b string) int { return cmp.Compare(isPCDN(a), isPCDN(b)) })
	return out
}

func isPCDN(rawURL string) int {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 1
	}
	h := u.Hostname()
	if strings.Contains(h, ".mcdn.bilivideo") || strings.HasSuffix(h, ".szbdyd.com") {
		return 1
	}
	return 0
}

// PlayURL fetches the DASH streams of one part of a video. The streams
// returned depend on the client's login state.
func (c *Client) PlayURL(ctx context.Context, bvid string, cid int64) (*PlayInfo, error) {
	q, err := c.signWBI(ctx, url.Values{
		"bvid":  {bvid},
		"cid":   {strconv.FormatInt(cid, 10)},
		"qn":    {"127"},
		"fnval": {"4048"}, // DASH with HDR, 4K, Dolby, 8K and AV1
		"fnver": {"0"},
		"fourk": {"1"},
	})
	if err != nil {
		return nil, err
	}
	var p PlayInfo
	if err := c.get(ctx, "https://api.bilibili.com/x/player/wbi/playurl?"+q, &p); err != nil {
		return nil, err
	}
	if p.Dash == nil || len(p.Dash.Video) == 0 {
		return nil, errors.New("video has no DASH streams")
	}
	return &p, nil
}

// Prefs chooses among the available streams.
type Prefs struct {
	MaxQuality int   // highest quality id to use; 0 means the best available
	VideoCodec int   // 0 permits any codec
	Codecs     []int // codec ids, most preferred first
	AllowHDR   bool  // allow HDR and Dolby Vision
}

// Pick chooses the video and audio streams to play.
func (p *PlayInfo) Pick(pref Prefs) (video, audio *DashStream, err error) {
	compatible := func(s *DashStream) bool {
		return pref.VideoCodec == 0 || s.CodecID == pref.VideoCodec
	}
	eligible := func(s *DashStream) bool {
		if !compatible(s) {
			return false
		}
		if !pref.AllowHDR && (s.ID == 125 || s.ID == 126) {
			return false
		}
		return true
	}
	codecRank := func(s *DashStream) int {
		if i := slices.Index(pref.Codecs, s.CodecID); i >= 0 {
			return i
		}
		return len(pref.Codecs)
	}
	var lowest *DashStream
	for i := range p.Dash.Video {
		s := &p.Dash.Video[i]
		if !eligible(s) {
			continue
		}
		if lowest == nil || s.ID < lowest.ID || s.ID == lowest.ID && codecRank(s) < codecRank(lowest) {
			lowest = s
		}
		if pref.MaxQuality != 0 && s.ID > pref.MaxQuality {
			continue
		}
		if video == nil || s.ID > video.ID || s.ID == video.ID && codecRank(s) < codecRank(video) {
			video = s
		}
	}
	if video == nil {
		video = lowest
	}
	if video == nil {
		return nil, nil, errors.New("no video stream matches the codec and HDR preferences")
	}
	for i := range p.Dash.Audio {
		if s := &p.Dash.Audio[i]; audio == nil || s.Bandwidth > audio.Bandwidth {
			audio = s
		}
	}
	if audio == nil && p.Dash.Flac != nil {
		audio = p.Dash.Flac.Audio
	}
	if audio == nil {
		return nil, nil, errors.New("video has no audio stream")
	}
	return video, audio, nil
}
