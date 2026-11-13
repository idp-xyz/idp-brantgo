package release

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipment"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The CommercialRelease rules (tactical model FUL-AGG-CommercialRelease, SM-FUL-RELEASE,
// FUL-INV-002; SC-08, TD-AT08, BT13; S5 decision D-11). Python V0.50 has no commercial release
// ("—（Python 未实现）"): the vectors are SC-08's (70 % before shipment, 672.00 due, 670.00 applied,
// 2.00 short).

var (
	t0      = time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	doc     = Act{By: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "doc.1"}, Epoch: 3, At: t0}
	manager = Act{By: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "manager.1"}, Epoch: 3, At: t0.Add(time.Minute)}
	agent   = Act{By: kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.1"}, Epoch: 3, At: t0}
	order   = shipment.OrderBinding{OrderID: "order.1", OrderRevision: 2, OrderHash: strings.Repeat("a", 64)}
)

func code(err error) string {
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code
	}
	if err != nil {
		return err.Error()
	}
	return "OK"
}

// unpaid is SC-08 before the 2.00 is resolved: P1 288.00 applied, P2 670.00 of 672.00.
func unpaid() PaymentVerdict {
	return PaymentVerdict{Scope: ScopeShipment, Status: "blocked", Reason: PaymentStageUnpaid, OrderRevision: 2, OrderHash: order.OrderHash,
		Currency: "USD", RequiredMinor: "96000", AppliedMinor: "95800", StageID: "P2", StageTrigger: "before_shipment", StageDueMinor: "67200",
		StageAppliedMinor: "67000", ShortfallMinor: "200"}
}

func paid() PaymentVerdict {
	p := unpaid()
	p.Satisfied, p.Status, p.Reason, p.AppliedMinor, p.StageID, p.ShortfallMinor = true, "satisfied", "", "96000", "", ""
	return p
}

func blocked(code string) *kernel.Rejection { return Reject(code, "/args/shipment_id", "because") }

func evaluated(t *testing.T, failure *kernel.Rejection) *Release {
	t.Helper()
	r := New("rls.1", "shipment.1", ScopeShipment, order, doc)
	r.Evaluate(Verdict{Failure: failure, Guard: shipment.GuardPaymentCondition}, doc)
	return r
}

func exceptionRequest(r *Release) ExceptionRequest {
	return ExceptionRequest{ExpectedVersion: r.RowVersion, StageID: "P2", Currency: "USD", ShortfallMinor: "200", ValidUntil: t0.Add(72 * time.Hour),
		Reason: "客户书面承诺尾款差额随下批支付"}
}

// SM-FUL-RELEASE: the first evaluation opens the release at version 1, blocked or granted; each
// evaluation moves it by one and records its outcome; only a move to granted reports granted; a
// consumed release is never evaluated again.
func TestEvaluationsMoveTheRelease(t *testing.T) {
	r := New("rls.1", "shipment.1", ScopeShipment, order, doc)
	if r.Status != StatusBlocked || r.RowVersion != 0 || r.Latest() != nil {
		t.Fatalf("new %+v", r)
	}
	if r.Evaluate(Verdict{Failure: blocked(shipment.CodeCommercialReleaseBlocked), Guard: shipment.GuardPaymentCondition}, doc) {
		t.Fatal("a blocked evaluation grants nothing")
	}
	e := r.Latest()
	if r.Status != StatusBlocked || r.RowVersion != 1 || e.Seq != 1 || e.ReleaseVersion != 1 || e.Outcome != OutcomeBlocked ||
		e.Code != shipment.CodeCommercialReleaseBlocked || e.Guard != shipment.GuardPaymentCondition || e.Reason != "because" ||
		e.EntriesJSON != "[]" || len(e.EntriesSHA256) != 64 {
		t.Fatalf("blocked %+v %+v", r, e)
	}
	entries := []Entry{{Name: shipment.GuardQualityRelease, Value: "granted"}}
	if !r.Evaluate(Verdict{Entries: entries, ExceptionSeq: 0}, doc) || r.Status != StatusGranted || r.RowVersion != 2 {
		t.Fatalf("granted %+v", r)
	}
	if r.Evaluate(Verdict{Entries: entries}, agent) || r.Status != StatusGranted || r.RowVersion != 3 || r.Latest().Evaluated.By != agent.By {
		t.Fatalf("granted again %+v", r)
	}
	r.Evaluate(Verdict{Failure: blocked(shipment.CodeQualityReleaseRequired), Guard: shipment.GuardQualityRelease}, doc)
	if r.Status != StatusBlocked || r.RowVersion != 4 || len(r.Evaluations) != 4 || r.Latest().Code != shipment.CodeQualityReleaseRequired {
		t.Fatalf("blocked again %+v", r)
	}
	if r.Evaluable() != nil {
		t.Fatal("a blocked release is evaluated")
	}
	r.Status = StatusConsumed
	if code(r.Evaluable()) != kernel.CodeInvalidState {
		t.Fatal("a consumed release is terminal")
	}
}

