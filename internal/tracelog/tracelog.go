// Package tracelog incrementally reads EventLink execution trace logs and
// turns them into per-node counters and collected → distributed latencies.
package tracelog

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Trace log engine.
//
// EventLink writes one execution trace per stream and day, rotated as
// execution_trace_<stream>_<YYYYMMDD>_<seq>.log (seq 0,1,2,… ~10 MB each).
// Every line of a file's journey carries the file ID, whose first part is the
// collection epoch:
//
//	20261026 082106.273 L NB [DIS_TAP]  File TAP:CDIR… 1793016004x003_0011282: Distributed file …
//	                                                   └ collected at 1793016004
//
// Latency is measured two ways:
//   - by ID: distribution time minus the collection epoch in the first ID on
//     the "Distributed file" line;
//   - by name: distribution time minus the time of the "Collected file" line
//     whose file name has the same part before the first "." (in the same
//     stream). This also covers distributors whose line carries another
//     file's ID (trigger-driven distributors such as Dis_KISH_LI).
//
// Tailer reads every trace file once, only the bytes appended since the
// previous poll, and turns lines into counters per node. It never re-reads.

// LatencyBuckets are the histogram upper bounds (seconds) for collected →
// distributed latency.
var LatencyBuckets = []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600, 7200, 21600, 86400}

// maxLatency rejects IDs that are clearly not a collection time.
const maxLatency = 30 * 24 * time.Hour

var (
	fileIDRegex       = regexp.MustCompile(`\b(\d{10})x\d{3}_\d+`)
	bytesRegex        = regexp.MustCompile(`, (\d+) bytes`)
	wroteRecordsRegex = regexp.MustCompile(`Wrote (\d+) records`)
	readRecordsRegex  = regexp.MustCompile(`Read (\d+) records`)
	nodeFailedRegex   = regexp.MustCompile(`aborted and was disabled|Node index .* failed`)
)

// NodeKey identifies one node inside one stream.
type NodeKey struct {
	Stream string
	Node   string
}

// LatencyHistogram is a cumulative Prometheus-style histogram.
type LatencyHistogram struct {
	Counts []uint64 // per bucket in LatencyBuckets, non-cumulative
	Count  uint64
	Sum    float64
}

// Observe adds one value.
func (h *LatencyHistogram) Observe(v float64) {
	if h.Counts == nil {
		h.Counts = make([]uint64, len(LatencyBuckets))
	}
	for i, ub := range LatencyBuckets {
		if v <= ub {
			h.Counts[i]++
			break
		}
	}
	h.Count++
	h.Sum += v
}

func (h LatencyHistogram) clone() LatencyHistogram {
	h.Counts = append([]uint64(nil), h.Counts...)
	return h
}

// Match methods for a "Distributed file" line.
const (
	MatchName = iota // collected time found by file name
	MatchID          // collected time from the file ID
	MatchNone        // neither
	matchKinds
)

// MatchNames are the label values of the Match* methods.
var MatchNames = [matchKinds]string{"name", "id", "none"}

// NodeStats accumulates everything the trace logs say about one node
// since the exporter started.
type NodeStats struct {
	CollectedFiles   uint64
	CollectedBytes   uint64
	DistributedFiles uint64
	DistributedBytes uint64
	RecordsIn        uint64
	RecordsOut       uint64
	Failures         uint64

	LastFailure         float64 // epoch of last failure line
	LastExecution       float64 // epoch of last "Scheduled execution … finished"
	LastExecutionOK     bool
	LastCollected       float64 // epoch of last "Collected file" line (also from the startup seed)
	LastCollectedFile   string
	LastDistributed     float64 // epoch of last "Distributed file" line (also from the startup seed)
	LastDistributedFile string
	LastLatency         float64 // seconds, collected → distributed of the last distributed file (by ID)
	HasLatency          bool
	Latency             LatencyHistogram // by ID

	// By name (part before the first "."), falling back to the ID.
	NameLatency     LatencyHistogram
	NameLastLatency float64
	HasNameLatency  bool
	Matches         [matchKinds]uint64 // Distributed lines per Match* method
}

// PendingStats describes files of one stream that were collected but have
// no "Distributed file" line yet (matched by name).
type PendingStats struct {
	Files          int
	Oldest         float64 // time of the oldest such "Collected file" line; 0 when none
	EvictedExpired uint64  // dropped after NameTTL without a distribution
	EvictedLimit   uint64  // dropped because MaxNames was reached
}

