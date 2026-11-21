package cli

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/UseTrace-SI/trace-graph/internal/graph"
	"github.com/UseTrace-SI/trace-graph/internal/store"
	"github.com/spf13/cobra"
)

// Revision output contains hashes and locations, never raw source/signature
// values from history. Internally the shared comparator compares signature hashes.
type revisionSymbol struct {
	ID            string `json:"id"`
	File          string `json:"file"`
	Name          string `json:"name"`
	Kind          string `json:"kind"`
	Line          int    `json:"line"`
	EndLine       int    `json:"end_line"`
	SignatureHash string `json:"signature_hash"`
	SourceHash    string `json:"source_hash"`
}
type revisionChange struct {
	Kind   string           `json:"kind"`
	File   string           `json:"file"`
	Name   string           `json:"name"`
	Before []revisionSymbol `json:"before"`
	After  []revisionSymbol `json:"after"`
}

func revisionSymbols(input []snapshotSymbol) []revisionSymbol {
	result := make([]revisionSymbol, 0, len(input))
	for _, s := range input {
		result = append(result, revisionSymbol{ID: s.ID, File: s.File, Name: s.Name, Kind: s.Kind, Line: s.Line, EndLine: s.EndLine, SignatureHash: s.Signature, SourceHash: s.SourceHash})
	}
	return result
}
func readRevisionSymbols(ctx context.Context, db *sql.DB, repo string) ([]snapshotSymbol, map[string]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT s.id,s.file,s.name,s.kind,s.line,substr(COALESCE(s.signature,''),1,65537),COALESCE(c.source_hash,''),COALESCE(c.end_line,s.line) FROM symbols s LEFT JOIN symbol_content c ON c.symbol_id=s.id WHERE s.repo=? ORDER BY s.file,s.line,s.id LIMIT 100001`, repo)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	result := []snapshotSymbol{}
	shapes := map[string]string{}
	size := 0
	for rows.Next() {
		var s snapshotSymbol
		if err := rows.Scan(&s.ID, &s.File, &s.Name, &s.Kind, &s.Line, &s.Signature, &s.SourceHash, &s.EndLine); err != nil {
			return nil, nil, err
		}
		if len(s.Signature) > 65536 || len(result) >= 100000 {
			return nil, nil, fmt.Errorf("revision exceeds 64 KiB per signature or 100000 symbols")
		}
		size += len(s.ID) + len(s.File) + len(s.Name) + len(s.Kind) + len(s.Signature) + len(s.SourceHash) + 2*128
		if size > 32<<20 {
			return nil, nil, fmt.Errorf("revision symbol metadata exceeds 32 MiB")
		}
		shapes[s.ID] = graph.SignatureShape(s.Name, s.Signature)
		s.Signature = fmt.Sprintf("%x", sha256.Sum256([]byte(s.Signature)))
		result = append(result, s)
	}
	return result, shapes, rows.Err()
}

type symbolRenameReport struct {
	From revisionSymbol `json:"from"`
	To   revisionSymbol `json:"to"`
	graph.SymbolRename
}

type symbolRenameAmbiguityReport struct {
	Kind    string           `json:"kind"`
	Removed []revisionSymbol `json:"removed"`
	Added   []revisionSymbol `json:"added"`
}

// pairSymbolRenames replaces each removed/added pair that SymbolRenames
// matches with one "renamed" change at the removed symbol's position.
func pairSymbolRenames(differences []symbolChange, base, head graph.RevisionStats, signatures map[string]string) ([]symbolChange, []symbolRenameReport, []symbolRenameAmbiguityReport, bool) {
	var removed, added []graph.RenameSymbol
	position := map[string]int{}
	symbols := map[string]snapshotSymbol{}
	for i, change := range differences {
		switch {
		case change.Kind == "removed" && len(change.Before) == 1:
			s := change.Before[0]
			removed = append(removed, graph.RenameSymbol{ID: s.ID, File: s.File, Name: s.Name, Kind: s.Kind, SignatureShape: signatures["base\x00"+s.ID]})
			position["base\x00"+s.ID], symbols["base\x00"+s.ID] = i, s
		case change.Kind == "added" && len(change.After) == 1:
			s := change.After[0]
			added = append(added, graph.RenameSymbol{ID: s.ID, File: s.File, Name: s.Name, Kind: s.Kind, SignatureShape: signatures["head\x00"+s.ID]})
			position["head\x00"+s.ID], symbols["head\x00"+s.ID] = i, s
		}
	}
	renames, ambiguities, checked := graph.SymbolRenames(base, head, removed, added)
	replace, drop := map[int]symbolChange{}, map[int]bool{}
	reports := []symbolRenameReport{}
	for _, rename := range renames {
		from, to := symbols["base\x00"+rename.FromID], symbols["head\x00"+rename.ToID]
		replace[position["base\x00"+rename.FromID]] = symbolChange{Kind: "renamed", File: to.File, Name: to.Name, Before: []snapshotSymbol{from}, After: []snapshotSymbol{to}}
		drop[position["head\x00"+rename.ToID]] = true
		reports = append(reports, symbolRenameReport{From: revisionSymbols([]snapshotSymbol{from})[0], To: revisionSymbols([]snapshotSymbol{to})[0], SymbolRename: rename})
	}
	paired := make([]symbolChange, 0, len(differences))
	for i, change := range differences {
		if drop[i] {
			continue
		}
		if renamed, ok := replace[i]; ok {
			change = renamed
		}
		paired = append(paired, change)
	}
	ambiguous := []symbolRenameAmbiguityReport{}
	for _, entry := range ambiguities {
		report := symbolRenameAmbiguityReport{Kind: entry.Kind, Removed: []revisionSymbol{}, Added: []revisionSymbol{}}
		for _, id := range entry.Removed {
			report.Removed = append(report.Removed, revisionSymbols([]snapshotSymbol{symbols["base\x00"+id]})...)
		}
		for _, id := range entry.Added {
			report.Added = append(report.Added, revisionSymbols([]snapshotSymbol{symbols["head\x00"+id]})...)
		}
		ambiguous = append(ambiguous, report)
	}
	return paired, reports, ambiguous, checked
}
func compareCmd() *cobra.Command {
	var base, head string
	var detectRenames bool
	cmd := &cobra.Command{Use: "compare --base REF [--head REF]", Short: "Compare symbols in two local Git commits without changing the checkout or index", SilenceUsage: true, SilenceErrors: true, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if base == "" {
			return fmt.Errorf("--base requires a local commit reference")
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
		defer cancel()
		pair, err := graph.PrepareRevisionPair(ctx, repoPath, base, head)
		if err != nil {
			return err
		}
		signatures := map[string]string{}
		analyze := func(side string) ([]snapshotSymbol, graph.RevisionStats, error) {
			db, err := store.Open(":memory:")
			if err != nil {
				return nil, graph.RevisionStats{}, err
			}
			defer db.Close()
			stats, err := pair.Analyze(ctx, side, db)
			if err != nil {
				return nil, stats, err
			}
			symbols, shapes, err := readRevisionSymbols(ctx, db, repoPath)
			for id, shape := range shapes {
				signatures[side+"\x00"+id] = shape
			}
			return symbols, stats, err
		}
		previous, baseStats, err := analyze("base")
		if err != nil {
			return err
		}
		current, headStats, err := analyze("head")
		if err != nil {
			return err
		}
		renameMode := "disabled"
		renames, ambiguous := []graph.FileRename{}, []graph.RenameAmbiguity{}
		renamed := map[string]string{}
		if detectRenames {
			renameMode = "unique_exact_content_same_extension"
			renames, ambiguous = graph.ExactFileRenames(baseStats, headStats)
			for _, rename := range renames {
				renamed[rename.From] = rename.To
			}
		}
		differences := compareSymbolsWithRenames(previous, current, renamed)
		symbolMode := "disabled"
		symbolRenames, symbolAmbiguities := []symbolRenameReport{}, []symbolRenameAmbiguityReport{}
		if detectRenames {
			var similarityChecked bool
			differences, symbolRenames, symbolAmbiguities, similarityChecked = pairSymbolRenames(differences, baseStats, headStats, signatures)
			symbolMode = "name_normalized_body"
			if !similarityChecked {
				symbolMode = "identical_body_only"
			}
		}
		changes := make([]revisionChange, 0, len(differences))
		for _, change := range differences {
			changes = append(changes, revisionChange{Kind: change.Kind, File: change.File, Name: change.Name, Before: revisionSymbols(change.Before), After: revisionSymbols(change.After)})
		}
		// Buffer the bounded report so any failure emits no misleading partial JSON.
		var output compareBuffer
		if err := json.NewEncoder(&output).Encode(map[string]any{"protocol": 1, "repo": repoPath, "base_commit": pair.BaseCommit, "head_commit": pair.HeadCommit, "policy_hash": pair.PolicyHash, "exclusions": "current_and_both_commits", "analysis": graph.Capabilities(), "source_mode": "local_git_objects", "signatures": "sha256_only", "rename_detection": renameMode, "file_renames": renames, "rename_ambiguities": ambiguous, "symbol_rename_detection": symbolMode, "symbol_renames": symbolRenames, "symbol_rename_ambiguities": symbolAmbiguities, "base": baseStats, "head": headStats, "changes": changes}); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err = cmd.OutOrStdout().Write(output.data)
		return err
	}}
	cmd.Flags().StringVar(&base, "base", "", "base commit reference (required; resolved once)")
	cmd.Flags().StringVar(&head, "head", "HEAD", "head commit reference (resolved once)")
	cmd.Flags().BoolVar(&detectRenames, "detect-renames", false, "pair unique exact-content relocated files with the same extension, and renamed symbols by name-normalized body evidence")
	return cmd
}

type compareBuffer struct{ data []byte }

func (b *compareBuffer) Write(data []byte) (int, error) {
	if len(data) > 32*1024*1024-len(b.data) {
		return 0, fmt.Errorf("comparison report exceeds 32 MiB")
	}
	b.data = append(b.data, data...)
	return len(data), nil
}
