# trace-graph

Code map for Trace: analyzes a repository and maps files, symbols, and relationships.

Licensed under [Apache-2.0](LICENSE).

The coordinated Trace bundle supplies this executable beside the other two
local components, with one release version. With an approved personal connection,
CLI publishes bounded derived context to the hosted dashboard. Raw companion
SQLite files and manual/MCP-authored Brain notes remain local. Graph snapshots
describe the checkout when indexed, with their index timestamp.

## Quick start

```sh
go build -o trace-graph ./cmd/trace-graph          # default build (pure Go)
go build -tags treesitter -o trace-graph ./cmd/trace-graph   # Tree-sitter build (needs a C toolchain)

cd /path/to/repo
trace-graph analyze                      # index; later runs re-analyze only changed files
trace-graph query analyze repo           # ranked search: AnalyzeRepo, analyzeFile, ...
trace-graph def AnalyzeRepo              # definitions with signatures
trace-graph neighbors AnalyzeRepo        # direct callers
trace-graph impact AnalyzeRepo --depth 2 # transitive callers (--direction callees|both)
trace-graph context --query "parse config" --semantic   # protocol-1 JSON for agents
trace-graph health                       # coverage, resolution and file warnings
trace-graph mcp                          # the same lookups as read-only MCP tools
```

## What each language gets

| Language (extensions) | Default build (`inventory-v9`) | Tree-sitter build (`treesitter-v6`) |
| --- | --- | --- |
| Go (`.go`) | `go/parser` declarations (func, method, type, var/const) with extents; calls resolved package-wide by `go/types` (`typed`) or syntax evidence; `implements` and interface `dispatch` edges; net/http, gorilla, chi, gin and echo routes | grammar declarations; calls observed, targets unresolved; no type pass |
| TypeScript / JavaScript (`.ts` `.tsx` `.mts` `.cts` `.js` `.jsx` `.mjs` `.cjs`) | declaration-aware lexical analysis: declarations with extents and containers, calls resolved through same-file declarations and explicit imports, imports resolved to repository files, `extends`/`implements`, Express/Hono routes | grammar declarations; calls observed, unresolved; imports and routes from the shared lexical extractor, resolved to module files |
| Python (`.py`) | as TypeScript/JavaScript; FastAPI/Flask decorator routes | as TypeScript/JavaScript; decorated route functions are handler targets |
| Rust (`.rs`), Java (`.java`) | as TypeScript/JavaScript (Rust `crate::`/`self::`/`super::` and `mod` files; Java class files and same-package classes) | grammar declarations; calls observed, unresolved; imports from the shared extractor |
| C (`.c`), C++ (`.cc` `.cpp` `.cxx`), headers (`.h` `.hh` `.hpp` `.hxx`) | lexical inventory: line-level declarations (classes, structs, enums), no calls | grammar declarations (functions); calls observed, unresolved; headers use the C++ grammar |
| Ruby (`.rb`) | lexical inventory | grammar declarations (methods); calls observed, unresolved |
| C#, Kotlin, Swift, Scala, Dart, PHP, Lua, R, SQL, shell, Haskell, Elixir, Erlang, Clojure, CUE, Groovy, HCL/Terraform, Julia, OCaml, Objective-C (`.m`), Perl, Protobuf, Zig | lexical inventory: line-level declarations, no calls | not indexed |

YAML and other configuration files are not indexed. `trace-graph capabilities`
reports the analyzer and limitations of the binary at hand.

## Commands

Registered in `newRoot` (`root.AddCommand`): `version`, `analyze`, `symbols`,
`query`, `neighbors`, `impact`, `health`, `snapshot`, `changes`, `compare`,
`def`, `cochange`, `capabilities`, `context`, `refresh`, `mcp`.

- `trace-graph version` — print the coordinated bundle version.
- `trace-graph analyze [--repo .]` — index the current checkout in place.
- `trace-graph symbols [--limit N] [--json]` — list indexed symbols as `file:line kind name` (all by default).
- `trace-graph query <words> [--limit N] [--json]` — ranked symbol search (every match by default). Words match name
  prefixes and split identifier words (`analyze` finds `AnalyzeRepo` and
  `analyzeFile`; `repo` finds `AnalyzeRepoContext`; `http` finds
  `parse_http_request`) as well as literal signature words. An exact name
  ranks first, then names starting with a word, then BM25 with names
  weighted over split words over signatures.
