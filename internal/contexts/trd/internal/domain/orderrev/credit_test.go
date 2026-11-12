package orderrev

import (
	"bytes"
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
)

// TRD-INV-022 and UL-INV-30 (S5 plan unit A2: CreditPortionMinor = Σ stages triggered after_shipment
// or after_invoice whose release control is not documents; D-5, F-04). Positive case: BRC-001 pays
// 30 % on order confirmation and 70 % 30 days after shipment — 672.00 of 960.00 is on credit and is
// reserved before activation. A plan paid before shipment has none; after_invoice counts; a stage
// whose documents FUL holds back is secured and does not count. A stage amount that is not an
// integer of minor units is no sealed plan.
func TestCreditPortionIsWhatFallsDueAfterTheGoodsLeft(t *testing.T) {
	r1 := revisionOne(t)
	if got, err := CreditPortion(r1.Amounts.Payments); err != nil || got != "67200" {
		t.Fatalf("BRC-001 30 %% order_confirmed + 70 %% after_shipment: %q %v", got, err)
	}
	if OnCredit(r1.Amounts.Payments[0]) || !OnCredit(r1.Amounts.Payments[1]) {
		t.Fatalf("stages on credit: %+v", r1.Amounts.Payments)
	}
	stage := func(trigger, control, amount string) quote.Payment {
		return quote.Payment{StageID: trigger + control, Percent: "25", Trigger: trigger, Days: 30, AmountMinor: amount, ReleaseControl: control}
	}
	for name, c := range map[string]struct {
		plan []quote.Payment
		want string
	}{
		"before shipment only": {[]quote.Payment{stage(TriggerOrderConfirmed, "", "40000"), stage(TriggerBeforeShipment, "", "56000")}, "0"},
		"after invoice counts": {[]quote.Payment{stage(TriggerBeforeShipment, "", "30000"), stage(TriggerAfterInvoice, "", "66000")}, "66000"},
		"documents secure a stage (D-5)": {[]quote.Payment{stage(TriggerOrderConfirmed, "", "28800"),
			stage(TriggerAfterShipment, ReleaseControlDocuments, "38400"), stage(TriggerAfterInvoice, "", "28800")}, "28800"},
		"a control before shipment changes nothing": {[]quote.Payment{stage(TriggerBeforeShipment, ReleaseControlDocuments, "96000")}, "0"},
		"beyond int64": {[]quote.Payment{stage(TriggerAfterShipment, "", "9223372036854775807"), stage(TriggerAfterInvoice, "", "1")},
			"9223372036854775808"},
	} {
		if got, err := CreditPortion(c.plan); err != nil || got != c.want {
			t.Errorf("%s: %q %v, want %q", name, got, err, c.want)
		}
	}
	for _, bad := range []string{"", "-1", "01", "1.00", "1000000000000000000000000"} {
		_, err := CreditPortion([]quote.Payment{stage(TriggerOrderConfirmed, "", "100"), stage(TriggerAfterShipment, "", bad)})
		if code(err) != "REFERENCE_INVALID at /payments/1/amount_minor" {
			t.Errorf("amount %q: %s", bad, code(err))
		}
	}
}

