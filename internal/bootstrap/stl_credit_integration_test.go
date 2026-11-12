//go:build integration

package bootstrap_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"sync"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl"
	stlapi "go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The STL customer credit suite (WBS S5-12a; tactical model STL-AGG-CustomerCredit,
// SM-STL-CREDIT-LIMIT, SM-STL-CREDIT-RESERVATION, SM-STL-CREDIT-DECISION; STL-INV-004, STL-INV-005;
// TD-AT11, TD-AT16; Context Map R57 CreditGuard v1; S5 plan D-4 to D-7; owner decision E-2) on
// PostgreSQL 16. Python V0.50 has no STL, so the tests cite design IDs only.
//
// The credit world runs STL with a stand-in for TRD's OrderCommercialSnapshot v1 (STL's port
// Orders), so a test can give an order any payment plan; stl_credit_http_integration_test.go runs
// the composed STL against TRD's real orders. POL holds E-2's synthetic credit policy (USD, default
// limit 0, tiers credit_officer 1,000 and credit_manager 5,000, two approvers above, 30 days overdue
// tolerance, document-controlled payments secured, reservations valid 30 days, decisions 7 days).
// The people hold role templates with company data scope: sales.1 (sales_clerk), officer.1
// (credit_officer), manager.1 and manager.2 (credit_manager); system:inbox.stl is the system actor
// of the order event consumers (unit B6).

const creditReason = "合成信用测试，非真实财务数据"

// creditOrders is the stand-in for TRD's order lookup.
type creditOrders struct {
	mu     sync.Mutex
	orders map[string]stl.Order
}

func (f *creditOrders) ResolveOrders(_ context.Context, ids []string) (map[string]stl.Order, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]stl.Order{}
	for _, id := range ids {
		if o, ok := f.orders[id]; ok {
			out[id] = o
		}
	}
	return out, nil
}

func (f *creditOrders) put(o stl.Order) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.orders[o.Binding.OrderID] = o
}

func (f *creditOrders) setStatus(id, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.orders[id]
	o.Status = status
	f.orders[id] = o
}

type creditWorld struct {
	*world
	stl                                               *stl.Module
	orders                                            *creditOrders
	editor, reviewer                                  context.Context
	sales, officer, manager1, manager2, system, agent context.Context
	usd, cny, policy                                  polapi.ConfigRef
	win                                               window
}

func e2Policy() polapi.ConfigData {
	return polapi.ConfigData{CreditPolicy: &polapi.CreditPolicy{Name: "E-2 synthetic credit policy", Currency: "USD", DefaultLimit: "0",
		LimitSources:         []string{polapi.LimitSourceInternalAssessment, polapi.LimitSourceSinosureBuyerLimit},
		ApprovalTiers:        []polapi.CreditApprovalTier{{Level: "credit_officer", MaxAmount: "1000"}, {Level: "credit_manager", MaxAmount: "5000"}},
		OverdueToleranceDays: 30, DocumentsSecured: true, ReservationValidityDays: 30, DecisionValidityDays: 7, TwoApproversAboveTop: true}}
}

func newCreditWorld(t *testing.T) *creditWorld {
	t.Helper()
	w := newWorld(t)
	editor, reviewer := w.polActors()
	cw := &creditWorld{world: w, orders: &creditOrders{orders: map[string]stl.Order{}}, editor: editor, reviewer: reviewer, win: newWindow()}
	cw.usd, cw.cny = w.currency(editor, reviewer, "USD", cw.win), w.currency(editor, reviewer, "CNY", cw.win)
	cw.policy = w.configure(editor, reviewer, polapi.KindCreditPolicy, "credit.usd", 0, cw.win.from, time.Time{}, e2Policy(), true)
	w.party(editor, "customer.1", true, "evidence")
	w.source(editor, "assessment.1", "合成内部信用评估：建议额度 10,000.00 USD")
	cw.sales, cw.officer = w.assign("sales.1", "sales_clerk"), w.assign("officer.1", "credit_officer")
	cw.manager1, cw.manager2 = w.assign("manager.1", "credit_manager"), w.assign("manager.2", "credit_manager")
	cw.system = w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorSystem, ID: "system:inbox.stl"},
		Permissions: []string{"credit.consume", "credit.release", "credit.reserve", "stl.read"}, DataScope: "company"})
	cw.agent = w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.credit"},
		Permissions: []string{"credit_exception.decide", "credit_limit.publish", "credit_limit.request", "credit.reserve", "stl.read", "evd.read"},
		DataScope:   "company"})
	db, err := bentopg.NewDB(pgtest.AppPool(t))
	w.must(err)
	cw.stl, err = stl.New(db, stl.Upstream{Orders: cw.orders, Configs: w.c.POL, Parties: w.c.PTY, Sources: w.c.EVD})
	w.must(err)
	return cw
}

func stlRef(r polapi.ConfigRef) stlapi.ConfigRef {
	return stlapi.ConfigRef{Kind: r.Kind, ConfigID: r.ConfigID, Revision: r.Revision}
}

func orderHashOf(id string, rev int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s@%d", id, rev)))
	return hex.EncodeToString(sum[:])
}

// draftOrder gives TRD's stand-in a draft order of customer.1 in currency whose payment plan is
// stages; its credit portion is what TRD counts: the stages on credit.
func (cw *creditWorld) draftOrder(id string, currency polapi.ConfigRef, stages ...stl.OrderStage) stl.Order {
	portion := new(big.Int)
	for _, s := range stages {
		if s.OnCredit {
			n, _ := new(big.Int).SetString(s.AmountMinor, 10)
			portion.Add(portion, n)
		}
	}
	o := stl.Order{Binding: stl.OrderBinding{OrderID: id, OrderRevision: 1, OrderHash: orderHashOf(id, 1)}, Status: stl.OrderDraft,
		CustomerID: "customer.1", Currency: stl.CurrencyRef{ID: currency.ConfigID, Revision: currency.Revision}, Stages: stages,
		CreditPortionMinor: portion.String()}
	cw.orders.put(o)
	return o
}

