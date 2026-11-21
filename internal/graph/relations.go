package graph

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

func scopedID(repo, id string) string { return fmt.Sprintf("%x:%s", sha256.Sum256([]byte(repo)), id) }

// goSymbolID is the unscoped identity of a declaration at a physical line.
func goSymbolID(rel, name string, line int) string { return rel + ":" + name + ":" + itoa(line) }

// goRelationID is the unscoped identity of a call relation.
func goRelationID(fromID, callee string, line, column int) string {
	return fromID + "->" + callee + ":" + itoa(line) + ":" + itoa(column)
}

func (w *indexWriter) call(repo, rel, fromID, fromName, toName string, line, column int, hint string) {
	if w.err != nil {
		return
	}
	id := goRelationID(fromID, toName, line, column)
	if _, err := w.Exec("INSERT INTO relations(id,repo,from_symbol,to_symbol,kind,file,line) VALUES(?,?,?,?,?,?,?)", id, repo, fromName, toName, "call", rel, line); err != nil {
		return
	}
	_, _ = w.write("INSERT INTO relation_targets(relation_id,from_id,namespace,hint) VALUES(?,?,?,?) ON CONFLICT(relation_id) DO NOTHING", scopedID(repo, id), scopedID(repo, fromID), w.namespace, hint)
}

// relation writes a relationship other than a Go call. fromID is empty for
// module-level relationships such as imports.
func (w *indexWriter) relation(repo, rel, id, fromID, fromName, toName, kind string, line int, hint string) {
	if w.err != nil {
		return
	}
	if _, err := w.Exec("INSERT INTO relations(id,repo,from_symbol,to_symbol,kind,file,line) VALUES(?,?,?,?,?,?,?)", id, repo, fromName, toName, kind, rel, line); err != nil {
		return
	}
	from := ""
	if fromID != "" {
		from = scopedID(repo, fromID)
	}
	_, _ = w.write("INSERT INTO relation_targets(relation_id,from_id,namespace,hint) VALUES(?,?,?,?) ON CONFLICT(relation_id) DO NOTHING", scopedID(repo, id), from, w.namespace, hint)
}

// Resolve unique package functions and explicitly declared local receiver
// methods. Dynamic dispatch and ambiguous names remain unresolved observations.
func resolveCalls(ctx context.Context, tx *sql.Tx, repo string) error {
	_, err := tx.ExecContext(ctx, `UPDATE relation_targets AS t SET to_id=(
 SELECT min(s.id) FROM symbols s JOIN symbol_scopes scope ON scope.symbol_id=s.id JOIN relations r ON r.id=t.relation_id LEFT JOIN symbol_receivers receiver ON receiver.symbol_id=s.id
 WHERE s.repo=? AND s.name=r.to_symbol AND (
 (s.kind='func' AND t.hint='identifier' AND scope.namespace=t.namespace) OR
 (s.kind='func' AND substr(t.hint,1,8)='package:' AND substr(scope.namespace,1,instr(scope.namespace,'|')-1)=substr(t.hint,9)) OR
 (s.kind='method' AND substr(t.hint,1,9)='receiver:' AND scope.namespace=t.namespace AND receiver.receiver=substr(t.hint,10)))
 HAVING count(*)=1),resolution='unresolved' WHERE relation_id IN (SELECT id FROM relations WHERE repo=?)`, repo, repo)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE relation_targets SET resolution='lexical' WHERE to_id IS NOT NULL AND relation_id IN (SELECT id FROM relations WHERE repo=?)", repo)
	return err
}

func moduleName(repo string) string {
	f, err := os.Open(filepath.Join(repo, "go.mod"))
	if err != nil {
		return ""
	}
	defer f.Close()
	var b [4096]byte
	n, _ := f.Read(b[:])
	return moduleFromText(b[:n])
}

func moduleFromText(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "module" {
			return strings.Trim(fields[1], `"`)
		}
	}
	return ""
}

