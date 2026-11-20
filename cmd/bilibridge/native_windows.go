package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"time"
)

type bridgeRequest struct {
	Action  string  `json:"action"`
	Ref     string  `json:"ref,omitempty"`
	At      float64 `json:"at,omitempty"`
	Quality int     `json:"quality,omitempty"`
	Codec   int     `json:"codec,omitempty"`
	Cookie  string  `json:"cookie,omitempty"`
}

type bridgeReply struct {
	OK           bool   `json:"ok"`
	Error        string `json:"error,omitempty"`
	Height       int    `json:"height,omitempty"`
	Quality      int    `json:"quality,omitempty"`
	QualityLabel string `json:"qualityLabel,omitempty"`
	Codec        int    `json:"codec,omitempty"`
}

func runNative(origin string) error {
	if origin != extensionSrc {
		return errors.New("unregistered native messaging origin")
	}
	data, err := readNativeFrame(os.Stdin)
	if err != nil {
		return err
	}
	reply := forwardNative(data)
	data, err = json.Marshal(reply)
	if err != nil {
		return err
	}
	var size [4]byte
	binary.LittleEndian.PutUint32(size[:], uint32(len(data)))
	_, err = os.Stdout.Write(append(size[:], data...))
	return err
}

func readNativeFrame(r io.Reader) ([]byte, error) {
	var size [4]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(size[:])
	if n == 0 || n > 64<<10 {
		return nil, fmt.Errorf("invalid native message length %d", n)
	}
	data := make([]byte, n)
	_, err := io.ReadFull(r, data)
	return data, err
}

func forwardNative(data []byte) bridgeReply {
	cfg, err := loadConfig(false)
	if err != nil {
		return bridgeReply{Error: "请先打开 BiliBridge 桌面程序"}
	}
	if !desktopAvailable() {
		var request bridgeRequest
		if json.Unmarshal(data, &request) != nil || request.Action != "play" {
			return bridgeReply{Error: "请先打开 BiliBridge 托盘程序"}
		}
		exe, err := os.Executable()
		if err != nil {
			return bridgeReply{Error: err.Error()}
		}
		cmd := exec.Command(exe, "-desktop")
		if err := cmd.Start(); err != nil {
			return bridgeReply{Error: "无法启动 BiliBridge 桌面程序"}
		}
		go cmd.Wait()
		for deadline := time.Now().Add(5 * time.Second); !desktopAvailable(); {
			if time.Now().After(deadline) {
				return bridgeReply{Error: "BiliBridge 托盘服务未启动"}
			}
			time.Sleep(125 * time.Millisecond)
		}
	}
	client := &http.Client{
		Timeout:       35 * time.Second,
		Transport:     &http.Transport{DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	challenge := rand.Text()
	probe, _ := http.NewRequest(http.MethodGet, "http://"+desktopAddr+"/ready", nil)
	probe.Header.Set("X-BiliBridge-Challenge", challenge)
	proof, err := client.Do(probe)
	if err != nil {
		return bridgeReply{Error: "无法确认 BiliBridge 托盘服务"}
	}
	signature, err := io.ReadAll(io.LimitReader(proof.Body, 65))
	proof.Body.Close()
	if err != nil || proof.StatusCode != http.StatusOK || !hmac.Equal(signature, []byte(localhostProof(cfg.Key, challenge))) {
		return bridgeReply{Error: "本机端口上的程序与 BiliBridge 配置不匹配"}
	}
	resp, err := client.Post(cfg.baseURL()+"/bridge", "application/json", bytes.NewReader(data))
	if err != nil {
		return bridgeReply{Error: "未能连接 BiliBridge 托盘服务"}
	}
	defer resp.Body.Close()
	var reply bridgeReply
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&reply); err != nil {
		return bridgeReply{Error: "BiliBridge 托盘服务返回了无效响应"}
	}
	return reply
}

func desktopAvailable() bool {
	conn, err := net.DialTimeout("tcp", desktopAddr, 200*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
