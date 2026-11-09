# trace-graph

Code map for Trace: analyzes a repository and maps files, symbols, and relationships.

Licensed under [Apache-2.0](LICENSE).

The coordinated Trace bundle supplies this executable beside the other two
local components, with one release version. With an approved personal connection,
CLI publishes bounded derived context to the hosted dashboard. Raw companion
SQLite files and manual/MCP-authored Brain notes remain local. Graph snapshots
describe the checkout when indexed, with their index timestamp.

## Commands
- `trace-graph version`
- `trace-graph analyze [--repo .]`
- `trace-graph symbols`
- `trace-graph query <term>`
- `trace-graph impact <symbol-or-id> [--direction callers|callees|both] [--depth 3] [--json]`
- `trace-graph health`
- `trace-graph snapshot [--limit 100000]`
- `trace-graph changes --base snapshot.ndjson`
- `trace-graph compare --base REF [--head REF] [--detect-renames]`
- `trace-graph refresh [--repo .]` (index and return protocol-1 JSON)
- `trace-graph context [--path FILE] [--query words]` (bounded, read-only protocol-1 JSON)

## Build
`go build ./...`

## Index integrity

Default database paths are anchored to `--repo`. New initialized Trace projects use `.trace/graph/graph.db`; pre-existing `.trace-graph/graph.db` is retained, and `--db` remains explicit. With the binary on the CLI daemon's `PATH`, Graph refreshes after completed turns/sessions and CLI recovery. `trace context` and MCP `get_context` include repository-scoped symbols, relationships, and index time. Lookup never creates an index. Graph analyzes the current checkout; derived databases are rebuilt locally.

Git checkouts index tracked and untracked non-ignored sources. Generated Trace state, dependency trees and secret paths are excluded even if tracked. Non-Git source directories remain supported. Newly ignored files are removed on the next successful generation.

Write transactions reserve the SQLite writer before reading cached generations. Concurrent first opens retry transient WAL setup contention, and schema upgrades run under one write transaction so a second opener cannot clear a newly written generation.

Reindexing replaces changed files and removes deleted files, symbols, call relationships, and search entries in one transaction. A failed scan or database write leaves the previous generation intact. Cache keys and symbol IDs include the canonical repository identity; content hashes detect edits even when file size and timestamps are unchanged. Switching between default and Tree-sitter builds invalidates the cache.

The old derived index is cleared once on schema upgrade. Run `trace-graph analyze` again to rebuild it. This does not change source files.

Validation: `go test -race ./...`, `go test -race -tags treesitter ./...`, and either command with `-count=100`. Tree-sitter requires a C toolchain.

## Continuous integration

`.github/workflows/ci.yml` runs pure Go builds, vet, race tests, and 100 repetitions of the regression cases on GitHub-hosted macOS and Linux runners for pushes, pull requests, and manual runs. The workflow has read-only repository access. Hosted execution begins after these changes are pushed.
Tree-sitter vet, race tests, and 100-run regressions are also included.

## Production bounds

Ordinary search text is interpreted as literal words, including punctuation. Context queries have a two-second deadline and keep at most 50 ranked candidates in memory. Local embeddings are hashed word projections rather than a learned semantic model.

Indexing has a one-minute deadline, an 8 MiB limit per source file, a 512 MiB total source limit, and a 100,000-file limit. Git path listings are capped at 4 MiB. Limit failures preserve the previous index generation. Valid bodyless Go declarations remain symbols without body traversal. Removed symbols cascade to embeddings; existing orphan embeddings are cleaned on upgrade. Unchanged symbol embeddings are reused.

Production builds select Go 1.27.3 to include the patched certificate verifier. CI pins this compiler and runs Govulncheck. The language version remains Go 1.26; release builds reject a different compiler. Existing runtime dependencies are pinned to their reviewed updates in go.mod/go.sum.

## Analysis quality and confidence

`trace-graph capabilities` returns JSON for the analyzer actually compiled into
the executable. Default builds use Go AST analysis and lexical inventory for
other languages, including JS module extensions and simple TypeScript arrow
functions. Comment/string declarations are masked during lexical inventory.
This is not a type-checked language-service graph.

