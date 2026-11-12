//go:build integration

package bootstrap_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/contexts/qlt"
	qltapi "go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// WBS S5-02 on PostgreSQL 16: QLT's inspections, dispositions and quality releases (tactical model
// QLT-AGG-Inspection, QLT-AGG-QualityRelease, SM-QLT-INSPECTION, SM-QLT-RELEASE, QLT-INV-001,
// QLT-INV-002; F-06, BT08, BT21). Python V0.50 has no quality context; test names cite the design.
//
// PUR's InspectionSubject v1 (R24) is planned for S5 unit A4 and not published yet, so QLT runs here
// on an in-memory implementation of its Subjects port that answers purchase commitment lines as
// PUR will; the composed process (bootstrap.Contexts) wires qlt.SubjectsNotWired. EVD (R72) and PTY
// (R73) are the real contexts.

// fakeSubjects answers QLT's port to PUR from memory.
type fakeSubjects struct {
	mu    sync.Mutex
	lines map[qltapi.SubjectRef]qlt.Subject
}

func (f *fakeSubjects) put(s qlt.Subject) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lines[s.Ref] = s
}

func (f *fakeSubjects) drop(ref qltapi.SubjectRef) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.lines, ref)
}

func (f *fakeSubjects) ResolveSubjects(_ context.Context, refs []qltapi.SubjectRef) (map[qltapi.SubjectRef]qlt.Subject, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[qltapi.SubjectRef]qlt.Subject{}
	for _, r := range refs {
		if s, ok := f.lines[r]; ok {
			out[r] = s
		}
	}
	return out, nil
}

func hex64(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// commitmentLine is line L1 of purchase commitment id for quantity PCS of sku.1, as PUR would answer
// it: issued, from supplier.1, serving line OL1 of order.1.
func commitmentLine(id, quantity string) qlt.Subject {
	return qlt.Subject{Ref: qltapi.SubjectRef{Kind: qltapi.SubjectCommitmentLine, ID: id, LineID: "L1"}, Revision: 1, Hash: hex64("commitment " + id),
		Status: "issued", SupplierID: "supplier.1", OrderID: "order.1", OrderRevision: 1, OrderHash: hex64("order.1"), OrderLineID: "OL1",
		SkuID: "sku.1", SpecRevision: 1, UnitID: "pcs", UnitRevision: 1, Quantity: quantity}
}

type qltWorld struct {
	*world
	q        *qlt.Module
	subjects *fakeSubjects
	editor   context.Context // every permission, company data scope (polActors)
	qc       context.Context // our quality inspector: role template quality_inspector, company data scope
	agent    context.Context // an AI agent holding the QLT permissions and evd.read
	now      time.Time
}

// newQltWorld builds QLT on its own transaction owner with the in-memory PUR port, the standard
// (EVD source "standard"), the report (EVD file "report"), and the external agency: supplier party
// "agency" with the verified service role "final" of kind inspection (PTY, revision 1).
func newQltWorld(t *testing.T) *qltWorld {
	t.Helper()
	w := newWorld(t)
	editor, _ := w.polActors()
	subjects := &fakeSubjects{lines: map[qltapi.SubjectRef]qlt.Subject{}}
	db, err := bentopg.NewDB(pgtest.AppPool(t))
	w.must(err)
	q, err := qlt.New(db, qlt.Upstream{Subjects: subjects, Sources: w.c.EVD, Parties: w.c.PTY})
	w.must(err)
	x := &qltWorld{world: w, q: q, subjects: subjects, editor: editor, qc: w.assign("qc.1", "quality_inspector"), now: time.Now().UTC()}
	x.agent = w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: actorRef("ai", "agent.1"), DataScope: "company",
		Permissions: []string{"inspection.record", "disposition.decide", "quality_release.grant", "quality_release.revoke", "qlt.read", "evd.read"}})
	w.source(x.qc, "standard", "合成检验标准：AQL 2.5，一般检验水平 II")
	_, err = w.file(x.qc, "report", "final-inspection.txt", evdapi.MimeText, "合成第三方尾期验货报告：120 件合格", "")
	w.must(err)
	w.staffParty(editor, "agency", []string{ptyapi.RoleSupplier})
	from := x.now.Add(-24 * time.Hour).Truncate(time.Second)
	_, err = w.c.PTY.VerifyServiceRole(editor, ptyapi.VerifyServiceRole{IdempotencyKey: w.key(), PartyID: "agency", ExpectedVersion: 2,
		ServiceRoleID: "final", Role: ptyapi.ServiceInspection, ServiceCode: "final.inspection", Origin: "CNSHA", Destination: "CNSHA",
		ValidFrom: kernel.FormatTimestamp(from), ValidUntil: kernel.FormatTimestamp(from.Add(30 * 24 * time.Hour)), EvidenceIDs: []string{"evidence"}})
	w.must(err)
	return x
}

