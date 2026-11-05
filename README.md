# trace-graph

Code map for Trace: analyzes a repository and maps files, symbols, and relationships.

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

Go calls resolve to unique package-function identities and module-local imported
functions when unambiguous. Source symbol IDs, optional target IDs and `lexical`
or `unresolved` status are returned with relationships. Member dispatch and
ambiguous targets stay unresolved. Context selects edges by symbol identity,
preventing same-name functions in different packages from sharing callers.
Tree-sitter builds report observed calls and unresolved targets explicitly.

Graph honors the same project-root `.traceignore` syntax as CLI capture. An
excluded file's old derived symbols disappear on the next successful generation.
This upgrade invalidates the previous derived index once; run `refresh` to rebuild.
