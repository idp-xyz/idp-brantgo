package application

import (
	"context"
	"errors"
	"math/big"
	"slices"
	"sort"
	"strconv"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/internal/adapters/postgres"
	tr "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/taxrefund"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Tax refund cases (WBS S5-05b; CNX-AGG-TaxRefundCase, SM-CNX-TAXCASE, CNX-INV-004; CN-R04, CN-R08,
// CN-R09, CN-E01, CN-AT04, CN-AT10, CN-AT12). Upstreams, all read before CNX's transaction: FUL (R36,
// the export lot and its lines through the port Exports), POL (R50, the export_tax_policy revisions
// and the currency revision of an amount), EVD (R54, the sources of a result) and STL (R38,
// FundRecordRef v1 of a received refund, through the port Funds). Inside the transaction the case
// root is locked FOR UPDATE, then the invoice roots of the allocations FOR UPDATE in ID order, then
// the ledger's authority guard FOR SHARE. No upstream row is locked: R36 and R38 carry no guard,
// and the policy binding needs none (see policyAt).

// policyAt asks POL's ConfigAt v1 (R50; S5R-D23, POL-INV-007) which export_tax_policy revision of policyID applies at
// instant at, the export date (CN-AT12; 财税〔2012〕39号: the rate follows the 出口日期 on the declaration): the revision whose
// validity contains it, whenever it was published, the latest published when several do. Before CNX's transaction; it
// needs an active grant only. Nil with POL's rejection at /args/policy_id when POL has no such configuration
// (REFERENCE_INVALID) or none applies then (CONFIG_NOT_ACTIVE).
//
// A published revision never changes, so the binding needs no guard lock; but a revision published later whose
// validity also contains the export date would answer instead: policyStillAt asks again inside the transaction, and
// an expected case shows the newer answer as policy_superseded (tax_refund_case.get) and may rebind it.
func (s Service) policyAt(ctx context.Context, policyID string, at time.Time) (*polapi.RevisionAt, error) {
	const path = "/args/policy_id"
	r, err := s.Upstream.Configs.ConfigAt(ctx, polapi.ConfigAtQuery{Kind: polapi.KindExportTaxPolicy, ConfigID: policyID, At: at})
	if err != nil {
		return nil, err
	}
	if err = r.Check(path); err != nil {
		return nil, err
	}
	if r.Data.ExportTaxPolicy == nil {
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, path, "")
	}
	return &r, nil
}

// configAt runs POL's ConfigAt v1 SQL function with CNX's executor inside CNX's transaction or read (no lock).
func configAt(ctx context.Context, exec bentopg.Executor, policyID string, at time.Time) (polapi.ConfigAtVerdict, error) {
	q := polapi.ConfigAtQuery{Kind: polapi.KindExportTaxPolicy, ConfigID: policyID, At: kernel.Instant(at)}
	return polapi.ScanConfigAt(q, exec.QueryRow(ctx, polapi.ConfigAtSQL, polapi.ConfigAtArgs(q)...).Scan)
}

// policyStillAt checks inside CNX's transaction that ConfigAt v1 still answers revision r for instant at: a revision
// published since that also applies then is REFERENCE_STALE at path (send again).
func policyStillAt(ctx context.Context, exec bentopg.Executor, r *polapi.RevisionAt, at time.Time, path string) error {
	v, err := configAt(ctx, exec, r.Ref.ConfigID, at)
	if err != nil {
		return err
	}
	if err = v.Check(path); err != nil {
		return err
	}
	if v.Ref.Revision != r.Ref.Revision || v.ContentHash != r.ContentHash {
		return api.Reject(api.CodeReferenceStale, path, "Revision "+strconv.FormatInt(v.Ref.Revision, 10)+
			" of the export tax policy applies at the export date now: send again")
	}
	return nil
}

// bindingOf is the binding of revision r.
func bindingOf(r *polapi.RevisionAt) tr.Binding {
	return tr.Binding{PolicyID: r.Ref.ConfigID, Revision: r.Ref.Revision, ContentHash: r.ContentHash, RefundMethod: r.Data.ExportTaxPolicy.RefundMethod,
		ValidFrom: kernel.Instant(r.ValidFrom), ValidUntil: r.ValidUntil}
}

// tax_refund_case.allocate (human or AI; tax specialist). Before CNX's transaction: the export lot
// and its lines (FUL, port Exports), the lot's export date — the 出口日期 customs states on its
// declaration, which CNX records itself (S5 C3a; not FUL's departure milestone, CN-R07) — and the
// export_tax_policy revision ConfigAt v1 answers for it (POL, policyAt; S5R-D23). In the transaction, after the lot's
// case FOR UPDATE, the invoices FOR UPDATE in ID order and the lot's declaration case FOR SHARE, the
// refusals in this order: a lot FUL does not resolve or whose record is not intact
// (REFERENCE_INVALID), a cancelled lot or one without a declared export date (INVALID_STATE), an
// export date that changed since it was read (REFERENCE_STALE: send again), an
// allocation ID taken (BUSINESS_KEY_CONFLICT), the case's policy other than policy_id
// (REFERENCE_STALE), for a new case the policy binding (REFERENCE_INVALID, CONFIG_NOT_ACTIVE), a taxable
// revision (INVALID_STATE: a taxable export opens no case, S5R-D14), a revision published since that applies at
// the export date now (REFERENCE_STALE: send again), then the domain's (INVALID_STATE, REFERENCE_INVALID,
// INVALID_KNOWN_VALUE, TAX_EVIDENCE_EXCEEDED). The lot's first allocation, or the first after its case was rejected,
// opens the case. TaxEvidenceAllocated is published.
type allocateTaxEvidence struct {
	s         Service
	cmd       api.AllocateTaxEvidence
	lot       *Export
	exported  lotExport // as read before the transaction, for the policy binding
	export    lotExport // as read in the transaction, under the declaration's FOR SHARE lock
	policy    *polapi.RevisionAt
	policyErr error
	taxCase   *tr.Case
}

func (h *allocateTaxEvidence) Prepare(ctx context.Context) error {
	h.lot, h.policy, h.policyErr = nil, nil, nil
	ref := ExportRef{Kind: api.TargetExportLot, ID: h.cmd.ExportLotID}
	exports, err := h.s.Upstream.Exports.ResolveExports(ctx, []ExportRef{ref})
	if err != nil {
		return err
	}
	x, ok := exports[ref]
	if !ok || x.Code != "" {
		if ok {
			h.lot = &x
		}
		return nil
	}
	h.lot = &x
	if err = h.s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		var err error
		h.exported, err = exportOfLot(ctx, exec, scope(ctx), h.cmd.ExportLotID, postgres.NoLock)
		return err
	}); err != nil || h.exported.ExportedAt.IsZero() {
		return err
	}
	h.policy, err = h.s.policyAt(ctx, h.cmd.PolicyID, kernel.Instant(h.exported.ExportedAt))
	if _, ok := kernel.AsRejection(err); ok {
		h.policyErr, err = err, nil
	}
	return err
}

