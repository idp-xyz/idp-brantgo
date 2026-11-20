package events

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"go.idp.xyz/idp-bento-go/eventing"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/application"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
)

// Unit B3 (WBS S4-06b): the Inbox consumer trd.cost_source_holds (Context Map R11; WF-03, PL-07,
// SC-04; S4 decision D-9). Python V0.50 has no consumer of CST's denials; the tests cite design IDs.

type denials struct {
	got []application.CostSourceDenial
	err error
}

func (d *denials) ApplyCostSourceDenial(_ context.Context, _ bentopg.Executor, x application.CostSourceDenial) (application.CostDenialEffect, error) {
	d.got = append(d.got, x)
	return application.CostDenialEffect{}, d.err
}

// denial is CST's CostSourceDenied as costing.attest_source's ledger writes it, with fields changed
// (nil deletes one).
func denial(fields map[string]any) messaging.Event {
	p := map[string]any{"tenant_id": "tenant.1", "company_id": "company.A", "aggregate_type": "CostSnapshot", "aggregate_id": "src.1",
		"aggregate_version": 2, "actor_kind": "human", "actor_id": "reviewer", "authority_epoch": 3, "action_id": "act-1",
		"attestation_id": "denial.1", "source_id": "src.1", "snapshot_ids": []string{"snapshot.1", "snapshot.2"}}
	for k, v := range fields {
		if v == nil {
			delete(p, k)
			continue
		}
		p[k] = v
	}
	body, _ := json.Marshal(p)
	return messaging.Event{Consumer: CostSourceHolds, Envelope: eventing.Envelope{ID: "evt-1", Source: cstapi.Source,
		Type: cstapi.EventCostSourceDenied, Payload: body}}
}

func TestSubscription(t *testing.T) {
	subs := Subscriptions(nil, &denials{})
	if len(subs) != 1 {
		t.Fatalf("%d subscriptions", len(subs))
	}
	s := subs[0]
	if s.Consumer != "trd.cost_source_holds" || s.Source != "idp-brant.cst" ||
		!slices.Equal(s.EventTypes, []eventing.EventType{"idp-brant.cst.cost_source_denied.v1"}) || s.Prepare != nil ||
		!slices.Equal(s.Permissions, []string{"quote.risk_hold.register", "order.risk.record", "trd.read"}) {
		t.Fatalf("%+v", s)
	}
	if messaging.ConsumerContext(s.Consumer) != "trd" || messaging.SourceContext(s.Source) != "cst" {
		t.Fatalf("%s from %s", s.Consumer, s.Source)
	}
}

// The handler passes the denial on as the event names it; a denial naming no snapshot is passed on
// too (nothing rested on the source). A payload that is not a denial's is refused for good, without
// asking the application; the application's failures are delivered again.
func TestDenialsReachTheApplication(t *testing.T) {
	d := &denials{}
	h := CostSourceHandler(d)
	if err := h(context.Background(), denial(nil)); err != nil {
		t.Fatal(err)
	}
	if err := h(context.Background(), denial(map[string]any{"snapshot_ids": nil})); err != nil {
		t.Fatal(err)
	}
	if len(d.got) != 2 || d.got[0].EventID != "evt-1" || d.got[0].AttestationID != "denial.1" || d.got[0].SourceID != "src.1" ||
		!slices.Equal(d.got[0].SnapshotIDs, []string{"snapshot.1", "snapshot.2"}) || len(d.got[1].SnapshotIDs) != 0 {
		t.Fatalf("%+v", d.got)
	}
	for name, fields := range map[string]map[string]any{
		"another aggregate":           {"aggregate_type": "CostScenario"},
		"the source is not the root":  {"aggregate_id": "src.2"},
		"no source":                   {"source_id": nil},
		"no attestation":              {"attestation_id": nil},
		"an attestation not an ID":    {"attestation_id": "denial 1"},
		"a snapshot not an ID":        {"snapshot_ids": []string{"snapshot.1", ""}},
		"snapshots not a list":        {"snapshot_ids": "snapshot.1"},
		"a source that is not an ID":  {"source_id": "../src", "aggregate_id": "../src"},
		"an attestation of a number":  {"attestation_id": 7},
		"snapshot_ids of other types": {"snapshot_ids": []int{1}},
	} {
		before := len(d.got)
		err := h(context.Background(), denial(fields))
		if !errors.Is(err, messaging.ErrEventRejected) || !strings.Contains(err.Error(), string(FailureDenialInvalid)) || len(d.got) != before {
			t.Errorf("%s: %v", name, err)
		}
	}
	e := denial(nil)
	e.Envelope.Type = cstapi.EventCostSnapshotFrozen
	if err := h(context.Background(), e); !errors.Is(err, messaging.ErrEventRejected) {
		t.Errorf("another event type: %v", err)
	}
	cause := errors.New("database down")
	if err := CostSourceHandler(&denials{err: cause})(context.Background(), denial(nil)); !errors.Is(err, cause) ||
		errors.Is(err, messaging.ErrEventRejected) {
		t.Fatalf("an application failure: %v", err)
	}
}
