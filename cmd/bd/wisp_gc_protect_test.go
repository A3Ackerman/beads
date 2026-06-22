package main

import (
	"sort"
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

func TestIsProtectedWispStatus(t *testing.T) {
	protected := []types.Status{
		types.StatusInProgress, types.StatusHooked, types.StatusBlocked,
		types.StatusDeferred, types.StatusPinned,
	}
	for _, s := range protected {
		if !isProtectedWispStatus(s) {
			t.Errorf("status %q should be protected from age GC", s)
		}
	}
	notProtected := []types.Status{types.StatusOpen, types.StatusClosed, types.Status("")}
	for _, s := range notProtected {
		if isProtectedWispStatus(s) {
			t.Errorf("status %q should NOT be protected (GC-collectible)", s)
		}
	}
}

func TestIsAgeGCAbandonableRoot(t *testing.T) {
	cases := []struct {
		status   types.Status
		cleanAll bool
		want     bool
	}{
		{types.StatusOpen, false, true},
		{types.StatusOpen, true, true},
		{types.StatusClosed, false, false},
		{types.StatusClosed, true, true},
		{types.StatusInProgress, false, false},
		{types.StatusInProgress, true, false},
		{types.StatusHooked, true, false},
		{types.StatusBlocked, true, false},
		{types.StatusDeferred, true, false},
		{types.StatusPinned, true, false},
	}
	for _, c := range cases {
		if got := isAgeGCAbandonableRoot(c.status, c.cleanAll); got != c.want {
			t.Errorf("isAgeGCAbandonableRoot(%q, all=%v) = %v, want %v", c.status, c.cleanAll, got, c.want)
		}
	}
}

func sortedCopy(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}

func eqSet(a, b []string) bool {
	a, b = sortedCopy(a), sortedCopy(b)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPartitionDeletableWispTrees(t *testing.T) {
	subtrees := map[string][]string{
		"A": {"A", "A1", "A2"}, // fully dead tree
		"B": {"B", "B1", "B2"}, // contains an in_progress step
		"C": {"C"},             // lone dead root
	}
	subtreeOf := func(r string) []string { return subtrees[r] }

	t.Run("dead trees deleted, live tree fully protected", func(t *testing.T) {
		statusOf := map[string]types.Status{
			"A": types.StatusOpen, "A1": types.StatusOpen, "A2": types.StatusClosed,
			"B": types.StatusOpen, "B1": types.StatusInProgress, "B2": types.StatusOpen,
			"C": types.StatusOpen,
		}
		del, prot := partitionDeletableWispTrees([]string{"A", "B", "C"}, statusOf, subtreeOf)
		if !eqSet(del, []string{"A", "A1", "A2", "C"}) {
			t.Errorf("deletable = %v, want [A A1 A2 C]", sortedCopy(del))
		}
		if prot != 3 { // B, B1, B2 all shielded
			t.Errorf("protected = %d, want 3", prot)
		}
	})

	t.Run("each protected status shields its whole tree", func(t *testing.T) {
		for _, live := range []types.Status{
			types.StatusInProgress, types.StatusHooked, types.StatusBlocked,
			types.StatusDeferred, types.StatusPinned,
		} {
			statusOf := map[string]types.Status{
				"B": types.StatusOpen, "B1": live, "B2": types.StatusOpen,
			}
			del, prot := partitionDeletableWispTrees([]string{"B"}, statusOf, subtreeOf)
			if len(del) != 0 {
				t.Errorf("status %q: deletable = %v, want none", live, del)
			}
			if prot != 3 {
				t.Errorf("status %q: protected = %d, want 3 (whole tree)", live, prot)
			}
		}
	})

	t.Run("infra/unknown nodes neither deleted nor counted", func(t *testing.T) {
		// "A2" absent from statusOf simulates an infra child excluded upstream.
		statusOf := map[string]types.Status{
			"A": types.StatusOpen, "A1": types.StatusOpen,
		}
		del, prot := partitionDeletableWispTrees([]string{"A"}, statusOf, subtreeOf)
		if !eqSet(del, []string{"A", "A1"}) {
			t.Errorf("deletable = %v, want [A A1]", sortedCopy(del))
		}
		if prot != 0 {
			t.Errorf("protected = %d, want 0", prot)
		}
	})

	t.Run("empty roots", func(t *testing.T) {
		del, prot := partitionDeletableWispTrees(nil, map[string]types.Status{}, subtreeOf)
		if len(del) != 0 || prot != 0 {
			t.Errorf("empty: del=%v prot=%d, want none/0", del, prot)
		}
	})
}