- `trace-graph neighbors <symbol-or-id>` — show definitions and direct callers (calls and interface dispatch).
- `trace-graph impact <symbol-or-id> [--direction callers|callees|both] [--depth 3] [--limit 200] [--include-dispatch] [--json]` — transitive resolved-call report.
- `trace-graph health` — index time, coverage, file warnings, resolved/unresolved call counts, call resolution, relationship kinds and the Go type pass as JSON.
- `trace-graph snapshot [--limit 1000000]` — stream a versioned `trace.graph/v1` NDJSON snapshot.
- `trace-graph changes --base snapshot.ndjson` — compare a complete snapshot with the current index.
- `trace-graph compare --base REF [--head REF] [--detect-renames]` — compare two local Git commits without changing the checkout.
- `trace-graph def <name>` — show definitions and signatures for a symbol name.
- `trace-graph cochange [--max-count 200] [--max-pairs 100000] [--max-log-bytes 8388608]` — list source-file pairs that change together, from a bounded `git log` stream.
- `trace-graph capabilities` — JSON description of the compiled analyzer.
- `trace-graph context [--path FILE] [--query words] [--limit 20] [--semantic]` — bounded, read-only protocol-1 JSON; `--query` uses the ranked `query` search even with `--path`, and `--semantic` reranks a bounded candidate set (the 500 best search matches plus up to 500 symbols in files whose path contains a query word, within `--path` when given) by local embeddings.
- `trace-graph refresh [--repo .]` — index and return protocol-1 JSON.
- `trace-graph mcp [--repo .]` — serve read-only MCP tools over stdio (see below).

`--json` on `symbols` and `query` prints one protocol-1 object
(`repo`, `symbols` with `id`, `file`, `name`, `kind`, `line`, `signature`,
and `truncated` when `--limit` cut the list).

Symbol IDs are stored as `<sha256 of the repository path>:<file>:<name>:<line>`
so several repositories can share one database. Output scoped to one
repository (`context`, `impact --json`, `--json` lists and MCP tools) shows
them without the 64-character hash, as `<file>:<name>:<line>`; `impact`,
`neighbors` and the MCP tools accept either form. `snapshot`, `changes` and
`compare` keep the stored form.

All analysis and inspection commands remain repository-scoped. `cochange` pairs
indexable sources and common configuration files (YAML, JSON, TOML, INI,
XML, Dockerfiles, Makefiles, Terraform, Gradle, Bazel), with the secret and
generated-state exclusions of indexing and the project's `.traceignore`,
without lockfiles or Terraform variable files, scans at most
`--max-count` commits, and refuses to exceed `--max-pairs` unique file pairs or
`--max-log-bytes`. It reads the log NUL-delimited, so non-ASCII and
special-character paths are kept. `symbols`, `query`, `neighbors` and `def`
open the index read-only like `health` and `impact`: they never create or
migrate it and answer from the last committed generation while a refresh runs.

## MCP server

`trace-graph mcp` serves the code map of `--repo` to an MCP client over
stdin/stdout, using the same MCP Go SDK as Trace Brain. Every tool is
read-only and repository-scoped, opens the last committed index generation
per call (it never creates, migrates or refreshes it), has a two-second
deadline (ten seconds for `graph_cochange`) and returns at most 64 KiB of
structured JSON, dropping trailing entries with `truncated: true` beyond that.

