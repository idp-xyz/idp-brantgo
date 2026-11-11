//go:build integration

package bootstrap_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The CST suite (WBS S2-03) drives costing contexts and measurement snapshots through cst/api
// against the composed INQ, CAT and POL on PostgreSQL 16. The fixture is the Python costing
// fixture (tests/costing_fixture.py, costing_inputs.json): RFQ rfq.1 ready at row version 2 with
// line L1, 120 PCS of sku.1; calculation currency USD; pricing policy policy.1; a required freight
// fee category; sales trade and payment terms. Python's location, trade_scope and sku_profile
// references have no Brant owner; the Go contract takes trade terms, a CAT SKU and specification
// revision, and checks locations for form only.

type costingWorld struct {
	*world
	editor, reviewer context.Context // POL and master data
	author, checker  context.Context // CST: the author drafts, the checker verifies and records
	usd, pcs, policy polapi.ConfigRef
	fee, trade, pay  polapi.ConfigRef
	calendar         polapi.ConfigRef
	win              window
}

func newCostingWorld(t *testing.T) *costingWorld {
	w := newWorld(t)
	c := &costingWorld{world: w, win: newWindow()}
	c.editor, c.reviewer = w.polActors()
	human := func(id string) context.Context {
		return w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: id},
			Permissions: allPermissions, DataScope: "company", Team: "trade"})
	}
	c.author, c.checker = human("author"), human("checker")
	win := c.win
	c.usd = w.currency(c.editor, c.reviewer, "USD", win)
	c.pcs = w.configure(c.editor, c.reviewer, polapi.KindUnit, "PCS", 0, win.from, time.Time{},
		polapi.ConfigData{Unit: &polapi.Unit{Code: "PCS", Name: "piece", Dimension: polapi.DimensionCount, Precision: 0}}, true)
	tax := w.tax(c.editor, c.reviewer, polapi.RecoveryNonrecoverable, "tax.zero", win)
	c.policy = w.configure(c.editor, c.reviewer, polapi.KindPricingPolicy, "policy.1", 0, win.from, time.Time{},
		polapi.ConfigData{PricingPolicy: &polapi.PricingPolicy{Name: "synthetic costing policy", CalculationCurrency: "USD", MinimumMargin: "0.25",
			QuoteValidDays: 7, MaxFXAgeSeconds: 86400, TaxProfile: tax}}, true)
	c.fee = w.configure(c.editor, c.reviewer, polapi.KindFeeCategory, "freight", 0, win.from, time.Time{},
		polapi.ConfigData{FeeCategory: &polapi.FeeCategory{Name: "freight", Category: "freight", Side: polapi.FeeSidePurchaseCost,
			Inclusion: polapi.InclusionSeparate, AllocationBasis: polapi.AllocationQuantity, PolicyBasis: "synthetic"}}, true)
	c.pay = w.configure(c.editor, c.reviewer, polapi.KindPaymentTerms, "payment.test", 0, win.from, time.Time{}, polapi.ConfigData{PaymentTerms: &polapi.PaymentTerms{
		Name: "100 before shipment", Stages: []polapi.PaymentStage{{StageID: "all", Percent: "100", Trigger: polapi.TriggerBeforeShipment}}}}, true)
	c.trade = w.configure(c.editor, c.reviewer, polapi.KindTradeTerms, "terms.sale", 0, win.from, time.Time{}, polapi.ConfigData{TradeTerms: &polapi.TradeTerms{
		Incoterm: "FOB", Edition: polapi.IncotermsEdition2020, NamedPlace: "TEST PORT", ResponsibilityNote: "synthetic evidence only",
		Delivery: polapi.Delivery{Kind: polapi.DeliveryFixedDate, Date: "2027-01-31"}}}, true)
	c.calendar = w.configure(c.editor, c.reviewer, polapi.KindCalendar, "cal.sh", 0, win.from, time.Time{}, polapi.ConfigData{Calendar: &polapi.Calendar{
		TimeZone: "Asia/Shanghai", WorkingWeekdays: []int{1, 2, 3, 4, 5}}}, true)
	_, err := w.save(c.editor, w.readyArgs(c.editor))
	w.must(err)
	_, err = w.confirm(c.editor, "rfq.1", 1, "src.1")
	w.must(err)
	return c
}

// rfqArgs is a complete draft RFQ id like rfq.1, on the fixture's customer, contact, SKU and source.
func (c *costingWorld) rfqArgs(id string) inqapi.SaveRfq {
	return inqapi.SaveRfq{RfqID: id, CustomerID: "customer.1", ContactID: "contact.1", SourceIDs: []string{"src.1"}, UnresolvedFields: []string{},
		Lines: []inqapi.RfqLine{{LineID: "L1", SkuID: "sku.1", Quantity: "120", UOM: "PCS", SourceID: "src.1", Locator: "第1段"}}}
}

func cfg(r polapi.ConfigRef) cstapi.ConfigRef {
	return cstapi.ConfigRef{Kind: r.Kind, ID: r.ConfigID, Revision: r.Revision}
}

func cfgPtr(r polapi.ConfigRef) *cstapi.ConfigRef {
	c := cfg(r)
	return &c
}

