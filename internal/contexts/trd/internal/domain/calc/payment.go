package calc

import (
	"fmt"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Stage is one stage of the POL payment_terms revision a quote names: its ID and its percentage of
// the payable on the 0–100 scale (a discount ratio is on 0–1; the scales never mix). Trigger, days
// and calendar stay with the caller, who keeps the stages' order.
type Stage struct {
	StageID string
	Percent kernel.Decimal
}

// Payment is one stage of a quote's payment plan (UL TRD-PaymentSchedule): the planned amount in
// the quote currency. It is a plan, not a receipt, a receivable or a bank credit.
type Payment struct {
	StageID string
	Percent kernel.Decimal
	Amount  kernel.Money
}

// AllocatePayments applies TRD-INV-008 to the payable (T03 §5.3): 1–20 stages with distinct IDs and
// positive percentages that sum to exactly 100. Each stage first receives the floor of its exact
// share in minor units; the units left go one each to the largest fractional remainders, ties to
// the smaller stage ID in ASCII order. The amounts sum to the payable and come back in the stages'
// order: 960.00 at 30/70 is 288.00 + 672.00, and 0.05 at 33.33/33.33/33.34 is 0.02, 0.01, 0.02. A
// payable so small that a stage would get 0 is refused, never patched with a fake unit or other
// percentages. Every failure is PAYMENT_TOTAL_MISMATCH at path + "/payment_terms_ref".
func AllocatePayments(path string, payable kernel.Money, stages []Stage) ([]Payment, error) {
	at := path + "/payment_terms_ref"
	mismatch := func(reason string) error { return fail(CodePaymentTotalMismatch, at, reason) }
	if len(stages) == 0 || len(stages) > MaxStages {
		return nil, mismatch(fmt.Sprintf("Payment terms have 1 to %d stages", MaxStages))
	}
	if payable.Sign() < 0 {
		return nil, fail(kernel.CodeInvalidKnownValue, path, "A payable is not negative")
	}
	sum := kernel.Decimal{}
	seen := make(map[string]bool, len(stages))
	shares := make([]kernel.Share, len(stages))
	for i, s := range stages {
		switch {
		case s.StageID == "" || seen[s.StageID]:
			return nil, mismatch("Each payment stage has an ID of its own")
		case !fits(s.Percent, PercentScale) || s.Percent.Sign() <= 0:
			return nil, mismatch(fmt.Sprintf("Stage %s has a positive percentage with at most %d fractional digits", s.StageID, PercentScale))
		}
		seen[s.StageID] = true
		sum = sum.Add(s.Percent)
		shares[i] = kernel.Share{ID: s.StageID, Weight: s.Percent}
	}
	if !sum.Equal(hundred) {
		return nil, mismatch(fmt.Sprintf("The stage percentages sum to %s, not 100", sum))
	}
	parts, err := payable.Allocate(shares)
	if err != nil {
		return nil, mismatch(err.Error())
	}
	out := make([]Payment, len(stages))
	for i, s := range stages {
		if parts[i].IsZero() {
			return nil, mismatch(fmt.Sprintf("The payable %s leaves stage %s at 0; a stage is never forced positive", payable, s.StageID))
		}
		out[i] = Payment{StageID: s.StageID, Percent: s.Percent, Amount: parts[i]}
	}
	return out, nil
}
