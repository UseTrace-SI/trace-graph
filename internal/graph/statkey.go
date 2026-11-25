package graph

import (
	"fmt"
	"io/fs"
	"time"
)

// racyWindow is how close to the start of an index a file may have been
// changed and still have its stat key trusted. Filesystems with coarse
// timestamps can record a second same-size write within one tick with
// identical times, so such files are hashed again by the next refresh.
const racyWindow = 2 * time.Second

// statKey identifies a file's content cheaply: size, modification time,
// inode change time, device and inode. Writes update the change time even
// when a tool restores the modification time, and an atomic replace changes
// the inode, so a matching key means the content hash would match too. It
// is empty when the platform has no change time or the file changed too
// close to start to be trusted.
func statKey(info fs.FileInfo, start time.Time) string {
	ctime, dev, ino, ok := fileChangeTime(info)
	if !ok {
		return ""
	}
	mtime := info.ModTime().UnixNano()
	limit := start.Add(-racyWindow).UnixNano()
	if mtime >= limit || ctime >= limit {
		return ""
	}
	return fmt.Sprintf("%d:%d:%d:%d:%d", info.Size(), mtime, ctime, dev, ino)
}
