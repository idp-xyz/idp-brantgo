package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// A cost scenario (CostScenario, T02 §7, §13.3; SM-CST-SCENARIO) selects, for the demand lines of
// one verified costing context revision, the supply condition lines they are bought under
// (SupplyConditionRef v1 of a supplier offer or purchase terms revision), the logistics offer
// revisions whose charges it adds and the cost_estimate exchange rates it adopts. A draft is saved
// whole; cost_scenario.evaluate computes an immutable evaluation of the current draft without
// changing its root version; cost_snapshot.freeze (next step) freezes a verified and comparable
// evaluation once.

const (
	// EventCostScenarioEvaluated is the event catalog's CostScenarioEvaluated.
	EventCostScenarioEvaluated = "idp-brant.cst.cost_scenario_evaluated.v1"

	CommandSaveScenario     = "cost_scenario.save_draft"
	CommandEvaluateScenario = "cost_scenario.evaluate"

	// Statuses of a scenario, and the receipt status of an evaluation.
	ScenarioDraft   = "draft"
	ScenarioFrozen  = "frozen"
	StatusEvaluated = "evaluated"

	// Kinds of the supply a scenario selects (SupplyConditionRef v1) and of its exchange rates.
	SupplySupplierOffer  = "supplier_offer"
	SupplyPurchaseTerms  = "purchase_terms"
	SupplyLogisticsOffer = "logistics_offer"
	KindFXRate           = "fx_rate"

	// Outcomes of an evaluation (CST-INV-001): only a verified and comparable one may be frozen.
	VerificationVerified    = "verified"
	VerificationProvisional = "provisional"
	ComparisonComparable    = "comparable"
	ComparisonNotComparable = "not_comparable"

	// Upstream contexts and uses of the inputs an evaluation binds (BoundInput).
	InputFromCST  = "cst"
	InputFromINQ  = "inq"
	InputFromCAT  = "cat"
	InputFromPOL  = "pol"
	InputFromSUP  = "sup"
	UseInput      = "input"      // in force for the evaluation; a freeze re-guards it
	UseAnchor     = "anchor"     // the verified event a delivery counts from
	UseProvenance = "provenance" // the source offer of independent purchase terms, history only

	MaxAllocations     = 200
	MaxLogisticsOffers = 200
	MaxFXRefs          = 200
)

// SupplyRef is SupplyConditionRef v1: a supply condition kind (supplier_offer, purchase_terms or
// logistics_offer), its ID and the exact revision.
type SupplyRef struct {
	Kind     string
	ID       string
	Revision int64
}

// SupplyLineRef is a condition line of a supplier offer or purchase terms revision.
type SupplyLineRef struct {
	Kind            string // supplier_offer or purchase_terms
	ID              string
	Revision        int64
	ConditionLineID string
}

// ScenarioAllocation buys PurchaseQuantity (a decimal string, never a float) of demand line
// RfqLineID in the POL unit PurchaseUnitRef under condition line Source.
type ScenarioAllocation struct {
	RfqLineID        string
	PurchaseQuantity string
	PurchaseUnitRef  ConfigRef
	Source           SupplyLineRef
}

// ScenarioData is the content of a scenario draft. A draft may lack any field; an evaluation needs
// ContextRef, at least one allocation and SelectionReason. Omitted LogisticsOfferRefs and FXRefs
// select none (a charge the comparison basis requires is then missing, never zero).
type ScenarioData struct {
	ContextRef         *ContextRef
	Allocations        []ScenarioAllocation
	LogisticsOfferRefs []SupplyRef // kind logistics_offer
	FXRefs             []ConfigRef // POL fx_rate revisions
	SelectionReason    string
}

// SaveScenario creates a scenario at ExpectedVersion 0 or replaces its draft whole at the current
// root version (Python cost_scenario.save_draft). Every reference it states must be known to its
// owner and visible to the caller; whether it is in force is judged when the draft is evaluated.
type SaveScenario struct {
	IdempotencyKey  string
	ScenarioID      string
	ExpectedVersion int64
	Data            ScenarioData
}

