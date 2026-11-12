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
	return &inspection.Inspection{ID: id, Status: status, Subject: subject, Inspected: q(conforming), Conforming: q(conforming)}
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
		r.Granted != (inspection.Act{By: qc, Epoch: 4, At: now}) || r.OwnerActor != "qc.1" || r.State(now) != StatusGranted || r.Code(now) != "" {
		t.Fatalf("%+v", r)
	}
}

// QLT-INV-001 negatives: the conforming quantity of valid inspections at the subject's
// specification less what was released before, revoked releases included, bounds every release.
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
	if _, err = r.Revoke("色差", []string{"secret"}, map[string]string{}, inspection.Act{}); outcome(err) != "REFERENCE_INVALID /args/evidence_ids/0" ||
		r.Status != StatusGranted {
		t.Fatalf("unreadable evidence: %s", outcome(err))
	}
	changed, err := r.Revoke("色差", nil, nil, inspection.Act{By: qc, Epoch: 5, At: now})
	if err != nil || !changed || r.Status != StatusRevoked || r.RowVersion != 2 || r.Revocation.Reason != "色差" {
		t.Fatalf("revoke %+v, %v", r, err)
	}
	changed, err = r.Revoke("again", nil, nil, inspection.Act{By: qc, At: now.Add(time.Hour)})
	if err != nil || changed || r.RowVersion != 2 || r.Revocation.Reason != "色差" {
		t.Fatalf("second revoke %+v, %v", r, err)
	}
	if _, err = Grant(grant("rel.2", "1"), inputs(is, r)); outcome(err) != "QUALITY_RELEASE_EXCEEDS_CONFORMING /args/quantity" {
		t.Fatalf("a revocation gives quantity back: %s", outcome(err))
	}
	if !Released([]*Release{r}, subject.Spec()).Equal(q("200")) {
		t.Fatal("released")
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
