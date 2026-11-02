package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// NodeConfig holds the fields of ${nodeDir}/control/1/config that the
// exporter uses. Values are taken verbatim from the quoted config values.
type NodeConfig struct {
	NodeType      string   // raw NodeType, lower-cased (e.g. "collector", "collector,decoder", "blt")
	SourceDirs    []string // collector SourceDirectory entries
	TargetDir     string   // distributor TargetDirectory ("" when not set)
	FilenameMasks []string // collector Filename masks (glob), empty = all files
}

// readNodeConfig parses ${nodeDir}/control/1/config. A missing or unreadable
// config returns the zero NodeConfig and ok=false.
func readNodeConfig(nodeDir string) (NodeConfig, bool) {
	data, err := os.ReadFile(filepath.Join(nodeDir, "control", "1", "config"))
	if err != nil {
		return NodeConfig{}, false
	}
	var nc NodeConfig
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key := line
		if i := strings.IndexAny(line, " \t:"); i > 0 {
			key = line[:i]
		}
		switch key {
		case "NodeType":
			if m := quotedValueRegex.FindStringSubmatch(line); len(m) > 1 {
				nc.NodeType = strings.ToLower(strings.TrimSpace(m[1]))
			}
		case "SourceDirectory":
			if dir := extractSourceDirectory(line); dir != "" {
				nc.SourceDirs = append(nc.SourceDirs, dir)
			}
		case "TargetDirectory":
			if m := quotedValueRegex.FindStringSubmatch(line); len(m) > 1 {
				nc.TargetDir = strings.TrimSpace(m[1])
			}
		case "Filename":
			if m := quotedValueRegex.FindStringSubmatch(line); len(m) > 1 {
				for _, mask := range strings.FieldsFunc(m[1], func(r rune) bool { return r == ',' || r == ' ' }) {
					nc.FilenameMasks = append(nc.FilenameMasks, mask)
				}
			}
		}
	}
	return nc, true
}

// NodeStatus describes liveness and identity of one stream node, read from
// control/1/{config,heartbeat,info}.
//
// EventLink writes a *deadline* into control/1/heartbeat: the epoch by which
// the node promises to write its next heartbeat (now + interval). A running
// node keeps pushing it into the future; a stopped node has no heartbeat file
// or a deadline in the past.
type NodeStatus struct {
	Base              string
	Stream            string
	Node              string
	NodeType          string  // raw NodeType from control/1/config ("" when unknown)
	HeartbeatDeadline float64 // epoch from control/1/heartbeat; 0 when missing
	State             string  // e.g. IDLE, TIMER (from control/1/info); "" when missing
	StateSince        float64 // epoch from control/1/info; 0 when missing
	Up                bool    // heartbeat present and deadline + grace not passed
}

// readEpochFile returns the first whitespace-separated field of path as an
// epoch, plus the rest of the line.
func readEpochFile(path string) (int64, string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, "", false
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, "", false
	}
	epoch, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil || epoch <= 0 {
		return 0, "", false
	}
	return epoch, strings.Join(fields[1:], " "), true
}

// ScanNodeStatus reads the status files of one node.
func ScanNodeStatus(sn StreamNode, grace time.Duration, now time.Time) NodeStatus {
	st := NodeStatus{Base: sn.Base, Stream: sn.Stream, Node: sn.Node, NodeType: sn.RawType}
	ctl := filepath.Join(sn.NodeDir, "control", "1")

	if deadline, _, ok := readEpochFile(filepath.Join(ctl, "heartbeat")); ok {
		st.HeartbeatDeadline = float64(deadline)
		st.Up = now.Unix() <= deadline+int64(grace.Seconds())
	}
	if since, state, ok := readEpochFile(filepath.Join(ctl, "info")); ok {
		st.StateSince = float64(since)
		st.State = strings.TrimSpace(state)
	}
	return st
}

// ScanAllNodeStatus reads status for every discovered node. It only reads
// three small files per node, so it runs sequentially.
func ScanAllNodeStatus(nodes []StreamNode, grace time.Duration, now time.Time) []NodeStatus {
	out := make([]NodeStatus, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, ScanNodeStatus(n, grace, now))
	}
	return out
}
