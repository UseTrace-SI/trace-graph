//go:build !treesitter

package graph

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/UseTrace-SI/trace-graph/internal/store"
)

func goTypeFixture(t *testing.T, files map[string]string) (*sql.DB, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, src := range files {
		revisionTestWrite(t, root, name, src)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := AnalyzeRepo(root, db); err != nil {
		t.Fatal(err)
	}
	return db, root
}

// unscoped strips the repository hash from a stored symbol ID.
func unscoped(id string) string {
	if i := strings.IndexByte(id, ':'); i >= 0 {
		return id[i+1:]
	}
	return id
}

type goEdge struct{ resolution, target string }

// goEdges maps "from->to@line" to the stored resolution and target for one
// relation kind.
func goEdges(t *testing.T, db *sql.DB, root, kind string) map[string]goEdge {
	t.Helper()
	rows, err := db.Query(`SELECT r.from_symbol,r.to_symbol,r.line,t.resolution,coalesce(t.to_id,'') FROM relations r JOIN relation_targets t ON t.relation_id=r.id WHERE r.repo=? AND r.kind=?`, root, kind)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	edges := map[string]goEdge{}
	for rows.Next() {
		var from, to, resolution, target string
		var line int
		if err := rows.Scan(&from, &to, &line, &resolution, &target); err != nil {
			t.Fatal(err)
		}
		key := from + "->" + to + "@" + itoa(line)
		if previous, ok := edges[key]; ok {
			target = previous.target + "|" + unscoped(target)
		} else {
			target = unscoped(target)
		}
		edges[key] = goEdge{resolution, target}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return edges
}

func expectEdges(t *testing.T, got, want map[string]goEdge) {
	t.Helper()
	for key, edge := range want {
		if got[key] != edge {
			t.Errorf("%s: got %+v, want %+v", key, got[key], edge)
		}
	}
}

// Module packages are type-checked together, so calls resolve through
// factory returns, import aliases, cross-file aliases, promoted methods and
// fields, while interface and type-parameter calls stay dynamic and their
// possible module implementations become dispatch edges.
func TestGoTypesResolveAcrossFilesAndPackages(t *testing.T) {
	db, root := goTypeFixture(t, map[string]string{
		"go.mod": "module example.test/m\n\ngo 1.26\n",
		"store/store.go": `package store

type Store interface {
	Get(key string) string
}

type Memory struct{ data map[string]string }

func New() *Memory { return &Memory{data: map[string]string{}} }

func (m *Memory) Get(key string) string { return m.data[key] }

type Base struct{}

func (Base) Describe() string { return "base" }

type Cached struct {
	Base
	inner *Memory
}

func (c *Cached) Get(key string) string { return c.inner.Get(key) }
`,
		"store/alias.go": "package store\n\ntype Alias = Memory\n",
		"app/app.go": `package app

import s "example.test/m/store"

type Handler struct{ store s.Store }

func Run(h Handler) string {
	m := s.New()
	m.Get("a")
	var a s.Alias = *m
	a.Get("b")
	c := &s.Cached{}
	c.Describe()
	h.store.Get("c")
	return Generic(m)
}

func Generic[T interface{ Get(string) string }](v T) string { return v.Get("d") }
`,
		"app/app_test.go": `package app_test

import (
	"testing"

	"example.test/m/app"
)

func TestRun(t *testing.T) { app.Run(app.Handler{}) }
`,
	})
	expectEdges(t, goEdges(t, db, root, "call"), map[string]goEdge{
		"Run->New@8":       {"typed", "store/store.go:New:9"},
		"Run->Get@9":       {"typed", "store/store.go:Get:11"},
		"Run->Get@11":      {"typed", "store/store.go:Get:11"},
		"Run->Describe@13": {"typed", "store/store.go:Describe:15"},
		"Run->Get@14":      {"dynamic", ""},
		"Run->Generic@15":  {"typed", "app/app.go:Generic:18"},
		"Generic->Get@18":  {"dynamic", ""},
		"Get->Get@22":      {"typed", "store/store.go:Get:11"},
		"TestRun->Run@9":   {"typed", "app/app.go:Run:7"},
	})
	expectEdges(t, goEdges(t, db, root, "implements"), map[string]goEdge{
		"Memory->Store@7":  {"typed", "store/store.go:Store:3"},
		"Cached->Store@17": {"typed", "store/store.go:Store:3"},
	})
	expectEdges(t, goEdges(t, db, root, "dispatch"), map[string]goEdge{
		"Run->Get@14": {"typed", "store/store.go:Get:22|store/store.go:Get:11"},
	})
	var detail string
	if err := db.QueryRow("SELECT detail FROM repo_resolution WHERE repo=?", root).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	var stats goTypeStats
	if err := json.Unmarshal([]byte(detail), &stats); err != nil || stats.Status != "complete" || stats.Packages != 3 || stats.Implements != 2 || stats.Dispatch != 2 {
		t.Fatalf("unexpected type pass stats: %s %v", detail, err)
	}
}

// Packages outside the module are never loaded. A promoted method that an
// embedded type of unknown origin could shadow, calls on unknown types and
// interfaces whose method set includes an unknown embedded interface must
// not produce typed edges.
func TestGoTypesKeepUnknownDependenciesUnresolved(t *testing.T) {
	db, root := goTypeFixture(t, map[string]string{
		"go.mod": "module example.test/m\n\ngo 1.26\n",
		"p/p.go": `package p

import (
	"strings"

	"ext.example/dep"
)

type Wrapper struct {
	dep.Base
	inner
}

type inner struct{ deep }

type deep struct{}

func (deep) Run() {}

type Reader interface {
	dep.Closer
	Run()
}

type Impl struct{}

func (Impl) Run() {}

func Use(w Wrapper, x dep.Thing) string {
	w.Run()
	w.inner.Run()
	x.Do()
	return strings.ToUpper("a")
}
`,
	})
	expectEdges(t, goEdges(t, db, root, "call"), map[string]goEdge{
		"Use->Run@30":     {"unresolved", ""},
		"Use->Run@31":     {"typed", "p/p.go:Run:18"},
		"Use->Do@32":      {"unresolved", ""},
		"Use->ToUpper@33": {"unresolved", ""},
	})
	if edges := goEdges(t, db, root, "implements"); len(edges) != 0 {
		t.Fatalf("implements edge through an incomplete method set: %+v", edges)
	}
}

// Build constraints select one platform variant, which then resolves; plain
// duplicate declarations stay ambiguous instead of resolving to the first.
func TestGoTypesFollowBuildConstraintsAndRefuseDuplicates(t *testing.T) {
	db, root := goTypeFixture(t, map[string]string{
		"p/windows.go": "//go:build windows\n\npackage p\n\nfunc platform() string { return \"windows\" }\n",
		"p/other.go":   "//go:build !windows\n\npackage p\n\nfunc platform() string { return \"other\" }\n",
		"p/a.go":       "package p\n\nfunc helper() {}\n",
		"p/b.go":       "package p\n\nfunc helper() {}\n",
		"p/use.go":     "package p\n\nfunc Use() string {\n\thelper()\n\treturn platform()\n}\n",
	})
	want := "p/other.go:platform:5"
	if runtime.GOOS == "windows" {
		want = "p/windows.go:platform:5"
	}
	expectEdges(t, goEdges(t, db, root, "call"), map[string]goEdge{
		"Use->helper@4":   {"unresolved", ""},
		"Use->platform@5": {"typed", want},
	})
}

// An unchanged refresh keeps the complete previous resolution instead of
// recomputing it; a change recomputes cross-file evidence.
func TestGoTypesUnchangedRefreshKeepsResolution(t *testing.T) {
	db, root := goTypeFixture(t, map[string]string{
		"p/a.go": "package p\n\ntype T struct{}\n\nfunc (T) Run() {}\n",
		"p/b.go": "package p\n\nfunc Make() T { return T{} }\n\nfunc Use() { Make().Run() }\n",
	})
	want := map[string]goEdge{"Use->Run@5": {"typed", "p/a.go:Run:5"}}
	expectEdges(t, goEdges(t, db, root, "call"), want)
	if n, err := AnalyzeRepo(root, db); err != nil || n != 0 {
		t.Fatalf("unchanged refresh: %d %v", n, err)
	}
	expectEdges(t, goEdges(t, db, root, "call"), want)
	// Only a.go changes; b.go's cached call must follow the new method.
	revisionTestWrite(t, root, "p/a.go", "package p\n\ntype T struct{}\n\n\nfunc (T) Run() {}\n")
	if _, err := AnalyzeRepo(root, db); err != nil {
		t.Fatal(err)
	}
	expectEdges(t, goEdges(t, db, root, "call"), map[string]goEdge{"Use->Run@5": {"typed", "p/a.go:Run:6"}})
}

// Over the source budget the type pass is skipped deterministically and every
// call keeps its syntax evidence.
func TestGoTypesSourceBudgetKeepsSyntaxEvidence(t *testing.T) {
	previous := goTypeSourceBudget
	goTypeSourceBudget = 16
	t.Cleanup(func() { goTypeSourceBudget = previous })
	db, root := goTypeFixture(t, map[string]string{
		"p/p.go": "package p\n\ntype T struct{}\n\nfunc (T) Run() {}\n\nfunc Make() T { return T{} }\n\nfunc Use(t T) {\n\tt.Run()\n\tMake().Run()\n}\n",
	})
	expectEdges(t, goEdges(t, db, root, "call"), map[string]goEdge{
		"Use->Run@10":  {"lexical", "p/p.go:Run:5"},
		"Use->Make@11": {"lexical", "p/p.go:Make:7"},
		"Use->Run@11":  {"unresolved", ""},
	})
	var detail string
	if err := db.QueryRow("SELECT detail FROM repo_resolution WHERE repo=?", root).Scan(&detail); err != nil || !strings.Contains(detail, `"reason":"go_source_budget"`) {
		t.Fatalf("budget skip not recorded: %s %v", detail, err)
	}
}

// Without time left before the index deadline the pass records itself as
// partial and keeps syntax evidence; the next refresh, even with no changed
// file, completes it.
func TestGoTypesDeadlineReserveDefersToNextRefresh(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	revisionTestWrite(t, root, "p/p.go", "package p\n\ntype T struct{}\n\nfunc (T) Run() {}\n\nfunc Make() T { return T{} }\n\nfunc Use() { Make().Run() }\n")
	db, err := store.Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), goTypeReserve-5*time.Second)
	defer cancel()
	if _, err := AnalyzeRepoContext(ctx, root, db); err != nil {
		t.Fatal(err)
	}
	var complete int
	var detail string
	if err := db.QueryRow("SELECT complete,detail FROM repo_resolution WHERE repo=?", root).Scan(&complete, &detail); err != nil || complete != 0 || !strings.Contains(detail, `"reason":"time_budget"`) {
		t.Fatalf("partial pass not recorded: %d %s %v", complete, detail, err)
	}
	expectEdges(t, goEdges(t, db, root, "call"), map[string]goEdge{"Use->Run@9": {"unresolved", ""}})
	if _, err := AnalyzeRepo(root, db); err != nil {
		t.Fatal(err)
	}
	expectEdges(t, goEdges(t, db, root, "call"), map[string]goEdge{"Use->Run@9": {"typed", "p/p.go:Run:5"}})
}