func (h *allocateTaxEvidence) invoiceIDs() []string {
	var ids []string
	for _, a := range h.cmd.Allocations {
		if !slices.Contains(ids, a.PurchaseInvoiceID) {
			ids = append(ids, a.PurchaseInvoiceID)
		}
	}
	sort.Strings(ids)
	return ids
}

func (h *allocateTaxEvidence) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	sc := scope(ctx)
	if h.taxCase, err = postgres.CaseOfLot(ctx, exec, sc, h.cmd.ExportLotID, postgres.ForUpdate); err != nil {
		return err
	}
	if err = postgres.LockInvoices(ctx, exec, sc, h.invoiceIDs()); err != nil {
		return err
	}
	// The export date is the declaration's (S5 C3a): its case root is held FOR SHARE so no receipt
	// changes it before this allocation commits.
	h.export, err = exportOfLot(ctx, exec, sc, h.cmd.ExportLotID, postgres.ForShare)
	return err
}

func (h *allocateTaxEvidence) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, cmd, lot := scope(ctx), h.cmd, h.lot
	const lotPath = "/args/export_lot_id"
	now := kernel.Instant(h.s.Clock.Now())
	switch {
	case lot == nil:
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, lotPath, "FUL resolves no such export lot")
	case lot.Code != "":
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, lotPath, "FUL's record of the export lot is not intact")
	case lot.Status == ExportLotCancelled:
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, lotPath, "The export lot is cancelled")
	case h.export.ExportedAt.IsZero() || kernel.Instant(h.export.ExportedAt).After(now):
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, lotPath, "No declaration of the export lot states customs' export date yet")
	case h.export != h.exported:
		return action.Outcome{}, api.Reject(api.CodeReferenceStale, lotPath, "The declaration's export date changed: send the allocation again")
	}
	for i, a := range cmd.Allocations {
		taken, err := postgres.TaxAllocationExists(ctx, exec, sc, a.AllocationID)
		if err != nil {
			return action.Outcome{}, err
		}
		if taken {
			return action.Outcome{}, api.Reject(api.CodeBusinessKeyConflict, "/args/allocations/"+strconv.Itoa(i)+"/allocation_id", "The allocation ID is taken")
		}
	}
	c := h.taxCase
	opened := c == nil || c.Status == tr.Rejected
	if !opened && c.Policy.PolicyID != cmd.PolicyID {
		return action.Outcome{}, api.Reject(api.CodeReferenceStale, "/args/policy_id", "The case binds export tax policy "+c.Policy.PolicyID)
	}
	if opened {
		if h.policyErr != nil {
			return action.Outcome{}, h.policyErr
		}
		if err := tr.CheckOpens(h.policy.Data.ExportTaxPolicy.RefundMethod, "/args/policy_id"); err != nil {
			return action.Outcome{}, err
		}
		if err := policyStillAt(ctx, exec, h.policy, h.export.ExportedAt, "/args/policy_id"); err != nil {
			return action.Outcome{}, err
		}
		attempt := 1
		if c != nil {
			attempt = c.Attempt + 1
		}
		c = &tr.Case{ID: action.NewID("trc", now), ExportLotID: cmd.ExportLotID, Attempt: attempt, OrderID: lot.OrderID,
			ExportedAt: kernel.Instant(h.export.ExportedAt), Policy: bindingOf(h.policy), Status: tr.Expected, RowVersion: 1, OpenedBy: d.Actor,
			Epoch: d.Epoch, OpenedAt: now}
	}
	var existing []*tr.Allocation
	var err error
	if !opened {
		if existing, err = postgres.CaseAllocations(ctx, exec, sc, c); err != nil {
			return action.Outcome{}, err
		}
	}
	var refs []tr.InvoiceLineRef
	var keys []tr.LineKey
	var in []tr.AllocateInput
	for _, a := range cmd.Allocations {
		q, _ := kernel.ParseDecimal(a.Quantity, kernel.QuantityFormat)
		in = append(in, tr.AllocateInput{ID: a.AllocationID, ExportLineID: a.ExportLineID, InvoiceID: a.PurchaseInvoiceID, LineNo: a.LineNo,
			Purpose: a.Purpose, Quantity: q, Replaces: a.Replaces})
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
	exportLines := map[string]tr.ExportLine{}
	for _, l := range lot.Lines {
		exportLines[l.LineID] = tr.ExportLine{LineID: l.LineID, ShipmentID: l.ShipmentID, OrderLineID: l.OrderLineID, SkuID: l.SkuID,
			SpecRevision: l.SpecRevision, Quantity: l.Quantity, Unit: l.Unit}
	}
	allocations, err := tr.Allocate(tr.Context{Case: c, Allocations: existing, ExportLines: exportLines, InvoiceLines: lines, Used: used,
		By: d.Actor, Epoch: d.Epoch, At: now}, in)
	if err != nil {
		return action.Outcome{}, err
	}
	if opened {
		err = postgres.InsertCase(ctx, exec, sc, c)
	} else {
		from := c.RowVersion
		c.RowVersion++
		err = postgres.TouchCase(ctx, exec, sc, c, from, now)
	}
	if err != nil {
		return action.Outcome{}, err
	}
	ids, invoices := []string{}, []string{}
	for _, a := range allocations {
		if err = postgres.InsertTaxAllocation(ctx, exec, sc, a); err != nil {
			return action.Outcome{}, err
		}
		ids = append(ids, a.ID)
		if !slices.Contains(invoices, a.InvoiceID) {
			invoices = append(invoices, a.InvoiceID)
		}
	}
	sort.Strings(invoices)
	return action.Outcome{ObjectID: c.ID, RowVersion: c.RowVersion, Status: c.Status, Events: []action.Event{{
		Type: api.EventTaxEvidenceAllocated, AggregateType: "TaxRefundCase", AggregateID: c.ID, AggregateVersion: c.RowVersion,
		Fields: map[string]any{"case_id": c.ID, "export_lot_id": c.ExportLotID, "attempt": c.Attempt, "order_id": c.OrderID,
			"policy_id": c.Policy.PolicyID, "policy_revision": c.Policy.Revision, "refund_method": c.Policy.RefundMethod, "allocation_ids": ids,
			"purchase_invoice_ids": invoices}}}}, nil
}

