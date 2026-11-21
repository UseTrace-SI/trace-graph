//go:build treesitter

package graph

import (
	"context"
	"database/sql"
)

// Tree-sitter builds keep their own Go symbol identities, which the type
// pass does not target; Go calls there remain observed and unresolved.
const goTypesEnabled = false

func resolveGoTypes(context.Context, *sql.Tx, string, string, []goSource, bool) (goTypeStats, error) {
	return goTypeStats{Status: "disabled", Reason: "tree_sitter_build"}, nil
}
