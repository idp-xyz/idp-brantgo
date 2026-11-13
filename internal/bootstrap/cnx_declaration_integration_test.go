//go:build integration

package bootstrap_test

import (
	"context"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	"go.idp.xyz/idp-brant/internal/contexts/cnx"
	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// CNX's customs declarations, document bundles and external reconciliation, and CNX's reading of
// FUL (S5 C3a, WBS S5-05a; tactical model CNX-AGG-DeclarationCase, CNX-AGG-DocumentBundle,
// SM-CNX-DECLARATION, CNX-INV-003, CNX-INV-006, CNX-INV-008; Context Map R36, R38, R39, R46;
// baseline CN-R05, CN-R07, CN-R11, CN-R13; CN-AT02, CN-AT03, CN-AT06, CN-AT13, CN-AT14; S5 decisions
// E-1 manual paths, D-8, D-17). Python V0.50 has no CNX: the vectors are synthetic and cite the
// design.

// errPath is the path of the first field error of an HTTP rejection body.
func errPath(out map[string]any) string {
	fields, _ := out["field_errors"].([]any)
	if len(fields) == 0 {
		return ""
	}
	f, _ := fields[0].(map[string]any)
	p, _ := f["path"].(string)
	return p
}

// ResolveDeclarationProfiles answers sku.1 r1 under CN-2026 as verified, with its declaration
// content: 棉制针织T恤衫, two elements, legal units 件 (1 per PCS) and 千克 (0.25 per PCS).
func (fixedProfiles) ResolveDeclarationProfiles(_ context.Context, refs []cnx.ProfileRef) (map[cnx.ProfileRef]cnx.DeclarationProfile, error) {
	out := map[cnx.ProfileRef]cnx.DeclarationProfile{}
	for _, r := range refs {
		if r.SkuID == "sku.1" && r.SpecRevision == 1 && r.CodeTableVersion == codeTable {
			out[r] = cnx.DeclarationProfile{ProfileID: "decl.sku.1", Revision: 1, ContentHash: strings.Repeat("c", 64), Verified: true,
				CommodityCode: "6109100021", DeclarationName: "棉制针织T恤衫",
				Elements: []cnx.DeclarationElement{{Name: "品名", Value: "T恤衫"}, {Name: "成分含量", Value: "100%棉"}},
				LegalUnits: []cnx.LegalUnit{{Unit: "件", PerBaseUnit: kernel.MustDecimal("1"), Basis: "一件即一 PCS"},
					{Unit: "千克", PerBaseUnit: kernel.MustDecimal("0.25"), Basis: "合成：实测净重"}}}
		}
	}
	return out, nil
}

// fixedOrders is a TRD port that answers the PUR world's in-memory order.1 as active at revision 1
// (the PUR and invoice worlds' orders are in-memory, not TRD's).
type fixedOrders struct{}

func (fixedOrders) ResolveOrders(_ context.Context, ids []string) (map[string]cnx.Order, error) {
	out := map[string]cnx.Order{}
	for _, id := range ids {
		if id == "order.1" {
			out[id] = cnx.Order{Binding: cnx.OrderBinding{OrderID: id, OrderRevision: 1, OrderHash: strings.Repeat("0", 64)}, Status: cnx.OrderActive,
				CustomerID: "customer.1"}
		}
	}
	return out, nil
}

// declarer runs a declaration through CNX's commands as a documentation clerk.
type declarer struct {
	t        *testing.T
	cnx      cnxapi.DeclarationCommands
	doc      context.Context
	key      func() string
	evidence string
}

func (d declarer) must(err error) {
	d.t.Helper()
	if err != nil {
		d.t.Fatal(err)
	}
}

// items is one item declaring every line.
func items(lines ...string) []cnxapi.DeclarationItem {
	it := cnxapi.DeclarationItem{ItemNo: 1}
	for _, l := range lines {
		it.Lines = append(it.Lines, cnxapi.DeclarationLine{LineID: l})
	}
	return []cnxapi.DeclarationItem{it}
}

// confirmed prepares lot's declaration of items and confirms it.
func (d declarer) confirmed(lot string, it []cnxapi.DeclarationItem) cnxapi.DeclarationReceipt {
	d.t.Helper()
	p, err := d.cnx.PrepareDeclaration(d.doc, cnxapi.PrepareDeclaration{IdempotencyKey: d.key(), ExportLotID: lot, CodeTableVersion: codeTable, Items: it})
	d.must(err)
	c, err := d.cnx.ConfirmDeclaration(d.doc, cnxapi.ConfirmDeclaration{IdempotencyKey: d.key(), DeclarationID: p.DeclarationID,
		ExpectedVersion: p.RowVersion, Version: p.Version})
	d.must(err)
	return c
}

// submitted prepares lot's declaration of items, confirms it and records its manual submission by
// our own clerk under a fresh operation reference.
func (d declarer) submitted(lot string, it []cnxapi.DeclarationItem) cnxapi.DeclarationReceipt {
	d.t.Helper()
	c := d.confirmed(lot, it)
	s, err := d.cnx.RecordSubmission(d.doc, cnxapi.RecordSubmission{IdempotencyKey: d.key(), DeclarationID: c.DeclarationID, ExpectedVersion: c.RowVersion,
		OperationReference: "op." + lot + "." + d.key(), Submitter: cnxapi.Submitter{Kind: cnxapi.SubmitterOwnClerk, Name: "合成报关员（测试）"},
		SubmittedAt: kernel.FormatTimestamp(time.Now().Add(-time.Minute)), EvidenceIDs: []string{d.evidence}})
	d.must(err)
	return s
}

// receipt is a receipt of outcome at sequence for declaration r, received now.
func (d declarer) receipt(r cnxapi.DeclarationReceipt, outcome string, sequence int64) cnxapi.RecordReceipt {
	return cnxapi.RecordReceipt{IdempotencyKey: d.key(), DeclarationID: r.DeclarationID, ExpectedVersion: r.RowVersion, ReceiptID: "rcpt." + d.key(),
		Outcome: outcome, External: &cnxapi.ExternalReceipt{Sequence: sequence, StatusCode: "SYN-" + strings.ToUpper(outcome),
			OriginalText: "合成海关回执：" + outcome + "（测试数据）", ReceivedAt: kernel.FormatTimestamp(time.Now())},
		MappingVersion: "syn-customs-status.v1", DeclarationNumber: "SYN-DECL-" + r.ExportLotID, EvidenceIDs: []string{d.evidence}}
}

// released runs lot's declaration of one item with every line to customs' release with export date
// at (the 出口日期 a tax refund case binds its policy at).
func (d declarer) released(lot string, at time.Time, lines ...string) cnxapi.DeclarationReceipt {
	d.t.Helper()
	s := d.submitted(lot, items(lines...))
	r := d.receipt(s, cnxapi.DeclarationReleased, 1)
	r.ExportedAt = kernel.FormatTimestamp(at)
	out, err := d.cnx.RecordReceipt(d.doc, r)
	d.must(err)
	return out
}

// cnxDeclarationPermissions are what an AI agent of these tests holds: every declaration and bundle
// command, so a refusal shows the human-only rule, not a missing grant.
var cnxDeclarationPermissions = []string{cnxapi.CommandPrepareDeclaration, cnxapi.CommandConfirmDeclaration, cnxapi.CommandRecordSubmission,
	cnxapi.CommandRecordReceipt, cnxapi.CommandCheckBundle, cnxapi.CommandResolveReconciliation, "cnx.read", "evd.read", "ful.read", "trd.read",
	"cat.read", "stl.read", "pol.read"}

// declarationAgent provisions an AI agent with every declaration and bundle permission.
func (w *world) declarationAgent(id string) context.Context {
	return w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: actorRef("ai", id), Permissions: cnxDeclarationPermissions, DataScope: "company"})
}

// dlot puts into the invoice world's FUL an export lot of order.1 (revision 1) whose lines are id,
// quantity and unit triples of sku.1, 8.00 USD a unit.
func (w *invoiceWorld) dlot(id string, lines ...string) {
	x := cnx.Export{Ref: cnx.ExportRef{Kind: cnxapi.TargetExportLot, ID: id}, Status: cnx.ExportLotRegistered, Version: 1, OrderID: "order.1",
		OrderRevision: 1, EndUserPartyID: "customer.1"}
	usd := kernel.MustCurrency("USD", 2)
	for i := 0; i+2 < len(lines); i += 3 {
		q := kernel.MustDecimal(lines[i+1])
		amount, err := kernel.RoundMoney(usd, q.Mul(kernel.MustDecimal("8")))
		w.must(err)
		x.Lines = append(x.Lines, cnx.ExportLine{LineID: lines[i], SkuID: "sku.1", SpecRevision: w.spec, Quantity: q, Unit: kernel.UnitCode(lines[i+2]),
			ShipmentID: "ship." + id, OrderLineID: "L1", Amount: amount})
	}
	w.ful.put(x)
}

func (w *invoiceWorld) declaration(id string) cnxapi.DeclarationView {
	w.t.Helper()
	v, err := w.cnx.GetDeclaration(w.declare.doc, id)
	w.must(err)
	return v
}

