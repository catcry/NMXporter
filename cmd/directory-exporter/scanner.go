package main

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"time"
)

// ScanResult is the raw output of a single directory scan.
type ScanResult struct {
	FileCount       int64
	OldestTimestamp float64
	NewestTimestamp float64
	ScanDuration    float64
	ScanSuccess     float64
	Truncated       bool  // true when MaxFilesPerDir cap, MaxStatFiles cap, or ctx cancel fired
	StatLimited     bool  // true when only the MaxStatFiles cap was hit (count is still exact)
	OldestCollected float64 // smallest collection epoch from EventLink file IDs in names; 0 when none
	Err             error
}

// collectedEpochFromName returns the collection epoch encoded in an EventLink
// buffer file name ("1769850107x001_001898_…" → 1769850107), or 0.
func collectedEpochFromName(name string) int64 {
	if len(name) < 11 || name[10] != 'x' {
		return 0
	}
	var n int64
	for i := 0; i < 10; i++ {
		c := name[i]
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int64(c-'0')
	}
	return n
}

// dirChunkSize controls how many directory entries are read per syscall.
// Small enough to avoid large allocations; large enough to amortise syscall overhead.
const dirChunkSize = 256

// scanDir reads a single directory, counts every regular file (and symlinks to regular files),
// ignores subdirectories (and symlinks to directories), and computes oldest/newest modification timestamps.
//
//   - ctx         — cancels the scan if the parent ScanAll times out.
//   - maxFiles    — cap on total regular files counted (0 = unlimited). When hit,
//     Truncated=true and the count reflects only what was read so far.
//   - maxStatFiles — cap on lstat calls for timestamp computation (0 = unlimited).
//     Files beyond this cap are counted but not stat-ed — timestamps come from
//     only the first maxStatFiles entries. This is the key tunable for large
//     directories: counting via getdents is ~10x faster than calling lstat per file.
func scanDir(ctx context.Context, path string, maxFiles, maxStatFiles int) ScanResult {
	start := time.Now()

	f, err := os.Open(path)
	if err != nil {
		return ScanResult{
			ScanDuration: time.Since(start).Seconds(),
			ScanSuccess:  0,
			Err:          err,
		}
	}
	defer f.Close()

	var count int64    // regular files seen
	var statsDone int  // stat calls made for timestamp computation
	oldest := int64(math.MaxInt64)
	newest := int64(math.MinInt64)
	oldestCollected := int64(math.MaxInt64)
	truncated := false
	statLimited := false

	for {
		if ctx.Err() != nil {
			truncated = true
			break
		}

		entries, readErr := f.ReadDir(dirChunkSize)

		for _, e := range entries {
			if ctx.Err() != nil {
				truncated = true
				break
			}

			isSymlink := e.Type()&os.ModeSymlink != 0
			var symlinkTargetInfo os.FileInfo
			if isSymlink {
				targetPath := filepath.Join(path, e.Name())
				targetInfo, statErr := os.Stat(targetPath)
				if statErr != nil {
					// Broken symlink or inaccessible target — skip
					continue
				}
				if targetInfo.IsDir() {
					// Symlink pointing to a directory — skip (not a file)
					continue
				}
				symlinkTargetInfo = targetInfo
			} else if e.IsDir() {
				// Regular directory — skip
				continue
			}

			// Count cap: stop counting (and statting) once hit.
			if maxFiles > 0 && count >= int64(maxFiles) {
				truncated = true
				break
			}
			count++

			// The file name carries the collection time for EventLink files;
			// this needs no syscall, so it covers every file.
			if ce := collectedEpochFromName(e.Name()); ce > 0 && ce < oldestCollected {
				oldestCollected = ce
			}

			// Stat cap: keep counting files but skip stat calls once exhausted.
			// Timestamps will reflect only the first maxStatFiles entries.
			if maxStatFiles > 0 && statsDone >= maxStatFiles {
				truncated = true
				statLimited = true
				continue // don't break — we still want the accurate count
			}

			var mt int64
			if isSymlink {
				mt = symlinkTargetInfo.ModTime().Unix()
			} else {
				info, statErr := e.Info()
				if statErr != nil {
					continue
				}
				mt = info.ModTime().Unix()
			}
			statsDone++
			if mt < oldest {
				oldest = mt
			}
			if mt > newest {
				newest = mt
			}
		}

		if truncated && maxFiles > 0 {
			// Count cap hit — no point reading more entries.
			break
		}
		if readErr != nil {
			break
		}
	}

	res := ScanResult{
		FileCount:    count,
		ScanDuration: time.Since(start).Seconds(),
		ScanSuccess:  1,
		Truncated:    truncated,
	}
	if statsDone > 0 {
		res.OldestTimestamp = float64(oldest)
		res.NewestTimestamp = float64(newest)
	}
	if oldestCollected != math.MaxInt64 {
		res.OldestCollected = float64(oldestCollected)
	}
	// Only the stat cap was hit when the count itself is complete.
	res.StatLimited = statLimited && ctx.Err() == nil && (maxFiles == 0 || count < int64(maxFiles))
	return res
}
