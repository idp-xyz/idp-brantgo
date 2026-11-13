//go:build integration

package bootstrap_test

import (
	"context"
	"strings"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// S5R unit R10 on PostgreSQL 16 (simulated sign-off E0 of 2026-11-13; WBS S5R-10): CNX's customs declarations
// (S5R-D13 not_received, D36 the customs broker's service role), certificates (D12 the preferential certificate of
// origin, one per declaration), purchase invoices (D20 partial red reversal, the rollback before a filed case's
// invoice is reversed, PurchaseInvoiceRef v1; D21 the buyer of another entity; PURCHASE_INVOICE_MATCH_EXCEEDED) and
// tax refund cases (D14 a taxable export opens no case, converted_to_exemption; D23 the revision valid at the export
// date, policy_superseded and tax_refund_case.rebind_policy). Each runs on C3b's invoice world: real POL, EVD, PUR,
// STL and PTY, an in-memory FUL and fixed TRD and CAT ports. Python V0.50 has no CNX: the vectors are synthetic.

// r10Policy saves and publishes the next revision of export_tax_policy id with method, valid from..until (zero: open),
// on its own professional review, and returns its revision.
func (w *invoiceWorld) r10Policy(id, method string, from, until time.Time) int64 {
	w.t.Helper()
	review := w.source(w.author, "r10.review."+w.key(), "合成专业复核结论（S5R R10，测试数据，非税务结论）")
	var expected int64
	if v, err := w.c.POL.GetConfig(w.author, polapi.KindExportTaxPolicy, id); err == nil {
		expected = v.RootVersion
	}
	_, err := w.saveConfig(w.author, polapi.KindExportTaxPolicy, id, expected, from, until, exportTax(method, review))
	w.must(err)
	v, err := w.c.POL.GetConfig(w.author, polapi.KindExportTaxPolicy, id)
	w.must(err)
	_, err = w.c.POL.Publish(w.reviewer, polapi.Publish{IdempotencyKey: w.key(), Kind: polapi.KindExportTaxPolicy, ConfigID: id,
		ExpectedVersion: v.RootVersion, Revision: v.WorkingRevision, EvidenceIDs: []string{review}, Reason: "核实合成退税政策（S5R R10 测试）"})
	w.must(err)
	return v.WorkingRevision
}

// redPart is a red invoice number reversing quantity of line 1 of blue invoice of with the net and tax it states.
func (w *invoiceWorld) redPart(number string, of cnxapi.InvoiceFacts, quantity, net, tax string) cnxapi.InvoiceFacts {
	f := w.red(number, of)
	cny := kernel.MustCurrency("CNY", 2)
	n, err := kernel.ParseAmount(cny, net)
	w.must(err)
	x, err := kernel.ParseAmount(cny, tax)
	w.must(err)
	gross, err := n.Add(x)
	w.must(err)
	f.Lines = []cnxapi.InvoiceLine{{LineNo: 1, Description: of.Lines[0].Description, Quantity: quantity, Unit: "PCS", NetAmount: net, TaxRate: "0.13",
		TaxAmount: tax}}
	f.NetTotal, f.TaxTotal, f.GrossTotal = net, tax, gross.Amount()
	return f
}

// verifyRed imports red facts f as the purchaser and has the tax specialist verify them with the matches taken back.
func (w *invoiceWorld) verifyRed(f cnxapi.InvoiceFacts, takeBacks ...cnxapi.InvoiceMatch) (cnxapi.InvoiceReceipt, error) {
	r := w.imported(w.purchaser, f)
	return w.cnx.VerifyInvoice(w.tax, w.verifyCmd(r, f, takeBacks...))
}

func (w *invoiceWorld) plan(c cnxapi.TaxCaseReceipt) (cnxapi.TaxCaseReceipt, error) {
	return w.record(w.tax, c, cnxapi.RecordTaxResult{Stage: cnxapi.TaxPlanned, Currency: &cnxapi.CurrencyRef{ID: w.cny.ConfigID, Revision: w.cny.Revision},
		Amount: "1.00", TaxPeriod: "2026-11", Review: &cnxapi.ProfessionalReview{Reviewer: "合成税务师（测试）", EvidenceIDs: []string{w.review}}})
}

func (w *invoiceWorld) submit(c cnxapi.TaxCaseReceipt) (cnxapi.TaxCaseReceipt, error) {
	return w.record(w.tax, c, cnxapi.RecordTaxResult{Stage: cnxapi.TaxSubmitted, ExternalReference: "SB-" + w.key(), EvidenceIDs: []string{w.receipt}})
}

func allocationStatus(v cnxapi.TaxCaseView, id string) string {
	for _, a := range v.Allocations {
		if a.AllocationID == id {
			return a.Status
		}
	}
	return "none"
}

// S5R-D13 (SM-CNX-DECLARATION unknown → not_received, CNX-INV-003): a submission times out (unknown); the
// verification finds customs never received it. Only a human records not_received (PERMISSION_DENIED for an AI agent
// holding the permission), and only on the verification's evidence (INVALID_KNOWN_VALUE without), with a reason. The
// case is final — no receipt and no submission follow — and the lot's next attempt is prepared, declared and released;
// its export date opens the tax refund case. The database refuses not_received from any other state.
func TestS5RDeclarationNotReceivedThenTheNextAttemptD13(t *testing.T) {
	w := newInvoiceWorld(t)
	doc := w.declare.doc
	w.lot("lot.13", time.Time{}, "x1", "10")
	s := w.declare.submitted("lot.13", items("x1"))
	u, err := w.cnx.RecordReceipt(doc, cnxapi.RecordReceipt{IdempotencyKey: w.key(), DeclarationID: s.DeclarationID, ExpectedVersion: s.RowVersion,
		ReceiptID: "rcpt.13.unknown", Outcome: cnxapi.DeclarationUnknown, Reason: "单一窗口提交超时，未收到回执（合成）"})
	w.must(err)
	verification := w.source(doc, "r10.query.13", "合成：海关总署报关数据查询无该申报的入库记录（截图，测试数据）")
	nr := cnxapi.RecordReceipt{IdempotencyKey: w.key(), DeclarationID: u.DeclarationID, ExpectedVersion: u.RowVersion, ReceiptID: "rcpt.13.nr",
		Outcome: cnxapi.DeclarationNotReceived, Reason: "查证：海关从未收到该申报（合成）"}
	ai := w.declarationAgent("agent.decl.13")
	withEvidence := nr
	withEvidence.EvidenceIDs = []string{verification}
	_, err = w.cnx.RecordReceipt(ai, withEvidence)
	w.want(err, kernel.CodePermissionDenied)
	_, err = w.cnx.RecordReceipt(doc, nr)
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/evidence_ids" {
		t.Fatalf("not_received without its evidence: %s %s", code(err), path(err))
	}
	withEvidence.IdempotencyKey = w.key()
	got, err := w.cnx.RecordReceipt(doc, withEvidence)
	w.must(err)
	if got.Status != cnxapi.DeclarationNotReceived || got.RowVersion != u.RowVersion+1 {
		t.Fatalf("not received %+v", got)
	}
	_, err = w.cnx.RecordReceipt(doc, w.declare.receipt(got, cnxapi.DeclarationReceived, 1))
	if code(err) != kernel.CodeInvalidState || path(err) != "/args/outcome" {
		t.Fatalf("nothing follows not_received: %s %s", code(err), path(err))
	}
	_, err = w.cnx.RecordSubmission(doc, cnxapi.RecordSubmission{IdempotencyKey: w.key(), DeclarationID: got.DeclarationID, ExpectedVersion: got.RowVersion,
		OperationReference: "op.13.again", Submitter: cnxapi.Submitter{Kind: cnxapi.SubmitterOwnClerk, Name: "合成报关员（测试）"},
		SubmittedAt: kernel.FormatTimestamp(time.Now().Add(-time.Minute))})
	if code(err) != kernel.CodeInvalidState {
		t.Fatalf("the case is final; the next attempt submits: %s", code(err))
	}
	v, err := w.cnx.GetDeclaration(doc, got.DeclarationID)
	w.must(err)
	last := v.Receipts[len(v.Receipts)-1]
	if v.Status != cnxapi.DeclarationNotReceived || len(v.Receipts) != 2 || last.Outcome != cnxapi.DeclarationNotReceived ||
		last.From != cnxapi.DeclarationUnknown || len(last.Evidence) != 1 || last.Reason == "" || !v.ExportedAt.IsZero() {
		t.Fatalf("declaration %+v", v)
	}
	events := w.cnxEvents(cnxapi.EventDeclarationReceiptRecorded)
	if e := events[len(events)-1]; e["outcome"] != cnxapi.DeclarationNotReceived || e["from"] != cnxapi.DeclarationUnknown || e["external_sequence"] != nil {
		t.Fatalf("event %v", e)
	}
	next := w.declare.released("lot.13", time.Now().UTC(), "x1")
	if next.Attempt != 2 || next.DeclarationID == got.DeclarationID {
		t.Fatalf("the lot's next attempt %+v", next)
	}
	inv := w.invoice(w.facts("SYN1301", "10"))
	if _, err := w.allocate(w.tax, "lot.13", alloc("t13", "x1", inv.PurchaseInvoiceID, cnxapi.PurposeExportRefund, "10")); err != nil {
		t.Fatalf("the next attempt's release opens the case: %v", err)
	}
	if err := w.exec("company.A", `UPDATE brant_cnx.declaration_case SET status = 'not_received', receipts = receipts + 1, row_version = row_version + 1
		WHERE declaration_id = '`+next.DeclarationID+`'`); err == nil || !strings.Contains(err.Error(), "follows SM-CNX-DECLARATION") {
		t.Fatalf("not_received from released: %v", err)
	}
}

