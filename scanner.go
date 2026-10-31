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
	Err             error
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
	truncated := false

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

			// Stat cap: keep counting files but skip stat calls once exhausted.
			// Timestamps will reflect only the first maxStatFiles entries.
			if maxStatFiles > 0 && statsDone >= maxStatFiles {
				truncated = true
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
	return res
}