func subjectOf(id string) qltapi.SubjectRef {
	return qltapi.SubjectRef{Kind: qltapi.SubjectCommitmentLine, ID: id, LineID: "L1"}
}

// inspection is an external agency's inspection of subject: inspected of which conforming conform,
// fully inspected against "standard", reported in "report", an hour ago.
func (x *qltWorld) inspection(id string, subject qltapi.SubjectRef, inspected, conforming string, edit ...func(*qltapi.RecordInspection)) qltapi.RecordInspection {
	in, c := kernel.MustDecimal(inspected), kernel.MustDecimal(conforming)
	cmd := qltapi.RecordInspection{IdempotencyKey: x.key(), InspectionID: id, Subject: subject, InspectedQuantity: inspected,
		ConformingQuantity: conforming, NonconformingQuantity: in.Sub(c).String(), Sampling: qltapi.Sampling{Method: qltapi.SamplingFull},
		Standard: qltapi.StandardRef{SourceID: "standard"}, Inspector: qltapi.Inspector{Kind: qltapi.InspectorAgency,
			Agency: &qltapi.AgencyRef{PartyID: "agency", ServiceRoleID: "final", Revision: 1}},
		ReportFileID: "report", InspectedAt: kernel.FormatTimestamp(x.now.Add(-time.Hour))}
	for _, e := range edit {
		e(&cmd)
	}
	return cmd
}

func (x *qltWorld) inspect(ctx context.Context, id string, subject qltapi.SubjectRef, inspected, conforming string,
	edit ...func(*qltapi.RecordInspection)) (qltapi.InspectionReceipt, error) {
	return x.q.RecordInspection(ctx, x.inspection(id, subject, inspected, conforming, edit...))
}

func internal(c *qltapi.RecordInspection) {
	c.Inspector = qltapi.Inspector{Kind: qltapi.InspectorInternal}
}

func supersedes(id string) func(*qltapi.RecordInspection) {
	return func(c *qltapi.RecordInspection) {
		c.SupersedesInspectionID, c.Reason = id, "返工后复检，取代原结果"
	}
}

func (x *qltWorld) grantCmd(id string, subject qltapi.SubjectRef, quantity string) qltapi.GrantRelease {
	return qltapi.GrantRelease{IdempotencyKey: x.key(), ReleaseID: id, Subject: subject, Quantity: quantity,
		ValidUntil: kernel.FormatTimestamp(x.now.Add(30 * 24 * time.Hour))}
}

func (x *qltWorld) grant(ctx context.Context, id string, subject qltapi.SubjectRef, quantity string) (qltapi.ReleaseReceipt, error) {
	return x.q.GrantRelease(ctx, x.grantCmd(id, subject, quantity))
}

func (x *qltWorld) revoke(ctx context.Context, id, key string) (qltapi.ReleaseReceipt, error) {
	return x.q.RevokeRelease(ctx, qltapi.RevokeRelease{IdempotencyKey: key, ReleaseID: id, Reason: "复检发现色差，撤回放行"})
}

// released records an inspection of subject id (quantity, all conforming) and grants it all.
func (x *qltWorld) released(id, quantity string) qltapi.ReleaseReceipt {
	x.t.Helper()
	x.subjects.put(commitmentLine(id, quantity))
	_, err := x.inspect(x.qc, "insp."+id, subjectOf(id), quantity, quantity)
	x.must(err)
	r, err := x.grant(x.qc, "rel."+id, subjectOf(id), quantity)
	x.must(err)
	return r
}

func (x *qltWorld) standing(id string) qltapi.SubjectView {
	x.t.Helper()
	v, err := x.q.GetSubject(x.qc, subjectOf(id))
	x.must(err)
	return v
}

