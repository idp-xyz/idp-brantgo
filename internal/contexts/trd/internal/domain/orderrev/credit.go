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
	return (p.Trigger == TriggerAfterShipment || p.Trigger == TriggerAfterInvoice) && p.ReleaseControl != ReleaseControlDocuments
}
