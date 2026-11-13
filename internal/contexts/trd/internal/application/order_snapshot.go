package application

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"strings"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/amendment"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/order"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/orderrev"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The order snapshot lookups (S5 plan unit A2; Context Map R27 TRD→FUL, R32 TRD→STL, R46 TRD→CNX;
// api/order_snapshot.go): OrderSnapshotLookups for drafts and active orders, the Terms block of
// ActiveOrderRef v1 and the audited read order.snapshot.get. Every fact is one TRD sealed: the
// order's current revision, the source revision's commercial document its snapshot holds and the
// bindings of its S/C and PI. The Incoterm and edition are what the POL trade_terms revision the
// order binds states, resolved through POL's ConfigRef v1 lookup (R15) after TRD's own read
// transaction ended — TRD never calls another context from inside its transaction.

// CommandGetOrderSnapshot is the audited read of an order's snapshot fact (trd.read).
const CommandGetOrderSnapshot = "order.snapshot.get"

// ResolveOrderSnapshots answers OrderCommercialSnapshot v1 lookups (api.OrderSnapshotLookups): only
// an active grant is needed; orders of quotes outside the caller's data scope are absent.
func (s Service) ResolveOrderSnapshots(ctx context.Context, ids []string) (map[string]api.OrderSnapshotFact, error) {
	if err := api.ValidateOrderIDs(ids); err != nil {
		return nil, err
	}
	out := map[string]api.OrderSnapshotFact{}
	orders := map[string]*order.Order{}
	reads := map[string]revisionRead{}
	shipped := map[string]fulfilmentRead{}
	err := s.lookupAcceptances(ctx, func(ctx context.Context, exec bentopg.Executor, sees func(quoteID string) (bool, error)) error {
		for _, id := range ids {
			o, err := postgres.LoadOrder(ctx, exec, scope(ctx), id, postgres.NoLock)
			if err != nil {
				return err
			}
			if o == nil {
				continue
			}
			ok, err := sees(o.Source.QuoteID)
			if err != nil {
				return err
			}
			if ok {
				out[id], orders[id] = orderSnapshotFact(o, scope(ctx)), o
				if x, ok := currentRead(ctx, o); ok {
					reads[id] = x
				}
				if shipped[id], err = s.readFulfilment(ctx, exec, o); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err = s.stateSnapshotTrade(ctx, out); err != nil {
		return nil, err
	}
	if err = s.stateSnapshotCosts(ctx, reads, out); err != nil {
		return nil, err
	}
	return out, s.stateProposed(ctx, orders, shipped, out)
}

// currentRead is order o's snapshot and current revision as TRD's read transaction (ctx) reads
// them, when both are intact.
func currentRead(ctx context.Context, o *order.Order) (revisionRead, bool) {
	snap, err := o.Verify(scope(ctx), "/order_ids")
	if err != nil {
		return revisionRead{}, false
	}
	r, err := o.Revision()
	if err != nil {
		return revisionRead{}, false
	}
	return revisionRead{snap: snap, r: r}, true
}

// stateSnapshotCosts states, after TRD's read transaction, the costing cost of the lines of every
// intact fact (S5R-D33, OrderLine.CostMinor; lineCosts) from the revisions that transaction read.
func (s Service) stateSnapshotCosts(ctx context.Context, reads map[string]revisionRead, facts map[string]api.OrderSnapshotFact) error {
	for id := range reads {
		if facts[id].Code != "" {
			delete(reads, id)
		}
	}
	costs, err := lineCosts(ctx, s, reads)
	if err != nil {
		return err
	}
	for id, c := range costs {
		f := facts[id]
		for i, l := range f.Lines {
			f.Lines[i].CostMinor = c[l.LineID]
		}
		facts[id] = f
	}
	return nil
}

// fulfilmentRead is what FUL confirmed of an order, or the rejection that says TRD cannot compare
// FUL's answer with the order's revision.
type fulfilmentRead struct {
	f       orderrev.Fulfilment
	refusal error
}

// readFulfilment reads in TRD's read transaction what FUL confirmed of order o (ShipmentFacts v1),
// for the revisions its open amendments propose.
func (s Service) readFulfilment(ctx context.Context, exec bentopg.Executor, o *order.Order) (fulfilmentRead, error) {
	if o.Status != order.Active {
		return fulfilmentRead{}, nil
	}
	f, err := s.activeFulfilment(ctx, exec, o)
	if _, ok := kernel.AsRejection(err); ok {
		return fulfilmentRead{refusal: err}, nil
	}
	return fulfilmentRead{f: f}, err
}

// GetOrderSnapshot returns what ResolveOrderSnapshots says of order id, audited as
// order.snapshot.get (trd.read): NOT_FOUND for an order of a quote the caller does not see.
func (s Service) GetOrderSnapshot(ctx context.Context, id string) (api.OrderSnapshotFact, error) {
	var f api.OrderSnapshotFact
	var loaded *order.Order
	var shipped fulfilmentRead
	reads := map[string]revisionRead{}
	err := s.Ledger.Read(ctx, CommandGetOrderSnapshot, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		o, err := postgres.LoadOrder(ctx, exec, scope(ctx), id, postgres.NoLock)
		loaded = o
		if err != nil {
			return err
		}
		if o == nil {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		root, err := postgres.LoadRoot(ctx, exec, scope(ctx), o.Source.QuoteID)
		if err != nil {
			return err
		}
		if root == nil || !d.Sees(root.OwnerActor, root.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		f = orderSnapshotFact(o, scope(ctx))
		if x, ok := currentRead(ctx, o); ok {
			reads[id] = x
		}
		shipped, err = s.readFulfilment(ctx, exec, o)
		return err
	})
	if err != nil {
		return api.OrderSnapshotFact{}, err
	}
	facts := map[string]api.OrderSnapshotFact{id: f}
	if err = s.stateSnapshotTrade(ctx, facts); err != nil {
		return api.OrderSnapshotFact{}, err
	}
	if err = s.stateSnapshotCosts(ctx, reads, facts); err != nil {
		return api.OrderSnapshotFact{}, err
	}
	if err = s.stateProposed(ctx, map[string]*order.Order{id: loaded}, map[string]fulfilmentRead{id: shipped}, facts); err != nil {
		return api.OrderSnapshotFact{}, err
	}
	return facts[id], nil
}

// stateProposed states, after TRD's read transaction, the revisions the open amendments of every
// intact active fact propose (api.ProposedRevision; S5 D-9, A6 gap 8): for each amendment pending
// approval or approved on the order's current revision, in the order's amendment order, the
// revision orderrev.Next derives from it with the facts POL gives now (amendmentInputs, what
// order.amendment.apply reads) and the shipments FUL confirmed as TRD's read transaction saw them
// (shipped). An amendment whose revision cannot be derived now carries the refusal's code.
func (s Service) stateProposed(ctx context.Context, orders map[string]*order.Order, shipped map[string]fulfilmentRead,
	facts map[string]api.OrderSnapshotFact) error {
	for _, id := range slices.Sorted(maps.Keys(orders)) {
		o, f := orders[id], facts[id]
		if o == nil || f.Code != "" || o.Status != order.Active {
			continue
		}
		st := amendment.Order{Status: orderrev.Active, Current: current(o)}
		for _, a := range o.Amendments {
			if !a.Proposes(st) {
				continue
			}
			p := api.ProposedRevision{AmendmentID: a.ID, AmendmentHash: a.Hash, AmendmentStatus: string(a.Status)}
			in, err := s.amendmentInputs(ctx, id, a.Content.Proposal, "/proposal", s.Clock.Now())
			if err == nil {
				err = shipped[id].refusal
			}
			var next *orderrev.Revision
			if err == nil {
				next, err = a.Preview(st, shipped[id].f, in.facts)
			}
			if err != nil {
				rej, ok := kernel.AsRejection(err)
				if !ok {
					return err
				}
				p.Code = rej.Code
				f.Proposed = append(f.Proposed, p)
				continue
			}
			p.Binding = api.OrderBinding{OrderID: id, OrderRevision: next.Number, OrderHash: next.Hash}
			if p.Stages, p.CreditPortionMinor, err = paymentPlan(next); err != nil {
				p = api.ProposedRevision{AmendmentID: a.ID, AmendmentHash: a.Hash, AmendmentStatus: string(a.Status), Code: kernel.CodeReferenceInvalid}
			} else {
				p.NetMinor, p.TaxMinor, p.PayableMinor = next.Amounts.Net, next.Amounts.Tax, next.Amounts.Payable
			}
			f.Proposed = append(f.Proposed, p)
		}
		facts[id] = f
	}
	return nil
}

// paymentPlan is revision r's payment plan as OrderTerms shows it, with its credit portion.
func paymentPlan(r *orderrev.Revision) ([]api.PaymentStage, string, error) {
	portion, err := orderrev.CreditPortion(r.Amounts.Payments)
	if err != nil {
		return nil, "", err
	}
	var stages []api.PaymentStage
	for _, p := range r.Amounts.Payments {
		stages = append(stages, api.PaymentStage{StageID: p.StageID, Trigger: p.Trigger, Days: p.Days, DayBasis: p.DayBasis,
			CalendarRef: optAPIRef(p.Calendar), Percent: p.Percent, AmountMinor: p.AmountMinor, ReleaseControl: p.ReleaseControl,
			OnCredit: orderrev.OnCredit(p)})
	}
	return stages, portion, nil
}

// orderSnapshotFact is what OrderCommercialSnapshot v1 says of order o now: its head always, the
// rest when its snapshot and current revision are intact (Code REFERENCE_INVALID otherwise). The
// Incoterm and edition are left to stateSnapshotTrade.
func orderSnapshotFact(o *order.Order, sc kernel.Scope) api.OrderSnapshotFact {
	head := api.OrderSnapshotFact{Binding: api.OrderBinding{OrderID: o.ID, OrderRevision: o.OrderRevision, OrderHash: o.OrderHash},
		Status: string(o.Status), Ended: o.Ended(), RowVersion: o.RowVersion, QuoteID: o.Source.QuoteID, BusinessRevision: o.Source.Revision,
		BusinessHash: o.Source.BusinessHash, AcceptanceID: o.Source.AcceptanceID}
	invalid := head
	invalid.Code = kernel.CodeReferenceInvalid
	snap, err := o.Verify(sc, "/order_ids")
	if err != nil {
		return invalid
	}
	// The current revision: the snapshot at revision 1, an amendment's revision after (S3-15).
	r, err := o.Revision()
	if err != nil {
		return invalid
	}
	terms, err := orderTerms(snap, r)
	if err != nil {
		return invalid
	}
	fx, err := snap.Analysis("/order_ids")
	if err != nil {
		return invalid
	}
	f := head
	f.CustomerRef = api.PartyRef{PartyID: r.Customer.PartyID, Revision: r.Customer.Revision}
	f.CurrencyRef = apiRef(r.Currency)
	f.OrderTerms = terms
	f.Delivery = apiDelivery(r.Terms.Delivery)
	f.Lines = orderLines(r)
	f.Charges = orderCharges(r)
	f.NetMinor, f.TaxMinor, f.PayableMinor = r.Amounts.Net, r.Amounts.Tax, r.Amounts.Payable
	for _, d := range o.Documents {
		b := d.Binding
		ref := api.OrderDocumentRef{DocumentKind: b.DocumentKind, Number: b.Number, OrderRevision: b.OrderRevision, OrderHash: b.OrderHash,
			DocumentSHA256: b.ContentSHA256, Current: b.OrderRevision == o.OrderRevision && b.OrderHash == o.OrderHash}
		for _, x := range d.Signatures {
			ref.Signatures = append(ref.Signatures, api.SignedCopyRef{SignatureKind: x.Kind, FileID: x.FileID, FileSHA256: x.FileSHA256})
		}
		f.DocumentRefs = append(f.DocumentRefs, ref)
	}
	slices.SortStableFunc(f.DocumentRefs, func(a, b api.OrderDocumentRef) int {
		return cmp.Or(cmp.Compare(a.OrderRevision, b.OrderRevision), strings.Compare(a.DocumentKind, b.DocumentKind))
	})
	f.AnalysisFX = api.AnalysisFX{OrderCurrency: fx.OrderCurrency, ComparisonCurrencyRef: apiRef(fx.Comparison), FXRateRef: optAPIRef(fx.FX),
		Rate: fx.Rate}
	return f
}

// orderLines are the lines of revision r as ActiveOrderRef v1 shows them, with their amounts.
func orderLines(r *orderrev.Revision) []api.OrderLine {
	var out []api.OrderLine
	for _, l := range r.Lines {
		a, _ := r.LineAmounts(l.LineID)
		out = append(out, api.OrderLine{LineID: l.LineID, RfqLineID: l.RfqLineID, Sku: api.SkuRef{SkuID: l.Sku.SkuID,
			SpecRevision: l.Sku.SpecRevision}, Quantity: l.Quantity, UnitRef: apiRef(l.Unit), NormalizedQuantity: l.NormalizedQuantity,
			CostUnit: l.CostUnit, UnitPrice: l.UnitPrice, PriceBasisQuantity: l.PriceBasisQuantity, DiscountRatio: l.DiscountRatio,
			NetMinor: a.NetMinor, TaxMinor: a.TaxMinor, PayableMinor: a.PayableMinor})
	}
	return out
}

// orderTerms are the terms of revision r of the order whose snapshot is snap (api.OrderTerms): its
// trade terms reference and named place, the customer addresses the source revision sealed with
// their roles, its payment terms reference and the payment plan its amounts hold, with the credit
// portion of that plan (orderrev.CreditPortion), and the notify party it binds. What r sealed
// (S5R-D35, D40) gives the Incoterm and edition, the countries and the notify party's address; a
// revision written before S5R seals none, and the Incoterm and edition are then POL's
// (stateTradeTerms).
func orderTerms(snap order.Snapshot, r *orderrev.Revision) (api.OrderTerms, error) {
	addresses, err := snap.Addresses("/order_ids")
	if err != nil {
		return api.OrderTerms{}, err
	}
	stages, portion, err := paymentPlan(r)
	if err != nil {
		return api.OrderTerms{}, err
	}
	t := api.OrderTerms{TradeTerms: api.TradeTerms{Ref: apiRef(r.Terms.TradeTerms), NamedPlace: apiBusiness(r.Terms.NamedPlace)},
		PaymentTerms: apiRef(r.Terms.PaymentTerms), Stages: stages, CreditPortionMinor: portion}
	for _, a := range addresses {
		country, _ := r.Seal.Country(a.Ref.AddressID)
		t.Addresses = append(t.Addresses, api.OrderAddress{Role: a.Role, PartyID: a.Ref.PartyID, PartyRevision: a.Ref.PartyRevision,
			AddressID: a.Ref.AddressID, Address: a.Address, Country: country})
	}
	if seal := r.Seal; seal != nil {
		t.TradeTerms.Incoterm, t.TradeTerms.Edition = seal.Incoterm, seal.Edition
		if n := r.Terms.NotifyParty; n != nil && seal.Notify != nil {
			t.NotifyParty = &api.OrderAddress{Role: orderrev.RoleNotify, PartyID: n.PartyID, PartyRevision: n.PartyRevision, AddressID: n.AddressID,
				Address: seal.Notify.Address, Country: seal.Notify.Country}
		}
	}
	return t, nil
}

// stateSnapshotTrade states the Incoterm and edition of every intact fact; a fact whose trade_terms
// revision POL does not know (or that is not trade terms) keeps only its head, with Code
// REFERENCE_INVALID.
func (s Service) stateSnapshotTrade(ctx context.Context, facts map[string]api.OrderSnapshotFact) error {
	var refs []api.ConfigRef
	for _, f := range facts {
		if f.Code == "" && f.TradeTerms.Incoterm == "" {
			refs = append(refs, f.TradeTerms.Ref)
		}
	}
	known, err := s.tradeTermsOf(ctx, refs)
	if err != nil {
		return err
	}
	for id, f := range facts {
		if f.Code != "" || f.TradeTerms.Incoterm != "" {
			continue
		}
		if !stateTradeTerms(&f.TradeTerms, known) {
			f = api.OrderSnapshotFact{Binding: f.Binding, Status: f.Status, Ended: f.Ended, RowVersion: f.RowVersion, QuoteID: f.QuoteID,
				BusinessRevision: f.BusinessRevision, BusinessHash: f.BusinessHash, AcceptanceID: f.AcceptanceID, Code: kernel.CodeReferenceInvalid}
		}
		facts[id] = f
	}
	return nil
}

// stateActiveTrade states the Incoterm and edition of the Terms of every ActiveOrderRef v1 fact
// that has them; one whose trade_terms revision POL does not know becomes REFERENCE_INVALID.
func (s Service) stateActiveTrade(ctx context.Context, facts map[string]api.ActiveOrderFact) error {
	var refs []api.ConfigRef
	for _, f := range facts {
		if f.Terms.TradeTerms.Ref.Kind != "" && f.Terms.TradeTerms.Incoterm == "" {
			refs = append(refs, f.Terms.TradeTerms.Ref)
		}
	}
	known, err := s.tradeTermsOf(ctx, refs)
	if err != nil {
		return err
	}
	for id, f := range facts {
		if f.Terms.TradeTerms.Ref.Kind == "" || f.Terms.TradeTerms.Incoterm != "" {
			continue
		}
		if !stateTradeTerms(&f.Terms.TradeTerms, known) {
			f.Code = kernel.CodeReferenceInvalid
		}
		facts[id] = f
	}
	return nil
}

// tradeTermsOf resolves the POL trade_terms revisions refs name through POL's lookup (an active
// grant), whatever their state now: an order binds the revision, which never changes once
// published. Revisions POL does not know, or that are not trade terms, are absent.
func (s Service) tradeTermsOf(ctx context.Context, refs []api.ConfigRef) (map[api.ConfigRef]polapi.TradeTerms, error) {
	out := map[api.ConfigRef]polapi.TradeTerms{}
	var list []polapi.ConfigRef
	for _, r := range refs {
		p := polRef(ref(r))
		if !slices.Contains(list, p) {
			list = append(list, p)
		}
	}
	if len(list) == 0 {
		return out, nil
	}
	slices.SortFunc(list, func(a, b polapi.ConfigRef) int {
		return strings.Compare(a.Kind+"/"+a.ConfigID+"/"+itoa(a.Revision), b.Kind+"/"+b.ConfigID+"/"+itoa(b.Revision))
	})
	got, err := s.Upstream.Configs.ResolveConfigs(ctx, list, s.Clock.Now())
	if err != nil {
		return nil, err
	}
	for _, p := range list {
		if rc, ok := got[p]; ok && rc.Data.TradeTerms != nil {
			out[api.ConfigRef{Kind: p.Kind, ID: p.ConfigID, Revision: p.Revision}] = *rc.Data.TradeTerms
		}
	}
	return out, nil
}

// stateTradeTerms fills t's Incoterm and edition from known; false when its revision is not there.
func stateTradeTerms(t *api.TradeTerms, known map[api.ConfigRef]polapi.TradeTerms) bool {
	tt, ok := known[t.Ref]
	if !ok || t.Ref.Kind != quote.KindTradeTerms {
		return false
	}
	t.Incoterm, t.Edition = tt.Incoterm, tt.Edition
	return true
}
