# trace-graph

Code map for Trace: analyzes a repository and maps files, symbols, and relationships.

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

Reindexing replaces changed files and removes deleted files, symbols, call relationships, and search entries in one transaction. A failed scan or database write leaves the previous generation intact. Cache keys and symbol IDs include the canonical repository identity; content hashes detect edits even when file size and timestamps are unchanged. Switching between default and Tree-sitter builds invalidates the cache.

The old derived index is cleared once on schema upgrade. Run `trace-graph analyze` again to rebuild it. This does not change source files.

Validation: `go test -race ./...`, `go test -race -tags treesitter ./...`, and either command with `-count=100`. Tree-sitter requires a C toolchain.

## Continuous integration

`.github/workflows/ci.yml` runs pure Go builds, vet, race tests, and 100 repetitions of the regression cases on GitHub-hosted macOS and Linux runners for pushes, pull requests, and manual runs. The workflow has read-only repository access. Hosted execution begins after these changes are pushed.
Tree-sitter vet, race tests, and 100-run regressions are also included.
