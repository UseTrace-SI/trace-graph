package graph

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Keep the documented .traceignore contract identical to CLI capture: relative
// paths, directory prefixes and single-level globs; no negation or ** patterns.
func projectExclusions(root string) (func(string) bool, error) {
	file := filepath.Join(root, ".traceignore")
	info, err := os.Lstat(file)
	if os.IsNotExist(err) {
		return func(string) bool { return false }, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 65536 {
		return nil, fmt.Errorf(".traceignore must be a regular file at most 64 KiB")
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	if len(b) > 65536 {
		return nil, fmt.Errorf(".traceignore exceeds 64 KiB")
	}
	var patterns []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "!") || strings.Contains(line, "**") {
			return nil, fmt.Errorf(".traceignore supports relative paths, directory prefixes and single-level globs; ! and ** are unsupported")
		}
		if _, err := path.Match(line, ""); err != nil {
			return nil, fmt.Errorf("invalid .traceignore pattern: %w", err)
		}
		patterns = append(patterns, strings.TrimPrefix(line, "/"))
	}
	return func(rel string) bool {
		for _, pattern := range patterns {
			if strings.HasSuffix(pattern, "/") && (rel == strings.TrimSuffix(pattern, "/") || strings.HasPrefix(rel, pattern)) {
				return true
			}
			if matched, _ := path.Match(pattern, rel); matched {
				return true
			}
			if !strings.Contains(pattern, "/") {
				for _, part := range strings.Split(rel, "/") {
					if matched, _ := path.Match(pattern, part); matched {
						return true
					}
				}
			}
		}
		return false
	}, nil
}
