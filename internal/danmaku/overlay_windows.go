package danmaku

import (
	"cmp"
	"context"
	"log"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unsafe"

	"bilibridge/internal/bili"
	"bilibridge/internal/potplayer"
	"golang.org/x/sys/windows"
)

const segmentDuration = 6 * time.Minute

var (
	user             = windows.NewLazySystemDLL("user32.dll")
	gdi              = windows.NewLazySystemDLL("gdi32.dll")
	kernel           = windows.NewLazySystemDLL("kernel32.dll")
	getDC            = user.NewProc("GetDC")
	releaseDC        = user.NewProc("ReleaseDC")
	monitorFrom      = user.NewProc("MonitorFromWindow")
	monitorInfo      = user.NewProc("GetMonitorInfoW")
	createDC         = gdi.NewProc("CreateDCW")
	deviceCaps       = gdi.NewProc("GetDeviceCaps")
	selectObject     = gdi.NewProc("SelectObject")
	deleteObject     = gdi.NewProc("DeleteObject")
	deleteDC         = gdi.NewProc("DeleteDC")
	patBlt           = gdi.NewProc("PatBlt")
	bitBlt           = gdi.NewProc("BitBlt")
	transparentBlt   = windows.NewLazySystemDLL("msimg32.dll").NewProc("TransparentBlt")
	compatibleDC     = gdi.NewProc("CreateCompatibleDC")
	compatibleBitmap = gdi.NewProc("CreateCompatibleBitmap")
	setBkMode        = gdi.NewProc("SetBkMode")
	gdiFlush         = gdi.NewProc("GdiFlush")
	setTextColor     = gdi.NewProc("SetTextColor")
	textOut          = gdi.NewProc("TextOutW")
	textSize         = gdi.NewProc("GetTextExtentPoint32W")
	classOnce        sync.Once
	className, _     = windows.UTF16PtrFromString("BiliBridgeDanmaku")
	registered       bool
)

type rectangle struct{ left, top, right, bottom int32 }
type point struct{ x, y int32 }

func (r rectangle) empty() bool { return r.left >= r.right || r.top >= r.bottom }

func (r rectangle) union(other rectangle) rectangle {
	if r.empty() {
		return other
	}
	if other.empty() {
		return r
	}
	return rectangle{min(r.left, other.left), min(r.top, other.top), max(r.right, other.right), max(r.bottom, other.bottom)}
}

type display struct {
	size          uint32
	monitor, work rectangle
	flags         uint32
	device        [32]uint16
}

func currentDisplay(window uintptr) display {
	var info display
	info.size = uint32(unsafe.Sizeof(info))
	handle, _, _ := monitorFrom.Call(window, 2) // MONITOR_DEFAULTTONEAREST
	monitorInfo.Call(handle, uintptr(unsafe.Pointer(&info)))
	return info
}

func (d *display) refreshPeriod() time.Duration {
	if d.device[0] != 0 {
		dc, _, _ := createDC.Call(uintptr(unsafe.Pointer(&d.device[0])), 0, 0, 0)
		if dc != 0 {
			hz, _, _ := deviceCaps.Call(dc, 116) // VREFRESH
			deleteDC.Call(dc)
			if hz >= 24 && hz <= 1000 {
				return time.Second / time.Duration(hz)
			}
		}
	}
	return time.Second / 60
}

type winClass struct {
	style                              uint32
	callback                           uintptr
	classExtra, windowExtra            int32
	instance, icon, cursor, background uintptr
	menu, name                         *uint16
}
type message struct {
	window  uintptr
	message uint32
	wp, lp  uintptr
	time    uint32
	point   point
	private uint32
}
type sample struct {
	at     time.Duration
	when   time.Time
	status potplayer.Status
}
type segmentResult struct {
	index int
	items []bili.Danmaku
	err   error
}
type sprite struct {
	item   bili.Danmaku
	glyph  *glyph
	row    int
	expiry time.Duration
}

type glyph struct {
	bitmap        uintptr
	width, height int32
}

func discardSprites(items []sprite) {
	for _, s := range items {
		deleteObject.Call(s.glyph.bitmap)
	}
}

