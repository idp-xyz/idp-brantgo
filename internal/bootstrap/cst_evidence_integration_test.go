//go:build integration

package bootstrap_test

import (
	"context"
	"slices"
	"testing"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Context Map R64 EVD→CST (SourceRef v1): the human who verifies or withdraws a costing context,
// or records, supersedes or withdraws a measurement, reads every source the command cites, and
// whoever saves or verifies a draft reads the sources its assumptions cite. CST asks EVD before its
// transaction; a source the actor does not read is REFERENCE_INVALID at the entry's source_id,
// exactly as a source that does not exist, and an actor without evd.read is PERMISSION_DENIED.

// evidenceActors registers private.proof, a source of other in team legal, and returns a human
// with team data scope in trade, who sees the costing contexts and measurements of team trade but
// does not read private.proof, and a human with company data scope and every permission but
// evd.read.
func (c *costingWorld) evidenceActors() (teammate, blind context.Context) {
	c.t.Helper()
	other := c.provision(bootstrap.Grant{Scope: c.scope("company.A"), Actor: actorRef("human", "other"), Permissions: allPermissions,
		DataScope: "company", Team: "legal"})
	c.source(other, "private.proof", "独立私有核实来源")
	teammate = c.provision(bootstrap.Grant{Scope: c.scope("company.A"), Actor: actorRef("human", "teammate"), Permissions: allPermissions,
		DataScope: "team", Team: "trade"})
	blind = c.provision(bootstrap.Grant{Scope: c.scope("company.A"), Actor: actorRef("human", "blind"),
		Permissions: slices.DeleteFunc(slices.Clone(allPermissions), func(p string) bool { return p == "evd.read" }), DataScope: "company", Team: "trade"})
	return teammate, blind
}

// citing returns evidence of a readable entry, then source at index 1.
func citing(source string) []cstapi.Evidence {
	return []cstapi.Evidence{{SourceID: "src.1", Locator: "合成核价依据"}, {SourceID: source, Locator: "第二依据"}}
}

// hidden checks that err refuses the entry at entry, an evidence path, as unreadable, and that a
// source that does not exist is refused the same way, so the refusal reveals nothing.
func hidden(t *testing.T, err, missing error, entry string) {
	t.Helper()
	rej, ok := kernel.AsRejection(err)
	if !ok || rej.Code != kernel.CodeReferenceInvalid || len(rej.Fields) != 1 || rej.Fields[0].Path != entry+"/source_id" {
		t.Fatalf("private source: %v, want REFERENCE_INVALID at %s/source_id", err, entry)
	}
	other, ok := kernel.AsRejection(missing)
	if !ok || other.Code != rej.Code || other.Category != rej.Category || !slices.Equal(other.Fields, rej.Fields) {
		t.Fatalf("a private source %+v must look like a missing one %+v", rej, missing)
	}
}

// Context Map R64 for each command that cites evidence: readable evidence passes, duplicates
// included; another actor's private source is refused like a missing one and writes nothing; an
// actor without evd.read is PERMISSION_DENIED.
func TestCostingEvidenceMustBeReadable(t *testing.T) {
	c := newCostingWorld(t)
	teammate, blind := c.evidenceActors()
	assumed := func(ev []cstapi.Evidence) cstapi.ContextData {
		d := c.data()
		d.Assumptions = []cstapi.Assumption{{AssumptionID: "a.qty", Path: "/context/lines/0/quantity", ValueText: "120", Reason: "客户口头确认数量",
			Evidence: ev}}
		return d
	}
	measuredOn := func(ev []cstapi.Evidence) cstapi.MeasurementData {
		d := measured()
		d.Evidence = ev
		return d
	}
	cases := []struct {
		name  string
		setup func()
		send  func(ctx context.Context, ev []cstapi.Evidence) error
		path  string
	}{
		{"costing_context.save_draft", func() {}, func(ctx context.Context, ev []cstapi.Evidence) error {
			_, err := c.saveContext(ctx, "ev.save", 0, assumed(ev))
			return err
		}, "/args/data/assumptions/0/evidence"},
		{"costing_context.verify", func() {
			_, err := c.saveContext(c.author, "ev.verify", 0, c.data())
			c.must(err)
		}, func(ctx context.Context, ev []cstapi.Evidence) error {
			_, err := c.c.CST.VerifyContext(ctx, cstapi.VerifyContext{IdempotencyKey: c.key(), ContextID: "ev.verify", ExpectedVersion: 1,
				WorkingRevision: 1, Evidence: ev})
			return err
		}, "/args/evidence"},
		{"costing_context.withdraw", func() { c.verified("ev.withdraw", c.data()) }, func(ctx context.Context, ev []cstapi.Evidence) error {
			_, err := c.c.CST.WithdrawContext(ctx, cstapi.WithdrawContext{IdempotencyKey: c.key(), ContextID: "ev.withdraw", ExpectedVersion: 2,
				TargetRevision: 1, Reason: "原询价范围已经撤销", Evidence: ev})
			return err
		}, "/args/evidence"},
		{"measurement.record_verified", func() {}, func(ctx context.Context, ev []cstapi.Evidence) error {
			_, err := c.c.CST.RecordMeasurement(ctx, cstapi.RecordMeasurement{IdempotencyKey: c.key(), MeasurementID: "ev.record",
				Data: measuredOn(ev)})
			return err
		}, "/args/data/evidence"},
		{"measurement.supersede", func() {
			_, err := c.record("ev.supersede", measured())
			c.must(err)
		}, func(ctx context.Context, ev []cstapi.Evidence) error {
			_, err := c.c.CST.SupersedeMeasurement(ctx, cstapi.SupersedeMeasurement{IdempotencyKey: c.key(), MeasurementID: "ev.supersede",
				ExpectedVersion: 1, ActiveRevision: 1, Data: measuredOn(ev), Reason: "重新实测"})
			return err
		}, "/args/data/evidence"},
		{"measurement.withdraw", func() {
			_, err := c.record("ev.withdraw", measured())
			c.must(err)
		}, func(ctx context.Context, ev []cstapi.Evidence) error {
			_, err := c.c.CST.WithdrawMeasurement(ctx, cstapi.WithdrawMeasurement{IdempotencyKey: c.key(), MeasurementID: "ev.withdraw",
				ExpectedVersion: 1, TargetRevision: 1, Reason: "撤销旧实测", Evidence: ev})
			return err
		}, "/args/evidence"},
	}
	for _, k := range cases {
		t.Run(k.name, func(t *testing.T) {
			k.setup()
			hidden(t, k.send(teammate, citing("private.proof")), k.send(teammate, citing("no.such.source")), k.path+"/1")
			if err := k.send(blind, proof); code(err) != kernel.CodePermissionDenied {
				t.Fatalf("without evd.read: %v", err)
			}
			// Nothing was written: the same expected version still holds, and duplicate entries of
			// a readable source pass.
			if err := k.send(teammate, citing("src.1")); err != nil {
				t.Fatalf("readable evidence: %v", err)
			}
		})
	}
	// A command that cites no source asks EVD nothing and needs no evd.read.
	if _, err := c.saveContext(blind, "ev.plain", 0, c.data()); err != nil {
		t.Fatalf("a draft without assumptions: %v", err)
	}
	// The verifier reads the sources of the draft's assumptions too: the author reads private.proof
	// through its company data scope and saves; the teammate cannot verify on it, the checker can.
	_, err := c.saveContext(c.author, "ev.assumed", 0, assumed(citing("private.proof")))
	c.must(err)
	_, err = c.c.CST.VerifyContext(teammate, cstapi.VerifyContext{IdempotencyKey: c.key(), ContextID: "ev.assumed", ExpectedVersion: 1,
		WorkingRevision: 1, Evidence: proof})
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/data/assumptions/0/evidence/1/source_id" {
		t.Fatalf("verify on an assumption's private source: %v", err)
	}
	_, err = c.verify(c.checker, "ev.assumed", 1, 1, 0)
	c.must(err)
	if v := c.context("ev.assumed"); v.ActiveRevision != 1 || len(v.Revisions[0].Data.Assumptions[0].Evidence) != 2 {
		t.Fatalf("context %+v", v)
	}
}