// Snapshot is a copy of the tailer state for rendering.
type Snapshot struct {
	Nodes        map[NodeKey]NodeStats
	StreamLastTS map[string]float64 // newest line timestamp per stream
	Pending      map[string]PendingStats
	TrackedNames int // collected names kept for matching
	ReadBytes    uint64
	Lines        uint64
	FilesTracked int
	BudgetHits   uint64 // polls that stopped early because of the read budget
	SeedBytes    uint64 // bytes read by the startup seed
	SeedFiles    int    // trace files read by the startup seed
}

// Options configure a Tailer.
type Options struct {
	Backfill       bool          // read existing files from the start on first sight
	MaxReadPerPoll int64         // read budget per poll; the rest is read next poll (0 = unlimited)
	NameTTL        time.Duration // how long a collected name is kept for matching (0 = 24h)
	MaxNames       int           // cap on collected names kept (0 = 500000)

	// Without Backfill, the first poll reads the newest trace files of the
	// last SeedDays days (newest first, at most SeedMaxBytes per stream) only
	// for the last collected / distributed time of each node. Counters,
	// latencies and pending files are not touched. 0 = no seed.
	SeedDays     int
	SeedMaxBytes int64
}

type tailFile struct {
	offset int64
	size   int64
}

// Tailer incrementally reads EventLink trace logs.
type Tailer struct {
	dir string
	opt Options

	mu           sync.Mutex
	started      bool
	files        map[string]*tailFile
	nodes        map[NodeKey]*NodeStats
	streamLastTS map[string]float64
	names        *nameTracker
	readBytes    uint64
	lines        uint64
	budgetHits   uint64
	seedBytes    uint64
	seedFiles    int
}

// NewTailer returns a tailer for dir. Without Backfill, files that already
// exist when the exporter starts are read from their current end, so a
// restart never re-reads gigabytes of history.
func NewTailer(dir string, opt Options) *Tailer {
	if opt.NameTTL <= 0 {
		opt.NameTTL = 24 * time.Hour
	}
	if opt.MaxNames <= 0 {
		opt.MaxNames = 500000
	}
	return &Tailer{
		dir:          dir,
		opt:          opt,
		files:        make(map[string]*tailFile),
		nodes:        make(map[NodeKey]*NodeStats),
		streamLastTS: make(map[string]float64),
		names:        newNameTracker(opt.NameTTL, opt.MaxNames),
	}
}

// TraceName splits execution_trace_<stream>_<YYYYMMDD>_<seq>.log.
func TraceName(name string) (stream, date string, seq int, ok bool) {
	if !strings.HasPrefix(name, "execution_trace_") || !strings.HasSuffix(name, ".log") {
		return "", "", 0, false
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(name, "execution_trace_"), ".log")
	i := strings.LastIndex(rest, "_")
	if i < 0 {
		return "", "", 0, false
	}
	seq, err := strconv.Atoi(rest[i+1:])
	if err != nil {
		return "", "", 0, false
	}
	rest = rest[:i]
	j := strings.LastIndex(rest, "_")
	if j < 0 || len(rest)-j-1 != 8 {
		return "", "", 0, false
	}
	date = rest[j+1:]
	if _, err := strconv.Atoi(date); err != nil {
		return "", "", 0, false
	}
	return rest[:j], date, seq, rest[:j] != ""
}

type traceFileRef struct {
	path   string
	stream string
	date   string
	seq    int
	size   int64
}