// onCredit is a USD draft order paid entirely 30 days after shipment, amountMinor on credit.
func (cw *creditWorld) onCredit(id, amountMinor string) stl.Order {
	return cw.draftOrder(id, cw.usd, stl.OrderStage{StageID: "P2", Trigger: "after_shipment", Days: 30, Percent: "100", AmountMinor: amountMinor,
		OnCredit: true})
}

func (cw *creditWorld) limitCmd(id, amount string) stlapi.RequestCreditLimit {
	return stlapi.RequestCreditLimit{IdempotencyKey: cw.key(), RequestID: id, PartyID: "customer.1", Currency: stlRef(cw.usd),
		Sources:   []stlapi.LimitSource{{Source: stlapi.SourceInternalAssessment, Amount: amount, EvidenceIDs: []string{"assessment.1"}}},
		ValidFrom: kernel.FormatTimestamp(cw.win.from), ValidUntil: kernel.FormatTimestamp(cw.win.from.Add(365 * 24 * time.Hour)),
		CreditPolicy: stlRef(cw.policy), Reason: creditReason}
}

func (cw *creditWorld) publish(ctx context.Context, id string, expected int64, decision string) (stlapi.LimitReceipt, error) {
	return cw.stl.PublishCreditLimit(ctx, stlapi.PublishCreditLimit{IdempotencyKey: cw.key(), RequestID: id, ExpectedVersion: expected,
		Decision: decision, CreditPolicy: stlRef(cw.policy), Reason: creditReason})
}

// limit has the sales clerk request a limit of 10,000.00 USD on internal assessment and the two
// credit managers publish it (above the top tier: two approvers).
func (cw *creditWorld) limit() {
	cw.t.Helper()
	_, err := cw.stl.RequestCreditLimit(cw.sales, cw.limitCmd("limit.10000", "10000.00"))
	cw.must(err)
	_, err = cw.publish(cw.manager1, "limit.10000", 1, stlapi.DecisionPublish)
	cw.must(err)
	r, err := cw.publish(cw.manager2, "limit.10000", 2, stlapi.DecisionPublish)
	cw.must(err)
	if r.Status != stlapi.RequestPublished || r.LimitVersion < 1 {
		cw.t.Fatalf("limit %+v", r)
	}
}

func (cw *creditWorld) reserveCmd(id string, o stl.Order) stlapi.ReserveCredit {
	return stlapi.ReserveCredit{IdempotencyKey: cw.key(), ReservationID: id, OrderID: o.Binding.OrderID, OrderRevision: o.Binding.OrderRevision,
		OrderHash: o.Binding.OrderHash, CreditPolicy: stlRef(cw.policy)}
}

func (cw *creditWorld) reserve(ctx context.Context, id string, o stl.Order) (stlapi.ReservationReceipt, error) {
	return cw.stl.ReserveCredit(ctx, cw.reserveCmd(id, o))
}

func (cw *creditWorld) decide(ctx context.Context, id string, expected int64, decision string) (stlapi.DecisionReceipt, error) {
	return cw.stl.DecideCreditException(ctx, stlapi.DecideCreditException{IdempotencyKey: cw.key(), DecisionID: id, ExpectedVersion: expected,
		Decision: decision, CreditPolicy: stlRef(cw.policy), Reason: creditReason})
}

func (cw *creditWorld) release(ctx context.Context, id string, expected int64) (stlapi.ReservationReceipt, error) {
	return cw.stl.ReleaseCredit(ctx, stlapi.ReleaseCredit{IdempotencyKey: cw.key(), ReservationID: id, ExpectedVersion: expected,
		Reason: "订单未生效，放弃占用"})
}

// creditQuery is what TRD asks CreditGuard v1 of o: portionMinor in currency, now.
func creditQuery(o stl.Order, portionMinor, currency string) stlapi.CreditQuery {
	return stlapi.CreditQuery{OrderID: o.Binding.OrderID, OrderRevision: o.Binding.OrderRevision, OrderHash: o.Binding.OrderHash,
		PortionMinor: portionMinor, Currency: currency, At: time.Now()}
}

// creditGuard calls CreditGuard v1 for q in a transaction of scope with the downstream's executor.
func (d *downstream) creditGuard(scope kernel.Scope, q stlapi.CreditQuery) (stlapi.CreditVerdict, error) {
	var v stlapi.CreditVerdict
	err := d.in(scope, func(ctx context.Context, exec bentopg.Executor) error {
		var err error
		v, err = stlapi.ScanCreditGuard(q, exec.QueryRow(ctx, stlapi.CreditGuardSQL, stlapi.CreditGuardArgs(q)...).Scan)
		return err
	})
	return v, err
}

func (cw *creditWorld) guard(d *downstream, q stlapi.CreditQuery) stlapi.CreditVerdict {
	cw.t.Helper()
	v, err := d.creditGuard(d.scope, q)
	cw.must(err)
	return v
}

var errCreditRefused = errors.New("downstream: credit reservation does not cover the order")

