package application

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// quote.freeze (human; WBS S3-01, S3-02, TRD-INV-002).
//
// Before TRD's transaction, after the command is authorized and found not to be a replay, Prepare
// reads the stored working revision and asks every upstream about its references at the freeze's
// instant, each in a transaction of its own: EVD the evidence sources, PTY the customer party and
// contact, the trading profile and the addresses, INQ the RFQ (with its customer, contact and line
// set), CAT the SKU specifications, CST the cost snapshot with its adoptability and the costing
// context it froze (its comparison basis), POL every configuration the draft names and the
// costing's comparison currency and cost units (and, in a second round, the revisions those name,
// and the issuer's default legal terms, which a draft without legal terms adopts).
//
// Inside the transaction the lock and guard order is (ADR-BR-011, tactical lockOrder):
//
//  1. the quote root FOR UPDATE (the handler's Lock), which also holds its object control;
//  2. the company's authority guard row FOR SHARE (the ledger's Authorize);
//  3. after the domain judged the draft and the server calculation computed it (quote.SealDraft,
//     Calculator), the upstream guard rows FOR SHARE through their guard functions, in context
//     code order (guardUpstream): CAT (SkuGuard v1 per specification revision), CST
//     (AdoptabilityGuard v1, which in turn locks the snapshot, its sources and inputs), INQ
//     (RfqGuard v1), POL (ConfigGuard v1 per adopted revision in kind, ID and revision order), PTY
//     (TradingProfileGuard v1, which also locks the party and the pinned contacts, then
//     AddressRoleGuard v1 per address);
//  4. the revision, the action result, the audit entry and QuoteFrozen in the Outbox.
//
// An upstream change that ends what the freeze binds (a source denial, an RFQ cancellation, a
// configuration retirement, a party or specification revision) either committed before its guard
// read the row, and the freeze is refused with the owner's verdict, or waits until the freeze
// commits. The costing context's comparison basis needs no guard of its own: a sealed revision
// never changes, and CST's AdoptabilityGuard decides whether the snapshot built on it is adoptable.
//
// STL's CreditGuard v1 (Context Map R57) and CNX's TradeComplianceGuard v1 (R58) are not freeze
// guards: the design checks credit and trade compliance at order.activate (TRD-INV-022, 023), which
// comes with the order wave; by the owner's decision of 2026-11-10 both are S4–S5 integration and
// stay recorded gaps there. In context code order CNX falls between CAT and CST, STL after PTY.

// facts are what the upstreams said before the transaction: the domain's view and the raw answers
// the calculation needs.
type facts struct {
	quote.Facts
	configs  map[polapi.ConfigRef]polapi.ResolvedConfig
	rfq      *inqapi.RfqRef
	snapshot *cstapi.ResolvedSnapshot
	context  *cstapi.VerifiedContext
}