// STL-INV-003, SC-08: 670.00 applied of 672.00 is COMMERCIAL_RELEASE_BLOCKED; the paid condition
// passes; a live exception of the release covers the same stage of the same revision while the
// scope is no more short than it was granted for, and nothing else.
func TestPaymentConditionAndItsException(t *testing.T) {
	var none *Release
	if _, _, err := none.CheckPayment(paid(), t0, "/x"); err != nil {
		t.Fatal(err)
	}
	e, x, err := none.CheckPayment(unpaid(), t0, "/args/shipment_id")
	if code(err) != shipment.CodeCommercialReleaseBlocked || x != 0 || e.Name != shipment.GuardPaymentCondition ||
		e.Value != "blocked/stage_unpaid/r2/USD/applied=95800/required=96000/stage=P2/shortfall=200" {
		t.Fatalf("%+v %d %v", e, x, err)
	}
	if r, _ := kernel.AsRejection(err); !strings.Contains(r.Fields[0].Reason, "P2 (before_shipment) has 67000 of 67200 USD applied: 200 short") {
		t.Fatal(r.Fields)
	}
	r := evaluated(t, blocked(shipment.CodeCommercialReleaseBlocked))
	if _, err = r.GrantException(exceptionRequest(r), nil, unpaid(), nil, manager); err != nil {
		t.Fatal(err)
	}
	now := t0.Add(time.Hour)
	if e, x, err := r.CheckPayment(unpaid(), now, "/x"); err != nil || x != 1 || !strings.HasSuffix(e.Value, "/exception=1") {
		t.Fatalf("covered %+v %d %v", e, x, err)
	}
	for name, p := range map[string]func(*PaymentVerdict){
		"a larger shortfall": func(p *PaymentVerdict) {
			p.AppliedMinor, p.StageAppliedMinor, p.ShortfallMinor = "95700", "66900", "300"
		},
		"another stage":    func(p *PaymentVerdict) { p.StageID = "P1" },
		"another revision": func(p *PaymentVerdict) { p.OrderRevision = 3 },
		"another currency": func(p *PaymentVerdict) { p.Currency = "EUR" },
		"no account":       func(p *PaymentVerdict) { p.Reason = "no_account" },
	} {
		v := unpaid()
		p(&v)
		if _, _, err := r.CheckPayment(v, now, "/x"); code(err) != shipment.CodeCommercialReleaseBlocked {
			t.Errorf("%s: %v", name, err)
		}
	}
	smaller := unpaid()
	smaller.AppliedMinor, smaller.ShortfallMinor = "95900", "100"
	if _, x, err := r.CheckPayment(smaller, now, "/x"); err != nil || x != 1 {
		t.Fatalf("a smaller shortfall stays covered: %v", err)
	}
	if _, _, err := r.CheckPayment(unpaid(), t0.Add(72*time.Hour), "/x"); code(err) != shipment.CodeCommercialReleaseBlocked {
		t.Fatalf("an expired exception covers nothing: %v", err)
	}
}