// activateOnCredit is TRD's order activation as B4 wires it: lock the order root FOR UPDATE, guard
// the credit reservation, then activate and record the reservation, in one transaction.
func (d *downstream) activateOnCredit(quoteID string, q stlapi.CreditQuery, s stage) (stlapi.CreditVerdict, error) {
	var v stlapi.CreditVerdict
	err := d.in(d.scope, func(ctx context.Context, exec bentopg.Executor) error {
		if s.pid != nil {
			var pid int32
			if err := exec.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				return err
			}
			s.pid <- pid
		}
		t, c := string(d.scope.Tenant), string(d.scope.Company)
		var status string
		if err := exec.QueryRow(ctx, "SELECT status FROM "+d.quotes+" WHERE tenant_id = $1 AND company_id = $2 AND quote_id = $3 FOR UPDATE",
			t, c, quoteID).Scan(&status); err != nil {
			return err
		}
		var err error
		if v, err = stlapi.ScanCreditGuard(q, exec.QueryRow(ctx, stlapi.CreditGuardSQL, stlapi.CreditGuardArgs(q)...).Scan); err != nil {
			return err
		}
		if err = v.Check("/args/order_id"); err != nil {
			return errors.Join(errCreditRefused, err)
		}
		if s.hold != nil {
			s.hold()
		}
		if _, err = exec.Exec(ctx, "UPDATE "+d.quotes+" SET status = 'active', root_version = root_version + 1 WHERE tenant_id = $1 AND company_id = $2 AND quote_id = $3",
			t, c, quoteID); err != nil {
			return err
		}
		_, err = exec.Exec(ctx, "INSERT INTO "+d.adopted+" (tenant_id, company_id, quote_id, kind, config_id, revision, content_hash) VALUES ($1, $2, $3, 'credit', $4, $5, $6)",
			t, c, quoteID, v.ReservationID, q.OrderRevision, v.OrderHash)
		return err
	})
	return v, err
}

func hasEvent(events []event, typ string) bool {
	return slices.ContainsFunc(events, func(e event) bool { return e.Type == typ })
}

// SM-STL-CREDIT-LIMIT, STL-INV-005 and D-6: the sales clerk requests a limit on evidence; a credit
// officer may not publish it (credit_limit.publish is the credit manager's), and 10,000.00 is above
// the top tier, so two distinct credit managers publish it; then it is the limit in force. Lower of
// both sources; a party that is not a confirmed customer, evidence EVD does not show, and an AI
// actor are refused.
func TestCreditLimitsArePublishedByTier(t *testing.T) {
	cw := newCreditWorld(t)
	cmd := cw.limitCmd("limit.1", "10000.00")
	r, err := cw.stl.RequestCreditLimit(cw.sales, cmd)
	cw.must(err)
	if r.Status != stlapi.RequestRequested || r.RowVersion != 1 || r.AmountMinor != "1000000" || r.Currency != "USD" || r.PartyID != "customer.1" {
		t.Fatalf("request %+v", r)
	}
	if again, err := cw.stl.RequestCreditLimit(cw.sales, cmd); err != nil || !again.Replayed || again.ActionID != r.ActionID {
		t.Fatalf("replay %+v %v", again, err)
	}
	unknown := cw.limitCmd("limit.x", "1.00")
	unknown.PartyID = "customer.none"
	_, err = cw.stl.RequestCreditLimit(cw.sales, unknown)
	cw.want(err, kernel.CodeReferenceInvalid)
	noEvidence := cw.limitCmd("limit.y", "1.00")
	noEvidence.Sources[0].EvidenceIDs = []string{"evidence.none"}
	_, err = cw.stl.RequestCreditLimit(cw.sales, noEvidence)
	cw.want(err, kernel.CodeReferenceInvalid)
	inCNY := cw.limitCmd("limit.z", "1.00")
	inCNY.Currency = stlRef(cw.cny)
	_, err = cw.stl.RequestCreditLimit(cw.sales, inCNY)
	cw.want(err, stlapi.CodeFXScopeMismatch)
	cents := cw.limitCmd("limit.w", "1.001")
	_, err = cw.stl.RequestCreditLimit(cw.sales, cents)
	cw.want(err, kernel.CodeInvalidKnownValue)

	_, err = cw.publish(cw.officer, "limit.1", 1, stlapi.DecisionPublish)
	cw.want(err, kernel.CodePermissionDenied)
	_, err = cw.publish(cw.sales, "limit.1", 1, stlapi.DecisionPublish)
	cw.want(err, kernel.CodePermissionDenied)
	_, err = cw.publish(cw.agent, "limit.1", 1, stlapi.DecisionPublish)
	cw.want(err, kernel.CodePermissionDenied) // human only
	first, err := cw.publish(cw.manager1, "limit.1", 1, stlapi.DecisionPublish)
	cw.must(err)
	if first.Status != stlapi.RequestRequested || first.RowVersion != 2 || first.Approvals != 1 || first.RequiredApprovals != 2 || first.LimitVersion != 0 {
		t.Fatalf("first approval %+v", first)
	}
	_, err = cw.publish(cw.manager1, "limit.1", 2, stlapi.DecisionPublish)
	cw.want(err, stlapi.CodeCreditDecisionNotAuthorized)
	_, err = cw.publish(cw.manager2, "limit.1", 1, stlapi.DecisionPublish)
	cw.want(err, kernel.CodeVersionConflict)
	second, err := cw.publish(cw.manager2, "limit.1", 2, stlapi.DecisionPublish)
	cw.must(err)
	if second.Status != stlapi.RequestPublished || second.RowVersion != 3 || second.Approvals != 2 || second.LimitVersion != 1 {
		t.Fatalf("published %+v", second)
	}
	_, err = cw.publish(cw.manager1, "limit.1", 3, stlapi.DecisionReject)
	cw.want(err, kernel.CodeInvalidState)

	// A Sinosure limit beside the internal assessment, valid from the day after tomorrow: the lower
	// of both, published by one manager; until it starts, version 1 stays the limit in force.
	both := cw.limitCmd("limit.2", "6000.00")
	both.Sources = append(both.Sources, stlapi.LimitSource{Source: stlapi.SourceSinosureBuyerLimit, Amount: "4500", EvidenceIDs: []string{"evidence"}})
	both.ValidFrom = kernel.FormatTimestamp(cw.win.from.Add(48 * time.Hour))
	r2, err := cw.stl.RequestCreditLimit(cw.sales, both)
	cw.must(err)
	if r2.AmountMinor != "450000" {
		t.Fatalf("lower of both %+v", r2)
	}
	if p, err := cw.publish(cw.manager1, "limit.2", 1, stlapi.DecisionPublish); err != nil || p.Status != stlapi.RequestPublished || p.LimitVersion != 2 {
		t.Fatalf("one manager within the top tier: %+v %v", p, err)
	}

	v, err := cw.stl.GetCustomerCredit(cw.officer, "customer.1")
	cw.must(err)
	if v.LimitInForce == nil || v.LimitInForce.Version != 1 || v.LimitInForce.AmountMinor != "1000000" || len(v.Versions) != 2 ||
		v.Versions[1].Source != stlapi.SourceLowerOfBoth || v.Versions[1].InForce || len(v.Requests) != 2 || len(v.Requests[0].Approvals) != 2 ||
		v.Requests[0].Approvals[0].Level != "credit_manager" || v.Requests[0].Approvals[0].MaxMinor != "500000" {
		t.Fatalf("customer credit %+v", v)
	}
	_, err = cw.stl.GetCustomerCredit(cw.officer, "customer.none")
	cw.want(err, kernel.CodeNotFound)
	events := cw.events("company.A", stlapi.Source)
	if !hasEvent(events, stlapi.EventCreditLimitRequested) || !hasEvent(events, stlapi.EventCreditLimitPublished) {
		t.Fatalf("events %+v", events)
	}
}