// Freeze seals the working revision (Python quote.freeze).
func (s Service) Freeze(ctx context.Context, cmd api.Freeze) (api.FreezeReceipt, error) {
	h := &quoteCommand{s: s, id: cmd.QuoteID}
	var f facts
	var seen int64 // the root version the upstream facts were read for; 0: nothing was read
	h.prepare = func(ctx context.Context) error {
		var stored *quote.Quote
		err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
			var err error
			stored, err = postgres.LoadQuote(ctx, exec, scope(ctx), cmd.QuoteID, postgres.NoLock)
			if stored != nil && !d.Sees(stored.OwnerActor, stored.OwnerTeam) {
				stored = nil
			}
			return err
		})
		if err != nil || stored == nil || stored.Working() == nil {
			return err // Apply refuses what is missing, in the contract's order
		}
		seen = stored.RootVersion
		f, err = s.facts(ctx, stored.Working().Data, s.Clock.Now())
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := h.admit(ctx, exec, d, false); err != nil {
			return action.Outcome{}, err
		}
		if err := quote.Check(h.q, true, cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		if err := h.q.CheckFreeze(cmd.WorkingRevision); err != nil {
			return action.Outcome{}, err
		}
		if h.q.RootVersion != seen {
			// The draft changed after its references were read; freezing would judge stale facts.
			return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
		}
		w, now := h.q.Working(), s.Clock.Now()
		seal, err := quote.SealDraft(h.q.ID, w.Number, w.Data, f.Facts, s.calculation(w.Data, f, now), now)
		if err != nil {
			return action.Outcome{}, err
		}
		if _, err = s.guardUpstream(ctx, exec, freezeGuards(w.Data, seal, f.Profile.ContentHash), now); err != nil {
			return action.Outcome{}, err
		}
		if _, err = h.q.Freeze(cmd.WorkingRevision, seal, d.Actor, d.Epoch, now); err != nil {
			return action.Outcome{}, err
		}
		c := seal.Calculation
		event := action.Event{Type: api.EventQuoteFrozen, AggregateType: "QuoteRoot", AggregateID: h.q.ID, AggregateVersion: h.q.RootVersion,
			Fields: map[string]any{"quote_id": h.q.ID, "business_revision": w.Number, "business_hash": seal.BusinessHash,
				"issued_at": kernel.FormatTimestamp(seal.IssuedAt), "expires_at": kernel.FormatTimestamp(seal.ExpiresAt),
				"rfq_id": seal.Rfq.ID, "rfq_row_version": seal.Rfq.Version, "cost_snapshot_id": seal.Snapshot.SnapshotID,
				"customer_party_id": w.Data.Customer.PartyID, "calculation_server_owned": true, "margin_rule": c.MarginRule,
				"calculation_version": c.CalculationVersion}}
		return h.save(ctx, exec, false, api.StatusFrozen, event)
	}
	r, err := s.run(ctx, action.Command{Name: api.CommandFreeze, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.QuoteID},
		cmd, cmd.Validate(), &preparedQuote{h})
	if err != nil {
		return api.FreezeReceipt{}, err
	}
	return s.freezeReceipt(ctx, r)
}

// calculation is the server calculation of draft d on facts f at instant at, which SealDraft runs
// once it found every reference known and in effect. Without a Calculator TRD is misconfigured
// and SealDraft freezes nothing (quote.ErrNoCalculation).
func (s Service) calculation(d quote.Data, f facts, at time.Time) quote.Calculate {
	if s.Calculator == nil {
		return nil
	}
	return func() (*quote.Calculation, error) {
		if f.rfq == nil || f.snapshot == nil || f.Profile == nil {
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/args/data", "")
		}
		return s.Calculator.Calculate(CalculationInput{Data: d, Configs: maps.Clone(f.configs), Rfq: *f.rfq, Profile: *f.Profile,
			Snapshot: *f.snapshot, Context: f.context, At: at})
	}
}

// guarded is what a transaction re-checks upstream for a sealed revision (ADR-BR-004 option B): the
// draft's SKU specifications, cost snapshot, trading profile and addresses, the RFQ binding and the
// POL revisions adopted with their content hashes (as the freeze sealed them in the dependency
// manifest), and the trading profile's content hash. prefix is where refusals point: the freeze's
// request (/args/data) or the stored commercial document a formalization re-checks (/data).
type guarded struct {
	data        quote.Data
	configs     []quote.Dependency
	rfq         quote.Dependency
	profileHash string
	prefix      string
	// hooks run at their place in context code order (order activation: the extension points of CNX
	// after CAT and of STL after PTY, and PTY's screening after its address guards); nil for none.
	afterCAT, afterPTY func() error
	// skipSpecs leaves CAT's SkuGuard to the caller (GoalReadiness judges the SKU specifications as
	// Q01.industry, apart from the other upstream facts, goal.go).
	skipSpecs bool
	// legal is where refusals of the legal terms the freeze adopted from the issuer's default point
	// (quote/legal.go): the draft names none, so they are refused at its issuer_ref; "" otherwise.
	legal string
	// notify is the notify party an order's revision binds (S5R-D40), guarded with the addresses at
	// notifyPath; nil for none (a quote binds none).
	notify     *ptyapi.AddressRoleRef
	notifyPath string
}

// freezeGuards is what the freeze guards for draft d with seal s, the profile read as profileHash.
func freezeGuards(d quote.Data, s *quote.Seal, profileHash string) guarded {
	g := guarded{data: d, configs: s.Configs, rfq: s.Rfq, profileHash: profileHash, prefix: "/args/data"}
	if d.LegalTerms == nil && s.LegalTerms != nil {
		g.data.LegalTerms, g.legal = s.LegalTerms, "/args/data/issuer_ref"
	}
	return g
}