var port = cstapi.BusinessRef{Kind: cstapi.PlaceKind, ID: "port.test", Revision: 1}

var proof = []cstapi.Evidence{{SourceID: "src.1", Locator: "合成核价依据"}}

// data is the Python fixture's costing context context.1 as the Go contract carries it.
func (c *costingWorld) data() cstapi.ContextData {
	return cstapi.ContextData{RfqRef: &cstapi.RfqRef{RfqID: "rfq.1", RowVersion: 2}, Kind: cstapi.ContextActual,
		Lines:                  []cstapi.ContextLine{{LineID: "L1", SkuID: "sku.1", SkuSpecRevision: 1, Quantity: "120", UnitRef: cfg(c.pcs)}},
		CalculationCurrencyRef: cfgPtr(c.usd), PricingPolicyRef: cfgPtr(c.policy),
		ComparisonBasis: &cstapi.ComparisonBasis{SpecificationBasis: "SPEC-1", NamedPlaceRef: port, RequiredFeeTypeRefs: []cstapi.ConfigRef{cfg(c.fee)},
			TaxBasis: "synthetic_zero_nonrecoverable", CostBasis: cstapi.CostBasisNominalDirect, SalesTradeTermsRef: cfg(c.trade),
			SalesPaymentTermsRef: cfg(c.pay), Delivery: cstapi.Delivery{Kind: cstapi.DeliveryFixed, Date: "2026-11-15",
				Milestone: cstapi.MilestoneHandover, PlaceRef: port, TimeZone: "Asia/Shanghai"}}}
}

func (c *costingWorld) saveContext(ctx context.Context, id string, expected int64, d cstapi.ContextData) (cstapi.Receipt, error) {
	return c.c.CST.SaveContext(ctx, cstapi.SaveContext{IdempotencyKey: c.key(), ContextID: id, ExpectedVersion: expected, Data: d})
}

// verifyCommand verifies working revision 1 of a new context id, with a fresh key.
func (c *costingWorld) verifyCommand(id string) cstapi.VerifyContext {
	return cstapi.VerifyContext{IdempotencyKey: c.key(), ContextID: id, ExpectedVersion: 1, WorkingRevision: 1, Evidence: proof}
}

func (c *costingWorld) verify(ctx context.Context, id string, expected, working, supersedes int64) (cstapi.Receipt, error) {
	return c.c.CST.VerifyContext(ctx, cstapi.VerifyContext{IdempotencyKey: c.key(), ContextID: id, ExpectedVersion: expected,
		WorkingRevision: working, SupersedesRevision: supersedes, Evidence: proof})
}

// verified saves d as the author and verifies it as the checker.
func (c *costingWorld) verified(id string, d cstapi.ContextData) cstapi.Receipt {
	c.t.Helper()
	_, err := c.saveContext(c.author, id, 0, d)
	c.must(err)
	r, err := c.verify(c.checker, id, 1, 1, 0)
	c.must(err)
	return r
}

func (c *costingWorld) context(id string) cstapi.ContextView {
	c.t.Helper()
	v, err := c.c.CST.GetContext(c.checker, id)
	c.must(err)
	return v
}

// replayed sends cmd again and checks it returns the stored receipt.
func replayed[T any](t *testing.T, first cstapi.Receipt, send func(T) (cstapi.Receipt, error), cmd T) {
	t.Helper()
	again, err := send(cmd)
	if err != nil || !again.Replayed || again.ActionID != first.ActionID || again.RowVersion != first.RowVersion || again.Status != first.Status {
		t.Fatalf("replay %+v, %v; first %+v", again, err, first)
	}
}

