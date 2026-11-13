package settlement

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Opening balances (S5 unit C10, WBS S5-20; STL-INV-012; MD-50; baseline 11.2 期初、防重、切换; BT17, CN-AT16 STL
// half). Python V0.50 has no STL.

var cutoff = time.Date(2026, 10, 31, 16, 0, 0, 0, time.UTC) // 2026-11-01 00:00 China time

func openingIn(t *testing.T, kind, document, source, amount string, installments ...Installment) OpeningInput {
	return OpeningInput{ID: "ob." + document, Kind: kind, CutoffAt: cutoff, SourceDocument: document, SourceIssuedAt: cutoff.AddDate(0, -2, 0),
		SourceAmount: money(t, source), Amount: money(t, amount), CurrencyRevision: 1, CounterpartyID: "customer.1", OrderReference: "SO-LEGACY-42",
		Installments: installments, EvidenceIDs: []string{"ar.ledger"}, By: treasury, Team: "finance"}
}

func owing(t *testing.T, amount string, at time.Time) Installment {
	return Installment{Amount: money(t, amount), DueAt: at}
}

// STL-INV-012 positive (截止日导入未结应收 7,000; SC-09's "未结 7,000"): invoice AR-2026-0107 of 10,000.00 is open
// 7,000.00 at the cutoff — 3,000.00 was settled before it — due 4,000.00 on 15 November and 3,000.00 on 15
// December. The opening statement is a sales statement marked opening with no line and no order, net 7,000.00 and
// no tax, its two installments without a stage.
func TestAnOpeningReceivableIsAStatementWithoutSourceLines(t *testing.T) {
	d1, d2 := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 15, 0, 0, 0, 0, time.UTC)
	o, st, err := RecordOpening(openingIn(t, OpeningReceivable, "AR-2026-0107", "10000.00", "7000.00", owing(t, "4000.00", d1),
		owing(t, "3000.00", d2)))
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != OpeningRecorded || o.StatementID != o.ID || o.FundRecordID != "" || o.SettledBefore().String() != "3000.00 USD" ||
		o.OwnerActor != "treasury.1" || o.OwnerTeam != "finance" || !o.CutoffAt.Equal(cutoff) || o.OrderReference != "SO-LEGACY-42" {
		t.Fatalf("item %+v", o)
	}
	if st.ID != o.ID || st.Kind != KindSales || !st.Opening || st.Status != StatusConfirmed || st.Version != 1 || st.OrderID != "" ||
		len(st.Lines) != 0 || st.Net.String() != "7000.00 USD" || st.Tax.Sign() != 0 || st.Amount.String() != "7000.00 USD" ||
		st.CounterpartyID != "customer.1" || !st.ShippedAt.IsZero() {
		t.Fatalf("statement %+v", st)
	}
	if len(st.Installments) != 2 || st.Installments[0].Seq != 1 || st.Installments[0].StageID != "" || st.Installments[0].OrderID != "" ||
		!st.Installments[0].DueAt.Equal(d1) || st.Installments[1].Amount.String() != "3000.00 USD" || !st.Installments[1].DueAt.Equal(d2) {
		t.Fatalf("installments %+v", st.Installments)
	}
	if got := st.Open(map[int]kernel.Money{1: money(t, "4000.00")}); got.String() != "3000.00 USD" {
		t.Fatalf("open after the first installment is paid: %s", got)
	}
	// A payable is an opening purchase statement.
	_, ps, err := RecordOpening(openingIn(t, OpeningPayable, "AP-SUP1-88", "600.00", "600.00", owing(t, "600.00", d1)))
	if err != nil || ps.Kind != KindPurchase || !ps.Opening || ps.Amount.String() != "600.00 USD" {
		t.Fatalf("%+v %v", ps, err)
	}
}

