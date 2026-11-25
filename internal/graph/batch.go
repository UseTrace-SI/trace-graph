package graph

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"strings"

	"github.com/UseTrace-SI/trace-graph/internal/ident"
)

type observationKind uint8

const (
	observeSymbol observationKind = iota
	observeContent
	observeReceiver
	observeRelation
)

// observation is one typed row recorded by a parser. IDs are unscoped; the
// writer scopes them to the repository identity when it stores the batch.
type observation struct {
	kind      observationKind
	id        string
	namespace string
	line      int
	// Symbols.
	name, symbolKind, signature string
	// Content and receivers.
	hash     string
	endLine  int
	receiver string
	// Relations.
	fromID, fromName, toName, relationKind, hint string
}

// fileBatch collects the observations of one source file. Parsers never touch
// the database, so files can be analyzed concurrently and a file that cannot
// be stored leaves no partial rows behind. The first recording error is kept
// and later observations are ignored.
type fileBatch struct {
	rel string
	// namespace scopes the symbols and relation targets recorded next.
	namespace      string
	moduleOverride *string
	rows           []observation
	ids            map[string]bool
	// strict batches fail on colliding identities (revision comparisons);
	// others drop the later declaration, see symbol.
	strict bool
	// collisions lists dropped duplicate identities in recording order;
	// dropping is the identity whose observations are currently ignored.
	collisions []string
	dropping   string
	// shapes collects rename evidence during revision analysis only.
	shapes map[string]symbolShape
	err    error
}

// collisionError reports two declarations sharing a file, name and line.
// Identities are derived from those three values, so storing both would
// silently replace the first (losing its signature and receiver data and
// producing wrong edges).
type collisionError struct{ id string }

func (e *collisionError) Error() string {
	return fmt.Sprintf("repository contains colliding declaration identities (%s); separate same-name declarations onto different lines", e.id)
}

// collisionWarning describes the declarations a batch dropped.
func (b *fileBatch) collisionWarning() string {
	if len(b.collisions) == 0 {
		return ""
	}
	return fmt.Sprintf("colliding declaration identities (%s): kept the first declaration of each and dropped %d later one(s) with their content, receivers and relationships; separate same-name declarations onto different lines", strings.Join(b.collisions, ", "), len(b.collisions))
}

// symbol records a declaration. A second declaration with the same identity
// (same file, name and line) cannot be stored without replacing the first,
// so in a non-strict batch it is dropped together with the observations
// recorded for it until the next declaration: every parser records a
// declaration's content, receiver and calls right after the declaration
// itself. Declaration-aware lexical analysis never yields duplicates.
func (b *fileBatch) symbol(id, name, kind string, line int, signature string) {
	if b.err != nil {
		return
	}
	if b.ids == nil {
		b.ids = map[string]bool{}
	}
	if b.ids[id] {
		if b.strict {
			b.err = &collisionError{id: id}
			return
		}
		b.collisions = append(b.collisions, id)
		b.dropping = id
		return
	}
	b.dropping = ""
	b.ids[id] = true
	b.rows = append(b.rows, observation{kind: observeSymbol, id: id, namespace: b.namespace, name: name, symbolKind: kind, line: line, signature: signature})
}

// ignored reports observations of a dropped duplicate declaration.
func (b *fileBatch) ignored(id string) bool {
	return b.err != nil || (b.dropping != "" && id == b.dropping)
}

func (b *fileBatch) content(id string, src []byte, endLine int) {
	if b.ignored(id) {
		return
	}
	b.rows = append(b.rows, observation{kind: observeContent, id: id, hash: fmt.Sprintf("%x", sha256.Sum256(src)), endLine: endLine})
	if b.shapes != nil {
		if file, name := splitSymbolID(id); renameShapeLanguage(file) {
			b.shapes[id] = shapeOf(shapeLanguage(file), name, src)
		}
	}
}

func (b *fileBatch) receiverOf(id, receiver string) {
	if b.ignored(id) {
		return
	}
	b.rows = append(b.rows, observation{kind: observeReceiver, id: id, receiver: receiver})
}

// call records a call relation observed inside fromID.
func (b *fileBatch) call(fromID, fromName, toName string, line, column int, hint string) {
	b.relation(goRelationID(fromID, toName, line, column), fromID, fromName, toName, "call", line, hint)
}