// Python test_costing_lifecycle.py test_all_35_costing_commands_persist_and_replay, the
// costing_context and measurement commands (T02 §13.2): save, verify, revise, discard, revise,
// save, verify superseding revision 1, withdraw revision 1; every command replays its receipt.
func TestCostingContextCommandsPersistAndReplay(t *testing.T) {
	c := newCostingWorld(t)
	data := c.data()
	steps := []struct {
		name   string
		send   func() (cstapi.Receipt, error)
		status string
	}{}
	add := func(name, status string, send func() (cstapi.Receipt, error)) {
		steps = append(steps, struct {
			name   string
			send   func() (cstapi.Receipt, error)
			status string
		}{name, send, status})
	}
	save := func(expected int64) cstapi.SaveContext {
		return cstapi.SaveContext{IdempotencyKey: c.key(), ContextID: "matrix", ExpectedVersion: expected, Data: data}
	}
	verify := func(expected, working, supersedes int64) cstapi.VerifyContext {
		return cstapi.VerifyContext{IdempotencyKey: c.key(), ContextID: "matrix", ExpectedVersion: expected, WorkingRevision: working,
			SupersedesRevision: supersedes, Evidence: proof}
	}
	revise := func(expected int64) cstapi.ReviseContext {
		return cstapi.ReviseContext{IdempotencyKey: c.key(), ContextID: "matrix", ExpectedVersion: expected, BaseRevision: 1, Reason: "修订测试"}
	}
	s1, v1, r1 := save(0), verify(1, 1, 0), revise(2)
	d1 := cstapi.DiscardContextDraft{IdempotencyKey: c.key(), ContextID: "matrix", ExpectedVersion: 3, WorkingRevision: 2, Reason: "丢弃未完成修订"}
	r2, s2, v2 := revise(4), save(5), verify(6, 3, 1)
	w1 := cstapi.WithdrawContext{IdempotencyKey: c.key(), ContextID: "matrix", ExpectedVersion: 7, TargetRevision: 1, Reason: "撤销旧修订", Evidence: proof}
	add("save", cstapi.StatusWorking, func() (cstapi.Receipt, error) { return c.c.CST.SaveContext(c.author, s1) })
	add("verify", cstapi.StatusActive, func() (cstapi.Receipt, error) { return c.c.CST.VerifyContext(c.checker, v1) })
	add("revise", cstapi.StatusWorking, func() (cstapi.Receipt, error) { return c.c.CST.ReviseContext(c.author, r1) })
	add("discard", cstapi.StatusDiscarded, func() (cstapi.Receipt, error) { return c.c.CST.DiscardContextDraft(c.author, d1) })
	add("revise again", cstapi.StatusWorking, func() (cstapi.Receipt, error) { return c.c.CST.ReviseContext(c.author, r2) })
	add("save revision 3", cstapi.StatusWorking, func() (cstapi.Receipt, error) { return c.c.CST.SaveContext(c.author, s2) })
	add("verify revision 3", cstapi.StatusActive, func() (cstapi.Receipt, error) { return c.c.CST.VerifyContext(c.checker, v2) })
	add("withdraw revision 1", cstapi.StatusWithdrawn, func() (cstapi.Receipt, error) { return c.c.CST.WithdrawContext(c.checker, w1) })
	for i, s := range steps {
		r, err := s.send()
		if err != nil || r.RowVersion != int64(i+1) || r.Status != s.status || r.Replayed || r.ObjectID != "matrix" {
			t.Fatalf("%s: %+v, %v", s.name, r, err)
		}
		again, err := s.send()
		if err != nil || !again.Replayed || again.ActionID != r.ActionID || again.RowVersion != r.RowVersion {
			t.Fatalf("%s replay: %+v, %v", s.name, again, err)
		}
	}
	v := c.context("matrix")
	if v.RootVersion != 8 || v.ActiveRevision != 3 || v.WorkingRevision != 0 || v.LastRevision != 3 || len(v.Revisions) != 3 {
		t.Fatalf("context %+v", v)
	}
	statuses := []string{}
	for _, r := range v.Revisions {
		statuses = append(statuses, r.Status)
	}
	if !slices.Equal(statuses, []string{cstapi.StatusWithdrawn, cstapi.StatusDiscarded, cstapi.StatusActive}) {
		t.Fatalf("statuses %v", statuses)
	}
	old, latest := v.Revisions[0], v.Revisions[2]
	if old.SupersededAt.IsZero() || old.EndedBy.ID != "checker" || old.EndReason != "撤销旧修订" || len(old.EndEvidence) != 1 {
		t.Fatalf("withdrawn revision %+v", old)
	}
	if latest.BaseRevision != 1 || latest.VerifiedBy.ID != "checker" || latest.Rfq.RfqID != "rfq.1" || latest.Rfq.RowVersion != 2 ||
		len(latest.Rfq.LinesHash) != 64 || !slices.Equal(latest.Contributors, []string{"author"}) || latest.ContentHash != old.ContentHash {
		t.Fatalf("active revision %+v", latest)
	}
	if v.Revisions[1].EndReason != "丢弃未完成修订" || v.Revisions[1].EndedBy.ID != "author" {
		t.Fatalf("discarded revision %+v", v.Revisions[1])
	}
	// A replay with another expected version still answers with the stored receipt.
	stale := v2
	stale.ExpectedVersion = 1
	first, err := c.c.CST.VerifyContext(c.checker, v2)
	c.must(err)
	replayed(t, first, func(cmd cstapi.VerifyContext) (cstapi.Receipt, error) { return c.c.CST.VerifyContext(c.checker, cmd) }, stale)
	// The same key with other business arguments conflicts.
	other := v2
	other.Evidence = []cstapi.Evidence{{SourceID: "src.1", Locator: "另一依据"}}
	_, err = c.c.CST.VerifyContext(c.checker, other)
	c.want(err, kernel.CodeIdempotencyPayloadConflict)
	if events := c.events("company.A", cstapi.Source); len(events) != 2 || events[0].Type != cstapi.EventCostingContextVerified ||
		events[1].Payload["revision"] != float64(3) || events[1].Payload["supersedes_revision"] != float64(1) {
		t.Fatalf("events %+v", events)
	}
}

