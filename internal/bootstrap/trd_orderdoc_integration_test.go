//go:build integration

package bootstrap_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The order documents suite (WBS S3-15 acceptance "PI 绑定订单修订"; TRD-INV-020; SIM-02 step 6:
// 生成 PI，客户回签; master data MD-35: 绑定订单修订哈希；签署版本不可变；草稿不能冒充已签署合同; SAP
// reference O2C-05) on the order world with the legal terms of WBS S3-17: POL publishes the
// order-text/0.1.0 templates order.sc (an S/C with the law section and one clause), order.sc.plain
// (an S/C without it) and order.pi (a PI with the law section), and the number sequences order.seq
// (document type order, SC-) and pi.seq (proforma_invoice, PI-). "editor" generates and records the
// signed copies, "reviewer" decides and applies; an AI agent may generate, never record a signed
// copy. Python has none of this.

type docWorld struct {
	*legalWorld
	sc, plain, pi, scSeq, piSeq polapi.ConfigRef
}

const legalLines = "Legal terms: legal.default r1\nGoverning law: CN | CISG: applies\n" +
	"Contract formation: acceptance effective on arrival | CISG Art. 18(2), 23\n" +
	"Disputes: arbitration | China International Economic and Trade Arbitration Commission (CIETAC) | seat Shanghai | language en"

// newDocWorld is the order world with the templates and sequences, and with legal's setup
// (legalOn) when legal.
func newDocWorld(t *testing.T, legal bool) *docWorld {
	l := &legalWorld{trdWorld: newOrderWorld(t)}
	if legal {
		l = legalOn(l.trdWorld)
	}
	ed, rv, from := l.author, l.reviewer, l.win.from
	template := func(id, kind, title string, sections, clauses []string) polapi.ConfigRef {
		return l.configure(ed, rv, polapi.KindOrderDocumentTemplate, id, 0, from, time.Time{}, polapi.ConfigData{
			OrderDocumentTemplate: &polapi.OrderDocumentTemplate{Name: id, DocumentKind: kind, Language: polapi.LanguageEn,
				RendererVersion: polapi.OrderRendererVersion, Title: title, Sections: sections, Clauses: clauses, FooterNote: "合成测试，非真实合同"}}, true)
	}
	sequence := func(id, documentType, prefix string) polapi.ConfigRef {
		return l.configure(ed, rv, polapi.KindNumberSequence, id, 0, from, time.Time{}, polapi.ConfigData{NumberSequence: &polapi.NumberSequence{
			DocumentType: documentType, Prefix: prefix, Width: 5, Reset: polapi.ResetAnnual, TimeZone: "Asia/Shanghai"}}, true)
	}
	clauses := []string{"Quality as per the approved sample."}
	d := &docWorld{legalWorld: l}
	d.sc = template("order.sc", polapi.OrderDocumentSalesContract, "SALES CONTRACT", []string{"seller", "buyer", "identity", "lines", "totals",
		"terms", "delivery", "payments", "clauses", "law", "signatures", "notes"}, clauses)
	d.plain = template("order.sc.plain", polapi.OrderDocumentSalesContract, "SALES CONTRACT", []string{"seller", "buyer", "identity", "lines",
		"totals", "terms", "delivery", "payments", "clauses", "signatures", "notes"}, clauses)
	d.pi = template("order.pi", polapi.OrderDocumentProformaInvoice, "PROFORMA INVOICE", []string{"seller", "buyer", "identity", "lines",
		"totals", "terms", "delivery", "payments", "law", "notes"}, nil)
	d.scSeq = sequence("order.seq", polapi.DocumentTypeOrder, "SC-")
	d.piSeq = sequence("pi.seq", polapi.DocumentTypeProformaInvoice, "PI-")
	return d
}

// legalDraftOrder has quote id drafted to seal the tenant's default legal terms (legalWorld.draft)
// and accepted, and order "order.<id>" prepared: a draft order.
func (d *docWorld) legalDraftOrder(id string) trdapi.OrderReceipt {
	d.t.Helper()
	o, err := d.c.TRD.PrepareOrder(d.author, d.prepareCmd("order."+id, d.acceptedWith(id, d.draft())))
	d.must(err)
	return o
}

func (d *docWorld) generateCmd(order string, rev int64, hash, kind string, template, sequence polapi.ConfigRef) trdapi.GenerateOrderDocument {
	return trdapi.GenerateOrderDocument{IdempotencyKey: d.key(), OrderID: order, ExpectedOrderRevision: rev, ExpectedOrderHash: hash,
		DocumentKind: kind, TemplateRef: tref(template), SequenceRef: tref(sequence)}
}

func (d *docWorld) signCmd(order string, rev int64, kind, signature, file string) trdapi.RecordSignedOrderDocument {
	return trdapi.RecordSignedOrderDocument{IdempotencyKey: d.key(), OrderID: order, OrderRevision: rev, DocumentKind: kind,
		SignatureKind: signature, FileID: file, Evidence: d.proof}
}

// scan registers an EVD text file id holding content: a signed copy as scanned.
func (d *docWorld) scan(id, content string) {
	d.t.Helper()
	_, err := d.file(d.author, id, id+".txt", evdapi.MimeText, content, "")
	d.must(err)
}

func (d *docWorld) document(order string, rev int64, kind string) trdapi.OrderDocument {
	d.t.Helper()
	v, err := d.c.TRD.GetOrderDocument(d.author, order, rev, kind)
	d.must(err)
	return v
}

// WBS S3-15 "PI 绑定订单修订", TRD-INV-020 (positive: r1 订单生成 PI 并登记客户回签件; negative: 订单 r2
// 引用 r1 的 PI 返回 ORDER_DOCUMENT_STALE) and SIM-02 step 6. The draft order's PI is generated before
// order.activate, numbered from pi.seq, bound to revision 1 and its hash, and prints the legal terms
// the source quote sealed; the customer's countersigned copy is recorded; the unsigned bytes
// registered as a file are no signed copy; a recorded copy never changes. The order is activated and
// an AI agent generates its S/C (not from a PI template, a PI sequence or a template that leaves the
// law out), which we sign. The customer reduces L1 to 100: at revision 2 the S/C of revision 1 takes
// no countersigned copy and no generation binds revision 1 (ORDER_DOCUMENT_STALE); revision 1's
// documents stay readable. POL moves legal.default on to Hamburg arbitration, and revision 2's S/C —
// the order's one contract number, file "-r2" — still prints the terms the source quote sealed. Each
// document and copy is one row, its commit evidence names POL's template and sequence in force, its
// events follow; the database refuses a copy of a stale document and any change of a document.
func TestOrderDocumentsBindTheOrderRevision(t *testing.T) {
	d := newDocWorld(t, true)
	ed, rv := d.author, d.reviewer
	agent := d.actorOf(kernel.ActorAI, "agent.docs")
	o := d.legalDraftOrder("quote.d1")
	id := o.ObjectID
	v1 := d.getOrder(id)

	// SIM-02 step 6: the PI of the draft order.
	pi, err := d.c.TRD.GenerateOrderDocument(ed, d.generateCmd(id, 1, v1.OrderHash, trdapi.DocumentProformaInvoice, d.pi, d.piSeq))
	d.must(err)
	if pi.Status != trdapi.StatusGenerated || pi.ObjectID != id || pi.RowVersion != v1.RowVersion || pi.OrderRevision != 1 ||
		pi.OrderHash != v1.OrderHash || !strings.HasPrefix(pi.Number, "PI-") || pi.FileName != pi.Number+"-r1.txt" || pi.MimeType != evdapi.MimeText {
		t.Fatalf("the draft order's PI: %+v", pi)
	}
	got := d.document(id, 1, trdapi.DocumentProformaInvoice)
	text := string(got.Content)
	if sha256Hex(text) != pi.DocumentSHA256 || len(got.Content) != pi.SizeBytes || !got.Document.Current ||
		!strings.HasPrefix(text, "PROFORMA INVOICE\n\nSeller: ") || !strings.Contains(text, "\nNumber: "+pi.Number+"\nOrder: "+id+" r1\n") ||
		!strings.Contains(text, "\nNot a commercial invoice or a tax invoice.\n") || !strings.Contains(text, "\n\n"+legalLines+"\n\n") {
		t.Fatalf("the PI as stored:\n%s", text)
	}

	// The customer's countersigned PI, recorded by a human only, once.
	d.scan("file.pi.back", "countersigned "+pi.Number)
	_, err = d.c.TRD.RecordSignedOrderDocument(agent, d.signCmd(id, 1, trdapi.DocumentProformaInvoice, trdapi.SignatureCountersigned, "file.pi.back"))
	d.want(err, kernel.CodePermissionDenied)
	back := d.signCmd(id, 1, trdapi.DocumentProformaInvoice, trdapi.SignatureCountersigned, "file.pi.back")
	s, err := d.c.TRD.RecordSignedOrderDocument(ed, back)
	d.must(err)
	if s.Status != trdapi.SignatureCountersigned || s.SignedFileID != "file.pi.back" || s.SignedSHA256 != sha256Hex("countersigned "+pi.Number) ||
		s.DocumentSHA256 != pi.DocumentSHA256 || s.RowVersion != v1.RowVersion || s.Replayed {
		t.Fatalf("the countersigned PI: %+v", s)
	}
	if r, err := d.c.TRD.RecordSignedOrderDocument(ed, back); err != nil || !r.Replayed || r.SignedFileID != "file.pi.back" {
		t.Fatalf("a replay: %+v %v", r, err)
	}
	d.scan("file.pi.unsigned", text)
	_, err = d.c.TRD.RecordSignedOrderDocument(ed, d.signCmd(id, 1, trdapi.DocumentProformaInvoice, trdapi.SignatureSigned, "file.pi.unsigned"))
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/file_id" {
		t.Fatalf("the unsigned PI as a signed copy: %v", err)
	}
	d.scan("file.pi.other", "another countersigned copy")
	_, err = d.c.TRD.RecordSignedOrderDocument(ed, d.signCmd(id, 1, trdapi.DocumentProformaInvoice, trdapi.SignatureCountersigned, "file.pi.other"))
	if code(err) != kernel.CodeInvalidState || path(err) != "/args/signature_kind" {
		t.Fatalf("another countersigned copy: %v", err)
	}
	_, err = d.c.TRD.RecordSignedOrderDocument(ed, d.signCmd(id, 1, trdapi.DocumentSalesContract, trdapi.SignatureSigned, "file.pi.other"))
	d.want(err, trdapi.CodeDocumentBindingMissing)
	_, err = d.c.TRD.RecordSignedOrderDocument(ed, d.signCmd(id, 1, trdapi.DocumentProformaInvoice, trdapi.SignatureSigned, "file.none"))
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/file_id" {
		t.Fatalf("an unknown file: %v", err)
	}

	// The order is activated; an AI agent generates its S/C.
	rvw := d.reviewed(o, "review.quote.d1")
	_, err = d.c.TRD.ActivateOrder(ed, d.activateOrderCmd(id, rvw.RowVersion, "review.quote.d1"))
	d.must(err)
	v := d.getOrder(id)
	for _, c := range []struct {
		template, sequence polapi.ConfigRef
		path               string
	}{{d.pi, d.scSeq, "/args/template_ref"}, {d.sc, d.piSeq, "/args/sequence_ref"}, {d.plain, d.scSeq, "/template/sections"}} {
		_, err = d.c.TRD.GenerateOrderDocument(agent, d.generateCmd(id, 1, v.OrderHash, trdapi.DocumentSalesContract, c.template, c.sequence))
		if code(err) != kernel.CodeReferenceInvalid || path(err) != c.path {
			t.Fatalf("an S/C from %s and %s: %v", c.template.ConfigID, c.sequence.ConfigID, err)
		}
	}
	first := d.generateCmd(id, 1, v.OrderHash, trdapi.DocumentSalesContract, d.sc, d.scSeq)
	sc1, err := d.c.TRD.GenerateOrderDocument(agent, first)
	d.must(err)
	if !strings.HasPrefix(sc1.Number, "SC-") || sc1.FileName != sc1.Number+"-r1.txt" || sc1.RowVersion != v.RowVersion {
		t.Fatalf("the S/C of revision 1: %+v", sc1)
	}
	if r, err := d.c.TRD.GenerateOrderDocument(agent, first); err != nil || !r.Replayed || r.DocumentSHA256 != sc1.DocumentSHA256 {
		t.Fatalf("a replay: %+v %v", r, err)
	}
	_, err = d.c.TRD.GenerateOrderDocument(ed, d.generateCmd(id, 1, v.OrderHash, trdapi.DocumentSalesContract, d.sc, d.scSeq))
	if code(err) != trdapi.CodeBusinessKeyConflict || path(err) != "/args/document_kind" {
		t.Fatalf("a second S/C of revision 1: %v", err)
	}
	d.scan("file.sc.signed", "signed "+sc1.Number)
	_, err = d.c.TRD.RecordSignedOrderDocument(ed, d.signCmd(id, 1, trdapi.DocumentSalesContract, trdapi.SignatureSigned, "file.sc.signed"))
	d.must(err)

	// The customer reduces L1 to 100: revision 2.
	a := d.approvedAmendment(id, "amendment.1", 1, v.OrderHash, "100")
	r2, err := d.c.TRD.ApplyAmendment(rv, d.applyCmd(id, "amendment.1", a.RowVersion))
	d.must(err)
	d.scan("file.sc.back", "countersigned "+sc1.Number)
	_, err = d.c.TRD.RecordSignedOrderDocument(ed, d.signCmd(id, 1, trdapi.DocumentSalesContract, trdapi.SignatureCountersigned, "file.sc.back"))
	if rej, ok := kernel.AsRejection(err); !ok || rej.Code != trdapi.CodeOrderDocumentStale || rej.Category != kernel.CategoryConflict ||
		path(err) != "/document_binding" {
		t.Fatalf("countersigning revision 1's S/C at revision 2: %v", err)
	}
	_, err = d.c.TRD.GenerateOrderDocument(ed, d.generateCmd(id, 1, v.OrderHash, trdapi.DocumentProformaInvoice, d.pi, d.piSeq))
	if code(err) != trdapi.CodeOrderDocumentStale || path(err) != "/args/expected_order_revision" {
		t.Fatalf("a PI of revision 1 at revision 2: %v", err)
	}
	if old := d.document(id, 1, trdapi.DocumentSalesContract); old.Document.Current || sha256Hex(string(old.Content)) != sc1.DocumentSHA256 ||
		len(old.Document.Signatures) != 1 {
		t.Fatalf("revision 1's S/C after the amendment: %+v", old.Document)
	}

	// POL moves legal.default on; revision 2's S/C prints what the source quote sealed.
	d.configure(ed, rv, polapi.KindLegalTerms, "legal.default", d.rootVersion(polapi.KindLegalTerms, "legal.default"), d.win.from, time.Time{},
		termsData("CISG with PRC law, Hamburg", polapi.GoverningLawCN, polapi.CISGApplies, polapi.FormationArrival, hamburgArbitration), true)
	sc2, err := d.c.TRD.GenerateOrderDocument(ed, d.generateCmd(id, 2, r2.OrderHash, trdapi.DocumentSalesContract, d.sc, d.scSeq))
	d.must(err)
	text2 := string(d.document(id, 2, trdapi.DocumentSalesContract).Content)
	if sc2.Number != sc1.Number || sc2.FileName != sc1.Number+"-r2.txt" || sc2.OrderHash != r2.OrderHash || sha256Hex(text2) != sc2.DocumentSHA256 ||
		!strings.Contains(text2, "\nOrder: "+id+" r2\nOrder SHA-256: "+r2.OrderHash+"\n") || !strings.Contains(text2, "\nAmendment: amendment.1 of r1\n") ||
		!strings.Contains(text2, " | 100 PCS | ") || !strings.Contains(text2, "\n\n"+legalLines+"\n\n") || strings.Contains(text2, "Hamburg") ||
		!strings.Contains(text2, "\n1. Quality as per the approved sample.\n") {
		t.Fatalf("revision 2's S/C %+v:\n%s", sc2, text2)
	}
	d.scan("file.sc2.back", "countersigned "+sc2.FileName)
	_, err = d.c.TRD.RecordSignedOrderDocument(ed, d.signCmd(id, 2, trdapi.DocumentSalesContract, trdapi.SignatureCountersigned, "file.sc2.back"))
	d.must(err)

	// The reads, the commit evidence, the rows and the events.
	view := d.getOrder(id)
	if len(view.Documents) != 3 || view.RowVersion != r2.OrderRowVersion {
		t.Fatalf("the order's documents %+v", view.Documents)
	}
	current := 0
	for _, doc := range view.Documents {
		if doc.Current {
			current++
		}
	}
	last := view.Documents[2]
	e := last.Entries
	if current != 1 || !last.Current || last.OrderRevision != 2 || last.DocumentKind != trdapi.DocumentSalesContract || len(last.Signatures) != 1 ||
		last.Signatures[0].SignatureKind != trdapi.SignatureCountersigned || last.GeneratedBy != (kernel.ActorRef{Kind: kernel.ActorHuman, ID: "editor"}) ||
		e["command"] != trdapi.CommandGenerateOrderDocument || e["order_revision"] != "2" || e["order_hash"] != r2.OrderHash ||
		e["number"] != sc1.Number || e["template_content_hash"] != d.contentHash(d.sc) || e["renderer_version"] != polapi.OrderRendererVersion ||
		e["pol:order_document_template:order.sc@1"] != "in_force/"+d.contentHash(d.sc) ||
		e["pol:number_sequence:order.seq@1"] != "in_force/"+d.contentHash(d.scSeq) || e["content_sha256"] != sc2.DocumentSHA256 ||
		e["generated_by"] != "human:editor" || e["upstream_checked_at"] == "" {
		t.Fatalf("revision 2's S/C as read: %+v", last)
	}
	if docs, sigs := d.count("company.A", "brant_trd.sales_order_document"), d.count("company.A", "brant_trd.sales_order_document_signature"); docs != 3 ||
		sigs != 3 {
		t.Fatalf("%d documents, %d signed copies", docs, sigs)
	}
	generated, signed := d.eventsOf(trdapi.EventOrderDocumentGenerated), d.eventsOf(trdapi.EventOrderDocumentSigned)
	if len(generated) != 3 || len(signed) != 3 {
		t.Fatalf("%d OrderDocumentGenerated, %d OrderDocumentSigned", len(generated), len(signed))
	}
	g := generated[2].Payload
	if g["aggregate_type"] != "SalesOrder" || g["order_id"] != id || g["document_kind"] != trdapi.DocumentSalesContract ||
		g["document_order_revision"] != float64(2) || g["file_sha256"] != sc2.DocumentSHA256 || g["number"] != sc1.Number ||
		g["file_name"] != sc2.FileName || g["template_ref"] == "" {
		t.Fatalf("OrderDocumentGenerated %v", g)
	}
	if p := signed[0].Payload; p["signature_kind"] != trdapi.SignatureCountersigned || p["file_id"] != "file.pi.back" ||
		p["document_sha256"] != pi.DocumentSHA256 || p["document_kind"] != trdapi.DocumentProformaInvoice {
		t.Fatalf("OrderDocumentSigned %v", p)
	}

	// The database refuses a signed copy of revision 1's S/C and any change of a document.
	err = d.inTRD(func(ctx context.Context, exec bentopg.Executor) error {
		_, err := exec.Exec(ctx, `INSERT INTO brant_trd.sales_order_document_signature (tenant_id, company_id, order_id, order_revision,
			document_kind, signature_kind, file_id, file_sha256, file_mime_type, document_sha256, order_hash, idempotency_key, recorded_by_kind,
			recorded_by_id, record_epoch, recorded_at, evidence)
			SELECT tenant_id, company_id, order_id, order_revision, document_kind, 'countersigned', 'file.sc.back', repeat('a', 64), 'text/plain',
				content_sha256, order_hash, 'k.direct', 'human', 'editor', 1, now(), '[]'
			FROM brant_trd.sales_order_document WHERE order_id = $1 AND order_revision = 1 AND document_kind = 'sales_contract'`, id)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "order's current revision") {
		t.Fatalf("a signed copy of a stale document, inserted: %v", err)
	}
	err = d.inTRD(func(ctx context.Context, exec bentopg.Executor) error {
		_, err := exec.Exec(ctx, `UPDATE brant_trd.sales_order_document SET number = number WHERE order_id = $1`, id)
		return err
	})
	if err == nil {
		t.Fatal("a document changed")
	}
}

