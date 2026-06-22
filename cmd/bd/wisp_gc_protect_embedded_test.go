//go:build cgo

package main

import (
	"os"
	"strings"
	"testing"
)

// TestWispGCProtectsLiveMoleculeTrees is the end-to-end regression test for the
// 2026-06-19 patrol-cascade incident: age-based `bd mol wisp gc` force-deleted a
// live molecule's in-progress step subtree because abandonment was decided by
// age alone and the cascade pulled in every dependent step regardless of status.
//
// It drives the real CLI against an embedded Dolt store and asserts that a
// molecule tree containing a single in_progress step is protected IN FULL
// (root + the in_progress step + its OPEN sibling), while an entirely-stale tree
// in the same GC run is still collected.
func TestWispGCProtectsLiveMoleculeTrees(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", "gc")

	// Live molecule: root + 2 steps. One step is in_progress; its sibling is
	// open. The whole tree must survive GC.
	liveRoot := bdCreate(t, bd, dir, "Live patrol molecule", "--ephemeral", "--type", "task")
	liveActive := bdCreate(t, bd, dir, "Live step (in progress)", "--ephemeral", "--type", "task")
	liveSibling := bdCreate(t, bd, dir, "Live step sibling (open)", "--ephemeral", "--type", "task")
	bdDepAdd(t, bd, dir, liveActive.ID, liveRoot.ID, "--type", "parent-child")
	bdDepAdd(t, bd, dir, liveSibling.ID, liveRoot.ID, "--type", "parent-child")
	bdUpdate(t, bd, dir, liveActive.ID, "--status", "in_progress")

	// Dead molecule: root + step, both open and abandoned. Must be collected.
	deadRoot := bdCreate(t, bd, dir, "Dead molecule", "--ephemeral", "--type", "task")
	deadStep := bdCreate(t, bd, dir, "Dead step", "--ephemeral", "--type", "task")
	bdDepAdd(t, bd, dir, deadStep.ID, deadRoot.ID, "--type", "parent-child")

	// Age GC with a tiny threshold so every wisp qualifies by age. Only status
	// (and tree membership) should keep the live tree alive.
	gcOut, err := bdRunWithFlockRetry(t, bd, dir, "mol", "wisp", "gc", "--age", "1ms", "--force")
	if err != nil {
		t.Fatalf("wisp gc failed: %v\n%s", err, gcOut)
	}
	t.Logf("wisp gc output:\n%s", gcOut)

	// Enumerate surviving wisps (--all so any non-deleted wisp, regardless of
	// status, is listed).
	listOut, err := bdRunWithFlockRetry(t, bd, dir, "mol", "wisp", "list", "--all")
	if err != nil {
		t.Fatalf("wisp list failed: %v\n%s", err, listOut)
	}
	survivors := string(listOut)

	mustSurvive := map[string]string{
		liveRoot.ID:    "live molecule root",
		liveActive.ID:  "in_progress step",
		liveSibling.ID: "OPEN sibling of the in_progress step",
	}
	for id, desc := range mustSurvive {
		if !strings.Contains(survivors, id) {
			t.Errorf("expected %s (%s) to survive GC, but it was deleted\nsurvivors:\n%s", id, desc, survivors)
		}
	}

	mustBeGone := map[string]string{
		deadRoot.ID: "abandoned molecule root",
		deadStep.ID: "abandoned step",
	}
	for id, desc := range mustBeGone {
		if strings.Contains(survivors, id) {
			t.Errorf("expected %s (%s) to be collected by GC, but it survived\nsurvivors:\n%s", id, desc, survivors)
		}
	}
}