// AllocateTaxEvidence allocates purchase invoice lines to export lines (tax_refund_case.allocate).
func (s Service) AllocateTaxEvidence(ctx context.Context, cmd api.AllocateTaxEvidence) (api.TaxCaseReceipt, error) {
	h := &allocateTaxEvidence{s: s, cmd: cmd}
	var ids []string
	for _, a := range cmd.Allocations {
		ids = append(ids, a.AllocationID)
	}
	return s.runTaxCase(ctx, action.Command{Name: api.CommandAllocateTaxEvidence, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ExportLotID},
		cmd, cmd.Validate(), h, ids, nil)
}

// tax_refund_case.record_result (human only; tax specialist). Before CNX's transaction: the
// currency revision of the amount (POL), the evidence sources (EVD) and, for a received refund, the
// fund record (STL, port Funds). In the transaction, after the case FOR UPDATE (and, for a submission, its
// invoices FOR SHARE: S5R-D20, see postgres.LockCaseInvoices): NOT_FOUND, VERSION_CONFLICT, the currency
// (REFERENCE_INVALID, POL's verdict), the domain's INVALID_STATE (the stage does not follow; a plan without standing
// evidence or with evidence under review; a conversion to an exemption under a policy that refunds nothing, S5R-D14;
// an invoice's evidence rolled back in the case already, S5R-D20) and INVALID_KNOWN_VALUE (another currency than the
// case's; a rollback of an invoice the case's evidence does not use), the amounts' digits (INVALID_KNOWN_VALUE),
// the evidence (REFERENCE_INVALID), the fund record (REFERENCE_INVALID; not money in, in another
// currency or less than the amount: INVALID_KNOWN_VALUE). A rejection, the external result, returns
// the case's evidence to the invoice lines' balances. TaxRefundResultRecorded is published.
type recordTaxResult struct {
	s        Service
	cmd      api.RecordTaxResult
	currency *polapi.ResolvedConfig
	sources  map[string]evdapi.SourceRef
	funds    map[string]FundRecord
	taxCase  *tr.Case
	resultID string
}

