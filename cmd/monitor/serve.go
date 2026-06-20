package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"bitbucket.org/andrewburnsza/hinaess-battery-charge-monitor/internal/bms"
	"bitbucket.org/andrewburnsza/hinaess-battery-charge-monitor/internal/config"
	"bitbucket.org/andrewburnsza/hinaess-battery-charge-monitor/internal/store"
	"bitbucket.org/andrewburnsza/hinaess-battery-charge-monitor/internal/web"
)

// serveConfig holds the parsed flags for the "serve" subcommand. Each flag maps
// to a config.Overrides field; a zero value means "flag not set" so the config
// file or hardcoded default wins (CLI > YAML > default, FR-1/FR-2). --config and
// --debug are handled outside Overrides.
type serveConfig struct {
	configPath    string
	port          string
	address       int
	readTimeoutMS int
	interval      int
	dbPath        string
	retentionDays int
	logLevel      string
	listen        string
	debug         bool
}

// newServeFlags builds the flag.FlagSet for the "serve" subcommand and returns
// it alongside the serveConfig its flags write into (FR-2 flag table).
func newServeFlags() (*flag.FlagSet, *serveConfig) {
	cfg := &serveConfig{}
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.StringVar(&cfg.configPath, "config", config.DefaultConfigPath, "path to config.yaml")
	fs.StringVar(&cfg.port, "port", "", "serial port device path (overrides serial.port)")
	fs.IntVar(&cfg.address, "address", 0, "BMS pack address (overrides serial.address)")
	fs.IntVar(&cfg.readTimeoutMS, "read-timeout-ms", 0, "serial read timeout in ms (overrides serial.read_timeout_ms)")
	fs.IntVar(&cfg.interval, "interval-seconds", 0, "poll interval in seconds (overrides poll.interval_seconds)")
	fs.StringVar(&cfg.dbPath, "db", "", "SQLite database path (overrides storage.path)")
	fs.IntVar(&cfg.retentionDays, "retention-days", 0, "retention window in days (overrides storage.retention_days)")
	fs.StringVar(&cfg.logLevel, "log-level", "", "log level: debug, info, warn, error (overrides logging.level)")
	fs.StringVar(&cfg.listen, "listen", "", "HTTP listen address host:port (overrides web.listen)")
	fs.BoolVar(&cfg.debug, "debug", false, "force log level debug")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: bms-monitor serve [flags]\n\n")
		fmt.Fprintf(fs.Output(), "Flags:\n")
		fs.PrintDefaults()
	}
	return fs, cfg
}

// runServe loads config, opens the store and serial client, installs the signal
// handler, runs the poll loop until the context is cancelled, then flushes,
// checkpoint-closes the DB, and closes the serial client (FR-2 wiring sequence).
// It returns the process exit code: 0 on a clean shutdown, non-zero on a startup
// failure. A serial-port open failure at startup is NOT fatal — the loop enters
// the reconnect/backoff path (FR-5, wired in Step 6).
func runServe(args []string) int {
	fs, sc := newServeFlags()
	if err := fs.Parse(args); err != nil {
		// flag.ContinueOnError prints the error/usage; -h/-help yields ErrHelp.
		if err == flag.ErrHelp {
			return exitOK
		}
		return 2
	}

	cfg, err := config.Load(sc.configPath, config.Overrides{
		Port:            sc.port,
		Address:         sc.address,
		ReadTimeoutMS:   sc.readTimeoutMS,
		IntervalSeconds: sc.interval,
		StoragePath:     sc.dbPath,
		RetentionDays:   sc.retentionDays,
		LogLevel:        sc.logLevel,
		Listen:          sc.listen,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		return exitGeneric
	}
	if sc.debug {
		cfg.Logging.Level = "debug"
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		return exitGeneric
	}

	// Configure slog at the resolved level. JSON to stderr so journald captures
	// structured lines (FR-7); the per-poll INFO line is wired in Step 7.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: parseLevel(cfg.Logging.Level),
	})))

	st, err := store.Open(cfg.Storage.Path)
	if err != nil {
		slog.Error("serve: open store", "path", cfg.Storage.Path, "err", err)
		return exitGeneric
	}

	// Start the embedded HTTP server before the poll loop. Start binds the
	// listener synchronously and serves in a background goroutine, so a bind
	// failure (e.g. the port is already in use) is returned here and is fatal
	// (Q2, FR-8): unlike a serial-open failure, an unbindable listen address is
	// an operator error worth exiting non-zero on so systemd surfaces it. Close
	// the already-open store before exiting so the WAL is checkpointed cleanly.
	srv := web.New(st, time.Duration(cfg.Poll.IntervalSeconds)*time.Second, cfg.Web.Listen, slog.Default())
	if err := srv.Start(); err != nil {
		slog.Error("serve: start web server", "listen", cfg.Web.Listen, "err", err)
		if cerr := st.Close(); cerr != nil {
			slog.Error("serve: close store", "err", cerr)
		}
		return exitGeneric
	}
	slog.Info("serve: web listening", "listen", cfg.Web.Listen)

	// Install the signal handler before any long-running work so a SIGINT/SIGTERM
	// during startup retention or the first poll cancels cleanly.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Startup retention (FR-6); the 24 h ticker is wired in Step 6.
	if deleted, err := st.Prune(ctx, cfg.Storage.RetentionDays); err != nil {
		slog.Error("serve: startup prune", "err", err)
	} else if deleted > 0 {
		slog.Info("serve: startup prune", "deleted", deleted)
	}

	// Open the serial client. A failure here is non-fatal: the service must come
	// up even if the adapter is unplugged at boot, and reconnect (Step 6) retries.
	client, err := bms.Open(cfg.Serial.Port, cfg.Serial.Baud, cfg.Serial.Address,
		time.Duration(cfg.Serial.ReadTimeoutMS)*time.Millisecond)
	if err != nil {
		slog.Error("serve: open serial port", "port", cfg.Serial.Port, "err", err)
		client = nil
	}

	slog.Info("serve: started",
		"port", cfg.Serial.Port,
		"interval_seconds", cfg.Poll.IntervalSeconds,
		"db", cfg.Storage.Path)

	// runLoop may reconnect (close+reopen) the serial client; it returns the live
	// handle so the shutdown sequence below closes the right one.
	client = runLoop(ctx, cfg, st, client)

	// Shutdown sequence: stop the HTTP server first so in-flight requests finish
	// reading before the store's read handle closes, then flush the buffer,
	// checkpoint-close the DB, and close the serial client (FR-8 ordering, Goal
	// 1). Use a fresh context so the sequence is not skipped by the already-
	// cancelled serve context; a 5 s timeout bounds the graceful HTTP stop.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("serve: shutdown web server", "err", err)
	} else {
		slog.Info("serve: web server stopped")
	}
	if n, err := st.Flush(context.Background()); err != nil {
		slog.Error("serve: final flush", "err", err)
	} else if n > 0 {
		slog.Info("serve: final flush", "samples", n)
	}
	if err := st.Close(); err != nil {
		slog.Error("serve: close store", "err", err)
	}
	if err := client.Close(); err != nil {
		slog.Error("serve: close serial", "err", err)
	}

	slog.Info("serve: stopped")
	return exitOK
}

