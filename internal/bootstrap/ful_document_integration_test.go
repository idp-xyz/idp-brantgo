//go:build integration

package bootstrap_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	"go.idp.xyz/idp-brant/internal/contexts/ful"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// WBS S5-08 on PostgreSQL 16: FUL's bookings and the shipment document set (tactical model
// FUL-AGG-Shipment BookingRecord, FUL-AGG-ShipmentDocumentSet, SM-FUL-DOCUMENT, FUL-INV-004; UL
// FUL-BookingRecord, FUL-ShipmentDocumentSet; MD-39; baseline BT12, CN-AT17, INT-05; Context Map R44,
// R49, R74; S5 decisions D-12, D-17). Python V0.50 has no fulfilment context: test names cite the
// design.
//
// The world is the FUL world (newFulWorld: order.quote.1 active for 120 PCS of sku.1 at 8.0000 USD
// on line L1). On top of it: shipment.1 prepared for 50 PCS with its actual packing of 5 cartons
// numbered 1-5 (10 PCS, N.W. 9.5 kg, G.W. 10.5 kg, 0.06 cbm each); PTY's supplier party
// forwarder.1 with a verified forwarder service role "sea" and an inspection service role "qc"; POL
// publishes shipment-text/0.1.0 templates of the commercial invoice and the packing list in en and
// zh-CN, the number sequences ci.seq (document type invoice, CI-) and pl.seq (packing_list, PL-),
// and the order world's company profile company.A is the seller. "docs.1" holds the documentation
// role template alone (S5R-D25: it holds number.issue; S5 granted it besides the template),
// "merch.1" the merchandiser's; "agent.docs" is an AI agent with the document and booking permissions.

type fulDocWorld struct {
	*fulWorld
	docs, agentDocs                      context.Context
	ciEn, ciZh, plEn, plZh, ciSeq, plSeq polapi.ConfigRef
	forwarder, inspection                fulapi.ServiceRoleRef
}

func newFulDocWorld(t *testing.T) *fulDocWorld {
	t.Helper()
	w := &fulDocWorld{fulWorld: newFulWorld(t)}
	ed, rv := w.author, w.reviewer
	tpl, ok := authorization.TemplateByCode("documentation")
	if !ok {
		t.Fatal("no documentation role template")
	}
	w.docs = w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: actorRef("human", "docs.1"), DataScope: "company",
		Permissions: slices.Clone(tpl.Permissions)})
	w.agentDocs = w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: actorRef("ai", "agent.docs"), DataScope: "company",
		Permissions: []string{fulapi.CommandRecordBooking, fulapi.CommandPrepareShipmentDocument, fulapi.CommandIssueShipmentDocuments,
			fulapi.CommandRecordSignedShipmentDocument, "ful.read", "evd.read", "pty.read", "pol.read", "trd.read", "number.issue"}})
	from := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)
	template := func(id, kind, language, title string, sections, clauses []string) polapi.ConfigRef {
		return w.configure(ed, rv, polapi.KindShipmentDocumentTemplate, id, 0, w.win.from, time.Time{}, polapi.ConfigData{
			ShipmentDocumentTemplate: &polapi.ShipmentDocumentTemplate{Name: id, DocumentKind: kind, Language: language,
				RendererVersion: polapi.ShipmentRendererVersion, Title: title, Sections: sections, Clauses: clauses, FooterNote: "合成测试单证，非真实贸易"}}, true)
	}
	ci := []string{"seller", "buyer", "consignee", "notify", "identity", "shipment", "lines", "totals", "terms", "marks", "clauses", "signatures", "notes"}
	pl := []string{"seller", "buyer", "consignee", "identity", "shipment", "packages", "totals", "marks", "signatures", "notes"}
	w.ciEn = template("ship.ci.en", polapi.ShipmentDocumentCommercialInvoice, polapi.LanguageEn, "COMMERCIAL INVOICE", ci,
		[]string{"We certify that the goods are of Chinese origin."})
	w.ciZh = template("ship.ci.zh", polapi.ShipmentDocumentCommercialInvoice, polapi.LanguageZhCN, "商业发票", ci, []string{"兹证明货物原产于中国。"})
	w.plEn = template("ship.pl.en", polapi.ShipmentDocumentPackingList, polapi.LanguageEn, "PACKING LIST", pl, nil)
	w.plZh = template("ship.pl.zh", polapi.ShipmentDocumentPackingList, polapi.LanguageZhCN, "装箱单", pl, nil)
	sequence := func(id, documentType, prefix string) polapi.ConfigRef {
		return w.configure(ed, rv, polapi.KindNumberSequence, id, 0, w.win.from, time.Time{}, polapi.ConfigData{NumberSequence: &polapi.NumberSequence{
			DocumentType: documentType, Prefix: prefix, Width: 5, Reset: polapi.ResetAnnual, TimeZone: "Asia/Shanghai"}}, true)
	}
	w.ciSeq, w.plSeq = sequence("ci.seq", polapi.DocumentTypeInvoice, "CI-"), sequence("pl.seq", polapi.DocumentTypePackingList, "PL-")
	w.staffParty(ed, "forwarder.1", []string{ptyapi.RoleSupplier})
	role := func(id, kind string, expected int64) fulapi.ServiceRoleRef {
		_, err := w.c.PTY.VerifyServiceRole(ed, ptyapi.VerifyServiceRole{IdempotencyKey: w.key(), PartyID: "forwarder.1", ExpectedVersion: expected,
			ServiceRoleID: id, Role: kind, ServiceCode: id + ".service", Origin: "CNSHA", Destination: "DEHAM", ValidFrom: kernel.FormatTimestamp(from),
			ValidUntil: kernel.FormatTimestamp(from.Add(60 * 24 * time.Hour)), EvidenceIDs: []string{"evidence"}})
		w.must(err)
		return fulapi.ServiceRoleRef{PartyID: "forwarder.1", ServiceRoleID: id, Revision: 1}
	}
	w.forwarder, w.inspection = role("sea", ptyapi.ServiceForwarder, 2), role("qc", ptyapi.ServiceInspection, 3)
	return w
}

// shipment prepares shipment id of quantity PCS from commitment and records its actual packing: as
// many cartons of 10 as it takes, numbered from 1.
func (w *fulDocWorld) shipment(id, commitment, quantity string) {
	w.t.Helper()
	rel, d := w.released(commitment, quantity), w.delivered(commitment, quantity)
	_, err := w.prepare(w.merch, id, quantity, d, rel)
	w.must(err)
	cartons := kernel.MustDecimal(quantity)
	n, _ := cartons.Quo(kernel.MustDecimal("10"))
	_, err = w.ful.RecordPacking(w.merch, fulapi.RecordPacking{IdempotencyKey: w.key(), ShipmentID: id, ExpectedVersion: 1,
		Packages: []fulapi.Package{{Cartons: n.Floor().Int64(), CartonFrom: 1, Contents: []fulapi.PackageContent{{LineID: "S1", QuantityPerCarton: "10"}},
			GrossWeightKg: "10.5", NetWeightKg: "9.5", VolumeCbm: "0.06"}}, EvidenceIDs: []string{"evidence"}})
	w.must(err)
}

func (w *fulDocWorld) bookingCmd(shipment string, expected int64, role fulapi.ServiceRoleRef, discharge string) fulapi.RecordBooking {
	return fulapi.RecordBooking{IdempotencyKey: w.key(), ShipmentID: shipment, ExpectedVersion: expected, ServiceRole: role,
		BookingNumber: "SHA2611001", Vessel: "SYNTHETIC STAR", Voyage: "026E", ETD: "2026-12-20", CutOff: "2026-12-18T10:00:00.000000Z",
		ContainerType: "40HQ", ContainerCount: 1, PortOfLoading: "CNSHA", PortOfDischarge: discharge, EvidenceIDs: []string{"evidence"}}
}

