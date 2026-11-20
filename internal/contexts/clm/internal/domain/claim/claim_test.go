package claim

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// SM-CLM-CLAIM, CLM-INV-002 and CLM-INV-003. Python V0.50 has no claims context.

var (
	usd    = kernel.MustCurrency("USD", 2)
	opener = Act{By: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "merch.1"}, Epoch: 1, At: time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)}
	other  = Act{By: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "reviewer.1"}, Epoch: 2, At: opener.At.Add(time.Hour)}
	closer = Act{By: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "merch.2"}, Epoch: 3, At: opener.At.Add(2 * time.Hour)}
)

func qty(s string) kernel.Decimal { return kernel.MustDecimal(s) }

func money(minor int64) kernel.Money {
	m, err := kernel.NewMoney(usd, minor)
	if err != nil {
		panic(err)
	}
	return m
}

func outcome(err error) string {
	if err == nil {
		return "OK"
	}
	r, ok := kernel.AsRejection(err)
	if !ok {
		return err.Error()
	}
	return r.Code + " " + r.Fields[0].Path
}

func openCase(shipped string) *Case {
	return &Case{ID: "claim.1", Status: StatusOpen, Version: 1, ShipmentID: "shipment.1", LineID: "S1",
		OrderID: "order.1", CustomerID: "customer.1", Shipped: qty(shipped), UnitID: "PCS", Currency: usd,
		Opened: opener, OwnerActor: opener.By.ID, RequestHash: "open"}
}

func TestRegisterReturnStaysOpenAndCapsAtShipped(t *testing.T) {
	c := openCase("100")
	replay, err := c.Register("ret.1", qty("60"), "received", "", "h1", nil, opener, qty("0"))
	if err != nil || replay || c.Status != StatusOpen || c.Version != 2 || len(c.Returns) != 1 {
		t.Fatalf("60 of 100: %+v %s", c, outcome(err))
	}
	// The first return is already in counted when the second claim's line lock sums it.
	_, err = c.Register("ret.2", qty("40"), "received", "", "h2", nil, opener, qty("60"))
	if err != nil || c.Status != StatusOpen || c.Version != 3 {
		t.Fatalf("equal to shipped is allowed: %s %+v", outcome(err), c)
	}
	_, err = c.Register("ret.3", qty("0.000001"), "received", "", "h3", nil, opener, qty("100"))
	if outcome(err) != CodeClaimReturnExceedsShipped+" /args/quantity" || c.Version != 3 || len(c.Returns) != 2 {
		t.Fatalf("one more unit: %s version %d returns %d", outcome(err), c.Version, len(c.Returns))
	}
	// 99.999999 + 0.000001 is exactly 100, not a float.
	c2 := openCase("100")
	if _, err = c2.Register("ret.a", qty("0.000001"), "received", "", "ha", nil, opener, qty("99.999999")); err != nil {
		t.Fatal(outcome(err))
	}
	replay, err = c.Register("ret.1", qty("60"), "received", "", "h1", nil, opener, qty("100"))
	if err != nil || !replay || c.Version != 3 {
		t.Fatalf("same return replays: %v %s", replay, outcome(err))
	}
}

func TestRejectedClaimDoesNotCountAndCloseNeedsARemedy(t *testing.T) {
	c := openCase("100")
	if _, err := c.Decide("rem.1", KindReject, money(0), "无实物退货", "hd", other); err != nil {
		t.Fatal(outcome(err))
	}
	if c.Status != StatusOpen || c.Approved() || !c.Rejected() {
		t.Fatalf("reject stays open %+v", c.Remedy)
	}
	// Counted of other claims is already the whole shipment. This rejected claim's return is recorded
	// and does not consume the cap.
	if _, err := c.Register("ret.x", qty("50"), "received", "", "hx", nil, opener, qty("100")); err != nil || len(c.Returns) != 1 {
		t.Fatal(outcome(err))
	}
	if err := c.Close("结案", "hc", closer); err != nil || c.Status != StatusClosed || c.Version != 4 {
		t.Fatalf("close after reject: %s %+v", outcome(err), c)
	}
	bare := openCase("100")
	if outcome(bare.Close("结案", "hc", closer)) != kernel.CodeInvalidState+" /args/claim_id" {
		t.Fatal(outcome(bare.Close("结案", "hc2", closer)))
	}
	if _, err := c.Register("ret.y", qty("1"), "received", "", "hy", nil, opener, qty("0")); outcome(err) != kernel.CodeInvalidState+" /args/claim_id" {
		t.Fatal(outcome(err))
	}
}