// CN-AT03 (票据品名或单位与报关资料不同) and CNX-INV-006: a lot line in dozens has no professional basis
// for its conversion into the profile's base unit: the declaration is prepared as a draft whose
// mapping is unverified — no base or legal quantity is guessed — and its confirmation is refused with
// DECLARATION_MAPPING_UNVERIFIED. With POL's unit_conversion of sku.1 (12 PCS a dozen, 受控换算) the
// revised draft (version 2) is verified, 120 件 and 30 千克 by the verified profile; the old version
// is REFERENCE_STALE, the current one is confirmed, and a confirmed declaration is not revised.
func TestCNAT03MappingWithoutAProfessionalBasisIsNotConfirmed(t *testing.T) {
	w := newInvoiceWorld(t)
	if w.dozen.ConfigID == "" {
		w.dozen = w.configure(w.author, w.reviewer, polapi.KindUnit, "DOZEN", 0, w.win.from, time.Time{},
			polapi.ConfigData{Unit: &polapi.Unit{Code: "DOZEN", Name: "打", Dimension: polapi.DimensionCount, Precision: 0}}, true)
	}
	w.dlot("lot.3", "d1", "10", "DOZEN")
	doc := w.declare.doc
	p, err := w.cnx.PrepareDeclaration(doc, cnxapi.PrepareDeclaration{IdempotencyKey: w.key(), ExportLotID: "lot.3", CodeTableVersion: codeTable,
		Items: items("d1")})
	w.must(err)
	if p.Status != cnxapi.DeclarationDraft || p.Version != 1 || p.Mapping != cnxapi.MappingUnverified || p.Attempt != 1 {
		t.Fatalf("prepared %+v", p)
	}
	v := w.declaration(p.DeclarationID)
	it := v.Versions[0].Items[0]
	if l := it.Lines[0]; it.Mapping != cnxapi.MappingUnverified || it.BaseQuantity != "" || len(it.LegalQuantities) != 0 || l.Reason != "unit_not_converted" ||
		l.Conversion.Source != "none" || l.Unit != "DOZEN" || l.ProfileID != "decl.sku.1" || it.ValueMinor != "8000" || it.CommodityCode != "6109100021" {
		t.Fatalf("unverified item %+v", it)
	}
	_, err = w.cnx.ConfirmDeclaration(doc, cnxapi.ConfirmDeclaration{IdempotencyKey: w.key(), DeclarationID: p.DeclarationID,
		ExpectedVersion: p.RowVersion, Version: 1})
	if code(err) != cnxapi.CodeDeclarationMappingUnverified || path(err) != "/args/version" {
		t.Fatalf("confirm unverified: %s %s", code(err), path(err))
	}
	conversion := w.configure(w.author, w.reviewer, polapi.KindUnitConversion, "dozen.c3a", 0, w.win.from, time.Time{}, polapi.ConfigData{
		UnitConversion: &polapi.UnitConversion{SkuID: "sku.1", SpecRevision: w.spec, FromUnit: "DOZEN", ToUnit: "PCS", Factor: "12",
			Basis: "合成包装资料：每打12件"}}, true)
	revise := cnxapi.PrepareDeclaration{IdempotencyKey: w.key(), DeclarationID: p.DeclarationID, ExpectedVersion: p.RowVersion, ExportLotID: "lot.3",
		CodeTableVersion: codeTable, Items: []cnxapi.DeclarationItem{{ItemNo: 1, Lines: []cnxapi.DeclarationLine{{LineID: "d1",
			UnitConversion: &cnxapi.UnitConversionRef{ID: conversion.ConfigID, Revision: conversion.Revision + 9}}}}}}
	_, err = w.cnx.PrepareDeclaration(doc, revise)
	if code(err) == "" || path(err) != "/args/items/0/lines/0/unit_conversion" {
		t.Fatalf("a conversion revision POL does not have: %s %s", code(err), path(err))
	}
	revise.IdempotencyKey, revise.Items[0].Lines[0].UnitConversion.Revision = w.key(), conversion.Revision
	r, err := w.cnx.PrepareDeclaration(doc, revise)
	if err != nil || r.Version != 2 || r.Mapping != cnxapi.MappingVerified || r.RowVersion != 2 || r.DeclarationID != p.DeclarationID {
		t.Fatalf("revised %+v %v", r, err)
	}
	v = w.declaration(p.DeclarationID)
	it = v.Versions[1].Items[0]
	if l := it.Lines[0]; len(v.Versions) != 2 || it.BaseQuantity != "120" || it.LegalQuantities[0].Quantity != "120" || it.LegalQuantities[1].Quantity != "30" ||
		l.Conversion.Source != "unit_conversion" || l.Conversion.ConfigID != "dozen.c3a" || l.Conversion.Factor != "12" ||
		l.LegalQuantities[1].PerBaseUnit != "0.25" || v.Versions[0].Mapping != cnxapi.MappingUnverified {
		t.Fatalf("verified version %+v", v.Versions)
	}
	_, err = w.cnx.ConfirmDeclaration(doc, cnxapi.ConfirmDeclaration{IdempotencyKey: w.key(), DeclarationID: p.DeclarationID, ExpectedVersion: 2, Version: 1})
	w.want(err, cnxapi.CodeReferenceStale)
	c, err := w.cnx.ConfirmDeclaration(doc, cnxapi.ConfirmDeclaration{IdempotencyKey: w.key(), DeclarationID: p.DeclarationID, ExpectedVersion: 2, Version: 2,
		Reason: "件与打的换算依据为 POL 受控换算"})
	if err != nil || c.Status != cnxapi.DeclarationConfirmed || c.RowVersion != 3 {
		t.Fatalf("confirm %+v %v", c, err)
	}
	revise.IdempotencyKey, revise.ExpectedVersion = w.key(), 3
	_, err = w.cnx.PrepareDeclaration(doc, revise)
	w.want(err, kernel.CodeInvalidState)
}

// CN-AT06 (改单或撤销后重复、乱序回执), CN-R05, CN-R07, CNX-INV-003: receipts keep their original text,
// mapping version and external sequence; the release (sequence 3) recorded before the acceptance (2)
// moves the case to released with customs' export date, and the late acceptance and a repeated
// release are DECLARATION_RECEIPT_STALE and change nothing; an amendment of item 1 makes version 2
// and restates the export date the tax refund case then binds; a repeated amendment is stale; the
// withdrawal clears the export date, after which no tax evidence is allocated to the lot and a new
// attempt may be prepared. Every recorded receipt publishes DeclarationReceiptRecorded.
func TestCNAT06AmendedAndWithdrawnDeclarationsWithOutOfOrderReceipts(t *testing.T) {
	w := newInvoiceWorld(t)
	inv := w.invoice(w.facts("SYN0006", "1000"))
	w.dlot("lot.6", "x1", "100", "PCS")
	d := w.declare
	s := d.submitted("lot.6", items("x1"))
	received, err := w.cnx.RecordReceipt(d.doc, d.receipt(s, cnxapi.DeclarationReceived, 1))
	w.must(err)
	exported := kernel.Instant(time.Now())
	release := d.receipt(received, cnxapi.DeclarationReleased, 3)
	release.ExportedAt = kernel.FormatTimestamp(exported)
	released, err := w.cnx.RecordReceipt(d.doc, release)
	if err != nil || released.Status != cnxapi.DeclarationReleased || !released.ExportedAt.Equal(exported) || released.DeclarationNumber != "SYN-DECL-lot.6" {
		t.Fatalf("released %+v %v", released, err)
	}
	for _, x := range []struct {
		outcome  string
		sequence int64
	}{{cnxapi.DeclarationAccepted, 2}, {cnxapi.DeclarationReleased, 3}} {
		late := d.receipt(released, x.outcome, x.sequence)
		if x.outcome == cnxapi.DeclarationReleased {
			late.ExportedAt = kernel.FormatTimestamp(exported.Add(time.Hour))
		}
		_, err = w.cnx.RecordReceipt(d.doc, late)
		if code(err) != cnxapi.CodeDeclarationReceiptStale || path(err) != "/args/external/sequence" {
			t.Fatalf("a late %s: %s %s", x.outcome, code(err), path(err))
		}
	}
	if v := w.declaration(s.DeclarationID); v.Status != cnxapi.DeclarationReleased || len(v.Receipts) != 2 || v.LastSequence != 3 ||
		!v.ExportedAt.Equal(exported) || !v.CustomsReleased || v.Receipts[1].OriginalText == "" || v.Receipts[1].MappingVersion != "syn-customs-status.v1" ||
		v.Receipts[1].From != cnxapi.DeclarationReceived || v.Receipts[1].Sequence != 3 {
		t.Fatalf("nothing changed by stale receipts %+v", v)
	}
	amendedAt := kernel.Instant(time.Now())
	amend := d.receipt(released, cnxapi.DeclarationAmended, 4)
	amend.ExportedAt, amend.AmendedItems = kernel.FormatTimestamp(amendedAt), []cnxapi.AmendedItem{{ItemNo: 1, Quantity: "90"}}
	amended, err := w.cnx.RecordReceipt(d.doc, amend)
	if err != nil || amended.Status != cnxapi.DeclarationAmended || amended.Version != 2 || !amended.ExportedAt.Equal(amendedAt) {
		t.Fatalf("amended %+v %v", amended, err)
	}
	again := d.receipt(amended, cnxapi.DeclarationAmended, 4)
	_, err = w.cnx.RecordReceipt(d.doc, again)
	w.want(err, cnxapi.CodeDeclarationReceiptStale)
	v := w.declaration(s.DeclarationID)
	if a := v.Versions[1]; len(v.Versions) != 2 || a.Kind != "amended" || a.ReceiptID != amend.ReceiptID || len(a.Amendments) != 1 ||
		a.Amendments[0] != (cnxapi.AmendmentView{ItemNo: 1, Before: "100", After: "90"}) || v.Receipts[2].Version != 2 {
		t.Fatalf("amendment version %+v", v.Versions)
	}
	c, err := w.allocate(w.tax, "lot.6", alloc("t1", "x1", inv.PurchaseInvoiceID, cnxapi.PurposeExportRefund, "90"))
	if err != nil || !w.taxCase(c.CaseID).ExportedAt.Equal(amendedAt) {
		t.Fatalf("the case binds the export date customs stated last: %+v %v", c, err)
	}
	withdrawn, err := w.cnx.RecordReceipt(d.doc, d.receipt(amended, cnxapi.DeclarationWithdrawn, 5))
	if err != nil || withdrawn.Status != cnxapi.DeclarationWithdrawn || !withdrawn.ExportedAt.IsZero() {
		t.Fatalf("withdrawn %+v %v", withdrawn, err)
	}
	_, err = w.cnx.RecordReceipt(d.doc, d.receipt(withdrawn, cnxapi.DeclarationWithdrawn, 5))
	w.want(err, cnxapi.CodeDeclarationReceiptStale)
	after := d.receipt(withdrawn, cnxapi.DeclarationReleased, 6)
	after.ExportedAt = kernel.FormatTimestamp(exported)
	_, err = w.cnx.RecordReceipt(d.doc, after)
	w.want(err, kernel.CodeInvalidState)
	w.dlot("lot.6b", "y1", "10", "PCS")
	_, err = w.allocate(w.tax, "lot.6b", alloc("t2", "y1", inv.PurchaseInvoiceID, cnxapi.PurposeExportRefund, "1"))
	if code(err) != kernel.CodeInvalidState || path(err) != "/args/export_lot_id" {
		t.Fatalf("a lot without a released declaration: %s %s", code(err), path(err))
	}
	next, err := w.cnx.PrepareDeclaration(d.doc, cnxapi.PrepareDeclaration{IdempotencyKey: w.key(), ExportLotID: "lot.6", CodeTableVersion: codeTable,
		Items: items("x1")})
	if err != nil || next.Attempt != 2 || next.DeclarationID == s.DeclarationID {
		t.Fatalf("a new attempt after the withdrawal %+v %v", next, err)
	}
	var recorded []event
	for _, e := range w.events("company.A", cnxapi.Source) {
		if e.Type == cnxapi.EventDeclarationReceiptRecorded {
			recorded = append(recorded, e)
		}
	}
	if len(recorded) != 4 || recorded[1].Payload["outcome"] != "released" || recorded[1].Payload["external_sequence"] != float64(3) ||
		recorded[3].Payload["outcome"] != "withdrawn" || recorded[3].Payload["exported_at"] != nil || recorded[2].Payload["version"] != float64(2) {
		t.Fatalf("events %+v", recorded)
	}
}

