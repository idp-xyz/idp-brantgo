package application

import (
	"context"
	"slices"
	"strings"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/amendment"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/calc"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/order"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/orderrev"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The active order's amendments (WBS S3-15; api/amendment.go): order.amendment.prepare (human or
// AI), submit, decide, apply and withdraw (human only).
//
// Each runs as an order command (order.go): the quote root FOR UPDATE, then the order FOR UPDATE
// (which loads its amendments: every amendment write happens under that lock), then the ledger's
// authority guard row FOR SHARE. Prepare and apply read, before the transaction, what POL says of
// the references the proposal adopts that the current revision does not bind (units, conversions,
// tax applications and profiles, payment terms, trade terms) and turn it into orderrev.Facts;
// apply re-checks inside its transaction, after the decider's grant (Authorizer.Qualify, FOR SHARE),
// CAT's SkuGuard for every SKU specification it adopts and POL's ConfigGuard for every POL revision
// it adopts, with the content hash read before (REFERENCE_STALE when it moved), in context code
// order. FUL's shipments, which QUANTITY_BELOW_SHIPPED judges, are not wired in S3: the commit
// evidence and OrderAmended say so (fulfilment_source not_wired), as they name STL's CreditGuard and
// CNX's TradeComplianceGuard, which an amended revision would need again (TRD-INV-022, 023).

// adopted is a POL revision a proposal adopts, its content hash as read and where the proposal
// names it.
type adopted struct {
	ref  polapi.ConfigRef
	hash string
	path string
}

// spec is a CAT SKU specification a proposal adopts and where.
type spec struct {
	ref  catapi.SpecRef
	path string
}

// amendmentInputs is what an amendment command resolved before its transaction.
type amendmentInputs struct {
	facts   orderrev.Facts
	configs []adopted
	specs   []spec
}

// shipments is FUL's answer for the active order's commands: FUL is not built in S3, so no shipment
// can have been confirmed, and the answer says it was not asked (orderrev.NotWired).
func shipments() orderrev.Fulfilment { return orderrev.Fulfilment{Source: orderrev.NotWired} }

// downstream is PUR's, STK's and STL's answer, not wired either.
func downstream() orderrev.Downstream { return orderrev.Downstream{Source: orderrev.NotWired} }

// proposalOf is the domain proposal of a prepare command.
func proposalOf(cmd api.PrepareAmendment) orderrev.Proposal {
	p := orderrev.Proposal{Lines: []orderrev.ProposedLine{}, Charges: []orderrev.ProposedCharge{}}
	for _, l := range cmd.Lines {
		p.Lines = append(p.Lines, orderrev.ProposedLine{LineID: l.LineID, Sku: quote.SkuRef{SkuID: l.Sku.SkuID, SpecRevision: l.Sku.SpecRevision},
			Quantity: l.Quantity, Unit: ref(l.UnitRef), Conversion: optRef(l.ConversionRef), UnitPrice: l.UnitPrice,
			PriceBasisQuantity: l.PriceBasisQuantity, DiscountRatio: l.DiscountRatio, TaxApplication: ref(l.TaxApplicationRef)})
	}
	for _, c := range cmd.Charges {
		p.Charges = append(p.Charges, orderrev.ProposedCharge{ChargeID: c.ChargeID, Amount: c.Amount})
	}
	t := cmd.Terms
	d := t.Delivery
	delivery := quote.Delivery{Kind: d.Kind, Date: d.Date, Days: d.Days, DayBasis: d.DayBasis, Calendar: optRef(d.CalendarRef),
		Milestone: d.Milestone, Place: business(d.PlaceRef), TimeZone: d.TimeZone}
	if d.AnchorRef != nil {
		a := business(*d.AnchorRef)
		delivery.Anchor = &a
	}
	p.Terms = orderrev.Terms{TradeTerms: ref(t.TradeTermsRef), NamedPlace: business(t.NamedPlaceRef), PaymentTerms: ref(t.PaymentTermsRef),
		Delivery: delivery}
	return p
}

func polRef(r quote.Ref) polapi.ConfigRef {
	return polapi.ConfigRef{Kind: r.Kind, ConfigID: r.ID, Revision: r.Revision}
}

// amendmentInputs resolves, before the transaction of order id, what proposal p needs that the
// order's current revision does not hold (orderrev.Facts), at instant at: the order currency's minor
// units; the units, conversions, tax applications and payment terms of the lines and terms that
// change; and the POL revisions and CAT specifications p newly adopts. A reference the current
// revision binds is read whatever its state; one p adopts anew must be in force (POL's verdict at its
// path). An unknown order, or one that is not active, resolves nothing: the transaction refuses it.
func (s Service) amendmentInputs(ctx context.Context, id string, p orderrev.Proposal, base string, at time.Time) (amendmentInputs, error) {
	var in amendmentInputs
	o, _, err := s.orderOf(ctx, id)
	if err != nil || o == nil || o.Status != order.Active {
		return in, err
	}
	r, err := o.Revision()
	if err != nil {
		return in, err
	}
	in.facts = orderrev.Facts{Units: map[string]calc.Unit{}, Measures: map[orderrev.Measure]orderrev.Measurement{},
		Taxes: map[quote.Ref]*calc.TaxApplication{}}
	need := map[polapi.ConfigRef]string{polRef(r.Currency): ""} // the path of a reference adopted anew; "" when bound already
	adopt := func(q quote.Ref, path string) {
		if _, ok := need[polRef(q)]; !ok || path != "" {
			need[polRef(q)] = path
		}
	}
	read := func(q quote.Ref) {
		if _, ok := need[polRef(q)]; !ok {
			need[polRef(q)] = ""
		}
	}
	type pending struct {
		conversion quote.Ref
		pl         orderrev.ProposedLine
		path       string
		measure    bool
	}
	var conversions []pending
	var taxes []orderrev.ProposedLine
	for i, pl := range p.Lines {
		path := base + "/lines/" + itoa(int64(i))
		cur := r.Line(pl.LineID)
		sameSku := cur != nil && cur.Sku == pl.Sku
		if !sameSku {
			in.specs = append(in.specs, spec{ref: catapi.SpecRef{SkuID: pl.Sku.SkuID, SpecRevision: pl.Sku.SpecRevision}, path: path + "/sku_ref"})
		}
		if !sameSku || cur.Tax.Application != pl.TaxApplication {
			adopt(pl.TaxApplication, path+"/tax_application_ref")
			taxes = append(taxes, pl)
		}
		q, qerr := kernel.ParseDecimal(pl.Quantity, kernel.QuantityFormat)
		curQ, cerr := kernel.ParseDecimal(func() string {
			if cur == nil {
				return ""
			}
			return cur.Quantity
		}(), wideDecimal)
		if sameSku && qerr == nil && cerr == nil && q.Equal(curQ) {
			continue // unchanged quantities need no unit facts
		}
		if cur != nil && cur.Unit == pl.Unit {
			read(pl.Unit)
		} else {
			adopt(pl.Unit, path+"/unit_ref")
		}
		switch {
		case sameSku && cur.Conversion != nil:
			conversions = append(conversions, pending{conversion: *cur.Conversion, pl: pl, path: path + "/conversion_ref"})
		case !sameSku && pl.Conversion != nil:
			conversions = append(conversions, pending{conversion: *pl.Conversion, pl: pl, path: path + "/conversion_ref", measure: true})
		case !sameSku:
			in.facts.Measures[orderrev.Measure{Sku: pl.Sku, Unit: pl.Unit.ID}] = orderrev.Measurement{CostUnit: pl.Unit.ID}
		}
	}
	if p.Terms.PaymentTerms != r.Terms.PaymentTerms {
		adopt(p.Terms.PaymentTerms, base+"/terms/payment_terms_ref")
	}
	if p.Terms.TradeTerms != r.Terms.TradeTerms {
		adopt(p.Terms.TradeTerms, base+"/terms/trade_terms_ref")
	}
	got, err := s.resolve(ctx, need, at, &in)
	if err != nil {
		return in, err
	}
	if c := got[polRef(r.Currency)].Data.Currency; c != nil {
		if in.facts.Currency, err = kernel.NewCurrency(c.Code, c.MinorUnits); err != nil {
			return in, kernel.Reject(kernel.CodeReferenceInvalid, "/order/currency_ref", err.Error())
		}
	}
	for ref, rc := range got {
		if u := rc.Data.Unit; u != nil && ref.Kind == quote.KindUnit {
			in.facts.Units[u.Code] = calc.Unit{Code: u.Code, Precision: u.Precision}
		}
	}
	// The tax applications' profiles, read as their applications name them.
	profiles := map[polapi.ConfigRef]string{}
	for _, pl := range taxes {
		if t := got[polRef(pl.TaxApplication)].Data.TaxApplication; t != nil {
			profiles[t.TaxProfile] = ""
		}
	}
	gotProfiles, err := s.resolve(ctx, profiles, at, &in)
	if err != nil {
		return in, err
	}
	for _, pl := range taxes {
		t := got[polRef(pl.TaxApplication)].Data.TaxApplication
		if t == nil {
			continue // Next refuses the line: the application's profile is needed
		}
		pd := gotProfiles[t.TaxProfile].Data.TaxProfile
		if pd == nil {
			continue
		}
		rate, err := kernel.ParseDecimal(pd.Rate, kernel.FactorFormat)
		if err != nil {
			continue
		}
		in.facts.Taxes[pl.TaxApplication] = &calc.TaxApplication{Ref: quote.CalcRef(pl.TaxApplication), Side: t.Side, SubjectKind: t.SubjectKind,
			SubjectID: t.SubjectID, PartyID: t.PartyID, Profile: calc.TaxProfile{Ref: calc.Ref{Kind: t.TaxProfile.Kind, ID: t.TaxProfile.ConfigID,
				Revision: t.TaxProfile.Revision}, Rate: rate, PriceBasis: calc.PriceBasis(pd.PriceBasis), Recovery: pd.RecoveryTreatment}}
	}
	if p.Terms.PaymentTerms != r.Terms.PaymentTerms {
		if pt := got[polRef(p.Terms.PaymentTerms)].Data.PaymentTerms; pt != nil {
			var calendar *quote.Ref
			if c := pt.Calendar; c != nil {
				calendar = &quote.Ref{Kind: c.Kind, ID: c.ConfigID, Revision: c.Revision}
			}
			for _, st := range pt.Stages {
				in.facts.Stages = append(in.facts.Stages, orderrev.Stage{StageID: st.StageID, Percent: st.Percent, Trigger: st.Trigger, Days: st.Days,
					DayBasis: pt.DayBasis, Calendar: calendar, ReleaseControl: st.ReleaseControl})
			}
		}
	}
	// Conversions: POL's own lookup, in force, with the cost unit's precision.
	for _, c := range conversions {
		x, err := s.Upstream.Configs.LookupUnitConversion(ctx, polapi.UnitQuery{Conversion: polRef(c.conversion), SkuID: c.pl.Sku.SkuID,
			SpecRevision: c.pl.Sku.SpecRevision, At: at})
		if err != nil {
			return in, moved(err, "/", c.path)
		}
		to := string(x.To)
		in.facts.Units[to] = calc.Unit{Code: to, Precision: x.ToPrecision}
		in.configs = append(in.configs, adopted{ref: polRef(c.conversion), hash: x.ContentHash, path: c.path})
		if c.measure {
			in.facts.Measures[orderrev.Measure{Sku: c.pl.Sku, Unit: c.pl.Unit.ID}] = orderrev.Measurement{CostUnit: to,
				Conversion: &calc.Conversion{Ref: quote.CalcRef(c.conversion), SkuID: c.pl.Sku.SkuID, SpecRevision: c.pl.Sku.SpecRevision,
					From: string(x.From), To: to, Factor: x.Factor}}
		}
	}
	return in, nil
}

// wideDecimal reads stored quantities.
var wideDecimal = kernel.DecimalFormat{IntDigits: 24, Scale: 24}

// resolve asks POL for refs at instant at; a reference with a path is adopted anew and must be in
// force (POL's refusal at that path), and goes into in.configs for the transaction's guard.
func (s Service) resolve(ctx context.Context, refs map[polapi.ConfigRef]string, at time.Time, in *amendmentInputs) (map[polapi.ConfigRef]polapi.ResolvedConfig, error) {
	if len(refs) == 0 {
		return map[polapi.ConfigRef]polapi.ResolvedConfig{}, nil
	}
	list := make([]polapi.ConfigRef, 0, len(refs))
	for r := range refs {
		list = append(list, r)
	}
	slices.SortFunc(list, func(a, b polapi.ConfigRef) int {
		return strings.Compare(a.Kind+"/"+a.ConfigID+"/"+itoa(a.Revision), b.Kind+"/"+b.ConfigID+"/"+itoa(b.Revision))
	})
	got, err := s.Upstream.Configs.ResolveConfigs(ctx, list, at)
	if err != nil {
		return nil, err
	}
	for _, r := range list {
		path := refs[r]
		if path == "" {
			continue
		}
		rc, ok := got[r]
		if !ok {
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, path, "")
		}
		if err := rc.Check(path); err != nil {
			return nil, err
		}
		in.configs = append(in.configs, adopted{ref: r, hash: rc.ContentHash, path: path})
	}
	return got, nil
}

