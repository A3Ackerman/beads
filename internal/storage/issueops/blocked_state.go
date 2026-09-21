package issueops

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/steveyegge/beads/internal/types"
)

// DBTX is the minimal statement-execution surface the blocked-state
// recompute needs. *sql.Tx satisfies it (the classic embedded path) and so
// does the domain/db Runner (the server/proxied path): is_blocked is derived
// state shared by both stacks, so they must derive it with the same code
// (bd-6dnrw.44 item 3).
type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const waitsForGateBlockedSQL = `
		(
		  (
		    EXISTS (
		      SELECT 1 FROM dependencies cd JOIN issues child ON child.id = cd.issue_id
		      WHERE cd.type = 'parent-child'
		        AND ((d.depends_on_issue_id IS NOT NULL AND cd.depends_on_issue_id = d.depends_on_issue_id)
		          OR (d.depends_on_wisp_id IS NOT NULL AND cd.depends_on_wisp_id = d.depends_on_wisp_id))
		        AND child.status <> 'closed' AND child.status <> 'pinned'
		    )
		    OR EXISTS (
		      SELECT 1 FROM wisp_dependencies cd JOIN wisps child ON child.id = cd.issue_id
		      WHERE cd.type = 'parent-child'
		        AND ((d.depends_on_issue_id IS NOT NULL AND cd.depends_on_issue_id = d.depends_on_issue_id)
		          OR (d.depends_on_wisp_id IS NOT NULL AND cd.depends_on_wisp_id = d.depends_on_wisp_id))
		        AND child.status <> 'closed' AND child.status <> 'pinned'
		    )
		  )
		  AND NOT (
		    -- COALESCE: metadata without a gate key (legacy '{}' rows) means the
		    -- all-children default; a NULL here would poison the AND/NOT chain
		    -- and unblock the gate as soon as any child closes.
		    COALESCE(JSON_UNQUOTE(JSON_EXTRACT(d.metadata, '$.gate')), 'all-children') = 'any-children'
		    AND (
		      EXISTS (
		        SELECT 1 FROM dependencies cd JOIN issues child ON child.id = cd.issue_id
		        WHERE cd.type = 'parent-child'
		          AND ((d.depends_on_issue_id IS NOT NULL AND cd.depends_on_issue_id = d.depends_on_issue_id)
		            OR (d.depends_on_wisp_id IS NOT NULL AND cd.depends_on_wisp_id = d.depends_on_wisp_id))
		          AND child.status = 'closed'
		      )
		      OR EXISTS (
		        SELECT 1 FROM wisp_dependencies cd JOIN wisps child ON child.id = cd.issue_id
		        WHERE cd.type = 'parent-child'
		          AND ((d.depends_on_issue_id IS NOT NULL AND cd.depends_on_issue_id = d.depends_on_issue_id)
		            OR (d.depends_on_wisp_id IS NOT NULL AND cd.depends_on_wisp_id = d.depends_on_wisp_id))
		          AND child.status = 'closed'
		      )
		    )
		  )
		)
		OR (
		  -- also_blocks (GH#3783/GH#3875): a waits-for edge collapsed from a
		  -- redundant needs/depends_on blocks edge onto this same spawner
		  -- (cmd/bd/cook.go collectDependencies) additionally carries classic
		  -- blocking semantics — it must block while the spawner itself is
		  -- open, not only while the spawner has an open parent-child child.
		  -- This closes the pre-fanout window where the waiter could become
		  -- ready before the spawner (and its fanout) ever completed. Legacy
		  -- rows and plain (non-collapsed) waits-for edges lack the
		  -- also_blocks key, so COALESCE defaults to 'false' and this branch
		  -- is a no-op for them (zero behavior change).
		  --
		  -- This is a top-level OR, deliberately outside (and overriding) the
		  -- any-children early-open carve-out above: a collapsed edge means
		  -- the caller's needs/depends_on required the spawner itself to
		  -- close, so an early-open child close must NOT unblock the waiter
		  -- while the spawner remains open.
		  COALESCE(JSON_UNQUOTE(JSON_EXTRACT(d.metadata, '$.also_blocks')), 'false') = 'true'
		  AND (
		    EXISTS (
		      SELECT 1 FROM issues sp
		      WHERE sp.id = d.depends_on_issue_id
		        AND sp.status <> 'closed' AND sp.status <> 'pinned'
		    )
		    OR EXISTS (
		      SELECT 1 FROM wisps sp
		      WHERE sp.id = d.depends_on_wisp_id
		        AND sp.status <> 'closed' AND sp.status <> 'pinned'
		    )
		  )
		)
`