// CN-AT13 (提交超时，随后收到专业结果), CNX-INV-003, catalog EXTERNAL_RESULT_UNKNOWN: the submission times
// out and is recorded unknown with a reason; submitting again is refused while the result is unknown
// (the original business operation stays the only one; its reference is unique); the result found
// by verification (received, then accepted) is recorded; no export date exists before customs
// releases the goods, so no tax evidence is allocated ("无回执时不宣称办理成功").
func TestCNAT13SubmissionTimeoutIsVerifiedBeforeAnythingIsSubmittedAgain(t *testing.T) {
	w := newInvoiceWorld(t)
	inv := w.invoice(w.facts("SYN0013", "100"))
	w.dlot("lot.13", "x1", "50", "PCS")
	d := w.declare
	s := d.submitted("lot.13", items("x1"))
	_, err := w.cnx.RecordReceipt(d.doc, cnxapi.RecordReceipt{IdempotencyKey: w.key(), DeclarationID: s.DeclarationID, ExpectedVersion: s.RowVersion,
		ReceiptID: "rcpt.13.unknown", Outcome: cnxapi.DeclarationUnknown})
	w.want(err, kernel.CodeSchemaInvalid)
	unknown, err := w.cnx.RecordReceipt(d.doc, cnxapi.RecordReceipt{IdempotencyKey: w.key(), DeclarationID: s.DeclarationID, ExpectedVersion: s.RowVersion,
		ReceiptID: "rcpt.13.unknown", Outcome: cnxapi.DeclarationUnknown, Reason: "单一窗口提交超时，未收到回执"})
	if err != nil || unknown.Status != cnxapi.DeclarationUnknown || unknown.DeclarationNumber != "" {
		t.Fatalf("unknown %+v %v", unknown, err)
	}
	resubmit := cnxapi.RecordSubmission{IdempotencyKey: w.key(), DeclarationID: s.DeclarationID, ExpectedVersion: unknown.RowVersion,
		OperationReference: "op.13.again", Submitter: cnxapi.Submitter{Kind: cnxapi.SubmitterOwnClerk, Name: "合成报关员（测试）"},
		SubmittedAt: kernel.FormatTimestamp(time.Now().Add(-time.Second))}
	_, err = w.cnx.RecordSubmission(d.doc, resubmit)
	if code(err) != cnxapi.CodeExternalResultUnknown || path(err) != "/args/declaration_id" {
		t.Fatalf("resubmission while unknown: %s %s", code(err), path(err))
	}
	_, err = w.allocate(w.tax, "lot.13", alloc("t1", "x1", inv.PurchaseInvoiceID, cnxapi.PurposeExportRefund, "50"))
	w.want(err, kernel.CodeInvalidState)
	received, err := w.cnx.RecordReceipt(d.doc, d.receipt(unknown, cnxapi.DeclarationReceived, 1))
	w.must(err)
	accepted, err := w.cnx.RecordReceipt(d.doc, d.receipt(received, cnxapi.DeclarationAccepted, 2))
	if err != nil || accepted.Status != cnxapi.DeclarationAccepted || accepted.DeclarationNumber != "SYN-DECL-lot.13" {
		t.Fatalf("verified %+v %v", accepted, err)
	}
	v := w.declaration(s.DeclarationID)
	if len(v.Submissions) != 1 || len(v.Receipts) != 3 || v.Receipts[0].Outcome != cnxapi.DeclarationUnknown || v.Receipts[0].Sequence != 0 ||
		v.Receipts[0].Reason == "" || v.Receipts[1].From != cnxapi.DeclarationUnknown || v.CustomsReleased || !v.ExportedAt.IsZero() {
		t.Fatalf("one business operation %+v", v)
	}
	w.dlot("lot.13b", "y1", "5", "PCS")
	other := d.confirmed("lot.13b", items("y1"))
	twin := resubmit
	twin.IdempotencyKey, twin.DeclarationID, twin.ExpectedVersion = w.key(), other.DeclarationID, other.RowVersion
	twin.OperationReference = v.Submissions[0].OperationReference
	_, err = w.cnx.RecordReceipt(d.doc, d.receipt(other, cnxapi.DeclarationUnknown, 0))
	w.want(err, kernel.CodeSchemaInvalid)
	_, err = w.cnx.RecordSubmission(d.doc, twin)
	if code(err) != cnxapi.CodeBusinessKeyConflict || path(err) != "/args/operation_reference" {
		t.Fatalf("one business operation, one reference: %s %s", code(err), path(err))
	}
}

// Human-only commands (D-17; humanOnlyCommands): an AI agent holding every permission prepares a
// declaration and checks a bundle (candidates), and is refused confirmation, submission, receipt,
// a verified requirement and reconciliation with PERMISSION_DENIED; CNX's audit records each
// refusal.
func TestDeclarationHumanOnlyCommandsRefuseAnAIActor(t *testing.T) {
	w := newInvoiceWorld(t)
	ai := w.declarationAgent("agent.decl")
	w.dlot("lot.ai", "x1", "10", "PCS")
	p, err := w.cnx.PrepareDeclaration(ai, cnxapi.PrepareDeclaration{IdempotencyKey: w.key(), ExportLotID: "lot.ai", CodeTableVersion: codeTable,
		Items: items("x1")})
	if err != nil || p.Mapping != cnxapi.MappingVerified {
		t.Fatalf("AI prepares a candidate %+v %v", p, err)
	}
	if v := w.declaration(p.DeclarationID); v.OpenedBy.Kind != kernel.ActorAI || v.Versions[0].PreparedBy.ID != "agent.decl" {
		t.Fatalf("opened by %+v", v.OpenedBy)
	}
	_, err = w.cnx.ConfirmDeclaration(ai, cnxapi.ConfirmDeclaration{IdempotencyKey: w.key(), DeclarationID: p.DeclarationID, ExpectedVersion: 1, Version: 1})
	w.want(err, kernel.CodePermissionDenied)
	d := w.declare
	c, err := w.cnx.ConfirmDeclaration(d.doc, cnxapi.ConfirmDeclaration{IdempotencyKey: w.key(), DeclarationID: p.DeclarationID, ExpectedVersion: 1, Version: 1})
	w.must(err)
	_, err = w.cnx.RecordSubmission(ai, cnxapi.RecordSubmission{IdempotencyKey: w.key(), DeclarationID: c.DeclarationID, ExpectedVersion: c.RowVersion,
		OperationReference: "op.ai", Submitter: cnxapi.Submitter{Kind: cnxapi.SubmitterOwnClerk, Name: "agent"}, SubmittedAt: kernel.FormatTimestamp(time.Now())})
	w.want(err, kernel.CodePermissionDenied)
	_, err = w.cnx.RecordReceipt(ai, d.receipt(c, cnxapi.DeclarationReceived, 1))
	w.want(err, kernel.CodePermissionDenied)
	owner := cnxapi.RequirementOwner{Kind: cnxapi.OwnerActor, ID: "doc.c3a"}
	check := cnxapi.CheckBundle{IdempotencyKey: w.key(), ExportLotID: "lot.ai", Requirements: []cnxapi.Requirement{{RequirementID: "ci",
		Kind: cnxapi.RequirementCommercialInvoice, Version: "v1", Status: cnxapi.RequirementVerified, Owner: owner, EvidenceIDs: []string{d.evidence},
		Basis: "AI 比对"}}}
	_, err = w.cnx.CheckBundle(ai, check)
	if code(err) != kernel.CodePermissionDenied || path(err) != "/args/requirements/0/status" {
		t.Fatalf("an AI verification: %s %s", code(err), path(err))
	}
	check.IdempotencyKey, check.Requirements[0].Status, check.Requirements[0].Basis, check.Requirements[0].DueOn = w.key(), cnxapi.RequirementPresent, "",
		"2026-12-01"
	b, err := w.cnx.CheckBundle(ai, check)
	if err != nil || b.Status != cnxapi.BundleIncomplete || len(b.Unverified) != 1 {
		t.Fatalf("AI lists the attachment %+v %v", b, err)
	}
	_, err = w.cnx.ResolveReconciliation(ai, cnxapi.ResolveReconciliation{IdempotencyKey: w.key(), BundleID: b.BundleID, ExpectedVersion: 1,
		ReconciliationID: "rec.ai", Subject: cnxapi.ReconciliationSubject{Kind: cnxapi.SubjectExportLot, ID: "lot.ai"}, Measure: cnxapi.MeasureQuantity,
		Unit: "PCS", External: cnxapi.ReconciledValue{Value: "10", Source: "报关单", EvidenceIDs: []string{d.evidence}},
		Internal: cnxapi.ReconciledValue{Value: "10", Source: "出口批次"}, Responsibility: cnxapi.Responsibility{Kind: "none", Note: "一致"},
		Adjustment: cnxapi.Adjustment{Kind: cnxapi.AdjustmentNone}, Reason: "核对"})
	w.want(err, kernel.CodePermissionDenied)
	auditor := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: actorRef("human", "cnx.auditor"), Permissions: []string{"cnx.audit"},
		DataScope: "company"})
	audit, err := w.cnx.Audit(auditor)
	w.must(err)
	denied := 0
	for _, e := range audit {
		if e.Code == kernel.CodePermissionDenied && e.Actor.ID == "agent.decl" {
			denied++
		}
	}
	if denied != 5 {
		t.Fatalf("audited refusals %d", denied)
	}
}

