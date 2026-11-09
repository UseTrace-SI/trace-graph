package graph

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func scopedID(repo, id string) string { return fmt.Sprintf("%x:%s", sha256.Sum256([]byte(repo)), id) }

func (w *indexWriter) call(repo, rel, fromID, fromName, toName string, line, column int, hint string) {
	if w.err != nil {
		return
	}
	id := fromID + "->" + toName + ":" + fmt.Sprint(line) + ":" + fmt.Sprint(column)
	if _, err := w.Exec("INSERT INTO relations(id,repo,from_symbol,to_symbol,kind,file,line) VALUES(?,?,?,?,?,?,?)", id, repo, fromName, toName, "call", rel, line); err != nil {
		return
	}
	_, w.err = w.tx.Exec("INSERT INTO relation_targets(relation_id,from_id,namespace,hint) VALUES(?,?,?,?) ON CONFLICT(relation_id) DO NOTHING", scopedID(repo, id), scopedID(repo, fromID), w.namespace, hint)
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

// Mask quoted literals and comments while preserving byte offsets and lines.
// Inventory matches can then never be declarations pasted into documentation.
func maskInventory(src string) string {
	b := []byte(src)
	out := append([]byte(nil), b...)
	mask := func(i int) {
		if out[i] != '\n' && out[i] != '\r' {
			out[i] = ' '
		}
	}
	for i := 0; i < len(b); {
		if b[i] == '#' || (b[i] == '/' && i+1 < len(b) && b[i+1] == '/') {
			for i < len(b) && b[i] != '\n' {
				mask(i)
				i++
			}
			continue
		}
		if b[i] == '/' && i+1 < len(b) && b[i+1] == '*' {
			mask(i)
			mask(i + 1)
			i += 2
			for i < len(b) {
				if b[i] == '*' && i+1 < len(b) && b[i+1] == '/' {
					mask(i)
					mask(i + 1)
					i += 2
					break
				}
				mask(i)
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
				mask(i)
				i++
			}
			for i < len(b) {
				if b[i] == '\\' {
					mask(i)
					i++
					if i < len(b) {
						mask(i)
						i++
					}
					continue
				}
				if b[i] == quote && (width == 1 || (i+2 < len(b) && b[i+1] == quote && b[i+2] == quote)) {
					for j := 0; j < width; j++ {
						mask(i)
						i++
					}
					break
				}
				mask(i)
				i++
			}
			continue
		}
		i++
	}
	return string(out)
}
