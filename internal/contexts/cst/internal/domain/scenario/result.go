package scenario

import (
	"bytes"
	"encoding/json"
	"fmt"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Versions of the evaluation. EngineVersion names this composition of the calculation rules;
// NormalizationVersion the shape of the normalized input. A change of either changes the hashes,
// and a freeze compares both (CST-INV-001: 规范化输入与引擎版本相等).
const (
	EngineVersion        = "brant.cst.evaluation/0.1.0"
	NormalizationVersion = "brant.cst.normalization/0.1.0"
)

// Codes of the CST error catalog rows this package decides; package api declares the same.
const (
	CodeCostInputNotReady        = "COST_INPUT_NOT_READY"
	CodeComparisonBasisMismatch  = "COMPARISON_BASIS_MISMATCH"
	CodeDependencyNotSupported   = "DEPENDENCY_NOT_SUPPORTED"
	CodeCalculationLimitExceeded = "CALCULATION_LIMIT_EXCEEDED"
	CodeEvidenceHashMismatch     = "EVIDENCE_HASH_MISMATCH"
	CodeReferenceStale           = "REFERENCE_STALE"
)

// Categories maps the codes above to their error catalog category.
var Categories = map[string]kernel.Category{
	CodeCostInputNotReady:        kernel.CategoryInvalid,
	CodeComparisonBasisMismatch:  kernel.CategoryInvalid,
	CodeDependencyNotSupported:   kernel.CategoryInvalid,
	CodeCalculationLimitExceeded: kernel.CategoryInvalid,
	CodeEvidenceHashMismatch:     kernel.CategoryInvalid,
	CodeReferenceStale:           kernel.CategoryConflict,
}

// Outcomes of an evaluation (T02 §7, CST-INV-001). A verified result rests on verified inputs only;
// a provisional one carries an unverified assumption, a what_if context or a delivery that could
// not be resolved to a date. A comparable result is on the comparison basis; a not_comparable one
// stays analyzable and lists its differences. Only a verified and comparable result is frozen.
const (
	Verified      = "verified"
	Provisional   = "provisional"
	Comparable    = "comparable"
	NotComparable = "not_comparable"
)

// Upstream contexts of the inputs an evaluation binds.
const (
	FromCST = "cst"
	FromINQ = "inq"
	FromCAT = "cat"
	FromPOL = "pol"
	FromSUP = "sup"
	FromEVD = "evd"
)

// KindSource is the kind of an EVD source an evaluation binds.
const KindSource = "source"

// Uses of a bound input. An input is in force for the evaluation and is re-guarded by a freeze;
// an anchor is the verified event a delivery counts from, whose standing is its verification;
// provenance is the source offer of independent purchase terms, which is history only.
const (
	UseInput      = "input"
	UseAnchor     = "anchor"
	UseProvenance = "provenance"
	UseEvidence   = "evidence" // an EVD source an input's content or verification cites
)

// BoundInput is one revision an evaluation read: its upstream context, kind, identity, revision
// (or row version) and the content hash its owner gave (the RFQ's line set hash for an RFQ; empty
// for a CAT specification revision, which never changes).
type BoundInput struct {
	Context     string `json:"context"`
	Kind        string `json:"kind"`
	ID          string `json:"id"`
	Revision    int64  `json:"revision"`
	ContentHash string `json:"content_hash"`
	Use         string `json:"use"`
}

func compareInput(a, b BoundInput) int {
	switch {
	case a.Context != b.Context:
		return compareText(a.Context, b.Context)
	case a.Kind != b.Kind:
		return compareText(a.Kind, b.Kind)
	case a.ID != b.ID:
		return compareText(a.ID, b.ID)
	case a.Revision != b.Revision:
		if a.Revision < b.Revision {
			return -1
		}
		return 1
	}
	return compareText(a.Use, b.Use)
}

func compareText(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// NormalizedInput is what an evaluation is a function of (Python binding: the normalized scenario
// and the input manifest): the scenario's identity and normalized selection, and every input
// revision with its content hash, sorted. The scenario version is not part of it: re-saving the
// same selection changes the version, not the input.
type NormalizedInput struct {
	NormalizationVersion string       `json:"normalization_version"`
	ScenarioID           string       `json:"scenario_id"`
	Data                 Data         `json:"data"`
	Inputs               []BoundInput `json:"inputs"`
}

// Result is the stored evaluation result (Python result of cost_scenario.evaluate without its
// identity and calculation time). Amounts are integer minor units as strings.
type Result struct {
	EngineVersion          string             `json:"engine_version"`
	InputHash              string             `json:"input_hash"`
	ScenarioID             string             `json:"scenario_id"`
	ContextRef             ContextRef         `json:"context_ref"`
	ContextKind            string             `json:"context_kind"`
	Rfq                    RfqBinding         `json:"rfq"`
	Currency               string             `json:"currency"`
	MinorUnits             int                `json:"minor_units"`
	VerificationStatus     string             `json:"verification_status"`
	ComparisonStatus       string             `json:"comparison_status"`
	TotalMinor             string             `json:"total_minor"`
	Components             []Component        `json:"components"`
	ExcludedComponents     []Excluded         `json:"excluded_components"`
	Differences            []Difference       `json:"differences"`
	DeliveryChecks         []DeliveryCheck    `json:"delivery_checks"`
	CommercialChecks       []CommercialCheck  `json:"commercial_checks"`
	Assumptions            []BoundAssumption  `json:"assumptions"`
	CostBasis              string             `json:"cost_basis"`
	ExcludedCostCategories []string           `json:"excluded_cost_categories"`
	Budget                 BudgetUsage        `json:"budget"`
	SelectionReason        string             `json:"selection_reason"`
	Lines                  []LineTotal        `json:"lines"`
	Purchases              []PurchaseQuantity `json:"purchases"`
	Charges                []ChargeObligation `json:"charges"`
}

// RfqBinding is the RfqRef v1 the costing context bound.
type RfqBinding struct {
	RfqID      string `json:"rfq_id"`
	RowVersion int64  `json:"row_version"`
	LinesHash  string `json:"lines_hash"`
}

// Component is a purchase component or a payable charge, costed (T02 §13.3): the exact amount in
// its own currency, its tax split there (minor units), the cost after recovery, the exchange rate
// and the amount in the calculation currency, allocated to lines.
type Component struct {
	ComponentID    string       `json:"id"`
	Kind           string       `json:"kind"` // purchase or charge
	Source         Ref          `json:"source"`
	SourceLineID   string       `json:"source_line_id"` // condition line or fee ID
	ObligationID   string       `json:"obligation_id"`
	Quantity       string       `json:"quantity"`
	Unit           string       `json:"unit"`
	Currency       string       `json:"currency"`
	RawAmount      string       `json:"raw_amount"`
	NetMinor       string       `json:"net_minor"`
	TaxMinor       string       `json:"tax_minor"`
	GrossMinor     string       `json:"gross_minor"`
	RecoveredMinor string       `json:"recovered_minor"`
	CostMinor      string       `json:"cost_minor"`
	TaxProfile     Ref          `json:"tax_profile_ref"`
	FXRate         string       `json:"fx_rate_ref,omitempty"`
	AmountMinor    string       `json:"amount_minor"`
	Allocations    []LineAmount `json:"allocations"`
}

// LineAmount is what one demand line receives of a component (CST-INV-004): its integer weight
// and its amount in minor units.
type LineAmount struct {
	LineID      string `json:"line_id"`
	Basis       string `json:"basis"`
	BaseInteger string `json:"base_integer"`
	AmountMinor string `json:"amount_minor"`
}

// Excluded is a charge that is shown but not added: included in another component or borne by the
// customer.
type Excluded struct {
	ComponentID  string `json:"id"`
	Source       Ref    `json:"source"`
	SourceLineID string `json:"source_line_id"`
	ObligationID string `json:"obligation_id"`
	IncludedIn   string `json:"included_in_component_ref,omitempty"`
	ReasonCode   string `json:"reason_code"` // included or customer_borne
	Reason       string `json:"reason"`
}

// Difference is a finding that keeps the result from being comparable or verified without
// refusing it.
type Difference struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// DeliveryCheck is a delivery rule as the evaluation resolved it: a date, or unresolved with the
// reason no date was assumed (T02 §15.3).
type DeliveryCheck struct {
	Path      string `json:"path"`
	Role      string `json:"role"` // requirement or supply
	Milestone string `json:"milestone"`
	Place     string `json:"place"`
	TimeZone  string `json:"time_zone"`
	Status    string `json:"status"` // resolved or unresolved
	Date      string `json:"date,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// CommercialCheck records how a commercial term was reconciled for nominal direct cost (T02
// §16.1): matched, normalized, different_cashflow, customer_borne or mismatch.
type CommercialCheck struct {
	Path     string   `json:"path"`
	Category string   `json:"category"`
	Status   string   `json:"status"`
	Reason   string   `json:"reason"`
	Refs     []string `json:"refs"`
}

// BoundAssumption is an assumption of the costing context as the evaluation bound it: always
// unverified (T02 §15.2).
type BoundAssumption struct {
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

// LineTotal is one demand line's cost: the sum of its shares of every component.
type LineTotal struct {
	LineID      string `json:"line_id"`
	Quantity    string `json:"quantity"`
	Unit        string `json:"unit"`
	AmountMinor string `json:"amount_minor"`
}

// PurchaseQuantity is what the scenario buys under one condition line after aggregation.
type PurchaseQuantity struct {
	ComponentID string `json:"component_id"`
	Source      Ref    `json:"source"`
	LineID      string `json:"condition_line_id"`
	Quantity    string `json:"quantity"`
	Unit        string `json:"unit"`
}

// ChargeObligation is how one component counts in the charge graph (CST-INV-003).
type ChargeObligation struct {
	ComponentID  string `json:"component_id"`
	ObligationID string `json:"obligation_id"`
	Status       string `json:"status"` // payable, included or customer_borne
	IncludedIn   string `json:"included_in_component_ref,omitempty"`
}

// canonical returns the canonical JSON of v: object members in byte order of their names at every
// level, no insignificant whitespace, no HTML escaping. The documents hold only strings, integers,
// booleans, arrays and objects, so equal content always encodes to the same bytes.
func canonical(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("scenario: document does not encode: %v", err)) // only strings, integers and lists
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree any
	if err = dec.Decode(&tree); err != nil {
		panic(fmt.Sprintf("scenario: document does not decode: %v", err))
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err = enc.Encode(tree); err != nil {
		panic(fmt.Sprintf("scenario: document does not re-encode: %v", err))
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

// Canonical returns the canonical JSON of a document of this package.
func Canonical(v any) []byte { return canonical(v) }
