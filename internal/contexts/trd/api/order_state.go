package api

// The order-state read (S5 unit B4; the pattern of S5 decision D-1, ADR-BR-018): what an order is
// now, read without a lock by a downstream context inside its own transaction, after its own locks.
// It is a SECURITY DEFINER function in TRD's schema with a fixed search_path
// (migrations/trd/000008_trd_order_state.sql):
//
//	brant_trd.order_state_v1(p_order_id text)
//	    RETURNS TABLE (known boolean, status text, order_revision bigint, order_hash text, row_version bigint)
//
// It always returns one row in the caller's scope (app.tenant_id, app.company_id); known is false
// for an order that does not exist there. In READ COMMITTED each statement reads what committed
// before it began, so a caller that first waited for a lock a TRD command held through one of the
// caller's guards (STL's CreditGuard v1, held FOR SHARE by order.activate and
// order.amendment.apply) reads the order as that command left it:
//
//	s, err := trdapi.ScanOrderState(id, exec.QueryRow(ctx, trdapi.OrderStateSQL, trdapi.OrderStateArgs(id)...).Scan)
//
// It applies no data scope: the caller's own record binds the order.
const (
	OrderStateFunction = "brant_trd.order_state_v1"
	OrderStateSQL      = `SELECT known, status, order_revision, order_hash, row_version FROM brant_trd.order_state_v1($1)`
)

// OrderStateArgs returns the arguments of OrderStateSQL for order orderID.
func OrderStateArgs(orderID string) []any { return []any{orderID} }

// OrderState is what TRD says of order OrderID now: Known is false for an order that does not
// exist in the caller's company; otherwise its status (SM-TRD-ORDER: OrderDraft, OrderActive,
// OrderCancelled, OrderClosedUnfulfilled or OrderClosed), its current order revision and hash and
// its row version.
type OrderState struct {
	OrderID       string
	Known         bool
	Status        string
	OrderRevision int64
	OrderHash     string
	RowVersion    int64
}

// ScanOrderState reads the row of OrderStateSQL for order orderID through scan, such as a pgx
// row's Scan.
func ScanOrderState(orderID string, scan func(dest ...any) error) (OrderState, error) {
	s := OrderState{OrderID: orderID}
	var status, hash *string
	var revision, version *int64
	if err := scan(&s.Known, &status, &revision, &hash, &version); err != nil {
		return OrderState{}, err
	}
	if status != nil {
		s.Status = *status
	}
	if hash != nil {
		s.OrderHash = *hash
	}
	if revision != nil {
		s.OrderRevision = *revision
	}
	if version != nil {
		s.RowVersion = *version
	}
	return s, nil
}

// ActiveFrom reports whether the order is active at order revision revision or a later one: the
// revision is in force, or a later revision that stands on what it held (an amendment that asked no
// more credit; S5 D-9).
func (s OrderState) ActiveFrom(revision int64) bool {
	return s.Known && s.Status == OrderActive && s.OrderRevision >= revision
}
