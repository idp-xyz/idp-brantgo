package application

import (
	"context"
	"slices"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pur/api"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/commitment"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/draft"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// ResolveReceiptRequest answers ReceiptRequest v1 (R28) for STK. It reads the commitment in a PUR
// transaction of its own, without a lock: the relationship is not a guard. A verified supplier
// delivery on the line is reported as DirectShip and is not itself a receipt.
func (s Service) ResolveReceiptRequest(ctx context.Context, commitmentID, orderLineID string) (api.ReceiptRequest, error) {
	if !kernel.ValidIdentifier(commitmentID) || !kernel.ValidIdentifier(orderLineID) {
		return api.ReceiptRequest{}, kernel.Reject(kernel.CodeSchemaInvalid, "/args/source_id", "")
	}
	var out api.ReceiptRequest
	var found bool
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		sc := scope(ctx)
		c, err := postgres.LoadCommitment(ctx, exec, sc, commitmentID, postgres.NoLock)
		if err != nil || c == nil || !d.Sees(c.OwnerActor, c.OwnerTeam) {
			return err
		}
		dr, err := postgres.LoadDraft(ctx, exec, sc, c.DraftID, postgres.NoLock)
		if err != nil || dr == nil || dr.Verify(sc, "/") != nil {
			return err
		}
		doc, err := dr.Decode()
		if err != nil {
			return err
		}
		lines, err := c.Lines(dr)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(doc.Lines, func(x draft.DocLine) bool { return x.OrderLineID == orderLineID })
		q := slices.IndexFunc(lines, func(x commitment.LineQuantity) bool { return x.OrderLineID == orderLineID })
		if i < 0 || q < 0 {
			return nil
		}
		holds, err := postgres.DispositionsOfCommitment(ctx, exec, sc, commitmentID)
		if err != nil {
			return err
		}
		verified, err := postgres.VerifiedOfCommitment(ctx, exec, sc, commitmentID, "")
		if err != nil {
			return err
		}
		out = receiptRequest(c, doc, doc.Lines[i], lines[q], holds, verified[orderLineID])
		found = true
		return nil
	})
	if err != nil || !found {
		if err != nil {
			return api.ReceiptRequest{}, err
		}
		return api.ReceiptRequest{}, kernel.Reject(kernel.CodeNotFound, "/args/source_id", "")
	}
	return out, nil
}

func receiptRequest(c *commitment.Commitment, doc draft.Document, dl draft.DocLine, q commitment.LineQuantity, holds []*commitment.Disposition, verified kernel.Decimal) api.ReceiptRequest {
	basis := "1"
	for _, g := range doc.PriceGroups {
		if slices.Contains(g.OrderLineIDs, dl.OrderLineID) && g.PriceBasisQuantity != "" {
			basis = g.PriceBasisQuantity
			break
		}
	}
	ordered := textOf(q.Purchased())
	unitCost, _ := api.NetUnitCost(doc.Currency, doc.MinorUnits, dl.NetMinor, basis, ordered)
	var hold kernel.Decimal
	for _, d := range holds {
		if d.OrderLineID == dl.OrderLineID && d.Status == commitment.DispositionApproved && d.Kind == commitment.HoldForReuse {
			hold = hold.Add(d.Quantity)
		}
	}
	out := api.ReceiptRequest{
		CommitmentID: c.ID, OrderLineID: dl.OrderLineID, OrderID: c.OrderID, Status: c.Status, RowVersion: c.RowVersion,
		SupplierID: c.SupplierID, SkuID: dl.Sku.SkuID, SpecRevision: dl.Sku.SpecRevision, UnitID: dl.UnitRef.ID,
		UnitRevision: dl.UnitRef.Revision, OrderedQuantity: ordered, AllocatedQuantity: textOf(q.Allocated),
		Currency: doc.Currency, MinorUnits: doc.MinorUnits, NetMinor: dl.NetMinor, PriceBasisQuantity: basis,
		UnitCostMinor: unitCost, HoldForReuse: hold.Sign() > 0, HoldForReuseQuantity: textOf(hold),
		DirectShip: verified.Sign() > 0,
	}
	if c.Issue != nil {
		out.PONumber = c.Issue.Number
	}
	if !commitment.IsOpen(c.Status) {
		out.Code = kernel.CodeInvalidState
	}
	return out
}

func textOf(d kernel.Decimal) string {
	s, ok := d.Exact()
	if !ok {
		return "0"
	}
	return s
}
