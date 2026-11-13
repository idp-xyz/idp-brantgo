package release

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/qlt/internal/domain/inspection"
	"go.idp.xyz/idp-brant/internal/kernel"
)

var (
	now     = time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	qc      = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "qc.1"}
	ref     = inspection.SubjectRef{Kind: "purchase_commitment_line", ID: "commitment.1", LineID: "L1"}
	subject = inspection.Subject{Ref: ref, Revision: 1, Hash: "h", SupplierID: "supplier.1", OrderID: "order.1", OrderRevision: 1,
		OrderHash: "o", OrderLineID: "OL1", SkuID: "sku.1", SpecRevision: 2, UnitID: "pcs", UnitRevision: 1, Quantity: kernel.MustDecimal("200")}
)

func q(s string) kernel.Decimal { return kernel.MustDecimal(s) }

func outcome(err error) string {
	if err == nil {
		return "OK"
	}
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code + " " + r.Fields[0].Path
	}
	return err.Error()
}

func inspected(id, status, conforming string) *inspection.Inspection {
	return &inspection.Inspection{ID: id, Status: status, Subject: subject, Inspected: q(conforming), Conforming: q(conforming),
		Recorded: inspection.Act{By: qc, At: now.Add(-time.Hour)}}
}

func grant(id, quantity string) Request {
	return Request{ID: id, Subject: ref, Quantity: q(quantity), ValidUntil: now.Add(30 * 24 * time.Hour), RequestHash: "rh"}
}

func inputs(is []*inspection.Inspection, rs ...*Release) Inputs {
	s := subject
	return Inputs{Subject: &s, Inspections: is, Releases: rs, Sources: map[string]string{"ev": "x"}, Granter: qc, Epoch: 4, Team: "qc", Now: now}
}

// QLT-INV-001 positive: after an external agency's 120 conforming our QC releases 120; the release
// binds the subject, specification, validity and the quantities it was judged on.
func TestGrantWithinConforming(t *testing.T) {
	is := []*inspection.Inspection{inspected("insp.1", inspection.StatusRecorded, "120")}
	r, err := Grant(grant("rel.1", "120"), inputs(is))
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusGranted || r.RowVersion != 1 || r.Subject != subject || !r.Quantity.Equal(q("120")) || !r.ValidFrom.Equal(now) ||
		!r.Conforming.Equal(q("120")) || !r.ReleasedBefore.IsZero() || len(r.InspectionIDs) != 1 || r.InspectionIDs[0] != "insp.1" ||
		!r.Conceded.IsZero() || !r.ConcessionQuantity.IsZero() || r.Concession() ||
		r.Granted != (inspection.Act{By: qc, Epoch: 4, At: now}) || r.OwnerActor != "qc.1" || r.State(now) != StatusGranted || r.Code(now) != "" {
		t.Fatalf("%+v", r)
	}
}

