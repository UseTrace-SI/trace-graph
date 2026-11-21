//go:build !treesitter

package graph

import (
	"testing"
)

// The blank identifier declares nothing, but every `_` was indexed as a
// symbol named "_". `var _, _ = pair()` (valid Go, found in the Go tree's
// cmd/cgo tests) then produced two identical identities on one line and the
// collision guard failed the entire index.
func TestRegressionBlankIdentifiersAreNotSymbols(t *testing.T) {
	src := "package p\n\ntype I interface{ M() }\n\ntype T struct{}\n\nfunc (T) M() {}\n\nvar _ I = T{}\nvar _, _ = pair()\n\nfunc pair() (int, int) { return 1, 2 }\n\nfunc _() { pair() }\n\ntype _ struct{}\n\nconst (\n\t_ = iota\n\tFirst\n)\n"
	db, root := goTypeFixture(t, map[string]string{"p.go": src})
	for name, want := range map[string]int{"_": 0, "I": 1, "T": 1, "M": 1, "pair": 1, "First": 1} {
		var got int
		if err := db.QueryRow("SELECT count(*) FROM symbols WHERE repo=? AND name=?", root, name).Scan(&got); err != nil || got != want {
			t.Errorf("%s: %d symbols, want %d (%v)", name, got, want, err)
		}
	}
	var calls int
	if err := db.QueryRow("SELECT count(*) FROM relations WHERE repo=? AND from_symbol='_'", root).Scan(&calls); err != nil || calls != 0 {
		t.Errorf("blank function produced %d relations (%v)", calls, err)
	}
}
