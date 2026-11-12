package application

import (
	"context"
	"slices"
	"sort"
	"strconv"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/charge"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/orderresult"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Charges and their allocation (S5 unit C9, WBS S5-17; SM-STL-CHARGE, STL-INV-010; MD-49). Before the transaction
// a charge command reads what it names upstream: TRD's orders (R32) and FUL's shipments of them (R30) for the
// beneficiaries and allocation targets, POL's fee category, currency and settlement rate (R33), PTY's party (R43)
// and EVD's evidence (R75). Inside it, it locks the charge root FOR UPDATE, then the order results of the charge's
// orders FOR SHARE in ID order (created at version 0 when missing: the lock anchor order_result.close's FOR UPDATE
// waits on), then the ledger locks the authority guard, then POL's ConfigGuard v1 holds the configuration
// revisions FOR SHARE (R33 guard lock). A charge that benefits an order whose result is settled is confirmed or
// allocated only after the result is reopened (BT24: "关闭后补录费用重新打开并留痕"); recording it is allowed.

// lockResults locks FOR SHARE, in ID order, the order results of orders, creating the missing ones at version 0
// owned by the caller; it returns them by order.
func (s Service) lockResults(ctx context.Context, exec bentopg.Executor, orders []string, lock string) (map[string]*orderresult.Result, error) {
	ids := append([]string(nil), orders...)
	sort.Strings(ids)
	ids = slices.Compact(ids)
	owner := ""
	if p, err := identity.FromContext(ctx); err == nil {
		owner = p.Actor.ID
	}
	sc := scope(ctx)
	out := map[string]*orderresult.Result{}
	for _, id := range ids {
		if err := postgres.EnsureResults(ctx, exec, sc, []string{id}, owner, "", kernel.Instant(s.Clock.Now())); err != nil {
			return nil, err
		}
		r, err := postgres.LoadResult(ctx, exec, sc, id, lock)
		if err != nil {
			return nil, err
		}
		out[id] = r
	}
	return out, nil
}

// settledAmong refuses, at path, when the result of one of the charge's orders is settled.
func settledAmong(results map[string]*orderresult.Result, c *charge.Charge, path string) error {
	for _, id := range c.Orders() {
		if r := results[id]; r != nil && r.Status == orderresult.StatusSettled {
			return kernel.Reject(kernel.CodeInvalidState, path, "The result of order "+id+" is settled: reopen it (order_result.reopen) before "+
				"a late charge counts (BT24)")
		}
	}
	return nil
}

func targetOf(t api.ChargeTarget) charge.Target {
	return charge.Target{Kind: t.Kind, OrderID: t.OrderID, ShipmentID: t.ShipmentID}
}

func targetView(t charge.Target) api.ChargeTarget {
	return api.ChargeTarget{Kind: t.Kind, OrderID: t.OrderID, ShipmentID: t.ShipmentID}
}

// signedAmountIn parses a charge amount in c, a leading "-" for an adjustment below zero.
func signedAmountIn(c kernel.Currency, s, path string) (kernel.Money, error) {
	neg := len(s) > 1 && s[0] == '-'
	if neg {
		s = s[1:]
	}
	m, err := amountIn(c, s, path)
	if err != nil || !neg {
		return m, err
	}
	return m.Neg(), nil
}

// chargeOrders reads, before the transaction, the orders charge id benefits (nil when STL has none).
func (s Service) chargeOrders(ctx context.Context, id string) ([]string, error) {
	var out []string
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		c, err := postgres.LoadCharge(ctx, exec, scope(ctx), id, false)
		if err == nil && c != nil {
			out = c.Orders()
		}
		return err
	})
	return out, err
}

// orderShipments asks FUL, through STL's port, for the shipment lines of orders; shown is false when the port
// has no order-level half.
func (s Service) orderShipments(ctx context.Context, orders []string) (map[string][]OrderShipmentLine, bool, error) {
	p, ok := s.Upstream.Shipments.(OrderShipments)
	if !ok || len(orders) == 0 {
		return map[string][]OrderShipmentLine{}, ok, nil
	}
	out, err := p.ResolveOrderShipments(ctx, orders)
	return out, true, err
}

