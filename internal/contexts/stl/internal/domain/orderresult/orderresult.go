// Package orderresult holds STL's OrderResult rules (tactical model STL-AGG-OrderResult with its entity
// ResultSnapshot and value objects ForecastResult, ExecutingResult, ConfirmedResult, FxSettlementDifference;
// SM-STL-ORDER-RESULT; STL-INV-011; Context Map MD-49; UL STL-OrderResult; baseline 4.5, 4.6 S-A, 4.13; BT18, BT24;
// WBS S5-17).
//
// An order's operating result is explained from its sources (figures.go): the forecast (the active order's net
// sales less the expected purchase cost and the expected direct charges), the executing view (net sales less the
// confirmed costs and an estimate of the rest), the confirmed result (confirmed sales settlements less effective
// credit notes, less the confirmed cost of the goods sold and the confirmed direct charges, per shipment batch),
// the business settlement FX difference (each cross-currency receipt against the order's analysis rate) and the
// net cash (receipts less payments) — each in the order's analysis currency, as of when it was derived, with the
// sources it counts and what it could not count. None of it is legal profit, revenue recognition or FX accounting
// (O2C-16/17).
//
// The result is reviewed (open → reviewed, human or AI) and closed (reviewed → settled, human only) when every
// close condition holds (STL-INV-011): fulfilment complete or short-closed, receivables and payables settled,
// purchase costs and direct charges confirmed, no open return or claim — CLM is not enabled in S5, so that
// dimension is recorded not_enabled and never as met; otherwise ORDER_RESULT_NOT_SETTLED names every unmet
// dimension. Closing appends an immutable snapshot (MD-49); a late charge or adjustment reopens the result with a
// reason (settled → open), and the next close appends the next snapshot while the earlier ones stay (BT24).
package orderresult

import (
	"strings"
	"time"
	"unicode/utf8"

	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/fundrecord"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// States of an order result (SM-STL-ORDER-RESULT).
const (
	StatusOpen     = "open"
	StatusReviewed = "reviewed"
	StatusSettled  = "settled"
)

// CodeOrderResultNotSettled: a close condition does not hold (STL-INV-011; error catalog ORDER_RESULT_NOT_SETTLED,
// 409 conflict).
const CodeOrderResultNotSettled = "ORDER_RESULT_NOT_SETTLED"

// Codes are the codes the order result rules give besides the kernel's, with their categories.
var Codes = map[string]kernel.Category{CodeOrderResultNotSettled: kernel.CategoryConflict}

// The dimensions of the close conditions, in the order STL-INV-011 names them, and their states.
const (
	DimFulfilment    = "fulfilment"
	DimReceivables   = "receivables"
	DimPayables      = "payables"
	DimPurchaseCosts = "purchase_costs"
	DimDirectCharges = "direct_charges"
	DimClaims        = "claims"

	Met        = "met"
	Unmet      = "unmet"
	NotEnabled = "not_enabled"
)

// Dimension is one close condition as it stands: met, unmet (Items say what is open) or not_enabled (the
// context that holds the fact is not in this phase; never a pass).
type Dimension struct {
	Name   string
	Status string
	Items  []string
}

// Result is the root of STL-AGG-OrderResult (identity: the order).
type Result struct {
	OrderID    string
	Status     string
	Version    int64
	OwnerActor string
	OwnerTeam  string
	CreatedAt  time.Time
	Reviews    []Review
	Snapshots  []Snapshot
	Reopenings []Reopening
}

// Review is one order_result.review: the order revision it saw and the summary (figures and conditions, JSON)
// with its SHA-256.
type Review struct {
	Seq           int
	OrderRevision int64
	Summary       []byte
	SummarySHA256 string
	Note          string
	By            fundrecord.Act
}

// Snapshot is a closing snapshot (entity ResultSnapshot): what order_result.close settled — the order binding,
// the result currency, net sales and confirmed result, the full content (figures and conditions, JSON) and its
// SHA-256. It never changes.
type Snapshot struct {
	Seq           int
	OrderRevision int64
	OrderHash     string
	Currency      kernel.Currency
	NetSales      kernel.Money
	Result        kernel.Money
	Content       []byte
	ContentSHA256 string
	Reason        string
	By            fundrecord.Act
}

// Reopening is one order_result.reopen of snapshot Seq, with its reason and evidence.
type Reopening struct {
	Seq         int
	Reason      string
	EvidenceIDs []string
	By          fundrecord.Act
}

// New is the lock anchor of an order without a result yet: open at version 0.
func New(orderID, owner, team string, at time.Time) *Result {
	return &Result{OrderID: orderID, Status: StatusOpen, OwnerActor: owner, OwnerTeam: team, CreatedAt: at}
}

func state(reason string) error {
	return kernel.Reject(kernel.CodeInvalidState, "/args/order_id", reason)
}

// Review records a review of an open result (open → reviewed).
func (r *Result) Review(rv Review) error {
	if r.Status != StatusOpen {
		return state("The order result is " + r.Status + ": only an open result is reviewed")
	}
	rv.Seq = len(r.Reviews) + 1
	r.Reviews = append(r.Reviews, rv)
	r.Status, r.Version = StatusReviewed, r.Version+1
	return nil
}

// NotSettled returns ORDER_RESULT_NOT_SETTLED with one field per unmet dimension (path /conditions/<dimension>, the
// open items as its reason), or nil when none is unmet. A not_enabled dimension is recorded, not judged.
func NotSettled(dims []Dimension) error {
	var fields []kernel.FieldError
	for _, d := range dims {
		if d.Status == Unmet {
			fields = append(fields, kernel.FieldError{Path: "/conditions/" + d.Name, Reason: clip(d.Name + ": " + strings.Join(d.Items, "; "))})
		}
	}
	if len(fields) == 0 {
		return nil
	}
	return &kernel.Rejection{Code: CodeOrderResultNotSettled, Category: Codes[CodeOrderResultNotSettled], Fields: fields}
}

// clip keeps a reason within the 200 characters the response contract allows.
func clip(s string) string {
	if utf8.RuneCountInString(s) <= 200 {
		return s
	}
	r := []rune(s)
	return string(r[:197]) + "..."
}

// Close settles a reviewed result when every close condition holds (STL-INV-011): it appends snapshot s.
func (r *Result) Close(dims []Dimension, s Snapshot) error {
	if r.Status != StatusReviewed {
		return state("The order result is " + r.Status + ": it is closed after a review")
	}
	if err := NotSettled(dims); err != nil {
		return err
	}
	s.Seq = len(r.Snapshots) + 1
	r.Snapshots = append(r.Snapshots, s)
	r.Status, r.Version = StatusSettled, r.Version+1
	return nil
}

// Reopen opens a settled result again for a late charge or adjustment (BT24): the snapshot stays, the reopening
// records why.
func (r *Result) Reopen(re Reopening) error {
	if r.Status != StatusSettled {
		return state("The order result is " + r.Status + ": only a settled result is reopened")
	}
	re.Seq = len(r.Snapshots)
	r.Reopenings = append(r.Reopenings, re)
	r.Status, r.Version = StatusOpen, r.Version+1
	return nil
}

// Latest returns the newest closing snapshot, or nil.
func (r *Result) Latest() *Snapshot {
	if len(r.Snapshots) == 0 {
		return nil
	}
	return &r.Snapshots[len(r.Snapshots)-1]
}
