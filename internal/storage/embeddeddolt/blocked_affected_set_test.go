//go:build cgo

package embeddeddolt_test

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
)

// TestAffectedSetWalksThroughClosedDescendants pins the WIDTH of the
// status-change affected set and, in the same graph, that narrowing it strands
// nothing.
//
// The graph, under a parent that a blocker keeps blocked:
//
//	as-blocker  <--blocks--  as-parent
//	as-parent
//	├── as-open ── as-open-kid            open all the way down: inherits
//	├── as-shut ── as-shut-kid (OPEN)     closed node over an open one
//	└── as-done-N ── as-done-N-kid        closed all the way down (the width)
//
// Closed descendants are left OUT of the set — a recompute cannot move them —
// but the walk continues THROUGH them, so as-shut-kid, open under the closed
// as-shut, is IN it. On this settled graph visiting it changes nothing (its
// closed parent hands nothing down); it is in the set because the walk cannot
// know the graph is settled, and TestCloseSettlesOpenRowsBelowAStaleClosedNode
// is the graph where it is not. The lifecycle half of the test proves
// convergence on the stored column: after every close and reopen the doctor
// count is 0.
//
// The seeds themselves are never filtered. Reopen computes the affected set
// BEFORE it flips the status (the row is still closed when the walk starts), so
// a walk that dropped a closed SEED would leave the reopened row unsettled.
func TestAffectedSetWalksThroughClosedDescendants(t *testing.T) {
	skipUnlessEmbeddedDolt(t)

	const doneCount = 5

	for _, tc := range []struct {
		name string
		wisp func(id string) bool
	}{
		{"issues", func(string) bool { return false }},
		{"wisps", func(string) bool { return true }},
		// Every parent-child edge below as-parent crosses planes.
		{"mixed", func(id string) bool {
			return id != "as-blocker" && id != "as-parent" && !strings.HasSuffix(id, "-kid")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t, "as")
			ctx := t.Context()
			g := blockedGraph{t, te}

			mk := func(id, parent string) {
				g.create(ctx, id, tc.wisp(id))
				if parent != "" {
					g.dep(ctx, id, parent, types.DepParentChild)
				}
			}
			mk("as-blocker", "")
			mk("as-parent", "")
			mk("as-open", "as-parent")
			mk("as-open-kid", "as-open")
			mk("as-shut", "as-parent")
			mk("as-shut-kid", "as-shut")
			for i := 0; i < doneCount; i++ {
				done := fmt.Sprintf("as-done-%d", i)
				mk(done, "as-parent")
				mk(done+"-kid", done)
				g.close(ctx, done+"-kid")
				g.close(ctx, done)
			}
			g.close(ctx, "as-shut")
			g.dep(ctx, "as-parent", "as-blocker", types.DepBlocks)

			affected := func(id string) []string {
				t.Helper()
				var got []string
				g.inTx(ctx, func(tx *sql.Tx) {
					var issues, wisps []string
					var err error
					if tc.wisp(id) {
						issues, wisps, err = issueops.AffectedByStatusChangeForWispInTx(ctx, tx, id)
					} else {
						issues, wisps, err = issueops.AffectedByStatusChangeInTx(ctx, tx, id)
					}
					if err != nil {
						t.Fatalf("affected set for %s: %v", id, err)
					}
					for _, id := range issues {
						if tc.wisp(id) {
							t.Errorf("%s came back in the issue set, want the wisp set", id)
						}
					}
					for _, id := range wisps {
						if !tc.wisp(id) {
							t.Errorf("%s came back in the wisp set, want the issue set", id)
						}
					}
					got = append(append(got, issues...), wisps...)
				})
				sort.Strings(got)
				return got
			}
			assertAffected := func(id string, want ...string) {
				t.Helper()
				sort.Strings(want)
				if got := affected(id); strings.Join(got, " ") != strings.Join(want, " ") {
					t.Fatalf("affected set for %s:\n got  %v\n want %v", id, got, want)
				}
			}
			assertBlocked := func(when string, want map[string]bool) {
				t.Helper()
				for id, w := range want {
					if got := g.isBlocked(ctx, id, tc.wisp(id)); got != w {
						t.Errorf("%s: %s is_blocked = %v, want %v", when, id, got, w)
					}
				}
				g.inTx(ctx, func(tx *sql.Tx) {
					n, err := issueops.CountIsBlockedInconsistenciesInTx(ctx, tx)
					if err != nil {
						t.Fatalf("%s: count: %v", when, err)
					}
					if n != 0 {
						t.Errorf("%s: %d rows carry a stale is_blocked, want 0", when, n)
					}
				})
				if t.Failed() {
					t.FailNow()
				}
			}

			assertBlocked("seeded", map[string]bool{
				"as-parent": true, "as-open": true, "as-open-kid": true,
				"as-shut": false, "as-shut-kid": false, "as-done-0": false, "as-done-0-kid": false,
			})

			// The width. Under the parent sit 2*doneCount+1 closed rows and
			// three open ones; the set is the open ones plus the seeds, and
			// the same through a depender seed.
			assertAffected("as-parent", "as-parent", "as-open", "as-open-kid", "as-shut-kid")
			assertAffected("as-blocker", "as-blocker", "as-parent", "as-open", "as-open-kid", "as-shut-kid")
			// A closed SEED is returned as well as walked: this is the set
			// reopen asks for.
			assertAffected("as-shut", "as-shut", "as-shut-kid")

			g.close(ctx, "as-blocker")
			assertBlocked("blocker closed", map[string]bool{
				"as-parent": false, "as-open": false, "as-open-kid": false, "as-shut-kid": false,
			})

			reopen := func(id string) {
				t.Helper()
				if err := te.store.ReopenIssue(ctx, id, "again", "tester"); err != nil {
					t.Fatalf("reopen %s: %v", id, err)
				}
			}
			reopen("as-blocker")
			assertBlocked("blocker reopened", map[string]bool{
				"as-parent": true, "as-open": true, "as-open-kid": true,
				"as-shut": false, "as-shut-kid": false, "as-done-0": false, "as-done-0-kid": false,
			})

			// The row being closed is itself blocked here, so its open subtree
			// has an inherited flag to lose and, on reopen, to regain.
			g.close(ctx, "as-parent")
			assertBlocked("parent closed", map[string]bool{
				"as-parent": false, "as-open": false, "as-open-kid": false, "as-shut-kid": false,
			})
			reopen("as-parent")
			assertBlocked("parent reopened", map[string]bool{
				"as-parent": true, "as-open": true, "as-open-kid": true,
				"as-shut": false, "as-shut-kid": false, "as-done-0": false, "as-done-0-kid": false,
			})

			// Reopening the closed node in the middle re-attaches the open row
			// below it to the blocked hierarchy: the seed is closed when the
			// set is computed, and it and its child must be in it regardless.
			reopen("as-shut")
			assertBlocked("closed middle node reopened", map[string]bool{
				"as-shut": true, "as-shut-kid": true,
			})
			g.close(ctx, "as-shut")
			assertBlocked("middle node closed again", map[string]bool{
				"as-shut": false, "as-shut-kid": false, "as-open-kid": true,
			})
		})
	}
}