// guardAdopted re-checks, inside the transaction after the order's locks and the authority guard,
// the SKU specifications (CAT SkuGuard) and POL revisions (ConfigGuard, content hash as read) the
// proposal adopts, in context code order, and returns their evidence entries.
func guardAdopted(ctx context.Context, exec bentopg.Executor, in amendmentInputs, at time.Time) ([]quote.Entry, error) {
	var entries []quote.Entry
	specs := slices.Clone(in.specs)
	slices.SortFunc(specs, func(a, b spec) int {
		return strings.Compare(a.ref.SkuID+"@"+itoa(a.ref.SpecRevision), b.ref.SkuID+"@"+itoa(b.ref.SpecRevision))
	})
	for _, sp := range specs {
		v, err := catapi.ScanSkuGuard(sp.ref, exec.QueryRow(ctx, catapi.SkuGuardSQL, catapi.SkuGuardArgs(sp.ref)...).Scan)
		if err != nil {
			return nil, err
		}
		if err = v.Check(sp.path); err != nil {
			return nil, err
		}
		entries = append(entries, quote.Entry{Name: "cat:" + sp.ref.SkuID + "@" + itoa(sp.ref.SpecRevision), Value: "current"})
	}
	configs := slices.Clone(in.configs)
	slices.SortFunc(configs, func(a, b adopted) int {
		return strings.Compare(a.ref.Kind+"/"+a.ref.ConfigID+"/"+itoa(a.ref.Revision), b.ref.Kind+"/"+b.ref.ConfigID+"/"+itoa(b.ref.Revision))
	})
	seen := map[polapi.ConfigRef]bool{}
	for _, c := range configs {
		if seen[c.ref] {
			continue
		}
		seen[c.ref] = true
		v, err := polapi.ScanConfigGuard(c.ref, exec.QueryRow(ctx, polapi.ConfigGuardSQL, polapi.ConfigGuardArgs(c.ref, at)...).Scan)
		if err != nil {
			return nil, err
		}
		if err = v.Check(c.path); err != nil {
			return nil, err
		}
		if v.ContentHash != c.hash {
			return nil, api.Reject(api.CodeReferenceStale, c.path)
		}
		entries = append(entries, quote.Entry{Name: "pol:" + c.ref.Kind + ":" + c.ref.ConfigID + "@" + itoa(c.ref.Revision), Value: "in_force/" + c.hash})
	}
	return entries, nil
}

