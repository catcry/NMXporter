package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/catcry/NMXporter/internal/tracelog"
)

// NodeActivity holds activity metrics and delay for a single stream node (collector or distributor).
type NodeActivity struct {
	Base                  string
	Stream                string
	Node                  string
	NodeType              string  // "collector" or "distributor"
	LastActivityTimestamp float64 // Unix epoch seconds
	DelaySec              float64 // (now - timestamp) or 0 if <= minDelay
	LastFile              string
	TargetDirMtime        float64 // distributor TargetDirectory mtime; 0 when unknown
	Success               bool
}

// StreamNode represents a discovered node inside a stream directory.
type StreamNode struct {
	Base      string // target label or base path
	Stream    string
	Node      string
	NodeDir   string
	NodeType  string // "collector", "distributor", or empty
	RawType   string // NodeType exactly as configured, lower-cased ("" when unknown)
	TargetDir string // distributor TargetDirectory ("" when not set)
}

// ScanCollectorActivity inspects a collector node directory for storage/1/audit_info,
// extracting the last processed filename and epoch timestamp.
func ScanCollectorActivity(baseLabel, stream, node, nodeDir string, minDelay time.Duration, now time.Time) (NodeActivity, error) {
	auditPath := filepath.Join(nodeDir, "storage", "1", "audit_info")
	data, err := os.ReadFile(auditPath)
	if err != nil {
		return NodeActivity{
			Base:     baseLabel,
			Stream:   stream,
			Node:     node,
			NodeType: "collector",
			LastFile: "-",
			Success:  false,
		}, err
	}

	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return NodeActivity{
			Base:     baseLabel,
			Stream:   stream,
			Node:     node,
			NodeType: "collector",
			LastFile: "-",
			Success:  false,
		}, fmt.Errorf("malformed audit_info: expected at least 2 fields, got %d", len(fields))
	}

	lastFile := fields[0]
	epoch, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || epoch <= 0 {
		return NodeActivity{
			Base:     baseLabel,
			Stream:   stream,
			Node:     node,
			NodeType: "collector",
			LastFile: lastFile,
			Success:  false,
		}, fmt.Errorf("invalid epoch timestamp in audit_info: %q", fields[1])
	}

	delay := now.Unix() - epoch
	if delay < 0 {
		delay = 0
	}
	if minDelay > 0 && delay <= int64(minDelay.Seconds()) {
		delay = 0
	}

	return NodeActivity{
		Base:                  baseLabel,
		Stream:                stream,
		Node:                  node,
		NodeType:              "collector",
		LastActivityTimestamp: float64(epoch),
		DelaySec:              float64(delay),
		LastFile:              lastFile,
		Success:               true,
	}, nil
}

// parseDistributorLine checks if a log line represents a distribution event for the given node
// and extracts the timestamp and filename.
func parseDistributorLine(line, node string, loc *time.Location) (time.Time, string, bool) {
	if !strings.Contains(line, "["+node+"]") && !strings.Contains(line, "["+node+".") {
		return time.Time{}, "", false
	}
	idx := strings.Index(line, "Distributed file")
	if idx == -1 {
		return time.Time{}, "", false
	}

	// Extract filename after "Distributed file"
	rest := strings.TrimSpace(line[idx+len("Distributed file"):])
	if strings.HasPrefix(rest, ":") {
		rest = strings.TrimSpace(rest[1:])
	}
	fileFields := strings.Fields(rest)
	if len(fileFields) == 0 {
		return time.Time{}, "", false
	}
	lastFile := filepath.Base(fileFields[0])

	// Extract timestamp from line prefix (fields 0 and 1: date and time)
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return time.Time{}, "", false
	}

	dateStr := fields[0]
	timeStr := fields[1]

	// Strip subseconds if present (e.g. 103000.123 -> 103000, 10:30:00.123 -> 10:30:00)
	if dotIdx := strings.Index(timeStr, "."); dotIdx != -1 {
		timeStr = timeStr[:dotIdx]
	}

	// If time is 6 digits without colons (e.g. 103000), format as 10:30:00
	if len(timeStr) == 6 && !strings.Contains(timeStr, ":") {
		timeStr = timeStr[0:2] + ":" + timeStr[2:4] + ":" + timeStr[4:6]
	}

	if loc == nil {
		loc = time.Local
	}

	layouts := []string{
		"20060102 15:04:05",
		"2006-01-02 15:04:05",
		"2006/01/02 15:04:05",
		"20060102 150405",
	}

	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, dateStr+" "+timeStr, loc); err == nil {
			return t, lastFile, true
		}
	}

	return time.Time{}, "", false
}

