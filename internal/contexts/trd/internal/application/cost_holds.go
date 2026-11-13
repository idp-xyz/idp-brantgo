package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/order"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// TRD's half of WF-03 (WBS S4-06b; tactical model WF-03 "TRD 已生效对象登记风险暂停", event storming
// PL-07, Context Map R11 and SC-04 activation first; S4 decision D-9; S3-D67): what CST's
// CostSourceDenied leaves on TRD's active objects, applied by the Inbox consumer
// trd.cost_source_holds (adapters/events) in a transaction on TRD's own transaction owner, as the
// consumer's system actor.
//
// The event names the snapshots whose evaluation rests on the denied source. For each quote root
// whose active revision's commit evidence names one of them (quote.HoldCostSource,
// order.RecordCostRisk):
//
//	no contract formed on the revision          quote.risk_hold.register: a risk hold, cause
//	                                             cost_source_denied, code COST_SOURCE_DENIED; the
//	                                             root version moves; never lifted
//	the root's order is active                  order.risk.record: a risk record on the order that
//	                                             blocks nothing (the contract formed, ADR-BR-013)
//	a contract formed, the order not active     nothing: the order's approval acknowledges the
//	                                             denial as an upstream risk (TRD-INV-014)
//
// The two commands are the consumer's own (system actors only; brant-admin inbox grants them to
// system:inbox.trd.cost_source_holds); they are not in the Context Map's TRD command list, which has
// no command for a consumer's registration (reported as a design gap). Each is keyed by the event,
// the command and the object, so a delivery registers once whatever the Inbox does; the hold's own
// identifier (cause and attestation) keeps one hold per denial and revision.
//
// Lock order (ADR-BR-011): every root the event concerns FOR UPDATE in ID order first, then, in the
// commands, the company's authority guard FOR SHARE. Orders are read under their root's lock, which
// every order command takes first. The root's object control does not fence the registration: it
// is not a forward write (quote.HoldCostSource).

// The consumer's commands and the reason they record in the audit trail.
const (
	CommandRegisterCostHold = "quote.risk_hold.register"
	CommandRecordOrderRisk  = "order.risk.record"
)

// costRules are the consumer's commands: system actors only.
func costRules() []authorization.Rule {
	system := []kernel.ActorKind{kernel.ActorSystem}
	return []authorization.Rule{
		{Command: CommandRegisterCostHold, Permission: CommandRegisterCostHold, ActorKinds: system},
		{Command: CommandRecordOrderRisk, Permission: CommandRecordOrderRisk, ActorKinds: system},
	}
}

// CostSourceDenial is CST's CostSourceDenied as TRD's consumer reads it: the event, CST's
// attestation, the denied source and the snapshots whose evaluation rests on it.
type CostSourceDenial struct {
	EventID       string
	AttestationID string
	SourceID      string
	SnapshotIDs   []string
}

// CostDenialEffect is what a denial left: the roots whose active revision it held and the orders it
// recorded a risk on, in ID order.
type CostDenialEffect struct {
	Held     []string
	Recorded []string
}

// costKey is the idempotency key of command on object for event: the same on every delivery.
func costKey(event, command, object string) string {
	sum := sha256.Sum256([]byte(event + "\x1f" + command + "\x1f" + object))
	return "csd." + hex.EncodeToString(sum[:20])
}

// costRequest is what a consumer command's request hash covers.
type costRequest struct {
	EventID       string   `json:"event_id"`
	AttestationID string   `json:"attestation_id"`
	SourceID      string   `json:"source_id"`
	SnapshotIDs   []string `json:"snapshot_ids"`
	Object        string   `json:"object_id"`
}

