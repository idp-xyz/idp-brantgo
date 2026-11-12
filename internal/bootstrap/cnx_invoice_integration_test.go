//go:build integration

package bootstrap_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	"go.idp.xyz/idp-brant/internal/contexts/cnx"
	cnxapi "go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// CNX's purchase invoices and tax refund cases (S5 C3b, WBS S5-05b; tactical model
// CNX-AGG-PurchaseInvoice, CNX-AGG-TaxRefundCase, SM-CNX-INVOICE, SM-CNX-TAXCASE, CNX-INV-004,
// CNX-INV-005; baseline CN-R04, CN-R06, CN-R08, CN-R09, CN-E01; CN-AT04, CN-AT05, CN-AT10, CN-AT12,
// CN-AT15, CN-AT18; S5 decisions E-1, D-3, D-17). Python V0.50 has no CNX ("—（Python 未实现）"), so
// no Python test is ported; the vectors come from the design and are synthetic, and no refund is
// computed (CN-E01): every amount is a person's statement.
//
// The world is PUR's delivery world (commitment.1 issued: 120 PCS of line L1 of order.1 from
// supplier.1), with a delivery of 120 verified, the company's profile and CNY in POL, the
// export_tax_policy export.tax (revision 1, exempt_refund) published on its professional review,
// and STL's fund records. FUL's export lots are not in this tree (S5 B1b): the suite's CNX is
// wired to the real POL, EVD, PUR (PurchaseLineRef v1) and STL (FundRecordRef v1) and to an
// in-memory FUL port (fakeExports); the composed process's CNX answers DEPENDENCY_NOT_SUPPORTED for
// an allocation (shown over HTTP).

type invoiceWorld struct {
	*deliveryWorld
	cnx                         *cnx.Module
	ful                         *fakeExports
	pool                        *pgxpool.Pool
	tax, tax2, purchaser, agent context.Context
	treasury                    context.Context
	cny, bankCNY                polapi.ConfigRef
	company                     polapi.CompanyProfile
	original, receipt, review   string
	files                       int
}

func newInvoiceWorld(t *testing.T) *invoiceWorld {
	w := &invoiceWorld{deliveryWorld: newDeliveryWorld(t), ful: &fakeExports{exports: map[cnx.ExportRef]cnx.Export{}}}
	w.verified("delivery.1", "120", "120")
	ed, rv := w.author, w.reviewer
	if v, err := w.c.POL.GetConfig(ed, polapi.KindCurrency, "CNY"); err == nil && v.ActiveRevision > 0 {
		w.cny = polapi.ConfigRef{Kind: polapi.KindCurrency, ConfigID: "CNY", Revision: v.ActiveRevision}
	} else {
		w.cny = w.currency(ed, rv, "CNY", w.win)
	}
	profile, err := w.c.POL.GetConfig(ed, polapi.KindCompanyProfile, "company.A")
	if err != nil || profile.ActiveRevision == 0 {
		w.configure(ed, rv, polapi.KindCompanyProfile, "company.A", 0, w.win.from, time.Time{}, companyData("CNY"), true)
		profile, err = w.c.POL.GetConfig(ed, polapi.KindCompanyProfile, "company.A")
		w.must(err)
	}
	for _, r := range profile.Revisions {
		if r.Revision == profile.ActiveRevision {
			w.company = *r.Data.CompanyProfile
		}
	}
	w.review = w.source(ed, "tax.review.1", "合成专业复核结论：免退（仅供测试，非税务结论）")
	_, err = w.saveConfig(ed, polapi.KindExportTaxPolicy, "export.tax", 0, w.win.from, time.Time{}, exportTax(polapi.RefundExemptRefund, w.review))
	w.must(err)
	_, err = w.c.POL.Publish(rv, polapi.Publish{IdempotencyKey: w.key(), Kind: polapi.KindExportTaxPolicy, ConfigID: "export.tax", ExpectedVersion: 1,
		Revision: 1, EvidenceIDs: []string{w.review}, Reason: "核实合成退税政策"})
	w.must(err)
	w.bankCNY = w.configure(ed, rv, polapi.KindBankAccount, "bank.cny", 0, w.win.from, time.Time{},
		bank(polapi.OwnerCompany, "company.A", "CNY", polapi.AccountReceipt), true)
	w.party(ed, "tax.bureau", true, "evidence")
	w.tax, w.tax2 = w.assign("tax.1", "tax_specialist"), w.assign("tax.2", "tax_specialist")
	w.purchaser, w.treasury = w.assign("purchaser.1", "purchaser"), w.assign("treasury.1", "treasury")
	w.agent = w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.tax"},
		Permissions: []string{cnxapi.CommandImportInvoice, cnxapi.CommandVerifyInvoice, cnxapi.CommandAllocateTaxEvidence, cnxapi.CommandRecordTaxResult,
			"cnx.read", "evd.read", "pol.read", "pur.read", "stl.read"}, DataScope: "company"})
	w.original = w.newFile("合成增值税专用发票扫描件（测试数据）")
	w.receipt = w.source(w.purchaser, "tax.receipt.1", "合成退税核准通知原文（测试数据）")
	w.pool = pgtest.AppPool(t)
	db, err := bentopg.NewDB(pgtest.AppPool(t))
	w.must(err)
	w.cnx, err = cnx.New(db, cnx.Upstream{Orders: cnx.TRDOrders(w.c.TRD), Countries: cnx.PTYCountries(w.c.PTY), Profiles: fixedProfiles{},
		Programmes: cnx.REFProgrammes(w.c.REF), Configs: w.c.POL, Sources: w.c.EVD, Exports: w.ful, Purchases: cnx.PURLines(w.pur),
		Funds: cnx.STLFunds(w.c.STL)})
	w.must(err)
	return w
}

// newFile registers an original invoice scan as the purchaser and returns its file ID.
func (w *invoiceWorld) newFile(content string) string {
	w.t.Helper()
	w.files++
	id := "invoice.scan." + string(rune('a'+w.files))
	_, err := w.file(w.purchaser, id, "invoice.pdf.txt", evdapi.MimeText, content, "")
	w.must(err)
	return id
}

// facts is a blue special VAT invoice of supplier.1 to the company: one line of quantity PCS at
// 5.00 CNY net each, 13 % tax as printed.
func (w *invoiceWorld) facts(number, quantity string) cnxapi.InvoiceFacts {
	q := kernel.MustDecimal(quantity)
	net, err := kernel.RoundMoney(kernel.MustCurrency("CNY", 2), q.Mul(kernel.MustDecimal("5")))
	w.must(err)
	tax, err := kernel.RoundMoney(kernel.MustCurrency("CNY", 2), net.Decimal().Mul(kernel.MustDecimal("0.13")))
	w.must(err)
	gross, err := net.Add(tax)
	w.must(err)
	return cnxapi.InvoiceFacts{Kind: cnxapi.InvoiceSpecialVAT, Colour: cnxapi.ColourBlue, Number: number, IssuedOn: "2026-11-10",
		Supplier: cnxapi.InvoiceParty{PartyID: "supplier.1", Name: "合成供应商（测试）", TaxID: "SUPPLIER1"},
		Buyer:    cnxapi.InvoiceParty{Name: w.company.LegalName, TaxID: w.company.RegistrationNo},
		Currency: cnxapi.CurrencyRef{ID: w.cny.ConfigID, Revision: w.cny.Revision},
		Lines: []cnxapi.InvoiceLine{{LineNo: 1, Description: "合成针织衫（测试）", Quantity: quantity, Unit: "PCS", NetAmount: net.Amount(),
			TaxRate: "0.13", TaxAmount: tax.Amount()}},
		NetTotal: net.Amount(), TaxTotal: tax.Amount(), GrossTotal: gross.Amount()}
}

func (w *invoiceWorld) red(number string, of cnxapi.InvoiceFacts) cnxapi.InvoiceFacts {
	f := of
	f.Colour, f.Number, f.RedOf = cnxapi.ColourRed, number, &cnxapi.InvoiceRef{Kind: of.Kind, Number: of.Number}
	return f
}