// TD-AT11 and STL-INV-004: limit 10,000.00, 7,000.00 held by one order; another order's 4,000.00
// opens a credit decision with CREDIT_LIMIT_EXCEEDED — committed, nothing reserved, CreditGuard v1
// says missing — the sales clerk may not decide it (D-7), the credit officer releases the 1,000.00
// excess within its tier (D-6), and the next credit.reserve succeeds on the decision.
func TestCreditReservationWithinTheLimitOrByDecision(t *testing.T) {
	cw := newCreditWorld(t)
	cw.limit()
	d := newDownstream(cw.world)
	a := cw.onCredit("order.a", "700000")
	ra, err := cw.reserve(cw.sales, "res.a", a)
	cw.must(err)
	if ra.Status != stlapi.ReservationReserved || ra.Basis != stlapi.BasisWithinLimit || ra.PortionMinor != "700000" || ra.RowVersion != 1 ||
		ra.ExposureMinor != "0" || ra.LimitMinor != "1000000" {
		t.Fatalf("within the limit %+v", ra)
	}
	b := cw.onCredit("order.b", "400000")
	rb, err := cw.reserve(cw.sales, "res.b", b)
	cw.must(err)
	if rb.Status != stlapi.StatusDecisionOpened || rb.DecisionID == "" || rb.ObjectID != rb.DecisionID || rb.ReservationID != "" ||
		!slices.Equal(rb.Reasons, []string{stlapi.CodeCreditLimitExceeded}) || rb.ExcessMinor != "100000" || rb.ExposureMinor != "700000" {
		t.Fatalf("over the limit %+v", rb)
	}
	if v := cw.guard(d, creditQuery(b, "400000", "USD")); v.Status != stlapi.CreditMissing || v.Code != stlapi.CodeCreditReservationRequired {
		t.Fatalf("guard before the decision %+v", v)
	}
	_, err = cw.stl.GetReservation(cw.sales, "res.b")
	cw.want(err, kernel.CodeNotFound)
	x, err := cw.stl.GetDecision(cw.officer, rb.DecisionID)
	cw.must(err)
	if x.State != stlapi.DecisionOpen || x.CoverMinor != "100000" || x.LimitMinor != "1000000" || x.OpenedBy.ID != "sales.1" ||
		x.ExpiresAt.Sub(x.OpenedAt) != 7*24*time.Hour {
		t.Fatalf("decision %+v", x)
	}
	// Asking again while it is open answers the same decision.
	if again, err := cw.reserve(cw.sales, "res.b", b); err != nil || again.DecisionID != rb.DecisionID {
		t.Fatalf("open decision %+v %v", again, err)
	}
	_, err = cw.decide(cw.sales, rb.DecisionID, 1, stlapi.DecisionRelease)
	cw.want(err, kernel.CodePermissionDenied)
	_, err = cw.decide(cw.agent, rb.DecisionID, 1, stlapi.DecisionRelease)
	cw.want(err, kernel.CodePermissionDenied) // human only
	released, err := cw.decide(cw.officer, rb.DecisionID, 1, stlapi.DecisionRelease)
	cw.must(err)
	if released.Status != stlapi.DecisionReleased || released.RowVersion != 2 || released.Approvals != 1 {
		t.Fatalf("released %+v", released)
	}
	on, err := cw.reserve(cw.sales, "res.b", b)
	cw.must(err)
	if on.Status != stlapi.ReservationReserved || on.Basis != stlapi.BasisCreditDecision || on.DecisionID != rb.DecisionID {
		t.Fatalf("on the decision %+v", on)
	}
	if v := cw.guard(d, creditQuery(b, "400000", "USD")); v.Status != stlapi.CreditCovered || v.Check("/x") != nil || v.Basis != stlapi.BasisCreditDecision ||
		v.ReservationID != "res.b" || v.DecisionID != rb.DecisionID {
		t.Fatalf("guard after the decision %+v", v)
	}
	if x, _ = cw.stl.GetDecision(cw.officer, rb.DecisionID); x.ReservationID != "res.b" || x.State != stlapi.DecisionReleased {
		t.Fatalf("used decision %+v", x)
	}
	v, err := cw.stl.GetCustomerCredit(cw.sales, "customer.1")
	cw.must(err)
	if v.ExposureCurrency != "USD" || v.ExposureMinor != "1100000" || len(v.Reservations) != 2 || len(v.Decisions) != 1 {
		t.Fatalf("customer credit %+v", v)
	}
	events := cw.events("company.A", stlapi.Source)
	for _, typ := range []string{stlapi.EventCreditReserved, stlapi.EventCreditDecisionOpened, stlapi.EventCreditExceptionDecided} {
		if !hasEvent(events, typ) {
			t.Errorf("no %s", typ)
		}
	}
}