// S5R-D36 (Context Map R77 ServiceRoleRef v1; UL PTY-ServicePartyRole): a broker's submission names the verified PTY
// service role of kind customs_broker it rests on, read at the submission's instant: a revision PTY does not show is
// REFERENCE_INVALID, a forwarder's role INVALID_KNOWN_VALUE, a role not valid when the broker submitted REFERENCE_STALE;
// a customs broker's role without a route serves, and the declaration shows it.
func TestS5RBrokerSubmissionRestsOnAVerifiedCustomsBrokerD36(t *testing.T) {
	w := newInvoiceWorld(t)
	doc, ed := w.declare.doc, w.author
	w.staffParty(ed, "broker.36", []string{ptyapi.RoleSupplier})
	from, until := time.Now().Add(-time.Hour), time.Now().Add(30*24*time.Hour)
	verify := func(expected int64, id, role, origin, destination string) {
		_, err := w.c.PTY.VerifyServiceRole(ed, ptyapi.VerifyServiceRole{IdempotencyKey: w.key(), PartyID: "broker.36", ExpectedVersion: expected,
			ServiceRoleID: id, Role: role, ServiceCode: id + ".service", Origin: origin, Destination: destination, ValidFrom: kernel.FormatTimestamp(from),
			ValidUntil: kernel.FormatTimestamp(until), EvidenceIDs: []string{"evidence"}})
		w.must(err)
	}
	verify(2, "customs", ptyapi.ServiceCustomsBroker, "", "")
	verify(3, "sea", ptyapi.ServiceForwarder, "CNSHA", "DEHAM")
	w.lot("lot.36", time.Time{}, "x1", "10")
	c := w.declare.confirmed("lot.36", items("x1"))
	submit := func(role cnxapi.ServiceRoleRef, at time.Time) (cnxapi.DeclarationReceipt, error) {
		return w.cnx.RecordSubmission(doc, cnxapi.RecordSubmission{IdempotencyKey: w.key(), DeclarationID: c.DeclarationID, ExpectedVersion: c.RowVersion,
			OperationReference: "op.36." + w.key(), Submitter: cnxapi.Submitter{Kind: cnxapi.SubmitterBroker, PartyID: "broker.36", ServiceRole: &role,
				Name: "合成报关行（测试）"}, SubmittedAt: kernel.FormatTimestamp(at), EvidenceIDs: []string{w.declare.evidence}})
	}
	now := time.Now().Add(-time.Minute)
	for _, x := range []struct {
		role cnxapi.ServiceRoleRef
		at   time.Time
		want string
	}{
		{cnxapi.ServiceRoleRef{ID: "customs", Revision: 2}, now, kernel.CodeReferenceInvalid},
		{cnxapi.ServiceRoleRef{ID: "sea", Revision: 1}, now, kernel.CodeInvalidKnownValue},
		{cnxapi.ServiceRoleRef{ID: "customs", Revision: 1}, from.Add(-time.Hour), cnxapi.CodeReferenceStale},
	} {
		if _, err := submit(x.role, x.at); code(err) != x.want || path(err) != "/args/submitter/service_role" {
			t.Errorf("%+v at %s: %s %s, want %s", x.role, x.at, code(err), path(err), x.want)
		}
	}
	s, err := submit(cnxapi.ServiceRoleRef{ID: "customs", Revision: 1}, now)
	w.must(err)
	v, err := w.cnx.GetDeclaration(doc, s.DeclarationID)
	w.must(err)
	if len(v.Submissions) != 1 || v.Submissions[0].Submitter.ServiceRole == nil || *v.Submissions[0].Submitter.ServiceRole != (cnxapi.ServiceRoleRef{ID: "customs",
		Revision: 1}) || v.Submissions[0].ServiceRoleKind != ptyapi.ServiceCustomsBroker || v.Submissions[0].Submitter.PartyID != "broker.36" {
		t.Fatalf("submission %+v", v.Submissions)
	}
}

