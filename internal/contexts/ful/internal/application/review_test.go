package application

import (
	"errors"
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// SM-FUL-SHIPMENT-REVIEW: shipment_review.open is the system's (the Inbox consumer), never a
// person's or an agent's; shipment_review.settle is in humanOnlyCommands; the reads need ful.read.
func TestReviewRules(t *testing.T) {
	policy, err := authorization.NewPolicy(Rules()...)
	if err != nil {
		t.Fatal(err)
	}
	scope := kernel.Scope{Tenant: "tenant.1", Company: "company.A"}
	for _, c := range []struct {
		command string
		kind    kernel.ActorKind
		ok      bool
	}{
		{api.CommandOpenShipmentReview, kernel.ActorSystem, true},
		{api.CommandOpenShipmentReview, kernel.ActorHuman, false},
		{api.CommandOpenShipmentReview, kernel.ActorAI, false},
		{api.CommandSettleShipmentReview, kernel.ActorHuman, true},
		{api.CommandSettleShipmentReview, kernel.ActorAI, false},
		{api.CommandSettleShipmentReview, kernel.ActorSystem, false},
		{api.CommandGetShipmentReview, kernel.ActorAI, true},
		{api.CommandListShipmentReviews, kernel.ActorSystem, true},
	} {
		p := identity.Principal{Actor: kernel.ActorRef{Kind: c.kind, ID: "actor.1"}, Scope: scope}
		if _, err := policy.Admit(p, c.command); (err == nil) != c.ok {
			t.Errorf("%s by %s: %v", c.command, c.kind, err)
		}
	}
	for _, c := range []struct{ command, permission string }{
		{api.CommandOpenShipmentReview, "shipment_review.open"}, {api.CommandSettleShipmentReview, "shipment_review.settle"},
		{api.CommandGetShipmentReview, api.PermissionRead}, {api.CommandListShipmentReviews, api.PermissionRead},
	} {
		if r, err := policy.Rule(c.command); err != nil || r.Permission != c.permission {
			t.Errorf("%s: %+v %v", c.command, r, err)
		}
	}
	a := api.SettleShipmentReview{IdempotencyKey: "k1", ReviewID: "shr.1", ExpectedVersion: 1, Items: []api.ReviewItemDisposition{{ItemNo: 1,
		Disposition: api.DispositionShipmentCancelled, Reason: "x"}}}
	b := a
	b.IdempotencyKey, b.ExpectedVersion = "k2", 2
	if requestHash(a) != requestHash(b) || len(requestHash(a)) != 64 {
		t.Fatal("neither the key nor the expected version is part of a settlement")
	}
	b.Items = []api.ReviewItemDisposition{{ItemNo: 1, Disposition: api.DispositionReplaced, ShipmentID: "shipment.3", Reason: "x"}}
	if requestHash(a) == requestHash(b) {
		t.Fatal("the dispositions are")
	}
	if name, key, _ := request(api.OpenShipmentReview{IdempotencyKey: "evt.1"}); name != api.CommandOpenShipmentReview || key != "evt.1" {
		t.Fatalf("open: %s %s", name, key)
	}
}

// What an order change asks of TRD (WF-06 step 5 and its failure mode): an amendment reviews TRD's
// lines of the amended revision while TRD is at it; TRD at a later revision leaves it to that
// revision's event; no order, an earlier revision or another hash is a mismatch nothing retried
// changes. An end asks nothing of TRD.
func TestReviewTargets(t *testing.T) {
	hash := strings.Repeat("a", 64)
	e := OrderChange{EventID: "evt.1", Cause: api.CauseOrderAmended, OrderID: "order.1", Revision: 2, Hash: hash, Version: 5}
	lines := []api.ReviewTarget{{OrderLineID: "L1", Quantity: "110", SkuID: "sku.1", SpecRevision: 1, UnitID: "PCS"}}
	got, effect, err := targets(e, ReviewBasis{Known: true, Order: OrderFact{Revision: 2, Hash: hash, Lines: lines}})
	if err != nil || effect != "" || len(got) != 1 || got[0] != lines[0] {
		t.Fatalf("at the amended revision: %+v %s %v", got, effect, err)
	}
	if _, effect, err = targets(e, ReviewBasis{Known: true, Order: OrderFact{Revision: 3, Hash: "b"}}); effect != EffectLaterRevision || err != nil {
		t.Fatalf("later revision: %s %v", effect, err)
	}
	var mismatch *ReviewMismatch
	for what, b := range map[string]ReviewBasis{"no order": {}, "earlier revision": {Known: true, Order: OrderFact{Revision: 1, Hash: hash}},
		"another hash": {Known: true, Order: OrderFact{Revision: 2, Hash: strings.Repeat("c", 64)}}} {
		if _, _, err := targets(e, b); !errors.As(err, &mismatch) || mismatch.Code != FailureReviewNotOpenable {
			t.Errorf("%s: %v", what, err)
		}
	}
	e.Cause = api.CauseOrderRemainderClosed
	if got, effect, err := targets(e, ReviewBasis{}); got != nil || effect != "" || err != nil {
		t.Fatalf("an end: %+v %s %v", got, effect, err)
	}
	c, err := change(api.OpenShipmentReview{OrderID: "order.1", OrderRevision: 2, OrderHash: hash, OrderVersion: 5, Cause: api.CauseOrderAmended,
		EventID: "evt.1", Lines: lines})
	if err != nil || c.Targets["L1"].Quantity.String() != "110" || c.Targets["L1"].UnitID != "PCS" || c.Version != 5 {
		t.Fatalf("change %+v %v", c, err)
	}
}