// ApplyCostSourceDenial applies denial d in the consumer's transaction exec (ctx carries its scope
// and the consumer's system principal): it locks the roots whose active revision's commit evidence
// names a denied snapshot, then registers each hold and records each order risk through TRD's
// command ledger, which joins the transaction. A root or order with nothing to register is left
// alone; an event that names no snapshot of TRD changes nothing.
func (s Service) ApplyCostSourceDenial(ctx context.Context, exec bentopg.Executor, d CostSourceDenial) (CostDenialEffect, error) {
	var out CostDenialEffect
	if len(d.SnapshotIDs) == 0 {
		return out, nil
	}
	sc := scope(ctx)
	roots, err := postgres.RootsOfSnapshots(ctx, exec, sc, d.SnapshotIDs)
	if err != nil || len(roots) == 0 {
		return out, err
	}
	if err = postgres.LockRoots(ctx, exec, sc, roots); err != nil {
		return out, err
	}
	denial := quote.CostDenial{AttestationID: d.AttestationID, SourceID: d.SourceID, EventID: d.EventID}
	for _, id := range roots {
		q, err := postgres.LoadQuote(ctx, exec, sc, id, postgres.NoLock)
		if err != nil {
			return out, err
		}
		if q == nil {
			continue
		}
		snapshot, rev := q.CostSnapshotOf()
		if rev == nil || !slices.Contains(d.SnapshotIDs, snapshot) {
			continue
		}
		if !q.Contracted(rev.Number) {
			if slices.ContainsFunc(q.Holds, func(h *quote.Hold) bool { return h.ID == quote.CostHoldID(d.AttestationID) }) {
				continue
			}
			if err = s.registerCostHold(ctx, d, denial, id); err != nil {
				return out, err
			}
			out.Held = append(out.Held, id)
			continue
		}
		if q.Slot == nil {
			continue
		}
		o, err := postgres.LoadOrder(ctx, exec, sc, q.Slot.OrderID, postgres.NoLock)
		if err != nil {
			return out, err
		}
		if o == nil || o.Status != order.Active || !slices.Contains(d.SnapshotIDs, o.CostSnapshot()) ||
			slices.ContainsFunc(o.Risks, func(r *order.RiskRecord) bool { return r.ID == order.RiskCostSourceDenied+":"+d.AttestationID }) {
			continue
		}
		if err = s.recordOrderRisk(ctx, d, denial, o.ID); err != nil {
			return out, err
		}
		out.Recorded = append(out.Recorded, o.ID)
	}
	return out, nil
}

func (s Service) consumerCommand(ctx context.Context, d CostSourceDenial, command, object string, h action.Handler) error {
	hash, err := action.RequestHash(costRequest{EventID: d.EventID, AttestationID: d.AttestationID, SourceID: d.SourceID,
		SnapshotIDs: d.SnapshotIDs, Object: object})
	if err != nil {
		return err
	}
	_, err = s.Ledger.Execute(ctx, action.Command{Name: command, IdempotencyKey: costKey(d.EventID, command, object), RequestHash: hash,
		ObjectID: object, Reason: "CST denied cost source " + d.SourceID + " (attestation " + d.AttestationID + ")"}, h)
	return err
}

// costHold registers the hold of a denial on a root the consumer locked.
type costHold struct {
	s      Service
	id     string
	denial quote.CostDenial
	snaps  []string
	q      *quote.Quote
}

func (h *costHold) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.q, err = postgres.LoadQuote(ctx, exec, scope(ctx), h.id, postgres.ForUpdate)
	return err
}

func (h *costHold) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	q := h.q
	if q == nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/quote_id", "")
	}
	from, fromEpoch := q.RootVersion, q.Control.Epoch
	hold, err := q.HoldCostSource(h.denial, h.snaps, d.Actor, d.Epoch, h.s.Clock.Now())
	if err != nil {
		return action.Outcome{}, err
	}
	if hold == nil {
		// The consumer decided on the same locked root: nothing can have changed it since.
		return action.Outcome{}, fmt.Errorf("trd: quote %s has no revision to hold for attestation %s", q.ID, h.denial.AttestationID)
	}
	if err = postgres.SaveQuote(ctx, exec, scope(ctx), q, false, from, fromEpoch, h.s.Clock.Now()); err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: q.ID, RowVersion: q.RootVersion, Status: quote.HoldCostSourceDenied}, nil
}

func (s Service) registerCostHold(ctx context.Context, d CostSourceDenial, denial quote.CostDenial, quoteID string) error {
	return s.consumerCommand(ctx, d, CommandRegisterCostHold, quoteID, &costHold{s: s, id: quoteID, denial: denial, snaps: d.SnapshotIDs})
}

// orderRisk records the risk of a denial on an active order whose root the consumer locked.
type orderRisk struct {
	s      Service
	id     string
	denial quote.CostDenial
	snaps  []string
	o      *order.Order
}

func (h *orderRisk) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.o, err = postgres.LoadOrder(ctx, exec, scope(ctx), h.id, postgres.NoLock)
	return err
}

func (h *orderRisk) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	o := h.o
	if o == nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/order_id", "")
	}
	r, err := o.RecordCostRisk(h.denial, h.snaps, d.Actor, d.Epoch, h.s.Clock.Now())
	if err != nil {
		return action.Outcome{}, err
	}
	if r == nil {
		return action.Outcome{}, fmt.Errorf("trd: order %s has no risk to record for attestation %s", o.ID, h.denial.AttestationID)
	}
	if err = postgres.WriteOrderRisks(ctx, exec, scope(ctx), o); err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: o.ID, RowVersion: o.RowVersion, Status: order.RiskCostSourceDenied}, nil
}

func (s Service) recordOrderRisk(ctx context.Context, d CostSourceDenial, denial quote.CostDenial, orderID string) error {
	return s.consumerCommand(ctx, d, CommandRecordOrderRisk, orderID, &orderRisk{s: s, id: orderID, denial: denial, snaps: d.SnapshotIDs})
}
