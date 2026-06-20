// Command bms-monitor is the CLI entry point for the HinaEss Powergem Max
// BMS monitor. Slice 1 implements a single subcommand, "poll", which performs
// one analog read over RS485 and prints the result. Later slices add "serve"
// and "migrate".
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"bitbucket.org/andrewburnsza/hinaess-battery-charge-monitor/internal/bms"
)

// pollConfig holds the parsed flags for the "poll" subcommand.
type pollConfig struct {
	port          string
	baud          int
	address       int
	readTimeoutMS int
	format        string
	debug         bool
}

// readTimeout returns the configured read timeout as a time.Duration.
func (c *pollConfig) readTimeout() time.Duration {
	return time.Duration(c.readTimeoutMS) * time.Millisecond
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "poll":
		os.Exit(runPoll(os.Args[2:]))
	case "serve":
		os.Exit(runServe(os.Args[2:]))
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

// usage prints top-level usage for bms-monitor.
func usage() {
	fmt.Fprintf(os.Stderr, "bms-monitor — HinaEss Powergem Max BMS monitor\n\n")
	fmt.Fprintf(os.Stderr, "Usage:\n")
	fmt.Fprintf(os.Stderr, "  bms-monitor <subcommand> [flags]\n\n")
	fmt.Fprintf(os.Stderr, "Subcommands:\n")
	fmt.Fprintf(os.Stderr, "  poll    Perform one analog read and print the result\n")
	fmt.Fprintf(os.Stderr, "  serve   Poll on an interval and persist samples to SQLite\n")
}

// newPollFlags builds the flag.FlagSet for the "poll" subcommand and returns
// it alongside the pollConfig its flags write into.
func newPollFlags() (*flag.FlagSet, *pollConfig) {
	cfg := &pollConfig{}
	fs := flag.NewFlagSet("poll", flag.ContinueOnError)
	fs.StringVar(&cfg.port, "port", "/dev/ttyUSB0", "serial port device path")
	fs.IntVar(&cfg.baud, "baud", 9600, "serial baud rate")
	fs.IntVar(&cfg.address, "address", 0, "BMS pack address")
	fs.IntVar(&cfg.readTimeoutMS, "read-timeout-ms", 2000, "serial read timeout in milliseconds")
	fs.StringVar(&cfg.format, "format", "text", "output format: text or json")
	fs.BoolVar(&cfg.debug, "debug", false, "print raw TX/RX hex dumps to stderr")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: bms-monitor poll [flags]\n\n")
		fmt.Fprintf(fs.Output(), "Flags:\n")
		fs.PrintDefaults()
	}
	return fs, cfg
}

// Exit codes. 0 = success, 2 = usage error (set by main/flag parsing). The rest
// categorize a failed poll so callers and scripts can distinguish failure modes
// (requirements.md:173).
const (
	exitOK          = 0
	exitGeneric     = 1
	exitTimeout     = 3
	exitFraming     = 4
	exitBMS         = 5
	exitImplausible = 6
)

// runPoll parses the poll flags, performs one analog read, and prints the
// decoded sample as text or JSON. It returns the process exit code.
func runPoll(args []string) int {
	fs, cfg := newPollFlags()
	if err := fs.Parse(args); err != nil {
		// flag.ContinueOnError prints the error/usage; -h/-help yields ErrHelp.
		if err == flag.ErrHelp {
			return exitOK
		}
		return 2
	}

	if cfg.format != "text" && cfg.format != "json" {
		fmt.Fprintf(os.Stderr, "poll: invalid --format %q (want text or json)\n", cfg.format)
		return 2
	}

	// Route slog to stderr; --debug lowers the level so the client/parser Debug
	// dumps appear, while warnings (checksum mismatch, implausible values) always
	// show (requirements.md:373).
	level := slog.LevelInfo
	if cfg.debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	client, err := bms.Open(cfg.port, cfg.baud, cfg.address, cfg.readTimeout())
	if err != nil {
		fmt.Fprintf(os.Stderr, "poll: open %s: %v\n", cfg.port, err)
		return exitGeneric
	}
	defer client.Close()

	sample, tx, rx, err := client.Poll()
	if cfg.debug {
		dumpDebug(tx, rx)
	}
	if err != nil {
		return reportError(err)
	}

	switch cfg.format {
	case "json":
		if err := printJSON(os.Stdout, sample); err != nil {
			fmt.Fprintf(os.Stderr, "poll: encode JSON: %v\n", err)
			return exitGeneric
		}
	default:
		printText(os.Stdout, cfg.address, sample)
	}
	return exitOK
}

