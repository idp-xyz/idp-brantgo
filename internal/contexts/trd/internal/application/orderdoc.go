package application

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/order"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/orderdoc"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/orderrev"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The S/C and PI of a sales order (WBS S3-15, TRD-INV-020; api/orderdoc.go): order.document.generate
// (human or AI) and order.document.record_signed (human), and the audited read order.document.get.
//
// Generation follows quote formalization (formalize.go). Before TRD's transaction — after the ledger
// authorized the command and found no stored result for its key — Prepare:
//
//  1. reads the order and its quote in a TRD transaction of its own and runs the checks the command
//     transaction will run (visibility, the quote root's object control, Order.CheckDocument), so a
//     refusal costs no number;
//  2. reads what the document prints that the order revision names by reference only
//     (orderDocumentFacts: PTY's customer and contact names, CAT's item names, POL's currency and
//     trade terms at the revisions the order binds) and the POL template and number sequence the
//     command names, which must be in force;
//  3. prepares the document (orderdoc.Prepare: everything but the number, the size included), with
//     the legal terms the order revision carries from its source quote (never POL's current ones);
//  4. has POL issue the number for the order (number.issue as the acting actor, NumberIssue v1,
//     Context Map R15; business reference the order ID, so a retry, and every later revision's
//     document of the kind, gets the same number back).
//
// In the transaction: the quote root FOR UPDATE, the order FOR UPDATE (orderCommand), the authority
// guard row FOR SHARE (the ledger's Authorize); visibility, the object control and CheckDocument on
// the locked order (ORDER_DOCUMENT_STALE when an amendment moved it since); POL's ConfigGuard of the
// template and the sequence at the content hashes read before (REFERENCE_STALE when they moved);
// the document rendered with the number (Plan.Generate); the binding, bytes and commit evidence
// (Order.AddDocument), then the ledger's action result, audit entry and OrderDocumentGenerated. The
// order's row version does not move.

// CommandGetOrderDocument is the audited read of an order document and its bytes (trd.read).
const CommandGetOrderDocument = "order.document.get"

// documentInputs is what a generation read before its transaction.
type documentInputs struct {
	plan   *orderdoc.Plan
	number string
	guards amendmentInputs // the template and the sequence, for ConfigGuard in the transaction
}

