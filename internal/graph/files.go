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
	"unicode"
)

// cochangeConfigExts and cochangeConfigNames are configuration files whose
// history is worth pairing with sources even though they are not indexed.
// Lockfiles change with every manifest edit and are left out (other
// lockfiles, such as *.lock and go.sum, have no allowed extension).
var (
	cochangeConfigExts  = wordSetShared(".yaml .yml .json .toml .ini .cfg .conf .properties .xml .gradle .tf .hcl .mk .cmake .bazel .bzl")
	cochangeConfigNames = wordSetShared("dockerfile containerfile makefile gnumakefile jenkinsfile procfile cmakelists.txt build workspace go.mod")
	cochangeLockfiles   = wordSetShared("package-lock.json npm-shrinkwrap.json pnpm-lock.yaml packages.lock.json conda-lock.yml conda-lock.yaml .terraform.lock.hcl")
	// Agents' hook and MCP configuration, which the Trace CLI never
	// captures or restores (internal/manifest protectedFolded). Like the
	// CLI, these match relative to the root only.
	agentConfigFiles = wordSetShared(".mcp.json .claude/settings.local.json .cursor/hooks.json .gemini/settings.json .agents/hooks.json")
)

// agentConfig reports agent configuration the Trace CLI protects, for a
// path relative to the root it would capture.
func agentConfig(rel string) bool {
	return agentConfigFiles[foldPath(filepath.ToSlash(rel))]
}

// CochangeFile reports whether cochange pairs rel: indexable sources and
// common configuration files (YAML, JSON, TOML, Dockerfiles, Makefiles,
// Terraform, ...). The secret and generated-state exclusions of indexing
// and the agent configuration the Trace CLI never captures apply.
// CochangeFilter also applies the project's .traceignore.
func CochangeFile(rel string) bool {
	if excludedSource(rel) || agentConfig(rel) {
		return false
	}
	base := pathBase(foldPath(rel))
	if cochangeLockfiles[base] {
		return false
	}
	ext := filepath.Ext(base)
	return supportedFile(ext) || cochangeConfigExts[ext] || cochangeConfigNames[base] || strings.HasPrefix(base, "dockerfile.")
}

// CochangeFilter returns CochangeFile restricted by the repository's
// .traceignore, the policy capture and indexing honor.
func CochangeFilter(root string) (func(string) bool, error) {
	ignored, err := projectExclusions(root)
	if err != nil {
		return nil, err
	}
	return func(rel string) bool {
		return CochangeFile(rel) && !ignored(filepath.ToSlash(rel))
	}, nil
}

// CochangeScope maps the paths git log prints for a checkout (relative to
// the Git working-tree root) to paths relative to repo, which may be a
// subdirectory, and reports whether cochange pairs them: the path must lie
// under repo, pass CochangeFile relative to repo, and neither the
// working-tree root's .traceignore nor, for a subdirectory, repo's own
// .traceignore (the one indexing reads) may exclude it. Agent configuration
// at the working-tree root is excluded too.
//
// The built-in exclusions apply to the path inside repo, as for indexing:
// `analyze --repo vendor/lib` indexes that directory although vendor/ is
// excluded beneath a root, so cochange pairs its files as well.
func CochangeScope(ctx context.Context, repo string) (func(gitPath string) (string, bool), error) {
	out, err := revisionOutput(ctx, repo, 8192, "rev-parse", "--show-toplevel", "--show-prefix")
	if err != nil {
		return nil, fmt.Errorf("locating the Git working tree: %w", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	top, prefix := lines[0], ""
	if len(lines) > 1 {
		prefix = filepath.ToSlash(lines[1])
	}
	topIgnored, err := projectExclusions(top)
	if err != nil {
		return nil, err
	}
	subPolicy := func(string) bool { return false }
	if prefix != "" {
		if subPolicy, err = projectExclusions(repo); err != nil {
			return nil, err
		}
	}
	return func(gitPath string) (string, bool) {
		rel, under := strings.CutPrefix(gitPath, prefix)
		if !under || rel == "" || !CochangeFile(rel) || agentConfig(gitPath) || topIgnored(gitPath) || subPolicy(rel) {
			return "", false
		}
		return rel, true
	}, nil
}

func pathBase(slashed string) string {
	if i := strings.LastIndexByte(slashed, '/'); i >= 0 {
		return slashed[i+1:]
	}
	return slashed
}

// Credential files, matching the Trace CLI's built-in secret paths
// (internal/manifest SecretPath). Names and suffixes match a file's own name
// only, so a directory such as pkg/kubeconfig/ is not excluded.
var (
	secretNames = wordSetShared(".npmrc .pypirc .netrc _netrc .git-credentials .pgpass credentials.json kubeconfig kubeconfig.yaml kubeconfig.yml")
	// Certificate and key stores, Terraform state (it holds resource
	// secrets) and kubeconfig files.
	secretSuffixes = []string{".p12", ".pfx", ".jks", ".keystore", ".tfstate", ".tfstate.backup", ".kubeconfig"}
	// Files identified by their directory as well as their name.
	secretFiles = []string{".kube/config", ".docker/config.json"}
	// Generated dependency resolution: Yarn Plug'n'Play loaders and data.
	generatedNames = wordSetShared(".pnp.cjs .pnp.loader.mjs .pnp.js .pnp.data.json")
	// Default OpenSSH key names of every key type, with their .pub files.
	sshKeyPrefixes = []string{"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519"}
)

// foldPath normalizes a repository-relative path for policy matching like
// the Trace CLI (internal/manifest foldPath and SecretPath): backslashes are
// separators, a leading ./ is dropped, case is folded (case-insensitive file
// systems resolve .SSH to .ssh) and zero-width and bidirectional control
// code points, which render invisibly, are removed.
func foldPath(rel string) string {
	rel = strings.TrimPrefix(strings.ReplaceAll(rel, "\\", "/"), "./")
	return strings.Map(func(r rune) rune {
		switch {
		case r == 0x200c || r == 0x200d || r == 0x200e || r == 0x200f, r >= 0x202a && r <= 0x202e,
			r >= 0x206a && r <= 0x206f, r == 0xfeff:
			return -1
		}
		return unicode.ToLower(r)
	}, rel)
}

// excludedSource reports generated Trace state, dependency trees and secret
// paths for a repository-relative path in any case or separator form.
// Indexing, revision comparison and cochange share it, even for tracked
// files.
func excludedSource(rel string) bool {
	rel = foldPath(filepath.ToSlash(rel))
	for _, part := range strings.Split(rel, "/") {
		switch part {
		case ".git", ".trace", ".trace-brain", ".trace-graph", ".aws", ".ssh", "node_modules", "vendor":
			return true
		}
		if strings.HasPrefix(part, ".env") || strings.HasSuffix(part, ".pem") || strings.HasSuffix(part, ".key") {
			return true
		}
		for _, key := range sshKeyPrefixes {
			if strings.HasPrefix(part, key) {
				return true
			}
		}
	}
	base := pathBase(rel)
	if generatedNames[base] || secretNames[base] || (strings.HasPrefix(base, "service-account") && strings.HasSuffix(base, ".json")) {
		return true
	}
	for _, suffix := range secretSuffixes {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}
	for _, file := range secretFiles {
		if rel == file || strings.HasSuffix(rel, "/"+file) {
			return true
		}
	}
	// Yarn's vendored releases (.yarn/releases/yarn-4.5.0.cjs) are
	// third-party bundles.
	return strings.HasPrefix(rel, ".yarn/releases/") || strings.Contains(rel, "/.yarn/releases/")
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
