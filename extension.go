package main

// Bridge for the official RescueTime browser extension, so browser time is logged per URL.
// Protocol from the extension's lib/RescueTimeWebSocket.js (v3.2.10, protocol_version 1):
//
//	extension connects to ws://localhost:<port>?from=rtext, trying ports in extensionPorts order
//	-> {"message_id":0,"protocol_version":1,"account_key":"…","extension_version":"…"}
//	<- {"message_id":1,"valid":true,"protocol_version":1}
//	-> {"message_id":2,"url":"https://…","timestamp":"…"}   on every tab/focus change
//
// The URL is uploaded in the .rtg extended_info column (see buildActivityLog).

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"

	"golang.org/x/net/websocket"
)

// First ports of the extension's _portsAvailable list; it tries them in order.
var extensionPorts = []int{16587, 19968, 16485, 2961, 62743}

// chromeExtensionOrigin is the RescueTime extension in Chrome/Brave/Chromium.
// Any web page can open a WebSocket to localhost, so other origins are rejected.
const chromeExtensionOrigin = "chrome-extension://bdakmnplckeopfghnlpocafcepegjeap"

// browserURL holds the last URL the extension reported (the active tab of the focused window).
var browserURL struct {
	mu  sync.Mutex
	url string
}

func setBrowserURL(u string) {
	browserURL.mu.Lock()
	browserURL.url = u
	browserURL.mu.Unlock()
}

// urlForWindow returns the active tab URL when the focused window is a browser, else "".
func urlForWindow(class string) string {
	switch rescueTimeAppName(class) {
	case "brave", "chrome", "chromium", "firefox":
		browserURL.mu.Lock()
		defer browserURL.mu.Unlock()
		return browserURL.url
	}
	return ""
}

func allowedExtensionOrigin(origin string) bool {
	return origin == chromeExtensionOrigin || strings.HasPrefix(origin, "moz-extension://")
}

// handleExtensionMessage answers one decoded message; it returns the reply (nil for none)
// and whether the connection stays open.
func handleExtensionMessage(msg map[string]any, accountKey string) (map[string]any, bool) {
	id, _ := msg["message_id"].(float64)
	switch id {
	case 0: // initialize: accept only an extension logged in to this account
		ok := msg["account_key"] == accountKey
		if !ok {
			slog.Warn("browser extension rejected: different account_key")
		} else {
			slog.Info("browser extension connected", "version", msg["extension_version"])
		}
		return map[string]any{"message_id": 1, "valid": ok, "protocol_version": 1}, ok
	case 2: // url
		if u, ok := msg["url"].(string); ok {
			setBrowserURL(u)
			slog.Debug("browser url", "url", u)
		}
	}
	return nil, true
}

// startExtensionBridge listens on the first free extension port (loopback only).
func startExtensionBridge(accountKey string) {
	srv := websocket.Server{
		Handshake: func(c *websocket.Config, r *http.Request) error {
			if !allowedExtensionOrigin(r.Header.Get("Origin")) {
				slog.Warn("browser extension bridge: rejected connection", "origin", r.Header.Get("Origin"))
				return fmt.Errorf("origin %q not allowed", r.Header.Get("Origin"))
			}
			slog.Debug("browser extension bridge: connection", "origin", r.Header.Get("Origin"), "query", r.URL.RawQuery)
			return nil
		},
		Handler: func(ws *websocket.Conn) {
			defer ws.Close()
			// Extension 3.2.10 drops its own initialize message after a reconnect (sendMessage compares
			// a missing timestamp, NaN > 200 is false) and then sends url messages without a handshake.
			// The Origin check above already limits this to the RescueTime extension, so accept them.
			for {
				var msg map[string]any
				if err := websocket.JSON.Receive(ws, &msg); err != nil {
					return
				}
				reply, keep := handleExtensionMessage(msg, accountKey)
				if reply != nil {
					websocket.JSON.Send(ws, reply)
				}
				if !keep {
					return
				}
			}
		},
	}
	for _, port := range extensionPorts {
		// "localhost" may resolve to ::1 or 127.0.0.1 in the browser; serve both.
		ln4, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue
		}
		go http.Serve(ln4, srv)
		if ln6, err := net.Listen("tcp", fmt.Sprintf("[::1]:%d", port)); err == nil {
			go http.Serve(ln6, srv)
		}
		slog.Info("browser extension bridge listening", "port", port)
		return
	}
	slog.Warn("browser extension bridge: no free port, URLs will not be tracked")
}