// ScanDistributorActivity searches trace logs for the latest distribution entry for a node.
// Searches day by day backwards from now up to lookbackDays.
func ScanDistributorActivity(tracelogDir, baseLabel, stream, node string, lookbackDays int, minDelay time.Duration, now time.Time) (NodeActivity, error) {
	if tracelogDir == "" || lookbackDays <= 0 {
		return NodeActivity{
			Base:     baseLabel,
			Stream:   stream,
			Node:     node,
			NodeType: "distributor",
			LastFile: "-",
			Success:  false,
		}, nil
	}

	var latestTime time.Time
	var latestFile string
	found := false

	loc := now.Location()

	for d := 0; d < lookbackDays; d++ {
		dateStr := now.AddDate(0, 0, -d).Format("20060102")
		pattern := filepath.Join(tracelogDir, fmt.Sprintf("execution_trace_%s_%s_*", stream, dateStr))
		matches, err := filepath.Glob(pattern)
		if err != nil || len(matches) == 0 {
			continue
		}

		// Newest trace file first. EventLink rotates to _0, _1, … _101, so the
		// sequence number must be compared numerically: a plain string sort
		// puts _99 before _101 and _9 before _89.
		sortTraceFilesNewestFirst(matches)

		for _, tracePath := range matches {
			f, err := os.Open(tracePath)
			if err != nil {
				continue
			}

			scanner := bufio.NewScanner(f)
			for scanner.Scan() {
				line := scanner.Text()
				if t, file, ok := parseDistributorLine(line, node, loc); ok {
					// Lines are chronological and timestamps have 1s
					// resolution, so a later line in the same second wins.
					if !found || !t.Before(latestTime) {
						latestTime = t
						latestFile = file
						found = true
					}
				}
			}
			f.Close()

			if found {
				break
			}
		}

		if found {
			// Found the latest distribution on day d — stop looking further back.
			break
		}
	}

	if !found {
		return NodeActivity{
			Base:     baseLabel,
			Stream:   stream,
			Node:     node,
			NodeType: "distributor",
			LastFile: "-",
			Success:  false,
		}, nil
	}

	epoch := latestTime.Unix()
	delay := now.Unix() - epoch
	if delay < 0 {
		delay = 0
	}
	if minDelay > 0 && delay <= int64(minDelay.Seconds()) {
		delay = 0
	}

	return NodeActivity{
		Base:                  baseLabel,
		Stream:                stream,
		Node:                  node,
		NodeType:              "distributor",
		LastActivityTimestamp: float64(epoch),
		DelaySec:              float64(delay),
		LastFile:              latestFile,
		Success:               true,
	}, nil
}

// streamTraceSignatures returns, per stream, a string that changes whenever
// the stream's newest trace file grows or a new one appears. One directory
// read per scan cycle.
func streamTraceSignatures(dir string) map[string]string {
	sigs := make(map[string]string)
	if dir == "" {
		return sigs
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return sigs
	}
	type newest struct {
		date string
		seq  int
		name string
	}
	best := make(map[string]newest)
	for _, e := range entries {
		stream, date, seq, ok := tracelog.TraceName(e.Name())
		if !ok {
			continue
		}
		b, seen := best[stream]
		if !seen || date > b.date || date == b.date && seq > b.seq {
			best[stream] = newest{date, seq, e.Name()}
		}
	}
	for stream, b := range best {
		info, err := os.Stat(filepath.Join(dir, b.name))
		if err != nil {
			continue
		}
		sigs[stream] = fmt.Sprintf("%s/%d/%d", b.name, info.Size(), info.ModTime().UnixNano())
	}
	return sigs
}

type distMemoEntry struct {
	sig string
	act NodeActivity
}

