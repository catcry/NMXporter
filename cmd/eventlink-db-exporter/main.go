// eventlink-db-exporter reads EventLink's own database (PostgreSQL) and
// exposes Node Manager status, node status (RUNNING / STOPPED / FAILED),
// events (errors, failures) and collected / distributed files per stream to
// Prometheus. It only reads: every session is read-only. Several databases
// (servers) can be configured; each series carries a server label.
package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/lib/pq"
	"github.com/catcry/NMXporter/internal/logfile"
	"github.com/catcry/NMXporter/internal/procstats"
)

type server struct {
	build   BuildInfo
	pollers []*poller
}

func (s *server) snapshots() []Snapshot {
	out := make([]Snapshot, len(s.pollers))
	for i, p := range s.pollers {
		out[i] = p.snapshot()
	}
	return out
}

func (s *server) mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		render(&buf, s.build, s.snapshots())
		procstats.Write(&buf)
		w.Header().Set("Content-Type", prometheusContentType)
		_, _ = w.Write(buf.Bytes())
	})
	mux.HandleFunc("GET /-/healthy", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("OK\n"))
	})
	mux.HandleFunc("GET /-/ready", func(w http.ResponseWriter, r *http.Request) {
		for _, snap := range s.snapshots() {
			if !snap.Ready {
				http.Error(w, "Service Unavailable — first database poll running\n", http.StatusServiceUnavailable)
				return
			}
		}
		_, _ = w.Write([]byte("Ready\n"))
	})
	return mux
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "-healthcheck" {
		addr := os.Getenv("LISTEN_ADDR")
		if addr == "" {
			addr = ":9203"
		}
		if addr[0] == ':' {
			addr = "localhost" + addr
		}
		resp, err := http.Get("http://" + addr + "/-/healthy")
		if err != nil || resp.StatusCode != 200 {
			os.Exit(1)
		}
		os.Exit(0)
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := LoadConfig()
	if err != nil {
		log.Error("configuration error", "error", err)
		os.Exit(1)
	}
	if cfg.LogFile != "" {
		w, err := logfile.New(cfg.LogFile, cfg.LogMaxBytes, cfg.LogKeep)
		if err != nil {
			log.Error("cannot open log_file", "path", cfg.LogFile, "error", err)
			os.Exit(1)
		}
		log = slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}

	bi := currentBuildInfo()
	log.Info("eventlink-db-exporter starting",
		"version", bi.Version,
		"revision", bi.Revision,
		"databases", len(cfg.Databases),
		"poll_interval", cfg.PollInterval,
		"seed_days", cfg.SeedDays,
		"listen_addr", cfg.ListenAddr,
	)

	s := &server{build: bi}
	for _, d := range cfg.Databases {
		// The password is never logged.
		log.Info("database", "server", d.Name, "host", d.Host, "port", d.Port, "dbname", d.DBName,
			"user", d.User, "sslmode", d.SSLMode, "system_tz_offset", d.SystemTZOffset)
		p, err := newPoller(cfg, d, log)
		if err != nil {
			log.Error("database setup failed", "server", d.Name, "error", err)
			os.Exit(1)
		}
		s.pollers = append(s.pollers, p)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	procstats.Guard(log)
	for _, p := range s.pollers {
		go p.run(ctx) // one goroutine per database: a slow server never delays the others
	}

	srv := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      s.mux(),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	go func() {
		log.Info("HTTP server listening", "addr", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("HTTP server error", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	for _, p := range s.pollers {
		_ = p.sql.Close()
	}
	log.Info("stopped")
}
