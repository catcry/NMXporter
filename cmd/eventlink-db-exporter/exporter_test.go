package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// Host, port, name, user and password come from EventLink's parameters file
// unless set in db.yml; password, password_env and password_file win.
func TestResolveDatabase_ParametersAndPasswords(t *testing.T) {
	dir := t.TempDir()
	params := filepath.Join(dir, "parameters.txt")
	writeFile(t, params, "# EventLink\nEL_DB_HOST=127.0.0.1\nEL_DB_PORT=5432\nEL_DB_NAME=eventlink\nEL_DB_USER=elink\nEL_DB_PASSWORD=fromparams\nUI_DB_USER=ui\n")

	d := Database{Name: "ccelink", Parameters: params}
	if err := resolveDatabase(&d); err != nil {
		t.Fatal(err)
	}
	if d.Host != "127.0.0.1" || d.Port != 5432 || d.DBName != "eventlink" || d.User != "elink" || d.Password != "fromparams" {
		t.Errorf("from parameters: %+v", d)
	}
	if d.SSLMode != "disable" || d.SystemTZOffset != "auto" {
		t.Errorf("defaults: %+v", d)
	}

	d = Database{Parameters: params, ParamPrefix: "UI", User: "monitor", Host: "10.0.0.5"}
	if err := resolveDatabase(&d); err != nil {
		t.Fatal(err)
	}
	if d.User != "monitor" || d.Host != "10.0.0.5" || d.Name != "10.0.0.5" {
		t.Errorf("explicit values must win and name defaults to host: %+v", d)
	}

	pw := filepath.Join(dir, "pw")
	writeFile(t, pw, "secret\n")
	d = Database{User: "monitor", PasswordFile: pw, Parameters: params}
	if err := resolveDatabase(&d); err != nil || d.Password != "secret" {
		t.Errorf("password_file: %q %v", d.Password, err)
	}
	t.Setenv("DBX_PW", "fromenv")
	d = Database{User: "monitor", PasswordEnv: "DBX_PW", PasswordFile: pw}
	if err := resolveDatabase(&d); err != nil || d.Password != "fromenv" {
		t.Errorf("password_env before password_file: %q %v", d.Password, err)
	}
	if err := resolveDatabase(&Database{}); err == nil {
		t.Error("a database without user must be rejected")
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.yml")
	writeFile(t, path, `listen_addr: ":9303"
poll_interval: "30s"
seed_days: 2
databases:
  - name: a
    user: u1
    password: p
    system_tz_offset: "+03:30"
  - name: b
    host: 10.1.1.1
    user: u2
    password: p
`)
	t.Setenv("CONFIG_FILE", path)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":9303" || cfg.PollInterval != 30*time.Second || cfg.SeedDays != 2 || len(cfg.Databases) != 2 {
		t.Errorf("config: %+v", cfg)
	}
	if cfg.Databases[1].Host != "10.1.1.1" || cfg.Databases[0].Host != "127.0.0.1" {
		t.Errorf("hosts: %+v", cfg.Databases)
	}

	writeFile(t, path, "databases:\n  - name: a\n    user: u\n  - name: a\n    user: u\n")
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("duplicate names: %v", err)
	}
}

func TestParseOffset(t *testing.T) {
	for in, want := range map[string]int{"+03:30": 12600, "-04:00": -14400, "210": 12600, "0": 0} {
		if got, err := parseOffset(in); err != nil || got != want {
			t.Errorf("parseOffset(%s) = %d %v, want %d", in, got, err, want)
		}
	}
	if _, err := parseOffset("3:30"); err == nil {
		t.Error("invalid offset accepted")
	}
}

// Every session is read-only with a statement timeout; quotes in values
// cannot break out of the connection string.
func TestDSN_ReadOnly(t *testing.T) {
	d := Database{Host: "127.0.0.1", Port: 5432, DBName: "eventlink", User: "elink", Password: `a'b\c d`, SSLMode: "disable"}
	dsn := d.dsn(20 * time.Second)
	for _, want := range []string{
		`password='a\'b\\c d'`,
		`options='-c default_transaction_read_only=on -c statement_timeout=20000'`,
		`dbname='eventlink'`,
	} {
		if !strings.Contains(dsn, want) {
			t.Errorf("dsn %q misses %q", dsn, want)
		}
	}
}

func TestDecodeNodeType(t *testing.T) {
	for in, want := range map[string]string{
		"C":          "collector",
		" D":         "decoder",
		"CD":         "collector,decoder",
		"   E":       "encoder",
		"    D":      "distributor",
		"     B":     "blt",
		"      I":    "intermediate",
		"  B    S":   "businesslogic,standalone",
		"    D     ": "distributor",
	} {
		if got := decodeNodeType(in); got != want {
			t.Errorf("decodeNodeType(%q) = %q, want %q", in, got, want)
		}
	}
}

