package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// WatchEntry pairs an absolute directory path with its pre-computed Prometheus labels.
type WatchEntry struct {
	AbsPath string
	Labels  DirLabels
}

// WatchList is a thread-safe, copy-on-write list of directories to monitor.
type WatchList struct {
	mu      sync.RWMutex
	entries []WatchEntry
}

func NewWatchList() *WatchList { return &WatchList{} }

func (wl *WatchList) Set(entries []WatchEntry) {
	wl.mu.Lock()
	wl.entries = entries
	wl.mu.Unlock()
}

func (wl *WatchList) Get() []WatchEntry {
	wl.mu.RLock()
	cp := make([]WatchEntry, len(wl.entries))
	copy(cp, wl.entries)
	wl.mu.RUnlock()
	return cp
}

func (wl *WatchList) Len() int {
	wl.mu.RLock()
	n := len(wl.entries)
	wl.mu.RUnlock()
	return n
}

// discoverTargets builds the full watch list from all configured targets.
// For each target:
//   - Dirs non-empty → use exactly those subdirs (relative to Base), no filesystem walk.
//   - Dirs empty     → auto-discover subdirectories up to MaxDepth via WalkDir.
//     If Pattern is set, only matching directories are kept.
func discoverTargets(targets []Target) ([]WatchEntry, error) {
	var all []WatchEntry
	var firstErr error
	for _, t := range targets {
		entries, err := discoverTarget(t)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		all = append(all, entries...)
	}
	return all, firstErr
}

func discoverTarget(t Target) ([]WatchEntry, error) {
	base := filepath.Clean(t.Base)
	label := t.Label
	if label == "" {
		label = base
	}
	if len(t.Dirs) > 0 {
		return resolveExplicitDirs(base, label, t.Dirs)
	}
	if t.Pattern != "" {
		testPat := strings.ReplaceAll(t.Pattern, "#include", "dummy")
		if _, err := filepath.Match(testPat, "test"); err != nil {
			return nil, fmt.Errorf("invalid pattern %q: %w", t.Pattern, err)
		}
	}
	maxDepth := t.MaxDepth
	if maxDepth == 0 {
		if t.Pattern != "" {
			cleanPat := strings.Trim(filepath.ToSlash(t.Pattern), "/")
			if cleanPat != "" {
				maxDepth = len(strings.Split(cleanPat, "/"))
			} else {
				maxDepth = 1
			}
		} else {
			maxDepth = 1
		}
	}
	return discoverDirectories(base, label, maxDepth, t.Pattern, t.Include)
}

// resolveExplicitDirs turns a list of relative dir paths into WatchEntries
// without touching the filesystem. Missing directories are caught at scan time
// via scrape_success=0.
func resolveExplicitDirs(base, label string, dirs []string) ([]WatchEntry, error) {
	var entries []WatchEntry
	seen := make(map[string]bool, len(dirs))
	for _, d := range dirs {
		rel := filepath.Clean(d)
		if seen[rel] {
			continue
		}
		seen[rel] = true

		absPath := filepath.Join(base, rel)
		parts := strings.Split(rel, string(os.PathSeparator))

		labels := DirLabels{Base: label}
		switch len(parts) {
		case 1:
			labels.Stream = parts[0]
		default:
			labels.Stream = parts[0]
			labels.Type = strings.Join(parts[1:], "/")
		}
		entries = append(entries, WatchEntry{AbsPath: absPath, Labels: labels})
	}
	return entries, nil
}

