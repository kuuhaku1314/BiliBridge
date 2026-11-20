package main

import (
	_ "embed"
	"encoding/binary"
	"errors"
	"runtime"
	"unsafe"

	"bilibridge/internal/potplayer"
	"golang.org/x/sys/windows"
)

//go:embed bilibridge.ico
var trayIconBytes []byte

var (
	trayUser   = windows.NewLazySystemDLL("user32.dll")
	trayShell  = windows.NewLazySystemDLL("shell32.dll")
	trayKernel = windows.NewLazySystemDLL("kernel32.dll")
)

type trayClass struct {
	style                              uint32
	proc                               uintptr
	classExtra, windowExtra            int32
	instance, icon, cursor, background uintptr
	menu, name                         *uint16
}

type trayPoint struct{ x, y int32 }

type trayMessage struct {
	window         uintptr
	message        uint32
	wParam, lParam uintptr
	time           uint32
	point          trayPoint
	private        uint32
}

type notifyIcon struct {
	size               uint32
	window             uintptr
	id, flags, message uint32
	icon               uintptr
	tip                [128]uint16
	state, stateMask   uint32
	info               [256]uint16
	version            uint32
	infoTitle          [64]uint16
	infoFlags          uint32
	guid               windows.GUID
	balloonIcon        uintptr
}

func winText(s string) *uint16 {
	p, _ := windows.UTF16PtrFromString(s)
	return p
}

func showDesktopError(window uintptr, description string) {
	trayUser.NewProc("MessageBoxW").Call(window, uintptr(unsafe.Pointer(winText(description))), uintptr(unsafe.Pointer(winText("BiliBridge"))), 0x10)
}

func openBiliLogin(window uintptr) {
	const loginURL = "https://www.bilibili.com/?bilibridge_login=1"
	result, _, _ := trayShell.NewProc("ShellExecuteW").Call(window,
		uintptr(unsafe.Pointer(winText("open"))), uintptr(unsafe.Pointer(winText(loginURL))), 0, 0, 1)
	if result <= 32 {
		showDesktopError(window, "无法打开浏览器。请在安装了 BiliBridge 扩展的 Chrome 或 Edge 中打开并登录 B 站。")
	}
}

func loadTrayIcon() uintptr {
	size, _, _ := trayUser.NewProc("GetSystemMetrics").Call(49)
	if size == 0 {
		size = 16
	}
	files := int(binary.LittleEndian.Uint16(trayIconBytes[4:]))
	best, closest := []byte(nil), 256
	for i := range files {
		entry := trayIconBytes[6+16*i:]
		width := int(entry[0])
		if width == 0 {
			width = 256
		}
		distance := max(width-int(size), int(size)-width)
		if distance < closest {
			offset := binary.LittleEndian.Uint32(entry[12:])
			length := binary.LittleEndian.Uint32(entry[8:])
			best, closest = trayIconBytes[offset:offset+length], distance
		}
	}
	if len(best) == 0 {
		return 0
	}
	icon, _, _ := trayUser.NewProc("CreateIconFromResourceEx").Call(uintptr(unsafe.Pointer(&best[0])), uintptr(len(best)), 1, 0x00030000, size, size, 0)
	return icon
}