func (w *fulDocWorld) particulars(languages ...string) *fulapi.Particulars {
	names := map[string]string{fulapi.LanguageEn: "Cotton shirt", fulapi.LanguageZhCN: "棉质衬衫"}
	p := &fulapi.Particulars{Marks: "BUYER GMBH\nHAMBURG\nC/NO. 1-UP"}
	for _, l := range languages {
		p.Descriptions = append(p.Descriptions, fulapi.Description{SkuID: w.line.Sku.SkuID, Language: l, Text: names[l]})
	}
	return p
}

func ref(r polapi.ConfigRef) fulapi.ConfigRef {
	return fulapi.ConfigRef{Kind: r.Kind, ID: r.ConfigID, Revision: r.Revision}
}

func refp(r polapi.ConfigRef) *fulapi.ConfigRef {
	c := ref(r)
	return &c
}

// ciCmd prepares the commercial invoice of set on shipment in en and zh-CN.
func (w *fulDocWorld) ciCmd(set, shipment string, expected int64) fulapi.PrepareShipmentDocument {
	return fulapi.PrepareShipmentDocument{IdempotencyKey: w.key(), DocumentSetID: set, ShipmentID: shipment, ExpectedVersion: expected,
		DocumentKind: fulapi.DocumentCommercialInvoice, Templates: []fulapi.ConfigRef{ref(w.ciEn), ref(w.ciZh)}, SequenceRef: refp(w.ciSeq),
		SellerRef: refp(w.company), Particulars: w.particulars(fulapi.LanguageEn, fulapi.LanguageZhCN)}
}

// plCmd prepares the packing list of set on shipment in en and zh-CN, of packages when stated.
func (w *fulDocWorld) plCmd(set, shipment string, expected int64, packages ...fulapi.Package) fulapi.PrepareShipmentDocument {
	return fulapi.PrepareShipmentDocument{IdempotencyKey: w.key(), DocumentSetID: set, ShipmentID: shipment, ExpectedVersion: expected,
		DocumentKind: fulapi.DocumentPackingList, Templates: []fulapi.ConfigRef{ref(w.plEn), ref(w.plZh)}, SequenceRef: refp(w.plSeq),
		SellerRef: refp(w.company), Particulars: w.particulars(fulapi.LanguageEn, fulapi.LanguageZhCN), Packages: packages}
}

func (w *fulDocWorld) set(id string) fulapi.DocumentSetView {
	w.t.Helper()
	v, err := w.ful.GetDocumentSet(w.docs, id)
	w.must(err)
	return v
}

func (w *fulDocWorld) rendition(set, kind string, version int64, language string) string {
	w.t.Helper()
	f, err := w.ful.GetShipmentDocument(w.docs, set, kind, version, language)
	w.must(err)
	return string(f.Content)
}

func difference(v fulapi.DocumentSetView, path string) *fulapi.DifferenceView {
	for _, d := range v.Reconciliation.Differences {
		if d.Path == path {
			return &d
		}
	}
	return nil
}

func paths(v fulapi.DocumentSetView) []string {
	var out []string
	for _, d := range v.Reconciliation.Differences {
		out = append(out, d.Path)
	}
	return out
}

// O2C-11, UL FUL-BookingRecord: the merchandiser records shipment.1's booking with the forwarder's
// verified service role and the confirmation's evidence; an AI agent may record one as well
// (booking.record is not human only). A service role of another kind is INVALID_KNOWN_VALUE, an
// unknown one REFERENCE_INVALID; a correction without a reason SCHEMA_INVALID, at a stale version
// VERSION_CONFLICT; a correction makes version 2 and version 1 stays. The shipment's row version does
// not move. A cancelled shipment takes no booking; the database refuses changing or deleting one.
func TestBookingsAreRecordedInVersions(t *testing.T) {
	w := newFulDocWorld(t)
	w.shipment("shipment.1", "commitment.1", "50")
	before := w.fulWorld.shipment("shipment.1").RowVersion
	_, err := w.ful.RecordBooking(w.merch, w.bookingCmd("shipment.1", 0, w.inspection, "DEHAM"))
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/service_role" {
		t.Fatalf("an inspection agency's booking: %v", err)
	}
	_, err = w.ful.RecordBooking(w.merch, w.bookingCmd("shipment.1", 0, fulapi.ServiceRoleRef{PartyID: "forwarder.1", ServiceRoleID: "air", Revision: 1},
		"DEHAM"))
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/service_role" {
		t.Fatalf("an unknown service role: %v", err)
	}
	cmd := w.bookingCmd("shipment.1", 0, w.forwarder, "DEHAM")
	r, err := w.ful.RecordBooking(w.merch, cmd)
	if err != nil || r.Status != fulapi.StatusBooked || r.RowVersion != 1 || r.ObjectID != "shipment.1" {
		t.Fatalf("booking %+v %v", r, err)
	}
	if again, err := w.ful.RecordBooking(w.merch, cmd); err != nil || !again.Replayed || again.ActionID != r.ActionID {
		t.Fatalf("the same key %+v %v", again, err)
	}
	fix := w.bookingCmd("shipment.1", 1, w.forwarder, "DEHAM")
	fix.Vessel = "SYNTHETIC MOON"
	_, err = w.ful.RecordBooking(w.merch, fix)
	if code(err) != kernel.CodeSchemaInvalid || path(err) != "/args/reason" {
		t.Fatalf("a correction without a reason: %v", err)
	}
	fix.Reason = "船公司甩柜，改配下一航次"
	_, err = w.ful.RecordBooking(w.agentDocs, fix)
	w.must(err)
	stale := w.bookingCmd("shipment.1", 1, w.forwarder, "NLRTM")
	stale.Reason = "目的港变更"
	_, err = w.ful.RecordBooking(w.merch, stale)
	w.want(err, kernel.CodeVersionConflict)
	v, err := w.ful.GetBookings(w.merch, "shipment.1")
	w.must(err)
	if len(v.Versions) != 2 || v.Current == nil || v.Current.Version != 2 || v.Current.Vessel != "SYNTHETIC MOON" || v.Current.Reason != fix.Reason ||
		v.Versions[0].Vessel != "SYNTHETIC STAR" || v.Current.ServiceKind != fulapi.ServiceForwarder || v.Current.RecordedBy.Kind != kernel.ActorAI ||
		v.Current.CutOff.Format(time.RFC3339) != "2026-12-18T10:00:00Z" || v.Current.ETD != "2026-12-20" || v.ShipmentStatus != fulapi.StatusPrepared {
		t.Fatalf("bookings %+v", v)
	}
	if after := w.fulWorld.shipment("shipment.1").RowVersion; after != before {
		t.Fatalf("the booking moved the shipment from %d to %d", before, after)
	}
	_, err = w.ful.CancelShipment(w.merch, fulapi.CancelShipment{IdempotencyKey: w.key(), ShipmentID: "shipment.1", ExpectedVersion: before,
		Reason: "客户取消"})
	w.must(err)
	cancelled := w.bookingCmd("shipment.1", 2, w.forwarder, "DEHAM")
	cancelled.Reason = "补录"
	_, err = w.ful.RecordBooking(w.merch, cancelled)
	w.want(err, kernel.CodeInvalidState)
	for _, sql := range []string{`UPDATE brant_ful.shipment_booking SET vessel = 'X' WHERE shipment_id = 'shipment.1'`,
		`DELETE FROM brant_ful.shipment_booking WHERE shipment_id = 'shipment.1'`} {
		if err := w.exec("company.A", sql); err == nil {
			t.Fatalf("the database took %s", sql)
		}
	}
}

