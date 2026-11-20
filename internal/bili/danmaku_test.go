package bili

import (
	"encoding/binary"
	"testing"
	"time"
)

func TestParseDanmakuSegments(t *testing.T) {
	message := []byte{16}
	message = binary.AppendUvarint(message, 1250)
	message = append(message, 24, 5, 40)
	message = binary.AppendUvarint(message, 0xff0033)
	text := "弹幕测试"
	message = append(message, 58, byte(len(text)))
	message = append(message, text...)
	buffer := append([]byte{10, byte(len(message))}, message...)
	items, err := parseDanmaku(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0] != (Danmaku{At: 1250 * time.Millisecond, Mode: 5, Color: 0xff0033, Text: text}) {
		t.Fatalf("unexpected decoded items: %+v", items)
	}
	if _, err := parseDanmaku(buffer[:len(buffer)-1]); err == nil {
		t.Fatal("truncated segment was accepted")
	}
}
