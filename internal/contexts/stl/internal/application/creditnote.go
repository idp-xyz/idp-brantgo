package application

import (
	"context"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/adapters/postgres"
	fa "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/application"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/settlement"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Credit notes and settlement reversal (S5 unit C8, WBS S5-18; SM-STL-CREDIT-NOTE, SM-STL-SETTLEMENT;
// STL-INV-008, STL-INV-009). A credit note and a reversal are entities of the SettlementStatement aggregate:
// credit_note.request, credit.confirm and settlement.reverse lock the statement root FOR UPDATE — with, for a
// sales statement, its order account, in ID order (tactical lockOrder "资金记录根与订单账户或结算单根按 ID 排序"),
// as every application to the statement does — then the ledger locks the authority guard; each moves the
// statement's version by one (row_version CAS root). Applications to the statement lock the same roots, so a
// reversal and an application of one statement commit one after the other and the second sees the first
// ("冲销与核销并发按结算单根锁裁决"). No upstream row is locked: EVD's sources are read before the transaction.

// lockStatement locks statement id and (sales) its order account FOR UPDATE in ID order and returns the
// statement read under its lock, or nil.
func lockStatement(ctx context.Context, exec bentopg.Executor, id string) (*settlement.Statement, error) {
	st, err := postgres.LoadStatement(ctx, exec, scope(ctx), id, false)
	if err != nil || st == nil {
		return nil, err
	}
	_, locked, err := lockStatementRoots(ctx, exec, "", st)
	return locked, err
}

// statementEvent is an Outbox event of the SettlementStatement aggregate at statement st's version.
func statementEvent(typ string, st *settlement.Statement, fields map[string]any) action.Event {
	fields["statement_id"], fields["statement_version"] = st.ID, st.Version
	return action.Event{Type: typ, AggregateType: "SettlementStatement", AggregateID: st.ID, AggregateVersion: st.Version, Fields: fields}
}

// creditState is what the credit room of statement st is read from under its lock.
type creditState struct {
	applied     map[int]kernel.Money
	notes       []settlement.CreditNote
	noteApplied map[string]kernel.Money
}

func readCredits(ctx context.Context, exec bentopg.Executor, st *settlement.Statement) (creditState, error) {
	sc := scope(ctx)
	var cs creditState
	var err error
	if cs.applied, err = postgres.InstallmentApplied(ctx, exec, sc, st); err != nil {
		return cs, err
	}
	if cs.notes, err = postgres.CreditNotes(ctx, exec, sc, st.ID, ""); err != nil {
		return cs, err
	}
	cs.noteApplied, err = postgres.CreditNoteApplied(ctx, exec, sc, st.ID, st.Currency())
	return cs, err
}

// requestCredit runs credit_note.request (STL-INV-008; human or AI).
type requestCredit struct {
	s         Service
	cmd       api.RequestCreditNote
	evidence  map[string]bool
	statement *settlement.Statement
	taken     bool
}

func (h *requestCredit) Prepare(ctx context.Context) error {
	var err error
	h.evidence, err = h.s.resolveEvidence(ctx, h.cmd.EvidenceIDs)
	return err
}

func (h *requestCredit) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	if h.statement, err = lockStatement(ctx, exec, h.cmd.StatementID); err != nil {
		return err
	}
	n, err := postgres.LoadCreditNote(ctx, exec, scope(ctx), h.cmd.CreditNoteID)
	h.taken = n != nil
	return err
}