// S5R-D12 (CNX-INV-007, CNX-INV-008; UL CNX-CertificateResult): the documentation clerk registers an RCEP certificate of
// origin customs issued and an approved exporter's origin declaration as preferential certificates of origin with their
// agreement code, form and issuing body; one serves where a certificate of origin is needed (CertificateResultRef v1).
// A bundle check naming two certificates of origin for the lot one declaration declares is INVALID_KNOWN_VALUE; the
// preferential one alone verifies the requirement.
func TestS5RPreferentialCertificateAndOneOriginPerDeclarationD12(t *testing.T) {
	w := newInvoiceWorld(t)
	doc := w.declare.doc
	w.lot("lot.12", time.Now().UTC(), "x1", "10")
	_, err := w.file(doc, "r10.co.file", "rcep.pdf.txt", evdapi.MimeText, "合成 RCEP 原产地证书扫描件（测试数据）", "")
	w.must(err)
	from, until := time.Now().Add(-time.Hour), time.Now().Add(180*24*time.Hour)
	register := func(kind, number, agreement, form, body string) (cnxapi.CertificateReceipt, error) {
		return w.cnx.RegisterCertificate(doc, cnxapi.RegisterCertificate{IdempotencyKey: w.key(), CertificateKind: kind, AgreementCode: agreement,
			OriginForm: form, IssuingBody: body, Number: number, Issuer: "合成签发机构（测试）", IssuedOn: today(),
			Object: cnxapi.CertificateObject{Kind: cnxapi.ObjectExportLot, ID: "lot.12"}, Market: "DE", ValidFrom: kernel.FormatTimestamp(from),
			ValidUntil: kernel.FormatTimestamp(until), FileID: "r10.co.file", Basis: "原件与签发机构核验平台一致（合成）"})
	}
	rcep, err := register(cnxapi.CertificatePreferentialOrigin, "RCEP-012", "RCEP", cnxapi.OriginFormCertificate, cnxapi.IssuingBodyCustoms)
	w.must(err)
	declaration, err := register(cnxapi.CertificatePreferentialOrigin, "OD-012", "RCEP", cnxapi.OriginFormOriginDeclaration, "")
	w.must(err)
	plain, err := register(cnxapi.CertificateOfOrigin, "CO-012", "", "", "")
	w.must(err)
	v, err := w.cnx.GetCertificate(doc, rcep.CertificateID)
	w.must(err)
	if r := v.Revisions[0]; v.Kind != cnxapi.CertificatePreferentialOrigin || r.AgreementCode != "RCEP" || r.OriginForm != cnxapi.OriginFormCertificate ||
		r.IssuingBody != cnxapi.IssuingBodyCustoms {
		t.Fatalf("certificate %+v", v)
	}
	if d, err := w.cnx.GetCertificate(doc, declaration.CertificateID); err != nil || d.Revisions[0].IssuingBody != "" ||
		d.Revisions[0].OriginForm != cnxapi.OriginFormOriginDeclaration {
		t.Fatalf("origin declaration %+v %v", d, err)
	}
	results, err := w.cnx.ResolveCertificates(doc, []cnxapi.CertificateUse{{Ref: cnxapi.CertificateRef{CertificateID: rcep.CertificateID},
		Kind: cnxapi.CertificateOfOrigin, Target: cnxapi.ExportTarget{Kind: cnxapi.TargetExportLot, ID: "lot.12"}, At: time.Now()}})
	if err != nil || results[0].Code != "" || results[0].AgreementCode != "RCEP" {
		t.Fatalf("a preferential certificate serves a certificate of origin use: %+v %v", results, err)
	}
	scan := w.source(doc, "r10.co.scan", "合成原产地证扫描件（测试数据）")
	co := bundleRequirement("co", cnxapi.RequirementCertificateOfOrigin, cnxapi.RequirementVerified, scan)
	co.CertificateIDs = []string{rcep.CertificateID, plain.CertificateID}
	_, err = w.cnx.CheckBundle(doc, cnxapi.CheckBundle{IdempotencyKey: w.key(), ExportLotID: "lot.12", Requirements: []cnxapi.Requirement{co}})
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/requirements/0/certificate_ids/1" {
		t.Fatalf("two certificates of origin for one declaration: %s %s", code(err), path(err))
	}
	co.CertificateIDs = []string{rcep.CertificateID}
	b, err := w.cnx.CheckBundle(doc, cnxapi.CheckBundle{IdempotencyKey: w.key(), ExportLotID: "lot.12", Requirements: []cnxapi.Requirement{co},
		ExpectComplete: true})
	if err != nil || b.Status != cnxapi.BundleComplete {
		t.Fatalf("the preferential certificate verifies the requirement: %+v %v", b, err)
	}
}

