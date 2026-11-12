package application

import (
	"context"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	cc "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/customercredit"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Orders is STL's port to TRD (Context Map R32 TRD→STL, OHS, "OrderCommercialSnapshot v1（草稿与生效
// 订单；付款阶段、金额与信用部分）", guardLock false; risk: 付款计划被当作应收或到账). STL reads an order
// before its transaction and never locks a TRD row: TRD calls STL's CreditGuard v1 instead (R57).
//
//   - ResolveOrders, before STL's transaction: what TRD says of each order now — drafts and active
//     orders — at its current revision: the binding, its state, the customer, the POL currency
//     revision its amounts are in, the payment stages of its sealed calculation with their amounts
//     and release control (D-5), and CreditPortionMinor, the stages due after shipment or invoice
//     without document control; for an active order also the revisions its amendments pending
//     approval or approved propose, with theirs (S5 D-9: credit.reserve may reserve one before
//     the amendment is applied). Orders outside the caller's company or data scope are absent.
//
// adapters/orders implements the port on trdapi (OrderSnapshotLookups.ResolveOrderSnapshots); tests
// implement it to steer TRD's answers.
type Orders interface {
	ResolveOrders(ctx context.Context, orderIDs []string) (map[string]Order, error)
}

// Order states TRD reports (SM-TRD-ORDER).
const (
	OrderDraft  = "draft"
	OrderActive = "active"
)

// OrderBinding is an order at an order revision and its order hash (TRD OrderBinding).
type OrderBinding struct {
	OrderID       string
	OrderRevision int64
	OrderHash     string
}

// OrderStage is one payment stage of the order revision (TRD's sealed calculation, Payments): its
// trigger (order_confirmed, before_shipment, after_shipment, after_invoice), days, percentage,
// scheduled amount in minor units of the order currency, release control ("" or documents), and
// whether TRD counts it in the credit portion (OnCredit).
type OrderStage struct {
	StageID        string
	Trigger        string
	Days           int
	Percent        string
	AmountMinor    string
	ReleaseControl string
	OnCredit       bool
}

// CurrencyRef is the POL currency revision an order's amounts are in.
type CurrencyRef struct {
	ID       string
	Revision int64
}

// Order is what TRD says of an order now (OrderCommercialSnapshot v1). Code is "" when its sealed
// revision is intact, REFERENCE_INVALID otherwise. Proposed are the revisions the open amendments
// of an active order propose, which TRD derives (S5 D-9).
type Order struct {
	Binding            OrderBinding
	Status             string
	Code               string
	CustomerID         string
	Currency           CurrencyRef
	Stages             []OrderStage
	CreditPortionMinor string
	Proposed           []ProposedRevision
}

// ProposedRevision is a revision an amendment of the order proposes: the order at the revision
// number and hash order.amendment.apply would produce, with that revision's payment stages and
// credit portion in the order currency (TRD ProposedRevision, those TRD derives now only).
type ProposedRevision struct {
	AmendmentID        string
	Binding            OrderBinding
	Stages             []OrderStage
	CreditPortionMinor string
}

// revision returns the stages and credit portion of the order revision b binds: the current
// revision, or a revision an amendment proposes (only of an active order). ok is false for any
// other binding.
func (o Order) revision(b OrderBinding) (stages []OrderStage, portion string, ok bool) {
	if b == o.Binding {
		return o.Stages, o.CreditPortionMinor, true
	}
	if o.Status != OrderActive {
		return nil, "", false
	}
	for _, p := range o.Proposed {
		if p.Binding == b {
			return p.Stages, p.CreditPortionMinor, true
		}
	}
	return nil, "", false
}

// numbers reports whether revision is the order's current revision number or one an amendment of
// it proposes.
func (o Order) numbers(revision int64) bool {
	if revision == o.Binding.OrderRevision {
		return true
	}
	for _, p := range o.Proposed {
		if o.Status == OrderActive && p.Binding.OrderRevision == revision {
			return true
		}
	}
	return false
}

func polRef(r api.ConfigRef) polapi.ConfigRef {
	return polapi.ConfigRef{Kind: r.Kind, ConfigID: r.ConfigID, Revision: r.Revision}
}

// resolveConfigs asks POL, before STL's transaction, for refs at at; absent references are
// REFERENCE_INVALID at their paths.
func (s Service) resolveConfigs(ctx context.Context, at time.Time, refs map[string]polapi.ConfigRef) (map[string]polapi.ResolvedConfig, error) {
	list := make([]polapi.ConfigRef, 0, len(refs))
	for _, r := range refs {
		list = append(list, r)
	}
	found, err := s.Upstream.Configs.ResolveConfigs(ctx, list, at)
	if err != nil {
		return nil, err
	}
	out := map[string]polapi.ResolvedConfig{}
	for path, r := range refs {
		rc, ok := found[r]
		if !ok {
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, path, "POL has no such configuration revision")
		}
		out[path] = rc
	}
	return out, nil
}

