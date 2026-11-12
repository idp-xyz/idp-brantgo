package orderaccount

import (
	"fmt"
	"slices"
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/fundrecord"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The payment plan review (tactical model STL-AGG-OrderAccount, entity PaymentPlanReview;
// SM-STL-PAYMENT-PLAN-REVIEW; WF-06; S5 plan D-4, D-21). When TRD amends an order, the account
// adopts the payment stages of the new order revision (Move) and records a review of (order,
// revision) whose items are what no longer fits: a stage that is gone, a stage with more applied
// than it now asks, a changed credit portion. Applications remain facts; they stand on the stage of
// the same ID at every later revision (applied is passed in that way). A person settles the review
// item by item (Settle); a later revision's review supersedes an open one (Supersede).

// Review states (SM-STL-PAYMENT-PLAN-REVIEW).
const (
	ReviewOpen       = "open"
	ReviewSettled    = "settled"
	ReviewSuperseded = "superseded"
)

// Item kinds and dispositions.
const (
	ItemStageRemoved     = "stage_removed"
	ItemStageOverApplied = "stage_over_applied"
	ItemCreditPortion    = "credit_portion_changed"

	DispositionAccepted  = "accepted"
	DispositionCorrected = "corrected"
)

// Item is one impact of the new revision on the account. For a stage: From is its due at the
// earlier revision (0 when the stage is new), To its due now (0 when it is gone), Applied what
// stands on it. For the credit portion: From and To are the portion before and now, Applied 0.
// Disposition and Note are set when the review is settled.
type Item struct {
	Seq         int
	Kind        string
	StageID     string
	From        kernel.Money
	To          kernel.Money
	Applied     kernel.Money
	Disposition string
	Note        string
}

// Review is one payment plan review of an order account: the revision it moved the account to and
// the one it moved it from (0 when the review opened the account), the role responsible for
// settling it, its state and items.
type Review struct {
	OrderID         string
	OrderRevision   int64
	OrderHash       string
	FromRevision    int64
	FromHash        string
	ResponsibleRole string
	Status          string
	Version         int64
	Items           []Item
	Opened          fundrecord.Act
	Settled         *fundrecord.Act
	SupersededBy    int64
	SupersededAt    time.Time
}

// Disposition is how a person disposes of the item numbered Seq.
type Disposition struct {
	Seq         int
	Disposition string
	Note        string
}

// CreditPortion is what a plan asks on credit (TRD-INV-022, D-5): the stages due after shipment or
// invoice that are not under document control, in currency c.
func CreditPortion(c kernel.Currency, stages []Stage) kernel.Money {
	out, _ := kernel.NewMoney(c, 0)
	for _, s := range stages {
		if (s.Trigger == TriggerAfterShipment || s.Trigger == TriggerAfterInvoice) && s.ReleaseControl != ReleaseDocuments {
			out, _ = out.Add(s.Amount)
		}
	}
	return out
}

// amountOf returns m, or zero in c for the zero value.
func amountOf(c kernel.Currency, m kernel.Money) kernel.Money {
	if m.Currency().IsZero() {
		z, _ := kernel.NewMoney(c, 0)
		return z
	}
	return m
}

// Begin is the review an account opened by the review itself starts with: no earlier revision,
// nothing to dispose of, still open until a person settles it (SM-STL-PAYMENT-PLAN-REVIEW).
func (a *Account) Begin(role string, by fundrecord.Act) *Review {
	return &Review{OrderID: a.OrderID, OrderRevision: a.OrderRevision, OrderHash: a.OrderHash, ResponsibleRole: role, Status: ReviewOpen,
		Version: 1, Items: []Item{}, Opened: by}
}

// Move is payment_plan_review.open's rule on an existing account: a moves to the later order
// revision at hash with that revision's stages (checked as Open checks them) in the same currency,
// and the returned review lists, in this order, every stage of the earlier revision that is gone
// and every other stage ID something still stands on, every stage of the new revision with more
// applied than it asks, and a changed credit portion. applied holds what stands on each stage ID
// (applications of this revision or an earlier one; absent: none).
func (a *Account) Move(revision int64, hash string, currencyRevision int64, stages []Stage, applied map[string]kernel.Money, role string,
	by fundrecord.Act) (*Review, error) {
	if revision <= a.OrderRevision {
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/order_revision",
			fmt.Sprintf("The order account holds revision %d already", a.OrderRevision))
	}
	if err := checkStages(stages); err != nil {
		return nil, err
	}
	c := a.Currency()
	if stages[0].Amount.Currency() != c {
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/args/order_id", "TRD's order revision is in another currency than its account")
	}
	r := &Review{OrderID: a.OrderID, OrderRevision: revision, OrderHash: hash, FromRevision: a.OrderRevision, FromHash: a.OrderHash,
		ResponsibleRole: role, Status: ReviewOpen, Version: 1, Items: []Item{}, Opened: by}
	add := func(it Item) {
		it.Seq = len(r.Items) + 1
		r.Items = append(r.Items, it)
	}
	zero, _ := kernel.NewMoney(c, 0)
	next := map[string]Stage{}
	for _, s := range stages {
		next[s.StageID] = s
	}
	gone := map[string]bool{}
	for _, s := range a.Stages {
		if _, ok := next[s.StageID]; !ok {
			gone[s.StageID] = true
			add(Item{Kind: ItemStageRemoved, StageID: s.StageID, From: s.Amount, To: zero, Applied: amountOf(c, applied[s.StageID])})
		}
	}
	var stray []string
	for id, m := range applied {
		if _, ok := next[id]; !ok && !gone[id] && m.Sign() > 0 && a.Stage(id) == nil {
			stray = append(stray, id)
		}
	}
	slices.Sort(stray)
	for _, id := range stray {
		add(Item{Kind: ItemStageRemoved, StageID: id, From: zero, To: zero, Applied: applied[id]})
	}
	for _, s := range stages {
		got := amountOf(c, applied[s.StageID])
		if over, _ := got.Cmp(s.Amount); over <= 0 {
			continue
		}
		from := zero
		if old := a.Stage(s.StageID); old != nil {
			from = old.Amount
		}
		add(Item{Kind: ItemStageOverApplied, StageID: s.StageID, From: from, To: s.Amount, Applied: got})
	}
	if before, after := CreditPortion(c, a.Stages), CreditPortion(c, stages); !before.Equal(after) {
		add(Item{Kind: ItemCreditPortion, From: before, To: after, Applied: zero})
	}
	a.OrderRevision, a.OrderHash, a.CurrencyRevision, a.Stages = revision, hash, currencyRevision, stages
	a.Version++
	return r, nil
}

