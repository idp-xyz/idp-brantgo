package application

import (
	"context"
	"slices"
	"strconv"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/adapters/postgres"
	fa "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/application"
	cc "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/customercredit"
	fr "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/fundrecord"
	oa "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/orderaccount"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/settlement"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// confirmSettlement runs settlement.confirm (STL-INV-001, R30, R31, D-4; human only). Before the
// transaction it reads the source lines — FUL's shipment lines (ShipmentLineRef v1) or PUR's
// delivery lines (PurchaseSettlementSource v1) — and the evidence (EVD). Inside it, for a sales
// statement, it locks the order account and the fund records of the order's standing stage
// applications FOR UPDATE in ID order (tactical lockOrder: 资金记录根与订单账户或结算单根按 ID 排序), so
// two statements of one order and every application or unapply of its stages commit one after the
// other; then the authority guard. It reads under those locks which source lines a standing statement
// settles and what the order's statements put on each stage, writes the statement, its lines and
// installments, and moves the stage applications to the receivable. Two purchase statements of one
// delivery line race on the source index: the second waits and fails, and the ledger's retry finds
// the line settled (SETTLEMENT_SOURCE_DUPLICATE).
type confirmSettlement struct {
	s          Service
	cmd        api.ConfirmSettlement
	shipments  map[api.SettlementSourceRef]ShipmentSource
	deliveries map[api.SettlementSourceRef]DeliverySource
	evidence   map[string]bool

	taken    bool
	orderID  string
	account  *oa.Account
	standing []fa.Application
	stale    bool
	moved    []fa.Move
}

func (h *confirmSettlement) Prepare(ctx context.Context) error {
	var err error
	if h.cmd.StatementKind == api.StatementSales {
		h.shipments, err = h.s.Upstream.Shipments.ResolveShipmentSources(ctx, h.cmd.Sources)
	} else {
		h.deliveries, err = h.s.Upstream.Deliveries.ResolveDeliverySources(ctx, h.cmd.Sources)
	}
	if err != nil || len(h.cmd.EvidenceIDs) == 0 {
		return err
	}
	h.evidence, err = h.s.resolveEvidence(ctx, h.cmd.EvidenceIDs)
	return err
}

// root is an aggregate root to lock FOR UPDATE; roots lock in ID order (kind breaks a tie).
type root struct {
	id, kind string
}

func sortRoots(roots []root) {
	slices.SortFunc(roots, func(a, b root) int {
		if a.id != b.id {
			if a.id < b.id {
				return -1
			}
			return 1
		}
		if a.kind < b.kind {
			return -1
		}
		if a.kind > b.kind {
			return 1
		}
		return 0
	})
}

const (
	rootFund      = "fund_record"
	rootAccount   = "order_account"
	rootStatement = "settlement_statement"
)

func (h *confirmSettlement) Lock(ctx context.Context, exec bentopg.Executor) error {
	sc := scope(ctx)
	h.taken, h.orderID, h.account, h.standing, h.stale, h.moved = false, "", nil, nil, false, nil
	st, err := postgres.LoadStatement(ctx, exec, sc, h.cmd.StatementID, false)
	if err != nil {
		return err
	}
	h.taken = st != nil
	if h.cmd.StatementKind != api.StatementSales {
		return nil
	}
	for _, ref := range h.cmd.Sources {
		if src, ok := h.shipments[ref]; ok {
			h.orderID = src.Line.OrderID
			break
		}
	}
	if h.orderID == "" {
		return nil
	}
	pre, err := postgres.StandingStageApplications(ctx, exec, sc, h.orderID, 1<<62)
	if err != nil {
		return err
	}
	roots := []root{{id: h.orderID, kind: rootAccount}}
	locked := map[string]bool{}
	for _, x := range pre {
		if !locked[x.FundRecordID] {
			locked[x.FundRecordID] = true
			roots = append(roots, root{id: x.FundRecordID, kind: rootFund})
		}
	}
	sortRoots(roots)
	for _, r := range roots {
		if r.kind == rootAccount {
			if h.account, err = postgres.LoadAccount(ctx, exec, sc, r.id, true); err != nil {
				return err
			}
		} else if _, err = postgres.LoadFund(ctx, exec, sc, r.id, true); err != nil {
			return err
		}
	}
	if h.account == nil {
		return nil
	}
	if h.standing, err = postgres.StandingStageApplications(ctx, exec, sc, h.orderID, h.account.OrderRevision); err != nil {
		return err
	}
	for _, x := range h.standing {
		if !locked[x.FundRecordID] {
			// A stage application on another fund committed between the read and the account lock.
			h.stale = true
		}
	}
	return nil
}

func (h *confirmSettlement) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, c, now := scope(ctx), h.cmd, kernel.Instant(h.s.Clock.Now())
	if h.taken {
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/statement_id", "The statement ID is taken")
	}
	if err := evidenceFound(h.evidence, c.EvidenceIDs, "/args/evidence_ids"); err != nil {
		return action.Outcome{}, err
	}
	keys := make([]settlement.SourceKey, 0, len(c.Sources))
	for i, ref := range c.Sources {
		p := "/args/sources/" + strconv.Itoa(i)
		var check func(string) error
		if c.StatementKind == api.StatementSales {
			src, ok := h.shipments[ref]
			if !ok {
				return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, p, "FUL shows no such shipment line")
			}
			check = src.Check
			keys = append(keys, settlement.SourceKey{Kind: settlement.SourceShipmentLine, ID: ref.ShipmentID, LineID: ref.LineID})
		} else {
			src, ok := h.deliveries[ref]
			if !ok {
				return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, p, "PUR shows no such supplier delivery line")
			}
			check = src.Check
			keys = append(keys, settlement.SourceKey{Kind: settlement.SourceDeliveryLine, ID: ref.DeliveryID, LineID: ref.LineID})
		}
		if check != nil {
			if err := check(p); err != nil {
				return action.Outcome{}, err
			}
		}
	}
	settled, err := postgres.SettledSources(ctx, exec, sc, keys)
	if err != nil {
		return action.Outcome{}, err
	}
	by := fundAct(d, now, "")
	var st *settlement.Statement
	if c.StatementKind == api.StatementSales {
		if h.stale {
			return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/sources", "The order's applications changed: send it again")
		}
		lines := make([]settlement.ShipmentLine, 0, len(c.Sources))
		for _, ref := range c.Sources {
			lines = append(lines, h.shipments[ref].Line)
		}
		var prior map[string]kernel.Money
		if a := h.account; a != nil {
			if prior, err = postgres.SettledByStage(ctx, exec, sc, a.OrderID, a.Currency()); err != nil {
				return action.Outcome{}, err
			}
		}
		if st, err = settlement.Sales(settlement.SalesInput{ID: c.StatementID, Account: h.account, Lines: lines, Settled: settled, Prior: prior,
			EvidenceIDs: c.EvidenceIDs, By: by, Team: d.Team}); err != nil {
			return action.Outcome{}, err
		}
		h.moved = fa.MoveToReceivable(st, h.standing, func() string { return action.NewID("app", now) }, by, d.Team)
	} else {
		lines := make([]settlement.DeliveryLine, 0, len(c.Sources))
		for _, ref := range c.Sources {
			lines = append(lines, h.deliveries[ref].Line)
		}
		if st, err = settlement.Purchase(settlement.PurchaseInput{ID: c.StatementID, Lines: lines, Settled: settled, EvidenceIDs: c.EvidenceIDs,
			By: by, Team: d.Team}); err != nil {
			return action.Outcome{}, err
		}
	}
	if err = postgres.InsertStatement(ctx, exec, sc, st); err != nil {
		return action.Outcome{}, err
	}
	moved := make([]map[string]any, 0, len(h.moved))
	for _, m := range h.moved {
		if err = postgres.SaveMoved(ctx, exec, sc, m.From, now); err != nil {
			return action.Outcome{}, err
		}
		entry := map[string]any{"application_id": m.From.ID, "application_version": m.From.Version, "replacement_id": m.Replacement.ID}
		for _, x := range []*fa.Application{m.Replacement, m.Remainder} {
			if x == nil {
				continue
			}
			if err = postgres.InsertApplication(ctx, exec, sc, x); err != nil {
				return action.Outcome{}, err
			}
		}
		if m.Remainder != nil {
			entry["remainder_id"] = m.Remainder.ID
		}
		moved = append(moved, entry)
	}
	fields := map[string]any{"statement_id": st.ID, "statement_version": st.Version, "kind": st.Kind, "counterparty_id": st.CounterpartyID,
		"moved": moved}
	if st.OrderID != "" {
		fields["order_id"], fields["order_revision"] = st.OrderID, st.OrderRevision
	}
	return action.Outcome{ObjectID: st.ID, RowVersion: st.Version, Status: st.Status, Events: []action.Event{{Type: api.EventSettlementConfirmed,
		AggregateType: "SettlementStatement", AggregateID: st.ID, AggregateVersion: st.Version, Fields: fields}}}, nil
}