// shipmentOf reports whether FUL shows shipment id among lines, not cancelled.
func shipmentOf(lines []OrderShipmentLine, id string) bool {
	for _, l := range lines {
		if l.ShipmentID == id && l.Status != "cancelled" {
			return true
		}
	}
	return false
}

// resultCurrencyOf is the code of order o's analysis currency (its own when TRD states none).
func resultCurrencyOf(o Order) string {
	if a := o.Analysis; a != nil && a.Currency != "" {
		return a.Currency
	}
	return orderCurrencyOf(o)
}

// orderCurrencyOf is the code of order o's currency (TRD's AnalysisFX names it; the currency revision's ID
// otherwise).
func orderCurrencyOf(o Order) string {
	if a := o.Analysis; a != nil && a.OrderCurrency != "" {
		return a.OrderCurrency
	}
	return o.Currency.ID
}

// recordCharge runs charge.record: a new charge, or the bill of an estimated one (SM-STL-CHARGE; human or AI).
type recordCharge struct {
	s         Service
	cmd       api.RecordCharge
	party     *ptyapi.PartyRef
	evidence  map[string]bool
	orders    map[string]Order
	shipments map[string][]OrderShipmentLine
	configs   map[string]polapi.ResolvedConfig
	configErr error
	rate      settlementRate
	orderIDs  []string
	charge    *charge.Charge
	results   map[string]*orderresult.Result
}

func (h *recordCharge) beneficiaryOrders() []string {
	var ids []string
	for _, b := range h.cmd.Beneficiaries {
		if !slices.Contains(ids, b.OrderID) {
			ids = append(ids, b.OrderID)
		}
	}
	return ids
}

func (h *recordCharge) Prepare(ctx context.Context) error {
	c := h.cmd
	if c.Source.PartyID != "" {
		parties, err := h.s.Upstream.Parties.ResolveParties(ctx, ptyapi.Resolve{PartyIDs: []string{c.Source.PartyID}})
		if err != nil {
			return err
		}
		if p, ok := parties.Parties[c.Source.PartyID]; ok {
			h.party = &p
		}
	}
	var err error
	if h.evidence, err = h.s.resolveEvidence(ctx, c.EvidenceIDs); err != nil {
		return err
	}
	if c.ExpectedVersion > 0 {
		h.orderIDs, err = h.s.chargeOrders(ctx, c.ChargeID)
		return err
	}
	h.orderIDs = h.beneficiaryOrders()
	if h.orders, err = h.s.Upstream.Orders.ResolveOrders(ctx, h.orderIDs); err != nil {
		return err
	}
	if h.shipments, _, err = h.s.orderShipments(ctx, h.orderIDs); err != nil {
		return err
	}
	if h.configs, err = h.s.resolveConfigs(ctx, h.s.Clock.Now(), map[string]polapi.ConfigRef{"/args/fee_category": polRef(*c.FeeCategory),
		"/args/currency": polRef(*c.Currency)}); err != nil {
		if _, refused := kernel.AsRejection(err); !refused {
			return err
		}
		h.configs, h.configErr = nil, err // Apply answers it after the refusals before it
		return nil
	}
	if c.Conversion.Kind == api.ConversionFXRate && len(h.orderIDs) > 0 {
		if o, ok := h.orders[h.orderIDs[0]]; ok && h.configs["/args/currency"].Data.Currency != nil {
			h.rate, err = h.s.lookupRate(ctx, c.Conversion.FXRate, h.configs["/args/currency"].Data.Currency.Code, resultCurrencyOf(o))
			if h.rate.refusal != nil {
				h.rate.refusal = repath(h.rate.refusal, "/args/conversion/fx_rate")
			}
		}
	}
	return err
}

// repath moves a rejection to path.
func repath(err error, path string) error {
	rej, ok := kernel.AsRejection(err)
	if !ok || len(rej.Fields) == 0 {
		return err
	}
	return kernel.RejectAs(rej.Code, rej.Category, path, rej.Fields[0].Reason)
}

