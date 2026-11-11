package application

import (
	"context"
	"maps"
	"slices"
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
	"go.idp.xyz/idp-brant/internal/kernel"
)

// quote.freeze (human; WBS S3-01, TRD-INV-002).
//
// Before TRD's transaction, after the command is authorized and found not to be a replay, Prepare
// reads the stored working revision and asks every upstream about its references at the freeze's
// instant, each in a transaction of its own: EVD the evidence sources, PTY the customer party and
// contact, the trading profile and the addresses, INQ the RFQ, CAT the SKU specifications, POL every
// configuration (and, in a second round, the revisions those name), CST the cost snapshot with its
// adoptability.
//
// Inside the transaction the lock and guard order is (ADR-BR-011, tactical lockOrder):
//
//  1. the quote root FOR UPDATE (the handler's Lock), which also holds its object control;
//  2. the company's authority guard row FOR SHARE (the ledger's Authorize);
//  3. after the domain sealed the draft, the upstream guard rows FOR SHARE through their guard
//     functions, in context code order (guardUpstream): CAT, CST (AdoptabilityGuard v1, which in
//     turn locks the snapshot, its sources and inputs), INQ (RfqGuard v1), POL (ConfigGuard v1 per
//     adopted revision in kind, ID and revision order), PTY;
//  4. the revision, the action result, the audit entry and QuoteFrozen in the Outbox.
//
// An upstream change that ends what the freeze binds (a source denial, an RFQ cancellation, a
// configuration retirement) either committed before its guard read the row, and the freeze is
// refused with the owner's verdict, or waits until the freeze commits.

// Calculator is the server calculation of WBS S3-02 (sales charges, tax, sales FX, margin, payment
// allocation): the one place a freeze takes amounts from, never from the client. It computes on
// the draft and what POL and CST said before the transaction; the domain checks that its result
// fits the revision before the freeze seals it.
type Calculator interface {
	Calculate(in CalculationInput) (*quote.Calculation, error)
}

// CalculationInput is what the calculation of one freeze rests on.
type CalculationInput struct {
	Data     quote.Data
	Configs  map[polapi.ConfigRef]polapi.ResolvedConfig // every POL revision the draft adopts, direct and nested
	Snapshot cstapi.ResolvedSnapshot
	At       time.Time
}

// Guards are the guard functions of upstream contexts whose guards are not published yet. The
// freeze calls each, when set, at its place in context code order inside its transaction, with
// TRD's own executor (ADR-BR-004 option B).
type Guards struct {
	// Catalog re-checks and locks FOR SHARE the SKU specification revisions the quote binds
	// (Context Map R14, CAT SkuRef v1 guard). Until it is set the freeze relies on CAT's
	// ResolveSpecs before the transaction: a specification revised between that read and the
	// commit is not caught (recorded gap).
	Catalog func(ctx context.Context, exec bentopg.Executor, specs []catapi.SpecRef, path string) error
	// Parties re-checks and locks FOR SHARE the customer party revision, the contact, the trading
	// profile revision and the addresses the quote binds (Context Map R13, PTY guards). Until it is
	// set the freeze relies on PTY's lookups before the transaction (recorded gap).
	Parties func(ctx context.Context, exec bentopg.Executor, b PartyBinding) error
}

// PartyBinding is what a frozen quote binds of PTY.
type PartyBinding struct {
	Party     ptyapi.PartyRef // PartyID and Revision
	Contact   ptyapi.ContactRef
	Profile   ptyapi.TradingProfileRef
	Addresses []ptyapi.AddressRoleRef
}

// facts are what the upstreams said before the transaction: the domain's view and the raw answers
// the calculation needs.
type facts struct {
	quote.Facts
	configs  map[polapi.ConfigRef]polapi.ResolvedConfig
	snapshot *cstapi.ResolvedSnapshot
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
		if err := h.admit(ctx, d, false); err != nil {
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
		calc, err := s.calculate(w.Data, f, now)
		if err != nil {
			return action.Outcome{}, err
		}
		seal, err := quote.SealDraft(h.q.ID, w.Number, w.Data, f.Facts, calc, now)
		if err != nil {
			return action.Outcome{}, err
		}
		if err = s.guardUpstream(ctx, exec, w.Data, seal, now); err != nil {
			return action.Outcome{}, err
		}
		if _, err = h.q.Freeze(cmd.WorkingRevision, seal, d.Actor, d.Epoch, now); err != nil {
			return action.Outcome{}, err
		}
		event := action.Event{Type: api.EventQuoteFrozen, AggregateType: "QuoteRoot", AggregateID: h.q.ID, AggregateVersion: h.q.RootVersion,
			Fields: map[string]any{"quote_id": h.q.ID, "business_revision": w.Number, "business_hash": seal.BusinessHash,
				"issued_at": kernel.FormatTimestamp(seal.IssuedAt), "expires_at": kernel.FormatTimestamp(seal.ExpiresAt),
				"rfq_id": seal.Rfq.ID, "rfq_row_version": seal.Rfq.Version, "cost_snapshot_id": seal.Snapshot.SnapshotID,
				"customer_party_id": w.Data.Customer.PartyID, "calculation_server_owned": calc != nil}}
		return h.save(ctx, exec, false, api.StatusFrozen, event)
	}
	r, err := s.run(ctx, action.Command{Name: api.CommandFreeze, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.QuoteID},
		cmd, cmd.Validate(), &preparedQuote{h})
	if err != nil {
		return api.FreezeReceipt{}, err
	}
	return s.freezeReceipt(ctx, r)
}

