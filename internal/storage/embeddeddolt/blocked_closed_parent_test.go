//go:build cgo

package embeddeddolt_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/steveyegge/beads/internal/storage/embeddeddolt"
	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
)

// blockedGraph is the small fixture vocabulary the closed-parent cases share:
// rows and edges go in through the store's own write paths, and the derived
// column is read and (for the stale-data cases) corrupted over a raw
// connection, which is the only way to plant a value no verb produces.
type blockedGraph struct {
	t  *testing.T
	te *testEnv
}

func (g blockedGraph) create(ctx context.Context, id string, wisp bool) {
	g.t.Helper()
	iss := &types.Issue{ID: id, Title: id, Status: types.StatusOpen, Priority: 2, IssueType: types.TypeTask, Ephemeral: wisp}
	if err := g.te.store.CreateIssue(ctx, iss, "tester"); err != nil {
		g.t.Fatalf("create %s: %v", id, err)
	}
}

func (g blockedGraph) dep(ctx context.Context, src, tgt string, typ types.DependencyType) {
	g.t.Helper()
	if err := g.te.store.AddDependency(ctx, &types.Dependency{IssueID: src, DependsOnID: tgt, Type: typ}, "tester"); err != nil {
		g.t.Fatalf("add %s dep %s->%s: %v", typ, src, tgt, err)
	}
}

func (g blockedGraph) close(ctx context.Context, id string) {
	g.t.Helper()
	if err := g.te.store.CloseIssue(ctx, id, "done", "tester", ""); err != nil {
		g.t.Fatalf("close %s: %v", id, err)
	}
}

func (g blockedGraph) isBlocked(ctx context.Context, id string, wisp bool) bool {
	g.t.Helper()
	table := "issues"
	if wisp {
		table = "wisps"
	}
	var v int
	g.te.queryScalar(g.t, ctx, "SELECT is_blocked FROM "+table+" WHERE id = ?", []any{id}, &v)
	return v == 1
}

// inTx runs fn in one committed transaction on a raw connection, so a case can
// drive the issueops core directly against the real engine.
func (g blockedGraph) inTx(ctx context.Context, fn func(tx *sql.Tx)) {
	g.t.Helper()
	db, cleanup, err := embeddeddolt.OpenSQL(ctx, g.te.dataDir, g.te.database, "main")
	if err != nil {
		g.t.Fatalf("OpenSQL: %v", err)
	}
	defer cleanup()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		g.t.Fatalf("begin: %v", err)
	}
	fn(tx)
	if err := tx.Commit(); err != nil {
		g.t.Fatalf("commit: %v", err)
	}
}

// TestClosedParentWithStaleFlagDoesNotPropagate pins the closed/pinned guard on
// the parent-child legs of the should-be-blocked union, in every combination of
// parent and child plane.
//
// issueops.BlockedStateInvariant says a closed or pinned row is never blocked,
// so such a parent has no blockage to hand down. The legs used to read the
// parent's STORED is_blocked with no status filter, which agrees with the
// predicate only while the stored value is settled. A closed parent carrying a
// stale is_blocked = 1 (the merge clause admits exactly that) would mark its
// open children, and nothing would ever unmark them short of repairing the
// parent first. The cases plant the stale flag by hand and drive the three
// consumers of the union: the batched scoped recompute, the doctor count and
// the full repair.
func TestClosedParentWithStaleFlagDoesNotPropagate(t *testing.T) {
	skipUnlessEmbeddedDolt(t)

	for _, tc := range []struct {
		name                  string
		status                types.Status
		parentWisp, childWisp bool
	}{
		{"closed issue parent, issue child", types.StatusClosed, false, false},
		{"closed issue parent, wisp child", types.StatusClosed, false, true},
		{"closed wisp parent, issue child", types.StatusClosed, true, false},
		{"closed wisp parent, wisp child", types.StatusClosed, true, true},
		{"pinned issue parent, issue child", types.StatusPinned, false, false},
		{"pinned wisp parent, wisp child", types.StatusPinned, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t, "cp")
			ctx := t.Context()
			g := blockedGraph{t, te}

			g.create(ctx, "cp-parent", tc.parentWisp)
			g.create(ctx, "cp-child", tc.childWisp)
			g.dep(ctx, "cp-child", "cp-parent", types.DepParentChild)
			if tc.status == types.StatusClosed {
				g.close(ctx, "cp-parent")
			} else if err := te.store.UpdateIssue(ctx, "cp-parent", map[string]interface{}{"status": string(types.StatusPinned)}, "tester"); err != nil {
				t.Fatalf("pin parent: %v", err)
			}

			parentTable := "issues"
			if tc.parentWisp {
				parentTable = "wisps"
			}
			te.exec(t, ctx, "UPDATE "+parentTable+" SET is_blocked = 1 WHERE id = ?", "cp-parent")

			var childIssues, childWisps []string
			if tc.childWisp {
				childWisps = []string{"cp-child"}
			} else {
				childIssues = []string{"cp-child"}
			}

			// Scoped recompute of the child alone: the stale parent is outside
			// the batch, so only the union's own guard can keep the child clear.
			g.inTx(ctx, func(tx *sql.Tx) {
				if err := issueops.RecomputeIsBlockedInTx(ctx, tx, childIssues, childWisps); err != nil {
					t.Fatalf("scoped recompute: %v", err)
				}
			})
			if g.isBlocked(ctx, "cp-child", tc.childWisp) {
				t.Fatalf("open child was marked blocked by a %s parent's stale is_blocked = 1", tc.status)
			}

			// Doctor count: the parent's own orphaned flag is the one stale
			// row. The child is settled and must not be counted as mark-eligible.
			g.inTx(ctx, func(tx *sql.Tx) {
				n, err := issueops.CountIsBlockedInconsistenciesInTx(ctx, tx)
				if err != nil {
					t.Fatalf("count: %v", err)
				}
				if n != 1 {
					t.Fatalf("stale count = %d, want 1 (the parent's own flag only)", n)
				}
			})

			// A child already carrying the inherited flag is unmark-eligible
			// in the same pass as its parent, not one pass later.
			childTable := "issues"
			if tc.childWisp {
				childTable = "wisps"
			}
			te.exec(t, ctx, "UPDATE "+childTable+" SET is_blocked = 1 WHERE id = ?", "cp-child")
			g.inTx(ctx, func(tx *sql.Tx) {
				n, err := issueops.CountIsBlockedInconsistenciesInTx(ctx, tx)
				if err != nil {
					t.Fatalf("count: %v", err)
				}
				if n != 2 {
					t.Fatalf("stale count = %d, want 2 (parent and child)", n)
				}
				fixed, err := issueops.RecomputeAllIsBlockedInTx(ctx, tx)
				if err != nil {
					t.Fatalf("full repair: %v", err)
				}
				if fixed != 2 {
					t.Fatalf("full repair corrected %d rows, want 2", fixed)
				}
			})
			if g.isBlocked(ctx, "cp-parent", tc.parentWisp) || g.isBlocked(ctx, "cp-child", tc.childWisp) {
				t.Fatal("full repair left a stale flag behind")
			}
		})
	}
}
