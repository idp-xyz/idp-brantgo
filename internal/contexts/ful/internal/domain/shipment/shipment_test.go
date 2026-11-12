package shipment

import (
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func outcome(err error) string {
	if err == nil {
		return "OK"
	}
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code + " " + r.Fields[0].Path
	}
	return err.Error()
}

var (
	merch = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "merch.1"}
	agent = kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.1"}
	now   = time.Date(2026, 11, 12, 8, 0, 0, 123456789, time.UTC)
	ref1  = DeliveryRef{DeliveryID: "delivery.1", LineID: "L1"}
	ref2  = DeliveryRef{DeliveryID: "delivery.2", LineID: "L1"}
)

func dec(s string) kernel.Decimal { return kernel.MustDecimal(s) }

func scopeOf(commitment string) Scope {
	return Scope{CommitmentID: commitment, SupplierID: "supplier.1", OrderID: "order.1", OrderLineID: "L1", SkuID: "sku.1", SpecRevision: 2,
		UnitID: "pcs", UnitRevision: 1}
}

// inputs is order.1 at revision 1 with line L1 of 1000 PCS of sku.1, customer.1 with its buyer and
// payer addresses, release rel.1 of 600 and delivery.1 line L1 at version 1 with 800 verified, both
// of commitment.1 from supplier.1; nothing consumed.
func inputs() Inputs {
	return Inputs{
		Order: &OrderFact{Binding: OrderBinding{OrderID: "order.1", OrderRevision: 1, OrderHash: strings.Repeat("a", 64)}, CustomerID: "customer.1",
			Customer: 3, Lines: []OrderLine{{LineID: "L1", SkuID: "sku.1", SpecRevision: 2, UnitID: "pcs", UnitRevision: 1, Quantity: dec("1000")}},
			Addresses: []Party{{Role: "buyer", PartyID: "customer.1", PartyRevision: 3, AddressID: "hq"}, {Role: "payer", PartyID: "payer.1",
				PartyRevision: 1, AddressID: "ap"}}},
		Releases: map[string]ReleaseFact{"rel.1": {ID: "rel.1", State: "granted", SubjectKind: SubjectCommitmentLine, Scope: scopeOf("commitment.1"),
			Quantity: dec("600")}},
		Deliveries: map[DeliveryRef]DeliveryFact{ref1: {Ref: ref1, Version: 1, Status: "verified", Scope: scopeOf("commitment.1"), Verified: dec("800")}},
		Preparer:   merch, Epoch: 7, Team: "trade", Now: now,
	}
}

func request(lines ...RequestLine) Request {
	if len(lines) == 0 {
		lines = []RequestLine{{ID: "S1", OrderLineID: "L1", Quantity: dec("200"), Delivery: ref1, Version: 1, ReleaseID: "rel.1"}}
	}
	return Request{ID: "shipment.1", OrderID: "order.1", Route: RouteSupplierDirect, Lines: lines, RequestHash: strings.Repeat("b", 64)}
}

// shipment.prepare binds the order, its lines and the parties the confirmation screens (R27, R44).
func TestPrepareBindsTheOrderLinesAndParties(t *testing.T) {
	s, err := Prepare(request(), inputs())
	if err != nil {
		t.Fatal(err)
	}
	l := s.Lines[0]
	if s.Status != StatusPrepared || s.RowVersion != 1 || s.Order.OrderRevision != 1 || s.CustomerID != "customer.1" || s.OwnerActor != "merch.1" ||
		s.OwnerTeam != "trade" || s.Prepared.Epoch != 7 || !s.Prepared.At.Equal(kernel.Instant(now)) || s.Prepared.At.Nanosecond()%1000 != 0 {
		t.Fatalf("shipment %+v", s)
	}
	if l.OrderQuantity.String() != "1000" || l.Quantity.String() != "200" || l.DeliveryVersion != 1 || l.CommitmentID != "commitment.1" ||
		l.SupplierID != "supplier.1" || l.SkuID != "sku.1" || l.UnitID != "pcs" {
		t.Fatalf("line %+v", l)
	}
	if len(s.Parties) != 3 || s.Parties[0] != (Party{Role: RoleCustomer, PartyID: "customer.1", PartyRevision: 3}) || s.Parties[2].Role != "payer" {
		t.Fatalf("parties %+v", s.Parties)
	}
	if got := s.ScreenedParties(); len(got) != 2 || got[0] != "customer.1" || got[1] != "payer.1" {
		t.Fatalf("screened %v", got)
	}
}