// BT12 and FUL-INV-004 (positive: CI and PL that agree are issued; negative: a PL whose carton total
// differs from the CI quantity is SHIPMENT_DOCUMENTS_UNRECONCILED) with SM-FUL-DOCUMENT. An AI agent
// prepares shipment.1's commercial invoice; issuing without a packing list is refused at
// /packing_list, which no acknowledgement covers. The factory's packing list states 4 cartons of 10
// where the shipment ships 50 and the actual packing has 5 cartons: the reconciliation shows each
// difference with its path and what it was compared with, and the issue is refused until the PL is
// resolved by a new version from the actual packing (version 1 superseded). An AI agent never
// issues (humanOnlyCommands). The issue publishes ShipmentDocumentIssued. A bill of lading record
// that names another port of discharge than the booking is a difference a human acknowledges with
// a reason; the issue keeps it. A new CI version leaves the issued one as it was until it is issued
// itself, which supersedes version 1; versions, numbers and sources stay readable and traceable, and
// the database refuses changing an issued version, its bytes or deleting anything.
func TestShipmentDocumentsReconcileBeforeIssueBT12(t *testing.T) {
	w := newFulDocWorld(t)
	w.shipment("shipment.1", "commitment.1", "50")
	_, err := w.ful.RecordBooking(w.merch, w.bookingCmd("shipment.1", 0, w.forwarder, "DEHAM"))
	w.must(err)

	ci, err := w.ful.PrepareShipmentDocument(w.agentDocs, w.ciCmd("docs.1", "shipment.1", 0))
	if err != nil || ci.Status != fulapi.DocumentDraft || ci.RowVersion != 1 || ci.Version != 1 || !strings.HasPrefix(ci.Number, "CI-") ||
		len(ci.Renditions) != 2 || ci.Renditions[0].FileName != ci.Number+"-v1-en.txt" {
		t.Fatalf("CI %+v %v", ci, err)
	}
	_, err = w.ful.IssueShipmentDocuments(w.docs, fulapi.IssueShipmentDocuments{IdempotencyKey: w.key(), DocumentSetID: "docs.1", ExpectedVersion: 1})
	if code(err) != fulapi.CodeShipmentDocumentsUnreconciled || path(err) != "/packing_list" {
		t.Fatalf("issue without a PL: %v", err)
	}
	_, err = w.ful.IssueShipmentDocuments(w.docs, fulapi.IssueShipmentDocuments{IdempotencyKey: w.key(), DocumentSetID: "docs.1", ExpectedVersion: 1,
		Acknowledgements: []fulapi.Acknowledgement{{Path: "/packing_list", Reason: "稍后补"}}})
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/acknowledgements/0/path" {
		t.Fatalf("acknowledging a missing PL: %v", err)
	}

	factory := fulapi.Package{Cartons: 4, CartonFrom: 1, Contents: []fulapi.PackageContent{{LineID: "S1", QuantityPerCarton: "10"}},
		GrossWeightKg: "10.5", NetWeightKg: "9.5", VolumeCbm: "0.06"}
	pl, err := w.ful.PrepareShipmentDocument(w.docs, w.plCmd("docs.1", "shipment.1", 1, factory))
	if err != nil || pl.RowVersion != 2 || pl.Version != 1 || !strings.HasPrefix(pl.Number, "PL-") {
		t.Fatalf("PL %+v %v", pl, err)
	}
	v := w.set("docs.1")
	if v.Reconciliation.State != fulapi.Unreconciled {
		t.Fatalf("reconciliation %+v", v.Reconciliation)
	}
	want := map[string][3]string{
		"/packing_list/lines/S1/quantity":      {"40", "50", "shipment_line"},
		"/packing_list/totals/quantity/PCS":    {"40", "50", fulapi.DocumentCommercialInvoice},
		"/packing_list/totals/cartons":         {"4", "5", "actual_packing"},
		"/packing_list/totals/gross_weight_kg": {"42", "52.5", "actual_packing"},
		"/packing_list/totals/net_weight_kg":   {"38", "47.5", "actual_packing"},
		"/packing_list/totals/volume_cbm":      {"0.24", "0.3", "actual_packing"},
	}
	if got := paths(v); len(got) != len(want) {
		t.Fatalf("differences %v", got)
	}
	for p, x := range want {
		d := difference(v, p)
		if d == nil || d.Found != x[0] || d.Expected != x[1] || d.Against != x[2] || d.Document != fulapi.DocumentPackingList || d.Version != 1 {
			t.Fatalf("%s: %+v", p, d)
		}
	}
	issue := func(ctx context.Context, expected int64, acks ...fulapi.Acknowledgement) (fulapi.IssueReceipt, error) {
		return w.ful.IssueShipmentDocuments(ctx, fulapi.IssueShipmentDocuments{IdempotencyKey: w.key(), DocumentSetID: "docs.1",
			ExpectedVersion: expected, Acknowledgements: acks})
	}
	_, err = issue(w.docs, 2)
	if rej, ok := kernel.AsRejection(err); !ok || rej.Code != fulapi.CodeShipmentDocumentsUnreconciled || rej.Category != kernel.CategoryConflict ||
		path(err) != "/packing_list/lines/S1/quantity" || !strings.Contains(rej.Fields[0].Reason, "6 difference(s)") {
		t.Fatalf("issue with differences: %v", err)
	}
	_, err = issue(w.docs, 2, fulapi.Acknowledgement{Path: "/packing_list/lines/S1/quantity", Reason: "工厂装箱单待更正"})
	if code(err) != fulapi.CodeShipmentDocumentsUnreconciled || path(err) != "/packing_list/totals/cartons" {
		t.Fatalf("one acknowledged, five open: %v", err)
	}
	_, err = issue(w.docs, 2, fulapi.Acknowledgement{Path: "/commercial_invoice/lines/S1/quantity", Reason: "无此差异"})
	if code(err) != kernel.CodeInvalidKnownValue {
		t.Fatalf("acknowledging no difference: %v", err)
	}
	_, err = issue(w.agentDocs, 2)
	w.want(err, kernel.CodePermissionDenied)

	// Resolved: the PL's next version prints the actual packing.
	pl2, err := w.ful.PrepareShipmentDocument(w.docs, w.plCmd("docs.1", "shipment.1", 2))
	if err != nil || pl2.Version != 2 || pl2.Number != pl.Number || pl2.Renditions[0].FileName != pl.Number+"-v2-en.txt" {
		t.Fatalf("PL v2 %+v %v", pl2, err)
	}
	if v = w.set("docs.1"); v.Reconciliation.State != fulapi.Reconciled || len(v.Reconciliation.Differences) != 0 {
		t.Fatalf("after the PL's correction %v", paths(v))
	}
	_, err = issue(w.docs, 2)
	w.want(err, kernel.CodeVersionConflict)
	iss, err := issue(w.docs, 3)
	if err != nil || iss.Status != fulapi.DocumentIssued || iss.RowVersion != 4 || len(iss.Issued) != 2 ||
		iss.Issued[0] != (fulapi.DocumentRef{DocumentKind: fulapi.DocumentCommercialInvoice, Version: 1}) ||
		iss.Issued[1] != (fulapi.DocumentRef{DocumentKind: fulapi.DocumentPackingList, Version: 2}) {
		t.Fatalf("issue %+v %v", iss, err)
	}
	_, err = issue(w.docs, 4)
	w.want(err, kernel.CodeInvalidState) // nothing left to issue
	issued := w.fulEventsOf(fulapi.EventShipmentDocumentIssued)
	if len(issued) != 1 || issued[0].Payload["document_set_id"] != "docs.1" || issued[0].Payload["shipment_id"] != "shipment.1" ||
		issued[0].Payload["reconciliation_sha256"] != iss.ReconciliationSHA256 || len(issued[0].Payload["documents"].([]any)) != 2 {
		t.Fatalf("ShipmentDocumentIssued %+v", issued)
	}

	// A B/L record naming another port of discharge than the booking: acknowledged with a reason.
	w.scan("file.bl", "SYNTHETIC B/L COSU0001 SHIPPED ON BOARD")
	bl := fulapi.PrepareShipmentDocument{IdempotencyKey: w.key(), DocumentSetID: "docs.1", ShipmentID: "shipment.1", ExpectedVersion: 4,
		DocumentKind: fulapi.DocumentBillOfLading, BillOfLading: &fulapi.BillOfLading{Number: "COSU0001", FileID: "file.bl", Vessel: "SYNTHETIC STAR",
			Voyage: "026E", PortOfLoading: "CNSHA", PortOfDischarge: "DEBRV", ShippedOnBoard: "2026-12-20",
			Consignee: fulapi.DocumentParty{Name: "customer.1", Address: "合成买方地址\n上海"}, Marks: "BUYER GMBH\nHAMBURG\nC/NO. 1-UP", Packages: 5,
			GrossWeightKg: "52.5", VolumeCbm: "0.3", ContainerNumbers: []string{"COSU1234565"}}}
	b, err := w.ful.PrepareShipmentDocument(w.docs, bl)
	if err != nil || b.Version != 1 || b.Number != "COSU0001" || len(b.Renditions) != 0 {
		t.Fatalf("B/L record %+v %v", b, err)
	}
	v = w.set("docs.1")
	if got := paths(v); !slices.Equal(got, []string{"/bill_of_lading/port_of_discharge"}) {
		t.Fatalf("B/L differences %v", got)
	}
	if d := difference(v, "/bill_of_lading/port_of_discharge"); d.Found != "DEBRV" || d.Expected != "DEHAM" || d.Against != "booking" {
		t.Fatalf("port %+v", d)
	}
	ack := fulapi.Acknowledgement{Path: "/bill_of_lading/port_of_discharge", Reason: "船公司改靠不来梅港，客户书面同意"}
	iss2, err := issue(w.docs, 5, ack)
	if err != nil || len(iss2.Issued) != 1 || iss2.Issued[0].DocumentKind != fulapi.DocumentBillOfLading {
		t.Fatalf("issue the B/L %+v %v", iss2, err)
	}

	// A new CI version: version 1 stays issued until version 2 is issued.
	ci2, err := w.ful.PrepareShipmentDocument(w.docs, w.ciCmd("docs.1", "shipment.1", 6))
	w.must(err)
	v = w.set("docs.1")
	status := map[string]string{}
	for _, d := range v.Documents {
		status[d.DocumentKind+"/"+strconvI(d.Version)] = d.Status
	}
	if ci2.Version != 2 || ci2.Number != ci.Number || status["commercial_invoice/1"] != fulapi.DocumentIssued ||
		status["commercial_invoice/2"] != fulapi.DocumentDraft || status["packing_list/1"] != fulapi.DocumentSuperseded ||
		status["packing_list/2"] != fulapi.DocumentIssued || status["bill_of_lading/1"] != fulapi.DocumentIssued {
		t.Fatalf("states %v", status)
	}
	old := w.rendition("docs.1", fulapi.DocumentCommercialInvoice, 1, fulapi.LanguageEn)
	iss3, err := issue(w.docs, 7) // the B/L's difference stands with the same values: its acknowledgement is carried
	if err != nil || len(iss3.Issued) != 1 || iss3.Issued[0] != (fulapi.DocumentRef{DocumentKind: fulapi.DocumentCommercialInvoice, Version: 2}) {
		t.Fatalf("issue CI v2 %+v %v", iss3, err)
	}
	v = w.set("docs.1")
	status = map[string]string{}
	for _, d := range v.Documents {
		status[d.DocumentKind+"/"+strconvI(d.Version)] = d.Status
	}
	if status["commercial_invoice/1"] != fulapi.DocumentSuperseded || status["commercial_invoice/2"] != fulapi.DocumentIssued {
		t.Fatalf("states after CI v2 %v", status)
	}
	if len(v.Issues) != 3 || len(v.Issues[1].Acknowledgements) != 1 || v.Issues[1].Acknowledgements[0].Reason != ack.Reason ||
		v.Issues[1].Acknowledgements[0].Difference.Found != "DEBRV" || v.Issues[1].Acknowledgements[0].FromIssue != 0 ||
		v.Issues[1].IssuedBy.ID != "docs.1" || len(v.Issues[2].Acknowledgements) != 1 || v.Issues[2].Acknowledgements[0].FromIssue != 2 ||
		v.Issues[2].Acknowledgements[0].Reason != ack.Reason {
		t.Fatalf("issues %+v", v.Issues)
	}
	if again := w.rendition("docs.1", fulapi.DocumentCommercialInvoice, 1, fulapi.LanguageEn); again != old {
		t.Fatal("the issued CI changed")
	}
	var first fulapi.DocumentVersionView
	for _, d := range v.Documents {
		if d.DocumentKind == fulapi.DocumentCommercialInvoice && d.Version == 1 {
			first = d
		}
	}
	entries := map[string]string{}
	for _, e := range first.Entries {
		entries[e.Name] = e.Value
	}
	sources := map[string]string{}
	for _, e := range first.Sources {
		sources[e.Name] = e.Value
	}
	if first.Sequence == nil || first.Sequence.ID != "ci.seq" || first.Seller == nil || first.Seller.ID != w.company.ConfigID || first.IssueSeq != 1 ||
		first.PreparedBy.Kind != kernel.ActorAI || entries["number"] != ci.Number || entries["pol:number_sequence:ci.seq@1"] == "" ||
		entries["pol:shipment_document_template:ship.ci.zh@1"] != "in_force/"+w.polHash(w.ciZh) || sources["booking"] != "v1" ||
		!strings.HasPrefix(sources["template:zh-CN"], "shipment_document_template:ship.ci.zh@1 ") || sources["terms_order_revision"] == "" ||
		len(first.Renditions) != 2 || first.Renditions[1].Language != fulapi.LanguageZhCN {
		t.Fatalf("CI v1 traceability %+v", first)
	}

	for _, sql := range []string{
		`UPDATE brant_ful.shipment_document SET number = 'X' WHERE document_set_id = 'docs.1'`,
		`UPDATE brant_ful.shipment_document SET status = 'draft', issue_seq = NULL, issued_at = NULL WHERE document_set_id = 'docs.1' AND status = 'issued'`,
		`UPDATE brant_ful.shipment_document_rendition SET content = 'x' WHERE document_set_id = 'docs.1'`,
		`DELETE FROM brant_ful.shipment_document WHERE document_set_id = 'docs.1'`,
		`DELETE FROM brant_ful.shipment_document_issue WHERE document_set_id = 'docs.1'`,
	} {
		if err := w.exec("company.A", sql); err == nil {
			t.Fatalf("the database took %s", sql)
		}
	}
}

