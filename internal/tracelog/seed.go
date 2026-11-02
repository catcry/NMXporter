package tracelog

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Startup seed.
//
// Without backfill, counters start at the end of the trace files, so a quiet
// stream would show no last collected / distributed time until its next file.
// The seed reads the existing files once, newest first, and keeps only the
// newest "Collected file" and "Distributed file" time per node.

// seedNode is the newest collection and distribution seen for one node.
type seedNode struct {
	collected, distributed         float64
	collectedFile, distributedFile string
}

type seedResult struct {
	nodes  map[NodeKey]*seedNode
	lastTS map[string]float64
	bytes  uint64
	files  int
}

// seedLast reads the trace files of the last days days (today included),
// newest first per stream, until maxBytes per stream (0 = unlimited).
// Lines are out of order across rotations, so it keeps the maximum time.
func seedLast(ctx context.Context, dir string, entries []os.DirEntry, now time.Time, days int, maxBytes int64) *seedResult {
	minDate := now.AddDate(0, 0, -(days - 1)).Format("20060102")
	byStream := make(map[string][]traceFileRef)
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
		byStream[stream] = append(byStream[stream], traceFileRef{filepath.Join(dir, e.Name()), stream, date, seq, info.Size()})
	}

	res := &seedResult{nodes: make(map[NodeKey]*seedNode), lastTS: make(map[string]float64)}
	for stream, refs := range byStream {
		sort.Slice(refs, func(i, j int) bool {
			if refs[i].date != refs[j].date {
				return refs[i].date > refs[j].date
			}
			return refs[i].seq > refs[j].seq
		})
		var read int64
		for _, r := range refs {
			if ctx.Err() != nil {
				return res
			}
			if maxBytes > 0 && read >= maxBytes {
				break
			}
			read += res.readFile(stream, r.path, r.size)
			res.files++
		}
		res.bytes += uint64(read)
	}
	return res
}

func (res *seedResult) readFile(stream, path string, size int64) int64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	br := bufio.NewReaderSize(io.LimitReader(f, size), 256*1024)
	var n int64
	for {
		line, err := br.ReadSlice('\n')
		n += int64(len(line))
		if err == bufio.ErrBufferFull {
			for err == bufio.ErrBufferFull {
				line, err = br.ReadSlice('\n')
				n += int64(len(line))
			}
			continue
		}
		if err != nil {
			return n
		}
		res.parseLine(stream, bytes.TrimRight(line, "\r\n"))
	}
}

func (res *seedResult) parseLine(stream string, line []byte) {
	collected := bytes.Contains(line, []byte("Collected file "))
	distributed := !collected && bytes.Contains(line, []byte("Distributed file "))
	if !collected && !distributed {
		return
	}
	ts, ok := parseLineTime(line)
	if !ok {
		return
	}
	if ts > res.lastTS[stream] {
		res.lastTS[stream] = ts
	}
	node := lineNode(line)
	if node == "" {
		return
	}
	k := NodeKey{stream, node}
	s := res.nodes[k]
	if s == nil {
		s = &seedNode{}
		res.nodes[k] = s
	}
	if collected && ts >= s.collected {
		s.collected, s.collectedFile = ts, collectedFileName(line)
	}
	if distributed && ts >= s.distributed {
		s.distributed, s.distributedFile = ts, distributedFileName(line)
	}
}

// mergeSeed applies a seed result; newer values already read win. Called with
// t.mu held.
func (t *Tailer) mergeSeed(res *seedResult) {
	for k, sn := range res.nodes {
		s := t.nodes[k]
		if s == nil {
			s = &NodeStats{}
			t.nodes[k] = s
		}
		if sn.collected > s.LastCollected {
			s.LastCollected, s.LastCollectedFile = sn.collected, sn.collectedFile
		}
		if sn.distributed > s.LastDistributed {
			s.LastDistributed, s.LastDistributedFile = sn.distributed, sn.distributedFile
		}
	}
	for stream, ts := range res.lastTS {
		if ts > t.streamLastTS[stream] {
			t.streamLastTS[stream] = ts
		}
	}
	t.seedBytes += res.bytes
	t.seedFiles += res.files
}