// FUL-INV-002, BT13, role template credit_manager: an exception grants a blocked release for one
// unpaid stage exactly as STL states it, by a human, and never when another guard is unmet — that
// guard's own code refuses it.
func TestExceptionsCoverOneUnpaidStageOnly(t *testing.T) {
	r := evaluated(t, blocked(shipment.CodeCommercialReleaseBlocked))
	req := exceptionRequest(r)
	two := unpaid()
	two.AppliedMinor = "67000"
	for _, c := range []struct {
		name  string
		edit  func(*ExceptionRequest)
		other *kernel.Rejection
		p     PaymentVerdict
		act   Act
		want  string
	}{
		{"quality never", nil, blocked(shipment.CodeQualityReleaseRequired), unpaid(), manager, shipment.CodeQualityReleaseRequired},
		{"screening never", nil, blocked(shipment.CodePartyScreeningNotClear), unpaid(), manager, shipment.CodePartyScreeningNotClear},
		{"compliance never", nil, blocked(shipment.CodeTradeComplianceNotClear), unpaid(), manager, shipment.CodeTradeComplianceNotClear},
		{"licence never", nil, blocked(shipment.CodeExportLicenseRequired), unpaid(), manager, shipment.CodeExportLicenseRequired},
		{"an agent", nil, nil, unpaid(), agent, kernel.CodePermissionDenied},
		{"paid", nil, nil, paid(), manager, kernel.CodeInvalidState},
		{"no account", nil, nil, func() PaymentVerdict { p := unpaid(); p.Reason = "no_account"; return p }(), manager,
			shipment.CodeCommercialReleaseBlocked},
		{"two stages unpaid", nil, nil, two, manager, shipment.CodeCommercialReleaseBlocked},
		{"another stage", func(x *ExceptionRequest) { x.StageID = "P1" }, nil, unpaid(), manager, kernel.CodeInvalidKnownValue},
		{"another currency", func(x *ExceptionRequest) { x.Currency = "EUR" }, nil, unpaid(), manager, kernel.CodeInvalidKnownValue},
		{"another amount", func(x *ExceptionRequest) { x.ShortfallMinor = "201" }, nil, unpaid(), manager, kernel.CodeInvalidKnownValue},
		{"already ended", func(x *ExceptionRequest) { x.ValidUntil = manager.At }, nil, unpaid(), manager, kernel.CodeInvalidKnownValue},
		{"beyond 90 days", func(x *ExceptionRequest) { x.ValidUntil = manager.At.Add(MaxExceptionValidity + time.Second) }, nil, unpaid(), manager,
			kernel.CodeInvalidKnownValue},
	} {
		x := req
		if c.edit != nil {
			c.edit(&x)
		}
		if _, err := r.GrantException(x, c.other, c.p, nil, c.act); code(err) != c.want {
			t.Errorf("%s: %s", c.name, code(err))
		}
	}
	if r.Status != StatusBlocked || r.RowVersion != 1 || len(r.Exceptions) != 0 {
		t.Fatalf("refused exceptions change nothing %+v", r)
	}
	x, err := r.GrantException(req, nil, unpaid(), []Entry{{Name: "n", Value: "v"}}, manager)
	if err != nil || r.Status != StatusGranted || r.RowVersion != 2 || x.Seq != 1 || x.ReleaseVersion != 2 || x.Order != order || x.StageID != "P2" ||
		x.ShortfallMinor != "200" || x.Currency != "USD" || x.Granted.By.ID != "manager.1" || len(x.EntriesSHA256) != 64 {
		t.Fatalf("granted %+v %+v %v", r, x, err)
	}
	if code(r.Exceptable(2)) != kernel.CodeInvalidState {
		t.Fatal("a granted release takes no exception")
	}
	fresh := New("rls.2", "shipment.1", ScopeShipment, order, doc)
	if code(fresh.Exceptable(0)) != kernel.CodeInvalidState {
		t.Fatal("a release never evaluated takes no exception")
	}
	if code(evaluated(t, blocked(shipment.CodeCommercialReleaseBlocked)).Exceptable(5)) != kernel.CodeVersionConflict {
		t.Fatal("expected version")
	}
}

