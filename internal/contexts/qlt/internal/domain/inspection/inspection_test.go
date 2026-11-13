package inspection

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

var (
	now     = time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	human   = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "qc.1"}
	agent   = kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.1"}
	ref     = SubjectRef{Kind: "purchase_commitment_line", ID: "commitment.1", LineID: "L1"}
	subject = Subject{Ref: ref, Revision: 1, Hash: "h", SupplierID: "supplier.1", OrderID: "order.1", OrderRevision: 1, OrderHash: "o",
		OrderLineID: "OL1", SkuID: "sku.1", SpecRevision: 2, UnitID: "pcs", UnitRevision: 1, Quantity: kernel.MustDecimal("120")}
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

func request(id string, inspected, conforming string) Request {
	in, c := q(inspected), q(conforming)
	return Request{ID: id, Subject: ref, Inspected: in, Conforming: c, Nonconforming: in.Sub(c), Sampling: Sampling{Method: SamplingFull},
		StandardID: "standard", InspectorKind: InspectorAgency, Agency: &Inspector{PartyID: "agency", ServiceRoleID: "final", RoleRevision: 1},
		ReportFileID: "report", InspectedAt: now.Add(-time.Hour), RequestHash: "rh"}
}

func inputs(existing ...*Inspection) Inputs {
	s := subject
	return Inputs{Subject: &s, Existing: existing, Sources: map[string]string{"standard": "std-sha", "ev": "ev-sha"},
		Report: &Binding{ID: "report", SHA256: "rep-sha", MimeType: "application/pdf"},
		Agency: &Agency{InspectionAgency: true, Check: func(string) error { return nil }}, Recorder: human, Epoch: 3, Team: "qc", Now: now}
}

// QLT-INV-001 positive (F-06): an external agency's report of 120 conforming is recorded as an
// inspection; it binds the subject as PUR said it was, the standard's and the report's hashes and
// the agency, and releases nothing.
func TestRecordBindsSubjectStandardReportAndAgency(t *testing.T) {
	i, old, err := Record(request("insp.1", "120", "120"), inputs())
	if err != nil || old != nil {
		t.Fatal(err)
	}
	if i.Status != StatusRecorded || i.RowVersion != 1 || i.Seq != 1 || i.Subject != subject || i.Standard != (Binding{ID: "standard", SHA256: "std-sha"}) ||
		i.Report.SHA256 != "rep-sha" || i.Inspector != (Inspector{Kind: InspectorAgency, PartyID: "agency", ServiceRoleID: "final", RoleRevision: 1}) ||
		i.OwnerActor != "qc.1" || i.OwnerTeam != "qc" || i.Recorded != (Act{By: human, Epoch: 3, At: now}) || !i.Valid() {
		t.Fatalf("%+v", i)
	}
	internal := request("insp.2", "120", "120")
	internal.InspectorKind, internal.Agency = InspectorInternal, nil
	i, _, err = Record(internal, inputs())
	if err != nil || i.Inspector != (Inspector{Kind: InspectorInternal, ActorID: "qc.1"}) {
		t.Fatalf("internal %+v, %v", i, err)
	}
}

