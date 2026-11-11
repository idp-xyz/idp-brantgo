package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// ActiveOrderRef v1 (Context Map R19 TRD->PUR, Customer/Supplier with guardLock: 已生效且订单审批未撤销的
// 订单行与商业快照; risk 订单审批撤销后仍编制草稿). PUR resolves an order before its transaction
// (ActiveOrderLookups) and binds it as an OrderBinding — the order, its order revision and order
// hash — then re-checks it inside its own transaction with OrderGuard v1 and keeps it so until that
// transaction ends.
//
// OrderGuard v1 is a SECURITY DEFINER function in TRD's schema with a fixed search_path
// (migrations/trd/000005_trd_sales_orders.sql):
//
//	brant_trd.guard_order_active_v1(p_order_id text, p_order_revision bigint, p_order_hash text)
//	    RETURNS TABLE (known boolean, status text, order_revision bigint, order_hash text, quote_id text, revision bigint,
//	                   business_hash text, review_id text, approval_revoked boolean, code text)
//
// It always returns one row in the caller's scope (app.tenant_id and app.company_id) and locks the
// one order row FOR SHARE in the caller's transaction. order.approval.revoke, the order's activation
// and every later order command lock that row FOR UPDATE and move its row version, so each either
// committed before the guard read it, and the verdict says so, or waits until the caller's
// transaction ends; a guard called while one of them holds the row waits for it and reads the order
// as it left it. The guard locks no quote row: TRD's order commands lock the quote root first and
// the order second, and a downstream that locked its own root first and then the order never waits
// on a TRD lock held in the other order. A caller locks its own aggregate root FOR UPDATE first:
//
//	b := trdapi.OrderBinding{OrderID: id, OrderRevision: rev, OrderHash: hash}
//	row := exec.QueryRow(ctx, trdapi.OrderGuardSQL, trdapi.OrderGuardArgs(b)...)
//	verdict, err := trdapi.ScanOrderGuard(b, row.Scan)
//	if err == nil {
//		err = verdict.Check("/args/order_ref")
//	}
//
// The guard does not apply the caller's data scope (own, team): the caller checks visibility when it
// resolves the order before its transaction (ActiveOrderLookups).
const (
	OrderGuardFunction = "brant_trd.guard_order_active_v1"
	OrderGuardSQL      = `SELECT known, status, order_revision, order_hash, quote_id, revision, business_hash, review_id, approval_revoked,
		code FROM brant_trd.guard_order_active_v1($1, $2, $3)`
)

// OrderBinding is an order as a downstream fact binds it: the order at an order revision and its
// order hash.
type OrderBinding struct {
	OrderID       string
	OrderRevision int64
	OrderHash     string
}

// OrderGuardArgs returns the arguments of OrderGuardSQL for b.
func OrderGuardArgs(b OrderBinding) []any { return []any{b.OrderID, b.OrderRevision, b.OrderHash} }

// OrderVerdict is the guard's answer for Ref. Known is false, with Code REFERENCE_INVALID, for an
// order that does not exist in the caller's company. Otherwise the fields are the order's — its
// state, order revision and hash, the quote revision it converted, the review whose approval
// activated it and whether that approval was revoked — and Code is empty when the order is active at
// the bound revision and hash with its activating approval in force, REFERENCE_STALE (409) at
// another order revision or hash, ORDER_NOT_ACTIVE when it is not active, and
// ORDER_APPROVAL_REVOKED when its activating approval was revoked.
type OrderVerdict struct {
	Ref              OrderBinding
	Known            bool
	Status           string
	OrderRevision    int64
	OrderHash        string
	QuoteID          string
	BusinessRevision int64
	BusinessHash     string
	ReviewID         string
	ApprovalRevoked  bool
	Code             string
}

