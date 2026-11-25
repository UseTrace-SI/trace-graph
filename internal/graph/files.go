package graph

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// InspectableSource applies the same file policy as indexing. Read-only
// inspection commands use it so derived context never mentions excluded paths.
func InspectableSource(rel string) bool {
	return supportedFile(strings.ToLower(filepath.Ext(rel))) && !excludedSource(strings.ToLower(rel))
}

// cochangeConfigExts and cochangeConfigNames are configuration files whose
// history is worth pairing with sources even though they are not indexed.
var (
	cochangeConfigExts  = wordSetShared(".yaml .yml .json .toml .ini .cfg .conf .properties .xml .gradle .tf .tfvars .hcl .mk .cmake .bazel .bzl")
	cochangeConfigNames = wordSetShared("dockerfile containerfile makefile gnumakefile jenkinsfile procfile cmakelists.txt build workspace go.mod")
	cochangeLockfiles   = wordSetShared("package-lock.json npm-shrinkwrap.json yarn.lock pnpm-lock.yaml cargo.lock poetry.lock pipfile.lock composer.lock gemfile.lock go.sum")
)

// CochangeFile reports whether cochange pairs rel: indexable sources and
// common configuration files (YAML, JSON, TOML, Dockerfiles, Makefiles,
// Terraform, ...). The secret and generated-state exclusions of indexing
// apply, and lockfiles, which change with every manifest edit, are left out.
func CochangeFile(rel string) bool {
	lower := strings.ToLower(filepath.ToSlash(rel))
	if excludedSource(lower) {
		return false
	}
	base := pathBase(lower)
	if cochangeLockfiles[base] {
		return false
	}
	ext := filepath.Ext(base)
	return supportedFile(ext) || cochangeConfigExts[ext] || cochangeConfigNames[base] || strings.HasPrefix(base, "dockerfile.")
}

func pathBase(slashed string) string {
	if i := strings.LastIndexByte(slashed, '/'); i >= 0 {
		return slashed[i+1:]
	}
	return slashed
}

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

// buildManifests mark a directory whose dist/, build/ or target/ child is
// that tool's output. go.mod is not one: Go packages are often named build
// or dist (go/build, cmd/dist).
var buildManifests = []string{"package.json", "Cargo.toml", "pyproject.toml", "setup.py", "setup.cfg", "pom.xml", "build.gradle", "build.gradle.kts"}

// outputFilter reports build output, virtual environments and minified
// bundles of a directory walked without Git. A Git checkout leaves them to
// .gitignore, but a directory without Git has no ignore file, so walking it
// would index generated code. dist/, build/ and target/ are output only
// beside a build manifest, at the root or deeper; elsewhere directories of
// those names are often real packages (GOROOT/src/cmd/dist, go/build).
// .venv/ is skipped anywhere.
type outputFilter struct {
	root      string
	manifests map[string]bool
}

func (o *outputFilter) excluded(rel string, dir bool) bool {
	base := strings.ToLower(filepath.Base(rel))
	if !dir {
		return strings.HasSuffix(base, ".min.js")
	}
	switch base {
	case ".venv":
		return true
	case "dist", "build", "target":
		return o.hasManifest(filepath.Dir(rel))
	}
	return false
}

func (o *outputFilter) hasManifest(dir string) bool {
	if found, ok := o.manifests[dir]; ok {
		return found
	}
	found := false
	for _, name := range buildManifests {
		if info, err := os.Lstat(filepath.Join(o.root, dir, name)); err == nil && info.Mode().IsRegular() {
			found = true
			break
		}
	}
	if o.manifests == nil {
		o.manifests = map[string]bool{}
	}
	o.manifests[dir] = found
	return found
}

// sourceVisitor receives a repository-relative path and the source opened
// beneath the repository root. The walk closes the file after the visit.
type sourceVisitor func(rel string, file *os.File) error

// Git decides which source files belong to a checkout, matching Trace capture.
// Standalone source directories remain supported without requiring Git.
//
// Every read is anchored to the repository root. Git's index can still list
// dir/file.go after dir was replaced by a symlink leaving the checkout, and a
// final-component Lstat follows such a parent, so sources beneath a symlinked
// directory are skipped (Git itself treats them as deleted). The open cannot
// escape the root, does not block on a FIFO swapped in after the check, and
// must yield the same regular file that was checked.
func walkSourcesContext(ctx context.Context, root string, visit sourceVisitor) error {
	ignored, err := projectExclusions(root)
	if err != nil {
		return err
	}
	anchor, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer anchor.Close()
	var paths []string
	if _, err := os.Stat(filepath.Join(root, ".git")); err == nil {
		cmd := localGitCommand(ctx, root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
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
		output := &outputFilter{root: root}
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
			if rel != "." && (excludedSource(rel) || output.excluded(rel, entry.IsDir()) || ignored(rel)) {
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
	realDirs := map[string]bool{}
	var realParent func(dir string) (bool, error)
	realParent = func(dir string) (bool, error) {
		if dir == "." || dir == "" {
			return true, nil
		}
		if real, ok := realDirs[dir]; ok {
			return real, nil
		}
		real, err := realParent(filepath.Dir(dir))
		if err == nil && real {
			var info fs.FileInfo
			info, err = anchor.Lstat(dir)
			real = err == nil && info.IsDir()
			if os.IsNotExist(err) {
				err = nil
			}
		}
		if err != nil {
			return false, err
		}
		realDirs[dir] = real
		return real, nil
	}
	for _, rel := range paths {
		if rel == "" || excludedSource(rel) || ignored(rel) || !supportedFile(strings.ToLower(filepath.Ext(rel))) {
			continue
		}
		if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(filepath.ToSlash(rel), "../") {
			return fmt.Errorf("source path is outside repository")
		}
		if real, err := realParent(filepath.Dir(rel)); err != nil {
			return err
		} else if !real {
			continue
		}
		file, err := openRegularSource(anchor, rel)
		if err != nil {
			return err
		}
		if file == nil {
			continue
		}
		err = visit(rel, file)
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

// openRegularSource returns nil for a missing or non-regular path. A path that
// is atomically replaced between the check and the open (an editor save) is
// checked once more before the change is reported.
func openRegularSource(anchor *os.Root, rel string) (*os.File, error) {
	for attempt := 0; ; attempt++ {
		expected, err := anchor.Lstat(rel)
		if os.IsNotExist(err) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if !expected.Mode().IsRegular() {
			return nil, nil
		}
		file, err := anchor.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if os.IsNotExist(err) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		actual, err := file.Stat()
		if err == nil && actual.Mode().IsRegular() && os.SameFile(expected, actual) {
			return file, nil
		}
		file.Close()
		if err != nil {
			return nil, err
		}
		if attempt > 0 {
			return nil, fmt.Errorf("source changed while indexing: %s", filepath.ToSlash(rel))
		}
	}
}

type sourceListBuffer struct{ bytes.Buffer }

func (b *sourceListBuffer) Write(p []byte) (int, error) {
	if len(p) > 4*1024*1024-b.Len() {
		return 0, fmt.Errorf("repository path listing exceeds 4 MiB")
	}
	return b.Buffer.Write(p)
}
