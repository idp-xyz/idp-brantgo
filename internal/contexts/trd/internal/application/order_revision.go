package application

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"slices"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/order"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/orderrev"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// OrderRevisionRef v1, the seal of a new order revision and the costing cost of the order lines
// (S5R-D35, S5R-D40, S5R-D33; simulated sign-off E0; api/order_revision.go, orderrev/seal.go).
//
// A new revision seals the Incoterm and edition of its trade terms, its customer addresses'
// countries and the notify party's address, as POL's trade_terms revision and PTY's confirmed party
// revisions state them. They are resolved before the command's transaction (POL ConfigRef v1 R15,
// PTY AddressRoleRef v1 R13: published and confirmed revisions never change); a notify party bound
// anew is re-checked inside the transaction with AddressRoleGuard v1 (FOR SHARE, after the
// authority guard and POL, in context code order). The reads are after TRD's read transaction:
// TRD never calls another context from inside it.

// ResolveOrderRevisions answers OrderRevisionRef v1 lookups (api.OrderRevisionLookups): only an
// active grant is needed; orders of quotes outside the caller's data scope, unknown orders and
// revisions an order does not have are absent.
func (s Service) ResolveOrderRevisions(ctx context.Context, keys []api.OrderRevisionKey) (map[api.OrderRevisionKey]api.OrderRevisionFact,
	error) {
	if err := api.ValidateOrderRevisionKeys(keys); err != nil {
		return nil, err
	}
	out := map[api.OrderRevisionKey]api.OrderRevisionFact{}
	read := map[api.OrderRevisionKey]revisionRead{}
	err := s.lookupAcceptances(ctx, func(ctx context.Context, exec bentopg.Executor, sees func(quoteID string) (bool, error)) error {
		orders := map[string]*order.Order{}
		for _, k := range keys {
			o, loaded := orders[k.OrderID]
			if !loaded {
				var err error
				if o, err = postgres.LoadOrder(ctx, exec, scope(ctx), k.OrderID, postgres.NoLock); err != nil {
					return err
				}
				if o != nil {
					ok, err := sees(o.Source.QuoteID)
					if err != nil {
						return err
					}
					if !ok {
						o = nil
					}
				}
				orders[k.OrderID] = o
			}
			if o == nil || k.OrderRevision > o.OrderRevision {
				continue
			}
			f, x, err := s.readRevision(ctx, exec, o, k.OrderRevision)
			if err != nil {
				return err
			}
			out[k] = f
			if f.Code == "" {
				read[k] = x
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, s.stateRevisions(ctx, out, read)
}

// revisionRead is what TRD's read transaction found of one revision: its order's snapshot and the
// revision.
type revisionRead struct {
	snap order.Snapshot
	r    *orderrev.Revision
}

// readRevision reads revision n of order o: the snapshot (revision 1), the order's current revision
// or a stored later one. A revision that is not intact keeps only the head of its fact, with Code
// REFERENCE_INVALID.
func (s Service) readRevision(ctx context.Context, exec bentopg.Executor, o *order.Order, n int64) (api.OrderRevisionFact,
	revisionRead, error) {
	head := api.OrderRevisionFact{Binding: api.OrderBinding{OrderID: o.ID, OrderRevision: n}, Current: n == o.OrderRevision,
		Status: string(o.Status)}
	if n == 1 {
		head.Binding.OrderHash = o.SnapshotHash
	}
	for _, rec := range o.Revisions {
		if rec.Revision == n {
			head.Binding.OrderHash = rec.Hash
		}
	}
	invalid := head
	invalid.Code = kernel.CodeReferenceInvalid
	snap, verr := o.Verify(scope(ctx), "/keys")
	var r *orderrev.Revision
	var err error
	switch n {
	case o.OrderRevision:
		r, err = o.Revision()
	case 1:
		r, err = orderrev.FromSnapshot(o.Snapshot, o.SnapshotHash)
	default:
		r, err = postgres.LoadRevision(ctx, exec, scope(ctx), o.ID, n)
	}
	if _, refused := kernel.AsRejection(err); err != nil && !refused {
		return api.OrderRevisionFact{}, revisionRead{}, err
	}
	f, ferr := revisionFact(head, snap, r)
	if !intact(verr, err, ferr) || r.Number != n || r.OrderID != o.ID {
		// Not intact: the fact fails closed (Code), the lookup goes on.
		return invalid, revisionRead{}, nil
	}
	return f, revisionRead{snap: snap, r: r}, nil
}

// intact reports whether reading a revision found nothing wrong: each of errs is nil (each is a
// rejection of the revision, which then fails closed).
func intact(errs ...error) bool { return errors.Join(errs...) == nil }

// revisionFact is what OrderRevisionRef v1 says of revision r of the order whose snapshot is snap,
// from what TRD sealed; the Incoterm, edition and countries of a revision written before S5R are
// left to stateRevisions, the costing cost too.
func revisionFact(head api.OrderRevisionFact, snap order.Snapshot, r *orderrev.Revision) (api.OrderRevisionFact, error) {
	if r == nil {
		return head, kernel.Reject(kernel.CodeReferenceInvalid, "/keys", "The revision is not readable")
	}
	terms, err := orderTerms(snap, r)
	if err != nil {
		return api.OrderRevisionFact{}, err
	}
	f := head
	f.Binding.OrderHash = r.Hash
	if p := r.Previous; p != nil {
		f.Previous = &api.OrderBinding{OrderID: r.OrderID, OrderRevision: p.Revision, OrderHash: p.Hash}
	}
	if a := r.Amendment; a != nil {
		f.AmendmentID = a.ID
	}
	f.CustomerRef = api.PartyRef{PartyID: r.Customer.PartyID, Revision: r.Customer.Revision}
	f.CurrencyRef = apiRef(r.Currency)
	f.OrderTerms = terms
	f.Delivery = apiDelivery(r.Terms.Delivery)
	f.Lines = orderLines(r)
	f.Charges = orderCharges(r)
	f.NetMinor, f.TaxMinor, f.PayableMinor = r.Amounts.Net, r.Amounts.Tax, r.Amounts.Payable
	f.LegalTermsRef = optAPIRef(r.LegalTermsRef)
	f.Derived = r.Seal == nil
	return f, nil
}

// stateRevisions completes, after TRD's read transaction, the intact facts of revisions written
// before S5R (Derived): the Incoterm and edition from the POL trade_terms revision they bind — one
// POL does not know makes the fact REFERENCE_INVALID — and the addresses' countries from the PTY
// party revision they bind ("" for an address PTY does not show the caller); and every intact fact's
// costing cost (stateCosts).
func (s Service) stateRevisions(ctx context.Context, facts map[api.OrderRevisionKey]api.OrderRevisionFact,
	read map[api.OrderRevisionKey]revisionRead) error {
	var trade []api.ConfigRef
	var addresses []ptyapi.AddressRoleRef
	for _, f := range facts {
		if f.Code != "" || !f.Derived {
			continue
		}
		trade = append(trade, f.TradeTerms.Ref)
		for _, a := range f.Addresses {
			addresses = append(addresses, ptyAddress(a))
		}
	}
	known, err := s.tradeTermsOf(ctx, trade)
	if err != nil {
		return err
	}
	countries, err := s.countriesOf(ctx, addresses)
	if err != nil {
		return err
	}
	for k, f := range facts {
		if f.Code != "" || !f.Derived {
			continue
		}
		if !stateTradeTerms(&f.TradeTerms, known) {
			facts[k] = api.OrderRevisionFact{Binding: f.Binding, Current: f.Current, Status: f.Status, Code: kernel.CodeReferenceInvalid}
			delete(read, k)
			continue
		}
		for i, a := range f.Addresses {
			f.Addresses[i].Country = countries[ptyAddress(a)]
		}
		facts[k] = f
	}
	costs, err := lineCosts(ctx, s, read)
	if err != nil {
		return err
	}
	for k, c := range costs {
		f := facts[k]
		for i, l := range f.Lines {
			f.Lines[i].CostMinor = c[l.LineID]
		}
		facts[k] = f
	}
	return nil
}

func ptyAddress(a api.OrderAddress) ptyapi.AddressRoleRef {
	return ptyapi.AddressRoleRef{PartyID: a.PartyID, PartyRevision: a.PartyRevision, AddressID: a.AddressID}
}

// countriesOf asks PTY (AddressRoleRef v1, R13) the countries of addresses; one PTY does not show the
// caller is absent.
func (s Service) countriesOf(ctx context.Context, addresses []ptyapi.AddressRoleRef) (map[ptyapi.AddressRoleRef]string, error) {
	got, err := s.resolveAddresses(ctx, addresses)
	if err != nil {
		return nil, err
	}
	out := map[ptyapi.AddressRoleRef]string{}
	for r, a := range got {
		out[r] = a.Country
	}
	return out, nil
}

// lineCosts is the costing cost of the lines of every revision read (S5R-D33; order.LineCosts), by
// line ID: the cost snapshots their source revisions adopted are asked of CST once, after TRD's read
// transaction (CostSnapshotRef v1 at its result hash, R11; frozen, never changed). A snapshot CST
// does not show the caller gives no line a cost.
func lineCosts[K comparable](ctx context.Context, s Service, read map[K]revisionRead) (map[K]map[string]string, error) {
	out := map[K]map[string]string{}
	if len(read) == 0 || s.Upstream.Snapshots == nil {
		return out, nil
	}
	snapshotOf := func(x revisionRead) cstapi.CostSnapshotRef {
		d := x.snap.Commercial.Data.Snapshot
		return cstapi.CostSnapshotRef{SnapshotID: d.SnapshotID, ResultHash: d.ResultHash}
	}
	var refs []cstapi.CostSnapshotRef
	for _, x := range read {
		refs = append(refs, snapshotOf(x))
	}
	refs = slices.Compact(slices.SortedFunc(slices.Values(refs), func(a, b cstapi.CostSnapshotRef) int {
		return cmp.Or(cmp.Compare(a.SnapshotID, b.SnapshotID), cmp.Compare(a.ResultHash, b.ResultHash))
	}))
	got := map[cstapi.CostSnapshotRef]cstapi.ResolvedSnapshot{}
	for len(refs) > 0 {
		n := min(len(refs), cstapi.MaxSnapshotLookup)
		found, err := s.Upstream.Snapshots.ResolveSnapshots(ctx, refs[:n], s.Clock.Now())
		if err != nil {
			return nil, err
		}
		maps.Copy(got, found)
		refs = refs[n:]
	}
	for k, x := range read {
		rs, ok := got[snapshotOf(x)]
		if !ok {
			out[k] = map[string]string{}
			continue
		}
		costed := make([]order.CostedLine, 0, len(rs.Lines))
		for _, l := range rs.Lines {
			costed = append(costed, order.CostedLine{RfqLineID: l.LineID, Quantity: l.Quantity, Unit: l.Unit, AmountMinor: l.AmountMinor})
		}
		out[k] = x.snap.LineCosts(x.r, rs.Currency, costed)
	}
	return out, nil
}

// orderCharges are the charges of revision r with their amounts.
func orderCharges(r *orderrev.Revision) []api.OrderCharge {
	var out []api.OrderCharge
	for _, c := range r.Charges {
		a, _ := r.ChargeAmounts(c.ChargeID)
		out = append(out, api.OrderCharge{ChargeID: c.ChargeID, FeeCategoryRef: apiRef(c.FeeCategory), ObligationKey: c.ObligationKey,
			Amount: c.Amount, Inclusion: c.Inclusion, IncludedLineID: c.IncludedLineID, NetMinor: a.NetMinor, TaxMinor: a.TaxMinor,
			PayableMinor: a.PayableMinor})
	}
	return out
}

// prepareSeal resolves, before order.prepare's transaction, what revision 1 of an order converting
// formal revision revision of quote quoteID at hash seals (S5R-D35, D40; orderrev.SealFacts): the
// Incoterm and edition of the revision's trade terms (POL), the countries of its customer addresses
// and the address of the notify party notify names (PTY). A revision this read does not find gives
// empty facts: the transaction refuses the revision first.
func (s Service) prepareSeal(ctx context.Context, quoteID string, revision int64, hash string, notify *quote.AddressRef) (*orderrev.SealFacts,
	error) {
	f := &orderrev.SealFacts{Countries: map[string]string{}}
	var c *quote.Commercial
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		q, err := postgres.LoadQuote(ctx, exec, scope(ctx), quoteID, postgres.NoLock)
		if err != nil || q == nil {
			return err
		}
		rev := q.Revision(revision)
		if rev == nil || rev.Seal == nil || rev.Seal.BusinessHash != hash {
			return nil
		}
		if parsed, err := quote.ParseCommercial(rev.Seal.Commercial, rev.Seal.BusinessHash); err == nil {
			c = &parsed
		}
		return nil
	})
	if err != nil || c == nil {
		return f, err
	}
	f.TradeTerms = c.Data.TradeTerms
	known, err := s.tradeTermsOf(ctx, []api.ConfigRef{apiRef(c.Data.TradeTerms)})
	if err != nil {
		return nil, err
	}
	if tt, ok := known[apiRef(c.Data.TradeTerms)]; ok && c.Data.TradeTerms.Kind == quote.KindTradeTerms {
		f.Incoterm, f.Edition = tt.Incoterm, tt.Edition
	}
	var refs []ptyapi.AddressRoleRef
	for _, a := range c.Data.Addresses {
		refs = append(refs, ptyRef(a))
	}
	if notify != nil {
		refs = append(refs, ptyRef(*notify))
	}
	got, err := s.resolveAddresses(ctx, refs)
	if err != nil {
		return nil, err
	}
	for _, a := range c.Data.Addresses {
		if x, ok := got[ptyRef(a)]; ok {
			f.Countries[a.AddressID] = x.Country
		}
	}
	if notify != nil {
		if x, ok := got[ptyRef(*notify)]; ok {
			f.Notify = &orderrev.NotifyFact{Ref: *notify, Role: x.Role, Address: x.Address, Country: x.Country}
		}
	}
	return f, nil
}

// amendmentSeal resolves, before an amendment command's transaction, what the revision proposal p
// yields on the order's current revision r seals (S5R-D35, D40): the Incoterm and edition of p's
// trade terms — r's seal while they stay, POL's revision (got, as amendmentInputs read it)
// otherwise —, the customer addresses' countries — r's seal, or PTY's party revision when r has none
// —, and the notify party p names — r's sealed one while it stays, PTY's address otherwise. A
// notify party r does not bind is adopted anew: it must serve new business now (PTY's verdict at
// base/terms/notify_party_ref; REFERENCE_INVALID when PTY does not show it), and apply guards it
// (in.addresses).
func (s Service) amendmentSeal(ctx context.Context, r *orderrev.Revision, p orderrev.Proposal, got map[polapi.ConfigRef]polapi.ResolvedConfig,
	base string, in *amendmentInputs) (*orderrev.SealFacts, error) {
	f := &orderrev.SealFacts{TradeTerms: p.Terms.TradeTerms, Countries: map[string]string{}}
	if r.Seal != nil && p.Terms.TradeTerms == r.Terms.TradeTerms {
		f.Incoterm, f.Edition = r.Seal.Incoterm, r.Seal.Edition
	} else if tt := got[polRef(p.Terms.TradeTerms)].Data.TradeTerms; tt != nil && p.Terms.TradeTerms.Kind == quote.KindTradeTerms {
		f.Incoterm, f.Edition = tt.Incoterm, tt.Edition
	}
	cust := r.Parties.Customer
	var refs []ptyapi.AddressRoleRef
	if r.Seal != nil {
		for _, a := range r.Seal.Addresses {
			f.Countries[a.AddressID] = a.Country
		}
	} else {
		for _, a := range cust.Addresses {
			refs = append(refs, ptyapi.AddressRoleRef{PartyID: cust.PartyID, PartyRevision: cust.PartyRevision, AddressID: a.AddressID})
		}
	}
	n := p.Terms.NotifyParty
	kept := n != nil && r.Terms.NotifyParty != nil && *n == *r.Terms.NotifyParty && r.Seal != nil && r.Seal.Notify != nil
	if kept {
		f.Notify = &orderrev.NotifyFact{Ref: *n, Role: orderrev.RoleNotify, Address: r.Seal.Notify.Address, Country: r.Seal.Notify.Country}
	} else if n != nil {
		refs = append(refs, ptyRef(*n))
	}
	resolved, err := s.resolveAddresses(ctx, refs)
	if err != nil {
		return nil, err
	}
	if r.Seal == nil {
		for _, a := range cust.Addresses {
			if x, ok := resolved[ptyapi.AddressRoleRef{PartyID: cust.PartyID, PartyRevision: cust.PartyRevision, AddressID: a.AddressID}]; ok {
				f.Countries[a.AddressID] = x.Country
			}
		}
	}
	if n != nil && !kept {
		path := base + "/terms/notify_party_ref"
		x, ok := resolved[ptyRef(*n)]
		if !ok {
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, path, "")
		}
		if err := x.Check(path); err != nil {
			return nil, err
		}
		f.Notify = &orderrev.NotifyFact{Ref: *n, Role: x.Role, Address: x.Address, Country: x.Country}
		in.addresses = append(in.addresses, adoptedAddress{ref: ptyRef(*n), path: path})
	}
	return f, nil
}

// adoptedAddress is a PTY address a proposal adopts anew (its notify party) and where it names it.
type adoptedAddress struct {
	ref  ptyapi.AddressRoleRef
	path string
}

// resolveAddresses asks PTY (AddressRoleRef v1, R13) about refs, in lookups of at most
// ptyapi.MaxRefs; one PTY does not show the caller is absent.
func (s Service) resolveAddresses(ctx context.Context, refs []ptyapi.AddressRoleRef) (map[ptyapi.AddressRoleRef]ptyapi.ResolvedAddressRole, error) {
	out := map[ptyapi.AddressRoleRef]ptyapi.ResolvedAddressRole{}
	refs = slices.Compact(slices.SortedFunc(slices.Values(refs), compareAddress))
	for len(refs) > 0 {
		n := min(len(refs), ptyapi.MaxRefs)
		got, err := s.Upstream.Profiles.ResolveAddressRoles(ctx, refs[:n])
		if err != nil {
			return nil, err
		}
		maps.Copy(out, got)
		refs = refs[n:]
	}
	return out, nil
}

// guardAddress re-checks address ref inside TRD's transaction (PTY AddressRoleGuard v1, R13: the
// party row FOR SHARE until TRD commits): REFERENCE_INVALID or REFERENCE_STALE at path when it does
// not serve new business now.
func guardAddress(ctx context.Context, exec bentopg.Executor, ref ptyapi.AddressRoleRef, path string) error {
	v, err := ptyapi.ScanAddressRoleGuard(ref, exec.QueryRow(ctx, ptyapi.AddressRoleGuardSQL, ptyapi.AddressRoleGuardArgs(ref)...).Scan)
	if err != nil {
		return err
	}
	return v.Check(path)
}

func ptyRef(a quote.AddressRef) ptyapi.AddressRoleRef {
	return ptyapi.AddressRoleRef{PartyID: a.PartyID, PartyRevision: a.PartyRevision, AddressID: a.AddressID}
}
