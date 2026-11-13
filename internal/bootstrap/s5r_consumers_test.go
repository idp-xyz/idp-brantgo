package bootstrap

import (
	"encoding/json"
	"os"
	"slices"
	"testing"

	"go.idp.xyz/idp-bento-go/eventing"
	"go.idp.xyz/idp-brant/internal/ai/capability"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
)

// contextMapPath is the archive's Context Map, the source of the allowlist and of the consumers'
// registry (S5R-D43: Context Map "consumers").
const contextMapPath = "../../docs/03_STRATEGIC_DDD/01_IDP_Brant_Strategic_Context_Map_V0.1_Draft.json"

// mapConsumer is one entry of the Context Map's consumers: a system actor the Inbox dispatcher
// runs, the events it subscribes and the permissions brant-admin inbox grants it.
type mapConsumer struct {
	ID          string   `json:"id"`
	SystemActor string   `json:"systemActor"`
	Subscribes  []string `json:"subscribes"`
	Permissions []string `json:"permissions"`
}

func readMapConsumers(t *testing.T) []mapConsumer {
	t.Helper()
	raw, err := os.ReadFile(contextMapPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Consumers []mapConsumer `json:"consumers"`
	}
	if err = json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Consumers) == 0 {
		t.Fatalf("%s registers no consumers", contextMapPath)
	}
	return doc.Consumers
}

// S5R-D02, S5R-D29 (unit R1): what brant-admin inbox grants each consumer of the composed contexts
// (messaging.InboxGrants of their subscriptions) is what the Context Map's consumers registry
// declares for it, with its system actor and the events it subscribes. So
// system:inbox.stl.order_account holds order_account.open (D02) besides payment_plan_review.open and
// stl.read, and the review openings shipment_review.open and allocation_review.open, which no role
// template holds any more (D29), are granted to their consumers. A consumer the code declares but
// the registry does not list yet (S4's, registered by E2) is logged, not failed.
func TestS5RConsumerGrantsAreTheContextMapsConsumers(t *testing.T) {
	subs := unconnectedContexts(t).Subscriptions()
	grants, err := messaging.InboxGrants(subs)
	if err != nil {
		t.Fatal(err)
	}
	types := map[eventing.InboxConsumer][]string{}
	for _, s := range subs {
		for _, typ := range s.EventTypes {
			types[s.Consumer] = append(types[s.Consumer], string(typ))
		}
	}
	registered := map[eventing.InboxConsumer]bool{}
	for _, m := range readMapConsumers(t) {
		consumer := eventing.InboxConsumer(m.ID)
		registered[consumer] = true
		i := slices.IndexFunc(grants, func(g messaging.InboxGrant) bool { return g.Consumer == consumer })
		if i < 0 {
			t.Errorf("the Context Map registers consumer %s, which no composed context declares", m.ID)
			continue
		}
		g := grants[i]
		if g.Actor.String() != m.SystemActor {
			t.Errorf("%s runs as %s, the Context Map says %s", m.ID, g.Actor, m.SystemActor)
		}
		if want := slices.Sorted(slices.Values(m.Permissions)); !slices.Equal(g.Permissions, want) {
			t.Errorf("%s is granted %v, the Context Map says %v", m.ID, g.Permissions, want)
		}
		got := slices.Compact(slices.Sorted(slices.Values(types[consumer])))
		if want := slices.Sorted(slices.Values(m.Subscribes)); !slices.Equal(got, want) {
			t.Errorf("%s subscribes %v, the Context Map says %v", m.ID, got, want)
		}
	}
	for _, g := range grants {
		if !registered[g.Consumer] {
			t.Logf("consumer %s (%s) is not in the Context Map's consumers yet", g.Consumer, g.Actor)
		}
	}
}

// S5R-D29 and role templates principle 5: a permission that only a consumer's commands need — the
// review openings — is in no role template, and a consumer's grant holds no human-only command
// (its commands run as a system actor). Commands a person may also issue (payment_plan_review.open,
// order_account.open, trade_compliance.reopen) stay in their templates.
func TestS5RSystemOnlyPermissionsAreInNoTemplate(t *testing.T) {
	for _, p := range []string{"shipment_review.open", "allocation_review.open"} {
		for _, tpl := range authorization.Templates() {
			if slices.Contains(tpl.Permissions, p) {
				t.Errorf("template %s holds %s, which only system:inbox consumers hold (S5R-D29)", tpl.Code, p)
			}
		}
		if !slices.ContainsFunc(readMapConsumers(t), func(m mapConsumer) bool { return slices.Contains(m.Permissions, p) }) {
			t.Errorf("no consumer of the Context Map holds %s", p)
		}
	}
	grants, err := messaging.InboxGrants(unconnectedContexts(t).Subscriptions())
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range grants {
		for _, p := range g.Permissions {
			if capability.IsHumanOnly(p) {
				t.Errorf("%s is granted %s, a human-only command", g.Actor, p)
			}
		}
	}
}
