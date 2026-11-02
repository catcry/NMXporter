// eventlink-trace-exporter reads EventLink execution trace logs and exposes
// per-node file/record counters, failures and collected → distributed
// latencies to Prometheus. Folder, queue and node-status metrics stay in
// directory-exporter; the two run as separate processes so a slow or failing
// trace parser never affects buffer monitoring.
package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/catcry/NMXporter/internal/logfile"
	"github.com/catcry/NMXporter/internal/procstats"
	"github.com/catcry/NMXporter/internal/tracelog"
)

type server struct {
	cfg    *Config
	log    *slog.Logger
	tailer *tracelog.Tailer

	mu sync.RWMutex
	st state
}

// poll reads new trace lines once. A panic in the parser is logged and the
// next poll tries again, so one bad line cannot stop the exporter.
func (s *server) poll(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("trace poll panicked", "panic", r)
			s.mu.Lock()
			s.st.PollErrors++
			s.mu.Unlock()
		}
	}()
	t0 := time.Now()
	err := s.tailer.Poll(ctx, t0)
	snap := s.tailer.Snapshot()
	d := time.Since(t0)

	s.mu.Lock()
	prev := s.st.Trace
	if err != nil {
		s.st.PollErrors++
	}
	s.st.Trace, s.st.LastPoll, s.st.PollDuration = snap, time.Now(), d
	s.mu.Unlock()

	if err != nil {
		s.log.Warn("trace log poll failed", "dir", s.cfg.TracelogDir, "error", err)
		return
	}
	var dBytes, dLines uint64 = snap.ReadBytes, snap.Lines
	if prev != nil {
		dBytes, dLines = snap.ReadBytes-prev.ReadBytes, snap.Lines-prev.Lines
	}
	s.log.Debug("trace logs read", "bytes", dBytes, "lines", dLines, "files", snap.FilesTracked,
		"tracked_names", snap.TrackedNames, "duration", d.Round(time.Millisecond))
}

func (s *server) run(ctx context.Context) {
	s.poll(ctx)
	s.log.Info("first trace poll finished", "duration", s.snapshot().PollDuration.Round(time.Millisecond))
	t := time.NewTicker(s.cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.poll(ctx)
		}
	}
}

func (s *server) snapshot() state {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.st
}

func (s *server) mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		render(&buf, s.snapshot())
		procstats.Write(&buf)
		w.Header().Set("Content-Type", prometheusContentType)
		_, _ = w.Write(buf.Bytes())
	})
	mux.HandleFunc("GET /-/healthy", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("OK\n"))
	})
	mux.HandleFunc("GET /-/ready", func(w http.ResponseWriter, r *http.Request) {
		if s.snapshot().Trace == nil {
			http.Error(w, "Service Unavailable — first trace poll running\n", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("Ready\n"))
	})
	return mux
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "-healthcheck" {
		addr := os.Getenv("LISTEN_ADDR")
		if addr == "" {
			addr = ":9202"
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
	log.Info("eventlink-trace-exporter starting",
		"version", bi.Version,
		"revision", bi.Revision,
		"tracelog_dir", cfg.TracelogDir,
		"base_label", cfg.BaseLabel,
		"poll_interval", cfg.PollInterval,
		"backfill", cfg.Backfill,
		"max_read_bytes", cfg.MaxReadPerPoll,
		"name_ttl", cfg.NameTTL,
		"max_names", cfg.MaxNames,
		"seed_days", cfg.SeedDays,
		"seed_max_mb", cfg.SeedMaxBytes>>20,
		"listen_addr", cfg.ListenAddr,
	)

	s := &server{
		cfg: cfg,
		log: log,
		tailer: tracelog.NewTailer(cfg.TracelogDir, tracelog.Options{
			Backfill:       cfg.Backfill,
			MaxReadPerPoll: cfg.MaxReadPerPoll,
			NameTTL:        cfg.NameTTL,
			MaxNames:       cfg.MaxNames,
			SeedDays:       cfg.SeedDays,
			SeedMaxBytes:   cfg.SeedMaxBytes,
		}),
		st: state{Base: cfg.BaseLabel, Build: bi},
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	procstats.Guard(log)
	go s.run(ctx)

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
	log.Info("stopped")
}