// RecomputeIsBlockedResult reports which issue tables had rows changed while
// the blocked-state fixpoint converged.
type RecomputeIsBlockedResult struct {
	IssueRowsChanged bool
	WispRowsChanged  bool
}

// RecomputeIsBlockedInTx recomputes blocked state and discards the per-table
// change result retained by RecomputeIsBlockedInTxWithResult.
func RecomputeIsBlockedInTx(ctx context.Context, tx DBTX, issueIDs, wispIDs []string) error {
	_, err := RecomputeIsBlockedInTxWithResult(ctx, tx, issueIDs, wispIDs)
	return err
}

// RecomputeIsBlockedInTxWithResult recomputes blocked state to a fixpoint and
// reports whether an UPDATE changed rows in each issue table.
func RecomputeIsBlockedInTxWithResult(
	ctx context.Context, tx DBTX, issueIDs, wispIDs []string,
) (RecomputeIsBlockedResult, error) {
	var result RecomputeIsBlockedResult
	if len(issueIDs) == 0 && len(wispIDs) == 0 {
		return result, nil
	}
	before, err := captureBlockedJournalSnapshot(ctx, tx, issueIDs, wispIDs)
	if err != nil {
		return result, err
	}
	for {
		var changed int64

		n, err := recomputeIsBlockedPassForIssuesInTx(ctx, tx, issueIDs)
		if err != nil {
			return result, err
		}
		changed += n
		result.IssueRowsChanged = result.IssueRowsChanged || n > 0

		n, err = recomputeIsBlockedPassForWispsInTx(ctx, tx, wispIDs)
		if err != nil {
			return result, err
		}
		changed += n
		result.WispRowsChanged = result.WispRowsChanged || n > 0

		if changed == 0 {
			return result, recordBlockedJournalChanges(ctx, tx, before, issueIDs, wispIDs)
		}
	}
}

func MarkIsBlockedInTx(ctx context.Context, tx DBTX, issueIDs, wispIDs []string) error {
	if len(issueIDs) == 0 && len(wispIDs) == 0 {
		return nil
	}
	before, err := captureBlockedJournalSnapshot(ctx, tx, issueIDs, wispIDs)
	if err != nil {
		return err
	}
	for {
		var changed int64

		n, err := markIsBlockedPassForIssuesInTx(ctx, tx, issueIDs)
		if err != nil {
			return err
		}
		changed += n

		n, err = markIsBlockedPassForWispsInTx(ctx, tx, wispIDs)
		if err != nil {
			return err
		}
		changed += n

		if changed == 0 {
			return recordBlockedJournalChanges(ctx, tx, before, issueIDs, wispIDs)
		}
	}
}

func RecomputeIsBlockedForIDsInTx(ctx context.Context, tx DBTX, ids []string) error {
	return RecomputeIsBlockedInTx(ctx, tx, ids, nil)
}

func RecomputeIsBlockedForWispIDsInTx(ctx context.Context, tx DBTX, ids []string) error {
	return RecomputeIsBlockedInTx(ctx, tx, nil, ids)
}

//nolint:gosec // G201: SQL templates are constant; only IN-clause placeholders are formatted in.
func recomputeIsBlockedPassForIssuesInTx(ctx context.Context, tx DBTX, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}

	return runMarkUnmarkBatchedInTx(ctx, tx, markBlockedTemplateForIssues(), unmarkBlockedTemplateForIssues(), ids)
}

func markIsBlockedPassForIssuesInTx(ctx context.Context, tx DBTX, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	return runMarkBatchedInTx(ctx, tx, markBlockedTemplateForIssues(), ids)
}

// The mark/unmark templates explicitly assign updated_at to itself:
// issues.updated_at (and wisps.updated_at) carry ON UPDATE CURRENT_TIMESTAMP,
// and is_blocked is DERIVED state - letting a recompute bump updated_at
// plants per-clone wall clock in a synced table (merge conflicts between
// clones that recomputed the same flip at different times, bd-578h9.19) and
// makes stale-guard/conflict-guard consumers treat the row as user-edited.
// An explicit assignment suppresses the ON UPDATE clause.
//
// Both templates decide membership through shouldBeBlockedIDsUnionScopedSQL,
// the same uncorrelated union the full repair and the doctor count use
// (blocked_consistency.go), scoped to the batch: one derived blocked set per
// batch, computed once and probed by hash. The previous shape — five
// correlated EXISTS per outer row — was re-executed per row by the engine:
// ~4 s per 200-id batch on committed, indexed data, and unbounded (>69 min
// observed on dolt 2.1.8) over a large uncommitted working set, where every
// probe re-read the uncommitted overlay (gastownhall/beads#6288).
//
// The batch IN-list therefore appears more than once per statement — the
// outer row filter plus one per union leg; expandBatchTemplate repeats the
// placeholders and the bound ids to match.