// What a preparation refuses, in order (D-10, R26, R27, R59, FUL-INV-001, FUL-INV-003).
func TestPrepareRefusals(t *testing.T) {
	line := func(edit func(*RequestLine)) Request {
		l := RequestLine{ID: "S1", OrderLineID: "L1", Quantity: dec("200"), Delivery: ref1, Version: 1, ReleaseID: "rel.1"}
		edit(&l)
		return request(l)
	}
	for _, c := range []struct {
		name string
		req  Request
		edit func(*Inputs)
		want string
	}{
		{"stock route (D-10)", func() Request { r := request(); r.Route = RouteStock; return r }(), nil, "DEPENDENCY_NOT_SUPPORTED /args/route"},
		{"no order", request(), func(in *Inputs) { in.Order = nil }, "REFERENCE_INVALID /args/order_id"},
		{"order not active", request(), func(in *Inputs) { in.Order.Code = CodeOrderNotActive }, "ORDER_NOT_ACTIVE /args/order_id"},
		{"order code FUL does not declare", request(), func(in *Inputs) { in.Order.Code = "SURPRISE" }, "REFERENCE_INVALID /args/order_id"},
		{"order line missing", line(func(l *RequestLine) { l.OrderLineID = "L9" }), nil, "REFERENCE_INVALID /args/lines/0/order_line_id"},
		{"release unread", line(func(l *RequestLine) { l.ReleaseID = "rel.9" }), nil, "REFERENCE_INVALID /args/lines/0/quality_release_id"},
		{"release revoked", request(), func(in *Inputs) {
			r := in.Releases["rel.1"]
			r.Code, r.State = CodeQualityReleaseRequired, "revoked"
			in.Releases["rel.1"] = r
		}, "QUALITY_RELEASE_REQUIRED /args/lines/0/quality_release_id"},
		{"release of another order line", request(), func(in *Inputs) {
			r := in.Releases["rel.1"]
			r.Scope.OrderLineID = "L2"
			in.Releases["rel.1"] = r
		}, "REFERENCE_INVALID /args/lines/0/quality_release_id"},
		{"release in another unit", request(), func(in *Inputs) {
			r := in.Releases["rel.1"]
			r.Scope.UnitID = "dozen"
			in.Releases["rel.1"] = r
		}, "DEPENDENCY_NOT_SUPPORTED /args/lines/0/quality_release_id"},
		{"delivery unread", line(func(l *RequestLine) { l.Delivery = ref2 }), nil, "REFERENCE_INVALID /args/lines/0/supplier_delivery"},
		{"delivery at another version", line(func(l *RequestLine) { l.Version = 2 }), nil, "REFERENCE_INVALID /args/lines/0/supplier_delivery"},
		{"delivery unverified", request(), func(in *Inputs) {
			d := in.Deliveries[ref1]
			d.Code, d.Status = CodeSupplierDeliveryUnverified, "recorded"
			in.Deliveries[ref1] = d
		}, "SUPPLIER_DELIVERY_UNVERIFIED /args/lines/0/supplier_delivery"},
		{"delivery superseded", request(), func(in *Inputs) {
			d := in.Deliveries[ref1]
			d.Code = CodeReferenceStale
			in.Deliveries[ref1] = d
		}, "REFERENCE_STALE /args/lines/0/supplier_delivery"},
		{"delivery of another specification", request(), func(in *Inputs) {
			d := in.Deliveries[ref1]
			d.Scope.SpecRevision = 1
			in.Deliveries[ref1] = d
		}, "REFERENCE_INVALID /args/lines/0/supplier_delivery"},
		{"delivery and release of different commitments", request(), func(in *Inputs) {
			d := in.Deliveries[ref1]
			d.Scope.CommitmentID = "commitment.2"
			in.Deliveries[ref1] = d
		}, "REFERENCE_INVALID /args/lines/0/supplier_delivery"},
		{"delivery and release of different suppliers", request(), func(in *Inputs) {
			d := in.Deliveries[ref1]
			d.Scope.SupplierID = "supplier.2"
			in.Deliveries[ref1] = d
		}, "REFERENCE_INVALID /args/lines/0/supplier_delivery"},
		{"order line exceeded (FUL-INV-001)", line(func(l *RequestLine) { l.Quantity = dec("201") }), func(in *Inputs) {
			in.Consumed.OrderLines = map[string]kernel.Decimal{"L1": dec("800")}
		}, "SHIPMENT_QUANTITY_EXCEEDED /args/lines/0/quantity"},
		{"delivery insufficient (FUL-INV-003)", line(func(l *RequestLine) { l.Quantity = dec("101") }), func(in *Inputs) {
			in.Consumed.Deliveries = map[DeliveryRef]kernel.Decimal{ref1: dec("700")}
		}, "SUPPLIER_DELIVERY_INSUFFICIENT /args/lines/0/quantity"},
		{"release insufficient", line(func(l *RequestLine) { l.Quantity = dec("601") }), nil, "QUALITY_RELEASE_INSUFFICIENT /args/lines/0/quantity"},
	} {
		in := inputs()
		if c.edit != nil {
			c.edit(&in)
		}
		if _, err := Prepare(c.req, in); outcome(err) != c.want {
			t.Errorf("%s: %s, want %s", c.name, outcome(err), c.want)
		}
	}
}