// An order whose source quote sealed no legal terms prints none: a template with the law section is
// REFERENCE_INVALID, one without it serves. An order that ended takes no new document
// (INVALID_STATE), and its current documents still take their signed copies.
func TestOrderDocumentsWithoutLegalTermsAndAfterTheEnd(t *testing.T) {
	d := newDocWorld(t, false)
	ed := d.author
	o := d.activeOrder("quote.1")
	v := d.getOrder(o.ObjectID)
	_, err := d.c.TRD.GenerateOrderDocument(ed, d.generateCmd(o.ObjectID, 1, v.OrderHash, trdapi.DocumentSalesContract, d.sc, d.scSeq))
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/template/sections" {
		t.Fatalf("a law section without legal terms: %v", err)
	}
	sc, err := d.c.TRD.GenerateOrderDocument(ed, d.generateCmd(o.ObjectID, 1, v.OrderHash, trdapi.DocumentSalesContract, d.plain, d.scSeq))
	d.must(err)
	if text := string(d.document(o.ObjectID, 1, trdapi.DocumentSalesContract).Content); strings.Contains(text, "Legal terms") ||
		!strings.HasSuffix(text, "\n\n合成测试，非真实合同\n") {
		t.Fatalf("the S/C without legal terms:\n%s", text)
	}
	_, err = d.c.TRD.CancelOrder(ed, trdapi.CancelOrder{IdempotencyKey: d.key(), OrderID: o.ObjectID, ExpectedVersion: v.RowVersion,
		Reason: "客户撤单", Evidence: d.proof})
	d.must(err)
	ended := d.getOrder(o.ObjectID)
	_, err = d.c.TRD.GenerateOrderDocument(ed, d.generateCmd(o.ObjectID, 1, ended.OrderHash, trdapi.DocumentProformaInvoice, d.pi, d.piSeq))
	if code(err) != kernel.CodeInvalidState || path(err) != "/args/order_id" {
		t.Fatalf("a PI of a cancelled order: %v", err)
	}
	d.scan("file.signed", "signed "+sc.Number)
	if _, err = d.c.TRD.RecordSignedOrderDocument(ed, d.signCmd(o.ObjectID, 1, trdapi.DocumentSalesContract, trdapi.SignatureSigned,
		"file.signed")); err != nil {
		t.Fatalf("the signed copy of a cancelled order's S/C: %v", err)
	}
}

