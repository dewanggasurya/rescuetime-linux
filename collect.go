package main

// Activity upload via the official client's /collect endpoint (works on free accounts).
// Protocol reverse-engineered from RescueTime Linux client 2.16.5.1
// (RescueTime::Network::API::upload_activity_log, rt::network::request, rt::log_entry::to_csv):
//
//	POST https://{api_url}/collect   multipart/form-data, Accept: application/json, expects HTTP 200
//	fields: account_key, computer_name, os_username, client_version, os_id=6, data_version=2, gz=1,
//	        file_uuid=<uuid>, file=<uuid>.rtg (gzip of CSV log entries, format below)
//
// .rtg format, from a file captured from the official client (no header, all fields quoted, local time):
//
//	"kitty","0.0.0","","Laptop battery drain","","2026-11-09 08:07:49","2026-11-09 08:08:33","","","","",""
//	 app     version     window title            started               completed             (5 empty)

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"os/user"
	"strings"
	"time"
)

const (
	collectURL    = "https://api.rescuetime.com/collect"
	clientVersion = "2.16.5.1"
)

// collectSubmit uploads completed sessions via /collect and clears them only on success,
// so a failed upload is retried on the next tick instead of being dropped.
func collectSubmit(tracker *ActivityTracker, accountKey string) {
	sessions := tracker.GetSessions()
	if len(sessions) == 0 {
		slog.Debug("no completed sessions to upload")
		return
	}
	if err := submitSessions(accountKey, sessions); err != nil {
		slog.Warn("activity upload failed, will retry", "sessions", len(sessions), "error", err)
		tray.setUpload(false, fmt.Sprintf("Upload failed %s (retrying)", time.Now().Format("15:04")))
		return
	}
	tracker.ClearCompletedSessions()
	slog.Info("uploaded activity to RescueTime", "sessions", len(sessions))
	tray.setUpload(true, fmt.Sprintf("Uploaded %d sessions at %s", len(sessions), time.Now().Format("15:04")))
}

// csvQuote always quotes, like the official client; inner quotes doubled, newlines flattened.
func csvQuote(s string) string {
	s = strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// apiTime formats like rt::ptime_to_api_wstr ("%Y-%m-%d %H:%M:%S", local time).
func apiTime(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05") }

// defaultAppNames maps Wayland app IDs to the process names the official client sends,
// so RescueTime applies its existing categories instead of "Uncategorized" (-2).
var defaultAppNames = map[string]string{
	"com.microsoft.vscode": "code",
	"code-oss":             "code",
	"code-url-handler":     "code",
	"brave-browser":        "brave",
	"brave-browser-beta":   "brave",
	"brave-origin-beta":    "brave",
	"google-chrome":        "chrome",
	"org.mozilla.firefox":  "firefox",
	"dev.warp.warp":        "warp",
}

// appNameOverrides comes from config "app_names" and wins over defaultAppNames.
var appNameOverrides map[string]string

// rescueTimeAppName returns the name to upload for a window class (case-insensitive lookup).
func rescueTimeAppName(class string) string {
	key := strings.ToLower(class)
	for k, v := range appNameOverrides {
		if strings.ToLower(k) == key {
			return v
		}
	}
	if v, ok := defaultAppNames[key]; ok {
		return v
	}
	return class
}

// buildActivityLog renders sessions as the client's CSV and gzips it (.rtg contents).
func buildActivityLog(sessions []ActivitySession) ([]byte, error) {
	var csv strings.Builder
	for _, s := range sessions {
		fields := []string{
			// app, process_version, process_hash, window_title, extended_info (URL),
			// started, completed, entity_type, 4 unused (order from rt::log_entry::to_csv)
			rescueTimeAppName(s.AppClass), "0.0.0", "", s.WindowTitle, s.URL,
			apiTime(s.StartTime), apiTime(s.EndTime), "", "", "", "", "",
		}
		for i, f := range fields {
			fields[i] = csvQuote(f)
		}
		csv.WriteString(strings.Join(fields, ",") + "\n")
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(csv.String())); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func newUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// uploadActivityLog posts one gzipped activity log to /collect.
func uploadActivityLog(accountKey, computer, osUser string, rtg []byte) error {
	id := newUUID()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, kv := range [][2]string{
		{"account_key", accountKey}, {"computer_name", computer}, {"os_username", osUser},
		{"client_version", clientVersion}, {"os_id", "6"}, {"data_version", "2"},
		// rt::network::request::add_file adds gz=1|0; without it the server drops the gzipped file silently.
		{"gz", "1"}, {"file_uuid", id},
	} {
		mw.WriteField(kv[0], kv[1])
	}
	fw, err := mw.CreateFormFile("file", id+".rtg")
	if err != nil {
		return err
	}
	fw.Write(rtg)
	mw.Close()

	req, err := http.NewRequest("POST", collectURL, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "RescueTime Client v"+clientVersion)

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("collect request failed: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	// Success observed as HTTP 200 with body {"c":[0,"RT:ok"]}.
	if resp.StatusCode != http.StatusOK || !bytes.Contains(respBody, []byte("RT:ok")) {
		return fmt.Errorf("collect returned status %d: %s", resp.StatusCode, respBody)
	}
	return nil
}

// submitSessions uploads completed sessions; the caller clears them only on success.
func submitSessions(accountKey string, sessions []ActivitySession) error {
	if len(sessions) == 0 {
		return nil
	}
	computer, _ := os.Hostname()
	osUser := ""
	if u, err := user.Current(); err == nil {
		osUser = u.Username
	}
	rtg, err := buildActivityLog(sessions)
	if err != nil {
		return err
	}
	return uploadActivityLog(accountKey, computer, osUser, rtg)
}
