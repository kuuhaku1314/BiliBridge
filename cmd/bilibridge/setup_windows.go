package main

import (
	"bytes"
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"bilibridge/internal/potplayer"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

//go:embed potplayer-albums.as
var potplayerAlbumParser []byte

//go:embed bilibili.ico
var potplayerAlbumIcon []byte

func installAlbumParser() error {
	exe, err := potplayer.FindExe()
	if err != nil {
		return err
	}
	path := filepath.Join(filepath.Dir(exe), "Extension", "Media", "PlayParse", "MediaPlayParse - BiliBridge.as")
	data, err := os.ReadFile(path)
	if err == nil && !strings.HasPrefix(string(data), "// BiliBridge local album adapter") {
		return errors.New("已有同名的 PotPlayer BiliBridge 扩展")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !bytes.Equal(data, potplayerAlbumParser) {
		if err := writeAtomic(path, potplayerAlbumParser); err != nil {
			return err
		}
	}
	icon := strings.TrimSuffix(path, ".as") + ".ico"
	if _, err := os.Stat(icon); errors.Is(err, os.ErrNotExist) {
		return writeAtomic(icon, potplayerAlbumIcon)
	} else {
		return err
	}
}

const (
	nativeName   = "com.bilibridge.desktop"
	extensionID  = "mfbhjmiklieomnbbbbnlbnegcpeiepba"
	extensionSrc = "chrome-extension://" + extensionID + "/"
	desktopAddr  = "127.0.0.1:17863"
	startupKey   = `Software\Microsoft\Windows\CurrentVersion\Run`
	startupName  = "BiliBridgeDesktop"
)

func desktopStartupEnabled() bool {
	key, err := registry.OpenKey(registry.CURRENT_USER, startupKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer key.Close()
	command, _, err := key.GetStringValue(startupName)
	return err == nil && command != ""
}

func setDesktopStartup(enabled bool) error {
	if !enabled {
		key, err := registry.OpenKey(registry.CURRENT_USER, startupKey, registry.SET_VALUE)
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		defer key.Close()
		err = key.DeleteValue(startupName)
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	command := windows.ComposeCommandLine([]string{exe, "-desktop"})
	if len(windows.StringToUTF16(command))-1 > 260 {
		return errors.New("EXE 路径过长，请将 BiliBridge 安装在路径较短的目录")
	}
	key, _, err := registry.CreateKey(registry.CURRENT_USER, startupKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	return key.SetStringValue(startupName, command)
}

type desktopConfig struct {
	Key      string `json:"key"`
	Danmaku  bool   `json:"danmaku"`
	Home     bool   `json:"home"`
	Favorite bool   `json:"favorites"`
	Live     bool   `json:"live"`
}

func dataDir() string { return filepath.Join(os.Getenv("LOCALAPPDATA"), "BiliBridge") }

func loadConfig(create bool) (desktopConfig, error) {
	cfg := desktopConfig{Danmaku: true, Home: true, Favorite: true, Live: true}
	data, err := os.ReadFile(filepath.Join(dataDir(), "settings.json"))
	if errors.Is(err, os.ErrNotExist) && create {
		cfg.Key = rand.Text()
		return cfg, saveConfig(cfg)
	}
	if err != nil {
		return cfg, err
	}
	err = json.Unmarshal(data, &cfg)
	if err == nil && (len(cfg.Key) != 26 || strings.ContainsAny(cfg.Key, "/\\?#")) {
		err = errors.New("BiliBridge 的本机配置密钥无效")
	}
	return cfg, err
}

func saveConfig(cfg desktopConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dataDir(), "settings.json"), data)
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".bilibridge-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(data)
	if err := errors.Join(writeErr, f.Close()); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func registerNativeHost() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	manifest := map[string]any{
		"name": nativeName, "description": "BiliBridge desktop",
		"path": exe, "type": "stdio", "allowed_origins": []string{extensionSrc},
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dataDir(), nativeName+".json")
	if err := writeAtomic(path, data); err != nil {
		return err
	}
	for _, browser := range []string{`Google\Chrome`, `Microsoft\Edge`} {
		key, _, err := registry.CreateKey(registry.CURRENT_USER, `Software\`+browser+`\NativeMessagingHosts\`+nativeName, registry.SET_VALUE)
		if err != nil {
			return err
		}
		err = key.SetStringValue("", path)
		key.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func (cfg desktopConfig) baseURL() string { return "http://" + desktopAddr + "/" + cfg.Key }

func updateAlbums(cfg desktopConfig) error {
	exe, err := potplayer.FindExe()
	if err != nil {
		return err
	}
	base := filepath.Join(filepath.Dir(exe), "Playlist")
	for _, candidate := range []string{
		filepath.Join(os.Getenv("APPDATA"), "PotPlayerMini64", "Playlist"),
		filepath.Join(os.Getenv("APPDATA"), "PotPlayer", "Playlist"),
	} {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			if _, err := os.Stat(base); errors.Is(err, os.ErrNotExist) {
				base = candidate
			}
		}
	}
	var result error
	for _, album := range []struct {
		title string
		route string
		on    bool
	}{
		{"B站首页", "home", cfg.Home},
		{"我的收藏", "favorites", cfg.Favorite},
		{"我关注的直播", "live", cfg.Live},
	} {
		path := filepath.Join(base, album.title+".dpl")
		previous := "extplaylist=" + cfg.baseURL() + "/" + album.route + ".dpl"
		target := "extplaylist=" + cfg.baseURL() + "/" + album.route + ".bbalbum"
		data, err := os.ReadFile(path)
		body := []byte("\xef\xbb\xbfDAUMPLAYLIST\r\n" + target + "\r\n")
		current := hasAlbumTarget(string(data), target)
		ours := current || hasAlbumTarget(string(data), previous)
		switch {
		case err == nil && !ours:
			if album.on {
				result = errors.Join(result, fmt.Errorf("已有同名 PotPlayer 专辑 %s", album.title))
			}
		case err == nil && !album.on:
			result = errors.Join(result, os.Remove(path))
		case err == nil && !current:
			result = errors.Join(result, writeAtomic(path, body))
		case errors.Is(err, os.ErrNotExist) && album.on:
			result = errors.Join(result, writeAtomic(path, body))
		case err != nil && !errors.Is(err, os.ErrNotExist):
			result = errors.Join(result, err)
		}
	}
	return result
}

func hasAlbumTarget(data, target string) bool {
	for _, line := range strings.Split(data, "\n") {
		if strings.TrimRight(line, "\r") == target {
			return true
		}
	}
	return false
}
