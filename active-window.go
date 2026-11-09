package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func formatWindowOutput(windowName, windowClass string) string {
	if windowClass != "" {
		return fmt.Sprintf("Active Window: %s (%s)", windowName, windowClass)
	}
	return fmt.Sprintf("Active Window: %s", windowName)
}

// printActivitySummary prints a summary of tracked activities
func printActivitySummary(tracker *ActivityTracker) {
	fmt.Println("\n=== Activity Summary ===")

	summaries := tracker.GetRunSummaries()
	if len(summaries) == 0 {
		fmt.Println("No activities tracked.")
		return
	}

	totalTime := time.Duration(0)
	for _, summary := range summaries {
		totalTime += summary.TotalDuration
	}

	fmt.Printf("Total tracking time: %v\n\n", totalTime.Round(time.Second))

	for appClass, summary := range summaries {
		percentage := float64(summary.TotalDuration) / float64(totalTime) * 100
		fmt.Printf("%s: %v (%.1f%%) - %d sessions\n",
			appClass,
			summary.TotalDuration.Round(time.Second),
			percentage,
			summary.SessionCount)
		fmt.Printf("  └─ %s\n\n", summary.ActivityDetails)
	}
}

// printLiveStats prints a compact top-5 apps by duration
func printLiveStats(tracker *ActivityTracker, startTime time.Time) {
	summaries := tracker.GetRunSummaries()
	if len(summaries) == 0 {
		return
	}

	// Sort by duration (top 5)
	type entry struct {
		app      string
		duration time.Duration
	}
	var sorted []entry
	for app, s := range summaries {
		sorted = append(sorted, entry{app, s.TotalDuration})
	}
	// Simple insertion sort — at most ~20 apps
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j].duration > sorted[j-1].duration; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	if len(sorted) > 5 {
		sorted = sorted[:5]
	}

	elapsed := time.Since(startTime).Round(time.Second)
	fmt.Printf("\n--- Stats (uptime %v) ---\n", elapsed)
	for _, e := range sorted {
		fmt.Printf("  %s: %v\n", e.app, e.duration.Round(time.Second))
	}
	fmt.Println("---")
}

func getCurrentWindowInfo(backend WindowBackend) (string, error) {
	window, err := backend.GetActiveWindow()
	if err != nil {
		return "", err
	}
	return formatWindowOutput(window.Title, window.Class), nil
}

