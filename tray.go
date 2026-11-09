package main

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"fyne.io/systray"
)

// trayUI is the status-bar app indicator (StatusNotifierItem / AppIndicator).
// A nil *trayUI is valid: every method is a no-op, so the tracker runs unchanged without -tray.
type trayUI struct {
	mu     sync.Mutex
	paused bool
	status uploadStatus
	base   image.Image // RescueTime logo, or a plain circle when none is found
	window *systray.MenuItem
	upload *systray.MenuItem
	pause  *systray.MenuItem
}

// uploadStatus drives the corner dot: grey before the first upload, then green or red.
type uploadStatus int

const (
	uploadUnknown uploadStatus = iota
	uploadOK
	uploadFailed
)

var tray *trayUI

func (t *trayUI) setWindow(s string) {
	if t == nil {
		return
	}
	if r := []rune(s); len(r) > 70 {
		s = string(r[:69]) + "…"
	}
	t.window.SetTitle(s)
}

// setUpload records the last upload result: menu text plus the green/red dot.
func (t *trayUI) setUpload(ok bool, s string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.status = uploadFailed
	if ok {
		t.status = uploadOK
	}
	t.mu.Unlock()
	t.upload.SetTitle(s)
	t.refreshIcon()
}

func (t *trayUI) isPaused() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.paused
}

func (t *trayUI) togglePause() {
	t.mu.Lock()
	t.paused = !t.paused
	paused := t.paused
	t.mu.Unlock()
	if paused {
		t.pause.SetTitle("Resume tracking")
	} else {
		t.pause.SetTitle("Pause tracking")
	}
	t.refreshIcon()
}

func (t *trayUI) refreshIcon() {
	t.mu.Lock()
	paused, status := t.paused, t.status
	t.mu.Unlock()
	systray.SetIcon(renderIcon(t.base, paused, status))
	tip := map[uploadStatus]string{
		uploadUnknown: "no upload yet",
		uploadOK:      "last upload OK",
		uploadFailed:  "last upload failed",
	}[status]
	if paused {
		systray.SetTooltip("RescueTime: paused, " + tip)
	} else {
		systray.SetTooltip("RescueTime: tracking, " + tip)
	}
}

// loadLogo returns the RescueTime logo without shipping it: ~/.config/rescuetime-linux/icon.png,
// else the icon of the installed RescueTime browser extension (Brave/Chrome/Chromium). nil if none.
func loadLogo() image.Image {
	home, _ := os.UserHomeDir()
	candidates := []string{filepath.Join(configDir(), "icon.png")}
	for _, pattern := range []string{
		"/.config/BraveSoftware/*/*/Extensions/bdakmnplckeopfghnlpocafcepegjeap/*/icons/icon-64.png",
		"/.config/*/*/Extensions/bdakmnplckeopfghnlpocafcepegjeap/*/icons/icon-64.png",
	} {
		m, _ := filepath.Glob(home + pattern)
		candidates = append(candidates, m...)
	}
	for _, p := range candidates {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		img, err := png.Decode(f)
		f.Close()
		if err == nil {
			return img
		}
	}
	return nil
}

// renderIcon draws the logo (greyscale when paused) at 64x64 with a status dot bottom-right.
func renderIcon(base image.Image, paused bool, status uploadStatus) []byte {
	const n = 64
	img := image.NewRGBA(image.Rect(0, 0, n, n))
	if base != nil {
		// shortcut: nearest-neighbour scale; fine for the 64px extension icon, blurry for odd sizes.
		b := base.Bounds()
		for y := 0; y < n; y++ {
			for x := 0; x < n; x++ {
				img.Set(x, y, base.At(b.Min.X+x*b.Dx()/n, b.Min.Y+y*b.Dy()/n))
			}
		}
	} else {
		fillCircle(img, n/2, n/2, 26, color.RGBA{0x3d, 0x4a, 0x5c, 0xff})
	}
	if paused {
		for y := 0; y < n; y++ {
			for x := 0; x < n; x++ {
				c := color.RGBAModel.Convert(img.At(x, y)).(color.RGBA)
				g := uint8((uint16(c.R)*3 + uint16(c.G)*6 + uint16(c.B)) / 10)
				img.Set(x, y, color.RGBA{g, g, g, c.A})
			}
		}
	}
	dot := map[uploadStatus]color.RGBA{
		uploadUnknown: {0x9e, 0x9e, 0x9e, 0xff},
		uploadOK:      {0x2e, 0xb8, 0x4b, 0xff},
		uploadFailed:  {0xe0, 0x2f, 0x2f, 0xff},
	}[status]
	fillCircle(img, n-14, n-14, 13, color.RGBA{0xff, 0xff, 0xff, 0xff}) // white ring keeps it visible on any panel
	fillCircle(img, n-14, n-14, 10, dot)
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

func fillCircle(img draw.Image, cx, cy, r int, c color.Color) {
	for y := cy - r; y <= cy+r; y++ {
		for x := cx - r; x <= cx+r; x++ {
			if (x-cx)*(x-cx)+(y-cy)*(y-cy) <= r*r {
				img.Set(x, y, c)
			}
		}
	}
}

// runWithTray shows the indicator and runs the tracker loop alongside it.
// systray.Run must own the main goroutine; the tracker exits the tray when it returns.
func runWithTray(run func()) {
	systray.Run(func() {
		t := &trayUI{base: loadLogo()}
		systray.SetTitle("RescueTime")
		t.window = systray.AddMenuItem("Waiting for first window…", "")
		t.window.Disable()
		t.upload = systray.AddMenuItem("No upload yet", "")
		t.upload.Disable()
		systray.AddSeparator()
		t.pause = systray.AddMenuItem("Pause tracking", "")
		quit := systray.AddMenuItem("Quit", "")
		t.refreshIcon()
		tray = t

		go func() {
			for {
				select {
				case <-t.pause.ClickedCh:
					t.togglePause()
				case <-quit.ClickedCh:
					// Reuse the tracker's SIGTERM path: it ends the session and does a final upload.
					syscall.Kill(os.Getpid(), syscall.SIGTERM)
					return
				}
			}
		}()
		go func() {
			run()
			systray.Quit()
		}()
	}, func() {})
}