func createWindow(owner uintptr) uintptr {
	instance, _, _ := kernel.NewProc("GetModuleHandleW").Call(0)
	classOnce.Do(func() {
		def := user.NewProc("DefWindowProcW")
		handler := windows.NewCallback(func(w uintptr, msg uint32, wp, lp uintptr) uintptr {
			if msg == 0x84 {
				return ^uintptr(0)
			} // HTTRANSPARENT
			if msg == 0x21 {
				return 3
			} // MA_NOACTIVATE
			n, _, _ := def.Call(w, uintptr(msg), wp, lp)
			return n
		})
		cls := winClass{callback: handler, instance: instance, name: className}
		n, _, _ := user.NewProc("RegisterClassW").Call(uintptr(unsafe.Pointer(&cls)))
		registered = n != 0
	})
	if !registered {
		return 0
	}
	styles := uintptr(0x00080000 | 0x00000020 | 0x00000080 | 0x08000000) // layered, transparent, tool, no activate
	window, _, _ := user.NewProc("CreateWindowExW").Call(styles, uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(className)), 0x80000000, 0, 0, 1, 1, owner, 0, instance, 0)
	if window != 0 {
		user.NewProc("SetLayeredWindowAttributes").Call(window, 0, 0, 1) // LWA_COLORKEY
	}
	return window
}

type clipSearch struct{ bounds rectangle }

var sidebarChild = windows.NewCallback(func(child uintptr, target *clipSearch) uintptr {
	id, _, _ := user.NewProc("GetDlgCtrlID").Call(child)
	if id != 3230 {
		return 1
	}
	if visible, _, _ := user.NewProc("IsWindowVisible").Call(child); visible == 0 {
		return 1
	}
	var r rectangle
	user.NewProc("GetWindowRect").Call(child, uintptr(unsafe.Pointer(&r)))
	if r.left > target.bounds.left+100 && r.left < target.bounds.right &&
		r.right <= target.bounds.right+5 && r.bottom-r.top > 60 {
		target.bounds.right = r.left
	}
	return 1
})

func viewport(owner uintptr, screen rectangle, videoWidth, videoHeight int) rectangle {
	var rect rectangle
	if ok, _, _ := user.NewProc("GetClientRect").Call(owner, uintptr(unsafe.Pointer(&rect))); ok == 0 {
		return rectangle{}
	}
	var origin point
	user.NewProc("ClientToScreen").Call(owner, uintptr(unsafe.Pointer(&origin)))
	rect = rectangle{origin.x, origin.y, origin.x + rect.right, origin.y + rect.bottom}
	search := clipSearch{bounds: rect}
	user.NewProc("EnumChildWindows").Call(owner, sidebarChild, uintptr(unsafe.Pointer(&search)))
	rect = search.bounds
	if rect != screen && rect.bottom-rect.top > 160 {
		dpi, _, _ := user.NewProc("GetDpiForWindow").Call(owner)
		if dpi == 0 {
			dpi = 96
		}
		rect.top += int32(30 * dpi / 96)
		rect.bottom -= int32(43 * dpi / 96)
	}
	w, h := int(rect.right-rect.left), int(rect.bottom-rect.top)
	if w <= 0 || h <= 0 {
		return rectangle{}
	}
	if videoWidth > 0 && videoHeight > 0 {
		fitW, fitH := min(w, h*videoWidth/videoHeight), min(h, w*videoHeight/videoWidth)
		rect.left += int32((w - fitW) / 2)
		rect.top += int32((h - fitH) / 2)
		rect.right = rect.left + int32(fitW)
		rect.bottom = rect.top + int32(fitH)
	}
	return rect
}

type canvas struct {
	dc, bitmap, previous, font, previousFont uintptr
	glyphDC, glyphFont                       uintptr
	width, height, line, outline             int32
	dpi                                      uintptr
	painted                                  [2]rectangle
	placed, next                             []placement
	blank                                    bool
}

type placement struct {
	glyph *glyph
	x, y  int32
}

func (c *canvas) close() {
	if c.dc != 0 {
		selectObject.Call(c.glyphDC, c.glyphFont)
		deleteDC.Call(c.glyphDC)
		selectObject.Call(c.dc, c.previous)
		selectObject.Call(c.dc, c.previousFont)
		deleteObject.Call(c.font)
		deleteObject.Call(c.bitmap)
		deleteDC.Call(c.dc)
	}
	*c = canvas{}
}