// bundleRequirement is a checklist entry owned by the documentation clerk, due 2026-12-01.
func bundleRequirement(id, kind, status string, evidence ...string) cnxapi.Requirement {
	r := cnxapi.Requirement{RequirementID: id, Kind: kind, Version: "v1", Status: status, Owner: cnxapi.RequirementOwner{Kind: cnxapi.OwnerActor, ID: "doc.c3a"},
		EvidenceIDs: evidence}
	if status == cnxapi.RequirementVerified {
		r.Basis = "原件与合成资料逐项核对一致"
	} else {
		r.DueOn = "2026-12-01"
	}
	return r
}

// CN-AT14 bundle half (单证缺件或证书不适用: 列缺件、适用对象、责任人及期限；已上传文件不直接满足要求), CNX-INV-008,
// CN-R11, CN-R13: an AI agent lists the lot's requirements — the packing list missing with its owner
// (the supplier) and due date, the commercial invoice present — and the bundle is incomplete;
// DocumentBundleChecked names what is missing and what is only attached. Stating it complete while
// the invoice is only attached, or the customs declaration verified before customs released it, is
// DOCUMENT_REQUIREMENT_UNVERIFIED. Once a human verified every requirement — the released declaration
// and STL's fund record linked as a commercial allocation, never 出口收汇核销 — the bundle is complete.
// The tax specialist reconciles the declared value with the invoice value (external − internal,
// exactly), with responsibility and an approved adjustment.
func TestCNAT14BundleListsWhatIsMissingAndPresenceIsNotValidity(t *testing.T) {
	w := newInvoiceWorld(t)
	ai := w.declarationAgent("agent.bundle")
	w.dlot("lot.14", "x1", "120", "PCS")
	d := w.declare
	scan := w.source(d.doc, "ci.scan.14", "合成商业发票扫描件（测试数据）")
	pl := bundleRequirement("pl", cnxapi.RequirementPackingList, cnxapi.RequirementMissing)
	pl.Owner = cnxapi.RequirementOwner{Kind: cnxapi.OwnerParty, ID: "supplier.1"}
	first := []cnxapi.Requirement{bundleRequirement("ci", cnxapi.RequirementCommercialInvoice, cnxapi.RequirementPresent, scan), pl,
		bundleRequirement("decl", cnxapi.RequirementCustomsDeclaration, cnxapi.RequirementMissing)}
	b, err := w.cnx.CheckBundle(ai, cnxapi.CheckBundle{IdempotencyKey: w.key(), ExportLotID: "lot.14", Requirements: first})
	if err != nil || b.Status != cnxapi.BundleIncomplete || b.RowVersion != 1 || b.CheckNo != 1 || len(b.Missing) != 2 || b.Unverified[0] != "ci" {
		t.Fatalf("first check %+v %v", b, err)
	}
	_, err = w.cnx.CheckBundle(d.doc, cnxapi.CheckBundle{IdempotencyKey: w.key(), ExportLotID: "lot.14", ExpectedVersion: 1, Requirements: first,
		ExpectComplete: true})
	if code(err) != cnxapi.CodeDocumentRequirementUnverified || path(err) != "/args/requirements/0/status" {
		t.Fatalf("an attachment counted as met: %s %s", code(err), path(err))
	}
	s := d.submitted("lot.14", items("x1"))
	early := []cnxapi.Requirement{bundleRequirement("decl", cnxapi.RequirementCustomsDeclaration, cnxapi.RequirementVerified, d.evidence)}
	early[0].DeclarationID = s.DeclarationID
	_, err = w.cnx.CheckBundle(d.doc, cnxapi.CheckBundle{IdempotencyKey: w.key(), ExportLotID: "lot.14", ExpectedVersion: 1, Requirements: early})
	w.want(err, cnxapi.CodeDocumentRequirementUnverified)
	release := d.receipt(s, cnxapi.DeclarationReleased, 1)
	release.ExportedAt = release.External.ReceivedAt
	_, err = w.cnx.RecordReceipt(d.doc, release)
	w.must(err)
	fund := w.fund("fr.14", "1000.00", w.cny, w.bankCNY)
	bank := w.source(d.doc, "bank.14", "合成银行回单（测试数据）")
	all := []cnxapi.Requirement{bundleRequirement("ci", cnxapi.RequirementCommercialInvoice, cnxapi.RequirementVerified, scan),
		bundleRequirement("pl", cnxapi.RequirementPackingList, cnxapi.RequirementVerified, scan),
		bundleRequirement("decl", cnxapi.RequirementCustomsDeclaration, cnxapi.RequirementVerified, d.evidence),
		bundleRequirement("fx", cnxapi.RequirementFundRecord, cnxapi.RequirementVerified, bank)}
	all[2].DeclarationID, all[3].FundRecordIDs = s.DeclarationID, []string{fund}
	_, err = w.cnx.CheckBundle(d.doc, cnxapi.CheckBundle{IdempotencyKey: w.key(), ExportLotID: "lot.14", ExpectedVersion: 7, Requirements: all})
	w.want(err, kernel.CodeVersionConflict)
	w.dlot("lot.14b", "y1", "1", "PCS")
	foreign := d.confirmed("lot.14b", items("y1"))
	wrong := append([]cnxapi.Requirement{}, all...)
	wrong[2].DeclarationID = foreign.DeclarationID
	_, err = w.cnx.CheckBundle(d.doc, cnxapi.CheckBundle{IdempotencyKey: w.key(), ExportLotID: "lot.14", ExpectedVersion: 1, Requirements: wrong})
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/requirements/2/declaration_id" {
		t.Fatalf("another lot's declaration: %s %s", code(err), path(err))
	}
	done, err := w.cnx.CheckBundle(d.doc, cnxapi.CheckBundle{IdempotencyKey: w.key(), ExportLotID: "lot.14", ExpectedVersion: 1, Requirements: all,
		ExpectComplete: true})
	if err != nil || done.Status != cnxapi.BundleComplete || done.RowVersion != 2 || done.CheckNo != 2 || len(done.Missing) != 0 {
		t.Fatalf("complete %+v %v", done, err)
	}
	resolve := cnxapi.ResolveReconciliation{IdempotencyKey: w.key(), BundleID: done.BundleID, ExpectedVersion: 2, ReconciliationID: "rec.14",
		Subject: cnxapi.ReconciliationSubject{Kind: cnxapi.SubjectDeclaration, ID: s.DeclarationID}, Measure: cnxapi.MeasureAmount,
		Currency: &cnxapi.CurrencyRef{ID: w.cny.ConfigID, Revision: w.cny.Revision},
		External: cnxapi.ReconciledValue{Value: "960.00", Source: "报关单申报总价（FOB）", EvidenceIDs: []string{d.evidence}},
		Internal: cnxapi.ReconciledValue{Value: "958.50", Source: "商业发票金额"}, Responsibility: cnxapi.Responsibility{Kind: "internal",
			Note: "成交价与申报价口径不同"}, Adjustment: cnxapi.Adjustment{Kind: cnxapi.AdjustmentApproved, Note: "按报关口径桥接，不调整收入"},
		Reason: "申报价与商业发票价差异"}
	odd := resolve
	odd.IdempotencyKey, odd.Subject.ID = w.key(), foreign.DeclarationID
	_, err = w.cnx.ResolveReconciliation(w.tax, odd)
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/subject/id" {
		t.Fatalf("another lot's declaration: %s %s", code(err), path(err))
	}
	odd = resolve
	odd.IdempotencyKey, odd.External.Value = w.key(), "960.001"
	_, err = w.cnx.ResolveReconciliation(w.tax, odd)
	w.want(err, kernel.CodeInvalidKnownValue)
	odd = resolve
	odd.IdempotencyKey, odd.Internal.Value = w.key(), "960.00"
	_, err = w.cnx.ResolveReconciliation(w.tax, odd)
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/adjustment/kind" {
		t.Fatalf("nothing to adjust: %s %s", code(err), path(err))
	}
	r, err := w.cnx.ResolveReconciliation(w.tax, resolve)
	if err != nil || r.RowVersion != 3 || r.Status != cnxapi.BundleComplete || r.ReconciliationID != "rec.14" {
		t.Fatalf("resolved %+v %v", r, err)
	}
	if again, err := w.cnx.ResolveReconciliation(w.tax, resolve); err != nil || !again.Replayed {
		t.Fatalf("replay %+v %v", again, err)
	}
	resolve.IdempotencyKey, resolve.ExpectedVersion = w.key(), 3
	_, err = w.cnx.ResolveReconciliation(w.tax, resolve)
	w.want(err, cnxapi.CodeBusinessKeyConflict)

	v, err := w.cnx.GetBundle(w.tax, done.BundleID)
	w.must(err)
	fx := v.Requirements[3]
	if len(v.Checks) != 2 || v.Checks[0].Missing != 2 || v.Checks[0].Present != 1 || v.Checks[0].CheckedBy.Kind != kernel.ActorAI ||
		v.Checks[0].Requirements[1].Owner.ID != "supplier.1" || v.Checks[0].Requirements[1].DueOn != "2026-12-01" || v.Status != cnxapi.BundleComplete ||
		len(fx.FundRecords) != 1 || fx.FundRecords[0].Label != cnxapi.CommercialAllocation || fx.FundRecords[0].FundRecordID != fund ||
		v.Requirements[2].Declaration == nil || !v.Requirements[2].Declaration.CustomsReleased || len(v.Reconciliations) != 1 {
		t.Fatalf("bundle %+v", v)
	}
	if rc := v.Reconciliations[0]; rc.Difference != "1.5" || rc.Unit != "CNY" || rc.ExternalValue != "960" || rc.Adjustment.Kind != cnxapi.AdjustmentApproved ||
		rc.ResolvedBy.ID != "tax.1" || len(rc.ExternalEvidence) != 1 {
		t.Fatalf("reconciliation %+v", rc)
	}
	var checked []event
	for _, e := range w.events("company.A", cnxapi.Source) {
		if e.Type == cnxapi.EventDocumentBundleChecked {
			checked = append(checked, e)
		}
	}
	if len(checked) != 2 || checked[0].Payload["status"] != "incomplete" || len(checked[0].Payload["missing"].([]any)) != 2 ||
		checked[0].Payload["unverified"].([]any)[0] != "ci" || checked[1].Payload["status"] != "complete" {
		t.Fatalf("events %+v", checked)
	}
	if list, err := w.cnx.Bundles(w.tax); err != nil || len(list) != 1 || list[0].Reconciliations != 1 {
		t.Fatalf("bundles %+v %v", list, err)
	}
}