func (x *qltWorld) qltEvents(kind string) []event {
	var out []event
	for _, e := range x.events("company.A", qltapi.Source) {
		if e.Type == kind {
			out = append(out, e)
		}
	}
	return out
}

// F-06 and QLT-INV-001 (positive "外部机构报告 120 件合格后由我方质检放行 120 件", negative AI or system
// release → PERMISSION_DENIED, release above conforming → QUALITY_RELEASE_EXCEEDS_CONFORMING), WBS
// S5-02 acceptance "外部检验机构与报告来源可登记，放行仍由我方授权人员决定": an AI agent transcribes an
// external agency's report as an inspection; the inspection binds the agency's PTY service role, the
// report's and the standard's hashes, and the subject as PUR said it was; only our quality inspector
// (role template quality_inspector) releases.
func TestExternalAgencyReportAndReleaseByOurQC(t *testing.T) {
	x := newQltWorld(t)
	x.subjects.put(commitmentLine("commitment.1", "120"))
	line := subjectOf("commitment.1")

	cmd := x.inspection("insp.1", line, "120", "120")
	r, err := x.q.RecordInspection(x.agent, cmd)
	if err != nil || r.Status != qltapi.StatusRecorded || r.RowVersion != 1 || r.ObjectID != "insp.1" || r.Replayed || r.Subject != line ||
		r.ConformingQuantity != "120" || r.UnitID != "pcs" {
		t.Fatalf("record %+v, %v", r, err)
	}
	again, err := x.q.RecordInspection(x.agent, cmd)
	if err != nil || !again.Replayed || again.ActionID != r.ActionID {
		t.Fatalf("replay %+v, %v", again, err)
	}
	cmd.IdempotencyKey = x.key()
	if again, err = x.q.RecordInspection(x.agent, cmd); err != nil || !again.Replayed || again.ActionID == r.ActionID {
		t.Fatalf("the same inspection under another key %+v, %v", again, err)
	}
	cmd.IdempotencyKey, cmd.ConformingQuantity, cmd.NonconformingQuantity = x.key(), "119", "1"
	_, err = x.q.RecordInspection(x.agent, cmd)
	x.want(err, qltapi.CodeBusinessKeyConflict)

	v, err := x.q.GetInspection(x.qc, "insp.1")
	x.must(err)
	report, err := x.c.EVD.GetFile(x.qc, "report")
	x.must(err)
	if v.Inspector.Kind != qltapi.InspectorAgency || *v.Inspector.Agency != (qltapi.AgencyRef{PartyID: "agency", ServiceRoleID: "final", Revision: 1}) ||
		v.Report != (qltapi.FileBinding{FileID: "report", ContentSHA256: report.ContentSHA256, MimeType: evdapi.MimeText}) ||
		v.Standard.SourceID != "standard" || len(v.Standard.ContentSHA256) != 64 || v.Subject.Hash != hex64("commitment commitment.1") ||
		v.Subject.Quantity != "120" || v.RecordedBy != actorRef("ai", "agent.1") || v.Status != qltapi.StatusRecorded {
		t.Fatalf("inspection %+v", v)
	}
	if s := x.standing("commitment.1"); s.Conforming != "120" || s.Released != "0" || s.Releasable != "120" {
		t.Fatalf("an inspection releases nothing: %+v", s)
	}

	// Only our authorized human releases: an AI or a system actor holding the permission is refused.
	system := x.provision(bootstrap.Grant{Scope: x.scope("company.A"), Actor: actorRef("system", "job.1"), DataScope: "company",
		Permissions: []string{"quality_release.grant", "qlt.read"}})
	for _, ctx := range []context.Context{x.agent, system} {
		_, err = x.grant(ctx, "rel.1", line, "120")
		x.want(err, kernel.CodePermissionDenied)
	}
	_, err = x.grant(x.qc, "rel.1", line, "120.000001")
	if code(err) != qltapi.CodeQualityReleaseExceedsConforming || path(err) != "/args/quantity" {
		t.Fatalf("above conforming: %s at %s", code(err), path(err))
	}
	g, err := x.grant(x.qc, "rel.1", line, "120")
	if err != nil || g.Status != qltapi.StatusGranted || g.RowVersion != 1 || g.Quantity != "120" || g.UnitID != "pcs" || g.Subject != line {
		t.Fatalf("grant %+v, %v", g, err)
	}
	_, err = x.grant(x.qc, "rel.2", line, "1")
	x.want(err, qltapi.CodeQualityReleaseExceedsConforming)
	rv, err := x.q.GetRelease(x.qc, "rel.1")
	x.must(err)
	if rv.State != qltapi.StatusGranted || rv.Conforming != "120" || rv.ReleasedBefore != "0" || !slices.Equal(rv.InspectionIDs, []string{"insp.1"}) ||
		rv.GrantedBy != actorRef("human", "qc.1") || rv.Subject.OrderLineID != "OL1" || rv.Revocation != nil || rv.AuthorityEpoch < 1 {
		t.Fatalf("release %+v", rv)
	}
	recorded, released := x.qltEvents(qltapi.EventInspectionRecorded), x.qltEvents(qltapi.EventQualityReleased)
	if len(recorded) != 1 || recorded[0].Payload["report_sha256"] != report.ContentSHA256 || recorded[0].Payload["agency_party_id"] != "agency" ||
		len(released) != 1 || released[0].Payload["released_quantity"] != "120" || released[0].Payload["order_line_id"] != "OL1" {
		t.Fatalf("events %v %v", recorded, released)
	}
	rec, err := x.q.Reconcile(x.qc, x.grantCmd("rel.1", line, "120"))
	if err != nil || rec.State != kernel.ReconcileNotCommitted {
		t.Fatalf("another key %+v, %v", rec, err)
	}

	// The references are the recorder's to read; PUR's subject must serve.
	private := x.actor("private.owner", "source.register", "evd.read")
	x.source(private, "private", "合成内部标准")
	ownScope := x.provision(bootstrap.Grant{Scope: x.scope("company.A"), Actor: actorRef("human", "inspector.own"),
		Permissions: []string{"inspection.record", "evd.read", "qlt.read"}})
	x.subjects.put(commitmentLine("commitment.2", "50"))
	line2 := subjectOf("commitment.2")
	for _, c := range []struct {
		name string
		ctx  context.Context
		edit func(*qltapi.RecordInspection)
		code string
		path string
	}{
		{"an AI is no internal inspector", x.agent, internal, kernel.CodePermissionDenied, "/args/inspector/kind"},
		{"a standard the recorder does not read", ownScope, func(c *qltapi.RecordInspection) { internal(c); c.Standard.SourceID = "private" },
			kernel.CodeReferenceInvalid, "/args/standard/source_id"},
		{"no such report", x.qc, func(c *qltapi.RecordInspection) { c.ReportFileID = "nope" }, kernel.CodeReferenceInvalid, "/args/report_file_id"},
		{"no such agency revision", x.qc, func(c *qltapi.RecordInspection) { c.Inspector.Agency.Revision = 2 }, kernel.CodeReferenceInvalid,
			"/args/inspector/agency"},
		{"no such subject", x.qc, func(c *qltapi.RecordInspection) { c.Subject.LineID = "L9" }, kernel.CodeReferenceInvalid, "/args/subject"},
		{"more than the subject", x.qc, func(c *qltapi.RecordInspection) { c.InspectedQuantity, c.ConformingQuantity = "51", "51" },
			kernel.CodeInvalidKnownValue, "/args/inspected_quantity"},
	} {
		cmd := x.inspection("insp.bad", line2, "50", "50", c.edit)
		if _, err = x.q.RecordInspection(c.ctx, cmd); code(err) != c.code || path(err) != c.path {
			t.Errorf("%s: %s at %s", c.name, code(err), path(err))
		}
	}
	cancelled := commitmentLine("commitment.2", "50")
	cancelled.Code = "PURCHASE_COMMITMENT_CANCELLED"
	x.subjects.put(cancelled)
	_, err = x.inspect(x.qc, "insp.bad", line2, "50", "50")
	x.want(err, qltapi.CodeReferenceStale)
	if x.count("company.A", "brant_qlt.inspection") != 1 || x.count("company.B", "brant_qlt.inspection") != 0 {
		t.Fatal("a refusal left an inspection, or another company sees one")
	}

	// Another company sees nothing (FORCE RLS on app.tenant_id and app.company_id).
	other := x.provision(bootstrap.Grant{Scope: x.scope("company.B"), Actor: actorRef("human", "qc.b"), DataScope: "company",
		Permissions: []string{"qlt.read", "evd.read"}})
	_, err = x.q.GetInspection(other, "insp.1")
	x.want(err, kernel.CodeNotFound)
	_, err = x.q.GetRelease(other, "rel.1")
	x.want(err, kernel.CodeNotFound)
	if got, err := x.q.ResolveReleases(other, []string{"rel.1"}, time.Now()); err != nil || len(got) != 0 {
		t.Fatalf("another company resolves %v, %v", got, err)
	}
	// A reader who no longer reads the subject in PUR is refused the record.
	x.subjects.drop(line)
	_, err = x.q.GetInspection(x.qc, "insp.1")
	x.want(err, kernel.CodeReferenceInvalid)
	_, err = x.q.GetRelease(x.qc, "rel.1")
	x.want(err, kernel.CodeReferenceInvalid)
	_, err = x.q.GetSubject(x.qc, line)
	x.want(err, kernel.CodeNotFound)
}