// The lines of one shipment count together, and one delivery line is never named at two versions.
func TestPrepareCountsTheLinesTogether(t *testing.T) {
	in := inputs()
	in.Releases["rel.2"] = ReleaseFact{ID: "rel.2", State: "granted", SubjectKind: SubjectCommitmentLine, Scope: scopeOf("commitment.1"),
		Quantity: dec("600")}
	a := RequestLine{ID: "S1", OrderLineID: "L1", Quantity: dec("500"), Delivery: ref1, Version: 1, ReleaseID: "rel.1"}
	b := RequestLine{ID: "S2", OrderLineID: "L1", Quantity: dec("301"), Delivery: ref1, Version: 1, ReleaseID: "rel.2"}
	if _, err := Prepare(request(a, b), in); outcome(err) != "SUPPLIER_DELIVERY_INSUFFICIENT /args/lines/1/quantity" {
		t.Fatal(outcome(err))
	}
	b.Quantity = dec("300")
	if s, err := Prepare(request(a, b), in); err != nil || len(s.Deliveries()) != 1 || s.Deliveries()[0].Quantity.String() != "800" ||
		len(s.Releases()) != 2 || s.OrderLines()[0].Quantity.String() != "800" {
		t.Fatalf("%v", err)
	}
	b.Version = 2
	if _, err := Prepare(request(a, b), in); outcome(err) != "REFERENCE_INVALID /args/lines/1/supplier_delivery" {
		t.Fatal(outcome(err))
	}
}

// BT11: the same request by the same preparer is answered from the stored shipment; anything else
// under a taken shipment ID is BUSINESS_KEY_CONFLICT.
func TestAnswerATakenShipmentID(t *testing.T) {
	s, _ := Prepare(request(), inputs())
	if s.Answer(s.RequestHash, merch) != nil {
		t.Fatal("same request")
	}
	if outcome(s.Answer(strings.Repeat("c", 64), merch)) != "BUSINESS_KEY_CONFLICT /args/shipment_id" ||
		outcome(s.Answer(s.RequestHash, agent)) != "BUSINESS_KEY_CONFLICT /args/shipment_id" {
		t.Fatal("conflict")
	}
}