// EvaluateScenario evaluates the current draft at ExpectedVersion (Python cost_scenario.evaluate).
// It appends an immutable evaluation and leaves the root version as it is.
type EvaluateScenario struct {
	IdempotencyKey  string
	ScenarioID      string
	ExpectedVersion int64
}

func (SaveScenario) cstCommand()     {}
func (EvaluateScenario) cstCommand() {}

// Validate checks the closed contract (Python commands.schema.json, cost_scenario.save_draft).
func (c SaveScenario) Validate() error {
	if err := head(c.IdempotencyKey, c.ScenarioID, c.ExpectedVersion, 0); err != nil {
		return err
	}
	return c.Data.validate("/args/data")
}

// Validate checks the closed contract (Python commands.schema.json, cost_scenario.evaluate).
func (c EvaluateScenario) Validate() error {
	return head(c.IdempotencyKey, c.ScenarioID, c.ExpectedVersion, 1)
}

func (d ScenarioData) validate(p string) error {
	if r := d.ContextRef; r != nil {
		if !kernel.ValidIdentifier(r.ContextID) {
			return schema(p + "/context_ref/context_id")
		}
		if err := revision(r.Revision, p+"/context_ref/revision"); err != nil {
			return err
		}
	}
	if d.Allocations != nil && (len(d.Allocations) < 1 || len(d.Allocations) > MaxAllocations) {
		return schema(p + "/allocations")
	}
	for i, a := range d.Allocations {
		ap := index(p+"/allocations", i)
		switch {
		case !kernel.ValidIdentifier(a.RfqLineID):
			return schema(ap + "/rfq_line_id")
		case !decimalQuantity(a.PurchaseQuantity):
			return schema(ap + "/purchase_quantity")
		}
		if err := a.PurchaseUnitRef.Validate(ap + "/purchase_unit_ref"); err != nil {
			return err
		}
		if a.PurchaseUnitRef.Kind != KindUnit {
			return schema(ap + "/purchase_unit_ref/kind")
		}
		s := a.Source
		switch {
		case s.Kind != SupplySupplierOffer && s.Kind != SupplyPurchaseTerms:
			return schema(ap + "/source/kind")
		case !kernel.ValidIdentifier(s.ID):
			return schema(ap + "/source/id")
		case !kernel.ValidIdentifier(s.ConditionLineID):
			return schema(ap + "/source/condition_line_id")
		}
		if err := revision(s.Revision, ap+"/source/revision"); err != nil {
			return err
		}
	}
	if len(d.LogisticsOfferRefs) > MaxLogisticsOffers {
		return schema(p + "/logistics_offer_refs")
	}
	for i, r := range d.LogisticsOfferRefs {
		rp := index(p+"/logistics_offer_refs", i)
		if r.Kind != SupplyLogisticsOffer {
			return schema(rp + "/kind")
		}
		if err := ref(r.Kind, r.ID, r.Revision, rp); err != nil {
			return err
		}
	}
	if len(d.FXRefs) > MaxFXRefs {
		return schema(p + "/fx_refs")
	}
	for i, r := range d.FXRefs {
		rp := index(p+"/fx_refs", i)
		if err := r.Validate(rp); err != nil {
			return err
		}
		if r.Kind != KindFXRate {
			return schema(rp + "/kind")
		}
	}
	if d.SelectionReason != "" && !text(d.SelectionReason, maxText) {
		return schema(p + "/selection_reason")
	}
	return nil
}

// decimalQuantity reports whether s is a decimal of the quantity format; its sign and its unit's
// precision are judged by the evaluation (INVALID_KNOWN_VALUE).
func decimalQuantity(s string) bool {
	_, err := kernel.ParseDecimal(s, kernel.QuantityFormat)
	return err == nil
}

// EvaluationReceipt is the answer of cost_scenario.evaluate: the command receipt (ObjectID is the
// evaluation ID, as Python's object_id is the result ID; RowVersion the scenario's root version,
// which evaluating leaves unchanged; Status "evaluated") and what the evaluation found.
type EvaluationReceipt struct {
	Receipt
	ScenarioID         string
	EvaluationID       string
	Number             int64
	ScenarioVersion    int64
	EngineVersion      string
	InputHash          string
	ResultHash         string
	VerificationStatus string
	ComparisonStatus   string
	Currency           string
	TotalMinor         string
}