// Declarations and bundles are append-only (triggers) and confined to their company (FORCE RLS): the
// application role cannot change or delete entries; even the owner cannot move a case back, change
// its number or sequence, renumber entries, forge a reconciliation's difference or release a licence
// on a receipt of another kind; company.B sees nothing.
func TestDeclarationRecordsAreAppendOnly(t *testing.T) {
	w := newInvoiceWorld(t)
	w.dlot("lot.db", "x1", "10", "PCS")
	d := w.declare
	s := d.submitted("lot.db", items("x1"))
	release := d.receipt(s, cnxapi.DeclarationReleased, 1)
	release.ExportedAt = release.External.ReceivedAt
	_, err := w.cnx.RecordReceipt(d.doc, release)
	w.must(err)
	b, err := w.cnx.CheckBundle(d.doc, cnxapi.CheckBundle{IdempotencyKey: w.key(), ExportLotID: "lot.db",
		Requirements: []cnxapi.Requirement{bundleRequirement("pl", cnxapi.RequirementPackingList, cnxapi.RequirementMissing)}})
	w.must(err)
	_, err = w.cnx.ResolveReconciliation(w.tax, cnxapi.ResolveReconciliation{IdempotencyKey: w.key(), BundleID: b.BundleID, ExpectedVersion: 1,
		ReconciliationID: "rec.db", Subject: cnxapi.ReconciliationSubject{Kind: cnxapi.SubjectExportLot, ID: "lot.db"}, Measure: cnxapi.MeasureQuantity,
		Unit: "PCS", External: cnxapi.ReconciledValue{Value: "9", Source: "报关单数量", EvidenceIDs: []string{d.evidence}},
		Internal: cnxapi.ReconciledValue{Value: "10", Source: "出口批次"}, Responsibility: cnxapi.Responsibility{Kind: "broker", Note: "录入错误"},
		Adjustment: cnxapi.Adjustment{Kind: cnxapi.AdjustmentNone}, Reason: "数量差异待改单"})
	w.must(err)
	for _, sql := range []string{
		`UPDATE brant_cnx.declaration_receipt SET original_text = 'x'`, `DELETE FROM brant_cnx.declaration_version`,
		`UPDATE brant_cnx.declaration_case SET order_id = 'x'`, `DELETE FROM brant_cnx.document_bundle_check`,
		`UPDATE brant_cnx.external_reconciliation SET difference = 0`,
	} {
		if err := w.exec("company.A", sql); err == nil {
			t.Errorf("brant_app must not run %s", sql)
		}
	}
	owner := func(sql string) error {
		tx, err := pgtest.MigratorPool(w.t).Begin(context.Background())
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		if _, err = tx.Exec(context.Background(), `SELECT set_config('app.tenant_id', $1, true), set_config('app.company_id', 'company.A', true)`,
			w.tenant); err != nil {
			return err
		}
		if _, err = tx.Exec(context.Background(), sql); err != nil {
			return err
		}
		return tx.Commit(context.Background())
	}
	for _, x := range []struct{ sql, want string }{
		{`UPDATE brant_cnx.declaration_case SET status = 'submitted', exported_at = NULL, row_version = row_version + 1`, "follows SM-CNX-DECLARATION"},
		{`UPDATE brant_cnx.declaration_case SET declaration_number = 'OTHER', row_version = row_version + 1`, "follows SM-CNX-DECLARATION"},
		{`UPDATE brant_cnx.declaration_case SET last_sequence = NULL, row_version = row_version + 1`, "follows SM-CNX-DECLARATION"},
		{`UPDATE brant_cnx.declaration_case SET export_lot_id = 'lot.x', row_version = row_version + 1`, "follows SM-CNX-DECLARATION"},
		{`DELETE FROM brant_cnx.declaration_case`, "never deleted"},
		{`UPDATE brant_cnx.declaration_receipt SET original_text = 'forged'`, "never change"},
		{`DELETE FROM brant_cnx.declaration_receipt`, "never change"},
		{`INSERT INTO brant_cnx.declaration_receipt SELECT tenant_id, company_id, 'forged', declaration_id, 5, outcome, from_status,
			external_sequence, external_status, original_text, mapping_version, received_at, declaration_number, exported_at, version_no, evidence,
			reason, recorded_by_kind, recorded_by_id, authority_epoch, recorded_at FROM brant_cnx.declaration_receipt`, "without gaps"},
		{`UPDATE brant_cnx.document_bundle_check SET complete = true`, "never change"},
		{`INSERT INTO brant_cnx.external_reconciliation SELECT tenant_id, company_id, 'forged', bundle_id, 2, subject_kind, subject_id, measure, unit,
			external_value, internal_value, 0, external_source, internal_source, responsibility_kind, responsibility_party_id, responsibility_note,
			adjustment, adjustment_note, evidence, reason, resolved_by_kind, resolved_by_id, authority_epoch, resolved_at
			FROM brant_cnx.external_reconciliation`, "external_reconciliation_check"},
		{`UPDATE brant_cnx.document_bundle SET export_lot_id = 'lot.x', row_version = row_version + 1, checks = checks + 1`, "keeps its binding"},
	} {
		if err := owner(x.sql); err == nil || !strings.Contains(err.Error(), x.want) {
			t.Errorf("must refuse %s: %v", x.sql, err)
		}
	}
	for _, table := range []string{"brant_cnx.declaration_case", "brant_cnx.declaration_version", "brant_cnx.declaration_receipt",
		"brant_cnx.document_bundle", "brant_cnx.document_bundle_check", "brant_cnx.external_reconciliation"} {
		if n := w.count("company.B", table); n != 0 {
			t.Errorf("company.B sees %d rows of %s", n, table)
		}
		if n := w.count("company.A", table); n == 0 {
			t.Errorf("company.A sees no %s", table)
		}
	}
}

// declWorld is the FUL world (TRD's real active order of 120 PCS of sku.1 at 8.00 USD, real shipments
// and export lots on in-memory deliveries and QLT releases) with CNX composed on its FUL through
// CNX's real adapter (R36), TRD (R46), CAT's real declaration profiles (R39: decl.sku.1 verified by
// the compliance harness), PTY, POL, EVD, PUR and STL: what the composed process runs.
type declWorld struct {
	*fulWorld
	cx  *cnxWorld
	cnx *cnx.Module
	d   declarer
}

func newDeclWorld(t *testing.T) *declWorld {
	t.Helper()
	f := newFulWorld(t)
	w := &declWorld{fulWorld: f, cx: f.compliance()}
	db, err := bentopg.NewDB(pgtest.AppPool(t))
	w.must(err)
	w.cnx, err = cnx.New(db, cnx.Upstream{Orders: cnx.TRDOrders(f.c.TRD), Countries: cnx.PTYCountries(f.c.PTY), Profiles: cnx.CATProfiles(f.c.CAT),
		Programmes: cnx.REFProgrammes(f.c.REF), Configs: f.c.POL, Sources: f.c.EVD,
		Exports: cnx.FULExports(func() cnx.FULLookups { return f.ful }, f.c.POL, cnx.PTYCountries(f.c.PTY)), Purchases: cnx.PURLines(f.c.PUR),
		Funds: cnx.STLFunds(f.c.STL), CreditNotes: cnx.STLCreditNotes(f.c.STL)})
	w.must(err)
	w.d = declarer{t: t, cnx: w.cnx, doc: w.cx.doc1, key: w.key, evidence: w.source(w.cx.doc1, "customs.receipt.ful", "合成报关单回执（测试数据）")}
	return w
}