// S5R-D14 (UL CNX-TaxRefundCase; CNX-INV-004): an export whose export_tax_policy revision is taxable (征税) opens no
// tax refund case (INVALID_STATE /args/policy_id). A case not declared in time converts to an exemption from expected —
// a human only, on the evidence of the missed deadline and a reason — and is final: no stage follows, nothing more is
// allocated, its evidence keeps counting on the invoice line (CN-R09). A case under an exemption has nothing to convert.
func TestS5RTaxableExportOpensNoCaseAndAConversionToExemptionD14(t *testing.T) {
	w := newInvoiceWorld(t)
	inv := w.invoice(w.facts("SYN1401", "100"))
	w.r10Policy("export.taxable", polapi.RefundTaxable, w.win.from, time.Time{})
	w.lot("lot.14t", time.Now().UTC(), "x1", "10")
	_, err := w.cnx.AllocateTaxEvidence(w.tax, cnxapi.AllocateTaxEvidence{IdempotencyKey: w.key(), ExportLotID: "lot.14t", PolicyID: "export.taxable",
		Allocations: []cnxapi.EvidenceAllocation{alloc("t14t", "x1", inv.PurchaseInvoiceID, cnxapi.PurposeExportExemption, "10")}})
	if code(err) != kernel.CodeInvalidState || path(err) != "/args/policy_id" {
		t.Fatalf("a taxable export: %s %s", code(err), path(err))
	}
	w.lot("lot.14", time.Now().UTC(), "x1", "60")
	c, err := w.allocate(w.tax, "lot.14", alloc("t14", "x1", inv.PurchaseInvoiceID, cnxapi.PurposeExportRefund, "60"))
	w.must(err)
	conv := cnxapi.RecordTaxResult{Stage: cnxapi.TaxConvertedToExemption, EvidenceIDs: []string{w.receipt}, Reason: "超过退税申报期限，依法转为免税（合成）"}
	_, err = w.record(w.agent, c, conv)
	w.want(err, kernel.CodePermissionDenied)
	got, err := w.record(w.tax, c, conv)
	w.must(err)
	if got.Status != cnxapi.TaxConvertedToExemption {
		t.Fatalf("converted %+v", got)
	}
	v := w.taxCase(c.CaseID)
	if v.Status != cnxapi.TaxConvertedToExemption || allocationStatus(v, "t14") != cnxapi.EvidenceStanding || v.PolicyAt != nil ||
		v.Results[len(v.Results)-1].Stage != cnxapi.TaxConvertedToExemption || v.Results[len(v.Results)-1].Reason == "" {
		t.Fatalf("case %+v", v)
	}
	if b := balance(w.view(inv.PurchaseInvoiceID), 1, cnxapi.PurposeExportRefund); b != "60/40" {
		t.Fatalf("the converted case's evidence keeps counting: %s", b)
	}
	_, err = w.allocate(w.tax, "lot.14", alloc("t14b", "x1", inv.PurchaseInvoiceID, cnxapi.PurposeExportExemption, "1"))
	if code(err) != kernel.CodeInvalidState || path(err) != "/args/export_lot_id" {
		t.Fatalf("a converted case takes nothing: %s %s", code(err), path(err))
	}
	if _, err = w.plan(got); code(err) != kernel.CodeInvalidState {
		t.Fatalf("nothing follows the conversion: %s", code(err))
	}
	events := w.cnxEvents(cnxapi.EventTaxRefundResultRecord)
	if e := events[len(events)-1]; e["stage"] != cnxapi.TaxConvertedToExemption || e["from"] != cnxapi.TaxExpected {
		t.Fatalf("event %v", e)
	}
	w.r10Policy("export.exempt", polapi.RefundExempt, w.win.from, time.Time{})
	w.lot("lot.14x", time.Now().UTC(), "x1", "10")
	cx, err := w.cnx.AllocateTaxEvidence(w.tax, cnxapi.AllocateTaxEvidence{IdempotencyKey: w.key(), ExportLotID: "lot.14x", PolicyID: "export.exempt",
		Allocations: []cnxapi.EvidenceAllocation{alloc("t14x", "x1", inv.PurchaseInvoiceID, cnxapi.PurposeExportExemption, "10")}})
	w.must(err)
	if _, err = w.record(w.tax, cx, conv); code(err) != kernel.CodeInvalidState || path(err) != "/args/stage" {
		t.Fatalf("an exemption has no refund to convert: %s %s", code(err), path(err))
	}
	if err := w.exec("company.A", `UPDATE brant_cnx.tax_refund_case SET status = 'expected', row_version = row_version + 1 WHERE case_id = '`+
		c.CaseID+`'`); err == nil || !strings.Contains(err.Error(), "follows SM-CNX-TAXCASE") {
		t.Fatalf("converted is final: %v", err)
	}
	// An opening case (S5 C10) binds the same way: under a taxable revision valid at its export date it opens none.
	w.r10Policy("export.taxable.hist", polapi.RefundTaxable, time.Now().AddDate(0, 0, -400), time.Time{})
	register := w.openingRegister()
	op := caseOpening("op.14", "REG-14", "223320260001400001", time.Now().Add(-time.Hour), register, inv.PurchaseInvoiceID, "5",
		cnxapi.OpeningPosition{Stage: cnxapi.TaxSubmitted, ExternalReference: "SB-OPEN-14", EvidenceIDs: []string{register}})
	op.PolicyID = "export.taxable.hist"
	if _, err = w.opening(op); code(err) != kernel.CodeInvalidState || path(err) != "/args/policy_id" {
		t.Fatalf("a taxable opening case: %s %s", code(err), path(err))
	}
}

