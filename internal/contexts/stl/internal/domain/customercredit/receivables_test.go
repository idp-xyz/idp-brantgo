package customercredit

import (
	"testing"
	"time"
)

// The receivables of settlement statements in the credit exposure (S5-04b; STL-INV-004; S5 plan
// D-4 "credit exposure = open receivables + Σ max(0, live reservation − receivables formed for that
// order)"; D-5; SIM-02 step 11).

func owed(order, trigger, control, amount, applied string, due time.Time) OwedInstallment {
	return OwedInstallment{OrderID: order, Trigger: trigger, ReleaseControl: control, Amount: m(amount), Applied: m(applied), DueAt: due}
}

func consumed(order string, amount, secured string) Reservation {
	return Reservation{ID: "res." + order, Binding: Binding{OrderID: order, OrderRevision: 1, OrderHash: "h"}, Amount: m(amount),
		Secured: m(secured), Exposure: m(amount), Status: ReservationConsumed, Basis: BasisWithinLimit}
}

func exposure(t *testing.T, owe []OwedInstallment, reservations []Reservation, at time.Time) Verdict {
	t.Helper()
	r, err := ReceivablesOf(usd, owe, reservations)
	if err != nil {
		t.Fatal(err)
	}
	var held []Held
	for i := range reservations {
		if x := &reservations[i]; x.Live(at) {
			held = append(held, HeldOf(x))
		}
	}
	v, err := Assess(Assessment{Limit: m("10000"), Held: held, Receivables: r, OrderID: "o.new", Portion: m("0"), Now: at, ToleranceDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// BRC-001 (S5 plan §5 steps 6, 19, 20): the 672.00 reservation is consumed when the order activates;
// the sales statement of 960.00 forms P1 288.00 (paid: the prepayment moved) and P2 672.00 (open, due
// 30 days after shipment). The exposure is 672.00 before and after the statement — the consumed
// reservation and the receivable that replaced it are never counted twice — and 0.00 once 670.00 +
// 2.00 are applied. 4.6 S-A on 80 % after shipment (800.00): after the first statement of 600.00 (P2
// 400.00 formed and open) the exposure is 400.00 open + 400.00 not yet formed = 800.00.
func TestExposureCountsReceivablesOnce(t *testing.T) {
	shipped := t0.Add(-24 * time.Hour)
	res := []Reservation{consumed("o.1", "672", "0")}
	if v := exposure(t, nil, res, t0); v.Exposure.Amount() != "672.00" {
		t.Fatalf("before the statement: %+v", v)
	}
	brc := []OwedInstallment{owed("o.1", TriggerOrderConfirmed, "", "288", "288", shipped),
		owed("o.1", TriggerAfterShipment, "", "672", "0", shipped.AddDate(0, 0, 30))}
	r, err := ReceivablesOf(usd, brc, res)
	if err != nil || r.Open.Amount() != "672.00" || r.FormedBy["o.1"].Amount() != "672.00" || !r.EarliestDue.Equal(shipped.AddDate(0, 0, 30)) {
		t.Fatalf("receivables %+v %v", r, err)
	}
	if v := exposure(t, brc, res, t0); v.Exposure.Amount() != "672.00" || v.OverdueDays != -1 {
		t.Fatalf("after the statement: %+v", v)
	}
	brc[1].Applied = m("672")
	if v := exposure(t, brc, res, t0); v.Exposure.Amount() != "0.00" {
		t.Fatalf("receivable paid: %+v", v)
	}
	sa := []OwedInstallment{owed("o.2", TriggerOrderConfirmed, "", "200", "200", shipped), owed("o.2", TriggerAfterShipment, "", "400", "0",
		shipped.AddDate(0, 0, 30))}
	if v := exposure(t, sa, []Reservation{consumed("o.2", "800", "0")}, t0); v.Exposure.Amount() != "800.00" {
		t.Fatalf("4.6 S-A first statement: %+v", v)
	}
	// The order closed and its reservation released (PL-13): only the open receivable counts.
	released := consumed("o.2", "800", "0")
	released.Status = ReservationReleased
	if v := exposure(t, sa, []Reservation{released}, t0); v.Exposure.Amount() != "400.00" {
		t.Fatalf("after the release: %+v", v)
	}
}

// D-5: a reservation that left a document-controlled stage out as secured is not replaced by that
// stage's receivable; one that included it is. The open receivable counts either way.
func TestDocumentControlledReceivablesReplaceOnlyWhatWasReserved(t *testing.T) {
	shipped := t0.Add(-24 * time.Hour)
	owe := []OwedInstallment{owed("o.1", TriggerAfterShipment, ReleaseDocuments, "300", "0", shipped.AddDate(0, 0, 10)),
		owed("o.1", TriggerAfterShipment, "", "500", "0", shipped.AddDate(0, 0, 30))}
	r, err := ReceivablesOf(usd, owe, nil)
	if err != nil || r.FormedBy["o.1"].Amount() != "500.00" || r.FormedDocumentsBy["o.1"].Amount() != "300.00" || r.Open.Amount() != "800.00" {
		t.Fatalf("receivables %+v %v", r, err)
	}
	// Secured: reserved 500.00 (300.00 secured); formed 500.00 replaces it → exposure = open 800.00.
	if v := exposure(t, owe, []Reservation{consumed("o.1", "500", "300")}, t0); v.Exposure.Amount() != "800.00" {
		t.Fatalf("secured: %+v", v)
	}
	// Not secured: reserved 800.00; formed 800.00 replaces it → exposure = open 800.00.
	if v := exposure(t, owe, []Reservation{consumed("o.1", "800", "0")}, t0); v.Exposure.Amount() != "800.00" {
		t.Fatalf("not secured: %+v", v)
	}
	// Before the statement the two differ by what was reserved.
	if v := exposure(t, nil, []Reservation{consumed("o.1", "500", "300")}, t0); v.Exposure.Amount() != "500.00" {
		t.Fatalf("secured before: %+v", v)
	}
}

// SIM-02 step 11 and STL-INV-004: the earliest open installment 45 days past due exceeds the 30 days'
// tolerance (CREDIT_OVERDUE_EXCEEDED); a paid one does not count.
func TestTheEarliestOpenInstallmentFeedsTheOverdueTolerance(t *testing.T) {
	owe := []OwedInstallment{owed("o.1", TriggerAfterShipment, "", "100", "100", t0.AddDate(0, 0, -60)),
		owed("o.1", TriggerAfterShipment, "", "100", "40", t0.AddDate(0, 0, -45)), owed("o.2", TriggerAfterShipment, "", "50", "0", t0.AddDate(0, 0, 5))}
	r, err := ReceivablesOf(usd, owe, nil)
	if err != nil || !r.EarliestDue.Equal(t0.AddDate(0, 0, -45)) || r.Open.Amount() != "110.00" {
		t.Fatalf("receivables %+v %v", r, err)
	}
	v, err := Assess(Assessment{Limit: m("10000"), Receivables: r, OrderID: "o.new", Portion: m("100"), Now: t0, ToleranceDays: 30})
	if err != nil || !v.Overdue() || v.OverdueDays != 45 || v.Exposure.Amount() != "110.00" {
		t.Fatalf("overdue %+v %v", v, err)
	}
}

// STL-INV-004 across currencies: a receivable in the order currency (CNY) converts into the limit
// currency (USD) at the rate its order's reservation was made at (exposure ÷ portion, HALF_UP); with
// no such reservation an open amount is FX_SCOPE_MISMATCH (fail closed), a zero one converts to zero.
func TestReceivablesInAnotherCurrencyConvertAtTheReservationRate(t *testing.T) {
	res := Reservation{ID: "res.c", Binding: Binding{OrderID: "o.c", OrderRevision: 2}, Amount: mc(cny, "7000"), Secured: mc(cny, "0"),
		Exposure: m("1000"), Status: ReservationConsumed}
	owe := []OwedInstallment{{OrderID: "o.c", Trigger: TriggerAfterShipment, Amount: mc(cny, "7000"), Applied: mc(cny, "3500"), DueAt: t0},
		{OrderID: "o.c", Trigger: TriggerAfterShipment, Amount: mc(cny, "1"), Applied: mc(cny, "0"), DueAt: t0}}
	r, err := ReceivablesOf(usd, owe, []Reservation{res})
	if err != nil || r.Open.Amount() != "500.14" || r.FormedBy["o.c"].Amount() != "1000.14" {
		t.Fatalf("converted %+v %v", r, err)
	}
	if _, err = ReceivablesOf(usd, owe, nil); code(err) != CodeFXScopeMismatch {
		t.Fatalf("no rate: %v", err)
	}
	owe[0].Applied, owe = owe[0].Amount, owe[:1]
	owe[0].Trigger = TriggerBeforeShipment
	if r, err = ReceivablesOf(usd, owe, nil); err != nil || r.Open.Amount() != "0.00" {
		t.Fatalf("nothing open: %+v %v", r, err)
	}
}

// An opening receivable (S5 C10, STL-INV-012 positive "截止日导入未结应收 7,000"; SC-09's limit 10,000 with 7,000 open
// from a prior order): it binds no order, so it counts as open and forms no order's credit stage — a reservation
// of a new order keeps its whole share. SC-09's portion 4,000 then exceeds the 10,000 limit by 1,000
// (CREDIT_LIMIT_EXCEEDED: a decision opens); 45 days overdue it also exceeds the 30 days' tolerance.
func TestAnOpeningReceivableCountsAsOpenInTheExposure(t *testing.T) {
	at := time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	owe := []OwedInstallment{owed("", "", "", "7000.00", "0.00", at.AddDate(0, 0, 3))}
	r, err := ReceivablesOf(usd, owe, nil)
	if err != nil || r.Open.String() != "7000.00 USD" || len(r.FormedBy) != 0 || len(r.FormedDocumentsBy) != 0 || !r.EarliestDue.Equal(at.AddDate(0, 0, 3)) {
		t.Fatalf("%+v %v", r, err)
	}
	v, err := Assess(Assessment{Limit: m("10000"), Receivables: r, OrderID: "o.sc09", Portion: m("4000"), Now: at, ToleranceDays: 30})
	if err != nil || v.Exposure.String() != "7000.00 USD" || v.After.String() != "11000.00 USD" || v.Excess.String() != "1000.00 USD" ||
		v.Within() || v.Overdue() {
		t.Fatalf("%+v %v", v, err)
	}
	if v = exposure(t, []OwedInstallment{owed("", "", "", "7000.00", "6000.00", at.AddDate(0, 0, -45))}, nil, at); v.Exposure.String() != "1000.00 USD" ||
		!v.Overdue() || v.OverdueDays != 45 {
		t.Fatalf("overdue opening receivable: %+v", v)
	}
}
