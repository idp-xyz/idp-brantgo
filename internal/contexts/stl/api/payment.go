package api

import (
	"fmt"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// PaymentConditionGuard v1 (Context Map R45 STL→FUL, Customer/Supplier with guardLock: 合同付款条件
// 满足状态; risk 撤销核销或退汇后仍放单; STL-INV-003, UL-INV-28, MD-37; S5 plan D-4, D-5) lets FUL re-check,
// inside its own transaction, that an order revision's payment condition is met for a release scope,
// and keeps it so until that transaction ends. STL publishes it as a SECURITY DEFINER function in its
// schema with a fixed search_path (migrations/stl/000002_stl_funds_applications.sql):
//
//	brant_stl.guard_payment_condition_v1(p_order_id text, p_order_revision bigint, p_scope text, p_at timestamptz)
//	    RETURNS TABLE (status text, reason text, order_revision bigint, order_hash text, currency text,
//	                   required_minor numeric, applied_minor numeric, stage_id text, stage_trigger text,
//	                   stage_due_minor numeric, stage_applied_minor numeric, shortfall_minor numeric,
//	                   checked_at timestamptz, code text)
//
// It always returns one row in the caller's scope (app.tenant_id and app.company_id) and locks the
// order's account row FOR SHARE in the caller's transaction. application.apply and
// application.unapply lock that row FOR UPDATE (after the fund record, in ID order), so an unapply
// either committed before the guard read the account, and the verdict says so, or waits until the
// caller's transaction ends; a guard called while an unapply holds the row waits and then judges
// what it left. The guard locks no other row.
//
// p_scope is shipment (every stage due on order confirmation or before shipment is fully applied)
// or documents (those and every after_shipment or after_invoice stage under document control, D-5).
// Applied amounts are the applications that stand on the stages; an unexplained fund difference
// never counts, because no application can use it. Its status is satisfied or blocked; reason is
// empty for satisfied, otherwise no_account (the order has no account: STL has no stages to judge
// by), revision_mismatch (the account holds another revision, order_revision says which: its payment
// plan review is pending) or stage_unpaid (the first required stage, in plan order, that is not
// fully applied: stage_id, stage_trigger, stage_due_minor, stage_applied_minor, shortfall_minor).
// required_minor and applied_minor total the scope's stages, in minor units of currency. checked_at
// echoes p_at. Code is empty for satisfied and COMMERCIAL_RELEASE_BLOCKED (409) otherwise.
//
// FUL calls it at commercial_release.evaluate, shipment.confirm and document_release.record (unit
// C1), after locking its own roots FOR UPDATE and in context code order (ADR-BR-011), with its own
// executor:
//
//	q := stlapi.PaymentQuery{OrderID: id, OrderRevision: rev, Scope: stlapi.ScopeShipment, At: now}
//	verdict, err := stlapi.ScanPaymentCondition(q, exec.QueryRow(ctx, stlapi.PaymentConditionSQL, stlapi.PaymentConditionArgs(q)...).Scan)
//	if err == nil {
//		err = verdict.Check("/args/shipment_id")
//	}
//
// and records verdict.OrderRevision, RequiredMinor and AppliedMinor in its commit evidence (or, for
// an exception, StageID and ShortfallMinor). The guard does not apply the caller's data scope.
const (
	PaymentConditionFunction = "brant_stl.guard_payment_condition_v1"
	PaymentConditionSQL      = `SELECT status, reason, order_revision, order_hash, currency, required_minor::text, applied_minor::text,
		stage_id, stage_trigger, stage_due_minor::text, stage_applied_minor::text, shortfall_minor::text, checked_at, code
		FROM brant_stl.guard_payment_condition_v1($1, $2, $3, $4)`

	ScopeShipment  = "shipment"
	ScopeDocuments = "documents"

	PaymentSatisfied = "satisfied"
	PaymentBlocked   = "blocked"

	PaymentNoAccount        = "no_account"
	PaymentRevisionMismatch = "revision_mismatch"
	PaymentStageUnpaid      = "stage_unpaid"
)

// PaymentQuery asks whether order OrderID's revision OrderRevision meets its payment condition for
// Scope (shipment or documents) at instant At.
type PaymentQuery struct {
	OrderID       string
	OrderRevision int64
	Scope         string
	At            time.Time
}

// PaymentConditionArgs returns the arguments of PaymentConditionSQL for q.
func PaymentConditionArgs(q PaymentQuery) []any {
	return []any{q.OrderID, q.OrderRevision, q.Scope, kernel.Instant(q.At)}
}

// PaymentVerdict is the guard's answer for Query (see PaymentConditionSQL).
type PaymentVerdict struct {
	Query             PaymentQuery
	Status            string
	Reason            string
	OrderRevision     int64
	OrderHash         string
	Currency          string
	RequiredMinor     string
	AppliedMinor      string
	StageID           string
	StageTrigger      string
	StageDueMinor     string
	StageAppliedMinor string
	ShortfallMinor    string
	CheckedAt         time.Time
	Code              string
}

// ScanPaymentCondition reads the row of PaymentConditionSQL for q through scan, such as a pgx row's
// Scan.
func ScanPaymentCondition(q PaymentQuery, scan func(dest ...any) error) (PaymentVerdict, error) {
	v := PaymentVerdict{Query: q}
	var rev *int64
	var hash, currency, required, applied, stage, trigger, due, stageApplied, shortfall, code *string
	var checked *time.Time
	if err := scan(&v.Status, &v.Reason, &rev, &hash, &currency, &required, &applied, &stage, &trigger, &due, &stageApplied, &shortfall,
		&checked, &code); err != nil {
		return PaymentVerdict{}, err
	}
	if rev != nil {
		v.OrderRevision = *rev
	}
	for _, f := range []struct {
		from *string
		to   *string
	}{{hash, &v.OrderHash}, {currency, &v.Currency}, {required, &v.RequiredMinor}, {applied, &v.AppliedMinor}, {stage, &v.StageID},
		{trigger, &v.StageTrigger}, {due, &v.StageDueMinor}, {stageApplied, &v.StageAppliedMinor}, {shortfall, &v.ShortfallMinor},
		{code, &v.Code}} {
		if f.from != nil {
			*f.to = *f.from
		}
	}
	if checked != nil {
		v.CheckedAt = kernel.Instant(*checked)
	}
	return v, nil
}

// Satisfied reports whether the order revision's payment condition is met for the query's scope.
func (v PaymentVerdict) Satisfied() bool {
	return v.Status == PaymentSatisfied && v.Code == "" && v.OrderRevision == v.Query.OrderRevision
}

// Check returns nil when the verdict is satisfied and COMMERCIAL_RELEASE_BLOCKED (409) at path of the
// caller's request otherwise, saying why.
func (v PaymentVerdict) Check(path string) error {
	if v.Satisfied() {
		return nil
	}
	var reason string
	switch v.Reason {
	case PaymentNoAccount:
		reason = "Order " + v.Query.OrderID + " has no order account in STL"
	case PaymentRevisionMismatch:
		reason = fmt.Sprintf("The order account holds revision %d, not %d: its payment plan review is pending", v.OrderRevision,
			v.Query.OrderRevision)
	default:
		reason = fmt.Sprintf("Payment stage %s (%s) has %s of %s %s applied: %s short for %s", v.StageID, v.StageTrigger, v.StageAppliedMinor,
			v.StageDueMinor, v.Currency, v.ShortfallMinor, v.Query.Scope)
	}
	return Reject(CodeCommercialReleaseBlocked, path, reason)
}
