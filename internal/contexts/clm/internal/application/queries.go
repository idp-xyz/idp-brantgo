package application

import (
	"context"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/clm/api"
	"go.idp.xyz/idp-brant/internal/contexts/clm/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/clm/internal/domain/claim"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

func viewOf(c *claim.Case) api.ClaimView {
	v := api.ClaimView{ClaimID: c.ID, Status: c.Status, RowVersion: c.Version, OrderID: c.OrderID, CustomerID: c.CustomerID,
		ShipmentID: c.ShipmentID, ShipmentLineID: c.LineID, ShippedQuantity: postgres.Text(c.Shipped), UnitID: c.UnitID,
		Currency: c.Currency.Code(), MinorUnits: c.Currency.MinorUnits(), Reason: c.Reason, EvidenceIDs: append([]string(nil), c.Evidence...),
		OpenedBy: c.Opened.By, OpenedAt: c.Opened.At}
	for _, r := range c.Returns {
		v.Returns = append(v.Returns, api.ReturnView{ReturnID: r.ID, Quantity: postgres.Text(r.Quantity), Condition: r.Condition, Note: r.Note,
			EvidenceIDs: append([]string(nil), r.Evidence...), RegisteredBy: r.By.By, RegisteredAt: r.By.At})
	}
	if v.Returns == nil {
		v.Returns = []api.ReturnView{}
	}
	if c.Remedy != nil {
		v.Remedy = &api.RemedyView{RemedyID: c.Remedy.ID, Kind: c.Remedy.Kind, AmountMinor: c.Remedy.Amount.MinorString(),
			Currency: c.Remedy.Amount.Currency().Code(), Reason: c.Remedy.Reason, DecidedBy: c.Remedy.By.By, DecidedAt: c.Remedy.By.At}
	}
	if c.Closure != nil {
		v.Summary, v.ClosedBy, v.ClosedAt = c.Closure.Summary, c.Closure.By.By, c.Closure.By.At
	}
	return v
}

func creditOf(c *claim.Case) api.CreditRequest {
	return api.CreditRequest{ClaimID: c.ID, Status: c.Status, OrderID: c.OrderID, CustomerID: c.CustomerID, ShipmentID: c.ShipmentID,
		ShipmentLineID: c.LineID, RemedyID: c.Remedy.ID, Kind: c.Remedy.Kind, Amount: c.Remedy.Amount, Approved: c.Approved()}
}

// GetClaim returns a claim the caller sees, audited as claim.get.
func (s Service) GetClaim(ctx context.Context, claimID string) (api.ClaimView, error) {
	if !kernel.ValidIdentifier(claimID) {
		return api.ClaimView{}, kernel.Reject(kernel.CodeSchemaInvalid, "/claim_id", "")
	}
	var view api.ClaimView
	err := s.Ledger.Read(ctx, api.CommandGetClaim, claimID, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		c, err := postgres.LoadCase(ctx, exec, scopeOf(ctx), claimID, postgres.NoLock)
		if err != nil {
			return err
		}
		if c == nil {
			return kernel.Reject(kernel.CodeNotFound, "/claim_id", "")
		}
		if err = seen(d, c.OwnerActor, c.OwnerTeam); err != nil {
			return err
		}
		view = viewOf(c)
		return nil
	})
	return view, err
}

// ResolveCreditRequest answers CreditRequest v1. A remedy that is not an approved credit or refund
// is returned with Approved false.
func (s Service) ResolveCreditRequest(ctx context.Context, claimID, remedyID string) (api.CreditRequest, error) {
	switch {
	case !kernel.ValidIdentifier(claimID):
		return api.CreditRequest{}, kernel.Reject(kernel.CodeSchemaInvalid, "/claim_id", "")
	case !kernel.ValidIdentifier(remedyID):
		return api.CreditRequest{}, kernel.Reject(kernel.CodeSchemaInvalid, "/remedy_id", "")
	}
	var out api.CreditRequest
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		c, err := postgres.LoadCase(ctx, exec, scopeOf(ctx), claimID, postgres.NoLock)
		if err != nil {
			return err
		}
		if c == nil {
			return kernel.Reject(kernel.CodeNotFound, "/claim_id", "")
		}
		if err = seen(d, c.OwnerActor, c.OwnerTeam); err != nil {
			return err
		}
		if c.Remedy == nil || c.Remedy.ID != remedyID {
			return kernel.Reject(kernel.CodeNotFound, "/remedy_id", "")
		}
		out = creditOf(c)
		return nil
	})
	return out, err
}

// ResolveOrderClaims answers the claims of an order the caller sees. A claim is open until it is
// closed, including one whose remedy is reject.
func (s Service) ResolveOrderClaims(ctx context.Context, orderID string) ([]api.OrderClaim, error) {
	if !kernel.ValidIdentifier(orderID) {
		return nil, kernel.Reject(kernel.CodeSchemaInvalid, "/order_id", "")
	}
	var out []api.OrderClaim
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		list, err := postgres.ListOrder(ctx, exec, scopeOf(ctx), orderID)
		if err != nil {
			return err
		}
		out = []api.OrderClaim{}
		for _, c := range list {
			if !d.Sees(c.OwnerActor, c.OwnerTeam) {
				continue
			}
			kind := ""
			if c.Remedy != nil {
				kind = c.Remedy.Kind
			}
			out = append(out, api.OrderClaim{ClaimID: c.ID, Status: c.Status, Open: c.Status != claim.StatusClosed, OrderID: c.OrderID,
				ShipmentID: c.ShipmentID, ShipmentLineID: c.LineID, RemedyKind: kind})
		}
		return nil
	})
	return out, err
}