// An unapplied prepayment is an opening fund record: no statement, the item names the fund record of its ID; its
// money was received or paid before the cutoff.
func TestAnOpeningPrepaymentIsAFundRecord(t *testing.T) {
	in := openingIn(t, OpeningCustomerPrepayment, "RCV-2026-0311", "5000.00", "2000.00")
	in.ValueDate = cutoff.AddDate(0, 0, -20)
	o, st, err := RecordOpening(in)
	if err != nil || st != nil || o.FundRecordID != o.ID || o.StatementID != "" || !o.Prepayment() || o.SettledBefore().String() != "3000.00 USD" {
		t.Fatalf("%+v %+v %v", o, st, err)
	}
	in.ValueDate = cutoff.Add(time.Second)
	_, _, err = RecordOpening(in)
	want(t, err, kernel.CodeInvalidKnownValue, "/args/value_date")
}

// STL-INV-012 negative (重复导入返回 OPENING_BALANCE_DUPLICATE) and BT17 (上线后不重建同一账款): a source document that
// stands as an opening item of the kind — recorded at this cutoff or another — is refused before anything else
// is judged, so importing the same batch twice creates nothing.
func TestASourceDocumentStandsOnce(t *testing.T) {
	first, _, err := RecordOpening(openingIn(t, OpeningReceivable, "AR-2026-0107", "10000.00", "7000.00", owing(t, "7000.00", cutoff)))
	if err != nil {
		t.Fatal(err)
	}
	again := openingIn(t, OpeningReceivable, "AR-2026-0107", "10000.00", "7000.00", owing(t, "7000.00", cutoff))
	again.Standing = first
	_, _, err = RecordOpening(again)
	want(t, err, CodeOpeningBalanceDuplicate, "/args/source/document")
	later := again
	later.CutoffAt, later.Amount = cutoff.AddDate(0, 1, 0), money(t, "1.00") // another cutoff, other values: still the same document
	_, _, err = RecordOpening(later)
	want(t, err, CodeOpeningBalanceDuplicate, "/args/source/document")
	if Codes[CodeOpeningBalanceDuplicate] != kernel.CategoryConflict {
		t.Fatal("OPENING_BALANCE_DUPLICATE is a conflict (409)")
	}
	// Once its statement is reversed the item no longer stands: the document may be recorded again.
	if err = first.ReverseOpening(treasury.At); err != nil || first.Status != OpeningReversed {
		t.Fatalf("%+v %v", first, err)
	}
	want(t, first.ReverseOpening(treasury.At), kernel.CodeInvalidState, "/args/statement_id")
}

// The cutoff, the source document and the amounts reconcile (CN-AT16 截止、来源余额可核对): a cutoff after now, a source
// issued after the cutoff, nothing open, more open than the document amounts to, or installments that do not add up
// are INVALID_KNOWN_VALUE.
func TestTheCutoffAndTheSourceReconcile(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(*OpeningInput)
		path string
	}{
		{"cutoff after now", func(in *OpeningInput) { in.CutoffAt = treasury.At.Add(time.Minute) }, "/args/cutoff_at"},
		{"issued after the cutoff", func(in *OpeningInput) { in.SourceIssuedAt = cutoff.Add(time.Second) }, "/args/source/issued_at"},
		{"nothing open", func(in *OpeningInput) {
			in.Amount, in.Installments = money(t, "0.00"), nil
		}, "/args/amount"},
		{"more open than the document", func(in *OpeningInput) { in.SourceAmount = money(t, "6999.99") }, "/args/source/amount"},
		{"installments short", func(in *OpeningInput) { in.Installments = []Installment{owing(t, "6999.99", cutoff)} }, "/args/installments"},
		{"an empty installment", func(in *OpeningInput) {
			in.Installments = []Installment{owing(t, "7000.00", cutoff), owing(t, "0.00", cutoff)}
		}, "/args/installments/1/amount"},
	} {
		in := openingIn(t, OpeningReceivable, "AR-1", "10000.00", "7000.00", owing(t, "7000.00", cutoff))
		c.edit(&in)
		_, _, err := RecordOpening(in)
		if err == nil {
			t.Fatalf("%s: accepted", c.name)
		}
		want(t, err, kernel.CodeInvalidKnownValue, c.path)
	}
}