// QLT-INV-001 negatives: the conforming quantity of valid inspections at the subject's
// specification less what was released before, a revoked release's frozen quantity included until
// it is inspected again, bounds every release.
func TestGrantRefusals(t *testing.T) {
	valid := inspected("insp.2", inspection.StatusDisposed, "150")
	superseded := inspected("insp.1", inspection.StatusSuperseded, "200")
	otherSpec := inspected("insp.3", inspection.StatusRecorded, "50")
	otherSpec.Subject.SpecRevision = 1
	revoked := &Release{ID: "rel.0", Status: StatusRevoked, Subject: subject, Quantity: q("100")}
	is := []*inspection.Inspection{superseded, valid, otherSpec}
	for _, c := range []struct {
		name string
		req  Request
		in   func(*Inputs)
		want string
	}{
		{"exactly the rest", grant("rel.1", "50"), nil, "OK"},
		{"above the rest", grant("rel.1", "50.000001"), nil, "QUALITY_RELEASE_EXCEEDS_CONFORMING /args/quantity"},
		{"no subject", grant("rel.1", "1"), func(in *Inputs) { in.Subject = nil }, "REFERENCE_INVALID /args/subject"},
		{"subject cancelled", grant("rel.1", "1"), func(in *Inputs) { in.SubjectCode = "cancelled" }, "REFERENCE_STALE /args/subject"},
		{"ended", Request{ID: "rel.1", Subject: ref, Quantity: q("1"), ValidUntil: now}, nil, "INVALID_KNOWN_VALUE /args/valid_until"},
		{"evidence not read", Request{ID: "rel.1", Subject: ref, Quantity: q("1"), ValidUntil: now.Add(time.Hour), Evidence: []string{"ev", "x"}}, nil,
			"REFERENCE_INVALID /args/evidence_ids/1"},
		{"a correction lowered conforming below what was released", grant("rel.1", "1"), func(in *Inputs) {
			in.Releases = append(in.Releases, &Release{ID: "rel.9", Status: StatusGranted, Subject: subject, Quantity: q("60")})
		}, "QUALITY_RELEASE_EXCEEDS_CONFORMING /args/quantity"},
	} {
		in := inputs(is, revoked)
		if c.in != nil {
			c.in(&in)
		}
		r, err := Grant(c.req, in)
		if outcome(err) != c.want {
			t.Errorf("%s: %s, want %s", c.name, outcome(err), c.want)
		}
		if err == nil && (!r.Conforming.Equal(q("150")) || !r.ReleasedBefore.Equal(q("100")) || len(r.InspectionIDs) != 1) {
			t.Errorf("%s: basis %+v", c.name, r)
		}
	}
}

// Validity is derived at an instant (tactical principle 3): not yet valid before it starts, expired
// from its end; QualityReleaseGuard v1's code is QUALITY_RELEASE_REQUIRED outside it.
func TestStateAtAnInstant(t *testing.T) {
	r := &Release{Status: StatusGranted, ValidFrom: now, ValidUntil: now.Add(time.Hour)}
	for _, c := range []struct {
		at    time.Time
		state string
	}{
		{now.Add(-time.Microsecond), StateNotYetValid},
		{now, StatusGranted},
		{now.Add(time.Hour - time.Microsecond), StatusGranted},
		{now.Add(time.Hour), StateExpired},
	} {
		code := ""
		if c.state != StatusGranted {
			code = CodeQualityReleaseRequired
		}
		if r.State(c.at) != c.state || r.Code(c.at) != code {
			t.Errorf("%s: %s %s", c.at, r.State(c.at), r.Code(c.at))
		}
	}
	r.Status = StatusRevoked
	if r.State(now) != StatusRevoked || r.Code(now) != CodeQualityReleaseRequired {
		t.Fatal("revoked")
	}
}

// QLT-INV-002 and BT21: a revocation records reason and evidence once; a second changes nothing and
// gives no quantity back (the revoked release still counts).
func TestRevokeOnce(t *testing.T) {
	is := []*inspection.Inspection{inspected("insp.1", inspection.StatusRecorded, "200")}
	r, err := Grant(grant("rel.1", "200"), inputs(is))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Revoke("色差", []string{"secret"}, map[string]string{}, q("100"), inspection.Act{}); outcome(err) != "REFERENCE_INVALID /args/evidence_ids/0" ||
		r.Status != StatusGranted {
		t.Fatalf("unreadable evidence: %s", outcome(err))
	}
	for _, shipped := range []string{"-1", "200.000001"} {
		if _, err = r.Revoke("色差", nil, nil, q(shipped), inspection.Act{}); err == nil || r.Status != StatusGranted {
			t.Fatalf("FUL shipped %s: %v", shipped, err)
		}
	}
	changed, err := r.Revoke("色差", nil, nil, q("100"), inspection.Act{By: qc, Epoch: 5, At: now})
	if err != nil || !changed || r.Status != StatusRevoked || r.RowVersion != 2 || r.Revocation.Reason != "色差" ||
		!r.Revocation.Shipped.Equal(q("100")) || !r.Frozen().Equal(q("100")) {
		t.Fatalf("revoke %+v, %v", r, err)
	}
	changed, err = r.Revoke("again", nil, nil, q("150"), inspection.Act{By: qc, At: now.Add(time.Hour)})
	if err != nil || changed || r.RowVersion != 2 || r.Revocation.Reason != "色差" || !r.Revocation.Shipped.Equal(q("100")) {
		t.Fatalf("second revoke %+v, %v", r, err)
	}
	if _, err = Grant(grant("rel.2", "1"), inputs(is, r)); outcome(err) != "QUALITY_RELEASE_EXCEEDS_CONFORMING /args/quantity" {
		t.Fatalf("a revocation gives quantity back before a re-inspection: %s", outcome(err))
	}
	spec := subject.Spec()
	if !Released([]*Release{r}, is, spec).Equal(q("200")) || !Frozen([]*Release{r}, is, spec).Equal(q("100")) ||
		!Releasable(is, []*Release{r}, spec).IsZero() {
		t.Fatal("released")
	}
}

