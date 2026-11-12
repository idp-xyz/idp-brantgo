package fulfilment

import (
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The shipment review's rules (SM-FUL-SHIPMENT-REVIEW; WF-06 step 5, its idempotency and failure
// mode; PL-09; S5 D-18, D-21; BT10). Python V0.50 has no fulfilment context: the tests cite the
// design.

var hash2 = strings.Repeat("2", 64)

func q(s string) kernel.Decimal { return kernel.MustDecimal(s) }

func amended(revision, version int64, targets ...Target) Change {
	c := Change{OrderID: "order.1", Revision: revision, Hash: hash2, Version: version, Cause: CauseOrderAmended, EventID: "evt.r" + string(rune('0'+revision)),
		Targets: map[string]Target{}}
	for _, t := range targets {
		c.Targets[t.LineID] = t
	}
	return c
}

func target(line, quantity string) Target {
	return Target{LineID: line, Quantity: q(quantity), SkuID: "sku.1", SpecRevision: 1, UnitID: "PCS"}
}

func line(id, orderLine, quantity string) HeldLine {
	return HeldLine{LineID: id, OrderLineID: orderLine, SkuID: "sku.1", SpecRevision: 1, UnitID: "PCS", Quantity: q(quantity)}
}

// bt10 is BT10's order on L1 of 120: shipment.1 confirmed 40 at revision 1 with its documents release
// granted; shipment.2 prepared for 80 at revision 1 with its packing plan and actual packing, a
// booking, two drafts and its shipment release granted (its documents release blocked); shipment.3
// prepared for 10 at revision 2; shipment.0 cancelled (not held).
func bt10() Holdings {
	return Holdings{Rooted: true, Shipped: map[LineUnit]kernel.Decimal{{"L1", "PCS"}: q("40")}, Shipments: []HeldShipment{
		{ID: "shipment.3", Status: ShipmentPrepared, RowVersion: 1, Revision: 2, Lines: []HeldLine{line("S1", "L1", "10")}},
		{ID: "shipment.2", Status: ShipmentPrepared, RowVersion: 3, Revision: 1, Lines: []HeldLine{line("S1", "L1", "80")},
			Packing:       []HeldPacking{{Kind: PackingActual, Seq: 2, Cartons: 8, Packages: 1}, {Kind: PackingPlan, Seq: 1, Cartons: 8, Packages: 1}},
			Booking:       &HeldBooking{Version: 2, Number: "BK1", Vessel: "V", Voyage: "1E", ETD: "2026-12-20"},
			DocumentSetID: "docs.2", Drafts: []HeldDocument{{Kind: "packing_list", Version: 1, Number: "PL-1"}, {Kind: "commercial_invoice", Version: 2, Number: "CI-1"}},
			Releases: []HeldRelease{{ID: "rls.2d", Scope: ScopeDocuments, Status: "blocked", RowVersion: 1, Evaluations: 1},
				{ID: "rls.2s", Scope: ScopeShipment, Status: ReleaseGranted, RowVersion: 1, Evaluations: 1}}},
		{ID: "shipment.1", Status: ShipmentConfirmed, RowVersion: 2, Revision: 1, Lines: []HeldLine{line("S1", "L1", "40")},
			Releases: []HeldRelease{{ID: "rls.1s", Scope: ScopeShipment, Status: "consumed", RowVersion: 2, Evaluations: 1},
				{ID: "rls.1d", Scope: ScopeDocuments, Status: ReleaseGranted, RowVersion: 2, Evaluations: 2}}},
	}}
}

func kinds(items []Item) string {
	var out []string
	for i, it := range items {
		if it.No != i+1 {
			return "misnumbered"
		}
		s := it.Facts.Kind + ":" + it.Facts.ShipmentID
		switch it.Facts.Kind {
		case ItemPacking:
			s += "/" + it.Facts.PackingKind
		case ItemDocument:
			s += "/" + it.Facts.DocumentKind
		case ItemRelease:
			s += "/" + it.Facts.ReleaseScope
		}
		out = append(out, s)
	}
	return strings.Join(out, " ")
}

func factLine(f FactLine) string {
	return strings.Join([]string{f.Impact, f.Quantity, f.TargetQuantity, f.ShippedQuantity, f.RemainingQuantity}, " ")
}

// BT10 ("部分已出运后减量…已发生部分保留"): reducing L1 to 110 at revision 2 leaves stale what binds revision
// 1 and can still change — shipment.2 (80 of the 70 left: reduced), its packing plan and actual
// packing, its booking, its drafts by kind, its granted shipment release (the blocked documents
// release is not an item) — and the granted documents release of the confirmed shipment.1, which
// itself stays a fact. shipment.3 binds revision 2: not stale.
func TestAssessAReductionBT10(t *testing.T) {
	items := Assess(amended(2, 5, target("L1", "110")), bt10())
	want := "release:shipment.1/documents shipment:shipment.2 packing:shipment.2/plan packing:shipment.2/actual booking:shipment.2 " +
		"document:shipment.2/commercial_invoice document:shipment.2/packing_list release:shipment.2/shipment"
	if got := kinds(items); got != want {
		t.Fatalf("items\n%s\nwant\n%s", got, want)
	}
	s := items[1].Facts
	if len(s.Lines) != 1 || factLine(s.Lines[0]) != "reduced 80 110 40 70" || s.ShipmentVersion != 3 || s.ShipmentRevision != 1 ||
		s.Impact != ImpactRevisionChanged || s.ShipmentStatus != ShipmentPrepared {
		t.Fatalf("shipment item %+v", s)
	}
	if f := items[0].Facts; f.ShipmentStatus != ShipmentConfirmed || f.ReleaseID != "rls.1d" || f.ReleaseEvaluations != 2 || f.Lines != nil {
		t.Fatalf("release item of the confirmed shipment %+v", f)
	}
	if f := items[4].Facts; f.BookingVersion != 2 || f.BookingNumber != "BK1" || f.ETD != "2026-12-20" {
		t.Fatalf("booking item %+v", f)
	}
	if f := items[5].Facts; f.DocumentSetID != "docs.2" || f.DocumentVersion != 2 || f.DocumentNumber != "CI-1" {
		t.Fatalf("document item %+v", f)
	}
	doc := items[1].Facts.JSON()
	if back, err := ParseFacts(doc); err != nil || back.JSON() != doc || !strings.Contains(doc, `"remaining_quantity":"70"`) {
		t.Fatalf("facts round trip %v %s", err, doc)
	}
}

// BT10 "改色" and a removed line: a line the amended revision states in another SKU specification or
// unit is specification_changed, a line it no longer has is line_removed; a shipment whose quantity
// still fits is unchanged — and stale all the same, since it binds the earlier revision.
func TestAssessAColourChangeAndARemovedLine(t *testing.T) {
	h := Holdings{Rooted: true, Shipped: map[LineUnit]kernel.Decimal{}, Shipments: []HeldShipment{{ID: "shipment.2", Status: ShipmentPrepared,
		RowVersion: 1, Revision: 1, Lines: []HeldLine{line("S1", "L1", "50"), line("S2", "L2", "20"), line("S3", "L3", "5")}}}}
	blue := target("L2", "20")
	blue.SkuID, blue.SpecRevision = "sku.blue", 1
	items := Assess(amended(2, 4, target("L1", "120"), blue), h)
	if kinds(items) != "shipment:shipment.2" {
		t.Fatalf("items %s", kinds(items))
	}
	var got []string
	for _, l := range items[0].Facts.Lines {
		got = append(got, l.LineID+" "+factLine(l))
	}
	if strings.Join(got, "|") != "S1 unchanged 50 120 0 120|S2 specification_changed 20 20 0 20|S3 line_removed 5  0 0" {
		t.Fatalf("lines %s", strings.Join(got, "|"))
	}
}

// After an order's end every prepared shipment is stale, whatever revision it binds, and so is every
// granted release; nothing is left of any line.
func TestAssessAnEnd(t *testing.T) {
	for _, cause := range []string{CauseOrderCancelled, CauseOrderRemainderClosed} {
		c := Change{OrderID: "order.1", Revision: 2, Hash: hash2, Version: 6, Cause: cause, EventID: "evt.end"}
		items := Assess(c, bt10())
		want := "release:shipment.1/documents shipment:shipment.2 packing:shipment.2/plan packing:shipment.2/actual booking:shipment.2 " +
			"document:shipment.2/commercial_invoice document:shipment.2/packing_list release:shipment.2/shipment shipment:shipment.3"
		impact := map[string]string{CauseOrderCancelled: ImpactOrderCancelled, CauseOrderRemainderClosed: ImpactRemainderClosed}[cause]
		if kinds(items) != want || items[8].Facts.Impact != impact || factLine(items[8].Facts.Lines[0]) != impact+" 10  40 0" {
			t.Fatalf("%s: %s %+v", cause, kinds(items), items[8].Facts)
		}
	}
}

func open(id string, revision, version int64, cause string) *Review {
	return &Review{ID: id, OrderID: "order.1", Revision: revision, Version: version, Cause: cause, Status: ReviewOpen, RowVersion: 1}
}

// WF-06's idempotency: one review per (order, revision, cause) — a redelivery, or one settled since,
// opens nothing; an older event than a review opens nothing; FUL opens nothing for an order it never
// prepared a shipment of, nor when nothing is stale and no review is open; a later change supersedes
// the open review, even with no item of its own. At revision 3, shipment.3 (bound to revision 2) is
// stale too: nine items.
func TestPlanOpensOncePerRevisionAndSupersedes(t *testing.T) {
	c := amended(3, 7, target("L1", "100"))
	h := bt10()
	items := Assess(c, h)
	a := open("shr.a", 2, 5, CauseOrderAmended)
	settled := open("shr.s", 3, 7, CauseOrderAmended)
	settled.Status = ReviewSettled
	later := open("shr.l", 4, 8, CauseOrderAmended)
	for _, x := range []struct {
		what    string
		reviews []*Review
		h       Holdings
		items   []Item
		want    Effect
		gone    int
	}{
		{"redelivery of a settled revision", []*Review{a, settled}, h, items, EffectAlreadyOpen, 0},
		{"older than a review", []*Review{later}, h, items, EffectLater, 0},
		{"no fulfilment root", nil, Holdings{}, nil, EffectNothingHeld, 0},
		{"nothing stale", nil, Holdings{Rooted: true}, nil, EffectNothingStale, 0},
		{"stale objects", nil, h, items, EffectOpened, 0},
		{"supersede with items", []*Review{a}, h, items, EffectOpened, 1},
		{"supersede without items", []*Review{a}, Holdings{Rooted: true}, nil, EffectOpened, 1},
	} {
		got, gone := Plan(c, x.reviews, x.h, x.items)
		if got != x.want || len(gone) != x.gone {
			t.Fatalf("%s: %s, superseding %d", x.what, got, len(gone))
		}
	}
	now := time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	r := OpenReview("shr.b", c, items, Act{By: kernel.ActorRef{Kind: kernel.ActorSystem, ID: "inbox.ful.shipment_review"}, Epoch: 3, At: now,
		Reason: "dropped"})
	a.Supersede(r.ID, now)
	if r.Status != ReviewOpen || r.RowVersion != 1 || r.Role != ResponsibleRole || r.Opened.Reason != "" || len(r.Items) != 9 ||
		a.Status != ReviewSuperseded || a.RowVersion != 2 || a.SupersededBy != "shr.b" {
		t.Fatalf("opened %+v superseded %+v", r, a)
	}
}

// now is what FUL shows after the merchandiser cancelled shipment.2 and re-planned the 70 left as
// shipment.4 at revision 2 (packing plan, booking, commercial invoice, shipment release), and the
// documentation clerk evaluated shipment.1's documents release again.
func reviewNow() Now {
	cancelled := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	return Now{Shipments: map[string]ShipmentNow{
		"shipment.1": {OrderID: "order.1", Status: ShipmentConfirmed, Revision: 1, Releases: map[string]ReleaseNow{
			ScopeDocuments: {ID: "rls.1d", Status: "blocked", Evaluations: 3, EvaluatedAt: cancelled.Add(time.Hour)}}},
		"shipment.2": {OrderID: "order.1", Status: ShipmentCancelled, Revision: 1, CancelledAt: cancelled},
		"shipment.3": {OrderID: "order.1", Status: ShipmentPrepared, Revision: 2},
		"shipment.4": {OrderID: "order.1", Status: ShipmentPrepared, Revision: 2, Packing: map[string]int64{PackingPlan: 1}, Booking: 1,
			Documents: map[string]int64{"commercial_invoice": 1}, Releases: map[string]ReleaseNow{ScopeShipment: {ID: "rls.4s", Status: ReleaseGranted,
				Evaluations: 1}}},
		"shipment.x": {OrderID: "order.2", Status: ShipmentPrepared, Revision: 5},
	}}
}

func settle(kind string, shipment string) Settlement {
	return Settlement{Kind: kind, ShipmentID: shipment, Reason: "合成：处置"}
}

// SM-FUL-SHIPMENT-REVIEW open → settled ("装箱、订舱与未签发单证逐项处置；已确认出运保留"): every item with what was done
// — the release of the confirmed shipment evaluated again, the stale shipment and what hangs on it
// replaced by shipment.4 of revision 2 holding each counterpart, or cancelled with it — traced in the
// disposition; refusals in order.
func TestSettleByWhatWasDone(t *testing.T) {
	c := amended(2, 5, target("L1", "110"))
	review := func() *Review {
		// shipment.1's documents release and shipment.2's seven items
		return OpenReview("shr.1", c, Assess(c, bt10()), Act{By: kernel.ActorRef{Kind: kernel.ActorSystem, ID: "inbox"}, Epoch: 1})
	}
	good := []Settlement{settle(DispositionReevaluated, ""), settle(DispositionReplaced, "shipment.4"), settle(DispositionReplaced, "shipment.4"),
		settle(DispositionShipmentCancelled, ""), settle(DispositionReplaced, "shipment.4"), settle(DispositionReplaced, "shipment.4"),
		settle(DispositionShipmentCancelled, ""), settle(DispositionReplaced, "shipment.4")}
	for i := range good {
		good[i].ItemNo = i + 1
	}
	with := func(i int, s Settlement) []Settlement {
		out := append([]Settlement(nil), good...)
		s.ItemNo = i + 1
		out[i] = s
		return out
	}
	human := Act{By: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "merch.1"}, Epoch: 9, At: time.Now(), Reason: "合成"}
	superseded := review()
	superseded.Supersede("shr.2", time.Now())
	settledOnce := review()
	if err := settledOnce.Settle(1, good, reviewNow(), human); err != nil {
		t.Fatal(err)
	}
	for _, x := range []struct {
		what     string
		r        *Review
		expected int64
		ss       []Settlement
		code     string
		path     string
	}{
		{"another version", review(), 2, good, kernel.CodeVersionConflict, "/args/expected_version"},
		{"a superseded review", superseded, 2, good, codeReferenceStale, "/args/review_id"},
		{"a settled review", settledOnce, 2, good, kernel.CodeInvalidState, "/args/review_id"},
		{"no such item", review(), 1, append(good, Settlement{ItemNo: 9, Kind: DispositionNoAction}), kernel.CodeReferenceInvalid, "/args/items/8/item_no"},
		{"an item left out", review(), 1, good[1:], kernel.CodeInvalidKnownValue, "/args/items"},
		{"no_action on a prepared shipment", review(), 1, with(1, settle(DispositionNoAction, "")), kernel.CodeInvalidKnownValue, "/args/items/1/disposition"},
		{"a confirmed shipment cancelled", review(), 1, with(0, settle(DispositionShipmentCancelled, "")), kernel.CodeInvalidKnownValue, "/args/items/0/disposition"},
		{"a confirmed shipment replaced", review(), 1, with(0, settle(DispositionReplaced, "shipment.4")), kernel.CodeInvalidKnownValue, "/args/items/0/disposition"},
		{"a packing evaluated again", review(), 1, with(2, settle(DispositionReevaluated, "")), kernel.CodeInvalidKnownValue, "/args/items/2/disposition"},
		{"replaced by itself", review(), 1, with(1, settle(DispositionReplaced, "shipment.2")), kernel.CodeInvalidKnownValue, "/args/items/1/shipment_id"},
		{"replaced by another order's", review(), 1, with(1, settle(DispositionReplaced, "shipment.x")), kernel.CodeReferenceInvalid, "/args/items/1/shipment_id"},
		{"replaced by an unknown one", review(), 1, with(1, settle(DispositionReplaced, "shipment.9")), kernel.CodeReferenceInvalid, "/args/items/1/shipment_id"},
		{"replaced by revision 1", review(), 1, with(1, settle(DispositionReplaced, "shipment.1")), kernel.CodeInvalidState, "/args/items/1/shipment_id"},
		{"a replacement without the actual packing", review(), 1, with(3, settle(DispositionReplaced, "shipment.4")), kernel.CodeInvalidState, "/args/items/3/shipment_id"},
		{"a replacement without a packing list", review(), 1, with(6, settle(DispositionReplaced, "shipment.4")), kernel.CodeInvalidState, "/args/items/6/shipment_id"},
		{"a replacement without a booking", review(), 1, with(4, settle(DispositionReplaced, "shipment.3")), kernel.CodeInvalidState, "/args/items/4/shipment_id"},
	} {
		err := x.r.Settle(x.expected, x.ss, reviewNow(), human)
		rej, ok := kernel.AsRejection(err)
		if !ok || rej.Code != x.code || len(rej.Fields) == 0 || rej.Fields[0].Path != x.path {
			t.Fatalf("%s: %v, want %s at %s", x.what, err, x.code, x.path)
		}
	}
	notYet := reviewNow()
	s2 := notYet.Shipments["shipment.2"]
	s2.Status, s2.CancelledAt = ShipmentPrepared, time.Time{}
	notYet.Shipments["shipment.2"] = s2
	r1 := notYet.Shipments["shipment.1"]
	r1.Releases = map[string]ReleaseNow{ScopeDocuments: {ID: "rls.1d", Status: ReleaseGranted, Evaluations: 2}}
	notYet.Shipments["shipment.1"] = r1
	cancelledWith := with(1, settle(DispositionShipmentCancelled, ""))
	cancelledWith[0] = Settlement{ItemNo: 1, Kind: DispositionNoAction, Reason: "x"}
	for _, x := range []struct {
		what string
		ss   []Settlement
		path string
	}{
		{"a release not evaluated again", good, "/args/items/0/disposition"},
		{"a shipment not cancelled", cancelledWith, "/args/items/1/disposition"},
		{"a replaced shipment not cancelled", append([]Settlement{{ItemNo: 1, Kind: DispositionNoAction, Reason: "x"}}, good[1:]...), "/args/items/1/disposition"},
	} {
		err := review().Settle(1, x.ss, notYet, human)
		if rej, ok := kernel.AsRejection(err); !ok || rej.Code != kernel.CodeInvalidState || rej.Fields[0].Path != x.path {
			t.Fatalf("%s: %v, want INVALID_STATE at %s", x.what, err, x.path)
		}
	}

	r := settledOnce
	var got []string
	for _, it := range r.Items {
		d := it.Done
		got = append(got, strings.Join([]string{d.Kind, d.ShipmentID, kernel.DecimalFromInt(d.Revision).String(), d.Ref, d.Status}, ","))
	}
	want := "reevaluated,,0,rls.1d,blocked replaced,shipment.4,2,,prepared replaced,shipment.4,2,plan/1,prepared shipment_cancelled,,0,, " +
		"replaced,shipment.4,2,booking/1,prepared replaced,shipment.4,2,commercial_invoice/1,prepared shipment_cancelled,,0,, " +
		"replaced,shipment.4,2,rls.4s,granted"
	if strings.Join(got, " ") != want || r.Status != ReviewSettled || r.RowVersion != 2 || r.Settled.By.ID != "merch.1" ||
		r.Items[1].Done.At.IsZero() || r.Items[0].Done.At.IsZero() || r.Verify() != nil {
		t.Fatalf("settled\n%s\nwant\n%s\n%+v", strings.Join(got, " "), want, r)
	}
	// A confirmed shipment's release may also be left as it is: it stays, and nothing consumes it.
	keep := review()
	if err := keep.Settle(1, with(0, settle(DispositionNoAction, "")), reviewNow(), human); err != nil || keep.Items[0].Done.Kind != DispositionNoAction {
		t.Fatalf("no_action on the confirmed shipment's release: %v", err)
	}
	// An end's review has no later revision to re-plan on.
	ended := Change{OrderID: "order.1", Revision: 2, Hash: hash2, Version: 6, Cause: CauseOrderCancelled, EventID: "evt.end"}
	end := OpenReview("shr.end", ended, Assess(ended, Holdings{Rooted: true, Shipments: bt10().Shipments[1:2]}), Act{})
	var ss []Settlement
	for _, it := range end.Items {
		ss = append(ss, Settlement{ItemNo: it.No, Kind: DispositionShipmentCancelled, Reason: "x"})
	}
	ss[0] = Settlement{ItemNo: 1, Kind: DispositionReplaced, ShipmentID: "shipment.4", Reason: "x"}
	err := end.Settle(1, ss, reviewNow(), human)
	if rej, ok := kernel.AsRejection(err); len(end.Items) != 7 || !ok || rej.Code != kernel.CodeInvalidKnownValue || rej.Fields[0].Path != "/args/items/0/disposition" {
		t.Fatalf("re-planning on an ended order: %d items, %v", len(end.Items), err)
	}
	ss[0].Kind, ss[0].ShipmentID = DispositionShipmentCancelled, ""
	if err := end.Settle(1, ss, reviewNow(), human); err != nil {
		t.Fatalf("an ended order's shipment cancelled: %v", err)
	}
	// A record whose dispositions do not match its state is not intact.
	r.Items[2].Done = nil
	if err := r.Verify(); err == nil {
		t.Fatal("a settled review with an item not disposed of")
	}
}