// shipment prepares shipment id of the order's line L1 with one line per commitment, quantity each,
// on that commitment's delivery and quality release.
func (w *declWorld) shipment(id, quantity string, commitments ...string) {
	w.t.Helper()
	var lines []fulapi.ShipmentLine
	for i, c := range commitments {
		lines = append(lines, fulapi.ShipmentLine{LineID: "S" + strconv.Itoa(i+1), OrderLineID: "L1", Quantity: quantity,
			SupplierDelivery: w.delivered(c, "60"), QualityReleaseID: w.released(c, "60")})
	}
	_, err := w.ful.PrepareShipment(w.merch, fulapi.PrepareShipment{IdempotencyKey: w.key(), ShipmentID: id, OrderID: w.order.ObjectID,
		Route: fulapi.RouteSupplierDirect, Lines: lines})
	w.must(err)
}

// CN-AT02 (一单分批出口、同次出口包含多销售行), CNX-INV-006, R36/R39/R46 on real records: two shipments
// of the order line, each with two shipment lines from two deliveries, go into two export lots
// (FUL); CNX prepares lot.1's declaration through FUL's lookups as one item of both lines (one
// classification: 60 件, 15 千克, 480.00 USD) and lot.2's as two items, each line once — sales,
// shipment and declaration item mapping can be traced line by line, and the items declare exactly
// the lots' quantities. An AI agent prepares; only a human confirms. A second open declaration of a
// lot, a line of no lot, a lot line left out and a code table without a verified profile are
// refused.
func TestCNAT02DeclaresMultiLineLotsFromFUL(t *testing.T) {
	w := newDeclWorld(t)
	w.shipment("shipment.1", "30", "commitment.1", "commitment.2")
	w.shipment("shipment.2", "30", "commitment.3", "commitment.4")
	for _, id := range []string{"shipment.1", "shipment.2"} {
		w.granted(id)
		_, err := w.confirm(w.merch, id, 1)
		w.must(err)
	}
	_, err := w.registerLot(w.merch, "lot.1", "shipment.1", lotLine("E1", "S1", "30"), lotLine("E2", "S2", "30"))
	w.must(err)
	_, err = w.registerLot(w.merch, "lot.2", "shipment.2", lotLine("E1", "S2", "30"), lotLine("E2", "S1", "30"))
	w.must(err)
	ai := w.declarationAgent("agent.decl")
	p, err := w.cnx.PrepareDeclaration(ai, cnxapi.PrepareDeclaration{IdempotencyKey: w.key(), ExportLotID: "lot.1", CodeTableVersion: codeTable,
		Items: items("E1", "E2")})
	if err != nil || p.Mapping != cnxapi.MappingVerified || p.Status != cnxapi.DeclarationDraft {
		t.Fatalf("prepared %+v %v", p, err)
	}
	v, err := w.cnx.GetDeclaration(w.d.doc, p.DeclarationID)
	w.must(err)
	ver := v.Versions[0]
	it := ver.Items[0]
	if v.ShipmentID != "shipment.1" || v.OrderID != w.order.ObjectID || v.OrderRevision != w.order.OrderRevision || v.OrderHash != w.order.OrderHash ||
		ver.LotCurrency != "USD" || ver.LotValueMinor != "48000" || ver.OrderStatus != "active" || len(ver.Items) != 1 || len(it.Lines) != 2 ||
		it.BaseQuantity != "60" || it.LegalQuantities[0].Unit != "件" || it.LegalQuantities[0].Quantity != "60" || it.LegalQuantities[1].Quantity != "15" ||
		it.ValueMinor != "48000" || it.CurrencyCode != "USD" || it.CommodityCode != "6109100021" || it.DeclarationName != "棉制针织T恤衫" ||
		len(it.Elements) != 3 {
		t.Fatalf("lot.1 declared %+v", v)
	}
	for _, l := range it.Lines {
		if l.ShipmentID != "shipment.1" || l.OrderLineID != "L1" || l.ProfileID != "decl.sku.1" || l.ProfileRevision != 1 || l.Conversion.Source != "base_unit" ||
			l.Quantity != "30" || l.AmountMinor != "24000" || l.Mapping != cnxapi.MappingVerified {
			t.Errorf("line %+v", l)
		}
	}
	_, err = w.cnx.ConfirmDeclaration(ai, cnxapi.ConfirmDeclaration{IdempotencyKey: w.key(), DeclarationID: p.DeclarationID, ExpectedVersion: 1, Version: 1})
	w.want(err, kernel.CodePermissionDenied)
	if c, err := w.cnx.ConfirmDeclaration(w.cx.doc2, cnxapi.ConfirmDeclaration{IdempotencyKey: w.key(), DeclarationID: p.DeclarationID,
		ExpectedVersion: 1, Version: 1}); err != nil || c.Status != cnxapi.DeclarationConfirmed {
		t.Fatalf("confirm %+v %v", c, err)
	}
	_, err = w.cnx.PrepareDeclaration(w.d.doc, cnxapi.PrepareDeclaration{IdempotencyKey: w.key(), ExportLotID: "lot.1", CodeTableVersion: codeTable,
		Items: items("E1", "E2")})
	if code(err) != cnxapi.CodeBusinessKeyConflict || path(err) != "/args/export_lot_id" {
		t.Fatalf("a second open declaration: %s %s", code(err), path(err))
	}
	for _, x := range []struct {
		lot, table string
		items      []cnxapi.DeclarationItem
		want, path string
	}{
		{"lot.2", codeTable, items("E1"), kernel.CodeInvalidKnownValue, "/args/items"},
		{"lot.2", codeTable, items("E1", "E2", "E3"), kernel.CodeReferenceInvalid, "/args/items/0/lines/2/line_id"},
		{"lot.2", "CN-2027", items("E1", "E2"), cnxapi.CodeDeclarationProfileUnverified, "/args/items/0/lines/0/line_id"},
		{"lot.none", codeTable, items("E1"), kernel.CodeReferenceInvalid, "/args/export_lot_id"},
	} {
		_, err = w.cnx.PrepareDeclaration(w.d.doc, cnxapi.PrepareDeclaration{IdempotencyKey: w.key(), ExportLotID: x.lot, CodeTableVersion: x.table,
			Items: x.items})
		if code(err) != x.want || path(err) != x.path {
			t.Errorf("%s %v: want %s %s, got %s %s", x.lot, x.items, x.want, x.path, code(err), path(err))
		}
	}
	two, err := w.cnx.PrepareDeclaration(w.d.doc, cnxapi.PrepareDeclaration{IdempotencyKey: w.key(), ExportLotID: "lot.2", CodeTableVersion: codeTable,
		Items: []cnxapi.DeclarationItem{{ItemNo: 1, Lines: []cnxapi.DeclarationLine{{LineID: "E2"}}}, {ItemNo: 2, Lines: []cnxapi.DeclarationLine{{LineID: "E1"}}}}})
	w.must(err)
	v, err = w.cnx.GetDeclaration(w.d.doc, two.DeclarationID)
	w.must(err)
	declared := kernel.Decimal{}
	for _, it := range v.Versions[0].Items {
		declared = declared.Add(kernel.MustDecimal(it.BaseQuantity))
	}
	lot, shipped := w.exportLot("lot.2"), kernel.Decimal{}
	for _, l := range lot.Lines {
		shipped = shipped.Add(kernel.MustDecimal(l.Quantity))
	}
	if len(v.Versions[0].Items) != 2 || v.Versions[0].Items[0].Lines[0].LineID != "E2" || !declared.Equal(shipped) || declared.String() != "60" {
		t.Fatalf("lot.2 declared %+v against %+v", v.Versions[0].Items, lot.Lines)
	}
	if list, err := w.cnx.Declarations(w.d.doc); err != nil || len(list) != 2 {
		t.Fatalf("declarations %+v %v", list, err)
	}
}

