package db

import (
	"context"
	"database/sql/driver"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/steveyegge/beads/internal/types"
)

// TestGetReadyWorkIDPageWalksTheParentDescendantsOnce pins that a scoped ready
// page computes the parent's transitive descendants once and reuses them for
// both planes. The union used to build its per-plane predicates independently,
// running the descendant walk — the dominant cost of `bd ready --parent` — and
// the deferred-parent probes once for issues and again for wisps (#6129).
// The script below is the whole allowed sequence: one deferred probe per issue
// table, one walk, the wisps-plane probe, the union. A second walk or probe
// before the union is an unexpected query and fails the test.
func TestGetReadyWorkIDPageWalksTheParentDescendantsOnce(t *testing.T) {
	t.Parallel()

	mock, repo := newMockRepo(t)
	parent := "rw-parent"

	mock.ExpectQuery(`SELECT 1 FROM issues WHERE defer_until IS NOT NULL`).
		WillReturnRows(sqlmock.NewRows([]string{"1"}))
	mock.ExpectQuery(`SELECT 1 FROM wisps WHERE defer_until IS NOT NULL`).
		WillReturnRows(sqlmock.NewRows([]string{"1"}))
	mock.ExpectQuery(`WITH RECURSIVE`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "depth"}).AddRow("rw-child", 1))
	mock.ExpectQuery(`SELECT 1 FROM wisps LIMIT 1`).
		WillReturnRows(sqlmock.NewRows([]string{"1"}).AddRow(1))
	// 20 arguments: the union carries the parent's descendant set in BOTH planes'
	// IN lists plus the scope, status and page bounds; a wisp plane built with
	// ParentDescendantIDs=nil issues the same query count with fewer arguments,
	// which is exactly the slip this pins.
	mock.ExpectQuery(`SELECT id, src FROM`).WithArgs(anyArgs(20)...).
		WillReturnRows(sqlmock.NewRows([]string{"id", "src"}).AddRow("rw-child", "i"))

	page, _, err := repo.getReadyWorkIDPage(context.Background(), types.WorkFilter{ParentID: &parent, Limit: 10})
	if err != nil {
		t.Fatalf("getReadyWorkIDPage: %v", err)
	}
	if len(page.issueIDs) != 1 || page.issueIDs[0] != "rw-child" {
		t.Fatalf("page issue ids = %v, want [rw-child]", page.issueIDs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected query sequence: %v", err)
	}
}

// anyArgs is n sqlmock.AnyArg() matchers: the test pins how many arguments the
// union binds, not their values.
func anyArgs(n int) []driver.Value {
	out := make([]driver.Value, n)
	for i := range out {
		out[i] = sqlmock.AnyArg()
	}
	return out
}