// TRD-INV-020 under concurrency. A generation rendered for revision 1 while an amendment applies:
// the apply holds the order; the generation, prepared and numbered on revision 1, waits for the
// quote root, then finds revision 2 (ORDER_DOCUMENT_STALE) and writes nothing; revision 2 then gets
// its S/C with the order's number. Two unsynchronized generations of one kind and revision: one
// wins, the other is BUSINESS_KEY_CONFLICT, and both were issued the order's one number.
func TestADocumentRenderedWhileTheOrderMovesIsStale(t *testing.T) {
	d := newDocWorld(t, false)
	ds := newDownstream(d.world)
	o := d.activeOrder("quote.1")
	h := d.getOrder(o.ObjectID).OrderHash
	a := d.approvedAmendment(o.ObjectID, "amendment.1", 1, h, "100")
	apply := d.applyCmd(o.ObjectID, "amendment.1", a.RowVersion)
	blocker, release := ds.keyBlocker(t, "brant_trd", trdapi.CommandApplyAmendment, apply.IdempotencyKey)
	done := make(chan error, 1)
	go func() {
		_, err := d.c.TRD.ApplyAmendment(d.reviewer, apply)
		done <- err
	}()
	if !awaitInHold(t, "the apply waits at its result", func() bool { return ds.blocks(blocker) }, done) {
		release()
		return
	}
	late := make(chan error, 1)
	go func() {
		_, err := d.c.TRD.GenerateOrderDocument(d.author, d.generateCmd(o.ObjectID, 1, h, trdapi.DocumentSalesContract, d.plain, d.scSeq))
		late <- err
	}()
	ok := awaitInHold(t, "the generation waits for the quote root", func() bool { return lockWaiter(t, "brant_trd.quote_root") }, late)
	release()
	if !ok {
		return
	}
	d.must(<-done)
	if err := <-late; code(err) != trdapi.CodeOrderDocumentStale {
		t.Fatalf("the generation behind the apply: %v", err)
	}
	if n := d.count("company.A", "brant_trd.sales_order_document"); n != 0 {
		t.Fatalf("%d documents", n)
	}
	v := d.getOrder(o.ObjectID)
	var wg sync.WaitGroup
	results := make([]trdapi.OrderDocumentReceipt, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = d.c.TRD.GenerateOrderDocument(d.author, d.generateCmd(o.ObjectID, 2, v.OrderHash, trdapi.DocumentSalesContract,
				d.plain, d.scSeq))
		}()
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("two generations: %v, %v", errs[0], errs[1])
	}
	for i, err := range errs {
		if err != nil && code(err) != trdapi.CodeBusinessKeyConflict {
			t.Fatalf("the losing generation: %v", err)
		}
		if err == nil && results[i].FileName != results[i].Number+"-r2.txt" {
			t.Fatalf("the winner %+v", results[i])
		}
	}
	if n := d.count("company.A", "brant_trd.sales_order_document"); n != 1 {
		t.Fatalf("%d documents", n)
	}
}