// checkCited refuses evidence the actor does not read (REFERENCE_INVALID at path/<i>/source_id).
func checkCited(es []quote.Evidence, readable map[string]bool, path string) error {
	for i, e := range es {
		if !readable[e.SourceID] {
			return kernel.Reject(kernel.CodeReferenceInvalid, path+"/"+itoa(int64(i))+"/source_id", "")
		}
	}
	return nil
}

func impactOf(as []api.ImpactAssessment) []amendment.Assessment {
	out := make([]amendment.Assessment, len(as))
	for i, a := range as {
		out[i] = amendment.Assessment{Area: a.Area, Status: a.Status, Note: a.Note, Evidence: evidence(a.Evidence)}
	}
	return out
}

// saveAmendment writes the order command's amendment and answers with the amendment's version and
// state.
func (h *orderCommand) saveAmendment(ctx context.Context, exec bentopg.Executor, a *amendment.Amendment, events ...action.Event) (action.Outcome, error) {
	out, err := h.save(ctx, exec, string(a.Status), events...)
	out.RowVersion = a.Version
	return out, err
}

// amendmentEvent is an amendment event on the SalesOrder aggregate, at the order's row version, so
// that it is ordered with the order's other events.
func (h *orderCommand) amendmentEvent(eventType string, a *amendment.Amendment, fields map[string]any) action.Event {
	base := map[string]any{"amendment_id": a.ID, "amendment_hash": a.Hash, "amendment_version": a.Version, "amendment_status": string(a.Status),
		"base_order_revision": a.Content.Base.Revision, "base_order_hash": a.Content.Base.Hash}
	for k, v := range fields {
		base[k] = v
	}
	return h.event(eventType, base)
}