// calculate runs the S3-02 server calculation when it is wired. Until then the commercial document
// carries calculation null: nothing is computed, and nothing claims it was (the review and
// activation waves must refuse a revision without a calculation).
func (s Service) calculate(d quote.Data, f facts, at time.Time) (*quote.Calculation, error) {
	if s.Calculator == nil || f.snapshot == nil {
		return nil, nil
	}
	return s.Calculator.Calculate(CalculationInput{Data: d, Configs: maps.Clone(f.configs), Snapshot: *f.snapshot, At: at})
}

// guardUpstream re-checks, inside TRD's transaction after the root is locked FOR UPDATE, what the
// freeze binds upstream, in context code order; each guard locks its upstream row FOR SHARE until
// TRD's transaction ends.
func (s Service) guardUpstream(ctx context.Context, exec bentopg.Executor, d quote.Data, seal *quote.Seal, at time.Time) error {
	const p = "/args/data"
	// CAT (R14): the SKU specification revisions — extension point for CAT's guard.
	if g := s.Upstream.Guards.Catalog; g != nil {
		specs := make([]catapi.SpecRef, len(d.Lines))
		for i, l := range d.Lines {
			specs[i] = catapi.SpecRef{SkuID: l.Sku.SkuID, SpecRevision: l.Sku.SpecRevision}
		}
		if err := g(ctx, exec, specs, p+"/lines"); err != nil {
			return err
		}
	}
	// CST (R11): AdoptabilityGuard v1 on the cost snapshot and its result hash.
	ref := cstapi.CostSnapshotRef{SnapshotID: seal.Snapshot.SnapshotID, ResultHash: seal.Snapshot.ResultHash}
	adoptable, err := cstapi.ScanAdoptabilityGuard(ref, exec.QueryRow(ctx, cstapi.AdoptabilityGuardSQL, cstapi.AdoptabilityGuardArgs(ref, at)...).Scan)
	if err != nil {
		return err
	}
	if err = adoptable.Check(p + "/cost_snapshot_ref"); err != nil {
		return err
	}
	// INQ (R12): RfqGuard v1 on the RFQ at its row version with its line set.
	rfq := inqapi.RfqBinding{RfqID: seal.Rfq.ID, RowVersion: seal.Rfq.Version, LinesHash: seal.Rfq.ContentHash}
	ready, err := inqapi.ScanRfqGuard(rfq, exec.QueryRow(ctx, inqapi.RfqGuardSQL, inqapi.RfqGuardArgs(rfq)...).Scan)
	if err != nil {
		return err
	}
	if err = ready.Check(p + "/rfq_ref"); err != nil {
		return err
	}
	// POL (R15): ConfigGuard v1 on every revision adopted, directly or through another; a revision's
	// content never changes, so the hash it answers is the one the manifest binds.
	paths := map[quote.Ref]string{}
	for _, field := range d.Fields() {
		if _, ok := paths[field.Ref]; !ok {
			paths[field.Ref] = field.Path
		}
	}
	for _, c := range seal.Configs {
		r := polapi.ConfigRef{Kind: c.Kind, ConfigID: c.ID, Revision: c.Version}
		path, ok := paths[quote.Ref{Kind: c.Kind, ID: c.ID, Revision: c.Version}]
		if !ok {
			path = p
		}
		v, err := polapi.ScanConfigGuard(r, exec.QueryRow(ctx, polapi.ConfigGuardSQL, polapi.ConfigGuardArgs(r, at)...).Scan)
		if err != nil {
			return err
		}
		if err = v.Check(path); err != nil {
			return err
		}
		if v.ContentHash != c.ContentHash {
			return kernel.RejectAs(api.CodeReferenceStale, kernel.CategoryConflict, path, "The configuration changed since it was read")
		}
	}
	// PTY (R13): party, contact, trading profile and addresses — extension point for PTY's guards.
	if g := s.Upstream.Guards.Parties; g != nil {
		b := PartyBinding{Party: ptyapi.PartyRef{PartyID: d.Customer.PartyID, Revision: d.Customer.Revision},
			Contact: ptyapi.ContactRef{ContactID: d.Contact.ContactID, PartyID: d.Customer.PartyID, RowVersion: d.Contact.RowVersion},
			Profile: ptyapi.TradingProfileRef{PartyID: d.Profile.PartyID, Revision: d.Profile.Revision}}
		for _, a := range d.Addresses {
			b.Addresses = append(b.Addresses, ptyapi.AddressRoleRef{PartyID: a.PartyID, PartyRevision: a.PartyRevision, AddressID: a.AddressID})
		}
		if err = g(ctx, exec, b); err != nil {
			return err
		}
	}
	return nil
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
// transaction of its own, before TRD's.
func (s Service) facts(ctx context.Context, d quote.Data, at time.Time) (facts, error) {
	f := facts{Facts: quote.Facts{Company: string(scope(ctx).Company), Addresses: map[quote.AddressRef]quote.Address{},
		Specs: map[quote.SkuRef]quote.Verdict{}, Configs: map[quote.Ref]quote.Config{}}, configs: map[polapi.ConfigRef]polapi.ResolvedConfig{}}
	var err error
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
			f.Rfq = &quote.Rfq{RowVersion: r.RowVersion, Ready: r.Ready(), LinesHash: r.LinesHash}
			for _, l := range r.Lines {
				f.Rfq.Lines = append(f.Rfq.Lines, quote.RfqLine{LineID: l.LineID, SkuID: l.SkuID, SpecRevision: l.SkuSpecRevision,
					Quantity: l.Quantity, UOM: l.UOM})
			}
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
	if err = s.configs(ctx, d.ConfigRefs(), at, &f); err != nil {
		return f, err
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
			f.Snapshot = &quote.Snapshot{RfqID: rs.Rfq.RfqID, RfqVersion: rs.Rfq.RowVersion, RfqLinesHash: rs.Rfq.LinesHash,
				Currency: rs.Currency, EvidenceHash: rs.EvidenceHash, Check: func(path string) error {
					if a.Adoptable() {
						return nil
					}
					return kernel.RejectAs(cstapi.CodeCostSnapshotIneligible, kernel.CategoryInvalid, path, "The cost snapshot is "+a.Status)
				}}
			for _, l := range rs.Lines {
				f.Snapshot.LineIDs = append(f.Snapshot.LineIDs, l.LineID)
			}
		}
	}
	return f, nil
}

