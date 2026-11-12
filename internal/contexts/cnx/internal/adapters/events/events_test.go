package events

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-bento-go/eventing"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/internal/application"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
	"go.idp.xyz/idp-brant/internal/kernel"
	refapi "go.idp.xyz/idp-brant/internal/reference/api"
)

// Unit C4b-CNX (WBS S5-09b): the Inbox consumer cnx.trade_compliance (event storming PL-14; WF-06
// last step; SM-CNX-COMPLIANCE clear → reopen → pending; S5 D-9, D-18; MD-47; Context Map R46, R50).
// Python V0.50 has no CNX; the tests cite design IDs.

// follow records what the consumer asks of the application and answers err.
type follow struct {
	amended     []application.AmendedOrder
	bases       []application.OrderBasis
	regulations []application.PublishedRegulation
	lists       []application.ImportedList
	err         error
}

func (f *follow) PrepareAmendedOrder(_ context.Context, e application.AmendedOrder) (application.OrderBasis, error) {
	return application.OrderBasis{Order: &application.Order{Binding: e.Binding, Status: application.OrderActive}}, nil
}

func (f *follow) OpenOnAmendment(_ context.Context, _ bentopg.Executor, e application.AmendedOrder, b application.OrderBasis) (string, error) {
	f.amended, f.bases = append(f.amended, e), append(f.bases, b)
	return application.EffectCheckOpened, f.err
}

func (f *follow) PrepareRegulation(context.Context, application.PublishedRegulation) (application.RegulationBasis, error) {
	return application.RegulationBasis{}, nil
}

func (f *follow) ReopenOnRegulation(_ context.Context, _ bentopg.Executor, e application.PublishedRegulation, _ application.RegulationBasis) (string, []string, error) {
	f.regulations = append(f.regulations, e)
	return application.EffectReopened, nil, f.err
}

func (f *follow) ReopenOnProgrammes(_ context.Context, _ bentopg.Executor, e application.ImportedList) ([]string, error) {
	f.lists = append(f.lists, e)
	return nil, f.err
}

const eventID = "evt-0190a1b2-c3d4-7e5f-8a6b-0c1d2e3f4a5b"

// refusedAs reports whether err refuses the event for good under code.
func refusedAs(err error, code string) bool {
	return errors.Is(err, messaging.ErrEventRejected) && err != nil && strings.Contains(err.Error(), "("+code+")")
}

var hashA = strings.Repeat("a", 64)

func envelope(source string, typ eventing.EventType, p map[string]any, fields map[string]any) eventing.Envelope {
	for k, v := range fields {
		if v == nil {
			delete(p, k)
		} else {
			p[k] = v
		}
	}
	b, err := json.Marshal(p)
	if err != nil {
		panic(err)
	}
	at := time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	return eventing.Envelope{SpecVersion: eventing.SpecVersion, ID: eventID, Source: source, Type: typ, Version: 1, Scope: "tenant.1/company.A",
		Subject: "x", PartitionKey: "tenant.1/company.A/x", OccurredAt: at, RecordedAt: at, ContentType: eventing.JSONContentType, Payload: b}
}

// amended is TRD's OrderAmended as TRD's command ledger writes it: the ledger's members, the order
// binding of the amended revision and the amendment's members.
func amended(fields map[string]any) eventing.Envelope {
	return envelope(trdapi.Source, trdapi.EventOrderAmended, map[string]any{"tenant_id": "tenant.1", "company_id": "company.A",
		"aggregate_type": "SalesOrder", "aggregate_id": "order.1", "aggregate_version": 7, "actor_kind": "human", "actor_id": "reviewer",
		"order_id": "order.1", "order_revision": 2, "order_hash": hashA, "amendment_id": "amendment.1", "base_order_revision": 1,
		"from_order_revision": 1, "unchecked_guards": "ful.shipments"}, fields)
}

// published is POL's ConfigPublished as POL's command ledger writes it.
func published(fields map[string]any) eventing.Envelope {
	return envelope(polapi.Source, polapi.EventConfigPublished, map[string]any{"tenant_id": "tenant.1", "company_id": "company.A",
		"aggregate_type": "ConfigurationRoot", "aggregate_id": "screening_regulation:screening", "aggregate_version": 4,
		"kind": polapi.KindScreeningRegulation, "config_id": "screening", "revision": 2, "root_version": 4, "content_hash": hashA}, fields)
}