// diffFields are what OrderAmended and OrderAmendmentRequested say of the change.
func diffFields(d *orderrev.Diff) map[string]any {
	lines := make([]map[string]any, 0, len(d.Lines))
	for _, l := range d.Lines {
		lines = append(lines, map[string]any{"line_id": l.LineID, "change": l.Change, "fields": strings.Join(l.Fields, ","), "from_quantity": l.From,
			"to_quantity": l.To, "unit": l.Unit})
	}
	charges := make([]map[string]any, 0, len(d.Charges))
	for _, c := range d.Charges {
		charges = append(charges, map[string]any{"charge_id": c.ChargeID, "change": c.Change, "from_amount": c.From, "to_amount": c.To})
	}
	return map[string]any{"changed_lines": lines, "changed_charges": charges, "changed_terms": strings.Join(d.Terms, ","),
		"from_payable_minor": d.From.Payable, "to_payable_minor": d.To.Payable, "from_net_minor": d.From.Net, "to_net_minor": d.To.Net}
}

// PrepareAmendment prepares an amendment of an active order (order.amendment.prepare).
func (s Service) PrepareAmendment(ctx context.Context, cmd api.PrepareAmendment) (api.AmendmentReceipt, error) {
	h := &orderCommand{s: s, orderID: cmd.OrderID}
	p := proposalOf(cmd)
	src := amendment.Source{Kind: cmd.Source.Kind, ResponseID: cmd.Source.ResponseID, Evidence: evidence(cmd.Source.Evidence)}
	impact := impactOf(cmd.Impact)
	var in amendmentInputs
	var readable map[string]bool
	h.prepare = func(ctx context.Context) error {
		ids := evidenceIDs(src.Evidence)
		for _, a := range impact {
			ids = append(ids, evidenceIDs(a.Evidence)...)
		}
		var err error
		if readable, err = s.readEvidence(ctx, ids); err != nil {
			return err
		}
		in, err = s.amendmentInputs(ctx, cmd.OrderID, p, "/args", s.Clock.Now())
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := h.admit(ctx, d, "/args/order_id"); err != nil {
			return action.Outcome{}, err
		}
		if err := checkCited(src.Evidence, readable, "/args/source/evidence"); err != nil {
			return action.Outcome{}, err
		}
		for i, a := range impact {
			if err := checkCited(a.Evidence, readable, "/args/impact/"+itoa(int64(i))+"/evidence"); err != nil {
				return action.Outcome{}, err
			}
		}
		if src.Kind == amendment.SourceCustomerResponse {
			r, err := postgres.LoadResponse(ctx, exec, scope(ctx), src.ResponseID)
			if err != nil {
				return action.Outcome{}, err
			}
			if r == nil || r.QuoteID != h.o.Source.QuoteID {
				return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/source/response_id", "No such response of the order's quote")
			}
		}
		a, preview, diff, err := h.o.PrepareAmendment(amendment.Preparation{AmendmentID: cmd.AmendmentID, OrderID: cmd.OrderID,
			ExpectedVersion: cmd.ExpectedVersion, Base: orderrev.Link{Revision: cmd.ExpectedOrderRevision, Hash: cmd.ExpectedOrderHash},
			Source: src, Proposal: p, Impact: impact}, shipments(), in.facts, d.Actor, d.Epoch, s.Clock.Now())
		if err != nil {
			return action.Outcome{}, err
		}
		fields := diffFields(diff)
		fields["preview_order_revision"], fields["preview_order_hash"] = preview.Number, preview.Hash
		fields["source_kind"], fields["response_id"] = src.Kind, src.ResponseID
		fields["open_items"] = strings.Join(a.OpenItems(), ",")
		return h.saveAmendment(ctx, exec, a, h.amendmentEvent(api.EventOrderAmendmentRequested, a, fields))
	}
	return s.runAmendment(ctx, api.CommandPrepareAmendment, cmd.IdempotencyKey, cmd.OrderID, "", cmd, cmd.Validate(), h, cmd.AmendmentID)
}

