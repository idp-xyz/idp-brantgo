package application

import (
	"context"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
)

// Orders is PUR's port to TRD (Context Map R19 TRD→PUR, Customer/Supplier, "ActiveOrderRef v1",
// guardLock true; risk: 订单审批撤销后仍编制草稿). Everything PUR needs of TRD passes through it:
//
//   - ResolveOrders, before PUR's transaction: what TRD says of each order now (TRD
//     ActiveOrderLookups): the binding (order revision and hash), state, the quote revision it
//     converted, the review whose approval activated it, its source RFQ, the cost snapshot its quote
//     adopted, the customer's delivery requirement and its lines, and Code, the first reason it does
//     not serve new purchase work. Orders outside the caller's company or data scope are absent.
//   - GuardOrder, inside PUR's transaction with PUR's own executor, after PUR locked its own roots
//     FOR UPDATE (ADR-BR-011): TRD's OrderGuard v1 (brant_trd.guard_order_active_v1) re-checks the
//     binding and locks the exact order row FOR SHARE until PUR's transaction ends, so an approval
//     revocation, a cancellation or a closure either committed first and the draft is refused, or
//     waits for PUR to commit (ADR-BR-004 option B).
//
// PUR never calls TRD's transaction owner. The adapter on trdapi (ActiveOrderLookups and
// OrderGuardSQL) arrives with TRD's sales orders (S3-07); until then tests implement the port.
type Orders interface {
	ResolveOrders(ctx context.Context, orderIDs []string) (map[string]Order, error)
	GuardOrder(ctx context.Context, exec bentopg.Executor, b OrderBinding) (OrderVerdict, error)
}

// Codes TRD answers about an order, which PUR passes on (error catalog: ORDER_NOT_ACTIVE,
// ORDER_APPROVAL_REQUIRED and ORDER_APPROVAL_REVOKED list PUR; REFERENCE_STALE for an order at
// another revision or hash than bound).
const (
	OrderNotActive        = "ORDER_NOT_ACTIVE"
	OrderApprovalRequired = "ORDER_APPROVAL_REQUIRED"
	OrderApprovalRevoked  = "ORDER_APPROVAL_REVOKED"
	OrderStale            = "REFERENCE_STALE"
)

// OrderBinding is an order as a draft binds it: at an order revision and its order hash (TRD
// OrderBinding).
type OrderBinding struct {
	OrderID       string
	OrderRevision int64
	OrderHash     string
}

// OrderUnit is a POL unit revision.
type OrderUnit struct {
	ID       string
	Revision int64
}

// OrderLine is an order line (TRD OrderLine): its RFQ line, SKU specification revision, quantity
// (kernel.QuantityFormat) and unit.
type OrderLine struct {
	LineID       string
	RfqLineID    string
	SkuID        string
	SpecRevision int64
	Quantity     string
	Unit         OrderUnit
}

// OrderRfq is the order's source RFQ at the row version its quote bound (INQ RfqRef v1).
type OrderRfq struct {
	RfqID      string
	RowVersion int64
}

// OrderSnapshot is the cost snapshot the order's quote adopted (CST CostSnapshotRef v1).
type OrderSnapshot struct {
	SnapshotID string
	ResultHash string
}

// OrderDelivery is the customer's delivery requirement the order's quote states: kind (fixed or
// lead_time), date (fixed), milestone, the place's location ID and the IANA time zone.
type OrderDelivery struct {
	Kind      string
	Date      string
	Milestone string
	PlaceID   string
	TimeZone  string
}

// Order is what TRD says of an order now (TRD ActiveOrderFact). Code is "" when it serves new
// purchase work, otherwise OrderNotActive, OrderApprovalRequired, OrderApprovalRevoked or OrderStale.
type Order struct {
	Binding          OrderBinding
	Status           string
	Code             string
	QuoteID          string
	BusinessRevision int64
	BusinessHash     string
	ReviewID         string
	CustomerID       string
	Rfq              *OrderRfq
	CostSnapshot     *OrderSnapshot
	Delivery         *OrderDelivery
	Lines            []OrderLine
}

// OrderVerdict is OrderGuard v1's answer: Known false for an order not in the caller's company;
// otherwise its state and Code ("" when it is active at the bound revision and hash with its
// activating approval in force).
type OrderVerdict struct {
	Known  bool
	Status string
	Code   string
}