// distMemo caches each distributor's trace-log result until its stream's
// trace logs change, so idle streams are not re-read every cycle.
var distMemo = struct {
	sync.Mutex
	m map[string]distMemoEntry
}{m: make(map[string]distMemoEntry)}

func scanDistributorMemo(sig, tracelogDir string, sn StreamNode, lookbackDays int, minDelay time.Duration, now time.Time) (NodeActivity, error) {
	key := tracelogDir + "|" + sn.Base + "/" + sn.Stream + "/" + sn.Node
	if sig != "" {
		distMemo.Lock()
		e, ok := distMemo.m[key]
		distMemo.Unlock()
		if ok && e.sig == sig {
			act := e.act
			if act.Success {
				act.DelaySec = delaySince(int64(act.LastActivityTimestamp), minDelay, now)
			}
			return act, nil
		}
	}
	act, err := ScanDistributorActivity(tracelogDir, sn.Base, sn.Stream, sn.Node, lookbackDays, minDelay, now)
	if err == nil && sig != "" {
		distMemo.Lock()
		distMemo.m[key] = distMemoEntry{sig, act}
		distMemo.Unlock()
	}
	return act, err
}

// traceSeq returns the rotation sequence number of a trace file
// (execution_trace_<stream>_<date>_<seq>.log), or -1 when there is none.
func traceSeq(path string) int {
	name := strings.TrimSuffix(filepath.Base(path), ".log")
	i := strings.LastIndex(name, "_")
	if i < 0 {
		return -1
	}
	n, err := strconv.Atoi(name[i+1:])
	if err != nil {
		return -1
	}
	return n
}

// sortTraceFilesNewestFirst orders trace files of one stream and day by
// descending rotation sequence number.
func sortTraceFilesNewestFirst(paths []string) {
	sort.SliceStable(paths, func(i, j int) bool {
		si, sj := traceSeq(paths[i]), traceSeq(paths[j])
		if si != sj {
			return si > sj
		}
		return paths[i] > paths[j]
	})
}

// applyTargetDirActivity raises a distributor's last activity to the mtime of
// its local TargetDirectory when that is newer. A directory's mtime changes
// whenever a file is added or removed, so it keeps working when EventLink
// stops writing trace lines (seen under heavy load). Remote targets (no local
// directory) are left unchanged.
func applyTargetDirActivity(act NodeActivity, targetDir string, minDelay time.Duration, now time.Time) NodeActivity {
	if targetDir == "" {
		return act
	}
	info, err := os.Stat(targetDir)
	if err != nil || !info.IsDir() {
		return act
	}
	mtime := info.ModTime().Unix()
	act.TargetDirMtime = float64(mtime)
	if act.Success && float64(mtime) <= act.LastActivityTimestamp {
		return act
	}
	if !act.Success && act.LastFile == "" {
		act.LastFile = "-"
	}
	act.LastActivityTimestamp = float64(mtime)
	act.DelaySec = delaySince(mtime, minDelay, now)
	act.Success = true
	return act
}

// delaySince returns now-epoch in seconds, or 0 when it is <= minDelay.
func delaySince(epoch int64, minDelay time.Duration, now time.Time) float64 {
	delay := now.Unix() - epoch
	if delay < 0 {
		delay = 0
	}
	if minDelay > 0 && delay <= int64(minDelay.Seconds()) {
		delay = 0
	}
	return float64(delay)
}

var nodeTypeRegex = regexp.MustCompile(`NodeType\s+"([^"]+)"`)

// detectNodeType attempts to read control/1/config or deduce the node type.
func detectNodeType(nodeDir, nodeName string) string {
	configPath := filepath.Join(nodeDir, "control", "1", "config")
	if data, err := os.ReadFile(configPath); err == nil {
		if m := nodeTypeRegex.FindStringSubmatch(string(data)); len(m) > 1 {
			t := strings.ToLower(m[1])
			if strings.Contains(t, "collector") || strings.Contains(t, "col") {
				return "collector"
			}
			if strings.Contains(t, "distributor") || strings.Contains(t, "dis") {
				return "distributor"
			}
		}
	}

	// Fallback to storage/1/audit_info presence
	auditPath := filepath.Join(nodeDir, "storage", "1", "audit_info")
	if _, err := os.Stat(auditPath); err == nil {
		return "collector"
	}

	lower := strings.ToLower(nodeName)
	if strings.Contains(lower, "col") {
		return "collector"
	}
	if strings.Contains(lower, "dis") {
		return "distributor"
	}

	return ""
}

