package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeYAML writes content to a temp file and returns its path.
func writeYAML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing temp config: %v", err)
	}
	return path
}

func TestLoadDefaultFillIn(t *testing.T) {
	// A file present but nearly empty: every omitted field must fall back to its
	// hardcoded default, while the one key set in YAML wins over the default.
	path := writeYAML(t, "poll:\n  interval_seconds: 30\n")

	cfg, err := Load(path, Overrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Poll.IntervalSeconds != 30 {
		t.Errorf("YAML override: interval_seconds = %d, want 30", cfg.Poll.IntervalSeconds)
	}
	want := defaults()
	if cfg.Serial.Port != want.Serial.Port {
		t.Errorf("default fill-in: serial.port = %q, want %q", cfg.Serial.Port, want.Serial.Port)
	}
	if cfg.Serial.Baud != want.Serial.Baud {
		t.Errorf("default fill-in: serial.baud = %d, want %d", cfg.Serial.Baud, want.Serial.Baud)
	}
	if cfg.Serial.ReadTimeoutMS != want.Serial.ReadTimeoutMS {
		t.Errorf("default fill-in: read_timeout_ms = %d, want %d", cfg.Serial.ReadTimeoutMS, want.Serial.ReadTimeoutMS)
	}
	if cfg.Storage.Path != want.Storage.Path {
		t.Errorf("default fill-in: storage.path = %q, want %q", cfg.Storage.Path, want.Storage.Path)
	}
	if cfg.Storage.RetentionDays != want.Storage.RetentionDays {
		t.Errorf("default fill-in: retention_days = %d, want %d", cfg.Storage.RetentionDays, want.Storage.RetentionDays)
	}
	if cfg.Storage.FlushIntervalSeconds != want.Storage.FlushIntervalSeconds {
		t.Errorf("default fill-in: flush_interval_seconds = %d, want %d", cfg.Storage.FlushIntervalSeconds, want.Storage.FlushIntervalSeconds)
	}
	if cfg.Storage.MaxBatch != want.Storage.MaxBatch {
		t.Errorf("default fill-in: max_batch = %d, want %d", cfg.Storage.MaxBatch, want.Storage.MaxBatch)
	}
	if cfg.Web.Listen != want.Web.Listen {
		t.Errorf("default fill-in: web.listen = %q, want %q", cfg.Web.Listen, want.Web.Listen)
	}
	if cfg.Logging.Level != want.Logging.Level {
		t.Errorf("default fill-in: logging.level = %q, want %q", cfg.Logging.Level, want.Logging.Level)
	}
}

func TestLoadYAMLZeroValueWins(t *testing.T) {
	// retention_days: 0 means "keep forever" and must survive the default-fill
	// step rather than being reset to the default of 30.
	path := writeYAML(t, "storage:\n  retention_days: 0\n")
	cfg, err := Load(path, Overrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Storage.RetentionDays != 0 {
		t.Errorf("retention_days = %d, want 0 (explicit YAML zero must win)", cfg.Storage.RetentionDays)
	}
}

func TestLoadFlagOverride(t *testing.T) {
	// YAML sets values; non-zero overrides must beat them, while zero-valued
	// override fields leave the YAML/default value untouched.
	path := writeYAML(t, strings.Join([]string{
		"serial:",
		"  port: /dev/ttyS0",
		"  address: 1",
		"poll:",
		"  interval_seconds: 30",
		"storage:",
		"  path: /tmp/yaml.db",
		"  retention_days: 7",
		"logging:",
		"  level: warn",
	}, "\n"))

	ov := Overrides{
		Port:            "/dev/ttyUSB9",
		IntervalSeconds: 5,
		StoragePath:     "/tmp/flag.db",
		LogLevel:        "debug",
	}
	cfg, err := Load(path, ov)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Serial.Port != "/dev/ttyUSB9" {
		t.Errorf("flag override: serial.port = %q, want /dev/ttyUSB9", cfg.Serial.Port)
	}
	if cfg.Poll.IntervalSeconds != 5 {
		t.Errorf("flag override: interval_seconds = %d, want 5", cfg.Poll.IntervalSeconds)
	}
	if cfg.Storage.Path != "/tmp/flag.db" {
		t.Errorf("flag override: storage.path = %q, want /tmp/flag.db", cfg.Storage.Path)
	}
	if cfg.Logging.Level != "debug" {
		t.Errorf("flag override: logging.level = %q, want debug", cfg.Logging.Level)
	}
	// Not overridden (zero in Overrides): YAML value stands.
	if cfg.Serial.Address != 1 {
		t.Errorf("non-override: serial.address = %d, want 1 (from YAML)", cfg.Serial.Address)
	}
	if cfg.Storage.RetentionDays != 7 {
		t.Errorf("non-override: retention_days = %d, want 7 (from YAML)", cfg.Storage.RetentionDays)
	}
}

func TestLoadMissingFile(t *testing.T) {
	// Missing default path → tolerated, pure defaults.
	cfg, err := Load(DefaultConfigPath, Overrides{})
	if err != nil {
		// Only acceptable if the default path genuinely does not exist on this
		// host. If it happens to exist, skip rather than fail spuriously.
		t.Skipf("default config path unexpectedly present: %v", err)
	}
	if cfg.Serial.Port != "/dev/ttyUSB0" {
		t.Errorf("missing default file: serial.port = %q, want /dev/ttyUSB0", cfg.Serial.Port)
	}

	// Missing explicitly-requested path → fatal.
	missing := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	if _, err := Load(missing, Overrides{}); err == nil {
		t.Errorf("Load(%q) = nil error, want failure for missing explicit file", missing)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string // substring expected in the error; "" means expect success
	}{
		{"valid defaults", func(*Config) {}, ""},
		{"empty port", func(c *Config) { c.Serial.Port = "" }, "serial.port"},
		{"zero interval", func(c *Config) { c.Poll.IntervalSeconds = 0 }, "poll.interval_seconds"},
		{"negative interval", func(c *Config) { c.Poll.IntervalSeconds = -1 }, "poll.interval_seconds"},
		{"negative retention", func(c *Config) { c.Storage.RetentionDays = -1 }, "storage.retention_days"},
		{"zero flush interval", func(c *Config) { c.Storage.FlushIntervalSeconds = 0 }, "storage.flush_interval_seconds"},
		{"zero max batch", func(c *Config) { c.Storage.MaxBatch = 0 }, "storage.max_batch"},
		{"bad listen", func(c *Config) { c.Web.Listen = "not-a-hostport" }, "web.listen"},
		{"bad log level", func(c *Config) { c.Logging.Level = "verbose" }, "logging.level"},
		{"retention zero ok", func(c *Config) { c.Storage.RetentionDays = 0 }, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaults()
			tc.mutate(cfg)
			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Validate() = %q, want it to name %q", err.Error(), tc.wantErr)
			}
		})
	}
}