// T02 §13.2: a verified context is revised, not saved; verify names the working revision and the
// active revision it supersedes; withdraw targets a sealed revision once; discarding revives
// nothing.
func TestCostingContextStateRules(t *testing.T) {
	c := newCostingWorld(t)
	c.verified("ctx", c.data())
	_, err := c.saveContext(c.author, "ctx", 2, c.data())
	if code(err) != kernel.CodeInvalidState || path(err) != "/args/working_revision" {
		t.Fatalf("save over a verified context: %v", err)
	}
	_, err = c.saveContext(c.author, "ctx", 0, c.data())
	c.want(err, kernel.CodeVersionConflict)
	_, err = c.saveContext(c.author, "missing", 1, c.data())
	c.want(err, kernel.CodeNotFound)
	_, err = c.c.CST.ReviseContext(c.author, cstapi.ReviseContext{IdempotencyKey: c.key(), ContextID: "ctx", ExpectedVersion: 2, BaseRevision: 2, Reason: "无此修订"})
	c.want(err, kernel.CodeInvalidState)
	_, err = c.c.CST.ReviseContext(c.author, cstapi.ReviseContext{IdempotencyKey: c.key(), ContextID: "ctx", ExpectedVersion: 2, BaseRevision: 1, Reason: "修订"})
	c.must(err)
	_, err = c.verify(c.checker, "ctx", 3, 2, 0)
	if code(err) != cstapi.CodeReferenceStale || path(err) != "/args/supersedes_revision" {
		t.Fatalf("verify without naming the active revision: %v", err)
	}
	_, err = c.verify(c.checker, "ctx", 3, 1, 1)
	c.want(err, kernel.CodeInvalidState)
	// The reviser contributed to revision 2 and does not verify it.
	_, err = c.verify(c.author, "ctx", 3, 2, 1)
	c.want(err, kernel.CodePermissionDenied)
	_, err = c.verify(c.checker, "ctx", 3, 2, 1)
	c.must(err)
	withdraw := func(expected, target int64) error {
		_, err := c.c.CST.WithdrawContext(c.checker, cstapi.WithdrawContext{IdempotencyKey: c.key(), ContextID: "ctx", ExpectedVersion: expected,
			TargetRevision: target, Reason: "撤销", Evidence: proof})
		return err
	}
	c.must(withdraw(4, 2))
	if v := c.context("ctx"); v.ActiveRevision != 0 || v.Revisions[0].Status != cstapi.StatusSuperseded {
		t.Fatalf("withdrawing the active revision revives nothing: %+v", v)
	}
	c.want(withdraw(5, 2), kernel.CodeInvalidState)
	c.must(withdraw(5, 1))
	_, err = c.c.CST.DiscardContextDraft(c.author, cstapi.DiscardContextDraft{IdempotencyKey: c.key(), ContextID: "ctx", ExpectedVersion: 6,
		WorkingRevision: 3, Reason: "无工作稿"})
	c.want(err, kernel.CodeInvalidState)
}

// Python test_costing.py test_discard_keeps_exact_command_fact_after_restart: an empty draft is
// saved and discarded with its reason, which the revision and the audit trail keep.
func TestDiscardKeepsExactCommandFact(t *testing.T) {
	c := newCostingWorld(t)
	_, err := c.saveContext(c.author, "ctx.discard", 0, cstapi.ContextData{})
	c.must(err)
	r, err := c.c.CST.DiscardContextDraft(c.author, cstapi.DiscardContextDraft{IdempotencyKey: "discard.fact", ContextID: "ctx.discard",
		ExpectedVersion: 1, WorkingRevision: 1, Reason: "重复询价，明确丢弃此草稿"})
	c.must(err)
	if r.Status != cstapi.StatusDiscarded || r.RowVersion != 2 {
		t.Fatalf("discard %+v", r)
	}
	v := c.context("ctx.discard")
	if v.WorkingRevision != 0 || v.LastRevision != 1 || v.Revisions[0].EndReason != "重复询价，明确丢弃此草稿" || v.Revisions[0].EndedBy.ID != "author" {
		t.Fatalf("context %+v", v)
	}
	audit, err := c.c.CST.Audit(c.checker)
	c.must(err)
	found := slices.ContainsFunc(audit, func(e kernel.AuditEntry) bool {
		return e.Command == cstapi.CommandDiscardContextDraft && e.Outcome == "succeeded" && e.Reason == "重复询价，明确丢弃此草稿" &&
			e.Actor.ID == "author" && e.ObjectID == "ctx.discard"
	})
	if !found {
		t.Fatalf("audit %+v", audit)
	}
	// A new save starts revision 2; the discarded number is not reused.
	_, err = c.saveContext(c.author, "ctx.discard", 2, cstapi.ContextData{Kind: cstapi.ContextWhatIf})
	c.must(err)
	if v = c.context("ctx.discard"); v.WorkingRevision != 2 {
		t.Fatalf("context %+v", v)
	}
}

