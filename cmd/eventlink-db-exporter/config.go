package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config holds every runtime tunable of eventlink-db-exporter.
type Config struct {
	ListenAddr       string
	PollInterval     time.Duration // how often each database is queried
	QueryTimeout     time.Duration // statement_timeout of the regular queries
	SeedTimeout      time.Duration // statement_timeout of the one-time startup queries
	SeedDays         int           // days looked back at startup for last times (events, collected, distributed)
	ReportingLag     time.Duration // collected / distributed counters stay this far behind now, so late-written rows are counted
	MaxRowsPerPoll   int           // events read per poll; the rest next poll
	NodeManagerGrace time.Duration // Node Manager is up while its status row is younger than this (EventLink: 2m30s)
	FailureKeys      []string      // event keys counted as node failures
	MessageMaxLen    int           // longest event message kept in labels
	Databases        []Database
	LogFile          string // "" = stdout
	LogMaxBytes      int64  // rotate LogFile above this size (0 = never)
	LogKeep          int    // rotated files kept (db-exporter.log.1 … .N)
}

// Database is one EventLink database. Values not set here are taken from
// the EventLink parameters file (EVENTLINK_DB/parameters.txt) when given.
type Database struct {
	Name           string // value of the "server" label
	Host           string
	Port           int
	DBName         string
	User           string
	Password       string
	PasswordEnv    string // environment variable holding the password
	PasswordFile   string // file holding only the password
	Parameters     string // EventLink parameters.txt with <prefix>_DB_HOST/PORT/NAME/USER/PASSWORD
	ParamPrefix    string // prefix in that file (default EL)
	SSLMode        string // lib/pq sslmode (default disable)
	SystemTZOffset string // "auto" (from el_events) or "+03:30": zone of the *system and reporting columns
}

type fileDatabase struct {
	Name           string `yaml:"name"`
	Host           string `yaml:"host"`
	Port           int    `yaml:"port"`
	DBName         string `yaml:"dbname"`
	User           string `yaml:"user"`
	Password       string `yaml:"password"`
	PasswordEnv    string `yaml:"password_env"`
	PasswordFile   string `yaml:"password_file"`
	Parameters     string `yaml:"eventlink_parameters"`
	ParamPrefix    string `yaml:"parameters_prefix"`
	SSLMode        string `yaml:"sslmode"`
	SystemTZOffset string `yaml:"system_tz_offset"`
}

type fileConfig struct {
	ListenAddr       string         `yaml:"listen_addr"`
	PollInterval     string         `yaml:"poll_interval"`
	QueryTimeout     string         `yaml:"query_timeout"`
	SeedTimeout      string         `yaml:"seed_timeout"`
	SeedDays         *int           `yaml:"seed_days"`
	ReportingLag     string         `yaml:"reporting_lag"`
	MaxRowsPerPoll   *int           `yaml:"max_rows_per_poll"`
	NodeManagerGrace string         `yaml:"nodemanager_grace"`
	FailureKeys      []string       `yaml:"failure_keys"`
	MessageMaxLen    *int           `yaml:"message_max_len"`
	Databases        []fileDatabase `yaml:"databases"`
	LogFile          string         `yaml:"log_file"`
	LogMaxSizeMB     *int           `yaml:"log_max_size_mb"`
	LogKeep          *int           `yaml:"log_keep"`
}