// Poll reads everything appended to today's and yesterday's trace files since
// the last poll. Files are processed oldest first so per-node "last" values
// end up being the newest.
func (t *Tailer) Poll(ctx context.Context, now time.Time) error {
	entries, err := os.ReadDir(t.dir)
	if err != nil {
		return err
	}
	minDate := now.AddDate(0, 0, -1).Format("20060102")

	var refs []traceFileRef
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		stream, date, seq, ok := TraceName(e.Name())
		if !ok || date < minDate {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		refs = append(refs, traceFileRef{filepath.Join(t.dir, e.Name()), stream, date, seq, info.Size()})
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].stream != refs[j].stream {
			return refs[i].stream < refs[j].stream
		}
		if refs[i].date != refs[j].date {
			return refs[i].date < refs[j].date
		}
		return refs[i].seq < refs[j].seq
	})

	t.mu.Lock()
	started := t.started
	t.mu.Unlock()
	if !started && !t.opt.Backfill && t.opt.SeedDays > 0 {
		// Read without the lock so /metrics is not blocked meanwhile.
		seed := seedLast(ctx, t.dir, entries, now, t.opt.SeedDays, t.opt.SeedMaxBytes)
		t.mu.Lock()
		t.mergeSeed(seed)
		t.mu.Unlock()
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	seen := make(map[string]bool, len(refs))
	budget := t.opt.MaxReadPerPoll
	for _, r := range refs {
		seen[r.path] = true
		st, known := t.files[r.path]
		if !known {
			// A file that appears after startup is a fresh rotation and is
			// read from 0. Files present at startup are read from their end
			// unless backfill is enabled.
			st = &tailFile{}
			if !t.started && !t.opt.Backfill {
				st.offset = r.size
			}
			t.files[r.path] = st
		}
		if r.size < st.offset {
			st.offset = 0 // truncated or replaced
		}
		st.size = r.size
		if r.size == st.offset {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		if t.opt.MaxReadPerPoll > 0 && budget <= 0 {
			t.budgetHits++
			break
		}
		n := t.readFrom(r, st, budget)
		budget -= n
	}
	for p := range t.files {
		if !seen[p] {
			delete(t.files, p)
		}
	}
	t.names.expire(float64(now.Unix()))
	t.started = true
	return nil
}

// readFrom parses complete lines of r starting at st.offset and advances the
// offset past the last complete line. Returns the bytes consumed.
func (t *Tailer) readFrom(r traceFileRef, st *tailFile, budget int64) int64 {
	f, err := os.Open(r.path)
	if err != nil {
		return 0
	}
	defer f.Close()
	if _, err := f.Seek(st.offset, io.SeekStart); err != nil {
		return 0
	}
	limit := r.size - st.offset
	if t.opt.MaxReadPerPoll > 0 && budget < limit {
		limit = budget
	}
	br := bufio.NewReaderSize(io.LimitReader(f, limit), 256*1024)
	var consumed int64
	for {
		line, err := br.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			// Overlong line: skip it without parsing.
			consumed += int64(len(line))
			for err == bufio.ErrBufferFull {
				line, err = br.ReadSlice('\n')
				consumed += int64(len(line))
			}
			continue
		}
		if err != nil {
			break // partial last line stays for the next poll
		}
		consumed += int64(len(line))
		t.lines++
		t.parseLine(r.stream, bytes.TrimRight(line, "\r\n"))
	}
	st.offset += consumed
	t.readBytes += uint64(consumed)
	return consumed
}

