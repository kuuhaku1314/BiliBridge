//go:build windows

// Package potplayer launches PotPlayer and controls it through the window
// messages it answers (the PotPlayer remote control API).
package potplayer

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows/registry"
)

const exeName = "PotPlayerMini64.exe"

// FindExe locates PotPlayerMini64.exe.
func FindExe() (string, error) {
	var candidates []string
	if k, err := registry.OpenKey(registry.CURRENT_USER, `Software\DAUM\PotPlayer64`, registry.QUERY_VALUE); err == nil {
		if dir, _, err := k.GetStringValue("ProgramFolder"); err == nil && dir != "" {
			candidates = append(candidates, filepath.Join(dir, exeName))
		}
		k.Close()
	}
	// The potplayer:// protocol handler names the executable.
	if k, err := registry.OpenKey(registry.CLASSES_ROOT, `potplayer\shell\open\command`, registry.QUERY_VALUE); err == nil {
		if cmd, _, err := k.GetStringValue(""); err == nil {
			if m := regexp.MustCompile(`(?i)"?([^"]*?\.exe)`).FindStringSubmatch(cmd); m != nil {
				candidates = append(candidates, m[1])
			}
		}
		k.Close()
	}
	for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)"} {
		if dir := os.Getenv(env); dir != "" {
			candidates = append(candidates, filepath.Join(dir, "DAUM", "PotPlayer", exeName), filepath.Join(dir, "PotPlayer", exeName))
		}
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, nil
		}
	}
	return "", errors.New("PotPlayer not found")
}

// Launch opens url in PotPlayer.
func Launch(exe, url string) error {
	cmd := exec.Command(exe, url)
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

// OpenTracks opens video and its external audio in the current player, starting
// the application if there is no window yet.
func OpenTracks(exe, videoURL, audioURL string) error {
	return openCurrent(exe, videoURL, audioURL, "/same")
}

// OpenMedia opens one input with the same window reuse as OpenTracks.
func OpenMedia(exe, mediaURL string) error {
	return openCurrent(exe, mediaURL)
}

func openCurrent(exe string, args ...string) error {
	mode := "/current"
	if FindWindow() == 0 {
		mode = "/new"
	}
	cmd := exec.Command(exe, append(args, mode)...)
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

var (
	user32                       = syscall.NewLazyDLL("user32.dll")
	procFindWindowW              = user32.NewProc("FindWindowW")
	procFindWindowExW            = user32.NewProc("FindWindowExW")
	procGetWindowTextW           = user32.NewProc("GetWindowTextW")
	procSendMessageTimeoutW      = user32.NewProc("SendMessageTimeoutW")
	procGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")
)

const (
	wmUser = 0x0400

	cmdGetTotalTime   = 0x5002
	cmdGetCurrentTime = 0x5004
	cmdSetCurrentTime = 0x5005
	cmdGetPlayStatus  = 0x5006
	cmdSetPlayStatus  = 0x5007
)

// Status is PotPlayer's playback state.
type Status int

const (
	Stopped Status = -1
	Paused  Status = 1
	Playing Status = 2
)

func (s Status) String() string {
	switch s {
	case Stopped:
		return "stopped"
	case Paused:
		return "paused"
	case Playing:
		return "playing"
	}
	return "unknown"
}

// Window is a PotPlayer main window.
type Window uintptr

// FindWindow returns the first PotPlayer window, or 0 if none exists.
func FindWindow() Window {
	cls, _ := syscall.UTF16PtrFromString("PotPlayer64")
	h, _, _ := procFindWindowW.Call(uintptr(unsafe.Pointer(cls)), 0)
	return Window(h)
}

// FindSingleWindow returns the player only when there is an unambiguous target.
func FindSingleWindow() Window {
	first := FindWindow()
	if first == 0 {
		return 0
	}
	cls, _ := syscall.UTF16PtrFromString("PotPlayer64")
	next, _, _ := procFindWindowExW.Call(0, uintptr(first), uintptr(unsafe.Pointer(cls)), 0)
	if next != 0 {
		return 0
	}
	return first
}

func FindPlaybackWindow(ctx context.Context, pid uint32, title string) (Window, error) {
	cls, _ := syscall.UTF16PtrFromString("PotPlayer64")
	ticker := time.NewTicker(120 * time.Millisecond)
	defer ticker.Stop()
	for {
		var match Window
		for after := uintptr(0); ; {
			next, _, _ := procFindWindowExW.Call(0, after, uintptr(unsafe.Pointer(cls)), 0)
			if next == 0 {
				break
			}
			after = next
			w := Window(next)
			if pid != 0 && w.PID() == pid {
				return w, nil
			}
			if pid == 0 && title != "" && strings.Contains(w.Title(), title) {
				if match != 0 {
					match = 0
					break
				}
				match = w
			}
		}
		if match != 0 {
			return match, nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (w Window) Title() string {
	var buf [1024]uint16
	n, _, _ := procGetWindowTextW.Call(uintptr(w), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return syscall.UTF16ToString(buf[:n])
}

// WaitWindow waits up to timeout for a PotPlayer window to appear.
func WaitWindow(timeout time.Duration) (Window, error) {
	for deadline := time.Now().Add(timeout); ; time.Sleep(100 * time.Millisecond) {
		if w := FindWindow(); w != 0 {
			return w, nil
		}
		if time.Now().After(deadline) {
			return 0, errors.New("PotPlayer window did not appear")
		}
	}
}

// PID returns the id of the process owning the window.
func (w Window) PID() uint32 {
	var pid uint32
	procGetWindowThreadProcessId.Call(uintptr(w), uintptr(unsafe.Pointer(&pid)))
	return pid
}

// send sends a remote control command, failing if PotPlayer is hung or gone.
func (w Window) send(cmd, arg uintptr) (uintptr, error) {
	const smtoAbortIfHung = 0x0002
	var result uintptr
	ok, _, err := procSendMessageTimeoutW.Call(uintptr(w), wmUser, cmd, arg, smtoAbortIfHung, 500, uintptr(unsafe.Pointer(&result)))
	if ok == 0 {
		return 0, err
	}
	return result, nil
}

// Position returns the playback position.
func (w Window) Position() (time.Duration, error) {
	ms, err := w.send(cmdGetCurrentTime, 0)
	return time.Duration(int32(ms)) * time.Millisecond, err
}

// Duration returns the length of the current media.
func (w Window) Duration() (time.Duration, error) {
	ms, err := w.send(cmdGetTotalTime, 0)
	return time.Duration(int32(ms)) * time.Millisecond, err
}

// Seek jumps to pos.
func (w Window) Seek(pos time.Duration) error {
	_, err := w.send(cmdSetCurrentTime, uintptr(pos.Milliseconds()))
	return err
}

// Status returns the playback state.
func (w Window) Status() (Status, error) {
	s, err := w.send(cmdGetPlayStatus, 0)
	return Status(int32(s)), err
}

// SetStatus pauses or resumes playback.
func (w Window) SetStatus(s Status) error {
	_, err := w.send(cmdSetPlayStatus, uintptr(s))
	return err
}
