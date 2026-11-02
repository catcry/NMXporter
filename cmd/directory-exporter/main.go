package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/catcry/NMXporter/internal/logfile"
	"github.com/catcry/NMXporter/internal/procstats"
)

func main() {
	// Built-in healthcheck mode: called by Docker as /directory-exporter -healthcheck
	if len(os.Args) == 2 && os.Args[1] == "-healthcheck" {
		addr := os.Getenv("LISTEN_ADDR")
		if addr == "" {
			addr = ":9200"
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

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	cfg, err := LoadConfig(log)
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

	// Log each configured target so the operator can verify the config at a glance
	for _, t := range cfg.Targets {
		switch {
		case len(t.Dirs) > 0:
			log.Info("target configured",
				"base", t.Base,
				"dirs", fmt.Sprintf("%v", t.Dirs),
				"mode", "explicit")
		case t.Pattern != "":
			log.Info("target configured",
				"base", t.Base,
				"pattern", t.Pattern,
				"max_depth", t.MaxDepth,
				"mode", "auto-discover-pattern")
		default:
			log.Info("target configured",
				"base", t.Base,
				"max_depth", t.MaxDepth,
				"mode", "auto-discover")
		}
	}
	bi := currentBuildInfo()
	log.Info("directory-exporter starting",
		"version", bi.Version,
		"revision", bi.Revision,
		"heartbeat_grace", cfg.HeartbeatGrace,
		"targets", len(cfg.Targets),
		"scan_interval", cfg.ScanInterval,
		"scan_workers", cfg.ScanWorkers,
		"scan_timeout", cfg.ScanTimeout,
		"max_files_per_dir", cfg.MaxFilesPerDir,
		"max_stat_files", cfg.MaxStatFiles,
		"unchanged_rescan_interval", cfg.UnchangedRescan,
		"tracelog_dir", cfg.TracelogDir,
		"min_delay", cfg.MinDelay,
		"trace_lookback_days", cfg.TraceLookbackDays,
		"listen_addr", cfg.ListenAddr,
	)

	procstats.Guard(log)
	exp := NewExporter(cfg, log)
	exp.ValidateTargets()

	if err := exp.Discover(); err != nil {
		log.Warn("initial discovery errors (non-fatal)", "error", err)
	}

	go exp.RunScanLoop()
	go exp.RunDiscoveryLoop()

	srv := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      NewMux(exp, log),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go func() {
		log.Info("HTTP server listening", "addr", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("HTTP server error", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	stop()

	exp.Stop()

	shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)

	log.Info("stopped")
}
