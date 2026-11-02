// Package procstats writes the exporter's own CPU, memory, file descriptor
// and Go runtime figures in the standard Prometheus names (process_*, go_*),
// so the dashboard and alerts can watch what each exporter costs the server.
package procstats

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

var start = time.Now()

// Guard stops the process when its resident memory stays above MAX_RSS_MB
// (environment; unset or 0 = no limit) for two checks in a row. GOMEMLIMIT
// only makes the Go GC work harder; this is the hard limit. The control
// script's cron watchdog starts the exporter again.
func Guard(log *slog.Logger) {
	mb, _ := strconv.Atoi(os.Getenv("MAX_RSS_MB"))
	if mb <= 0 {
		return
	}
	limit := float64(int64(mb) << 20)
	log.Info("memory guard on", "max_rss_mb", mb, "gomemlimit", os.Getenv("GOMEMLIMIT"), "gomaxprocs", runtime.GOMAXPROCS(0))
	go func() {
		over := 0
		for range time.Tick(10 * time.Second) {
			p, ok := readProc()
			if !ok {
				return
			}
			if p.rss <= limit {
				over = 0
				continue
			}
			if over++; over >= 2 {
				log.Error("resident memory above MAX_RSS_MB, exiting", "rss_mb", int(p.rss)>>20, "max_rss_mb", mb)
				os.Exit(3)
			}
		}
	}()
}

// Write writes process_* (Linux, from /proc/self) and go_* metrics.
func Write(w io.Writer) {
	if p, ok := readProc(); ok {
		gauge(w, "process_cpu_seconds_total", "counter", "Total user and system CPU time spent in seconds.", p.cpu)
		gauge(w, "process_resident_memory_bytes", "gauge", "Resident memory size in bytes.", p.rss)
		gauge(w, "process_virtual_memory_bytes", "gauge", "Virtual memory size in bytes.", p.vsize)
		gauge(w, "process_threads", "gauge", "Number of OS threads.", p.threads)
		if n, ok := openFDs(); ok {
			gauge(w, "process_open_fds", "gauge", "Number of open file descriptors.", float64(n))
		}
		if n, ok := maxFDs(); ok {
			gauge(w, "process_max_fds", "gauge", "Maximum number of open file descriptors.", n)
		}
	}
	gauge(w, "process_start_time_seconds", "gauge", "Start time of the process since unix epoch in seconds.", float64(start.Unix()))

	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	gauge(w, "go_goroutines", "gauge", "Number of goroutines that currently exist.", float64(runtime.NumGoroutine()))
	gauge(w, "go_memstats_heap_inuse_bytes", "gauge", "Number of heap bytes that are in use.", float64(m.HeapInuse))
	gauge(w, "go_memstats_sys_bytes", "gauge", "Number of bytes obtained from the OS.", float64(m.Sys))
	gauge(w, "go_gc_cycles_total", "counter", "Number of completed GC cycles.", float64(m.NumGC))
	gauge(w, "go_gomaxprocs", "gauge", "GOMAXPROCS: CPUs the Go scheduler may use at once.", float64(runtime.GOMAXPROCS(0)))
}

func gauge(w io.Writer, name, kind, help string, v float64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %g\n", name, help, name, kind, name, v)
}

type proc struct{ cpu, rss, vsize, threads float64 }

// clockTicks is USER_HZ, 100 on every Linux system Go supports.
const clockTicks = 100

func readProc() (proc, bool) {
	b, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return proc{}, false
	}
	// Fields after the command name in parentheses (which may contain spaces).
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return proc{}, false
	}
	f := strings.Fields(s[i+1:])
	// f[0] is field 3 (state); utime=14, stime=15, threads=20, vsize=23, rss=24.
	if len(f) < 22 {
		return proc{}, false
	}
	num := func(k int) float64 { v, _ := strconv.ParseFloat(f[k-3], 64); return v }
	return proc{
		cpu:     (num(14) + num(15)) / clockTicks,
		threads: num(20),
		vsize:   num(23),
		rss:     num(24) * float64(os.Getpagesize()),
	}, true
}

func openFDs() (int, bool) {
	d, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0, false
	}
	return len(d), true
}

func maxFDs() (float64, bool) {
	b, err := os.ReadFile("/proc/self/limits")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "Max open files") {
			if f := strings.Fields(line); len(f) >= 4 {
				v, err := strconv.ParseFloat(f[3], 64)
				return v, err == nil
			}
		}
	}
	return 0, false
}