// WBS S2-03 acceptance 完整询价行覆盖 (T02 §13.1, Python _document_ready): an actual context covers
// the RFQ's complete line set with the same SKU, specification, unit and quantity; a partial,
// enlarged or changed line set is refused at verification. A what_if context may change
// quantities only, and is never frozen for its RFQ.
func TestVerifyCoversTheCompleteRfqLineSet(t *testing.T) {
	c := newCostingWorld(t)
	c.sku(c.editor, "sku.2", true, "src.1")
	two := c.rfqArgs("rfq.2")
	two.Lines = append(two.Lines, inqapi.RfqLine{LineID: "L2", SkuID: "sku.2", Quantity: "30", UOM: "PCS", SourceID: "src.1", Locator: "第2段"})
	_, err := c.save(c.editor, two)
	c.must(err)
	_, err = c.confirm(c.editor, "rfq.2", 1, "src.1")
	c.must(err)
	base := c.data()
	base.RfqRef.RfqID = "rfq.2"
	l2 := cstapi.ContextLine{LineID: "L2", SkuID: "sku.2", SkuSpecRevision: 1, Quantity: "30", UnitRef: cfg(c.pcs)}
	cases := []struct {
		name  string
		edit  func(d *cstapi.ContextData)
		code  string
		where string
	}{
		{"partial line set", func(d *cstapi.ContextData) {}, cstapi.CodeComparisonBasisMismatch, "/args/data/lines"},
		{"another line", func(d *cstapi.ContextData) {
			d.Lines = append(d.Lines, cstapi.ContextLine{LineID: "L9", SkuID: "sku.2", SkuSpecRevision: 1, Quantity: "30", UnitRef: cfg(c.pcs)})
		}, cstapi.CodeComparisonBasisMismatch, "/args/data/lines"},
		{"another quantity", func(d *cstapi.ContextData) {
			l := l2
			l.Quantity = "31"
			d.Lines = append(d.Lines, l)
		}, cstapi.CodeComparisonBasisMismatch, "/args/data/lines/1/quantity"},
		{"another SKU", func(d *cstapi.ContextData) {
			l := l2
			l.SkuID = "sku.1"
			d.Lines = append(d.Lines, l)
		}, cstapi.CodeComparisonBasisMismatch, "/args/data/lines/1"},
		{"a fractional piece", func(d *cstapi.ContextData) {
			l := l2
			l.Quantity = "30.5"
			d.Lines = append(d.Lines, l)
		}, kernel.CodeInvalidKnownValue, "/args/data/lines/1/quantity"},
		{"duplicate line", func(d *cstapi.ContextData) { d.Lines = append(d.Lines, d.Lines[0]) }, kernel.CodeDuplicateLineID, "/args/data/lines"},
		{"complete", func(d *cstapi.ContextData) { d.Lines = append(d.Lines, l2) }, "OK", ""},
	}
	for i, k := range cases {
		t.Run(k.name, func(t *testing.T) {
			d := base
			d.Lines = slices.Clone(base.Lines)
			k.edit(&d)
			id := "ctx." + string(rune('a'+i))
			_, err := c.saveContext(c.author, id, 0, d)
			if err == nil {
				_, err = c.verify(c.checker, id, 1, 1, 0)
			}
			if code(err) != k.code || path(err) != k.where {
				t.Fatalf("%s at %s, want %s at %s", code(err), path(err), k.code, k.where)
			}
		})
	}
	// what_if: the same lines, SKUs and units with other quantities.
	whatIf := base
	whatIf.Kind = cstapi.ContextWhatIf
	more := l2
	more.Quantity = "300"
	whatIf.Lines = append(slices.Clone(base.Lines), more)
	c.verified("ctx.what_if", whatIf)
	got, err := c.c.CST.ResolveContexts(c.checker, []cstapi.ContextRef{{ContextID: "ctx.what_if", Revision: 1}})
	c.must(err)
	v := got[cstapi.ContextRef{ContextID: "ctx.what_if", Revision: 1}]
	if !v.Current() || v.Freezable("/args/context_ref") == nil || path(v.Freezable("/args/context_ref")) != "/args/context_ref/context_kind" {
		t.Fatalf("what_if %+v: %v", v, v.Freezable("/args/context_ref"))
	}
	whatIf.Lines[1].SkuID = "sku.1"
	_, err = c.saveContext(c.author, "ctx.what_if.sku", 0, whatIf)
	c.must(err)
	_, err = c.verify(c.checker, "ctx.what_if.sku", 1, 1, 0)
	c.want(err, cstapi.CodeComparisonBasisMismatch)
}