// GenerateOrderDocument renders and binds an S/C or PI of the order's current revision
// (order.document.generate).
func (s Service) GenerateOrderDocument(ctx context.Context, cmd api.GenerateOrderDocument) (api.OrderDocumentReceipt, error) {
	h := &orderCommand{s: s, orderID: cmd.OrderID}
	var in documentInputs
	h.prepare = func(ctx context.Context) error {
		in = documentInputs{}
		var r *orderrev.Revision
		err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
			x := &orderCommand{s: s, orderID: cmd.OrderID}
			var err error
			if x.o, err = postgres.LoadOrder(ctx, exec, scope(ctx), cmd.OrderID, postgres.NoLock); err != nil {
				return err
			}
			if x.o == nil {
				return kernel.Reject(kernel.CodeNotFound, "/args/order_id", "")
			}
			if x.q, err = postgres.LoadQuote(ctx, exec, scope(ctx), x.o.Source.QuoteID, postgres.NoLock); err != nil {
				return err
			}
			if err = x.admit(ctx, d, "/args/order_id"); err != nil {
				return err
			}
			if err = x.o.CheckDocument(cmd.ExpectedOrderRevision, cmd.ExpectedOrderHash, cmd.DocumentKind); err != nil {
				return err
			}
			r, err = x.o.Revision()
			return err
		})
		if err != nil {
			return err
		}
		now := s.Clock.Now()
		template, facts, err := s.orderDocumentFacts(ctx, r, cmd, &in.guards, now)
		if err != nil {
			return err
		}
		if in.plan, err = orderdoc.Prepare(r, template, facts, now); err != nil {
			return moved(err, "/facts/sequence", "/args/sequence_ref") // a sequence of another document type
		}
		seq := in.plan.Number.Sequence
		issued, err := s.Upstream.Numbers.IssueNumber(ctx, polapi.IssueNumber{IdempotencyKey: numberKey(api.CommandGenerateOrderDocument,
			cmd.IdempotencyKey), Sequence: polRef(seq), BusinessRef: in.plan.Number.BusinessRef})
		if err != nil {
			return moved(err, "/args/sequence", "/args/sequence_ref")
		}
		in.number = issued.ObjectID
		return nil
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := h.admit(ctx, d, "/args/order_id"); err != nil {
			return action.Outcome{}, err
		}
		if err := h.o.CheckDocument(cmd.ExpectedOrderRevision, cmd.ExpectedOrderHash, cmd.DocumentKind); err != nil {
			return action.Outcome{}, err
		}
		p := in.plan
		if p == nil || p.OrderRevision != h.o.OrderRevision || p.OrderHash != h.o.OrderHash || p.DocumentKind != cmd.DocumentKind {
			return action.Outcome{}, errors.New("trd: the document was not prepared for this order revision")
		}
		now := s.Clock.Now()
		upstream, err := guardAdopted(ctx, exec, in.guards, now)
		if err != nil {
			return action.Outcome{}, err
		}
		upstream = append([]quote.Entry{{Name: "upstream_checked_at", Value: kernel.FormatTimestamp(now)}}, upstream...)
		g, err := p.Generate(in.number)
		if err != nil {
			return action.Outcome{}, err
		}
		doc, err := h.o.AddDocument(order.Generation{Generated: g, IdempotencyKey: cmd.IdempotencyKey, ControlEpoch: h.q.Control.Epoch,
			Upstream: upstream}, d.Actor, d.Epoch, now)
		if err != nil {
			return action.Outcome{}, err
		}
		b := doc.Binding
		return h.save(ctx, exec, api.StatusGenerated, h.event(api.EventOrderDocumentGenerated, map[string]any{"document_kind": b.DocumentKind,
			"document_order_revision": b.OrderRevision, "document_order_hash": b.OrderHash, "number": b.Number,
			"sequence_ref": b.Sequence.String(), "template_ref": b.Template.String(), "template_content_hash": b.TemplateHash,
			"renderer_version": b.RendererVersion, "generated_at": b.GeneratedAt, "file_name": b.FileName, "mime_type": b.MimeType,
			"file_sha256": b.ContentSHA256, "file_size": b.SizeBytes, "generated_by": doc.By.ID}))
	}
	return s.runDocument(ctx, api.CommandGenerateOrderDocument, cmd.IdempotencyKey, cmd.OrderID, cmd, cmd.Validate(), h,
		cmd.ExpectedOrderRevision, cmd.DocumentKind, "")
}

