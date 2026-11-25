# trace-graph

Code map for Trace: analyzes a repository and maps files, symbols, and relationships.

Licensed under [Apache-2.0](LICENSE).

The coordinated Trace bundle supplies this executable beside the other two
local components, with one release version. With an approved personal connection,
CLI publishes bounded derived context to the hosted dashboard. Raw companion
SQLite files and manual/MCP-authored Brain notes remain local. Graph snapshots
describe the checkout when indexed, with their index timestamp.

## Commands

Registered in `newRoot` (`root.AddCommand`): `version`, `analyze`, `symbols`,
`query`, `neighbors`, `impact`, `health`, `snapshot`, `changes`, `compare`,
`def`, `cochange`, `capabilities`, `context`, `refresh`.

- `trace-graph version` — print the coordinated bundle version.
- `trace-graph analyze [--repo .]` — index the current checkout in place.
- `trace-graph symbols` — list indexed symbols as `file:line kind name`.
- `trace-graph query <term>` — ranked symbol search. Words match name
  prefixes and split identifier words (`analyze` finds `AnalyzeRepo` and
  `analyzeFile`; `repo` finds `AnalyzeRepoContext`; `http` finds
  `parse_http_request`) as well as literal signature words. An exact name
  ranks first, then names starting with a word, then BM25 with names
  weighted over split words over signatures.
- `trace-graph neighbors <symbol-or-id>` — show definitions and direct callers (calls and interface dispatch).
- `trace-graph impact <symbol-or-id> [--direction callers|callees|both] [--depth 3] [--limit 200] [--include-dispatch] [--json]` — transitive resolved-call report.
- `trace-graph health` — index time, coverage, resolved/unresolved call counts, call resolution, relationship kinds and the Go type pass as JSON.
- `trace-graph snapshot [--limit 1000000]` — stream a versioned `trace.graph/v1` NDJSON snapshot.
- `trace-graph changes --base snapshot.ndjson` — compare a complete snapshot with the current index.
- `trace-graph compare --base REF [--head REF] [--detect-renames]` — compare two local Git commits without changing the checkout.
- `trace-graph def <name>` — show definitions and signatures for a symbol name.
- `trace-graph cochange [--max-count 200] [--max-pairs 100000] [--max-log-bytes 8388608]` — list source-file pairs that change together, from a bounded `git log` stream.
- `trace-graph capabilities` — JSON description of the compiled analyzer.
- `trace-graph context [--path FILE] [--query words] [--limit 20] [--semantic]` — bounded, read-only protocol-1 JSON; `--query` uses the ranked `query` search even with `--path`, and `--semantic` reranks a bounded candidate set (the 500 best search matches plus up to 500 symbols in files whose path contains a query word, within `--path` when given) by local embeddings.
- `trace-graph refresh [--repo .]` — index and return protocol-1 JSON.

All analysis and inspection commands remain repository-scoped. `cochange` uses
the same supported-file and exclusion policy as indexing, scans at most
`--max-count` commits, and refuses to exceed `--max-pairs` unique file pairs or
`--max-log-bytes`. It reads the log NUL-delimited, so non-ASCII and
special-character paths are kept. `symbols`, `query`, `neighbors` and `def`
open the index read-only like `health` and `impact`: they never create or
migrate it and answer from the last committed generation while a refresh runs.

## Build
`go build ./...`

## Index integrity

Default database paths are anchored to `--repo`. New initialized Trace projects use `.trace/graph/graph.db`; pre-existing `.trace-graph/graph.db` is retained, and `--db` remains explicit. With the binary on the CLI daemon's `PATH`, Graph refreshes after completed turns/sessions and CLI recovery. `trace context` and MCP `get_context` include repository-scoped symbols, relationships, and index time. Lookup never creates an index. Graph analyzes the current checkout; derived databases are rebuilt locally.

Git checkouts index tracked and untracked non-ignored sources. Generated Trace state, dependency trees and secret paths are excluded even if tracked. Non-Git source directories remain supported. Newly ignored files are removed on the next successful generation.

Every Git command Graph runs (indexing, `cochange`, `compare`) drops ambient `GIT_*` variables, so a hook's `GIT_DIR`, `GIT_WORK_TREE`, `GIT_INDEX_FILE` or `GIT_CONFIG_*` cannot redirect it to another repository, index or configuration, and runs with `core.fsmonitor` disabled, lazy fetches prohibited, replacement objects ignored and optional locks off. Source reads are anchored to the repository root: a source beneath a directory that was replaced by a symlink is skipped (Git treats it as deleted), an open cannot leave the root, does not block on a FIFO and must yield the regular file that was checked.

Write transactions reserve the SQLite writer before reading cached generations. Concurrent first opens retry transient WAL setup contention, and schema upgrades run under one write transaction so a second opener cannot clear a newly written generation.

Reindexing replaces changed files and removes deleted files, symbols, call relationships, and search entries in one transaction. A failed scan or database write leaves the previous generation intact. Cache keys and symbol IDs include the canonical repository identity; content hashes detect edits even when file size and timestamps are unchanged. Switching between default and Tree-sitter builds invalidates the cache.