// The composed process has no InspectionSubject v1 from PUR yet (S5 unit A4): QLT answers
// DEPENDENCY_NOT_SUPPORTED, records nothing, and audits the refusal.
func TestQLTWithoutPURSubjects(t *testing.T) {
	x := newQltWorld(t)
	_, err := x.c.QLT.RecordInspection(x.qc, x.inspection("insp.1", subjectOf("commitment.1"), "1", "1"))
	if code(err) != qltapi.CodeDependencyNotSupported || path(err) != "/args/subject" {
		t.Fatalf("record without PUR: %s at %s", code(err), path(err))
	}
	_, err = x.c.QLT.GrantRelease(x.qc, x.grantCmd("rel.1", subjectOf("commitment.1"), "1"))
	x.want(err, qltapi.CodeDependencyNotSupported)
	_, err = x.c.QLT.GetSubject(x.qc, subjectOf("commitment.1"))
	x.want(err, qltapi.CodeDependencyNotSupported)
	if x.count("company.A", "brant_qlt.inspection") != 0 {
		t.Fatal("recorded without PUR")
	}
	admin := x.actor("auditor", "qlt.audit")
	entries, err := x.c.QLT.Audit(admin)
	x.must(err)
	if !slices.ContainsFunc(entries, func(e kernel.AuditEntry) bool {
		return e.Command == qltapi.CommandRecordInspection && e.Code == qltapi.CodeDependencyNotSupported && e.Outcome == "rejected"
	}) {
		t.Fatalf("audit %+v", entries)
	}
}