// orderDocumentFacts reads, before the transaction, what the kind document of order revision r
// prints that r names by reference only — from PTY the customer's name at the bound party revision
// and the contact's at the bound row version (audited reads, pty.read), from CAT each SKU's name at
// the bound specification revision (audited, cat.read), from POL the currency and the trade terms at
// the revisions r binds, whatever their state now — and the template and number sequence cmd names,
// which must be in force at instant at; those two go into guards for ConfigGuard in the transaction.
// Unknown or hidden records are REFERENCE_INVALID, a contact moved on REFERENCE_STALE, at the order's
// path; the template's and sequence's refusals at the command's.
func (s Service) orderDocumentFacts(ctx context.Context, r *orderrev.Revision, cmd api.GenerateOrderDocument, guards *amendmentInputs,
	at time.Time) (orderdoc.Template, orderdoc.Facts, error) {
	var t orderdoc.Template
	var f orderdoc.Facts
	party, err := s.Upstream.Parties.GetParty(ctx, r.Customer.PartyID)
	if err != nil {
		return t, f, factErr(err, "/order/customer_ref")
	}
	for _, x := range party.Revisions {
		if x.Revision == r.Customer.Revision {
			f.Customer = orderdoc.Party{Ref: r.Customer, Name: x.Name}
		}
	}
	if f.Customer.Name == "" {
		return t, f, kernel.Reject(kernel.CodeReferenceInvalid, "/order/customer_ref", "The customer revision is unknown")
	}
	contact, err := s.Upstream.Parties.GetContact(ctx, r.Contact.ContactID)
	if err != nil {
		return t, f, factErr(err, "/order/contact_ref")
	}
	if contact.RowVersion != r.Contact.RowVersion || contact.PartyID != r.Customer.PartyID {
		return t, f, api.Reject(api.CodeReferenceStale, "/order/contact_ref")
	}
	f.Contact = orderdoc.Contact{Ref: r.Contact, Name: contact.Name}
	f.Items = map[quote.SkuRef]orderdoc.Item{}
	for i, l := range r.Lines {
		if _, done := f.Items[l.Sku]; done {
			continue
		}
		path := fmt.Sprintf("/order/lines/%d/sku_ref", i)
		sku, err := s.Upstream.Skus.GetSku(ctx, l.Sku.SkuID)
		if err != nil {
			return t, f, factErr(err, path)
		}
		j := slices.IndexFunc(sku.Revisions, func(x catapi.SpecRevision) bool { return x.Revision == l.Sku.SpecRevision })
		if j < 0 {
			return t, f, kernel.Reject(kernel.CodeReferenceInvalid, path, "The specification revision is unknown")
		}
		f.Items[l.Sku] = orderdoc.Item{Name: sku.Revisions[j].Name, Specification: fmt.Sprintf("%s r%d", l.Sku.SkuID, l.Sku.SpecRevision)}
	}
	template := quote.Ref{Kind: cmd.TemplateRef.Kind, ID: cmd.TemplateRef.ID, Revision: cmd.TemplateRef.Revision}
	sequence := quote.Ref{Kind: cmd.SequenceRef.Kind, ID: cmd.SequenceRef.ID, Revision: cmd.SequenceRef.Revision}
	got, err := s.resolve(ctx, map[polapi.ConfigRef]string{polRef(template): "/args/template_ref", polRef(sequence): "/args/sequence_ref",
		polRef(r.Currency): "", polRef(r.Terms.TradeTerms): ""}, at, guards)
	if err != nil {
		return t, f, err
	}
	rc := got[polRef(template)]
	ot := rc.Data.OrderDocumentTemplate
	switch {
	case ot == nil:
		return t, f, kernel.Reject(kernel.CodeReferenceInvalid, "/args/template_ref", "Not an order document template")
	case ot.DocumentKind != cmd.DocumentKind:
		return t, f, kernel.Reject(kernel.CodeReferenceInvalid, "/args/template_ref", "The template lays out a "+ot.DocumentKind)
	}
	t = orderdoc.Template{Ref: template, ContentHash: rc.ContentHash, DocumentKind: ot.DocumentKind, Language: ot.Language,
		RendererVersion: ot.RendererVersion, Title: ot.Title, Sections: slices.Clone(ot.Sections), Clauses: slices.Clone(ot.Clauses),
		FooterNote: ot.FooterNote}
	ns := got[polRef(sequence)].Data.NumberSequence
	if ns == nil {
		return t, f, kernel.Reject(kernel.CodeReferenceInvalid, "/args/sequence_ref", "Not a number sequence")
	}
	f.Sequence = orderdoc.Sequence{Ref: sequence, DocumentType: ns.DocumentType}
	c := got[polRef(r.Currency)].Data.Currency
	if c == nil {
		return t, f, kernel.Reject(kernel.CodeReferenceInvalid, "/order/currency_ref", "")
	}
	f.Currency = orderdoc.Currency{Ref: r.Currency, MinorUnits: c.MinorUnits}
	tt := got[polRef(r.Terms.TradeTerms)].Data.TradeTerms
	if tt == nil {
		return t, f, kernel.Reject(kernel.CodeReferenceInvalid, "/order/terms/trade_terms_ref", "")
	}
	f.TradeTerms = orderdoc.TradeTerms{Ref: r.Terms.TradeTerms, Incoterm: tt.Incoterm, Edition: tt.Edition, ResponsibilityNote: tt.ResponsibilityNote,
		NamedPlace: tt.NamedPlace}
	return t, f, nil
}