func strconvI(v int64) string { return kernel.DecimalFromInt(v).String() }

// polHash is the content hash of POL revision ref.
func (w *fulDocWorld) polHash(ref polapi.ConfigRef) string {
	w.t.Helper()
	v, err := w.c.POL.GetConfig(w.author, ref.Kind, ref.ConfigID)
	w.must(err)
	for _, r := range v.Revisions {
		if r.Revision == ref.Revision {
			return r.ContentHash
		}
	}
	w.t.Fatalf("no revision %v", ref)
	return ""
}

// scan registers an EVD text file id holding content (a scanned copy).
func (w *fulDocWorld) scan(id, content string) {
	w.t.Helper()
	_, err := w.file(w.docs, id, id+".txt", evdapi.MimeText, content, "")
	w.must(err)
}

var (
	amountsEn = regexp.MustCompile(`net ([0-9.]+) \| tax ([0-9.]+) \| total ([0-9.]+)`)
	amountsZh = regexp.MustCompile(`净额 ([0-9.]+) \| 税额 ([0-9.]+) \| 合计 ([0-9.]+)`)
	totalsEn  = regexp.MustCompile(`Net: ([0-9.]+) \| Tax: ([0-9.]+) \| Total: ([0-9.]+)`)
	totalsZh  = regexp.MustCompile(`净额：([0-9.]+) \| 税额：([0-9.]+) \| 合计：([0-9.]+)`)
	stagesEn  = regexp.MustCompile(`Payment: ([^ ]+) \| ([0-9.]+%) \| [^|]+ \| ([0-9]+) `)
	stagesZh  = regexp.MustCompile(`付款：([^ ]+) \| ([0-9.]+%) \| [^|]+ \| ([0-9]+) `)
)