func (h *recordCharge) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.charge, h.results = nil, nil
	var err error
	if h.charge, err = postgres.LoadCharge(ctx, exec, scope(ctx), h.cmd.ChargeID, h.cmd.ExpectedVersion > 0); err != nil {
		return err
	}
	h.results, err = h.s.lockResults(ctx, exec, h.orderIDs, postgres.ForShare)
	return err
}

func (h *recordCharge) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	c := h.cmd
	if c.ExpectedVersion > 0 {
		return h.bill(ctx, exec, d)
	}
	sc, now := scope(ctx), kernel.Instant(h.s.Clock.Now())
	if h.charge != nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/charge_id", "The charge ID is taken")
	}
	if err := h.common(d); err != nil {
		return action.Outcome{}, err
	}
	if h.configErr != nil {
		return action.Outcome{}, h.configErr
	}
	fee, cur := h.configs["/args/fee_category"], h.configs["/args/currency"]
	if err := fee.Check("/args/fee_category"); err != nil {
		return action.Outcome{}, err
	}
	if fee.Data.FeeCategory == nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/fee_category", "Not a fee category")
	}
	currency, err := currencyOf(cur, "/args/currency")
	if err != nil {
		return action.Outcome{}, err
	}
	amount, err := signedAmountIn(currency, c.Amount, "/args/amount")
	if err != nil {
		return action.Outcome{}, err
	}
	occurred, analysed, err := dates(c.OccurredOn, c.AnalysisOn)
	if err != nil {
		return action.Outcome{}, err
	}
	beneficiaries := make([]charge.Target, 0, len(c.Beneficiaries))
	for i, b := range c.Beneficiaries {
		p := "/args/beneficiaries/" + strconv.Itoa(i)
		o, ok := h.orders[b.OrderID]
		switch {
		case !ok:
			return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, p+"/order_id", "TRD shows no such order")
		case o.Status == OrderDraft:
			return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, p+"/order_id", "A draft order has no operating result")
		case b.Kind == api.ChargeTargetShipment && !shipmentOf(h.shipments[b.OrderID], b.ShipmentID):
			return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, p+"/shipment_id", "FUL shows no such shipment of the order")
		}
		beneficiaries = append(beneficiaries, targetOf(b))
	}
	conv := charge.Conversion{Kind: c.Conversion.Kind}
	if conv.Kind == charge.ConversionFXRate {
		x, err := h.rate.input(ctx, exec, h.s.Clock.Now(), nil)
		if err != nil {
			return action.Outcome{}, repath(err, "/args/conversion/fx_rate")
		}
		if x == nil {
			return action.Outcome{}, api.Reject(api.CodeFXScopeMismatch, "/args/conversion/fx_rate", "POL has no settlement rate for the charge")
		}
		conv.Rate = &charge.RateUse{ID: x.Use.ID, Revision: x.Use.Revision, Hash: x.Use.Hash, Value: x.Rate.Rate(), To: x.Rate.To().Code()}
	}
	for _, id := range h.orderIDs {
		if err := conversionFits(conv, currency, h.orders[id]); err != nil {
			return action.Outcome{}, err
		}
	}
	if err := h.duplicate(ctx, exec, ""); err != nil {
		return action.Outcome{}, err
	}
	ch, err := charge.Record(charge.RecordInput{ID: c.ChargeID, Status: c.Status, Source: sourceOf(c.Source), FeeCategory: charge.ConfigUse{
		ID: fee.Ref.ConfigID, Revision: fee.Ref.Revision}, CurrencyRevision: cur.Ref.Revision, Amount: amount, Conversion: conv, OccurredOn: occurred,
		AnalysisOn: analysed, Basis: c.Basis, EvidenceIDs: c.EvidenceIDs, Beneficiaries: beneficiaries, By: fundAct(d, now, ""), Team: d.Team})
	if err != nil {
		return action.Outcome{}, err
	}
	for path, rc := range map[string]polapi.ResolvedConfig{"/args/fee_category": fee, "/args/currency": cur} {
		if err := guardConfig(ctx, exec, rc.Ref, rc.ContentHash, h.s.Clock.Now(), path); err != nil {
			return action.Outcome{}, err
		}
	}
	if err = postgres.InsertCharge(ctx, exec, sc, ch); err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: ch.ID, RowVersion: ch.Version, Status: ch.Status}, nil
}

