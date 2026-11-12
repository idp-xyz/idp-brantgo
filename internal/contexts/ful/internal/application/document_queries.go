package application

import (
	"context"
	"slices"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/adapters/postgres"
	docs "go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipmentdocumentset"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

func entries(e []docs.Entry) []api.Entry {
	out := make([]api.Entry, 0, len(e))
	for _, x := range e {
		out = append(out, api.Entry(x))
	}
	return out
}

func differenceView(d docs.Difference) api.DifferenceView {
	return api.DifferenceView{Path: d.Path, Document: d.Document, Version: d.Version, Item: d.Item, Found: d.Found, Expected: d.Expected,
		Against: d.Against}
}

func versionView(d *docs.Document) api.DocumentVersionView {
	v := api.DocumentVersionView{DocumentKind: d.Kind, Version: d.Version, Status: d.Status, Number: d.Number, SetVersion: d.SetVersion,
		SequenceHash: d.SequenceHash, SellerHash: d.SellerHash, RendererVersion: d.RendererVersion, ContentSHA256: d.ContentSHA256,
		Sources: entries(d.Sources), Renditions: []api.RenditionView{}, EvidenceIDs: slices.Clone(d.Evidence), PreparedBy: d.Prepared.By,
		PreparedAt: d.Prepared.At, AuthorityEpoch: d.Prepared.Epoch, Entries: entries(d.Entries), EntriesSHA256: d.EntriesSHA256,
		IssueSeq: d.IssueSeq, IssuedAt: d.IssuedAt, SupersededAt: d.SupersededAt, SignedCopies: []api.SignedCopyView{}}
	if d.Sequence != nil {
		r := apiRef(*d.Sequence)
		v.Sequence = &r
	}
	if d.Seller != nil {
		r := apiRef(*d.Seller)
		v.Seller = &r
	}
	if f := d.File; f != nil {
		v.FileID, v.FileSHA256, v.FileMimeType = f.ID, f.SHA256, f.MimeType
	}
	for _, r := range d.Renditions {
		v.Renditions = append(v.Renditions, api.RenditionView{Language: r.Language, Template: apiRef(r.Template), TemplateHash: r.TemplateHash,
			FileName: r.FileName, MimeType: r.MimeType, SHA256: r.SHA256, SizeBytes: r.Size})
	}
	for _, x := range d.Signatures {
		v.SignedCopies = append(v.SignedCopies, api.SignedCopyView{Language: x.Language, FileID: x.FileID, FileSHA256: x.FileSHA256, MimeType: x.MimeType,
			DocumentSHA256: x.DocumentSHA256, EvidenceIDs: slices.Clone(x.Evidence), RecordedBy: x.Recorded.By, RecordedAt: x.Recorded.At,
			AuthorityEpoch: x.Recorded.Epoch})
	}
	return v
}

// reconciliationView is the reconciliation of set's current versions on facts f, judged at now.
func reconciliationView(set *docs.Set, f docs.Facts, now time.Time) api.ReconciliationView {
	diffs := docs.Reconcile(set, f)
	_, sum := docs.Seal(diffs)
	v := api.ReconciliationView{State: api.Reconciled, Differences: []api.DifferenceView{}, SHA256: sum, CheckedAt: now}
	for _, d := range diffs {
		v.Differences = append(v.Differences, differenceView(d))
	}
	if len(diffs) > 0 {
		v.State = api.Unreconciled
	}
	return v
}

func setView(set *docs.Set, f docs.Facts, now time.Time) api.DocumentSetView {
	v := api.DocumentSetView{DocumentSetID: set.ID, ShipmentID: set.ShipmentID, Order: api.OrderBinding{OrderID: set.Order.OrderID,
		OrderRevision: set.Order.OrderRevision, OrderHash: set.Order.OrderHash}, RowVersion: set.RowVersion, Documents: []api.DocumentVersionView{},
		Issues: []api.IssueView{}, Reconciliation: reconciliationView(set, f, now), CreatedBy: set.Created.By, CreatedAt: set.Created.At,
		AuthorityEpoch: set.Created.Epoch}
	for _, d := range set.Documents {
		v.Documents = append(v.Documents, versionView(d))
	}
	for _, i := range set.Issues {
		iv := api.IssueView{Seq: i.Seq, SetVersion: i.SetVersion, Issued: []api.DocumentRef{}, Acknowledgements: []api.AcknowledgementView{},
			ReconciliationSHA256: i.ReconciliationSHA256, IssuedBy: i.Issued.By, IssuedAt: i.Issued.At, AuthorityEpoch: i.Issued.Epoch,
			Entries: entries(i.Entries), EntriesSHA256: i.EntriesSHA256}
		for _, x := range i.Documents {
			iv.Issued = append(iv.Issued, api.DocumentRef{DocumentKind: x.Kind, Version: x.Version})
		}
		for _, a := range i.Acknowledged {
			iv.Acknowledgements = append(iv.Acknowledgements, api.AcknowledgementView{Difference: differenceView(a.Difference), Reason: a.Reason,
				FromIssue: a.FromIssue})
		}
		v.Issues = append(v.Issues, iv)
	}
	return v
}

// GetDocumentSet returns a set the caller sees with its reconciliation now, audited as
// shipment_document_set.get (ful.read): NOT_FOUND otherwise.
func (s Service) GetDocumentSet(ctx context.Context, id string) (api.DocumentSetView, error) {
	if !kernel.ValidIdentifier(id) {
		return api.DocumentSetView{}, invalidID(s, ctx, api.CommandGetDocumentSet)
	}
	var v api.DocumentSetView
	err := s.Ledger.Read(ctx, api.CommandGetDocumentSet, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		set, err := postgres.LoadSet(ctx, exec, scope(ctx), id, postgres.NoLock)
		if err != nil {
			return err
		}
		if set == nil || !d.Sees(set.OwnerActor, set.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		var f docs.Facts
		if f.Shipment, err = postgres.LoadShipment(ctx, exec, scope(ctx), set.ShipmentID, postgres.NoLock); err != nil {
			return err
		}
		if f.Prices, err = postgres.LoadPrices(ctx, exec, scope(ctx), set.ShipmentID); err != nil {
			return err
		}
		if f.Booking, err = postgres.CurrentBooking(ctx, exec, scope(ctx), set.ShipmentID); err != nil {
			return err
		}
		v = setView(set, f, kernel.Instant(s.Clock.Now()))
		return nil
	})
	return v, err
}

// GetShipmentDocument returns the rendition of one language of a version with its bytes, audited as
// shipment_document.get (ful.read): NOT_FOUND for a set the caller does not see or no such rendition;
// DOCUMENT_INTEGRITY_FAILURE for bytes that are not the bound ones.
func (s Service) GetShipmentDocument(ctx context.Context, id, kind string, version int64, language string) (api.DocumentFile, error) {
	if !kernel.ValidIdentifier(id) || !api.DocumentKind(kind) || version < 1 || !api.Language(language) {
		return api.DocumentFile{}, invalidID(s, ctx, api.CommandGetShipmentDocument)
	}
	var v api.DocumentFile
	err := s.Ledger.Read(ctx, api.CommandGetShipmentDocument, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		set, err := postgres.LoadSet(ctx, exec, scope(ctx), id, postgres.NoLock)
		if err != nil {
			return err
		}
		if set == nil || !d.Sees(set.OwnerActor, set.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		doc := set.Document(kind, version)
		if doc == nil || doc.Rendition(language) == nil {
			return kernel.Reject(kernel.CodeNotFound, "/version", "")
		}
		r := doc.Rendition(language)
		content, err := postgres.LoadRenditionContent(ctx, exec, scope(ctx), id, kind, version, language)
		if err != nil {
			return err
		}
		if len(content) == 0 || len(content) != r.Size || docs.Hash(content) != r.SHA256 {
			return docs.Reject(docs.CodeDocumentIntegrityFailure, "/document", "The stored bytes are not the bound document")
		}
		v = api.DocumentFile{DocumentSetID: set.ID, Document: versionView(doc), Language: language, Content: content}
		return nil
	})
	return v, err
}
