//go:build cgo

package embeddeddolt_test

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/beads/internal/storage/embeddeddolt"
	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
)

// BenchmarkCloseAffectedSetAndRecompute measures the derived-state half of
// closing a parent against a real embedded Dolt engine: build the affected set
// (issueops.AffectedByStatusChangeInTx), move the status, settle is_blocked over
// the set (issueops.RecomputeIsBlockedInTx). Each iteration runs in its own
// transaction and rolls back, so every iteration sees the same graph. It uses
// only API that predates the affected-set narrowing, so the same file measures
// the old walk when dropped onto an older tree.
//
// The parent is blocked by an open blocker, so its live descendants carry an
// inherited flag that the close really has to clear. Shapes:
//
//   - closed_children/N: N children, all closed — the finished-epic close that
//     used to recompute N settled rows (gastownhall/beads#5427, #5939).
//   - mostly_open/N: N children, every tenth closed — the walk's width barely
//     changes, so this isolates the per-parent vs per-level query cost.
//   - wide_level/250x1: 250 open children with one open grandchild each, so one
//     level holds more than queryBatchSize (200) parents.
//
// Each shape runs over COMMITTED data (the fixture is flushed to Dolt history,
// as a fresh `bd` process sees it) and over an UNCOMMITTED WORKING SET (the
// fixture left in the working set). The rows_affected_set metric is the size of
// the set handed to the recompute. Set BEADS_TEST_EMBEDDED_DOLT=1 to run.
func BenchmarkCloseAffectedSetAndRecompute(b *testing.B) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		b.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt benchmarks")
	}

	shapes := []struct {
		name                  string
		children, closedEvery int // closedEvery: 1 = all closed, 10 = every tenth
		grandchildren         bool
	}{
		{"closed_children/50", 50, 1, false},
		{"closed_children/500", 500, 1, false},
		{"mostly_open/50", 50, 10, false},
		{"mostly_open/500", 500, 10, false},
		{"wide_level/250x1", 250, 0, true},
	}
	for _, shape := range shapes {
		for _, committed := range []bool{true, false} {
			mode := "working_set"
			if committed {
				mode = "committed"
			}
			b.Run(shape.name+"/"+mode, func(b *testing.B) {
				ctx := b.Context()
				beadsDir := filepath.Join(b.TempDir(), ".beads")
				store, err := embeddeddolt.Open(ctx, beadsDir, "bench", "main")
				if err != nil {
					b.Fatalf("open embedded Dolt store: %v", err)
				}
				b.Cleanup(func() {
					if err := store.Close(); err != nil {
						b.Errorf("close embedded Dolt store: %v", err)
					}
				})
				if err := store.SetConfig(ctx, "issue_prefix", "bench"); err != nil {
					b.Fatalf("set issue_prefix: %v", err)
				}

				mk := func(id string, parent string) *types.Issue {
					iss := &types.Issue{ID: id, Title: id, Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask}
					if parent != "" {
						iss.Dependencies = []*types.Dependency{{IssueID: id, DependsOnID: parent, Type: types.DepParentChild}}
					}
					return iss
				}
				if err := store.CreateIssues(ctx, []*types.Issue{mk("bench-blocker", ""), mk("bench-parent", "")}, "bench"); err != nil {
					b.Fatalf("create roots: %v", err)
				}
				var kids, grandkids []*types.Issue
				for i := 0; i < shape.children; i++ {
					kid := fmt.Sprintf("bench-kid-%d", i)
					kids = append(kids, mk(kid, "bench-parent"))
					if shape.grandchildren {
						grandkids = append(grandkids, mk(kid+"-g", kid))
					}
				}
				if err := store.CreateIssues(ctx, kids, "bench"); err != nil {
					b.Fatalf("create children: %v", err)
				}
				if len(grandkids) > 0 {
					if err := store.CreateIssues(ctx, grandkids, "bench"); err != nil {
						b.Fatalf("create grandchildren: %v", err)
					}
				}

				// The embedded engine is single-writer: a raw connection is
				// opened only while the store is idle, and closed before the
				// store writes again.
				raw := func(fn func(db *sql.DB)) {
					b.Helper()
					db, cleanup, err := embeddeddolt.OpenSQL(ctx, filepath.Join(beadsDir, "embeddeddolt"), "bench", "main")
					if err != nil {
						b.Fatalf("OpenSQL: %v", err)
					}
					defer func() { _ = cleanup() }()
					fn(db)
				}

				// Close the settled children the cheap way: nothing is blocked
				// yet, so status alone is a consistent close.
				if shape.closedEvery > 0 {
					raw(func(db *sql.DB) {
						for i := 0; i < shape.children; i += shape.closedEvery {
							if _, err := db.ExecContext(ctx, "UPDATE issues SET status = 'closed', closed_at = NOW() WHERE id = ?", fmt.Sprintf("bench-kid-%d", i)); err != nil {
								b.Fatalf("close child: %v", err)
							}
						}
					})
				}
				if err := store.AddDependency(ctx, &types.Dependency{IssueID: "bench-parent", DependsOnID: "bench-blocker", Type: types.DepBlocks}, "bench"); err != nil {
					b.Fatalf("block parent: %v", err)
				}
				if committed {
					if err := store.Commit(ctx, "bench fixture"); err != nil {
						b.Fatalf("commit fixture: %v", err)
					}
				}

				raw(func(db *sql.DB) {
					var edges, dirty int
					if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dependencies WHERE type = 'parent-child'").Scan(&edges); err != nil {
						b.Fatalf("count edges: %v", err)
					}
					if want := shape.children + len(grandkids); edges != want {
						b.Fatalf("fixture has %d parent-child edges, want %d", edges, want)
					}
					if n, err := issueops.CountIsBlockedInconsistenciesInTx(ctx, db); err != nil || n != 0 {
						b.Fatalf("fixture is not settled: stale = %d, err = %v", n, err)
					}
					if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dolt_status WHERE table_name IN ('issues', 'dependencies')").Scan(&dirty); err != nil {
						b.Fatalf("read dolt_status: %v", err)
					}
					if committed != (dirty == 0) {
						b.Fatalf("mode %s but %d graph tables are dirty", mode, dirty)
					}

					var setSize int
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						setSize = closeParentOnce(b, db)
					}
					b.StopTimer()
					b.ReportMetric(float64(setSize), "rows_affected_set")
				})
			})
		}
	}
}

func closeParentOnce(b *testing.B, db *sql.DB) int {
	b.Helper()
	ctx := b.Context()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		b.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	issues, wisps, err := issueops.AffectedByStatusChangeInTx(ctx, tx, "bench-parent")
	if err != nil {
		b.Fatalf("affected set: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE issues SET status = 'closed', closed_at = NOW() WHERE id = 'bench-parent'"); err != nil {
		b.Fatalf("close parent: %v", err)
	}
	if err := issueops.RecomputeIsBlockedInTx(ctx, tx, issues, wisps); err != nil {
		b.Fatalf("recompute: %v", err)
	}
	return len(issues) + len(wisps)
}