// BT08 ("20 件隔离、返工并复检 → 放行量按有效结果更新；重复复检不重复增加可用量；权限与标准版本可查") and
// baseline 7.5: an internal inspection finds 20 of 120 nonconforming; 100 are released; the 20 are
// disposed (rework) by a human; an extra inspection of the reworked 20 is refused, the re-inspection
// supersedes the first result and restates it (120 conforming), so 20 more may be released; a
// repeated re-inspection adds nothing.
func TestReinspectionNeverAddsBT08(t *testing.T) {
	x := newQltWorld(t)
	x.subjects.put(commitmentLine("commitment.1", "120"))
	line := subjectOf("commitment.1")
	_, err := x.inspect(x.qc, "insp.1", line, "120", "100", internal, func(c *qltapi.RecordInspection) {
		c.Sampling = qltapi.Sampling{Method: qltapi.SamplingSample, SampleSize: "32", Plan: "AQL 2.5 GII"}
	})
	x.must(err)
	_, err = x.grant(x.qc, "rel.1", line, "100")
	x.must(err)

	dispose := qltapi.DecideDisposition{IdempotencyKey: x.key(), InspectionID: "insp.1", ExpectedVersion: 1, Reason: "20 件隔离返工",
		Items: []qltapi.DispositionItem{{Action: qltapi.DispositionRework, Quantity: "20"}}}
	_, err = x.q.DecideDisposition(x.agent, dispose)
	x.want(err, kernel.CodePermissionDenied)
	wrong := dispose
	wrong.IdempotencyKey, wrong.Items = x.key(), []qltapi.DispositionItem{{Action: qltapi.DispositionRework, Quantity: "19"}}
	_, err = x.q.DecideDisposition(x.qc, wrong)
	x.want(err, kernel.CodeInvalidKnownValue)
	stale := dispose
	stale.IdempotencyKey, stale.ExpectedVersion = x.key(), 2
	_, err = x.q.DecideDisposition(x.qc, stale)
	x.want(err, kernel.CodeVersionConflict)
	d, err := x.q.DecideDisposition(x.qc, dispose)
	if err != nil || d.Status != qltapi.StatusDisposed || d.RowVersion != 2 {
		t.Fatalf("disposition %+v, %v", d, err)
	}
	if s := x.standing("commitment.1"); s.Releasable != "0" || s.Conforming != "100" {
		t.Fatalf("a disposition releases nothing: %+v", s)
	}

	_, err = x.inspect(x.qc, "insp.extra", line, "20", "20", internal)
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/inspected_quantity" {
		t.Fatalf("an extra inspection of the reworked 20: %s at %s", code(err), path(err))
	}
	r, err := x.inspect(x.qc, "insp.2", line, "120", "120", internal, supersedes("insp.1"))
	if err != nil || r.SupersedesInspectionID != "insp.1" {
		t.Fatalf("re-inspection %+v, %v", r, err)
	}
	if s := x.standing("commitment.1"); s.Inspected != "120" || s.Conforming != "120" || s.Released != "100" || s.Releasable != "20" {
		t.Fatalf("after the re-inspection %+v", s)
	}
	_, err = x.inspect(x.qc, "insp.3", line, "120", "120", internal, supersedes("insp.2"))
	x.must(err)
	_, err = x.inspect(x.qc, "insp.4", line, "120", "120", internal, supersedes("insp.1"))
	x.want(err, kernel.CodeInvalidState)
	s := x.standing("commitment.1")
	statuses := []string{}
	for _, i := range s.Inspections {
		statuses = append(statuses, i.Status)
	}
	if s.Conforming != "120" || s.Releasable != "20" || !slices.Equal(statuses, []string{qltapi.StatusSuperseded, qltapi.StatusSuperseded,
		qltapi.StatusRecorded}) {
		t.Fatalf("a repeated re-inspection adds: %+v", s)
	}
	_, err = x.grant(x.qc, "rel.2", line, "20.000001")
	x.want(err, qltapi.CodeQualityReleaseExceedsConforming)
	g, err := x.grant(x.qc, "rel.2", line, "20")
	x.must(err)
	rv, err := x.q.GetRelease(x.qc, g.ObjectID)
	x.must(err)
	if rv.Conforming != "120" || rv.ReleasedBefore != "100" || !slices.Equal(rv.InspectionIDs, []string{"insp.3"}) {
		t.Fatalf("release judged on %+v", rv)
	}
	first, err := x.q.GetInspection(x.qc, "insp.1")
	x.must(err)
	if first.Status != qltapi.StatusSuperseded || first.RowVersion != 3 || first.SupersededBy != "insp.2" || first.Disposition == nil ||
		first.Disposition.Items[0] != (qltapi.DispositionItem{Action: qltapi.DispositionRework, Quantity: "20"}) ||
		first.Sampling != (qltapi.Sampling{Method: qltapi.SamplingSample, SampleSize: "32", Plan: "AQL 2.5 GII"}) ||
		first.Inspector != (qltapi.InspectorView{Kind: qltapi.InspectorInternal, ActorID: "qc.1"}) || len(first.Standard.ContentSHA256) != 64 {
		t.Fatalf("the first inspection keeps its history: %+v", first)
	}
	if e := x.qltEvents(qltapi.EventDispositionDecided); len(e) != 1 || e[0].Payload["nonconforming_quantity"] != "20" {
		t.Fatalf("disposition events %v", e)
	}
}

