package application

import (
	"context"
	"slices"
	"sort"
	"strconv"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/internal/adapters/postgres"
	pin "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/purchaseinvoice"
	tr "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/taxrefund"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Opening refund material (S5 unit C10, WBS S5-20; CNX-AGG-TaxRefundCase value object OpeningPosition, CNX-INV-004;
// MD-51; baseline 11.2; CN-R09; CN-AT16 CNX half, BT17). Upstreams, read before CNX's transaction: EVD (R54, the
// evidence of the item and of an opening case's position) and POL (R50: the export_tax_policy revision in force at
// an opening case's export date, policyAt as tax_refund_case.allocate binds it, and the currency revision of its
// position's amount). In the transaction CNX locks, for an opening case, the export's latest case FOR UPDATE (a
// second opening of the export waits and sees the first) and then the invoices of its allocations FOR UPDATE in
// ID order; for an invoice balance the invoice FOR UPDATE — the order tax_refund_case.allocate locks in, so
// allocations and opening consumption of one invoice line serialize; then the ledger takes the authority guard FOR
// SHARE. No upstream row is locked.

// openingRules: humanOnlyCommands name tax_refund_case.record_opening (tax specialist); the reads need cnx.read.
func openingRules() []authorization.Rule {
	return []authorization.Rule{
		authorization.HumanOnly(api.CommandRecordTaxOpening, api.CommandRecordTaxOpening),
		{Command: api.CommandGetTaxOpening, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandTaxOpenings, Permission: api.PermissionRead, ActorKinds: anyActor},
	}
}

// openingRequest is request's part for tax_refund_case.record_opening: everything but the key.
func openingRequest(cmd api.Command) (name, key string, args any) {
	if c, ok := cmd.(api.RecordTaxOpening); ok {
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return api.CommandRecordTaxOpening, key, c
	}
	return "", "", nil
}

// visibleOpening is Visible's part for opening items (company-wide like the cases).
func visibleOpening(ctx context.Context, exec bentopg.Executor, command, id string) (ok, visible bool, err error) {
	if command != api.CommandRecordTaxOpening {
		return false, false, nil
	}
	o, err := postgres.LoadTaxOpening(ctx, exec, scope(ctx), id)
	return true, o != nil, err
}

// tax_refund_case.record_opening (human only; tax specialist). The refusals, in this order: opening material
// recorded already — the source document as an item of the kind, the invoice's balance, a case of the export that
// was not rejected (OPENING_BALANCE_DUPLICATE) —; the opening ID taken (INVALID_STATE); the evidence
// (REFERENCE_INVALID); a cutoff after now, an export date after the cutoff (INVALID_KNOWN_VALUE). An invoice
// balance: an invoice the company lacks (REFERENCE_INVALID), not a verified blue one (INVALID_STATE), the domain's
// (REFERENCE_INVALID, TAX_EVIDENCE_EXCEEDED). An opening case: an export CNX declared itself (REFERENCE_INVALID: its
// lot's case is allocated from FUL's lot), the policy binding (REFERENCE_INVALID, CONFIG_NOT_ACTIVE,
// CONFIG_EXPIRED), an allocation ID taken (BUSINESS_KEY_CONFLICT), the domain's allocation refusals, then the
// position's currency (REFERENCE_INVALID, POL's verdict), amounts (INVALID_KNOWN_VALUE) and evidence
// (REFERENCE_INVALID). TaxOpeningRecorded is published.
type recordTaxOpening struct {
	s         Service
	cmd       api.RecordTaxOpening
	sources   map[string]evdapi.SourceRef
	currency  *polapi.ResolvedConfig
	policy    *polapi.ResolvedConfig
	policyErr error

	recorded *tr.Opening
	taken    bool
	lotCase  *tr.Case
	invoice  *pin.Invoice
	declared bool

	allocationIDs []string
	resultID      string
}

func (h *recordTaxOpening) Prepare(ctx context.Context) error {
	c := h.cmd
	h.currency, h.policy, h.policyErr = nil, nil, nil
	ids := slices.Clone(c.EvidenceIDs)
	if p := c.Position; p != nil {
		ids = append(ids, p.EvidenceIDs...)
		if p.Review != nil {
			ids = append(ids, p.Review.EvidenceIDs...)
		}
	}
	var err error
	if h.sources, err = h.s.sources(ctx, ids); err != nil {
		return err
	}
	if c.OpeningKind != api.OpeningRefundCase {
		return nil
	}
	if cur := c.Position.Currency; cur != nil {
		if h.currency, err = h.s.resolveCurrency(ctx, *cur); err != nil {
			return err
		}
	}
	exported, _ := kernel.ParseTimestamp(c.Export.ExportedAt)
	h.policy, err = h.s.policyValidAt(ctx, c.PolicyID, kernel.Instant(exported))
	if _, ok := kernel.AsRejection(err); ok {
		h.policyErr, err = err, nil
	}
	return err
}

// policyValidAt finds the export_tax_policy revision of policyID an opening case binds for its export date at
// (taxrefund.ValidAt: published, not retired, its validity containing at — its publication may follow the export)
// and reads it from POL as it stands now; REFERENCE_INVALID when POL has no such policy, CONFIG_NOT_ACTIVE when no
// published revision's validity contains at.
func (s Service) policyValidAt(ctx context.Context, policyID string, at time.Time) (*polapi.ResolvedConfig, error) {
	const path = "/args/policy_id"
	view, err := s.Upstream.Configs.GetConfig(ctx, polapi.KindExportTaxPolicy, policyID)
	if rej, ok := kernel.AsRejection(err); ok && rej.Code == kernel.CodeNotFound {
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, path, "POL has no such export tax policy")
	}
	if err != nil {
		return nil, err
	}
	var revisions []tr.Revision
	for _, r := range view.Revisions {
		revisions = append(revisions, tr.Revision{Revision: r.Revision, Status: r.Status, PublishedAt: r.PublishedAt, EndedAt: r.EndedAt,
			ValidFrom: r.ValidFrom, ValidUntil: r.ValidUntil})
	}
	revision := tr.ValidAt(revisions, at)
	if revision == 0 {
		return nil, polapi.Reject(polapi.CodeConfigNotActive, path)
	}
	ref := polapi.ConfigRef{Kind: polapi.KindExportTaxPolicy, ConfigID: policyID, Revision: revision}
	resolved, err := s.Upstream.Configs.ResolveConfigs(ctx, []polapi.ConfigRef{ref}, s.Clock.Now())
	if err != nil {
		return nil, err
	}
	r, ok := resolved[ref]
	if !ok || r.Data.ExportTaxPolicy == nil {
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, path, "")
	}
	return &r, nil
}