func monitorWindowChanges(backend WindowBackend, cfg Config, submitToAPI bool, apiKey string, showStats bool, statsInterval time.Duration) {
	var lastAppClass, lastWindowTitle, lastURL string

	// Create activity tracker and title cleaner
	tracker := NewActivityTracker(cfg.MergeThreshold.Duration, cfg.MinDuration.Duration)
	cleaner := NewTitleCleaner()

	// Set up persistence
	store, err := NewPersistenceStore(cfg.PersistencePath)
	if err != nil {
		slog.Warn("persistence disabled", "error", err)
	} else {
		// Load saved sessions from previous run
		saved, err := store.LoadSessions()
		if err != nil {
			slog.Warn("failed to load saved sessions", "error", err)
		} else if len(saved) > 0 {
			tracker.LoadSessions(saved)
		}
	}

	// Set up signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Get initial window info and start the first session
	// Retry: at login, autostart can run before GNOME Shell loads the focused-window extension.
	window, err := backend.GetActiveWindow()
	for i := 0; err != nil && i < 60; i++ {
		select {
		case <-sigChan:
			return
		case <-time.After(2 * time.Second):
		}
		window, err = backend.GetActiveWindow()
	}
	if err != nil {
		slog.Error("failed to get initial window info", "error", err)
		return
	}

	// Start the initial session with cleaned title
	cleanedTitle := cleaner.Clean(window.Class, window.Title)
	lastURL = urlForWindow(window.Class)
	tracker.StartSession(window.Class, cleanedTitle, lastURL)
	lastAppClass = window.Class
	lastWindowTitle = window.Title

	// Print initial window (user-facing)
	currentInfo := formatWindowOutput(window.Title, window.Class)
	fmt.Printf("%s [%s]\n", currentInfo, time.Now().Format("15:04:05"))

	pollTicker := time.NewTicker(cfg.PollingInterval.Duration)
	defer pollTicker.Stop()

	var submitTicker *time.Ticker
	var submitChan <-chan time.Time

	if submitToAPI {
		submitTicker = time.NewTicker(cfg.SubmissionInterval.Duration)
		defer submitTicker.Stop()
		submitChan = submitTicker.C
		slog.Info("API submission enabled", "interval", cfg.SubmissionInterval.Duration)
	}

	var statsTicker *time.Ticker
	var statsChan <-chan time.Time
	startTime := time.Now()

	if showStats {
		statsTicker = time.NewTicker(statsInterval)
		defer statsTicker.Stop()
		statsChan = statsTicker.C
		slog.Info("live stats enabled", "interval", statsInterval)
	}

	// Native account key enables /collect uploads (free accounts); otherwise use the legacy API.
	accountKey := os.Getenv("RESCUE_TIME_ACCOUNT_KEY")
	if submitToAPI && accountKey != "" {
		startExtensionBridge(accountKey)
	}
	persistRemaining := func() {
		if store == nil {
			return
		}
		if remaining := tracker.GetSessions(); len(remaining) > 0 {
			if err := store.SaveSessions(remaining); err != nil {
				slog.Warn("failed to save remaining sessions", "error", err)
			}
		} else {
			store.Clear()
		}
	}

	for {
		select {
		case <-sigChan:
			slog.Info("shutting down window monitor")

			// End the current session
			tracker.EndCurrentSession()

			if submitToAPI && accountKey != "" {
				collectSubmit(tracker, accountKey)
				persistRemaining() // keeps sessions whose upload failed for the next run
			} else if submitToAPI {
				// Submit final data; keep it on disk for the next run if any upload failed
				if res := submitActivitiesToRescueTime(apiKey, tracker.GetActivitySummaries()); res.Failed == 0 {
					tracker.ClearCompletedSessions()
				}
				persistRemaining()
			} else if store != nil {
				// Save sessions for next run
				if err := store.SaveSessions(tracker.GetSessions()); err != nil {
					slog.Error("failed to save sessions on shutdown", "error", err)
				}
			}

			// Print summary before exit (user-facing)
			printActivitySummary(tracker)
			return

		case <-submitChan:
			slog.Debug("submission tick triggered")
			if accountKey != "" {
				collectSubmit(tracker, accountKey)
				persistRemaining()
				continue
			}
			// Clear completed sessions only when everything was accepted, so failures are retried
			if res := submitActivitiesToRescueTime(apiKey, tracker.GetActivitySummaries()); res.Failed == 0 {
				tracker.ClearCompletedSessions()
			}

			// Save any remaining sessions to disk
			if store != nil {
				remaining := tracker.GetSessions()
				if len(remaining) > 0 {
					if err := store.SaveSessions(remaining); err != nil {
						slog.Warn("failed to save remaining sessions", "error", err)
					}
				} else {
					store.Clear()
				}
			}

		case <-statsChan:
			printLiveStats(tracker, startTime)

		case <-pollTicker.C:
			if tray.isPaused() {
				if lastAppClass != "" || lastWindowTitle != "" {
					tracker.EndCurrentSession()
					slog.Info("tracking paused from tray")
					tray.setWindow("Paused")
					lastAppClass, lastWindowTitle, lastURL = "", "", ""
				}
				continue
			}

			if idle, err := GetIdleTime(); err == nil && idle >= cfg.IdleThreshold.Duration {
				if lastAppClass != "" || lastWindowTitle != "" {
					// End session at the moment input stopped, not now.
					tracker.EndCurrentSessionAt(time.Now().Add(-idle))
					slog.Info("user idle, session ended", "idle", idle.Round(time.Second))
					fmt.Printf("Idle (no input for %s) [%s]\n", idle.Round(time.Second), time.Now().Format("15:04:05"))
					tray.setWindow("Idle")
					lastAppClass, lastWindowTitle, lastURL = "", "", ""
				}
				continue
			}

			window, err := backend.GetActiveWindow()
			if err != nil {
				slog.Debug("failed to get active window", "error", err)
				continue
			}

			// Check if the application or window title changed
			// A new tab URL from the extension starts a new session even if the title is unchanged
			url := urlForWindow(window.Class)
			if window.Class != lastAppClass || window.Title != lastWindowTitle || url != lastURL {
				slog.Debug("window changed",
					"from_app", lastAppClass, "to_app", window.Class,
					"title", window.Title)

				// Start a new session with cleaned title
				cleanedTitle := cleaner.Clean(window.Class, window.Title)
				tracker.StartSession(window.Class, cleanedTitle, url)

				// Print the change (user-facing)
				currentInfo := formatWindowOutput(window.Title, window.Class)
				fmt.Printf("%s [%s]\n", currentInfo, time.Now().Format("15:04:05"))
				tray.setWindow(window.Class + " — " + cleanedTitle)

				// Update tracking variables
				lastAppClass = window.Class
				lastWindowTitle = window.Title
				lastURL = url
			}
		}
	}
}