// CN-AT17 (中文操作、双语单证与盖章件修订: 币种、金额、品名及条款对应；保存签署版本，草稿不能冒充已签署) at the
// API, the UI half not executed. One prepare renders the CI and the PL in en and zh-CN from one
// content: the number, currency, every amount, the goods (the same SKU, each language's name), the
// Incoterm, named place and payment stages correspond; the labels are the language's. Only an
// issued version takes a signed or stamped copy, an EVD file a human records per language: the
// generated bytes registered as a file are never the signed copy (INVALID_KNOWN_VALUE, and the
// database refuses one), an AI agent is refused (humanOnlyCommands), the same file again is a replay
// and another file for a language already signed INVALID_STATE.
func TestBilingualDocumentsCorrespondAndSignedCopiesCNAT17(t *testing.T) {
	w := newFulDocWorld(t)
	w.shipment("shipment.1", "commitment.1", "50")
	ci, err := w.ful.PrepareShipmentDocument(w.docs, w.ciCmd("docs.1", "shipment.1", 0))
	w.must(err)
	_, err = w.ful.PrepareShipmentDocument(w.docs, w.plCmd("docs.1", "shipment.1", 1))
	w.must(err)
	en, zh := w.rendition("docs.1", fulapi.DocumentCommercialInvoice, 1, fulapi.LanguageEn),
		w.rendition("docs.1", fulapi.DocumentCommercialInvoice, 1, fulapi.LanguageZhCN)
	pen, pzh := w.rendition("docs.1", fulapi.DocumentPackingList, 1, fulapi.LanguageEn),
		w.rendition("docs.1", fulapi.DocumentPackingList, 1, fulapi.LanguageZhCN)
	for _, c := range []struct{ text, want string }{
		{en, "COMMERCIAL INVOICE\n\nSeller: 合成企业\n"}, {zh, "商业发票\n\n卖方：合成企业\n"},
		{en, "\nNumber: " + ci.Number + "\nVersion: 1\n"}, {zh, "\n编号：" + ci.Number + "\n版本：1\n"},
		{en, "\nS1 | " + w.line.Sku.SkuID + " r"}, {en, " | Cotton shirt | 50 PCS | price 8"},
		{zh, "\nS1 | " + w.line.Sku.SkuID + " r"}, {zh, " | 棉质衬衫 | 50 PCS | 单价 8"},
		{en, "\nCurrency: USD\nQuantity: 50 PCS\n"}, {zh, "\n币种：USD\n数量：50 PCS\n"},
		{en, "\nTerms: FCA 2020 | Named place: DEHAM\n"}, {zh, "\n贸易术语：FCA 2020 | 指定地点：DEHAM\n"},
		{en, "\nBuyer: customer.1\n合成买方地址\n上海\n"}, {zh, "\n买方：customer.1\n合成买方地址\n上海\n"},
		{en, "\nNotify: same as consignee\n"}, {zh, "\n通知方：同收货人\n"}, {en, "\nBooking: none\n"}, {zh, "\n订舱号：无\n"},
		{en, "\n1. We certify that the goods are of Chinese origin.\n"}, {zh, "\n1. 兹证明货物原产于中国。\n"},
		{en, "\nSignatures (this generated copy is unsigned; signed or stamped copies are recorded as evidence):\n"},
		{pen, "\n1-5 | 5 ctns | S1 " + w.line.Sku.SkuID + " Cotton shirt 10 PCS | N.W. 9.5 kg | G.W. 10.5 kg | 0.06 cbm\n"},
		{pzh, "\n1-5 | 5 箱 | S1 " + w.line.Sku.SkuID + " 棉质衬衫 10 PCS | 净重 9.5 kg | 毛重 10.5 kg | 0.06 立方米\n"},
		{pen, "\nCartons: 5\nQuantity: 50 PCS\nN.W.: 47.5 kg | G.W.: 52.5 kg | Volume: 0.3 cbm\n"},
		{pzh, "\n箱数：5\n数量：50 PCS\n净重：47.5 kg | 毛重：52.5 kg | 体积：0.3 立方米\n"},
	} {
		if !strings.Contains(c.text, c.want) {
			t.Fatalf("%q not in\n%s", c.want, c.text)
		}
	}
	for _, p := range [][2]*regexp.Regexp{{amountsEn, amountsZh}, {totalsEn, totalsZh}, {stagesEn, stagesZh}} {
		a, b := p[0].FindAllStringSubmatch(en, -1), p[1].FindAllStringSubmatch(zh, -1)
		if len(a) == 0 || len(a) != len(b) {
			t.Fatalf("%s: %v / %v", p[0], a, b)
		}
		for i := range a {
			if !slices.Equal(a[i][1:], b[i][1:]) {
				t.Fatalf("en %v, zh-CN %v", a[i], b[i])
			}
		}
	}
	if m := totalsEn.FindStringSubmatch(en); m == nil || m[1] != "400.00" {
		t.Fatalf("50 of 120 PCS at 960.00: %v", m)
	}

	// Signed copies: only of an issued version, never the generated bytes, by a human, once.
	w.scan("file.ci.stamped", "STAMPED "+ci.Number)
	sign := func(ctx context.Context, version int64, language, file string) (fulapi.Receipt, error) {
		return w.ful.RecordSignedShipmentDocument(ctx, fulapi.RecordSignedShipmentDocument{IdempotencyKey: w.key(), DocumentSetID: "docs.1",
			DocumentKind: fulapi.DocumentCommercialInvoice, Version: version, Language: language, FileID: file, EvidenceIDs: []string{"evidence"}})
	}
	_, err = sign(w.docs, 1, fulapi.LanguageEn, "file.ci.stamped")
	if code(err) != kernel.CodeInvalidState || path(err) != "/args/version" {
		t.Fatalf("signing a draft: %v", err)
	}
	_, err = w.ful.IssueShipmentDocuments(w.docs, fulapi.IssueShipmentDocuments{IdempotencyKey: w.key(), DocumentSetID: "docs.1", ExpectedVersion: 2})
	w.must(err)
	w.scan("file.ci.generated", en)
	_, err = sign(w.docs, 1, fulapi.LanguageZhCN, "file.ci.generated")
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/file_id" {
		t.Fatalf("the generated en bytes as the signed zh-CN copy: %v", err)
	}
	_, err = sign(w.agentDocs, 1, fulapi.LanguageEn, "file.ci.stamped")
	w.want(err, kernel.CodePermissionDenied)
	_, err = sign(w.docs, 1, fulapi.LanguageEn, "file.none")
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/file_id" {
		t.Fatalf("an unknown file: %v", err)
	}
	s, err := sign(w.docs, 1, fulapi.LanguageEn, "file.ci.stamped")
	if err != nil || s.Status != fulapi.StatusSigned || s.ObjectID != "docs.1" {
		t.Fatalf("signed %+v %v", s, err)
	}
	if r, err := sign(w.docs, 1, fulapi.LanguageEn, "file.ci.stamped"); err != nil || r.Replayed {
		t.Fatalf("the same file again (another key) is answered without a second copy %+v %v", r, err)
	}
	w.scan("file.ci.other", "ANOTHER STAMPED COPY")
	_, err = sign(w.docs, 1, fulapi.LanguageEn, "file.ci.other")
	if code(err) != kernel.CodeInvalidState || path(err) != "/args/language" {
		t.Fatalf("another signed copy: %v", err)
	}
	v := w.set("docs.1")
	var signed []fulapi.SignedCopyView
	for _, d := range v.Documents {
		if d.DocumentKind == fulapi.DocumentCommercialInvoice {
			signed = d.SignedCopies
		}
	}
	if len(signed) != 1 || signed[0].FileID != "file.ci.stamped" || signed[0].FileSHA256 != sha256Hex("STAMPED "+ci.Number) ||
		signed[0].DocumentSHA256 != sha256Hex(en) || signed[0].RecordedBy.ID != "docs.1" || w.count("company.A", "brant_ful.shipment_document_signature") != 1 {
		t.Fatalf("signed copies %+v", signed)
	}
	err = w.exec("company.A", `INSERT INTO brant_ful.shipment_document_signature (tenant_id, company_id, document_set_id, document_kind, version,
		language, file_id, file_sha256, file_mime_type, document_sha256, evidence_ids, idempotency_key, recorded_by_kind, recorded_by_id,
		record_epoch, recorded_at)
		SELECT tenant_id, company_id, document_set_id, document_kind, version, 'zh-CN', 'file.ci.generated', r.content_sha256, 'text/plain',
			repeat('b', 64), '{}', 'k.direct', 'human', 'docs.1', 1, now()
		FROM brant_ful.shipment_document_rendition r WHERE document_set_id = 'docs.1' AND document_kind = 'commercial_invoice' AND language = 'en'`)
	if err == nil || !strings.Contains(err.Error(), "not a signed copy") {
		t.Fatalf("a generated file as a signed copy, inserted: %v", err)
	}
}

