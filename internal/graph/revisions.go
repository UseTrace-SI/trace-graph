package graph

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

type revisionEntry struct {
	mode, kind, oid, file string
	size                  int64
}
type revision struct {
	commit   string
	entries  []revisionEntry
	policy   []byte
	excluded func(string) bool
}
type RevisionPair struct {
	BaseCommit string `json:"base_commit"`
	HeadCommit string `json:"head_commit"`
	PolicyHash string `json:"policy_hash"`
	root       string
	base, head revision
	excluded   func(string) bool
}
type RevisionStats struct {
	Files        int   `json:"files"`
	Excluded     int   `json:"excluded"`
	NonRegular   int   `json:"non_regular"`
	Observations int   `json:"observations"`
	SourceBytes  int64 `json:"source_bytes"`
}

// The explicit flags fail closed on older Git, prohibit lazy network fetches
// and replacement objects, and avoid index refreshes. No checkout/filter runs.
func revisionGit(ctx context.Context, root string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--no-pager", "--no-lazy-fetch", "--no-replace-objects", "--no-optional-locks", "-C", root}, args...)...)
	for _, value := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(value), "GIT_") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1", "GIT_NO_REPLACE_OBJECTS=1", "GIT_OPTIONAL_LOCKS=0")
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	return cmd
}

type revisionBuffer struct {
	bytes.Buffer
	limit int
}

