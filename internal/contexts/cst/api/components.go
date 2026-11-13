package api

import (
	"context"
	"math/big"
)

// The components of a frozen snapshot by cost category (Context Map R78 CST→FUL, CostSnapshotRef v1:
// "订单来源报价的成本快照中的保险费组件（CIF／CIP 投保核对，只读）"; S5 decision D-13; WBS S5-16, F-03).
// A snapshot's result names each charge component's source — the logistics offer revision and the
// fee in it — but not the fee's POL fee_category; CST reads them where its evaluation found them:
// the SUP logistics offer revision the evaluation bound (SupplyConditionRef v1, R09), its fee's
// fee_type_ref, and that POL fee_category revision (ConfigRef v1, R10), each checked against the
// content hash the evaluation bound. A purchase component is goods. What CST cannot read now (a
// revision the caller does not see, or one whose content hash is not the one bound) leaves the
// component's category empty and its ID in Unresolved: the answer never guesses.

// Cost categories of a component: a purchase is goods; a charge is its POL fee category's
// category (goods, freight, insurance, packaging, inspection, commission, bank or other).
const (
	CategoryGoods     = "goods"
	CategoryInsurance = "insurance"

	ComponentPurchase = "purchase"
	ComponentCharge   = "charge"
)

// CategorizedComponent is one payable component of a frozen snapshot with its cost category: its
// kind (purchase or charge), the source it was costed from with the fee in it (SourceLineID), the
// POL fee_category revision of a charge (nil for a purchase or when unresolved) and its category
// ("" when unresolved), the amount in its own currency (Currency, CostMinor) and in the snapshot's
// (AmountMinor), integer minor units as decimal strings.
type CategorizedComponent struct {
	ComponentID  string
	Kind         string
	Source       SupplyRef // the logistics offer or supplier offer revision
	SourceLineID string
	FeeCategory  *ConfigRef
	Category     string
	Currency     string
	CostMinor    string
	AmountMinor  string
}

// SnapshotComponents is what CST says of one CostSnapshotRef v1 by category: the snapshot's
// currency and its minor units, its total, every payable component with its category, and the
// charge components whose category CST could not read now.
type SnapshotComponents struct {
	Ref        CostSnapshotRef
	Currency   string
	MinorUnits int
	TotalMinor string
	Components []CategorizedComponent
	Unresolved []string
}

// Total returns the sum of the components of category in the snapshot's currency (minor units as a
// decimal string), their IDs, and whether the sum is complete — no component's category is
// unresolved, so none of them can be of the category too.
func (s SnapshotComponents) Total(category string) (minor string, ids []string, complete bool) {
	sum := new(big.Int)
	for _, c := range s.Components {
		if c.Category != category {
			continue
		}
		v, ok := new(big.Int).SetString(c.AmountMinor, 10)
		if !ok {
			return "", nil, false
		}
		sum.Add(sum, v)
		ids = append(ids, c.ComponentID)
	}
	return sum.String(), ids, len(s.Unresolved) == 0
}

// ComponentLookups answers CostSnapshotRef v1 lookups by category (R78). It needs no permission
// beyond an active grant and is not audited; snapshots that do not exist in the caller's company,
// lie outside its data scope or whose result hash differs from the reference's are absent, as for
// SnapshotLookups. At most MaxSnapshotLookup references are asked at once (SCHEMA_INVALID
// otherwise). It reads SUP and POL after its own read transaction, never inside it; ask before
// opening yours.
type ComponentLookups interface {
	ResolveSnapshotComponents(ctx context.Context, refs []CostSnapshotRef) (map[CostSnapshotRef]SnapshotComponents, error)
}