// RfqRef v1 (Context Map R08, risk 部分行或 what_if 冒充正式核价): verification binds an RFQ that is
// ready at the bound row version; a draft, a closed RFQ or another row version is stale; an RFQ the
// author cannot see is no reference at all.
func TestVerifyRefusesRfqThatIsNotReadyOrChanged(t *testing.T) {
	c := newCostingWorld(t)
	_, err := c.save(c.editor, c.rfqArgs("rfq.draft"))
	c.must(err)
	d := c.data()
	d.RfqRef = &cstapi.RfqRef{RfqID: "rfq.draft", RowVersion: 1}
	_, err = c.saveContext(c.author, "ctx.draft", 0, d)
	c.must(err) // a draft may bind an RFQ that is not ready yet
	_, err = c.verify(c.checker, "ctx.draft", 1, 1, 0)
	if code(err) != cstapi.CodeReferenceStale || path(err) != "/args/data/rfq_ref" {
		t.Fatalf("draft RFQ: %v", err)
	}
	d = c.data()
	d.RfqRef.RowVersion = 1
	_, err = c.saveContext(c.author, "ctx.version", 0, d)
	c.must(err)
	_, err = c.verify(c.checker, "ctx.version", 1, 1, 0)
	c.want(err, cstapi.CodeReferenceStale)
	d.RfqRef = &cstapi.RfqRef{RfqID: "rfq.unknown", RowVersion: 1}
	_, err = c.saveContext(c.author, "ctx.unknown", 0, d)
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/data/rfq_ref" {
		t.Fatalf("unknown RFQ: %v", err)
	}
	// Closed after the draft bound it: verification refuses it.
	_, err = c.saveContext(c.author, "ctx.closed", 0, c.data())
	c.must(err)
	_, err = c.c.INQ.CloseRfq(c.editor, inqapi.CloseRfq{IdempotencyKey: c.key(), RfqID: "rfq.1", ExpectedVersion: 2, Reason: "客户不再推进"})
	c.must(err)
	_, err = c.verify(c.checker, "ctx.closed", 1, 1, 0)
	c.want(err, cstapi.CodeReferenceStale)
	if v := c.context("ctx.closed"); v.WorkingRevision != 1 || v.ActiveRevision != 0 || v.RootVersion != 1 {
		t.Fatalf("refused verification changed the context: %+v", v)
	}
}

// R10 ConfigRef v1: a draft names configurations that exist, of the kind each field takes;
// verification needs them in force, a pricing policy of the calculation currency, units of the RFQ
// line and a calendar of the delivery's time zone.
func TestVerifyJudgesConfigurations(t *testing.T) {
	c := newCostingWorld(t)
	d := c.data()
	d.CalculationCurrencyRef = cfgPtr(c.pcs)
	_, err := c.saveContext(c.author, "ctx.kind", 0, d)
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/data/calculation_currency_ref/kind" {
		t.Fatalf("a unit as currency: %v", err)
	}
	d = c.data()
	d.PricingPolicyRef.Revision = 9
	_, err = c.saveContext(c.author, "ctx.missing", 0, d)
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/data/pricing_policy_ref" {
		t.Fatalf("a missing revision: %v", err)
	}
	// A CNY policy does not calculate in USD.
	cny := c.currency(c.editor, c.reviewer, "CNY", c.win)
	d = c.data()
	d.CalculationCurrencyRef = cfgPtr(cny)
	_, err = c.saveContext(c.author, "ctx.currency", 0, d)
	c.must(err)
	_, err = c.verify(c.checker, "ctx.currency", 1, 1, 0)
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/data/pricing_policy_ref" {
		t.Fatalf("policy currency: %v", err)
	}
	// A lead time counts on a calendar of its own time zone.
	d = c.data()
	d.ComparisonBasis.Delivery = cstapi.Delivery{Kind: cstapi.DeliveryLeadTime, AnchorRef: &cstapi.BusinessRef{Kind: cstapi.AnchorKind,
		ID: "costing_context:ctx.lead", Revision: 1}, Days: 10, DayBasis: cstapi.DayBasisWorking, CalendarRef: cfgPtr(c.calendar),
		Milestone: cstapi.MilestoneHandover, PlaceRef: port, TimeZone: "Europe/Berlin"}
	_, err = c.saveContext(c.author, "ctx.lead", 0, d)
	c.must(err)
	_, err = c.verify(c.checker, "ctx.lead", 1, 1, 0)
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/data/comparison_basis/delivery/calendar_ref" {
		t.Fatalf("calendar time zone: %v", err)
	}
	d.ComparisonBasis.Delivery.AnchorRef.ID = "order_confirmed:o.1"
	_, err = c.saveContext(c.author, "ctx.anchor", 0, d)
	c.want(err, cstapi.CodeDependencyNotSupported)
	d.ComparisonBasis.Delivery.AnchorRef.ID = "costing_context:ctx.lead"
	d.ComparisonBasis.Delivery.TimeZone = "Asia/Shanghai"
	_, err = c.saveContext(c.author, "ctx.lead", 1, d)
	c.must(err)
	_, err = c.verify(c.checker, "ctx.lead", 2, 1, 0)
	c.must(err)
	// Retired after the draft named it: not in force at verification.
	_, err = c.saveContext(c.author, "ctx.retired", 0, c.data())
	c.must(err)
	_, err = c.c.POL.Retire(c.reviewer, polapi.Retire{IdempotencyKey: c.key(), Kind: polapi.KindFeeCategory, ConfigID: "freight", ExpectedVersion: 2,
		Reason: "停止采用"})
	c.must(err)
	_, err = c.verify(c.checker, "ctx.retired", 1, 1, 0)
	if code(err) != polapi.CodeConfigNotActive || path(err) != "/args/data/comparison_basis/required_fee_type_refs/0" {
		t.Fatalf("retired fee category: %v", err)
	}
}