// common checks what a new charge and a bill share: the evidence and the source's party.
func (h *recordCharge) common(_ authorization.Decision) error {
	c := h.cmd
	if err := evidenceFound(h.evidence, c.EvidenceIDs, "/args/evidence_ids"); err != nil {
		return err
	}
	if c.Source.PartyID != "" && (h.party == nil || h.party.Status != ptyapi.StatusConfirmed) {
		return kernel.Reject(kernel.CodeReferenceInvalid, "/args/source/party_id", "PTY shows no such confirmed party")
	}
	return nil
}

// duplicate refuses a source reference another charge than own was recorded with (BUSINESS_KEY_CONFLICT: a bill
// or offer counts once).
func (h *recordCharge) duplicate(ctx context.Context, exec bentopg.Executor, own string) error {
	s := sourceOf(h.cmd.Source)
	if s.Reference == "" {
		return nil
	}
	by, err := postgres.ChargeBySource(ctx, exec, scope(ctx), s)
	if err != nil || by == "" || by == own {
		return err
	}
	return api.Reject(api.CodeBusinessKeyConflict, "/args/source/reference", "Charge "+by+" records this "+s.Kind+" of "+s.PartyID)
}

func (h *recordCharge) bill(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, c, ch, now := scope(ctx), h.cmd, h.charge, kernel.Instant(h.s.Clock.Now())
	switch {
	case ch == nil || !d.Sees(ch.OwnerActor, ch.OwnerTeam):
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/charge_id", "")
	case ch.Version != c.ExpectedVersion:
		return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	if err := h.common(d); err != nil {
		return action.Outcome{}, err
	}
	amount, err := signedAmountIn(ch.Currency(), c.Amount, "/args/amount")
	if err != nil {
		return action.Outcome{}, err
	}
	occurred, analysed, err := dates(c.OccurredOn, c.AnalysisOn)
	if err != nil {
		return action.Outcome{}, err
	}
	if err = h.duplicate(ctx, exec, ch.ID); err != nil {
		return action.Outcome{}, err
	}
	from := ch.Version
	if err = ch.Bill(charge.BillInput{Source: sourceOf(c.Source), Amount: amount, OccurredOn: occurred, AnalysisOn: analysed, Basis: c.Basis,
		EvidenceIDs: c.EvidenceIDs, By: fundAct(d, now, "")}); err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.SaveCharge(ctx, exec, sc, ch, from, now); err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: ch.ID, RowVersion: ch.Version, Status: ch.Status}, nil
}

func sourceOf(s api.ChargeSource) charge.Source {
	return charge.Source{Kind: s.Kind, PartyID: s.PartyID, Reference: s.Reference}
}

// dates parses a charge's occurred and analysis dates (calendar dates, baseline 4.13): the analysis date within
// 366 days of the occurrence (INVALID_KNOWN_VALUE otherwise).
func dates(occurred, analysed string) (time.Time, time.Time, error) {
	o, _ := api.Date(occurred)
	a, _ := api.Date(analysed)
	if d := a.Sub(o); d > 366*24*time.Hour || d < -366*24*time.Hour {
		return o, a, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/analysis_on", "The analysis date is within a year of the occurrence")
	}
	return o, a, nil
}