// R36 composed, CNX-INV-002, S5 D-8 and S5 B5's announced release on a receipt: a licence is
// allocated to a real prepared FUL shipment — FUL's adapter gives its lines, value (400.00 USD of 50
// PCS at the order's price) and end user (the customer: the order binds no consignee, a design gap),
// and the destination is the one the order revision's trade compliance evaluation recorded (DE).
// After customs released the shipment's export lot and then the declaration was withdrawn, a stated
// release is refused (a declaration in CNX covers the target), a release on the release receipt is
// refused (not a withdrawal), the release on the withdrawal receipt releases everything outstanding
// with the receipt's text and evidence, and the same receipt does not release twice (CN-AT06
// "不重复恢复凭证用途").
func TestExportLicenceOnAFULShipmentReleasedOnADeclarationReceipt(t *testing.T) {
	w := newDeclWorld(t)
	doc := w.cx.doc1
	_, err := w.file(doc, "licence.c3a", "licence.txt", evdapi.MimeText, "合成出口许可证（测试数据）", "")
	w.must(err)
	now := time.Now().UTC()
	reg, err := w.cnx.RegisterLicense(doc, cnxapi.RegisterLicense{IdempotencyKey: w.key(), LicenseNumber: "SYN-EL-C3A", Issuer: "合成发证机关（测试）",
		Holder: "合成出口商（测试）", Goods: cnxapi.LicensedGoods{SkuSpecs: []cnxapi.SpecRef{{SkuID: "sku.1", SpecRevision: w.spec}}},
		LicensedQuantity: "1000", Unit: "PCS", Currency: cnxapi.CurrencyRef{ID: w.usd.ConfigID, Revision: w.usd.Revision}, LicensedValue: "10000.00",
		Destination: "DE", EndUser: cnxapi.EndUser{PartyID: "customer.1", Name: "合成客户"}, ValidFrom: kernel.FormatTimestamp(now.Add(-time.Hour)),
		ValidUntil: kernel.FormatTimestamp(now.Add(30 * 24 * time.Hour)), FileID: "licence.c3a"})
	w.must(err)
	w.shipment("shipment.L", "50", "commitment.1")
	a, err := w.cnx.AllocateLicense(doc, cnxapi.AllocateLicense{IdempotencyKey: w.key(), LicenseID: reg.LicenseID, AllocationID: "alloc.L",
		Target: cnxapi.ExportTarget{Kind: cnxapi.TargetShipment, ID: "shipment.L"}})
	if err != nil || a.Usage.AllocatedQuantity != "50" || a.Usage.AllocatedValueMinor != "40000" {
		t.Fatalf("allocated %+v %v", a, err)
	}
	_, err = w.cnx.AllocateLicense(doc, cnxapi.AllocateLicense{IdempotencyKey: w.key(), LicenseID: reg.LicenseID, AllocationID: "alloc.none",
		Target: cnxapi.ExportTarget{Kind: cnxapi.TargetShipment, ID: "shipment.none"}})
	w.want(err, kernel.CodeReferenceInvalid)
	// ExportLicenseGuard v1 covers the shipment's lines exactly as FUL holds them (the allocation's
	// lines hash is of the lines CNX's adapter read from FUL), as FUL's release will check them.
	sv, err := w.ful.GetShipment(w.merch, "shipment.L")
	w.must(err)
	var lines []cnxapi.ShipmentLine
	for _, l := range sv.Lines {
		lines = append(lines, cnxapi.ShipmentLine{LineID: l.LineID, SkuID: l.SkuID, SpecRevision: l.SpecRevision, Quantity: l.Quantity, Unit: l.UnitID})
	}
	w.must(w.cx.inApp("company.A", func(ctx context.Context, exec bentopg.Executor) error {
		v, err := cnxapi.ScanExportLicenseGuard("shipment.L", exec.QueryRow(ctx, cnxapi.ExportLicenseGuardSQL,
			cnxapi.ExportLicenseGuardArgs("shipment.L", time.Now())...).Scan)
		if err != nil {
			return err
		}
		return v.Check("/args/shipment_id", lines)
	}))
	w.granted("shipment.L")
	_, err = w.confirm(w.merch, "shipment.L", 1)
	w.must(err)
	_, err = w.registerLot(w.merch, "lot.L", "shipment.L", lotLine("E1", "S1", "50"))
	w.must(err)
	released := w.d.released("lot.L", time.Now(), "E1")
	withdrawn, err := w.cnx.RecordReceipt(doc, w.d.receipt(released, cnxapi.DeclarationWithdrawn, 2))
	w.must(err)
	v, err := w.cnx.GetDeclaration(doc, withdrawn.DeclarationID)
	w.must(err)
	releaseReceipt, withdrawal := v.Receipts[0], v.Receipts[1]
	release := cnxapi.ReleaseLicense{IdempotencyKey: w.key(), LicenseID: reg.LicenseID, ExpectedVersion: a.RowVersion, AllocationID: "alloc.L",
		ReleaseID: "release.L", ExternalResult: &cnxapi.ExternalResult{Kind: cnxapi.ExternalWithdrawn, DeclarationNumber: "SYN", ReceiptText: "撤销",
			ReceivedAt: kernel.FormatTimestamp(time.Now()), EvidenceIDs: []string{w.d.evidence}}, Reason: "报关撤销"}
	_, err = w.cnx.ReleaseLicense(doc, release)
	if code(err) != kernel.CodeInvalidState || path(err) != "/args/external_result/receipt_id" {
		t.Fatalf("a stated result where CNX holds the declaration: %s %s", code(err), path(err))
	}
	release.IdempotencyKey, release.ExternalResult = w.key(), &cnxapi.ExternalResult{Kind: cnxapi.ExternalWithdrawn, ReceiptID: releaseReceipt.ReceiptID}
	_, err = w.cnx.ReleaseLicense(doc, release)
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/external_result/kind" {
		t.Fatalf("a release receipt is no withdrawal: %s %s", code(err), path(err))
	}
	release.IdempotencyKey, release.ExternalResult = w.key(), &cnxapi.ExternalResult{Kind: cnxapi.ExternalWithdrawn, ReceiptID: "rcpt.none"}
	_, err = w.cnx.ReleaseLicense(doc, release)
	w.want(err, kernel.CodeReferenceInvalid)
	release.IdempotencyKey, release.ExternalResult = w.key(), &cnxapi.ExternalResult{Kind: cnxapi.ExternalWithdrawn, ReceiptID: withdrawal.ReceiptID}
	r, err := w.cnx.ReleaseLicense(doc, release)
	if err != nil || r.Usage.AllocatedQuantity != "0" || r.Usage.AvailableQuantity != "1000" {
		t.Fatalf("released on the receipt %+v %v", r, err)
	}
	l, err := w.cnx.GetLicense(doc, reg.LicenseID)
	w.must(err)
	rel := l.Allocations[0].Releases[0]
	if l.Allocations[0].Status != cnxapi.AllocationReleased || rel.Source != cnxapi.ResultDeclarationReceipt || rel.ReceiptID != withdrawal.ReceiptID ||
		rel.DeclarationNumber != "SYN-DECL-lot.L" || rel.ReceiptText != withdrawal.OriginalText || len(rel.Evidence) != 1 ||
		l.Allocations[0].TargetVersion != 1 {
		t.Fatalf("licence %+v", l.Allocations)
	}
	release.IdempotencyKey, release.ReleaseID, release.ExpectedVersion = w.key(), "release.L2", r.RowVersion
	_, err = w.cnx.ReleaseLicense(doc, release)
	w.want(err, kernel.CodeInvalidState)
}

// The export date of S5 C3a on real records (lead decision 2026-11-12; CN-AT12; CN-R07): a tax
// refund allocation on a real FUL export lot is refused while no declaration of the lot states the
// export date customs gives — FUL's shipment confirmation is not a customs export — and once the
// release receipt states it, the case binds the export_tax_policy revision in force at that date and
// keeps it.
func TestTaxRefundCaseBindsTheExportDateOfTheDeclaration(t *testing.T) {
	w := newDeclWorld(t)
	w.shipment("shipment.T", "60", "commitment.1")
	w.granted("shipment.T")
	_, err := w.confirm(w.merch, "shipment.T", 1)
	w.must(err)
	_, err = w.registerLot(w.merch, "lot.T", "shipment.T", lotLine("E1", "S1", "60"))
	w.must(err)
	cny := w.currency(w.author, w.reviewer, "CNY", w.win)
	review := w.source(w.author, "tax.review.c3a", "合成专业复核结论：免退（仅供测试，非税务结论）")
	_, err = w.saveConfig(w.author, polapi.KindExportTaxPolicy, "export.tax", 0, w.win.from, time.Time{}, exportTax(polapi.RefundExemptRefund, review))
	w.must(err)
	_, err = w.c.POL.Publish(w.reviewer, polapi.Publish{IdempotencyKey: w.key(), Kind: polapi.KindExportTaxPolicy, ConfigID: "export.tax", ExpectedVersion: 1,
		Revision: 1, EvidenceIDs: []string{review}, Reason: "核实合成退税政策"})
	w.must(err)
	tax := w.assign("tax.c3a", "tax_specialist")
	_, err = w.file(w.cx.doc1, "invoice.c3a", "invoice.pdf.txt", evdapi.MimeText, "合成增值税专用发票（测试数据）", "")
	w.must(err)
	facts := cnxapi.InvoiceFacts{Kind: cnxapi.InvoiceSpecialVAT, Colour: cnxapi.ColourBlue, Number: "SYNC3A01", IssuedOn: "2026-11-10",
		Supplier: cnxapi.InvoiceParty{PartyID: "supplier.1", Name: "合成供应商（测试）", TaxID: "SUPPLIER1"},
		Buyer:    cnxapi.InvoiceParty{Name: "合成企业", TaxID: "SYNTHETIC-NOT-REAL"}, Currency: cnxapi.CurrencyRef{ID: cny.ConfigID, Revision: cny.Revision},
		Lines: []cnxapi.InvoiceLine{{LineNo: 1, Description: "合成针织衫（测试）", Quantity: "60", Unit: "PCS", NetAmount: "300.00", TaxRate: "0.13",
			TaxAmount: "39.00"}}, NetTotal: "300.00", TaxTotal: "39.00", GrossTotal: "339.00"}
	imported, err := w.cnx.ImportInvoice(tax, cnxapi.ImportInvoice{IdempotencyKey: w.key(), FileID: "invoice.c3a", Candidate: facts})
	w.must(err)
	inv, err := w.cnx.VerifyInvoice(tax, cnxapi.VerifyInvoice{IdempotencyKey: w.key(), PurchaseInvoiceID: imported.PurchaseInvoiceID,
		ExpectedVersion: imported.RowVersion, Decision: cnxapi.DecisionVerify, Facts: &facts})
	w.must(err)
	allocate := cnxapi.AllocateTaxEvidence{IdempotencyKey: w.key(), ExportLotID: "lot.T", PolicyID: "export.tax", Allocations: []cnxapi.EvidenceAllocation{
		{AllocationID: "t1", ExportLineID: "E1", PurchaseInvoiceID: inv.PurchaseInvoiceID, LineNo: 1, Purpose: cnxapi.PurposeExportRefund, Quantity: "60"}}}
	_, err = w.cnx.AllocateTaxEvidence(tax, allocate)
	if code(err) != kernel.CodeInvalidState || path(err) != "/args/export_lot_id" {
		t.Fatalf("a confirmed shipment is no customs export: %s %s", code(err), path(err))
	}
	exported := kernel.Instant(time.Now())
	w.d.released("lot.T", exported, "E1")
	allocate.IdempotencyKey = w.key()
	c, err := w.cnx.AllocateTaxEvidence(tax, allocate)
	if err != nil || c.Policy.Revision != 1 || c.Policy.RefundMethod != polapi.RefundExemptRefund || c.ExportLotID != "lot.T" {
		t.Fatalf("allocated %+v %v", c, err)
	}
	if v, err := w.cnx.GetTaxCase(tax, c.CaseID); err != nil || !v.ExportedAt.Equal(exported) || v.OrderID != w.order.ObjectID ||
		len(v.Allocations) != 1 || v.Allocations[0].ShipmentID != "shipment.T" {
		t.Fatalf("case %+v %v", v, err)
	}
}