// ScenarioCommands are the scenario commands of CST. Errors are as for Commands.
type ScenarioCommands interface {
	SaveScenario(ctx context.Context, cmd SaveScenario) (Receipt, error)
	EvaluateScenario(ctx context.Context, cmd EvaluateScenario) (EvaluationReceipt, error)
}

// EvaluationSummary is one evaluation of a scenario as its view lists it.
type EvaluationSummary struct {
	EvaluationID       string
	Number             int64
	ScenarioVersion    int64
	EvaluatedBy        kernel.ActorRef
	EvaluatedAt        time.Time
	InputHash          string
	ResultHash         string
	VerificationStatus string
	ComparisonStatus   string
	TotalMinor         string
}

// ScenarioView is a scenario with its draft and the evaluations computed from it, oldest first.
type ScenarioView struct {
	ScenarioID     string
	RootVersion    int64
	Status         string
	ContentHash    string
	Data           ScenarioData
	Contributors   []string
	SavedBy        kernel.ActorRef
	SavedAt        time.Time
	LastEvaluation int64
	Evaluations    []EvaluationSummary
}

// BoundInput is one revision an evaluation read: its upstream context (cst, inq, cat, pol, sup),
// kind, ID, revision or row version, the content hash its owner gave (an RFQ's line set hash;
// empty for a CAT specification revision, which never changes) and its use.
type BoundInput struct {
	Context     string `json:"context"`
	Kind        string `json:"kind"`
	ID          string `json:"id"`
	Revision    int64  `json:"revision"`
	ContentHash string `json:"content_hash"`
	Use         string `json:"use"`
}

// EvaluationView is one stored evaluation: who computed it when, on which scenario version, the
// engine and normalization versions, the canonical documents with their SHA-256 hashes (Input is
// the normalized input, Document the result), the result and every input it bound.
type EvaluationView struct {
	EvaluationID         string
	ScenarioID           string
	Number               int64
	ScenarioVersion      int64
	EvaluatedBy          kernel.ActorRef
	AuthorityEpoch       int64
	EvaluatedAt          time.Time
	EngineVersion        string
	NormalizationVersion string
	Input                string
	InputHash            string
	Document             string
	ResultHash           string
	Result               EvaluationResult
	Inputs               []BoundInput
}

// EvaluationResult is the result document of an evaluation. Amounts are integer minor units as
// decimal strings; the JSON names are those of the stored document.
type EvaluationResult struct {
	EngineVersion          string                `json:"engine_version"`
	InputHash              string                `json:"input_hash"`
	ScenarioID             string                `json:"scenario_id"`
	ContextRef             EvaluatedContext      `json:"context_ref"`
	ContextKind            string                `json:"context_kind"`
	Rfq                    EvaluatedRfq          `json:"rfq"`
	Currency               string                `json:"currency"`
	MinorUnits             int                   `json:"minor_units"`
	VerificationStatus     string                `json:"verification_status"`
	ComparisonStatus       string                `json:"comparison_status"`
	TotalMinor             string                `json:"total_minor"`
	Components             []CostComponent       `json:"components"`
	ExcludedComponents     []ExcludedCharge      `json:"excluded_components"`
	Differences            []Difference          `json:"differences"`
	DeliveryChecks         []DeliveryCheck       `json:"delivery_checks"`
	CommercialChecks       []CommercialCheck     `json:"commercial_checks"`
	Assumptions            []EvaluatedAssumption `json:"assumptions"`
	CostBasis              string                `json:"cost_basis"`
	ExcludedCostCategories []string              `json:"excluded_cost_categories"`
	Budget                 BudgetUsage           `json:"budget"`
	SelectionReason        string                `json:"selection_reason"`
	Lines                  []LineTotal           `json:"lines"`
	Purchases              []PurchaseLine        `json:"purchases"`
	Charges                []ChargeDecision      `json:"charges"`
}

// EvaluatedContext is the costing context revision an evaluation costed.
type EvaluatedContext struct {
	ContextID string `json:"context_id"`
	Revision  int64  `json:"revision"`
}