func (w *invoiceWorld) importCmd(f cnxapi.InvoiceFacts) cnxapi.ImportInvoice {
	return cnxapi.ImportInvoice{IdempotencyKey: w.key(), FileID: w.original, Candidate: f}
}

func (w *invoiceWorld) imported(ctx context.Context, f cnxapi.InvoiceFacts) cnxapi.InvoiceReceipt {
	w.t.Helper()
	r, err := w.cnx.ImportInvoice(ctx, w.importCmd(f))
	w.must(err)
	return r
}

func (w *invoiceWorld) verifyCmd(r cnxapi.InvoiceReceipt, f cnxapi.InvoiceFacts, matches ...cnxapi.InvoiceMatch) cnxapi.VerifyInvoice {
	return cnxapi.VerifyInvoice{IdempotencyKey: w.key(), PurchaseInvoiceID: r.PurchaseInvoiceID, ExpectedVersion: r.RowVersion,
		Decision: cnxapi.DecisionVerify, Facts: &f, Matches: matches}
}

// invoice imports f as the purchaser and has the tax specialist verify it with matches.
func (w *invoiceWorld) invoice(f cnxapi.InvoiceFacts, matches ...cnxapi.InvoiceMatch) cnxapi.InvoiceReceipt {
	w.t.Helper()
	r := w.imported(w.purchaser, f)
	v, err := w.cnx.VerifyInvoice(w.tax, w.verifyCmd(r, f, matches...))
	w.must(err)
	return v
}

func l1(quantity string) cnxapi.InvoiceMatch {
	return cnxapi.InvoiceMatch{LineNo: 1, CommitmentID: "commitment.1", OrderLineID: "L1", Quantity: quantity}
}

// lot puts into FUL an export lot of order.1 exported at with one line id of quantity PCS of
// sku.1 from ship.1.
func (w *invoiceWorld) lot(id string, at time.Time, lines ...string) {
	x := cnx.Export{Ref: cnx.ExportRef{Kind: cnxapi.TargetExportLot, ID: id}, Status: cnx.ExportLotRegistered, Version: 1, OrderID: "order.1",
		OrderRevision: 1, Destination: "DE", EndUserPartyID: "customer.1", ExportedAt: at}
	for i := 0; i+1 < len(lines); i += 2 {
		x.Lines = append(x.Lines, cnx.ExportLine{LineID: lines[i], SkuID: "sku.1", SpecRevision: w.spec, Quantity: kernel.MustDecimal(lines[i+1]),
			Unit: "PCS", ShipmentID: "ship.1", OrderLineID: "L1"})
	}
	w.ful.put(x)
}

func alloc(id, line, invoice, purpose, quantity string) cnxapi.EvidenceAllocation {
	return cnxapi.EvidenceAllocation{AllocationID: id, ExportLineID: line, PurchaseInvoiceID: invoice, LineNo: 1, Purpose: purpose, Quantity: quantity}
}

func (w *invoiceWorld) allocate(ctx context.Context, lot string, a ...cnxapi.EvidenceAllocation) (cnxapi.TaxCaseReceipt, error) {
	return w.cnx.AllocateTaxEvidence(ctx, cnxapi.AllocateTaxEvidence{IdempotencyKey: w.key(), ExportLotID: lot, PolicyID: "export.tax", Allocations: a})
}

func (w *invoiceWorld) taxCase(id string) cnxapi.TaxCaseView {
	w.t.Helper()
	v, err := w.cnx.GetTaxCase(w.tax, id)
	w.must(err)
	return v
}

func (w *invoiceWorld) view(id string) cnxapi.InvoiceView {
	w.t.Helper()
	v, err := w.cnx.GetInvoice(w.tax, id)
	w.must(err)
	return v
}

func (w *invoiceWorld) cnxEvents(eventType string) []map[string]any {
	var out []map[string]any
	for _, e := range w.events("company.A", cnxapi.Source) {
		if e.Type == eventType {
			out = append(out, e.Payload)
		}
	}
	return out
}

func balance(v cnxapi.InvoiceView, line int, purpose string) string {
	for _, b := range v.Balances {
		if b.LineNo == line && b.Purpose == purpose {
			return b.Allocated + "/" + b.Available
		}
	}
	return "none"
}

// CN-AT18 and CNX-INV-005: an AI agent reads an invoice scan and imports a candidate with two
// misreadings — the same command path a person takes; INVOICE_DUPLICATE refuses a second import of
// kind and number; the AI's verification is PERMISSION_DENIED (humanOnlyCommands); the tax
// specialist verifies the facts as the original shows them and matches the 120 PCS delivered
// against commitment.1 ("专票核验后匹配 120 件交付行"); the view shows each difference with the
// candidate's source (the AI agent and the original's file) and the verified one's (the tax
// specialist). A rejected invoice frees its kind and number. Reads, the event, replay and the audit
// trail behave as for every context.
func TestCNAT18CandidatesAreVerifiedByAHuman(t *testing.T) {
	w := newInvoiceWorld(t)
	verified := w.facts("SYN0001", "120")
	candidate := w.facts("SYN0001", "120")
	candidate.Lines[0].Quantity, candidate.Lines[0].Description = "12", "合成针织杉"
	imp, err := w.cnx.ImportInvoice(w.agent, w.importCmd(candidate))
	w.must(err)
	if imp.Status != cnxapi.InvoiceImported || imp.RowVersion != 1 || imp.Kind != cnxapi.InvoiceSpecialVAT || imp.Number != "SYN0001" ||
		!strings.HasPrefix(imp.PurchaseInvoiceID, "pin-") {
		t.Fatalf("import %+v", imp)
	}
	_, err = w.cnx.ImportInvoice(w.purchaser, w.importCmd(w.facts("SYN0001", "120")))
	w.want(err, cnxapi.CodeInvoiceDuplicate)
	if path(err) != "/args/candidate/number" {
		t.Fatalf("duplicate at %s", path(err))
	}
	bad := w.importCmd(w.facts("SYN0099", "120"))
	bad.Candidate.GrossTotal = "1.00"
	_, err = w.cnx.ImportInvoice(w.purchaser, bad)
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/candidate/gross_total" {
		t.Fatalf("totals that do not add up: %s %s", code(err), path(err))
	}
	bad = w.importCmd(w.facts("SYN0099", "120"))
	bad.FileID = "no.such.file"
	_, err = w.cnx.ImportInvoice(w.purchaser, bad)
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/file_id" {
		t.Fatalf("no original: %s %s", code(err), path(err))
	}

	// The AI's own candidate never becomes a verified fact through the AI.
	_, err = w.cnx.VerifyInvoice(w.agent, w.verifyCmd(imp, verified, l1("120")))
	w.want(err, kernel.CodePermissionDenied)
	_, err = w.cnx.VerifyInvoice(w.purchaser, w.verifyCmd(imp, verified, l1("120")))
	w.want(err, kernel.CodePermissionDenied)
	stale := w.verifyCmd(imp, verified)
	stale.ExpectedVersion = 2
	_, err = w.cnx.VerifyInvoice(w.tax, stale)
	w.want(err, kernel.CodeVersionConflict)
	over := w.verifyCmd(imp, verified, l1("121"))
	_, err = w.cnx.VerifyInvoice(w.tax, over)
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/matches/0/quantity" {
		t.Fatalf("more than the invoice line: %s %s", code(err), path(err))
	}
	cmd := w.verifyCmd(imp, verified, l1("120"))
	v, err := w.cnx.VerifyInvoice(w.tax, cmd)
	w.must(err)
	if v.Status != cnxapi.InvoiceVerified || v.RowVersion != 2 || v.ReversedInvoiceID != "" {
		t.Fatalf("verify %+v", v)
	}
	if replay, err := w.cnx.VerifyInvoice(w.tax, cmd); err != nil || !replay.Replayed || replay.ActionID != v.ActionID {
		t.Fatalf("replay %+v %v", replay, err)
	}
	view := w.view(imp.PurchaseInvoiceID)
	if view.Status != cnxapi.InvoiceVerified || view.ImportedBy.Kind != kernel.ActorAI || view.ImportedBy.ID != "agent.tax" || view.DecidedBy.ID != "tax.1" ||
		view.Candidate.Lines[0].Quantity != "12" || view.Verified == nil || view.Verified.Lines[0].Quantity != "120" || view.Verified.CurrencyCode != "CNY" ||
		view.File.FileID != w.original || len(view.File.ContentSHA256) != 64 || len(view.Differences) != 2 {
		t.Fatalf("view %+v", view)
	}
	d := view.Differences
	if d[0].Field != "/lines/0/description" || d[1].Field != "/lines/0/quantity" || d[1].Candidate != "12" || d[1].Verified != "120" ||
		d[1].CandidateSource.Step != "import" || d[1].CandidateSource.Actor.Kind != kernel.ActorAI || d[1].CandidateSource.FileID != w.original ||
		d[1].VerifiedSource.Step != "verify" || d[1].VerifiedSource.Actor.ID != "tax.1" || d[1].VerifiedSource.Actor.Kind != kernel.ActorHuman {
		t.Fatalf("differences %+v", d)
	}
	if len(view.Matches) != 1 || view.Matches[0].Status != cnxapi.MatchStanding || view.Matches[0].DeliveredQuantity != "120" ||
		view.Matches[0].PurchaseQuantity != "120" || view.Matches[0].SupplierID != "supplier.1" || !strings.HasPrefix(view.Matches[0].PONumber, "PO-") ||
		balance(view, 1, cnxapi.PurposeExportRefund) != "0/120" || balance(view, 1, cnxapi.PurposeExportExemption) != "0/120" {
		t.Fatalf("matches and balances %+v %+v", view.Matches, view.Balances)
	}
	events := w.cnxEvents(cnxapi.EventInvoiceVerified)
	if len(events) != 1 || events[0]["purchase_invoice_id"] != imp.PurchaseInvoiceID || events[0]["number"] != "SYN0001" ||
		events[0]["reversed_invoice_id"] != "" {
		t.Fatalf("events %v", events)
	}

	// The commitment line takes no more than it buys across invoices.
	second := w.imported(w.purchaser, w.facts("SYN0002", "10"))
	_, err = w.cnx.VerifyInvoice(w.tax, w.verifyCmd(second, w.facts("SYN0002", "10"), l1("1")))
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/matches/0/quantity" {
		t.Fatalf("121 matched on 120 bought: %s %s", code(err), path(err))
	}
	// A rejection keeps the record and frees the number for a correct import.
	rejected, err := w.cnx.VerifyInvoice(w.tax, cnxapi.VerifyInvoice{IdempotencyKey: w.key(), PurchaseInvoiceID: second.PurchaseInvoiceID,
		ExpectedVersion: 1, Decision: cnxapi.DecisionReject, Reason: "合成：重复扫描件"})
	w.must(err)
	if rejected.Status != cnxapi.InvoiceRejected || rejected.RowVersion != 2 {
		t.Fatalf("reject %+v", rejected)
	}
	_, err = w.cnx.VerifyInvoice(w.tax, w.verifyCmd(cnxapi.InvoiceReceipt{PurchaseInvoiceID: second.PurchaseInvoiceID, Receipt: cnxapi.Receipt{RowVersion: 2}},
		w.facts("SYN0002", "10")))
	w.want(err, kernel.CodeInvalidState)
	again := w.imported(w.purchaser, w.facts("SYN0002", "10"))
	if again.PurchaseInvoiceID == second.PurchaseInvoiceID {
		t.Fatal("a new import is a new invoice")
	}
	list, err := w.cnx.Invoices(w.tax)
	if err != nil || len(list) != 3 {
		t.Fatalf("list %+v %v", list, err)
	}
	if _, err := w.cnx.GetInvoice(w.tax, "pin-none"); code(err) != kernel.CodeNotFound {
		t.Fatalf("no such invoice: %v", err)
	}
	audit, err := w.cnx.Audit(w.tax)
	if err == nil {
		t.Fatalf("the tax specialist holds no cnx.audit: %d entries", len(audit))
	}
}

