// Package api is the contract of the CRM bounded context (optional leads, opportunities and
// follow-ups) for transports and for INQ. It exposes no CRM domain type. CRM is conditional
// (S6-D04, BC-AT24): a process composed without it never imports a value of this package into INQ.
package api

import (
	"context"
	"time"
	"unicode/utf8"

	"go.idp.xyz/idp-brant/internal/kernel"
)

const (
	// Contract names the request and response shapes of CRM commands.
	Contract = "brant.crm/0.1.0"
	// Source identifies CRM as the producer of integration events.
	Source = "idp-brant.crm"

	EventOpportunityAdvanced = "idp-brant.crm.opportunity_advanced.v1"

	CommandCaptureLead        = "lead.capture"
	CommandAdvanceOpportunity = "opportunity.advance"
	CommandRecordFollowUp     = "follow_up.record"

	// StatusCaptured is the only status of a lead (SM has no lead machine; the tactical note is
	// append-only).
	StatusCaptured = "captured"

	// Stages of SM-CRM-OPPORTUNITY. Qualified is the initial state.
	StageQualified   = "qualified"
	StageProposal    = "proposal"
	StageNegotiation = "negotiation"
	StageWon         = "won"
	StageLost        = "lost"
	StageAbandoned   = "abandoned"

	// CodeBusinessKeyConflict is the catalog code for an id this scope already holds (409).
	CodeBusinessKeyConflict = "BUSINESS_KEY_CONFLICT"
)

// Stages is every stage SM-CRM-OPPORTUNITY names, in layout order.
var Stages = []string{StageQualified, StageProposal, StageNegotiation, StageWon, StageLost, StageAbandoned}

// ActiveStage reports whether stage can still be cited as a trade source (OpportunityRef.Active).
// Won stays active: winning does not create the quote or the order. Lost and abandoned do not.
func ActiveStage(stage string) bool {
	switch stage {
	case StageQualified, StageProposal, StageNegotiation, StageWon:
		return true
	}
	return false
}

// CaptureLead records a lead. expected_version is 0: a lead is appended, never revised (R1 has no
// assignment and no public pool).
type CaptureLead struct {
	IdempotencyKey  string
	LeadID          string
	ExpectedVersion int64
	SourceChannel   string
	CompanyName     string
	ContactName     string
	Summary         string
}

// AdvanceOpportunity creates an opportunity at expected_version 0 in stage qualified, or moves an
// existing one along SM-CRM-OPPORTUNITY. Create-only fields (lead, customer, title, amount) are
// refused on an advance. Amount, when set, is integer minor units of currency; CRM does not round.
type AdvanceOpportunity struct {
	IdempotencyKey      string
	OpportunityID       string
	ExpectedVersion     int64
	Stage               string `json:",omitempty"`
	LeadID              string `json:",omitempty"`
	CustomerID          string `json:",omitempty"`
	Title               string `json:",omitempty"`
	ExpectedAmountMinor int64  `json:",omitempty"`
	Currency            string `json:",omitempty"`
}

// RecordFollowUp appends a follow-up on an opportunity. It is not a BusinessTask and it does not
// change the stage. recorded_at is the server clock, not a request field.
type RecordFollowUp struct {
	IdempotencyKey  string
	OpportunityID   string
	ExpectedVersion int64
	FollowUpID      string
	Summary         string
	NextStep        string `json:",omitempty"`
}

// Validate checks CaptureLead.
func (c CaptureLead) Validate() error {
	if err := head(c.IdempotencyKey, c.LeadID, "/args/lead_id"); err != nil {
		return err
	}
	if c.ExpectedVersion != 0 {
		return schema("/args/expected_version")
	}
	switch {
	case !text(c.SourceChannel, 200):
		return schema("/args/source_channel")
	case !text(c.CompanyName, 200):
		return schema("/args/company_name")
	case !text(c.ContactName, 200):
		return schema("/args/contact_name")
	case !text(c.Summary, 200):
		return schema("/args/summary")
	}
	return nil
}

// Validate checks AdvanceOpportunity.
func (c AdvanceOpportunity) Validate() error {
	if err := head(c.IdempotencyKey, c.OpportunityID, "/args/opportunity_id"); err != nil {
		return err
	}
	if c.ExpectedVersion < 0 || c.ExpectedVersion > 2147483647 {
		return schema("/args/expected_version")
	}
	if c.Stage != "" && !knownStage(c.Stage) {
		return schema("/args/stage")
	}
	if c.ExpectedVersion == 0 {
		// Create starts at qualified. The stage is omitted; naming one is not an advance.
		if c.Stage != "" {
			return schema("/args/stage")
		}
		if !text(c.Title, 200) {
			return schema("/args/title")
		}
		if c.LeadID != "" && !kernel.ValidIdentifier(c.LeadID) {
			return schema("/args/lead_id")
		}
		if c.CustomerID != "" && !kernel.ValidIdentifier(c.CustomerID) {
			return schema("/args/customer_id")
		}
		return amount(c.ExpectedAmountMinor, c.Currency)
	}
	if c.Stage == "" {
		return schema("/args/stage")
	}
	switch {
	case c.Title != "":
		return schema("/args/title")
	case c.LeadID != "":
		return schema("/args/lead_id")
	case c.CustomerID != "":
		return schema("/args/customer_id")
	case c.ExpectedAmountMinor != 0 || c.Currency != "":
		return schema("/args/expected_amount_minor")
	}
	return nil
}

