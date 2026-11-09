package main

import (
	"bytes"
	"compress/gzip"
	"image/color"
	"image/png"
	"io"
	"os"
	"testing"
	"time"
)

func TestApplyCLIFlagsRejectsBadDuration(t *testing.T) {
	c := DefaultConfig()
	if err := c.ApplyCLIFlags(map[string]string{"submission-interval": "1m,"}); err == nil {
		t.Fatal("expected error for 1m,")
	}
	if err := c.ApplyCLIFlags(map[string]string{"submission-interval": "1m"}); err != nil || c.SubmissionInterval.Duration != time.Minute {
		t.Fatalf("1m: %v %v", c.SubmissionInterval, err)
	}
}

func TestIdleEndsSessionAtIdleStart(t *testing.T) {
	if d, err := parseIdleTime([]byte(`{"type":"t","data":[5193]}`)); err != nil || d != 5193*time.Millisecond {
		t.Fatalf("parseIdleTime: %v %v", d, err)
	}
	tr := NewActivityTracker(30*time.Second, 10*time.Second)
	tr.StartSession("firefox", "x", "")
	tr.mu.Lock()
	tr.currentSession.StartTime = time.Now().Add(-20 * time.Minute)
	tr.mu.Unlock()
	tr.EndCurrentSessionAt(time.Now().Add(-15 * time.Minute))
	s := tr.GetSessions()
	if len(s) != 1 || s[0].Duration.Round(time.Minute) != 5*time.Minute {
		t.Fatalf("want one 5m session, got %+v", s)
	}
}

func TestParseGnomeFocusedWindow(t *testing.T) {
	out := []byte(`{"type":"s","data":["{\"title\":\"It's \\\"quoted\\\"\",\"wm_class\":\"firefox\",\"focus\":true}"]}`)
	w, err := parseGnomeFocusedWindow(out)
	if err != nil {
		t.Fatal(err)
	}
	if w.Class != "firefox" || w.Title != `It's "quoted"` {
		t.Fatalf("got %+v", w)
	}
	if _, err := parseGnomeFocusedWindow([]byte(`{"type":"s","data":[""]}`)); err == nil {
		t.Fatal("expected error for no focused window")
	}
}

func TestBuildActivityLog(t *testing.T) {
	st := time.Date(2026, 11, 9, 7, 43, 34, 0, time.Local)
	rtg, err := buildActivityLog([]ActivitySession{{StartTime: st, EndTime: st.Add(2 * time.Minute), AppClass: "kitty", WindowTitle: `a, "b"`}})
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(rtg))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(zr)
	want := `"kitty","0.0.0","","a, ""b""","","2026-11-09 07:43:34","2026-11-09 07:45:34","","","","",""` + "\n"
	if string(got) != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestRunSummaryKeepsSubmittedSessions(t *testing.T) {
	tr := NewActivityTracker(30*time.Second, 10*time.Second)
	tr.LoadSessions([]ActivitySession{{AppClass: "kitty", Duration: time.Minute}})
	tr.ClearCompletedSessions() // what a successful upload does
	if len(tr.GetActivitySummaries()) != 0 {
		t.Fatal("submitted sessions must not be resubmitted")
	}
	if s := tr.GetRunSummaries()["kitty"]; s.TotalDuration != time.Minute {
		t.Fatalf("run summary lost submitted session: %+v", s)
	}
}

func TestRescueTimeAppName(t *testing.T) {
	appNameOverrides = map[string]string{"KittyTest": "kitty"}
	defer func() { appNameOverrides = nil }()
	for in, want := range map[string]string{"com.microsoft.VSCode": "code", "brave-origin-beta": "brave", "kittytest": "kitty", "mailspring": "mailspring"} {
		if got := rescueTimeAppName(in); got != want {
			t.Errorf("%s: got %s want %s", in, got, want)
		}
	}
}

func TestRotatingFileCapsSize(t *testing.T) {
	path := t.TempDir() + "/x.log"
	r, err := openRotatingFile(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		r.Write(bytes.Repeat([]byte("a"), 30))
	}
	r.Close()
	for _, p := range []string{path, path + ".1"} {
		st, err := os.Stat(p)
		if err != nil || st.Size() > 100 {
			t.Fatalf("%s: size %v err %v", p, st, err)
		}
	}
}

func TestExtensionHandshakeAndURL(t *testing.T) {
	reply, keep := handleExtensionMessage(map[string]any{"message_id": float64(0), "account_key": "other"}, "mine")
	if keep || reply["valid"] != false {
		t.Fatalf("wrong account must be rejected: %v", reply)
	}
	reply, keep = handleExtensionMessage(map[string]any{"message_id": float64(0), "account_key": "mine"}, "mine")
	if !keep || reply["valid"] != true || reply["protocol_version"] != 1 {
		t.Fatalf("handshake: %v", reply)
	}
	handleExtensionMessage(map[string]any{"message_id": float64(2), "url": "https://github.com/x"}, "mine")
	if urlForWindow("brave-origin-beta") != "https://github.com/x" || urlForWindow("kitty") != "" {
		t.Fatal("url only applies to browser windows")
	}
	if allowedExtensionOrigin("https://evil.example") || !allowedExtensionOrigin(chromeExtensionOrigin) {
		t.Fatal("origin check")
	}
}

func TestShortPiecesOfSameSiteAreKept(t *testing.T) {
	tr := NewActivityTracker(30*time.Second, 10*time.Second)
	t0 := time.Now().Add(-time.Minute)
	tr.mu.Lock()
	tr.sessions = []ActivitySession{{AppClass: "brave", URL: "https://www.google.com/search?q=a", StartTime: t0, EndTime: t0.Add(20 * time.Second), Duration: 20 * time.Second}}
	// 3s piece on the same site (new search URL) and a 3s piece of another app
	tr.currentSession = &ActivitySession{AppClass: "brave", URL: "https://www.google.com/search?q=b", StartTime: t0.Add(20 * time.Second), Active: true}
	tr.endCurrentSessionUnsafe(t0.Add(23 * time.Second))
	tr.currentSession = &ActivitySession{AppClass: "kitty", StartTime: t0.Add(23 * time.Second), Active: true}
	tr.endCurrentSessionUnsafe(t0.Add(26 * time.Second))
	tr.mu.Unlock()
	s := tr.GetSessions()
	if len(s) != 1 || s[0].Duration != 23*time.Second || s[0].URL != "https://www.google.com/search?q=b" {
		t.Fatalf("want one 23s google session, got %+v", s)
	}
}

func TestRenderIconStatusDot(t *testing.T) {
	for st, want := range map[uploadStatus]color.RGBA{uploadOK: {0x2e, 0xb8, 0x4b, 0xff}, uploadFailed: {0xe0, 0x2f, 0x2f, 0xff}} {
		img, err := png.Decode(bytes.NewReader(renderIcon(nil, false, st)))
		if err != nil {
			t.Fatal(err)
		}
		if got := color.RGBAModel.Convert(img.At(50, 50)).(color.RGBA); got != want {
			t.Errorf("status %d: dot %v want %v", st, got, want)
		}
	}
}