// CN-AT15 (invoice buyer) and CNX-INV-005 "受票主体与本公司不一致时不自动接受，待获准模式复核": a buyer
// that is not the company's entity — another taxpayer, even under the company's name — is refused
// with PARTY_ENTITY_MISMATCH and nothing is recorded: the invoice stays imported, under review. A
// buyer field never moves an invoice to another company (the invoice is the caller's company's).
// The supplier of a match must be the commitment's.
func TestCNAT15BuyerEntityMismatchStaysInReview(t *testing.T) {
	w := newInvoiceWorld(t)
	f := w.facts("SYN0015", "120")
	affiliate := f
	affiliate.Buyer = cnxapi.InvoiceParty{Name: "合成关联企业（测试）", TaxID: "AFFILIATE9"}
	r := w.imported(w.purchaser, affiliate)
	_, err := w.cnx.VerifyInvoice(w.tax, w.verifyCmd(r, affiliate, l1("120")))
	if code(err) != cnxapi.CodePartyEntityMismatch || path(err) != "/args/facts/buyer/tax_id" {
		t.Fatalf("another buyer: %s %s", code(err), path(err))
	}
	sameName := f
	sameName.Buyer.TaxID = "AFFILIATE9"
	_, err = w.cnx.VerifyInvoice(w.tax, w.verifyCmd(r, sameName))
	w.want(err, cnxapi.CodePartyEntityMismatch)
	if v := w.view(r.PurchaseInvoiceID); v.Status != cnxapi.InvoiceImported || v.RowVersion != 1 || v.Verified != nil || len(v.Matches) != 0 {
		t.Fatalf("the invoice stays imported: %+v", v)
	}
	if n := len(w.cnxEvents(cnxapi.EventInvoiceVerified)); n != 0 {
		t.Fatalf("%d events", n)
	}
	other := f
	other.Supplier.PartyID = "supplier.2"
	_, err = w.cnx.VerifyInvoice(w.tax, w.verifyCmd(r, other, l1("120")))
	if code(err) != cnxapi.CodePartyEntityMismatch || path(err) != "/args/matches/0/commitment_id" {
		t.Fatalf("another supplier than the commitment's: %s %s", code(err), path(err))
	}
	if _, err = w.cnx.VerifyInvoice(w.tax, w.verifyCmd(r, f, l1("120"))); err != nil {
		t.Fatalf("the company's own buyer: %v", err)
	}
	if v := w.view(r.PurchaseInvoiceID); v.Status != cnxapi.InvoiceVerified || len(v.Differences) != 2 {
		t.Fatalf("verified with the candidate's buyer shown as a difference: %+v", v.Differences)
	}
	// Another company of the tenant never sees it.
	outside := w.provision(bootstrap.Grant{Scope: w.scope("company.B"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "tax.b"},
		Permissions: []string{"cnx.read"}, DataScope: "company"})
	if _, err := w.cnx.GetInvoice(outside, r.PurchaseInvoiceID); code(err) != kernel.CodeNotFound {
		t.Fatalf("company.B: %v", err)
	}
}