func pkg(cartons int64, from int64, per string) Package {
	g, n, v := dec("10.5"), dec("9.5"), dec("0.05")
	return Package{Cartons: cartons, CartonFrom: from, Contents: []Content{{LineID: "S1", PerCarton: dec(per)}}, Gross: &g, Net: &n, Volume: &v}
}

// Packing (UL FUL-ActualPackage, BT11 "箱至订单数量一致"): the packages hold exactly every line's
// quantity; a new record replaces the one before of its kind and moves the row version.
func TestPackingAccountsForEveryLine(t *testing.T) {
	s, _ := Prepare(request(), inputs())
	act := Act{By: agent, Epoch: 8, At: now}
	if _, err := s.Pack(KindPlan, 1, []Package{pkg(19, 0, "10")}, nil, nil, act); outcome(err) != "INVALID_KNOWN_VALUE /args/packages" {
		t.Fatal(outcome(err))
	}
	other := pkg(1, 0, "200")
	other.Contents[0].LineID = "S9"
	if _, err := s.Pack(KindPlan, 1, []Package{other}, nil, nil, act); outcome(err) != "REFERENCE_INVALID /args/packages/0/contents/0/line_id" {
		t.Fatal(outcome(err))
	}
	if _, err := s.Pack(KindPlan, 1, []Package{pkg(20, 0, "10")}, []string{"list"}, map[string]bool{}, act); outcome(err) != "REFERENCE_INVALID /args/evidence_ids/0" {
		t.Fatal(outcome(err))
	}
	if _, err := s.Pack(KindPlan, 2, []Package{pkg(20, 0, "10")}, nil, nil, act); outcome(err) != "VERSION_CONFLICT /args/expected_version" {
		t.Fatal(outcome(err))
	}
	plan, err := s.Pack(KindPlan, 1, []Package{pkg(20, 0, "10")}, nil, nil, act)
	if err != nil || plan.Seq != 1 || plan.ShipmentVersion != 2 || s.RowVersion != 2 || s.PackingRecords != 1 || s.Plan != plan || plan.Cartons() != 20 {
		t.Fatalf("plan %+v %v", plan, err)
	}
	actual, err := s.Pack(KindActual, 2, []Package{pkg(10, 1, "12"), pkg(8, 11, "10")}, []string{"list"}, map[string]bool{"list": true}, act)
	if err != nil || actual.Seq != 1 || actual.ShipmentVersion != 3 || s.Actual != actual || actual.Cartons() != 18 {
		t.Fatalf("actual %+v %v", actual, err)
	}
	gross, net, volume, ok := actual.Totals()
	if !ok || gross.String() != "189" || net.String() != "171" || volume.String() != "0.9" {
		t.Fatalf("totals %s %s %s %v", gross, net, volume, ok)
	}
	again, err := s.Pack(KindActual, 3, []Package{pkg(20, 1, "10")}, nil, nil, act)
	if err != nil || again.Seq != 2 || s.RowVersion != 4 || s.PackingRecords != 3 || s.Plan != plan {
		t.Fatalf("second actual %+v %v", again, err)
	}
	if _, _, _, ok := (&Packing{Packages: []Package{{Cartons: 1}}}).Totals(); ok {
		t.Fatal("a plan without weights has no totals")
	}
}

// SM-FUL-SHIPMENT: prepared → cancelled once; a cancelled shipment takes nothing more.
func TestCancel(t *testing.T) {
	s, _ := Prepare(request(), inputs())
	if outcome(s.Cancel(2, "客户推迟", Act{By: agent, Epoch: 9, At: now})) != "VERSION_CONFLICT /args/expected_version" {
		t.Fatal("version")
	}
	if err := s.Cancel(1, "客户推迟", Act{By: agent, Epoch: 9, At: now}); err != nil || s.Status != StatusCancelled || s.RowVersion != 2 ||
		s.Cancellation.Reason != "客户推迟" {
		t.Fatalf("%+v %v", s, err)
	}
	if outcome(s.Cancel(2, "again", Act{By: agent, Epoch: 9, At: now})) != "INVALID_STATE /args/shipment_id" ||
		outcome(s.Confirmable(2)) != "INVALID_STATE /args/shipment_id" {
		t.Fatal("cancelled is final")
	}
	if _, err := s.Pack(KindPlan, 2, []Package{pkg(20, 0, "10")}, nil, nil, Act{By: agent}); outcome(err) != "INVALID_STATE /args/shipment_id" {
		t.Fatal("no packing after cancel")
	}
}

