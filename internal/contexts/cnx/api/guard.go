package api

import (
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// TradeComplianceGuard v1 (Context Map R58 CNX->TRD and R56 CNX->FUL, Customer/Supplier with a
// guard function, guardLock true; TRD-INV-023, CNX-INV-001, ADR-BR-004 option B, ADR-BR-011,
// ADR-BR-014) lets TRD's order.activate and FUL's release re-check, inside their own transaction,
// that a valid clear trade compliance conclusion covers the order revision they act on, and keep it
// so until that transaction ends. CNX publishes it as a SECURITY DEFINER function in its schema
// with a fixed search_path (migrations/cnx/000001_cnx_trade_compliance.sql):
//
//	brant_cnx.guard_trade_compliance_v1(p_order_id text, p_order_revision bigint, p_order_hash text, p_at timestamptz)
//	    RETURNS TABLE (clear boolean, status text, reason text, check_id text, decision_id text, license_required boolean,
//	                   regulation_id text, regulation_revision bigint, valid_until timestamptz, code text)
//
// It always returns one row, in the caller's scope (app.tenant_id and app.company_id, which
// bento.ScopeTransactor sets), for the order revision at the instant p_at the caller relies on the
// conclusion. It locks the check of (order, revision) FOR SHARE in the caller's transaction:
// trade_compliance.decide, .evaluate and .reopen lock that row FOR UPDATE, so each either committed
// before the guard read it, and the verdict says so, or waits until the caller's transaction ends.
// A revision without a check locks nothing: a check is opened pending, so no conclusion can appear
// in between. Clear is true, and code NULL, only for a clear conclusion whose validity contains
// p_at, on a check that binds exactly the order hash; otherwise code is TRADE_COMPLIANCE_NOT_CLEAR
// and status and reason say why: pending (not_checked, order_revised — only another revision of the
// order has a check, "订单变更后旧结论不适用" — not_evaluated, awaiting_decision), blocked (blocked),
// expired (expired) or the check's own status with order_changed for another order hash. A blocked
// conclusion is never cleared by a business approval: nothing but compliance's evaluate and decide
// moves it. license_required is the clear conclusion's statement that the goods need an export
// licence (FUL's ExportLicenseGuard, R56, S5-14), false otherwise.
//
// The guard does not apply the caller's data scope and does not re-read POL: a new
// screening_regulation revision reopens clear conclusions through trade_compliance.reopen (system,
// S5 D-18), and each conclusion's validity never passes its regulation revision's. A caller locks
// its own aggregate root FOR UPDATE first and calls the guard after, CNX after CAT in context code
// order (ADR-BR-011):
//
//	b := cnxapi.OrderBinding{OrderID: id, OrderRevision: rev, OrderHash: hash}
//	row := exec.QueryRow(ctx, cnxapi.TradeComplianceGuardSQL, cnxapi.TradeComplianceGuardArgs(b, now)...)
//	verdict, err := cnxapi.ScanTradeComplianceGuard(b, row.Scan)
//	if err == nil {
//		err = verdict.Check("/args/order_id")
//	}
//	// verdict.Evidence() names what the caller's commit evidence records.
const (
	TradeComplianceGuardFunction = "brant_cnx.guard_trade_compliance_v1"
	TradeComplianceGuardSQL      = `SELECT clear, status, reason, check_id, decision_id, license_required, regulation_id, regulation_revision,
		valid_until, code FROM brant_cnx.guard_trade_compliance_v1($1, $2, $3, $4)`
	// TradeComplianceGuardName is the guard's name in a caller's commit evidence (TRD's
	// GuardCNXTradeCompliance).
	TradeComplianceGuardName = "cnx.trade_compliance_guard.v1"
)

// OrderBinding is an order revision as the guard's caller acts on it: the order, its order revision
// and its order hash (TRD's OrderBinding).
type OrderBinding struct {
	OrderID       string
	OrderRevision int64
	OrderHash     string
}

// TradeComplianceGuardArgs returns the arguments of TradeComplianceGuardSQL for b at instant at.
func TradeComplianceGuardArgs(b OrderBinding, at time.Time) []any {
	return []any{b.OrderID, b.OrderRevision, b.OrderHash, kernel.Instant(at)}
}

// TradeComplianceVerdict is the guard's answer for Ref. CheckID and DecisionID are "" without a
// check or a current decision; RegulationID, RegulationRevision and ValidUntil are the current
// decision's (ValidUntil zero for blocked).
type TradeComplianceVerdict struct {
	Ref                OrderBinding
	Clear              bool
	Status             string
	Reason             string
	CheckID            string
	DecisionID         string
	LicenseRequired    bool
	RegulationID       string
	RegulationRevision int64
	ValidUntil         time.Time
	Code               string
}

// ScanTradeComplianceGuard reads the row of TradeComplianceGuardSQL for b through scan, such as a
// pgx row's Scan.
func ScanTradeComplianceGuard(b OrderBinding, scan func(dest ...any) error) (TradeComplianceVerdict, error) {
	v := TradeComplianceVerdict{Ref: b}
	var status, reason, check, decision, regulation, code *string
	var revision *int64
	var until *time.Time
	if err := scan(&v.Clear, &status, &reason, &check, &decision, &v.LicenseRequired, &regulation, &revision, &until, &code); err != nil {
		return TradeComplianceVerdict{}, err
	}
	for _, f := range []struct {
		from *string
		to   *string
	}{{status, &v.Status}, {reason, &v.Reason}, {check, &v.CheckID}, {decision, &v.DecisionID}, {regulation, &v.RegulationID}, {code, &v.Code}} {
		if f.from != nil {
			*f.to = *f.from
		}
	}
	if revision != nil {
		v.RegulationRevision = *revision
	}
	if until != nil {
		v.ValidUntil = kernel.Instant(*until)
	}
	return v, nil
}

// Check returns nil when a valid clear conclusion covers the order revision and
// TRADE_COMPLIANCE_NOT_CLEAR (409) at path of the caller's request otherwise, with the status and
// reason. A clear row that names a code is never taken as clear.
func (v TradeComplianceVerdict) Check(path string) error {
	if v.Clear && v.Code == "" {
		return nil
	}
	return Reject(CodeTradeComplianceNotClear, path, "Trade compliance is "+v.Status+": "+v.Reason)
}

// Evidence is the value a caller's commit evidence records under TradeComplianceGuardName:
// status/reason/check_id/decision_id/license_required.
func (v TradeComplianceVerdict) Evidence() string {
	return v.Status + "/" + v.Reason + "/" + v.CheckID + "/" + v.DecisionID + "/" + strconv.FormatBool(v.LicenseRequired)
}