// ConfirmSettlement confirms a sales or purchase settlement statement (settlement.confirm). Human
// only.
func (s Service) ConfirmSettlement(ctx context.Context, cmd api.ConfirmSettlement) (api.StatementReceipt, error) {
	h := &confirmSettlement{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandConfirmSettlement, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.StatementID}, cmd,
		cmd.Validate(), h)
	if err != nil {
		return api.StatementReceipt{}, err
	}
	out := api.StatementReceipt{Receipt: res}
	err = s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		sc := scope(ctx)
		st, err := postgres.LoadStatement(ctx, exec, sc, res.ObjectID, false)
		if err != nil || st == nil {
			return err
		}
		applied, err := postgres.InstallmentApplied(ctx, exec, sc, st)
		if err != nil {
			return err
		}
		out.StatementKind, out.CounterpartyID, out.OrderID, out.Currency = st.Kind, st.CounterpartyID, st.OrderID, st.Currency().Code()
		out.AmountMinor, out.OpenMinor, out.Lines, out.Installments = st.Amount.MinorString(), st.Open(applied).MinorString(), len(st.Lines),
			len(st.Installments)
		apps, err := postgres.StatementApplications(ctx, exec, sc, st.ID)
		for _, x := range apps {
			if x.MovedFrom != "" {
				out.MovedApplications = append(out.MovedApplications, x.MovedFrom)
			}
		}
		return err
	})
	return out, err
}