// SM-FUL-RELEASE granted → consumed: shipment.confirm consumes a shipment release,
// document_release.record a documents release with how the documents were released; a missing or
// blocked release is COMMERCIAL_RELEASE_BLOCKED; only a human consumes.
func TestConsumption(t *testing.T) {
	var none *Release
	if code(none.Consumable(ScopeShipment, "/args/shipment_id")) != shipment.CodeCommercialReleaseBlocked {
		t.Fatal("no release")
	}
	r := evaluated(t, blocked(shipment.CodeQualityReleaseRequired))
	if code(r.Consume("", nil, nil, doc)) != shipment.CodeCommercialReleaseBlocked {
		t.Fatal("a blocked release is not consumed")
	}
	r.Evaluate(Verdict{}, doc)
	if code(r.Consume("", nil, nil, agent)) != kernel.CodePermissionDenied {
		t.Fatal("an agent consumes")
	}
	if e := r.Entry(0); e.Name != shipment.GuardCommercialRelease || e.Value != "rls.1/v2/granted" {
		t.Fatal(e)
	}
	if err := r.Consume(KindTelexRelease, []string{"ev"}, []Entry{{Name: "a", Value: "b"}}, doc); err != nil || r.Status != StatusConsumed ||
		r.RowVersion != 3 || r.Consumption.Command != CommandConfirmShipment || r.Consumption.ReleaseKind != "" || len(r.Consumption.EntriesSHA256) != 64 {
		t.Fatalf("consumed %+v %+v %v", r, r.Consumption, err)
	}
	d := New("rls.3", "shipment.1", ScopeDocuments, order, doc)
	d.Evaluate(Verdict{}, doc)
	if code(d.Consume("courier", nil, nil, doc)) != kernel.CodeSchemaInvalid {
		t.Fatal("kind")
	}
	if err := d.Consume(KindOriginalBL, nil, nil, doc); err != nil || d.Consumption.Command != CommandRecordDocumentRelease ||
		d.Consumption.ReleaseKind != KindOriginalBL || d.Consumption.Evidence == nil {
		t.Fatalf("documents %+v %v", d.Consumption, err)
	}
}

// document_release.record re-judges a confirmed shipment's quantities on what the order's other
// confirmed shipments consumed (FUL-INV-001, FUL-INV-003): its own lines are taken out, and a
// prepared shipment is judged on everything confirmed.
func TestConfirmedShipmentIsJudgedWithoutItself(t *testing.T) {
	ref := shipment.DeliveryRef{DeliveryID: "delivery.1", LineID: "L1"}
	s := &shipment.Shipment{Status: shipment.StatusConfirmed, Lines: []shipment.Line{{ID: "S1", OrderLineID: "L1", Quantity: kernel.MustDecimal("40"),
		Delivery: ref, ReleaseID: "rel.1"}}}
	hundred := kernel.MustDecimal("100")
	c := shipment.Consumed{OrderLines: map[string]kernel.Decimal{"L1": hundred}, Deliveries: map[shipment.DeliveryRef]kernel.Decimal{ref: hundred},
		Releases: map[string]kernel.Decimal{"rel.1": hundred}}
	b := s.Before(c)
	if b.OrderLines["L1"].String() != "60" || b.Deliveries[ref].String() != "60" || b.Releases["rel.1"].String() != "60" || c.OrderLines["L1"].String() != "100" {
		t.Fatalf("%+v", b)
	}
	s.Status = shipment.StatusPrepared
	if s.Before(c).OrderLines["L1"].String() != "100" {
		t.Fatal("a prepared shipment")
	}
}

// The bindings' JSON is the published api.ReleaseBindings: absent guards are absent members.
func TestBindingsJSON(t *testing.T) {
	b := Bindings{Screenings: []ScreeningBinding{{PartyID: "customer.1", Status: "clear", ScreeningID: "scr.1", PartyRevision: 2}},
		Deliveries: []DeliveryBinding{}, QualityReleases: []QualityBinding{{ReleaseID: "rel.1", Status: "granted", Released: "100"}},
		Payment: &PaymentBinding{Scope: ScopeShipment, Status: "satisfied", AppliedMinor: "96000", RequiredMinor: "96000", Currency: "USD"}}
	doc, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err = json.Unmarshal(doc, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["trade_compliance"]; ok || m["screenings"].([]any)[0].(map[string]any)["screening_id"] != "scr.1" ||
		m["payment_condition"].(map[string]any)["applied_minor"] != "96000" || m["quality_releases"].([]any)[0].(map[string]any)["released_quantity"] != "100" {
		t.Fatal(string(doc))
	}
}
