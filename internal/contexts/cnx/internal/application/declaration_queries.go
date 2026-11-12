package application

import (
	"context"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/internal/adapters/postgres"
	dcl "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/declaration"
	db "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/documentbundle"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Declarations and bundles are company-wide (like licences, invoices and tax refund cases): every
// holder of cnx.read in the company reads them.

func declarationEvidenceViews(e []dcl.Evidence) []api.EvidenceView {
	out := []api.EvidenceView{}
	for _, x := range e {
		out = append(out, api.EvidenceView{SourceID: x.SourceID, ContentSHA256: x.ContentSHA256})
	}
	return out
}

func legalViews(l []dcl.DocLegal) []api.LegalQuantityView {
	out := []api.LegalQuantityView{}
	for _, q := range l {
		out = append(out, api.LegalQuantityView{Unit: q.Unit, Quantity: q.Quantity, PerBaseUnit: q.PerBaseUnit, Basis: q.Basis})
	}
	return out
}

func versionView(v *dcl.Version) api.DeclarationVersionView {
	d := v.Document
	out := api.DeclarationVersionView{Version: d.Version, Kind: d.Kind, DocumentSHA256: v.SHA256, ExportLotID: d.ExportLot.ExportLotID,
		LotVersion: d.ExportLot.Version, ShipmentID: d.ExportLot.ShipmentID, OrderID: d.Order.OrderID, OrderRevision: d.Order.OrderRevision,
		OrderHash: d.Order.OrderHash, OrderStatus: d.Order.Status, SalesDocuments: []api.SalesDocumentView{}, LotCurrency: d.ExportLot.Currency,
		LotValueMinor: d.ExportLot.ValueMinor, Mapping: d.Mapping, Items: []api.DeclarationItemView{}, Amendments: []api.AmendmentView{},
		ReceiptID: d.ReceiptID, PreparedBy: kernel.ActorRef{Kind: kernel.ActorKind(d.PreparedBy.Kind), ID: d.PreparedBy.ID},
		AuthorityEpoch: d.AuthorityEpoch, PreparedAt: timestampOf(d.PreparedAt)}
	for _, s := range d.Order.Documents {
		out.SalesDocuments = append(out.SalesDocuments, api.SalesDocumentView(s))
	}
	for _, it := range d.Items {
		iv := api.DeclarationItemView{ItemNo: it.ItemNo, CommodityCode: it.CommodityCode, DeclarationName: it.DeclarationName,
			Elements: []api.ElementView{}, BaseUnit: it.BaseUnit, BaseQuantity: it.BaseQuantity, LegalQuantities: legalViews(it.LegalQuantities),
			CurrencyCode: it.Currency, ValueMinor: it.ValueMinor, Mapping: it.Mapping, Lines: []api.DeclarationLineView{}}
		for _, e := range it.Elements {
			iv.Elements = append(iv.Elements, api.ElementView(e))
		}
		for _, l := range it.Lines {
			iv.Lines = append(iv.Lines, api.DeclarationLineView{LineID: l.LineID, ShipmentID: l.ShipmentID, OrderLineID: l.OrderLineID, SkuID: l.SkuID,
				SpecRevision: l.SpecRevision, Quantity: l.Quantity, Unit: l.Unit, AmountMinor: l.AmountMinor, ProfileID: l.Profile.ProfileID,
				ProfileRevision: l.Profile.Revision, ProfileHash: l.Profile.ContentHash, Conversion: api.ConversionView(l.Conversion),
				BaseQuantity: l.BaseQuantity, LegalQuantities: legalViews(l.LegalQuantities), Mapping: l.Mapping, Reason: l.Reason})
		}
		out.Items = append(out.Items, iv)
	}
	for _, a := range d.Amendments {
		out.Amendments = append(out.Amendments, api.AmendmentView(a))
	}
	return out
}

// GetDeclaration returns a declaration case with every version and entry (declaration_case.get,
// cnx.read).
func (s Service) GetDeclaration(ctx context.Context, id string) (api.DeclarationView, error) {
	var v api.DeclarationView
	if !kernel.ValidIdentifier(id) {
		return v, kernel.Reject(kernel.CodeSchemaInvalid, "/id", "")
	}
	err := s.Ledger.Read(ctx, api.CommandGetDeclaration, id, func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		sc := scope(ctx)
		c, err := postgres.LoadDeclaration(ctx, exec, sc, id, postgres.NoLock)
		if err != nil {
			return err
		}
		if c == nil {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v = api.DeclarationView{DeclarationID: c.ID, ExportLotID: c.ExportLotID, Attempt: c.Attempt, ShipmentID: c.ShipmentID, OrderID: c.OrderID,
			OrderRevision: c.OrderRevision, OrderHash: c.OrderHash, CodeTableVersion: c.CodeTableVersion, Status: c.Status, RowVersion: c.RowVersion,
			CurrentVersion: c.Version, Mapping: c.Mapping, DeclarationNumber: c.DeclarationNumber, CustomsReleased: dcl.Exported(c.Status),
			ExportedAt: c.ExportedAt, LastSequence: c.LastSequence, Versions: []api.DeclarationVersionView{},
			Confirmations: []api.ConfirmationEntryView{}, Submissions: []api.SubmissionView{}, Receipts: []api.ReceiptView{}, OpenedBy: c.OpenedBy,
			AuthorityEpoch: c.Epoch, OpenedAt: c.OpenedAt}
		versions, err := postgres.Versions(ctx, exec, sc, c.ID, 0)
		if err != nil {
			return err
		}
		for _, x := range versions {
			v.Versions = append(v.Versions, versionView(x))
		}
		confirmations, err := postgres.Confirmations(ctx, exec, sc, c.ID)
		if err != nil {
			return err
		}
		for _, x := range confirmations {
			v.Confirmations = append(v.Confirmations, api.ConfirmationEntryView{Version: x.Version, Evidence: declarationEvidenceViews(x.Evidence), Reason: x.Reason,
				ConfirmedBy: x.By, AuthorityEpoch: x.Epoch, ConfirmedAt: x.At})
		}
		submissions, err := postgres.Submissions(ctx, exec, sc, c.ID)
		if err != nil {
			return err
		}
		for _, x := range submissions {
			v.Submissions = append(v.Submissions, api.SubmissionView{SubmissionNo: x.No, Version: x.Version, OperationReference: x.OperationReference,
				Submitter: api.Submitter{Kind: x.SubmitterKind, PartyID: x.SubmitterPartyID, Name: x.SubmitterName}, SubmittedAt: x.SubmittedAt,
				Evidence: declarationEvidenceViews(x.Evidence), Note: x.Note, RecordedBy: x.By, AuthorityEpoch: x.Epoch, RecordedAt: x.At})
		}
		receipts, err := postgres.Receipts(ctx, exec, sc, c.ID)
		if err != nil {
			return err
		}
		for _, x := range receipts {
			v.Receipts = append(v.Receipts, api.ReceiptView{ReceiptID: x.ID, ReceiptNo: x.No, Outcome: x.Outcome, From: x.From, Sequence: x.Sequence,
				StatusCode: x.StatusCode, OriginalText: x.OriginalText, MappingVersion: x.MappingVersion, ReceivedAt: x.ReceivedAt,
				DeclarationNumber: x.DeclarationNumber, ExportedAt: x.ExportedAt, Version: x.Version, Evidence: declarationEvidenceViews(x.Evidence),
				Reason: x.Reason, RecordedBy: x.By, AuthorityEpoch: x.Epoch, RecordedAt: x.At})
		}
		return nil
	})
	return v, err
}

// Declarations returns the company's declaration cases, newest first (declaration_case.list,
// cnx.read).
func (s Service) Declarations(ctx context.Context) ([]api.DeclarationSummary, error) {
	out := []api.DeclarationSummary{}
	err := s.Ledger.Read(ctx, api.CommandDeclarations, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		cs, err := postgres.Declarations(ctx, exec, scope(ctx))
		for _, c := range cs {
			out = append(out, api.DeclarationSummary{DeclarationID: c.ID, ExportLotID: c.ExportLotID, Attempt: c.Attempt, Status: c.Status,
				RowVersion: c.RowVersion, CurrentVersion: c.Version, Mapping: c.Mapping, DeclarationNumber: c.DeclarationNumber,
				ExportedAt: c.ExportedAt, OpenedAt: c.OpenedAt})
		}
		return err
	})
	return out, err
}

