package upstream

import (
	"context"

	"go.idp.xyz/idp-brant/internal/contexts/cnx/internal/application"
	purapi "go.idp.xyz/idp-brant/internal/contexts/pur/api"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// PUR answers application.PurchaseLines from PUR's DeliveryLookups (PurchaseLineRef v1, R37; an
// active grant, PUR's data scope), at most purapi.MaxCommitmentLineRefs refs per lookup.
type PUR struct {
	Lines purapi.DeliveryLookups
}

var _ application.PurchaseLines = PUR{}

// ResolvePurchaseLines asks PUR before CNX's transaction.
func (p PUR) ResolvePurchaseLines(ctx context.Context, refs []application.PurchaseLineRef) (map[application.PurchaseLineRef]application.PurchaseLine, error) {
	out := map[application.PurchaseLineRef]application.PurchaseLine{}
	for start := 0; start < len(refs); start += purapi.MaxCommitmentLineRefs {
		chunk := refs[start:min(start+purapi.MaxCommitmentLineRefs, len(refs))]
		asked := make([]purapi.CommitmentLineRef, len(chunk))
		for i, r := range chunk {
			asked[i] = purapi.CommitmentLineRef{CommitmentID: r.CommitmentID, OrderLineID: r.OrderLineID}
		}
		got, err := p.Lines.ResolvePurchaseLines(ctx, asked)
		if err != nil {
			return nil, err
		}
		for ref, l := range got {
			bought, err := kernel.ParseDecimal(l.Line.PurchaseQuantity, kernel.QuantityFormat)
			if err != nil {
				return nil, err
			}
			delivered := kernel.Decimal{}
			if l.VerifiedQuantity != "" {
				if delivered, err = kernel.ParseDecimal(l.VerifiedQuantity, kernel.QuantityFormat); err != nil {
					return nil, err
				}
			}
			out[application.PurchaseLineRef{CommitmentID: ref.CommitmentID, OrderLineID: ref.OrderLineID}] = application.PurchaseLine{
				Code: l.Line.Code, Status: l.Line.Status, SupplierID: l.Line.SupplierID, Unit: l.Line.UnitID, PONumber: l.Line.PONumber,
				PurchaseQuantity: bought, DeliveredQuantity: delivered}
		}
	}
	return out, nil
}

// STL answers application.Funds from STL's FundRecordLookups (FundRecordRef v1, R38; an active
// grant, STL's data scope).
type STL struct {
	Funds stlapi.FundRecordLookups
}

var _ application.Funds = STL{}

// ResolveFundRecords asks STL before CNX's transaction.
func (s STL) ResolveFundRecords(ctx context.Context, ids []string) (map[string]application.FundRecord, error) {
	out := map[string]application.FundRecord{}
	if len(ids) == 0 {
		return out, nil
	}
	got, err := s.Funds.ResolveFundRecords(ctx, ids)
	if err != nil {
		return nil, err
	}
	for id, f := range got {
		out[id] = application.FundRecord{ID: f.FundRecordID, Kind: f.Kind, Direction: f.Direction, Status: f.Status, Currency: f.Currency,
			ReceivedMinor: f.ReceivedMinor}
	}
	return out, nil
}

// STLCredits answers application.CreditNotes from STL's CreditNoteLookups (CreditNoteRef v1 on R38; an active
// grant, STL's data scope).
type STLCredits struct {
	Notes stlapi.CreditNoteLookups
}

var _ application.CreditNotes = STLCredits{}

// ResolveCreditNotes asks STL before CNX's transaction.
func (s STLCredits) ResolveCreditNotes(ctx context.Context, ids []string) (map[string]application.CreditNote, error) {
	out := map[string]application.CreditNote{}
	if len(ids) == 0 {
		return out, nil
	}
	got, err := s.Notes.ResolveCreditNotes(ctx, ids)
	if err != nil {
		return nil, err
	}
	for id, n := range got {
		c := application.CreditNote{ID: n.CreditNoteID, Kind: n.CreditKind, Status: n.Status, Version: n.Version, StatementID: n.StatementID,
			StatementStatus: n.StatementStatus, OrderID: n.OrderID, Currency: n.Currency, AmountMinor: n.AmountMinor, Effective: n.Effective}
		for _, src := range n.Sources {
			c.Sources = append(c.Sources, application.ShipmentLineKey{ShipmentID: src.ShipmentID, LineID: src.LineID})
		}
		out[id] = c
	}
	return out, nil
}