// The composed process over HTTP with OIDC access tokens, on a real FUL export lot: a documentation
// clerk prepares the declaration through CNX's command route (the composed CNX reads FUL, TRD and
// CAT), an AI agent is refused its confirmation (403), another clerk confirms it, the submission and
// the release receipt are recorded, the bundle is checked and the tax specialist reconciles the
// declared quantity; the reads answer the case with its version, receipt and export date, the
// bundle with its checklist and reconciliation, and both lists; a schema refusal names its member;
// reconcile reports a command never sent.
func TestDeclarationsAndBundlesOverHTTP(t *testing.T) {
	w := newDeclWorld(t)
	w.shipment("shipment.H", "40", "commitment.1")
	w.granted("shipment.H")
	_, err := w.confirm(w.merch, "shipment.H", 1)
	w.must(err)
	_, err = w.registerLot(w.merch, "lot.H", "shipment.H", lotLine("E1", "S1", "40"))
	w.must(err)
	w.declarationAgent("agent.http")
	w.assign("tax.http", "tax_specialist")
	is := newIssuer(t)
	handler, err := w.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	w.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	doc1 := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "doc.1", "human"), n: 800}
	doc2 := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "doc.2", "human"), n: 900}
	ai := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "agent.http", "ai"), n: 1000}
	tax := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "tax.http", "human"), n: 1100}

	status, out := doc1.try("cnx", cnxapi.Contract, cnxapi.CommandPrepareDeclaration, map[string]any{"export_lot_id": "lot.H", "expected_version": 0,
		"code_table_version": codeTable, "items": []any{map[string]any{"item_no": 0, "lines": []any{map[string]any{"line_id": "E1"}}}}})
	if status != 422 || out["code"] != kernel.CodeSchemaInvalid || errPath(out) != "/args/items/0/item_no" {
		t.Fatalf("schema %d %v", status, out)
	}
	prepareArgs := map[string]any{"export_lot_id": "lot.H", "expected_version": 0, "code_table_version": codeTable,
		"items": []any{map[string]any{"item_no": 1, "lines": []any{map[string]any{"line_id": "E1"}}}}}
	prepared := doc1.command("cnx", cnxapi.Contract, cnxapi.CommandPrepareDeclaration, prepareArgs)
	id, _ := prepared["object_id"].(string)
	if prepared["status"] != "draft" || prepared["row_version"] != float64(1) || id == "" {
		t.Fatalf("prepare %v", prepared)
	}
	status, out = ai.try("cnx", cnxapi.Contract, cnxapi.CommandConfirmDeclaration, map[string]any{"declaration_id": id, "expected_version": 1, "version": 1})
	if status != 403 || out["code"] != kernel.CodePermissionDenied {
		t.Fatalf("AI confirm %d %v", status, out)
	}
	doc2.command("cnx", cnxapi.Contract, cnxapi.CommandConfirmDeclaration, map[string]any{"declaration_id": id, "expected_version": 1, "version": 1,
		"reason": "单证复核"})
	doc1.command("cnx", cnxapi.Contract, cnxapi.CommandRecordSubmission, map[string]any{"declaration_id": id, "expected_version": 2,
		"operation_reference": "op.http", "submitter": map[string]any{"kind": "own_clerk", "name": "合成报关员"},
		"submitted_at": kernel.FormatTimestamp(time.Now().Add(-time.Minute)), "evidence_ids": []any{w.d.evidence}})
	at := kernel.FormatTimestamp(time.Now())
	receipt := doc1.command("cnx", cnxapi.Contract, cnxapi.CommandRecordReceipt, map[string]any{"declaration_id": id, "expected_version": 3,
		"receipt_id": "rcpt.http", "outcome": "released", "external": map[string]any{"sequence": 1, "status_code": "SYN-R", "original_text": "合成放行回执",
			"received_at": at}, "mapping_version": "syn-customs-status.v1", "declaration_number": "SYN-DECL-HTTP", "exported_at": at,
		"evidence_ids": []any{w.d.evidence}})
	if receipt["status"] != "released" || receipt["row_version"] != float64(4) {
		t.Fatalf("receipt %v", receipt)
	}
	status, view := doc1.do("GET", "/api/brant/v1/cnx/declaration_cases/"+id, nil)
	receipts, _ := view["receipts"].([]any)
	versions, _ := view["versions"].([]any)
	if status != 200 || view["status"] != "released" || view["customs_released"] != true || view["declaration_number"] != "SYN-DECL-HTTP" ||
		len(receipts) != 1 || len(versions) != 1 || receipts[0].(map[string]any)["original_text"] != "合成放行回执" {
		t.Fatalf("declaration %d %v", status, view)
	}
	checked := doc1.command("cnx", cnxapi.Contract, cnxapi.CommandCheckBundle, map[string]any{"export_lot_id": "lot.H", "expected_version": 0,
		"requirements": []any{map[string]any{"requirement_id": "decl", "kind": "customs_declaration", "version": "1", "status": "verified",
			"owner": map[string]any{"kind": "actor", "id": "doc.1"}, "evidence_ids": []any{w.d.evidence}, "declaration_id": id, "basis": "回执核对"},
			map[string]any{"requirement_id": "bl", "kind": "bill_of_lading", "version": "1", "status": "missing",
				"owner": map[string]any{"kind": "party", "id": "carrier.1"}, "due_on": "2026-12-15"}}})
	bundle, _ := checked["object_id"].(string)
	if checked["status"] != "incomplete" || bundle == "" {
		t.Fatalf("check %v", checked)
	}
	resolveArgs := map[string]any{"bundle_id": bundle, "expected_version": 1, "reconciliation_id": "rec.http",
		"subject": map[string]any{"kind": "export_lot", "id": "lot.H"}, "measure": "quantity", "unit": "PCS",
		"external": map[string]any{"value": "40", "source": "报关单数量", "evidence_ids": []any{w.d.evidence}},
		"internal": map[string]any{"value": "40", "source": "出口批次数量"}, "responsibility": map[string]any{"kind": "none", "note": "一致"},
		"adjustment": map[string]any{"kind": "none"}, "reason": "数量核对"}
	tax.command("cnx", cnxapi.Contract, cnxapi.CommandResolveReconciliation, resolveArgs)
	status, rec := tax.do("POST", "/api/brant/v1/cnx/commands/"+cnxapi.CommandResolveReconciliation+"/reconcile", map[string]any{"contract": cnxapi.Contract,
		"command": cnxapi.CommandResolveReconciliation, "idempotency_key": "never.sent", "args": resolveArgs})
	if status != 200 || rec["state"] != "not_committed" {
		t.Fatalf("reconcile %d %v", status, rec)
	}
	status, bv := doc1.do("GET", "/api/brant/v1/cnx/document_bundles/"+bundle, nil)
	reqs, _ := bv["requirements"].([]any)
	recs, _ := bv["reconciliations"].([]any)
	if status != 200 || bv["status"] != "incomplete" || len(reqs) != 2 || len(recs) != 1 || recs[0].(map[string]any)["difference"] != "0" {
		t.Fatalf("bundle %d %v", status, bv)
	}
	for _, x := range []struct{ path, member string }{{"/api/brant/v1/cnx/declaration_cases", "declaration_cases"},
		{"/api/brant/v1/cnx/document_bundles", "document_bundles"}} {
		if status, list := doc1.do("GET", x.path, nil); status != 200 || len(list[x.member].([]any)) != 1 {
			t.Fatalf("%s %d %v", x.path, status, list)
		}
	}
}

// CNX-INV-003 concurrency ("两份回执并发登记按案件根锁裁决"): two receipts recorded at once on the same
// version of a case queue on its root FOR UPDATE; one is recorded, the other is VERSION_CONFLICT and
// records nothing; sent again on the new version it is stale or follows, never both.
func TestConcurrentReceiptsAreDecidedByTheCaseRoot(t *testing.T) {
	w := newInvoiceWorld(t)
	d := w.declare
	for round := 0; round < 4; round++ {
		lot := "lot.cc" + strconv.Itoa(round)
		w.dlot(lot, "x1", "10", "PCS")
		s := d.submitted(lot, items("x1"))
		received, accepted := d.receipt(s, cnxapi.DeclarationReceived, 1), d.receipt(s, cnxapi.DeclarationAccepted, 2)
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, r := range []cnxapi.RecordReceipt{received, accepted} {
			wg.Add(1)
			go func(i int, r cnxapi.RecordReceipt) {
				defer wg.Done()
				_, errs[i] = w.cnx.RecordReceipt(d.doc, r)
			}(i, r)
		}
		wg.Wait()
		ok, conflict := 0, 0
		for _, err := range errs {
			switch code(err) {
			case "OK":
				ok++
			case kernel.CodeVersionConflict:
				conflict++
			default:
				t.Fatalf("round %d: %v", round, err)
			}
		}
		v := w.declaration(s.DeclarationID)
		if ok != 1 || conflict != 1 || len(v.Receipts) != 1 || v.RowVersion != s.RowVersion+1 {
			t.Fatalf("round %d: %d recorded, %d conflicts, %+v", round, ok, conflict, v.Receipts)
		}
	}
}
