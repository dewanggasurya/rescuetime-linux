package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ActiveWindow represents the active window's essential properties.
type ActiveWindow struct {
	Class string
	Title string
}

// WindowBackend abstracts active window detection across compositors.
type WindowBackend interface {
	GetActiveWindow() (*ActiveWindow, error)
	Name() string
}

// HyprlandBackend gets the active window via hyprctl (Hyprland compositor).
type HyprlandBackend struct{}

type hyprctlWindow struct {
	Class string `json:"class"`
	Title string `json:"title"`
}

func (b *HyprlandBackend) GetActiveWindow() (*ActiveWindow, error) {
	cmd := exec.Command("hyprctl", "activewindow", "-j")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get active window from hyprctl: %v", err)
	}

	var window hyprctlWindow
	err = json.Unmarshal(output, &window)
	if err != nil {
		return nil, fmt.Errorf("failed to parse hyprctl JSON output: %v", err)
	}

	return &ActiveWindow{Class: window.Class, Title: window.Title}, nil
}

func (b *HyprlandBackend) Name() string { return "hyprland" }

// XdotoolBackend gets the active window via xdotool (X11/XWayland).
type XdotoolBackend struct{}

func (b *XdotoolBackend) GetActiveWindow() (*ActiveWindow, error) {
	// Get window ID first to avoid race between class/title lookups
	idOut, err := exec.Command("xdotool", "getactivewindow").Output()
	if err != nil {
		return nil, fmt.Errorf("xdotool getactivewindow failed: %v", err)
	}
	windowID := strings.TrimSpace(string(idOut))

	classOut, err := exec.Command("xdotool", "getwindowclassname", windowID).Output()
	if err != nil {
		return nil, fmt.Errorf("xdotool getwindowclassname failed: %v", err)
	}

	titleOut, err := exec.Command("xdotool", "getwindowname", windowID).Output()
	if err != nil {
		return nil, fmt.Errorf("xdotool getwindowname failed: %v", err)
	}

	return &ActiveWindow{
		Class: strings.TrimSpace(string(classOut)),
		Title: strings.TrimSpace(string(titleOut)),
	}, nil
}

func (b *XdotoolBackend) Name() string { return "xdotool" }

// GnomeBackend gets the active window on GNOME Wayland via the
// focused-window-dbus@flexagoon.com Shell extension.
type GnomeBackend struct{}

func (b *GnomeBackend) GetActiveWindow() (*ActiveWindow, error) {
	// busctl --json avoids parsing GVariant text escaping from gdbus.
	out, err := exec.Command("busctl", "--user", "call", "org.gnome.Shell",
		"/org/gnome/shell/extensions/FocusedWindow",
		"org.gnome.shell.extensions.FocusedWindow", "Get", "--json=short").Output()
	if err != nil {
		return nil, fmt.Errorf("FocusedWindow.Get failed (is focused-window-dbus@flexagoon.com enabled?): %v", err)
	}
	return parseGnomeFocusedWindow(out)
}

func parseGnomeFocusedWindow(out []byte) (*ActiveWindow, error) {
	var reply struct {
		Data []string `json:"data"`
	}
	if err := json.Unmarshal(out, &reply); err != nil {
		return nil, fmt.Errorf("failed to parse busctl JSON output: %v", err)
	}
	if len(reply.Data) == 0 || reply.Data[0] == "" {
		return nil, fmt.Errorf("no focused window")
	}
	var w struct {
		Title   string `json:"title"`
		WMClass string `json:"wm_class"`
	}
	if err := json.Unmarshal([]byte(reply.Data[0]), &w); err != nil {
		return nil, fmt.Errorf("failed to parse focused window JSON: %v", err)
	}
	return &ActiveWindow{Class: w.WMClass, Title: w.Title}, nil
}

func (b *GnomeBackend) Name() string { return "gnome" }

// GetIdleTime returns time since last keyboard/mouse input via Mutter's IdleMonitor (GNOME only).
// shortcut: GNOME-only; on other compositors it errors and idle detection is skipped.
func GetIdleTime() (time.Duration, error) {
	out, err := exec.Command("busctl", "--user", "call", "org.gnome.Mutter.IdleMonitor",
		"/org/gnome/Mutter/IdleMonitor/Core", "org.gnome.Mutter.IdleMonitor",
		"GetIdletime", "--json=short").Output()
	if err != nil {
		return 0, fmt.Errorf("IdleMonitor.GetIdletime failed: %v", err)
	}
	return parseIdleTime(out)
}

func parseIdleTime(out []byte) (time.Duration, error) {
	var reply struct {
		Data []uint64 `json:"data"`
	}
	if err := json.Unmarshal(out, &reply); err != nil || len(reply.Data) == 0 {
		return 0, fmt.Errorf("failed to parse idle time: %s", out)
	}
	return time.Duration(reply.Data[0]) * time.Millisecond, nil
}

// DetectBackend auto-detects the best available window backend.
// Preference order: hyprctl (Hyprland) > GNOME Wayland (FocusedWindow D-Bus) > xdotool (X11/XWayland).
func DetectBackend() (WindowBackend, error) {
	if _, err := exec.LookPath("hyprctl"); err == nil {
		return &HyprlandBackend{}, nil
	}
	if os.Getenv("XDG_SESSION_TYPE") == "wayland" && strings.Contains(os.Getenv("XDG_CURRENT_DESKTOP"), "GNOME") {
		if _, err := exec.LookPath("busctl"); err == nil {
			return &GnomeBackend{}, nil
		}
	}
	if _, err := exec.LookPath("xdotool"); err == nil {
		return &XdotoolBackend{}, nil
	}
	return nil, fmt.Errorf("no window backend found: install hyprctl (Hyprland) or xdotool (X11/XWayland)")
}
