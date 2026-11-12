package exportlicense

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The vectors come from the design (CNX-INV-002, TD-AT13, ES-12 step 3); Python V0.50 has no CNX
// ("—（Python 未实现）"). All licence data is synthetic (S5 E-4).

var (
	usd   = kernel.MustCurrency("USD", 2)
	from  = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	until = time.Date(2027, 5, 1, 0, 0, 0, 0, time.UTC)
	now   = time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
)

func money(t *testing.T, minor int64) kernel.Money {
	t.Helper()
	m, err := kernel.NewMoney(usd, minor)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func licence(t *testing.T) *License {
	return &License{ID: "elc.1", Number: "SYN-EL-2026-0001", Issuer: "合成发证机关", Holder: "合成出口商", Goods: Goods{Specs: []Spec{{"sku.1", 2}}},
		Unit: "PCS", Quantity: kernel.MustDecimal("1000"), Currency: Currency{ConfigID: "USD", Revision: 1, Code: "USD", MinorUnits: 2},
		Value: money(t, 1_000_000), Destination: "DE", EndUserPartyID: "customer.1", EndUserName: "合成最终用户",
		Validity: kernel.ValidityInterval{From: from, Until: until}, RowVersion: 1}
}

func target(t *testing.T, id string, qty string, valueMinor int64) Target {
	return Target{Kind: TargetShipment, ID: id, Version: 1, OrderID: "order.1", OrderRevision: 1, Destination: "DE", EndUserPartyID: "customer.1",
		Lines: []Line{{LineID: "l1", SkuID: "sku.1", SpecRevision: 2, Quantity: kernel.MustDecimal(qty), Unit: "PCS"}}, Value: money(t, valueMinor)}
}

func allocate(t *testing.T, l *License, allocs []*Allocation, tg Target, at time.Time) (*Allocation, error) {
	return Allocate(l, allocs, AllocateInput{ID: "ela." + tg.ID, Target: tg, Attempt: 1, By: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "doc.1"}, Epoch: 1, At: at})
}

func code(err error) string {
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code + " " + r.Fields[0].Path
	}
	if err != nil {
		return err.Error()
	}
	return "OK"
}