// S5R-D23 (CNX-INV-004, CN-AT12; POL ConfigAt v1, R50): a case binds the export_tax_policy revision whose validity
// contains its export date, whenever it was published. A revision published after an export whose validity covers it
// binds a later case of that export date, and shows on the earlier, expected case as policy_superseded: the tax
// specialist rebinds it (human only; the revision ConfigAt answers, REFERENCE_STALE otherwise; recorded with its
// reason), never to a revision that refunds nothing while export_refund evidence counts; once planned, the binding no
// longer changes. An export date no revision's validity contains has none (CONFIG_NOT_ACTIVE).
func TestS5RCaseBindsTheRevisionValidAtTheExportDateAndRebindsD23(t *testing.T) {
	w := newInvoiceWorld(t)
	inv := w.invoice(w.facts("SYN2301", "1000"))
	exported := time.Now().UTC().Add(-10 * time.Minute)
	w.lot("lot.23a", exported, "a1", "100")
	a, err := w.allocate(w.tax, "lot.23a", alloc("t23a", "a1", inv.PurchaseInvoiceID, cnxapi.PurposeExportRefund, "100"))
	w.must(err)
	if v := w.taxCase(a.CaseID); a.Policy.Revision != 1 || v.PolicyAt == nil || v.PolicyAt.Revision != 1 || v.PolicySuperseded {
		t.Fatalf("lot.23a binds revision 1: %+v %+v", a.Policy, v.PolicyAt)
	}
	// A retroactive change: revision 2 (免抵退), published now, valid since before the export.
	if r := w.r10Policy("export.tax", polapi.RefundExemptCreditRefund, w.win.from, time.Time{}); r != 2 {
		t.Fatalf("revision %d", r)
	}
	w.lot("lot.23b", exported, "b1", "100")
	b, err := w.allocate(w.tax, "lot.23b", alloc("t23b", "b1", inv.PurchaseInvoiceID, cnxapi.PurposeExportRefund, "100"))
	w.must(err)
	if b.Policy.Revision != 2 || b.Policy.RefundMethod != polapi.RefundExemptCreditRefund {
		t.Fatalf("a revision published after the export binds by its validity: %+v", b.Policy)
	}
	v := w.taxCase(a.CaseID)
	if v.Policy.Revision != 1 || !v.PolicySuperseded || v.PolicyAt.Revision != 2 || v.PolicyAt.Code != "" {
		t.Fatalf("policy_superseded %+v %+v", v.Policy, v.PolicyAt)
	}
	rebind := cnxapi.RebindPolicy{IdempotencyKey: w.key(), CaseID: a.CaseID, ExpectedVersion: v.RowVersion, Revision: 1, Reason: "出口后发布的修订覆盖出口日期（合成）"}
	if _, err = w.cnx.RebindPolicy(w.tax, rebind); code(err) != cnxapi.CodeReferenceStale || path(err) != "/args/revision" {
		t.Fatalf("not the revision ConfigAt answers: %s %s", code(err), path(err))
	}
	ai := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.rebind"},
		Permissions: []string{cnxapi.CommandRebindPolicy, "cnx.read"}, DataScope: "company"})
	rebind.IdempotencyKey, rebind.Revision = w.key(), 2
	if _, err = w.cnx.RebindPolicy(ai, rebind); code(err) != kernel.CodePermissionDenied {
		t.Fatalf("human only: %s", code(err))
	}
	rebind.IdempotencyKey = w.key()
	got, err := w.cnx.RebindPolicy(w.tax, rebind)
	w.must(err)
	v = w.taxCase(a.CaseID)
	if got.RowVersion != rebind.ExpectedVersion+1 || got.Policy.Revision != 2 || v.Policy.Revision != 2 || v.Policy.RefundMethod != polapi.RefundExemptCreditRefund ||
		v.PolicySuperseded || len(v.Rebindings) != 1 || v.Rebindings[0].FromRevision != 1 || v.Rebindings[0].ToRevision != 2 ||
		v.Rebindings[0].Reason != rebind.Reason || v.Rebindings[0].CaseVersion != got.RowVersion || allocationStatus(v, "t23a") != cnxapi.EvidenceStanding {
		t.Fatalf("rebound %+v %+v", got, v)
	}
	again := rebind
	again.IdempotencyKey, again.ExpectedVersion = w.key(), got.RowVersion
	if _, err = w.cnx.RebindPolicy(w.tax, again); code(err) != kernel.CodeInvalidState || path(err) != "/args/revision" {
		t.Fatalf("it binds revision 2 already: %s %s", code(err), path(err))
	}
	// Revision 3 refunds nothing (免税) and also covers the export date: case a's export_refund evidence keeps it bound.
	w.r10Policy("export.tax", polapi.RefundExempt, w.win.from, time.Time{})
	if v = w.taxCase(a.CaseID); !v.PolicySuperseded || v.PolicyAt.Revision != 3 {
		t.Fatalf("revision 3 supersedes it: %+v", v.PolicyAt)
	}
	again.IdempotencyKey, again.Revision = w.key(), 3
	if _, err = w.cnx.RebindPolicy(w.tax, again); code(err) != kernel.CodeInvalidState || path(err) != "/args/revision" {
		t.Fatalf("export_refund evidence under a method that refunds nothing: %s %s", code(err), path(err))
	}
	// Planned: the binding no longer changes and the read no longer asks.
	planned, err := w.plan(b)
	w.must(err)
	if v = w.taxCase(b.CaseID); v.PolicyAt != nil || v.PolicySuperseded {
		t.Fatalf("a planned case %+v", v.PolicyAt)
	}
	_, err = w.cnx.RebindPolicy(w.tax, cnxapi.RebindPolicy{IdempotencyKey: w.key(), CaseID: b.CaseID, ExpectedVersion: planned.RowVersion, Revision: 3,
		Reason: "x"})
	if code(err) != kernel.CodeInvalidState || path(err) != "/args/case_id" {
		t.Fatalf("a planned case keeps its binding: %s %s", code(err), path(err))
	}
	w.lot("lot.23c", w.win.from.Add(-time.Hour), "c1", "10")
	_, err = w.allocate(w.tax, "lot.23c", alloc("t23c", "c1", inv.PurchaseInvoiceID, cnxapi.PurposeExportExemption, "1"))
	if code(err) != polapi.CodeConfigNotActive || path(err) != "/args/policy_id" {
		t.Fatalf("no revision's validity contains the export date: %s %s", code(err), path(err))
	}
}

