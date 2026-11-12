package application

import (
	"context"
	"errors"
	"slices"
	"sort"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/internal/adapters/postgres"
	pin "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/purchaseinvoice"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Purchase invoices (WBS S5-05b; CNX-AGG-PurchaseInvoice, SM-CNX-INVOICE, CNX-INV-005; CN-R06,
// CN-AT05, CN-AT15, CN-AT18). Upstreams, all read before CNX's transaction: EVD (R54, the original's
// file), POL (R50, the invoice currency's revision and the company_profile that is the buyer
// entity), PUR (R37, PurchaseLineRef v1 through the port PurchaseLines). Inside the transaction the
// invoice root is locked FOR UPDATE (with the blue invoice a red one reverses, in ID order), then
// the advisory locks of the commitment lines matched, then the ledger's authority guard FOR SHARE;
// no upstream row is locked.

// currencyOf judges a POL currency revision resolved before the transaction: it must exist as one
// (REFERENCE_INVALID) and be in force (POL's verdict).
func currencyOf(r *polapi.ResolvedConfig, path string) (pin.Currency, error) {
	if r == nil || r.Ref.Kind != polapi.KindCurrency || r.Data.Currency == nil {
		return pin.Currency{}, kernel.Reject(kernel.CodeReferenceInvalid, path, "")
	}
	if err := r.Check(path); err != nil {
		return pin.Currency{}, err
	}
	return pin.Currency{ConfigID: r.Ref.ConfigID, Revision: r.Ref.Revision, Code: r.Data.Currency.Code, MinorUnits: r.Data.Currency.MinorUnits}, nil
}

func (s Service) resolveCurrency(ctx context.Context, c api.CurrencyRef) (*polapi.ResolvedConfig, error) {
	ref := polapi.ConfigRef{Kind: polapi.KindCurrency, ConfigID: c.ID, Revision: c.Revision}
	resolved, err := s.Upstream.Configs.ResolveConfigs(ctx, []polapi.ConfigRef{ref}, s.Clock.Now())
	if err != nil {
		return nil, err
	}
	if r, ok := resolved[ref]; ok {
		return &r, nil
	}
	return nil, nil
}

func factsOf(f api.InvoiceFacts, c pin.Currency) pin.Facts {
	out := pin.Facts{Kind: f.Kind, Colour: f.Colour, Number: f.Number, IssuedOn: f.IssuedOn,
		Supplier: pin.Party{PartyID: f.Supplier.PartyID, Name: f.Supplier.Name, TaxID: f.Supplier.TaxID},
		Buyer:    pin.Party{Name: f.Buyer.Name, TaxID: f.Buyer.TaxID}, Currency: c, Lines: []pin.Line{},
		NetTotal: f.NetTotal, TaxTotal: f.TaxTotal, GrossTotal: f.GrossTotal}
	for _, l := range f.Lines {
		out.Lines = append(out.Lines, pin.Line{LineNo: l.LineNo, Description: l.Description, Quantity: l.Quantity, Unit: l.Unit,
			NetAmount: l.NetAmount, TaxRate: l.TaxRate, TaxAmount: l.TaxAmount})
	}
	if r := f.RedOf; r != nil {
		out.RedOf = &pin.Ref{Kind: r.Kind, Number: r.Number}
	}
	return out
}

// purchase_invoice.import (human or AI; purchaser, tax specialist). Before CNX's transaction: the
// candidate's currency revision (POL) and the original's file (EVD). In the transaction, the
// refusals in this order: an invoice of the same kind and number that is not rejected
// (INVOICE_DUPLICATE), the currency (REFERENCE_INVALID, POL's CONFIG_NOT_ACTIVE or CONFIG_EXPIRED),
// the candidate's amounts (INVALID_KNOWN_VALUE: digits, totals), the file (REFERENCE_INVALID). An
// import publishes nothing: it is a candidate (the tactical model names PurchaseInvoiceVerified
// only).
type importInvoice struct {
	s        Service
	cmd      api.ImportInvoice
	currency *polapi.ResolvedConfig
	file     evdapi.FileView
	fileErr  error
}

func (h *importInvoice) Prepare(ctx context.Context) error {
	var err error
	h.fileErr = nil
	if h.currency, err = h.s.resolveCurrency(ctx, h.cmd.Candidate.Currency); err != nil {
		return err
	}
	h.file, err = h.s.Upstream.Sources.GetFile(ctx, h.cmd.FileID)
	if rej, ok := kernel.AsRejection(err); ok && rej.Code == kernel.CodeNotFound {
		h.fileErr = licenseRefusal(err, "/args/file_id")
		return nil
	}
	return err
}

func (h *importInvoice) Lock(context.Context, bentopg.Executor) error { return nil }

func (h *importInvoice) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, cmd := scope(ctx), h.cmd
	taken, err := postgres.InvoiceByKey(ctx, exec, sc, cmd.Candidate.Kind, cmd.Candidate.Number)
	if err != nil {
		return action.Outcome{}, err
	}
	if taken != "" {
		return action.Outcome{}, api.Reject(api.CodeInvoiceDuplicate, "/args/candidate/number", "Invoice "+taken+" has this kind and number")
	}
	cur, err := currencyOf(h.currency, "/args/candidate/currency")
	if err != nil {
		return action.Outcome{}, err
	}
	facts := factsOf(cmd.Candidate, cur)
	if err = facts.CheckAmounts("/args/candidate"); err != nil {
		return action.Outcome{}, err
	}
	if h.fileErr != nil {
		return action.Outcome{}, h.fileErr
	}
	if h.file.FileID != cmd.FileID {
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/file_id", "")
	}
	now := kernel.Instant(h.s.Clock.Now())
	inv := &pin.Invoice{ID: action.NewID("pin", now), Status: pin.Imported, RowVersion: 1, FileID: h.file.FileID, FileSHA256: h.file.ContentSHA256,
		Candidate: facts, ImportedBy: d.Actor, ImportEpoch: d.Epoch, ImportedAt: now}
	if err = postgres.InsertInvoice(ctx, exec, sc, inv); err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: inv.ID, RowVersion: 1, Status: pin.Imported}, nil
}