// conversionFits checks that a charge in currency converts into order o's analysis currency by conv
// (FX_SCOPE_MISMATCH at /args/conversion otherwise: never 1 by default, never a guessed rate).
func conversionFits(conv charge.Conversion, currency kernel.Currency, o Order) error {
	result := resultCurrencyOf(o)
	ok := false
	switch conv.Kind {
	case charge.ConversionSameCurrency:
		ok = currency.Code() == result
	case charge.ConversionOrderAnalysis:
		ok = currency.Code() == orderCurrencyOf(o)
	case charge.ConversionFXRate:
		ok = conv.Rate != nil && conv.Rate.To == result
	}
	if !ok {
		return api.Reject(api.CodeFXScopeMismatch, "/args/conversion", "The charge in "+currency.Code()+" does not convert into order "+
			o.Binding.OrderID+"'s analysis currency "+result+" by "+conv.Kind)
	}
	return nil
}

// RecordCharge records a charge or the bill of an estimated one (charge.record).
func (s Service) RecordCharge(ctx context.Context, cmd api.RecordCharge) (api.ChargeReceipt, error) {
	h := &recordCharge{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandRecordCharge, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ChargeID}, cmd,
		cmd.Validate(), h)
	if err != nil {
		return api.ChargeReceipt{}, err
	}
	return s.chargeReceipt(ctx, res)
}

// confirmCharge runs charge.confirm (SM-STL-CHARGE; human only).
type confirmCharge struct {
	s        Service
	cmd      api.ConfirmCharge
	evidence map[string]bool
	orderIDs []string
	charge   *charge.Charge
	results  map[string]*orderresult.Result
}

func (h *confirmCharge) Prepare(ctx context.Context) error {
	var err error
	if h.evidence, err = h.s.resolveEvidence(ctx, h.cmd.EvidenceIDs); err != nil {
		return err
	}
	h.orderIDs, err = h.s.chargeOrders(ctx, h.cmd.ChargeID)
	return err
}

func (h *confirmCharge) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.charge, h.results = nil, nil
	var err error
	if h.charge, err = postgres.LoadCharge(ctx, exec, scope(ctx), h.cmd.ChargeID, true); err != nil {
		return err
	}
	h.results, err = h.s.lockResults(ctx, exec, h.orderIDs, postgres.ForShare)
	return err
}

func (h *confirmCharge) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, c, ch, now := scope(ctx), h.cmd, h.charge, kernel.Instant(h.s.Clock.Now())
	switch {
	case ch == nil || !d.Sees(ch.OwnerActor, ch.OwnerTeam):
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/charge_id", "")
	case ch.Version != c.ExpectedVersion:
		return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	if err := evidenceFound(h.evidence, c.EvidenceIDs, "/args/evidence_ids"); err != nil {
		return action.Outcome{}, err
	}
	if err := settledAmong(h.results, ch, "/args/charge_id"); err != nil {
		return action.Outcome{}, err
	}
	from := ch.Version
	if err := ch.Confirm(fundAct(d, now, c.Reason), c.EvidenceIDs); err != nil {
		return action.Outcome{}, err
	}
	if err := postgres.SaveCharge(ctx, exec, sc, ch, from, now); err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: ch.ID, RowVersion: ch.Version, Status: ch.Status}, nil
}

// ConfirmCharge confirms a charge with its bill (charge.confirm). Human only.
func (s Service) ConfirmCharge(ctx context.Context, cmd api.ConfirmCharge) (api.ChargeReceipt, error) {
	h := &confirmCharge{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandConfirmCharge, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ChargeID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
	if err != nil {
		return api.ChargeReceipt{}, err
	}
	return s.chargeReceipt(ctx, res)
}

// allocateCharge runs charge.allocate (STL-INV-010; human or AI).
type allocateCharge struct {
	s         Service
	cmd       api.AllocateCharge
	evidence  map[string]bool
	orderIDs  []string
	shipments map[string][]OrderShipmentLine
	charge    *charge.Charge
	results   map[string]*orderresult.Result
}

func (h *allocateCharge) Prepare(ctx context.Context) error {
	var err error
	if h.evidence, err = h.s.resolveEvidence(ctx, h.cmd.EvidenceIDs); err != nil {
		return err
	}
	if h.orderIDs, err = h.s.chargeOrders(ctx, h.cmd.ChargeID); err != nil {
		return err
	}
	h.shipments, _, err = h.s.orderShipments(ctx, h.orderIDs)
	return err
}

func (h *allocateCharge) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.charge, h.results = nil, nil
	var err error
	if h.charge, err = postgres.LoadCharge(ctx, exec, scope(ctx), h.cmd.ChargeID, true); err != nil {
		return err
	}
	h.results, err = h.s.lockResults(ctx, exec, h.orderIDs, postgres.ForShare)
	return err
}