// SubmitAmendment submits a draft amendment for approval (order.amendment.submit).
func (s Service) SubmitAmendment(ctx context.Context, cmd api.SubmitAmendment) (api.AmendmentReceipt, error) {
	h := &orderCommand{s: s, orderID: cmd.OrderID}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := h.admit(ctx, d, "/args/order_id"); err != nil {
			return action.Outcome{}, err
		}
		a, err := h.o.SubmitAmendment(cmd.AmendmentID, cmd.ExpectedVersion, d.Actor, d.Epoch, s.Clock.Now())
		if err != nil {
			return action.Outcome{}, err
		}
		return h.saveAmendment(ctx, exec, a)
	}
	return s.runAmendment(ctx, api.CommandSubmitAmendment, cmd.IdempotencyKey, cmd.OrderID, "", cmd, cmd.Validate(), h, cmd.AmendmentID)
}

// DecideAmendment approves or rejects a pending amendment (order.amendment.decide).
func (s Service) DecideAmendment(ctx context.Context, cmd api.DecideAmendment) (api.AmendmentReceipt, error) {
	h := &orderCommand{s: s, orderID: cmd.OrderID}
	proofs := evidence(cmd.Evidence)
	var readable map[string]bool
	h.prepare = func(ctx context.Context) error {
		var err error
		readable, err = s.readEvidence(ctx, evidenceIDs(proofs))
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := h.admit(ctx, d, "/args/order_id"); err != nil {
			return action.Outcome{}, err
		}
		own, err := s.Ledger.Authorizer.Qualify(ctx, exec, d.Actor.ID)
		if err != nil {
			return action.Outcome{}, err
		}
		if !own.Holds(amendment.PermissionRead) {
			// The decider reads what it approves now, as an order reviewer must.
			return action.Outcome{}, kernel.Reject(kernel.CodePermissionDenied, "/", "")
		}
		a, err := h.o.DecideAmendment(cmd.AmendmentID, cmd.ExpectedVersion, amendment.Decision{Hash: cmd.AmendmentHash,
			Approve: cmd.Decision == api.DecisionApprove, Reason: cmd.Reason, Evidence: proofs}, d.Actor, d.Epoch, s.Clock.Now())
		if err != nil {
			return action.Outcome{}, err
		}
		if err = checkCited(proofs, readable, "/args/evidence"); err != nil {
			return action.Outcome{}, err
		}
		return h.saveAmendment(ctx, exec, a, h.amendmentEvent(api.EventOrderAmendmentDecided, a, map[string]any{"decision": cmd.Decision,
			"decided_by": a.Decided.By.ID}))
	}
	return s.runAmendment(ctx, api.CommandDecideAmendment, cmd.IdempotencyKey, cmd.OrderID, cmd.Reason, cmd, cmd.Validate(), h, cmd.AmendmentID)
}