// Numbering per the S3 rules (MD-14: 号码不回收；在下游事务之前分配并由下游同事务绑定，回滚留号段空缺). POL issues
// the CI number of set docs.A before FUL's transaction, which then refuses the version (no zh-CN name
// of the SKU): the number is issued to docs.A and bound to nothing. Set docs.B of another shipment
// gets the next number, never that one; docs.A's corrected prepare gets its own number back, and the
// set's later versions keep it. A refusal before the number (a template of another kind) issues
// none. Two sets of one shipment are BUSINESS_KEY_CONFLICT; a set prepared on another shipment
// INVALID_KNOWN_VALUE.
func TestShipmentDocumentNumbersAreNeverReused(t *testing.T) {
	w := newFulDocWorld(t)
	w.shipment("shipment.1", "commitment.1", "50")
	w.shipment("shipment.2", "commitment.2", "30")
	numbers := func() []string {
		var out []string
		for _, e := range w.events("company.A", polapi.Source) {
			if e.Type == polapi.EventNumberIssued && e.Payload["document_type"] == polapi.DocumentTypeInvoice {
				out = append(out, e.Payload["business_ref"].(string)+" "+e.Payload["document_number"].(string))
			}
		}
		return out
	}
	bad := w.ciCmd("docs.A", "shipment.1", 0)
	bad.Templates = []fulapi.ConfigRef{ref(w.plEn)}
	_, err := w.ful.PrepareShipmentDocument(w.docs, bad)
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/templates/0" || len(numbers()) != 0 {
		t.Fatalf("a PL template for a CI: %v %v", err, numbers())
	}
	missing := w.ciCmd("docs.A", "shipment.1", 0)
	missing.Particulars = w.particulars(fulapi.LanguageEn)
	_, err = w.ful.PrepareShipmentDocument(w.docs, missing)
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/particulars/descriptions" {
		t.Fatalf("no zh-CN name: %v", err)
	}
	first := numbers()
	if len(first) != 1 || !strings.HasPrefix(first[0], "docs.A CI-") || w.count("company.A", "brant_ful.shipment_document_set") != 0 {
		t.Fatalf("the refused prepare's number %v", first)
	}
	gap := strings.TrimPrefix(first[0], "docs.A ")
	b, err := w.ful.PrepareShipmentDocument(w.docs, w.ciCmd("docs.B", "shipment.2", 0))
	w.must(err)
	if b.Number == gap {
		t.Fatalf("docs.B got %s after the gap %s", b.Number, gap)
	}
	a, err := w.ful.PrepareShipmentDocument(w.docs, w.ciCmd("docs.A", "shipment.1", 0))
	if err != nil || a.Number != gap {
		t.Fatalf("docs.A's number back %+v %v", a, err)
	}
	a2, err := w.ful.PrepareShipmentDocument(w.docs, w.ciCmd("docs.A", "shipment.1", 1))
	if err != nil || a2.Number != gap || a2.Version != 2 {
		t.Fatalf("docs.A's second version %+v %v", a2, err)
	}
	if got := numbers(); len(got) != 2 {
		t.Fatalf("numbers issued %v", got)
	}
	_, err = w.ful.PrepareShipmentDocument(w.docs, w.ciCmd("docs.C", "shipment.1", 0))
	if code(err) != fulapi.CodeBusinessKeyConflict || path(err) != "/args/shipment_id" {
		t.Fatalf("a second set of shipment.1: %v", err)
	}
	_, err = w.ful.PrepareShipmentDocument(w.docs, w.ciCmd("docs.A", "shipment.2", 2))
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/shipment_id" {
		t.Fatalf("docs.A on shipment.2: %v", err)
	}
	if got := numbers(); len(got) != 2 {
		t.Fatalf("refusals issued numbers %v", got)
	}
}

// S5-08 over HTTP with OIDC access tokens on the composed process (bootstrap wires FUL's document
// ports to POL, PTY and TRD): the merchandiser records the booking; the documentation clerk prepares
// the CI and the PL in en and zh-CN, reads the set reconciled, issues it (an AI agent's issue is
// 403 PERMISSION_DENIED), reconciles the issue's lost answer, reads a rendition's bytes and records
// the stamped copy; the booking reads back in its wire form.
func TestShipmentDocumentsOverHTTP(t *testing.T) {
	w := newFulDocWorld(t)
	w.shipment("shipment.1", "commitment.1", "50")
	is := newIssuer(t)
	handler, err := w.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	w.must(err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	merch := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "merch.1", "human")}
	docs := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "docs.1", "human"), n: 200}
	agent := &client{t: t, base: srv.URL, token: is.token(w.tenant, "company.A", "agent.docs", "ai"), n: 400}

	b := merch.command("ful", fulapi.Contract, fulapi.CommandRecordBooking, map[string]any{"shipment_id": "shipment.1", "expected_version": 0,
		"service_role": map[string]any{"party_id": "forwarder.1", "service_role_id": "sea", "revision": 1}, "booking_number": "SHA2611001",
		"vessel": "SYNTHETIC STAR", "voyage": "026E", "etd": "2026-12-20", "cut_off": "2026-12-18T10:00:00.000000Z", "container_type": "40HQ",
		"container_count": 1, "port_of_loading": "CNSHA", "port_of_discharge": "DEHAM", "evidence_ids": []any{"evidence"}})
	if b["status"] != fulapi.StatusBooked || b["row_version"] != float64(1) || b["object_id"] != "shipment.1" {
		t.Fatalf("booking %v", b)
	}
	cfgRef := func(r polapi.ConfigRef) map[string]any {
		return map[string]any{"kind": r.Kind, "id": r.ConfigID, "revision": r.Revision}
	}
	particulars := map[string]any{"marks": "BUYER GMBH\nHAMBURG", "descriptions": []any{
		map[string]any{"sku_id": w.line.Sku.SkuID, "language": "en", "text": "Cotton shirt"},
		map[string]any{"sku_id": w.line.Sku.SkuID, "language": "zh-CN", "text": "棉质衬衫"}}}
	ci := docs.command("ful", fulapi.Contract, fulapi.CommandPrepareShipmentDocument, map[string]any{"document_set_id": "docs.http",
		"shipment_id": "shipment.1", "expected_version": 0, "document_kind": fulapi.DocumentCommercialInvoice,
		"templates": []any{cfgRef(w.ciEn), cfgRef(w.ciZh)}, "sequence_ref": cfgRef(w.ciSeq), "seller_ref": cfgRef(w.company), "particulars": particulars})
	if ci["status"] != fulapi.DocumentDraft || ci["row_version"] != float64(1) || ci["object_id"] != "docs.http" {
		t.Fatalf("CI %v", ci)
	}
	docs.command("ful", fulapi.Contract, fulapi.CommandPrepareShipmentDocument, map[string]any{"document_set_id": "docs.http",
		"shipment_id": "shipment.1", "expected_version": 1, "document_kind": fulapi.DocumentPackingList,
		"templates": []any{cfgRef(w.plEn), cfgRef(w.plZh)}, "sequence_ref": cfgRef(w.plSeq), "seller_ref": cfgRef(w.company), "particulars": particulars})
	status, set := docs.do("GET", "/api/brant/v1/ful/document_sets/docs.http", nil)
	rec, _ := set["reconciliation"].(map[string]any)
	if status != http.StatusOK || set["row_version"] != float64(2) || len(set["documents"].([]any)) != 2 || rec["state"] != fulapi.Reconciled {
		t.Fatalf("set %d %v", status, set)
	}
	status, out := agent.try("ful", fulapi.Contract, fulapi.CommandIssueShipmentDocuments, map[string]any{"document_set_id": "docs.http",
		"expected_version": 2})
	if status != http.StatusForbidden || out["code"] != kernel.CodePermissionDenied {
		t.Fatalf("an AI agent's issue %d %v", status, out)
	}
	issue := map[string]any{"contract": fulapi.Contract, "command": fulapi.CommandIssueShipmentDocuments, "idempotency_key": "ful.http.issue",
		"args": map[string]any{"document_set_id": "docs.http", "expected_version": 2}}
	status, iss := docs.do("POST", "/api/brant/v1/ful/commands/"+fulapi.CommandIssueShipmentDocuments, issue)
	if status != http.StatusOK || iss["status"] != fulapi.DocumentIssued || iss["row_version"] != float64(3) {
		t.Fatalf("issue %d %v", status, iss)
	}
	status, r := docs.do("POST", "/api/brant/v1/ful/commands/"+fulapi.CommandIssueShipmentDocuments+"/reconcile", issue)
	if status != http.StatusOK || r["state"] != "committed" || r["action_id"] != iss["action_id"] {
		t.Fatalf("reconcile %d %v", status, r)
	}
	status, f := docs.do("GET", "/api/brant/v1/ful/document_sets/docs.http/documents/commercial_invoice/versions/1/zh-CN", nil)
	content, _ := base64.StdEncoding.DecodeString(fmt.Sprint(f["content"]))
	document, _ := f["document"].(map[string]any)
	if status != http.StatusOK || !strings.HasPrefix(string(content), "商业发票\n\n卖方：") || document["status"] != fulapi.DocumentIssued ||
		sha256Hex(string(content)) != document["renditions"].([]any)[1].(map[string]any)["sha256"] {
		t.Fatalf("rendition %d %v", status, f)
	}
	if status, out = docs.do("GET", "/api/brant/v1/ful/document_sets/docs.http/documents/commercial_invoice/versions/0/en", nil); status !=
		http.StatusUnprocessableEntity || out["code"] != kernel.CodeSchemaInvalid {
		t.Fatalf("version 0 %d %v", status, out)
	}
	w.scan("file.http.stamped", "STAMPED COPY")
	s := docs.command("ful", fulapi.Contract, fulapi.CommandRecordSignedShipmentDocument, map[string]any{"document_set_id": "docs.http",
		"document_kind": fulapi.DocumentCommercialInvoice, "version": 1, "language": "zh-CN", "file_id": "file.http.stamped"})
	if s["status"] != fulapi.StatusSigned {
		t.Fatalf("signed %v", s)
	}
	status, bk := merch.do("GET", "/api/brant/v1/ful/shipments/shipment.1/bookings", nil)
	current, _ := bk["current"].(map[string]any)
	if status != http.StatusOK || current["booking_number"] != "SHA2611001" || current["etd"] != "2026-12-20" ||
		current["service_role"].(map[string]any)["service_role_id"] != "sea" {
		t.Fatalf("bookings %d %v", status, bk)
	}
}