func (h *allocateCharge) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, c, ch, now := scope(ctx), h.cmd, h.charge, kernel.Instant(h.s.Clock.Now())
	switch {
	case ch == nil || !d.Sees(ch.OwnerActor, ch.OwnerTeam):
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/charge_id", "")
	case ch.Version != c.ExpectedVersion:
		return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	if err := evidenceFound(h.evidence, c.EvidenceIDs, "/args/evidence_ids"); err != nil {
		return action.Outcome{}, err
	}
	if err := settledAmong(h.results, ch, "/args/charge_id"); err != nil {
		return action.Outcome{}, err
	}
	allowed := map[string]bool{}
	for _, id := range ch.Orders() {
		allowed[charge.Target{Kind: charge.TargetOrder, OrderID: id}.Key()] = true
		for _, l := range h.shipments[id] {
			if l.Status != "cancelled" {
				allowed[charge.Target{Kind: charge.TargetShipment, OrderID: id, ShipmentID: l.ShipmentID}.Key()] = true
			}
		}
	}
	lines := make([]charge.LineInput, 0, len(c.Lines))
	for _, l := range c.Lines {
		lines = append(lines, charge.LineInput{LineID: l.LineID, Target: targetOf(l.Target), Weight: l.Weight})
	}
	from := ch.Version
	a, err := ch.Allocate(charge.AllocateInput{Basis: c.Basis, DataBasis: c.DataBasis, EvidenceIDs: c.EvidenceIDs, Lines: lines, Allowed: allowed,
		By: fundAct(d, now, "")})
	if err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.InsertAllocation(ctx, exec, sc, ch.ID, a); err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.SaveCharge(ctx, exec, sc, ch, from, now); err != nil {
		return action.Outcome{}, err
	}
	var orders, shipments []string
	targets := make([]map[string]any, 0, len(a.Lines))
	for _, l := range a.Lines {
		if !slices.Contains(orders, l.Target.OrderID) {
			orders = append(orders, l.Target.OrderID)
		}
		if l.Target.ShipmentID != "" {
			shipments = append(shipments, l.Target.ShipmentID)
		}
		targets = append(targets, map[string]any{"line_id": l.LineID, "kind": l.Target.Kind, "order_id": l.Target.OrderID,
			"shipment_id": l.Target.ShipmentID})
	}
	ev := action.Event{Type: api.EventChargeAllocated, AggregateType: "Charge", AggregateID: ch.ID, AggregateVersion: ch.Version,
		Fields: map[string]any{"charge_id": ch.ID, "charge_version": ch.Version, "allocation_seq": a.Seq, "status": ch.Status, "basis": a.Basis,
			"order_ids": orders, "shipment_ids": nonNilStrings(shipments), "lines": targets}}
	return action.Outcome{ObjectID: ch.ID, RowVersion: ch.Version, Status: ch.Status, Events: []action.Event{ev}}, nil
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// AllocateCharge allocates a charge to orders or shipments (charge.allocate).
func (s Service) AllocateCharge(ctx context.Context, cmd api.AllocateCharge) (api.ChargeReceipt, error) {
	h := &allocateCharge{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandAllocateCharge, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ChargeID}, cmd,
		cmd.Validate(), h)
	if err != nil {
		return api.ChargeReceipt{}, err
	}
	return s.chargeReceipt(ctx, res)
}

