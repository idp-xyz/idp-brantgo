//go:build integration

package bootstrap_test

import (
	"reflect"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The POL additions of S5 (owner decision 2026-11-12; s5-plan A3a), the prerequisite of S5-07,
// S5-08, S5-12, S5-18 and CN-AT12, against PostgreSQL 16 and migrations/pol/000005. Python V0.50 has
// nothing for S5 (s5-plan §7), so the tests cite design IDs.

func shipmentTemplate(kind, language string, sections []string, clauses ...string) polapi.ConfigData {
	return polapi.ConfigData{ShipmentDocumentTemplate: &polapi.ShipmentDocumentTemplate{Name: "synthetic " + kind, DocumentKind: kind,
		Language: language, RendererVersion: polapi.ShipmentRendererVersion, Title: "SYNTHETIC " + kind, Sections: sections, Clauses: clauses,
		FooterNote: "SYNTHETIC ONLY"}}
}

func exportTax(method, review string) polapi.ConfigData {
	return polapi.ConfigData{ExportTaxPolicy: &polapi.ExportTaxPolicy{Name: "synthetic export tax policy", RefundMethod: method,
		PolicyBasis: "synthetic policy basis, not a tax determination", ProfessionalReviewEvidenceID: review}}
}

func numberSequence(documentType, prefix string) polapi.ConfigData {
	return polapi.ConfigData{NumberSequence: &polapi.NumberSequence{DocumentType: documentType, Prefix: prefix, Width: 5,
		Reset: polapi.ResetAnnual, TimeZone: "Asia/Shanghai"}}
}

// Every S5 addition is saved, independently published, resolved for downstream contexts and
// re-checked by ConfigGuard v1 inside a downstream transaction like every kind: the E-2 credit
// policy (STL-INV-004/005, S5 D-6), payment terms paid against the documents (S5 D-5, G0 F-04),
// credit and settlement rates (STL-INV-004, STL-INV-007), shipment document templates with the
// commercial invoice's and the packing list's own number sequences (WBS S5-08, D-12), and the export
// tax policy, published only on its cited professional review (CN-D02, CN-AT12).
func TestS5KindsPublishResolveAndGuard(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	win := newWindow()
	w.currency(editor, reviewer, "USD", win)
	w.currency(editor, reviewer, "CNY", win)
	var refs []polapi.ConfigRef

	e2 := bootstrap.SyntheticCreditPolicy()
	credit := w.configure(editor, reviewer, polapi.KindCreditPolicy, "credit", 0, win.from, time.Time{}, polapi.ConfigData{CreditPolicy: &e2}, true)
	incomplete := bootstrap.SyntheticCreditPolicy()
	incomplete.DecisionValidityDays = 0
	_, err := w.saveConfig(editor, polapi.KindCreditPolicy, "credit.incomplete", 0, win.from, time.Time{},
		polapi.ConfigData{CreditPolicy: &incomplete})
	if code(err) != kernel.CodeSchemaInvalid || path(err) != "/args/data/decision_validity_days" {
		t.Fatalf("a credit policy without a decision validity: %v", err)
	}
	refs = append(refs, credit)

	stages := []polapi.PaymentStage{{StageID: "deposit", Percent: "30", Trigger: polapi.TriggerOrderConfirmed},
		{StageID: "balance", Percent: "70", Trigger: polapi.TriggerAfterShipment, ReleaseControl: polapi.ReleaseControlDocuments}}
	cad := w.configure(editor, reviewer, polapi.KindPaymentTerms, "payment.cad", 0, win.from, time.Time{},
		polapi.ConfigData{PaymentTerms: &polapi.PaymentTerms{Name: "30% deposit, 70% against B/L copy", Stages: stages}}, true)
	early := []polapi.PaymentStage{stages[0], {StageID: "balance", Percent: "70", Trigger: polapi.TriggerBeforeShipment,
		ReleaseControl: polapi.ReleaseControlDocuments}}
	_, err = w.saveConfig(editor, polapi.KindPaymentTerms, "payment.early", 0, win.from, time.Time{},
		polapi.ConfigData{PaymentTerms: &polapi.PaymentTerms{Name: "early", Stages: early}})
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/data/stages/1/release_control" {
		t.Fatalf("documents control on a stage due before shipment: %v", err)
	}
	refs = append(refs, cad)

	rate := func(id, purpose string) polapi.ConfigRef {
		return w.configure(editor, reviewer, polapi.KindFXRate, id, 0, win.from, win.until, polapi.ConfigData{FXRate: &polapi.FXRate{
			FromCurrency: "CNY", ToCurrency: "USD", Rate: "0.14", Purpose: purpose, SourceName: "SYNTHETIC RATE",
			ObservedAt: kernel.FormatTimestamp(win.from)}}, true)
	}
	creditFX, settlementFX := rate("cny.usd.credit", polapi.PurposeCredit), rate("cny.usd.settlement", polapi.PurposeSettlement)
	for _, c := range []struct {
		query polapi.FXQuery
		want  string
	}{
		{fxQuery("CNY", "USD", polapi.PurposeCredit, &creditFX), "OK"},
		{fxQuery("CNY", "USD", polapi.PurposeSettlement, &settlementFX), "OK"},
		{fxQuery("CNY", "USD", polapi.PurposeSettlement, &creditFX), polapi.CodeFXScopeMismatch + " /purpose"},
		{fxQuery("CNY", "USD", polapi.PurposeCredit, &settlementFX), polapi.CodeFXScopeMismatch + " /purpose"},
		// STL-INV-007: a settlement rate applies in its direction only, never inverted.
		{fxQuery("USD", "CNY", polapi.PurposeSettlement, &settlementFX), polapi.CodeFXScopeMismatch + " /rate"},
		{fxQuery("CNY", "USD", polapi.PurposeSettlement, nil), polapi.CodeFXScopeMismatch + " /rate"},
	} {
		got, err := w.c.POL.LookupFX(editor, c.query)
		if v := code(err); v != "OK" {
			v += " " + path(err)
			if v != c.want {
				t.Errorf("%+v: %s, want %s", c.query, v, c.want)
			}
		} else if c.want != "OK" || got.Value.Rate().String() != "0.14" || got.Value.From().Code() != "CNY" {
			t.Errorf("%+v: %+v, want %s", c.query, got, c.want)
		}
	}
	// A FoundationSnapshot is for a calculation: never of purpose credit, and a credit rate is not
	// a cost estimate's rate.
	_, err = w.c.POL.CaptureSnapshot(editor, polapi.CaptureSnapshot{IdempotencyKey: w.key(), SnapshotID: "credit", Purpose: polapi.PurposeCredit,
		Refs: []polapi.ConfigRef{creditFX}})
	if code(err) != kernel.CodeSchemaInvalid || path(err) != "/args/purpose" {
		t.Fatalf("a credit snapshot: %v", err)
	}
	_, err = w.c.POL.CaptureSnapshot(editor, polapi.CaptureSnapshot{IdempotencyKey: w.key(), SnapshotID: "estimate", Purpose: polapi.PurposeCostEstimate,
		Refs: []polapi.ConfigRef{creditFX}})
	if code(err) != polapi.CodeFXScopeMismatch || path(err) != "/args/refs/0" {
		t.Fatalf("a credit rate in a cost estimate: %v", err)
	}
	refs = append(refs, creditFX, settlementFX)

	ciSections := []string{"seller", "buyer", "consignee", "identity", "shipment", "lines", "totals", "terms", "marks", "clauses", "notes"}
	plSections := []string{"seller", "buyer", "identity", "shipment", "packages", "totals", "marks", "notes"}
	ci := w.configure(editor, reviewer, polapi.KindShipmentDocumentTemplate, "ci.en", 0, win.from, time.Time{},
		shipmentTemplate(polapi.ShipmentDocumentCommercialInvoice, polapi.LanguageEn, ciSections, "We certify that the goods are of Chinese origin."), true)
	pl := w.configure(editor, reviewer, polapi.KindShipmentDocumentTemplate, "pl.zh", 0, win.from, time.Time{},
		shipmentTemplate(polapi.ShipmentDocumentPackingList, polapi.LanguageZhCN, plSections), true)
	_, err = w.saveConfig(editor, polapi.KindShipmentDocumentTemplate, "pl.lines", 0, win.from, time.Time{},
		shipmentTemplate(polapi.ShipmentDocumentPackingList, polapi.LanguageEn, append([]string{"lines"}, plSections...)))
	if code(err) != kernel.CodeSchemaInvalid || path(err) != "/args/data/sections/0" {
		t.Fatalf("a packing list with invoice lines: %v", err)
	}
	ciSeq := w.configure(editor, reviewer, polapi.KindNumberSequence, "ci.sequence", 0, win.from, time.Time{},
		numberSequence(polapi.DocumentTypeInvoice, "CI-"), true)
	plSeq := w.configure(editor, reviewer, polapi.KindNumberSequence, "pl.sequence", 0, win.from, time.Time{},
		numberSequence(polapi.DocumentTypePackingList, "PL-"), true)
	year := quoteYear(t)
	for seq, want := range map[polapi.ConfigRef]string{ciSeq: "CI-" + year + "-00001", plSeq: "PL-" + year + "-00001"} {
		// The CI and the PL of one shipment are numbered by counters of their own.
		issued, err := w.c.POL.IssueNumber(editor, polapi.IssueNumber{IdempotencyKey: w.key(), Sequence: seq, BusinessRef: "shipment.1"})
		if err != nil || issued.ObjectID != want {
			t.Fatalf("%s: %+v %v", seq.ConfigID, issued, err)
		}
	}
	refs = append(refs, ci, pl, plSeq)

	w.source(editor, "tax.review", "合成专业复核结论：免退（仅供测试）")
	_, err = w.saveConfig(editor, polapi.KindExportTaxPolicy, "export.tax", 0, win.from, time.Time{}, exportTax(polapi.RefundExemptRefund, "tax.review"))
	w.must(err)
	_, err = w.publish(reviewer, polapi.KindExportTaxPolicy, "export.tax", 1, 1)
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/evidence_ids" {
		t.Fatalf("a publication that does not cite the professional review: %v", err)
	}
	published, err := w.c.POL.Publish(reviewer, polapi.Publish{IdempotencyKey: w.key(), Kind: polapi.KindExportTaxPolicy, ConfigID: "export.tax",
		ExpectedVersion: 1, Revision: 1, EvidenceIDs: []string{"evidence", "tax.review"}, Reason: "核实合成退税政策"})
	w.must(err)
	tax := polapi.ConfigRef{Kind: polapi.KindExportTaxPolicy, ConfigID: "export.tax", Revision: 1}
	refs = append(refs, tax)

	resolved, err := w.c.POL.ResolveConfigs(editor, refs, time.Now())
	w.must(err)
	if r := resolved[credit]; r.Data.CreditPolicy == nil || !reflect.DeepEqual(*r.Data.CreditPolicy, e2) {
		t.Fatalf("the credit policy reads back as %+v", r.Data.CreditPolicy)
	}
	if r := resolved[cad]; !reflect.DeepEqual(r.Data.PaymentTerms.Stages, stages) {
		t.Fatalf("the payment terms read back as %+v", r.Data.PaymentTerms)
	}
	if r := resolved[pl]; r.Data.ShipmentDocumentTemplate == nil || !reflect.DeepEqual(r.Data.ShipmentDocumentTemplate.Sections, plSections) ||
		len(r.Data.ShipmentDocumentTemplate.Clauses) != 0 {
		t.Fatalf("the packing list template reads back as %+v", r.Data.ShipmentDocumentTemplate)
	}
	if r := resolved[tax]; r.Data.ExportTaxPolicy == nil || *r.Data.ExportTaxPolicy != *exportTax(polapi.RefundExemptRefund, "tax.review").ExportTaxPolicy {
		t.Fatalf("the export tax policy reads back as %+v", r.Data.ExportTaxPolicy)
	}
	d := newDownstream(w)
	for _, ref := range refs {
		r := resolved[ref]
		v, err := d.guard(w.scope("company.A"), ref, time.Now())
		if err != nil || r.Code != "" || !v.InForce() || v.ContentHash != r.ContentHash || len(r.ContentHash) != 64 {
			t.Fatalf("%+v: resolved %s %s, guard %+v %v", ref, r.Code, r.ContentHash, v, err)
		}
	}
	_, err = w.c.POL.Retire(reviewer, polapi.Retire{IdempotencyKey: w.key(), Kind: polapi.KindExportTaxPolicy, ConfigID: "export.tax",
		ExpectedVersion: published.RowVersion, Reason: "合成政策停用"})
	w.must(err)
	if v, err := d.guard(w.scope("company.A"), tax, time.Now()); err != nil || v.Check("/args/export_tax_policy") == nil ||
		v.Code != polapi.CodeConfigNotActive {
		t.Fatalf("a retired export tax policy: %+v %v", v, err)
	}
}