func TestRecordRefusals(t *testing.T) {
	valid := &Inspection{ID: "insp.0", Status: StatusRecorded, RowVersion: 1, Seq: 1, Subject: subject, Inspected: q("100"), Conforming: q("100"),
		OwnerActor: "qc.1"}
	superseded := &Inspection{ID: "insp.s", Status: StatusSuperseded, RowVersion: 2, Seq: 2, Subject: subject, Inspected: q("100"), OwnerActor: "qc.1"}
	hidden := &Inspection{ID: "insp.h", Status: StatusRecorded, RowVersion: 1, Seq: 3, Subject: subject, Inspected: q("10"), OwnerActor: "other"}
	for _, c := range []struct {
		name string
		req  func(*Request)
		in   func(*Inputs)
		want string
	}{
		{"no subject", nil, func(in *Inputs) { in.Subject = nil }, "REFERENCE_INVALID /args/subject"},
		{"subject cancelled", nil, func(in *Inputs) { in.SubjectCode = "cancelled" }, "REFERENCE_STALE /args/subject"},
		{"AI as internal inspector", func(r *Request) { r.InspectorKind, r.Agency = InspectorInternal, nil },
			func(in *Inputs) { in.Recorder = agent }, "PERMISSION_DENIED /args/inspector/kind"},
		{"in the future", func(r *Request) { r.InspectedAt = now.Add(time.Second) }, nil, "INVALID_KNOWN_VALUE /args/inspected_at"},
		{"supersedes nothing of the subject", func(r *Request) { r.Supersedes, r.Reason = "insp.x", "复检" }, nil,
			"REFERENCE_INVALID /args/supersedes_inspection_id"},
		{"supersedes one not seen", func(r *Request) { r.Supersedes, r.Reason = "insp.h", "复检" },
			func(in *Inputs) {
				in.Existing = []*Inspection{hidden}
				in.Visible = func(i *Inspection) bool { return i.OwnerActor == "qc.1" }
			}, "REFERENCE_INVALID /args/supersedes_inspection_id"},
		{"supersedes a superseded one", func(r *Request) { r.Supersedes, r.Reason = "insp.s", "复检" },
			func(in *Inputs) { in.Existing = []*Inspection{superseded} }, "INVALID_STATE /args/supersedes_inspection_id"},
		// The error catalog's INSPECTION_QUANTITY_EXCEEDED (S5R, adopted by R5) replaces INVALID_KNOWN_VALUE.
		{"covers more than the subject", func(r *Request) { r.Inspected, r.Conforming = q("20.000001"), q("20.000001") },
			func(in *Inputs) { in.Existing = []*Inspection{valid} }, "INSPECTION_QUANTITY_EXCEEDED /args/inspected_quantity"},
		{"more than the subject", func(r *Request) { r.Inspected, r.Conforming = q("121"), q("121") }, nil,
			"INSPECTION_QUANTITY_EXCEEDED /args/inspected_quantity"},
		{"no agency", nil, func(in *Inputs) { in.Agency = nil }, "REFERENCE_INVALID /args/inspector/agency"},
		{"not an inspection agency (a forwarder, S5R-D36)", nil, func(in *Inputs) { in.Agency.InspectionAgency = false },
			"REFERENCE_INVALID /args/inspector/agency"},
		{"agency not current", nil, func(in *Inputs) {
			in.Agency.Check = func(p string) error {
				return kernel.RejectAs("REFERENCE_STALE", kernel.CategoryConflict, p, "superseded")
			}
		}, "REFERENCE_STALE /args/inspector/agency"},
		{"standard not read", nil, func(in *Inputs) { delete(in.Sources, "standard") }, "REFERENCE_INVALID /args/standard/source_id"},
		{"evidence not read", func(r *Request) { r.Evidence = []string{"ev", "secret"} }, nil, "REFERENCE_INVALID /args/evidence_ids/1"},
		{"report not read", nil, func(in *Inputs) { in.Report = nil }, "REFERENCE_INVALID /args/report_file_id"},
	} {
		req, in := request("insp.1", "20", "20"), inputs()
		in.Agency = &Agency{InspectionAgency: true, Check: func(string) error { return nil }}
		if c.req != nil {
			c.req(&req)
		}
		if c.in != nil {
			c.in(&in)
		}
		if _, _, err := Record(req, in); outcome(err) != c.want {
			t.Errorf("%s: %s, want %s", c.name, outcome(err), c.want)
		}
	}
	// The other valid inspections count, superseded ones do not: 100 + 20 fits 120 next to a
	// superseded 100.
	if _, _, err := Record(request("insp.1", "20", "20"), inputs(valid, superseded)); err != nil {
		t.Fatal(err)
	}
}