func main() {
	// Command line flags
	monitor := flag.Bool("monitor", false, "Continuously monitor for window changes")
	track := flag.Bool("track", false, "Monitor and track time spent in applications")
	submit := flag.Bool("submit", false, "Submit activity data to RescueTime API")
	trayFlag := flag.Bool("tray", false, "Show a status-bar app indicator (pause/resume, last upload, quit)")
	configPath := flag.String("config", "", "Config file path (default: ~/.config/rescuetime-linux/config.json)")

	// Stats flags
	stats := flag.Bool("stats", false, "Show periodic activity statistics")
	statsInterval := flag.Duration("stats-interval", 60*time.Second, "Interval for live stats display (e.g., 10s, 60s)")

	// These flags override config file values
	interval := flag.String("interval", "", "Polling interval (e.g., 100ms, 1s)")
	submissionInterval := flag.String("submission-interval", "", "Submission interval (e.g., 15m, 1h)")
	logLevel := flag.String("log-level", "", "Log level: debug, info, warn, error")
	logFile := flag.String("log-file", "", "Log file path")
	flag.Parse()

	// Load configuration: defaults < config file < CLI flags
	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}

	// Apply CLI flag overrides (only flags that were explicitly set)
	cliFlags := make(map[string]string)
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "interval":
			cliFlags["interval"] = *interval
		case "submission-interval":
			cliFlags["submission-interval"] = *submissionInterval
		case "log-level":
			cliFlags["log-level"] = *logLevel
		case "log-file":
			cliFlags["log-file"] = *logFile
		}
	})
	if err := cfg.ApplyCLIFlags(cliFlags); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
	appNameOverrides = cfg.AppNames

	// Set up structured logging (using final resolved config)
	level := parseLogLevel(cfg.LogLevel)
	cleanupLogger := setupLogger(level, cfg.LogFile)
	defer cleanupLogger()

	slog.Debug("configuration loaded",
		"polling_interval", cfg.PollingInterval.Duration,
		"submission_interval", cfg.SubmissionInterval.Duration,
		"merge_threshold", cfg.MergeThreshold.Duration,
		"min_duration", cfg.MinDuration.Duration,
		"log_level", cfg.LogLevel)

	// Check if we're running in a graphical environment (Wayland or X11)
	if os.Getenv("WAYLAND_DISPLAY") == "" && os.Getenv("DISPLAY") == "" {
		slog.Error("no graphical display found",
			"hint", "Make sure you're running this in a Wayland or X11 environment")
		os.Exit(1)
	}

	// Auto-detect window backend (hyprctl or xdotool)
	backend, err := DetectBackend()
	if err != nil {
		slog.Error("no window backend available", "error", err)
		os.Exit(1)
	}
	slog.Info("detected window backend", "backend", backend.Name())

	if *monitor || *track {
		if *track {
			fmt.Printf("Tracking application usage (polling every %v). Press Ctrl+C to stop and see summary.\n",
				cfg.PollingInterval.Duration)
		} else {
			fmt.Printf("Monitoring window changes (polling every %v). Press Ctrl+C to stop.\n",
				cfg.PollingInterval.Duration)
		}

		// Handle API submission setup
		var apiKey string
		if *submit {
			// Load environment variables from .env file
			err := loadEnvFile(cfg.EnvFilePath)
			if err != nil {
				slog.Error("failed to load .env file", "path", cfg.EnvFilePath, "error", err)
				os.Exit(1)
			}

			// Get API key from environment
			apiKey = os.Getenv("RESCUE_TIME_API_KEY")
			if apiKey == "" && os.Getenv("RESCUE_TIME_ACCOUNT_KEY") == "" {
				slog.Error("neither RESCUE_TIME_ACCOUNT_KEY nor RESCUE_TIME_API_KEY found in environment")
				os.Exit(1)
			}
		}

		run := func() { monitorWindowChanges(backend, cfg, *submit, apiKey, *stats, *statsInterval) }
		if *trayFlag {
			runWithTray(run)
		} else {
			run()
		}
	} else {
		// Single execution mode
		currentInfo, err := getCurrentWindowInfo(backend)
		if err != nil {
			slog.Error("failed to get window info", "error", err)
			os.Exit(1)
		}
		fmt.Println(currentInfo)
	}
}