// sealedGuards is what a formalization guards for revision r: what its freeze guarded, read back from
// its commercial document c (the manifest's POL revisions, RFQ and trading profile). The legal terms
// the revision sealed, its own or its issuer's default, are bound at data.legal_terms_ref of c, where
// a lapse of them points.
func sealedGuards(r *quote.Revision, c quote.Commercial) guarded {
	g := guarded{data: r.Data, prefix: "/data"}
	g.data.LegalTerms = c.Data.LegalTerms
	for _, d := range c.DependencyManifest {
		switch d.Kind {
		case quote.DependencyRfq:
			g.rfq = d
		case quote.DependencyTradingProfile:
			g.profileHash = d.ContentHash
		case quote.DependencyCostSnapshot, quote.DependencySource:
		default:
			g.configs = append(g.configs, d)
		}
	}
	return g
}

// at is path, a request path under /args/data, under the guard set's prefix.
func (g guarded) at(path string) string {
	const data = "/args/data"
	if rest, ok := strings.CutPrefix(path, data); ok {
		return g.prefix + rest
	}
	return path
}

// guardUpstream re-checks, inside TRD's transaction after the root is locked FOR UPDATE, what a
// sealed revision binds upstream, in context code order; each guard locks its upstream rows FOR
// SHARE until TRD's transaction ends, so a change that would end a bound fact either committed first
// and the command is refused with the owner's verdict, or waits until the command commits. It
// returns CST's adoptability verdict, which a formalization records in its commit evidence.
func (s Service) guardUpstream(ctx context.Context, exec bentopg.Executor, g guarded, at time.Time) (cstapi.AdoptabilityVerdict, error) {
	return s.checkUpstream(ctx, exec, g, at, refuseLapse)
}

// lapse decides what a re-check does with an upstream verdict that a bound fact no longer serves new
// business (refusal, nil when it does), detail being the owner's own pointer: the freeze and the
// formalization refuse (refuseLapse); an order's review and activation record the lapse as a risk
// and go on (ADR-BR-004; order_upstream.go).
type lapse func(refusal error, detail string) error

func refuseLapse(refusal error, _ string) error { return refusal }