// The guard verdicts the confirmation judges (R44, R59, R26, R27) and FUL-INV-001 at confirmation.
func TestConfirmationVerdicts(t *testing.T) {
	s, _ := Prepare(request(), inputs())
	if e, err := s.CheckScreening("payer.1", ScreeningVerdict{Status: "matched", Reason: "list_match", ScreeningID: "scr.1"}); outcome(err) !=
		"PARTY_SCREENING_NOT_CLEAR /shipment/parties/2" || e.Name != "screening:payer.1" || e.Value != "matched/list_match/scr.1" {
		t.Fatalf("screening %+v %s", e, outcome(err))
	}
	if _, err := s.CheckScreening("customer.1", ScreeningVerdict{Clear: true, Status: "clear"}); err != nil {
		t.Fatal(err)
	}
	u := s.Deliveries()[0]
	good := DeliveryVerdict{Known: true, Status: "verified", VerifiedVersion: 1, Scope: scopeOf("commitment.1"), Verified: dec("800")}
	if e, err := s.CheckDelivery(u, good, dec("600")); err != nil || e.Name != "supplier_delivery:delivery.1/L1/v1" ||
		e.Value != "verified/verified_version=1/verified=800" {
		t.Fatalf("delivery %+v %v", e, err)
	}
	stale := good
	stale.Code, stale.VerifiedVersion = CodeReferenceStale, 2
	scope := good
	scope.Scope.CommitmentID = "commitment.2"
	for v, want := range map[*DeliveryVerdict]string{
		{}:     "SUPPLIER_DELIVERY_UNVERIFIED /shipment/lines/0/supplier_delivery",
		&stale: "REFERENCE_STALE /shipment/lines/0/supplier_delivery",
		&scope: "REFERENCE_INVALID /shipment/lines/0/supplier_delivery",
	} {
		if _, err := s.CheckDelivery(u, *v, kernel.Decimal{}); outcome(err) != want {
			t.Errorf("%+v: %s, want %s", v, outcome(err), want)
		}
	}
	if _, err := s.CheckDelivery(u, good, dec("600.000001")); outcome(err) != "SUPPLIER_DELIVERY_INSUFFICIENT /shipment/lines/0/quantity" {
		t.Fatal(outcome(err))
	}
	r := s.Releases()[0]
	until := now.Add(24 * time.Hour)
	granted := ReleaseVerdict{Known: true, Status: "granted", SubjectKind: SubjectCommitmentLine, Scope: scopeOf("commitment.1"), Quantity: dec("600"),
		ValidUntil: until}
	if e, err := s.CheckRelease(r, granted, dec("400")); err != nil || e.Value != "granted/released=600/valid_until="+kernel.FormatTimestamp(until) {
		t.Fatalf("release %+v %v", e, err)
	}
	if _, err := s.CheckRelease(r, granted, dec("400.5")); outcome(err) != "QUALITY_RELEASE_INSUFFICIENT /shipment/lines/0/quantity" {
		t.Fatal(outcome(err))
	}
	revoked := granted
	revoked.Status, revoked.Code = "revoked", CodeQualityReleaseRequired
	expired := granted
	expired.Code = CodeQualityReleaseRequired
	other := granted
	other.Scope.SupplierID = "supplier.2"
	for v, want := range map[*ReleaseVerdict]string{
		{}:       "QUALITY_RELEASE_REQUIRED /shipment/lines/0/quality_release_id",
		&revoked: "QUALITY_RELEASE_REQUIRED /shipment/lines/0/quality_release_id",
		&expired: "QUALITY_RELEASE_REQUIRED /shipment/lines/0/quality_release_id",
		&other:   "REFERENCE_INVALID /shipment/lines/0/quality_release_id",
	} {
		if _, err := s.CheckRelease(r, *v, kernel.Decimal{}); outcome(err) != want {
			t.Errorf("%+v: %s, want %s", v, outcome(err), want)
		}
	}
	for v, want := range map[OrderVerdict]string{
		{Known: true, Status: "active", OrderRevision: 1}: "OK",
		{}: "REFERENCE_INVALID /args/shipment_id",
		{Known: true, Status: "active", OrderRevision: 2, Code: CodeReferenceStale}:       "REFERENCE_STALE /args/shipment_id",
		{Known: true, Status: "active", OrderRevision: 1, Code: CodeOrderApprovalRevoked}: "ORDER_APPROVAL_REVOKED /args/shipment_id",
		{Known: true, Status: "closed", OrderRevision: 1, Code: CodeOrderNotActive}:       "ORDER_NOT_ACTIVE /args/shipment_id",
	} {
		if e, err := CheckOrder(s.Order, v, "/args/shipment_id"); outcome(err) != want || e.Name != GuardOrder {
			t.Errorf("%+v: %s, want %s", v, outcome(err), want)
		}
	}
	if outcome(s.CheckOrderLines(map[string]kernel.Decimal{"L1": dec("800")})) != "OK" ||
		outcome(s.CheckOrderLines(map[string]kernel.Decimal{"L1": dec("800.000001")})) != "SHIPMENT_QUANTITY_EXCEEDED /shipment/lines/0/quantity" {
		t.Fatal("FUL-INV-001 at confirmation")
	}
}

