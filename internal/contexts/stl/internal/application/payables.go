package application

import (
	"context"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/adapters/postgres"
	fr "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/fundrecord"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/settlement"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Payable adjustments and fund reversal (S5R unit R8; simulated sign-off E0 of 2026-11-13: S5R-D20, S5R-D22;
// STL-INV-013, STL-INV-014; SM-STL-FUND). A payable adjustment is an entity of its purchase statement: its commands
// lock the statement root FOR UPDATE (as credit notes do), then the ledger locks the authority guard; CNX's
// PurchaseInvoiceRef v1 (R55, no lock) is read before the transaction. fund.reverse locks the fund record FOR UPDATE —
// application.apply and every draw lock it too, so a reversal and an application of one fund commit one after the
// other ("冲销与核销并发按资金根锁裁决") — then an opening prepayment's item FOR UPDATE, then the authority guard.

// requestAdjustment runs payable_adjustment.request (STL-INV-013; human only, treasury).
type requestAdjustment struct {
	s         Service
	cmd       api.RequestPayableAdjustment
	evidence  map[string]bool
	invoices  invoiceFacts
	statement *settlement.Statement
	taken     bool
}

func (h *requestAdjustment) Prepare(ctx context.Context) error {
	var err error
	if h.evidence, err = h.s.resolveEvidence(ctx, h.cmd.EvidenceIDs); err != nil {
		return err
	}
	var st *settlement.Statement
	if err = h.s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		st, err = postgres.LoadStatement(ctx, exec, scope(ctx), h.cmd.StatementID, false)
		return err
	}); err != nil {
		return err
	}
	h.invoices, err = h.s.statementInvoices(ctx, st, h.cmd.PurchaseInvoiceID)
	return err
}

func (h *requestAdjustment) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	if h.statement, err = lockStatement(ctx, exec, h.cmd.StatementID); err != nil {
		return err
	}
	a, err := postgres.LoadAdjustment(ctx, exec, scope(ctx), h.cmd.AdjustmentID)
	h.taken = a != nil
	return err
}

func (h *requestAdjustment) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, c, st, now := scope(ctx), h.cmd, h.statement, kernel.Instant(h.s.Clock.Now())
	switch {
	case h.taken:
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/adjustment_id", "The adjustment ID is taken")
	case st == nil || !d.Sees(st.OwnerActor, st.OwnerTeam):
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/statement_id", "")
	}
	if err := evidenceFound(h.evidence, c.EvidenceIDs, "/args/evidence_ids"); err != nil {
		return action.Outcome{}, err
	}
	amount, err := amountIn(st.Currency(), c.Amount, "/args/amount")
	if err != nil {
		return action.Outcome{}, err
	}
	if st.Kind == settlement.KindPurchase && st.Status == settlement.StatusConfirmed {
		if err = adjustmentInvoice(st, c.AdjustmentKind, c.PurchaseInvoiceID, h.invoices); err != nil {
			return action.Outcome{}, err
		}
	}
	applied, err := postgres.InstallmentApplied(ctx, exec, sc, st)
	if err != nil {
		return action.Outcome{}, err
	}
	a, err := settlement.RequestAdjustment(settlement.AdjustmentInput{ID: c.AdjustmentID, Statement: st, Kind: c.AdjustmentKind, Amount: amount,
		PurchaseInvoiceID: c.PurchaseInvoiceID, Basis: c.Basis, EvidenceIDs: c.EvidenceIDs, Applied: applied, By: fundAct(d, now, ""), Team: d.Team})
	if err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.InsertAdjustment(ctx, exec, sc, a); err != nil {
		return action.Outcome{}, err
	}
	from := st.Version
	st.Version++
	if err = postgres.BumpStatement(ctx, exec, sc, st, from, now); err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: a.ID, RowVersion: a.Version, Status: a.Status}, nil
}

// RequestPayableAdjustment requests an adjustment of a purchase statement's payable (payable_adjustment.request).
// Human only.
func (s Service) RequestPayableAdjustment(ctx context.Context, cmd api.RequestPayableAdjustment) (api.PayableAdjustmentReceipt, error) {
	h := &requestAdjustment{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandRequestPayableAdjustment, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.AdjustmentID},
		cmd, cmd.Validate(), h)
	if err != nil {
		return api.PayableAdjustmentReceipt{}, err
	}
	return s.adjustmentReceipt(ctx, res)
}