func (b *revisionBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, fmt.Errorf("Git output exceeds comparison budget")
	}
	return b.Buffer.Write(p)
}
func revisionOutput(ctx context.Context, root string, limit int, args ...string) ([]byte, error) {
	var out revisionBuffer
	out.limit = limit
	cmd := revisionGit(ctx, root, args...)
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("local Git comparison command failed (Git with --no-lazy-fetch support and locally available objects is required): %w", err)
	}
	return out.Bytes(), nil
}
func objectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && value == strings.ToLower(value)
}
func resolveCommit(ctx context.Context, root, ref string) (string, error) {
	if ref == "" || len(ref) > 256 || strings.HasPrefix(ref, "-") || strings.ContainsFunc(ref, unicode.IsControl) || !utf8.ValidString(ref) {
		return "", fmt.Errorf("revision must be a valid commit reference of at most 256 bytes")
	}
	out, err := revisionOutput(ctx, root, 1024, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	oid := strings.TrimSpace(string(out))
	if !objectID(oid) {
		return "", fmt.Errorf("Git returned an invalid commit identity")
	}
	return oid, nil
}
func readProjectPolicy(root string) ([]byte, error) {
	anchor, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer anchor.Close()
	expected, err := anchor.Lstat(".traceignore")
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !expected.Mode().IsRegular() || expected.Size() > 65536 {
		return nil, fmt.Errorf(".traceignore must be a regular file of at most 64 KiB")
	}
	file, err := anchor.OpenFile(".traceignore", os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	actual, err := file.Stat()
	if err != nil || !os.SameFile(expected, actual) {
		return nil, fmt.Errorf(".traceignore changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, 65537))
	if err != nil {
		return nil, err
	}
	if len(data) > 65536 {
		return nil, fmt.Errorf(".traceignore exceeds 64 KiB")
	}
	return data, nil
}
func listRevision(ctx context.Context, root, commit string) (revision, error) {
	result := revision{commit: commit}
	data, err := revisionOutput(ctx, root, 4<<20, "ls-tree", "--full-tree", "-r", "-z", "--long", commit)
	if err != nil {
		return result, err
	}
	if len(data) > 0 && data[len(data)-1] != 0 {
		return result, fmt.Errorf("incomplete Git tree listing")
	}
	seen := map[string]bool{}
	for _, record := range bytes.Split(data, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		parts := bytes.SplitN(record, []byte{'\t'}, 2)
		if len(parts) != 2 {
			return result, fmt.Errorf("invalid Git tree entry")
		}
		fields := strings.Fields(string(parts[0]))
		file := string(parts[1])
		if len(fields) != 4 || !objectID(fields[2]) || !utf8.ValidString(file) || len(file) > 4096 || file == "." || path.Clean(file) != file || path.IsAbs(file) || strings.HasPrefix(file, "../") || strings.ContainsAny(file, "\\") || strings.ContainsFunc(file, unicode.IsControl) || seen[file] {
			return result, fmt.Errorf("invalid, duplicate or oversized Git tree path")
		}
		seen[file] = true
		if len(seen) > 100000 {
			return result, fmt.Errorf("Git revision exceeds 100000 entries")
		}
		size := int64(-1)
		if fields[3] != "-" {
			size, err = strconv.ParseInt(fields[3], 10, 64)
			if err != nil || size < 0 {
				return result, fmt.Errorf("invalid Git blob size")
			}
		}
		result.entries = append(result.entries, revisionEntry{fields[0], fields[1], fields[2], file, size})
	}
	batch, err := startRevisionBatch(ctx, root)
	if err != nil {
		return result, err
	}
	defer batch.stop(false)
	for _, entry := range result.entries {
		if entry.file == ".traceignore" {
			if !entry.regular() {
				return result, fmt.Errorf("committed .traceignore must be a regular file")
			}
			result.policy, err = batch.read(entry, 65536)
			if err != nil {
				return result, err
			}
			break
		}
	}
	if err := batch.stop(true); err != nil {
		return result, err
	}
	result.excluded, err = parseProjectExclusions(result.policy)
	return result, err
}
func (e revisionEntry) regular() bool {
	return e.kind == "blob" && (e.mode == "100644" || e.mode == "100755")
}

// Exclusions are the union of the working policy and both committed policies;
// removing a later ignore rule never exposes a previously excluded source.
func PrepareRevisionPair(ctx context.Context, root, base, head string) (*RevisionPair, error) {
	canonical, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	canonical, err = filepath.EvalSymlinks(canonical)
	if err != nil {
		return nil, err
	}
	top, err := revisionOutput(ctx, canonical, 4096, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	gitRoot, err := filepath.EvalSymlinks(strings.TrimSuffix(string(top), "\n"))
	if err != nil || gitRoot != canonical {
		return nil, fmt.Errorf("--repo must select the Git working-tree root")
	}
	current, err := readProjectPolicy(canonical)
	if err != nil {
		return nil, err
	}
	ignoreCurrent, err := parseProjectExclusions(current)
	if err != nil {
		return nil, err
	}
	baseID, err := resolveCommit(ctx, canonical, base)
	if err != nil {
		return nil, err
	}
	headID, err := resolveCommit(ctx, canonical, head)
	if err != nil {
		return nil, err
	}
	before, err := listRevision(ctx, canonical, baseID)
	if err != nil {
		return nil, err
	}
	after, err := listRevision(ctx, canonical, headID)
	if err != nil {
		return nil, err
	}
	sum := sha256.New()
	for _, policy := range [][]byte{current, before.policy, after.policy} {
		digest := sha256.Sum256(policy)
		sum.Write(digest[:])
	}
	return &RevisionPair{BaseCommit: baseID, HeadCommit: headID, PolicyHash: fmt.Sprintf("%x", sum.Sum(nil)), root: canonical, base: before, head: after, excluded: func(file string) bool {
		return excludedSource(strings.ToLower(file)) || ignoreCurrent(file) || before.excluded(file) || after.excluded(file)
	}}, nil
}

type revisionBatch struct {
	cmd    *exec.Cmd
	input  io.WriteCloser
	output *bufio.Reader
	closed bool
}

func startRevisionBatch(ctx context.Context, root string) (*revisionBatch, error) {
	cmd := revisionGit(ctx, root, "cat-file", "--batch")
	input, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		input.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		input.Close()
		output.Close()
		return nil, err
	}
	return &revisionBatch{cmd: cmd, input: input, output: bufio.NewReader(output)}, nil
}
func (b *revisionBatch) read(entry revisionEntry, limit int64) ([]byte, error) {
	if !entry.regular() || entry.size < 0 || entry.size > limit {
		return nil, fmt.Errorf("committed source exceeds its size budget or is not regular")
	}
	if _, err := fmt.Fprintln(b.input, entry.oid); err != nil {
		return nil, err
	}
	header, err := b.output.ReadSlice('\n')
	if err != nil {
		return nil, fmt.Errorf("reading local object header: %w", err)
	}
	fields := strings.Fields(string(header))
	if len(fields) != 3 || fields[0] != entry.oid || fields[1] != "blob" {
		return nil, fmt.Errorf("Git object is unavailable locally or has an unexpected identity/type")
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || size != entry.size || size > limit {
		return nil, fmt.Errorf("Git object size does not match its tree entry")
	}
	data := make([]byte, int(size))
	if _, err := io.ReadFull(b.output, data); err != nil {
		return nil, err
	}
	end, err := b.output.ReadByte()
	if err != nil || end != '\n' {
		return nil, fmt.Errorf("incomplete Git object response")
	}
	return data, nil
}
func (b *revisionBatch) stop(success bool) error {
	if b.closed {
		return nil
	}
	b.closed = true
	b.input.Close()
	if !success {
		_ = b.cmd.Process.Kill()
	}
	return b.cmd.Wait()
}

// A revision can only be analyzed into an ephemeral in-memory index. This
// prevents a caller accidentally replacing the project's persistent graph.
func (p *RevisionPair) Analyze(ctx context.Context, side string, db *sql.DB) (RevisionStats, error) {
	stats := RevisionStats{}
	selected := p.base
	if side == "head" {
		selected = p.head
	} else if side != "base" {
		return stats, fmt.Errorf("comparison side must be base or head")
	}
	rows, err := db.QueryContext(ctx, "PRAGMA database_list")
	if err != nil {
		return stats, err
	}
	for rows.Next() {
		var seq int
		var name, file string
		if err := rows.Scan(&seq, &name, &file); err != nil {
			rows.Close()
			return stats, err
		}
		if file != "" {
			rows.Close()
			return stats, fmt.Errorf("revision analysis requires an in-memory index")
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return stats, err
	}
	batch, err := startRevisionBatch(ctx, p.root)
	if err != nil {
		return stats, err
	}
	defer batch.stop(false)
	module := ""
	for _, entry := range selected.entries {
		if entry.file == "go.mod" && !p.excluded(entry.file) {
			if !entry.regular() {
				return stats, fmt.Errorf("committed go.mod must be regular")
			}
			data, err := batch.read(entry, 65536)
			if err != nil {
				return stats, err
			}
			module = moduleFromText(data)
			break
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return stats, err
	}
	defer tx.Rollback()
	for _, table := range []string{"relations", "symbols", "file_cache_v2", "repo_state"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE repo=?", p.root); err != nil {
			return stats, err
		}
	}
	writer := &indexWriter{tx: tx, moduleOverride: &module, maxRecords: 100000}
	for _, entry := range selected.entries {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		if p.excluded(entry.file) {
			stats.Excluded++
			continue
		}
		if !entry.regular() {
			stats.NonRegular++
			continue
		}
		if !supportedFile(strings.ToLower(path.Ext(entry.file))) {
			continue
		}
		if entry.size > 8<<20 || stats.SourceBytes+entry.size > 512<<20 {
			return stats, fmt.Errorf("revision exceeds 8 MiB per source or 512 MiB total")
		}
		data, err := batch.read(entry, 8<<20)
		if err != nil {
			return stats, err
		}
		if _, err := analyzeFile(p.root, entry.file, entry.file, data, writer); err != nil {
			return stats, err
		}
		if writer.err != nil {
			return stats, writer.err
		}
		stats.Files++
		stats.SourceBytes += entry.size
	}
	if err := resolveCalls(ctx, tx, p.root); err != nil {
		return stats, err
	}
	if err := batch.stop(true); err != nil {
		return stats, err
	}
	if err := tx.Commit(); err != nil {
		return stats, err
	}
	stats.Observations = writer.records
	return stats, nil
}