// ImportInvoice imports a purchase invoice as a candidate (purchase_invoice.import).
func (s Service) ImportInvoice(ctx context.Context, cmd api.ImportInvoice) (api.InvoiceReceipt, error) {
	h := &importInvoice{s: s, cmd: cmd}
	return s.runInvoice(ctx, action.Command{Name: api.CommandImportInvoice, IdempotencyKey: cmd.IdempotencyKey}, cmd, cmd.Validate(), h)
}

// purchase_invoice.verify (human only; tax specialist). Before CNX's transaction, for a
// verification: the verified currency revision and the company's profile (POL; the profile needs
// pol.read), and the commitment lines matched (PUR, PurchaseLineRef v1). In the transaction, after
// the invoice (and, for a red one, the blue one it names) FOR UPDATE and the matched lines' advisory
// locks: NOT_FOUND, VERSION_CONFLICT, INVALID_STATE (not imported); a rejection is stored. A
// verification then refuses in this order: the verified kind and number held by another invoice
// (INVOICE_DUPLICATE), the currency, the amounts (INVALID_KNOWN_VALUE), a buyer that is not the
// company's entity (PARTY_ENTITY_MISMATCH: the invoice stays imported, CN-AT15), for a red invoice
// the blue one it names (REFERENCE_INVALID, INVALID_STATE, PARTY_ENTITY_MISMATCH), then the matches
// (REFERENCE_INVALID, INVALID_STATE, PARTY_ENTITY_MISMATCH, INVALID_KNOWN_VALUE). A verified red
// invoice reverses the blue one: its matches and the evidence allocated from it are under review
// from then on (CN-R06). PurchaseInvoiceVerified is published.
type verifyInvoice struct {
	s        Service
	cmd      api.VerifyInvoice
	currency *polapi.ResolvedConfig
	company  pin.Company
	lines    map[PurchaseLineRef]PurchaseLine
	invoice  *pin.Invoice
	original *pin.Invoice
}