// ApplyAmendment applies an approved amendment: the order moves to its next revision
// (order.amendment.apply).
func (s Service) ApplyAmendment(ctx context.Context, cmd api.ApplyAmendment) (api.AmendmentReceipt, error) {
	h := &orderCommand{s: s, orderID: cmd.OrderID}
	var in amendmentInputs
	h.prepare = func(ctx context.Context) error {
		o, _, err := s.orderOf(ctx, cmd.OrderID)
		if err != nil || o == nil {
			return err
		}
		a := o.Amendment(cmd.AmendmentID)
		if a == nil {
			return nil
		}
		in, err = s.amendmentInputs(ctx, cmd.OrderID, a.Content.Proposal, "/proposal", s.Clock.Now())
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := h.admit(ctx, d, "/args/order_id"); err != nil {
			return action.Outcome{}, err
		}
		now := s.Clock.Now()
		standings := s.standings(ctx, exec, h.q)
		// The decider's grant first (the authority guard, the first upstream guard in lock order),
		// then CAT and POL.
		if a := h.o.Amendment(cmd.AmendmentID); a != nil && a.Decided != nil {
			if _, err := standings(a.Decided.By.ID); err != nil {
				return action.Outcome{}, err
			}
		}
		var upstream []quote.Entry
		if a := h.o.Amendment(cmd.AmendmentID); a != nil && a.Status == amendment.Approved && a.Version == cmd.ExpectedVersion &&
			h.o.Status == order.Active && !a.Stale(amendment.Order{Status: orderrev.Active, Current: current(h.o)}) {
			var err error
			if upstream, err = guardAdopted(ctx, exec, in, now); err != nil {
				return action.Outcome{}, err
			}
		}
		upstream = append([]quote.Entry{{Name: "upstream_checked_at", Value: kernel.FormatTimestamp(now)}}, upstream...)
		upstream = append(upstream, quote.Entry{Name: api.GuardCNXTradeCompliance, Value: notWired},
			quote.Entry{Name: api.GuardSTLCredit, Value: notWired})
		a, next, diff, err := h.o.ApplyAmendment(cmd.AmendmentID, amendment.Applying{ExpectedVersion: cmd.ExpectedVersion,
			IdempotencyKey: cmd.IdempotencyKey, Fulfilment: shipments(), Facts: in.facts, Upstream: upstream, ControlEpoch: h.q.Control.Epoch},
			standings, d.Actor, d.Epoch, now)
		if err != nil {
			return action.Outcome{}, err
		}
		fields := diffFields(diff)
		fields["from_order_revision"], fields["from_order_hash"] = a.Applied.From.Revision, a.Applied.From.Hash
		fields["fulfilment_source"] = orderrev.NotWired
		fields["unchecked_guards"] = joinGuards([]string{api.GuardCNXTradeCompliance, api.GuardSTLCredit, api.GuardFULShipments})
		fields["applied_at"] = kernel.FormatTimestamp(a.Applied.Act.At)
		_ = next
		return h.saveAmendment(ctx, exec, a, h.amendmentEvent(api.EventOrderAmended, a, fields))
	}
	return s.runAmendment(ctx, api.CommandApplyAmendment, cmd.IdempotencyKey, cmd.OrderID, "", cmd, cmd.Validate(), h, cmd.AmendmentID)
}