func (h *recordTaxOpening) invoiceIDs() []string {
	var ids []string
	if h.cmd.PurchaseInvoiceID != "" {
		ids = append(ids, h.cmd.PurchaseInvoiceID)
	}
	for _, a := range h.cmd.Allocations {
		if !slices.Contains(ids, a.PurchaseInvoiceID) {
			ids = append(ids, a.PurchaseInvoiceID)
		}
	}
	sort.Strings(ids)
	return ids
}

func (h *recordTaxOpening) Lock(ctx context.Context, exec bentopg.Executor) error {
	sc, c := scope(ctx), h.cmd
	h.recorded, h.taken, h.lotCase, h.invoice, h.declared = nil, false, nil, nil, false
	var err error
	if c.OpeningKind == api.OpeningRefundCase {
		if h.lotCase, err = postgres.CaseOfLot(ctx, exec, sc, c.Export.Reference, postgres.ForUpdate); err != nil {
			return err
		}
	}
	if err = postgres.LockInvoices(ctx, exec, sc, h.invoiceIDs()); err != nil {
		return err
	}
	if h.recorded, err = postgres.TaxOpeningOf(ctx, exec, sc, c.OpeningKind, c.SourceDocument, c.PurchaseInvoiceID, ""); err != nil {
		return err
	}
	o, err := postgres.LoadTaxOpening(ctx, exec, sc, c.OpeningID)
	if err != nil {
		return err
	}
	h.taken = o != nil
	if c.OpeningKind == api.OpeningRefundCase {
		h.declared, err = postgres.DeclaredAs(ctx, exec, sc, c.Export.Reference)
		return err
	}
	h.invoice, err = postgres.LoadInvoice(ctx, exec, sc, c.PurchaseInvoiceID, postgres.NoLock)
	return err
}

