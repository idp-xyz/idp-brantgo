// Package plan is the CollaborationPlan aggregate (MFG-AGG-CollaborationPlan, SM-MFG-PLAN,
// MFG-INV-001; S6-D02). OperationTask is an entity and MaterialResponsibility is a value of the
// one root. collaboration_plan.publish is the only command that moves the plan. A company that
// has not enabled manufacturing does not publish (DEPENDENCY_NOT_SUPPORTED).
package plan

import (
	"slices"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// States of a collaboration plan (SM-MFG-PLAN).
const (
	StatusDraft      = "draft"
	StatusPublished  = "published"
	StatusInProgress = "in_progress"
	StatusPaused     = "paused"
	StatusCompleted  = "completed"
	StatusCancelled  = "cancelled"
)

// Material responsibility (UL MFG-MaterialResponsibility). Supply does not choose a delivery route.
const (
	MaterialFactoryPurchased = "factory_purchased"
	MaterialOurSupply        = "our_supply"
	MaterialCustomerSupply   = "customer_supply"
	MaterialMixed            = "mixed"
)

const (
	// CodeDependencyNotSupported: the company has not enabled MFG (MFG-INV-001).
	CodeDependencyNotSupported = "DEPENDENCY_NOT_SUPPORTED"
	// CodeBusinessKeyConflict: the plan ID holds another request.
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

// KnownMaterial reports whether kind is one of the four responsibilities.
func KnownMaterial(kind string) bool {
	switch kind {
	case MaterialFactoryPurchased, MaterialOurSupply, MaterialCustomerSupply, MaterialMixed:
		return true
	default:
		return false
	}
}

// KnownTarget reports whether to is a plan state.
func KnownTarget(to string) bool {
	switch to {
	case StatusDraft, StatusPublished, StatusInProgress, StatusPaused, StatusCompleted, StatusCancelled:
		return true
	default:
		return false
	}
}

// Operation is one operation task of the plan.
type Operation struct {
	ID           string
	Name         string
	Sequence     int64
	SkuID        string
	SpecRevision int64
	Quantity     kernel.Decimal
	UnitID       string
	UnitRevision int64
}

// Material is one material responsibility.
type Material struct {
	ID           string
	Kind         string
	SkuID        string
	Quantity     kernel.Decimal
	UnitID       string
	UnitRevision int64
	LossBasis    string
}

// Plan is one collaboration plan. Version 0 means it has not been stored.
type Plan struct {
	ID          string
	Status      string
	Version     int64
	FactoryID   string
	Note        string
	Operations  []Operation
	Materials   []Material
	RequestHash string
	OwnerActor  string
	OwnerTeam   string
}

// Terminal reports whether the plan no longer moves.
func (p *Plan) Terminal() bool {
	return p.Status == StatusCompleted || p.Status == StatusCancelled
}

// Answer reports whether an existing plan answers the same creating request.
func (p *Plan) Answer(requestHash, actorID string) error {
	if p.RequestHash != requestHash || p.OwnerActor != actorID {
		return fail(CodeBusinessKeyConflict, "/args/plan_id", "The plan ID holds another plan")
	}
	return nil
}

// HoldDraft stores a new plan as draft, or replaces the body of a stored draft. It is not a
// state-machine edge: draft is the initial state.
func (p *Plan) HoldDraft() error {
	if p.Version > 0 && p.Status != StatusDraft {
		return fail(kernel.CodeInvalidState, "/args/target", "The plan is "+p.Status)
	}
	p.Status = StatusDraft
	if p.Version == 0 {
		p.Version = 1
	} else {
		p.Version++
	}
	return nil
}

// Transition moves the plan along SM-MFG-PLAN. Publishing needs at least one operation and one
// material responsibility. A new plan (version 0, status draft) lands on the target at version 1.
func (p *Plan) Transition(to string) error {
	if p.Version == 0 {
		p.Status = StatusDraft
	}
	if !edge(p.Status, to) {
		return fail(kernel.CodeInvalidState, "/args/target", "The plan is "+p.Status)
	}
	if to == StatusPublished && (len(p.Operations) == 0 || len(p.Materials) == 0) {
		return fail(kernel.CodeDraftNotReady, "/args/operations", "Material responsibility and operation granularity are required")
	}
	p.Status = to
	if p.Version == 0 {
		p.Version = 1
	} else {
		p.Version++
	}
	return nil
}

func edge(from, to string) bool {
	switch from {
	case StatusDraft:
		return to == StatusPublished || to == StatusCancelled
	case StatusPublished:
		return to == StatusInProgress || to == StatusCancelled
	case StatusInProgress:
		return to == StatusPaused || to == StatusCompleted || to == StatusCancelled
	case StatusPaused:
		return to == StatusInProgress || to == StatusCancelled
	default:
		return false
	}
}

// OperationByID returns the operation and whether the plan has it.
func (p *Plan) OperationByID(id string) (Operation, bool) {
	i := slices.IndexFunc(p.Operations, func(o Operation) bool { return o.ID == id })
	if i < 0 {
		return Operation{}, false
	}
	return p.Operations[i], true
}
