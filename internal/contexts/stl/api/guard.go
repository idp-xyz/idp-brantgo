package api

import (
	"fmt"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// CreditGuard v1 (Context Map R57 STL→TRD, Customer/Supplier with guardLock: 客户信用占用; risk 无占用或
// 占用不足仍生效赊销订单; TRD-INV-022) lets TRD re-check, inside its own transaction, that a live
// credit reservation covers an order revision's credit portion, and keeps it so until that
// transaction ends. STL publishes it as a SECURITY DEFINER function in its schema with a fixed
// search_path (migrations/stl/000001_stl_customer_credit.sql):
//
//	brant_stl.guard_credit_reservation_v1(p_order_id text, p_order_revision bigint, p_portion_minor numeric,
//	        p_currency text, p_at timestamptz)
//	    RETURNS TABLE (status text, reservation_id text, party_id text, order_hash text, reservation_status text,
//	                   currency text, reserved_minor numeric, secured_minor numeric, basis text, decision_id text,
//	                   expires_at timestamptz, code text)
//
// It always returns one row in the caller's scope (app.tenant_id and app.company_id) and locks the
// order revision's live (reserved or consumed) reservation row FOR SHARE in the caller's
// transaction. credit.release and credit.consume update that row (after locking the customer's
// credit root), so each either committed before the guard read it, and the verdict says so, or waits
// until the caller's transaction ends; a guard called while one of them holds the row waits and reads
// the row as it left it (a released row no longer matches: missing). The guard locks no other row.
//
// Its status is covered (the reservation's amount, in the order currency p_currency, is at least
// p_portion_minor), documents_secured (the order's post-shipment stages are all document-controlled
// and the credit policy treats them as secured, D-5: a reservation of amount 0 with that basis, and
// p_portion_minor 0), missing (no live reservation of the order revision), expired (reserved, and
// p_at is at or after its expiry; a consumed reservation does not expire) or insufficient (another
// currency, or less than the portion). Code is empty for covered and documents_secured and
// CREDIT_RESERVATION_REQUIRED otherwise.
//
// TRD computes the portion from the sealed calculation of the order revision (the stages due after
// shipment or invoice without document control: OrderCommercialSnapshot v1's CreditPortionMinor)
// and calls it at order.activate's STL extension point, after locking its own roots FOR UPDATE and
// in context code order (ADR-BR-011), with its own executor:
//
//	q := stlapi.CreditQuery{OrderID: id, OrderRevision: rev, OrderHash: hash, PortionMinor: "67200", Currency: "USD", At: now}
//	row := exec.QueryRow(ctx, stlapi.CreditGuardSQL, stlapi.CreditGuardArgs(q)...)
//	verdict, err := stlapi.ScanCreditGuard(q, row.Scan)
//	if err == nil {
//		err = verdict.Check("/args/order_id")
//	}
//
// and records verdict.ReservationID, ReservedMinor and Basis in its commit evidence. The guard does
// not apply the caller's data scope.
const (
	CreditGuardFunction = "brant_stl.guard_credit_reservation_v1"
	CreditGuardSQL      = `SELECT status, reservation_id, party_id, order_hash, reservation_status, currency, reserved_minor::text,
		secured_minor::text, basis, decision_id, expires_at, code
		FROM brant_stl.guard_credit_reservation_v1($1, $2, $3::text::numeric, $4, $5)`

	CreditCovered          = "covered"
	CreditDocumentsSecured = "documents_secured"
	CreditMissing          = "missing"
	CreditExpired          = "expired"
	CreditInsufficient     = "insufficient"
)

// CreditQuery asks whether order OrderID's revision OrderRevision has a live reservation covering
// PortionMinor (minor units of the order currency Currency) at instant At. OrderHash, when set, must
// be the hash the reservation bound (Check refuses another).
type CreditQuery struct {
	OrderID       string
	OrderRevision int64
	OrderHash     string
	PortionMinor  string
	Currency      string
	At            time.Time
}

// CreditGuardArgs returns the arguments of CreditGuardSQL for q.
func CreditGuardArgs(q CreditQuery) []any {
	return []any{q.OrderID, q.OrderRevision, q.PortionMinor, q.Currency, kernel.Instant(q.At)}
}

// CreditVerdict is the guard's answer for Query: its status and code, and the reservation it found
// (empty for missing): its ID, customer, order hash, state, currency, reserved and secured amounts
// in minor units, basis (within_limit, credit_decision or documents_secured), the decision it was
// reserved under and its expiry.
type CreditVerdict struct {
	Query             CreditQuery
	Status            string
	ReservationID     string
	PartyID           string
	OrderHash         string
	ReservationStatus string
	Currency          string
	ReservedMinor     string
	SecuredMinor      string
	Basis             string
	DecisionID        string
	ExpiresAt         time.Time
	Code              string
}

// ScanCreditGuard reads the row of CreditGuardSQL for q through scan, such as a pgx row's Scan.
func ScanCreditGuard(q CreditQuery, scan func(dest ...any) error) (CreditVerdict, error) {
	v := CreditVerdict{Query: q}
	var id, party, hash, state, currency, reserved, secured, basis, decision, code *string
	var expires *time.Time
	if err := scan(&v.Status, &id, &party, &hash, &state, &currency, &reserved, &secured, &basis, &decision, &expires, &code); err != nil {
		return CreditVerdict{}, err
	}
	for _, f := range []struct {
		from *string
		to   *string
	}{{id, &v.ReservationID}, {party, &v.PartyID}, {hash, &v.OrderHash}, {state, &v.ReservationStatus}, {currency, &v.Currency},
		{reserved, &v.ReservedMinor}, {secured, &v.SecuredMinor}, {basis, &v.Basis}, {decision, &v.DecisionID}, {code, &v.Code}} {
		if f.from != nil {
			*f.to = *f.from
		}
	}
	if expires != nil {
		v.ExpiresAt = kernel.Instant(*expires)
	}
	return v, nil
}

// Satisfied reports whether the order revision's credit portion is covered or secured by
// documents, for the order hash the query names (when it names one).
func (v CreditVerdict) Satisfied() bool {
	return v.Code == "" && (v.Status == CreditCovered || v.Status == CreditDocumentsSecured) &&
		(v.Query.OrderHash == "" || v.Query.OrderHash == v.OrderHash)
}

// Check returns nil when the verdict is satisfied and CREDIT_RESERVATION_REQUIRED (409) at path of
// the caller's request otherwise, saying why.
func (v CreditVerdict) Check(path string) error {
	if v.Satisfied() {
		return nil
	}
	reason := "The credit reservation is " + v.Status
	switch {
	case v.Code == "" && v.Query.OrderHash != v.OrderHash:
		reason = fmt.Sprintf("The credit reservation %s is for order hash %s", v.ReservationID, v.OrderHash)
	case v.Status == CreditInsufficient:
		reason = fmt.Sprintf("The credit reservation %s holds %s %s, the portion is %s %s", v.ReservationID, v.ReservedMinor, v.Currency,
			v.Query.PortionMinor, v.Query.Currency)
	}
	return Reject(CodeCreditReservationRequired, path, reason)
}