// Counted slices follow each other without gaps or overlap and stay
// reporting_lag behind now; nothing before the start is counted.
func TestFlow_Slices(t *testing.T) {
	start, _ := parseNominal("2026-10-31 11:31:18")
	lag := 5 * time.Minute
	f := &flow{cut: start}
	if _, _, ok := f.nextSlice(start.Add(3*time.Minute), lag); ok {
		t.Error("no slice before start + lag")
	}
	from, to, ok := f.nextSlice(start.Add(6*time.Minute), lag)
	if !ok || !from.Equal(start) || !to.Equal(start.Add(time.Minute)) {
		t.Errorf("first slice: %v %v %v", from, to, ok)
	}
	f.cut = to
	from, to, _ = f.nextSlice(start.Add(7*time.Minute), lag)
	if !from.Equal(start.Add(time.Minute)) || !to.Equal(start.Add(2*time.Minute)) {
		t.Errorf("second slice must start where the first ended: %v %v", from, to)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("Invalid  parameter\nvalue", 100); got != "Invalid parameter value" {
		t.Errorf("whitespace: %q", got)
	}
	if got := truncate("abcdef", 3); got != "abc…" {
		t.Errorf("cut: %q", got)
	}
}

func TestRender(t *testing.T) {
	snap := Snapshot{
		Name: "ccelink", Ready: true, Up: true, LastPoll: time.Unix(1793460000, 0), TZOffset: 12600,
		Errors:     map[string]uint64{"events": 1},
		Components: []Component{{Name: "EventLink", Host: "ccelink", Status: "S", Updated: 1793433682}},
		Nodes: map[NodeKey]NodeStatus{
			{"el2_javad", "EL_CFG_FTP_DISTRIBUTOR"}: {Host: "ccelink", NodeType: "distributor", Status: "FAILED", LastStart: 1793432101},
		},
		Events: map[NodeKey]NodeEvents{
			{"el2_javad", "EL_CFG_FTP_DISTRIBUTOR"}: {Count: map[string]uint64{"E": 2}, Last: map[string]float64{"E": 1793432098},
				LastFailure: 1793432098, ErrKey: "NODEMANAGER03011", ErrMsg: `Node 'EL_CFG_FTP_DISTRIBUTOR' aborted and was "disabled".`},
		},
		Streams: map[string]StreamFlow{"el2_javad": {LastCollected: 1793433678, LastDistributed: 1793433645, Files: [2]uint64{3, 2}}},
	}
	var buf bytes.Buffer
	render(&buf, BuildInfo{Version: "1.6.0"}, []Snapshot{snap, {Name: "warming"}})
	out := buf.String()
	for _, want := range []string{
		`eventlink_db_exporter_build_info{version="1.6.0",revision="",goversion=""} 1`,
		`eventlink_db_up{server="ccelink"} 1`,
		`eventlink_db_query_errors_total{server="ccelink",query="events"} 1`,
		`eventlink_db_system_tz_offset_seconds{server="ccelink"} 12600`,
		`eventlink_nodemanager_up{server="ccelink",host="ccelink"} 0`,
		`eventlink_db_component_status{server="ccelink",component="EventLink",host="ccelink",status="S"} 1`,
		`eventlink_db_node_status{server="ccelink",stream="el2_javad",node="EL_CFG_FTP_DISTRIBUTOR",host="ccelink",node_type="distributor",status="FAILED"} 1`,
		`eventlink_db_events_total{server="ccelink",stream="el2_javad",node="EL_CFG_FTP_DISTRIBUTOR",type="E"} 2`,
		`eventlink_db_node_last_failure_timestamp_seconds{server="ccelink",stream="el2_javad",node="EL_CFG_FTP_DISTRIBUTOR"} 1.793432098e+09`,
		`eventlink_db_node_last_error_info{server="ccelink",stream="el2_javad",node="EL_CFG_FTP_DISTRIBUTOR",key="NODEMANAGER03011",message="Node 'EL_CFG_FTP_DISTRIBUTOR' aborted and was \"disabled\"."} 1`,
		`eventlink_db_stream_last_collected_timestamp_seconds{server="ccelink",stream="el2_javad"} 1.793433678e+09`,
		`eventlink_db_stream_files_total{server="ccelink",stream="el2_javad",event="distributed"} 2`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(out, `server="warming"`) {
		t.Error("a database before its first poll must not be rendered")
	}
}
