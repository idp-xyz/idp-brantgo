// Package scenario is the CostScenario aggregate (tactical model CST-AGG-CostScenario, T02 §7,
// §13.3, SM-CST-SCENARIO): a selection of supply for the demand lines of one verified costing
// context revision, identified by (tenant, company, scenario_id), and the immutable evaluations
// computed from it.
//
//	draft → draft    cost_scenario.save_draft replaces the selection whole (root version + 1)
//	draft → draft    cost_scenario.evaluate appends an evaluation; the root version stays
//	draft → frozen   cost_snapshot.freeze (the next step of S2-05)
//
// A scenario names the costing context revision (context_ref), the supply condition line each
// demand line is bought under and in what quantity (allocations: SupplyConditionRef v1 of a
// supplier offer or purchase terms revision and its condition line), the logistics offer
// revisions whose charges it adds (logistics_offer_refs), the cost_estimate exchange rates it
// adopts (fx_refs) and why this selection was made (selection_reason). The other inputs of the
// evaluation follow from those: the POL configurations the context and the supply conditions
// cite, the measurements the charges are measured on, the RFQ the context bound.
//
// The evaluation (Evaluate) composes the calculation rules of package rules, delivery and budget
// in the order of Python V0.50 integrated_model._cost on inputs the application read from CST, INQ,
// CAT, POL and SUP before CST's transaction. Its result is a pure function of the normalized input:
// the scenario's normalized selection and every input revision with its content hash. Neither the
// instant of the evaluation nor its identity is part of the result, so evaluating the same input
// again yields the same bytes and the same hashes.
package scenario

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Status of a scenario.
type Status string

const (
	Draft  Status = "draft"
	Frozen Status = "frozen"
)

// Kinds of the references a scenario carries.
const (
	KindCostingContext = "costing_context"
	KindSupplierOffer  = "supplier_offer"
	KindPurchaseTerms  = "purchase_terms"
	KindLogisticsOffer = "logistics_offer"
	KindFXRate         = "fx_rate"
	KindUnit           = "unit"
	KindMeasurement    = "measurement"
)

// Ref is a typed reference to an exact revision: a POL configuration or a SUP supply condition.
type Ref struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
}

func (r Ref) String() string { return r.Kind + ":" + r.ID + "@" + strconv.FormatInt(r.Revision, 10) }

func compareRef(a, b Ref) int {
	return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.ID, b.ID), cmp.Compare(a.Revision, b.Revision))
}

// ContextRef is the costing context revision a scenario costs.
type ContextRef struct {
	ContextID string `json:"context_id"`
	Revision  int64  `json:"revision"`
}

// LineRef names one condition line of a supply condition revision (SupplyConditionRef v1 and the
// line's ID).
type LineRef struct {
	Kind            string `json:"kind"` // supplier_offer or purchase_terms
	ID              string `json:"id"`
	Revision        int64  `json:"revision"`
	ConditionLineID string `json:"condition_line_id"`
}

// Supply returns the supply condition revision the line belongs to.
func (l LineRef) Supply() Ref { return Ref{Kind: l.Kind, ID: l.ID, Revision: l.Revision} }

// Allocation buys PurchaseQuantity of demand line RfqLineID, in the POL unit PurchaseUnit, under
// condition line Source.
type Allocation struct {
	RfqLineID        string  `json:"rfq_line_id"`
	PurchaseQuantity string  `json:"purchase_quantity"`
	PurchaseUnit     Ref     `json:"purchase_unit_ref"`
	Source           LineRef `json:"source"`
}

// Data is the content of a scenario draft, stored as this JSON document. A draft may lack any
// field; an evaluation needs Context, at least one allocation and SelectionReason. Absent lists of
// logistics offers and exchange rates select none.
type Data struct {
	Context         *ContextRef  `json:"context_ref,omitempty"`
	Allocations     []Allocation `json:"allocations,omitempty"`
	LogisticsOffers []Ref        `json:"logistics_offer_refs,omitempty"`
	FX              []Ref        `json:"fx_refs,omitempty"`
	SelectionReason string       `json:"selection_reason,omitempty"`
}

// Hash returns the content hash of the draft as saved: the SHA-256 of its canonical JSON.
func (d Data) Hash() string { return hashOf(d) }

// Complete refuses an evaluation of a draft that lacks what a calculation needs (Python
// cost_scenario.evaluate: the scenario schema, COST_INPUT_NOT_READY).
func (d Data) Complete() error {
	const p = "/args/data"
	switch {
	case d.Context == nil:
		return fail(CodeCostInputNotReady, p+"/context_ref", "The scenario names no costing context revision")
	case len(d.Allocations) == 0:
		return fail(CodeCostInputNotReady, p+"/allocations", "The scenario allocates no demand line to a supply condition")
	case d.SelectionReason == "":
		return fail(CodeCostInputNotReady, p+"/selection_reason", "The scenario states no reason for its selection")
	}
	return nil
}

// Supplies returns the distinct supply condition revisions the allocations draw on, sorted.
func (d Data) Supplies() []Ref {
	var out []Ref
	for _, a := range d.Allocations {
		out = append(out, a.Source.Supply())
	}
	slices.SortFunc(out, compareRef)
	return slices.Compact(out)
}