// parseLineTime parses "YYYYMMDD HHMMSS.mmm" at the start of a line.
func parseLineTime(line []byte) (float64, bool) {
	if len(line) < 15 || line[8] != ' ' {
		return 0, false
	}
	d, t := line[:8], line[9:15]
	for _, c := range append(append([]byte{}, d...), t...) {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	atoi := func(b []byte) int {
		n := 0
		for _, c := range b {
			n = n*10 + int(c-'0')
		}
		return n
	}
	ts := time.Date(atoi(d[0:4]), time.Month(atoi(d[4:6])), atoi(d[6:8]),
		atoi(t[0:2]), atoi(t[2:4]), atoi(t[4:6]), 0, time.Local)
	frac := 0.0
	if len(line) > 16 && line[15] == '.' {
		end := 16
		for end < len(line) && end < 19 && line[end] >= '0' && line[end] <= '9' {
			end++
		}
		if v, err := strconv.ParseFloat("0."+string(line[16:end]), 64); err == nil {
			frac = v
		}
	}
	return float64(ts.Unix()) + frac, true
}

// lineNode returns the node name inside the first [..] of the line.
func lineNode(line []byte) string {
	i := bytes.IndexByte(line, '[')
	if i < 0 {
		return ""
	}
	j := bytes.IndexByte(line[i:], ']')
	if j < 0 {
		return ""
	}
	return string(line[i+1 : i+j])
}

func firstUint(re *regexp.Regexp, s []byte) uint64 {
	m := re.FindSubmatch(s)
	if len(m) < 2 {
		return 0
	}
	n, _ := strconv.ParseUint(string(m[1]), 10, 64)
	return n
}

func (t *Tailer) parseLine(stream string, line []byte) {
	ts, ok := parseLineTime(line)
	if !ok {
		return
	}
	if ts > t.streamLastTS[stream] {
		t.streamLastTS[stream] = ts
	}
	node := lineNode(line)
	if node == "" {
		return
	}
	stats := func() *NodeStats {
		k := NodeKey{stream, node}
		s := t.nodes[k]
		if s == nil {
			s = &NodeStats{}
			t.nodes[k] = s
		}
		return s
	}

	switch {
	case bytes.Contains(line, []byte("Distributed file ")):
		s := stats()
		s.DistributedFiles++
		s.DistributedBytes += firstUint(bytesRegex, line)
		// The first ID on the line is the original one (later IDs belong to
		// files re-created by encoders).
		lat, latOK := 0.0, false
		if m := fileIDRegex.FindSubmatch(line); len(m) > 1 {
			if epoch, err := strconv.ParseInt(string(m[1]), 10, 64); err == nil {
				lat = ts - float64(epoch)
				latOK = lat >= 0 && lat <= maxLatency.Seconds()
			}
		}
		if latOK {
			s.Latency.Observe(lat)
		}
		name := distributedFileName(line)
		nameLat, method := lat, MatchID
		if l, found := t.names.distributed(stream, baseName(name), ts); found {
			nameLat, method = l, MatchName
		} else if !latOK {
			method = MatchNone
		}
		s.Matches[method]++
		if method != MatchNone {
			s.NameLatency.Observe(nameLat)
		}
		// EventLink writes lines out of order across rotated files, so the
		// "last" values follow the newest timestamp, not the read order.
		if ts >= s.LastDistributed {
			s.LastDistributed = ts
			s.LastDistributedFile = name
			if latOK {
				s.LastLatency = lat
				s.HasLatency = true
			}
			if method != MatchNone {
				s.NameLastLatency = nameLat
				s.HasNameLatency = true
			}
		}
	case bytes.Contains(line, []byte("Collected file ")):
		s := stats()
		s.CollectedFiles++
		s.CollectedBytes += firstUint(bytesRegex, line)
		name := collectedFileName(line)
		t.names.collected(stream, baseName(name), ts)
		if ts >= s.LastCollected {
			s.LastCollected = ts
			s.LastCollectedFile = name
		}
	case bytes.Contains(line, []byte(" records")) && bytes.Contains(line, []byte("Wrote ")):
		stats().RecordsOut += firstUint(wroteRecordsRegex, line)
	case bytes.Contains(line, []byte(" records")) && bytes.Contains(line, []byte("Read ")):
		stats().RecordsIn += firstUint(readRecordsRegex, line)
	case bytes.Contains(line, []byte("Scheduled execution id ")) && bytes.Contains(line, []byte(" finished")):
		s := stats()
		if ts >= s.LastExecution {
			s.LastExecution = ts
			s.LastExecutionOK = bytes.Contains(line, []byte("successfully"))
		}
	case (bytes.Contains(line, []byte("aborted")) || bytes.Contains(line, []byte("failed"))) && nodeFailedRegex.Match(line):
		s := stats()
		s.Failures++
		if ts > s.LastFailure {
			s.LastFailure = ts
		}
	}
}

// fileNameAfter returns the first field after marker, without directories.
func fileNameAfter(line []byte, marker string) string {
	i := bytes.Index(line, []byte(marker))
	if i < 0 {
		return ""
	}
	rest := bytes.TrimLeft(line[i+len(marker):], ": ")
	if f := bytes.Fields(rest); len(f) > 0 {
		return filepath.Base(string(f[0]))
	}
	return ""
}

// distributedFileName extracts NAME from "Distributed file NAME (…".
func distributedFileName(line []byte) string {
	return fileNameAfter(line, "Distributed file ")
}

// collectedFileName extracts NAME from "Collected file NAME (…". Some nodes
// log "Collected file -"; the name then comes from "File <source>:NAME".
func collectedFileName(line []byte) string {
	if n := fileNameAfter(line, "Collected file "); n != "" && n != "-" {
		return n
	}
	i := bytes.Index(line, []byte("] "))
	if i < 0 {
		return ""
	}
	n := fileNameAfter(bytes.TrimLeft(line[i+1:], " "), "File ")
	if i := strings.IndexByte(n, ':'); i >= 0 {
		n = n[i+1:]
	}
	if n == "-" {
		return ""
	}
	return n
}

// baseName is the part of a file name before the first ".":
// file_20261031101050.el.gz and file_20261031101050.el.mvne are the same file.
func baseName(name string) string {
	if i := strings.IndexByte(name, '.'); i >= 0 {
		return name[:i]
	}
	return name
}

// Snapshot copies the current state.
func (t *Tailer) Snapshot() *Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := &Snapshot{
		Nodes:        make(map[NodeKey]NodeStats, len(t.nodes)),
		StreamLastTS: make(map[string]float64, len(t.streamLastTS)),
		Pending:      t.names.pending(),
		TrackedNames: len(t.names.byKey),
		ReadBytes:    t.readBytes,
		Lines:        t.lines,
		FilesTracked: len(t.files),
		BudgetHits:   t.budgetHits,
		SeedBytes:    t.seedBytes,
		SeedFiles:    t.seedFiles,
	}
	for k, v := range t.nodes {
		c := *v
		c.Latency = v.Latency.clone()
		c.NameLatency = v.NameLatency.clone()
		s.Nodes[k] = c
	}
	for k, v := range t.streamLastTS {
		s.StreamLastTS[k] = v
	}
	return s
}
