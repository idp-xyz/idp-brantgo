// Package customercredit holds the rules of STL's CustomerCredit aggregate (tactical model
// STL-AGG-CustomerCredit): one root per customer of a company, locked FOR UPDATE by every command
// that changes its limit requests, limit versions, reservations or decisions, so they are judged one
// at a time (STL-INV-004 concurrency: two reservations that together exceed the limit — one wins).
// It decides; adapters/postgres stores; application orders the steps.
package customercredit

import (
	"fmt"
	"slices"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// TemplateSalesClerk is the role template of 销售经办, who never approves credit (forbidden
// combinations sales_clerk with credit_officer and credit_manager; STL-INV-005).
const TemplateSalesClerk = "sales_clerk"

// ConfigUse is a POL configuration revision a fact was decided under, with its content hash.
type ConfigUse struct {
	ID       string
	Revision int64
	Hash     string
}

// Tier is one approval level of the credit policy: the largest amount its holders approve.
type Tier struct {
	Level string
	Max   kernel.Money
}

// Policy is a credit_policy revision as STL judges by it (POL R33, ADR-BR-016), its amounts in
// Currency: the default limit of a customer without a limit version in force, the accepted limit
// sources, the approval tiers, the overdue tolerance, and the S5 members (A3a): whether
// document-controlled payments after shipment are secured (D-5), how long a reservation and a
// decision stay valid, and whether two top-tier approvers decide above the top tier.
type Policy struct {
	Use                     ConfigUse
	Currency                kernel.Currency
	DefaultLimit            kernel.Money
	LimitSources            []string
	Tiers                   []Tier
	OverdueToleranceDays    int
	DocumentsSecured        bool
	ReservationValidityDays int
	DecisionValidityDays    int
	TwoApproversAboveTop    bool
}

// Top returns the tier with the largest amount.
func (p Policy) Top() Tier {
	var top Tier
	for i, t := range p.Tiers {
		if c, _ := t.Max.Cmp(top.Max); i == 0 || c > 0 {
			top = t
		}
	}
	return top
}

// TierOf returns the approver's tier (S5 D-6): the tier with the largest amount whose level is one
// of the approver's role templates.
func (p Policy) TierOf(templates []string) (Tier, bool) {
	var best Tier
	found := false
	for _, t := range p.Tiers {
		if !slices.Contains(templates, t.Level) {
			continue
		}
		if c, _ := t.Max.Cmp(best.Max); !found || c > 0 {
			best, found = t, true
		}
	}
	return best, found
}

// ReservationExpiry and DecisionExpiry return when a reservation or a decision made at now ends.
func (p Policy) ReservationExpiry(now time.Time) time.Time {
	return kernel.Instant(now.Add(time.Duration(p.ReservationValidityDays) * 24 * time.Hour))
}

func (p Policy) DecisionExpiry(now time.Time) time.Time {
	return kernel.Instant(now.Add(time.Duration(p.DecisionValidityDays) * 24 * time.Hour))
}

// Approver is a human deciding a limit request or a credit decision, with the role templates its
// roles resolve to (authorization.Grants.Templates).
type Approver struct {
	Actor     kernel.ActorRef
	Templates []string
}

// Approval is one approver's approval of an amount, with the tier it approved under.
type Approval struct {
	Approver kernel.ActorRef
	Level    string
	Max      kernel.Money
	Epoch    int64
	At       time.Time
}

// Independent refuses an approver who is the requester or a sales clerk (STL-INV-005: 不是该客户的销售
// 经办或申请人), at path.
func Independent(a Approver, requester, path string) error {
	switch {
	case a.Actor.ID == requester:
		return Reject(CodeCreditDecisionNotAuthorized, path, "The requester does not decide its own credit request")
	case slices.Contains(a.Templates, TemplateSalesClerk):
		return Reject(CodeCreditDecisionNotAuthorized, path, "A sales clerk does not decide credit")
	}
	return nil
}

// Approve decides whether a may approve cover, an amount in the policy's currency, requested by
// requester, after the approvals prior (STL-INV-005): a must be independent and not have approved
// already; up to the top tier one approver whose tier covers cover decides; above it, when the
// policy says so, two distinct approvers of the top tier decide (otherwise nobody may). It returns
// a's approval and how many approvals cover needs.
func (p Policy) Approve(cover kernel.Money, a Approver, requester string, prior []Approval, path string) (Approval, int, error) {
	if err := Independent(a, requester, path); err != nil {
		return Approval{}, 0, err
	}
	if slices.ContainsFunc(prior, func(x Approval) bool { return x.Approver.ID == a.Actor.ID }) {
		return Approval{}, 0, Reject(CodeCreditDecisionNotAuthorized, path, "A second approval needs another approver")
	}
	if cover.Currency() != p.Currency {
		return Approval{}, 0, Reject(CodeFXScopeMismatch, path, fmt.Sprintf("The amount is in %s, the credit policy's tiers in %s",
			cover.Currency(), p.Currency))
	}
	tier, ok := p.TierOf(a.Templates)
	if !ok {
		return Approval{}, 0, Reject(CodeCreditDecisionNotAuthorized, path, "The approver holds no credit approval tier")
	}
	top := p.Top()
	above, _ := cover.Cmp(top.Max)
	required := 1
	if above > 0 {
		if !p.TwoApproversAboveTop {
			return Approval{}, 0, Reject(CodeCreditDecisionNotAuthorized, path, fmt.Sprintf("%s exceeds the top tier %s", cover.Amount(), top.Max.Amount()))
		}
		required = 2
		if c, _ := tier.Max.Cmp(top.Max); c < 0 {
			return Approval{}, 0, Reject(CodeCreditDecisionNotAuthorized, path,
				fmt.Sprintf("%s exceeds the top tier %s: two %s approvers decide", cover.Amount(), top.Max.Amount(), top.Level))
		}
	} else if c, _ := cover.Cmp(tier.Max); c > 0 {
		return Approval{}, 0, Reject(CodeCreditDecisionNotAuthorized, path,
			fmt.Sprintf("%s exceeds the %s tier %s", cover.Amount(), tier.Level, tier.Max.Amount()))
	}
	return Approval{Approver: a.Actor, Level: tier.Level, Max: tier.Max}, required, nil
}