// relation records a relationship. fromID is empty for module-level
// relationships such as imports.
func (b *fileBatch) relation(id, fromID, fromName, toName, kind string, line int, hint string) {
	if b.err != nil || (fromID != "" && b.ignored(fromID)) {
		return
	}
	b.rows = append(b.rows, observation{kind: observeRelation, id: id, namespace: b.namespace, line: line, fromID: fromID, fromName: fromName, toName: toName, relationKind: kind, hint: hint})
}

// indexWriter is the single writer of a generation. It stores batches in the
// order it receives them and preserves the first database error; the
// surrounding transaction is never committed on failure.
type indexWriter struct {
	tx             *sql.Tx
	err            error
	moduleOverride *string
	maxRecords     int
	records        int
	// shapes collects rename evidence during revision analysis only.
	shapes map[string]symbolShape
	stmts  map[string]*sql.Stmt
}

// batch starts a file batch configured like the writer.
func (w *indexWriter) batch(rel string) *fileBatch {
	b := &fileBatch{rel: rel, moduleOverride: w.moduleOverride}
	if w.shapes != nil {
		// Revision comparisons refuse ambiguous identities.
		b.strict = true
		b.shapes = map[string]symbolShape{}
	}
	return b
}

const (
	insertSymbol         = "INSERT INTO symbols(id,repo,file,name,kind,line,signature,terms) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET signature=excluded.signature,kind=excluded.kind"
	insertSymbolScope    = "INSERT INTO symbol_scopes(symbol_id,repo,namespace) VALUES(?,?,?) ON CONFLICT(symbol_id) DO UPDATE SET namespace=excluded.namespace"
	insertSymbolContent  = "INSERT INTO symbol_content(symbol_id,source_hash,end_line) VALUES(?,?,?) ON CONFLICT(symbol_id) DO UPDATE SET source_hash=excluded.source_hash,end_line=excluded.end_line"
	insertSymbolReceiver = "INSERT INTO symbol_receivers(symbol_id,receiver) VALUES(?,?) ON CONFLICT(symbol_id) DO UPDATE SET receiver=excluded.receiver"
	insertRelation       = "INSERT INTO relations(id,repo,from_symbol,to_symbol,kind,file,line) VALUES(?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING"
	insertRelationTarget = "INSERT INTO relation_targets(relation_id,from_id,namespace,hint) VALUES(?,?,?,?) ON CONFLICT(relation_id) DO NOTHING"
)

// write is the single counted write path. Symbol metadata rows share the
// observation cap with symbols and relations, otherwise the stated row budget
// would be exceeded roughly threefold by scopes, content and targets alone.
func (w *indexWriter) write(query string, args ...any) {
	if w.err != nil {
		return
	}
	if w.maxRecords > 0 && w.records >= w.maxRecords {
		w.err = fmt.Errorf("analysis exceeds %d observation rows", w.maxRecords)
		return
	}
	w.records++
	stmt := w.stmts[query]
	if stmt == nil {
		stmt, w.err = w.tx.Prepare(query)
		if w.err != nil {
			return
		}
		if w.stmts == nil {
			w.stmts = map[string]*sql.Stmt{}
		}
		w.stmts[query] = stmt
	}
	_, w.err = stmt.Exec(args...)
}

// flush stores a batch in recording order. A batch that failed to record is
// returned as its error without writing anything.
func (w *indexWriter) flush(repo string, b *fileBatch) error {
	if w.err != nil {
		return w.err
	}
	if b.err != nil {
		return b.err
	}
	// Symbol and relation IDs must be unique across repositories in one DB.
	prefix := scopedID(repo, "")
	for i := range b.rows {
		o := &b.rows[i]
		id := prefix + o.id
		switch o.kind {
		case observeSymbol:
			w.write(insertSymbol, id, repo, b.rel, o.name, o.symbolKind, o.line, o.signature, ident.Terms(o.name))
			w.write(insertSymbolScope, id, repo, o.namespace)
		case observeContent:
			w.write(insertSymbolContent, id, o.hash, o.endLine)
		case observeReceiver:
			w.write(insertSymbolReceiver, id, o.receiver)
		case observeRelation:
			from := ""
			if o.fromID != "" {
				from = prefix + o.fromID
			}
			w.write(insertRelation, id, repo, o.fromName, o.toName, o.relationKind, b.rel, o.line)
			w.write(insertRelationTarget, id, from, o.namespace, o.hint)
		}
		if w.err != nil {
			return w.err
		}
	}
	for id, shape := range b.shapes {
		w.shapes[prefix+id] = shape
	}
	return nil
}

// close releases the prepared statements of the writer.
func (w *indexWriter) close() {
	for _, stmt := range w.stmts {
		stmt.Close()
	}
	w.stmts = nil
}
