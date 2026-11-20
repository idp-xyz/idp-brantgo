// Package subcontract is the SubcontractOrder aggregate (MFG-AGG-SubcontractOrder,
// SM-MFG-SUBCONTRACT, MFG-INV-001, MFG-INV-002; S6-D02). operation.record_progress records an
// external factory's source claim. It does not create qualified stock, a quality release or a
// customer delivery, and it does not move the collaboration plan. Material handover kinds stay
// distinct: transfer, consume, return and scrap are not aliases of each other.
package subcontract

import "go.idp.xyz/idp-brant/internal/kernel"

// States of a subcontract order (SM-MFG-SUBCONTRACT). Issue creates it issued.
const (
	StatusIssued     = "issued"
	StatusInProgress = "in_progress"
	StatusCompleted  = "completed"
	StatusCancelled  = "cancelled"
)

// Handover kinds (R42, MFG-INV-002). They are the stock movement kinds, not aliases.
const (
	HandoverTransfer = "handover_transfer"
	HandoverConsume  = "handover_consume"
	HandoverReturn   = "handover_return"
	HandoverScrap    = "handover_scrap"
)

const (
	// CodeDependencyNotSupported: the company has not enabled MFG (MFG-INV-001).
	CodeDependencyNotSupported = "DEPENDENCY_NOT_SUPPORTED"
	// CodeBusinessKeyConflict: the subcontract or progress ID holds another request.
	CodeBusinessKeyConflict = "BUSINESS_KEY_CONFLICT"
)

// Codes are the codes of this package and their categories.
var Codes = map[string]kernel.Category{
	CodeDependencyNotSupported: kernel.CategoryInvalid,
	CodeBusinessKeyConflict:    kernel.CategoryConflict,
}

func fail(code, path, reason string) error {
	if c, ok := Codes[code]; ok {
		return kernel.RejectAs(code, c, path, reason)
	}
	return kernel.Reject(code, path, reason)
}

// Gate refuses every operational command while manufacturing is off (MFG-INV-001).
func Gate(enabled bool) error {
	if enabled {
		return nil
	}
	return fail(CodeDependencyNotSupported, "/", "Manufacturing is not enabled")
}

// HandoverKind accepts one of the four handover kinds and refuses any other spelling, including
// one that would record scrap as a transfer (MFG-INV-002).
func HandoverKind(kind string) error {
	switch kind {
	case HandoverTransfer, HandoverConsume, HandoverReturn, HandoverScrap:
		return nil
	default:
		return fail(kernel.CodeInvalidKnownValue, "/args/kind", "A handover is a transfer, a consume, a return or a scrap")
	}
}

// MovesStock reports whether a progress claim posts stock. It never does (MFG-INV-001).
func MovesStock() bool { return false }

// ReleasesQuality reports whether a progress claim is a quality release. It never is (R25).
func ReleasesQuality() bool { return false }

// Progress is one source claim recorded against the subcontract.
type Progress struct {
	ID        string
	Operation string
	Kind      string
	Quantity  kernel.Decimal
	UnitID    string
	Hash      string
	ActorID   string
}

// Order is one subcontract. Version 1 is the issue. Progress claims do not carry stock.
type Order struct {
	ID          string
	Status      string
	Version     int64
	PlanID      string
	OperationID string
	Quantity    kernel.Decimal
	UnitID      string
	IssueHash   string
	RequestHash string
	OwnerActor  string
	Claims      []Progress
}

// Terminal reports whether the subcontract no longer moves.
func (o *Order) Terminal() bool {
	return o.Status == StatusCompleted || o.Status == StatusCancelled
}

// Answer reports whether an existing subcontract answers the same issue.
// Answer reports whether an existing subcontract answers the same issue. IssueHash is the issue
// request and does not change when a later progress claim is recorded.
func (o *Order) Answer(issueHash, actorID string) error {
	if o.IssueHash != issueHash || o.OwnerActor != actorID {
		return fail(CodeBusinessKeyConflict, "/args/subcontract_id", "The subcontract ID holds another subcontract")
	}
	return nil
}

// Advance records a progress claim and moves SM-MFG-SUBCONTRACT. The claim is the only thing
// added: no stock movement and no quality release is represented on the order.
func (o *Order) Advance(claim Progress) error {
	for _, c := range o.Claims {
		if c.ID != claim.ID {
			continue
		}
		if c.Hash == claim.Hash && c.ActorID == claim.ActorID {
			return nil
		}
		return fail(CodeBusinessKeyConflict, "/args/progress_id", "The progress ID holds another claim")
	}
	if !progressEdge(o.Status, claim.Kind) {
		return fail(kernel.CodeInvalidState, "/args/progress_kind", "The subcontract is "+o.Status)
	}
	if claim.Operation != o.OperationID {
		return fail(kernel.CodeReferenceInvalid, "/args/operation_id", "The claim is for another operation")
	}
	if claim.UnitID != o.UnitID {
		return fail(kernel.CodeInvalidKnownValue, "/args/unit_id", "The claim is in the subcontract unit")
	}
	if claim.Quantity.Sign() <= 0 {
		return fail(kernel.CodeSchemaInvalid, "/args/quantity", "")
	}
	o.Claims = append(o.Claims, claim)
	o.Status = claim.Kind
	o.Version++
	return nil
}

func progressEdge(from, kind string) bool {
	switch from {
	case StatusIssued:
		return kind == StatusInProgress || kind == StatusCancelled
	case StatusInProgress:
		return kind == StatusCompleted || kind == StatusCancelled
	default:
		return false
	}
}

// Replay reports whether claimID already holds this same claim.
func (o *Order) Replay(claimID, hash, actorID string) bool {
	for _, c := range o.Claims {
		if c.ID == claimID && c.Hash == hash && c.ActorID == actorID {
			return true
		}
	}
	return false
}