// validateTargets checks that every configured base path exists and is readable.
// Returns a multi-error string listing all failures; returns nil when all are OK.
func validateTargets(targets []Target) error {
	var errs []string
	for _, t := range targets {
		base := filepath.Clean(t.Base)
		info, err := os.Stat(base)
		if err != nil {
			errs = append(errs, fmt.Sprintf("  base %q: %v", base, err))
			continue
		}
		if !info.IsDir() {
			errs = append(errs, fmt.Sprintf("  base %q: not a directory", base))
			continue
		}
		f, err := os.Open(base)
		if err != nil {
			errs = append(errs, fmt.Sprintf("  base %q: cannot open: %v", base, err))
			continue
		}
		f.Close()

		// Validate explicit dirs exist too
		for _, d := range t.Dirs {
			abs := filepath.Join(base, filepath.Clean(d))
			if _, err := os.Stat(abs); err != nil {
				errs = append(errs, fmt.Sprintf("  target %q: %v", abs, err))
			}
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("target validation errors:\n%s", strings.Join(errs, "\n"))
}

// matchPattern checks if a relative directory path matches the pattern,
// expanding any #include segment using the provided include list.
func matchPattern(pattern, relPath string, include []string) (bool, error) {
	if pattern == "" {
		return true, nil
	}
	patParts := strings.Split(strings.Trim(filepath.ToSlash(pattern), "/"), "/")
	parts := strings.Split(strings.Trim(filepath.ToSlash(relPath), "/"), "/")
	if len(patParts) != len(parts) {
		return false, nil
	}

	for i := 0; i < len(patParts); i++ {
		if patParts[i] == "#include" {
			if !matchesInclude(parts[i], include) {
				return false, nil
			}
		} else {
			matched, err := filepath.Match(patParts[i], parts[i])
			if err != nil {
				return false, err
			}
			if !matched {
				return false, nil
			}
		}
	}
	return true, nil
}

// matchPatternPrefix checks if the current relative path matches the prefix of pattern.
// This allows pruning branches early during directory discovery.
func matchPatternPrefix(pattern, relPath string, include []string) bool {
	if pattern == "" {
		return true
	}
	patParts := strings.Split(strings.Trim(filepath.ToSlash(pattern), "/"), "/")
	parts := strings.Split(strings.Trim(filepath.ToSlash(relPath), "/"), "/")
	if len(parts) > len(patParts) {
		return false
	}

	for i := 0; i < len(parts); i++ {
		if patParts[i] == "#include" {
			if !matchesInclude(parts[i], include) {
				return false
			}
		} else {
			matched, err := filepath.Match(patParts[i], parts[i])
			if err != nil || !matched {
				return false
			}
		}
	}
	return true
}

func matchesInclude(name string, include []string) bool {
	if len(include) == 0 {
		return false
	}
	for _, inc := range include {
		if name == inc {
			return true
		}
		if matched, _ := filepath.Match(inc, name); matched {
			return true
		}
	}
	return false
}

// discoverDirectories walks basePath up to maxDepth and returns one WatchEntry
// per subdirectory. This is used for auto-discovery when Dirs is not specified.
//
// Symbolic links to directories are followed and traversed up to maxDepth.
// Symlink loops / cycles are detected and skipped to prevent infinite recursion.
//
// If pattern is non-empty, only directories whose relative path matches the glob
// pattern are added to the watch list. A * in the pattern matches one path segment.
// The pattern may include #include to match only directories listed in the include slice.
//
// Label derivation:
//
//	depth 1: stream=<dir>,        type=""
//	depth 2: stream=<dir>,        type=<subdir>
//	depth N: stream=<first-part>, type=<rest joined with "/">
func discoverDirectories(basePath, label string, maxDepth int, pattern string, include []string) ([]WatchEntry, error) {
	basePath = filepath.Clean(basePath)
	var entries []WatchEntry
	var firstErr error

	info, err := os.Stat(basePath)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("base path %q is not a directory", basePath)
	}

	realBasePath, err := filepath.EvalSymlinks(basePath)
	if err != nil {
		realBasePath = basePath
	}

	ancestors := make(map[string]bool)
	ancestors[realBasePath] = true

	var walk func(currentPath, relPath string, depth int)
	walk = func(currentPath, relPath string, depth int) {
		if depth > maxDepth {
			return
		}

		if depth >= 1 {
			matched, matchErr := matchPattern(pattern, relPath, include)
			if matchErr != nil {
				if firstErr == nil {
					firstErr = matchErr
				}
				return
			}

			if matched {
				parts := strings.Split(relPath, string(os.PathSeparator))
				labels := DirLabels{Base: label}
				labels.Stream = parts[0]
				if depth > 1 {
					if pattern != "" {
						patParts := strings.Split(strings.Trim(filepath.ToSlash(pattern), "/"), "/")
						if len(patParts) == len(parts) {
							var typeParts []string
							for i := 1; i < len(parts); i++ {
								if strings.ContainsAny(patParts[i], "*?[") || patParts[i] == "#include" {
									typeParts = append(typeParts, parts[i])
								}
							}
							labels.Type = strings.Join(typeParts, "/")
						} else {
							labels.Type = strings.Join(parts[1:], "/")
						}
					} else {
						labels.Type = strings.Join(parts[1:], "/")
					}
				}

				entries = append(entries, WatchEntry{AbsPath: currentPath, Labels: labels})
			}
		}

		if depth < maxDepth {
			dirEntries, readErr := os.ReadDir(currentPath)
			if readErr != nil {
				if firstErr == nil {
					firstErr = readErr
				}
				return
			}

			for _, d := range dirEntries {
				childPath := filepath.Join(currentPath, d.Name())
				childRel := d.Name()
				if relPath != "" {
					childRel = filepath.Join(relPath, d.Name())
				}

				isDir := d.IsDir()
				if !isDir && d.Type()&os.ModeSymlink != 0 {
					if statInfo, statErr := os.Stat(childPath); statErr == nil {
						isDir = statInfo.IsDir()
					}
				}

				if !isDir {
					continue
				}

				// Early branch pruning
				if !matchPatternPrefix(pattern, childRel, include) {
					continue
				}

				realChildPath, evalErr := filepath.EvalSymlinks(childPath)
				if evalErr == nil {
					if ancestors[realChildPath] {
						// Cycle detected, skip descending into this directory
						continue
					}
					ancestors[realChildPath] = true
				}

				walk(childPath, childRel, depth+1)

				if evalErr == nil {
					delete(ancestors, realChildPath)
				}
			}
		}
	}

	walk(basePath, "", 0)

	return entries, firstErr
}
