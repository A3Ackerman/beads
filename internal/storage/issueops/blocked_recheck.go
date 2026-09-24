package issueops

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/steveyegge/beads/internal/types"
)

// BlockedRecheck names the dependents whose is_blocked a write recomputed
// inside a transaction, so the store can recompute them again on a fresh
// snapshot once that transaction has committed.
//
// The in-transaction recompute reads the transaction's start snapshot, and
// every write that records here can only UNBLOCK a dependent: a close, an
// update to an inactive status, a dependency removal, a delete. Two such
// writes racing on the blockers of one dependent each see the other blocker
// still in place, each leaves the dependent blocked without writing its row,
// and both commit with no conflicting cell — so the dependent stays hidden
// from `bd ready` until a repair (gastownhall/beads#6716, first seen as two
// closes of sibling blockers). A recheck over the same ids after commit sees
// both writes and settles the flag.
type BlockedRecheck struct {
	IssueIDs []string
	WispIDs  []string
	// Sources label the writes that recorded the dependents, one short human
	// phrase each ("close of rb-a", "dependency removal rb-c -> rb-a",
	// "delete of rb-b"); they name the Dolt commit a recheck mints.
	Sources []string
}

// Empty reports whether there is nothing to recheck.
func (r BlockedRecheck) Empty() bool {
	return len(r.IssueIDs) == 0 && len(r.WispIDs) == 0
}

// CommitMessage is the Dolt commit message a recheck that changed a row mints.
func (r BlockedRecheck) CommitMessage() string {
	return "bd: recheck blocked after " + strings.Join(r.Sources, ", ")
}

// blockedRecheckTransactions holds the open scopes, one per transaction. It is
// keyed by the DBTX interface value: a write records into a scope only when
// the very same *sql.Tx the store scoped reaches noteBlockedRecheck. A future
// DBTX wrapper around that tx would compare unequal and silently record
// nothing — the same shape, and the same caveat, as the events-journal scope
// in journal.go.
var blockedRecheckTransactions sync.Map // map[DBTX]*BlockedRecheck; entries live for one transaction

// ErrBlockedRecheckFailed marks a failure of the post-commit recheck of
// dependents' blocked state. The write that preceded it is committed and
// durable; only the recheck failed, so at worst a dependent carries the stale
// is_blocked flag that `bd doctor` and `bd recompute-blocked` already repair.
// Callers use errors.Is with it to tell a committed write from one that never
// landed, since both reach them as an error from the same store call.
var ErrBlockedRecheckFailed = errors.New("blocked-state recheck after a committed write failed")

// BlockedRecheckFailed wraps a recheck failure so both ErrBlockedRecheckFailed
// and the underlying cause stay reachable through errors.Is and errors.As.
func BlockedRecheckFailed(err error) error {
	return fmt.Errorf("%w: %w", ErrBlockedRecheckFailed, err)
}

// ScopeBlockedRecheckTransaction lets the unblocking writes in tx record the
// dependents they recomputed, for TakeBlockedRecheck once tx has committed.
// Store implementations call it right after BeginTx and run the returned
// cleanup when the transaction ends. An unscoped transaction records nothing.
func ScopeBlockedRecheckTransaction(tx DBTX) func() {
	if tx == nil {
		return func() {}
	}
	blockedRecheckTransactions.Store(tx, &BlockedRecheck{})
	return func() { blockedRecheckTransactions.Delete(tx) }
}

// TakeBlockedRecheck returns the dependents recorded in tx and clears them.
func TakeBlockedRecheck(tx DBTX) BlockedRecheck {
	scope, ok := blockedRecheckTransactions.Load(tx)
	if !ok {
		return BlockedRecheck{}
	}
	pending := scope.(*BlockedRecheck)
	taken := *pending
	*pending = BlockedRecheck{}
	return taken
}

// noteBlockedRecheck records the dependents an unblocking write recomputed in
// tx. source labels the write for the recheck's commit message. exclude names
// ids the recheck must never touch: for a close or status update, the issue
// whose row this transaction wrote (a concurrent writer conflicts on that row
// instead of racing past it); for a delete, the rows that no longer exist. A
// dependency removal excludes nothing — its dependent is exactly the row
// that needs rechecking.
func noteBlockedRecheck(tx DBTX, source string, exclude []string, issueIDs, wispIDs []string) {
	scope, ok := blockedRecheckTransactions.Load(tx)
	if !ok {
		return
	}
	pending := scope.(*BlockedRecheck)
	pending.IssueIDs = appendRecheckIDs(pending.IssueIDs, issueIDs, exclude)
	pending.WispIDs = appendRecheckIDs(pending.WispIDs, wispIDs, exclude)
	pending.Sources = appendRecheckIDs(pending.Sources, []string{source}, nil)
}

func appendRecheckIDs(pending, ids, exclude []string) []string {
	seen := make(map[string]bool, len(pending)+len(exclude))
	for _, id := range pending {
		seen[id] = true
	}
	for _, id := range exclude {
		seen[id] = true
	}
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			pending = append(pending, id)
		}
	}
	return pending
}

// deleteRecheckLabel is the Sources label for a delete of ids. Up to three
// ids are named; beyond that only the count, so a bulk delete cannot grow a
// commit message without bound. scope, when set, says where the ids came
// from ("from <sourceRepo>").
func deleteRecheckLabel(ids []string, scope string) string {
	var label string
	if len(ids) <= 3 {
		label = "delete of " + strings.Join(ids, ", ")
	} else {
		label = fmt.Sprintf("delete of %d issues", len(ids))
	}
	if scope != "" {
		label += " " + scope
	}
	return label
}

// statusChangeRecheckLabel is the Sources label for a status change of id to
// the inactive newStatus. A close through update reads the same as a close
// through CloseIssue, so the two paths mint the same commit message.
func statusChangeRecheckLabel(id, newStatus string) string {
	if newStatus == string(types.StatusClosed) {
		return "close of " + id
	}
	return fmt.Sprintf("status change of %s to %s", id, newStatus)
}
