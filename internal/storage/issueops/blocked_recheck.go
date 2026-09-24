package issueops

import "sync"

// BlockedRecheck names the dependents whose is_blocked a status change
// recomputed inside a transaction, so the store can recompute them again on a
// fresh snapshot once that transaction has committed.
//
// The in-transaction recompute reads the transaction's start snapshot. Two
// concurrent closes of sibling blockers each see the other blocker still open,
// each leaves their shared dependent blocked without writing its row, and both
// commit with no conflicting cell — so the dependent stays hidden from
// `bd ready` until a repair (gastownhall/beads#6716). A recheck over the same
// ids after commit sees both closes and settles the flag.
type BlockedRecheck struct {
	IssueIDs []string
	WispIDs  []string
	// Sources are the issues whose status changes recorded the dependents;
	// they name the Dolt commit a recheck mints and are never rechecked.
	Sources []string
}

// Empty reports whether there is nothing to recheck.
func (r BlockedRecheck) Empty() bool {
	return len(r.IssueIDs) == 0 && len(r.WispIDs) == 0
}

var blockedRecheckTransactions sync.Map // map[DBTX]*BlockedRecheck; entries live for one transaction

// ScopeBlockedRecheckTransaction lets the status changes in tx record the
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

// noteBlockedRecheck records the dependents a status change of self
// recomputed in tx. self is left out: this transaction wrote its row, so a
// concurrent writer conflicts on it instead of racing past it.
func noteBlockedRecheck(tx DBTX, self string, issueIDs, wispIDs []string) {
	scope, ok := blockedRecheckTransactions.Load(tx)
	if !ok {
		return
	}
	pending := scope.(*BlockedRecheck)
	pending.IssueIDs = appendRecheckIDs(pending.IssueIDs, issueIDs, self)
	pending.WispIDs = appendRecheckIDs(pending.WispIDs, wispIDs, self)
	pending.Sources = appendRecheckIDs(pending.Sources, []string{self}, "")
}

func appendRecheckIDs(pending, ids []string, self string) []string {
	seen := make(map[string]bool, len(pending)+len(ids))
	for _, id := range pending {
		seen[id] = true
	}
	for _, id := range ids {
		if id != self && !seen[id] {
			seen[id] = true
			pending = append(pending, id)
		}
	}
	return pending
}