func (h *requestCredit) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, c, st, now := scope(ctx), h.cmd, h.statement, kernel.Instant(h.s.Clock.Now())
	switch {
	case h.taken:
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/credit_note_id", "The credit note ID is taken")
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
	cs, err := readCredits(ctx, exec, st)
	if err != nil {
		return action.Outcome{}, err
	}
	n, err := settlement.RequestCredit(settlement.CreditInput{ID: c.CreditNoteID, Statement: st, OrderID: c.OrderID, Kind: c.CreditKind,
		Amount: amount, Basis: c.Basis, EvidenceIDs: c.EvidenceIDs, Applied: cs.applied, Notes: cs.notes, NoteApplied: cs.noteApplied,
		By: fundAct(d, now, ""), Team: d.Team})
	if err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.InsertCreditNote(ctx, exec, sc, n); err != nil {
		return action.Outcome{}, err
	}
	from := st.Version
	st.Version++
	if err = postgres.BumpStatement(ctx, exec, sc, st, from, now); err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: n.ID, RowVersion: n.Version, Status: n.Status}, nil
}

// RequestCreditNote requests a credit note on a sales statement's receivable (credit_note.request).
func (s Service) RequestCreditNote(ctx context.Context, cmd api.RequestCreditNote) (api.CreditNoteReceipt, error) {
	h := &requestCredit{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandRequestCreditNote, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.CreditNoteID},
		cmd, cmd.Validate(), h)
	if err != nil {
		return api.CreditNoteReceipt{}, err
	}
	return s.creditNoteReceipt(ctx, res)
}

// confirmCredit runs credit.confirm (SM-STL-CREDIT-NOTE; human only, independent of the requester).
type confirmCredit struct {
	s         Service
	cmd       api.ConfirmCredit
	note      *settlement.CreditNote
	statement *settlement.Statement
}

func (h *confirmCredit) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.note, h.statement = nil, nil
	n, err := postgres.LoadCreditNote(ctx, exec, scope(ctx), h.cmd.CreditNoteID)
	if err != nil || n == nil {
		return err
	}
	if h.statement, err = lockStatement(ctx, exec, n.StatementID); err != nil {
		return err
	}
	h.note, err = postgres.LoadCreditNote(ctx, exec, scope(ctx), h.cmd.CreditNoteID)
	return err
}

func (h *confirmCredit) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, c, n, st, now := scope(ctx), h.cmd, h.note, h.statement, kernel.Instant(h.s.Clock.Now())
	switch {
	case n == nil || st == nil || !d.Sees(n.OwnerActor, n.OwnerTeam):
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/credit_note_id", "")
	case n.Version != c.ExpectedVersion:
		return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	cs, err := readCredits(ctx, exec, st)
	if err != nil {
		return action.Outcome{}, err
	}
	from := n.Version
	if err = n.Decide(c.Decision, st, settlement.CreditRoom(st, cs.applied, cs.notes, cs.noteApplied, n.ID), fundAct(d, now, c.Reason)); err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.SaveCreditDecision(ctx, exec, sc, n, from); err != nil {
		return action.Outcome{}, err
	}
	sv := st.Version
	st.Version++
	if err = postgres.BumpStatement(ctx, exec, sc, st, sv, now); err != nil {
		return action.Outcome{}, err
	}
	out := action.Outcome{ObjectID: n.ID, RowVersion: n.Version, Status: n.Status}
	if n.Status == settlement.CreditConfirmed {
		out.Events = []action.Event{statementEvent(api.EventCreditConfirmed, st, map[string]any{"credit_note_id": n.ID,
			"credit_note_version": n.Version, "kind": n.Kind, "order_id": n.OrderID, "counterparty_id": st.CounterpartyID})}
	}
	return out, nil
}