// ScanOrderGuard reads the row of OrderGuardSQL for b through scan, such as a pgx row's Scan.
func ScanOrderGuard(b OrderBinding, scan func(dest ...any) error) (OrderVerdict, error) {
	v := OrderVerdict{Ref: b}
	var status, hash, quote, bhash, review, code *string
	var revision, brev *int64
	if err := scan(&v.Known, &status, &revision, &hash, &quote, &brev, &bhash, &review, &v.ApprovalRevoked, &code); err != nil {
		return OrderVerdict{}, err
	}
	for _, f := range []struct {
		from *string
		to   *string
	}{{status, &v.Status}, {hash, &v.OrderHash}, {quote, &v.QuoteID}, {bhash, &v.BusinessHash}, {review, &v.ReviewID}, {code, &v.Code}} {
		if f.from != nil {
			*f.to = *f.from
		}
	}
	if revision != nil {
		v.OrderRevision = *revision
	}
	if brev != nil {
		v.BusinessRevision = *brev
	}
	return v, nil
}

// Usable reports whether the order is active at the bound revision and hash with its activating
// approval in force.
func (v OrderVerdict) Usable() bool { return v.Known && v.Code == "" }

// Check returns nil for a usable order and its refusal at path of the caller's request otherwise:
// REFERENCE_INVALID, REFERENCE_STALE (409), ORDER_NOT_ACTIVE or ORDER_APPROVAL_REVOKED.
func (v OrderVerdict) Check(path string) error {
	switch {
	case v.Usable():
		return nil
	case !v.Known || v.Code == "":
		return kernel.Reject(kernel.CodeReferenceInvalid, path, "")
	}
	return refuse(v.Code, path)
}

// OrderLine is an order line as ActiveOrderRef v1 shows it: the quoted line (its RFQ line, SKU
// specification, quantity and unit, unit price), its quantity in the costing's unit (the
// calculation's cost scope binding) and its amounts in the quote currency's minor units.
type OrderLine struct {
	LineID             string
	RfqLineID          string
	Sku                SkuRef
	Quantity           string
	UnitRef            ConfigRef
	NormalizedQuantity string
	CostUnit           string
	UnitPrice          string
	NetMinor           string
	TaxMinor           string
	PayableMinor       string
}

// ActiveOrderFact is what TRD says of an order now (ActiveOrderRef v1): its binding (OrderBinding),
// state and row version, the quote revision it converted with the customer, currency, source RFQ,
// cost snapshot, delivery requirement and lines of its snapshot, when and on which review it was activated, whether that approval was revoked, and
// Code, the first reason it does not serve new downstream work now ("" when it does; the guard's
// codes).
type ActiveOrderFact struct {
	Binding          OrderBinding
	Status           string
	RowVersion       int64
	QuoteID          string
	BusinessRevision int64
	BusinessHash     string
	AcceptanceID     string
	CustomerRef      PartyRef
	CurrencyRef      ConfigRef
	RfqRef           RfqRef          // the source RFQ the quote answered (PUR-INV-004)
	CostSnapshotRef  CostSnapshotRef // the cost snapshot the quote adopted (R21)
	Delivery         Delivery        // the customer's delivery requirement (PUR-INV-006)
	Lines            []OrderLine
	ReviewID         string
	ActivatedAt      time.Time
	ApprovalRevoked  bool
	Code             string
}

// Check returns nil when the order serves new downstream work now and its refusal at path
// otherwise.
func (f ActiveOrderFact) Check(path string) error {
	if f.Code == "" {
		return nil
	}
	return refuse(f.Code, path)
}

// MaxOrderRefs bounds one order lookup.
const MaxOrderRefs = 200

// ActiveOrderLookups answer ActiveOrderRef v1 lookups of the contexts downstream of TRD (PUR, R19).
// Like Lookups, they need no permission beyond an active grant; orders of quotes outside the
// caller's data scope are absent. Ask before opening your own transaction; inside it, use
// OrderGuard v1.
type ActiveOrderLookups interface {
	// ResolveActiveOrders answers at most MaxOrderRefs order IDs.
	ResolveActiveOrders(ctx context.Context, orderIDs []string) (map[string]ActiveOrderFact, error)
}

// ValidateOrderIDs checks a lookup's order IDs.
func ValidateOrderIDs(ids []string) error {
	if len(ids) > MaxOrderRefs {
		return schema("/order_ids")
	}
	for i, id := range ids {
		if !kernel.ValidIdentifier(id) {
			return schema(index("/order_ids", i))
		}
	}
	return nil
}