// Supersede marks open review r superseded by the review of the later revision at at.
func (r *Review) Supersede(revision int64, at time.Time) error {
	if r.Status != ReviewOpen || revision <= r.OrderRevision {
		return kernel.Reject(kernel.CodeInvalidState, "/args/order_revision", "Only an open review of an earlier revision is superseded")
	}
	r.Status, r.Version, r.SupersededBy, r.SupersededAt = ReviewSuperseded, r.Version+1, revision, kernel.Instant(at)
	return nil
}

// Settle is payment_plan_review.settle's rule (human only; "付款阶段与信用部分的变化逐项处置"): open
// review r of account a, the revision a holds, is settled when ds dispose of every item exactly
// once. An item disposed of as corrected must be corrected now, with applied holding what stands on
// each stage ID of a: nothing on a stage that is gone, no more than a stage asks. The account moves
// by one version.
func (r *Review) Settle(a *Account, ds []Disposition, applied map[string]kernel.Money, by fundrecord.Act) error {
	if r.Status != ReviewOpen {
		return kernel.Reject(kernel.CodeInvalidState, "/args/order_revision", "The review is "+r.Status)
	}
	if a.OrderRevision != r.OrderRevision {
		return kernel.Reject(kernel.CodeInvalidState, "/args/order_revision", "The order account holds another revision")
	}
	if len(ds) != len(r.Items) {
		return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/items", fmt.Sprintf("The review has %d items: each is disposed of once", len(r.Items)))
	}
	c := a.Currency()
	byItem := map[int]Disposition{}
	for i, d := range ds {
		path := "/args/items/" + strconv.Itoa(i)
		if d.Seq < 1 || d.Seq > len(r.Items) || byItem[d.Seq].Seq != 0 {
			return kernel.Reject(kernel.CodeInvalidKnownValue, path+"/seq", "Not an item of the review, or named twice")
		}
		it := r.Items[d.Seq-1]
		if d.Disposition == DispositionCorrected && it.Kind != ItemCreditPortion {
			now := amountOf(c, applied[it.StageID])
			switch st := a.Stage(it.StageID); {
			case st == nil && now.Sign() > 0:
				return kernel.Reject(kernel.CodeInvalidState, path+"/disposition",
					fmt.Sprintf("Stage %s is gone and %s still stands on it", it.StageID, now.Amount()))
			case st != nil:
				if over, _ := now.Cmp(st.Amount); over > 0 {
					return kernel.Reject(kernel.CodeInvalidState, path+"/disposition",
						fmt.Sprintf("Stage %s asks %s and %s stands on it", it.StageID, st.Amount.Amount(), now.Amount()))
				}
			}
		}
		byItem[d.Seq] = d
	}
	for i := range r.Items {
		d := byItem[r.Items[i].Seq]
		r.Items[i].Disposition, r.Items[i].Note = d.Disposition, d.Note
	}
	r.Status, r.Version, r.Settled = ReviewSettled, r.Version+1, &by
	a.Version++
	return nil
}