// BT08 and baseline 7.5: 20 of 120 are nonconforming, reworked and re-inspected. The re-inspection
// supersedes the first result and restates it; inspecting again by superseding never adds, and an
// inspection that does not supersede cannot cover the same goods again.
func TestReinspectionSupersedesNeverAdds(t *testing.T) {
	first, _, err := Record(request("insp.1", "120", "100"), inputs())
	if err != nil {
		t.Fatal(err)
	}
	spec := subject.Spec()
	if _, c, ids := Totals([]*Inspection{first}, spec); !c.Equal(q("100")) || len(ids) != 1 {
		t.Fatalf("first %s %v", c, ids)
	}
	err = first.Decide(1, []Item{{Action: ActionRework, Quantity: q("20")}}, "隔离返工", nil, nil, Act{By: human, Epoch: 3, At: now})
	if err != nil || first.Status != StatusDisposed || first.RowVersion != 2 {
		t.Fatalf("disposed %+v, %v", first, err)
	}
	if _, _, err = Record(request("insp.extra", "20", "20"), inputs(first)); outcome(err) != "INSPECTION_QUANTITY_EXCEEDED /args/inspected_quantity" {
		t.Fatalf("an extra inspection of the reworked 20: %s", outcome(err))
	}
	again := request("insp.2", "120", "120")
	again.Supersedes, again.Reason = "insp.1", "返工后复检"
	second, old, err := Record(again, inputs(first))
	if err != nil || old != first || first.Status != StatusSuperseded || first.RowVersion != 3 || first.SupersededBy != "insp.2" ||
		!first.SupersededAt.Equal(now) || second.Seq != 2 || second.Supersedes != "insp.1" {
		t.Fatalf("re-inspection %+v / %+v, %v", second, first, err)
	}
	covered, conforming, ids := Totals([]*Inspection{first, second}, spec)
	if !covered.Equal(q("120")) || !conforming.Equal(q("120")) || len(ids) != 1 || ids[0] != "insp.2" {
		t.Fatalf("after the re-inspection %s %s %v", covered, conforming, ids)
	}
	repeat := request("insp.3", "120", "120")
	repeat.Supersedes, repeat.Reason = "insp.2", "重复复检"
	third, _, err := Record(repeat, inputs(first, second))
	if err != nil {
		t.Fatal(err)
	}
	if _, conforming, _ = Totals([]*Inspection{first, second, third}, spec); !conforming.Equal(q("120")) {
		t.Fatalf("a repeated re-inspection adds: %s", conforming)
	}
	if err = first.Decide(3, []Item{{Action: ActionScrap, Quantity: q("20")}}, "r", nil, nil, Act{}); outcome(err) != "INVALID_STATE /args/inspection_id" {
		t.Fatalf("disposing a superseded inspection: %s", outcome(err))
	}
	// Results at another specification are covered but not counted at this one.
	other := *third
	other.Subject.SpecRevision = 3
	if covered, conforming, _ := Totals([]*Inspection{&other}, spec); !covered.Equal(q("120")) || !conforming.IsZero() {
		t.Fatalf("another specification %s %s", covered, conforming)
	}
}

func TestDecideRefusals(t *testing.T) {
	mk := func(nonconforming string) *Inspection {
		return &Inspection{ID: "insp.1", Status: StatusRecorded, RowVersion: 1, Inspected: q("120"), Conforming: q("120").Sub(q(nonconforming)),
			Nonconforming: q(nonconforming)}
	}
	items := []Item{{Action: ActionRework, Quantity: q("15")}, {Action: ActionScrap, Quantity: q("5")}}
	for _, c := range []struct {
		i        *Inspection
		expected int64
		items    []Item
		evidence []string
		want     string
	}{
		{mk("20"), 1, items, []string{"ev"}, "OK"},
		{mk("20"), 2, items, nil, "VERSION_CONFLICT /args/expected_version"},
		{mk("0"), 1, items, nil, "INVALID_STATE /args/inspection_id"},
		{mk("20"), 1, items[:1], nil, "INVALID_KNOWN_VALUE /args/items"},
		{mk("20"), 1, items, []string{"secret"}, "REFERENCE_INVALID /args/evidence_ids/0"},
		// S5R-D18: a concession cites the customer's consent.
		{mk("20"), 1, []Item{{Action: ActionConcession, Quantity: q("20")}}, nil, "SCHEMA_INVALID /args/evidence_ids"},
		{mk("20"), 1, []Item{{Action: ActionConcession, Quantity: q("20")}}, []string{"ev"}, "OK"},
	} {
		err := c.i.Decide(c.expected, c.items, "处置", c.evidence, map[string]string{"ev": "x"}, Act{By: human, Epoch: 1, At: now})
		if outcome(err) != c.want {
			t.Errorf("%+v: %s, want %s", c, outcome(err), c.want)
		}
	}
	i := mk("20")
	if err := i.Decide(1, items, "处置", nil, nil, Act{By: human}); err != nil || i.Disposition == nil || len(i.Disposition.Items) != 2 {
		t.Fatal(err)
	}
	if outcome(i.Decide(2, items, "again", nil, nil, Act{})) != "INVALID_STATE /args/inspection_id" {
		t.Fatal("a second disposition")
	}
}