func (h *recordTaxOpening) evidence(ids []string, path string) ([]tr.Evidence, error) {
	out := []tr.Evidence{}
	for i, id := range ids {
		src, ok := h.sources[id]
		if !ok {
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, path+"/"+strconv.Itoa(i), "No such source")
		}
		out = append(out, tr.Evidence{SourceID: src.SourceID, ContentSHA256: src.ContentSHA256})
	}
	return out, nil
}

func (h *recordTaxOpening) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	c, now := h.cmd, kernel.Instant(h.s.Clock.Now())
	if r := h.recorded; r != nil {
		if r.Kind == c.OpeningKind && r.SourceDocument == c.SourceDocument {
			return action.Outcome{}, tr.Duplicate("/args/source_document", "Source document "+r.SourceDocument+" is opening item "+r.ID)
		}
		return action.Outcome{}, tr.Duplicate("/args/purchase_invoice_id", "The invoice's opening balance is opening item "+r.ID)
	}
	if lc := h.lotCase; lc != nil && lc.Status != tr.Rejected {
		return action.Outcome{}, tr.Duplicate("/args/export/reference", "The export has case "+lc.ID+", "+lc.Status)
	}
	if h.taken {
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/opening_id", "The opening ID is taken")
	}
	ev, err := h.evidence(c.EvidenceIDs, "/args/evidence_ids")
	if err != nil {
		return action.Outcome{}, err
	}
	cutoff, _ := kernel.ParseTimestamp(c.CutoffAt)
	cutoff = kernel.Instant(cutoff)
	if cutoff.After(now) {
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/cutoff_at", "The cutoff is after now")
	}
	o := &tr.Opening{ID: c.OpeningID, Kind: c.OpeningKind, CutoffAt: cutoff, SourceDocument: c.SourceDocument, Evidence: ev, By: d.Actor,
		Epoch: d.Epoch, At: now}
	if c.OpeningKind == api.OpeningInvoiceBalance {
		return h.balance(ctx, exec, o)
	}
	return h.openCase(ctx, exec, d, o)
}