func runTray(d *desktop) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	const (
		wmIcon    = 0x8001
		wmTimer   = 0x0113
		wmDestroy = 0x0002
		wmClose   = 0x0010
	)
	dirty := true
	applyAlbums := func(window uintptr) {
		if dirty && potplayer.FindWindow() == 0 {
			dirty = false
			cfg := d.settings()
			if err := updateAlbums(cfg); err != nil {
				showDesktopError(window, err.Error())
			}
		}
	}
	applyParser := func(window uintptr) {
		cfg := d.settings()
		if cfg.Home || cfg.Favorite || cfg.Live {
			if err := installAlbumParser(); err != nil {
				showDesktopError(window, err.Error())
			}
		}
	}
	instance, _, _ := trayKernel.NewProc("GetModuleHandleW").Call(0)
	taskbarCreated, _, _ := trayUser.NewProc("RegisterWindowMessageW").Call(uintptr(unsafe.Pointer(winText("TaskbarCreated"))))
	var icon notifyIcon
	notify := trayShell.NewProc("Shell_NotifyIconW")
	callback := windows.NewCallback(func(w uintptr, msg uint32, wp, lp uintptr) uintptr {
		switch msg {
		case wmIcon:
			if lp == 0x0205 || lp == 0x0202 {
				cfg := d.settings()
				startupEnabled := desktopStartupEnabled()
				switch trayMenu(w, cfg, d.hasSession(), startupEnabled) {
				case 1:
					cfg.Danmaku = !cfg.Danmaku
				case 2:
					cfg.Home = !cfg.Home
				case 3:
					cfg.Favorite = !cfg.Favorite
				case 4:
					cfg.Live = !cfg.Live
				case 5:
					trayUser.NewProc("DestroyWindow").Call(w)
					return 0
				case 6:
					openBiliLogin(w)
					return 0
				case 7:
					if err := setDesktopStartup(!startupEnabled); err != nil {
						showDesktopError(w, "无法设置开机自启。"+err.Error())
					}
					return 0
				default:
					return 0
				}
				if err := d.setSettings(cfg); err != nil {
					showDesktopError(w, err.Error())
				} else {
					dirty = true
					applyParser(w)
					applyAlbums(w)
				}
			}
			return 0
		case wmTimer:
			applyAlbums(w)
			return 0
		case wmClose:
			trayUser.NewProc("DestroyWindow").Call(w)
			return 0
		case wmDestroy:
			notify.Call(2, uintptr(unsafe.Pointer(&icon)))
			trayUser.NewProc("PostQuitMessage").Call(0)
			return 0
		}
		if msg != 0 && uintptr(msg) == taskbarCreated {
			notify.Call(0, uintptr(unsafe.Pointer(&icon)))
			return 0
		}
		value, _, _ := trayUser.NewProc("DefWindowProcW").Call(w, uintptr(msg), wp, lp)
		return value
	})
	name, _ := windows.UTF16PtrFromString("BiliBridgeTrayWindow")
	class := trayClass{proc: callback, instance: instance, name: name}
	if ok, _, err := trayUser.NewProc("RegisterClassW").Call(uintptr(unsafe.Pointer(&class))); ok == 0 {
		return err
	}
	window, _, err := trayUser.NewProc("CreateWindowExW").Call(0, uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(winText("BiliBridge"))), 0, 0, 0, 0, 0, 0, 0, instance, 0)
	if window == 0 {
		return err
	}
	image := loadTrayIcon()
	if image == 0 {
		image, _, _ = trayUser.NewProc("LoadIconW").Call(0, 32512)
	} else {
		defer trayUser.NewProc("DestroyIcon").Call(image)
	}
	icon = notifyIcon{size: uint32(unsafe.Sizeof(icon)), window: window, id: 1, flags: 7, message: wmIcon, icon: image}
	copy(icon.tip[:], windows.StringToUTF16("BiliBridge"))
	if ok, _, _ := notify.Call(0, uintptr(unsafe.Pointer(&icon))); ok == 0 {
		trayUser.NewProc("DestroyWindow").Call(window)
		return errors.New("无法创建 BiliBridge 托盘图标")
	}
	applyParser(window)
	applyAlbums(window)
	trayUser.NewProc("SetTimer").Call(window, 1, 1500, 0)
	var msg trayMessage
	for {
		value, _, err := trayUser.NewProc("GetMessageW").Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		switch int32(value) {
		case 0:
			return nil
		case -1:
			return err
		}
		trayUser.NewProc("TranslateMessage").Call(uintptr(unsafe.Pointer(&msg)))
		trayUser.NewProc("DispatchMessageW").Call(uintptr(unsafe.Pointer(&msg)))
	}
}

func trayMenu(window uintptr, cfg desktopConfig, signedIn, startupEnabled bool) uintptr {
	menu, _, _ := trayUser.NewProc("CreatePopupMenu").Call()
	if menu == 0 {
		return 0
	}
	defer trayUser.NewProc("DestroyMenu").Call(menu)
	appendMenu := trayUser.NewProc("AppendMenuW")
	login := "登录 B 站…"
	if signedIn {
		login = "B 站已同步（重新连接）"
	}
	appendMenu.Call(menu, 0, 6, uintptr(unsafe.Pointer(winText(login))))
	appendMenu.Call(menu, 0x0800, 0, 0)
	for i, item := range []struct {
		title string
		on    bool
	}{
		{"弹幕", cfg.Danmaku}, {"主页", cfg.Home}, {"收藏", cfg.Favorite}, {"关注的直播", cfg.Live},
	} {
		var flags uintptr
		if item.on {
			flags = 8
		}
		appendMenu.Call(menu, flags, uintptr(i+1), uintptr(unsafe.Pointer(winText(item.title))))
	}
	appendMenu.Call(menu, 0x0800, 0, 0)
	var startupFlags uintptr
	if startupEnabled {
		startupFlags = 8
	}
	appendMenu.Call(menu, startupFlags, 7, uintptr(unsafe.Pointer(winText("开机自启"))))
	appendMenu.Call(menu, 0, 5, uintptr(unsafe.Pointer(winText("退出"))))
	var pt trayPoint
	trayUser.NewProc("GetCursorPos").Call(uintptr(unsafe.Pointer(&pt)))
	trayUser.NewProc("SetForegroundWindow").Call(window)
	result, _, _ := trayUser.NewProc("TrackPopupMenu").Call(menu, 0x0102, uintptr(pt.x), uintptr(pt.y), 0, window, 0)
	trayUser.NewProc("PostMessageW").Call(window, 0, 0, 0)
	return result
}