// RecordSignedOrderDocument records a signed or countersigned copy of an order document
// (order.document.record_signed). Before the transaction it reads which cited sources the actor
// reads and the EVD business file (its SHA-256 and MIME type; NOT_FOUND there is REFERENCE_INVALID
// at /args/file_id); in it, under the order's lock, Order.RecordSignature decides.
func (s Service) RecordSignedOrderDocument(ctx context.Context, cmd api.RecordSignedOrderDocument) (api.OrderDocumentReceipt, error) {
	h := &orderCommand{s: s, orderID: cmd.OrderID}
	proofs := evidence(cmd.Evidence)
	var readable map[string]bool
	var file evdapi.FileView
	h.prepare = func(ctx context.Context) error {
		var err error
		if readable, err = s.readEvidence(ctx, evidenceIDs(proofs)); err != nil {
			return err
		}
		if file, err = s.Upstream.Sources.GetFile(ctx, cmd.FileID); err != nil {
			return factErr(err, "/args/file_id")
		}
		return nil
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := h.admit(ctx, d, "/args/order_id"); err != nil {
			return action.Outcome{}, err
		}
		if file.FileID != cmd.FileID {
			return action.Outcome{}, errors.New("trd: the signed copy's file was not read")
		}
		sig, replayed, err := h.o.RecordSignature(cmd.OrderRevision, cmd.DocumentKind, order.Signature{Signature: orderdoc.Signature{
			Kind: cmd.SignatureKind, FileID: cmd.FileID, FileSHA256: file.ContentSHA256, Recorded: quote.Act{By: d.Actor, Epoch: d.Epoch,
				At: s.Clock.Now(), Evidence: proofs}}, MimeType: file.MimeType, IdempotencyKey: cmd.IdempotencyKey})
		if err != nil {
			return action.Outcome{}, err
		}
		if err = checkCited(proofs, readable, "/args/evidence"); err != nil {
			return action.Outcome{}, err
		}
		if replayed {
			return h.save(ctx, exec, cmd.SignatureKind) // recorded before under another key: nothing to write
		}
		b := h.o.Document(cmd.OrderRevision, cmd.DocumentKind).Binding
		return h.save(ctx, exec, cmd.SignatureKind, h.event(api.EventOrderDocumentSigned, map[string]any{"document_kind": b.DocumentKind,
			"document_order_revision": b.OrderRevision, "document_order_hash": b.OrderHash, "number": b.Number, "document_sha256": b.ContentSHA256,
			"signature_kind": sig.Kind, "file_id": sig.FileID, "file_sha256": sig.FileSHA256, "file_mime_type": sig.MimeType,
			"recorded_by": sig.Recorded.By.ID, "recorded_at": kernel.FormatTimestamp(sig.Recorded.At)}))
	}
	return s.runDocument(ctx, api.CommandRecordSignedOrderDocument, cmd.IdempotencyKey, cmd.OrderID, cmd, cmd.Validate(), h, cmd.OrderRevision,
		cmd.DocumentKind, cmd.SignatureKind)
}

// runDocument runs a document command and completes its receipt from the stored order: the kind
// document of order revision rev and, for a signed copy, the copy of kind signature.
func (s Service) runDocument(ctx context.Context, name, key, orderID string, req api.Command, validation error, h *orderCommand, rev int64,
	kind, signature string) (api.OrderDocumentReceipt, error) {
	res, err := s.run(ctx, action.Command{Name: name, IdempotencyKey: key, ObjectID: orderID}, req, validation, h)
	if err != nil {
		return api.OrderDocumentReceipt{}, err
	}
	out := api.OrderDocumentReceipt{Receipt: res}
	err = s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		o, err := postgres.LoadOrder(ctx, exec, scope(ctx), res.ObjectID, postgres.NoLock)
		if err != nil || o == nil {
			return err
		}
		d := o.Document(rev, kind)
		if d == nil {
			return nil
		}
		b := d.Binding
		out.OrderRevision, out.OrderHash, out.DocumentKind, out.Number = b.OrderRevision, b.OrderHash, b.DocumentKind, b.Number
		out.FileName, out.MimeType, out.DocumentSHA256, out.SizeBytes = b.FileName, b.MimeType, b.ContentSHA256, b.SizeBytes
		for _, x := range d.Signatures {
			if x.Kind == signature {
				out.SignatureKind, out.SignedFileID, out.SignedSHA256 = x.Kind, x.FileID, x.FileSHA256
			}
		}
		return nil
	})
	return out, err
}