// laterClock is the system clock moved by d.
type laterClock struct{ d time.Duration }

func (c laterClock) Now() time.Time { return time.Now().Add(c.d) }

// TD-AT16 and STL-INV-005: an excess of 3,000.00 — the credit officer's 1,000 tier does not cover
// it (CREDIT_DECISION_NOT_AUTHORIZED), the credit manager's 5,000 does, and the reservation then
// succeeds; a decision left past its 7 days is expired: it is no longer decided, and credit.reserve
// opens a new one (re-request).
func TestCreditExceptionTiersAndExpiry(t *testing.T) {
	cw := newCreditWorld(t)
	cw.limit()
	_, err := cw.reserve(cw.sales, "res.a", cw.onCredit("order.a", "700000"))
	cw.must(err)
	c := cw.onCredit("order.c", "600000")
	rc, err := cw.reserve(cw.sales, "res.c", c)
	cw.must(err)
	if rc.Status != stlapi.StatusDecisionOpened || rc.ExcessMinor != "300000" {
		t.Fatalf("excess 3,000 %+v", rc)
	}
	_, err = cw.decide(cw.officer, rc.DecisionID, 1, stlapi.DecisionRelease)
	cw.want(err, stlapi.CodeCreditDecisionNotAuthorized)
	if _, err = cw.decide(cw.manager1, rc.DecisionID, 1, stlapi.DecisionRelease); err != nil {
		t.Fatal(err)
	}
	if r, err := cw.reserve(cw.sales, "res.c", c); err != nil || r.Basis != stlapi.BasisCreditDecision {
		t.Fatalf("reserve after the manager's release %+v %v", r, err)
	}

	e := cw.onCredit("order.e", "100000")
	re, err := cw.reserve(cw.sales, "res.e", e)
	cw.must(err)
	if re.Status != stlapi.StatusDecisionOpened {
		t.Fatalf("over the limit %+v", re)
	}
	cw.stl.Clock = laterClock{8 * 24 * time.Hour}
	_, err = cw.decide(cw.manager1, re.DecisionID, 1, stlapi.DecisionRelease)
	cw.want(err, stlapi.CodeApprovalExpired)
	if x, err := cw.stl.GetDecision(cw.officer, re.DecisionID); err != nil || x.State != stlapi.StateExpired || x.Status != stlapi.DecisionOpen {
		t.Fatalf("expired decision %+v %v", x, err)
	}
	again, err := cw.reserve(cw.sales, "res.e", e)
	cw.must(err)
	if again.Status != stlapi.StatusDecisionOpened || again.DecisionID == re.DecisionID {
		t.Fatalf("re-request %+v", again)
	}
	if r, err := cw.decide(cw.manager1, again.DecisionID, 1, stlapi.DecisionReject); err != nil || r.Status != stlapi.DecisionRejected {
		t.Fatalf("reject %+v %v", r, err)
	}
}

// TD-AT11 concurrency (STL-INV-004: 两笔并发占用合计超额时只有一笔成功): two orders of 6,000.00 each
// against the limit of 10,000.00 reserved at the same time — the credit root's lock makes one wait
// for the other, so exactly one is reserved and the other opens a decision; five rounds.
func TestCreditReservationsInParallelHaveOneWinner(t *testing.T) {
	cw := newCreditWorld(t)
	cw.limit()
	for round := range 5 {
		x, y := cw.onCredit(fmt.Sprintf("order.x%d", round), "600000"), cw.onCredit(fmt.Sprintf("order.y%d", round), "600000")
		start := make(chan struct{})
		type result struct {
			r   stlapi.ReservationReceipt
			err error
		}
		results := make(chan result, 2)
		for i, o := range []stl.Order{x, y} {
			ctx := []context.Context{cw.sales, cw.officer}[i]
			cmd := cw.reserveCmd(fmt.Sprintf("res.%s", o.Binding.OrderID), o)
			go func() {
				<-start
				r, err := cw.stl.ReserveCredit(ctx, cmd)
				results <- result{r, err}
			}()
		}
		close(start)
		var winners []stlapi.ReservationReceipt
		decisions := 0
		for range 2 {
			res := <-results
			cw.must(res.err)
			switch res.r.Status {
			case stlapi.ReservationReserved:
				winners = append(winners, res.r)
			case stlapi.StatusDecisionOpened:
				decisions++
			}
		}
		if len(winners) != 1 || decisions != 1 {
			t.Fatalf("round %d: %d reserved, %d decisions", round, len(winners), decisions)
		}
		_, err := cw.release(cw.officer, winners[0].ReservationID, 1)
		cw.must(err)
	}
}