// TRD-INV-022 and S5 decision D-9 (S5 unit B4): what a plan asks of STL's CreditGuard. A plan paid
// before shipment asks nothing; one with a stage after shipment or invoice is put to the guard with
// its credit portion, 0 when documents control every such stage (STL answers documents_secured). An
// amended revision needs a reservation of its own when more falls due after shipment or more of it
// is uncontrolled than in the revision it amends — WF-06: 信用部分增加时须新的占用 — and none when
// it asks the same or less.
func TestCreditNeedOfAPlanAndItsGrowth(t *testing.T) {
	stage := func(trigger, control, amount string) quote.Payment {
		return quote.Payment{StageID: trigger + control, Percent: "25", Trigger: trigger, Days: 30, AmountMinor: amount, ReleaseControl: control}
	}
	need := func(plan ...quote.Payment) CreditNeed {
		t.Helper()
		n, err := NeedOf(plan)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	prepaid := need(stage(TriggerOrderConfirmed, "", "28800"), stage(TriggerBeforeShipment, "", "67200"))
	if prepaid.Required() || prepaid != (CreditNeed{PostShipmentMinor: "0", PortionMinor: "0"}) {
		t.Fatalf("paid before shipment: %+v", prepaid)
	}
	brc := need(revisionOne(t).Amounts.Payments...)
	if !brc.Required() || brc != (CreditNeed{Stages: 1, PostShipmentMinor: "67200", PortionMinor: "67200"}) {
		t.Fatalf("BRC-001 30/70 after shipment: %+v", brc)
	}
	secured := need(stage(TriggerOrderConfirmed, "", "28800"), stage(TriggerAfterShipment, ReleaseControlDocuments, "67200"))
	if !secured.Required() || secured != (CreditNeed{Stages: 1, PostShipmentMinor: "67200", PortionMinor: "0"}) {
		t.Fatalf("documents-controlled after shipment: %+v", secured)
	}
	mixed := need(stage(TriggerAfterShipment, ReleaseControlDocuments, "38400"), stage(TriggerAfterInvoice, "", "28800"))
	if mixed != (CreditNeed{Stages: 2, PostShipmentMinor: "67200", PortionMinor: "28800"}) {
		t.Fatalf("mixed: %+v", mixed)
	}
	less := need(stage(TriggerOrderConfirmed, "", "25920"), stage(TriggerAfterShipment, "", "60480"))
	more := need(stage(TriggerOrderConfirmed, "", "31680"), stage(TriggerAfterShipment, "", "73920"))
	for name, c := range map[string]struct {
		from, to CreditNeed
		grows    bool
	}{
		"credit appears":                           {prepaid, brc, true},
		"a secured stage appears":                  {prepaid, secured, true},
		"the same plan":                            {brc, brc, false},
		"a smaller order":                          {brc, less, false},
		"a larger order":                           {brc, more, true},
		"credit goes":                              {brc, prepaid, false},
		"a stage loses its documents control":      {secured, brc, true},
		"a stage gains documents control":          {brc, secured, false},
		"less is controlled, the same sum":         {secured, mixed, true},
		"nothing after shipment before or after":   {prepaid, prepaid, false},
		"the zero need (no plan read) to a credit": {CreditNeed{}, brc, true},
	} {
		if got := c.to.Grows(c.from); got != c.grows {
			t.Errorf("%s: grows %v, want %v", name, got, c.grows)
		}
	}
	if _, err := NeedOf([]quote.Payment{stage(TriggerAfterShipment, "", "1.5")}); code(err) != "REFERENCE_INVALID at /payments/0/amount_minor" {
		t.Fatalf("a malformed amount: %s", code(err))
	}
}

// D-5 (S5 plan unit A2: the stage's release control is read from the sealed plan, never from POL's
// current payment terms): a calculation that seals a documents control keeps it in the commercial
// document, revision 1 reads it from the snapshot, and an amendment that keeps the payment terms
// carries it into the next revision, which Parse reads back; an amendment to other payment terms
// takes the controls of their stages. A stage without a control has no release_control member, so
// every plan sealed before POL stated one hashes as it did (the fixture's snapshot and the golden
// revision of revision_test.go).
func TestTheReleaseControlIsSealedWithThePlan(t *testing.T) {
	plain := revisionOne(t)
	if bytes.Contains(plain.Document, []byte("release_control")) {
		t.Fatal("a plan without controls names release_control")
	}

	d := draft()
	inner := calculator(d)
	controlled := func() (*quote.Calculation, error) {
		c, err := inner()
		if err == nil {
			c.Payments[1].ReleaseControl = ReleaseControlDocuments // P2, 70 % after shipment, as POL will state it (D-5)
		}
		return c, err
	}
	s, err := quote.SealDraft("quote.1", 1, d, freezeFacts(), controlled, issuedAt)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(s.Commercial, []byte(`"release_control":"documents"`)) || s.BusinessHash == sealed(t).BusinessHash {
		t.Fatalf("the sealed commercial document %s", s.Commercial)
	}
	doc, hash := snapshot(t, s)
	r1, err := FromSnapshot(doc, hash)
	if err != nil {
		t.Fatal(err)
	}
	if p := r1.Amounts.Payments; p[0].ReleaseControl != "" || p[1].ReleaseControl != ReleaseControlDocuments {
		t.Fatalf("revision 1's plan %+v", p)
	}
	if got, _ := CreditPortion(r1.Amounts.Payments); got != "0" {
		t.Fatalf("documents-controlled after_shipment on credit: %s", got)
	}

	p := ProposalOf(r1)
	p.Lines[0].Quantity = "20" // L1 24 → 20: 928.00 USD
	r2, _, err := Next(r1, amendment1, p, Fulfilment{Source: NotWired}, facts(), "/args/proposal")
	if err != nil {
		t.Fatal(err)
	}
	if pay := r2.Amounts.Payments; len(pay) != 2 || pay[1].ReleaseControl != ReleaseControlDocuments || pay[1].AmountMinor != "64960" ||
		pay[0].ReleaseControl != "" {
		t.Fatalf("revision 2's plan %+v", pay)
	}
	back, err := Parse(r2.Document, r2.Hash)
	if err != nil || back.Amounts.Payments[1].ReleaseControl != ReleaseControlDocuments {
		t.Fatalf("Parse: %v %+v", err, back)
	}

	other := quote.Ref{Kind: quote.KindPaymentTerms, ID: "payment.40-60", Revision: 1}
	x := facts()
	x.Stages = []Stage{{StageID: "D", Percent: "40", Trigger: TriggerAfterShipment, Days: 30, ReleaseControl: ReleaseControlDocuments},
		{StageID: "I", Percent: "60", Trigger: TriggerAfterInvoice, Days: 60}}
	p = ProposalOf(r1)
	p.Terms.PaymentTerms = other
	r3, _, err := Next(r1, amendment1, p, Fulfilment{Source: NotWired}, x, "/args/proposal")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := CreditPortion(r3.Amounts.Payments); got != "57600" || r3.Amounts.Payments[0].ReleaseControl != ReleaseControlDocuments ||
		r3.Amounts.Payments[1].ReleaseControl != "" {
		t.Fatalf("revision on other terms: %s %+v", got, r3.Amounts.Payments)
	}
}
