package dolt

import (
	"context"
	"database/sql"
	"slices"
	"testing"

	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
)

// seedSiblingBlockers creates rb-a and rb-b, both blocking rb-c.
func seedSiblingBlockers(t *testing.T, ctx context.Context, store *DoltStore) {
	t.Helper()
	for _, id := range []string{"rb-a", "rb-b", "rb-c"} {
		createPerm(t, ctx, store, id)
	}
	addDependency(t, ctx, store, "rb-c", "rb-a", types.DepBlocks)
	addDependency(t, ctx, store, "rb-c", "rb-b", types.DepBlocks)
	assertIsBlocked(t, ctx, store, "issues", "rb-c", true)
}

// beginRacingClose opens a transaction on its own pooled connection, pins its
// snapshot by reading both blockers, and closes id inside it with the ordinary
// in-transaction recompute, exactly as the store's close paths do. It returns
// the dependents that close recorded for the post-commit recheck and a commit
// step that lands the close and frees the connection.
func beginRacingClose(t *testing.T, ctx context.Context, store *DoltStore, id string) (pending issueops.BlockedRecheck, commit func()) {
	t.Helper()
	conn, err := store.db.Conn(ctx)
	if err != nil {
		t.Fatalf("acquire connection for %s: %v", id, err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx for %s: %v", id, err)
	}
	clearScope := issueops.ScopeBlockedRecheckTransaction(tx)
	defer clearScope()
	var open int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM issues WHERE id IN ('rb-a', 'rb-b') AND status <> 'closed'").Scan(&open); err != nil {
		t.Fatalf("pin snapshot for %s: %v", id, err)
	}
	if open != 2 {
		t.Fatalf("snapshot for %s sees %d open blockers, want 2", id, open)
	}
	if _, err := issueops.CloseIssueInTx(ctx, tx, id, "done", "tester", ""); err != nil {
		t.Fatalf("close %s in tx: %v", id, err)
	}
	return issueops.TakeBlockedRecheck(tx), func() {
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit close of %s: %v", id, err)
		}
		_ = conn.Close()
	}
}

// TestCloseRecheckBlocked_ConcurrentSiblingCloses is gastownhall/beads#6716:
// two closes of sibling blockers whose transactions both began before either
// committed each see the other blocker open, so neither writes the dependent's
// row and both commit without a conflicting cell. The in-transaction recompute
// cannot see the other close; the post-commit recheck over the same ids can.
func TestCloseRecheckBlocked_ConcurrentSiblingCloses(t *testing.T) {
	store, cleanup := setupConcurrentTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()
	seedSiblingBlockers(t, ctx, store)

	pendingA, commitA := beginRacingClose(t, ctx, store, "rb-a")
	pendingB, commitB := beginRacingClose(t, ctx, store, "rb-b")
	for _, pending := range []issueops.BlockedRecheck{pendingA, pendingB} {
		if !slices.Contains(pending.IssueIDs, "rb-c") || slices.Contains(pending.IssueIDs, "rb-a") || slices.Contains(pending.IssueIDs, "rb-b") {
			t.Fatalf("recorded recheck ids = %v, want the dependent rb-c and never the closed issue", pending.IssueIDs)
		}
	}

	commitA()
	assertIsBlocked(t, ctx, store, "issues", "rb-c", true)
	if err := store.recheckBlockedAfterCommit(ctx, pendingA); err != nil {
		t.Fatalf("recheck after closing rb-a: %v", err)
	}
	// rb-b is still open on every committed snapshot: rb-c stays blocked.
	assertIsBlocked(t, ctx, store, "issues", "rb-c", true)

	commitB()
	// The stale state the in-transaction recompute leaves behind.
	assertIsBlocked(t, ctx, store, "issues", "rb-c", true)
	if err := store.recheckBlockedAfterCommit(ctx, pendingB); err != nil {
		t.Fatalf("recheck after closing rb-b: %v", err)
	}
	assertIsBlocked(t, ctx, store, "issues", "rb-c", false)
	requireCleanTables(ctx, t, store, "issues")
	if !doltHasCommitMessage(ctx, t, store, "bd: recheck blocked after close of rb-b") {
		t.Fatal("the recheck corrected rb-c but minted no Dolt commit for it")
	}
}

// TestCloseRecheckBlocked_PublicClosePaths covers the store methods that own
// the recheck: a close with dependents leaves the dependent settled and the
// working set clean, and a close with no dependents records nothing to recheck.
func TestCloseRecheckBlocked_PublicClosePaths(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()
	seedSiblingBlockers(t, ctx, store)
	createPerm(t, ctx, store, "rb-lone")

	if err := store.CloseIssue(ctx, "rb-a", "done", "tester", ""); err != nil {
		t.Fatalf("close rb-a: %v", err)
	}
	assertIsBlocked(t, ctx, store, "issues", "rb-c", true)
	if err := store.UpdateIssue(ctx, "rb-b", map[string]interface{}{"status": types.StatusClosed}, "tester"); err != nil {
		t.Fatalf("close rb-b through update: %v", err)
	}
	assertIsBlocked(t, ctx, store, "issues", "rb-c", false)
	requireCleanTables(ctx, t, store, "issues")

	var pending issueops.BlockedRecheck
	if err := store.withWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := issueops.CloseIssueInTx(ctx, tx, "rb-lone", "done", "tester", ""); err != nil {
			return err
		}
		pending = issueops.TakeBlockedRecheck(tx)
		return nil
	}); err != nil {
		t.Fatalf("close rb-lone: %v", err)
	}
	if !pending.Empty() {
		t.Fatalf("close of an issue with no dependents recorded %+v to recheck, want nothing", pending)
	}
}