// balance records an invoice balance: the consumption of each line and purpose stated.
func (h *recordTaxOpening) balance(ctx context.Context, exec bentopg.Executor, o *tr.Opening) (action.Outcome, error) {
	sc, c := scope(ctx), h.cmd
	inv := h.invoice
	switch {
	case inv == nil:
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/purchase_invoice_id", "CNX has no such purchase invoice")
	case inv.Status != pin.Verified:
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/purchase_invoice_id", "The invoice is "+inv.Status+": only a verified invoice has balances")
	case inv.Verified == nil || inv.Verified.Colour != pin.Blue:
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/purchase_invoice_id", "A red invoice evidences nothing")
	}
	refs := make([]tr.InvoiceLineRef, 0, len(c.Balances))
	keys := make([]tr.LineKey, 0, len(c.Balances))
	in := make([]tr.BalanceInput, 0, len(c.Balances))
	for _, b := range c.Balances {
		q, _ := kernel.ParseDecimal(b.Consumed, kernel.QuantityFormat)
		refs = append(refs, tr.InvoiceLineRef{InvoiceID: inv.ID, LineNo: b.LineNo})
		keys = append(keys, tr.LineKey{InvoiceID: inv.ID, LineNo: b.LineNo, Purpose: b.Purpose})
		in = append(in, tr.BalanceInput{LineNo: b.LineNo, Purpose: b.Purpose, Consumed: q})
	}
	lines, err := postgres.InvoiceLines(ctx, exec, sc, refs)
	if err != nil {
		return action.Outcome{}, err
	}
	used, err := postgres.UsedEvidence(ctx, exec, sc, keys)
	if err != nil {
		return action.Outcome{}, err
	}
	consumed, err := tr.ConsumedAtCutoff(o.ID, inv.ID, lines, used, in)
	if err != nil {
		return action.Outcome{}, err
	}
	o.InvoiceID = inv.ID
	if err = postgres.InsertTaxOpening(ctx, exec, sc, o); err != nil {
		return action.Outcome{}, err
	}
	for _, x := range consumed {
		if err = postgres.InsertConsumption(ctx, exec, sc, x); err != nil {
			return action.Outcome{}, err
		}
	}
	return action.Outcome{ObjectID: o.ID, RowVersion: 1, Status: "recorded", Events: []action.Event{{Type: api.EventTaxOpeningRecorded,
		AggregateType: "TaxOpening", AggregateID: o.ID, AggregateVersion: 1, Fields: map[string]any{"opening_id": o.ID, "kind": o.Kind,
			"cutoff_at": kernel.FormatTimestamp(o.CutoffAt), "source_document": o.SourceDocument, "purchase_invoice_id": inv.ID}}}}, nil
}