func (c *canvas) size(window, dpi uintptr, width, height int32) bool {
	if c.dc != 0 && width == c.width && height == c.height && dpi == c.dpi {
		return true
	}
	c.close()
	dc, _, _ := getDC.Call(window)
	if dc == 0 {
		return false
	}
	defer releaseDC.Call(window, dc)
	mem, _, _ := compatibleDC.Call(dc)
	textDC, _, _ := compatibleDC.Call(dc)
	bitmap, _, _ := compatibleBitmap.Call(dc, uintptr(width), uintptr(height))
	if mem == 0 || textDC == 0 || bitmap == 0 {
		if mem != 0 {
			deleteDC.Call(mem)
		}
		if bitmap != 0 {
			deleteObject.Call(bitmap)
		}
		if textDC != 0 {
			deleteDC.Call(textDC)
		}
		return false
	}
	previous, _, _ := selectObject.Call(mem, bitmap)
	scale := int32(dpi)
	size := max(18*scale/96, min(36*scale/96, height/27))
	outline := max(1, size/20)
	face, _ := windows.UTF16PtrFromString("Microsoft YaHei UI")
	font, _, _ := gdi.NewProc("CreateFontW").Call(uintptr(-size), 0, 0, 0, 600, 0, 0, 0, 1, 0, 0, 4, 0, uintptr(unsafe.Pointer(face)))
	oldFont, _, _ := selectObject.Call(mem, font)
	oldTextFont, _, _ := selectObject.Call(textDC, font)
	setBkMode.Call(textDC, 1) // TRANSPARENT
	*c = canvas{
		dc: mem, bitmap: bitmap, previous: previous, font: font, previousFont: oldFont,
		glyphDC: textDC, glyphFont: oldTextFont, width: width, height: height, dpi: dpi,
		line: size + size/3 + 2*outline, outline: outline, blank: true,
	}
	return true
}

func (c *canvas) draw(window uintptr, active []sprite, now time.Duration) {
	var occupied [2]rectangle
	c.next = c.next[:0]
	speed := float64(c.width) / 5
	for _, s := range active {
		width := s.glyph.width
		x, y := (c.width-width)/2, int32(4)+int32(s.row)*c.line
		band := 0
		offset := int32((now - s.item.At).Seconds() * speed)
		switch s.item.Mode {
		case 4:
			y = c.height - int32(s.row+1)*c.line - 4
			band = 1
		case 5:
		case 6:
			x = offset - width
		default:
			x = c.width - offset
		}
		if y < 0 || x >= c.width || x+width <= 0 {
			continue
		}
		ink := rectangle{max(0, x), y, min(c.width, x+width), min(c.height, y+s.glyph.height)}
		occupied[band] = occupied[band].union(ink)
		c.next = append(c.next, placement{s.glyph, x, y})
	}
	if !c.blank && slices.Equal(c.placed, c.next) {
		return
	}
	dc, _, _ := getDC.Call(window)
	if dc == 0 {
		return
	}
	defer releaseDC.Call(window, dc)
	// Clear the old text and repaint only the bands occupied by this frame.
	dirty := [2]rectangle{c.painted[0].union(occupied[0]), c.painted[1].union(occupied[1])}
	if c.blank {
		dirty = [2]rectangle{{0, 0, c.width, c.height}}
	}
	for _, r := range dirty {
		if !r.empty() {
			patBlt.Call(c.dc, uintptr(r.left), uintptr(r.top), uintptr(r.right-r.left), uintptr(r.bottom-r.top), 0x42)
		}
	}
	for _, p := range c.next {
		w, h := uintptr(p.glyph.width), uintptr(p.glyph.height)
		previous, _, _ := selectObject.Call(c.glyphDC, p.glyph.bitmap)
		transparentBlt.Call(c.dc, uintptr(p.x), uintptr(p.y), w, h, c.glyphDC, 0, 0, w, h, 0)
		selectObject.Call(c.glyphDC, previous)
	}
	for _, r := range dirty {
		if !r.empty() {
			x, y := uintptr(r.left), uintptr(r.top)
			bitBlt.Call(dc, x, y, uintptr(r.right-r.left), uintptr(r.bottom-r.top), c.dc, x, y, 0xcc0020)
		}
	}
	gdiFlush.Call()
	c.placed, c.next = c.next, c.placed
	c.painted, c.blank = occupied, false
}