// confirmAdjustment runs payable_adjustment.confirm (STL-INV-013; human only, independent of the requester).
type confirmAdjustment struct {
	s          Service
	cmd        api.ConfirmPayableAdjustment
	adjustment *settlement.PayableAdjustment
	statement  *settlement.Statement
}

func (h *confirmAdjustment) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.adjustment, h.statement = nil, nil
	a, err := postgres.LoadAdjustment(ctx, exec, scope(ctx), h.cmd.AdjustmentID)
	if err != nil || a == nil {
		return err
	}
	if h.statement, err = lockStatement(ctx, exec, a.StatementID); err != nil {
		return err
	}
	h.adjustment, err = postgres.LoadAdjustment(ctx, exec, scope(ctx), h.cmd.AdjustmentID)
	return err
}

func (h *confirmAdjustment) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, c, a, st, now := scope(ctx), h.cmd, h.adjustment, h.statement, kernel.Instant(h.s.Clock.Now())
	switch {
	case a == nil || st == nil || !d.Sees(a.OwnerActor, a.OwnerTeam):
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/adjustment_id", "")
	case a.Version != c.ExpectedVersion:
		return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	applied, err := postgres.InstallmentApplied(ctx, exec, sc, st)
	if err != nil {
		return action.Outcome{}, err
	}
	from := a.Version
	if err = a.Decide(c.Decision, st, st.Open(applied), fundAct(d, now, c.Reason)); err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.SaveAdjustmentDecision(ctx, exec, sc, a, from); err != nil {
		return action.Outcome{}, err
	}
	sv := st.Version
	st.Version++
	if err = postgres.BumpStatement(ctx, exec, sc, st, sv, now); err != nil {
		return action.Outcome{}, err
	}
	out := action.Outcome{ObjectID: a.ID, RowVersion: a.Version, Status: a.Status}
	if a.Status == settlement.AdjustmentConfirmed {
		out.Events = []action.Event{statementEvent(api.EventPayableAdjustmentConfirmed, st, map[string]any{"adjustment_id": a.ID, "kind": a.Kind,
			"amount_minor": a.Amount.MinorString(), "currency": a.Amount.Currency().Code(), "purchase_invoice_id": a.PurchaseInvoiceID})}
	}
	return out, nil
}

// ConfirmPayableAdjustment confirms or rejects a requested payable adjustment (payable_adjustment.confirm). Human only.
func (s Service) ConfirmPayableAdjustment(ctx context.Context, cmd api.ConfirmPayableAdjustment) (api.PayableAdjustmentReceipt, error) {
	h := &confirmAdjustment{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandConfirmPayableAdjustment, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.AdjustmentID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
	if err != nil {
		return api.PayableAdjustmentReceipt{}, err
	}
	return s.adjustmentReceipt(ctx, res)
}

func (s Service) adjustmentReceipt(ctx context.Context, res api.Receipt) (api.PayableAdjustmentReceipt, error) {
	out := api.PayableAdjustmentReceipt{Receipt: res}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		sc := scope(ctx)
		a, err := postgres.LoadAdjustment(ctx, exec, sc, res.ObjectID)
		if err != nil || a == nil {
			return err
		}
		st, err := postgres.LoadStatement(ctx, exec, sc, a.StatementID, false)
		if err != nil || st == nil {
			return err
		}
		applied, err := postgres.InstallmentApplied(ctx, exec, sc, st)
		out.AdjustmentKind, out.StatementID, out.StatementVersion, out.PurchaseInvoiceID = a.Kind, a.StatementID, st.Version, a.PurchaseInvoiceID
		out.Currency, out.AmountMinor, out.StatementOpenMinor = a.Amount.Currency().Code(), a.Amount.MinorString(), balanceOpen(st, applied).MinorString()
		return err
	})
	return out, err
}