// EvaluatedRfq is the RfqRef v1 the costing context bound.
type EvaluatedRfq struct {
	RfqID      string `json:"rfq_id"`
	RowVersion int64  `json:"row_version"`
	LinesHash  string `json:"lines_hash"`
}

// CostComponent is a purchase component or a payable charge (T02 §13.3): its exact amount in its
// own currency, its tax split there in minor units, the cost after recovery, the exchange rate and
// the amount in the calculation currency, allocated to lines in integer minor units (CST-INV-004).
type CostComponent struct {
	ComponentID    string           `json:"id"`
	Kind           string           `json:"kind"` // purchase or charge
	Source         EvaluatedRef     `json:"source"`
	SourceLineID   string           `json:"source_line_id"`
	ObligationID   string           `json:"obligation_id"`
	Quantity       string           `json:"quantity"`
	Unit           string           `json:"unit"`
	Currency       string           `json:"currency"`
	RawAmount      string           `json:"raw_amount"`
	NetMinor       string           `json:"net_minor"`
	TaxMinor       string           `json:"tax_minor"`
	GrossMinor     string           `json:"gross_minor"`
	RecoveredMinor string           `json:"recovered_minor"`
	CostMinor      string           `json:"cost_minor"`
	TaxProfile     EvaluatedRef     `json:"tax_profile_ref"`
	FXRate         string           `json:"fx_rate_ref,omitempty"`
	AmountMinor    string           `json:"amount_minor"`
	Allocations    []LineAllocation `json:"allocations"`
}

// EvaluatedRef is a typed reference to an exact revision in a result.
type EvaluatedRef struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
}

// LineAllocation is what one demand line receives of a component: its integer weight and amount.
type LineAllocation struct {
	LineID      string `json:"line_id"`
	Basis       string `json:"basis"`
	BaseInteger string `json:"base_integer"`
	AmountMinor string `json:"amount_minor"`
}

// ExcludedCharge is a charge shown but not added: included in another component or borne by the
// customer.
type ExcludedCharge struct {
	ComponentID  string       `json:"id"`
	Source       EvaluatedRef `json:"source"`
	SourceLineID string       `json:"source_line_id"`
	ObligationID string       `json:"obligation_id"`
	IncludedIn   string       `json:"included_in_component_ref,omitempty"`
	ReasonCode   string       `json:"reason_code"`
	Reason       string       `json:"reason"`
}