// CreditGuard v1 (R57): covered, documents_secured (D-5), missing, expired and insufficient; the
// caller's company only; published as a SECURITY DEFINER function brant_app executes and PUBLIC
// does not.
func TestCreditGuardVerdicts(t *testing.T) {
	cw := newCreditWorld(t)
	cw.limit()
	d := newDownstream(cw.world)
	a := cw.onCredit("order.a", "700000")
	_, err := cw.reserve(cw.sales, "res.a", a)
	cw.must(err)
	docs := cw.draftOrder("order.docs", cw.usd, stl.OrderStage{StageID: "P1", Trigger: "before_shipment", Percent: "30", AmountMinor: "30000"},
		stl.OrderStage{StageID: "P2", Trigger: "after_shipment", Percent: "70", AmountMinor: "70000", ReleaseControl: "documents"})
	rd, err := cw.reserve(cw.sales, "res.docs", docs)
	cw.must(err)
	if rd.Basis != stlapi.BasisDocumentsSecured || rd.PortionMinor != "0" || rd.SecuredMinor != "70000" {
		t.Fatalf("documents secured %+v", rd)
	}
	prepaid := cw.draftOrder("order.prepaid", cw.usd, stl.OrderStage{StageID: "P1", Trigger: "before_shipment", Percent: "100", AmountMinor: "100000"})
	_, err = cw.reserve(cw.sales, "res.prepaid", prepaid)
	cw.want(err, kernel.CodeInvalidState)

	later := creditQuery(a, "700000", "USD")
	later.At = time.Now().Add(31 * 24 * time.Hour)
	other := creditQuery(a, "700000", "USD")
	other.OrderHash = orderHashOf("order.a", 9)
	for _, c := range []struct {
		name   string
		q      stlapi.CreditQuery
		scope  kernel.Scope
		status string
		ok     bool
	}{
		{"covered", creditQuery(a, "700000", "USD"), d.scope, stlapi.CreditCovered, true},
		{"covered, less", creditQuery(a, "1", "USD"), d.scope, stlapi.CreditCovered, true},
		{"insufficient", creditQuery(a, "700001", "USD"), d.scope, stlapi.CreditInsufficient, false},
		{"another currency", creditQuery(a, "700000", "CNY"), d.scope, stlapi.CreditInsufficient, false},
		{"expired", later, d.scope, stlapi.CreditExpired, false},
		{"another order hash", other, d.scope, stlapi.CreditCovered, false},
		{"documents secured", creditQuery(docs, "0", "USD"), d.scope, stlapi.CreditDocumentsSecured, true},
		{"documents secured, portion asked", creditQuery(docs, "70000", "USD"), d.scope, stlapi.CreditInsufficient, false},
		{"no reservation", creditQuery(prepaid, "0", "USD"), d.scope, stlapi.CreditMissing, false},
		{"another company", creditQuery(a, "700000", "USD"), cw.scope("company.B"), stlapi.CreditMissing, false},
	} {
		v, err := d.creditGuard(c.scope, c.q)
		cw.must(err)
		if v.Status != c.status || v.Satisfied() != c.ok || (code(v.Check("/args/order_id")) == "OK") != c.ok {
			t.Errorf("%s: %+v", c.name, v)
		}
		if !c.ok && code(v.Check("/x")) != stlapi.CodeCreditReservationRequired {
			t.Errorf("%s: %v", c.name, v.Check("/x"))
		}
	}
	if _, err = cw.release(cw.officer, "res.a", 1); err != nil {
		t.Fatal(err)
	}
	if v := cw.guard(d, creditQuery(a, "700000", "USD")); v.Status != stlapi.CreditMissing {
		t.Fatalf("after the release %+v", v)
	}
	if err = d.in(d.scope, func(ctx context.Context, exec bentopg.Executor) error {
		_, err := exec.Exec(ctx, stlapi.CreditGuardSQL, "order.a", 1, nil, "USD", time.Now())
		return err
	}); err == nil {
		t.Fatal("the guard needs a portion")
	}
	if err = d.in(d.scope, func(ctx context.Context, exec bentopg.Executor) error {
		_, err := exec.Exec(ctx, stlapi.CreditGuardSQL, "order.a", 1, "-1", "USD", time.Now())
		return err
	}); err == nil {
		t.Fatal("a negative portion")
	}
	var definer, executable, public bool
	var config []string
	err = d.app.QueryRow(context.Background(), `SELECT p.prosecdef, p.proconfig,
		has_function_privilege('brant_app', p.oid, 'EXECUTE'), has_function_privilege('public', p.oid, 'EXECUTE')
		FROM pg_proc p WHERE p.oid = 'brant_stl.guard_credit_reservation_v1(text, bigint, numeric, text, timestamptz)'::regprocedure`).
		Scan(&definer, &config, &executable, &public)
	cw.must(err)
	if !definer || len(config) != 1 || config[0] != "search_path=pg_catalog, pg_temp" || !executable || public {
		t.Fatalf("security definer %v, config %v, brant_app %v, public %v", definer, config, executable, public)
	}
}