func TestRemedyAndCloseAreIndependentOfTheOpener(t *testing.T) {
	c := openCase("100")
	if _, err := c.Decide("rem.1", KindCreditNote, money(10000), "色差折让", "hd", opener); outcome(err) != CodeIndependentReviewRequired+" /args/claim_id" {
		t.Fatal(outcome(err))
	}
	if c.Remedy != nil || c.Version != 1 {
		t.Fatal("a refused decision changes nothing")
	}
	replay, err := c.Decide("rem.1", KindCreditNote, money(10000), "色差折让", "hd", other)
	if err != nil || replay || !c.Approved() || c.Remedy.Amount.MinorString() != "10000" || c.Remedy.Amount.Currency().Code() != "USD" {
		t.Fatalf("credit %+v %s", c.Remedy, outcome(err))
	}
	if c.Status != StatusOpen {
		t.Fatal(c.Status)
	}
	if _, err = c.Decide("rem.2", KindRefund, money(1), "再次", "hd2", other); outcome(err) != kernel.CodeInvalidState+" /args/claim_id" {
		t.Fatal(outcome(err))
	}
	replay, err = c.Decide("rem.1", KindCreditNote, money(10000), "色差折让", "hd", other)
	if err != nil || !replay {
		t.Fatalf("same remedy replays: %v %s", replay, outcome(err))
	}
	if outcome(c.Close("结案", "hc", opener)) != CodeIndependentReviewRequired+" /args/claim_id" || c.Status != StatusOpen {
		t.Fatal(outcome(c.Close("x", "hx", opener)))
	}
	if err = c.Close("折让已决", "hc", closer); err != nil || c.Status != StatusClosed {
		t.Fatal(outcome(err))
	}
	eur, _ := kernel.NewMoney(kernel.MustCurrency("EUR", 2), 100)
	c2 := openCase("100")
	if _, err = c2.Decide("rem.e", KindRefund, eur, "币种", "he", other); outcome(err) != kernel.CodeInvalidKnownValue+" /args/amount_minor" {
		t.Fatal(outcome(err))
	}
	if _, err = c2.Decide("rem.z", KindRefund, money(0), "零", "hz", other); outcome(err) != kernel.CodeInvalidKnownValue+" /args/amount_minor" {
		t.Fatal(outcome(err))
	}
	if _, err = c2.Decide("rem.r", KindReshipment, money(1), "补发", "hr", other); outcome(err) != kernel.CodeInvalidKnownValue+" /args/amount_minor" {
		t.Fatal(outcome(err))
	}
	if _, err = c2.Decide("rem.r", KindRework, money(0), "返工引用", "hr", other); err != nil || c2.Approved() || c2.Status != StatusOpen {
		t.Fatalf("rework ref %+v %s", c2.Remedy, outcome(err))
	}
}

func TestAnswerOpenIsTheSameRequestOnly(t *testing.T) {
	c := openCase("100")
	if err := c.Answer("open", opener.By); err != nil {
		t.Fatal(err)
	}
	if outcome(c.Answer("other", opener.By)) != CodeBusinessKeyConflict+" /args/claim_id" {
		t.Fatal(outcome(c.Answer("other", opener.By)))
	}
}