// batchScopeSQL is the per-leg predicate that confines the should-be-blocked
// union to the batch (see shouldBeBlockedIDsUnionScopedSQL); its %s is filled
// with the batch placeholders by expandBatchTemplate, never by fmt here.
const batchScopeSQL = "AND d.issue_id IN (%s)"

func markBlockedTemplateForIssues() string {
	return markBlockedTemplate("issues", "i", "dependencies")
}

func unmarkBlockedTemplateForIssues() string {
	return unmarkBlockedTemplate("issues", "i", "dependencies")
}

func markBlockedTemplateForWisps() string {
	return markBlockedTemplate("wisps", "w", "wisp_dependencies")
}

func unmarkBlockedTemplateForWisps() string {
	return unmarkBlockedTemplate("wisps", "w", "wisp_dependencies")
}

// markBlockedTemplate is the batched mark statement for one table: the
// batch-scoped analog of markAllBlockedSQL. The union is confined to the
// batch's own dependency rows, so `<alias>.id IN (union)` is exactly the
// old correlated disjunction for every id in the batch.
//
//nolint:gosec // G201: table, alias, and depTable are constants from the four callers above.
func markBlockedTemplate(table, alias, depTable string) string {
	return fmt.Sprintf(`
		UPDATE %[1]s %[2]s SET %[2]s.is_blocked = 1, %[2]s.updated_at = %[2]s.updated_at
		WHERE %[2]s.id IN (%%s)
		  AND %[2]s.is_blocked = 0
		  AND %[2]s.status <> 'closed' AND %[2]s.status <> 'pinned'
		  AND %[2]s.id IN (%[3]s)
	`, table, alias, shouldBeBlockedIDsUnionScopedSQL(depTable, batchScopeSQL))
}

// unmarkBlockedTemplate is the batched unmark statement for one table: the
// batch-scoped analog of unmarkAllBlockedSQL. NOT IN is null-hostile; the
// union's d.issue_id IS NOT NULL guards keep it total.
//
//nolint:gosec // G201: table, alias, and depTable are constants from the four callers above.
func unmarkBlockedTemplate(table, alias, depTable string) string {
	return fmt.Sprintf(`
		UPDATE %[1]s %[2]s SET %[2]s.is_blocked = 0, %[2]s.updated_at = %[2]s.updated_at
		WHERE %[2]s.id IN (%%s)
		  AND %[2]s.is_blocked = 1
		  AND ( %[2]s.status = 'closed' OR %[2]s.status = 'pinned'
		        OR %[2]s.id NOT IN (%[3]s) )
	`, table, alias, shouldBeBlockedIDsUnionScopedSQL(depTable, batchScopeSQL))
}

// expandBatchTemplate fills every %s in a batched template with the same
// IN-list placeholders and repeats the bound ids once per occurrence, in
// order. Templates carry the batch list in the outer row filter and in each
// leg of the scoped union (six occurrences today); a template with a single
// %s degrades to the plain Sprintf it always was. The count is textual, so a
// template must contain no other percent sign (no LIKE 'x%' pattern, no %%)
// and at least one %s — a template with none is a programmer error that
// surfaces as an %!(EXTRA …) syntax error at exec.
//
//nolint:gosec // G201: tmpl is a constant template; only IN-clause placeholders are formatted in.
func expandBatchTemplate(tmpl, placeholders string, args []interface{}) (string, []interface{}) {
	n := strings.Count(tmpl, "%s")
	if n <= 1 {
		return fmt.Sprintf(tmpl, placeholders), args
	}
	fills := make([]interface{}, n)
	expanded := make([]interface{}, 0, n*len(args))
	for k := range fills {
		fills[k] = placeholders
		expanded = append(expanded, args...)
	}
	return fmt.Sprintf(tmpl, fills...), expanded
}

func recomputeIsBlockedPassForWispsInTx(ctx context.Context, tx DBTX, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}

	return runMarkUnmarkBatchedInTx(ctx, tx, markBlockedTemplateForWisps(), unmarkBlockedTemplateForWisps(), ids)
}

func markIsBlockedPassForWispsInTx(ctx context.Context, tx DBTX, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	return runMarkBatchedInTx(ctx, tx, markBlockedTemplateForWisps(), ids)
}

