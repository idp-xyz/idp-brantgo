package charge

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/fundrecord"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Unit tests of STL-INV-010 and SM-STL-CHARGE (baseline 4.5, 4.6 S-A step 5). Python V0.50 has no STL.

var (
	cny = mustCurrency("CNY")
	usd = mustCurrency("USD")
	at  = time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	by  = fundrecord.Act{By: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "officer.1"}, Epoch: 1, At: at}
)

func mustCurrency(code string) kernel.Currency {
	c, err := kernel.NewCurrency(code, 2)
	if err != nil {
		panic(err)
	}
	return c
}

func money(c kernel.Currency, minor int64) kernel.Money {
	m, err := kernel.NewMoney(c, minor)
	if err != nil {
		panic(err)
	}
	return m
}

func ship(order, id string) Target {
	return Target{Kind: TargetShipment, OrderID: order, ShipmentID: id}
}

func record(t *testing.T, status string, amount kernel.Money, beneficiaries ...Target) *Charge {
	t.Helper()
	c, err := Record(RecordInput{ID: "charge.1", Status: status, Source: Source{Kind: SourceLogisticsOffer, PartyID: "forwarder.1", Reference: "OFFER-1"},
		FeeCategory: ConfigUse{ID: "freight", Revision: 2}, CurrencyRevision: 2, Amount: amount, Conversion: Conversion{Kind: ConversionSameCurrency},
		OccurredOn: at, AnalysisOn: at, Basis: "合成：海运费", Beneficiaries: beneficiaries, By: by})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func allowed(targets ...Target) map[string]bool {
	out := map[string]bool{}
	for _, t := range targets {
		out[t.Key()] = true
	}
	return out
}

func want(t *testing.T, name string, err error, code, path string) {
	t.Helper()
	rej, ok := kernel.AsRejection(err)
	if !ok || rej.Code != code || rej.Fields[0].Path != path {
		t.Fatalf("%s: want %s at %s, got %v", name, code, path, err)
	}
}

// S-A step 5 and STL-INV-010's positive example: the actual freight of CNY 650 is allocated by quantity 60 / 40 to
// the two shipments: 390 + 260. The estimate of CNY 500 stays the forecast's amount; the bill of 650 is the
// charge's amount; confirming changes nothing else. SM-STL-CHARGE: estimated → pending (bill) → confirmed.
func TestSAFreightIsAllocatedByQuantity(t *testing.T) {
	s60, s40 := ship("order.sa", "shipment.60"), ship("order.sa", "shipment.40")
	c := record(t, StatusEstimated, money(cny, 50000), s60, s40)
	if c.Status != StatusEstimated || c.Version != 1 || c.Estimate == nil || c.Estimate.MinorString() != "50000" {
		t.Fatalf("estimate %+v", c)
	}
	err := c.Bill(BillInput{Source: Source{Kind: SourceSupplierBill, PartyID: "forwarder.1", Reference: "BILL-650"}, Amount: money(cny, 65000),
		OccurredOn: at, AnalysisOn: at, Basis: "合成：货代账单", EvidenceIDs: []string{"bill.650"}, By: by})
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != StatusPending || c.Version != 2 || c.Amount.MinorString() != "65000" || c.ForecastAmount().MinorString() != "50000" ||
		c.EstimateReference != "OFFER-1" || c.Source.Reference != "BILL-650" {
		t.Fatalf("billed %+v", c)
	}
	if err := c.Bill(BillInput{Amount: money(cny, 1)}); err == nil {
		t.Fatal("a pending charge took another bill")
	}
	a, err := c.Allocate(AllocateInput{Basis: BasisQuantity, DataBasis: "FUL: shipment.60 60 PCS, shipment.40 40 PCS",
		Lines: []LineInput{{LineID: "A", Target: s60, Weight: "60"}, {LineID: "B", Target: s40, Weight: "40"}}, Allowed: allowed(s60, s40), By: by})
	if err != nil {
		t.Fatal(err)
	}
	if a.Seq != 1 || a.ChargeVersion != 3 || a.Lines[0].Amount.MinorString() != "39000" || a.Lines[1].Amount.MinorString() != "26000" ||
		c.AllocationSeq != 1 || c.Version != 3 {
		t.Fatalf("allocation %+v", a)
	}
	if err := c.Confirm(by, []string{"bill.650"}); err != nil || c.Status != StatusConfirmed || c.Version != 4 || c.Current() != a {
		t.Fatalf("confirmed %+v %v", c, err)
	}
	if err := c.Confirm(by, []string{"bill.650"}); err == nil {
		t.Fatal("confirmed twice")
	}
	shares, ok := c.SharesOf(c.Amount)
	if !ok || len(shares) != 2 || shares[0].Amount.MinorString() != "39000" || shares[1].Target != s40 {
		t.Fatalf("shares %+v", shares)
	}
	forecast, ok := c.SharesOf(c.ForecastAmount())
	if !ok || forecast[0].Amount.MinorString() != "30000" || forecast[1].Amount.MinorString() != "20000" {
		t.Fatalf("the estimate by the same weights %+v", forecast)
	}
}

// STL-INV-010's negative example and its rules: weights summing to zero, a negative weight or no line are
// ALLOCATION_INVALID; a target twice or outside the beneficiaries' orders and shipments is refused; a line ID
// twice is DUPLICATE_LINE_ID.
func TestAllocationNeedsAPositiveBasis(t *testing.T) {
	s60, s40, other := ship("order.sa", "shipment.60"), ship("order.sa", "shipment.40"), ship("order.x", "shipment.x")
	c := record(t, StatusPending, money(cny, 65000), s60, s40)
	in := func(lines ...LineInput) AllocateInput {
		return AllocateInput{Basis: BasisWeight, DataBasis: "B/L gross weights", Lines: lines, Allowed: allowed(s60, s40), By: by}
	}
	for name, x := range map[string]struct {
		in         AllocateInput
		code, path string
	}{
		"zero weights":    {in(LineInput{"A", s60, "0"}, LineInput{"B", s40, "0.000"}), CodeAllocationInvalid, "/args/lines"},
		"a negative":      {in(LineInput{"A", s60, "70"}, LineInput{"B", s40, "-10"}), CodeAllocationInvalid, "/args/lines/1/weight"},
		"not a number":    {in(LineInput{"A", s60, "1e3"}), CodeAllocationInvalid, "/args/lines/0/weight"},
		"no line":         {in(), CodeAllocationInvalid, "/args/lines"},
		"a target twice":  {in(LineInput{"A", s60, "1"}, LineInput{"B", s60, "1"}), kernel.CodeInvalidKnownValue, "/args/lines/1/target"},
		"another order":   {in(LineInput{"A", other, "1"}), kernel.CodeInvalidKnownValue, "/args/lines/0/target"},
		"a line ID twice": {in(LineInput{"A", s60, "1"}, LineInput{"A", s40, "1"}), kernel.CodeDuplicateLineID, "/args/lines/1/line_id"},
	} {
		_, err := c.Allocate(x.in)
		want(t, name, err, x.code, x.path)
	}
	if c.Version != 1 || c.AllocationSeq != 0 || len(c.Allocations) != 0 {
		t.Fatalf("a refused allocation changed the charge %+v", c)
	}
	if _, ok := (&Charge{Beneficiaries: []Target{s60, other}}).SharesOf(money(cny, 1)); ok {
		t.Fatal("a charge of two orders without an allocation is attributable")
	}
}

// Baseline 4.5: the largest remainder in minor units, ties by line ID, on the absolute value of a negative
// adjustment then signed — no line gets a share against the charge's sign, and the shares add up exactly.
func TestLargestRemainderTiesAndNegativeAdjustments(t *testing.T) {
	a, b, c := ship("o", "a"), ship("o", "b"), ship("o", "c")
	for name, x := range map[string]struct {
		amount int64
		w      [3]string
		want   [3]string
	}{
		"0.10 over three equal weights": {10, [3]string{"1", "1", "1"}, [3]string{"4", "3", "3"}},
		"-0.10 signed back":             {-10, [3]string{"1", "1", "1"}, [3]string{"-4", "-3", "-3"}},
		"0.05 at 1:1:2":                 {5, [3]string{"1", "1", "2"}, [3]string{"1", "1", "3"}},
		"-6.50 at 60:40:0":              {-650, [3]string{"60", "40", "0"}, [3]string{"-390", "-260", "0"}},
		"1.00 at 0.333333 each":         {100, [3]string{"0.333333", "0.333333", "0.333333"}, [3]string{"34", "33", "33"}},
	} {
		ch := record(t, StatusPending, money(usd, x.amount), a, b, c)
		al, err := ch.Allocate(AllocateInput{Basis: BasisAmount, DataBasis: "basis", Allowed: allowed(a, b, c), By: by,
			Lines: []LineInput{{"L1", a, x.w[0]}, {"L2", b, x.w[1]}, {"L3", c, x.w[2]}}})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var sum int64
		for i, l := range al.Lines {
			if l.Amount.MinorString() != x.want[i] {
				t.Fatalf("%s: line %d %s, want %s", name, i, l.Amount.MinorString(), x.want[i])
			}
			sum += l.Amount.Minor().Int64()
		}
		if sum != x.amount {
			t.Fatalf("%s: shares add up to %d", name, sum)
		}
	}
}

// A bill of another amount than the estimate drops the allocation (it split the estimate); the same amount keeps
// it. A charge of one order with several shipments and no allocation is the order's.
func TestABillOfAnotherAmountIsAllocatedAgain(t *testing.T) {
	s60, s40 := ship("order.sa", "shipment.60"), ship("order.sa", "shipment.40")
	c := record(t, StatusEstimated, money(cny, 50000), s60, s40)
	if shares, ok := c.SharesOf(c.Amount); !ok || len(shares) != 1 || shares[0].Target != (Target{Kind: TargetOrder, OrderID: "order.sa"}) {
		t.Fatalf("unallocated charge of one order %+v", shares)
	}
	if _, err := c.Allocate(AllocateInput{Basis: BasisQuantity, DataBasis: "plan", Allowed: allowed(s60, s40), By: by,
		Lines: []LineInput{{"A", s60, "60"}, {"B", s40, "40"}}}); err != nil {
		t.Fatal(err)
	}
	same := *c
	same.Allocations = append([]Allocation(nil), c.Allocations...)
	if err := same.Bill(BillInput{Source: c.Source, Amount: money(cny, 50000), OccurredOn: at, AnalysisOn: at, Basis: "bill", By: by}); err != nil ||
		same.AllocationSeq != 1 {
		t.Fatalf("same amount %+v %v", same, err)
	}
	if err := c.Bill(BillInput{Source: c.Source, Amount: money(cny, 65000), OccurredOn: at, AnalysisOn: at, Basis: "bill", By: by}); err != nil ||
		c.AllocationSeq != 0 || c.Current() != nil || len(c.Allocations) != 1 {
		t.Fatalf("another amount %+v %v", c, err)
	}
	if err := c.Bill(BillInput{Amount: money(usd, 1)}); err == nil {
		t.Fatal("billed in another currency")
	}
}
