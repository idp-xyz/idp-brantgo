package api

import (
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// QuoteView is a quote root with all its revisions, oldest first.
type QuoteView struct {
	QuoteID         string
	RootVersion     int64
	WorkingRevision int64 // 0: none
	ActiveRevision  int64 // 0: none
	LastRevision    int64
	Labels          []string
	InternalNote    string
	Control         ControlView
	Revisions       []RevisionView
	Reviews         []ReviewView   // in the order they were requested
	Approvals       []ApprovalView // in the order they were given
	Holds           []HoldView     // risk holds, in the order they were registered
	Conversion      *ConversionView
}

// ControlView is the root's object control (control.go). A root that is not unbound names its task
// and, when bound from TRD 000009 on, the delegation its last bind or handoff recorded: the task's
// initiator and AI agent (S4 decision D-5).
type ControlView struct {
	State        string // unbound, bound, stopping or manual
	ControlEpoch int64
	TaskID       string // bound, stopping and manual
	InitiatorID  string
	AgentID      string
	ChangedBy    kernel.ActorRef
	ChangedAt    time.Time
}

// RevisionView is one business revision. Data is the draft as saved (for a frozen revision, as it
// was frozen; ExpiresAt in kernel.FormatTimestamp form). A frozen revision has its business hash,
// its issued_at and expires_at, who froze it, and Commercial: the normalized canonical commercial
// document, byte for byte what the business hash covers (Python protocol.business_hash). A
// discarded revision has who ended it, why and on which evidence.
type RevisionView struct {
	Revision       int64
	Status         string
	BaseRevision   int64 // 0: none
	ContentHash    string
	Data           QuoteData
	Contributors   []string
	SavedBy        kernel.ActorRef
	SavedAt        time.Time
	BusinessHash   string
	IssuedAt       time.Time
	ExpiresAt      time.Time
	FrozenBy       kernel.ActorRef
	FreezeEpoch    int64
	FrozenAt       time.Time
	Commercial     string
	Calculation    *CalculationView // frozen: the server calculation Commercial seals
	DiscardedBy    kernel.ActorRef
	DiscardedAt    time.Time
	DiscardReason  string
	DiscardProofs  []Evidence
	AuthorityEpoch int64        // of the last save
	Formal         *FormalView  // active, superseded or cancelled: its formal document and commit evidence
	Closed         *ClosureView // withdrawn or cancelled
}

// CalculationView is the server calculation a frozen revision seals (its commercial document's
// calculation): amounts in minor units of the quote currency as integer strings (the comparison
// amounts in the comparison currency's), the margin verdict with the exact minimum margin and
// shortfall (major units of the comparison currency; "0" when the margin passes), the pricing
// policy revision and calculation version it was computed under, and the payment plan.
type CalculationView struct {
	NetMinor               string
	TaxMinor               string
	PayableMinor           string
	ComparisonCurrencyRef  ConfigRef
	SalesFXRef             *ConfigRef
	ComparisonRevenueMinor string
	CostMinor              string
	ProfitMinor            string
	MinimumMargin          string
	MarginRule             string // pass or requires_approval
	MarginShortfall        string
	PricingPolicyRef       ConfigRef
	CalculationVersion     string
	Payments               []PaymentView
}

// PaymentView is one stage of a payment plan.
type PaymentView struct {
	StageID     string
	Percent     string
	AmountMinor string
}