func (s Service) chargeReceipt(ctx context.Context, res api.Receipt) (api.ChargeReceipt, error) {
	out := api.ChargeReceipt{Receipt: res, Lines: []api.AllocationLineView{}}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		c, err := postgres.LoadCharge(ctx, exec, scope(ctx), res.ObjectID, false)
		if err != nil || c == nil {
			return err
		}
		out.Currency, out.AmountMinor, out.AllocationSeq = c.Currency().Code(), c.Amount.MinorString(), c.AllocationSeq
		if c.Estimate != nil {
			out.EstimateMinor = c.Estimate.MinorString()
		}
		if a := c.Current(); a != nil {
			out.Lines = allocationLines(a)
		}
		return nil
	})
	return out, err
}

func allocationLines(a *charge.Allocation) []api.AllocationLineView {
	out := make([]api.AllocationLineView, 0, len(a.Lines))
	for _, l := range a.Lines {
		out = append(out, api.AllocationLineView{LineID: l.LineID, Target: targetView(l.Target), Weight: l.Weight.String(),
			AmountMinor: l.Amount.MinorString()})
	}
	return out
}

func chargeView(c *charge.Charge) api.ChargeView {
	v := api.ChargeView{ChargeID: c.ID, Status: c.Status, Version: c.Version, Source: api.ChargeSource{Kind: c.Source.Kind, PartyID: c.Source.PartyID,
		Reference: c.Source.Reference}, EstimateReference: c.EstimateReference, FeeCategory: api.ConfigRef{Kind: api.KindFeeCategory,
		ConfigID: c.FeeCategory.ID, Revision: c.FeeCategory.Revision}, Currency: c.Currency().Code(), CurrencyRevision: c.CurrencyRevision,
		AmountMinor: c.Amount.MinorString(), Conversion: api.ChargeConversionView{Kind: c.Conversion.Kind}, OccurredOn: api.FormatDate(c.OccurredOn),
		AnalysisOn: api.FormatDate(c.AnalysisOn), Basis: c.Basis, EvidenceIDs: c.EvidenceIDs, Beneficiaries: []api.ChargeTarget{},
		AllocationSeq: c.AllocationSeq, Allocations: []api.ChargeAllocationView{}, RecordedBy: c.Recorded.By, RecordedAt: c.Recorded.At,
		ConfirmEvidenceIDs: c.ConfirmEvidence}
	if c.Estimate != nil {
		v.EstimateMinor = c.Estimate.MinorString()
	}
	if r := c.Conversion.Rate; r != nil {
		v.Conversion.FXRate = &api.ConfigRef{Kind: api.KindFXRate, ConfigID: r.ID, Revision: r.Revision}
		v.Conversion.FXRateHash, v.Conversion.Rate, v.Conversion.To = r.Hash, r.Value.String(), r.To
	}
	for _, b := range c.Beneficiaries {
		v.Beneficiaries = append(v.Beneficiaries, targetView(b))
	}
	for i := range c.Allocations {
		a := &c.Allocations[i]
		v.Allocations = append(v.Allocations, api.ChargeAllocationView{Seq: a.Seq, ChargeVersion: a.ChargeVersion, Basis: a.Basis,
			DataBasis: a.DataBasis, EvidenceIDs: a.EvidenceIDs, AmountMinor: a.Amount.MinorString(), Current: a.Seq == c.AllocationSeq,
			Lines: allocationLines(a), AllocatedBy: a.By.By, AllocatedAt: a.By.At})
	}
	if b := c.Billed; b != nil {
		v.BilledBy, v.BilledAt = b.By, b.At
	}
	if x := c.Confirmed; x != nil {
		v.ConfirmedBy, v.ConfirmedAt, v.ConfirmReason = x.By, x.At, x.Reason
	}
	return v
}

// GetCharge returns a charge (charge.get).
func (s Service) GetCharge(ctx context.Context, id string) (api.ChargeView, error) {
	var v api.ChargeView
	err := s.read(ctx, api.CommandGetCharge, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		c, err := postgres.LoadCharge(ctx, exec, scope(ctx), id, false)
		if err != nil {
			return err
		}
		if c == nil || !d.Sees(c.OwnerActor, c.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v = chargeView(c)
		v.CheckedAt = kernel.Instant(s.Clock.Now())
		return nil
	})
	return v, err
}
