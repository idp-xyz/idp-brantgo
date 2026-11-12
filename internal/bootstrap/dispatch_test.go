package bootstrap

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.idp.xyz/idp-bento-go/eventing"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
)

// allowlistPath is the cross-context allowlist tools/dependency-check generates from the Context
// Map (make allowlist).
const allowlistPath = "../../tools/dependency-check/allowlist.txt"

// unconnectedContexts composes every context on a pool that never connects: module construction
// touches no database, so the subscriptions can be checked without one.
func unconnectedContexts(t *testing.T) *Contexts {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://unit:unit@127.0.0.1:1/unit?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	c, err := (&Platform{pool: pool, schema: bentopg.DefaultSchema}).Contexts()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// readAllowlist returns the "downstream upstream" pairs of the generated allowlist.
func readAllowlist(t *testing.T) map[string]bool {
	t.Helper()
	text, err := os.ReadFile(allowlistPath)
	if err != nil {
		t.Fatal(err)
	}
	allow := map[string]bool{}
	for _, line := range strings.Split(string(text), "\n") {
		if f := strings.Fields(line); len(f) == 2 && !strings.HasPrefix(f[0], "#") {
			allow[f[0]+" "+f[1]] = true
		}
	}
	if len(allow) == 0 {
		t.Fatalf("%s lists no relationships", allowlistPath)
	}
	return allow
}

// contextMapViolations reports every subscription whose source is not an upstream of its
// consumer's context in allow (rule D1: a context subscribes only to the events of its registered
// upstreams).
func contextMapViolations(subs []messaging.Subscription, allow map[string]bool) []string {
	var out []string
	for _, s := range subs {
		down, up := messaging.ConsumerContext(s.Consumer), messaging.SourceContext(s.Source)
		if down == "" || up == "" || !allow[down+" "+up] {
			out = append(out, fmt.Sprintf("%s subscribes to %s, which is not an upstream of %q in %s", s.Consumer, s.Source, down, allowlistPath))
		}
	}
	return out
}

// Every subscription of the composed contexts follows the Context Map: its source is an upstream
// of the consuming context in the allowlist generated from it, and the dispatcher accepts them.
func TestSubscriptionsFollowTheContextMap(t *testing.T) {
	c := unconnectedContexts(t)
	subs := c.Subscriptions()
	for _, v := range contextMapViolations(subs, readAllowlist(t)) {
		t.Error(v)
	}
	if _, err := c.Dispatcher(); err != nil {
		t.Fatal(err)
	}
	if _, err := messaging.InboxGrants(subs); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d subscriptions", len(subs))
}

// The check itself refuses what the Context Map does not allow: a source that is no upstream of
// the consumer (PUR is downstream of TRD, never the reverse), a source outside Brant's naming and
// a consumer of a context the allowlist does not know.
func TestContextMapCheckRefusesSubscriptionsAgainstTheMap(t *testing.T) {
	allow := readAllowlist(t)
	sub := func(consumer eventing.InboxConsumer, source string) messaging.Subscription {
		return messaging.Subscription{Consumer: consumer, Source: source}
	}
	if v := contextMapViolations([]messaging.Subscription{sub("pur.order_review", trdapi.Source), sub("trd.cost_source_holds", cstapi.Source)}, allow); len(v) != 0 {
		t.Fatalf("allowed subscriptions refused: %v", v)
	}
	for _, s := range []messaging.Subscription{
		sub("inq.quote_events", "idp-brant.trd"), sub("pur.probe", "trd"), sub("zzz.probe", trdapi.Source), sub("probe", trdapi.Source),
	} {
		if v := contextMapViolations([]messaging.Subscription{s}, allow); len(v) != 1 {
			t.Errorf("%s from %s: %v, want one violation", s.Consumer, s.Source, v)
		}
	}
}

// policyConsumers are the event-driven policies that need an Inbox consumer: S5's PL-09, PL-12,
// PL-13 and PL-14 (event storming; S5 plan S4-06a) and S4's WF-03 and R65 (S4 plan S4-06b, c).
// Each event type needs a subscription of the consuming context, or the reason it has none yet;
// a reason that a subscription made stale fails too, so the list shrinks as consumers arrive.
var policyConsumers = []struct {
	policy   string
	consumer string // context code
	source   string
	types    []eventing.EventType
	pending  string // why there is no subscription yet; empty once there must be one
}{
	{"PL-12 OrderActivated → STL consumes the credit reservation", "stl", trdapi.Source,
		[]eventing.EventType{trdapi.EventOrderActivated}, ""},
	{"PL-13 order ends → STL releases the credit reservation", "stl", trdapi.Source,
		[]eventing.EventType{trdapi.EventOrderCancelled, trdapi.EventOrderClosedUnfulfilled, trdapi.EventOrderClosed,
			trdapi.EventOrderRemainderClosed}, ""},
	{"PL-09 OrderAmended → PUR reviews allocations and commitments", "pur", trdapi.Source,
		[]eventing.EventType{trdapi.EventOrderAmended, trdapi.EventOrderCancelled, trdapi.EventOrderRemainderClosed}, ""},
	{"PL-09 OrderAmended → FUL reviews packing, booking and documents", "ful", trdapi.Source,
		[]eventing.EventType{trdapi.EventOrderAmended}, "FUL is not built yet (S5-09b, unit C4b)"},
	{"PL-09 OrderAmended → STL reviews the payment plan", "stl", trdapi.Source,
		[]eventing.EventType{trdapi.EventOrderAmended}, "S5-09b, unit C4b"},
	{"PL-14 OrderAmended → CNX re-checks trade compliance", "cnx", trdapi.Source,
		[]eventing.EventType{trdapi.EventOrderAmended}, "CNX is not built yet (S5-09b, unit C4b)"},
	{"WF-03 CostSourceDenied → TRD registers the risk hold", "trd", cstapi.Source,
		[]eventing.EventType{cstapi.EventCostSourceDenied}, "S4-06b (S4 plan unit B3)"},
	{"R65 QuoteActivated, QuoteReplaced → EVD registers the formal file", "evd", trdapi.Source,
		[]eventing.EventType{trdapi.EventQuoteActivated, trdapi.EventQuoteReplaced}, "S4-06c (S4 plan unit C4)"},
}

func TestEventPoliciesHaveConsumers(t *testing.T) {
	subs := unconnectedContexts(t).Subscriptions()
	subscribed := func(bc, source string, typ eventing.EventType) bool {
		return slices.ContainsFunc(subs, func(s messaging.Subscription) bool {
			return messaging.ConsumerContext(s.Consumer) == bc && s.Source == source && slices.Contains(s.EventTypes, typ)
		})
	}
	for _, p := range policyConsumers {
		for _, typ := range p.types {
			switch has := subscribed(p.consumer, p.source, typ); {
			case !has && p.pending == "":
				t.Errorf("%s: no %s consumer subscribes to %s", p.policy, p.consumer, typ)
			case has && p.pending != "":
				t.Errorf("%s: %s subscribes to %s now; remove the pending reason %q", p.policy, p.consumer, typ, p.pending)
			case !has:
				t.Logf("%s: %s pending, %s", p.policy, typ, p.pending)
			}
		}
	}
}

type subscribingModule struct{ subs []messaging.Subscription }

func (m *subscribingModule) Subscriptions() []messaging.Subscription { return m.subs }

type silentModule struct{}

// Subscriptions are collected from every module that declares them, in field order; modules
// without subscriptions and nil modules are skipped.
func TestCollectSubscriptionsFromEveryModule(t *testing.T) {
	a := &subscribingModule{subs: []messaging.Subscription{{Consumer: "pur.a"}, {Consumer: "pur.b"}}}
	b := &subscribingModule{subs: []messaging.Subscription{{Consumer: "stl.c"}}}
	modules := struct {
		A      *subscribingModule
		Silent *silentModule
		Nil    *subscribingModule
		B      *subscribingModule
		hidden *subscribingModule
	}{A: a, Silent: &silentModule{}, B: b, hidden: &subscribingModule{subs: []messaging.Subscription{{Consumer: "cnx.d"}}}}
	var got []eventing.InboxConsumer
	for _, s := range collectSubscriptions(&modules) {
		got = append(got, s.Consumer)
	}
	if want := []eventing.InboxConsumer{"pur.a", "pur.b", "stl.c"}; !slices.Equal(got, want) {
		t.Fatalf("collected %v, want %v", got, want)
	}
}

// The relay and the replayer publish to exactly one of an HTTP sink and a publisher.
func TestRelayAndReplayerNeedExactlyOnePublisher(t *testing.T) {
	d, err := messaging.NewDispatcher()
	if err != nil {
		t.Fatal(err)
	}
	for _, cfg := range []RelayConfig{{}, {SinkURL: "http://sink", Publisher: d}} {
		if _, err := (&Platform{}).Relay(cfg); err == nil {
			t.Errorf("Relay(%+v) must fail", cfg)
		}
	}
	for _, cfg := range []ReplayConfig{{PublishTimeout: time.Second}, {SinkURL: "http://sink", Publisher: d, PublishTimeout: time.Second}, {Publisher: d}} {
		if _, err := (&Platform{}).Replayer(cfg); err == nil {
			t.Errorf("Replayer(%+v) must fail", cfg)
		}
	}
	for _, p := range []struct {
		sink      string
		publisher eventing.Publisher
	}{{"http://sink", nil}, {"", d}} {
		if got, err := publisherFor(p.sink, p.publisher); err != nil || got == nil {
			t.Errorf("publisherFor(%q, %v) = %v, %v", p.sink, p.publisher, got, err)
		}
	}
}