// DiscoverStreamNodes discovers stream node directories under configured targets.
func DiscoverStreamNodes(targets []Target) []StreamNode {
	var nodes []StreamNode
	seen := make(map[string]bool)

	for _, t := range targets {
		base := filepath.Clean(t.Base)
		baseLabel := t.Label
		if baseLabel == "" {
			baseLabel = base
		}

		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}

		for _, streamEntry := range entries {
			if !streamEntry.IsDir() {
				continue
			}
			streamName := streamEntry.Name()
			nodesDir := filepath.Join(base, streamName, "nodes")
			info, err := os.Stat(nodesDir)
			if err != nil || !info.IsDir() {
				continue
			}

			nodeEntries, err := os.ReadDir(nodesDir)
			if err != nil {
				continue
			}

			for _, ne := range nodeEntries {
				if !ne.IsDir() {
					continue
				}
				nodeName := ne.Name()
				if strings.HasPrefix(nodeName, "mail") || strings.HasPrefix(nodeName, "rap_import") {
					continue
				}

				nodeDir := filepath.Join(nodesDir, nodeName)
				key := baseLabel + "/" + streamName + "/" + nodeName
				if seen[key] {
					continue
				}
				seen[key] = true

				nodeType := detectNodeType(nodeDir, nodeName)
				nc, _ := readNodeConfig(nodeDir)
				nodes = append(nodes, StreamNode{
					Base:      baseLabel,
					Stream:    streamName,
					Node:      nodeName,
					NodeDir:   nodeDir,
					NodeType:  nodeType,
					RawType:   nc.NodeType,
					TargetDir: nc.TargetDir,
				})
			}
		}
	}

	return nodes
}

// ScanAllNodeActivities scans all discovered nodes for collector and distributor activities.
func ScanAllNodeActivities(ctx context.Context, nodes []StreamNode, tracelogDir string, minDelay time.Duration, lookbackDays int, workers int, now time.Time) map[string]NodeActivity {
	if len(nodes) == 0 {
		return make(map[string]NodeActivity)
	}

	if workers <= 0 {
		workers = 2
	}

	results := make(map[string]NodeActivity, len(nodes))
	var mu sync.Mutex
	sigs := streamTraceSignatures(tracelogDir)

	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup

	for _, n := range nodes {
		if ctx.Err() != nil {
			break
		}

		wg.Add(1)
		sem <- struct{}{}

		go func(sn StreamNode) {
			defer wg.Done()
			defer func() { <-sem }()

			var act NodeActivity
			var err error

			switch sn.NodeType {
			case "collector":
				act, err = ScanCollectorActivity(sn.Base, sn.Stream, sn.Node, sn.NodeDir, minDelay, now)
			case "distributor":
				act, err = scanDistributorMemo(sigs[sn.Stream], tracelogDir, sn, lookbackDays, minDelay, now)
				if err == nil {
					act = applyTargetDirActivity(act, sn.TargetDir, minDelay, now)
				}
			default:
				// If unspecified, test for audit_info first
				auditPath := filepath.Join(sn.NodeDir, "storage", "1", "audit_info")
				if _, statErr := os.Stat(auditPath); statErr == nil {
					act, err = ScanCollectorActivity(sn.Base, sn.Stream, sn.Node, sn.NodeDir, minDelay, now)
				} else if tracelogDir != "" {
					act, err = ScanDistributorActivity(tracelogDir, sn.Base, sn.Stream, sn.Node, lookbackDays, minDelay, now)
				}
			}

			if err == nil && act.Success {
				key := act.Base + "/" + act.Stream + "/" + act.Node + "/" + act.NodeType
				mu.Lock()
				results[key] = act
				mu.Unlock()
			}
		}(n)
	}

	wg.Wait()
	return results
}