// TestCloseSettlesOpenRowsBelowAStaleClosedNode is the recovery the walk must
// not give up, and the reason it traverses THROUGH closed nodes instead of
// stopping at them.
//
//	sg-blocker  <--blocks--  sg-root ── sg-mid ── sg-leaf
//
// Everything is open and sg-root, sg-mid and sg-leaf are blocked. Then sg-mid is
// closed the way a merge closes a row — the status moves and no recompute runs
// (the merge clause of issueops.BlockedStateInvariant admits exactly this) — so
// sg-mid and sg-leaf both keep a stale is_blocked = 1. Closing sg-blocker
// through the real close path must still reach sg-leaf: it is OPEN, nothing
// blocks it, and a stale 1 hides it from ready work. A walk that stops at the
// closed sg-mid never visits it, and no later verb has a reason to.
//
// sg-mid's own orphaned flag is the one thing the close does not repair: a
// closed row is left out of the recompute set, the union's parent-child legs
// refuse it as a parent, and the doctor count reports it for the full repair.
func TestCloseSettlesOpenRowsBelowAStaleClosedNode(t *testing.T) {
	skipUnlessEmbeddedDolt(t)

	for _, tc := range []struct {
		name string
		wisp func(id string) bool
	}{
		{"issues", func(string) bool { return false }},
		{"wisps", func(string) bool { return true }},
		{"cross-plane", func(id string) bool { return id == "sg-root" || id == "sg-leaf" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t, "sg")
			ctx := t.Context()
			g := blockedGraph{t, te}

			for _, id := range []string{"sg-blocker", "sg-root", "sg-mid", "sg-leaf"} {
				g.create(ctx, id, tc.wisp(id))
			}
			g.dep(ctx, "sg-mid", "sg-root", types.DepParentChild)
			g.dep(ctx, "sg-leaf", "sg-mid", types.DepParentChild)
			g.dep(ctx, "sg-root", "sg-blocker", types.DepBlocks)
			for _, id := range []string{"sg-root", "sg-mid", "sg-leaf"} {
				if !g.isBlocked(ctx, id, tc.wisp(id)) {
					t.Fatalf("precondition: %s should be blocked", id)
				}
			}

			midTable := "issues"
			if tc.wisp("sg-mid") {
				midTable = "wisps"
			}
			te.exec(t, ctx, "UPDATE "+midTable+" SET status = 'closed', closed_at = NOW() WHERE id = ?", "sg-mid")
			if !g.isBlocked(ctx, "sg-mid", tc.wisp("sg-mid")) || !g.isBlocked(ctx, "sg-leaf", tc.wisp("sg-leaf")) {
				t.Fatal("precondition: the unsettled close must leave sg-mid and sg-leaf flagged")
			}

			g.close(ctx, "sg-blocker")

			if g.isBlocked(ctx, "sg-root", tc.wisp("sg-root")) {
				t.Error("sg-root is still blocked after its only blocker closed")
			}
			if g.isBlocked(ctx, "sg-leaf", tc.wisp("sg-leaf")) {
				t.Error("open sg-leaf kept a stale is_blocked = 1: the walk did not reach below the closed sg-mid")
			}
			// The closed row's own orphaned flag is the one thing the close may
			// leave behind (the walk does not return closed descendants); if it
			// does, that row is the ONLY stale one. Not asserted as required:
			// a walk that also cleared it would be no less correct.
			want := int64(0)
			if g.isBlocked(ctx, "sg-mid", tc.wisp("sg-mid")) {
				want = 1
			}
			g.inTx(ctx, func(tx *sql.Tx) {
				n, err := issueops.CountIsBlockedInconsistenciesInTx(ctx, tx)
				if err != nil {
					t.Fatalf("count: %v", err)
				}
				if n != want {
					t.Errorf("stale count = %d, want %d (nothing but the closed sg-mid's own flag may stay stale)", n, want)
				}
			})
		})
	}
}

