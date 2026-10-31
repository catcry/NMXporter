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
	Success               bool
}

// StreamNode represents a discovered node inside a stream directory.
type StreamNode struct {
	Base     string // target label or base path
	Stream   string
	Node     string
	NodeDir  string
	NodeType string // "collector", "distributor", or empty
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

		// Sort trace files in reverse so we scan the newest trace files first
		sort.Sort(sort.Reverse(sort.StringSlice(matches)))

		for _, tracePath := range matches {
			f, err := os.Open(tracePath)
			if err != nil {
				continue
			}

			scanner := bufio.NewScanner(f)
			for scanner.Scan() {
				line := scanner.Text()
				if t, file, ok := parseDistributorLine(line, node, loc); ok {
					if !found || t.After(latestTime) {
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
				nodes = append(nodes, StreamNode{
					Base:     baseLabel,
					Stream:   streamName,
					Node:     nodeName,
					NodeDir:  nodeDir,
					NodeType: nodeType,
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
				act, err = ScanDistributorActivity(tracelogDir, sn.Base, sn.Stream, sn.Node, lookbackDays, minDelay, now)
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
