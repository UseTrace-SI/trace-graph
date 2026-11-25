//go:build darwin || freebsd || netbsd || dragonfly

package graph

import (
	"io/fs"
	"syscall"
)

// fileChangeTime returns the inode change time, device and inode number.
func fileChangeTime(info fs.FileInfo) (ctime int64, dev, ino uint64, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, 0, false
	}
	return st.Ctimespec.Nano(), uint64(st.Dev), uint64(st.Ino), true
}
