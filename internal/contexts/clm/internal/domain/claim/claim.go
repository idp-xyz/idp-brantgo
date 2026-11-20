// Package claim is the ClaimCase aggregate (tactical model CLM-AGG-ClaimCase, SM-CLM-CLAIM,
// CLM-INV-001..003; S6-D03). ReturnRecord and Remedy are entities of the one root. The root's
// state is only open or closed: claim.open creates it open, return.register and remedy.decide
// leave it open, and claim.close is the only way to closed.
//
// A claim is anchored to one confirmed shipment line. Returns never change that line and never
// write stock. The cumulative quantity of returns on claims that are not rejected is at most the
// shipped quantity (CLAIM_RETURN_EXCEEDS_SHIPPED). remedy.decide and claim.close are refused to
// the actor who opened the claim (INDEPENDENT_REVIEW_REQUIRED). A remedy does not post a credit
// note or a refund.
package claim

import (
	"slices"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// States of a claim (SM-CLM-CLAIM).
const (
	StatusOpen   = "open"
	StatusClosed = "closed"
)

// Remedy kinds. credit_note and refund carry a positive amount in the shipment currency. reject,
// reshipment_ref and rework_ref carry none: a reshipment or a rework is a reference, not a new
// shipment and not a stock movement (S6-D03).
const (
	KindCreditNote = "credit_note"
	KindRefund     = "refund"
	KindReject     = "reject"
	KindReshipment = "reshipment_ref"
	KindRework     = "rework_ref"
)

const (
	// CodeClaimReturnExceedsShipped: the return would take the counted quantity of the shipment
	// line above what that line confirmed (CLM-INV-002).
	CodeClaimReturnExceedsShipped = "CLAIM_RETURN_EXCEEDS_SHIPPED"
	// CodeIndependentReviewRequired: the actor who opened the claim does not decide its remedy
	// and does not close it (CLM-INV-003).
	CodeIndependentReviewRequired = "INDEPENDENT_REVIEW_REQUIRED"
	// CodeBusinessKeyConflict: the claim, return or remedy ID holds another request.
	CodeBusinessKeyConflict = "BUSINESS_KEY_CONFLICT"
)

// Codes are the codes of this package and their categories (error catalog).
var Codes = map[string]kernel.Category{
	CodeClaimReturnExceedsShipped: kernel.CategoryInvalid,
	CodeIndependentReviewRequired: kernel.CategoryInvalid,
	CodeBusinessKeyConflict:       kernel.CategoryConflict,
}

func fail(code, path, reason string) error {
	if c, ok := Codes[code]; ok {
		return kernel.RejectAs(code, c, path, reason)
	}
	return kernel.Reject(code, path, reason)
}

// Act is who did one thing, and the authority epoch they held.
type Act struct {
	By    kernel.ActorRef
	Epoch int64
	At    time.Time
	Team  string
}

// ReturnRecord is one registered return of the claim, in the shipment line's unit.
type ReturnRecord struct {
	ID          string
	Quantity    kernel.Decimal
	Condition   string
	Note        string
	Evidence    []string
	RequestHash string
	By          Act
}

// Remedy is the one remedy of the claim. Amount is zero of the shipment currency for a kind that
// moves no money.
type Remedy struct {
	ID          string
	Kind        string
	Amount      kernel.Money
	Reason      string
	RequestHash string
	By          Act
}

// Closure is how the claim was closed.
type Closure struct {
	Summary     string
	RequestHash string
	By          Act
}

// Case is one claim. Shipped is the confirmed quantity of its shipment line. Currency is that
// line's currency.
type Case struct {
	ID               string
	Status           string
	Version          int64
	ShipmentID       string
	LineID           string
	ShipmentVersion  int64
	OrderID          string
	OrderRevision    int64
	OrderHash        string
	CustomerID       string
	OrderLineID      string
	SkuID            string
	SpecRevision     int64
	UnitID           string
	UnitRevision     int64
	Shipped          kernel.Decimal
	Currency         kernel.Currency
	CurrencyRevision int64
	Reason           string
	Evidence         []string
	RequestHash      string
	OwnerActor       string
	OwnerTeam        string
	Opened           Act
	Returns          []ReturnRecord
	Remedy           *Remedy
	Closure          *Closure
}

// MoneyKind reports whether kind carries a positive amount.
func MoneyKind(kind string) bool {
	return kind == KindCreditNote || kind == KindRefund
}

// KnownKind reports whether kind is one of the five remedy kinds.
func KnownKind(kind string) bool {
	switch kind {
	case KindCreditNote, KindRefund, KindReject, KindReshipment, KindRework:
		return true
	default:
		return false
	}
}

// Rejected reports whether the claim's remedy is a rejection. A claim with no remedy is not rejected.
func (c *Case) Rejected() bool {
	return c.Remedy != nil && c.Remedy.Kind == KindReject
}

// Approved reports whether the remedy is an approved credit note or refund: a positive amount of
// one of those kinds. Closing the claim is not required.
func (c *Case) Approved() bool {
	return c.Remedy != nil && MoneyKind(c.Remedy.Kind) && c.Remedy.Amount.Sign() > 0
}

// Answer reports whether an existing claim answers the same claim.open. The same request by the
// same opener is a replay; anything else is BUSINESS_KEY_CONFLICT.
func (c *Case) Answer(requestHash string, by kernel.ActorRef) error {
	if c.RequestHash != requestHash || c.Opened.By != by {
		return fail(CodeBusinessKeyConflict, "/args/claim_id", "The claim ID holds another claim")
	}
	return nil
}

// Register adds a return. counted is the quantity already registered on every non-rejected claim
// of the same shipment line, this claim included, not counting qty. A rejected claim's new return
// is recorded and does not count (CLM-INV-002). replay is true when returnID already holds this
// same request.
func (c *Case) Register(returnID string, qty kernel.Decimal, condition, note, requestHash string, evidence []string, by Act, counted kernel.Decimal) (replay bool, err error) {
	if c.Status != StatusOpen {
		return false, fail(kernel.CodeInvalidState, "/args/claim_id", "The claim is "+c.Status)
	}
	for _, r := range c.Returns {
		if r.ID != returnID {
			continue
		}
		if r.RequestHash == requestHash && r.By.By == by.By {
			return true, nil
		}
		return false, fail(CodeBusinessKeyConflict, "/args/return_id", "The return ID holds another return")
	}
	if qty.Sign() <= 0 {
		return false, fail(kernel.CodeSchemaInvalid, "/args/quantity", "")
	}
	next := counted
	if !c.Rejected() {
		next = counted.Add(qty)
	}
	if next.Cmp(c.Shipped) > 0 {
		return false, fail(CodeClaimReturnExceedsShipped, "/args/quantity",
			"Registered returns would exceed the confirmed shipped quantity")
	}
	c.Returns = append(c.Returns, ReturnRecord{ID: returnID, Quantity: qty, Condition: condition, Note: note,
		Evidence: slices.Clone(evidence), RequestHash: requestHash, By: by})
	c.Version++
	return false, nil
}

// Decide records the claim's one remedy. The same request by the same decider is a replay. The
// opener is refused (INDEPENDENT_REVIEW_REQUIRED) before a second, different remedy is considered
// only when none is stored yet; a stored remedy is answered from what was stored.
func (c *Case) Decide(remedyID, kind string, amount kernel.Money, reason, requestHash string, by Act) (replay bool, err error) {
	if c.Status != StatusOpen {
		return false, fail(kernel.CodeInvalidState, "/args/claim_id", "The claim is "+c.Status)
	}
	if c.Remedy != nil {
		if c.Remedy.ID == remedyID && c.Remedy.RequestHash == requestHash && c.Remedy.By.By == by.By {
			return true, nil
		}
		if c.Remedy.ID == remedyID {
			return false, fail(CodeBusinessKeyConflict, "/args/remedy_id", "The remedy ID holds another remedy")
		}
		return false, fail(kernel.CodeInvalidState, "/args/claim_id", "The claim already has a remedy")
	}
	if by.By.ID == c.Opened.By.ID {
		return false, fail(CodeIndependentReviewRequired, "/args/claim_id", "The actor who opened the claim does not decide its remedy")
	}
	if !KnownKind(kind) {
		return false, fail(kernel.CodeInvalidKnownValue, "/args/kind", "The remedy is a credit_note, a refund, a reject, a reshipment_ref or a rework_ref")
	}
	if amount.Currency() != c.Currency {
		return false, fail(kernel.CodeInvalidKnownValue, "/args/amount_minor", "The amount is in the shipment currency")
	}
	if MoneyKind(kind) && amount.Sign() <= 0 {
		return false, fail(kernel.CodeInvalidKnownValue, "/args/amount_minor", "The amount is above zero")
	}
	if !MoneyKind(kind) && amount.Sign() != 0 {
		return false, fail(kernel.CodeInvalidKnownValue, "/args/amount_minor", "This remedy records no amount")
	}
	c.Remedy = &Remedy{ID: remedyID, Kind: kind, Amount: amount, Reason: reason, RequestHash: requestHash, By: by}
	c.Version++
	return false, nil
}

// Close closes an open claim that has a remedy. The opener is refused.
func (c *Case) Close(summary, requestHash string, by Act) error {
	switch {
	case c.Status != StatusOpen:
		return fail(kernel.CodeInvalidState, "/args/claim_id", "The claim is "+c.Status)
	case c.Remedy == nil:
		return fail(kernel.CodeInvalidState, "/args/claim_id", "A claim closes after its remedy is decided")
	case by.By.ID == c.Opened.By.ID:
		return fail(CodeIndependentReviewRequired, "/args/claim_id", "The actor who opened the claim does not close it")
	}
	c.Status = StatusClosed
	c.Version++
	c.Closure = &Closure{Summary: summary, RequestHash: requestHash, By: by}
	return nil
}