func (h *verifyInvoice) Prepare(ctx context.Context) error {
	h.currency, h.company, h.lines = nil, pin.Company{}, nil
	f := h.cmd.Facts
	if f == nil {
		return nil
	}
	var err error
	if h.currency, err = h.s.resolveCurrency(ctx, f.Currency); err != nil {
		return err
	}
	p, err := identity.FromContext(ctx)
	if err != nil {
		return err
	}
	profile, err := h.s.Upstream.Configs.GetConfig(ctx, polapi.KindCompanyProfile, string(p.Scope.Company))
	if rej, ok := kernel.AsRejection(err); ok && rej.Code == kernel.CodeNotFound {
		err = nil
	}
	if err != nil {
		return err
	}
	for _, r := range profile.Revisions {
		if r.Revision == profile.ActiveRevision && r.Data.CompanyProfile != nil {
			h.company = pin.Company{LegalName: r.Data.CompanyProfile.LegalName, RegistrationNo: r.Data.CompanyProfile.RegistrationNo}
		}
	}
	var refs []PurchaseLineRef
	for _, m := range h.cmd.Matches {
		r := PurchaseLineRef{CommitmentID: m.CommitmentID, OrderLineID: m.OrderLineID}
		if !slices.Contains(refs, r) {
			refs = append(refs, r)
		}
	}
	if len(refs) > 0 {
		h.lines, err = h.s.Upstream.Purchases.ResolvePurchaseLines(ctx, refs)
	}
	return err
}

func (h *verifyInvoice) matchInputs() []pin.MatchInput {
	var out []pin.MatchInput
	for _, m := range h.cmd.Matches {
		q, _ := kernel.ParseDecimal(m.Quantity, kernel.QuantityFormat)
		out = append(out, pin.MatchInput{LineNo: m.LineNo, Ref: pin.LineRef{CommitmentID: m.CommitmentID, OrderLineID: m.OrderLineID}, Quantity: q})
	}
	return out
}

func (h *verifyInvoice) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.invoice, h.original = nil, nil
	sc := scope(ctx)
	ids := []string{h.cmd.PurchaseInvoiceID}
	if f := h.cmd.Facts; f != nil && f.RedOf != nil {
		id, err := postgres.InvoiceByKey(ctx, exec, sc, f.RedOf.Kind, f.RedOf.Number)
		if err != nil {
			return err
		}
		if id != "" && id != h.cmd.PurchaseInvoiceID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		inv, err := postgres.LoadInvoice(ctx, exec, sc, id, postgres.ForUpdate)
		if err != nil {
			return err
		}
		if id == h.cmd.PurchaseInvoiceID {
			h.invoice = inv
		} else {
			h.original = inv
		}
	}
	return postgres.LockMatchLines(ctx, exec, sc, pin.Refs(h.matchInputs()))
}

