package api

import "context"

// SubcontractDemand v1 (Context Map R23 PUR→MFG, guardLock false: 外协加工需求). MFG reads it
// before issuing a subcontract that cites a commitment, and does not write the processing quantity
// back as a sales demand. There is no PUR schema change: the demand is the commitment line PUR
// already publishes as InspectionSubject v1, and OperationQuantity is that line's purchase
// quantity (the quantity to process), not an added sales quantity.

// SubcontractDemand is one commitment line as a processing demand. Code is "" when the commitment
// is open. OperationQuantity is the processing quantity (the line's purchase quantity).
type SubcontractDemand struct {
	CommitmentID      string
	OrderLineID       string
	OrderID           string
	OrderRevision     int64
	OrderHash         string
	Status            string
	SupplierID        string
	SkuID             string
	SpecRevision      int64
	UnitID            string
	UnitRevision      int64
	OperationQuantity string
	Code              string
}

// SubcontractDemands answer SubcontractDemand v1 lookups of MFG (R23). A commitment the caller
// cannot see, or a line the commitment does not have, is NOT_FOUND. Ask before opening your own
// transaction. An active grant is enough; the lookup is not a command.
type SubcontractDemands interface {
	ResolveSubcontractDemand(ctx context.Context, commitmentID, orderLineID string) (SubcontractDemand, error)
}