The old derived index is cleared once on schema upgrade. Run `trace-graph analyze` again to rebuild it. This does not change source files.

Validation: `go test -race ./...`, `go test -race -tags treesitter ./...`, and either command with `-count=100`. Tree-sitter requires a C toolchain.

## Continuous integration

`.github/workflows/ci.yml` runs pure Go builds, vet, race tests, and 100 repetitions of the regression cases on GitHub-hosted macOS and Linux runners for pushes, pull requests, and manual runs. The workflow has read-only repository access. Hosted execution begins after these changes are pushed.
Tree-sitter vet, race tests, and 100-run regressions are also included.

## Production bounds

Ordinary search text is interpreted as literal words, including punctuation; FTS operators are never interpreted. Identifiers are split at camelCase, snake_case and letter/digit boundaries (`HTTPServer` → `HTTP Server`, `utf8Decode` → `utf 8 Decode`) when a symbol is written. Index schema 4 adds these words to an existing index in place on the next `analyze`/`refresh`; until then read-only lookups fall back to name prefixes. Context queries have a two-second deadline; semantic ranking scores at most 1,000 candidates. Local embeddings are hashed word projections of a symbol's split name, signature and file path rather than a learned semantic model. Index schema 5 drops older vectors, which the next `analyze`/`refresh` recomputes.

Indexing has a one-minute deadline, an 8 MiB limit per source file, a 512 MiB total source limit, and a 100,000-file limit. Git path listings are capped at 4 MiB. Limit failures preserve the previous index generation. Valid bodyless Go declarations remain symbols without body traversal. Removed symbols cascade to embeddings; existing orphan embeddings are cleaned on upgrade. Unchanged symbol embeddings are reused. Per-file relation deletes use a `(repo, file)` index, so a cold index grows with the repository rather than with files × relations.

Package-wide Go type checking retains at most 32 MiB of Go source per index (a larger checkout keeps syntax evidence for every Go call), stops starting packages after 20 seconds or 15 seconds before the index deadline (a pass cut short is recorded as partial and completed by the next refresh, even with no changed file), records at most 32 dispatch edges per interface call and 100,000 per repository, and never loads packages outside the analyzed module. A refresh with no changed or deleted file and a complete previous pass skips resolution. Measured on this machine: trace-cli (1.3 MB of Go) indexes cold in about 1.5 s; the Go tree's `cmd` (29 MB of Go in 1,472 files) in about 25–27 s with 0.5–0.6 GB peak memory, of which the type pass takes about 4 s. Lexical analysis is linear in file size: body searches are bounded per declaration and brace bodies matched once per file (a 568 KB JavaScript file with 6,000 functions indexes in under 2 s; 40,000 semicolon-less type aliases analyze in under a second).

Production builds select Go 1.27.4 to include the patched certificate verifier. CI pins this compiler and runs Govulncheck. The language version remains Go 1.26; release builds reject a different compiler. Existing runtime dependencies are pinned to their reviewed updates in go.mod/go.sum.

## Analysis quality and confidence

`trace-graph capabilities` returns JSON for the analyzer actually compiled into
the executable (at most eight limitations, which the Trace CLI enforces).
Relationship kinds are `call`, `implements`, `dispatch`, `extends`, `import`
and `route`; resolutions are `typed` (Go type checker), `lexical` (syntax
evidence), `dynamic` (interface or function-value dispatch, target unknown),
`module` (an import resolved to a repository file but not to a declaration)
and `unresolved`. Unresolved and dynamic targets are never guessed.

Go: after the per-file pass, every package of the analyzed module is
type-checked together with `go/types` (standard library only). Module packages
are imported from the analyzed source; the standard library and dependencies
are never loaded and their types are unknown. Files are matched for the host
platform without cgo; `testdata`, `_`/`.` directories and directories with more
than one package are skipped, and in-package and external tests are checked.
A call resolves `typed` through variable and factory-return types, cross-file
and import aliases, promoted methods and fields, and generic instantiations.
Anything depending on an unknown type keeps its syntax result, and duplicate
declarations or test-file targets of non-test callers are never targets. A
promoted method is typed only when the receiver and every type embedded at a
shallower depth are fully known: of known type and underlying type, with a
complete interface method set, and declared in a package whose directory holds
no Go file the pass did not see (excluded by policy or `.gitignore`,
symlinked, unreadable or unparsable; only entry names are read, and
`go_types.incomplete_packages` counts them). Fully known module types
satisfying a module interface get `implements` edges (interfaces whose method
set involves unknown types are skipped, because unknown types compare
identical). An interface call through a static named interface, or a named
interface field embedded on the selection path, gets `dispatch` edges to its
possible non-test module implementations; calls through unnamed interfaces or
type parameters get none. Syntax evidence (unique package
functions, module-local import calls and receivers from declarations,
literals and local conversions, package-level types only) remains the result
for files the pass does not check. On trace-cli every `typed` edge agreed with
a full type check using the real standard library and dependencies (2,448
edges, none wrong, no in-repository target missed). Calls inside closures are
omitted from enclosing-function calls. HTTP routes registered with net/http or
gorilla `HandleFunc`/`Handle` (Go 1.22 `"GET /path"` patterns give the method)
and chi, gin and echo verb methods with a literal path become `route` edges
whose handler resolves like a call. Positions are physical lines even under
`//line` directives, and blank identifiers are not symbols.