// imported is reference data's CountryProgrammesImported as its command ledger writes it.
func imported(fields map[string]any) eventing.Envelope {
	return envelope(refapi.Source, refapi.EventCountryProgrammesImported, map[string]any{"tenant_id": "tenant.1", "company_id": "company.A",
		"aggregate_type": "CountryProgrammeList", "aggregate_id": "cn.embargo", "aggregate_version": 2, "list_code": "cn.embargo",
		"version": "v2", "previous_version": "v1", "jurisdiction": "CN", "import_seq": 2}, fields)
}

// CNX declares one consumer, cnx.trade_compliance, with three subscriptions: TRD's OrderAmended
// (R46, PL-14; it reads TRD in its Prepare step), POL's ConfigPublished (R50, D-18; it reads the
// revision in its Prepare step) and reference data's CountryProgrammesImported (MD-47). Its system
// actor system:inbox.cnx.trade_compliance holds trade_compliance.reopen and cnx.read.
func TestTradeComplianceSubscriptions(t *testing.T) {
	subs := Subscriptions(nil, &follow{})
	if len(subs) != 3 {
		t.Fatalf("%d subscriptions", len(subs))
	}
	for i, want := range []struct {
		source  string
		typ     eventing.EventType
		prepare bool
	}{{trdapi.Source, trdapi.EventOrderAmended, true}, {polapi.Source, polapi.EventConfigPublished, true},
		{refapi.Source, refapi.EventCountryProgrammesImported, false}} {
		s := subs[i]
		if s.Consumer != "cnx.trade_compliance" || s.Source != want.source || !slices.Equal(s.EventTypes, []eventing.EventType{want.typ}) ||
			(s.Prepare != nil) != want.prepare || s.Handler == nil {
			t.Fatalf("subscription %d: %+v", i, s)
		}
	}
	grants, err := messaging.InboxGrants(subs)
	if err != nil || len(grants) != 1 || grants[0].Actor != (kernel.ActorRef{Kind: kernel.ActorSystem, ID: "inbox.cnx.trade_compliance"}) ||
		!slices.Equal(grants[0].Permissions, []string{"cnx.read", "trade_compliance.reopen"}) {
		t.Fatalf("grants %+v %v", grants, err)
	}
}