// checkUpstream is guardUpstream with onLapse deciding each lapse; every guard is called, in the
// same order, whatever it answers.
func (s Service) checkUpstream(ctx context.Context, exec bentopg.Executor, g guarded, at time.Time, onLapse lapse) (cstapi.AdoptabilityVerdict, error) {
	d, p := g.data, g.prefix
	var none cstapi.AdoptabilityVerdict
	lapsed := func(refusal error, detail string) error {
		if refusal == nil {
			return nil
		}
		return onLapse(refusal, detail)
	}
	// CAT (R14): SkuGuard v1 on every SKU specification revision, in (SKU, revision) order.
	if !g.skipSpecs {
		skus := make([]quote.SkuRef, len(d.Lines))
		for i, l := range d.Lines {
			skus[i] = l.Sku
		}
		if err := guardSpecs(ctx, exec, skus, p, lapsed); err != nil {
			return none, err
		}
	}
	if g.afterCAT != nil {
		if err := g.afterCAT(); err != nil {
			return none, err
		}
	}
	// CST (R11): AdoptabilityGuard v1 on the cost snapshot and its result hash.
	if d.Snapshot == nil {
		return none, kernel.Reject(kernel.CodeReferenceInvalid, p+"/cost_snapshot_ref", "")
	}
	ref := cstapi.CostSnapshotRef{SnapshotID: d.Snapshot.SnapshotID, ResultHash: d.Snapshot.ResultHash}
	adoptable, err := cstapi.ScanAdoptabilityGuard(ref, exec.QueryRow(ctx, cstapi.AdoptabilityGuardSQL, cstapi.AdoptabilityGuardArgs(ref, at)...).Scan)
	if err != nil {
		return none, err
	}
	if err = lapsed(adoptable.Check(p+"/cost_snapshot_ref"), adoptable.Input); err != nil {
		return none, err
	}
	// INQ (R12): RfqGuard v1 on the RFQ at its row version with its line set.
	rfq := inqapi.RfqBinding{RfqID: g.rfq.ID, RowVersion: g.rfq.Version, LinesHash: g.rfq.ContentHash}
	ready, err := inqapi.ScanRfqGuard(rfq, exec.QueryRow(ctx, inqapi.RfqGuardSQL, inqapi.RfqGuardArgs(rfq)...).Scan)
	if err != nil {
		return none, err
	}
	if err = lapsed(ready.Check(p+"/rfq_ref"), ready.Status); err != nil {
		return none, err
	}
	// POL (R15): ConfigGuard v1 on every revision adopted, directly or through another; a revision's
	// content never changes, so the hash it answers is the one the manifest binds.
	paths := map[quote.Ref]string{}
	for _, field := range d.Fields() {
		if _, ok := paths[field.Ref]; !ok {
			paths[field.Ref] = g.at(field.Path)
		}
	}
	if d.LegalTerms != nil && g.legal != "" {
		paths[*d.LegalTerms] = g.legal
	}
	for _, c := range g.configs {
		r := polapi.ConfigRef{Kind: c.Kind, ConfigID: c.ID, Revision: c.Version}
		path, ok := paths[quote.Ref{Kind: c.Kind, ID: c.ID, Revision: c.Version}]
		if !ok {
			path = p
		}
		v, err := polapi.ScanConfigGuard(r, exec.QueryRow(ctx, polapi.ConfigGuardSQL, polapi.ConfigGuardArgs(r, at)...).Scan)
		if err != nil {
			return none, err
		}
		if err = lapsed(v.Check(path), ""); err != nil {
			return none, err
		}
		if v.InForce() && v.ContentHash != c.ContentHash {
			if err = lapsed(api.Reject(api.CodeReferenceStale, path), "content_hash"); err != nil {
				return none, err
			}
		}
	}
	// PTY (R13): TradingProfileGuard v1 on the exact profile revision (it also locks the party row
	// and the pinned contacts' rows, so the customer revision and the contact the domain matched
	// against the profile stay as read), then AddressRoleGuard v1 on every address in (party,
	// revision, address) order.
	if d.Profile == nil || d.Customer == nil {
		return none, kernel.Reject(kernel.CodeReferenceInvalid, p+"/trading_profile_ref", "")
	}
	profile := ptyapi.TradingProfileRef{PartyID: d.Profile.PartyID, Revision: d.Profile.Revision}
	pv, err := ptyapi.ScanTradingProfileGuard(profile, exec.QueryRow(ctx, ptyapi.TradingProfileGuardSQL, ptyapi.TradingProfileGuardArgs(profile)...).Scan)
	if err != nil {
		return none, err
	}
	if err = lapsed(pv.Check(p+"/trading_profile_ref"), pv.Stale); err != nil {
		return none, err
	}
	if pv.Current() && (pv.ContentHash != g.profileHash || pv.PartyRevision != d.Customer.Revision) {
		if err = lapsed(api.Reject(api.CodeReferenceStale, p+"/trading_profile_ref"), "content_hash"); err != nil {
			return none, err
		}
	}
	addresses := map[ptyapi.AddressRoleRef]string{}
	for i, a := range d.Addresses {
		addresses[ptyapi.AddressRoleRef{PartyID: a.PartyID, PartyRevision: a.PartyRevision, AddressID: a.AddressID}] =
			fmt.Sprintf("%s/customer_address_refs/%d", p, i)
	}
	if n := g.notify; n != nil {
		if _, ok := addresses[*n]; !ok {
			addresses[*n] = g.notifyPath
		}
	}
	for _, a := range slices.SortedFunc(maps.Keys(addresses), compareAddress) {
		v, err := ptyapi.ScanAddressRoleGuard(a, exec.QueryRow(ctx, ptyapi.AddressRoleGuardSQL, ptyapi.AddressRoleGuardArgs(a)...).Scan)
		if err != nil {
			return none, err
		}
		if err = lapsed(v.Check(addresses[a]), v.Stale); err != nil {
			return none, err
		}
	}
	if g.afterPTY != nil {
		if err := g.afterPTY(); err != nil {
			return none, err
		}
	}
	return adoptable, nil
}