// openCase records an opening case: the case in its position with its allocations and the position's result.
func (h *recordTaxOpening) openCase(ctx context.Context, exec bentopg.Executor, d authorization.Decision, o *tr.Opening) (action.Outcome, error) {
	sc, c, now := scope(ctx), h.cmd, o.At
	x := c.Export
	exported, _ := kernel.ParseTimestamp(x.ExportedAt)
	exported = kernel.Instant(exported)
	switch {
	case exported.After(o.CutoffAt):
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/export/exported_at", "An export declared before the cutoff was exported before it")
	case h.declared:
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/export/reference",
			"CNX declared this export itself: its refund case is allocated from its FUL export lot")
	case h.policyErr != nil:
		return action.Outcome{}, h.policyErr
	}
	for i, a := range c.Allocations {
		taken, err := postgres.TaxAllocationExists(ctx, exec, sc, a.AllocationID)
		if err != nil {
			return action.Outcome{}, err
		}
		if taken {
			return action.Outcome{}, api.Reject(api.CodeBusinessKeyConflict, "/args/allocations/"+strconv.Itoa(i)+"/allocation_id", "The allocation ID is taken")
		}
	}
	attempt := 1
	if h.lotCase != nil {
		attempt = h.lotCase.Attempt + 1
	}
	p := h.policy
	tc := &tr.Case{ID: action.NewID("trc", now), ExportLotID: x.Reference, Attempt: attempt, ExportedAt: exported, Status: c.Position.Stage,
		RowVersion: 1, OpenedBy: d.Actor, Epoch: d.Epoch, OpenedAt: now, Opening: true,
		Policy: tr.Binding{PolicyID: p.Ref.ConfigID, Revision: p.Ref.Revision, ContentHash: p.ContentHash, RefundMethod: p.Data.ExportTaxPolicy.RefundMethod,
			ValidFrom: kernel.Instant(p.ValidFrom), ValidUntil: p.ValidUntil}}
	exportLines := map[string]tr.ExportLine{}
	for _, l := range x.Lines {
		q, _ := kernel.ParseDecimal(l.Quantity, kernel.QuantityFormat)
		exportLines[l.LineID] = tr.ExportLine{LineID: l.LineID, ShipmentID: x.Reference, Quantity: q, Unit: kernel.UnitCode(l.Unit)}
	}
	var refs []tr.InvoiceLineRef
	var keys []tr.LineKey
	var in []tr.AllocateInput
	for _, a := range c.Allocations {
		q, _ := kernel.ParseDecimal(a.Quantity, kernel.QuantityFormat)
		in = append(in, tr.AllocateInput{ID: a.AllocationID, ExportLineID: a.ExportLineID, InvoiceID: a.PurchaseInvoiceID, LineNo: a.LineNo,
			Purpose: a.Purpose, Quantity: q})
		refs = append(refs, tr.InvoiceLineRef{InvoiceID: a.PurchaseInvoiceID, LineNo: a.LineNo})
		keys = append(keys, tr.LineKey{InvoiceID: a.PurchaseInvoiceID, LineNo: a.LineNo, Purpose: a.Purpose})
	}
	lines, err := postgres.InvoiceLines(ctx, exec, sc, refs)
	if err != nil {
		return action.Outcome{}, err
	}
	used, err := postgres.UsedEvidence(ctx, exec, sc, keys)
	if err != nil {
		return action.Outcome{}, err
	}
	allocations, err := tr.OpenAllocations(tr.Context{Case: tc, ExportLines: exportLines, InvoiceLines: lines, Used: used, By: d.Actor, Epoch: d.Epoch,
		At: now}, in)
	if err != nil {
		return action.Outcome{}, err
	}
	r, err := h.position(tc, d, now)
	if err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.InsertCase(ctx, exec, sc, tc); err != nil {
		return action.Outcome{}, err
	}
	invoices := []string{}
	for _, a := range allocations {
		if err = postgres.InsertTaxAllocation(ctx, exec, sc, a); err != nil {
			return action.Outcome{}, err
		}
		h.allocationIDs = append(h.allocationIDs, a.ID)
		if !slices.Contains(invoices, a.InvoiceID) {
			invoices = append(invoices, a.InvoiceID)
		}
	}
	sort.Strings(invoices)
	if err = postgres.InsertTaxResult(ctx, exec, sc, r); err != nil {
		return action.Outcome{}, err
	}
	o.CaseID, o.ExportReference, o.ExportedAt, o.OrderReference, o.Position = tc.ID, x.Reference, exported, x.OrderReference, tc.Status
	if err = postgres.InsertTaxOpening(ctx, exec, sc, o); err != nil {
		return action.Outcome{}, err
	}
	h.resultID = r.ID
	return action.Outcome{ObjectID: o.ID, RowVersion: 1, Status: tc.Status, Events: []action.Event{{Type: api.EventTaxOpeningRecorded,
		AggregateType: "TaxRefundCase", AggregateID: tc.ID, AggregateVersion: tc.RowVersion, Fields: map[string]any{"opening_id": o.ID,
			"kind": o.Kind, "cutoff_at": kernel.FormatTimestamp(o.CutoffAt), "source_document": o.SourceDocument, "case_id": tc.ID,
			"export_lot_id": tc.ExportLotID, "position": tc.Status, "policy_id": tc.Policy.PolicyID, "policy_revision": tc.Policy.Revision,
			"allocation_ids": h.allocationIDs, "purchase_invoice_ids": invoices}}}}, nil
}