// Rasterize once per comment. Subsequent frames only move the bitmap.
func (c *canvas) renderGlyph(text []uint16, extent point, rgb uint32) *glyph {
	pad := c.outline + 2
	w, h := extent.x+2*pad, extent.y+2*pad
	bitmap, _, _ := compatibleBitmap.Call(c.dc, uintptr(w), uintptr(h))
	if bitmap == 0 {
		return nil
	}
	previous, _, _ := selectObject.Call(c.glyphDC, bitmap)
	defer selectObject.Call(c.glyphDC, previous)
	patBlt.Call(c.glyphDC, 0, 0, uintptr(w), uintptr(h), 0x42)
	setTextColor.Call(c.glyphDC, 0x171717)
	count := uintptr(len(text) - 1)
	for _, offset := range [...]point{{-1, -1}, {0, -1}, {1, -1}, {-1, 0}, {1, 0}, {-1, 1}, {0, 1}, {1, 1}} {
		textOut.Call(c.glyphDC, uintptr(pad+offset.x*c.outline), uintptr(pad+offset.y*c.outline), uintptr(unsafe.Pointer(&text[0])), count)
	}
	if rgb == 0 {
		rgb = 0xffffff
	}
	setTextColor.Call(c.glyphDC, uintptr((rgb&0xff)<<16|rgb&0xff00|rgb>>16))
	textOut.Call(c.glyphDC, uintptr(pad), uintptr(pad), uintptr(unsafe.Pointer(&text[0])), count)
	return &glyph{bitmap, w, h}
}

func (c *canvas) add(items []sprite, item bili.Danmaku) []sprite {
	text, err := windows.UTF16FromString(item.Text)
	if err != nil {
		return items
	}
	var extent point
	textSize.Call(c.dc, uintptr(unsafe.Pointer(&text[0])), uintptr(len(text)-1), uintptr(unsafe.Pointer(&extent)))
	width := extent.x + 2*(c.outline+2)
	if width > c.width || extent.x <= 0 || extent.y <= 0 {
		return items
	}
	rows := min(10, max(1, int(c.height/(3*c.line))))
	dwell := 5*time.Second + time.Duration(float64(5*time.Second)*float64(width)/float64(c.width))
	if item.Mode == 4 || item.Mode == 5 {
		rows, dwell = 2, 4*time.Second
	}
	for row := 0; row < rows; row++ {
		busy := false
		for _, s := range items {
			if s.row != row || s.expiry <= item.At {
				continue
			}
			sFixed, itemFixed := s.item.Mode == 4 || s.item.Mode == 5, item.Mode == 4 || item.Mode == 5
			if s.item.Mode == 4 || item.Mode == 4 {
				busy = s.item.Mode == item.Mode
			} else {
				gap := time.Duration(float64(5*time.Second) * float64(s.glyph.width+42) / float64(c.width))
				busy = sFixed || itemFixed || (s.item.Mode == 6) != (item.Mode == 6) || item.At-s.item.At < gap
			}
			if busy {
				break
			}
		}
		if !busy {
			if glyph := c.renderGlyph(text, extent, item.Color); glyph != nil {
				return append(items, sprite{item, glyph, row, item.At + dwell})
			}
			return items
		}
	}
	return items
}