// S5R-D20 (SM-CNX-INVOICE, CNX-INV-005, CNX-INV-004; CN-AT05; Context Map R55 PurchaseInvoiceRef v1) and the catalog's
// PURCHASE_INVOICE_MATCH_EXCEEDED: a blue invoice of 120 PCS (600.00 + 78.00 CNY) matches commitment.1's 120 and is
// allocated to an export; a 121st PCS matched by another invoice exceeds what the commitment line buys. A red invoice
// reverses 20 PCS: it must take back 20 of the match (INVALID_KNOWN_VALUE until it does); the blue invoice stays
// verified with 100 PCS of evidence, its match and the over-used allocation are under review, the plan waits; a
// replacement of 100 from the same line and the supplier's corrected invoice for the 20 (matched again: the cap counts
// what was taken back) evidence the export, which is planned and submitted. Once filed, a second red invoice waits for
// the rollback of the blue invoice's evidence in the case (INVALID_STATE /args/facts/red_of): recorded once, by a human,
// for an invoice the case uses, without moving the case. The last red invoice reverses what remains: the blue invoice is
// reversed. PurchaseInvoiceRef v1 shows STL every step: matched invoices, red/blue links, reversals, standing matches.
func TestS5RPartialRedReversalRollbackAndPurchaseInvoiceRefD20(t *testing.T) {
	w := newInvoiceWorld(t)
	blue := w.facts("SYN2001", "120")
	b := w.invoice(blue, l1("120"))
	extra := w.imported(w.purchaser, w.facts("SYN2009", "20"))
	_, err := w.cnx.VerifyInvoice(w.tax, w.verifyCmd(extra, w.facts("SYN2009", "20"), l1("1")))
	if code(err) != cnxapi.CodePurchaseInvoiceMatchExceeded || path(err) != "/args/matches/0/quantity" {
		t.Fatalf("121 matched of 120 bought: %s %s", code(err), path(err))
	}
	w.lot("lot.20", time.Now().UTC(), "x1", "120")
	c, err := w.allocate(w.tax, "lot.20", alloc("t20", "x1", b.PurchaseInvoiceID, cnxapi.PurposeExportRefund, "120"))
	w.must(err)

	red1 := w.redPart("SYN2002", blue, "20", "100.00", "13.00")
	imported1 := w.imported(w.purchaser, red1)
	if _, err = w.cnx.VerifyInvoice(w.tax, w.verifyCmd(imported1, red1)); code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/matches" {
		t.Fatalf("120 matched of the 100 evidenced: %s %s", code(err), path(err))
	}
	r1, err := w.cnx.VerifyInvoice(w.tax, w.verifyCmd(imported1, red1, l1("20")))
	w.must(err)
	bv := w.view(b.PurchaseInvoiceID)
	if r1.ReversedInvoiceID != b.PurchaseInvoiceID || bv.Status != cnxapi.InvoiceVerified || bv.RowVersion != 3 || bv.Reversal != cnxapi.ReversalPartial ||
		len(bv.ReversedByIDs) != 1 || bv.ReversedByIDs[0] != r1.PurchaseInvoiceID || bv.Matches[0].Status != cnxapi.MatchReview || bv.Matches[0].TakenBack != "20" ||
		balance(bv, 1, cnxapi.PurposeExportRefund) != "120/-20" || bv.Balances[0].Reversed != "20" {
		t.Fatalf("the partly reversed invoice %+v", bv)
	}
	if rv := w.view(r1.PurchaseInvoiceID); len(rv.Matches) != 1 || rv.Matches[0].Status != cnxapi.MatchReversal || rv.Matches[0].Quantity != "20" {
		t.Fatalf("the red invoice's take-back %+v", rv.Matches)
	}
	if v := w.taxCase(c.CaseID); allocationStatus(v, "t20") != cnxapi.EvidenceReview {
		t.Fatalf("the over-used allocation %+v", v.Allocations)
	}
	if _, err = w.plan(c); code(err) != kernel.CodeInvalidState {
		t.Fatalf("the plan waits for the review: %s", code(err))
	}
	repl := alloc("t20b", "x1", b.PurchaseInvoiceID, cnxapi.PurposeExportRefund, "101")
	repl.Replaces = "t20"
	_, err = w.allocate(w.tax, "lot.20", repl)
	w.want(err, cnxapi.CodeTaxEvidenceExceeded)
	repl.Quantity = "100"
	c, err = w.allocate(w.tax, "lot.20", repl)
	w.must(err)
	fix := w.invoice(w.facts("SYN2003", "20"), l1("20"))
	c, err = w.allocate(w.tax, "lot.20", alloc("t20c", "x1", fix.PurchaseInvoiceID, cnxapi.PurposeExportRefund, "20"))
	w.must(err)
	if v := w.taxCase(c.CaseID); allocationStatus(v, "t20") != cnxapi.EvidenceReplaced || allocationStatus(v, "t20b") != cnxapi.EvidenceStanding ||
		allocationStatus(v, "t20c") != cnxapi.EvidenceStanding {
		t.Fatalf("replaced from the same line %+v", v.Allocations)
	}
	if c, err = w.plan(c); err != nil {
		t.Fatalf("planned: %v", err)
	}
	if c, err = w.submit(c); err != nil {
		t.Fatalf("submitted: %v", err)
	}
	refs, err := w.cnx.ResolvePurchaseInvoices(w.tax, cnxapi.PurchaseInvoiceQuery{CommitmentIDs: []string{"commitment.1"}})
	w.must(err)
	byID := map[string]cnxapi.PurchaseInvoiceRef{}
	for _, r := range refs {
		byID[r.PurchaseInvoiceID] = r
	}
	if len(refs) != 3 || !byID[b.PurchaseInvoiceID].Reversed() || byID[b.PurchaseInvoiceID].Reversal != cnxapi.ReversalPartial ||
		byID[b.PurchaseInvoiceID].Lines[0].ReversedQuantity != "20" || byID[b.PurchaseInvoiceID].Lines[0].ReversedNetMinor != "10000" ||
		byID[b.PurchaseInvoiceID].Matches[0].StandingQuantity != "100" || byID[r1.PurchaseInvoiceID].RedOf != b.PurchaseInvoiceID ||
		byID[r1.PurchaseInvoiceID].Matches[0].Status != cnxapi.MatchReversal || byID[fix.PurchaseInvoiceID].Reversed() ||
		byID[fix.PurchaseInvoiceID].Matches[0].Status != cnxapi.MatchStanding || byID[b.PurchaseInvoiceID].NetMinor != "60000" {
		t.Fatalf("PurchaseInvoiceRef v1 %+v", refs)
	}

	red2 := w.redPart("SYN2004", blue, "10", "50.00", "6.50")
	r2 := w.imported(w.purchaser, red2)
	_, err = w.cnx.VerifyInvoice(w.tax, w.verifyCmd(r2, red2, l1("10")))
	if code(err) != kernel.CodeInvalidState || path(err) != "/args/facts/red_of" {
		t.Fatalf("a filed case uses the invoice: %s %s", code(err), path(err))
	}
	rollback := cnxapi.RecordTaxResult{Stage: cnxapi.TaxEvidenceRolledBack, ResultText: "进货凭证信息回退已确认（合成）", EvidenceIDs: []string{w.receipt}}
	if _, err = w.record(w.tax, c, cnxapi.RecordTaxResult{Stage: rollback.Stage, PurchaseInvoiceID: extra.PurchaseInvoiceID, ResultText: rollback.ResultText,
		EvidenceIDs: rollback.EvidenceIDs}); code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/purchase_invoice_id" {
		t.Fatalf("a rollback of an invoice the case does not use: %s %s", code(err), path(err))
	}
	rollback.PurchaseInvoiceID = b.PurchaseInvoiceID
	rolled, err := w.record(w.tax, c, rollback)
	w.must(err)
	if rolled.Status != cnxapi.TaxSubmitted || rolled.RowVersion != c.RowVersion+1 {
		t.Fatalf("a rollback keeps the case submitted %+v", rolled)
	}
	if _, err = w.record(w.tax, rolled, rollback); code(err) != kernel.CodeInvalidState || path(err) != "/args/purchase_invoice_id" {
		t.Fatalf("rolled back once: %s %s", code(err), path(err))
	}
	if _, err = w.cnx.VerifyInvoice(w.tax, w.verifyCmd(r2, red2, l1("10"))); err != nil {
		t.Fatalf("after the rollback: %v", err)
	}
	if v := w.taxCase(c.CaseID); allocationStatus(v, "t20b") != cnxapi.EvidenceReview || v.Status != cnxapi.TaxSubmitted {
		t.Fatalf("the filed case's over-used evidence is under review %+v", v.Allocations)
	}
	r3, err := w.verifyRed(w.redPart("SYN2005", blue, "90", "450.00", "58.50"))
	w.must(err)
	bv = w.view(b.PurchaseInvoiceID)
	if bv.Status != cnxapi.InvoiceReversed || bv.Reversal != cnxapi.ReversalFull || bv.ReversedByID != r3.PurchaseInvoiceID || len(bv.ReversedByIDs) != 3 ||
		bv.Matches[0].Status != cnxapi.MatchReview {
		t.Fatalf("what remained is reversed %+v", bv)
	}
	events := w.cnxEvents(cnxapi.EventInvoiceVerified)
	if e := events[len(events)-1]; e["reversal"] != cnxapi.ReversalFull || e["reversed_invoice_id"] != b.PurchaseInvoiceID {
		t.Fatalf("event %v", e)
	}
	if e := events[len(events)-2]; e["reversal"] != cnxapi.ReversalPartial {
		t.Fatalf("event %v", e)
	}
	refs, err = w.cnx.ResolvePurchaseInvoices(w.tax, cnxapi.PurchaseInvoiceQuery{InvoiceIDs: []string{b.PurchaseInvoiceID, extra.PurchaseInvoiceID}})
	w.must(err)
	if len(refs) != 1 || refs[0].Status != cnxapi.InvoiceReversed || refs[0].Reversal != cnxapi.ReversalFull || refs[0].Matches[0].StandingQuantity != "0" {
		t.Fatalf("an imported invoice is absent, the reversed one stands at 0: %+v", refs)
	}
	err = w.exec("company.A", `INSERT INTO brant_cnx.purchase_invoice_match SELECT tenant_id, company_id, '`+r3.PurchaseInvoiceID+`', line_no, commitment_id,
		order_line_id, 91, unit, supplier_id, po_number, purchase_quantity, delivered_quantity, matched_at FROM brant_cnx.purchase_invoice_match
		WHERE purchase_invoice_id = '`+b.PurchaseInvoiceID+`'`)
	if err == nil || !strings.Contains(err.Error(), "takes back more") {
		t.Fatalf("a take-back above what remains of the blue match: %v", err)
	}
}