// lockStatementRoots locks the roots an application to statement st touches FOR UPDATE in ID order:
// its fund record, the statement and, for a receivable, the order account (the receivable
// application counts for the account's stage, which PaymentConditionGuard v1 reads under the
// account's lock). It returns the fund record and the statement read under their locks.
func lockStatementRoots(ctx context.Context, exec bentopg.Executor, fundID string, st *settlement.Statement) (*fr.FundRecord, *settlement.Statement, error) {
	sc := scope(ctx)
	roots := []root{{id: fundID, kind: rootFund}, {id: st.ID, kind: rootStatement}}
	if st.OrderID != "" {
		roots = append(roots, root{id: st.OrderID, kind: rootAccount})
	}
	sortRoots(roots)
	var f *fr.FundRecord
	var locked *settlement.Statement
	var err error
	for _, r := range roots {
		switch r.kind {
		case rootFund:
			f, err = postgres.LoadFund(ctx, exec, sc, r.id, true)
		case rootStatement:
			locked, err = postgres.LoadStatement(ctx, exec, sc, r.id, true)
		default:
			_, err = postgres.LoadAccount(ctx, exec, sc, r.id, true)
		}
		if err != nil {
			return nil, nil, err
		}
	}
	return f, locked, nil
}

// applyStatement runs application.apply to a receivable or payable (STL-INV-002; human only): inside
// the transaction it locks the fund record, the statement and — for a receivable — the order account
// FOR UPDATE in ID order, derives the fund's available balance and what stands on each installment
// from the immutable applications under those locks (D-3), and writes the application.
type applyStatement struct {
	s         Service
	cmd       api.Apply
	fund      *fr.FundRecord
	statement *settlement.Statement
	taken     bool
}