func requirementViews(rs []db.Requirement) []api.RequirementView {
	out := []api.RequirementView{}
	for _, r := range rs {
		v := api.RequirementView{RequirementID: r.RequirementID, Kind: r.Kind, Version: r.Version, Status: r.Status,
			Owner: api.RequirementOwner(r.Owner), DueOn: r.DueOn, Evidence: []api.EvidenceView{}, FundRecords: []api.FundLink{}, Basis: r.Basis,
			Note: r.Note}
		for _, e := range r.Evidence {
			v.Evidence = append(v.Evidence, api.EvidenceView(e))
		}
		if d := r.Declaration; d != nil {
			v.Declaration = &api.DeclarationLink{DeclarationID: d.DeclarationID, Status: d.Status, DeclarationNumber: d.DeclarationNumber,
				ExportedAt: d.ExportedAt, CustomsReleased: d.Exported}
		}
		for _, f := range r.FundRecords {
			v.FundRecords = append(v.FundRecords, api.FundLink(f))
		}
		out = append(out, v)
	}
	return out
}

// GetBundle returns a document bundle with its checks and reconciliations (document_bundle.get,
// cnx.read).
func (s Service) GetBundle(ctx context.Context, id string) (api.BundleView, error) {
	var v api.BundleView
	if !kernel.ValidIdentifier(id) {
		return v, kernel.Reject(kernel.CodeSchemaInvalid, "/id", "")
	}
	err := s.Ledger.Read(ctx, api.CommandGetBundle, id, func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		sc := scope(ctx)
		b, err := postgres.LoadBundle(ctx, exec, sc, id, postgres.NoLock)
		if err != nil {
			return err
		}
		if b == nil {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v = api.BundleView{BundleID: b.ID, ExportLotID: b.ExportLotID, OrderID: b.OrderID, Status: b.Status, RowVersion: b.RowVersion,
			Requirements: []api.RequirementView{}, Checks: []api.BundleCheckView{}, Reconciliations: []api.ReconciliationView{}, OpenedBy: b.OpenedBy,
			OpenedAt: b.OpenedAt}
		checks, err := postgres.BundleChecks(ctx, exec, sc, b.ID)
		if err != nil {
			return err
		}
		for _, c := range checks {
			v.Checks = append(v.Checks, api.BundleCheckView{CheckNo: c.CheckNo, DocumentSHA256: c.SHA256, Status: c.Status, Missing: c.Missing,
				Present: c.Present, Verified: c.Verified, Requirements: requirementViews(c.Requirements),
				CheckedBy: kernel.ActorRef{Kind: kernel.ActorKind(c.CheckedBy.Kind), ID: c.CheckedBy.ID}, AuthorityEpoch: c.AuthorityEpoch,
				CheckedAt: timestampOf(c.CheckedAt)})
		}
		if n := len(v.Checks); n > 0 {
			v.Requirements = v.Checks[n-1].Requirements
		}
		rs, err := postgres.Reconciliations(ctx, exec, sc, b.ID)
		if err != nil {
			return err
		}
		for _, r := range rs {
			ev := []api.EvidenceView{}
			for _, e := range r.Evidence {
				ev = append(ev, api.EvidenceView(e))
			}
			v.Reconciliations = append(v.Reconciliations, api.ReconciliationView{ReconciliationID: r.ID, ReconciliationNo: r.No,
				Subject: api.ReconciliationSubject{Kind: r.SubjectKind, ID: r.SubjectID}, Measure: r.Measure, Unit: r.Unit,
				ExternalValue: r.External.String(), ExternalSource: r.ExternalSource, ExternalEvidence: ev, InternalValue: r.Internal.String(),
				InternalSource: r.InternalSource, Difference: r.Difference.String(),
				Responsibility: api.Responsibility{Kind: r.ResponsibilityKind, PartyID: r.ResponsibilityPartyID, Note: r.ResponsibilityNote},
				Adjustment:     api.Adjustment{Kind: r.Adjustment, Note: r.AdjustmentNote}, Reason: r.Reason, ResolvedBy: r.By, AuthorityEpoch: r.Epoch,
				ResolvedAt: r.At})
		}
		return nil
	})
	return v, err
}

// Bundles returns the company's document bundles, newest first (document_bundle.list, cnx.read).
func (s Service) Bundles(ctx context.Context) ([]api.BundleSummary, error) {
	out := []api.BundleSummary{}
	err := s.Ledger.Read(ctx, api.CommandBundles, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		bs, err := postgres.Bundles(ctx, exec, scope(ctx))
		for _, b := range bs {
			out = append(out, api.BundleSummary{BundleID: b.ID, ExportLotID: b.ExportLotID, Status: b.Status, RowVersion: b.RowVersion, Checks: b.Checks,
				Reconciliations: b.Reconciliations, OpenedAt: b.OpenedAt})
		}
		return err
	})
	return out, err
}