func compareSpec(a, b catapi.SpecRef) int {
	if a.SkuID != b.SkuID {
		if a.SkuID < b.SkuID {
			return -1
		}
		return 1
	}
	return int(a.SpecRevision - b.SpecRevision)
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
		for _, r := range refs {
			pr := polapi.ConfigRef{Kind: r.Kind, ConfigID: r.ID, Revision: r.Revision}
			if _, done := f.configs[pr]; !done && pr.Validate("/") == nil {
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
			}
		}
		refs = next
	}
	return nil
}

// config is what the freeze judges of a POL revision.
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
	case d.Currency != nil:
		c.CurrencyCode = d.Currency.Code
	case d.Unit != nil:
		c.UnitCode = d.Unit.Code
	case d.UnitConversion != nil:
		u := d.UnitConversion
		c.Conversion = &quote.Conversion{SkuID: u.SkuID, SpecRevision: u.SpecRevision, From: u.FromUnit, To: u.ToUnit, Factor: u.Factor}
	case d.FXRate != nil:
		c.FX = &quote.FX{From: d.FXRate.FromCurrency, To: d.FXRate.ToCurrency, Purpose: d.FXRate.Purpose}
	case d.TradeTerms != nil:
		nested(d.TradeTerms.Delivery.Calendar)
	case d.PaymentTerms != nil:
		nested(d.PaymentTerms.Calendar)
	case d.TaxApplication != nil:
		t := d.TaxApplication
		c.TaxApplication = &quote.TaxApplication{Side: t.Side, SubjectKind: t.SubjectKind, SubjectID: t.SubjectID, PartyID: t.PartyID}
		nested(&t.TaxProfile)
	case d.FeeCategory != nil:
		c.FeeSide = d.FeeCategory.Side
	case d.PricingPolicy != nil:
		c.Policy = &quote.Policy{CalculationCurrency: d.PricingPolicy.CalculationCurrency, QuoteValidDays: d.PricingPolicy.QuoteValidDays}
		nested(&d.PricingPolicy.TaxProfile)
	case d.NumberSequence != nil:
		c.SequenceType = d.NumberSequence.DocumentType
	case d.QuoteDocumentTemplate != nil:
		c.Renderer = d.QuoteDocumentTemplate.RendererVersion
	}
	return c
}
