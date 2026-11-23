package application

import (
	"context"

	clmapi "go.idp.xyz/idp-brant/internal/contexts/clm/api"
	stkapi "go.idp.xyz/idp-brant/internal/contexts/stk/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/orderresult"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Claims is CLM's CreditRequest v1 (R35). STL reads it before its transaction and keeps the
// uniqueness of a settled remedy itself. Nil means the order has no claim.
type Claims interface {
	ResolveCreditRequest(ctx context.Context, claimID, remedyID string) (clmapi.CreditRequest, error)
	ResolveOrderClaims(ctx context.Context, orderID string) ([]clmapi.OrderClaim, error)
}

// StockCosts is STK's CostLayerRef v1 (R41). Nil leaves a stock shipment's cost as a gap.
type StockCosts interface {
	ResolveShipmentStockCost(ctx context.Context, shipmentIDs []string) ([]stkapi.ShipmentCost, error)
}

// remedyCite is an approved remedy the command named, read before the transaction.
type remedyCite struct {
	set bool
	req clmapi.CreditRequest
}

func (s Service) readRemedy(ctx context.Context, claimID, remedyID, wantKind string) (remedyCite, error) {
	if claimID == "" && remedyID == "" {
		return remedyCite{}, nil
	}
	if s.Upstream.Claims == nil {
		return remedyCite{}, kernel.Reject(kernel.CodeInvalidState, "/args/claim_id", "CLM is not wired")
	}
	req, err := s.Upstream.Claims.ResolveCreditRequest(ctx, claimID, remedyID)
	if err != nil {
		return remedyCite{}, err
	}
	if !req.Approved || req.Kind != wantKind {
		return remedyCite{}, kernel.Reject(kernel.CodeInvalidState, "/args/remedy_id", "The remedy is not an approved "+wantKind)
	}
	return remedyCite{set: true, req: req}, nil
}

// matchRemedy checks the approved remedy against the order, the customer and the amount being settled.
func matchRemedy(c remedyCite, orderID, customerID string, amount kernel.Money, path string) error {
	if !c.set {
		return nil
	}
	req := c.req
	switch {
	case orderID == "" || orderID != req.OrderID:
		return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/order_id", "The remedy is of another order")
	case customerID == "" || customerID != req.CustomerID:
		return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/counterparty_id", "The remedy is of another customer")
	case amount.Currency() != req.Amount.Currency():
		return kernel.Reject(kernel.CodeInvalidKnownValue, path, "The remedy is in "+req.Amount.Currency().Code())
	}
	cmp, err := amount.Cmp(req.Amount)
	if err != nil {
		return kernel.Reject(kernel.CodeInvalidKnownValue, path, "The remedy is in another currency")
	}
	if cmp > 0 {
		return kernel.Reject(kernel.CodeInvalidKnownValue, path, "The amount is above the approved remedy")
	}
	return nil
}

func (s Service) orderClaims(ctx context.Context, orderID string) ([]orderresult.ClaimFact, error) {
	if s.Upstream.Claims == nil {
		return nil, nil
	}
	list, err := s.Upstream.Claims.ResolveOrderClaims(ctx, orderID)
	if err != nil {
		return nil, err
	}
	out := make([]orderresult.ClaimFact, 0, len(list))
	for _, c := range list {
		out = append(out, orderresult.ClaimFact{ClaimID: c.ClaimID, Open: c.Open})
	}
	return out, nil
}

// stockCosts reads CostLayerRef for the confirmed stock shipments, at most 200 ids at a time.
func (s Service) stockCosts(ctx context.Context, lines []OrderShipmentLine, order, result kernel.Currency) ([]orderresult.StockCost, error) {
	if s.Upstream.Stock == nil {
		return nil, nil
	}
	seen := map[string]bool{}
	var ids []string
	for _, l := range lines {
		if l.Route != "stock" || l.Status != "confirmed" || seen[l.ShipmentID] {
			continue
		}
		seen[l.ShipmentID] = true
		ids = append(ids, l.ShipmentID)
	}
	var out []orderresult.StockCost
	for start := 0; start < len(ids); start += 200 {
		end := start + 200
		if end > len(ids) {
			end = len(ids)
		}
		got, err := s.Upstream.Stock.ResolveShipmentStockCost(ctx, ids[start:end])
		if err != nil {
			return nil, err
		}
		for _, c := range got {
			item := orderresult.StockCost{ShipmentID: c.ShipmentID, LineID: c.ShipmentLineID, MovementID: c.MovementID}
			switch c.Currency {
			case order.Code():
				item.Amount, err = kernel.NewMoney(order, c.TotalCostMinor)
			case result.Code():
				item.Amount, err = kernel.NewMoney(result, c.TotalCostMinor)
			default:
				item.Gap = "stock cost of shipment " + c.ShipmentID + " line " + c.ShipmentLineID + " is in " + c.Currency
			}
			if err != nil {
				return nil, err
			}
			out = append(out, item)
		}
	}
	return out, nil
}

// settledRemedy is the catalog code a second settlement of one remedy gets.
func settledRemedy(err error) error {
	if postgres.UniqueViolation(err) {
		return api.Reject(api.CodeClaimRemedyAlreadySettled, "/args/remedy_id", "The remedy is already settled")
	}
	return err
}