// CN-AT12 (POL half; the CNX half is C3b): the applicable policy changes while historical business
// is open. A new export_tax_policy revision supersedes the old one, which keeps its method, content
// hash and evidence, and is still the revision in force at an export date before the change; the
// new revision is in force only from its publication (CN-R08: 适用方法变化不追溯覆盖历史).
func TestExportTaxPolicyChangeKeepsTheRevisionOfOldExports(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	win := newWindow()
	w.source(editor, "tax.review.1", "合成专业复核结论一")
	w.source(editor, "tax.review.2", "合成专业复核结论二")
	publish := func(expected, revision int64, review string) {
		_, err := w.c.POL.Publish(reviewer, polapi.Publish{IdempotencyKey: w.key(), Kind: polapi.KindExportTaxPolicy, ConfigID: "export.tax",
			ExpectedVersion: expected, Revision: revision, EvidenceIDs: []string{review}, Reason: "核实合成退税政策"})
		w.must(err)
	}
	_, err := w.saveConfig(editor, polapi.KindExportTaxPolicy, "export.tax", 0, win.from, time.Time{}, exportTax(polapi.RefundExemptRefund, "tax.review.1"))
	w.must(err)
	publish(1, 1, "tax.review.1")
	before, err := w.c.POL.GetConfig(editor, polapi.KindExportTaxPolicy, "export.tax")
	w.must(err)
	exportDate := before.Revisions[0].PublishedAt
	_, err = w.saveConfig(editor, polapi.KindExportTaxPolicy, "export.tax", 2, win.from, time.Time{},
		exportTax(polapi.RefundExemptCreditRefund, "tax.review.2"))
	w.must(err)
	publish(3, 2, "tax.review.2")

	old := polapi.ConfigRef{Kind: polapi.KindExportTaxPolicy, ConfigID: "export.tax", Revision: 1}
	current := polapi.ConfigRef{Kind: polapi.KindExportTaxPolicy, ConfigID: "export.tax", Revision: 2}
	for _, c := range []struct {
		ref    polapi.ConfigRef
		at     time.Time
		code   string
		method string
	}{
		{old, exportDate, "", polapi.RefundExemptRefund},
		{old, time.Now(), polapi.CodeConfigNotActive, polapi.RefundExemptRefund},
		{current, exportDate, polapi.CodeConfigNotActive, polapi.RefundExemptCreditRefund},
		{current, time.Now(), "", polapi.RefundExemptCreditRefund},
	} {
		resolved, err := w.c.POL.ResolveConfigs(editor, []polapi.ConfigRef{c.ref}, c.at)
		w.must(err)
		if r := resolved[c.ref]; r.Code != c.code || r.Data.ExportTaxPolicy.RefundMethod != c.method {
			t.Errorf("revision %d at %s: %s %+v, want %q %s", c.ref.Revision, c.at, r.Code, r.Data.ExportTaxPolicy, c.code, c.method)
		}
	}
	after, err := w.c.POL.GetConfig(editor, polapi.KindExportTaxPolicy, "export.tax")
	w.must(err)
	if r := after.Revisions[0]; r.Status != polapi.StatusSuperseded || r.ContentHash != before.Revisions[0].ContentHash ||
		!reflect.DeepEqual(r.Data, before.Revisions[0].Data) || !reflect.DeepEqual(r.EvidenceIDs, []string{"tax.review.1"}) ||
		after.ActiveRevision != 2 {
		t.Fatalf("the superseded revision %+v", after)
	}
}