// TestAffectedSetPinnedAndMultiParentDescendants covers the two shapes the
// closed-chain cases do not: a PINNED node in the middle of the hierarchy, and
// a child with TWO parents, one closed and one live and blocked.
//
//	pm-blocker  <--blocks--  pm-root
//	pm-root ── pm-pin (PINNED) ── pm-pin-kid
//	pm-root ── pm-live ──┐
//	           pm-shut ──┴── pm-both        pm-shut is closed and parentless
//
// pm-pin is walked through and left out, exactly like a closed node. pm-both
// inherits from pm-live alone: its closed parent neither blocks it nor shields
// it, and the walk reaches it once however many parents lead there.
func TestAffectedSetPinnedAndMultiParentDescendants(t *testing.T) {
	skipUnlessEmbeddedDolt(t)

	te := newTestEnv(t, "pm")
	ctx := t.Context()
	g := blockedGraph{t, te}

	for _, id := range []string{"pm-blocker", "pm-root", "pm-pin", "pm-pin-kid", "pm-live", "pm-shut", "pm-both"} {
		g.create(ctx, id, false)
	}
	g.dep(ctx, "pm-pin", "pm-root", types.DepParentChild)
	g.dep(ctx, "pm-pin-kid", "pm-pin", types.DepParentChild)
	g.dep(ctx, "pm-live", "pm-root", types.DepParentChild)
	g.dep(ctx, "pm-both", "pm-live", types.DepParentChild)
	g.dep(ctx, "pm-both", "pm-shut", types.DepParentChild)
	if err := te.store.UpdateIssue(ctx, "pm-pin", map[string]interface{}{"status": string(types.StatusPinned)}, "tester"); err != nil {
		t.Fatalf("pin pm-pin: %v", err)
	}
	g.close(ctx, "pm-shut")
	g.dep(ctx, "pm-root", "pm-blocker", types.DepBlocks)

	affected := func(id string) string {
		t.Helper()
		var got []string
		g.inTx(ctx, func(tx *sql.Tx) {
			issues, wisps, err := issueops.AffectedByStatusChangeInTx(ctx, tx, id)
			if err != nil {
				t.Fatalf("affected set for %s: %v", id, err)
			}
			got = append(issues, wisps...)
		})
		sort.Strings(got)
		return strings.Join(got, " ")
	}
	settled := func(when string, want map[string]bool) {
		t.Helper()
		for id, w := range want {
			if got := g.isBlocked(ctx, id, false); got != w {
				t.Errorf("%s: %s is_blocked = %v, want %v", when, id, got, w)
			}
		}
		g.inTx(ctx, func(tx *sql.Tx) {
			if n, err := issueops.CountIsBlockedInconsistenciesInTx(ctx, tx); err != nil || n != 0 {
				t.Errorf("%s: stale count = %d, err = %v, want 0", when, n, err)
			}
		})
		if t.Failed() {
			t.FailNow()
		}
	}

	settled("seeded", map[string]bool{
		"pm-root": true, "pm-pin": false, "pm-pin-kid": false, "pm-live": true, "pm-both": true, "pm-shut": false,
	})
	if got, want := affected("pm-blocker"), "pm-blocker pm-both pm-live pm-pin-kid pm-root"; got != want {
		t.Fatalf("affected set for pm-blocker:\n got  %s\n want %s", got, want)
	}
	// From the closed parent's side: the seed is returned, the shared child
	// once.
	if got, want := affected("pm-shut"), "pm-both pm-shut"; got != want {
		t.Fatalf("affected set for pm-shut:\n got  %s\n want %s", got, want)
	}

	g.close(ctx, "pm-blocker")
	settled("blocker closed", map[string]bool{"pm-root": false, "pm-live": false, "pm-both": false, "pm-pin-kid": false})
	if err := te.store.ReopenIssue(ctx, "pm-blocker", "again", "tester"); err != nil {
		t.Fatalf("reopen blocker: %v", err)
	}
	settled("blocker reopened", map[string]bool{
		"pm-root": true, "pm-live": true, "pm-both": true, "pm-pin": false, "pm-pin-kid": false, "pm-shut": false,
	})
	// Closing the live parent leaves pm-both under two closed parents.
	g.close(ctx, "pm-live")
	settled("live parent closed", map[string]bool{"pm-live": false, "pm-both": false, "pm-root": true})
}
