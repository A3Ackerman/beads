package issueops

import (
	"context"
	"database/sql"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/steveyegge/beads/internal/types"
)

// TestGetReadyWorkInTxHandsTheDescendantSetToTheWispLeg pins the hand-off
// that #6129 introduced on the text route: the issues leg walks the parent's
// descendants once and the wisp leg scopes by THAT set. The wisp here is a
// descendant only through the walk — its id carries no `<parent>.` prefix —
// so a wisp leg handed a nil set (or a copy of the predicates that dropped
// the set) silently loses it, which is the mutation the review found
// unpinned. Expectations are unordered on purpose: the pin is the answer,
// not the query sequence, which TestFilterReadyWispsInTxUsesTheHoistedDescendantSet
// already owns.
func TestGetReadyWorkInTxHandsTheDescendantSetToTheWispLeg(t *testing.T) {
	t.Parallel()

	_, mock, tx := beginMockTx(t)
	mock.MatchExpectationsInOrder(false)
	parent := "rw-parent"
	wisp := "rw-wisp-by-walk"

	mock.ExpectQuery(deferredParentProbeRegex("issues")).WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(deferredParentProbeRegex("wisps")).WillReturnError(sql.ErrNoRows)
	// The one walk: the wisp is a descendant, by edge, not by id.
	mock.ExpectQuery(`WITH RECURSIVE`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "depth"}).AddRow(wisp, 1))
	// The issues leg finds nothing durable under the parent.
	mock.ExpectQuery(`SELECT id FROM issues`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	// The wisp leg: the plane exists, the search finds the wisp, nothing
	// blocks or defers it.
	mock.ExpectQuery(`SELECT 1 FROM wisps LIMIT 1`).
		WillReturnRows(sqlmock.NewRows([]string{"1"}).AddRow(1))
	mock.ExpectQuery(`SELECT [\s\S]*FROM wisps LEFT JOIN leases`).
		WillReturnRows(issueRows().AddRow(issueRowValues(wisp, "reached by the walk")...))
	// Hydration of the found wisp (labels, edges) and the parented-ID probes
	// of the wisp leg all read empty; extra expectations here are harmless
	// because the test never asserts they were all consumed.
	mock.ExpectQuery(`FROM (wisp_)?labels`).
		WillReturnRows(sqlmock.NewRows([]string{"issue_id", "label"}))
	for i := 0; i < 2; i++ {
		mock.ExpectQuery(`FROM dependencies`).
			WillReturnRows(sqlmock.NewRows([]string{"issue_id"}))
		mock.ExpectQuery(`FROM wisp_dependencies`).
			WillReturnRows(sqlmock.NewRows([]string{"issue_id"}))
	}
	mock.ExpectQuery(`SELECT id FROM wisps WHERE id IN \(\?\) AND is_blocked = 1`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	got, err := GetReadyWorkInTx(context.Background(), tx, types.WorkFilter{ParentID: &parent, IncludeEphemeral: true})
	if err != nil {
		t.Fatalf("GetReadyWorkInTx: %v", err)
	}
	if len(got) != 1 || got[0].ID != wisp {
		t.Fatalf("ready = %v, want [%s]: the wisp leg must scope by the set the issues leg walked", ids(got), wisp)
	}
}