// BT21 with S5R-D18 (QLT-INV-001, QLT-INV-002): 200 released, FUL ships 100, the release is revoked
// and 100 frozen. Without a re-inspection the frozen 100 cannot be released again
// (QUALITY_RELEASE_EXCEEDS_CONFORMING); a re-inspection after the revocation that supersedes the
// inspection the release was judged on and restates the result (200 inspected, 190 conforming: the
// 100 shipped and 90 of the frozen) lets the revoked release count only its 100 shipped, so 90 may be
// released again; a further re-inspection restates again and never adds.
func TestFrozenQuantityReleasedAgainAfterReinspectionBT21(t *testing.T) {
	first := inspected("insp.1", inspection.StatusRecorded, "200")
	r, err := Grant(grant("rel.1", "200"), inputs([]*inspection.Inspection{first}))
	if err != nil {
		t.Fatal(err)
	}
	revokedAt := now.Add(time.Hour)
	if _, err = r.Revoke("色差", nil, nil, q("100"), inspection.Act{By: qc, Epoch: 5, At: revokedAt}); err != nil {
		t.Fatal(err)
	}
	if _, err = Grant(grant("rel.2", "1"), inputs([]*inspection.Inspection{first}, r)); outcome(err) != "QUALITY_RELEASE_EXCEEDS_CONFORMING /args/quantity" {
		t.Fatalf("the frozen 100 without a re-inspection: %s", outcome(err))
	}
	// The re-inspection supersedes insp.1 after the revocation and restates the result.
	again := &inspection.Inspection{ID: "insp.2", Status: inspection.StatusRecorded, Subject: subject, Inspected: q("200"), Conforming: q("190"),
		Nonconforming: q("10"), Supersedes: "insp.1", Recorded: inspection.Act{By: qc, At: revokedAt.Add(time.Minute)}}
	old := *first
	old.Status, old.SupersededBy = inspection.StatusSuperseded, "insp.2"
	is := []*inspection.Inspection{&old, again}
	spec := subject.Spec()
	if !r.Thawed(is) || !r.Counted(is).Equal(q("100")) || !Frozen([]*Release{r}, is, spec).IsZero() || !Releasable(is, []*Release{r}, spec).Equal(q("90")) {
		t.Fatalf("after the re-inspection: counted %s", r.Counted(is))
	}
	if _, err = Grant(grant("rel.2", "90.000001"), inputs(is, r)); outcome(err) != "QUALITY_RELEASE_EXCEEDS_CONFORMING /args/quantity" {
		t.Fatalf("above the restated result: %s", outcome(err))
	}
	g, err := Grant(grant("rel.2", "90"), inputs(is, r))
	if err != nil || !g.Conforming.Equal(q("190")) || !g.ReleasedBefore.Equal(q("100")) || len(g.InspectionIDs) != 1 || g.InspectionIDs[0] != "insp.2" ||
		g.Concession() {
		t.Fatalf("the frozen 90 again %+v %v", g, err)
	}
	// A re-inspection recorded before the revocation (a correction between grant and revocation) is
	// no re-inspection of the frozen quantity: the release keeps counting in full.
	early := *again
	early.Recorded.At = revokedAt.Add(-time.Minute)
	if r.Thawed([]*inspection.Inspection{&old, &early}) || !Releasable([]*inspection.Inspection{&old, &early}, []*Release{r}, spec).IsZero() {
		t.Fatal("a re-inspection before the revocation")
	}
	// A revocation recorded before S5R-D18 knows no shipped quantity and keeps counting in full.
	legacy := *r
	legacy.Revocation = &Revocation{Reason: "色差", Revoked: r.Revocation.Revoked}
	if legacy.Thawed(is) || !legacy.Counted(is).Equal(q("200")) || !legacy.Frozen().IsZero() {
		t.Fatal("a revocation without a shipped quantity")
	}
	// A granted release always counts in full.
	if !g.Counted(is).Equal(q("90")) || g.Thawed(is) {
		t.Fatal("granted")
	}
}