// Python test_costing.py test_ai_with_wildcard_cannot_attest_or_freeze, for this slice: AI and
// system actors with every permission draft but never verify, withdraw or record a measurement
// (tactical model humanOnlyCommands; Python HUMAN_ACTIONS).
func TestHumanOnlyCommandsRefuseAIAndSystem(t *testing.T) {
	c := newCostingWorld(t)
	ai := c.provision(bootstrap.Grant{Scope: c.scope("company.A"), Actor: actorRef("ai", "ai.1"), Permissions: allPermissions, DataScope: "company"})
	system := c.provision(bootstrap.Grant{Scope: c.scope("company.A"), Actor: actorRef("system", "sys.1"), Permissions: allPermissions, DataScope: "company"})
	_, err := c.saveContext(ai, "ctx.ai", 0, c.data())
	c.must(err)
	_, err = c.saveContext(system, "ctx.system", 0, c.data())
	c.want(err, kernel.CodePermissionDenied)
	for _, actor := range []context.Context{ai, system} {
		_, err = c.verify(actor, "ctx.ai", 1, 1, 0)
		c.want(err, kernel.CodePermissionDenied)
		_, err = c.c.CST.WithdrawContext(actor, cstapi.WithdrawContext{IdempotencyKey: c.key(), ContextID: "ctx.ai", ExpectedVersion: 1,
			TargetRevision: 1, Reason: "撤销", Evidence: proof})
		c.want(err, kernel.CodePermissionDenied)
		_, err = c.c.CST.RecordMeasurement(actor, cstapi.RecordMeasurement{IdempotencyKey: c.key(), MeasurementID: "m.ai", Data: measured()})
		c.want(err, kernel.CodePermissionDenied)
	}
	// The human checker verifies what the agent drafted.
	_, err = c.verify(c.checker, "ctx.ai", 1, 1, 0)
	c.must(err)
}

// Python test_costing_domains.py test_competing_verifications_cannot_seal_the_same_input_twice:
// two verifications of the same working revision at the same root version, under different keys;
// one seals it, the other is VERSION_CONFLICT.
func TestCompetingVerificationsSealOnce(t *testing.T) {
	c := newCostingWorld(t)
	for round := 0; round < 5; round++ {
		id := "ctx.race." + string(rune('a'+round))
		_, err := c.saveContext(c.author, id, 0, c.data())
		c.must(err)
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i := range errs {
			cmd := cstapi.VerifyContext{IdempotencyKey: c.key(), ContextID: id, ExpectedVersion: 1, WorkingRevision: 1, Evidence: proof}
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = c.c.CST.VerifyContext(c.checker, cmd)
			}()
		}
		wg.Wait()
		codes := []string{code(errs[0]), code(errs[1])}
		slices.Sort(codes)
		if !slices.Equal(codes, []string{"OK", kernel.CodeVersionConflict}) {
			t.Fatalf("round %d: %v", round, codes)
		}
		if v := c.context(id); v.RootVersion != 2 || v.ActiveRevision != 1 || v.Revisions[0].Status != cstapi.StatusActive {
			t.Fatalf("round %d: %+v", round, v)
		}
	}
}

// Data scope and tenancy: a context is NOT_FOUND outside its owner's data scope and another
// company; its rows are confined by FORCE RLS; sealed revisions and measurements do not change and
// nothing is deleted.
func TestCostingContextScopeAndImmutability(t *testing.T) {
	c := newCostingWorld(t)
	c.verified("ctx", c.data())
	own := c.provision(bootstrap.Grant{Scope: c.scope("company.A"), Actor: actorRef("human", "stranger"), Permissions: allPermissions})
	_, err := c.c.CST.GetContext(own, "ctx")
	c.want(err, kernel.CodeNotFound)
	_, err = c.verify(own, "ctx", 2, 1, 0)
	c.want(err, kernel.CodeNotFound)
	other := c.provision(bootstrap.Grant{Scope: c.scope("company.B"), Actor: actorRef("human", "checker"), Permissions: allPermissions, DataScope: "company"})
	_, err = c.c.CST.GetContext(other, "ctx")
	c.want(err, kernel.CodeNotFound)
	if n := c.count("company.B", "brant_cst.costing_context_revision"); n != 0 {
		t.Fatalf("company.B sees %d revisions", n)
	}
	reader := c.provision(bootstrap.Grant{Scope: c.scope("company.A"), Actor: actorRef("human", "reader"), Permissions: []string{"cst.read"},
		DataScope: "company"})
	if _, err = c.c.CST.GetContext(reader, "ctx"); err != nil {
		t.Fatal(err)
	}
	_, err = c.c.CST.GetContext(c.provision(bootstrap.Grant{Scope: c.scope("company.A"), Actor: actorRef("human", "nobody"), DataScope: "company"}), "ctx")
	c.want(err, kernel.CodePermissionDenied)
	_, err = c.c.CST.RecordMeasurement(c.checker, cstapi.RecordMeasurement{IdempotencyKey: c.key(), MeasurementID: "m", Data: measured()})
	c.must(err)
	for _, sql := range []string{
		`UPDATE brant_cst.costing_context_revision SET data = '{}'::jsonb WHERE context_id = 'ctx'`,
		`UPDATE brant_cst.costing_context_revision SET status = 'working' WHERE context_id = 'ctx'`,
		`UPDATE brant_cst.costing_context_revision SET verified_by_id = 'other' WHERE context_id = 'ctx'`,
		`UPDATE brant_cst.costing_context SET owner_actor_id = 'other', root_version = root_version + 1 WHERE context_id = 'ctx'`,
	} {
		if err := c.exec("company.A", sql); sqlState(err) != "23000 " && sqlState(err) != "42501 " {
			t.Errorf("%s: %v", sql, err)
		}
	}
	for _, sql := range []string{
		`DELETE FROM brant_cst.costing_context_revision WHERE context_id = 'ctx'`,
		`DELETE FROM brant_cst.measurement_revision WHERE measurement_id = 'm'`,
		`UPDATE brant_cst.measurement_revision SET data = '{}'::jsonb WHERE measurement_id = 'm'`,
	} {
		if err := c.exec("company.A", sql); sqlState(err) != "42501 " {
			t.Errorf("%s: %v", sql, err)
		}
	}
}