// docFixture is what issuedBillOfLading needs once per FUL world: a documentation clerk who may have
// POL issue numbers, en templates of a CI and a PL and their number sequences.
type docFixture struct {
	ctx                  context.Context
	ci, pl, ciSeq, plSeq polapi.ConfigRef
}

var docFixtures sync.Map // *fulWorld → *docFixture

func (w *fulWorld) docFixture() *docFixture {
	w.t.Helper()
	if v, ok := docFixtures.Load(w); ok {
		return v.(*docFixture)
	}
	tpl, ok := authorization.TemplateByCode("documentation")
	if !ok {
		w.t.Fatal("no documentation role template")
	}
	f := &docFixture{ctx: w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: actorRef("human", "docs.fixture"), DataScope: "company",
		Permissions: slices.Clone(tpl.Permissions)})}
	ed, rv := w.author, w.reviewer
	template := func(id, kind, title string, sections []string) polapi.ConfigRef {
		return w.configure(ed, rv, polapi.KindShipmentDocumentTemplate, id, 0, w.win.from, time.Time{}, polapi.ConfigData{
			ShipmentDocumentTemplate: &polapi.ShipmentDocumentTemplate{Name: id, DocumentKind: kind, Language: polapi.LanguageEn,
				RendererVersion: polapi.ShipmentRendererVersion, Title: title, Sections: sections, FooterNote: "合成测试单证"}}, true)
	}
	f.ci = template("fixture.ci", polapi.ShipmentDocumentCommercialInvoice, "COMMERCIAL INVOICE",
		[]string{"seller", "buyer", "identity", "shipment", "lines", "totals", "terms", "notes"})
	f.pl = template("fixture.pl", polapi.ShipmentDocumentPackingList, "PACKING LIST",
		[]string{"seller", "buyer", "identity", "shipment", "packages", "totals", "marks", "notes"})
	sequence := func(id, documentType, prefix string) polapi.ConfigRef {
		return w.configure(ed, rv, polapi.KindNumberSequence, id, 0, w.win.from, time.Time{}, polapi.ConfigData{NumberSequence: &polapi.NumberSequence{
			DocumentType: documentType, Prefix: prefix, Width: 5, Reset: polapi.ResetAnnual, TimeZone: "Asia/Shanghai"}}, true)
	}
	f.ciSeq, f.plSeq = sequence("fixture.ci.seq", polapi.DocumentTypeInvoice, "FCI-"), sequence("fixture.pl.seq", polapi.DocumentTypePackingList, "FPL-")
	docFixtures.Store(w, f)
	return f
}

// issuedBillOfLading prepares on module m the document set "docs.<shipment>" of shipment id — a CI
// and a PL in en (one package per line) and a B/L record — and issues it, acknowledging whatever
// the reconciliation shows: what document_release.record needs since S5-08 binds a documents release
// to the issued B/L record.
func (w *fulWorld) issuedBillOfLading(m *ful.Module, id string) fulapi.IssueReceipt {
	w.t.Helper()
	f := w.docFixture()
	sh, err := m.GetShipment(f.ctx, id)
	w.must(err)
	set := "docs." + id
	p := &fulapi.Particulars{}
	var packages []fulapi.Package
	seen := map[string]bool{}
	for i, l := range sh.Lines {
		if !seen[l.SkuID] {
			p.Descriptions = append(p.Descriptions, fulapi.Description{SkuID: l.SkuID, Language: fulapi.LanguageEn, Text: "Goods " + l.SkuID})
			seen[l.SkuID] = true
		}
		packages = append(packages, fulapi.Package{Cartons: 1, CartonFrom: int64(i + 1), Contents: []fulapi.PackageContent{{LineID: l.LineID,
			QuantityPerCarton: l.Quantity}}, GrossWeightKg: "10", NetWeightKg: "9", VolumeCbm: "0.1"})
	}
	prepare := func(cmd fulapi.PrepareShipmentDocument) {
		w.t.Helper()
		_, err := m.PrepareShipmentDocument(f.ctx, cmd)
		w.must(err)
	}
	prepare(fulapi.PrepareShipmentDocument{IdempotencyKey: w.key(), DocumentSetID: set, ShipmentID: id, DocumentKind: fulapi.DocumentCommercialInvoice,
		Templates: []fulapi.ConfigRef{ref(f.ci)}, SequenceRef: refp(f.ciSeq), SellerRef: refp(w.company), Particulars: p})
	prepare(fulapi.PrepareShipmentDocument{IdempotencyKey: w.key(), DocumentSetID: set, ShipmentID: id, ExpectedVersion: 1,
		DocumentKind: fulapi.DocumentPackingList, Templates: []fulapi.ConfigRef{ref(f.pl)}, SequenceRef: refp(f.plSeq), SellerRef: refp(w.company),
		Particulars: p, Packages: packages})
	_, err = w.file(f.ctx, "file.bl."+id, "bl-"+id+".txt", evdapi.MimeText, "SYNTHETIC B/L OF "+id, "")
	w.must(err)
	prepare(fulapi.PrepareShipmentDocument{IdempotencyKey: w.key(), DocumentSetID: set, ShipmentID: id, ExpectedVersion: 2,
		DocumentKind: fulapi.DocumentBillOfLading, BillOfLading: &fulapi.BillOfLading{Number: "BL-" + id, FileID: "file.bl." + id,
			Vessel: "SYNTHETIC STAR", Voyage: "026E", PortOfLoading: "CNSHA", PortOfDischarge: "DEHAM", ShippedOnBoard: "2026-12-20",
			Consignee: fulapi.DocumentParty{Name: "customer.1", Address: "合成买方地址\n上海"}, Packages: int64(len(sh.Lines)),
			GrossWeightKg: kernel.DecimalFromInt(10 * int64(len(sh.Lines))).String()}})
	v, err := m.GetDocumentSet(f.ctx, set)
	w.must(err)
	var acks []fulapi.Acknowledgement
	for _, d := range v.Reconciliation.Differences {
		acks = append(acks, fulapi.Acknowledgement{Path: d.Path, Reason: "合成测试：差异已核对"})
	}
	r, err := m.IssueShipmentDocuments(f.ctx, fulapi.IssueShipmentDocuments{IdempotencyKey: w.key(), DocumentSetID: set, ExpectedVersion: 3,
		Acknowledgements: acks})
	w.must(err)
	return r
}

