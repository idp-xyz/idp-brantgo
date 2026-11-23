package application

import (
	"context"

	"go.idp.xyz/idp-brant/internal/contexts/pur/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// ResolveSubcontractDemand answers SubcontractDemand v1 (R23) from the commitment line PUR already
// resolves for InspectionSubject v1. A line the caller cannot see is NOT_FOUND. OperationQuantity
// is the purchase quantity of that line: the quantity to process, not a sales quantity added to
// the order. Code is "" when the commitment is open and INVALID_STATE otherwise.
func (s Service) ResolveSubcontractDemand(ctx context.Context, commitmentID, orderLineID string) (api.SubcontractDemand, error) {
	ref := api.CommitmentLineRef{CommitmentID: commitmentID, OrderLineID: orderLineID}
	lines, err := s.ResolveCommitmentLines(ctx, []api.CommitmentLineRef{ref})
	if err != nil {
		return api.SubcontractDemand{}, err
	}
	line, ok := lines[ref]
	if !ok {
		return api.SubcontractDemand{}, kernel.Reject(kernel.CodeNotFound, "/args/commitment_id", "")
	}
	return api.SubcontractDemand{CommitmentID: line.Ref.CommitmentID, OrderLineID: line.Ref.OrderLineID, OrderID: line.OrderID,
		OrderRevision: line.OrderRevision, OrderHash: line.OrderHash, Status: line.Status, SupplierID: line.SupplierID,
		SkuID: line.SkuID, SpecRevision: line.SpecRevision, UnitID: line.UnitID, UnitRevision: line.UnitRevision,
		OperationQuantity: line.PurchaseQuantity, Code: line.Code}, nil
}