// backoffSchedule is the capped escalation backoff applied once a run of
// consecutive poll failures reaches the escalation threshold (FR-5): the wait
// before the next attempt climbs 5 s → 10 s → 30 s → 60 s and then holds at
// 60 s, replacing the normal poll interval until a poll succeeds.
var backoffSchedule = []time.Duration{
	5 * time.Second,
	10 * time.Second,
	30 * time.Second,
	60 * time.Second,
}

// escalateAfter is the consecutive-failure count at which the loop logs ERROR
// (rather than WARN) and switches from the normal interval to backoffSchedule
// (FR-5).
const escalateAfter = 5

// runLoop polls on the configured interval, buffers successful samples, and
// flushes and prunes on their own tickers until the context is cancelled
// (FR-5/FR-6). Three timers feed one select so shutdown via ctx.Done() is
// immediate, never delayed by a poll interval or a backoff sleep:
//
//   - pollTimer fires each poll. Its reset interval is the normal poll interval
//     on success or transient failure, or the capped backoff once a failure run
//     reaches escalateAfter.
//   - flushTicker drains the writer buffer to one transaction per interval.
//   - retentionTicker prunes rows past the retention window once every 24 h.
//
// A transient Poll error (timeout, framing, implausible decode, BMS error code)
// keeps the existing serial handle and just counts toward the failure run; an
// OS-level port error (cable pulled) closes the handle so the next attempt
// reopens it. The serial client may start nil (open failed at boot); attempt
// reopens it before polling. It returns the live serial client (which may differ
// from the one passed in, or be nil if the port is currently down) so the
// caller's shutdown closes the correct handle.
func runLoop(ctx context.Context, cfg *config.Config, st *store.Store, client *bms.Client) *bms.Client {
	interval := time.Duration(cfg.Poll.IntervalSeconds) * time.Second
	flushInterval := time.Duration(cfg.Storage.FlushIntervalSeconds) * time.Second

	flushTicker := time.NewTicker(flushInterval)
	defer flushTicker.Stop()
	retentionTicker := time.NewTicker(24 * time.Hour)
	defer retentionTicker.Stop()

	// Fire the first poll immediately; subsequent waits are computed per attempt.
	pollTimer := time.NewTimer(0)
	defer pollTimer.Stop()

	var consecutive int

	flush := func() {
		n, err := st.Flush(ctx)
		if err != nil {
			slog.Error("serve: flush", "err", err)
		} else if n > 0 {
			slog.Debug("serve: flush", "samples", n)
		}
	}

	// attempt runs one poll, mutating client and consecutive. It opens the serial
	// client first if it is nil (startup or post-unplug), counting a failed reopen
	// as a failure. On success it buffers the sample and resets the failure run; on
	// failure it logs WARN, or ERROR once the run reaches escalateAfter, tagging the
	// error category, and closes the handle on an OS-level port error so the next
	// attempt reopens it (FR-5).
	attempt := func() {
		if client == nil {
			c, err := bms.Open(cfg.Serial.Port, cfg.Serial.Baud, cfg.Serial.Address,
				time.Duration(cfg.Serial.ReadTimeoutMS)*time.Millisecond)
			if err != nil {
				consecutive++
				logFailure("serve: serial port unavailable", "serial", err, consecutive)
				return
			}
			client = c
			slog.Info("serve: serial port reopened", "port", cfg.Serial.Port)
		}

		sample, _, _, err := client.Poll()
		if err == nil {
			st.Add(sample)
			if consecutive > 0 {
				slog.Info("serve: poll recovered", "after_failures", consecutive)
			}
			consecutive = 0
			logPollOK(sample)
			return
		}

		consecutive++
		category := errCategory(err)
		if isPortError(err) {
			// The handle is likely dead (USB unplugged); drop it so the next
			// attempt reopens. A timeout does NOT reach here — it keeps the handle.
			_ = client.Close()
			client = nil
		}
		logFailure("serve: poll failed", category, err, consecutive)
	}

	for {
		select {
		case <-ctx.Done():
			return client
		case <-flushTicker.C:
			flush()
		case <-retentionTicker.C:
			if deleted, err := st.Prune(ctx, cfg.Storage.RetentionDays); err != nil {
				slog.Error("serve: periodic prune", "err", err)
			} else if deleted > 0 {
				slog.Info("serve: periodic prune", "deleted", deleted)
			}
		case <-pollTimer.C:
			attempt()
			// On an escalated failure run, wait the capped backoff before the next
			// attempt instead of the normal interval (FR-5).
			next := interval
			if consecutive >= escalateAfter {
				idx := min(consecutive-escalateAfter, len(backoffSchedule)-1)
				next = backoffSchedule[idx]
			}
			pollTimer.Reset(next)
		}
	}
}