func (h *recordTaxResult) Prepare(ctx context.Context) error {
	var err error
	h.currency, h.funds = nil, nil
	if c := h.cmd.Currency; c != nil {
		if h.currency, err = h.s.resolveCurrency(ctx, *c); err != nil {
			return err
		}
	}
	ids := slices.Clone(h.cmd.EvidenceIDs)
	if r := h.cmd.Review; r != nil {
		ids = append(ids, r.EvidenceIDs...)
	}
	if h.sources, err = h.s.sources(ctx, ids); err != nil {
		return err
	}
	if h.cmd.FundRecordID != "" {
		h.funds, err = h.s.Upstream.Funds.ResolveFundRecords(ctx, []string{h.cmd.FundRecordID})
	}
	return err
}

func (h *recordTaxResult) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	if h.taxCase, err = postgres.LoadCase(ctx, exec, scope(ctx), h.cmd.CaseID, postgres.ForUpdate); err != nil || h.taxCase == nil {
		return err
	}
	if h.cmd.Stage == tr.Submitted {
		return postgres.LockCaseInvoices(ctx, exec, scope(ctx), h.taxCase.ID)
	}
	return nil
}

func (h *recordTaxResult) evidence(ids []string, path string) ([]tr.Evidence, error) {
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

func (h *recordTaxResult) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, cmd, c := scope(ctx), h.cmd, h.taxCase
	if c == nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/case_id", "")
	}
	if err := tr.CheckVersion(c, cmd.ExpectedVersion); err != nil {
		return action.Outcome{}, err
	}
	now := kernel.Instant(h.s.Clock.Now())
	r := &tr.Result{ID: action.NewID("trr", now), CaseID: c.ID, Stage: cmd.Stage, From: c.Status, TaxPeriod: cmd.TaxPeriod,
		ExternalReference: cmd.ExternalReference, ResultText: cmd.ResultText, FundRecordID: cmd.FundRecordID, PurchaseInvoiceID: cmd.PurchaseInvoiceID,
		Reason: cmd.Reason, By: d.Actor, Epoch: d.Epoch, At: now}
	var kc kernel.Currency
	if cmd.Currency != nil {
		cur, err := currencyOf(h.currency, "/args/currency")
		if err != nil {
			return action.Outcome{}, err
		}
		r.Currency = &tr.Currency{ConfigID: cur.ConfigID, Revision: cur.Revision, Code: cur.Code, MinorUnits: cur.MinorUnits}
		if kc, err = cur.Kernel(); err != nil {
			return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/currency", "")
		}
	}
	results, err := postgres.TaxResults(ctx, exec, sc, c.ID)
	if err != nil {
		return action.Outcome{}, err
	}
	allocations, err := postgres.CaseAllocations(ctx, exec, sc, c)
	if err != nil {
		return action.Outcome{}, err
	}
	status, err := tr.Record(c, results, allocations, r)
	if err != nil {
		return action.Outcome{}, err
	}
	for _, x := range []struct {
		s, path string
		to      **kernel.Money
	}{{cmd.Amount, "/args/amount", &r.Amount}, {cmd.OffsetAmount, "/args/offset_amount", &r.Offset}} {
		if x.s == "" {
			continue
		}
		m, err := kernel.ParseAmount(kc, x.s)
		if err != nil {
			return action.Outcome{}, kernel.Reject(kernel.CodeInvalidKnownValue, x.path, "More digits than "+kc.Code()+" has minor units")
		}
		*x.to = &m
	}
	if r.Evidence, err = h.evidence(cmd.EvidenceIDs, "/args/evidence_ids"); err != nil {
		return action.Outcome{}, err
	}
	if rv := cmd.Review; rv != nil {
		r.Reviewer = rv.Reviewer
		if r.ReviewEvidence, err = h.evidence(rv.EvidenceIDs, "/args/review/evidence_ids"); err != nil {
			return action.Outcome{}, err
		}
	}
	if cmd.FundRecordID != "" {
		const fPath = "/args/fund_record_id"
		f, ok := h.funds[cmd.FundRecordID]
		received, _ := new(big.Int).SetString(f.ReceivedMinor, 10)
		switch {
		case !ok:
			return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, fPath, "STL resolves no such fund record")
		case f.Direction != FundIn:
			return action.Outcome{}, kernel.Reject(kernel.CodeInvalidKnownValue, fPath, "The fund record brings no money in")
		case f.Currency != r.Currency.Code:
			return action.Outcome{}, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/currency", "The fund record is in "+f.Currency)
		case received == nil || r.Amount.Minor().Cmp(received) > 0:
			return action.Outcome{}, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/amount", "More than the fund record received")
		}
	}
	from := c.RowVersion
	c.RowVersion++
	c.Status = status
	if err = postgres.InsertTaxResult(ctx, exec, sc, r); err == nil {
		err = postgres.TouchCase(ctx, exec, sc, c, from, now)
	}
	if err != nil {
		return action.Outcome{}, err
	}
	h.resultID = r.ID
	fields := map[string]any{"case_id": c.ID, "result_id": r.ID, "export_lot_id": c.ExportLotID, "stage": r.Stage, "from": r.From,
		"policy_id": c.Policy.PolicyID, "policy_revision": c.Policy.Revision}
	if r.FundRecordID != "" {
		fields["fund_record_id"] = r.FundRecordID
	}
	if r.PurchaseInvoiceID != "" {
		fields["purchase_invoice_id"] = r.PurchaseInvoiceID
	}
	return action.Outcome{ObjectID: c.ID, RowVersion: c.RowVersion, Status: c.Status, Events: []action.Event{{
		Type: api.EventTaxRefundResultRecord, AggregateType: "TaxRefundCase", AggregateID: c.ID, AggregateVersion: c.RowVersion,
		Fields: fields}}}, nil
}

