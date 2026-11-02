package main

import (
	"os"
	"syscall"
)

// dirStamp identifies the state of a directory's entry list. Creating,
// removing or renaming a file in it changes mtime and ctime (nanoseconds on
// XFS) and usually the size, so an equal stamp means the same set of files.
type dirStamp struct {
	ino, size, mtimeNs, ctimeNs int64
}

func stampOf(fi os.FileInfo) (dirStamp, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return dirStamp{}, false
	}
	return dirStamp{
		ino:     int64(st.Ino),
		size:    st.Size,
		mtimeNs: st.Mtim.Sec*1e9 + st.Mtim.Nsec,
		ctimeNs: st.Ctim.Sec*1e9 + st.Ctim.Nsec,
	}, true
}
