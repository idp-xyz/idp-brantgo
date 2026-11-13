package orderaccount

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func rejected(t *testing.T, err error, code, path string) {
	t.Helper()
	rej, ok := kernel.AsRejection(err)
	if !ok || rej.Code != code || (path != "" && (len(rej.Fields) == 0 || rej.Fields[0].Path != path)) {
		t.Fatalf("want %s at %s, got %v", code, path, err)
	}
}

// An order on 30 % on confirmation / 70 % before shipment, both paid in full at revision 1.
func paid(t *testing.T) (*Account, map[string]kernel.Money) {
	t.Helper()
	a, err := Open("order.1", 1, "h1", "customer.1", 1, []Stage{
		{StageID: "deposit", Trigger: TriggerOrderConfirmed, Percent: "30", Amount: money(t, "288.00")},
		{StageID: "balance", Trigger: TriggerBeforeShipment, Percent: "70", Amount: money(t, "672.00")}}, by, "")
	if err != nil {
		t.Fatal(err)
	}
	return a, map[string]kernel.Money{"deposit": money(t, "288.00"), "balance": money(t, "672.00")}
}

// SM-STL-PAYMENT-PLAN-REVIEW, WF-06, D-4 (BT10 payment-plan half): an amendment that reduces the order
// (960.00 → 800.00) keeps the stage IDs; the account moves to revision 2 with the new stages, the
// earlier applications stand on the stages of the same ID, and the balance now has 112.00 more
// applied than it asks — an item; the deposit fits and is no item. Nothing on credit changed.
func TestMoveAdoptsTheNewStagesAndListsWhatNoLongerFits(t *testing.T) {
	a, applied := paid(t)
	r, err := a.Move(2, "h2", 1, []Stage{
		{StageID: "deposit", Trigger: TriggerOrderConfirmed, Percent: "30", Amount: money(t, "240.00")},
		{StageID: "balance", Trigger: TriggerBeforeShipment, Percent: "70", Amount: money(t, "560.00")}},
		map[string]kernel.Money{"deposit": money(t, "240.00"), "balance": applied["balance"]}, "treasury", by)
	if err != nil {
		t.Fatal(err)
	}
	if a.OrderRevision != 2 || a.OrderHash != "h2" || a.Version != 2 || len(a.Stages) != 2 || a.Stages[1].Seq != 2 || a.Stage("balance").Amount.Amount() != "560.00" {
		t.Fatalf("account %+v", a)
	}
	if r.OrderRevision != 2 || r.FromRevision != 1 || r.FromHash != "h1" || r.Status != ReviewOpen || r.Version != 1 || r.ResponsibleRole != "treasury" ||
		len(r.Items) != 1 {
		t.Fatalf("review %+v", r)
	}
	it := r.Items[0]
	if it.Seq != 1 || it.Kind != ItemStageOverApplied || it.StageID != "balance" || it.From.Amount() != "672.00" || it.To.Amount() != "560.00" ||
		it.Applied.Amount() != "672.00" {
		t.Fatalf("item %+v", it)
	}
	// The payment condition of the new revision is judged on the new stages (STL-INV-003).
	if v := Condition(a, 2, ScopeShipment, map[string]kernel.Money{"deposit": money(t, "240.00"), "balance": money(t, "672.00")}, by.At); v.Status != Satisfied {
		t.Fatalf("condition after the move %+v", v)
	}
}

// WF-06 "信用部分增加时须新的占用": terms changed from 100 % before shipment to 30/70 with the balance
// 30 days after shipment. The stage "all" is gone with 960.00 standing on it; the new deposit and
// balance fit (nothing stands on them); the credit portion moved from 0 to 672.00. A stage ID that
// something still stands on although the earlier revision no longer had it is listed as gone too.
func TestMoveListsGoneStagesAndTheCreditPortion(t *testing.T) {
	a, err := Open("order.1", 1, "h1", "customer.1", 1, []Stage{{StageID: "all", Trigger: TriggerBeforeShipment, Percent: "100",
		Amount: money(t, "960.00")}}, by, "")
	if err != nil {
		t.Fatal(err)
	}
	r, err := a.Move(2, "h2", 1, []Stage{
		{StageID: "deposit", Trigger: TriggerOrderConfirmed, Percent: "30", Amount: money(t, "288.00")},
		{StageID: "balance", Trigger: TriggerAfterShipment, Days: 30, Percent: "70", Amount: money(t, "672.00")}},
		map[string]kernel.Money{"all": money(t, "960.00"), "older": money(t, "1.00")}, "treasury", by)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Items) != 3 {
		t.Fatalf("items %+v", r.Items)
	}
	gone, stray, credit := r.Items[0], r.Items[1], r.Items[2]
	if gone.Kind != ItemStageRemoved || gone.StageID != "all" || gone.From.Amount() != "960.00" || !gone.To.IsZero() || gone.Applied.Amount() != "960.00" {
		t.Fatalf("gone %+v", gone)
	}
	if stray.Kind != ItemStageRemoved || stray.StageID != "older" || !stray.From.IsZero() || stray.Applied.Amount() != "1.00" || stray.Seq != 2 {
		t.Fatalf("stray %+v", stray)
	}
	if credit.Kind != ItemCreditPortion || credit.StageID != "" || !credit.From.IsZero() || credit.To.Amount() != "672.00" || credit.Seq != 3 {
		t.Fatalf("credit %+v", credit)
	}
	// A stage under document control is not on credit (D-5).
	if p := CreditPortion(usd, []Stage{{Trigger: TriggerAfterShipment, Amount: money(t, "5.00"), ReleaseControl: ReleaseDocuments},
		{Trigger: TriggerAfterInvoice, Amount: money(t, "7.00")}, {Trigger: TriggerBeforeShipment, Amount: money(t, "11.00")}}); p.Amount() != "7.00" {
		t.Fatalf("portion %s", p)
	}
}