// CNX-INV-004 positive and CN-AT04 ("同一购进凭证部分用于多批业务"): one purchase invoice line of
// 1,000 PCS evidences two export lots, 600 + 400 for the refund; one more is TAX_EVIDENCE_EXCEEDED;
// the exemption purpose keeps a balance of its own; a retry with the same key allocates nothing
// twice. Each lot's first allocation opens its case (expected), which binds export.tax revision 1;
// the case moves by one version with each allocation; TaxEvidenceAllocated is in the Outbox.
func TestCNAT04PurposeBalancesAcrossBatches(t *testing.T) {
	w := newInvoiceWorld(t)
	inv := w.invoice(w.facts("SYN0004", "1000"))
	now := time.Now().UTC()
	w.lot("lot.1", now, "x1", "600")
	w.lot("lot.2", now, "y1", "500", "y2", "500")
	cmd := cnxapi.AllocateTaxEvidence{IdempotencyKey: w.key(), ExportLotID: "lot.1", PolicyID: "export.tax", Allocations: []cnxapi.EvidenceAllocation{
		alloc("t1", "x1", inv.PurchaseInvoiceID, cnxapi.PurposeExportRefund, "600")}}
	a, err := w.cnx.AllocateTaxEvidence(w.agent, cmd)
	w.must(err)
	if a.Status != cnxapi.TaxExpected || a.RowVersion != 1 || a.ExportLotID != "lot.1" || a.Attempt != 1 || a.Policy.PolicyID != "export.tax" ||
		a.Policy.Revision != 1 || a.Policy.RefundMethod != polapi.RefundExemptRefund || len(a.Policy.ContentHash) != 64 || len(a.AllocationIDs) != 1 {
		t.Fatalf("allocate %+v", a)
	}
	if replay, err := w.cnx.AllocateTaxEvidence(w.agent, cmd); err != nil || !replay.Replayed || replay.ActionID != a.ActionID {
		t.Fatalf("a retry allocates once: %+v %v", replay, err)
	}
	_, err = w.allocate(w.tax, "lot.2", alloc("t2", "y1", inv.PurchaseInvoiceID, cnxapi.PurposeExportRefund, "401"))
	if code(err) != cnxapi.CodeTaxEvidenceExceeded || path(err) != "/args/allocations/0/quantity" {
		t.Fatalf("600 + 401 of 1,000: %s %s", code(err), path(err))
	}
	b, err := w.allocate(w.tax, "lot.2", alloc("t2", "y1", inv.PurchaseInvoiceID, cnxapi.PurposeExportRefund, "400"))
	w.must(err)
	_, err = w.allocate(w.tax, "lot.2", alloc("t3", "y2", inv.PurchaseInvoiceID, cnxapi.PurposeExportRefund, "1"))
	w.want(err, cnxapi.CodeTaxEvidenceExceeded)
	_, err = w.allocate(w.tax, "lot.2", alloc("t2", "y2", inv.PurchaseInvoiceID, cnxapi.PurposeExportExemption, "1"))
	w.want(err, cnxapi.CodeBusinessKeyConflict)
	c, err := w.allocate(w.tax, "lot.2", alloc("t3", "y2", inv.PurchaseInvoiceID, cnxapi.PurposeExportExemption, "500"))
	w.must(err)
	if c.CaseID != b.CaseID || c.RowVersion != 2 {
		t.Fatalf("the lot's case moves by one version: %+v %+v", b, c)
	}
	if v := w.view(inv.PurchaseInvoiceID); balance(v, 1, cnxapi.PurposeExportRefund) != "1000/0" || balance(v, 1, cnxapi.PurposeExportExemption) != "500/500" {
		t.Fatalf("balances %+v", v.Balances)
	}
	view := w.taxCase(b.CaseID)
	if view.ExportLotID != "lot.2" || len(view.Allocations) != 2 || view.Allocations[0].Status != cnxapi.EvidenceStanding ||
		view.Allocations[0].InvoiceNumber != "SYN0004" || view.Allocations[0].ShipmentID != "ship.1" || view.Allocations[0].ExportQuantity != "500" ||
		view.Allocations[1].Purpose != cnxapi.PurposeExportExemption || view.OrderID != "order.1" {
		t.Fatalf("case %+v", view)
	}
	events := w.cnxEvents(cnxapi.EventTaxEvidenceAllocated)
	if len(events) != 3 || events[0]["export_lot_id"] != "lot.1" || events[0]["policy_revision"] != float64(1) {
		t.Fatalf("events %v", events)
	}
	// Refusals before the domain's.
	_, err = w.allocate(w.tax, "lot.none", alloc("t9", "x1", inv.PurchaseInvoiceID, cnxapi.PurposeExportRefund, "1"))
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/export_lot_id" {
		t.Fatalf("no such lot: %s %s", code(err), path(err))
	}
	w.lot("lot.future", time.Time{}, "z1", "10")
	_, err = w.allocate(w.tax, "lot.future", alloc("t9", "z1", inv.PurchaseInvoiceID, cnxapi.PurposeExportExemption, "1"))
	w.want(err, kernel.CodeInvalidState)
	_, err = w.cnx.AllocateTaxEvidence(w.tax, cnxapi.AllocateTaxEvidence{IdempotencyKey: w.key(), ExportLotID: "lot.1", PolicyID: "export.other",
		Allocations: []cnxapi.EvidenceAllocation{alloc("t9", "x1", inv.PurchaseInvoiceID, cnxapi.PurposeExportExemption, "1")}})
	w.want(err, cnxapi.CodeReferenceStale)
	if cases, err := w.cnx.TaxCases(w.tax); err != nil || len(cases) != 2 {
		t.Fatalf("cases %+v %v", cases, err)
	}
}