// CNX-INV-002 positive "许可 1,000 件，分配 600＋400", negative "再分配 1 件返回 EXPORT_LICENSE_EXCEEDED";
// TD-AT13: licensed 1,000, allocated 600, allocating 500 → EXPORT_LICENSE_EXCEEDED. Value is a second
// ceiling checked the same way.
func TestTDAT13QuantityAndValueNeverExceedTheLicence(t *testing.T) {
	l := licence(t)
	a600, err := allocate(t, l, nil, target(t, "s1", "600", 600_000), now)
	if err != nil || a600.Quantity.String() != "600" || a600.LinesHash == "" {
		t.Fatalf("%+v %v", a600, err)
	}
	if _, err := allocate(t, l, []*Allocation{a600}, target(t, "s2", "500", 1_000), now); code(err) != "EXPORT_LICENSE_EXCEEDED /args/target/id" {
		t.Fatalf("600+500: %s", code(err))
	}
	a400, err := allocate(t, l, []*Allocation{a600}, target(t, "s2", "400", 400_000), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := allocate(t, l, []*Allocation{a600, a400}, target(t, "s3", "1", 0), now); code(err) != "EXPORT_LICENSE_EXCEEDED /args/target/id" {
		t.Fatalf("600+400+1: %s", code(err))
	}
	u, err := UsageOf(l, []*Allocation{a600, a400})
	if err != nil || u.Available.Sign() != 0 || !u.AvailableValue.IsZero() || u.Allocated.String() != "1000" {
		t.Fatalf("usage %+v %v", u, err)
	}
	if _, err := allocate(t, l, []*Allocation{a600}, target(t, "s4", "100", 400_001), now); code(err) != "EXPORT_LICENSE_EXCEEDED /args/target/id" {
		t.Fatalf("value: %s", code(err))
	}
}

// TD-AT13 "另一许可证已过期" and the catalog's EXPORT_LICENSE_EXPIRED ("许可证已过期或目的国、最终用户不一致"):
// outside the validity, another destination or end user, or goods the licence does not cover.
func TestTDAT13LicenceMustApplyToTheTarget(t *testing.T) {
	l := licence(t)
	for name, c := range map[string]struct {
		edit func(*Target)
		at   time.Time
		want string
	}{
		"expired":     {func(*Target) {}, until, "EXPORT_LICENSE_EXPIRED /args/license_id"},
		"not yet":     {func(*Target) {}, from.Add(-time.Second), "EXPORT_LICENSE_EXPIRED /args/license_id"},
		"destination": {func(x *Target) { x.Destination = "FR" }, now, "EXPORT_LICENSE_EXPIRED /args/target/id"},
		"end user":    {func(x *Target) { x.EndUserPartyID = "customer.2" }, now, "EXPORT_LICENSE_EXPIRED /args/target/id"},
		"goods":       {func(x *Target) { x.Lines[0].SpecRevision = 3 }, now, "EXPORT_LICENSE_EXPIRED /args/target/id"},
		"unit":        {func(x *Target) { x.Lines[0].Unit = "DOZEN" }, now, "INVALID_KNOWN_VALUE /args/target/id"},
		"currency":    {func(x *Target) { x.Value, _ = kernel.NewMoney(kernel.MustCurrency("EUR", 2), 1) }, now, "INVALID_KNOWN_VALUE /args/target/id"},
		"no lines":    {func(x *Target) { x.Lines = nil }, now, "INVALID_STATE /args/target/id"},
	} {
		tg := target(t, "s1", "10", 100)
		c.edit(&tg)
		if _, err := allocate(t, l, nil, tg, c.at); code(err) != c.want {
			t.Errorf("%s: %s, want %s", name, code(err), c.want)
		}
	}
	if _, err := allocate(t, l, nil, target(t, "s1", "10", 100), until.Add(-time.Microsecond)); err != nil {
		t.Fatalf("last instant of the validity: %v", err)
	}
	// A licence by classification covers a line through its verified profile's commodity code.
	byCode := licence(t)
	byCode.Goods = Goods{CodeTable: "CN-2026", CommodityCodes: []string{"6109100021"}}
	tg := target(t, "s1", "10", 100)
	if _, err := allocate(t, byCode, nil, tg, now); code(err) != "EXPORT_LICENSE_EXPIRED /args/target/id" {
		t.Fatalf("unclassified line: %s", code(err))
	}
	tg.Lines[0].CommodityCode = "6109100021"
	if _, err := allocate(t, byCode, nil, tg, now); err != nil {
		t.Fatalf("classified line: %v", err)
	}
}

// CNX-INV-002 "报关改单或撤销按外部结果释放用量": an amended receipt releases what it names, never more
// than is outstanding; a withdrawn one the rest; released usage is available again; a fully
// released allocation is not released again.
func TestReleasesFollowTheExternalResult(t *testing.T) {
	l := licence(t)
	a, err := allocate(t, l, nil, target(t, "s1", "600", 600_000), now)
	if err != nil {
		t.Fatal(err)
	}
	in := ReleaseInput{ID: "elr.1", Kind: Amended, DeclarationNumber: "SYN-DEC-1", ReceiptText: "改单回执（合成）", ReceivedAt: now,
		Quantity: kernel.MustDecimal("601"), Value: money(t, 0), Reason: "改单", At: now}
	if _, err := ReleaseAllocation(a, in); code(err) != "INVALID_STATE /args/quantity" {
		t.Fatalf("more than outstanding: %s", code(err))
	}
	in.Quantity, in.Value = kernel.MustDecimal("100"), money(t, 600_001)
	if _, err := ReleaseAllocation(a, in); code(err) != "INVALID_STATE /args/value" {
		t.Fatalf("value: %s", code(err))
	}
	in.Value = money(t, 100_000)
	r, err := ReleaseAllocation(a, in)
	if err != nil || r.Source != Stated || r.Quantity.String() != "100" {
		t.Fatalf("%+v %v", r, err)
	}
	if err := a.Apply(r); err != nil {
		t.Fatal(err)
	}
	if q, v, _ := a.Outstanding(); q.String() != "500" || v.MinorString() != "500000" || a.Status() != Allocated {
		t.Fatalf("after amended: %s %s", q, v)
	}
	u, _ := UsageOf(l, []*Allocation{a})
	if u.Available.String() != "500" {
		t.Fatalf("available %s", u.Available)
	}
	w, err := ReleaseAllocation(a, ReleaseInput{ID: "elr.2", Kind: Withdrawn, Quantity: kernel.MustDecimal("1"), At: now})
	if err != nil || w.Quantity.String() != "500" || w.Value.MinorString() != "500000" {
		t.Fatalf("withdrawn %+v %v", w, err)
	}
	if err := a.Apply(w); err != nil || a.Status() != Released {
		t.Fatalf("%v %s", err, a.Status())
	}
	if _, err := ReleaseAllocation(a, ReleaseInput{ID: "elr.3", Kind: Withdrawn, At: now}); code(err) != "INVALID_STATE /args/allocation_id" {
		t.Fatalf("again: %s", code(err))
	}
	if u, _ := UsageOf(l, []*Allocation{a}); u.Available.String() != "1000" || !u.AllocatedValue.IsZero() {
		t.Fatalf("usage %+v", u)
	}
}

// The lines hash is order-independent and covers line, specification, quantity (six decimals) and
// unit.
func TestLinesHash(t *testing.T) {
	a := []Line{{LineID: "l1", SkuID: "sku.1", SpecRevision: 2, Quantity: kernel.MustDecimal("10"), Unit: "PCS"},
		{LineID: "l2", SkuID: "sku.2", SpecRevision: 1, Quantity: kernel.MustDecimal("5.5"), Unit: "PCS"}}
	b := []Line{a[1], a[0]}
	if LinesHash(a) != LinesHash(b) {
		t.Fatal("order")
	}
	c := []Line{a[0], {LineID: "l2", SkuID: "sku.2", SpecRevision: 1, Quantity: kernel.MustDecimal("5.500000"), Unit: "PCS"}}
	if LinesHash(a) != LinesHash(c) {
		t.Fatal("scale")
	}
	c[1].Quantity = kernel.MustDecimal("5.500001")
	if LinesHash(a) == LinesHash(c) {
		t.Fatal("quantity")
	}
}

func TestStates(t *testing.T) {
	l := licence(t)
	if l.State(from.Add(-1)) != NotYetValid || l.State(from) != Valid || l.State(until) != Expired {
		t.Fatal("states")
	}
	if CheckVersion(l, 2) == nil || CheckVersion(l, 1) != nil {
		t.Fatal("version")
	}
}