| Tool | Input | Returns |
| --- | --- | --- |
| `graph_search` | `query`, `path?`, `limit` 1–50 (20) | ranked symbols, as `query` |
| `graph_def` | `name`, `limit` 1–50 (20) | definitions with signatures, as `def` |
| `graph_neighbors` | `symbol` (name or ID), `limit` 1–50 (20) | definitions and direct callers, as `neighbors` |
| `graph_impact` | `symbol`, `direction`, `depth` 1–20 (3), `limit` 1–200 (50), `include_dispatch` | transitive report, as `impact --json` |
| `graph_context` | `query?`, `path?`, `limit` 1–50 (20), `semantic` | protocol-1 context, as `context` |
| `graph_cochange` | `max_count` 1–1000 (200), `limit` 1–50 (20) | co-changing file pairs, as `cochange` |

Example client configuration:

```json
{"mcpServers": {"trace-graph": {"command": "trace-graph", "args": ["--repo", "/path/to/repo", "mcp"]}}}
```

## Build
`go build ./...`

## Index integrity

Default database paths are anchored to `--repo`. New initialized Trace projects use `.trace/graph/graph.db`; pre-existing `.trace-graph/graph.db` is retained, and `--db` remains explicit. With the binary on the CLI daemon's `PATH`, Graph refreshes after completed turns/sessions and CLI recovery. `trace context` and MCP `get_context` include repository-scoped symbols, relationships, and index time. Lookup never creates an index. Graph analyzes the current checkout; derived databases are rebuilt locally.

Git checkouts index tracked and untracked non-ignored sources. Generated Trace state, dependency trees and secret paths are excluded even if tracked. Secret paths follow the Trace CLI's built-in list: `.aws/` and `.ssh/` directories, `.env*`, `*.pem`, `*.key` and default SSH key names in any path element, and by file name `credentials.json`, `service-account*.json`, `kubeconfig*`, `*.kubeconfig`, `.npmrc`, `.pypirc`, `.netrc`, `.git-credentials`, `.pgpass`, `*.tfstate[.backup]`, `*.p12`, `*.pfx`, `*.jks`, `*.keystore`, `.kube/config` and `.docker/config.json`. Non-Git source directories remain supported; without a `.gitignore` they also skip `.venv/` directories and `*.min.js` bundles anywhere, and `dist/`, `build/` and `target/` directories beside a build manifest (`package.json`, `Cargo.toml`, `pyproject.toml`, `setup.py`, `setup.cfg`, `pom.xml`, `build.gradle[.kts]`), at the root or deeper; elsewhere directories with those names, such as `go/build` or `cmd/dist`, are indexed. `node_modules/` is excluded everywhere, as before. Newly ignored files are removed on the next successful generation.

Two same-named declarations on one line (typical of minified or macro-heavy code) share a file/name/line identity. Instead of failing the whole index, the first is kept and later ones are dropped together with their content, receivers and relationships, and the file gets a recorded warning: `analyze` prints it to stderr, `refresh` lists it in `file_warnings`, and `health` reports `files_with_warnings` and up to 20 `file_warnings`. The file's Go code stays out of the type pass (its package counts as incomplete) until its content changes. Revision comparisons still refuse such files.

Every Git command Graph runs (indexing, `cochange`, `compare`) drops ambient `GIT_*` variables, so a hook's `GIT_DIR`, `GIT_WORK_TREE`, `GIT_INDEX_FILE` or `GIT_CONFIG_*` cannot redirect it to another repository, index or configuration, and runs with `core.fsmonitor` disabled, lazy fetches prohibited, replacement objects ignored and optional locks off. Source reads are anchored to the repository root: a source beneath a directory that was replaced by a symlink is skipped (Git treats it as deleted), an open cannot leave the root, does not block on a FIFO and must yield the regular file that was checked.

Write transactions reserve the SQLite writer before reading cached generations. Concurrent first opens retry transient WAL setup contention, and schema upgrades run under one write transaction so a second opener cannot clear a newly written generation.

Reindexing replaces changed files and removes deleted files, symbols, call relationships, and search entries in one transaction. A failed scan or database write leaves the previous generation intact. Cache keys and symbol IDs include the canonical repository identity. A refresh skips reading and hashing a source whose stat key (size, modification time, inode change time, device and inode) matches the previous generation; any write changes the inode change time even when a tool restores the modification time, and an atomic replace changes the inode. Other sources are read and SHA-256 hashed, so a touched but identical file is not re-analyzed. A file changed within two seconds of the start of an index has no trusted stat key and is hashed again next time, covering filesystems with coarse timestamps. Platforms without an inode change time hash every source. Go sources skipped this way are read only when the type pass runs. Switching between default and Tree-sitter builds invalidates the cache.