// Lookups for the scenario evaluation: a verified revision with its RFQ binding (the hash INQ's
// RfqRef v1 gives), the lines, the POL references and the assumptions; a superseded revision is
// stale; an unsealed revision is absent.
func TestResolveContextsForEvaluation(t *testing.T) {
	c := newCostingWorld(t)
	c.verified("ctx", c.data())
	refs, err := c.c.INQ.ResolveRfqs(c.checker, []string{"rfq.1"})
	c.must(err)
	ref := cstapi.ContextRef{ContextID: "ctx", Revision: 1}
	got, err := c.c.CST.ResolveContexts(c.checker, []cstapi.ContextRef{ref, {ContextID: "ctx", Revision: 2}, {ContextID: "none", Revision: 1}})
	c.must(err)
	v, ok := got[ref]
	if !ok || len(got) != 1 || !v.Current() || v.Check("/x") != nil || v.Provisional() || v.Freezable("/x") != nil {
		t.Fatalf("lookup %+v", got)
	}
	if v.Rfq.LinesHash != refs["rfq.1"].LinesHash || v.Rfq.RowVersion != 2 || v.Kind != cstapi.ContextActual || len(v.Lines) != 1 ||
		v.CalculationCurrencyRef != cfg(c.usd) || v.PricingPolicyRef != cfg(c.policy) || v.VerifiedBy.ID != "checker" ||
		!slices.Equal(v.RfqLines, []cstapi.BoundLine{{LineID: "L1", SkuID: "sku.1", SkuSpecRevision: 1, Quantity: "120", UOM: "PCS"}}) {
		t.Fatalf("lookup %+v", v)
	}
	if view := c.context("ctx"); !slices.Equal(view.Revisions[0].RfqLines, v.RfqLines) || len(view.Revisions[0].VerificationEvidence) != 1 {
		t.Fatalf("view %+v", view.Revisions[0])
	}
	want := []cstapi.ConfigRef{cfg(c.pcs), cfg(c.usd), cfg(c.fee), cfg(c.pay), cfg(c.policy), cfg(c.trade)}
	slices.SortFunc(want, func(a, b cstapi.ConfigRef) int {
		if a.Kind != b.Kind {
			if a.Kind < b.Kind {
				return -1
			}
			return 1
		}
		if a.ID < b.ID {
			return -1
		}
		return 1
	})
	if !slices.Equal(v.ConfigRefs, want) {
		t.Fatalf("config refs %+v, want %+v", v.ConfigRefs, want)
	}
	_, err = c.c.CST.ReviseContext(c.author, cstapi.ReviseContext{IdempotencyKey: c.key(), ContextID: "ctx", ExpectedVersion: 2, BaseRevision: 1, Reason: "修订"})
	c.must(err)
	if got, _ = c.c.CST.ResolveContexts(c.checker, []cstapi.ContextRef{ref}); !got[ref].Current() {
		t.Fatal("an unverified revision does not remove the active one (Python test_unpublished_revision_does_not_remove_current_eligibility)")
	}
	_, err = c.verify(c.checker, "ctx", 3, 2, 1)
	c.must(err)
	got, err = c.c.CST.ResolveContexts(c.checker, []cstapi.ContextRef{ref})
	c.must(err)
	if code(got[ref].Check("/args/context_ref")) != cstapi.CodeReferenceStale || got[ref].Status != cstapi.StatusSuperseded {
		t.Fatalf("superseded revision %+v (Python test_replacement_revision_invalidates_old_snapshot_adoption)", got[ref])
	}
	_, err = c.c.CST.WithdrawContext(c.checker, cstapi.WithdrawContext{IdempotencyKey: c.key(), ContextID: "ctx", ExpectedVersion: 4,
		TargetRevision: 2, Reason: "原询价范围已经撤销", Evidence: proof})
	c.must(err)
	ref2 := cstapi.ContextRef{ContextID: "ctx", Revision: 2}
	if got, _ = c.c.CST.ResolveContexts(c.checker, []cstapi.ContextRef{ref2}); got[ref2].Status != cstapi.StatusWithdrawn {
		t.Fatalf("withdrawn revision %+v (Python test_withdrawn_input_blocks_adoption)", got[ref2])
	}
}