// Contract 06 §07 for order.document.record_signed (sources.go; Context Map R16): a countersigned
// copy recorded on a message another team registered. Once the author's data scope no longer shows
// that source (it still sees the order), the replay and the reconciliations with a matching and a
// conflicting hash are REFERENCE_INVALID and write nothing; with the source readable again they
// answer.
func TestSignedCopyReplaysNeedTheCitedSources(t *testing.T) {
	d := newDocWorld(t, false)
	outsider := d.provision(bootstrap.Grant{Scope: d.scope("company.A"), Actor: actorRef("human", "outsider"), Permissions: allPermissions,
		DataScope: "team", Team: "other"})
	d.source(outsider, "foreign", "其他团队登记的客户邮件：回签合同")
	o := d.activeOrder("quote.1")
	v := d.getOrder(o.ObjectID)
	_, err := d.c.TRD.GenerateOrderDocument(d.author, d.generateCmd(o.ObjectID, 1, v.OrderHash, trdapi.DocumentSalesContract, d.plain, d.scSeq))
	d.must(err)
	d.scan("file.back", "countersigned contract")
	cmd := d.signCmd(o.ObjectID, 1, trdapi.DocumentSalesContract, trdapi.SignatureCountersigned, "file.back")
	cmd.Evidence = []trdapi.Evidence{{SourceID: "foreign", Locator: "附件"}}
	_, err = d.c.TRD.RecordSignedOrderDocument(d.author, cmd)
	d.must(err)
	other := cmd
	other.FileID = "file.other"
	before := d.business()
	team := d.regrant("editor", "team", allPermissions)
	_, err = d.c.TRD.RecordSignedOrderDocument(team, cmd)
	d.want(err, kernel.CodeReferenceInvalid)
	for _, c := range []trdapi.Command{cmd, other} {
		_, err = d.c.TRD.Reconcile(team, c)
		d.want(err, kernel.CodeReferenceInvalid)
	}
	if d.business() != before {
		t.Fatal("the refused replay wrote business state")
	}
	again := d.regrant("editor", "company", allPermissions)
	r, err := d.c.TRD.RecordSignedOrderDocument(again, cmd)
	if err != nil || !r.Replayed || r.SignedFileID != "file.back" {
		t.Fatalf("the replay with the source readable: %+v %v", r, err)
	}
	rec, err := d.c.TRD.Reconcile(again, cmd)
	if err != nil || rec.Receipt == nil || rec.Receipt.ObjectID != o.ObjectID {
		t.Fatalf("the reconciliation: %+v %v", rec, err)
	}
}