func Show(ctx context.Context, player potplayer.Window, mediaTitle string, width, height int,
	enabled func() bool, fetch func(context.Context, int) ([]bili.Danmaku, error)) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	owner := uintptr(player)
	window := createWindow(owner)
	if window == 0 {
		log.Print("无法创建弹幕窗口")
		return
	}
	defer user.NewProc("DestroyWindow").Call(window)
	var surface canvas
	defer surface.close()
	positions := make(chan sample, 1)
	go samplePlayer(ctx, player, positions)
	segments := make(map[int][]bili.Danmaku)
	pending := make(map[int]bool)
	retry := make(map[int]time.Time)
	results := make(chan segmentResult, 4)
	var clock playbackClock
	var last time.Duration
	var nextGeometry time.Time
	var nextRefresh time.Time
	var screen display
	var bounds rectangle
	var items []sprite
	defer func() { discardSprites(items) }()
	shown := false
	const idlePeriod = time.Second / 30
	period, refresh := idlePeriod, time.Second/60
	frames := time.NewTicker(period)
	defer frames.Stop()
	show := user.NewProc("ShowWindow")
	setPos := user.NewProc("SetWindowPos")
	peek := user.NewProc("PeekMessageW")
	dispatch := user.NewProc("DispatchMessageW")
	foreground := user.NewProc("GetForegroundWindow")
	valid := user.NewProc("IsWindow")
	for {
		select {
		case <-ctx.Done():
			return
		case measured := <-positions:
			if clock.observe(measured) {
				discardSprites(items)
				items, last = nil, measured.at-250*time.Millisecond
			}
		case result := <-results:
			delete(pending, result.index)
			if result.err != nil {
				retry[result.index] = time.Now().Add(8 * time.Second)
			} else {
				slices.SortStableFunc(result.items, func(a, b bili.Danmaku) int { return cmp.Compare(a.At, b.At) })
				segments[result.index] = result.items
			}
		case <-frames.C:
			now := time.Now()
			var msg message
			for {
				n, _, _ := peek.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0, 1)
				if n == 0 {
					break
				}
				dispatch.Call(uintptr(unsafe.Pointer(&msg)))
			}
			if ok, _, _ := valid.Call(owner); ok == 0 {
				return
			}
			if now.After(nextGeometry) {
				if mediaTitle != "" && !strings.Contains(player.Title(), mediaTitle) {
					return
				}
			}
			fg, _, _ := foreground.Call()
			visible := enabled() && fg == owner && clock.lastSample.status != potplayer.Stopped && !clock.lastSample.when.IsZero()
			wantedPeriod := idlePeriod
			if visible && clock.lastSample.status == potplayer.Playing {
				wantedPeriod = refresh
			}
			if wantedPeriod != period {
				frames.Reset(wantedPeriod)
				period = wantedPeriod
			}
			if !visible {
				if shown {
					show.Call(window, 0)
					shown = false
				}
				discardSprites(items)
				items, last = nil, clock.position(now)-250*time.Millisecond
				continue
			}
			at := clock.position(now)
			if at < last {
				continue
			}
			index := 1 + int(max(0, at)/segmentDuration)
			for key := range segments {
				if key < index-1 || key > index+1 {
					delete(segments, key)
				}
			}
			for _, i := range []int{index, index + 1} {
				_, current := segments[i]
				if current || pending[i] || now.Before(retry[i]) || len(pending) >= 4 {
					continue
				}
				pending[i] = true
				go func(index int) {
					data, err := fetch(ctx, index)
					select {
					case results <- segmentResult{index, data, err}:
					case <-ctx.Done():
					}
				}(i)
			}
			if now.After(nextGeometry) || !shown {
				nextGeometry = now.Add(125 * time.Millisecond)
				current := currentDisplay(owner)
				if current.device != screen.device || now.After(nextRefresh) {
					refresh = current.refreshPeriod()
					nextRefresh = now.Add(2 * time.Second)
				}
				screen = current
				r := viewport(owner, screen.monitor, width, height)
				w, h := r.right-r.left, r.bottom-r.top
				if w <= 0 || h <= 0 {
					continue
				}
				dpi, _, _ := user.NewProc("GetDpiForWindow").Call(owner)
				if dpi == 0 {
					dpi = 96
				}
				if w != surface.width || h != surface.height || dpi != surface.dpi {
					discardSprites(items)
					items, last = nil, at-250*time.Millisecond
					if !surface.size(window, dpi, w, h) {
						continue
					}
				}
				if r != bounds || !shown {
					setPos.Call(window, ^uintptr(0), uintptr(r.left), uintptr(r.top), uintptr(w), uintptr(h), 0x0050)
					shown, bounds = true, r
					surface.blank = true
				}
			}
			items = slices.DeleteFunc(items, func(s sprite) bool {
				if s.expiry > at {
					return false
				}
				deleteObject.Call(s.glyph.bitmap)
				return true
			})
			prev := 1 + int(max(0, last)/segmentDuration)
			if _, ok := segments[index]; !ok {
				surface.draw(window, items, at)
				continue
			}
			for i := max(prev, index-1); i <= index; i++ {
				list := segments[i]
				first := sort.Search(len(list), func(n int) bool { return list[n].At > last })
				for _, item := range list[first:] {
					if item.At > at {
						break
					}
					if at-item.At <= 700*time.Millisecond {
						items = surface.add(items, item)
					}
				}
			}
			last = at
			surface.draw(window, items, at)
		}
	}
}

func samplePlayer(ctx context.Context, player potplayer.Window, result chan<- sample) {
	tick := time.NewTicker(90 * time.Millisecond)
	defer tick.Stop()
	for {
		position, err := player.Position()
		status, statusErr := player.Status()
		if err == nil && statusErr == nil {
			select {
			case result <- sample{position, time.Now(), status}:
			default:
			}
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			return
		}
	}
}