// RecordTaxResult records a stage of a case's refund declaration (tax_refund_case.record_result).
func (s Service) RecordTaxResult(ctx context.Context, cmd api.RecordTaxResult) (api.TaxCaseReceipt, error) {
	h := &recordTaxResult{s: s, cmd: cmd}
	return s.runTaxCase(ctx, action.Command{Name: api.CommandRecordTaxResult, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.CaseID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h, nil, h)
}

func (s Service) runTaxCase(ctx context.Context, cmd action.Command, req api.Command, validation error, h action.Handler, allocations []string,
	result *recordTaxResult) (api.TaxCaseReceipt, error) {
	if validation != nil {
		if rej, ok := kernel.AsRejection(validation); ok {
			if err := s.Ledger.AuditRejected(ctx, cmd.Name, cmd.ObjectID, rej.Code); err != nil {
				return api.TaxCaseReceipt{}, errors.Join(validation, err)
			}
		}
		return api.TaxCaseReceipt{}, validation
	}
	_, _, args := request(req)
	hash, err := action.RequestHash(args)
	if err != nil {
		return api.TaxCaseReceipt{}, err
	}
	cmd.RequestHash = hash
	res, err := s.Ledger.Execute(ctx, cmd, h)
	if err != nil {
		return api.TaxCaseReceipt{}, err
	}
	out := api.TaxCaseReceipt{Receipt: api.Receipt(res), CaseID: res.ObjectID, AllocationIDs: allocations}
	if result != nil {
		out.ResultID = result.resultID
	}
	err = s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		c, err := postgres.LoadCase(ctx, exec, scope(ctx), res.ObjectID, postgres.NoLock)
		if err != nil || c == nil {
			return err
		}
		out.ExportLotID, out.Attempt, out.Policy = c.ExportLotID, c.Attempt, policyView(c.Policy)
		return nil
	})
	return out, err
}

func policyView(b tr.Binding) api.PolicyBindingView {
	return api.PolicyBindingView{PolicyID: b.PolicyID, Revision: b.Revision, ContentHash: b.ContentHash, RefundMethod: b.RefundMethod,
		ValidFrom: b.ValidFrom, ValidUntil: b.ValidUntil}
}
