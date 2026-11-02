package main

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config holds every runtime tunable of eventlink-trace-exporter.
type Config struct {
	ListenAddr     string
	TracelogDir    string
	BaseLabel      string        // value of the base label, same as directory-exporter's target label
	PollInterval   time.Duration // how often new trace lines are read
	Backfill       bool          // read existing trace files from the start at startup
	MaxReadPerPoll int64         // trace bytes read per poll; the rest waits for the next poll (0 = unlimited)
	NameTTL        time.Duration // how long a collected file name is kept for matching
	MaxNames       int           // cap on collected file names kept
	SeedDays       int           // days of trace logs read at startup for the last collected / distributed times (0 = off)
	SeedMaxBytes   int64         // bytes read per stream by that startup seed (0 = unlimited)
	LogFile        string        // "" = stdout
	LogMaxBytes    int64         // rotate LogFile above this size (0 = never)
	LogKeep        int           // rotated files kept (trace-exporter.log.1 … .N)
}

type fileConfig struct {
	ListenAddr   string `yaml:"listen_addr"`
	TracelogDir  string `yaml:"tracelog_dir"`
	BaseLabel    string `yaml:"base_label"`
	PollInterval string `yaml:"poll_interval"`
	Backfill     *bool  `yaml:"backfill"`
	MaxReadMB    *int   `yaml:"max_read_mb"`
	NameTTL      string `yaml:"name_ttl"`
	MaxNames     *int   `yaml:"max_names"`
	SeedDays     *int   `yaml:"seed_days"`
	SeedMaxMB    *int   `yaml:"seed_max_mb"`
	LogFile      string `yaml:"log_file"`
	LogMaxSizeMB *int   `yaml:"log_max_size_mb"`
	LogKeep      *int   `yaml:"log_keep"`
}

// LoadConfig reads defaults, then the YAML file named by CONFIG_FILE (if
// set), then the LISTEN_ADDR and TRACELOG_DIR environment variables.
func LoadConfig() (*Config, error) {
	cfg := &Config{
		ListenAddr:     ":9202",
		BaseLabel:      "/streams",
		PollInterval:   30 * time.Second,
		MaxReadPerPoll: 1024 << 20,
		NameTTL:        24 * time.Hour,
		MaxNames:       500000,
		SeedDays:       7,
		SeedMaxBytes:   1024 << 20,
		LogMaxBytes:    10 << 20,
		LogKeep:        3,
	}
	if path := os.Getenv("CONFIG_FILE"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		var fc fileConfig
		if err := yaml.Unmarshal(data, &fc); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", path, err)
		}
		if err := applyFileConfig(cfg, &fc); err != nil {
			return nil, err
		}
	}
	if v := os.Getenv("LISTEN_ADDR"); v != "" {
		cfg.ListenAddr = v
	}
	if v := os.Getenv("TRACELOG_DIR"); v != "" {
		cfg.TracelogDir = v
	}
	if cfg.TracelogDir == "" {
		return nil, fmt.Errorf("tracelog_dir is not set")
	}
	return cfg, nil
}

func applyFileConfig(cfg *Config, fc *fileConfig) error {
	duration := func(key, s string, dst *time.Duration) error {
		if s == "" {
			return nil
		}
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			return fmt.Errorf("%s: invalid duration %q", key, s)
		}
		*dst = d
		return nil
	}
	if fc.ListenAddr != "" {
		cfg.ListenAddr = fc.ListenAddr
	}
	if fc.TracelogDir != "" {
		cfg.TracelogDir = fc.TracelogDir
	}
	if fc.BaseLabel != "" {
		cfg.BaseLabel = fc.BaseLabel
	}
	if err := duration("poll_interval", fc.PollInterval, &cfg.PollInterval); err != nil {
		return err
	}
	if err := duration("name_ttl", fc.NameTTL, &cfg.NameTTL); err != nil {
		return err
	}
	if fc.Backfill != nil {
		cfg.Backfill = *fc.Backfill
	}
	if fc.MaxReadMB != nil && *fc.MaxReadMB >= 0 {
		cfg.MaxReadPerPoll = int64(*fc.MaxReadMB) << 20
	}
	if fc.MaxNames != nil && *fc.MaxNames > 0 {
		cfg.MaxNames = *fc.MaxNames
	}
	if fc.SeedDays != nil && *fc.SeedDays >= 0 {
		cfg.SeedDays = *fc.SeedDays
	}
	if fc.SeedMaxMB != nil && *fc.SeedMaxMB >= 0 {
		cfg.SeedMaxBytes = int64(*fc.SeedMaxMB) << 20
	}
	if fc.LogFile != "" {
		cfg.LogFile = fc.LogFile
	}
	if fc.LogMaxSizeMB != nil && *fc.LogMaxSizeMB >= 0 {
		cfg.LogMaxBytes = int64(*fc.LogMaxSizeMB) << 20
	}
	if fc.LogKeep != nil && *fc.LogKeep >= 0 {
		cfg.LogKeep = *fc.LogKeep
	}
	return nil
}