// guardSpecs calls CAT's SkuGuard v1 (R14) on every SKU specification revision of skus, the SKUs of
// lines prefix/lines/<i>, in (SKU, revision) order; lapsed decides each refusal, whose path is the
// first line binding the revision.
func guardSpecs(ctx context.Context, exec bentopg.Executor, skus []quote.SkuRef, prefix string, lapsed lapse) error {
	specs := map[catapi.SpecRef]string{}
	for i, sku := range skus {
		ref := catapi.SpecRef{SkuID: sku.SkuID, SpecRevision: sku.SpecRevision}
		if _, ok := specs[ref]; !ok {
			specs[ref] = fmt.Sprintf("%s/lines/%d/sku_ref", prefix, i)
		}
	}
	for _, ref := range slices.SortedFunc(maps.Keys(specs), compareSpec) {
		v, err := catapi.ScanSkuGuard(ref, exec.QueryRow(ctx, catapi.SkuGuardSQL, catapi.SkuGuardArgs(ref)...).Scan)
		if err != nil {
			return err
		}
		if err = lapsed(v.Check(specs[ref]), v.Status); err != nil {
			return err
		}
	}
	return nil
}

func compareAddress(a, b ptyapi.AddressRoleRef) int {
	return cmp.Or(cmp.Compare(a.PartyID, b.PartyID), cmp.Compare(a.PartyRevision, b.PartyRevision), cmp.Compare(a.AddressID, b.AddressID))
}

// freezeReceipt completes the receipt of a freeze, executed or replayed, from the stored revision.
func (s Service) freezeReceipt(ctx context.Context, r api.Receipt) (api.FreezeReceipt, error) {
	out := api.FreezeReceipt{Receipt: r}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		q, err := postgres.LoadQuote(ctx, exec, scope(ctx), r.ObjectID, postgres.NoLock)
		if err != nil || q == nil {
			return err
		}
		if w := q.FrozenAt(r.RowVersion); w != nil {
			out.BusinessRevision, out.BusinessHash = w.Number, w.Seal.BusinessHash
			out.IssuedAt, out.ExpiresAt = kernel.FormatTimestamp(w.Seal.IssuedAt), kernel.FormatTimestamp(w.Seal.ExpiresAt)
		}
		return nil
	})
	return out, err
}

// facts asks EVD, PTY, INQ, CAT, POL and CST about the references of d at instant at, each in a
// transaction of its own, before TRD's. The company is the verified principal's (no transaction
// scope exists yet).
func (s Service) facts(ctx context.Context, d quote.Data, at time.Time) (facts, error) {
	p, err := identity.FromContext(ctx)
	if err != nil {
		return facts{}, err
	}
	f := facts{Facts: quote.Facts{Company: string(p.Scope.Company), Addresses: map[quote.AddressRef]quote.Address{},
		Specs: map[quote.SkuRef]quote.Verdict{}, Configs: map[quote.Ref]quote.Config{}}, configs: map[polapi.ConfigRef]polapi.ResolvedConfig{}}
	if f.Sources, err = s.sources(ctx, d.Sources()); err != nil {
		return f, err
	}
	if err = s.parties(ctx, d, &f.Facts); err != nil {
		return f, err
	}
	if d.Rfq != nil {
		rfqs, err := s.Upstream.Rfqs.ResolveRfqs(ctx, []string{d.Rfq.RfqID})
		if err != nil {
			return f, err
		}
		if r, ok := rfqs[d.Rfq.RfqID]; ok {
			f.rfq = &r
			f.Rfq = &quote.Rfq{RowVersion: r.RowVersion, Ready: r.Ready(), LinesHash: r.LinesHash}
		}
	}
	if len(d.Lines) > 0 {
		var specs []catapi.SpecRef
		for _, l := range d.Lines {
			specs = append(specs, catapi.SpecRef{SkuID: l.Sku.SkuID, SpecRevision: l.Sku.SpecRevision})
		}
		standings, err := s.Upstream.Skus.ResolveSpecs(ctx, slices.Compact(slices.SortedFunc(slices.Values(specs), compareSpec)))
		if err != nil {
			return f, err
		}
		for ref, standing := range standings {
			f.Specs[quote.SkuRef{SkuID: ref.SkuID, SpecRevision: ref.SpecRevision}] = standing.Check
		}
	}
	if d.Snapshot != nil {
		ref := cstapi.CostSnapshotRef{SnapshotID: d.Snapshot.SnapshotID, ResultHash: d.Snapshot.ResultHash}
		found, err := s.Upstream.Snapshots.ResolveSnapshots(ctx, []cstapi.CostSnapshotRef{ref}, at)
		if err != nil {
			return f, err
		}
		if rs, ok := found[ref]; ok {
			f.snapshot = &rs
			a := rs.Adoptability
			f.Snapshot = &quote.Snapshot{RfqLinesHash: rs.Rfq.LinesHash, EvidenceHash: rs.EvidenceHash, Check: func(path string) error {
				if a.Adoptable() {
					return nil
				}
				return kernel.RejectAs(cstapi.CodeCostSnapshotIneligible, kernel.CategoryInvalid, path, "The cost snapshot is "+a.Status)
			}}
			contexts, err := s.Upstream.Contexts.ResolveContexts(ctx, []cstapi.ContextRef{rs.Context})
			if err != nil {
				return f, err
			}
			if vc, ok := contexts[rs.Context]; ok {
				f.context = &vc
				f.Adopted = adoptedRefs(&vc)
			}
		}
	}
	if err = s.configs(ctx, append(d.ConfigRefs(), f.Adopted...), at, &f); err != nil {
		return f, err
	}
	return f, nil
}

