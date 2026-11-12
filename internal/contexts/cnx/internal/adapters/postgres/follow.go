package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	tcc "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/tradecompliancecheck"
	"go.idp.xyz/idp-brant/internal/kernel"
	refapi "go.idp.xyz/idp-brant/internal/reference/api"
)

// ClearRow is a clear check with its current decision, as the follow-up reads and consumers find
// it.
type ClearRow struct {
	Check           *tcc.Check
	DecisionID      string
	Regulation      tcc.RegulationRef
	LicenseRequired bool
	ValidUntil      time.Time
}

const clearColumns = `c.check_id, c.order_id, c.order_revision, c.order_hash, c.status, c.row_version,
	coalesce(c.current_evaluation_id, ''), coalesce(c.current_decision_id, ''), coalesce(c.previous_check_id, ''), c.owner_actor_id,
	coalesce(c.owner_team_id, ''), c.opened_by_kind, c.opened_by_id, c.open_epoch, c.opened_at,
	d.regulation_id, d.regulation_revision, d.regulation_hash, d.license_required, d.valid_until`

const clearFrom = ` FROM brant_cnx.trade_compliance_check c JOIN brant_cnx.trade_compliance_decision d
	ON d.tenant_id = c.tenant_id AND d.company_id = c.company_id AND d.decision_id = c.current_decision_id
	WHERE c.tenant_id = $1 AND c.company_id = $2 AND c.status = 'clear'`

// The list a clear conclusion's evaluation used, joined on the check's current evaluation.
const usedList = ` AND EXISTS (SELECT 1 FROM brant_cnx.trade_compliance_list_used u WHERE u.tenant_id = c.tenant_id
	AND u.company_id = c.company_id AND u.evaluation_id = c.current_evaluation_id AND u.list_code = $3`

func clearRows(ctx context.Context, exec bentopg.Executor, sql string, args ...any) ([]ClearRow, error) {
	rows, err := exec.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ClearRow
	for rows.Next() {
		var c tcc.Check
		var r ClearRow
		var status, kind string
		var until *time.Time
		if err := rows.Scan(&c.ID, &c.Binding.OrderID, &c.Binding.OrderRevision, &c.Binding.OrderHash, &status, &c.RowVersion, &c.Evaluation,
			&c.Decision, &c.Previous, &c.OwnerActor, &c.OwnerTeam, &kind, &c.OpenedBy.ID, &c.OpenEpoch, &c.OpenedAt,
			&r.Regulation.ID, &r.Regulation.Revision, &r.Regulation.ContentHash, &r.LicenseRequired, &until); err != nil {
			return nil, err
		}
		c.Status, c.OpenedBy.Kind, c.OpenedAt = tcc.Status(status), kernel.ActorKind(kind), kernel.Instant(c.OpenedAt)
		if until != nil {
			r.ValidUntil = kernel.Instant(*until)
		}
		r.Check, r.DecisionID = &c, c.Decision
		out = append(out, r)
	}
	return out, rows.Err()
}

// ClearByRegulation returns the clear checks whose current decision follows screening_regulation
// regulationID — at revision only when revision is positive — in check ID order, without locks.
func ClearByRegulation(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, regulationID string, revision int64) ([]ClearRow, error) {
	return clearRows(ctx, exec, `SELECT `+clearColumns+clearFrom+` AND d.regulation_id = $3 AND ($4::bigint = 0 OR d.regulation_revision = $4)
		ORDER BY c.check_id`, scoped(scope, regulationID, revision)...)
}

// LockClearBeforeRevision locks FOR UPDATE, in check ID order, the clear checks whose current
// decision follows screening_regulation regulationID at a revision before revision, and returns
// them as locked.
func LockClearBeforeRevision(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, regulationID string, revision int64) ([]ClearRow, error) {
	return clearRows(ctx, exec, `SELECT `+clearColumns+clearFrom+` AND d.regulation_id = $3 AND d.regulation_revision < $4
		ORDER BY c.check_id FOR UPDATE OF c`, scoped(scope, regulationID, revision)...)
}

// ClearByList returns the clear checks whose current evaluation used programme list listCode — at
// version only when version is not empty — in check ID order, without locks.
func ClearByList(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, listCode, version string) ([]ClearRow, error) {
	return clearRows(ctx, exec, `SELECT `+clearColumns+clearFrom+usedList+` AND ($4 = '' OR u.version = $4))
		ORDER BY c.check_id`, scoped(scope, listCode, version)...)
}

// LockClearOnOutdatedList locks FOR UPDATE, in check ID order, the clear checks whose current
// evaluation used programme list listCode at a version that is not the list's current one, as
// reference data's list function answers it without a lock; the reopening holds the list FOR
// SHARE when it decides.
func LockClearOnOutdatedList(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, listCode string) ([]ClearRow, error) {
	return clearRows(ctx, exec, `SELECT `+clearColumns+clearFrom+usedList+` AND u.version IS DISTINCT FROM `+
		refapi.CountryProgrammeListGuardFunction+`(u.list_code, false))
		ORDER BY c.check_id FOR UPDATE OF c`, scoped(scope, listCode)...)
}

// ListsUsed returns the programme list versions evaluation evaluationID used, by list code.
func ListsUsed(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, evaluationID string) ([]tcc.ListVersion, error) {
	rows, err := exec.Query(ctx, `SELECT list_code, jurisdiction, version, content_hash FROM brant_cnx.trade_compliance_list_used
		WHERE tenant_id = $1 AND company_id = $2 AND evaluation_id = $3 ORDER BY list_code`, scoped(scope, evaluationID)...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (tcc.ListVersion, error) {
		var v tcc.ListVersion
		err := row.Scan(&v.ListCode, &v.Jurisdiction, &v.Version, &v.ContentHash)
		return v, err
	})
}
