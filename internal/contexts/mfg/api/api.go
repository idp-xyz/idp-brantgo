// Package api is MFG's published contract (production collaboration and subcontracting, WBS S6-02;
// MFG-AGG-CollaborationPlan, MFG-AGG-SubcontractOrder; SM-MFG-PLAN, SM-MFG-SUBCONTRACT;
// MFG-INV-001, MFG-INV-002; S6-D02). Python has no manufacturing context. The company switch
// defaults to off.
//
//	mfg.module.configure       human only, administrator: enables or disables the company.
//	                           expected_version 0 inserts the setting.
//	collaboration_plan.publish human or AI, merchandiser: saves a draft or moves SM-MFG-PLAN.
//	                           expected_version 0 creates the plan. target draft saves it.
//	                           Publishing needs a material responsibility and an operation.
//	subcontract_order.issue    human only, purchaser: creates the subcontract issued. A cited
//	                           commitment is read from PUR before the transaction (R23).
//	operation.record_progress  human or AI, merchandiser: a source claim on the subcontract.
//	                           It does not move the plan, write stock or release quality.
//
// RecordMaterialHandover is in process (R42). It is not an HTTP command. The composition root
// calls STK outside MFG's transaction. HTTP writing routes refuse an AI actor before the body
// is read.
//
// Request members in snake_case. Scope, actor and tenant come from the principal.
package api

