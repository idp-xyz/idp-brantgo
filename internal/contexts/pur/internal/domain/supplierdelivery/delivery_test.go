package supplierdelivery

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The rules of PUR-AGG-SupplierDelivery (SM-PUR-SUPPLIER-DELIVERY, PUR-INV-008, UL-INV-32; WBS S5-15)
// on a synthetic commitment: commitment.1 of order.1 from supplier.1 buys L1 (100, of which 10
// overbuy) and L2 (50). Python V0.50 has no supplier delivery: the tests cite the design IDs.

var (
	now      = time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	recorder = kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent"}
	verifier = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "purchaser"}
	other    = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "merchandiser"}
	readable = map[string]bool{"receipt": true, "photos": true}
)

func code(err error) string {
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code + " " + r.Fields[0].Path
	}
	if err != nil {
		return err.Error()
	}
	return "OK"
}

func q(t *testing.T, s string) kernel.Decimal {
	t.Helper()
	d, err := kernel.ParseDecimal(s, kernel.QuantityFormat)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func committed(t *testing.T) Commitment {
	t.Helper()
	return Commitment{ID: "commitment.1", Status: "issued", Committed: true, OrderID: "order.1", SupplierID: "supplier.1", OwnerActor: "buyer",
		OwnerTeam: "trade", Lines: []CommitmentLine{
			{OrderLineID: "L1", SkuID: "sku.1", SpecRevision: 2, UnitID: "pcs", UnitRevision: 1, Purchased: q(t, "100")},
			{OrderLineID: "L2", SkuID: "sku.2", SpecRevision: 1, UnitID: "pcs", UnitRevision: 1, Purchased: q(t, "50")},
		}}
}

func claimOf(t *testing.T, l1, l2 string) Request {
	t.Helper()
	r := Request{DeliveryID: "delivery.1", CommitmentID: "commitment.1", HandoverPlace: "CNNGB", HandedOverAt: now.Add(-time.Hour),
		Evidence: []Evidence{{SourceID: "receipt", Locator: "p1"}}, RequestHash: "hash.1"}
	for _, l := range []struct{ id, q string }{{"L1", l1}, {"L2", l2}} {
		if l.q != "" {
			r.Lines = append(r.Lines, RequestLine{OrderLineID: l.id, Quantity: q(t, l.q)})
		}
	}
	return r
}

func inputs(t *testing.T, by kernel.ActorRef) Inputs {
	return Inputs{Commitment: committed(t), Readable: readable, Recorder: by, Epoch: 3, Now: now}
}

func recorded(t *testing.T, l1, l2 string) *Delivery {
	t.Helper()
	d, err := Record(claimOf(t, l1, l2), inputs(t, recorder))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func verification(t *testing.T, elsewhere map[string]kernel.Decimal, lines ...string) Verification {
	t.Helper()
	x := Verification{Commitment: committed(t), Elsewhere: elsewhere, Readable: readable}
	for i := 0; i+1 < len(lines); i += 2 {
		x.Lines = append(x.Lines, Quantity{OrderLineID: lines[i], Verified: q(t, lines[i+1])})
	}
	return x
}

func act(by kernel.ActorRef) Act { return Act{By: by, Epoch: 4, At: now.Add(time.Minute)} }

func verified(t *testing.T, l1 string) *Delivery {
	t.Helper()
	d := recorded(t, l1, "")
	if err := d.Verify(verification(t, nil, "L1", l1), act(verifier)); err != nil {
		t.Fatal(err)
	}
	return d
}

// ES-12 step 1, UL-INV-32: a human or an AI records the supplier's claim against lines of an issued
// or confirmed commitment; the delivery is owned as its commitment is, its lines carry what the
// commitment line buys, and nothing of it counts yet. A taken ID answers the same request only.
func TestRecordIsAClaimAgainstCommittedLines(t *testing.T) {
	d := recorded(t, "60", "5")
	v := d.Versions[0]
	if d.RowVersion != 1 || d.LatestVersion != 1 || d.VerifiedVersion != 0 || d.Status() != StatusRecorded || d.Current() != nil ||
		d.Pending() != v || d.Fresh() != v || d.OwnerActor != "buyer" || d.OwnerTeam != "trade" || d.OrderID != "order.1" ||
		d.SupplierID != "supplier.1" || v.Recorded.By != recorder || v.Files == nil || len(v.Lines) != 2 {
		t.Fatalf("delivery %+v version %+v", d, v)
	}
	if l, ok := v.Line("L1"); !ok || l.SkuID != "sku.1" || l.SpecRevision != 2 || l.UnitID != "pcs" || l.Decided || l.Claimed.Cmp(q(t, "60")) != 0 {
		t.Fatalf("line %+v", l)
	}
	if len(Sum([]*Delivery{d}, "")) != 0 {
		t.Fatal("a claim counts")
	}
	if d.Answer("hash.1", recorder) != nil || code(d.Answer("hash.2", recorder)) != "VERSION_CONFLICT /args/delivery_id" ||
		code(d.Answer("hash.1", other)) != "VERSION_CONFLICT /args/delivery_id" {
		t.Fatal("a taken ID is answered for the same request by the same recorder only")
	}

	// A claim is not capped: it never counts.
	if _, err := Record(claimOf(t, "1000", ""), inputs(t, recorder)); err != nil {
		t.Fatal(err)
	}
	for want, edit := range map[string]func(r *Request, in *Inputs){
		"INVALID_STATE /args/commitment_id": func(_ *Request, in *Inputs) { in.Commitment.Status, in.Commitment.Committed = "approved", false },
		"REFERENCE_INVALID /args/lines/1/order_line_id": func(r *Request, _ *Inputs) {
			r.Lines = append(r.Lines, RequestLine{OrderLineID: "L9", Quantity: r.Lines[0].Quantity})
		},
		"INVALID_KNOWN_VALUE /args/handed_over_at":     func(r *Request, _ *Inputs) { r.HandedOverAt = now.Add(time.Second) },
		"REFERENCE_INVALID /args/evidence/0/source_id": func(r *Request, _ *Inputs) { r.Evidence[0].SourceID = "secret" },
	} {
		r, in := claimOf(t, "10", ""), inputs(t, recorder)
		edit(&r, &in)
		if _, err := Record(r, in); code(err) != want {
			t.Errorf("%s: %s", want, code(err))
		}
	}
}

// SM-PUR-SUPPLIER-DELIVERY, role templates (supplier_delivery.verify independentOf 交付的登记人): a
// claim is decided once, at the delivery's row version, by someone who did not record it.
func TestVerificationIsIndependentAndOnce(t *testing.T) {
	d := recorded(t, "10", "")
	for _, c := range []struct {
		expected int64
		by       kernel.ActorRef
		want     string
	}{
		{2, verifier, "VERSION_CONFLICT /args/expected_version"},
		{1, kernel.ActorRef{Kind: kernel.ActorHuman, ID: recorder.ID}, "INDEPENDENT_REVIEW_REQUIRED /args/delivery_id"},
		{1, verifier, "OK"},
	} {
		if got := code(d.Decidable(c.expected, c.by)); got != c.want {
			t.Errorf("%d %s: %s, want %s", c.expected, c.by.ID, got, c.want)
		}
	}
	if err := d.Verify(verification(t, nil, "L1", "10"), act(verifier)); err != nil {
		t.Fatal(err)
	}
	if code(d.Decidable(2, other)) != "INVALID_STATE /args/delivery_id" {
		t.Fatal("a verified claim is decided again")
	}

	r := recorded(t, "10", "")
	r.Reject(Act{By: verifier, Epoch: 4, At: now, Reason: "The receipt shows another container"})
	if r.Status() != StatusRejected || r.RowVersion != 2 || r.VerifiedVersion != 0 || r.Decided() != r.Versions[0] ||
		r.Versions[0].Decision.Verify || r.Versions[0].Decision.Evidence == nil || code(r.Decidable(2, other)) != "INVALID_STATE /args/delivery_id" {
		t.Fatalf("rejected %+v", r.Versions[0])
	}
	if Code(StatusRejected) != CodeSupplierDeliveryUnverified || Code(StatusRecorded) != CodeSupplierDeliveryUnverified || Code("") != CodeSupplierDeliveryUnverified {
		t.Fatal("a claim serves shipments")
	}
}

// PUR-INV-008: a verification states every line of the claim once, never above what was claimed,
// and something above zero; the evidence is what is verified, so the verifier reads it; over all
// its verified deliveries a commitment line never exceeds what it buys (no tolerance in S5).
func TestVerifyStatesEveryLineWithinTheClaimAndTheCommitment(t *testing.T) {
	ten := map[string]kernel.Decimal{"L1": q(t, "40")}
	for _, c := range []struct {
		name string
		x    func() Verification
		want string
	}{
		{"unstated", func() Verification { return verification(t, nil, "L1", "60") }, "REFERENCE_INVALID /args/lines"},
		{"unknown", func() Verification { return verification(t, nil, "L1", "60", "L3", "1") }, "REFERENCE_INVALID /args/lines/1/order_line_id"},
		{"twice", func() Verification { return verification(t, nil, "L1", "60", "L1", "1") }, "REFERENCE_INVALID /args/lines/1/order_line_id"},
		{"above claim", func() Verification { return verification(t, nil, "L2", "5", "L1", "60.6") }, "INVALID_KNOWN_VALUE /args/lines/1/verified_quantity"},
		{"nothing", func() Verification { return verification(t, nil, "L1", "0", "L2", "0") }, "INVALID_KNOWN_VALUE /args/lines"},
		{"not committed", func() Verification {
			x := verification(t, nil, "L1", "60", "L2", "5")
			x.Commitment.Status, x.Commitment.Committed = "requested", false
			return x
		}, "INVALID_STATE /args/delivery_id"},
		{"own evidence unread", func() Verification {
			x := verification(t, nil, "L1", "60", "L2", "5")
			x.Evidence = []Evidence{{SourceID: "secret", Locator: "x"}}
			return x
		}, "REFERENCE_INVALID /args/evidence/0/source_id"},
		{"claim evidence unread", func() Verification {
			x := verification(t, nil, "L1", "60", "L2", "5")
			x.Readable = map[string]bool{"photos": true}
			return x
		}, "REFERENCE_INVALID /args/delivery_id"},
		{"line gone", func() Verification {
			x := verification(t, nil, "L1", "60", "L2", "5")
			x.Commitment.Lines = x.Commitment.Lines[:1]
			return x
		}, "REFERENCE_INVALID /args/delivery_id"},
		{"exceeded", func() Verification { return verification(t, ten, "L2", "5", "L1", "60.001") }, "SUPPLIER_DELIVERY_EXCEEDED /args/lines/1/verified_quantity"},
		{"exactly", func() Verification { return verification(t, ten, "L2", "5", "L1", "60") }, "OK"},
	} {
		d := recorded(t, "60.5", "5")
		err := d.Verify(c.x(), act(verifier))
		if got := code(err); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
			continue
		}
		if err != nil {
			if d.Status() != StatusRecorded || d.RowVersion != 1 || d.Decided() != nil {
				t.Errorf("%s: a refused verification changed the delivery", c.name)
			}
			continue
		}
		v := d.Current()
		if d.Status() != StatusVerified || d.VerifiedVersion != 1 || d.RowVersion != 2 || d.Pending() != nil || d.Decided() != v ||
			d.Superseded() != nil || !v.Decision.Verify || v.Decision.By != verifier || v.Decision.Evidence == nil {
			t.Fatalf("verified %+v", v)
		}
		if sum := Sum([]*Delivery{d}, ""); sum["L1"].Cmp(q(t, "60")) != 0 || sum["L2"].Cmp(q(t, "5")) != 0 || len(Sum([]*Delivery{d}, d.ID)) != 0 {
			t.Fatalf("sum %v", sum)
		}
	}
}

// Corrections supersede (SM-PUR-SUPPLIER-DELIVERY verified → superseded): a correction is the next
// version of the delivery, a claim again, recorded once no claim awaits verification, against the
// same commitment; until it is verified the version verified before is the one that counts; its
// verification supersedes that version and may bring it down to nothing.
func TestCorrectionsSupersede(t *testing.T) {
	d := verified(t, "60")
	correction := claimOf(t, "40", "")
	correction.Reason, correction.RequestHash = "The receipt counted 40", "hash.2"
	for want, edit := range map[string]func(r *Request) (int64, kernel.ActorRef){
		"VERSION_CONFLICT /args/expected_version": func(*Request) (int64, kernel.ActorRef) { return 1, recorder },
		"REFERENCE_INVALID /args/commitment_id":   func(r *Request) (int64, kernel.ActorRef) { r.CommitmentID = "commitment.2"; return 2, recorder },
	} {
		r := correction
		expected, by := edit(&r)
		if err := d.Correct(expected, r, inputs(t, by)); code(err) != want {
			t.Errorf("%s: %s", want, code(err))
		}
	}
	if err := d.Correct(2, correction, inputs(t, other)); err != nil {
		t.Fatal(err)
	}
	v2 := d.Versions[1]
	if d.LatestVersion != 2 || d.RowVersion != 3 || d.VerifiedVersion != 1 || d.Status() != StatusVerified || d.Pending() != v2 ||
		d.Fresh() != v2 || v2.Number != 2 || v2.CorrectionReason == "" || Code(d.Versions[0].Status) != "" {
		t.Fatalf("corrected %+v", d)
	}
	if code(d.Correct(3, correction, inputs(t, other))) != "INVALID_STATE /args/delivery_id" {
		t.Fatal("two claims await verification")
	}
	if code(d.Decidable(3, other)) != "INDEPENDENT_REVIEW_REQUIRED /args/delivery_id" {
		t.Fatal("the recorder of the correction verifies it")
	}
	// The correction replaces the version it supersedes: with 60 of L1 verified by other deliveries,
	// 40 fills the line (100) and is not added to this delivery's 60.
	if err := d.Verify(verification(t, map[string]kernel.Decimal{"L1": q(t, "60")}, "L1", "40"), act(verifier)); err != nil {
		t.Fatal(err)
	}
	v1 := d.Versions[0]
	if d.VerifiedVersion != 2 || d.RowVersion != 4 || v1.Status != StatusSuperseded || v1.SupersededBy != 2 || d.Superseded() != v1 ||
		Code(v1.Status) != CodeReferenceStale || Code(v2.Status) != "" || Sum([]*Delivery{d}, "")["L1"].Cmp(q(t, "40")) != 0 {
		t.Fatalf("superseded %+v, current %+v", v1, v2)
	}

	// A correction may withdraw a wrongly verified delivery: zero on every line.
	w := verified(t, "60")
	withdraw := claimOf(t, "60", "")
	withdraw.Reason = "Another supplier's goods"
	if err := w.Correct(2, withdraw, inputs(t, other)); err != nil {
		t.Fatal(err)
	}
	if err := w.Verify(verification(t, nil, "L1", "0"), act(verifier)); err != nil {
		t.Fatal(err)
	}
	if w.VerifiedVersion != 2 || Sum([]*Delivery{w}, "")["L1"].Sign() != 0 {
		t.Fatalf("withdrawn %+v", w)
	}

	// A rejected correction leaves the version verified before the one that counts.
	r := verified(t, "60")
	if err := r.Correct(2, withdraw, inputs(t, other)); err != nil {
		t.Fatal(err)
	}
	r.Reject(Act{By: verifier, Epoch: 4, At: now, Reason: "No evidence of a miscount"})
	if r.VerifiedVersion != 1 || r.Status() != StatusVerified || r.Versions[1].Status != StatusRejected || r.Superseded() != nil {
		t.Fatalf("rejected correction %+v", r)
	}
}

// The tables of codes and categories are the error catalog's (SUPPLIER_DELIVERY_EXCEEDED and
// SUPPLIER_DELIVERY_UNVERIFIED are conflicts).
func TestCodes(t *testing.T) {
	for code, want := range map[string]kernel.Category{CodeSupplierDeliveryExceeded: kernel.CategoryConflict,
		CodeSupplierDeliveryUnverified: kernel.CategoryConflict, CodeReferenceStale: kernel.CategoryConflict,
		CodeIndependentReviewRequired: kernel.CategoryInvalid} {
		if r := fail(code, "/p", ""); Codes[code] != want || r.Code != code || r.Category != want {
			t.Errorf("%s: %s", code, r.Category)
		}
	}
}
