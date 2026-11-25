//go:build !linux && !darwin && !freebsd && !netbsd && !dragonfly

package graph

import "io/fs"

// fileChangeTime is unavailable here, so every refresh hashes every source.
func fileChangeTime(fs.FileInfo) (ctime int64, dev, ino uint64, ok bool) {
	return 0, 0, 0, false
}