// ADR-BR-004 option B against STL: credit.release ∥ TRD's activation (B4 wires it; here a
// downstream stand-in locks its root FOR UPDATE and calls CreditGuard v1). Whichever transaction
// takes the reservation row first decides: a release that committed first refuses the activation;
// an activation holding the guard makes the release wait until it commits; a release holding the
// row makes the guard wait and then answer missing.
func TestCreditGuardDecidesEveryCommitOrder(t *testing.T) {
	cw := newCreditWorld(t)
	cw.limit()
	d := newDownstream(cw.world)
	type outcome struct {
		at  time.Time
		err error
	}

	t.Run("STL commits first", func(t *testing.T) {
		o := cw.onCredit("order.c1", "100000")
		_, err := cw.reserve(cw.sales, "res.c1", o)
		cw.must(err)
		_, err = cw.release(cw.officer, "res.c1", 1)
		cw.must(err)
		d.createQuote("q.c1")
		v, err := d.activateOnCredit("q.c1", creditQuery(o, "100000", "USD"), stage{})
		if !errors.Is(err, errCreditRefused) || code(err) != stlapi.CodeCreditReservationRequired || v.Status != stlapi.CreditMissing {
			t.Fatalf("activation %v, verdict %+v", err, v)
		}
		if status, adopted := d.quote("q.c1"); status != "draft" || adopted != 0 {
			t.Fatalf("order %s with %d bindings", status, adopted)
		}
	})

	t.Run("TRD holds the guard first", func(t *testing.T) {
		o := cw.onCredit("order.c2", "100000")
		_, err := cw.reserve(cw.sales, "res.c2", o)
		cw.must(err)
		d.createQuote("q.c2")
		released := make(chan outcome, 1)
		pids := make(chan int32, 1)
		var moved time.Time
		v, err := d.activateOnCredit("q.c2", creditQuery(o, "100000", "USD"), stage{pid: pids, hold: func() {
			pid := <-pids
			go func() {
				_, err := cw.release(cw.officer, "res.c2", 1)
				released <- outcome{time.Now(), err}
			}()
			await(t, "the release waits on the guard", func() bool { return d.blocks(pid) }, released)
			moved = time.Now()
		}})
		if err != nil || !v.Satisfied() {
			t.Fatalf("activation %v, verdict %+v", err, v)
		}
		r := <-released
		if r.err != nil || !r.at.After(moved) {
			t.Fatalf("release %v, after the activation: %v", r.err, r.at.After(moved))
		}
		if status, adopted := d.quote("q.c2"); status != "active" || adopted != 1 {
			t.Fatalf("order %s with %d bindings", status, adopted)
		}
		if v := cw.guard(d, creditQuery(o, "100000", "USD")); v.Status != stlapi.CreditMissing {
			t.Fatalf("after the release %+v", v)
		}
	})

	t.Run("STL holds the row first", func(t *testing.T) {
		o := cw.onCredit("order.c3", "100000")
		_, err := cw.reserve(cw.sales, "res.c3", o)
		cw.must(err)
		d.createQuote("q.c3")
		key := cw.key()
		hold, blockerPID := make(chan struct{}), make(chan int32, 1)
		blocker := make(chan error, 1)
		go func() {
			blocker <- d.in(d.scope, func(ctx context.Context, exec bentopg.Executor) error {
				var pid int32
				if err := exec.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
					return err
				}
				if _, err := exec.Exec(ctx, `INSERT INTO brant_stl.action_result (tenant_id, company_id, command, idempotency_key, request_hash,
					action_id, object_id, row_version, status, actor_kind, actor_id, authority_epoch, correlation_id, recorded_at)
					VALUES ($1, $2, 'credit.release', $3, repeat('0', 64), 'act.blocker', 'blocker', 1, 'released', 'human', 'blocker', 1,
					'corr.blocker', now())`, string(d.scope.Tenant), string(d.scope.Company), key); err != nil {
					return err
				}
				blockerPID <- pid
				<-hold
				return errors.New("blocker rolls back")
			})
		}()
		var pid int32
		select {
		case pid = <-blockerPID:
		case err := <-blocker:
			t.Fatalf("blocker: %v", err)
		}
		released := make(chan error, 1)
		go func() {
			_, err := cw.stl.ReleaseCredit(cw.officer, stlapi.ReleaseCredit{IdempotencyKey: key, ReservationID: "res.c3", ExpectedVersion: 1,
				Reason: "订单未生效，放弃占用"})
			released <- err
		}()
		await(t, "the release holds the reservation and waits on the blocker", func() bool { return d.blocks(pid) }, released)
		type activation struct {
			v   stlapi.CreditVerdict
			err error
		}
		activated := make(chan activation, 1)
		pids := make(chan int32, 1)
		go func() {
			v, err := d.activateOnCredit("q.c3", creditQuery(o, "100000", "USD"), stage{pid: pids})
			activated <- activation{v, err}
		}()
		downstreamPID := <-pids
		await(t, "the guard waits on the release", func() bool { return d.waitsForLock(downstreamPID) }, activated)
		close(hold)
		if err := <-blocker; err == nil {
			t.Fatal("the blocker must roll back")
		}
		if err := <-released; err != nil {
			t.Fatalf("release: %v", err)
		}
		a := <-activated
		if !errors.Is(a.err, errCreditRefused) || a.v.Status != stlapi.CreditMissing {
			t.Fatalf("activation %v, verdict %+v; want refused after the release", a.err, a.v)
		}
		if status, adopted := d.quote("q.c3"); status != "draft" || adopted != 0 {
			t.Fatalf("order %s with %d bindings", status, adopted)
		}
	})
}