// creditPolicy checks a resolved credit_policy revision: in force, with its data.
func creditPolicy(rc polapi.ResolvedConfig, path string) (*polapi.CreditPolicy, error) {
	if err := rc.Check(path); err != nil {
		return nil, err
	}
	if rc.Data.CreditPolicy == nil {
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, path, "Not a credit policy")
	}
	return rc.Data.CreditPolicy, nil
}

// policyOf is a credit_policy revision as STL judges by it, its amounts in c, the policy's currency
// with its minor units.
func policyOf(rc polapi.ResolvedConfig, c kernel.Currency) (cc.Policy, error) {
	p := rc.Data.CreditPolicy
	path := "/args/credit_policy"
	if p == nil {
		return cc.Policy{}, kernel.Reject(kernel.CodeReferenceInvalid, path, "Not a credit policy")
	}
	if p.Currency != c.Code() {
		return cc.Policy{}, api.Reject(api.CodeFXScopeMismatch, path, "The credit policy is in "+p.Currency+", the amount in "+c.Code())
	}
	if p.ReservationValidityDays < 1 || p.DecisionValidityDays < 1 {
		// A revision saved before S5 states no validities; STL fails closed (POL CreditPolicy).
		return cc.Policy{}, kernel.Reject(kernel.CodeReferenceInvalid, path, "The credit policy states no reservation or decision validity")
	}
	out := cc.Policy{Use: cc.ConfigUse{ID: rc.Ref.ConfigID, Revision: rc.Ref.Revision, Hash: rc.ContentHash}, Currency: c,
		LimitSources: p.LimitSources, OverdueToleranceDays: p.OverdueToleranceDays, DocumentsSecured: p.DocumentsSecured,
		ReservationValidityDays: p.ReservationValidityDays, DecisionValidityDays: p.DecisionValidityDays,
		TwoApproversAboveTop: p.TwoApproversAboveTop}
	var err error
	if out.DefaultLimit, err = kernel.ParseAmount(c, p.DefaultLimit); err != nil {
		return cc.Policy{}, kernel.Reject(kernel.CodeReferenceInvalid, path, "The default limit has more digits than "+c.Code())
	}
	for _, t := range p.ApprovalTiers {
		m, err := kernel.ParseAmount(c, t.MaxAmount)
		if err != nil {
			return cc.Policy{}, kernel.Reject(kernel.CodeReferenceInvalid, path, "A tier has more digits than "+c.Code())
		}
		out.Tiers = append(out.Tiers, cc.Tier{Level: t.Level, Max: m})
	}
	return out, nil
}

// guardConfig re-checks, inside STL's transaction and after its root is locked, a POL revision STL
// decides by, through POL's ConfigGuard v1 with STL's executor (R33 guard lock): in force at at and
// at the content hash read before the transaction (REFERENCE_STALE otherwise). The row stays locked
// FOR SHARE until STL commits.
func guardConfig(ctx context.Context, exec bentopg.Executor, ref polapi.ConfigRef, hash string, at time.Time, path string) error {
	v, err := polapi.ScanConfigGuard(ref, exec.QueryRow(ctx, polapi.ConfigGuardSQL, polapi.ConfigGuardArgs(ref, at)...).Scan)
	if err != nil {
		return err
	}
	if err = v.Check(path); err != nil {
		return err
	}
	if v.ContentHash != hash {
		return api.Reject(api.CodeReferenceStale, path, "The configuration revision changed since it was read")
	}
	return nil
}

// approver is the deciding human with the role templates its roles resolve to (D-6), read in the
// transaction after the ledger authorized it.
func (s Service) approver(ctx context.Context, exec bentopg.Executor, actor kernel.ActorRef) (cc.Approver, error) {
	templates, err := s.Grants.Templates(ctx, exec, scope(ctx), actor.ID)
	return cc.Approver{Actor: actor, Templates: templates}, err
}

func configOf(u cc.ConfigUse, kind string) api.ConfigRef {
	return api.ConfigRef{Kind: kind, ConfigID: u.ID, Revision: u.Revision}
}