// OrderAmended reaches the application with the amended revision's binding and what Prepare read.
func TestAmendedOrderIsMapped(t *testing.T) {
	f := &follow{}
	env := amended(nil)
	prepared, err := AmendedOrderPrepare(f)(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if err = AmendedOrderHandler(f)(context.Background(), messaging.Event{Envelope: env, Prepared: prepared}); err != nil {
		t.Fatal(err)
	}
	if len(f.amended) != 1 || f.amended[0].EventID != eventID || f.amended[0].EventType != trdapi.EventOrderAmended ||
		f.amended[0].Binding.OrderID != "order.1" || f.amended[0].Binding.OrderRevision != 2 || f.amended[0].Binding.OrderHash != hashA ||
		f.bases[0].Order == nil {
		t.Fatalf("%+v %+v", f.amended, f.bases)
	}
	if err = AmendedOrderHandler(f)(context.Background(), messaging.Event{Envelope: env}); err == nil || errors.Is(err, messaging.ErrEventRejected) {
		t.Fatalf("without its prepared order: %v; want a retry", err)
	}
}

// POL's ConfigPublished and reference data's import reach the application with what they name.
func TestPublicationsAreMapped(t *testing.T) {
	f := &follow{}
	env := published(nil)
	prepared, err := RegulationPrepare(f)(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if err = RegulationHandler(f)(context.Background(), messaging.Event{Envelope: env, Prepared: prepared}); err != nil {
		t.Fatal(err)
	}
	want := polapi.ConfigRef{Kind: polapi.KindScreeningRegulation, ConfigID: "screening", Revision: 2}
	if len(f.regulations) != 1 || f.regulations[0].Ref != want || f.regulations[0].Kind != polapi.KindScreeningRegulation ||
		f.regulations[0].EventID != eventID {
		t.Fatalf("%+v", f.regulations)
	}
	if err = ProgrammesHandler(f)(context.Background(), messaging.Event{Envelope: imported(nil)}); err != nil {
		t.Fatal(err)
	}
	if len(f.lists) != 1 || f.lists[0].ListCode != "cn.embargo" || f.lists[0].Version != "v2" || f.lists[0].EventID != eventID {
		t.Fatalf("%+v", f.lists)
	}
	// Another kind is the application's to settle (nothing to reopen), not a malformed event.
	other := published(map[string]any{"kind": polapi.KindCreditPolicy, "aggregate_id": "credit_policy:credit.usd"})
	if err = RegulationHandler(f)(context.Background(), messaging.Event{Envelope: other, Prepared: application.RegulationBasis{}}); err != nil ||
		f.regulations[1].Kind != polapi.KindCreditPolicy {
		t.Fatalf("another kind: %v %+v", err, f.regulations)
	}
}

// A payload that is not its producer's is refused for good (cnx.event_invalid), before anything is
// read or written.
func TestMalformedEventsAreRefused(t *testing.T) {
	f := &follow{}
	refusedFor := func(name string, err error) {
		t.Helper()
		if !refusedAs(err, string(FailureEventInvalid)) {
			t.Errorf("%s: %v; want a rejection %s", name, err, FailureEventInvalid)
		}
	}
	for name, fields := range map[string]map[string]any{
		"not a SalesOrder":    {"aggregate_type": "Quote"},
		"order_id elsewhere":  {"order_id": "order.2"},
		"no aggregate":        {"aggregate_version": 0},
		"revision 1":          {"order_revision": 1},
		"no hash":             {"order_hash": nil},
		"hash in upper case":  {"order_hash": strings.Repeat("A", 64)},
		"order_id not an id":  {"order_id": "a b", "aggregate_id": "a b"},
		"revision not number": {"order_revision": "2"},
	} {
		env := amended(fields)
		_, err := AmendedOrderPrepare(f)(context.Background(), env)
		refusedFor("prepare "+name, err)
		refusedFor("handle "+name, AmendedOrderHandler(f)(context.Background(), messaging.Event{Envelope: env, Prepared: application.OrderBasis{}}))
	}
	for name, fields := range map[string]map[string]any{
		"not a root":      {"aggregate_type": "ConfigRevision"},
		"no kind":         {"kind": nil},
		"no config":       {"config_id": ""},
		"revision 0":      {"revision": 0},
		"revision string": {"revision": "2"},
	} {
		env := published(fields)
		_, err := RegulationPrepare(f)(context.Background(), env)
		refusedFor("prepare "+name, err)
		refusedFor("handle "+name, RegulationHandler(f)(context.Background(), messaging.Event{Envelope: env, Prepared: application.RegulationBasis{}}))
	}
	for name, fields := range map[string]map[string]any{
		"not a list":    {"aggregate_type": "CountryProgrammeVersion"},
		"no list code":  {"list_code": nil},
		"long list":     {"list_code": strings.Repeat("l", 33)},
		"no version":    {"version": ""},
		"version space": {"version": "v 2"},
	} {
		refusedFor("handle "+name, ProgrammesHandler(f)(context.Background(), messaging.Event{Envelope: imported(fields)}))
	}
	if len(f.amended)+len(f.regulations)+len(f.lists) != 0 {
		t.Fatalf("malformed events reached the application: %d %d %d", len(f.amended), len(f.regulations), len(f.lists))
	}
}

// A *FollowMismatch refuses the event for good with its code (dead letter and alert at once,
// ADR-BR-015); any other failure — the actor not provisioned, a conflict, the database — is
// delivered again.
func TestMismatchRefusesAndOtherFailuresRetry(t *testing.T) {
	mismatch := &application.FollowMismatch{Code: application.FailureOrderNotCheckable, Detail: "TRD shows no order order.1"}
	for _, c := range []struct {
		err    error
		refuse string
	}{
		{mismatch, application.FailureOrderNotCheckable},
		{&application.FollowMismatch{Code: application.FailureReopenRefused, Detail: "x"}, application.FailureReopenRefused},
		{kernel.Reject(kernel.CodePermissionDenied, "", ""), ""},
		{errors.New("connection reset"), ""},
	} {
		f := &follow{err: c.err}
		for name, err := range map[string]error{
			"amended":    AmendedOrderHandler(f)(context.Background(), messaging.Event{Envelope: amended(nil), Prepared: application.OrderBasis{}}),
			"regulation": RegulationHandler(f)(context.Background(), messaging.Event{Envelope: published(nil), Prepared: application.RegulationBasis{}}),
			"programmes": ProgrammesHandler(f)(context.Background(), messaging.Event{Envelope: imported(nil)}),
		} {
			switch {
			case c.refuse != "" && !refusedAs(err, c.refuse):
				t.Errorf("%s with %v: %v; want refused as %s", name, c.err, err, c.refuse)
			case c.refuse == "" && (errors.Is(err, messaging.ErrEventRejected) || !errors.Is(err, c.err)):
				t.Errorf("%s with %v: %v; want the error to retry", name, c.err, err)
			}
		}
	}
}
