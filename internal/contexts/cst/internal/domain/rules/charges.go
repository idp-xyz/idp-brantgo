package rules

import (
	"fmt"
	"slices"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Party is who bears a charge under the export sale: the exporter (seller) or the customer (buyer).
// It is read from the sales relationship, whatever purchase contract supplies the goods.
type Party string

const (
	Seller Party = "seller"
	Buyer  Party = "buyer"
)

// Obligation is a charge obligation of the reviewed export sale scope: which fee type it is and
// who bears it.
type Obligation struct {
	Path    string
	ID      string
	FeeType string
	Bearer  Party
}

// RequiredFee is a fee type the comparison basis requires the cost to account for.
type RequiredFee struct {
	Path    string
	FeeType string
}

// PurchaseCharge is a purchase component in the charge graph: one priced condition line
// (PurchaseGroup), its own obligation and the obligations its price includes (Includes, the
// condition's included_obligation_ids). SupplierBears lists the obligations the supplier bears
// under the reviewed purchase scope; each must be among Includes.
type PurchaseCharge struct {
	Path          string
	ComponentID   string
	ObligationID  string
	Includes      []string
	SupplierBears []string
}

// Fee is one charge of a logistics offer revision (Offer). ID is the fee's id inside the offer and
// ComponentID its component ID in the evaluation. IncludedIn is the included_in_component_ref as
// written: a fee ID of the same offer or a purchase component ID; empty when the fee stands alone.
type Fee struct {
	Path         string
	Offer        string
	ID           string
	ComponentID  string
	ObligationID string
	FeeType      string
	Party        Party
	IncludedIn   string
	Chargeable   ChargeableRule
	Billing      Billing
	Allocation   AllocationRule
}

// CheckFees applies the verify-time rules of one logistics offer's fees: IDs once; an obligation
// once, whatever ID carries it (CHARGE_DOUBLE_COUNT); a known party; each tariff with its
// chargeable rule and each allocation rule.
func CheckFees(fees []Fee) error {
	if len(fees) > MaxComponents {
		return fail(CodeCalculationLimitExceeded, fees[MaxComponents].Path, "An offer has at most 500 charges")
	}
	ids := make([]string, len(fees))
	obligations := make([]string, len(fees))
	for i, f := range fees {
		ids[i], obligations[i] = f.ID, f.ObligationID
	}
	if i := repeated(ids); i >= 0 {
		return fail(kernel.CodeDuplicateLineID, fees[i].Path+"/id", "")
	}
	if i := repeated(obligations); i >= 0 {
		return fail(CodeChargeDoubleCount, fees[i].Path+"/obligation_id", "The obligation is already charged under another ID")
	}
	for _, f := range fees {
		if f.Party != Seller && f.Party != Buyer {
			return fail(kernel.CodeSchemaInvalid, f.Path+"/responsible_party", "")
		}
		if err := f.Billing.Check(f.Chargeable); err != nil {
			return err
		}
		if err := f.Allocation.Check(); err != nil {
			return err
		}
	}
	return nil
}

// ChargeStatus is what a component counts for in the cost.
type ChargeStatus string

const (
	Payable       ChargeStatus = "payable"        // costed and allocated
	Included      ChargeStatus = "included"       // shown, but its amount is inside IncludedIn
	CustomerBorne ChargeStatus = "customer_borne" // excluded from the exporter's cost
)

// ChargeDecision is the status of one component; IncludedIn is the component that includes it.
type ChargeDecision struct {
	ComponentID  string
	Path         string
	ObligationID string
	Status       ChargeStatus
	IncludedIn   string
}

// ChargePlan says how each component counts. Differences keep the result not_comparable without
// refusing it; InclusionEdges counts toward the resource budget.
type ChargePlan struct {
	Decisions      []ChargeDecision // purchases, then fees, in the order given
	InclusionEdges int
	Differences    []Difference
}

// Decision returns the decision on a component.
func (p ChargePlan) Decision(componentID string) (ChargeDecision, bool) {
	for _, d := range p.Decisions {
		if d.ComponentID == componentID {
			return d, true
		}
	}
	return ChargeDecision{}, false
}

type chargeNode struct {
	id, obligation, parent, path string
	party                        Party
	fee                          *Fee
}

// PlanCharges applies CST-INV-003 to the purchase components and the fees of the selected logistics
// offers (in the order of their offer references, then fee IDs) against the sale scope's
// obligations and the fee types the comparison basis requires:
//
//   - one obligation is counted once: a second fee with the same obligation, whatever its ID, or a
//     fee repeating a purchase component's obligation is CHARGE_DOUBLE_COUNT;
//   - a fee whose obligation a purchase price includes, or that names an including component, is
//     included and not added again (a supplier price with freight included does not add the
//     freight fee); an inclusion parent that cannot be identified, two different parents, or a
//     fee included in a fee of the other bearer is CHARGE_SCOPE_UNRESOLVED; an inclusion cycle is
//     CHARGE_DOUBLE_COUNT, since a charge cannot remove its own cost; nothing is silently dropped;
//   - every obligation the seller bears is covered by a purchase price or a seller's fee, and
//     every required fee type is accounted for, else COST_INPUT_NOT_READY: a missing freight is
//     never taken as zero;
//   - customer-borne charges are excluded; one bundled into another price, or a customer-borne
//     obligation inside a purchase price, is a Difference, never a guessed deduction.
func PlanCharges(purchases []PurchaseCharge, fees []Fee, sale []Obligation, required []RequiredFee) (ChargePlan, error) {
	var plan ChargePlan
	nodes := map[string]*chargeNode{}
	var order []*chargeNode
	add := func(n *chargeNode) error {
		if len(order) == MaxComponents {
			return fail(CodeCalculationLimitExceeded, n.path, "An evaluation has at most 500 components")
		}
		if _, dup := nodes[n.id]; dup || n.id == "" {
			return fail(kernel.CodeDuplicateLineID, n.path, fmt.Sprintf("Component ID %q is missing or repeated", n.id))
		}
		nodes[n.id] = n
		order = append(order, n)
		return nil
	}

	included := map[string]string{} // obligation → purchase component whose price includes it
	for _, p := range purchases {
		if err := add(&chargeNode{id: p.ComponentID, obligation: p.ObligationID, path: p.Path, party: Seller}); err != nil {
			return ChargePlan{}, err
		}
		if err := unique(p.Includes, p.Path+"/included_obligation_ids"); err != nil {
			return ChargePlan{}, err
		}
		for i, o := range p.Includes {
			if _, twice := included[o]; twice {
				return ChargePlan{}, fail(CodeChargeScopeUnresolved, index(p.Path, "included_obligation_ids", i),
					fmt.Sprintf("Obligation %s is included in two purchase prices", o))
			}
			included[o] = p.ComponentID
		}
		for _, o := range p.SupplierBears {
			if !slices.Contains(p.Includes, o) {
				return ChargePlan{}, fail(CodeCostInputNotReady, p.Path+"/included_obligation_ids",
					fmt.Sprintf("The supplier bears obligation %s under the purchase scope, but its price does not list it as included", o))
			}
		}
	}

	saleBy := map[string]Obligation{}
	feeTypes := map[string]bool{}
	for _, o := range sale {
		if _, dup := saleBy[o.ID]; dup {
			return ChargePlan{}, fail(CodeChargeDoubleCount, o.Path, fmt.Sprintf("Obligation %s is listed twice", o.ID))
		}
		saleBy[o.ID] = o
		_, inPrice := included[o.ID]
		if inPrice || o.Bearer == Buyer {
			feeTypes[o.FeeType] = true
		}
		if inPrice && o.Bearer == Buyer {
			plan.Differences = append(plan.Differences, Difference{Path: o.Path,
				Reason: "A customer-borne obligation is embedded in a purchase price; nothing is deducted without proof"})
		}
	}

	type localKey struct{ offer, id string }
	locals := map[localKey]*chargeNode{}
	feeObligations := map[string]bool{}
	for i := range fees {
		f := &fees[i]
		if f.Party != Seller && f.Party != Buyer {
			return ChargePlan{}, fail(kernel.CodeSchemaInvalid, f.Path+"/responsible_party", "")
		}
		if feeObligations[f.ObligationID] {
			return ChargePlan{}, fail(CodeChargeDoubleCount, f.Path+"/obligation_id", "The obligation is already charged under another ID")
		}
		feeObligations[f.ObligationID] = true
		feeTypes[f.FeeType] = true
		if o, ok := saleBy[f.ObligationID]; ok && (o.Bearer != f.Party || o.FeeType != f.FeeType) {
			plan.Differences = append(plan.Differences, Difference{Path: f.Path,
				Reason: "The charge's party or fee type contradicts the reviewed sales responsibility"})
		}
		k := localKey{f.Offer, f.ID}
		if _, dup := locals[k]; dup {
			return ChargePlan{}, fail(kernel.CodeDuplicateLineID, f.Path+"/id", "")
		}
		n := &chargeNode{id: f.ComponentID, obligation: f.ObligationID, path: f.Path, party: f.Party, fee: f}
		if err := add(n); err != nil {
			return ChargePlan{}, err
		}
		locals[k] = n
	}

	for _, n := range order {
		f := n.fee
		if f == nil {
			continue
		}
		parent := included[f.ObligationID]
		if f.IncludedIn != "" {
			var candidates []string
			if p, ok := nodes[f.IncludedIn]; ok && p.fee == nil {
				candidates = append(candidates, p.id)
			}
			if l, ok := locals[localKey{f.Offer, f.IncludedIn}]; ok {
				candidates = append(candidates, l.id)
			}
			if len(candidates) != 1 {
				return ChargePlan{}, fail(CodeChargeScopeUnresolved, f.Path+"/included_in_component_ref",
					fmt.Sprintf("The including component %q cannot be identified; an inclusion is never ignored", f.IncludedIn))
			}
			if parent != "" && parent != candidates[0] {
				return ChargePlan{}, fail(CodeChargeScopeUnresolved, f.Path+"/included_in_component_ref",
					"The charge would be included in two different components")
			}
			parent = candidates[0]
		}
		if parent == "" {
			continue
		}
		n.parent = parent
		if plan.InclusionEdges++; plan.InclusionEdges > MaxInclusionEdges {
			return ChargePlan{}, fail(CodeCalculationLimitExceeded, f.Path+"/included_in_component_ref", "An evaluation has at most 2000 inclusion edges")
		}
		if p := nodes[parent]; p.fee != nil && p.party != n.party {
			return ChargePlan{}, fail(CodeChargeScopeUnresolved, f.Path+"/included_in_component_ref",
				"An inclusion between charges of different bearers is not resolved")
		}
	}

	obligations := map[string]bool{}
	for _, n := range order {
		if obligations[n.obligation] {
			return ChargePlan{}, fail(CodeChargeDoubleCount, n.path+"/obligation_id", fmt.Sprintf("Obligation %s is counted twice", n.obligation))
		}
		obligations[n.obligation] = true
	}
	for _, n := range order {
		seen := map[string]bool{}
		for x := n; x.parent != ""; x = nodes[x.parent] {
			if seen[x.id] {
				return ChargePlan{}, fail(CodeChargeDoubleCount, n.path+"/included_in_component_ref",
					"The inclusions form a cycle; a charge cannot remove its own cost")
			}
			if seen[x.id] = true; len(seen) > MaxInclusionDepth {
				return ChargePlan{}, fail(CodeCalculationLimitExceeded, n.path+"/included_in_component_ref", "An inclusion chain has at most 8 links")
			}
		}
	}

	for _, n := range order {
		d := ChargeDecision{ComponentID: n.id, Path: n.path, ObligationID: n.obligation, IncludedIn: n.parent, Status: Payable}
		switch {
		case n.fee != nil && n.party == Buyer:
			d.Status = CustomerBorne
			if n.parent != "" {
				plan.Differences = append(plan.Differences, Difference{Path: n.path,
					Reason: "A customer-borne charge is bundled into another price; its amount is not deducted without proof"})
			}
		case n.parent != "":
			d.Status = Included
		}
		plan.Decisions = append(plan.Decisions, d)
	}

	for _, o := range sale {
		if o.Bearer != Seller {
			continue
		}
		_, covered := included[o.ID]
		for _, f := range fees {
			covered = covered || (f.ObligationID == o.ID && f.Party == Seller)
		}
		if !covered {
			return ChargePlan{}, fail(CodeCostInputNotReady, o.Path,
				fmt.Sprintf("The seller bears obligation %s and no purchase price or charge covers it; it is never taken as zero", o.ID))
		}
	}
	for _, r := range required {
		if !feeTypes[r.FeeType] {
			return ChargePlan{}, fail(CodeCostInputNotReady, r.Path, "No charge or price inclusion accounts for a required fee type")
		}
	}
	return plan, nil
}