// S5R-D18 (QLT-INV-001): an approved concession is releasable next to the conforming quantity, and
// the release that carries conceded goods is marked: conforming goods go first, the part beyond the
// conforming quantity left is the concession quantity.
func TestConcessionIsReleasableAndMarked(t *testing.T) {
	disposed := &inspection.Inspection{ID: "insp.1", Status: inspection.StatusDisposed, Subject: subject, Inspected: q("120"), Conforming: q("100"),
		Nonconforming: q("20"), Recorded: inspection.Act{By: qc, At: now.Add(-time.Hour)},
		Disposition: &inspection.Disposition{Items: []inspection.Item{{Action: inspection.ActionConcession, Quantity: q("15")},
			{Action: inspection.ActionScrap, Quantity: q("5")}}}}
	is := []*inspection.Inspection{disposed}
	first, err := Grant(grant("rel.1", "90"), inputs(is))
	if err != nil || first.Concession() || !first.Conceded.Equal(q("15")) || !first.ConcessionQuantity.IsZero() {
		t.Fatalf("conforming goods first %+v %v", first, err)
	}
	if _, err = Grant(grant("rel.2", "25.000001"), inputs(is, first)); outcome(err) != "QUALITY_RELEASE_EXCEEDS_CONFORMING /args/quantity" {
		t.Fatalf("above conforming plus concession: %s", outcome(err))
	}
	second, err := Grant(grant("rel.2", "25"), inputs(is, first))
	if err != nil || !second.Concession() || !second.ConcessionQuantity.Equal(q("15")) || !second.ReleasedBefore.Equal(q("90")) ||
		!second.Conforming.Equal(q("100")) || !second.Conceded.Equal(q("15")) {
		t.Fatalf("the conceded 15 %+v %v", second, err)
	}
	if !Releasable(is, []*Release{first, second}, subject.Spec()).IsZero() {
		t.Fatal("all released")
	}
	for _, c := range []struct{ quantity, conforming, released, want string }{
		{"10", "100", "0", "0"}, {"10", "100", "95", "5"}, {"10", "100", "120", "10"}, {"10", "0", "0", "10"},
	} {
		if got := ConcessionPart(q(c.quantity), q(c.conforming), q(c.released)); !got.Equal(q(c.want)) {
			t.Errorf("%+v: %s", c, got)
		}
	}
}

func TestAnswer(t *testing.T) {
	r := &Release{RequestHash: "rh", Granted: inspection.Act{By: qc}}
	other := kernel.ActorRef{Kind: kernel.ActorHuman, ID: "qc.2"}
	if r.Answer("rh", qc) != nil || outcome(r.Answer("x", qc)) != "BUSINESS_KEY_CONFLICT /args/release_id" ||
		outcome(r.Answer("rh", other)) != "BUSINESS_KEY_CONFLICT /args/release_id" {
		t.Fatal("answer")
	}
}