// documentViews are the order's documents as the reads show them.
func documentViews(o *order.Order) []api.OrderDocumentView {
	var out []api.OrderDocumentView
	for _, d := range o.Documents {
		out = append(out, documentView(o, d))
	}
	return out
}

func documentView(o *order.Order, d *order.Document) api.OrderDocumentView {
	b := d.Binding
	entries := map[string]string{}
	for _, e := range d.Entries {
		entries[e.Name] = e.Value
	}
	doc, _ := d.EntriesJSON()
	v := api.OrderDocumentView{OrderRevision: b.OrderRevision, OrderHash: b.OrderHash, DocumentKind: b.DocumentKind, Number: b.Number,
		SequenceRef: apiRef(b.Sequence), BusinessRef: b.BusinessRef, TemplateRef: apiRef(b.Template), TemplateHash: b.TemplateHash,
		RendererVersion: b.RendererVersion, GeneratedAt: b.GeneratedAt, FileName: b.FileName, MimeType: b.MimeType, DocumentSHA256: b.ContentSHA256,
		SizeBytes: b.SizeBytes, IdempotencyKey: d.IdempotencyKey, AuthorityEpoch: d.Epoch, ControlEpoch: d.ControlEpoch, GeneratedBy: d.By,
		CommittedAt: d.At, Entries: entries, EntriesSHA256: quote.Hash(doc),
		Current: b.OrderRevision == o.OrderRevision && b.OrderHash == o.OrderHash}
	for _, x := range d.Signatures {
		v.Signatures = append(v.Signatures, api.OrderSignatureView{SignatureKind: x.Kind, FileID: x.FileID, FileSHA256: x.FileSHA256,
			MimeType: x.MimeType, RecordedBy: x.Recorded.By, RecordedAt: x.Recorded.At, Evidence: apiEvidence(x.Recorded.Evidence)})
	}
	return v
}

// GetOrderDocument returns an order document with its bytes, audited as order.document.get
// (trd.read): NOT_FOUND for an order of a quote the caller does not see or no such document;
// orderdoc.Verify's DOCUMENT_INTEGRITY_FAILURE for bytes that are not the bound ones.
func (s Service) GetOrderDocument(ctx context.Context, orderID string, rev int64, kind string) (api.OrderDocument, error) {
	var v api.OrderDocument
	err := s.Ledger.Read(ctx, CommandGetOrderDocument, orderID, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		o, err := postgres.LoadOrder(ctx, exec, scope(ctx), orderID, postgres.NoLock)
		if err != nil {
			return err
		}
		if o == nil {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		root, err := postgres.LoadRoot(ctx, exec, scope(ctx), o.Source.QuoteID)
		if err != nil {
			return err
		}
		if root == nil || !d.Sees(root.OwnerActor, root.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		doc := o.Document(rev, kind)
		if doc == nil {
			return kernel.Reject(kernel.CodeNotFound, "/order_revision", "")
		}
		content, _, err := postgres.LoadOrderDocumentContent(ctx, exec, scope(ctx), orderID, rev, kind)
		if err != nil {
			return err
		}
		if err = orderdoc.Verify(&doc.Binding, content); err != nil {
			return err
		}
		v = api.OrderDocument{OrderID: o.ID, Document: documentView(o, doc), Content: content}
		return nil
	})
	return v, err
}
