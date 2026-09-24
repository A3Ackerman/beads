package issueops

import (
	"slices"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// scopedRecheckTx opens a sqlmock transaction scoped for recheck recording.
func scopedRecheckTx(t *testing.T) DBTX {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	mock.ExpectBegin()
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	t.Cleanup(ScopeBlockedRecheckTransaction(tx))
	return tx
}

// TestBlockedRecheck_RecordsEveryUnblockingWrite pins the recording contract
// the stores rely on: ids are deduplicated across writes, an excluded id never
// reaches the recheck even when a later write names it, sources are labels
// joined in order, and Take clears the scope.
func TestBlockedRecheck_RecordsEveryUnblockingWrite(t *testing.T) {
	tx := scopedRecheckTx(t)

	noteBlockedRecheck(tx, "close of rb-a", []string{"rb-a"}, []string{"rb-a", "rb-c"}, nil)
	noteBlockedRecheck(tx, "dependency removal rb-c -> rb-b", nil, []string{"rb-c"}, []string{"rb-w"})
	noteBlockedRecheck(tx, deleteRecheckLabel([]string{"rb-b"}, ""), []string{"rb-b"}, []string{"rb-b", "rb-d"}, nil)
	noteBlockedRecheck(tx, "close of rb-a", []string{"rb-a"}, []string{"rb-c"}, nil)

	pending := TakeBlockedRecheck(tx)
	if want := []string{"rb-c", "rb-d"}; !slices.Equal(pending.IssueIDs, want) {
		t.Fatalf("IssueIDs = %v, want %v", pending.IssueIDs, want)
	}
	if want := []string{"rb-w"}; !slices.Equal(pending.WispIDs, want) {
		t.Fatalf("WispIDs = %v, want %v", pending.WispIDs, want)
	}
	if want := "bd: recheck blocked after close of rb-a, dependency removal rb-c -> rb-b, delete of rb-b"; pending.CommitMessage() != want {
		t.Fatalf("CommitMessage() = %q, want %q", pending.CommitMessage(), want)
	}
	if again := TakeBlockedRecheck(tx); !again.Empty() || len(again.Sources) != 0 {
		t.Fatalf("second Take returned %+v, want an empty scope", again)
	}
}

// TestBlockedRecheck_UnscopedTransactionRecordsNothing: a transaction the
// store never scoped (or whose scope already ended) records and returns
// nothing, so the stores' Tx surfaces keep their pre-recheck behaviour.
func TestBlockedRecheck_UnscopedTransactionRecordsNothing(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	mock.ExpectBegin()
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	noteBlockedRecheck(tx, "close of rb-a", []string{"rb-a"}, []string{"rb-c"}, nil)
	if pending := TakeBlockedRecheck(tx); !pending.Empty() {
		t.Fatalf("unscoped tx recorded %+v, want nothing", pending)
	}
	ScopeBlockedRecheckTransaction(nil)() // a nil tx is a no-op scope
}

func TestBlockedRecheck_Labels(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"single delete", deleteRecheckLabel([]string{"rb-b"}, ""), "delete of rb-b"},
		{"three named", deleteRecheckLabel([]string{"a", "b", "c"}, ""), "delete of a, b, c"},
		{"count beyond three", deleteRecheckLabel([]string{"a", "b", "c", "d"}, ""), "delete of 4 issues"},
		{"source repo scope", deleteRecheckLabel(make([]string, 40), "from github.com/x/y"), "delete of 40 issues from github.com/x/y"},
		{"close through update", statusChangeRecheckLabel("rb-b", "closed"), "close of rb-b"},
		{"pin through update", statusChangeRecheckLabel("rb-b", "pinned"), "status change of rb-b to pinned"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}