func TestAnswer(t *testing.T) {
	i := &Inspection{RequestHash: "rh", Recorded: Act{By: human}}
	if i.Answer("rh", human) != nil || outcome(i.Answer("other", human)) != "BUSINESS_KEY_CONFLICT /args/inspection_id" ||
		outcome(i.Answer("rh", agent)) != "BUSINESS_KEY_CONFLICT /args/inspection_id" {
		t.Fatal("answer")
	}
}

// S5R-D18 and QLT-INV-001: the approved concession of a subject is what the dispositions of its valid
// inspections at the specification accept by concession; a superseded inspection's concession and
// one at another specification do not count.
func TestConcessions(t *testing.T) {
	disposed := func(id, status string, items ...Item) *Inspection {
		i := &Inspection{ID: id, Status: status, Subject: subject, Inspected: q("120"), Conforming: q("100"), Nonconforming: q("20")}
		if items != nil {
			i.Disposition = &Disposition{Items: items}
		}
		return i
	}
	spec := subject.Spec()
	a := disposed("insp.a", StatusDisposed, Item{Action: ActionConcession, Quantity: q("15")}, Item{Action: ActionScrap, Quantity: q("5")})
	b := disposed("insp.b", StatusSuperseded, Item{Action: ActionConcession, Quantity: q("20")})
	c := disposed("insp.c", StatusDisposed, Item{Action: ActionRework, Quantity: q("20")})
	d := disposed("insp.d", StatusDisposed, Item{Action: ActionConcession, Quantity: q("7")})
	d.Subject.SpecRevision = 3
	if !a.Conceded().Equal(q("15")) || !c.Conceded().IsZero() || !disposed("insp.e", StatusRecorded).Conceded().IsZero() {
		t.Fatal("conceded")
	}
	if got := Concessions([]*Inspection{a, b, c, d}, spec); !got.Equal(q("15")) {
		t.Fatalf("concessions %s", got)
	}
	if !ConsentRequired([]Item{{Action: ActionScrap}, {Action: ActionConcession}}) || ConsentRequired([]Item{{Action: ActionRework}}) {
		t.Fatal("consent")
	}
}

// QLT-INV-002 and S5R-D18 ("冻结量须复检后才能再放行"): an inspection counts as inspected again since
// an instant when it is superseded and the inspection valid now at the end of its chain was recorded
// at or after that instant.
func TestReinspected(t *testing.T) {
	at := now.Add(time.Hour)
	mk := func(id, status, by string, recorded time.Time) *Inspection {
		return &Inspection{ID: id, Status: status, SupersededBy: by, Recorded: Act{At: recorded}}
	}
	i1 := mk("i1", StatusSuperseded, "i2", now)
	i2 := mk("i2", StatusSuperseded, "i3", now.Add(time.Minute)) // a correction before the instant
	i3 := mk("i3", StatusRecorded, "", at)                       // the re-inspection at the instant
	j1 := mk("j1", StatusDisposed, "", now)
	k1 := mk("k1", StatusSuperseded, "k2", now)
	k2 := mk("k2", StatusRecorded, "", at.Add(-time.Microsecond))
	all := []*Inspection{i1, i2, i3, j1, k1, k2}
	for _, c := range []struct {
		ids  []string
		want bool
	}{
		{[]string{"i1"}, true},
		{[]string{"i2"}, true},
		{[]string{"i1", "j1"}, false}, // j1 is still valid: not inspected again
		{[]string{"k1"}, false},       // superseded before the instant only
		{[]string{"x"}, false},
		{nil, false},
	} {
		if got := Reinspected(all, c.ids, at); got != c.want {
			t.Errorf("%v: %v", c.ids, got)
		}
	}
	broken := []*Inspection{mk("b1", StatusSuperseded, "b2", now)}
	if Reinspected(broken, []string{"b1"}, now) {
		t.Fatal("a chain that ends nowhere")
	}
}