// CN-AT04 concurrency "并发及重试不重复占用" (CNX-INV-004 "两笔分配并发合计超额只有一笔成功"), on PG16: two
// allocations of 600 each from the same 1,000 PCS invoice line to two lots — two cases, so the case
// roots do not serialize them — start together while the invoice row is held; both wait on the
// invoice root; when it is let go, one commits and the other, deriving the balance under the lock,
// is TAX_EVIDENCE_EXCEEDED. Σ allocated stays 600.
func TestCNAT04ConcurrentAllocationsOneWins(t *testing.T) {
	w := newInvoiceWorld(t)
	inv := w.invoice(w.facts("SYN0040", "1000"))
	now := time.Now().UTC()
	w.lot("lot.a", now, "a1", "600")
	w.lot("lot.b", now, "b1", "600")
	pid, held, release := make(chan int32, 1), make(chan struct{}), make(chan struct{})
	holder := make(chan error, 1)
	db, err := bentopg.NewDB(w.pool)
	w.must(err)
	lw := &licenseWorld{world: w.world, app: db}
	go func() {
		holder <- lw.inApp("company.A", func(ctx context.Context, exec bentopg.Executor) error {
			var p int32
			if err := exec.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&p); err != nil {
				return err
			}
			if _, err := exec.Exec(ctx, "SELECT 1 FROM brant_cnx.purchase_invoice WHERE purchase_invoice_id = $1 FOR UPDATE", inv.PurchaseInvoiceID); err != nil {
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
	var once sync.Once
	letGo := func() { once.Do(func() { close(release) }) }
	defer letGo()
	results := make(chan error, 2)
	for _, s := range []string{"a", "b"} {
		go func() {
			_, err := w.allocate(w.tax, "lot."+s, alloc("t."+s, s+"1", inv.PurchaseInvoiceID, cnxapi.PurposeExportRefund, "600"))
			results <- err
		}()
	}
	waitBlocked(t, w.pool, p, 2)
	letGo()
	w.must(<-holder)
	var won, exceeded int
	for range 2 {
		switch err := <-results; {
		case err == nil:
			won++
		case code(err) == cnxapi.CodeTaxEvidenceExceeded:
			exceeded++
		default:
			t.Fatalf("unexpected %v", err)
		}
	}
	if won != 1 || exceeded != 1 {
		t.Fatalf("won %d, exceeded %d", won, exceeded)
	}
	if v := w.view(inv.PurchaseInvoiceID); balance(v, 1, cnxapi.PurposeExportRefund) != "600/400" {
		t.Fatalf("balances %+v", v.Balances)
	}
}

// CN-AT12 "企业适用政策变化且有历史未结业务" (CN-R08 "适用方法变化不追溯覆盖历史"): export.tax revision 1
// (exempt_refund) is in force when lot.old leaves; revision 2 (exempt_credit_refund) supersedes it;
// lot.old's case, opened afterwards, binds revision 1 — the revision in force at its export date,
// found through POL's revisions and confirmed by ResolveConfigs at that date — and keeps it for
// later allocations, while lot.new's binds revision 2. A lot that left before any revision was
// published has none: CONFIG_NOT_ACTIVE. The binding under revision 2 refuses nothing the method
// admits; under not_applicable the refund purpose is refused.
func TestCNAT12CaseBindsThePolicyInForceAtTheExportDate(t *testing.T) {
	w := newInvoiceWorld(t)
	inv := w.invoice(w.facts("SYN0012", "1000"))
	before, err := w.c.POL.GetConfig(w.author, polapi.KindExportTaxPolicy, "export.tax")
	w.must(err)
	exportDate := before.Revisions[0].PublishedAt
	w.lot("lot.old", exportDate, "o1", "100", "o2", "100")
	w.lot("lot.early", exportDate.Add(-time.Hour), "e1", "100")
	time.Sleep(5 * time.Millisecond)
	review2 := w.source(w.author, "tax.review.2", "合成专业复核结论二：免抵退（仅供测试）")
	_, err = w.saveConfig(w.author, polapi.KindExportTaxPolicy, "export.tax", 2, w.win.from, time.Time{},
		exportTax(polapi.RefundExemptCreditRefund, review2))
	w.must(err)
	_, err = w.c.POL.Publish(w.reviewer, polapi.Publish{IdempotencyKey: w.key(), Kind: polapi.KindExportTaxPolicy, ConfigID: "export.tax",
		ExpectedVersion: 3, Revision: 2, EvidenceIDs: []string{review2}, Reason: "核实合成退税政策变化"})
	w.must(err)
	w.lot("lot.new", time.Now().UTC(), "n1", "100")

	old, err := w.allocate(w.tax, "lot.old", alloc("t1", "o1", inv.PurchaseInvoiceID, cnxapi.PurposeExportRefund, "100"))
	w.must(err)
	if old.Policy.Revision != 1 || old.Policy.RefundMethod != polapi.RefundExemptRefund {
		t.Fatalf("lot.old binds %+v", old.Policy)
	}
	again, err := w.allocate(w.tax, "lot.old", alloc("t2", "o2", inv.PurchaseInvoiceID, cnxapi.PurposeExportRefund, "100"))
	if err != nil || again.CaseID != old.CaseID || again.Policy.Revision != 1 {
		t.Fatalf("the case keeps its binding: %+v %v", again, err)
	}
	fresh, err := w.allocate(w.tax, "lot.new", alloc("t3", "n1", inv.PurchaseInvoiceID, cnxapi.PurposeExportRefund, "100"))
	w.must(err)
	if fresh.Policy.Revision != 2 || fresh.Policy.RefundMethod != polapi.RefundExemptCreditRefund || fresh.Policy.ContentHash == old.Policy.ContentHash {
		t.Fatalf("lot.new binds %+v", fresh.Policy)
	}
	_, err = w.allocate(w.tax, "lot.early", alloc("t4", "e1", inv.PurchaseInvoiceID, cnxapi.PurposeExportRefund, "1"))
	if code(err) != polapi.CodeConfigNotActive || path(err) != "/args/policy_id" {
		t.Fatalf("before any revision: %s %s", code(err), path(err))
	}
	if v := w.taxCase(old.CaseID); v.Policy.Revision != 1 || !v.ExportedAt.Equal(kernel.Instant(exportDate)) || len(v.Allocations) != 2 {
		t.Fatalf("lot.old's case %+v", v)
	}
	w.lot("lot.x", time.Now().UTC(), "x1", "10")
	_, err = w.cnx.AllocateTaxEvidence(w.tax, cnxapi.AllocateTaxEvidence{IdempotencyKey: w.key(), ExportLotID: "lot.x", PolicyID: "export.none",
		Allocations: []cnxapi.EvidenceAllocation{alloc("t5", "x1", inv.PurchaseInvoiceID, cnxapi.PurposeExportRefund, "1")}})
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/policy_id" {
		t.Fatalf("no such policy: %s %s", code(err), path(err))
	}
	// A policy without a refund method admits the exemption purpose only.
	review3 := w.source(w.author, "tax.review.3", "合成专业复核结论三：不适用退税（仅供测试）")
	_, err = w.saveConfig(w.author, polapi.KindExportTaxPolicy, "export.none", 0, w.win.from, time.Time{}, exportTax(polapi.RefundNotApplicable, review3))
	w.must(err)
	_, err = w.c.POL.Publish(w.reviewer, polapi.Publish{IdempotencyKey: w.key(), Kind: polapi.KindExportTaxPolicy, ConfigID: "export.none",
		ExpectedVersion: 1, Revision: 1, EvidenceIDs: []string{review3}, Reason: "核实合成政策"})
	w.must(err)
	w.lot("lot.y", time.Now().UTC(), "y1", "10")
	_, err = w.cnx.AllocateTaxEvidence(w.tax, cnxapi.AllocateTaxEvidence{IdempotencyKey: w.key(), ExportLotID: "lot.y", PolicyID: "export.none",
		Allocations: []cnxapi.EvidenceAllocation{alloc("t6", "y1", inv.PurchaseInvoiceID, cnxapi.PurposeExportRefund, "1")}})
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/allocations/0/purpose" {
		t.Fatalf("refund under not_applicable: %s %s", code(err), path(err))
	}
}

// fund records a non-goods receipt of amount CNY from the tax bureau on the company's CNY account
// as the treasury, and returns its ID.
func (w *invoiceWorld) fund(id, amount string, currency, account polapi.ConfigRef) string {
	w.t.Helper()
	advice := w.source(w.author, "advice."+id, "合成银行入账通知：退税款（测试数据）")
	r, err := w.c.STL.RecordFund(w.treasury, stlapi.RecordFund{IdempotencyKey: w.key(), FundRecordID: id, FundKind: stlapi.FundNonGoods,
		CounterpartyID: "tax.bureau", Currency: stlRef(currency), BankAccount: stlRef(account), ReceivedAmount: amount,
		ValueDate: kernel.FormatTimestamp(time.Now()), BankReference: "ref." + id, EvidenceIDs: []string{advice}})
	w.must(err)
	return r.ObjectID
}

func (w *invoiceWorld) record(ctx context.Context, c cnxapi.TaxCaseReceipt, r cnxapi.RecordTaxResult) (cnxapi.TaxCaseReceipt, error) {
	r.IdempotencyKey, r.CaseID, r.ExpectedVersion = w.key(), c.CaseID, c.RowVersion
	return w.cnx.RecordTaxResult(ctx, r)
}

// CN-AT10 "预计退税、核准与到账金额不同" and CN-R08, SM-CNX-TAXCASE: the tax specialist states the
// expected 1,000.00, plans 1,000.00 for tax period 2026-11 with the professional reviewer, records
// the submission's external reference, the approval of 950.00 refund and 30.00 offset (免抵, not
// cash) with the approval's original text, and the receipt of 950.00 on an STL fund record
// (FundRecordRef v1). The view keeps each stage apart and shows the differences; nothing is
// computed. record_result is human only (PERMISSION_DENIED for the AI agent); a stage out of order
// is INVALID_STATE; a fund record in another currency or for less is INVALID_KNOWN_VALUE.
func TestCNAT10ExpectedApprovedAndReceivedStayApart(t *testing.T) {
	w := newInvoiceWorld(t)
	inv := w.invoice(w.facts("SYN0010", "1000"))
	w.lot("lot.10", time.Now().UTC(), "x1", "1000")
	c, err := w.allocate(w.agent, "lot.10", alloc("t1", "x1", inv.PurchaseInvoiceID, cnxapi.PurposeExportRefund, "1000"))
	w.must(err)
	cny := &cnxapi.CurrencyRef{ID: w.cny.ConfigID, Revision: w.cny.Revision}
	expected := cnxapi.RecordTaxResult{Stage: cnxapi.TaxExpected, Currency: cny, Amount: "1000.00", Reason: "按合成口径估计（非税务结论）"}
	_, err = w.record(w.agent, c, expected)
	w.want(err, kernel.CodePermissionDenied)
	_, err = w.record(w.tax, c, cnxapi.RecordTaxResult{Stage: cnxapi.TaxSubmitted, ExternalReference: "SB-0001", EvidenceIDs: []string{w.receipt}})
	w.want(err, kernel.CodeInvalidState)
	c, err = w.record(w.tax, c, expected)
	w.must(err)
	if c.Status != cnxapi.TaxExpected || c.RowVersion != 2 || c.ResultID == "" {
		t.Fatalf("expected %+v", c)
	}
	c, err = w.record(w.tax, c, cnxapi.RecordTaxResult{Stage: cnxapi.TaxPlanned, Currency: cny, Amount: "1000.00", TaxPeriod: "2026-11",
		Review: &cnxapi.ProfessionalReview{Reviewer: "合成税务师（测试）", EvidenceIDs: []string{w.review}}})
	w.must(err)
	_, err = w.allocate(w.tax, "lot.10", alloc("t2", "x1", inv.PurchaseInvoiceID, cnxapi.PurposeExportExemption, "1"))
	if err != nil {
		t.Fatalf("a planned case still takes evidence: %v", err)
	}
	c.RowVersion++
	c, err = w.record(w.tax, c, cnxapi.RecordTaxResult{Stage: cnxapi.TaxSubmitted, ExternalReference: "SB-0001", EvidenceIDs: []string{w.receipt}})
	w.must(err)
	_, err = w.allocate(w.tax, "lot.10", alloc("t3", "x1", inv.PurchaseInvoiceID, cnxapi.PurposeExportExemption, "1"))
	w.want(err, kernel.CodeInvalidState)
	c, err = w.record(w.tax, c, cnxapi.RecordTaxResult{Stage: cnxapi.TaxApproved, Currency: cny, Amount: "950.00", OffsetAmount: "30.00",
		ResultText: "合成核准通知：核准退税 950.00 元，免抵 30.00 元（测试数据）", EvidenceIDs: []string{w.receipt}})
	w.must(err)
	usd := w.fund("fund.usd", "950.00", w.usd, w.usdBank())
	_, err = w.record(w.tax, c, cnxapi.RecordTaxResult{Stage: cnxapi.TaxReceived, Currency: cny, Amount: "950.00", FundRecordID: usd})
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/currency" {
		t.Fatalf("a fund record in USD: %s %s", code(err), path(err))
	}
	small := w.fund("fund.small", "900.00", w.cny, w.bankCNY)
	_, err = w.record(w.tax, c, cnxapi.RecordTaxResult{Stage: cnxapi.TaxReceived, Currency: cny, Amount: "950.00", FundRecordID: small})
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/amount" {
		t.Fatalf("more than the fund record received: %s %s", code(err), path(err))
	}
	_, err = w.record(w.tax, c, cnxapi.RecordTaxResult{Stage: cnxapi.TaxReceived, Currency: cny, Amount: "950.00", FundRecordID: "fund.none"})
	w.want(err, kernel.CodeReferenceInvalid)
	paid := w.fund("fund.refund", "950.00", w.cny, w.bankCNY)
	c, err = w.record(w.tax, c, cnxapi.RecordTaxResult{Stage: cnxapi.TaxReceived, Currency: cny, Amount: "950.00", FundRecordID: paid})
	w.must(err)
	if c.Status != cnxapi.TaxReceived {
		t.Fatalf("received %+v", c)
	}
	v := w.taxCase(c.CaseID)
	a := v.Amounts
	if a.CurrencyCode != "CNY" || a.ExpectedMinor != "100000" || a.PlannedMinor != "100000" || a.ApprovedMinor != "95000" || a.OffsetMinor != "3000" ||
		a.ReceivedMinor != "95000" || a.ApprovedLessExpectedMinor != "-5000" || a.ReceivedLessApprovedMinor != "0" {
		t.Fatalf("amounts %+v", a)
	}
	if len(v.Results) != 5 || v.Results[1].Reviewer != "合成税务师（测试）" || v.Results[1].TaxPeriod != "2026-11" || len(v.Results[1].ReviewEvidence) != 1 ||
		v.Results[2].ExternalReference != "SB-0001" || v.Results[3].ResultText == "" || v.Results[4].FundRecordID != paid ||
		v.Results[4].From != cnxapi.TaxApproved || v.Results[4].RecordedBy.ID != "tax.1" || v.Status != cnxapi.TaxReceived {
		t.Fatalf("results %+v", v.Results)
	}
	if n := len(w.cnxEvents(cnxapi.EventTaxRefundResultRecord)); n != 5 {
		t.Fatalf("%d results published", n)
	}
	_, err = w.record(w.tax, c, cnxapi.RecordTaxResult{Stage: cnxapi.TaxRejected, ResultText: "x", EvidenceIDs: []string{w.receipt}})
	w.want(err, kernel.CodeInvalidState)
}