// Context Map R64, the order of the checks (S1-D16, SUP): EVD is asked before CST's transaction,
// so a missing evd.read refuses the command before CST judges it; inside the transaction an
// unreadable source is judged after the command's state, with a document's references (an
// assumption's or a measurement's evidence before the measurement rules) and a command's own
// evidence after the document (verify).
func TestCostingEvidenceIsJudgedInOrder(t *testing.T) {
	c := newCostingWorld(t)
	teammate, blind := c.evidenceActors()
	private := citing("private.proof")
	_, err := c.c.CST.VerifyContext(blind, cstapi.VerifyContext{IdempotencyKey: c.key(), ContextID: "missing", ExpectedVersion: 1,
		WorkingRevision: 1, Evidence: proof})
	c.want(err, kernel.CodePermissionDenied)
	_, err = c.c.CST.VerifyContext(teammate, cstapi.VerifyContext{IdempotencyKey: c.key(), ContextID: "missing", ExpectedVersion: 1,
		WorkingRevision: 1, Evidence: private})
	c.want(err, kernel.CodeNotFound)
	// verify: state first, then the document, then the command's evidence.
	changed := c.data()
	changed.Lines[0].Quantity = "121"
	_, err = c.saveContext(c.author, "ev.order", 0, changed)
	c.must(err)
	_, err = c.c.CST.VerifyContext(teammate, cstapi.VerifyContext{IdempotencyKey: c.key(), ContextID: "ev.order", ExpectedVersion: 9,
		WorkingRevision: 1, Evidence: private})
	c.want(err, kernel.CodeVersionConflict)
	_, err = c.c.CST.VerifyContext(teammate, cstapi.VerifyContext{IdempotencyKey: c.key(), ContextID: "ev.order", ExpectedVersion: 1,
		WorkingRevision: 1, Evidence: private})
	c.want(err, cstapi.CodeComparisonBasisMismatch)
	// withdraw: a revision withdrawn already is INVALID_STATE whatever the evidence.
	c.verified("ev.end", c.data())
	withdraw := func(ctx context.Context, expected int64, ev []cstapi.Evidence) error {
		_, err := c.c.CST.WithdrawContext(ctx, cstapi.WithdrawContext{IdempotencyKey: c.key(), ContextID: "ev.end", ExpectedVersion: expected,
			TargetRevision: 1, Reason: "撤销", Evidence: ev})
		return err
	}
	c.must(withdraw(c.checker, 2, proof))
	c.want(withdraw(teammate, 3, private), kernel.CodeInvalidState)
	// measurements: the evidence is a reference of the data, judged before the measurement rules
	// and after the revision superseded.
	contradicting := measured()
	contradicting.DeclaredVolumeM3, contradicting.Evidence = "2", private
	_, err = c.c.CST.RecordMeasurement(teammate, cstapi.RecordMeasurement{IdempotencyKey: c.key(), MeasurementID: "ev.m", Data: contradicting})
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/data/evidence/1/source_id" {
		t.Fatalf("record: %v", err)
	}
	_, err = c.record("ev.m", measured())
	c.must(err)
	_, err = c.c.CST.SupersedeMeasurement(teammate, cstapi.SupersedeMeasurement{IdempotencyKey: c.key(), MeasurementID: "ev.m",
		ExpectedVersion: 1, ActiveRevision: 2, Data: contradicting, Reason: "重新实测"})
	c.want(err, cstapi.CodeReferenceStale)
	_, err = c.c.CST.SupersedeMeasurement(teammate, cstapi.SupersedeMeasurement{IdempotencyKey: c.key(), MeasurementID: "ev.m",
		ExpectedVersion: 1, ActiveRevision: 1, Data: contradicting, Reason: "重新实测"})
	c.want(err, kernel.CodeReferenceInvalid)
	_, err = c.c.CST.WithdrawMeasurement(teammate, cstapi.WithdrawMeasurement{IdempotencyKey: c.key(), MeasurementID: "ev.m",
		ExpectedVersion: 1, TargetRevision: 9, Reason: "撤销", Evidence: private})
	c.want(err, kernel.CodeReferenceInvalid)
	if path(err) != "/args/target_revision" {
		t.Fatalf("withdraw an unknown revision: %v", err)
	}
}