// ConfirmCredit confirms or rejects a requested credit note (credit.confirm). Human only.
func (s Service) ConfirmCredit(ctx context.Context, cmd api.ConfirmCredit) (api.CreditNoteReceipt, error) {
	h := &confirmCredit{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandConfirmCredit, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.CreditNoteID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
	if err != nil {
		return api.CreditNoteReceipt{}, err
	}
	return s.creditNoteReceipt(ctx, res)
}

func (s Service) creditNoteReceipt(ctx context.Context, res api.Receipt) (api.CreditNoteReceipt, error) {
	out := api.CreditNoteReceipt{Receipt: res}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		sc := scope(ctx)
		n, err := postgres.LoadCreditNote(ctx, exec, sc, res.ObjectID)
		if err != nil || n == nil {
			return err
		}
		st, err := postgres.LoadStatement(ctx, exec, sc, n.StatementID, false)
		if err != nil || st == nil {
			return err
		}
		applied, err := postgres.InstallmentApplied(ctx, exec, sc, st)
		out.CreditKind, out.StatementID, out.StatementVersion, out.OrderID = n.Kind, n.StatementID, st.Version, n.OrderID
		out.Currency, out.AmountMinor, out.StatementOpenMinor = n.Amount.Currency().Code(), n.Amount.MinorString(), balanceOpen(st, applied).MinorString()
		return err
	})
	return out, err
}

// reverseSettlement runs settlement.reverse (SM-STL-SETTLEMENT, STL-INV-009; human only, independent of
// the confirmer).
type reverseSettlement struct {
	s         Service
	cmd       api.ReverseSettlement
	evidence  map[string]bool
	statement *settlement.Statement
}

func (h *reverseSettlement) Prepare(ctx context.Context) error {
	if len(h.cmd.EvidenceIDs) == 0 {
		return nil
	}
	var err error
	h.evidence, err = h.s.resolveEvidence(ctx, h.cmd.EvidenceIDs)
	return err
}

func (h *reverseSettlement) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.statement, err = lockStatement(ctx, exec, h.cmd.StatementID)
	return err
}

func (h *reverseSettlement) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, c, st, now := scope(ctx), h.cmd, h.statement, kernel.Instant(h.s.Clock.Now())
	switch {
	case st == nil || !d.Sees(st.OwnerActor, st.OwnerTeam):
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/statement_id", "")
	case st.Version != c.ExpectedVersion:
		return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	if err := evidenceFound(h.evidence, c.EvidenceIDs, "/args/evidence_ids"); err != nil {
		return action.Outcome{}, err
	}
	apps, err := postgres.StatementApplications(ctx, exec, sc, st.ID)
	if err != nil {
		return action.Outcome{}, err
	}
	var standing, pending []string
	for i := range apps {
		if apps[i].Stands() {
			standing = append(standing, apps[i].ID)
		}
	}
	notes, err := postgres.CreditNotes(ctx, exec, sc, st.ID, "")
	if err != nil {
		return action.Outcome{}, err
	}
	for _, n := range notes {
		if n.Status == settlement.CreditRequested {
			pending = append(pending, n.ID)
		}
	}
	r := settlement.Reversal{Reason: c.Reason, EvidenceIDs: c.EvidenceIDs, By: fundAct(d, now, c.Reason)}
	from := st.Version
	if err = st.Reverse(standing, pending, r); err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.BumpStatement(ctx, exec, sc, st, from, now); err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.SaveReversal(ctx, exec, sc, st, r); err != nil {
		return action.Outcome{}, err
	}
	if err = reverseOpening(ctx, exec, st, now); err != nil { // C10: an opening statement's item is reversed with it
		return action.Outcome{}, err
	}
	sources := make([]map[string]any, 0, len(st.Lines))
	for _, l := range st.Lines {
		sources = append(sources, map[string]any{"source_kind": l.Source.Kind, "source_id": l.Source.ID, "source_line_id": l.Source.LineID,
			"source_version": l.SourceVersion})
	}
	fields := map[string]any{"kind": st.Kind, "counterparty_id": st.CounterpartyID, "sources": sources}
	if st.OrderID != "" {
		fields["order_id"], fields["order_revision"] = st.OrderID, st.OrderRevision
	}
	return action.Outcome{ObjectID: st.ID, RowVersion: st.Version, Status: st.Status,
		Events: []action.Event{statementEvent(api.EventSettlementReversed, st, fields)}}, nil
}