func (w *invoiceWorld) usdBank() polapi.ConfigRef {
	w.t.Helper()
	if v, err := w.c.POL.GetConfig(w.author, polapi.KindBankAccount, "bank.usd.c3b"); err == nil && v.ActiveRevision > 0 {
		return polapi.ConfigRef{Kind: polapi.KindBankAccount, ConfigID: "bank.usd.c3b", Revision: v.ActiveRevision}
	}
	return w.configure(w.author, w.reviewer, polapi.KindBankAccount, "bank.usd.c3b", 0, w.win.from, time.Time{},
		bank(polapi.OwnerCompany, "company.A", "USD", polapi.AccountReceipt), true)
}

// CN-R09 and CNX-INV-004 "申报撤回、冲减与重新使用只按外部处理结果，不凭本地取消恢复": no command cancels a
// case or an allocation locally; the external rejection of a submitted case releases its evidence,
// the invoice line's balance returns, and the lot may open a new case (attempt 2) that uses it.
func TestCNR09OnlyAnExternalResultReturnsEvidence(t *testing.T) {
	w := newInvoiceWorld(t)
	inv := w.invoice(w.facts("SYN0009", "600"))
	w.lot("lot.9", time.Now().UTC(), "x1", "600")
	w.lot("lot.9b", time.Now().UTC(), "z1", "600")
	c, err := w.allocate(w.tax, "lot.9", alloc("t1", "x1", inv.PurchaseInvoiceID, cnxapi.PurposeExportRefund, "600"))
	w.must(err)
	_, err = w.allocate(w.tax, "lot.9b", alloc("t2", "z1", inv.PurchaseInvoiceID, cnxapi.PurposeExportRefund, "600"))
	w.want(err, cnxapi.CodeTaxEvidenceExceeded)
	cny := &cnxapi.CurrencyRef{ID: w.cny.ConfigID, Revision: w.cny.Revision}
	c, err = w.record(w.tax, c, cnxapi.RecordTaxResult{Stage: cnxapi.TaxPlanned, Currency: cny, Amount: "500.00", TaxPeriod: "2026-11",
		Review: &cnxapi.ProfessionalReview{Reviewer: "合成税务师（测试）", EvidenceIDs: []string{w.review}}})
	w.must(err)
	c, err = w.record(w.tax, c, cnxapi.RecordTaxResult{Stage: cnxapi.TaxSubmitted, ExternalReference: "SB-0009", EvidenceIDs: []string{w.receipt}})
	w.must(err)
	_, err = w.record(w.tax, c, cnxapi.RecordTaxResult{Stage: "cancelled", Reason: "本地取消"})
	w.want(err, kernel.CodeSchemaInvalid)
	c, err = w.record(w.tax, c, cnxapi.RecordTaxResult{Stage: cnxapi.TaxRejected, ResultText: "合成：不予核准（测试数据）", EvidenceIDs: []string{w.receipt}})
	w.must(err)
	if v := w.taxCase(c.CaseID); v.Status != cnxapi.TaxRejected || v.Allocations[0].Status != cnxapi.EvidenceReleased {
		t.Fatalf("rejected %+v", v)
	}
	if v := w.view(inv.PurchaseInvoiceID); balance(v, 1, cnxapi.PurposeExportRefund) != "0/600" {
		t.Fatalf("balances %+v", v.Balances)
	}
	next, err := w.allocate(w.tax, "lot.9", alloc("t3", "x1", inv.PurchaseInvoiceID, cnxapi.PurposeExportRefund, "600"))
	w.must(err)
	if next.CaseID == c.CaseID || next.Attempt != 2 || next.Status != cnxapi.TaxExpected {
		t.Fatalf("the lot's next case %+v", next)
	}
}