func compareSpec(a, b catapi.SpecRef) int {
	return cmp.Or(cmp.Compare(a.SkuID, b.SkuID), cmp.Compare(a.SpecRevision, b.SpecRevision))
}

// parties asks PTY about the customer, the contact, the trading profile and the addresses.
func (s Service) parties(ctx context.Context, d quote.Data, f *quote.Facts) error {
	var q ptyapi.Resolve
	if d.Customer != nil {
		q.PartyIDs = []string{d.Customer.PartyID}
	}
	if d.Contact != nil {
		q.ContactIDs = []string{d.Contact.ContactID}
	}
	if len(q.PartyIDs)+len(q.ContactIDs) > 0 {
		got, err := s.Upstream.Parties.ResolveParties(ctx, q)
		if err != nil {
			return err
		}
		if d.Customer != nil {
			if p, ok := got.Parties[d.Customer.PartyID]; ok {
				f.Party = &quote.Party{Confirmed: p.Status == ptyapi.StatusConfirmed, Revision: p.Revision,
					Customer: slices.Contains(p.Roles, ptyapi.RoleCustomer)}
			}
		}
		if d.Contact != nil {
			if c, ok := got.Contacts[d.Contact.ContactID]; ok {
				f.Contact = &quote.Contact{PartyID: c.PartyID, RowVersion: c.RowVersion, Confirmed: c.Status == ptyapi.StatusConfirmed}
			}
		}
	}
	if d.Profile != nil {
		ref := ptyapi.TradingProfileRef{PartyID: d.Profile.PartyID, Revision: d.Profile.Revision}
		got, err := s.Upstream.Profiles.ResolveTradingProfiles(ctx, []ptyapi.TradingProfileRef{ref})
		if err != nil {
			return err
		}
		if r, ok := got[ref]; ok {
			f.Profile = &quote.Profile{Check: r.Check, ContentHash: r.ContentHash, PartyRevision: r.PartyRevision,
				PaymentTerms: termsRef(r.PaymentTerms), TradeTerms: termsRef(r.TradeTerms)}
			for _, c := range r.ContactRefs {
				f.Profile.Contacts = append(f.Profile.Contacts, quote.ContactRef{ContactID: c.ContactID, RowVersion: c.RowVersion})
			}
		}
	}
	if len(d.Addresses) > 0 {
		refs := make([]ptyapi.AddressRoleRef, len(d.Addresses))
		for i, a := range d.Addresses {
			refs[i] = ptyapi.AddressRoleRef{PartyID: a.PartyID, PartyRevision: a.PartyRevision, AddressID: a.AddressID}
		}
		got, err := s.Upstream.Profiles.ResolveAddressRoles(ctx, refs)
		if err != nil {
			return err
		}
		for ref, r := range got {
			f.Addresses[quote.AddressRef{PartyID: ref.PartyID, PartyRevision: ref.PartyRevision, AddressID: ref.AddressID}] =
				quote.Address{Check: r.Check, Role: r.Role, Address: r.Address}
		}
	}
	return nil
}