// shipment.confirm is human only (humanOnlyCommands) and seals the guard evidence with its SHA-256;
// the extension points not wired are listed, never passed.
func TestConfirmSealsTheEvidence(t *testing.T) {
	s, _ := Prepare(request(), inputs())
	entries := []Entry{{Name: GuardCommercialRelease, Value: NotWired}, {Name: "screening:customer.1", Value: "clear/<ok>&/scr.1"}}
	if outcome(s.Confirm(nil, entries, nil, Act{By: agent, Epoch: 9, At: now})) != "PERMISSION_DENIED /" {
		t.Fatal("an AI never confirms")
	}
	if err := s.Confirm([]string{"bl"}, entries, []string{GuardCommercialRelease}, Act{By: merch, Epoch: 9, At: now}); err != nil {
		t.Fatal(err)
	}
	c := s.Confirmation
	doc, sum := Seal(entries)
	if s.Status != StatusConfirmed || s.RowVersion != 2 || c.EntriesJSON != doc || c.EntriesSHA256 != sum || len(sum) != 64 ||
		c.EntriesJSON != `[{"name":"ful.commercial_release.v1","value":"not_wired"},{"name":"screening:customer.1","value":"clear/<ok>&/scr.1"}]` ||
		c.Unchecked[0] != GuardCommercialRelease || c.Evidence[0] != "bl" {
		t.Fatalf("confirmation %+v", c)
	}
	if outcome(s.Confirm(nil, nil, nil, Act{By: merch})) != "INVALID_STATE /args/shipment_id" {
		t.Fatal("confirmed once")
	}
	if doc, _ := Seal(nil); doc != "[]" {
		t.Fatal(doc)
	}
}

// Upstream codes FUL does not declare never leave FUL.
func TestUpstreamCodes(t *testing.T) {
	if outcome(Upstream(CodeReferenceStale, "/x", "")) != "REFERENCE_STALE /x" || outcome(Upstream("PURCHASE_THING", "/x", "")) != "REFERENCE_INVALID /x" {
		t.Fatal("upstream")
	}
	if r := Reject(kernel.CodeVersionConflict, "/x", ""); r.Category != kernel.CategoryConflict {
		t.Fatal(r.Category)
	}
}