Go calls resolve to unique package-function identities, module-local imported
functions, explicit local receiver methods, same-file type aliases and receivers
from literal, copied-value or locally typed-conversion initializers when unambiguous. Source
symbol IDs, optional target IDs and `lexical` or `unresolved` status are returned
with relationships. Resolution follows bounded syntax evidence, with tests checked
against Go's type checker. Dynamic dispatch, cross-file aliases, factory returns,
package-shadowable builtins such as `new`, promoted embedded methods and ambiguous
targets stay unresolved. Context selects edges by symbol identity,
preventing same-name functions in different packages from sharing callers.
Tree-sitter builds report observed calls and unresolved targets explicitly.

Graph honors the same project-root `.traceignore` syntax as CLI capture. An
excluded file's old derived symbols disappear on the next successful generation.
This upgrade invalidates the previous derived index once; run `refresh` to rebuild.

## Impact, health and graph snapshots

`impact` walks resolved symbol identities transitively, with cycle detection,
repository isolation, a two-second deadline, depth 1–20 and at most 1,000 returned
symbols. Ambiguous names require an explicit ID; capped results report
`truncated: true`. Unresolved observations are never guessed into graph edges.
`health` reports index time, file/symbol counts and resolved/unresolved call counts.

`snapshot` streams versioned `trace.graph/v1` NDJSON from one read transaction,
including parser limitations, symbol source hashes, relationships and a completion
record. A missing completion record means an interrupted export. A capped export
has `truncated: true`. These inspection commands never create or migrate an index.

`changes --base FILE` compares a complete snapshot with the current index. It
reports additions, removals, signature/source changes and line movement; ambiguous
overloads remain explicit. It requires the same repository identity and analyzer
build and refuses incomplete/capped snapshots. Go AST and Tree-sitter symbols have
syntax-span hashes; lexical inventory hashes only its observed declaration line.
This is source comparison, not a proof of behavior or a type-checked Git revision
diff. Run `refresh` after upgrading to populate the new source hashes.

`compare --base REF [--head REF]` compares symbols in two locally available Git
commits (head defaults to `HEAD`). It resolves each reference once and analyzes raw
Git objects in memory, leaving the checkout, Git index, refs and persistent Graph
database untouched. Through the coordinated CLI use
`trace --repo /path/to/repo graph compare --base main --head HEAD`.
Select the working-tree root with `--repo`; subdirectories and bare repositories
are refused. Git must support `--no-lazy-fetch`; missing objects fail without a
fetch. Object replacements, checkout filters, symlinks and submodules are skipped.

The JSON report records exact commit IDs, analyzer limits, exclusion-policy hash,
source counts and symbol changes. It returns source/signature hashes and symbol
locations, without raw source or signature values. Built-in secret exclusions and
the union of current, base and head `.traceignore` rules apply to both revisions.
Invalid policies and ambiguous same-line symbol identities fail the comparison.
Limits are two minutes, 4 MiB of tree listing per revision, 100,000 tree entries,
8 MiB per source, 512 MiB of source and 100,000 symbol/relationship observations per
revision, 64 KiB per policy or signature, 32 MiB of symbol metadata per revision and
32 MiB of output. An error emits no partial report. This compares observed symbols;
type-aware behavioral analysis and complete file-only Git diffs remain outside
this command.

Add `--detect-renames` to pair unique exact-content source-file relocations with
the same extension. SHA-256 is computed from the committed bytes already analyzed;
both paths must be absent from the other tree, including as a directory or symlink.
Exactly one eligible source with that content and extension may exist in each
revision. The report
adds `file_renames` with original/new paths and whole-file hashes. Matched symbols
retain both locations and use `file_moved`; ambiguous overloads remain explicit.
Files without observed symbols can still appear in `file_renames`.

Duplicate-content candidates appear in `rename_ambiguities` with matching counts,
and their symbols keep ordinary added/removed changes. Copies and edited moves
are not paired. Different extensions are not paired even when their bytes match.
This is content evidence of relocation, not proof of author intent or Git history.
The option defaults off, retaining ordinary comparison classifications. Symbol
renaming, similarity-based edited moves and type-aware analysis remain open.