Schema upgrades run when `analyze` or `refresh` opens the index (`PRAGMA user_version`). An index older than schema 3 is cleared once; run `trace-graph analyze` again to rebuild it. Schemas 4–6 keep the indexed generation: 4 adds split identifier words to the search index, 5 drops embeddings for the same run to recompute, and 6 adds per-file stat keys. Upgrades never change source files.

Validation: `go test -race ./...`, `go test -race -tags treesitter ./...`, and either command with `-count=100`. Tree-sitter requires a C toolchain.

## Continuous integration

`.github/workflows/ci.yml` runs pure Go builds, vet, tests without the race detector (the only run that enforces wall-clock budgets), race tests, and 100 repetitions of the regression cases on GitHub-hosted macOS and Linux runners for pushes, pull requests, and manual runs. The workflow has read-only repository access. Hosted execution begins after these changes are pushed.
Tree-sitter vet, race tests, and 100-run regressions are also included.

## Production bounds

Ordinary search text is interpreted as literal words, including punctuation; FTS operators are never interpreted. Identifiers are split at camelCase, snake_case and letter/digit boundaries (`HTTPServer` → `HTTP Server`, `utf8Decode` → `utf 8 Decode`) when a symbol is written. Index schema 4 adds these words to an existing index in place on the next `analyze`/`refresh`; until then read-only lookups fall back to name prefixes. Context queries have a two-second deadline; semantic ranking scores at most 1,000 candidates. Local embeddings are hashed word projections of a symbol's split name, signature and file path rather than a learned semantic model. Index schema 5 drops older vectors, which the next `analyze`/`refresh` recomputes.

Indexing has a one-minute deadline, an 8 MiB limit per source file, a 512 MiB total source limit, and a 100,000-file limit. Git path listings are capped at 4 MiB. Limit failures preserve the previous index generation. Valid bodyless Go declarations remain symbols without body traversal. Removed symbols cascade to embeddings; existing orphan embeddings are cleaned on upgrade. Unchanged symbol embeddings are reused. Per-file relation deletes use a `(repo, file)` index, so a cold index grows with the repository rather than with files × relations. Changed sources are hashed and parsed by a bounded worker pool (up to 16 workers, at most 64 sources and 64 MiB of source in flight); a single writer stores their results in walk order, so a generation is identical to a sequential one.

Package-wide Go type checking retains at most 32 MiB of Go source per index (a larger checkout keeps syntax evidence for every Go call), stops starting packages after 20 seconds or 15 seconds before the index deadline (a pass cut short is recorded as partial and completed by the next refresh, even with no changed file), records at most 32 dispatch edges per interface call and 100,000 per repository, and never loads packages outside the analyzed module. A refresh with no changed or deleted file, a complete previous pass and unchanged Go file entries (names and types, including ignored and symlinked files) in every type-checked directory skips resolution; the entry fingerprint is stored in the pass record (`go_types.go_dirs_digest`), so a pass written by a binary that does not record it is redone (on the Go tree's `cmd`, such a refresh takes about 0.14 s instead of 0.24 s, using 24 MB instead of 59 MB of memory, because unchanged files are neither read nor hashed). Measured on this machine: trace-cli (1.3 MB of Go) indexes cold in about 1.5 s; the Go tree's `cmd` (29 MB of Go in 1,472 files) in about 20–28 s with 0.5–0.6 GB peak memory, of which the type pass takes about 4 s, and the Python 3.12 standard library (570 files) in about 5 s. Lexical analysis is linear in file size: body searches are bounded per declaration and brace bodies matched once per file (a 568 KB JavaScript file with 6,000 functions indexes in under 2 s; 40,000 semicolon-less type aliases analyze in under a second).

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
This upgrade (analyzer `inventory-v9`, Tree-sitter `treesitter-v6`) re-analyzes
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