// errCategory classifies a Poll error for structured logging. Anything not one
// of the four BMS sentinels is treated as a serial/port-level error.
func errCategory(err error) string {
	switch {
	case errors.Is(err, bms.ErrTimeout):
		return "timeout"
	case errors.Is(err, bms.ErrFraming):
		return "framing"
	case errors.Is(err, bms.ErrImplausible):
		return "implausible"
	case errors.Is(err, bms.ErrBMS):
		return "bms"
	default:
		return "serial"
	}
}

// isPortError reports whether a Poll error indicates the serial handle is gone
// (USB unplugged, port closed) and must be reopened, rather than a transient
// protocol failure that keeps the handle. The four BMS sentinels — timeout,
// framing, implausible, and device error code — are transient; everything else
// (an os.PathError, "file already closed", a failed write/reset) is a port error
// (FR-5).
func isPortError(err error) bool {
	return !errors.Is(err, bms.ErrTimeout) &&
		!errors.Is(err, bms.ErrFraming) &&
		!errors.Is(err, bms.ErrImplausible) &&
		!errors.Is(err, bms.ErrBMS)
}

// logFailure logs one poll failure at WARN, or ERROR once the consecutive run
// reaches escalateAfter, with the error category and run length (FR-5).
func logFailure(msg, category string, err error, consecutive int) {
	level := slog.LevelWarn
	if consecutive >= escalateAfter {
		level = slog.LevelError
	}
	slog.Log(context.Background(), level, msg,
		"category", category, "consecutive", consecutive, "err", err)
}

// logPollOK emits the per-poll success line at INFO with the headline balancing
// fields (FR-7): pack voltage and current, SOC, and the cell min/max/delta in mV
// computed from sample.Cells. cells_delta_mv = max(Cells) - min(Cells) is the
// headline balancing metric the whole tool exists to surface
// (requirements.md:269, requirements.md:332). The cell fields are omitted when a
// sample carries no cell voltages.
func logPollOK(s bms.PackSample) {
	attrs := []any{
		"pack_mv", s.PackMV,
		"pack_ma", s.PackMA,
		"soc_pct", s.SOCPct,
	}
	if len(s.Cells) > 0 {
		minMV, maxMV := s.Cells[0], s.Cells[0]
		for _, mv := range s.Cells {
			if mv < minMV {
				minMV = mv
			}
			if mv > maxMV {
				maxMV = mv
			}
		}
		attrs = append(attrs,
			"cells_min_mv", minMV,
			"cells_max_mv", maxMV,
			"cells_delta_mv", maxMV-minMV)
	}
	slog.Info("serve: poll ok", attrs...)
}

// parseLevel maps a validated logging.level string to a slog.Level. The config
// validator guarantees one of debug/info/warn/error, so the default is info.
func parseLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