TypeScript/JavaScript, Python, Rust and Java use declaration-aware lexical
analysis of masked source (comments, strings and JavaScript regular
expression literals blanked per language; it is not a type checker).
Declarations get extents and containers, so source hashes cover whole bodies
(comments masked, whitespace collapsed, literals kept), and calls are
attributed to the innermost enclosing function. Imports
resolve to repository files: relative JS/TS specifiers (including `./x.js`
for a `./x.ts` source), Python relative and unique absolute modules, Rust
`crate::`/`self::`/`super::` paths and `mod` files, and Java class files.
Package names and path aliases stay unresolved. A call, route handler or
`extends`/`implements` reference resolves only to exactly one declaration: a
module-level declaration of the same file, an explicitly imported one, a
member reached through a module binding or an imported or same-file type
(static members; any for Python classes), a statically bound member of the
caller's Java class, or a Java class of the same package. JavaScript and
TypeScript named and namespace imports resolve only to exported declarations
(`export`, export lists, `export default`, CommonJS `exports.f = f` and
shorthand `module.exports = { f }`); default imports and `require` namespace
values have unknown members; Python `import a.b` binds the package `a`. Names
bound as parameters or locals of the caller or of any enclosing function,
names a file both imports and declares, reassigned module-level bindings,
calls on `this`/`self` or on expressions, and names inside Rust inline `mod`
blocks (except `crate::` paths) never resolve. Express/Hono-style
`obj.method('/path', ..., handler)` calls and FastAPI/Flask decorators become
routes; an inline handler resolves only when it delegates to a single call,
optionally inside one middleware call. Other languages use lexical inventory.

Tree-sitter builds report observed calls and unresolved targets explicitly,
name declarations from an explicit per-grammar list, and record the same
imports and routes; their symbols are not import or handler targets except
decorated Python route functions.

Graph honors the same project-root `.traceignore` syntax as CLI capture. An
excluded file's old derived symbols disappear on the next successful generation.
This upgrade (analyzer `inventory-v8`, Tree-sitter `treesitter-v5`) re-analyzes
every file once on the next refresh; existing data is kept until then.

## Impact, health and graph snapshots

`impact` walks resolved call edges (and, with `--include-dispatch`, interface
dispatch edges) transitively, with cycle detection,
repository isolation, a two-second deadline, depth 1–20 and at most 1,000 returned
symbols. Ambiguous names require an explicit ID; capped results report
`truncated: true`. If the deadline expires mid-walk, the command returns the
best-effort symbols gathered so far with `truncated: true` and
`deadline_exceeded: true` rather than failing. Unresolved observations are
never guessed into graph edges.
`health` reports index time, file/symbol counts, resolved/unresolved counts of
`call` relations, `call_resolution` by resolution, `relations` by kind and the
stored `go_types` pass record (status, packages, incomplete packages, files,
type errors, typed and dynamic calls, implements and dispatch edges).

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
are refused. Git 2.45+ uses `--no-lazy-fetch`; older releases use the
`GIT_NO_LAZY_FETCH` environment fallback. Missing objects fail without a fetch.
Object replacements, checkout filters, symlinks and submodules are skipped.

The JSON report records exact commit IDs, analyzer limits, exclusion-policy hash,
source counts and symbol changes. It returns source/signature hashes and symbol
locations, without raw source or signature values. Built-in secret exclusions and
the union of current, base and head `.traceignore` rules apply to both revisions.
Invalid policies and ambiguous same-line symbol identities fail the comparison.
Limits are two minutes, 4 MiB of tree listing per revision, 100,000 tree entries,
8 MiB per source, 512 MiB of source and 100,000 symbol/relationship observation rows per
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
The option defaults off, retaining ordinary comparison classifications.

`--detect-renames` also pairs renamed symbols (`symbol_rename_detection`:
`name_normalized_body`). Declarations with body extents (Go, the
declaration-aware lexical languages, Tree-sitter) are compared by their
comment-masked tokens with their own name replaced. A removed and an added
symbol of the same kind and a different name pair as `identical_body` when
that body is unique on both sides (confidence 1, or 0.75 under 12 tokens), or
as `similar_body` when each is the other's best match with an estimated token
trigram similarity of at least 0.7 and a 0.1 margin (confidence is that
similarity, scaled by 0.9 when the name-normalized signature differs). Each pair
becomes one `renamed` change and a `symbol_renames` entry with both locations,
hashes, evidence, confidence, body similarity and signature match; identical
bodies that could pair more than one way appear in `symbol_rename_ambiguities`
and keep their changes. Above 250,000 candidate pairs only identical bodies are
paired (`identical_body_only`). A name and kind present on both sides is the
same symbol moved or edited and never takes part. This is evidence of a
rename, not proof of intent. Similarity-based edited file moves and type-aware
revision analysis remain open.