// LoadConfig reads defaults, then the YAML file named by CONFIG_FILE, then
// LISTEN_ADDR. Every database is resolved (parameters file, password).
func LoadConfig() (*Config, error) {
	cfg := &Config{
		ListenAddr:       ":9203",
		PollInterval:     60 * time.Second,
		QueryTimeout:     20 * time.Second,
		SeedTimeout:      120 * time.Second,
		SeedDays:         7,
		ReportingLag:     5 * time.Minute,
		MaxRowsPerPoll:   100000,
		NodeManagerGrace: 150 * time.Second,
		FailureKeys:      []string{"NODEMANAGER03011", "NODEMANAGER03023"},
		MessageMaxLen:    200,
		LogMaxBytes:      10 << 20,
		LogKeep:          3,
	}
	path := os.Getenv("CONFIG_FILE")
	if path == "" {
		return nil, fmt.Errorf("CONFIG_FILE is not set (a db.yml with at least one database)")
	}
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
	if v := os.Getenv("LISTEN_ADDR"); v != "" {
		cfg.ListenAddr = v
	}
	if len(cfg.Databases) == 0 {
		return nil, fmt.Errorf("no databases configured")
	}
	seen := make(map[string]bool)
	for i := range cfg.Databases {
		d := &cfg.Databases[i]
		if err := resolveDatabase(d); err != nil {
			return nil, fmt.Errorf("database %q: %w", d.Name, err)
		}
		if seen[d.Name] {
			return nil, fmt.Errorf("database name %q used twice", d.Name)
		}
		seen[d.Name] = true
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
	for _, d := range []struct {
		key string
		s   string
		dst *time.Duration
	}{
		{"poll_interval", fc.PollInterval, &cfg.PollInterval},
		{"query_timeout", fc.QueryTimeout, &cfg.QueryTimeout},
		{"seed_timeout", fc.SeedTimeout, &cfg.SeedTimeout},
		{"reporting_lag", fc.ReportingLag, &cfg.ReportingLag},
		{"nodemanager_grace", fc.NodeManagerGrace, &cfg.NodeManagerGrace},
	} {
		if err := duration(d.key, d.s, d.dst); err != nil {
			return err
		}
	}
	if fc.SeedDays != nil && *fc.SeedDays >= 0 {
		cfg.SeedDays = *fc.SeedDays
	}
	if fc.MaxRowsPerPoll != nil && *fc.MaxRowsPerPoll > 0 {
		cfg.MaxRowsPerPoll = *fc.MaxRowsPerPoll
	}
	if len(fc.FailureKeys) > 0 {
		cfg.FailureKeys = fc.FailureKeys
	}
	if fc.MessageMaxLen != nil && *fc.MessageMaxLen > 0 {
		cfg.MessageMaxLen = *fc.MessageMaxLen
	}
	for _, d := range fc.Databases {
		cfg.Databases = append(cfg.Databases, Database{
			Name: d.Name, Host: d.Host, Port: d.Port, DBName: d.DBName, User: d.User,
			Password: d.Password, PasswordEnv: d.PasswordEnv, PasswordFile: d.PasswordFile,
			Parameters: d.Parameters, ParamPrefix: d.ParamPrefix, SSLMode: d.SSLMode,
			SystemTZOffset: d.SystemTZOffset,
		})
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

// resolveDatabase fills unset fields from the EventLink parameters file and
// picks the password: password, password_env, password_file, then the
// parameters file.
func resolveDatabase(d *Database) error {
	if d.ParamPrefix == "" {
		d.ParamPrefix = "EL"
	}
	var params map[string]string
	if d.Parameters != "" {
		p, err := readParameters(d.Parameters)
		if err != nil {
			return err
		}
		params = p
	}
	param := func(key string) string { return params[d.ParamPrefix+"_DB_"+key] }
	if d.Host == "" {
		d.Host = param("HOST")
	}
	if d.Port == 0 {
		if p, err := strconv.Atoi(param("PORT")); err == nil {
			d.Port = p
		}
	}
	if d.DBName == "" {
		d.DBName = param("NAME")
	}
	if d.User == "" {
		d.User = param("USER")
	}
	switch {
	case d.Password != "":
	case d.PasswordEnv != "":
		d.Password = os.Getenv(d.PasswordEnv)
		if d.Password == "" {
			return fmt.Errorf("password_env %s is empty", d.PasswordEnv)
		}
	case d.PasswordFile != "":
		b, err := os.ReadFile(d.PasswordFile)
		if err != nil {
			return fmt.Errorf("password_file: %w", err)
		}
		d.Password = strings.TrimRight(string(b), "\r\n")
	default:
		d.Password = param("PASSWORD")
	}
	if d.Host == "" {
		d.Host = "127.0.0.1"
	}
	if d.Port == 0 {
		d.Port = 5432
	}
	if d.DBName == "" {
		d.DBName = "eventlink"
	}
	if d.User == "" {
		return fmt.Errorf("no user (set user or eventlink_parameters)")
	}
	if d.Name == "" {
		d.Name = d.Host
	}
	if d.SSLMode == "" {
		d.SSLMode = "disable"
	}
	if d.SystemTZOffset == "" {
		d.SystemTZOffset = "auto"
	}
	if d.SystemTZOffset != "auto" {
		if _, err := parseOffset(d.SystemTZOffset); err != nil {
			return err
		}
	}
	return nil
}

// readParameters reads KEY=VALUE lines (EventLink meta/*/parameters.txt).
func readParameters(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("eventlink_parameters: %w", err)
	}
	defer f.Close()
	m := make(map[string]string)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			m[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return m, sc.Err()
}

// parseOffset parses "+03:30", "-04:00" or "210" (minutes) into seconds.
func parseOffset(s string) (int, error) {
	if m, err := strconv.Atoi(s); err == nil {
		return m * 60, nil
	}
	if len(s) == 6 && (s[0] == '+' || s[0] == '-') && s[3] == ':' {
		h, err1 := strconv.Atoi(s[1:3])
		m, err2 := strconv.Atoi(s[4:6])
		if err1 == nil && err2 == nil {
			sec := h*3600 + m*60
			if s[0] == '-' {
				sec = -sec
			}
			return sec, nil
		}
	}
	return 0, fmt.Errorf("system_tz_offset: invalid offset %q (use auto, +03:30 or minutes)", s)
}

// dsn builds a lib/pq connection string. Every session is read-only and has
// a statement timeout, whatever rights the user has.
func (d Database) dsn(statementTimeout time.Duration) string {
	q := func(v string) string {
		return "'" + strings.ReplaceAll(strings.ReplaceAll(v, `\`, `\\`), `'`, `\'`) + "'"
	}
	return fmt.Sprintf("host=%s port=%d dbname=%s user=%s password=%s sslmode=%s connect_timeout=10 "+
		"fallback_application_name=eventlink-db-exporter options=%s",
		q(d.Host), d.Port, q(d.DBName), q(d.User), q(d.Password), q(d.SSLMode),
		q(fmt.Sprintf("-c default_transaction_read_only=on -c statement_timeout=%d", statementTimeout.Milliseconds())))
}