// Difference is a finding that keeps a result from being comparable or verified without refusing
// it.
type Difference struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// DeliveryCheck is a delivery rule as the evaluation resolved it: a date, or unresolved with the
// reason no date was assumed.
type DeliveryCheck struct {
	Path      string `json:"path"`
	Role      string `json:"role"`
	Milestone string `json:"milestone"`
	Place     string `json:"place"`
	TimeZone  string `json:"time_zone"`
	Status    string `json:"status"`
	Date      string `json:"date,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// CommercialCheck records how a commercial term was reconciled for nominal direct cost.
type CommercialCheck struct {
	Path     string   `json:"path"`
	Category string   `json:"category"`
	Status   string   `json:"status"`
	Reason   string   `json:"reason"`
	Refs     []string `json:"refs"`
}

// EvaluatedAssumption is an assumption of the costing context as the evaluation bound it: always
// unverified, with its proposer (T02 §15.2).
type EvaluatedAssumption struct {
	ID                 string `json:"id"`
	Path               string `json:"path"`
	ValueText          string `json:"value_text"`
	Reason             string `json:"reason"`
	ProposedByKind     string `json:"proposed_by_kind"`
	ProposedByID       string `json:"proposed_by_id"`
	ProposedAt         string `json:"proposed_at"`
	VerificationStatus string `json:"verification_status"`
}

// BudgetUsage is what the calculation used of the resource budget (T02 §16.4).
type BudgetUsage struct {
	ExpandedNodes  int   `json:"expanded_nodes"`
	LoadedObjects  int   `json:"loaded_objects"`
	Operations     int   `json:"operations"`
	LoadedBytes    int64 `json:"loaded_bytes"`
	Components     int   `json:"components"`
	Packages       int   `json:"packages"`
	InclusionEdges int   `json:"inclusion_edges"`
}

// LineTotal is one demand line's cost.
type LineTotal struct {
	LineID      string `json:"line_id"`
	Quantity    string `json:"quantity"`
	Unit        string `json:"unit"`
	AmountMinor string `json:"amount_minor"`
}

// PurchaseLine is what the scenario buys under one condition line after aggregation.
type PurchaseLine struct {
	ComponentID string       `json:"component_id"`
	Source      EvaluatedRef `json:"source"`
	LineID      string       `json:"condition_line_id"`
	Quantity    string       `json:"quantity"`
	Unit        string       `json:"unit"`
}

// ChargeDecision is how one component counts in the charge graph (CST-INV-003).
type ChargeDecision struct {
	ComponentID  string `json:"component_id"`
	ObligationID string `json:"obligation_id"`
	Status       string `json:"status"` // payable, included or customer_borne
	IncludedIn   string `json:"included_in_component_ref,omitempty"`
}

// ScenarioQueries is CST's audited read side of scenarios.
type ScenarioQueries interface {
	// GetScenario returns a scenario the caller sees, audited as cost_scenario.get.
	GetScenario(ctx context.Context, scenarioID string) (ScenarioView, error)
	// GetEvaluation returns an evaluation of a scenario the caller sees, audited as
	// cost_evaluation.get. Every input it bound must still be readable by the caller (Python
	// _history_access): PERMISSION_DENIED otherwise.
	GetEvaluation(ctx context.Context, evaluationID string) (EvaluationView, error)
}

// BoundEvaluation is the latest evaluation of a scenario as a freeze reads it (CST-INV-001): the
// scenario version it was computed on, the engine and normalization versions, the input and result
// hashes, the outcome and every input it bound, so the freeze re-guards them (RfqGuard v1 for the
// RFQ, ConfigGuard v1 for each POL input, SupplyConditionGuard v1 for each SUP input, the CST
// context and measurement revisions FOR SHARE) and compares the hashes.
type BoundEvaluation struct {
	EvaluationID         string
	ScenarioID           string
	Number               int64
	ScenarioVersion      int64
	ScenarioStatus       string
	ScenarioRootVersion  int64
	EvaluatedAt          time.Time
	EngineVersion        string
	NormalizationVersion string
	InputHash            string
	ResultHash           string
	VerificationStatus   string
	ComparisonStatus     string
	Currency             string
	TotalMinor           string
	Context              ContextRef
	Rfq                  RfqBinding
	Inputs               []BoundInput // sorted by context, kind, ID, revision and use
}

// Freezable returns nil for a verified and comparable result computed on the scenario's current
// version (CST-INV-001), and otherwise COST_INPUT_NOT_READY at path/verification_status for a
// provisional result, COMPARISON_BASIS_MISMATCH at path/comparison_status for a result that is not
// comparable, and REFERENCE_STALE at path/scenario_version when the draft changed since.
func (b BoundEvaluation) Freezable(path string) error {
	switch {
	case b.VerificationStatus != VerificationVerified:
		return kernel.RejectAs(CodeCostInputNotReady, kernel.CategoryInvalid, path+"/verification_status",
			"A provisional result is not frozen")
	case b.ComparisonStatus != ComparisonComparable:
		return kernel.RejectAs(CodeComparisonBasisMismatch, kernel.CategoryInvalid, path+"/comparison_status",
			"A result that is not comparable is not frozen")
	case b.ScenarioVersion != b.ScenarioRootVersion:
		return Reject(CodeReferenceStale, path+"/scenario_version")
	}
	return nil
}

// EvaluationLookups gives the freeze what an evaluation bound. It needs no permission beyond an
// active grant and is not audited; a scenario outside the caller's data scope or without an
// evaluation is NOT_FOUND. Inside its own transaction the freeze reads the same rows through CST's
// store, after locking the scenario root FOR UPDATE.
type EvaluationLookups interface {
	LatestEvaluation(ctx context.Context, scenarioID string) (BoundEvaluation, error)
}
