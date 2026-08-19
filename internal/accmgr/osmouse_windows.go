//go:build windows

package accmgr

// OS-level mouse control for the Baxia slider. CDP Input.dispatchMouseEvent
// events are DOM-trusted but carry no hardware traits (movementX/Y stays 0,
// no OS timestamps) — the Aliyun wasm scores exactly those. Moving the REAL
// cursor via Win32 SendInput produces genuine hardware mouse events.

import (
	"crypto/rand"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	osUser32           = windows.NewLazySystemDLL("user32.dll")
	procSendInput      = osUser32.NewProc("SendInput")
	procGetSysMet      = osUser32.NewProc("GetSystemMetrics")
	procSetCursorPos   = osUser32.NewProc("SetCursorPos")
	procEnumWindows    = osUser32.NewProc("EnumWindows")
	procGetWinTextW    = osUser32.NewProc("GetWindowTextW")
	procIsWinVisible   = osUser32.NewProc("IsWindowVisible")
	procSetForeground  = osUser32.NewProc("SetForegroundWindow")
	procShowWindow     = osUser32.NewProc("ShowWindow")
	procBringToTop     = osUser32.NewProc("BringWindowToTop")
)

const (
	mouseeventfMove     = 0x0001
	mouseeventfLeftDown = 0x0002
	mouseeventfLeftUp   = 0x0004
	mouseeventfAbsolute = 0x8000
)

// winMouseInput mirrors Windows MOUSEINPUT inside INPUT (64-bit layout:
// 4-byte type + 4-byte padding, then the mouse struct).
type winInput struct {
	typ     uint32
	_       uint32
	dx      int32
	dy      int32
	data    uint32
	flags   uint32
	time    uint32
	extrainfo uintptr
}

func osScreenSize() (int, int) {
	w, _, _ := procGetSysMet.Call(0) // SM_CXSCREEN
	h, _, _ := procGetSysMet.Call(1) // SM_CYSCREEN
	return int(w), int(h)
}

func osSendMouse(flags uint32, x, y int) {
	sw, sh := osScreenSize()
	in := winInput{typ: 0, flags: flags}
	if flags&mouseeventfAbsolute != 0 {
		in.dx = int32(x * 65535 / sw)
		in.dy = int32(y * 65535 / sh)
	}
	procSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
}

var (
	procGetCursorPos = osUser32.NewProc("GetCursorPos")
)

// osCursorPos returns the current real cursor position (physical pixels).
func osCursorPos() (int, int) {
	var pt struct{ x, y int32 }
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	return int(pt.x), int(pt.y)
}

// osMouseDragTo performs a human-like drag of the REAL cursor from
// (x,y) to (x+dist,y) in physical screen pixels. The trajectory is
// deliberately DIRTY: a scored-perfect smoothstep at exactly the track
// length is itself a bot signature once Baxia has hardened — humans
// approach from wherever the cursor was, move in bursts, undershoot the
// end, then micro-correct.
func osMouseDragTo(x, y, dist int) {
	rnd := func(n int) int { var b [1]byte; _, _ = rand.Read(b[:]); return int(b[0]) % n }

	// 1) approach: slide from wherever the cursor physically is to just
	// above the handle — a teleport straight onto the handle leaves no
	// pre-press movement trail, which the wasm can score.
	cx, cy := osCursorPos()
	ax, ay := x-2+rnd(5), y-1+rnd(3)
	apSteps := 18 + rnd(22)
	for i := 1; i <= apSteps; i++ {
		prog := float64(i) / float64(apSteps)
		ease := prog * prog * (3 - 2*prog)
		mx := cx + int(float64(ax-cx)*ease)
		my := cy + int(float64(ay-cy)*ease) + rnd(3) - 1
		osSendMouse(mouseeventfAbsolute|mouseeventfMove, mx, my)
		time.Sleep(time.Duration(6+rnd(18)) * time.Millisecond)
	}
	// hover, then press and hold
	time.Sleep(time.Duration(180 + rnd(350)) * time.Millisecond)
	osSendMouse(mouseeventfLeftDown, 0, 0)
	time.Sleep(time.Duration(220+rnd(380)) * time.Millisecond)

	// 2) main drag in 2-4 bursts with brief pauses; land SHORT of the end
	// (92-97%), like a human who stops early.
	target := int(float64(dist) * (0.92 + float64(rnd(6))/100.0))
	pos := 0
	yOff := 0
	bursts := 2 + rnd(3)
	for b := 0; b < bursts; b++ {
		remain := target - pos
		span := remain / (bursts - b)
		if span < 8 {
			span = remain
		}
		steps := 10 + rnd(18)
		for i := 1; i <= steps; i++ {
			prog := float64(i) / float64(steps)
			ease := prog * prog * (3 - 2*prog)
			yOff += rnd(3) - 1
			if yOff > 3 {
				yOff = 3
			} else if yOff < -3 {
				yOff = -3
			}
			osSendMouse(mouseeventfAbsolute|mouseeventfMove, x+pos+int(float64(span)*ease), y+yOff)
			time.Sleep(time.Duration(7+rnd(30)) * time.Millisecond)
		}
		pos += span
		if b < bursts-1 {
			time.Sleep(time.Duration(120 + rnd(400)) * time.Millisecond)
		}
	}

	// 3) micro-correction to the actual end: a few slow tiny steps.
	for pos < dist {
		step := 2 + rnd(6)
		if pos+step > dist {
			step = dist - pos
		}
		pos += step
		yOff += rnd(3) - 1
		osSendMouse(mouseeventfAbsolute|mouseeventfMove, x+pos, y+yOff)
		time.Sleep(time.Duration(40+rnd(90)) * time.Millisecond)
	}
	time.Sleep(time.Duration(140+rnd(240)) * time.Millisecond)
	osSendMouse(mouseeventfLeftUp, 0, 0)
}

// osMouseMoveTo moves the real cursor without pressing (approach path).
func osMouseMoveTo(x, y int) {
	osSendMouse(mouseeventfAbsolute|mouseeventfMove, x, y)
}

// osMouseAvailable reports whether OS-level mouse control works here.
func osMouseAvailable() bool { return true }

// osFocusWindowByTitle finds a visible top-level window whose title contains
// sub (case-insensitive) and forces it to the foreground. Real SendInput
// clicks land on whatever window is under the cursor — if the main browser
// window overlaps the SSO popup, the drag hits the wrong window. CDP's
// page.BringToFront activates the tab but does not reliably raise the OS
// window on Windows.
func osFocusWindowByTitle(sub string) bool {
	sub = strings.ToLower(sub)
	var found uintptr
	cb := windows.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		if v, _, _ := procIsWinVisible.Call(hwnd); v == 0 {
			return 1
		}
		buf := make([]uint16, 256)
		procGetWinTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		title := strings.ToLower(windows.UTF16ToString(buf))
		if sub != "" && strings.Contains(title, sub) {
			found = hwnd
			return 0
		}
		return 1
	})
	procEnumWindows.Call(cb, 0)
	if found == 0 {
		return false
	}
	procShowWindow.Call(found, 9) // SW_RESTORE
	procBringToTop.Call(found)
	procSetForeground.Call(found)
	return true
}