// CN-AT05 "先出运后到票，原票随后红冲" and CN-R06: the goods were delivered (120 verified) and exported
// before the invoice came; the invoice is verified and matched, its line allocated to the lot; a red
// invoice then reverses it: the blue invoice is reversed, its match and the allocation from it are
// under review (the delivery facts stay as PUR has them), and the plan is refused until a new blue
// invoice, verified and matched again, replaces the allocation. PurchaseInvoiceVerified of the red
// invoice names the reversed invoice and the commitments whose payables need review (B2b).
func TestCNAT05RedReversalPutsMatchesBackToReview(t *testing.T) {
	w := newInvoiceWorld(t)
	w.lot("lot.5", time.Now().UTC(), "x1", "120")
	blue := w.facts("SYN0005", "120")
	original := w.invoice(blue, l1("120"))
	c, err := w.allocate(w.tax, "lot.5", alloc("t1", "x1", original.PurchaseInvoiceID, cnxapi.PurposeExportRefund, "120"))
	w.must(err)

	redFacts := w.red("SYN0006", blue)
	r := w.imported(w.agent, redFacts)
	_, err = w.cnx.VerifyInvoice(w.tax, w.verifyCmd(r, redFacts, l1("1")))
	w.want(err, kernel.CodeSchemaInvalid)
	unknown := w.red("SYN0006", w.facts("SYN0404", "120"))
	_, err = w.cnx.VerifyInvoice(w.tax, w.verifyCmd(r, unknown))
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/facts/red_of" {
		t.Fatalf("a red invoice of nothing: %s %s", code(err), path(err))
	}
	red, err := w.cnx.VerifyInvoice(w.tax, w.verifyCmd(r, redFacts))
	w.must(err)
	if red.Status != cnxapi.InvoiceVerified || red.ReversedInvoiceID != original.PurchaseInvoiceID {
		t.Fatalf("red %+v", red)
	}
	o := w.view(original.PurchaseInvoiceID)
	if o.Status != cnxapi.InvoiceReversed || o.ReversedByID != red.PurchaseInvoiceID || o.RowVersion != 3 || len(o.Matches) != 1 ||
		o.Matches[0].Status != cnxapi.MatchReview {
		t.Fatalf("the reversed invoice %+v", o)
	}
	if rv := w.view(red.PurchaseInvoiceID); rv.ReversesInvoiceID != original.PurchaseInvoiceID || len(rv.Balances) != 0 {
		t.Fatalf("the red invoice %+v", rv)
	}
	if v := w.taxCase(c.CaseID); v.Allocations[0].Status != cnxapi.EvidenceReview {
		t.Fatalf("the allocation %+v", v.Allocations)
	}
	events := w.cnxEvents(cnxapi.EventInvoiceVerified)
	last := events[len(events)-1]
	if last["reversed_invoice_id"] != original.PurchaseInvoiceID || last["colour"] != cnxapi.ColourRed {
		t.Fatalf("event %v", last)
	}
	if ids, _ := last["review_commitment_ids"].([]any); len(ids) != 1 || ids[0] != "commitment.1" {
		t.Fatalf("review commitments %v", last)
	}
	lines, err := w.pur.ResolvePurchaseLines(w.tax, []purapi.CommitmentLineRef{{CommitmentID: "commitment.1", OrderLineID: "L1"}})
	if pl := lines[purapi.CommitmentLineRef{CommitmentID: "commitment.1", OrderLineID: "L1"}]; err != nil || pl.VerifiedQuantity != "120" {
		t.Fatalf("the delivery facts stay: %+v %v", pl, err)
	}
	cny := &cnxapi.CurrencyRef{ID: w.cny.ConfigID, Revision: w.cny.Revision}
	plan := cnxapi.RecordTaxResult{Stage: cnxapi.TaxPlanned, Currency: cny, Amount: "1.00", TaxPeriod: "2026-11",
		Review: &cnxapi.ProfessionalReview{Reviewer: "合成税务师（测试）", EvidenceIDs: []string{w.review}}}
	_, err = w.record(w.tax, c, plan)
	w.want(err, kernel.CodeInvalidState)
	_, err = w.allocate(w.tax, "lot.5", alloc("t2", "x1", original.PurchaseInvoiceID, cnxapi.PurposeExportRefund, "1"))
	w.want(err, kernel.CodeInvalidState)

	// The supplier's corrected invoice: matched again (the reversed match no longer stands) and
	// allocated in place of the reviewed allocation.
	replacement := w.invoice(w.facts("SYN0007", "120"), l1("120"))
	repl := alloc("t3", "x1", replacement.PurchaseInvoiceID, cnxapi.PurposeExportRefund, "120")
	_, err = w.allocate(w.tax, "lot.5", repl)
	w.want(err, cnxapi.CodeTaxEvidenceExceeded)
	repl.Replaces = "t1"
	c, err = w.allocate(w.tax, "lot.5", repl)
	w.must(err)
	v := w.taxCase(c.CaseID)
	if len(v.Allocations) != 2 || v.Allocations[0].Status != cnxapi.EvidenceReplaced || v.Allocations[0].ReplacedBy != "t3" ||
		v.Allocations[1].Status != cnxapi.EvidenceStanding || v.Allocations[1].Replaces != "t1" {
		t.Fatalf("replaced %+v", v.Allocations)
	}
	if _, err = w.record(w.tax, c, plan); err != nil {
		t.Fatalf("planned once nothing is under review: %v", err)
	}
}

// The commit orders where a red reversal meets an allocation of the invoice it reverses, on PG16
// (both lock the blue invoice's root FOR UPDATE): an allocation committed first is under review
// once the red invoice is verified; a red invoice verified first makes the allocation INVALID_STATE;
// started together while the invoice row is held, they serialize on it and end in one of these two
// outcomes, never in a standing allocation of a reversed invoice.
func TestRedReversalAndAllocationCommitOrders(t *testing.T) {
	w := newInvoiceWorld(t)
	db, err := bentopg.NewDB(w.pool)
	w.must(err)
	lw := &licenseWorld{world: w.world, app: db}
	for round := range 3 {
		n := string(rune('a' + round))
		blue := w.facts("SYN010"+n, "100")
		inv := w.invoice(blue)
		w.lot("lot."+n, time.Now().UTC(), "x1", "100")
		redFacts := w.red("SYN020"+n, blue)
		r := w.imported(w.purchaser, redFacts)
		allocate := func() (cnxapi.TaxCaseReceipt, error) {
			return w.allocate(w.tax, "lot."+n, alloc("t."+n, "x1", inv.PurchaseInvoiceID, cnxapi.PurposeExportRefund, "100"))
		}
		reverse := func() error {
			_, err := w.cnx.VerifyInvoice(w.tax2, w.verifyCmd(r, redFacts))
			return err
		}
		switch round {
		case 0:
			c, err := allocate()
			w.must(err)
			w.must(reverse())
			if v := w.taxCase(c.CaseID); v.Allocations[0].Status != cnxapi.EvidenceReview {
				t.Fatalf("allocation first: %+v", v.Allocations)
			}
		case 1:
			w.must(reverse())
			_, err := allocate()
			w.want(err, kernel.CodeInvalidState)
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
			type outcome struct {
				c   cnxapi.TaxCaseReceipt
				err error
			}
			allocated, reversed := make(chan outcome, 1), make(chan error, 1)
			go func() { c, err := allocate(); allocated <- outcome{c, err} }()
			go func() { reversed <- reverse() }()
			waitBlocked(t, w.pool, p, 2)
			close(release)
			w.must(<-holder)
			w.must(<-reversed)
			a := <-allocated
			switch {
			case a.err == nil:
				if v := w.taxCase(a.c.CaseID); v.Allocations[0].Status != cnxapi.EvidenceReview {
					t.Fatalf("allocated, then reversed: %+v", v.Allocations)
				}
			case code(a.err) != kernel.CodeInvalidState:
				t.Fatalf("reversed, then allocated: %v", a.err)
			}
		}
		if v := w.view(inv.PurchaseInvoiceID); v.Status != cnxapi.InvoiceReversed {
			t.Fatalf("round %d: %s", round, v.Status)
		}
	}
}