// Route registrations become route edges whose handler resolves like a call
// of it: method values and functions through the type checker, inline
// handlers stay unresolved, and lookalike calls without a literal path are
// not routes.
func TestGoRoutesResolveHandlers(t *testing.T) {
	db, root := goTypeFixture(t, map[string]string{
		"go.mod": "module example.test/m\n\ngo 1.26\n",
		"api/api.go": `package api

import "net/http"

type Server struct{}

func (s *Server) users(w http.ResponseWriter, r *http.Request) {}

func health(w http.ResponseWriter, r *http.Request) {}

func Routes(s *Server, mux *http.ServeMux, store map[string]int, r Router) {
	mux.HandleFunc("GET /users", s.users)
	mux.HandleFunc("/health", health)
	http.HandleFunc("/inline", func(w http.ResponseWriter, r *http.Request) {})
	mux.Handle("/files/", http.FileServer(http.Dir(".")))
	r.Get("/chi", health)
	r.Get("key", health)
}

type Router interface{ Get(string, http.HandlerFunc) }
`,
	})
	got := goEdges(t, db, root, "route")
	want := map[string]goEdge{
		"GET /users->users@12":   {"typed", "api/api.go:users:7"},
		"* /health->health@13":   {"typed", "api/api.go:health:9"},
		"* /inline->(inline)@14": {"unresolved", ""},
		"* /files/->(inline)@15": {"unresolved", ""},
		"GET /chi->health@16":    {"typed", "api/api.go:health:9"},
	}
	if len(got) != len(want) {
		t.Fatalf("routes %v, want %v", got, want)
	}
	expectEdges(t, got, want)
}