func termsRef(t ptyapi.TermsRef) quote.Ref {
	return quote.Ref{Kind: t.Kind, ID: t.ConfigID, Revision: t.Revision}
}

// configs asks POL about refs at instant at, then about the revisions those name, which the freeze
// adopts too. A reference POL cannot hold (an unknown kind or an over-long ID) is simply unknown.
func (s Service) configs(ctx context.Context, refs []quote.Ref, at time.Time, f *facts) error {
	for round := 0; round < 2 && len(refs) > 0; round++ {
		var asked []polapi.ConfigRef
		seen := map[polapi.ConfigRef]bool{}
		for _, r := range refs {
			pr := polapi.ConfigRef{Kind: r.Kind, ConfigID: r.ID, Revision: r.Revision}
			if _, done := f.configs[pr]; !done && !seen[pr] && pr.Validate("/") == nil {
				seen[pr] = true
				asked = append(asked, pr)
			}
		}
		var next []quote.Ref
		for chunk := range slices.Chunk(asked, polapi.MaxRefs) {
			answered, err := s.Upstream.Configs.ResolveConfigs(ctx, chunk, at)
			if err != nil {
				return err
			}
			for ref, rc := range answered {
				f.configs[ref] = rc
				c := config(rc)
				f.Configs[quote.Ref{Kind: ref.Kind, ID: ref.ConfigID, Revision: ref.Revision}] = c
				next = append(next, c.Nested...)
				if c.DefaultLegalTerms != nil {
					next = append(next, *c.DefaultLegalTerms) // adopted only by a draft without legal terms (quote/legal.go)
				}
			}
		}
		refs = next
	}
	return nil
}

// config is what the freeze judges itself of a POL revision; the calculation reads the raw
// revision (calculate.go).
func config(rc polapi.ResolvedConfig) quote.Config {
	c := quote.Config{Check: rc.Check, ContentHash: rc.ContentHash}
	nested := func(r *polapi.ConfigRef) {
		if r != nil {
			c.Nested = append(c.Nested, quote.Ref{Kind: r.Kind, ID: r.ConfigID, Revision: r.Revision})
		}
	}
	switch d := rc.Data; {
	case d.CompanyProfile != nil:
		p := d.CompanyProfile
		c.Company = &quote.IssuerSnapshot{LegalName: p.LegalName, RegistrationNo: p.RegistrationNo, Country: p.Country, Address: p.Address}
		if r := p.DefaultLegalTerms; r != nil {
			c.DefaultLegalTerms = &quote.Ref{Kind: r.Kind, ID: r.ConfigID, Revision: r.Revision}
		}
	case d.LegalTerms != nil:
		l := d.LegalTerms
		c.Legal = &quote.Legal{GoverningLaw: l.GoverningLaw, CISG: l.CISG, FormationRule: l.FormationRule,
			DisputeResolution: quote.DisputeResolution(l.DisputeResolution)}
	case d.TradeTerms != nil:
		nested(d.TradeTerms.Delivery.Calendar)
	case d.PaymentTerms != nil:
		nested(d.PaymentTerms.Calendar)
	case d.TaxApplication != nil:
		nested(&d.TaxApplication.TaxProfile)
	case d.PricingPolicy != nil:
		c.Policy = &quote.Policy{QuoteValidDays: d.PricingPolicy.QuoteValidDays}
		nested(&d.PricingPolicy.TaxProfile)
	case d.NumberSequence != nil:
		c.SequenceType = d.NumberSequence.DocumentType
	case d.QuoteDocumentTemplate != nil:
		c.Renderer = d.QuoteDocumentTemplate.RendererVersion
	}
	return c
}