// Revisions as POL stored them before S5 (main 481fa24), with the data and content hashes that code
// computed (pinned in the domain test TestRevisionsStoredBeforeS5KeepTheirDataAndHash): BRC-001's
// payment terms (30 % order confirmed, 70 % 30 days after shipment), the foundation demo's credit
// policy and a cost estimate rate.
var s3Rows = []struct{ kind, id, data, hash, until string }{
	{polapi.KindPaymentTerms, "payment.s3", `{"name":"30/70","stages":[{"stage_id":"deposit","percent":"30","trigger":"order_confirmed","days":0},` +
		`{"stage_id":"balance","percent":"70","trigger":"after_shipment","days":30}]}`,
		"d6c11b03fa478faa2d9b8da29c1d6fcca2e6cf3bcb6198574a582ae9b8f30048", ""},
	{polapi.KindCreditPolicy, "credit.s3", `{"name":"synthetic credit policy","currency":"CNY","default_limit":"0",` +
		`"limit_sources":["internal_assessment"],"approval_tiers":[{"level":"credit_officer","max_amount":"10000.00"}],"overdue_tolerance_days":30}`,
		"5b5c0de3d8b6d361a31b5a9535c05049e63417ef05418dab5ec325eedf93e764", ""},
	{polapi.KindFXRate, "usd.cny.s3", `{"from_currency":"USD","to_currency":"CNY","rate":"7.123456","purpose":"cost_estimate",` +
		`"source_name":"SYNTHETIC RATE","observed_at":"2026-10-22T00:00:00.000000Z"}`,
		"44186c3ebbb84048d0042efa2182e6b8ace16897a1d1c68c99c0a3fcff854488", "2026-10-23T00:00:00Z"},
}

