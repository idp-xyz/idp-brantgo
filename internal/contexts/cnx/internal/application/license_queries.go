package application

import (
	"context"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/internal/adapters/postgres"
	el "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/exportlicense"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Licences are company-wide (no data scope, like CAT's declaration profiles): every holder of
// cnx.read in the company reads them.

func usageView(l *el.License, allocations []*el.Allocation) (api.Usage, error) {
	u, err := el.UsageOf(l, allocations)
	if err != nil {
		return api.Usage{}, err
	}
	return api.Usage{Unit: string(l.Unit), LicensedQuantity: l.Quantity.String(), AllocatedQuantity: u.Allocated.String(),
		AvailableQuantity: u.Available.String(), Currency: l.Currency.Code, LicensedValueMinor: l.Value.MinorString(),
		AllocatedValueMinor: u.AllocatedValue.MinorString(), AvailableValueMinor: u.AvailableValue.MinorString()}, nil
}

// GetLicense returns a licence with its usage ledger (export_license.get, cnx.read).
func (s Service) GetLicense(ctx context.Context, licenseID string) (api.LicenseView, error) {
	var v api.LicenseView
	if !kernel.ValidIdentifier(licenseID) {
		return v, kernel.Reject(kernel.CodeSchemaInvalid, "/id", "")
	}
	err := s.Ledger.Read(ctx, api.CommandGetLicense, licenseID, func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		sc := scope(ctx)
		l, err := postgres.LoadLicense(ctx, exec, sc, licenseID, postgres.NoLock)
		if err != nil {
			return err
		}
		if l == nil {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		allocations, err := postgres.Allocations(ctx, exec, sc, l, "")
		if err != nil {
			return err
		}
		releases, err := postgres.Releases(ctx, exec, sc, l)
		if err != nil {
			return err
		}
		usage, err := usageView(l, allocations)
		if err != nil {
			return err
		}
		now := kernel.Instant(s.Clock.Now())
		v = api.LicenseView{LicenseID: l.ID, LicenseNumber: l.Number, Issuer: l.Issuer, Holder: l.Holder, Goods: goodsView(l.Goods),
			Unit: string(l.Unit), LicensedQuantity: l.Quantity.String(), Currency: api.LicenseCurrencyView{ID: l.Currency.ConfigID,
				Revision: l.Currency.Revision, Code: l.Currency.Code, MinorUnits: l.Currency.MinorUnits}, LicensedValue: l.Value.Amount(),
			Destination: l.Destination, EndUser: api.EndUser{PartyID: l.EndUserPartyID, Name: l.EndUserName}, ValidFrom: l.Validity.From,
			ValidUntil: l.Validity.Until, File: api.LicenseFileView{FileID: l.FileID, ContentSHA256: l.FileSHA256}, Status: el.Registered,
			State: l.State(now), RowVersion: l.RowVersion, Usage: usage, Allocations: []api.AllocationView{}, RegisteredBy: l.RegisteredBy,
			RegisteredAt: l.RegisteredAt, AuthorityEpoch: l.Epoch, CheckedAt: now}
		for _, a := range allocations {
			v.Allocations = append(v.Allocations, allocationView(a, releases))
		}
		return nil
	})
	return v, err
}

func goodsView(g el.Goods) api.LicensedGoods {
	var out api.LicensedGoods
	for _, s := range g.Specs {
		out.SkuSpecs = append(out.SkuSpecs, api.SpecRef{SkuID: s.SkuID, SpecRevision: s.SpecRevision})
	}
	if g.ByClassification() {
		out.Classification = &api.Classification{CodeTableVersion: g.CodeTable, CommodityCodes: g.CommodityCodes}
	}
	return out
}

func allocationView(a *el.Allocation, releases []*el.Release) api.AllocationView {
	q, val, _ := a.Outstanding()
	t := a.Target
	v := api.AllocationView{AllocationID: a.ID, TargetKind: t.Kind, TargetID: t.ID, TargetVersion: t.Version, Attempt: a.Attempt,
		OrderID: t.OrderID, OrderRevision: t.OrderRevision, Destination: t.Destination, EndUserPartyID: t.EndUserPartyID,
		Lines: []api.AllocationLineView{}, LinesHash: a.LinesHash, Quantity: a.Quantity.String(), ValueMinor: a.Value.MinorString(),
		OutstandingQuantity: q.String(), OutstandingValueMinor: val.MinorString(), Status: a.Status(), AllocatedBy: a.By,
		AllocatedAt: a.At, AuthorityEpoch: a.Epoch, Releases: []api.ReleaseView{}}
	for _, l := range t.Lines {
		v.Lines = append(v.Lines, api.AllocationLineView{LineID: l.LineID, SkuID: l.SkuID, SpecRevision: l.SpecRevision, Quantity: l.Quantity.String()})
	}
	for _, r := range releases {
		if r.AllocationID != a.ID {
			continue
		}
		rv := api.ReleaseView{ReleaseID: r.ID, Kind: r.Kind, Source: r.Source, ReceiptID: r.ReceiptID, DeclarationNumber: r.DeclarationNumber, ReceiptText: r.ReceiptText,
			ReceivedAt: r.ReceivedAt, Evidence: []api.EvidenceView{}, Quantity: r.Quantity.String(), ValueMinor: r.Value.MinorString(),
			Reason: r.Reason, ReleasedBy: r.By, ReleasedAt: r.At, AuthorityEpoch: r.Epoch}
		for _, e := range r.Evidence {
			rv.Evidence = append(rv.Evidence, api.EvidenceView{SourceID: e.SourceID, ContentSHA256: e.ContentSHA256})
		}
		v.Releases = append(v.Releases, rv)
	}
	return v
}

// Licenses returns the company's licences, newest first, with their usage (export_license.list,
// cnx.read).
func (s Service) Licenses(ctx context.Context) ([]api.LicenseSummary, error) {
	out := []api.LicenseSummary{}
	err := s.Ledger.Read(ctx, api.CommandLicenses, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		sc := scope(ctx)
		ls, err := postgres.Licenses(ctx, exec, sc)
		if err != nil {
			return err
		}
		now := kernel.Instant(s.Clock.Now())
		for _, l := range ls {
			allocations, err := postgres.Allocations(ctx, exec, sc, l, "")
			if err != nil {
				return err
			}
			usage, err := usageView(l, allocations)
			if err != nil {
				return err
			}
			out = append(out, api.LicenseSummary{LicenseID: l.ID, LicenseNumber: l.Number, Issuer: l.Issuer, Destination: l.Destination,
				EndUserID: l.EndUserPartyID, ValidFrom: l.Validity.From, ValidUntil: l.Validity.Until, State: l.State(now), RowVersion: l.RowVersion,
				Usage: usage, CheckedAt: now})
		}
		return nil
	})
	return out, err
}

// ShipmentLicense returns ExportLicenseGuard v1's verdict for a shipment now, without locks
// (export_license.shipment, cnx.read).
func (s Service) ShipmentLicense(ctx context.Context, shipmentID string) (api.ShipmentLicenseView, error) {
	var v api.ShipmentLicenseView
	if !kernel.ValidIdentifier(shipmentID) {
		return v, kernel.Reject(kernel.CodeSchemaInvalid, "/id", "")
	}
	err := s.Ledger.Read(ctx, api.CommandShipmentLicense, shipmentID, func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		now := kernel.Instant(s.Clock.Now())
		verdict, err := api.ScanExportLicenseGuard(shipmentID, exec.QueryRow(ctx, postgres.ShipmentStateSQL, shipmentID, now).Scan)
		v = api.ShipmentLicenseView{ExportLicenseVerdict: verdict, CheckedAt: now}
		return err
	})
	return v, err
}