// adjustmentViews are the payable adjustments of statement st that d sees.
func adjustmentViews(list []settlement.PayableAdjustment, st *settlement.Statement, d authorization.Decision) []api.PayableAdjustmentView {
	out := []api.PayableAdjustmentView{}
	for i := range list {
		a := &list[i]
		if !d.Sees(a.OwnerActor, a.OwnerTeam) {
			continue
		}
		net, tax := a.Split(st)
		v := api.PayableAdjustmentView{AdjustmentID: a.ID, AdjustmentKind: a.Kind, Status: a.Status, Version: a.Version,
			Currency: a.Amount.Currency().Code(), AmountMinor: a.Amount.MinorString(), NetMinor: net.MinorString(), TaxMinor: tax.MinorString(),
			PurchaseInvoiceID: a.PurchaseInvoiceID, Basis: a.Basis, EvidenceIDs: a.EvidenceIDs, Effective: a.Effective(st),
			RequestedBy: a.Requested.By, RequestedAt: a.Requested.At}
		v.DecidedBy, v.DecidedAt, v.DecisionReason = actOf(a.Decided)
		out = append(out, v)
	}
	return out
}

// reverseFund runs fund.reverse (S5R-D22, STL-INV-014; human only, credit manager, independent of the recorder).
type reverseFund struct {
	s        Service
	cmd      api.ReverseFund
	evidence map[string]bool
	fund     *fr.FundRecord
	opening  *settlement.Opening
}

func (h *reverseFund) Prepare(ctx context.Context) error {
	if len(h.cmd.EvidenceIDs) == 0 {
		return nil
	}
	var err error
	h.evidence, err = h.s.resolveEvidence(ctx, h.cmd.EvidenceIDs)
	return err
}

func (h *reverseFund) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.fund, h.opening = nil, nil
	sc := scope(ctx)
	var err error
	if h.fund, err = postgres.LoadFund(ctx, exec, sc, h.cmd.FundRecordID, true); err != nil || h.fund == nil {
		return err
	}
	// An opening prepayment's item (its ID is the fund record's) is reversed with it.
	h.opening, err = postgres.LoadOpening(ctx, exec, sc, h.fund.ID, true)
	if h.opening != nil && h.opening.FundRecordID != h.fund.ID {
		h.opening = nil
	}
	return err
}

func (h *reverseFund) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, c, f, now := scope(ctx), h.cmd, h.fund, kernel.Instant(h.s.Clock.Now())
	switch {
	case f == nil || !d.Sees(f.OwnerActor, f.OwnerTeam):
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/fund_record_id", "")
	case f.Version != c.ExpectedVersion:
		return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	if err := evidenceFound(h.evidence, c.EvidenceIDs, "/args/evidence_ids"); err != nil {
		return action.Outcome{}, err
	}
	apps, err := postgres.Applications(ctx, exec, sc, f.ID, "")
	if err != nil {
		return action.Outcome{}, err
	}
	var standing []string
	for i := range apps {
		if apps[i].Stands() {
			standing = append(standing, apps[i].ID)
		}
	}
	draws, err := postgres.DrawIDs(ctx, exec, sc, f.ID)
	if err != nil {
		return action.Outcome{}, err
	}
	from := f.Version
	if err = f.Reverse(standing, draws, c.EvidenceIDs, fundAct(d, now, c.Reason)); err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.SaveFundReversal(ctx, exec, sc, f, from); err != nil {
		return action.Outcome{}, err
	}
	if o := h.opening; o != nil && o.Status == settlement.OpeningRecorded {
		if err = o.ReverseOpening(now); err != nil {
			return action.Outcome{}, err
		}
		if err = postgres.SaveOpeningReversed(ctx, exec, sc, o); err != nil {
			return action.Outcome{}, err
		}
	}
	e := fundEvent(api.EventFundRecordReversed, f, map[string]any{"kind": f.Kind, "direction": f.Direction, "reason": c.Reason})
	return action.Outcome{ObjectID: f.ID, RowVersion: f.Version, Status: f.Status, Events: []action.Event{e}}, nil
}

// ReverseFund reverses a fund record (fund.reverse). Human only, not the recorder.
func (s Service) ReverseFund(ctx context.Context, cmd api.ReverseFund) (api.FundRecordReceipt, error) {
	h := &reverseFund{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandReverseFund, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.FundRecordID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
	if err != nil {
		return api.FundRecordReceipt{}, err
	}
	return s.fundReceipt(ctx, res)
}