// S5R-D21 (CNX-INV-005, CN-AT15; PARTY_ENTITY_MISMATCH's catalog note): a purchase invoice whose buyer is another entity
// is refused outright — no approved mode — and nothing is recorded; the tax specialist rejects it with the reason, and
// the supplier's reissued invoice to the company is verified and matched (the rejected one frees nothing it never took).
func TestS5RInvoiceToAnotherEntityIsRefusedAndReissuedD21(t *testing.T) {
	w := newInvoiceWorld(t)
	other := w.facts("SYN2101", "120")
	other.Buyer = cnxapi.InvoiceParty{Name: "合成关联企业（测试）", TaxID: "AFFILIATE9"}
	r := w.imported(w.purchaser, other)
	_, err := w.cnx.VerifyInvoice(w.tax, w.verifyCmd(r, other, l1("120")))
	if code(err) != cnxapi.CodePartyEntityMismatch || path(err) != "/args/facts/buyer/tax_id" {
		t.Fatalf("another entity's invoice: %s %s", code(err), path(err))
	}
	rejected, err := w.cnx.VerifyInvoice(w.tax, cnxapi.VerifyInvoice{IdempotencyKey: w.key(), PurchaseInvoiceID: r.PurchaseInvoiceID, ExpectedVersion: 1,
		Decision: cnxapi.DecisionReject, Reason: "受票主体为其他主体，供应商须重开（合成）"})
	w.must(err)
	if rejected.Status != cnxapi.InvoiceRejected {
		t.Fatalf("rejected %+v", rejected)
	}
	reissued := w.invoice(w.facts("SYN2102", "120"), l1("120"))
	if v := w.view(reissued.PurchaseInvoiceID); v.Status != cnxapi.InvoiceVerified || len(v.Matches) != 1 || v.Matches[0].Status != cnxapi.MatchStanding {
		t.Fatalf("the reissued invoice %+v", v)
	}
}