// rustCharLiteral reports the byte length of a Rust character literal starting
// at the single quote b[i]. Lifetimes and loop labels ('a, 'static, 'outer:)
// are not literals: treating their quote as an opening delimiter used to mask
// every following line up to the next quote, hiding later declarations.
func rustCharLiteral(b []byte, i int) int {
	if i+1 >= len(b) {
		return 0
	}
	if b[i+1] == '\\' {
		// '\n', '\'', '\x7f', '\u{10FFFF}': bounded escape forms.
		for j := i + 3; j < len(b) && j <= i+12; j++ {
			if b[j] == '\'' {
				return j - i + 1
			}
			if b[j] == '\n' {
				return 0
			}
		}
		return 0
	}
	_, width := utf8.DecodeRune(b[i+1:])
	if b[i+1] != '\n' && i+1+width < len(b) && b[i+1+width] == '\'' {
		return width + 2
	}
	return 0
}

// Comment syntax by language. Python's // is floor division and a
// JavaScript or Rust # starts a private member or an attribute, so treating
// both forms as comments everywhere blanked the rest of such lines. Other
// languages keep both forms.
var (
	hashCommentOnly  = wordSetShared("python ruby bash zsh r perl elixir yaml julia")
	slashCommentOnly = wordSetShared("go javascript typescript rust java c cpp csharp kotlin swift scala dart groovy zig protobuf objectivec cue")
)

func wordSetShared(words string) map[string]bool {
	set := map[string]bool{}
	for _, word := range strings.Fields(words) {
		set[word] = true
	}
	return set
}

// Mask quoted literals and comments while preserving byte offsets and lines.
// Inventory matches can then never be declarations pasted into documentation.
func maskInventory(lang, src string) string { return maskSource(lang, src, false) }

// maskSource masks comments and, unless keepStrings, string literals. Kept
// literals are still skipped, so comment markers inside them stay code.
func maskSource(lang, src string, keepStrings bool) string {
	b := []byte(src)
	out := append([]byte(nil), b...)
	comment := func(i int) {
		if out[i] != '\n' && out[i] != '\r' {
			out[i] = ' '
		}
	}
	literal := comment
	if keepStrings {
		literal = func(int) {}
	}
	hashComments, slashComments := !slashCommentOnly[lang], !hashCommentOnly[lang]
	for i := 0; i < len(b); {
		if lang == "rust" && b[i] == '\'' {
			width := rustCharLiteral(b, i)
			if width == 0 {
				i++ // lifetime or label
				continue
			}
			for j := 0; j < width; j++ {
				literal(i)
				i++
			}
			continue
		}
		if (hashComments && b[i] == '#') || (slashComments && b[i] == '/' && i+1 < len(b) && b[i+1] == '/') {
			for i < len(b) && b[i] != '\n' {
				comment(i)
				i++
			}
			continue
		}
		if slashComments && b[i] == '/' && i+1 < len(b) && b[i+1] == '*' {
			comment(i)
			comment(i + 1)
			i += 2
			for i < len(b) {
				if b[i] == '*' && i+1 < len(b) && b[i+1] == '/' {
					comment(i)
					comment(i + 1)
					i += 2
					break
				}
				comment(i)
				i++
			}
			continue
		}
		if b[i] == '\'' || b[i] == '"' || b[i] == '`' {
			quote := b[i]
			width := 1
			if i+2 < len(b) && b[i+1] == quote && b[i+2] == quote {
				width = 3
			}
			for j := 0; j < width; j++ {
				literal(i)
				i++
			}
			for i < len(b) {
				if b[i] == '\\' {
					literal(i)
					i++
					if i < len(b) {
						literal(i)
						i++
					}
					continue
				}
				if b[i] == quote && (width == 1 || (i+2 < len(b) && b[i+1] == quote && b[i+2] == quote)) {
					for j := 0; j < width; j++ {
						literal(i)
						i++
					}
					break
				}
				literal(i)
				i++
			}
			continue
		}
		i++
	}
	return string(out)
}
