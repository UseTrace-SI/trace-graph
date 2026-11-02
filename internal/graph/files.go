package graph

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func excludedSource(rel string) bool {
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		switch part {
		case ".git", ".trace", ".trace-brain", ".trace-graph", ".aws", ".ssh", "node_modules", "vendor":
			return true
		}
		if strings.HasPrefix(part, ".env") || strings.HasPrefix(part, "id_rsa") || strings.HasSuffix(part, ".pem") || strings.HasSuffix(part, ".key") {
			return true
		}
	}
	return false
}

// Git decides which source files belong to a checkout, matching Trace capture.
// Standalone source directories remain supported without requiring Git.
func walkSources(root string, visit fs.WalkDirFunc) error {
	return walkSourcesContext(context.Background(), root, visit)
}
func walkSourcesContext(ctx context.Context, root string, visit fs.WalkDirFunc) error {
	var paths []string
	if _, err := os.Stat(filepath.Join(root, ".git")); err == nil {
		cmd := exec.CommandContext(ctx, "git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
		var output sourceListBuffer
		cmd.Stdout = &output
		err := cmd.Run()
		out := output.Bytes()
		if err != nil {
			return fmt.Errorf("listing repository sources: %w", err)
		}
		paths = strings.Split(string(out), "\x00")
	} else if !os.IsNotExist(err) {
		return err
	} else {
		if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			if rel != "." && excludedSource(rel) {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.Type().IsRegular() {
				if len(paths) >= 100000 {
					return fmt.Errorf("repository exceeds 100000 source files")
				}
				paths = append(paths, rel)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	for _, rel := range paths {
		if rel == "" || excludedSource(rel) || !supportedFile(strings.ToLower(filepath.Ext(rel))) {
			continue
		}
		if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(filepath.ToSlash(rel), "../") {
			return fmt.Errorf("source path is outside repository")
		}
		path := filepath.Join(root, rel)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if err := visit(path, fs.FileInfoToDirEntry(info), nil); err != nil {
			return err
		}
	}
	return nil
}

type sourceListBuffer struct{ bytes.Buffer }

func (b *sourceListBuffer) Write(p []byte) (int, error) {
	if len(p) > 4*1024*1024-b.Len() {
		return 0, fmt.Errorf("repository path listing exceeds 4 MiB")
	}
	return b.Buffer.Write(p)
}