// The database keeps the records as the commands left them, whatever a caller sends it.
func TestQLTRecordsAreImmutable(t *testing.T) {
	x := newQltWorld(t)
	x.released("commitment.1", "120")
	for _, sql := range []string{
		`UPDATE brant_qlt.inspection SET conforming = 121, inspected = 121 WHERE inspection_id = 'insp.commitment.1'`,
		`UPDATE brant_qlt.inspection SET reason = 'x' WHERE inspection_id = 'insp.commitment.1'`,
		`DELETE FROM brant_qlt.inspection WHERE inspection_id = 'insp.commitment.1'`,
		`UPDATE brant_qlt.quality_release SET quantity = 1 WHERE release_id = 'rel.commitment.1'`,
		`UPDATE brant_qlt.quality_release SET valid_until = valid_until + interval '1 day' WHERE release_id = 'rel.commitment.1'`,
		`DELETE FROM brant_qlt.quality_release WHERE release_id = 'rel.commitment.1'`,
		`INSERT INTO brant_qlt.quality_release SELECT tenant_id, company_id, 'rel.copy', status, row_version, subject_kind, subject_id,
			subject_line_id, subject_revision, subject_hash, supplier_id, order_id, order_revision, order_hash, order_line_id, sku_id,
			spec_revision, unit_id, unit_revision, subject_quantity, quantity, conforming, released_before, inspection_ids, valid_from,
			valid_until, evidence_ids, request_hash, owner_actor_id, owner_team_id, granted_by_kind, granted_by_id, grant_epoch, granted_at
			FROM brant_qlt.quality_release WHERE release_id = 'rel.commitment.1'`,
	} {
		// The application role holds no privilege for it, or the trigger refuses it; the schema owner
		// is refused by the triggers.
		if err := x.exec("company.A", sql); err == nil || !strings.Contains(err.Error(), "brant_qlt") && !strings.Contains(err.Error(), "42501") {
			t.Errorf("as brant_app %s: %v", sql, err)
		}
		if err := x.asOwner("company.A", sql); err == nil || !strings.Contains(err.Error(), "brant_qlt") {
			t.Errorf("as the owner %s: %v", sql, err)
		}
	}
	if s := x.standing("commitment.1"); s.Conforming != "120" || s.Released != "120" {
		t.Fatalf("after the refused edits %+v", s)
	}
}

