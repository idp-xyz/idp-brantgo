package api

import (
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The price proposal's query is closed (S4 unit G4a; owner decision 2026-11-18): the RFQ, the
// currency's code and the pricing policy reference are required; the customer and the cost snapshot
// are optional, and a snapshot is named as a draft names it, with its result hash.
func TestPriceProposalQueryContract(t *testing.T) {
	hash := strings.Repeat("a", 64)
	valid := PriceProposalQuery{RfqID: "rfq.1", CustomerID: "customer.1", Currency: "USD",
		CostSnapshotRef:  &CostSnapshotRef{SnapshotID: "snapshot.1", ResultHash: hash},
		PricingPolicyRef: ConfigRef{Kind: PricingPolicyKind, ID: "policy.1", Revision: 1}}
	for name, c := range map[string]struct {
		edit func(*PriceProposalQuery)
		want string // "" for a valid query, else the path of SCHEMA_INVALID
	}{
		"complete":                  {func(*PriceProposalQuery) {}, ""},
		"without a customer":        {func(q *PriceProposalQuery) { q.CustomerID = "" }, ""},
		"without a snapshot":        {func(q *PriceProposalQuery) { q.CostSnapshotRef = nil }, ""},
		"no RFQ":                    {func(q *PriceProposalQuery) { q.RfqID = "" }, "/rfq_id"},
		"an RFQ that is no ID":      {func(q *PriceProposalQuery) { q.RfqID = "询价 1" }, "/rfq_id"},
		"a customer that is no ID":  {func(q *PriceProposalQuery) { q.CustomerID = "customer 1" }, "/customer_id"},
		"no currency":               {func(q *PriceProposalQuery) { q.Currency = "" }, "/currency"},
		"a currency name":           {func(q *PriceProposalQuery) { q.Currency = "dollar" }, "/currency"},
		"a lower-case code":         {func(q *PriceProposalQuery) { q.Currency = "usd" }, "/currency"},
		"a snapshot without an ID":  {func(q *PriceProposalQuery) { q.CostSnapshotRef = &CostSnapshotRef{ResultHash: hash} }, "/cost_snapshot_ref/snapshot_id"},
		"a snapshot without a hash": {func(q *PriceProposalQuery) { q.CostSnapshotRef = &CostSnapshotRef{SnapshotID: "s"} }, "/cost_snapshot_ref/result_hash"},
		"a hash that is none": {func(q *PriceProposalQuery) { q.CostSnapshotRef = &CostSnapshotRef{SnapshotID: "s", ResultHash: "ABC"} },
			"/cost_snapshot_ref/result_hash"},
		"no policy":               {func(q *PriceProposalQuery) { q.PricingPolicyRef = ConfigRef{} }, "/pricing_policy_ref/kind"},
		"another kind":            {func(q *PriceProposalQuery) { q.PricingPolicyRef.Kind = "credit_policy" }, "/pricing_policy_ref/kind"},
		"a policy without ID":     {func(q *PriceProposalQuery) { q.PricingPolicyRef.ID = "" }, "/pricing_policy_ref/id"},
		"a policy at no revision": {func(q *PriceProposalQuery) { q.PricingPolicyRef.Revision = 0 }, "/pricing_policy_ref/revision"},
	} {
		q := valid
		c.edit(&q)
		err := q.Validate()
		got := ""
		if rej, ok := kernel.AsRejection(err); ok && rej.Code == kernel.CodeSchemaInvalid {
			got = rej.Fields[0].Path
		} else if err != nil {
			got = err.Error()
		}
		if got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
	if QueryPriceProposal != "quote.price_proposal" || PriceSourceList != "price_list" || PriceSourceFormula != "formula" ||
		ProposalNoAdoptableSnapshot != "no_adoptable_snapshot" || ProposalNoTargetMargin != "no_target_margin" ||
		ProposalAnotherCurrency != "another_currency" {
		t.Fatal("the names of the owner's decision")
	}
}