// S5R-D20 lock order on PG16: filing a case (record_result submitted) holds the case's invoices FOR SHARE after the case's
// FOR UPDATE; a red invoice's verification locks the blue invoice FOR UPDATE and then asks which filed cases still need a
// rollback. Filed first, the red invoice is INVALID_STATE; verified first, the filing follows; started together while the
// invoice row is held, they serialize on it and end in one of those, never in a red invoice verified after the filing
// without a rollback.
func TestS5RFilingAndARedReversalSerializeOnTheInvoiceD20(t *testing.T) {
	w := newInvoiceWorld(t)
	db, err := bentopg.NewDB(w.pool)
	w.must(err)
	lw := &licenseWorld{world: w.world, app: db}
	for round := range 3 {
		n := string(rune('a' + round))
		blue := w.facts("SYN30"+n+"1", "100")
		inv := w.invoice(blue)
		w.lot("lot.f"+n, time.Now().UTC(), "x1", "100")
		c, err := w.allocate(w.tax, "lot.f"+n, alloc("t.f"+n, "x1", inv.PurchaseInvoiceID, cnxapi.PurposeExportRefund, "60"))
		w.must(err)
		c, err = w.plan(c)
		w.must(err)
		red := w.redPart("SYN30"+n+"2", blue, "10", "50.00", "6.50")
		r := w.imported(w.purchaser, red)
		reverse := func() error {
			_, err := w.cnx.VerifyInvoice(w.tax2, w.verifyCmd(r, red))
			return err
		}
		switch round {
		case 0:
			_, err = w.submit(c)
			w.must(err)
			w.want(reverse(), kernel.CodeInvalidState)
		case 1:
			w.must(reverse())
			_, err = w.submit(c)
			w.must(err)
		case 2:
			pid, held, release := make(chan int32, 1), make(chan struct{}), make(chan struct{})
			holder := make(chan error, 1)
			go func() {
				holder <- lw.inApp("company.A", func(ctx context.Context, exec bentopg.Executor) error {
					var p int32
					if err := exec.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&p); err != nil {
						return err
					}
					if _, err := exec.Exec(ctx, "SELECT 1 FROM brant_cnx.purchase_invoice WHERE purchase_invoice_id = $1 FOR UPDATE",
						inv.PurchaseInvoiceID); err != nil {
						return err
					}
					pid <- p
					close(held)
					<-release
					return nil
				})
			}()
			p := <-pid
			<-held
			filed, reversed := make(chan error, 1), make(chan error, 1)
			go func() { _, err := w.submit(c); filed <- err }()
			go func() { reversed <- reverse() }()
			waitBlocked(t, w.pool, p, 2)
			close(release)
			w.must(<-holder)
			w.must(<-filed)
			switch err := <-reversed; {
			case err == nil:
				v, iv := w.taxCase(c.CaseID), w.view(r.PurchaseInvoiceID)
				var submittedAt time.Time
				for _, x := range v.Results {
					if x.Stage == cnxapi.TaxSubmitted {
						submittedAt = x.RecordedAt
					}
				}
				if !iv.DecidedAt.Before(submittedAt) {
					t.Fatalf("a red invoice verified at %s after the filing at %s", iv.DecidedAt, submittedAt)
				}
			case code(err) != kernel.CodeInvalidState:
				t.Fatalf("filed, then the red invoice: %v", err)
			}
		}
	}
}