func runMarkUnmarkBatchedInTx(ctx context.Context, tx DBTX, markTmpl, unmarkTmpl string, ids []string) (int64, error) {
	var changed int64
	for start := 0; start < len(ids); start += queryBatchSize {
		end := start + queryBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		placeholders, args := buildSQLInClause(ids[start:end])

		stmt, stmtArgs := expandBatchTemplate(markTmpl, placeholders, args)
		res, err := tx.ExecContext(ctx, stmt, stmtArgs...)
		if err != nil {
			return changed, fmt.Errorf("recompute is_blocked (mark): %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return changed, fmt.Errorf("recompute is_blocked (mark rows affected): %w", err)
		}
		changed += n

		stmt, stmtArgs = expandBatchTemplate(unmarkTmpl, placeholders, args)
		res, err = tx.ExecContext(ctx, stmt, stmtArgs...)
		if err != nil {
			return changed, fmt.Errorf("recompute is_blocked (unmark): %w", err)
		}
		n, err = res.RowsAffected()
		if err != nil {
			return changed, fmt.Errorf("recompute is_blocked (unmark rows affected): %w", err)
		}
		changed += n
	}
	return changed, nil
}

func runMarkBatchedInTx(ctx context.Context, tx DBTX, markTmpl string, ids []string) (int64, error) {
	var changed int64
	for start := 0; start < len(ids); start += queryBatchSize {
		end := start + queryBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		placeholders, args := buildSQLInClause(ids[start:end])

		stmt, stmtArgs := expandBatchTemplate(markTmpl, placeholders, args)
		res, err := tx.ExecContext(ctx, stmt, stmtArgs...)
		if err != nil {
			return changed, fmt.Errorf("mark is_blocked: %w", err)
		}
		n, _ := res.RowsAffected()
		changed += n
	}
	return changed, nil
}

func AffectedByStatusChangeInTx(ctx context.Context, tx DBTX, id string) ([]string, []string, error) {
	issueSeed := []string{id}
	issueSeen := map[string]bool{id: true}
	var wispSeed []string
	wispSeen := make(map[string]bool)

	if err := loadBlockingDependersInTx(ctx, tx, "depends_on_issue_id", id, &issueSeed, issueSeen, &wispSeed, wispSeen); err != nil {
		return nil, nil, err
	}
	if err := loadWaitersWhoseSpawnerIsParentOfInTx(ctx, tx, id, false, &issueSeed, issueSeen, &wispSeed, wispSeen); err != nil {
		return nil, nil, err
	}
	// id's own status just changed, and an also_blocks waits-for edge blocks
	// while the spawner itself is open (pre-fanout window, GH#3783/GH#3875).
	// A waiter with only a DepWaitsFor edge on id (no DepBlocks edge — the
	// blocking semantics were collapsed into also_blocks) would otherwise
	// never get recomputed when its spawner closes.
	if err := loadWaitersOnSpawnerIDsInTx(ctx, tx, []string{id}, &issueSeed, issueSeen, &wispSeed, wispSeen); err != nil {
		return nil, nil, err
	}
	return expandByParentChildDescendantsInTx(ctx, tx, issueSeed, wispSeed, issueSeen, wispSeen)
}

func AffectedByStatusChangeForWispInTx(ctx context.Context, tx DBTX, id string) ([]string, []string, error) {
	var issueSeed []string
	issueSeen := make(map[string]bool)
	wispSeed := []string{id}
	wispSeen := map[string]bool{id: true}

	if err := loadBlockingDependersInTx(ctx, tx, "depends_on_wisp_id", id, &issueSeed, issueSeen, &wispSeed, wispSeen); err != nil {
		return nil, nil, err
	}
	if err := loadWaitersWhoseSpawnerIsParentOfInTx(ctx, tx, id, true, &issueSeed, issueSeen, &wispSeed, wispSeen); err != nil {
		return nil, nil, err
	}
	// See the issue-id sibling above: id's own status just changed, and a
	// waiter that waits directly on this wisp id as spawner needs to be
	// recomputed too.
	if err := loadWaitersOnSpawnerIDsInTx(ctx, tx, []string{id}, &issueSeed, issueSeen, &wispSeed, wispSeen); err != nil {
		return nil, nil, err
	}
	return expandByParentChildDescendantsInTx(ctx, tx, issueSeed, wispSeed, issueSeen, wispSeen)
}

func AffectedByDepChangeInTx(ctx context.Context, tx DBTX, source, target string, depType types.DependencyType) ([]string, []string, error) {
	switch depType {
	case types.DepBlocks, types.DepConditionalBlocks, types.DepWaitsFor, types.DepParentChild:
		issueSeed := []string{source}
		issueSeen := map[string]bool{source: true}
		var wispSeed []string
		wispSeen := map[string]bool{}
		if depType == types.DepParentChild && target != "" {
			if err := loadWaitersOnSpawnerIDsInTx(ctx, tx, []string{target}, &issueSeed, issueSeen, &wispSeed, wispSeen); err != nil {
				return nil, nil, err
			}
		}
		return expandByParentChildDescendantsInTx(ctx, tx, issueSeed, wispSeed, issueSeen, wispSeen)
	default:
		return nil, nil, nil
	}
}

func AffectedByDepChangeForWispInTx(ctx context.Context, tx DBTX, source, target string, depType types.DependencyType) ([]string, []string, error) {
	switch depType {
	case types.DepBlocks, types.DepConditionalBlocks, types.DepWaitsFor, types.DepParentChild:
		var issueSeed []string
		issueSeen := map[string]bool{}
		wispSeed := []string{source}
		wispSeen := map[string]bool{source: true}
		if depType == types.DepParentChild && target != "" {
			if err := loadWaitersOnSpawnerIDsInTx(ctx, tx, []string{target}, &issueSeed, issueSeen, &wispSeed, wispSeen); err != nil {
				return nil, nil, err
			}
		}
		return expandByParentChildDescendantsInTx(ctx, tx, issueSeed, wispSeed, issueSeen, wispSeen)
	default:
		return nil, nil, nil
	}
}

func loadBlockingDependersInTx(
	ctx context.Context, tx DBTX,
	targetCol, id string,
	issueSeed *[]string, issueSeen map[string]bool,
	wispSeed *[]string, wispSeen map[string]bool,
) error {
	return loadBlockingDependersForIDsInTx(ctx, tx, targetCol, []string{id}, issueSeed, issueSeen, wispSeed, wispSeen)
}

//nolint:gosec // G201: targetCol is one of two constant column names.
func loadBlockingDependersForIDsInTx(
	ctx context.Context, tx DBTX,
	targetCol string, ids []string,
	issueSeed *[]string, issueSeen map[string]bool,
	wispSeed *[]string, wispSeen map[string]bool,
) error {
	if len(ids) == 0 {
		return nil
	}
	tables := []struct {
		table  string
		seed   *[]string
		seen   map[string]bool
		errCtx string
	}{
		{"dependencies", issueSeed, issueSeen, "load issue dependers"},
		{"wisp_dependencies", wispSeed, wispSeen, "load wisp dependers"},
	}
	for _, id := range ids {
		for _, t := range tables {
			query := fmt.Sprintf(`
				SELECT issue_id FROM %s
				WHERE %s = ?
				  AND (type = 'blocks' OR type = 'conditional-blocks')
			`, t.table, targetCol)
			rows, err := tx.QueryContext(ctx, query, id)
			if err != nil {
				return fmt.Errorf("%s: query: %w", t.errCtx, err)
			}
			for rows.Next() {
				var dependerID string
				if err := rows.Scan(&dependerID); err != nil {
					_ = rows.Close()
					return fmt.Errorf("%s: scan: %w", t.errCtx, err)
				}
				if !t.seen[dependerID] {
					t.seen[dependerID] = true
					*t.seed = append(*t.seed, dependerID)
				}
			}
			_ = rows.Close()
			if err := rows.Err(); err != nil {
				return fmt.Errorf("%s: rows: %w", t.errCtx, err)
			}
		}
	}
	return nil
}

func AffectedByDeletionInTx(
	ctx context.Context, tx DBTX,
	deletedIssues, deletedWisps []string,
) ([]string, []string, error) {
	if len(deletedIssues) == 0 && len(deletedWisps) == 0 {
		return nil, nil, nil
	}

	issueSeen := make(map[string]bool, len(deletedIssues))
	wispSeen := make(map[string]bool, len(deletedWisps))
	for _, id := range deletedIssues {
		issueSeen[id] = true
	}
	for _, id := range deletedWisps {
		wispSeen[id] = true
	}
	var issueSeed, wispSeed []string

	if err := loadBlockingDependersForIDsInTx(ctx, tx, "depends_on_issue_id", deletedIssues, &issueSeed, issueSeen, &wispSeed, wispSeen); err != nil {
		return nil, nil, err
	}
	if err := loadBlockingDependersForIDsInTx(ctx, tx, "depends_on_wisp_id", deletedWisps, &issueSeed, issueSeen, &wispSeed, wispSeen); err != nil {
		return nil, nil, err
	}

	if err := loadWaitersOnSpawnerIDsByColInTx(ctx, tx, "depends_on_issue_id", deletedIssues, &issueSeed, issueSeen, &wispSeed, wispSeen); err != nil {
		return nil, nil, err
	}
	if err := loadWaitersOnSpawnerIDsByColInTx(ctx, tx, "depends_on_wisp_id", deletedWisps, &issueSeed, issueSeen, &wispSeed, wispSeen); err != nil {
		return nil, nil, err
	}
	for _, id := range deletedIssues {
		if err := loadWaitersWhoseSpawnerIsParentOfInTx(ctx, tx, id, false, &issueSeed, issueSeen, &wispSeed, wispSeen); err != nil {
			return nil, nil, err
		}
	}
	for _, id := range deletedWisps {
		if err := loadWaitersWhoseSpawnerIsParentOfInTx(ctx, tx, id, true, &issueSeed, issueSeen, &wispSeed, wispSeen); err != nil {
			return nil, nil, err
		}
	}

	// The deleted rows are walked THROUGH, not returned: their parent-child
	// children lose whatever they inherited and must be revisited, while the
	// deleted ids themselves have no row left to settle. That is the same
	// standing a closed child has in the walk, so they share its mechanism.
	return walkParentChildDescendantsInTx(ctx, tx, issueSeed, wispSeed, deletedIssues, deletedWisps, issueSeen, wispSeen)
}

// expandByParentChildDescendantsInTx closes the seed sets over parent-child
// descendants, in both planes, and returns the rows a recompute must visit:
// every seed, plus every LIVE descendant of a seed. See
// walkParentChildDescendantsInTx for what "live" leaves out and why.
func expandByParentChildDescendantsInTx(
	ctx context.Context, tx DBTX,
	issueSeed, wispSeed []string,
	issueSeen, wispSeen map[string]bool,
) ([]string, []string, error) {
	return walkParentChildDescendantsInTx(ctx, tx, issueSeed, wispSeed, nil, nil, issueSeen, wispSeen)
}

// walkParentChildDescendantsInTx is the descendant walk behind every affected
// set. It follows EVERY parent-child edge below the seeds and the through-ids,
// breadth-first across both planes, and returns the seeds plus the descendants
// that are neither closed nor pinned.
//
// A closed or pinned descendant is WALKED THROUGH BUT NOT RETURNED:
//
//   - Not returned, because on settled data a recompute has nothing to do
//     with it. A closed or pinned row is never blocked (BlockedStateInvariant);
//     the mark statement refuses it and it already sits at 0. Closing
//     the root of a finished epic used to hand every closed row under it to a
//     mark and an unmark statement per 200 ids to change none of them
//     (gastownhall/beads#5427, #5939).
//   - Walked through, because the rows BELOW it are not settled by that
//     argument. The stored column can be stale — the invariant's merge clause
//     admits it — and an open row under a node that was closed without a
//     recompute may still carry the flag it inherited from further up. When
//     the blockage further up moves, this walk is what visits that row on the
//     write path; a walk that stopped at the closed node would leave
//     an open, unblocked row hidden from ready work until a full repair. Once
//     visited it settles correctly whatever the closed node's own flag says,
//     because the should-be-blocked union's parent-child legs refuse a closed
//     or pinned parent (shouldBeBlockedIDsUnionScopedSQL).
//
// WHAT IS RELINQUISHED, and it is one thing: the incidental zeroing of a closed
// or pinned DESCENDANT's own stale is_blocked = 1. The unfiltered walk cleared
// it in passing; this one leaves it for the full repair, and the doctor count
// reports it. Meanwhile it cannot propagate (the union guard above) and it does
// not reach ready work, which filters on status; a reader of the bare column
// (IsBlockedInTx, IsBlockedBatchInTx) still reports the closed row as blocked.
//
// SEEDS ARE NEVER FILTERED BY STATUS. Reopen computes this set
// while the reopened row is still closed, and the row whose status moved must
// itself be settled (a blocked row that closes must drop to 0 in the same
// transaction). The status test applies to DISCOVERED rows only.
//
// throughIssues/throughWisps join the walk on the same footing as a closed
// child: expanded, never returned. AffectedByDeletionInTx passes the deleted
// ids. They must already be in the seen maps, and DISJOINT from the seeds: an id
// handed in as both is not returned, the through-list wins. No caller does
// that — deletion marks its ids seen before it loads a seed.
func walkParentChildDescendantsInTx(
	ctx context.Context, tx DBTX,
	issueSeed, wispSeed []string,
	throughIssues, throughWisps []string,
	issueSeen, wispSeen map[string]bool,
) ([]string, []string, error) {
	// The queues are the traversal frontier; the skip sets name the frontier
	// rows that are not part of the result.
	issueSkip := make(map[string]bool, len(throughIssues))
	wispSkip := make(map[string]bool, len(throughWisps))
	for _, id := range throughIssues {
		issueSkip[id] = true
	}
	for _, id := range throughWisps {
		wispSkip[id] = true
	}
	issueQueue := append(append(make([]string, 0, len(issueSeed)+len(throughIssues)), issueSeed...), throughIssues...)
	wispQueue := append(append(make([]string, 0, len(wispSeed)+len(throughWisps)), wispSeed...), throughWisps...)
	issueHead, wispHead := 0, 0

	for issueHead < len(issueQueue) || wispHead < len(wispQueue) {
		if issueHead < len(issueQueue) {
			end := issueHead + queryBatchSize
			if end > len(issueQueue) {
				end = len(issueQueue)
			}
			batch := issueQueue[issueHead:end]
			issueHead = end

			if err := appendChildrenInTx(ctx, tx, "dependencies", "depends_on_issue_id", batch, issueSeen, issueSkip, &issueQueue); err != nil {
				return nil, nil, err
			}
			if err := appendChildrenInTx(ctx, tx, "wisp_dependencies", "depends_on_issue_id", batch, wispSeen, wispSkip, &wispQueue); err != nil {
				return nil, nil, err
			}
		}
		if wispHead < len(wispQueue) {
			end := wispHead + queryBatchSize
			if end > len(wispQueue) {
				end = len(wispQueue)
			}
			batch := wispQueue[wispHead:end]
			wispHead = end

			if err := appendChildrenInTx(ctx, tx, "dependencies", "depends_on_wisp_id", batch, issueSeen, issueSkip, &issueQueue); err != nil {
				return nil, nil, err
			}
			if err := appendChildrenInTx(ctx, tx, "wisp_dependencies", "depends_on_wisp_id", batch, wispSeen, wispSkip, &wispQueue); err != nil {
				return nil, nil, err
			}
		}
	}
	return withoutSkipped(issueQueue, issueSkip), withoutSkipped(wispQueue, wispSkip), nil
}

// withoutSkipped returns the frontier minus the walked-through rows, in
// discovery order.
func withoutSkipped(queue []string, skip map[string]bool) []string {
	if len(skip) == 0 {
		return queue
	}
	kept := queue[:0:0]
	for _, id := range queue {
		if !skip[id] {
			kept = append(kept, id)
		}
	}
	return kept
}

// appendChildrenInTx appends to the frontier every parent-child child of
// parentIDs that seen does not already hold, and records in skip the ones that
// are closed or pinned (walked through, not returned — see
// walkParentChildDescendantsInTx).
//
// One IN query per call, not one query per parent: the walk is breadth-first
// and hands in at most queryBatchSize parents, so a level of N parents costs
// ceil(N/200) statements per dependency table.
//
// The child's status comes from a LEFT JOIN on purpose. A dependency row whose
// child row is missing (issue_id is a cascading foreign key, but a merge can
// land a violation, and the wisp tables are outside version control) still
// names a node with edges below it, and the unfiltered walk followed those. An
// inner join would silently stop there, which is the loss of recovery the
// closed-node rule exists to avoid; with the outer join such a child has a NULL
// status, counts as live, and costs a recompute that matches no row.
//
//nolint:gosec // G201: depTable and parentCol come from constant call sites; only IN-clause placeholders are formatted in.
func appendChildrenInTx(
	ctx context.Context, tx DBTX,
	depTable, parentCol string,
	parentIDs []string,
	seen, skip map[string]bool, queue *[]string,
) error {
	if len(parentIDs) == 0 {
		return nil
	}
	// A dependency table's issue_id always names a row in its own plane.
	childTable := "issues"
	if depTable == "wisp_dependencies" {
		childTable = "wisps"
	}
	placeholders, args := buildSQLInClause(parentIDs)
	query := fmt.Sprintf(`
		SELECT d.issue_id, c.status FROM %s d
		LEFT JOIN %s c ON c.id = d.issue_id
		WHERE d.type = 'parent-child'
		  AND d.%s IN (%s)
	`, depTable, childTable, parentCol, placeholders)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("expand children from %s on %s: %w", depTable, parentCol, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var childID string
		var status sql.NullString
		if err := rows.Scan(&childID, &status); err != nil {
			return fmt.Errorf("expand children: scan: %w", err)
		}
		// A child with two parents in the batch comes back twice.
		if seen[childID] {
			continue
		}
		seen[childID] = true
		*queue = append(*queue, childID)
		// The same pair the mark/unmark templates treat as never blocked — not
		// the done category, which they treat as live.
		if status.Valid && (types.Status(status.String) == types.StatusClosed || types.Status(status.String) == types.StatusPinned) {
			skip[childID] = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("expand children: rows: %w", err)
	}
	return nil
}

func loadWaitersWhoseSpawnerIsParentOfInTx(
	ctx context.Context, tx DBTX,
	childID string, childIsWisp bool,
	issueSeed *[]string, issueSeen map[string]bool,
	wispSeed *[]string, wispSeen map[string]bool,
) error {
	depTable := "dependencies"
	if childIsWisp {
		depTable = "wisp_dependencies"
	}
	//nolint:gosec // G201: depTable is one of two constant values.
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT depends_on_issue_id, depends_on_wisp_id
		FROM %s
		WHERE issue_id = ? AND type = 'parent-child'
	`, depTable), childID)
	if err != nil {
		return fmt.Errorf("waiters on parent of %s: load parents: %w", childID, err)
	}
	var issueParentIDs, wispParentIDs []string
	for rows.Next() {
		var ip, wp sql.NullString
		if err := rows.Scan(&ip, &wp); err != nil {
			_ = rows.Close()
			return fmt.Errorf("waiters on parent of %s: scan: %w", childID, err)
		}
		if ip.Valid {
			issueParentIDs = append(issueParentIDs, ip.String)
		}
		if wp.Valid {
			wispParentIDs = append(wispParentIDs, wp.String)
		}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("waiters on parent of %s: rows: %w", childID, err)
	}

	if len(issueParentIDs) > 0 {
		if err := loadWaitersOnSpawnerIDsByColInTx(ctx, tx, "depends_on_issue_id", issueParentIDs, issueSeed, issueSeen, wispSeed, wispSeen); err != nil {
			return err
		}
	}
	if len(wispParentIDs) > 0 {
		if err := loadWaitersOnSpawnerIDsByColInTx(ctx, tx, "depends_on_wisp_id", wispParentIDs, issueSeed, issueSeen, wispSeed, wispSeen); err != nil {
			return err
		}
	}
	return nil
}

func loadWaitersOnSpawnerIDsInTx(
	ctx context.Context, tx DBTX,
	spawnerIDs []string,
	issueSeed *[]string, issueSeen map[string]bool,
	wispSeed *[]string, wispSeen map[string]bool,
) error {
	if err := loadWaitersOnSpawnerIDsByColInTx(ctx, tx, "depends_on_issue_id", spawnerIDs, issueSeed, issueSeen, wispSeed, wispSeen); err != nil {
		return err
	}
	return loadWaitersOnSpawnerIDsByColInTx(ctx, tx, "depends_on_wisp_id", spawnerIDs, issueSeed, issueSeen, wispSeed, wispSeen)
}

//nolint:gosec // G201: targetCol is one of two constant column names.
func loadWaitersOnSpawnerIDsByColInTx(
	ctx context.Context, tx DBTX,
	targetCol string, spawnerIDs []string,
	issueSeed *[]string, issueSeen map[string]bool,
	wispSeed *[]string, wispSeen map[string]bool,
) error {
	if len(spawnerIDs) == 0 {
		return nil
	}
	tables := []struct {
		table  string
		seed   *[]string
		seen   map[string]bool
		errCtx string
	}{
		{"dependencies", issueSeed, issueSeen, "load issue waiters"},
		{"wisp_dependencies", wispSeed, wispSeen, "load wisp waiters"},
	}
	for _, spawnerID := range spawnerIDs {
		for _, t := range tables {
			query := fmt.Sprintf(`
				SELECT issue_id FROM %s
				WHERE type = 'waits-for' AND %s = ?
			`, t.table, targetCol)
			rows, err := tx.QueryContext(ctx, query, spawnerID)
			if err != nil {
				if optionalBlockedTable(t.table) && isTableNotExistError(err) {
					continue
				}
				return fmt.Errorf("%s: query: %w", t.errCtx, err)
			}
			for rows.Next() {
				var waiterID string
				if err := rows.Scan(&waiterID); err != nil {
					_ = rows.Close()
					return fmt.Errorf("%s: scan: %w", t.errCtx, err)
				}
				if !t.seen[waiterID] {
					t.seen[waiterID] = true
					*t.seed = append(*t.seed, waiterID)
				}
			}
			_ = rows.Close()
			if err := rows.Err(); err != nil {
				return fmt.Errorf("%s: rows: %w", t.errCtx, err)
			}
		}
	}
	return nil
}
