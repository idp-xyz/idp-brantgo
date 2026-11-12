package orderrev

import (
	"math/big"
	"regexp"
	"strconv"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The triggers of a payment stage (POL payment_terms; the payment plan of a revision copies them)
// and the release control a stage may carry (D-5, F-04).
const (
	TriggerOrderConfirmed = "order_confirmed"
	TriggerBeforeShipment = "before_shipment"
	TriggerAfterShipment  = "after_shipment"
	TriggerAfterInvoice   = "after_invoice"

	// ReleaseControlDocuments is a stage FUL's release gate secures: the shipping documents (an
	// original bill of lading or the telex release) stay with us until the stage is paid.
	ReleaseControlDocuments = "documents"
)

var minorAmount = regexp.MustCompile(`^(?:0|[1-9][0-9]{0,23})$`)

// CreditPortion is the part of a payment plan that falls due only after the goods left
// (TRD-INV-022, UL-INV-30): the sum, in integer minor units of the order currency, of the stages
// triggered after_shipment or after_invoice that carry no documents release control. Stages due on
// order_confirmed or before_shipment are paid before anything ships, and a documents-controlled
// stage is held by FUL's release gate (D-5), so neither needs STL's credit. 960.00 USD at 30 %
// order_confirmed and 70 % after_shipment is 67200 (TRD-INV-022's positive case: 672.00 is
// reserved first). The amounts are the plan's own — nothing is recalculated — and a stage amount
// that is not a minor amount is REFERENCE_INVALID at /payments/<i>/amount_minor (the plan is not
// one a calculation sealed).
func CreditPortion(payments []quote.Payment) (string, error) {
	sum := new(big.Int)
	for i, p := range payments {
		if !minorAmount.MatchString(p.AmountMinor) {
			return "", kernel.Reject(kernel.CodeReferenceInvalid, "/payments/"+strconv.Itoa(i)+"/amount_minor",
				"A payment stage's amount is an integer of minor units")
		}
		if !OnCredit(p) {
			continue
		}
		n, _ := new(big.Int).SetString(p.AmountMinor, 10)
		sum.Add(sum, n)
	}
	return sum.String(), nil
}

// OnCredit reports whether stage p is part of the credit portion: due after shipment or after the
// invoice, and not secured by the documents release control.
func OnCredit(p quote.Payment) bool {
	return AfterShipment(p) && p.ReleaseControl != ReleaseControlDocuments
}

// AfterShipment reports whether stage p falls due only after the goods left: triggered
// after_shipment or after_invoice, documents-controlled or not.
func AfterShipment(p quote.Payment) bool {
	return p.Trigger == TriggerAfterShipment || p.Trigger == TriggerAfterInvoice
}

// CreditNeed is what a payment plan asks of STL's CreditGuard v1 (TRD-INV-022, Context Map R57): how
// many of its stages fall due after shipment or after the invoice, their sum (PostShipmentMinor,
// documents-controlled stages included) and the credit portion among them (PortionMinor, the stages
// without documents control: CreditPortion), in integer minor units of the order currency.
//
// A plan without such a stage needs no reservation (付款全部在出运前到期). A plan with one is put to
// the guard with its credit portion, also when every such stage is documents-controlled and the
// portion is 0: whether a documents-controlled payment counts as secured is the credit policy's to
// say (D-5), so STL answers documents_secured, or covered when its reservation holds the stages.
type CreditNeed struct {
	Stages            int
	PostShipmentMinor string
	PortionMinor      string
}

// NeedOf returns what plan payments asks of the credit guard. A stage amount that is not an integer
// of minor units is REFERENCE_INVALID at /payments/<i>/amount_minor, as for CreditPortion.
func NeedOf(payments []quote.Payment) (CreditNeed, error) {
	portion, err := CreditPortion(payments)
	if err != nil {
		return CreditNeed{}, err
	}
	n := CreditNeed{PortionMinor: portion}
	post := new(big.Int)
	for _, p := range payments {
		if !AfterShipment(p) {
			continue
		}
		n.Stages++
		amount, _ := new(big.Int).SetString(p.AmountMinor, 10)
		post.Add(post, amount)
	}
	n.PostShipmentMinor = post.String()
	return n, nil
}

// Required reports whether the plan is put to the credit guard at all: it has a stage due after
// shipment or after the invoice.
func (n CreditNeed) Required() bool { return n.Stages > 0 }

// Grows reports whether a revision with need n asks more credit of STL than the revision it amends
// (from) did (S5 decision D-9, WF-06 信用部分增加时须新的占用): more falls due after shipment or
// invoice, or more of it is without documents control (a stage that lost its control adds to the
// credit portion while the post-shipment sum stays). The amended revision then needs a reservation
// of its own; one that asks the same or less stands on what the order holds.
func (n CreditNeed) Grows(from CreditNeed) bool {
	return n.Required() && (minor(n.PostShipmentMinor).Cmp(minor(from.PostShipmentMinor)) > 0 ||
		minor(n.PortionMinor).Cmp(minor(from.PortionMinor)) > 0)
}

// minor reads an amount NeedOf produced; the zero need (no amounts) reads as 0.
func minor(s string) *big.Int {
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return new(big.Int)
	}
	return n
}