// Existing configurations keep their JSON and content hash (s5-plan A3a): revisions stored before
// migrations/pol/000005 and the S5 code read back with their data and hash, are in force for
// downstream lookups and ConfigGuard v1, carry none of the S5 members, and the same content saved
// again through the S5 code hashes as before. A credit policy revision of S5 must state the
// validities, so the old shape is refused for a new revision while the old revision stays active.
func TestRevisionsStoredBeforeS5ReadBackUnchanged(t *testing.T) {
	w := newWorld(t)
	editor, _ := w.polActors()
	validFrom := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	at := validFrom.Add(9 * time.Hour)
	for _, r := range s3Rows {
		until := "NULL"
		if r.until != "" {
			until = "'" + r.until + "'"
		}
		w.must(w.exec("company.A", `INSERT INTO brant_pol.config_root (tenant_id, company_id, kind, config_id, root_version, last_revision,
			created_at, updated_at) VALUES (current_setting('app.tenant_id'), 'company.A', '`+r.kind+`', '`+r.id+`', 2, 1,
			'2026-10-22T07:00:00Z', '2026-10-22T08:00:00Z')`))
		w.must(w.exec("company.A", `INSERT INTO brant_pol.config_revision (tenant_id, company_id, kind, config_id, revision, status, data,
			content_hash, valid_from, valid_until, saved_by_kind, saved_by_id, saved_epoch, saved_at, published_by_kind, published_by_id,
			publish_epoch, published_at, evidence_ids) VALUES (current_setting('app.tenant_id'), 'company.A', '`+r.kind+`', '`+r.id+`', 1,
			'active', '`+r.data+`', '`+r.hash+`', '2026-10-22T00:00:00Z', `+until+`, 'human', 'editor', 1, '2026-10-22T07:00:00Z', 'human',
			'reviewer', 1, '2026-10-22T08:00:00Z', ARRAY['evidence'])`))
	}
	d := newDownstream(w)
	for _, r := range s3Rows {
		ref := polapi.ConfigRef{Kind: r.kind, ConfigID: r.id, Revision: 1}
		v, err := w.c.POL.GetConfig(editor, r.kind, r.id)
		w.must(err)
		if v.ActiveRevision != 1 || v.Revisions[0].ContentHash != r.hash {
			t.Fatalf("%s reads back as %+v", r.id, v)
		}
		resolved, err := w.c.POL.ResolveConfigs(editor, []polapi.ConfigRef{ref}, at)
		w.must(err)
		got := resolved[ref]
		if got.Code != "" || got.ContentHash != r.hash || !reflect.DeepEqual(got.Data, v.Revisions[0].Data) {
			t.Fatalf("%s resolves as %+v", r.id, got)
		}
		if g, err := d.guard(w.scope("company.A"), ref, at); err != nil || !g.InForce() || g.ContentHash != r.hash {
			t.Fatalf("%s guarded as %+v %v", r.id, g, err)
		}
		switch {
		case got.Data.PaymentTerms != nil:
			if s := got.Data.PaymentTerms.Stages; len(s) != 2 || s[1].ReleaseControl != "" || s[1].Days != 30 {
				t.Fatalf("stages %+v", s)
			}
		case got.Data.CreditPolicy != nil:
			if p := got.Data.CreditPolicy; p.DocumentsSecured || p.TwoApproversAboveTop || p.ReservationValidityDays != 0 || p.DecisionValidityDays != 0 {
				t.Fatalf("credit policy %+v", p)
			}
		case got.Data.FXRate == nil || got.Data.FXRate.Purpose != polapi.PurposeCostEstimate:
			t.Fatalf("rate %+v", got.Data)
		}
	}

	// The same payment terms saved again through the S5 code hash as they did.
	terms := s3Rows[0]
	stored, err := w.c.POL.GetConfig(editor, terms.kind, terms.id)
	w.must(err)
	_, err = w.saveConfig(editor, terms.kind, terms.id, 2, validFrom, time.Time{}, stored.Revisions[0].Data)
	w.must(err)
	again, err := w.c.POL.GetConfig(editor, terms.kind, terms.id)
	w.must(err)
	if len(again.Revisions) != 2 || again.Revisions[1].ContentHash != terms.hash || again.WorkingRevision != 2 {
		t.Fatalf("saved again %+v", again)
	}

	// The credit policy of S3 is not a valid new revision of S5; the stored one stays in force.
	credit := s3Rows[1]
	stored, err = w.c.POL.GetConfig(editor, credit.kind, credit.id)
	w.must(err)
	_, err = w.saveConfig(editor, credit.kind, credit.id, 2, validFrom, time.Time{}, stored.Revisions[0].Data)
	if code(err) != kernel.CodeSchemaInvalid || path(err) != "/args/data/reservation_validity_days" {
		t.Fatalf("an S3 credit policy saved under S5: %v", err)
	}
	if v, err := w.c.POL.GetConfig(editor, credit.kind, credit.id); err != nil || v.ActiveRevision != 1 || v.WorkingRevision != 0 {
		t.Fatalf("%+v %v", v, err)
	}
}