// position is the result an opening case opens with: the stage of its position with that stage's members, From
// "opening".
func (h *recordTaxOpening) position(tc *tr.Case, d authorization.Decision, now time.Time) (*tr.Result, error) {
	p := h.cmd.Position
	r := &tr.Result{ID: action.NewID("trr", now), CaseID: tc.ID, Stage: p.Stage, From: api.FromOpening, TaxPeriod: p.TaxPeriod,
		ExternalReference: p.ExternalReference, ResultText: p.ResultText, Reason: p.Reason, By: d.Actor, Epoch: d.Epoch, At: now}
	var kc kernel.Currency
	if p.Currency != nil {
		cur, err := currencyOf(h.currency, "/args/position/currency")
		if err != nil {
			return nil, err
		}
		r.Currency = &tr.Currency{ConfigID: cur.ConfigID, Revision: cur.Revision, Code: cur.Code, MinorUnits: cur.MinorUnits}
		if kc, err = cur.Kernel(); err != nil {
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/args/position/currency", "")
		}
	}
	for _, x := range []struct {
		s, path string
		to      **kernel.Money
	}{{p.Amount, "/args/position/amount", &r.Amount}, {p.OffsetAmount, "/args/position/offset_amount", &r.Offset}} {
		if x.s == "" {
			continue
		}
		m, err := kernel.ParseAmount(kc, x.s)
		if err != nil {
			return nil, kernel.Reject(kernel.CodeInvalidKnownValue, x.path, "More digits than "+kc.Code()+" has minor units")
		}
		*x.to = &m
	}
	var err error
	if r.Evidence, err = h.evidence(p.EvidenceIDs, "/args/position/evidence_ids"); err != nil {
		return nil, err
	}
	if rv := p.Review; rv != nil {
		r.Reviewer = rv.Reviewer
		if r.ReviewEvidence, err = h.evidence(rv.EvidenceIDs, "/args/position/review/evidence_ids"); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// RecordTaxOpening records opening refund material (tax_refund_case.record_opening).
func (s Service) RecordTaxOpening(ctx context.Context, cmd api.RecordTaxOpening) (api.TaxOpeningReceipt, error) {
	h := &recordTaxOpening{s: s, cmd: cmd}
	var out api.TaxOpeningReceipt
	validation := cmd.Validate()
	c := action.Command{Name: api.CommandRecordTaxOpening, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.OpeningID}
	if validation != nil {
		if rej, ok := kernel.AsRejection(validation); ok {
			if err := s.Ledger.AuditRejected(ctx, c.Name, c.ObjectID, rej.Code); err != nil {
				return out, err
			}
		}
		return out, validation
	}
	_, _, args := request(cmd)
	hash, err := action.RequestHash(args)
	if err != nil {
		return out, err
	}
	c.RequestHash = hash
	res, err := s.Ledger.Execute(ctx, c, h)
	if err != nil {
		return out, err
	}
	out = api.TaxOpeningReceipt{Receipt: api.Receipt(res), AllocationIDs: h.allocationIDs, ResultID: h.resultID}
	err = s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		o, err := postgres.LoadTaxOpening(ctx, exec, scope(ctx), res.ObjectID)
		if err != nil || o == nil {
			return err
		}
		out.OpeningKind, out.CutoffAt, out.SourceDocument, out.PurchaseInvoiceID, out.CaseID = o.Kind, o.CutoffAt, o.SourceDocument, o.InvoiceID, o.CaseID
		return nil
	})
	return out, err
}