// ReverseSettlement reverses a settlement statement (settlement.reverse). Human only.
func (s Service) ReverseSettlement(ctx context.Context, cmd api.ReverseSettlement) (api.ReversalReceipt, error) {
	h := &reverseSettlement{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandReverseSettlement, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.StatementID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
	if err != nil {
		return api.ReversalReceipt{}, err
	}
	out := api.ReversalReceipt{Receipt: res, Sources: []api.SettlementSourceRef{}}
	err = s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		st, err := postgres.LoadStatement(ctx, exec, scope(ctx), res.ObjectID, false)
		if err != nil || st == nil {
			return err
		}
		out.StatementKind, out.OrderID = st.Kind, st.OrderID
		for _, l := range st.Lines {
			ref := api.SettlementSourceRef{LineID: l.Source.LineID}
			if l.Source.Kind == settlement.SourceShipmentLine {
				ref.ShipmentID = l.Source.ID
			} else {
				ref.DeliveryID, ref.Version = l.Source.ID, l.SourceVersion
			}
			out.Sources = append(out.Sources, ref)
		}
		return nil
	})
	return out, err
}

// applyCreditNote runs application.apply of a confirmed credit note to its statement's receivable
// (STL-INV-008; human only): inside the transaction it locks the statement and its order account FOR UPDATE
// in ID order — the credit note is the statement's entity, there is no fund record — and derives the credit
// note's rest and the installment's open amount from the immutable applications under those locks.
type applyCreditNote struct {
	s         Service
	cmd       api.Apply
	note      *settlement.CreditNote
	statement *settlement.Statement
	taken     bool
}

func (h *applyCreditNote) Lock(ctx context.Context, exec bentopg.Executor) error {
	sc := scope(ctx)
	h.note, h.statement, h.taken = nil, nil, false
	var err error
	if h.statement, err = lockStatement(ctx, exec, h.cmd.Target.StatementID); err != nil {
		return err
	}
	if h.note, err = postgres.LoadCreditNote(ctx, exec, sc, h.cmd.CreditNoteID); err != nil {
		return err
	}
	x, err := postgres.LoadApplication(ctx, exec, sc, h.cmd.ApplicationID, false)
	h.taken = x != nil
	return err
}

func (h *applyCreditNote) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, c, n, st, now := scope(ctx), h.cmd, h.note, h.statement, kernel.Instant(h.s.Clock.Now())
	switch {
	case h.taken:
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/application_id", "The application ID is taken")
	case n == nil || !d.Sees(n.OwnerActor, n.OwnerTeam):
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/credit_note_id", "")
	case st == nil || !d.Sees(st.OwnerActor, st.OwnerTeam):
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/target/statement_id", "")
	}
	amount, err := amountIn(n.Amount.Currency(), c.Amount, "/args/amount")
	if err != nil {
		return action.Outcome{}, err
	}
	applied, err := postgres.InstallmentApplied(ctx, exec, sc, st)
	if err != nil {
		return action.Outcome{}, err
	}
	noteApplied, err := postgres.CreditNoteApplied(ctx, exec, sc, n.StatementID, n.Amount.Currency())
	if err != nil {
		return action.Outcome{}, err
	}
	used, ok := noteApplied[n.ID]
	if !ok {
		used = zeroIn(n.Amount.Currency())
	}
	x, err := fa.ApplyCredit(fa.CreditInput{ID: c.ApplicationID, Note: n, NoteApplied: used, Statement: st, TargetKind: c.Target.Kind,
		StageID: c.Target.StageID, Applied: applied, Amount: amount, By: fundAct(d, now, ""), Team: d.Team})
	if err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.InsertApplication(ctx, exec, sc, x); err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: x.ID, RowVersion: x.Version, Status: x.Status, Events: []action.Event{appEvent(api.EventApplied, x)}}, nil
}

