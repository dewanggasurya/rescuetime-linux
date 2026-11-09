package main

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
)

// maxLogSize caps the log file; at the cap it moves to <path>.1 (one backup), so disk use stays under ~10 MB.
const maxLogSize = 5 << 20

// rotatingFile is an append-only log file that rotates itself when it reaches max bytes.
type rotatingFile struct {
	mu   sync.Mutex
	path string
	max  int64
	f    *os.File
	size int64
}

func openRotatingFile(path string, max int64) (*rotatingFile, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &rotatingFile{path: path, max: max, f: f, size: st.Size()}, nil
}

func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size > 0 && r.size+int64(len(p)) > r.max {
		r.f.Close()
		os.Rename(r.path, r.path+".1")
		f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			return 0, err
		}
		r.f, r.size = f, 0
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *rotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close()
}

// parseLogLevel converts a string level name to slog.Level
func parseLogLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// setupLogger creates a multi-writer slog handler (stdout + optional file).
// Returns a cleanup function that should be deferred by the caller.
func setupLogger(level slog.Level, logFilePath string) func() {
	var writers []io.Writer
	writers = append(writers, os.Stdout)

	var logFile *rotatingFile
	cleanup := func() {}

	if logFilePath != "" {
		f, err := openRotatingFile(logFilePath, maxLogSize)
		if err != nil {
			// Fall back to stdout-only logging
			slog.Warn("failed to open log file, using stdout only", "path", logFilePath, "error", err)
		} else {
			logFile = f
			writers = append(writers, logFile)
			cleanup = func() {
				logFile.Close()
			}
		}
	}

	writer := io.MultiWriter(writers...)
	handler := slog.NewTextHandler(writer, &slog.HandlerOptions{
		Level: level,
	})
	slog.SetDefault(slog.New(handler))

	return cleanup
}