// Validate checks RecordFollowUp.
func (c RecordFollowUp) Validate() error {
	if err := head(c.IdempotencyKey, c.OpportunityID, "/args/opportunity_id"); err != nil {
		return err
	}
	if c.ExpectedVersion < 1 || c.ExpectedVersion > 2147483647 {
		return schema("/args/expected_version")
	}
	if !kernel.ValidIdentifier(c.FollowUpID) {
		return schema("/args/follow_up_id")
	}
	if !text(c.Summary, 200) {
		return schema("/args/summary")
	}
	if c.NextStep != "" && !text(c.NextStep, 200) {
		return schema("/args/next_step")
	}
	return nil
}

func amount(minor int64, currency string) error {
	if minor == 0 && currency == "" {
		return nil
	}
	if minor <= 0 || !validCurrency(currency) {
		return schema("/args/expected_amount_minor")
	}
	return nil
}

func validCurrency(s string) bool {
	if len(s) != 3 {
		return false
	}
	for i := 0; i < 3; i++ {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	return true
}

func knownStage(s string) bool {
	for _, stage := range Stages {
		if s == stage {
			return true
		}
	}
	return false
}

func head(key, id, idPath string) error {
	switch {
	case !kernel.ValidIdentifier(key):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(id):
		return schema(idPath)
	}
	return nil
}

func schema(path string) error { return kernel.Reject(kernel.CodeSchemaInvalid, path, "") }

func text(s string, max int) bool {
	n := utf8.RuneCountInString(s)
	return n >= 1 && n <= max && utf8.ValidString(s)
}

// Duplicate returns BUSINESS_KEY_CONFLICT at path (409).
func Duplicate(path string) *kernel.Rejection {
	return kernel.RejectAs(CodeBusinessKeyConflict, kernel.CategoryConflict, path, "")
}

// Receipt is the closed success answer of a command.
type Receipt struct {
	ActionID   string
	ObjectID   string
	RowVersion int64
	Status     string
	Replayed   bool
}

// Commands is CRM's command entry point.
type Commands interface {
	CaptureLead(ctx context.Context, cmd CaptureLead) (Receipt, error)
	AdvanceOpportunity(ctx context.Context, cmd AdvanceOpportunity) (Receipt, error)
	RecordFollowUp(ctx context.Context, cmd RecordFollowUp) (Receipt, error)
}

// Command is one of CRM's commands.
type Command interface{ crmCommand() }

func (CaptureLead) crmCommand()        {}
func (AdvanceOpportunity) crmCommand() {}
func (RecordFollowUp) crmCommand()     {}

// Reconciliation is what became of a command whose answer the caller lost.
type Reconciliation struct {
	State    kernel.ReconcileState
	ActionID string
	ObjectID string
	Receipt  *Receipt
}

// Reconciler recovers from an unknown outcome without executing anything.
type Reconciler interface {
	Reconcile(ctx context.Context, cmd Command) (Reconciliation, error)
}

// Auditor records a request an inbound adapter refused before it became a command.
type Auditor interface {
	AuditRejected(ctx context.Context, command, objectID, code string) error
}

// LeadView is a lead as its reader sees it.
type LeadView struct {
	LeadID        string
	SourceChannel string
	CompanyName   string
	ContactName   string
	Summary       string
	Status        string
	RowVersion    int64
}

// FollowUpView is one follow-up. RecordedAt is UTC, truncated to microseconds.
type FollowUpView struct {
	FollowUpID string
	Summary    string
	NextStep   string
	RecordedAt time.Time
}

// OpportunityView is an opportunity and its follow-ups, in record order.
type OpportunityView struct {
	OpportunityID       string
	LeadID              string
	CustomerID          string
	Title               string
	Stage               string
	ExpectedAmountMinor int64
	Currency            string
	RowVersion          int64
	FollowUps           []FollowUpView
}

// Queries is CRM's audited read side. These reads are in-process: the context map registers no
// CRM GET route, so the transport does not either.
type Queries interface {
	GetLead(ctx context.Context, leadID string) (LeadView, error)
	GetOpportunity(ctx context.Context, opportunityID string) (OpportunityView, error)
	Audit(ctx context.Context) ([]kernel.AuditEntry, error)
}

// OpportunityRef is OpportunityRef v1 (Context Map R40, CRM → INQ). Active is false for lost and
// abandoned. An empty OpportunityID means the id is not visible in the caller's scope.
type OpportunityRef struct {
	OpportunityID string
	CustomerID    string
	Stage         string
	Active        bool
}

// OpportunityLookups is the lock-free read INQ calls before its own transaction when CRM is
// composed. A nil lookup means CRM is off (DEPENDENCY_NOT_SUPPORTED when an id is supplied).
type OpportunityLookups interface {
	ResolveOpportunity(ctx context.Context, opportunityID string) (OpportunityRef, error)
}
