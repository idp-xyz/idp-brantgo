// Package events holds TRD's Inbox consumer: the inbound adapter that turns the integration events
// of TRD's upstream contexts into calls on TRD's application (repository structure §07). The
// Dispatcher runs the handler in a transaction on TRD's own transaction owner, as the consumer's
// system actor, once per event ID (foundation/messaging).
//
// trd.cost_source_holds consumes CST's CostSourceDenied (Context Map R11 CST→TRD, CostSnapshotRef v1;
// tactical model WF-03 step 4, event storming PL-07, Context Map SC-04 activation first; S4 decision
// D-9; WBS S4-06b): the active revisions whose commit evidence names a snapshot resting on the
// denied source are held (COST_SOURCE_DENIED) while no contract formed on them, and the active orders
// on such snapshots get a risk record that blocks nothing (application.ApplyCostSourceDenial).
package events

import (
	"context"
	"encoding/json"
	"fmt"

	"go.idp.xyz/idp-bento-go/eventing"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/application"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// CostSourceHolds is the consumer of CST's source denials. Its handler runs as
// system:inbox.trd.cost_source_holds, which brant-admin inbox grants CostSourceHoldPermissions in
// every company it serves.
const CostSourceHolds eventing.InboxConsumer = "trd.cost_source_holds"

// FailureDenialInvalid is the failure code of a CostSourceDenied whose payload does not carry what
// CST's denials carry: the event is refused for good (dead letter and alert, ADR-BR-015).
const FailureDenialInvalid eventing.FailureCode = "trd.cost_source_denial_invalid"

// costSnapshot is the aggregate type CST gives its source denials (the aggregate is the source).
const costSnapshot = "CostSnapshot"

// maxSnapshots bounds the snapshots one denial may name.
const maxSnapshots = 10000

// CostSourceHoldPermissions are what the consumer's commands need — quote.risk_hold.register and
// order.risk.record, the consumer's own system commands — and trd.read, which the command ledger
// asks for before it answers a command from its stored result.
func CostSourceHoldPermissions() []string {
	return []string{application.CommandRegisterCostHold, application.CommandRecordOrderRisk, application.PermissionRead}
}

// Denials is what trd.cost_source_holds asks of TRD's application; application.Service implements it.
type Denials interface {
	ApplyCostSourceDenial(ctx context.Context, exec bentopg.Executor, d application.CostSourceDenial) (application.CostDenialEffect, error)
}

// Subscriptions returns TRD's subscriptions on db, TRD's own transaction owner.
func Subscriptions(db *bentopg.DB, app Denials) []messaging.Subscription {
	return []messaging.Subscription{{
		Consumer:    CostSourceHolds,
		Source:      cstapi.Source,
		EventTypes:  []eventing.EventType{cstapi.EventCostSourceDenied},
		DB:          db,
		Permissions: CostSourceHoldPermissions(),
		Handler:     CostSourceHandler(app),
	}}
}

// CostSourceHandler applies one CostSourceDenied. A payload that is not a denial's is refused for
// good; every other failure — the consumer's actor not provisioned, a conflict, the database —
// rolls the transaction back and the event is delivered again. Nothing is read from CST: the event
// names the snapshots, and TRD's own commit evidence names the snapshot of each revision and order.
func CostSourceHandler(app Denials) messaging.EventFunc {
	return func(ctx context.Context, e messaging.Event) error {
		d, err := Denial(e.Envelope)
		if err != nil {
			return messaging.RejectEvent(FailureDenialInvalid, err)
		}
		_, err = app.ApplyCostSourceDenial(ctx, e.Exec, d)
		return err
	}
}

// denialPayload is what the consumer reads of a CostSourceDenied's payload (contract C4: identifiers
// and versions; CST's costing.attest_source writes it).
type denialPayload struct {
	AggregateType string   `json:"aggregate_type"`
	AggregateID   string   `json:"aggregate_id"`
	AttestationID string   `json:"attestation_id"`
	SourceID      string   `json:"source_id"`
	SnapshotIDs   []string `json:"snapshot_ids"`
}

// Denial reads envelope as CST's CostSourceDenied, or says what its payload lacks. A denial that
// names no snapshot is valid: no snapshot rested on the source.
func Denial(envelope eventing.Envelope) (application.CostSourceDenial, error) {
	var p denialPayload
	if err := json.Unmarshal(envelope.Payload, &p); err != nil {
		return application.CostSourceDenial{}, fmt.Errorf("trd: cost source denial payload: %w", err)
	}
	fail := func(what string) (application.CostSourceDenial, error) {
		return application.CostSourceDenial{}, fmt.Errorf("trd: cost source denial %s of type %s: %s", envelope.ID, envelope.Type, what)
	}
	switch {
	case envelope.Type != cstapi.EventCostSourceDenied:
		return fail("not " + cstapi.EventCostSourceDenied)
	case envelope.ID == "":
		return fail("no event ID")
	case p.AggregateType != costSnapshot:
		return fail("aggregate_type is not " + costSnapshot)
	case !kernel.ValidIdentifier(p.SourceID) || p.SourceID != p.AggregateID:
		return fail("source_id is not the aggregate's identifier")
	case !kernel.ValidIdentifier(p.AttestationID):
		return fail("attestation_id is not an identifier")
	case len(p.SnapshotIDs) > maxSnapshots:
		return fail("too many snapshot_ids")
	}
	for _, id := range p.SnapshotIDs {
		if !kernel.ValidIdentifier(id) {
			return fail("a snapshot_id is not an identifier")
		}
	}
	return application.CostSourceDenial{EventID: string(envelope.ID), AttestationID: p.AttestationID, SourceID: p.SourceID,
		SnapshotIDs: p.SnapshotIDs}, nil
}
