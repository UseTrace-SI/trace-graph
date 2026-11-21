package graph

import (
	"os"
	"path/filepath"
	"testing"
)

// Git's index still lists sub/inner.go after sub is replaced by a symlink to a
// directory outside the checkout. The final-component Lstat followed that
// parent symlink, so indexing read and stored code from outside the repository.
func TestRegressionIndexRefusesSourcesBeyondSymlinkedDirectories(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "inner.go"), []byte("package p\nfunc OutsideSecret(){}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "other.go"), []byte("package p\nfunc OutsideUntracked(){}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		git  bool
	}{{"git checkout", true}, {"plain directory", false}} {
		t.Run(tc.name, func(t *testing.T) {
			var root string
			if tc.git {
				root = revisionTestRepo(t)
			} else {
				var err error
				if root, err = filepath.EvalSymlinks(t.TempDir()); err != nil {
					t.Fatal(err)
				}
			}
			revisionTestWrite(t, root, "sub/inner.go", "package p\nfunc Inside(){}\n")
			revisionTestWrite(t, root, "real.go", "package p\nfunc Real(){}\n")
			if tc.git {
				revisionTestCommit(t, root)
			}
			if err := os.RemoveAll(filepath.Join(root, "sub")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(root, "sub")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("real.go", filepath.Join(root, "alias.go")); err != nil {
				t.Fatal(err)
			}
			count := isolationIndex(t, root)
			for name, want := range map[string]int{"OutsideSecret": 0, "OutsideUntracked": 0, "Inside": 0, "Real": 1} {
				if got := count(name); got != want {
					t.Errorf("%s: %d symbols, want %d", name, got, want)
				}
			}
		})
	}
}
