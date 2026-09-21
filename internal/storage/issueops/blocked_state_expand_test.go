package issueops

import (
	"context"
	"database/sql/driver"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// The descendant walk's STATEMENT COUNT and its SET ARITHMETIC are the contract
// here: one query per dependency table per level of up to queryBatchSize
// parents, not one per parent; closed and pinned children expanded but not
// returned. sqlmock's ordered expectations make an extra (or a per-parent)
// query fail as unexpected, and the bound args pin which rows were walked
// through. The same behavior against a real engine, with real statuses in both
// planes, is pinned in the embeddeddolt suite
// (TestAffectedSetWalksThroughClosedDescendants and its neighbors).

func idArgs(ids ...string) []driver.Value {
	args := make([]driver.Value, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return args
}

// walkedRows scripts one level's result: id/status pairs, "" for a dependency
// row whose child row is missing (the LEFT JOIN's NULL).
func walkedRows(idStatus ...string) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{"issue_id", "status"})
	for i := 0; i+1 < len(idStatus); i += 2 {
		if idStatus[i+1] == "" {
			rows.AddRow(idStatus[i], nil)
			continue
		}
		rows.AddRow(idStatus[i], idStatus[i+1])
	}
	return rows
}

func TestDescendantWalkQueriesOncePerLevelAndWalksThroughClosed(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	// Level 0: three issue seeds travel in one statement per child plane.
	mock.ExpectQuery(`(?s)FROM dependencies d\s+LEFT JOIN issues c .*depends_on_issue_id IN \(\?,\?,\?\)`).
		WithArgs(idArgs("p-1", "p-2", "p-3")...).
		WillReturnRows(walkedRows(
			"c-open", "open",
			"c-shut", "closed",
			"c-pin", "pinned",
			"c-gone", "", // dependency row with no child row: still a node
			"p-2", "open", // already seen
			"c-open", "open", // second parent in the same batch
		))
	mock.ExpectQuery(`(?s)FROM wisp_dependencies d\s+LEFT JOIN wisps c .*depends_on_issue_id IN \(\?,\?,\?\)`).
		WithArgs(idArgs("p-1", "p-2", "p-3")...).
		WillReturnRows(walkedRows("w-shut", "closed"))
	// The walk alternates planes: the closed wisp is expanded next, through
	// the other parent column — closed, and still asked for its children.
	mock.ExpectQuery(`(?s)FROM dependencies d\s+LEFT JOIN issues c .*depends_on_wisp_id IN \(\?\)`).
		WithArgs(idArgs("w-shut")...).
		WillReturnRows(walkedRows("g-under-wisp", "open"))
	mock.ExpectQuery(`(?s)FROM wisp_dependencies d\s+LEFT JOIN wisps c .*depends_on_wisp_id IN \(\?\)`).
		WithArgs(idArgs("w-shut")...).
		WillReturnRows(walkedRows())
	// Level 1, issue plane: closed, pinned and missing children are all on the
	// frontier beside the open one.
	mock.ExpectQuery(`(?s)FROM dependencies d\s+LEFT JOIN issues c .*depends_on_issue_id IN \(\?,\?,\?,\?,\?\)`).
		WithArgs(idArgs("c-open", "c-shut", "c-pin", "c-gone", "g-under-wisp")...).
		WillReturnRows(walkedRows("g-under-shut", "open", "g-shut", "closed"))
	mock.ExpectQuery(`(?s)FROM wisp_dependencies d\s+LEFT JOIN wisps c .*depends_on_issue_id IN \(\?,\?,\?,\?,\?\)`).
		WithArgs(idArgs("c-open", "c-shut", "c-pin", "c-gone", "g-under-wisp")...).
		WillReturnRows(walkedRows())
	// Level 2.
	mock.ExpectQuery(`(?s)FROM dependencies d\s+LEFT JOIN issues c .*depends_on_issue_id IN \(\?,\?\)`).
		WithArgs(idArgs("g-under-shut", "g-shut")...).
		WillReturnRows(walkedRows())
	mock.ExpectQuery(`(?s)FROM wisp_dependencies d\s+LEFT JOIN wisps c .*depends_on_issue_id IN \(\?,\?\)`).
		WithArgs(idArgs("g-under-shut", "g-shut")...).
		WillReturnRows(walkedRows())

	seeds := []string{"p-1", "p-2", "p-3"}
	issues, wisps, err := expandByParentChildDescendantsInTx(context.Background(), db,
		seeds, nil, map[string]bool{"p-1": true, "p-2": true, "p-3": true}, map[string]bool{})
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	sort.Strings(issues)
	// Seeds, the open rows (including the two under closed nodes) and the
	// status-less one; never c-shut, c-pin or g-shut.
	if got, want := strings.Join(issues, " "), "c-gone c-open g-under-shut g-under-wisp p-1 p-2 p-3"; got != want {
		t.Errorf("issues = %q, want %q", got, want)
	}
	if len(wisps) != 0 {
		t.Errorf("wisps = %v, want none (the only wisp found is closed)", wisps)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// A level wider than queryBatchSize is split like every other id batch in the
// package, so the IN-list never grows with the graph.
func TestDescendantWalkSplitsAWideLevel(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	seeds := make([]string, 2*queryBatchSize+50)
	seen := make(map[string]bool, len(seeds))
	for i := range seeds {
		seeds[i] = fmt.Sprintf("p-%d", i)
		seen[seeds[i]] = true
	}
	for start := 0; start < len(seeds); start += queryBatchSize {
		end := start + queryBatchSize
		if end > len(seeds) {
			end = len(seeds)
		}
		mock.ExpectQuery(`(?s)FROM dependencies d`).WithArgs(idArgs(seeds[start:end]...)...).WillReturnRows(walkedRows())
		mock.ExpectQuery(`(?s)FROM wisp_dependencies d`).WithArgs(idArgs(seeds[start:end]...)...).WillReturnRows(walkedRows())
	}

	issues, _, err := expandByParentChildDescendantsInTx(context.Background(), db, seeds, nil, seen, map[string]bool{})
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if len(issues) != len(seeds) {
		t.Errorf("returned %d issues, want the %d seeds", len(issues), len(seeds))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// AffectedByDeletionInTx puts the deleted ids on the walk as through-ids: their
// children are found, they themselves are not returned, and a closed child of a
// deleted row gets the same treatment as anywhere else.
func TestDescendantWalkThroughIDsAreExpandedNotReturned(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`(?s)FROM dependencies d.*depends_on_issue_id IN \(\?,\?\)`).
		WithArgs(idArgs("depender", "deleted")...).
		WillReturnRows(walkedRows("orphan", "open", "orphan-shut", "closed"))
	mock.ExpectQuery(`(?s)FROM wisp_dependencies d.*depends_on_issue_id IN \(\?,\?\)`).
		WithArgs(idArgs("depender", "deleted")...).
		WillReturnRows(walkedRows())
	mock.ExpectQuery(`(?s)FROM dependencies d.*depends_on_issue_id IN \(\?,\?\)`).
		WithArgs(idArgs("orphan", "orphan-shut")...).
		WillReturnRows(walkedRows())
	mock.ExpectQuery(`(?s)FROM wisp_dependencies d.*depends_on_issue_id IN \(\?,\?\)`).
		WithArgs(idArgs("orphan", "orphan-shut")...).
		WillReturnRows(walkedRows())

	issues, wisps, err := walkParentChildDescendantsInTx(context.Background(), db,
		[]string{"depender"}, nil, []string{"deleted"}, nil,
		map[string]bool{"depender": true, "deleted": true}, map[string]bool{})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if got, want := strings.Join(issues, " "), "depender orphan"; got != want {
		t.Errorf("issues = %q, want %q", got, want)
	}
	if len(wisps) != 0 {
		t.Errorf("wisps = %v, want none", wisps)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