// SM-STL-CREDIT-RESERVATION: the system consumes a reservation for the binding the activation
// bound (PL-12, unit B6) — a person cannot; a consumed reservation still covers its order and does
// not expire; a person cannot release it, the system does when the order ends (PL-13). A person
// releases a cancelled order's reserved reservation. The
// order's revision and hash must be TRD's (REFERENCE_STALE), an ended order is ORDER_NOT_ACTIVE,
// and an order in CNY needs a credit rate into the policy's USD (FX_SCOPE_MISMATCH), with which it
// reserves the converted amount (STL-INV-004).
func TestCreditConsumeReleaseAndCurrency(t *testing.T) {
	cw := newCreditWorld(t)
	cw.limit()
	d := newDownstream(cw.world)
	a := cw.onCredit("order.a", "700000")
	_, err := cw.reserve(cw.sales, "res.a", a)
	cw.must(err)
	consume := func(ctx context.Context, rev int64, at time.Time) (stlapi.ReservationReceipt, error) {
		return cw.stl.ConsumeCredit(ctx, stlapi.ConsumeCredit{IdempotencyKey: cw.key(), ReservationID: "res.a", OrderID: a.Binding.OrderID,
			OrderRevision: rev, OrderHash: orderHashOf(a.Binding.OrderID, rev), ActivatedAt: kernel.FormatTimestamp(at)})
	}
	_, err = consume(cw.officer, 1, time.Now())
	cw.want(err, kernel.CodePermissionDenied)
	_, err = consume(cw.system, 2, time.Now())
	cw.want(err, stlapi.CodeReferenceStale)
	c, err := consume(cw.system, 1, time.Now())
	cw.must(err)
	if c.Status != stlapi.ReservationConsumed || c.RowVersion != 2 {
		t.Fatalf("consumed %+v", c)
	}
	late := creditQuery(a, "700000", "USD")
	late.At = time.Now().Add(400 * 24 * time.Hour)
	if v := cw.guard(d, late); v.Status != stlapi.CreditCovered || v.ReservationStatus != stlapi.ReservationConsumed {
		t.Fatalf("consumed reservation %+v", v)
	}
	_, err = cw.release(cw.officer, "res.a", 2)
	cw.want(err, kernel.CodeInvalidState)
	if r, err := cw.release(cw.system, "res.a", 2); err != nil || r.Status != stlapi.ReservationReleased || r.RowVersion != 3 {
		t.Fatalf("released on the order's end %+v %v", r, err)
	}
	if v, err := cw.stl.GetReservation(cw.officer, "res.a"); err != nil || v.ConsumedBy.ID != "system:inbox.stl" || v.ReleasedBy.Kind != kernel.ActorSystem ||
		v.ActivatedAt.IsZero() {
		t.Fatalf("reservation %+v %v", v, err)
	}

	// credit.release reads the order's state from TRD itself (brant_trd.order_state_v1, S5 B4), which
	// knows nothing of the stand-in's orders: an active order's reservation is kept in
	// TestCreditReleaseWaitsForTheActivation on TRD's real orders.
	b := cw.onCredit("order.b", "100000")
	_, err = cw.reserve(cw.sales, "res.b", b)
	cw.must(err)
	cw.orders.setStatus("order.b", "cancelled")
	if _, err = cw.release(cw.officer, "res.b", 1); err != nil {
		t.Fatal(err)
	}
	_, err = cw.reserve(cw.sales, "res.b2", b)
	cw.want(err, stlapi.CodeOrderNotActive)
	stale := cw.reserveCmd("res.s", cw.onCredit("order.s", "100"))
	stale.OrderHash = orderHashOf("order.s", 2)
	_, err = cw.stl.ReserveCredit(cw.sales, stale)
	cw.want(err, stlapi.CodeReferenceStale)
	_, err = cw.stl.ReserveCredit(cw.sales, cw.reserveCmd("res.none", stl.Order{Binding: stl.OrderBinding{OrderID: "order.none", OrderRevision: 1,
		OrderHash: orderHashOf("order.none", 1)}}))
	cw.want(err, kernel.CodeNotFound)

	cnyOrder := cw.draftOrder("order.cny", cw.cny, stl.OrderStage{StageID: "P2", Trigger: "after_shipment", Percent: "100", AmountMinor: "700000",
		OnCredit: true})
	_, err = cw.reserve(cw.sales, "res.cny", cnyOrder)
	cw.want(err, stlapi.CodeFXScopeMismatch)
	rate := cw.configure(cw.editor, cw.reviewer, polapi.KindFXRate, "cny.usd.credit", 0, cw.win.from, cw.win.until,
		polapi.ConfigData{FXRate: &polapi.FXRate{FromCurrency: "CNY", ToCurrency: "USD", Rate: "0.140449", Purpose: polapi.PurposeCredit,
			SourceName: "SYNTHETIC CREDIT RATE", ObservedAt: kernel.FormatTimestamp(cw.win.from)}}, true)
	cmd := cw.reserveCmd("res.cny", cnyOrder)
	fx := stlRef(rate)
	cmd.FXRate = &fx
	r, err := cw.stl.ReserveCredit(cw.sales, cmd)
	cw.must(err)
	v, err := cw.stl.GetReservation(cw.sales, r.ReservationID)
	cw.must(err)
	// 7,000.00 CNY at 0.140449 USD/CNY = 983.143 → 983.14 USD (HALF_UP once).
	if v.Currency != "CNY" || v.PortionMinor != "700000" || v.LimitCurrency != "USD" || v.ExposureMinor != "98314" || v.FXRate == nil ||
		v.FXRate.ConfigID != "cny.usd.credit" {
		t.Fatalf("converted reservation %+v", v)
	}
	audit, err := cw.stl.Audit(cw.assign("admin.1", "administrator"))
	cw.must(err)
	if !hasAudit(audit, stlapi.CommandReserve) || !hasAudit(audit, stlapi.CommandConsume) {
		t.Fatal("audit")
	}
}