// current is the order's current revision for a staleness check, nil when it cannot be read (the
// command refuses then).
func current(o *order.Order) *orderrev.Revision {
	r, err := o.Revision()
	if err != nil {
		return nil
	}
	return r
}

// WithdrawAmendment withdraws a draft or pending amendment (order.amendment.withdraw).
func (s Service) WithdrawAmendment(ctx context.Context, cmd api.WithdrawAmendment) (api.AmendmentReceipt, error) {
	h := &orderCommand{s: s, orderID: cmd.OrderID}
	proofs := evidence(cmd.Evidence)
	var readable map[string]bool
	h.prepare = func(ctx context.Context) error {
		var err error
		readable, err = s.readEvidence(ctx, evidenceIDs(proofs))
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := h.admit(ctx, d, "/args/order_id"); err != nil {
			return action.Outcome{}, err
		}
		a, err := h.o.WithdrawAmendment(cmd.AmendmentID, cmd.ExpectedVersion, quote.Act{By: d.Actor, Epoch: d.Epoch, At: s.Clock.Now(),
			Reason: cmd.Reason, Evidence: proofs})
		if err != nil {
			return action.Outcome{}, err
		}
		if err = checkCited(proofs, readable, "/args/evidence"); err != nil {
			return action.Outcome{}, err
		}
		return h.saveAmendment(ctx, exec, a, h.amendmentEvent(api.EventOrderAmendmentWithdrawn, a, nil))
	}
	return s.runAmendment(ctx, api.CommandWithdrawAmendment, cmd.IdempotencyKey, cmd.OrderID, cmd.Reason, cmd, cmd.Validate(), h, cmd.AmendmentID)
}