// The account never moves back or sideways, nor to another currency, nor to a plan STL does not know.
func TestMoveRefusesWhatIsNotALaterRevision(t *testing.T) {
	a, applied := paid(t)
	next := func() []Stage {
		return []Stage{{StageID: "all", Trigger: TriggerBeforeShipment, Percent: "100", Amount: money(t, "960.00")}}
	}
	_, err := a.Move(1, "h1", 1, next(), applied, "treasury", by)
	rejected(t, err, kernel.CodeInvalidState, "/args/order_revision")
	eur := kernel.MustCurrency("EUR", 2)
	m, _ := kernel.ParseAmount(eur, "960.00")
	_, err = a.Move(2, "h2", 1, []Stage{{StageID: "all", Trigger: TriggerBeforeShipment, Amount: m}}, applied, "treasury", by)
	rejected(t, err, kernel.CodeReferenceInvalid, "/args/order_id")
	_, err = a.Move(2, "h2", 1, nil, applied, "treasury", by)
	rejected(t, err, kernel.CodeReferenceInvalid, "/args/order_id")
	if a.OrderRevision != 1 || a.Version != 1 {
		t.Fatalf("refused moves changed the account %+v", a)
	}
	r, err := a.Move(2, "h2", 1, next(), applied, "treasury", by)
	if err != nil || len(r.Items) != 2 || r.Items[0].StageID != "deposit" || r.Items[1].StageID != "balance" {
		t.Fatalf("both stages gone: %+v %v", r, err)
	}
}

// SM-STL-PAYMENT-PLAN-REVIEW: open → settled by a person who disposes of every item once; an item
// disposed of as corrected must be corrected; open → superseded by a later revision; nothing moves
// a settled or superseded review again.
func TestSettleDisposesOfEveryItemOnce(t *testing.T) {
	a, applied := paid(t)
	r, err := a.Move(2, "h2", 1, []Stage{
		{StageID: "deposit", Trigger: TriggerOrderConfirmed, Percent: "30", Amount: money(t, "240.00")},
		{StageID: "balance", Trigger: TriggerBeforeShipment, Percent: "70", Amount: money(t, "560.00")}}, applied, "treasury", by)
	if err != nil || len(r.Items) != 2 {
		t.Fatalf("both stages over-applied: %+v %v", r, err)
	}
	settler := by
	settler.Reason = "Refund of the excess agreed with the customer"
	rejected(t, r.Settle(a, []Disposition{{Seq: 1, Disposition: DispositionAccepted, Note: "n"}}, applied, settler), kernel.CodeInvalidKnownValue,
		"/args/items")
	rejected(t, r.Settle(a, []Disposition{{Seq: 1, Disposition: DispositionAccepted, Note: "n"}, {Seq: 1, Disposition: DispositionAccepted, Note: "n"}},
		applied, settler), kernel.CodeInvalidKnownValue, "/args/items/1/seq")
	rejected(t, r.Settle(a, []Disposition{{Seq: 1, Disposition: DispositionAccepted, Note: "n"}, {Seq: 2, Disposition: DispositionCorrected, Note: "n"}},
		applied, settler), kernel.CodeInvalidState, "/args/items/1/disposition")
	if r.Status != ReviewOpen || a.Version != 2 {
		t.Fatalf("refused settles changed the review %+v %+v", r, a)
	}
	// The balance's excess was unapplied and 560.00 applied again: corrected now.
	now := map[string]kernel.Money{"deposit": applied["deposit"], "balance": money(t, "560.00")}
	if err = r.Settle(a, []Disposition{{Seq: 2, Disposition: DispositionCorrected, Note: "112.00 unapplied"}, {Seq: 1, Disposition: DispositionAccepted,
		Note: "48.00 kept as advance"}}, now, settler); err != nil {
		t.Fatal(err)
	}
	if r.Status != ReviewSettled || r.Version != 2 || r.Settled.Reason != settler.Reason || a.Version != 3 ||
		r.Items[0].Disposition != DispositionAccepted || r.Items[1].Note != "112.00 unapplied" {
		t.Fatalf("settled %+v %+v", r, a)
	}
	rejected(t, r.Settle(a, nil, now, settler), kernel.CodeInvalidState, "/args/order_revision")
	rejected(t, r.Supersede(3, by.At), kernel.CodeInvalidState, "")

	b, applied := paid(t)
	first, _ := b.Move(2, "h2", 1, []Stage{{StageID: "all", Trigger: TriggerBeforeShipment, Amount: money(t, "960.00")}}, applied, "treasury", by)
	if err = first.Supersede(2, by.At); err == nil {
		t.Fatal("superseded by its own revision")
	}
	if err = first.Supersede(3, by.At); err != nil || first.Status != ReviewSuperseded || first.Version != 2 || first.SupersededBy != 3 {
		t.Fatalf("superseded %+v %v", first, err)
	}
	rejected(t, first.Settle(b, nil, applied, settler), kernel.CodeInvalidState, "")
	if begun := b.Begin("treasury", by); begun.FromRevision != 0 || begun.OrderRevision != 2 || len(begun.Items) != 0 || begun.Status != ReviewOpen {
		t.Fatalf("begun %+v", begun)
	}
}