// S5-07 with S5-08 (O2C-12: 正本提单与电放受商业条件放行控制; MD-39): a documents release is recorded on the
// issued bill of lading record. With shipment.1 confirmed (its shipment release granted) and its
// documents release granted, document_release.record is SHIPMENT_DOCUMENTS_UNRECONCILED while the
// shipment has no document set and while its B/L record is a draft; once the set is issued the
// telex release is recorded, its consumption names the set, the B/L version, number and content
// SHA-256 (and its evidence), DocumentsReleased carries them, and the database refuses a documents
// consumption that names no issued B/L record.
func TestDocumentReleaseIsRecordedOnTheIssuedBillOfLading(t *testing.T) {
	w := newFulDocWorld(t)
	w.shipment("shipment.1", "commitment.1", "50")
	w.granted("shipment.1")
	_, err := w.confirm(w.merch, "shipment.1", 2)
	w.must(err)
	docs := w.grantedOn(w.ful, "shipment.1", fulapi.ScopeDocuments)
	record := func() (fulapi.ReleaseReceipt, error) {
		return w.ful.RecordDocumentRelease(w.doc, fulapi.RecordDocumentRelease{IdempotencyKey: w.key(), ShipmentID: "shipment.1", ExpectedVersion: 1,
			ReleaseKind: fulapi.ReleaseTelexRelease, EvidenceIDs: []string{"evidence"}})
	}
	_, err = record()
	if code(err) != fulapi.CodeShipmentDocumentsUnreconciled || path(err) != "/args/shipment_id" {
		t.Fatalf("no document set: %v", err)
	}
	_, err = w.ful.PrepareShipmentDocument(w.docs, w.ciCmd("docs.1", "shipment.1", 0))
	w.must(err)
	_, err = w.ful.PrepareShipmentDocument(w.docs, w.plCmd("docs.1", "shipment.1", 1))
	w.must(err)
	w.scan("file.bl", "SYNTHETIC B/L COSU0001")
	_, err = w.ful.PrepareShipmentDocument(w.docs, fulapi.PrepareShipmentDocument{IdempotencyKey: w.key(), DocumentSetID: "docs.1",
		ShipmentID: "shipment.1", ExpectedVersion: 2, DocumentKind: fulapi.DocumentBillOfLading, BillOfLading: &fulapi.BillOfLading{Number: "COSU0001",
			FileID: "file.bl", Vessel: "SYNTHETIC STAR", Voyage: "026E", PortOfLoading: "CNSHA", PortOfDischarge: "DEHAM", ShippedOnBoard: "2026-12-20",
			Consignee: fulapi.DocumentParty{Name: "customer.1", Address: "合成买方地址\n上海"}, Marks: "BUYER GMBH\nHAMBURG\nC/NO. 1-UP", Packages: 5,
			GrossWeightKg: "52.5", VolumeCbm: "0.3"}})
	w.must(err)
	_, err = record()
	w.want(err, fulapi.CodeShipmentDocumentsUnreconciled) // the B/L record is a draft
	if v := w.releaseView(docs.ReleaseID); v.Status != fulapi.ReleaseGranted || v.Consumption != nil {
		t.Fatalf("the refused release %+v", v)
	}
	set := w.set("docs.1")
	var acks []fulapi.Acknowledgement
	for _, d := range set.Reconciliation.Differences {
		acks = append(acks, fulapi.Acknowledgement{Path: d.Path, Reason: "无订舱，港口以提单为准"})
	}
	_, err = w.ful.IssueShipmentDocuments(w.docs, fulapi.IssueShipmentDocuments{IdempotencyKey: w.key(), DocumentSetID: "docs.1", ExpectedVersion: 3,
		Acknowledgements: acks})
	w.must(err)
	var bl fulapi.DocumentVersionView
	for _, d := range w.set("docs.1").Documents {
		if d.DocumentKind == fulapi.DocumentBillOfLading {
			bl = d
		}
	}
	r, err := record()
	if err != nil || r.Status != fulapi.ReleaseConsumed {
		t.Fatalf("telex release %+v %v", r, err)
	}
	c := w.releaseView(docs.ReleaseID).Consumption
	if c == nil || c.BillOfLading == nil || *c.BillOfLading != (fulapi.BillOfLadingRef{DocumentSetID: "docs.1", Version: 1, Number: "COSU0001",
		ContentSHA256: bl.ContentSHA256}) || c.ReleaseKind != fulapi.ReleaseTelexRelease || c.EvidenceIDs[0] != "evidence" {
		t.Fatalf("consumption %+v", c)
	}
	found := false
	for _, g := range c.Guards {
		found = found || (g.Name == "ful.bill_of_lading" && g.Value == "docs.1/v1/COSU0001/"+bl.ContentSHA256)
	}
	ev := w.fulEventsOf(fulapi.EventDocumentsReleased)
	if !found || len(ev) != 1 || ev[0].Payload["bill_of_lading_number"] != "COSU0001" || ev[0].Payload["document_set_id"] != "docs.1" ||
		ev[0].Payload["bill_of_lading_version"] != float64(1) {
		t.Fatalf("evidence %+v, DocumentsReleased %+v", c.Guards, ev)
	}

	// The database: a documents consumption names an issued B/L record of its shipment.
	w.shipment("shipment.2", "commitment.2", "30")
	w.granted("shipment.2")
	_, err = w.confirm(w.merch, "shipment.2", 2)
	w.must(err)
	w.grantedOn(w.ful, "shipment.2", fulapi.ScopeDocuments)
	err = w.exec("company.A", `UPDATE brant_ful.commercial_release SET status = 'consumed', row_version = row_version + 1,
		consumed_command = 'document_release.record', consumed_by_kind = 'human', consumed_by_id = 'doc.1', consume_epoch = 1, consumed_at = now(),
		consume_evidence_ids = '{evidence}', consume_entries = '[]', consume_entries_sha256 = encode(sha256('[]'), 'hex'),
		document_release_kind = 'original_bl', consumed_document_set_id = 'docs.1', consumed_bl_version = 1, consumed_bl_number = 'COSU0001',
		consumed_bl_sha256 = '`+bl.ContentSHA256+`' WHERE shipment_id = 'shipment.2' AND scope = 'documents'`)
	if err == nil || !strings.Contains(err.Error(), "no issued bill of lading record") {
		t.Fatalf("a documents release on another shipment's B/L: %v", err)
	}
}