// Normalized returns d as the evaluation's input binds it (Python normalization 0.2.0): lists in a
// stable order (allocations by demand line, then canonically; references by kind, ID and
// revision) and decimals without trailing fractional zeros, so the same business selection
// spelled differently normalizes equally. Text is kept as given.
func (d Data) Normalized() Data {
	out := Data{Context: d.Context, SelectionReason: d.SelectionReason}
	for _, a := range d.Allocations {
		a.PurchaseQuantity = trimDecimal(a.PurchaseQuantity)
		out.Allocations = append(out.Allocations, a)
	}
	slices.SortFunc(out.Allocations, func(a, b Allocation) int {
		return cmp.Or(cmp.Compare(a.RfqLineID, b.RfqLineID), cmp.Compare(string(canonical(a)), string(canonical(b))))
	})
	out.LogisticsOffers = slices.SortedFunc(slices.Values(d.LogisticsOffers), compareRef)
	out.FX = slices.SortedFunc(slices.Values(d.FX), compareRef)
	return out
}

// trimDecimal removes trailing fractional zeros: "120.000000" is "120", "0.50" is "0.5".
func trimDecimal(s string) string {
	whole, frac, ok := cutPoint(s)
	if !ok {
		return s
	}
	for len(frac) > 0 && frac[len(frac)-1] == '0' {
		frac = frac[:len(frac)-1]
	}
	if frac == "" {
		return whole
	}
	return whole + "." + frac
}

func cutPoint(s string) (string, string, bool) {
	for i := range len(s) {
		if s[i] == '.' {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

// Scenario is the aggregate root.
type Scenario struct {
	ID             string
	RootVersion    int64
	Status         Status
	Data           Data
	Hash           string
	Contributors   []string
	SavedBy        kernel.ActorRef
	SavedEpoch     int64
	SavedAt        time.Time
	LastEvaluation int64 // the number of the latest evaluation, 0 for none
	OwnerActor     string
	OwnerTeam      string
}

// New returns an unsaved scenario the actor will own.
func New(id, ownerActor, ownerTeam string) *Scenario {
	return &Scenario{ID: id, Status: Draft, OwnerActor: ownerActor, OwnerTeam: ownerTeam}
}

// CheckSave admits a whole-draft save (Python cost_scenario.save_draft): a missing scenario only at
// expected version 0; an existing one visible, at its current root version and still a draft.
func CheckSave(s *Scenario, visible bool, expected int64) error {
	if s == nil {
		if expected != 0 {
			return kernel.Reject(kernel.CodeNotFound, "/args/id", "")
		}
		return nil
	}
	return Check(s, visible, expected)
}

// Check admits a command on an existing, visible draft scenario at its current root version
// (Python _admit_write: visibility, CAS, then state).
func Check(s *Scenario, visible bool, expected int64) error {
	switch {
	case s == nil || !visible:
		return kernel.Reject(kernel.CodeNotFound, "/args/id", "")
	case s.RootVersion != expected:
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	case s.Status != Draft:
		return kernel.Reject(kernel.CodeInvalidState, "/args/id", "A frozen scenario does not change")
	}
	return nil
}

// Save replaces the draft whole and records by as a contributor; the root version grows by one.
func (s *Scenario) Save(d Data, by kernel.ActorRef, epoch int64, now time.Time) {
	s.Data, s.Hash = d, d.Hash()
	if !slices.Contains(s.Contributors, by.ID) {
		s.Contributors = append(s.Contributors, by.ID)
	}
	s.SavedBy, s.SavedEpoch, s.SavedAt = by, epoch, kernel.Instant(now)
	s.RootVersion++
}

// Append records that an evaluation of the current draft was computed and returns its number. The
// root version does not change (SM-CST-SCENARIO: evaluate appends an immutable result).
func (s *Scenario) Append() int64 {
	s.LastEvaluation++
	return s.LastEvaluation
}

// Evaluation is one stored evaluation of a scenario: who computed it when, on which scenario
// version, and the immutable documents (normalized input and result) with their hashes.
type Evaluation struct {
	ID                   string
	ScenarioID           string
	Number               int64
	ScenarioVersion      int64
	EvaluatedBy          kernel.ActorRef
	Epoch                int64
	EvaluatedAt          time.Time
	EngineVersion        string
	NormalizationVersion string
	Input                []byte // canonical JSON of NormalizedInput
	InputHash            string
	Document             []byte // canonical JSON of Result
	ResultHash           string
	Result               Result
	Inputs               []BoundInput
}

func hashOf(v any) string {
	sum := sha256.Sum256(canonical(v))
	return hex.EncodeToString(sum[:])
}

// HashBytes returns the SHA-256 of b in lower-case hex.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func fail(code, path, reason string, more ...string) error {
	make1 := func(p string) *kernel.Rejection {
		if c, ok := Categories[code]; ok {
			return kernel.RejectAs(code, c, p, reason)
		}
		return kernel.Reject(code, p, reason)
	}
	r := make1(path)
	for _, p := range more {
		r.Fields = append(r.Fields, make1(p).Fields[0])
	}
	return r
}

func index(path string, i int) string { return fmt.Sprintf("%s/%d", path, i) }
