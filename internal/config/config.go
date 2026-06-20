// Package config loads the bms-monitor service configuration from a YAML file,
// applies hardcoded defaults for any unset field, and lets CLI flags override
// both. Precedence is CLI flag > YAML value > hardcoded default, matching the
// Python reference (src/config.py:260-264) and FR-1 of docs/plans/slice-2.md.
package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"

	"gopkg.in/yaml.v3"
)

// DefaultConfigPath is the path the service reads when --config is not passed.
// A missing file at this path is tolerated (defaults apply); a missing file at
// any other, explicitly-requested path is a fatal error (FR-1).
const DefaultConfigPath = "/etc/bms-monitor/config.yaml"

// Config is the full service configuration tree (FR-1, requirements.md:121-140).
type Config struct {
	Serial  SerialConfig  `yaml:"serial"`
	Poll    PollConfig    `yaml:"poll"`
	Storage StorageConfig `yaml:"storage"`
	Web     WebConfig     `yaml:"web"` // loaded + validated, unused until Slice 3
	Logging LoggingConfig `yaml:"logging"`
}

// SerialConfig configures the RS485 link to the BMS.
type SerialConfig struct {
	Port          string `yaml:"port"`            // default /dev/ttyUSB0
	Baud          int    `yaml:"baud"`            // default 9600
	Address       int    `yaml:"address"`         // default 0
	ReadTimeoutMS int    `yaml:"read_timeout_ms"` // default 2000
}

// PollConfig configures the poll loop cadence.
type PollConfig struct {
	IntervalSeconds int `yaml:"interval_seconds"` // default 15
}

// StorageConfig configures the SQLite store and its batched-write knobs.
type StorageConfig struct {
	Path                 string `yaml:"path"`                   // default /var/lib/bms-monitor/samples.db
	RetentionDays        int    `yaml:"retention_days"`         // default 30; 0 = forever
	FlushIntervalSeconds int    `yaml:"flush_interval_seconds"` // default 10
	MaxBatch             int    `yaml:"max_batch"`              // default 50
}

// WebConfig configures the HTTP listener (unused until Slice 3).
type WebConfig struct {
	Listen string `yaml:"listen"` // default ":8080"
}

// LoggingConfig configures the slog level.
type LoggingConfig struct {
	Level string `yaml:"level"` // default "info"
}

// Overrides carries CLI-flag values that take precedence over the YAML file and
// the defaults. A zero value means "flag not set, leave the config field as-is"
// (FR-1: "apply non-zero flag overrides").
type Overrides struct {
	Port            string
	Address         int
	ReadTimeoutMS   int
	IntervalSeconds int
	StoragePath     string
	RetentionDays   int
	LogLevel        string
	Listen          string
}

// defaults returns a Config pre-filled with the hardcoded defaults from FR-1.
// Decoding YAML on top of this struct only overwrites the keys present in the
// document, so any field the file omits keeps its default — this is what gives
// the YAML > default half of the precedence rule, including legitimately-zero
// YAML values such as retention_days: 0.
func defaults() *Config {
	return &Config{
		Serial: SerialConfig{
			Port:          "/dev/ttyUSB0",
			Baud:          9600,
			Address:       0,
			ReadTimeoutMS: 2000,
		},
		Poll: PollConfig{
			IntervalSeconds: 15,
		},
		Storage: StorageConfig{
			Path:                 "/var/lib/bms-monitor/samples.db",
			RetentionDays:        30,
			FlushIntervalSeconds: 10,
			MaxBatch:             50,
		},
		Web: WebConfig{
			Listen: ":8080",
		},
		Logging: LoggingConfig{
			Level: "info",
		},
	}
}

// Load reads the YAML config at path (if present), fills defaults, and applies
// the non-zero flag overrides. A missing file is tolerated only when path is
// DefaultConfigPath; an explicitly-passed path that does not exist is fatal.
func Load(path string, ov Overrides) (*Config, error) {
	cfg := defaults()

	f, err := os.Open(path)
	switch {
	case err == nil:
		defer f.Close()
		dec := yaml.NewDecoder(f)
		// Decoding into the defaults-filled struct overwrites only the keys the
		// document actually contains. An empty file yields io.EOF, which we treat
		// as "no overrides" rather than an error.
		if derr := dec.Decode(cfg); derr != nil && !errors.Is(derr, io.EOF) {
			return nil, fmt.Errorf("config: parse %s: %w", path, derr)
		}
	case errors.Is(err, fs.ErrNotExist) && path == DefaultConfigPath:
		// Default path absent: run on defaults + flags.
	default:
		return nil, fmt.Errorf("config: open %s: %w", path, err)
	}

	applyOverrides(cfg, ov)
	return cfg, nil
}

// applyOverrides copies each non-zero override field onto the config.
func applyOverrides(cfg *Config, ov Overrides) {
	if ov.Port != "" {
		cfg.Serial.Port = ov.Port
	}
	if ov.Address != 0 {
		cfg.Serial.Address = ov.Address
	}
	if ov.ReadTimeoutMS != 0 {
		cfg.Serial.ReadTimeoutMS = ov.ReadTimeoutMS
	}
	if ov.IntervalSeconds != 0 {
		cfg.Poll.IntervalSeconds = ov.IntervalSeconds
	}
	if ov.StoragePath != "" {
		cfg.Storage.Path = ov.StoragePath
	}
	if ov.RetentionDays != 0 {
		cfg.Storage.RetentionDays = ov.RetentionDays
	}
	if ov.LogLevel != "" {
		cfg.Logging.Level = ov.LogLevel
	}
	if ov.Listen != "" {
		cfg.Web.Listen = ov.Listen
	}
}

// validLogLevels is the set of accepted logging.level values.
var validLogLevels = map[string]bool{
	"debug": true,
	"info":  true,
	"warn":  true,
	"error": true,
}

// Validate checks that the resolved config is internally consistent, returning
// a wrapped error naming the offending field (FR-1). serve exits non-zero on a
// validation failure.
func (c *Config) Validate() error {
	if c.Serial.Port == "" {
		return fmt.Errorf("config: serial.port must not be empty")
	}
	if c.Poll.IntervalSeconds <= 0 {
		return fmt.Errorf("config: poll.interval_seconds must be > 0, got %d", c.Poll.IntervalSeconds)
	}
	if c.Storage.RetentionDays < 0 {
		return fmt.Errorf("config: storage.retention_days must be >= 0, got %d", c.Storage.RetentionDays)
	}
	if c.Storage.FlushIntervalSeconds <= 0 {
		return fmt.Errorf("config: storage.flush_interval_seconds must be > 0, got %d", c.Storage.FlushIntervalSeconds)
	}
	if c.Storage.MaxBatch <= 0 {
		return fmt.Errorf("config: storage.max_batch must be > 0, got %d", c.Storage.MaxBatch)
	}
	if _, _, err := net.SplitHostPort(c.Web.Listen); err != nil {
		return fmt.Errorf("config: web.listen %q is not a valid host:port: %w", c.Web.Listen, err)
	}
	if !validLogLevels[c.Logging.Level] {
		return fmt.Errorf("config: logging.level %q must be one of debug, info, warn, error", c.Logging.Level)
	}
	return nil
}