import (
	"context"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Source is MFG's event source.
const Source = "idp-brant.mfg"

// Contract is the envelope contract on POST /api/brant/v1/mfg/commands/<command>.
const Contract = "brant.manufacturing/0.1.0"

// Commands, reads and events.
const (
	CommandConfigure = "mfg.module.configure"
	CommandPublish   = "collaboration_plan.publish"
	CommandIssue     = "subcontract_order.issue"
	CommandProgress  = "operation.record_progress"
	CommandHandover  = "material.handover" // in process only (R42); not an HTTP route

	CommandGetSetting     = "mfg.setting.get"
	CommandGetPlan        = "collaboration_plan.get"
	CommandGetSubcontract = "subcontract_order.get"
	CommandGetSubject     = "inspection_subject.get"
	CommandAudit          = "mfg.audit"
	PermissionRead        = "mfg.read"
	PermissionAudit       = "mfg.audit"

	EventPlanPublished     = "idp-brant.mfg.collaboration_plan_published.v1"
	EventSubcontractIssued = "idp-brant.mfg.subcontract_order_issued.v1"
	EventProgressRecorded  = "idp-brant.mfg.operation_progress_recorded.v1"

	StatusDraft      = "draft"
	StatusPublished  = "published"
	StatusInProgress = "in_progress"
	StatusPaused     = "paused"
	StatusCompleted  = "completed"
	StatusCancelled  = "cancelled"
	StatusIssued     = "issued"
	StatusEnabled    = "enabled"
	StatusDisabled   = "disabled"

	MaterialFactoryPurchased = "factory_purchased"
	MaterialOurSupply        = "our_supply"
	MaterialCustomerSupply   = "customer_supply"
	MaterialMixed            = "mixed"

	HandoverTransfer = "handover_transfer"
	HandoverConsume  = "handover_consume"
	HandoverReturn   = "handover_return"
	HandoverScrap    = "handover_scrap"

	// SubjectSubcontractOperation is the inspection subject kind MFG publishes (R25).
	SubjectSubcontractOperation = "mfg_subcontract_operation"

	MaxNote   = 1000
	MaxName   = 200
	MaxReason = 1000
	MaxLoss   = 200
)

// Receipt is the closed success of a command.
type Receipt struct {
	ActionID   string
	ObjectID   string
	RowVersion int64
	Status     string
	Replayed   bool
}

// Reconciliation is what became of a command whose caller lost the answer.
type Reconciliation struct {
	State    kernel.ReconcileState
	ActionID string
	ObjectID string
	Receipt  *Receipt
}

// ConfigureModule sets the company switch. ExpectedVersion 0 creates it. Human only.
type ConfigureModule struct {
	IdempotencyKey  string
	Enabled         bool
	ExpectedVersion int64
}

// OperationInput is one operation task saved with the plan.
type OperationInput struct {
	OperationID  string
	Name         string
	Sequence     int64
	SkuID        string
	SpecRevision int64
	Quantity     string
	UnitID       string
	UnitRevision int64
}

// MaterialInput is one material responsibility saved with the plan.
type MaterialInput struct {
	MaterialID   string
	Kind         string
	SkuID        string
	Quantity     string
	UnitID       string
	UnitRevision int64
	LossBasis    string
}

// PublishPlan creates a plan or moves it. Target is the resulting state. ExpectedVersion 0
// creates it: target draft saves a draft, target published or cancelled applies that edge from
// draft in the same command. A later command sends the stored version. Human or AI.
type PublishPlan struct {
	IdempotencyKey  string
	PlanID          string
	ExpectedVersion int64
	Target          string
	FactoryPartyID  string
	Note            string
	Operations      []OperationInput
	Materials       []MaterialInput
}

// IssueSubcontract creates subcontract SubcontractID against an operation of a published,
// in-progress or paused plan. CommitmentID, when set, cites PUR SubcontractDemand v1 (R23);
// the processing quantity is not a sales demand. Human only.
type IssueSubcontract struct {
	IdempotencyKey     string
	SubcontractID      string
	PlanID             string
	OperationID        string
	Quantity           string
	UnitID             string
	UnitRevision       int64
	Currency           string
	MinorUnits         int
	ProcessingFeeMinor string
	CommitmentID       string
	OrderID            string
	OrderLineID        string
}

// RecordProgress records one source claim and moves the subcontract. It does not move the plan.
// Human or AI.
type RecordProgress struct {
	IdempotencyKey  string
	SubcontractID   string
	ProgressID      string
	ExpectedVersion int64
	OperationID     string
	ProgressKind    string
	Quantity        string
	UnitID          string
	ReportedAt      string
}

// RecordMaterialHandover associates a subcontract with one STK handover (R42). It is not an
// HTTP command. CounterMovementID and the destination are set only for a transfer or a return.
type RecordMaterialHandover struct {
	IdempotencyKey    string
	SubcontractID     string
	MovementID        string
	CounterMovementID string
	Kind              string
	SkuID             string
	LocationID        string
	BatchID           string
	CustodyKind       string
	ToLocationID      string
	ToBatchID         string
	ToCustodyKind     string
	Quantity          string
	UnitID            string
	UnitRevision      int64
	Reason            string
}

// Command is one of MFG's commands.
type Command interface{ mfgCommand() }

func (ConfigureModule) mfgCommand()        {}
func (PublishPlan) mfgCommand()            {}
func (IssueSubcontract) mfgCommand()       {}
func (RecordProgress) mfgCommand()         {}
func (RecordMaterialHandover) mfgCommand() {}

// Commands is MFG's command entry point. RecordMaterialHandover is in process only.
type Commands interface {
	ConfigureModule(ctx context.Context, cmd ConfigureModule) (Receipt, error)
	PublishPlan(ctx context.Context, cmd PublishPlan) (Receipt, error)
	IssueSubcontract(ctx context.Context, cmd IssueSubcontract) (Receipt, error)
	RecordProgress(ctx context.Context, cmd RecordProgress) (Receipt, error)
	RecordMaterialHandover(ctx context.Context, cmd RecordMaterialHandover) (Receipt, error)
}

// SettingView is the company switch. A company that was never configured is disabled at version 0.
type SettingView struct {
	Enabled    bool
	RowVersion int64
	UpdatedAt  string
}

// OperationView is one operation of a plan.
type OperationView struct {
	OperationID  string
	Name         string
	Sequence     int64
	SkuID        string
	SpecRevision int64
	Quantity     string
	UnitID       string
	UnitRevision int64
}

// MaterialView is one material responsibility.
type MaterialView struct {
	MaterialID   string
	Kind         string
	SkuID        string
	Quantity     string
	UnitID       string
	UnitRevision int64
	LossBasis    string
}

// PlanView is a collaboration plan.
type PlanView struct {
	PlanID         string
	Status         string
	RowVersion     int64
	FactoryPartyID string
	Note           string
	Operations     []OperationView
	Materials      []MaterialView
}

// SubcontractView is a subcontract order. DemandCommitmentID is empty when the issue cited none.
type SubcontractView struct {
	SubcontractID      string
	Status             string
	RowVersion         int64
	PlanID             string
	OperationID        string
	Quantity           string
	UnitID             string
	UnitRevision       int64
	Currency           string
	MinorUnits         int
	ProcessingFeeMinor string
	CommitmentID       string
	OrderID            string
	OrderLineID        string
}

// InspectionSubject is InspectionSubject v1 for kind mfg_subcontract_operation (R25). Code is
// empty when the subcontract can be inspected. Quantity is the subcontracted quantity, a source
// claim's completion is not a release, and this value is not a stock receipt.
type InspectionSubject struct {
	SubcontractID string
	OperationID   string
	PlanID        string
	FactoryID     string
	Status        string
	Revision      int64
	Hash          string
	OrderID       string
	OrderRevision int64
	OrderHash     string
	OrderLineID   string
	SkuID         string
	SpecRevision  int64
	UnitID        string
	UnitRevision  int64
	Quantity      string
	Code          string
}

// Queries are the in-process reads. They are not HTTP routes.
type Queries interface {
	GetSetting(ctx context.Context) (SettingView, error)
	GetPlan(ctx context.Context, planID string) (PlanView, error)
	GetSubcontract(ctx context.Context, subcontractID string) (SubcontractView, error)
	ResolveInspectionSubject(ctx context.Context, subcontractID, operationID string) (InspectionSubject, error)
}

// Auditor is the audit trail.
type Auditor interface {
	Audit(ctx context.Context) ([]kernel.AuditEntry, error)
	AuditRejected(ctx context.Context, command, objectID, code string) error
}

// Reconciler answers what became of a command without executing it.
type Reconciler interface {
	Reconcile(ctx context.Context, cmd Command) (Reconciliation, error)
}

// HandoverRequest is one material handover the composition root records in STK (MaterialHandover
// v1, R42) before MFG stores the association. It is not an HTTP body. MFG does not import STK;
// the composition root implements Handovers.
type HandoverRequest struct {
	IdempotencyKey    string
	MovementID        string
	CounterMovementID string
	Kind              string
	SkuID             string
	LocationID        string
	BatchID           string
	CustodyKind       string
	ToLocationID      string
	ToBatchID         string
	ToCustodyKind     string
	Quantity          string
	UnitID            string
	UnitRevision      int64
	Reason            string
}

// HandoverResult is the stock movement the port recorded. MovementID is the outbound movement.
type HandoverResult struct {
	MovementID string
	Replayed   bool
}

// Handovers is the port to STK. Enabled is false when the company has not enabled stock. Record
// runs in STK's own transaction, before MFG's.
type Handovers interface {
	Enabled(ctx context.Context) (bool, error)
	Record(ctx context.Context, req HandoverRequest) (HandoverResult, error)
}