// runAmendment runs an amendment command and completes its receipt from the stored order.
func (s Service) runAmendment(ctx context.Context, name, key, orderID, reason string, req api.Command, validation error, h *orderCommand,
	amendmentID string) (api.AmendmentReceipt, error) {
	res, err := s.run(ctx, action.Command{Name: name, IdempotencyKey: key, ObjectID: orderID, Reason: reason}, req, validation, h)
	if err != nil {
		return api.AmendmentReceipt{}, err
	}
	out := api.AmendmentReceipt{Receipt: res, AmendmentID: amendmentID}
	err = s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		o, err := postgres.LoadOrder(ctx, exec, scope(ctx), res.ObjectID, postgres.NoLock)
		if err != nil || o == nil {
			return err
		}
		out.OrderRevision, out.OrderHash, out.OrderStatus, out.OrderRowVersion = o.OrderRevision, o.OrderHash, string(o.Status), o.RowVersion
		if a := o.Amendment(amendmentID); a != nil {
			out.AmendmentHash, out.BaseRevision, out.BaseHash = a.Hash, a.Content.Base.Revision, a.Content.Base.Hash
		}
		return nil
	})
	return out, err
}

// amendmentViews are the order's later revisions, amendments, their commit evidence and end as the
// reads show them.
func amendmentViews(o *order.Order, v *api.OrderView) {
	v.SnapshotHash = o.SnapshotHash
	if o.OrderRevision > 1 {
		if r, err := o.Revision(); err == nil {
			v.CurrentRevision = string(r.Document)
		}
	}
	for _, r := range o.Revisions {
		v.Revisions = append(v.Revisions, api.OrderRevisionView{OrderRevision: r.Revision, OrderHash: r.Hash, PreviousHash: r.PreviousHash,
			AmendmentID: r.AmendmentID, AmendmentHash: r.AmendmentHash, RowVersion: r.RowVersion, At: r.At})
	}
	st := amendment.Order{Status: orderrev.Status(o.Status), Current: current(o)}
	for _, a := range o.Amendments {
		doc, _ := a.Document()
		av := api.OrderAmendmentView{AmendmentID: a.ID, Version: a.Version, Status: string(a.Status), BaseRevision: a.Content.Base.Revision,
			BaseHash: a.Content.Base.Hash, AmendmentHash: a.Hash, Content: string(doc), Preparers: a.Preparers(), OpenItems: a.OpenItems(),
			Stale: a.Stale(st)}
		if x := a.Submitted; x != nil {
			av.SubmittedBy, av.SubmittedAt = x.By, x.At
		}
		if x := a.Decided; x != nil {
			av.DecidedBy, av.DecidedAt, av.Reason = x.By, x.At, x.Reason
		}
		if x := a.Applied; x != nil {
			av.AppliedBy, av.AppliedAt, av.AppliedRevision = x.Act.By, x.Act.At, x.To.Revision
		}
		if x := a.Withdrawn; x != nil {
			av.WithdrawnBy, av.WithdrawnAt, av.WithdrawReason = x.By, x.At, x.Reason
		}
		v.Amendments = append(v.Amendments, av)
	}
	for _, c := range o.AmendmentCommits {
		entries := map[string]string{}
		for _, e := range c.Entries {
			entries[e.Name] = e.Value
		}
		doc, _ := c.EntriesJSON()
		v.AmendmentCommits = append(v.AmendmentCommits, api.OrderCommitView{IdempotencyKey: c.IdempotencyKey, OrderRevision: c.OrderRevision,
			OrderHash: c.OrderHash, AuthorityEpoch: c.AuthorityEpoch, ControlEpoch: c.ControlEpoch, RowVersion: c.RowVersion, CommittedBy: c.By,
			CommittedAt: c.At, Entries: entries, EntriesSHA256: quote.Hash(doc)})
	}
	if e := o.End; e != nil {
		ev := &api.OrderEndView{Command: e.Command, Status: string(e.Status), OrderRevision: e.OrderRevision, OrderHash: e.OrderHash,
			FulfilmentSource: e.Fulfilment, DownstreamSource: e.Downstream, EndedBy: e.Act.By, EndedAt: e.Act.At, Reason: e.Act.Reason,
			Evidence: apiEvidence(e.Act.Evidence), RowVersion: e.RowVersion}
		for _, l := range e.Lines {
			ev.Lines = append(ev.Lines, api.OrderEndLine(l))
		}
		v.End = ev
	}
}
