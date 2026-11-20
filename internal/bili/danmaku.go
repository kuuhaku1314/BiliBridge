package bili

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"
)

type Danmaku struct {
	At    time.Duration
	Mode  int
	Color uint32
	Text  string
}

func (c *Client) DanmakuSegment(ctx context.Context, cid, aid int64, segment int) ([]Danmaku, error) {
	address := fmt.Sprintf("https://api.bilibili.com/x/v2/dm/web/seg.so?type=1&oid=%d&pid=%d&segment_index=%d", cid, aid, segment)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	req.Header = MediaHeader()
	if c.Cookie != "" {
		req.Header.Set("Cookie", c.Cookie)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("弹幕接口返回 HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 4<<20 {
		return nil, errors.New("弹幕分段超出大小限制")
	}
	return parseDanmaku(data)
}

var badDanmaku = errors.New("无法解析 B 站弹幕分段")

// Each item is field 1 of the top-level protobuf message. Unknown fields
// are skipped using their wire type so additive schema changes are harmless.
func parseDanmaku(data []byte) ([]Danmaku, error) {
	var items []Danmaku
	err := walkWire(data, func(field, kind, value uint64, payload []byte) error {
		if field != 1 || kind != 2 {
			return nil
		}
		item := Danmaku{Mode: 1, Color: 0xffffff}
		err := walkWire(payload, func(field, kind, value uint64, payload []byte) error {
			if kind == 0 {
				switch field {
				case 2:
					if value <= 1<<31-1 {
						item.At = time.Duration(value) * time.Millisecond
					}
				case 3:
					item.Mode = int(value)
				case 5:
					item.Color = uint32(value) & 0xffffff
				}
			}
			if field == 7 && kind == 2 {
				text := strings.Map(func(r rune) rune {
					if unicode.IsControl(r) {
						return ' '
					}
					return r
				}, string(payload))
				var keep []rune
				for _, r := range text {
					if len(keep) == 120 {
						break
					}
					keep = append(keep, r)
				}
				item.Text = strings.TrimSpace(string(keep))
			}
			return nil
		})
		if err == nil && item.Text != "" && item.Mode >= 1 && item.Mode <= 6 {
			items = append(items, item)
		}
		return err
	})
	return items, err
}

func walkWire(data []byte, visit func(field, kind, value uint64, payload []byte) error) error {
	for len(data) != 0 {
		tag, n := binary.Uvarint(data)
		if n <= 0 || tag < 8 {
			return badDanmaku
		}
		data = data[n:]
		kind := tag & 7
		var value uint64
		var payload []byte
		switch kind {
		case 0:
			value, n = binary.Uvarint(data)
			if n <= 0 {
				return badDanmaku
			}
		case 1:
			n = 8
		case 2:
			size, length := binary.Uvarint(data)
			if length <= 0 || size > uint64(len(data)-length) {
				return badDanmaku
			}
			n = length + int(size)
			payload = data[length:n]
		case 5:
			n = 4
		default:
			return badDanmaku
		}
		if n > len(data) {
			return badDanmaku
		}
		if err := visit(tag>>3, kind, value, payload); err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}