// taxOpeningView is opening item o now: an invoice balance's lines with what is allocated and available now, an
// opening case's case now.
func taxOpeningView(ctx context.Context, exec bentopg.Executor, o *tr.Opening, now time.Time) (api.TaxOpeningView, error) {
	sc := scope(ctx)
	v := api.TaxOpeningView{OpeningID: o.ID, OpeningKind: o.Kind, CutoffAt: o.CutoffAt, SourceDocument: o.SourceDocument,
		Evidence: taxEvidenceViews(o.Evidence), PurchaseInvoiceID: o.InvoiceID, Lines: []api.TaxOpeningLineView{}, CaseID: o.CaseID,
		ExportReference: o.ExportReference, ExportedAt: o.ExportedAt, OrderReference: o.OrderReference, Position: o.Position, RecordedBy: o.By,
		RecordedAt: o.At, AuthorityEpoch: o.Epoch, CheckedAt: now}
	if o.CaseID != "" {
		c, err := postgres.LoadCase(ctx, exec, sc, o.CaseID, postgres.NoLock)
		if err != nil || c == nil {
			return v, err
		}
		v.Case = &api.TaxCaseSummary{CaseID: c.ID, ExportLotID: c.ExportLotID, Attempt: c.Attempt, Status: c.Status, PolicyID: c.Policy.PolicyID,
			PolicyRevision: c.Policy.Revision, RefundMethod: c.Policy.RefundMethod, RowVersion: c.RowVersion, OpenedAt: c.OpenedAt}
		return v, nil
	}
	consumed, err := postgres.Consumptions(ctx, exec, sc, o.InvoiceID)
	if err != nil {
		return v, err
	}
	usage, err := postgres.InvoiceUsage(ctx, exec, sc, o.InvoiceID)
	if err != nil {
		return v, err
	}
	refs := make([]tr.InvoiceLineRef, 0, len(consumed))
	for _, x := range consumed {
		refs = append(refs, tr.InvoiceLineRef{InvoiceID: o.InvoiceID, LineNo: x.LineNo})
	}
	lines, err := postgres.InvoiceLines(ctx, exec, sc, refs)
	if err != nil {
		return v, err
	}
	for _, x := range consumed {
		if x.OpeningID != o.ID {
			continue
		}
		l := lines[tr.InvoiceLineRef{InvoiceID: o.InvoiceID, LineNo: x.LineNo}]
		var allocated kernel.Decimal
		for _, u := range usage {
			if u.LineNo == x.LineNo && u.Purpose == x.Purpose {
				allocated = u.Used
			}
		}
		remaining := l.Quantity.Sub(x.Consumed)
		v.Lines = append(v.Lines, api.TaxOpeningLineView{LineNo: x.LineNo, Unit: string(x.Unit), Purpose: x.Purpose, Quantity: l.Quantity.String(),
			Consumed: x.Consumed.String(), RemainingAtCut: remaining.String(), AllocatedNow: allocated.String(),
			AvailableNow: remaining.Sub(allocated).String()})
	}
	return v, nil
}

// GetTaxOpening returns an opening item (tax_opening.get).
func (s Service) GetTaxOpening(ctx context.Context, openingID string) (api.TaxOpeningView, error) {
	var v api.TaxOpeningView
	if !kernel.ValidIdentifier(openingID) {
		return v, kernel.Reject(kernel.CodeSchemaInvalid, "/id", "")
	}
	err := s.Ledger.Read(ctx, api.CommandGetTaxOpening, openingID, func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		o, err := postgres.LoadTaxOpening(ctx, exec, scope(ctx), openingID)
		if err != nil {
			return err
		}
		if o == nil {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v, err = taxOpeningView(ctx, exec, o, kernel.Instant(s.Clock.Now()))
		return err
	})
	return v, err
}

// TaxOpenings returns the company's opening items (tax_opening.list).
func (s Service) TaxOpenings(ctx context.Context) ([]api.TaxOpeningView, error) {
	out := []api.TaxOpeningView{}
	err := s.Ledger.Read(ctx, api.CommandTaxOpenings, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		list, err := postgres.TaxOpenings(ctx, exec, scope(ctx))
		if err != nil {
			return err
		}
		now := kernel.Instant(s.Clock.Now())
		for _, o := range list {
			v, err := taxOpeningView(ctx, exec, o, now)
			if err != nil {
				return err
			}
			out = append(out, v)
		}
		return nil
	})
	return out, err
}

// openingRef is the opening an opening case was recorded from, for its TaxCaseView.
func openingRef(ctx context.Context, exec bentopg.Executor, c *tr.Case) (*api.TaxOpeningRef, error) {
	if !c.Opening {
		return nil, nil
	}
	o, err := postgres.TaxOpeningOf(ctx, exec, scope(ctx), "", "", "", c.ID)
	if err != nil || o == nil {
		return nil, err
	}
	return &api.TaxOpeningRef{OpeningID: o.ID, CutoffAt: o.CutoffAt, SourceDocument: o.SourceDocument, ExportReference: o.ExportReference,
		OrderReference: o.OrderReference, Position: o.Position}, nil
}