func (h *applyStatement) Lock(ctx context.Context, exec bentopg.Executor) error {
	sc := scope(ctx)
	h.fund, h.statement, h.taken = nil, nil, false
	st, err := postgres.LoadStatement(ctx, exec, sc, h.cmd.Target.StatementID, false)
	if err != nil {
		return err
	}
	if st == nil {
		h.fund, err = postgres.LoadFund(ctx, exec, sc, h.cmd.FundRecordID, true)
	} else {
		h.fund, h.statement, err = lockStatementRoots(ctx, exec, h.cmd.FundRecordID, st)
	}
	if err != nil {
		return err
	}
	x, err := postgres.LoadApplication(ctx, exec, sc, h.cmd.ApplicationID, false)
	h.taken = x != nil
	return err
}

func (h *applyStatement) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, c, f, st, now := scope(ctx), h.cmd, h.fund, h.statement, kernel.Instant(h.s.Clock.Now())
	switch {
	case h.taken:
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/application_id", "The application ID is taken")
	case f == nil || !d.Sees(f.OwnerActor, f.OwnerTeam):
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/fund_record_id", "")
	case st == nil || !d.Sees(st.OwnerActor, st.OwnerTeam):
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/target/statement_id", "")
	case fa.TargetOf(st.Kind) != c.Target.Kind:
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/target/kind", "Statement "+st.ID+" forms a "+fa.TargetOf(st.Kind))
	}
	amount, err := amountIn(f.Currency(), c.Amount, "/args/amount")
	if err != nil {
		return action.Outcome{}, err
	}
	entries, err := postgres.FundEntries(ctx, exec, sc, f)
	if err != nil {
		return action.Outcome{}, err
	}
	applied, err := postgres.InstallmentApplied(ctx, exec, sc, st)
	if err != nil {
		return action.Outcome{}, err
	}
	x, err := fa.ApplyToStatement(fa.StatementInput{ID: c.ApplicationID, Fund: f, FundEntries: entries, Statement: st, StageID: c.Target.StageID,
		Applied: applied, Amount: amount, By: fundAct(d, now, ""), Team: d.Team})
	if err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.InsertApplication(ctx, exec, sc, x); err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: x.ID, RowVersion: x.Version, Status: x.Status, Events: []action.Event{appEvent(api.EventApplied, x)}}, nil
}

// lockApplicationRoots locks the roots application x touches FOR UPDATE in ID order, before x itself:
// its fund record and order account for a stage application (B2a), its fund record, statement and
// (receivable) order account for an application to a statement.
func lockApplicationRoots(ctx context.Context, exec bentopg.Executor, x *fa.Application) error {
	if x.StatementID == "" {
		_, _, err := lockRoots(ctx, exec, x.FundRecordID, x.OrderID)
		return err
	}
	st, err := postgres.LoadStatement(ctx, exec, scope(ctx), x.StatementID, false)
	if err != nil || st == nil {
		return err
	}
	_, _, err = lockStatementRoots(ctx, exec, x.FundRecordID, st)
	return err
}

// receivablesOf is what customer party owes on business receivables as the credit exposure counts
// it in the limit currency c (D-4, STL-INV-004), read under the credit root's lock: the installments
// of its confirmed sales statements and what stands applied on them, converted at the reservation
// rates of reservations.
func receivablesOf(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, party string, c kernel.Currency, reservations []cc.Reservation) (cc.Receivables, error) {
	owed, err := postgres.OwedInstallments(ctx, exec, sc, party)
	if err != nil {
		return cc.Receivables{}, err
	}
	return cc.ReceivablesOf(c, owed, reservations)
}