// asOwner runs sql as the schema owner (brant_migrator) in company's scope and rolls it back.
func (w *world) asOwner(company, sql string) error {
	w.t.Helper()
	ctx := context.Background()
	tx, err := pgtest.MigratorPool(w.t).Begin(ctx)
	if err != nil {
		w.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	s := w.scope(company)
	if _, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true), set_config('app.company_id', $2, true)`, string(s.Tenant),
		string(s.Company)); err != nil {
		w.t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, sql); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "SET CONSTRAINTS ALL IMMEDIATE")
	return err
}

// One subject's quality commands commit one at a time (lock order: the release root, then the
// subject's inspections FOR UPDATE): two first inspections of 120 of a 120 line, or two releases of
// all of its 120 conforming, have exactly one winner each round.
func TestSubjectCommandsSerialize(t *testing.T) {
	x := newQltWorld(t)
	for round := range 3 {
		id := "commitment.race." + string(rune('a'+round))
		x.subjects.put(commitmentLine(id, "120"))
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for n := range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[n] = x.inspect(x.qc, "insp."+id+"."+string(rune('0'+n)), subjectOf(id), "120", "120")
			}()
		}
		wg.Wait()
		ok := 0
		for _, err := range errs {
			switch code(err) {
			case "OK":
				ok++
			case kernel.CodeInvalidKnownValue, kernel.CodeVersionConflict:
			default:
				t.Fatalf("round %d: inspection %v", round, err)
			}
		}
		if s := x.standing(id); ok != 1 || s.Inspected != "120" || len(s.Inspections) != 1 {
			t.Fatalf("round %d: %d inspections succeeded, standing %+v", round, ok, s)
		}
		for n := range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[n] = x.grant(x.qc, "rel."+id+"."+string(rune('0'+n)), subjectOf(id), "120")
			}()
		}
		wg.Wait()
		ok = 0
		for _, err := range errs {
			switch code(err) {
			case "OK":
				ok++
			case qltapi.CodeQualityReleaseExceedsConforming:
			default:
				t.Fatalf("round %d: grant %v", round, err)
			}
		}
		if s := x.standing(id); ok != 1 || s.Released != "120" || s.Releasable != "0" {
			t.Fatalf("round %d: %d grants succeeded, standing %+v", round, ok, s)
		}
	}
}