// Python test_costing.py test_history_root_rechecks_private_verification_evidence (PYT-0041), the
// verification half: another actor's source verifies a costing context while the verifier reads it
// through its company data scope; once the verifier's data scope no longer covers it, the same
// evidence is refused like a missing source. Python first attests the source
// (attest_costing_source); Go's EVD has no attestation, so readability is the whole check.
//
// Not ported: Python's history read refuses the author, narrowed to its own data, the context
// whose verification evidence it can no longer read. GetContext does not ask EVD (no Go context
// re-checks evidence on a read, as S1-D16 records for POL): the author still reads its context
// with the evidence's source ID and locator, while EVD refuses it the source.
func TestPrivateVerificationEvidenceIsRechecked(t *testing.T) {
	c := newCostingWorld(t)
	other := c.provision(bootstrap.Grant{Scope: c.scope("company.A"), Actor: actorRef("human", "other"), Permissions: allPermissions,
		DataScope: "company"})
	c.source(other, "private.proof", "独立私有核实来源")
	private := []cstapi.Evidence{{SourceID: "private.proof", Locator: "核实依据"}}
	verify := func(id string) error {
		_, err := c.c.CST.VerifyContext(c.checker, cstapi.VerifyContext{IdempotencyKey: c.key(), ContextID: id, ExpectedVersion: 1,
			WorkingRevision: 1, Evidence: private})
		return err
	}
	_, err := c.saveContext(c.author, "context.private", 0, c.data())
	c.must(err)
	c.must(verify("context.private"))
	// The checker narrowed to team trade still sees the team's costing contexts, not other's source.
	c.provision(bootstrap.Grant{Scope: c.scope("company.A"), Actor: actorRef("human", "checker"), Permissions: allPermissions,
		DataScope: "team", Team: "trade"})
	_, err = c.saveContext(c.author, "context.private.2", 0, c.data())
	c.must(err)
	refused := verify("context.private.2")
	private[0].SourceID = "no.such.source"
	hidden(t, refused, verify("context.private.2"), "/args/evidence/0")
	if v := c.context("context.private.2"); v.WorkingRevision != 1 || v.ActiveRevision != 0 || v.RootVersion != 1 {
		t.Fatalf("a refused verification changed the context: %+v", v)
	}
	c.provision(bootstrap.Grant{Scope: c.scope("company.A"), Actor: actorRef("human", "author"), Permissions: allPermissions, DataScope: "own"})
	v, err := c.c.CST.GetContext(c.author, "context.private")
	c.must(err)
	if ev := v.Revisions[0].VerificationEvidence; len(ev) != 1 || ev[0].SourceID != "private.proof" {
		t.Fatalf("history %+v", v.Revisions[0])
	}
	_, err = c.c.EVD.GetSource(c.author, "private.proof")
	c.want(err, kernel.CodeNotFound)
}