// CNX-INV-004/005 and D-3 in the database: brant_app cannot change an invoice's import, a case's
// binding, an allocation or a result, nor delete; the schema owner bypasses no trigger: an invoice
// keeps its import and is decided once, a case follows SM-CNX-TAXCASE, an allocation beyond the
// invoice line's balance is refused, entries never change.
func TestInvoiceAndTaxCaseRecordsAreAppendOnly(t *testing.T) {
	w := newInvoiceWorld(t)
	inv := w.invoice(w.facts("SYN0099", "100"))
	w.lot("lot.db", time.Now().UTC(), "x1", "100", "x2", "100")
	_, err := w.allocate(w.tax, "lot.db", alloc("t1", "x1", inv.PurchaseInvoiceID, cnxapi.PurposeExportRefund, "100"))
	w.must(err)
	for _, sql := range []string{
		`UPDATE brant_cnx.purchase_invoice SET candidate = '{}'`,
		`UPDATE brant_cnx.tax_refund_case SET policy_revision = 2`,
		`UPDATE brant_cnx.tax_evidence_allocation SET quantity = 1`,
		`DELETE FROM brant_cnx.tax_evidence_allocation`,
		`DELETE FROM brant_cnx.purchase_invoice`,
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
		{`UPDATE brant_cnx.purchase_invoice SET candidate = '{}', row_version = row_version + 1`, "keeps its import"},
		{`UPDATE brant_cnx.purchase_invoice SET status = 'imported', row_version = row_version + 1`, "keeps its import"},
		{`DELETE FROM brant_cnx.purchase_invoice`, "never deleted"},
		{`UPDATE brant_cnx.tax_refund_case SET status = 'received', row_version = row_version + 1`, "follows SM-CNX-TAXCASE"},
		{`UPDATE brant_cnx.tax_refund_case SET policy_revision = 2, row_version = row_version + 1`, "follows SM-CNX-TAXCASE"},
		{`UPDATE brant_cnx.tax_evidence_allocation SET quantity = 1`, "never change"},
		{`UPDATE brant_cnx.purchase_invoice_line SET quantity = 1000`, "never change"},
		// 100 more of the 100 PCS line for x2: Σ 200.
		{`INSERT INTO brant_cnx.tax_evidence_allocation SELECT tenant_id, company_id, 'forged', case_id, 'x2', shipment_id, order_line_id, sku_id,
			spec_revision, export_quantity, export_unit, purchase_invoice_id, line_no, purpose, quantity, unit, NULL, allocated_by_kind, allocated_by_id,
			authority_epoch, allocated_at FROM brant_cnx.tax_evidence_allocation`, "would be exceeded"},
		{`INSERT INTO brant_cnx.tax_evidence_allocation SELECT tenant_id, company_id, 'forged', case_id, 'x2', shipment_id, order_line_id, sku_id,
			spec_revision, export_quantity, export_unit, purchase_invoice_id, line_no, purpose, 1, unit, allocation_id, allocated_by_kind, allocated_by_id,
			authority_epoch, allocated_at FROM brant_cnx.tax_evidence_allocation`, "replaces no allocation under review"},
	} {
		if err := owner(x.sql); err == nil || !strings.Contains(err.Error(), x.want) {
			t.Errorf("must refuse %s: %v", x.sql, err)
		}
	}
}

// The composed process over HTTP with OIDC access tokens: the purchaser imports an invoice through
// CNX's command route, the tax specialist verifies it, reads it and the company's invoices and
// cases, and reconciles the verification; an allocation is refused (FUL is not wired:
// DEPENDENCY_NOT_SUPPORTED), the AI path to verification is 403.
func TestPurchaseInvoicesOverHTTP(t *testing.T) {
	w := newInvoiceWorld(t)
	is := newIssuer(t)
	handler, err := w.platform.API(bootstrap.APIConfig{Issuer: is.srv.URL, Audience: "brant-api", AllowInsecureHTTP: true})
	w.must(err)
	api := httptest.NewServer(handler)
	t.Cleanup(api.Close)
	buyer := &client{t: t, base: api.URL, token: is.token(w.tenant, "company.A", "purchaser.1", "human"), n: 500}
	tax := &client{t: t, base: api.URL, token: is.token(w.tenant, "company.A", "tax.1", "human"), n: 600}
	ai := &client{t: t, base: api.URL, token: is.token(w.tenant, "company.A", "agent.tax", "ai"), n: 700}
	f := w.facts("SYN0800", "120")
	facts := map[string]any{"kind": f.Kind, "colour": f.Colour, "number": f.Number, "issued_on": f.IssuedOn,
		"supplier": map[string]any{"party_id": "supplier.1", "name": f.Supplier.Name, "tax_id": f.Supplier.TaxID},
		"buyer":    map[string]any{"name": f.Buyer.Name, "tax_id": f.Buyer.TaxID}, "currency": map[string]any{"id": w.cny.ConfigID, "revision": w.cny.Revision},
		"lines": []any{map[string]any{"line_no": 1, "description": f.Lines[0].Description, "quantity": "120", "unit": "PCS",
			"net_amount": f.Lines[0].NetAmount, "tax_rate": "0.13", "tax_amount": f.Lines[0].TaxAmount}},
		"net_total": f.NetTotal, "tax_total": f.TaxTotal, "gross_total": f.GrossTotal}
	imported := buyer.command("cnx", cnxapi.Contract, cnxapi.CommandImportInvoice, map[string]any{"file_id": w.original, "candidate": facts})
	id, _ := imported["object_id"].(string)
	if imported["status"] != "imported" || imported["row_version"] != float64(1) || id == "" {
		t.Fatalf("import %v", imported)
	}
	verifyArgs := map[string]any{"purchase_invoice_id": id, "expected_version": 1, "decision": "verify", "facts": facts}
	status, out := ai.try("cnx", cnxapi.Contract, cnxapi.CommandVerifyInvoice, verifyArgs)
	if status != 403 || out["code"] != kernel.CodePermissionDenied {
		t.Fatalf("AI verify %d %v", status, out)
	}
	verify := map[string]any{"contract": cnxapi.Contract, "command": cnxapi.CommandVerifyInvoice, "idempotency_key": "c3b.http.verify", "args": verifyArgs}
	status, v := tax.do("POST", "/api/brant/v1/cnx/commands/"+cnxapi.CommandVerifyInvoice, verify)
	if status != 200 || v["status"] != "verified" || v["row_version"] != float64(2) {
		t.Fatalf("verify %d %v", status, v)
	}
	status, rec := tax.do("POST", "/api/brant/v1/cnx/commands/"+cnxapi.CommandVerifyInvoice+"/reconcile", verify)
	if status != 200 || rec["state"] != "committed" || rec["action_id"] != v["action_id"] {
		t.Fatalf("reconcile %d %v", status, rec)
	}
	status, view := tax.do("GET", "/api/brant/v1/cnx/purchase_invoices/"+id, nil)
	verified, _ := view["verified"].(map[string]any)
	items := func(v any) int { s, _ := v.([]any); return len(s) }
	if status != 200 || view["status"] != "verified" || verified["number"] != "SYN0800" || verified["currency_code"] != "CNY" ||
		items(view["balances"]) != 2 || items(view["differences"]) != 0 {
		t.Fatalf("invoice %d %v", status, view)
	}
	if status, list := tax.do("GET", "/api/brant/v1/cnx/purchase_invoices", nil); status != 200 || len(list["purchase_invoices"].([]any)) != 1 {
		t.Fatalf("list %d %v", status, list)
	}
	if status, list := tax.do("GET", "/api/brant/v1/cnx/tax_refund_cases", nil); status != 200 || len(list["tax_refund_cases"].([]any)) != 0 {
		t.Fatalf("cases %d %v", status, list)
	}
	status, out = tax.try("cnx", cnxapi.Contract, cnxapi.CommandAllocateTaxEvidence, map[string]any{"export_lot_id": "lot.1", "policy_id": "export.tax",
		"allocations": []any{map[string]any{"allocation_id": "t1", "export_line_id": "x1", "purchase_invoice_id": id, "line_no": 1,
			"purpose": "export_refund", "quantity": "120"}}})
	if status != 422 || out["code"] != cnxapi.CodeDependencyNotSupported {
		t.Fatalf("allocate %d %v", status, out)
	}
	status, out = tax.do("GET", "/api/brant/v1/cnx/tax_refund_cases/trc-none", nil)
	if status != 404 || out["code"] != kernel.CodeNotFound {
		t.Fatalf("no such case %d %v", status, out)
	}
}