// dumpDebug prints the raw request/response bytes as uppercase hex to stderr
// (requirements.md:174). rx may be nil/partial on a failed read.
func dumpDebug(tx, rx []byte) {
	fmt.Fprintf(os.Stderr, "TX: %X\n", tx)
	fmt.Fprintf(os.Stderr, "RX: %X\n", rx)
}

// reportError writes a categorized message to stderr and returns the matching
// non-zero exit code (requirements.md:173).
func reportError(err error) int {
	switch {
	case errors.Is(err, bms.ErrTimeout):
		fmt.Fprintf(os.Stderr, "poll: timeout — no response from BMS: %v\n", err)
		return exitTimeout
	case errors.Is(err, bms.ErrFraming):
		fmt.Fprintf(os.Stderr, "poll: framing error — malformed response: %v\n", err)
		return exitFraming
	case errors.Is(err, bms.ErrBMS):
		fmt.Fprintf(os.Stderr, "poll: BMS returned an error code: %v\n", err)
		return exitBMS
	case errors.Is(err, bms.ErrImplausible):
		fmt.Fprintf(os.Stderr, "poll: implausible reading discarded: %v\n", err)
		return exitImplausible
	default:
		fmt.Fprintf(os.Stderr, "poll: %v\n", err)
		return exitGeneric
	}
}

// printJSON writes the sample as a single-line JSON object suitable for piping
// to jq (requirements.md:182).
func printJSON(w *os.File, s bms.PackSample) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(b))
	return err
}

// printText writes a human-readable block — status, pack V/A/power, SOC/SOH,
// cycles, capacity, per-cell voltages with min/max/delta, and temperatures —
// modeled on the reference's print_battery_data (src/utils.py:44-105).
func printText(w *os.File, addr int, s bms.PackSample) {
	fmt.Fprintf(w, "BMS #%d — Battery Data\n", addr)

	currentA := float64(s.PackMA) / 1000.0
	voltageV := float64(s.PackMV) / 1000.0
	powerW := voltageV * currentA

	status := "IDLE"
	switch {
	case currentA > 0.5:
		status = "CHARGING"
	case currentA < -0.5:
		status = "DISCHARGING"
	}

	fmt.Fprintf(w, "  Status:          %s\n", status)
	fmt.Fprintf(w, "  Pack Voltage:    %.2f V\n", voltageV)
	fmt.Fprintf(w, "  Current:         %+.2f A\n", currentA)
	fmt.Fprintf(w, "  Power:           %.1f W\n", powerW)
	fmt.Fprintf(w, "  SOC:             %.1f %%\n", s.SOCPct)
	fmt.Fprintf(w, "  SOH:             %.1f %%\n", s.SOHPct)
	fmt.Fprintf(w, "  Cycles:          %d\n", s.Cycles)
	fmt.Fprintf(w, "  Remaining:       %.2f Ah\n", float64(s.RemainMAH)/1000.0)
	fmt.Fprintf(w, "  Full Capacity:   %.2f Ah\n", float64(s.FullMAH)/1000.0)

	if len(s.Cells) > 0 {
		minMV, maxMV := s.Cells[0], s.Cells[0]
		minIdx, maxIdx := 1, 1
		for i, mv := range s.Cells {
			if mv < minMV {
				minMV, minIdx = mv, i+1
			}
			if mv > maxMV {
				maxMV, maxIdx = mv, i+1
			}
		}
		fmt.Fprintf(w, "  Cells (%d):  min=%.3fV (#%d)  max=%.3fV (#%d)  delta=%dmV\n",
			len(s.Cells), float64(minMV)/1000.0, minIdx, float64(maxMV)/1000.0, maxIdx, maxMV-minMV)

		row := "    "
		for i, mv := range s.Cells {
			tag := ""
			switch i + 1 {
			case minIdx:
				tag = " min"
			case maxIdx:
				tag = " max"
			}
			row += fmt.Sprintf("C%02d=%.3fV%s  ", i+1, float64(mv)/1000.0, tag)
			if (i+1)%8 == 0 {
				fmt.Fprintln(w, row)
				row = "    "
			}
		}
		if len(row) > 4 {
			fmt.Fprintln(w, row)
		}
	}

	if len(s.Temps) > 0 {
		fmt.Fprintln(w, "  Temperatures:")
		for _, t := range s.Temps {
			fmt.Fprintf(w, "    %4s: %.1fC\n", t.Probe, float64(t.DeciC)/10.0)
		}
	}
}