func (h *verifyInvoice) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, cmd, inv := scope(ctx), h.cmd, h.invoice
	if inv == nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/purchase_invoice_id", "")
	}
	if err := pin.CheckVersion(inv, cmd.ExpectedVersion); err != nil {
		return action.Outcome{}, err
	}
	if err := pin.CheckDecidable(inv); err != nil {
		return action.Outcome{}, err
	}
	now := kernel.Instant(h.s.Clock.Now())
	from := inv.RowVersion
	inv.RowVersion++
	inv.Decision, inv.Reason, inv.DecidedBy, inv.DecideEpoch, inv.DecidedAt = cmd.Decision, cmd.Reason, d.Actor, d.Epoch, now
	if cmd.Decision == api.DecisionReject {
		inv.Status = pin.Rejected
		if err := postgres.DecideInvoice(ctx, exec, sc, inv, from); err != nil {
			return action.Outcome{}, err
		}
		return action.Outcome{ObjectID: inv.ID, RowVersion: inv.RowVersion, Status: inv.Status}, nil
	}
	f := cmd.Facts
	taken, err := postgres.InvoiceByKey(ctx, exec, sc, f.Kind, f.Number)
	if err != nil {
		return action.Outcome{}, err
	}
	if taken != "" && taken != inv.ID {
		return action.Outcome{}, api.Reject(api.CodeInvoiceDuplicate, "/args/facts/number", "Invoice "+taken+" has this kind and number")
	}
	cur, err := currencyOf(h.currency, "/args/facts/currency")
	if err != nil {
		return action.Outcome{}, err
	}
	facts := factsOf(*f, cur)
	if err = facts.CheckAmounts("/args/facts"); err != nil {
		return action.Outcome{}, err
	}
	if err = pin.CheckBuyer(facts, h.company, "/args/facts"); err != nil {
		return action.Outcome{}, err
	}
	if facts.Colour == pin.Red {
		if err = pin.CheckReversible(h.original, facts, "/args/facts/red_of"); err != nil {
			return action.Outcome{}, err
		}
		inv.RedOfID = h.original.ID
	}
	in := h.matchInputs()
	refs := pin.Refs(in)
	standing, err := postgres.StandingMatched(ctx, exec, sc, refs, inv.ID)
	if err != nil {
		return action.Outcome{}, err
	}
	lines := map[pin.LineRef]pin.PurchaseLine{}
	for r, l := range h.lines {
		lines[pin.LineRef(r)] = pin.PurchaseLine(l)
	}
	matches, err := pin.CheckMatches(facts, inv.ID, in, lines, standing, now)
	if err != nil {
		return action.Outcome{}, err
	}
	inv.Status, inv.Verified = pin.Verified, &facts
	if err = postgres.DecideInvoice(ctx, exec, sc, inv, from); err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.InsertInvoiceLines(ctx, exec, sc, inv.ID, facts); err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.InsertMatches(ctx, exec, sc, matches); err != nil {
		return action.Outcome{}, err
	}
	fields := map[string]any{"purchase_invoice_id": inv.ID, "kind": facts.Kind, "colour": facts.Colour, "number": facts.Number,
		"supplier_party_id": facts.Supplier.PartyID, "currency": facts.Currency.Code, "lines": len(facts.Lines), "matches": len(matches),
		"commitment_ids": commitmentIDs(refs), "reversed_invoice_id": inv.RedOfID}
	if o := h.original; facts.Colour == pin.Red {
		ofrom := o.RowVersion
		o.RowVersion++
		if err = postgres.ReverseInvoice(ctx, exec, sc, o, inv.ID, ofrom, now); err != nil {
			return action.Outcome{}, err
		}
		fields["reversed_invoice_version"] = o.RowVersion
		if ms, err := postgres.Matches(ctx, exec, sc, o.ID); err == nil {
			var ors []pin.LineRef
			for _, m := range ms {
				if !slices.Contains(ors, m.Ref) {
					ors = append(ors, m.Ref)
				}
			}
			fields["review_commitment_ids"] = commitmentIDs(ors)
		} else {
			return action.Outcome{}, err
		}
	}
	return action.Outcome{ObjectID: inv.ID, RowVersion: inv.RowVersion, Status: inv.Status, Events: []action.Event{{
		Type: api.EventInvoiceVerified, AggregateType: "PurchaseInvoice", AggregateID: inv.ID, AggregateVersion: inv.RowVersion,
		Fields: fields}}}, nil
}

func commitmentIDs(refs []pin.LineRef) []string {
	out := []string{}
	for _, r := range refs {
		if !slices.Contains(out, r.CommitmentID) {
			out = append(out, r.CommitmentID)
		}
	}
	sort.Strings(out)
	return out
}

// VerifyInvoice verifies or rejects an imported purchase invoice (purchase_invoice.verify).
func (s Service) VerifyInvoice(ctx context.Context, cmd api.VerifyInvoice) (api.InvoiceReceipt, error) {
	h := &verifyInvoice{s: s, cmd: cmd}
	return s.runInvoice(ctx, action.Command{Name: api.CommandVerifyInvoice, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.PurchaseInvoiceID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
}

func (s Service) runInvoice(ctx context.Context, cmd action.Command, req api.Command, validation error, h action.Handler) (api.InvoiceReceipt, error) {
	if validation != nil {
		if rej, ok := kernel.AsRejection(validation); ok {
			if err := s.Ledger.AuditRejected(ctx, cmd.Name, cmd.ObjectID, rej.Code); err != nil {
				return api.InvoiceReceipt{}, errors.Join(validation, err)
			}
		}
		return api.InvoiceReceipt{}, validation
	}
	_, _, args := request(req)
	hash, err := action.RequestHash(args)
	if err != nil {
		return api.InvoiceReceipt{}, err
	}
	cmd.RequestHash = hash
	res, err := s.Ledger.Execute(ctx, cmd, h)
	if err != nil {
		return api.InvoiceReceipt{}, err
	}
	out := api.InvoiceReceipt{Receipt: api.Receipt(res), PurchaseInvoiceID: res.ObjectID}
	err = s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		inv, err := postgres.LoadInvoice(ctx, exec, scope(ctx), res.ObjectID, postgres.NoLock)
		if err != nil || inv == nil {
			return err
		}
		k := inv.Key()
		out.Kind, out.Number, out.ReversedInvoiceID = k.Kind, k.Number, inv.RedOfID
		return nil
	})
	return out, err
}