// creditNoteRef is credit note n of statement st as CreditNoteRef v1 states it, with the applications d sees.
func creditNoteRef(ctx context.Context, exec bentopg.Executor, n *settlement.CreditNote, st *settlement.Statement, d authorization.Decision) (api.CreditNoteRef, error) {
	apps, err := postgres.CreditNoteApplications(ctx, exec, scope(ctx), n.ID)
	if err != nil {
		return api.CreditNoteRef{}, err
	}
	used := zeroIn(n.Amount.Currency())
	r := api.CreditNoteRef{CreditNoteID: n.ID, CreditKind: n.Kind, Status: n.Status, Version: n.Version, StatementID: n.StatementID,
		StatementStatus: st.Status, OrderID: n.OrderID, CounterpartyID: st.CounterpartyID, Currency: n.Amount.Currency().Code(),
		AmountMinor: n.Amount.MinorString(), Effective: n.Effective(st), Basis: n.Basis, EvidenceIDs: n.EvidenceIDs,
		Applications: []api.AllocationView{}, Sources: []api.SettlementSourceRef{}, RequestedBy: n.Requested.By, RequestedAt: n.Requested.At}
	for _, l := range st.Lines {
		r.Sources = append(r.Sources, api.SettlementSourceRef{ShipmentID: l.Source.ID, LineID: l.Source.LineID})
	}
	for i := range apps {
		x := &apps[i]
		if x.Stands() {
			used, _ = used.Add(x.Amount)
		}
		if d.Sees(x.OwnerActor, x.OwnerTeam) {
			r.Applications = append(r.Applications, allocationView(x))
		}
	}
	r.AppliedMinor, r.RestMinor = used.MinorString(), n.Rest(used).MinorString()
	if n.Decided != nil {
		r.DecidedBy, r.DecidedAt, r.DecisionReason = n.Decided.By, n.Decided.At, n.Decided.Reason
	}
	return r, nil
}

// visibleCreditNote loads credit note id and its statement for a read; nil when d does not see the note.
func visibleCreditNote(ctx context.Context, exec bentopg.Executor, id string, d authorization.Decision) (*settlement.CreditNote, *settlement.Statement, error) {
	sc := scope(ctx)
	n, err := postgres.LoadCreditNote(ctx, exec, sc, id)
	if err != nil || n == nil || !d.Sees(n.OwnerActor, n.OwnerTeam) {
		return nil, nil, err
	}
	st, err := postgres.LoadStatement(ctx, exec, sc, n.StatementID, false)
	if err != nil || st == nil {
		return nil, nil, err
	}
	return n, st, nil
}

// GetCreditNote returns a credit note (credit_note.get).
func (s Service) GetCreditNote(ctx context.Context, id string) (api.CreditNoteView, error) {
	var v api.CreditNoteView
	err := s.read(ctx, api.CommandGetCreditNote, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		n, st, err := visibleCreditNote(ctx, exec, id, d)
		if err != nil {
			return err
		}
		if n == nil {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		r, err := creditNoteRef(ctx, exec, n, st, d)
		v = api.CreditNoteView{CreditNoteRef: r, CheckedAt: kernel.Instant(s.Clock.Now())}
		return err
	})
	return v, err
}

// ResolveCreditNotes publishes CreditNoteRef v1 (for CNX, CN-AT11).
func (s Service) ResolveCreditNotes(ctx context.Context, ids []string) (map[string]api.CreditNoteRef, error) {
	out := map[string]api.CreditNoteRef{}
	if err := api.ValidateCreditNoteIDs(ids); err != nil || len(ids) == 0 {
		return out, err
	}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		for _, id := range ids {
			n, st, err := visibleCreditNote(ctx, exec, id, d)
			if err != nil {
				return err
			}
			if n == nil {
				continue
			}
			if out[id], err = creditNoteRef(ctx, exec, n, st, d); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// balanceOpen is what statement st has open now: nothing once it is reversed.
func balanceOpen(st *settlement.Statement, applied map[int]kernel.Money) kernel.Money {
	if st.Status != settlement.StatusConfirmed {
		return zeroIn(st.Currency())
	}
	return st.Open(applied)
}
