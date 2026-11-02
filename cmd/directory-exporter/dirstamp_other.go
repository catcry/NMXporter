//go:build !linux

package main

import "os"

// dirStamp identifies the state of a directory's entry list (see
// dirstamp_linux.go). Elsewhere only mtime and size are available.
type dirStamp struct {
	size, mtimeNs int64
}

func stampOf(fi os.FileInfo) (dirStamp, bool) {
	return dirStamp{size: fi.Size(), mtimeNs: fi.ModTime().UnixNano()}, true
}
