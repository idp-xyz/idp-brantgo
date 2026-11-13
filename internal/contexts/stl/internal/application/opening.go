package application

import (
	"context"
	"slices"
	"strconv"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/adapters/postgres"
	fr "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/fundrecord"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/settlement"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Opening balances (S5 unit C10, WBS S5-20; STL-INV-012; MD-50; baseline 11.2; BT17, CN-AT16 STL half).
//
// opening_balance.record (human only, treasury) reads before STL's transaction the counterparty (PTY, R43), the
// evidence (EVD, R75: the source document's 期初来源) and the currency — and for a prepayment the company's bank
// account — (POL, R33). In the transaction it locks, for an opening receivable, the customer's CustomerCredit root
// FOR UPDATE when there is one (a credit.reserve of the customer reads the receivables under that lock: it either
// committed before and the exposure counted no opening receivable, or it waits and counts it); then the ledger
// locks the authority guard FOR SHARE, and for a prepayment POL's ConfigGuard v1 the currency and bank account
// revisions FOR SHARE (R33 guard lock, as fund.record). It writes one aggregate: the opening statement with its
// installments, or the opening fund record, and the opening item. Two records of one source document race on the
// opening item's partial unique index: the second waits and fails, and the ledger's retry finds the item
// standing (OPENING_BALANCE_DUPLICATE). STL locks no upstream row.

// recordOpening runs opening_balance.record.
type recordOpening struct {
	s        Service
	cmd      api.RecordOpening
	party    *ptyapi.PartyRef
	evidence map[string]bool
	currency polapi.ResolvedConfig
	account  polapi.ResolvedConfig

	standing *settlement.Opening
	taken    bool
}

func (h *recordOpening) Prepare(ctx context.Context) error {
	c := h.cmd
	parties, err := h.s.Upstream.Parties.ResolveParties(ctx, ptyapi.Resolve{PartyIDs: []string{c.CounterpartyID}})
	if err != nil {
		return err
	}
	h.party = nil
	if p, ok := parties.Parties[c.CounterpartyID]; ok {
		h.party = &p
	}
	if h.evidence, err = h.s.resolveEvidence(ctx, c.EvidenceIDs); err != nil {
		return err
	}
	refs := map[string]polapi.ConfigRef{"/args/currency": polRef(c.Currency)}
	if c.BankAccount != nil {
		refs["/args/bank_account"] = polRef(*c.BankAccount)
	}
	configs, err := h.s.resolveConfigs(ctx, h.s.Clock.Now(), refs)
	if err != nil {
		return err
	}
	h.currency, h.account = configs["/args/currency"], configs["/args/bank_account"]
	return nil
}

func (h *recordOpening) Lock(ctx context.Context, exec bentopg.Executor) error {
	sc, c := scope(ctx), h.cmd
	h.standing, h.taken = nil, false
	if c.OpeningKind == api.OpeningReceivable {
		if _, err := postgres.LoadRoot(ctx, exec, sc, c.CounterpartyID, true); err != nil {
			return err
		}
	}
	var err error
	if h.standing, err = postgres.StandingOpening(ctx, exec, sc, c.OpeningKind, c.Source.Document); err != nil {
		return err
	}
	o, err := postgres.LoadOpening(ctx, exec, sc, c.OpeningID, false)
	if err != nil || o != nil {
		h.taken = o != nil
		return err
	}
	st, err := postgres.LoadStatement(ctx, exec, sc, c.OpeningID, false)
	if err != nil || st != nil {
		h.taken = st != nil
		return err
	}
	f, err := postgres.LoadFund(ctx, exec, sc, c.OpeningID, false)
	h.taken = f != nil
	return err
}

// partyRole is the PTY role the counterparty of an opening item of kind holds.
func partyRole(kind string) string {
	if kind == api.OpeningPayable || kind == api.OpeningSupplierPrepayment {
		return ptyapi.RoleSupplier
	}
	return ptyapi.RoleCustomer
}

func (h *recordOpening) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, c, now := scope(ctx), h.cmd, kernel.Instant(h.s.Clock.Now())
	if err := settlement.Duplicate(h.standing); err != nil {
		return action.Outcome{}, err
	}
	switch {
	case h.taken:
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/opening_id", "The ID is taken")
	case h.party == nil || h.party.Status != ptyapi.StatusConfirmed || !slices.Contains(h.party.Roles, partyRole(c.OpeningKind)):
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/counterparty_id", "PTY shows no such confirmed "+partyRole(c.OpeningKind))
	}
	if err := evidenceFound(h.evidence, c.EvidenceIDs, "/args/evidence_ids"); err != nil {
		return action.Outcome{}, err
	}
	cur, err := currencyOf(h.currency, "/args/currency")
	if err != nil {
		return action.Outcome{}, err
	}
	in := settlement.OpeningInput{ID: c.OpeningID, Kind: c.OpeningKind, SourceDocument: c.Source.Document, CurrencyRevision: c.Currency.Revision,
		CounterpartyID: c.CounterpartyID, OrderReference: c.OrderReference, EvidenceIDs: c.EvidenceIDs, By: fundAct(d, now, ""), Team: d.Team}
	in.CutoffAt, _ = kernel.ParseTimestamp(c.CutoffAt)
	in.SourceIssuedAt, _ = kernel.ParseTimestamp(c.Source.IssuedAt)
	if in.SourceAmount, err = amountIn(cur, c.Source.Amount, "/args/source/amount"); err != nil {
		return action.Outcome{}, err
	}
	if in.Amount, err = amountIn(cur, c.Amount, "/args/amount"); err != nil {
		return action.Outcome{}, err
	}
	for i, x := range c.Installments {
		m, err := amountIn(cur, x.Amount, "/args/installments/"+strconv.Itoa(i)+"/amount")
		if err != nil {
			return action.Outcome{}, err
		}
		due, _ := kernel.ParseTimestamp(x.DueAt)
		in.Installments = append(in.Installments, settlement.Installment{Amount: m, DueAt: due})
	}
	in.ValueDate, _ = kernel.ParseTimestamp(c.ValueDate)
	o, st, err := settlement.RecordOpening(in)
	if err != nil {
		return action.Outcome{}, err
	}
	var f *fr.FundRecord
	if api.Prepayment(c.OpeningKind) {
		if f, err = h.fund(ctx, exec, d, cur, now); err != nil {
			return action.Outcome{}, err
		}
	}
	fields := map[string]any{"opening_id": o.ID, "kind": o.Kind, "cutoff_at": kernel.FormatTimestamp(o.CutoffAt),
		"source_document": o.SourceDocument, "counterparty_id": o.CounterpartyID}
	e := action.Event{Type: api.EventOpeningBalanceRecorded, Fields: fields}
	if st != nil {
		if err = postgres.InsertStatement(ctx, exec, sc, st); err != nil {
			return action.Outcome{}, err
		}
		fields["statement_id"], fields["statement_version"] = st.ID, st.Version
		e.AggregateType, e.AggregateID, e.AggregateVersion = "SettlementStatement", st.ID, st.Version
	} else {
		if err = guardConfig(ctx, exec, polRef(c.Currency), h.currency.ContentHash, now, "/args/currency"); err != nil {
			return action.Outcome{}, err
		}
		if err = guardConfig(ctx, exec, polRef(*c.BankAccount), h.account.ContentHash, now, "/args/bank_account"); err != nil {
			return action.Outcome{}, err
		}
		if err = postgres.InsertFund(ctx, exec, sc, f); err != nil {
			return action.Outcome{}, err
		}
		fields["fund_record_id"], fields["fund_record_version"] = f.ID, f.Version
		e.AggregateType, e.AggregateID, e.AggregateVersion = "FundRecord", f.ID, f.Version
	}
	if err = postgres.InsertOpening(ctx, exec, sc, o); err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: o.ID, RowVersion: 1, Status: o.Status, Events: []action.Event{e}}, nil
}

// fund is the opening fund record of a prepayment: a receipt from the customer or a payment to the supplier of the
// amount on the company's bank account (fundrecord.Record's rules; PARTY_ENTITY_MISMATCH for an account that is not
// the company's), its bank reference recorded on the account by no fund record yet (BUSINESS_KEY_CONFLICT).
func (h *recordOpening) fund(ctx context.Context, exec bentopg.Executor, d authorization.Decision, cur kernel.Currency, now time.Time) (*fr.FundRecord, error) {
	c := h.cmd
	if err := h.account.Check("/args/bank_account"); err != nil {
		return nil, err
	}
	bank := h.account.Data.BankAccount
	if bank == nil {
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/args/bank_account", "Not a bank account")
	}
	kind := fr.KindReceipt
	if c.OpeningKind == api.OpeningSupplierPrepayment {
		kind = fr.KindPayment
	}
	in := fr.Input{ID: c.OpeningID, Kind: kind, CounterpartyID: c.CounterpartyID, Currency: cur, CurrencyRevision: c.Currency.Revision,
		Account: fr.BankAccount{ID: c.BankAccount.ConfigID, Revision: c.BankAccount.Revision, OwnerKind: bank.OwnerKind, OwnerID: bank.OwnerID,
			Currency: bank.Currency, Purpose: bank.Purpose}, Company: string(scope(ctx).Company), BankReference: c.BankReference,
		EvidenceIDs: c.EvidenceIDs, By: fundAct(d, now, ""), Team: d.Team}
	in.ValueDate, _ = kernel.ParseTimestamp(c.ValueDate)
	var err error
	if in.Received, err = amountIn(cur, c.Amount, "/args/amount"); err != nil {
		return nil, err
	}
	f, err := fr.Record(in)
	if err != nil {
		return nil, err
	}
	taken, err := postgres.FundByReference(ctx, exec, scope(ctx), c.BankAccount.ConfigID, c.BankReference)
	if err != nil {
		return nil, err
	}
	if taken != "" {
		return nil, api.Reject(api.CodeBusinessKeyConflict, "/args/bank_reference", "The bank reference is recorded on the account as "+taken)
	}
	return f, nil
}

// RecordOpening records an opening item (opening_balance.record). Human only.
func (s Service) RecordOpening(ctx context.Context, cmd api.RecordOpening) (api.OpeningReceipt, error) {
	h := &recordOpening{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandRecordOpening, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.OpeningID}, cmd,
		cmd.Validate(), h)
	if err != nil {
		return api.OpeningReceipt{}, err
	}
	out := api.OpeningReceipt{Receipt: res}
	err = s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		o, err := postgres.LoadOpening(ctx, exec, scope(ctx), res.ObjectID, false)
		if err != nil || o == nil {
			return err
		}
		out.OpeningKind, out.CutoffAt, out.SourceDocument, out.CounterpartyID = o.Kind, o.CutoffAt, o.SourceDocument, o.CounterpartyID
		out.Currency, out.AmountMinor, out.StatementID, out.FundRecordID = o.Amount.Currency().Code(), o.Amount.MinorString(), o.StatementID,
			o.FundRecordID
		return nil
	})
	return out, err
}

// reverseOpening moves the opening item of reversed statement st, if it is one, to reversed (settlement.reverse of
// an opening statement, C8's command: its source document may be recorded again).
func reverseOpening(ctx context.Context, exec bentopg.Executor, st *settlement.Statement, at time.Time) error {
	if !st.Opening {
		return nil
	}
	sc := scope(ctx)
	o, err := postgres.LoadOpening(ctx, exec, sc, st.ID, true)
	if err != nil || o == nil {
		return err
	}
	if err = o.ReverseOpening(at); err != nil {
		return err
	}
	return postgres.SaveOpeningReversed(ctx, exec, sc, o)
}

// openingView is opening item o now, with what d sees of its statement's applications or its fund record.
func openingView(ctx context.Context, exec bentopg.Executor, o *settlement.Opening, d authorization.Decision, now time.Time) (api.OpeningView, error) {
	sc := scope(ctx)
	cur := o.Amount.Currency()
	v := api.OpeningView{OpeningID: o.ID, OpeningKind: o.Kind, Status: o.Status, CutoffAt: o.CutoffAt, SourceDocument: o.SourceDocument,
		SourceIssuedAt: o.SourceIssuedAt, CounterpartyID: o.CounterpartyID, OrderReference: o.OrderReference, Currency: cur.Code(),
		CurrencyRevision: o.CurrencyRevision, SourceAmountMinor: o.SourceAmount.MinorString(), SettledBeforeMinor: o.SettledBefore().MinorString(),
		AmountMinor: o.Amount.MinorString(), StatementID: o.StatementID, FundRecordID: o.FundRecordID, EvidenceIDs: o.EvidenceIDs,
		RecordedBy: o.Recorded.By, RecordedAt: o.Recorded.At, CheckedAt: now}
	var open kernel.Money
	if o.StatementID != "" {
		st, err := postgres.LoadStatement(ctx, exec, sc, o.StatementID, false)
		if err != nil || st == nil {
			return v, err
		}
		b, err := balance(ctx, exec, sc, st, d, now)
		if err != nil {
			return v, err
		}
		v.Balance = &b
		if open, err = kernel.ParseMinor(cur, b.OpenMinor); err != nil {
			return v, err
		}
	} else {
		f, err := postgres.LoadFund(ctx, exec, sc, o.FundRecordID, false)
		if err != nil || f == nil {
			return v, err
		}
		ref, err := fundRef(ctx, exec, sc, f, d)
		if err != nil {
			return v, err
		}
		v.FundRecord = &ref
		e, err := postgres.FundEntries(ctx, exec, sc, f)
		if err != nil {
			return v, err
		}
		open = f.Available(e)
	}
	applied, err := o.Amount.Sub(open)
	if err != nil {
		return v, err
	}
	if o.Status != settlement.OpeningRecorded {
		applied = zeroIn(cur) // a reversed opening statement: nothing was applied to it (reversal needs none standing)
	}
	v.AppliedSinceMinor, v.OpenMinor = applied.MinorString(), open.MinorString()
	return v, nil
}

// GetOpening returns an opening item now (opening_balance.get).
func (s Service) GetOpening(ctx context.Context, id string) (api.OpeningView, error) {
	var v api.OpeningView
	err := s.read(ctx, api.CommandGetOpening, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		o, err := postgres.LoadOpening(ctx, exec, scope(ctx), id, false)
		if err != nil {
			return err
		}
		if o == nil || !d.Sees(o.OwnerActor, o.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v, err = openingView(ctx, exec, o, d, kernel.Instant(s.Clock.Now()))
		return err
	})
	return v, err
}

// totalKey groups opening items for their totals.
type totalKey struct {
	cutoff   time.Time
	kind     string
	currency string
}

// Openings returns the opening items the caller sees, of cutoffAt when it is not "", and the totals of the standing
// ones per cutoff, kind and currency (opening_balance.list; CN-AT16 截止、来源余额及已办状态可核对).
func (s Service) Openings(ctx context.Context, cutoffAt string) (api.OpeningsView, error) {
	var cutoff time.Time
	if cutoffAt != "" {
		t, err := kernel.ParseTimestamp(cutoffAt)
		if err != nil {
			err = kernel.Reject(kernel.CodeSchemaInvalid, "/cutoff_at", "")
			if auditErr := s.Ledger.AuditRejected(ctx, api.CommandOpenings, "", kernel.CodeSchemaInvalid); auditErr != nil {
				return api.OpeningsView{}, auditErr
			}
			return api.OpeningsView{}, err
		}
		cutoff = t
	}
	out := api.OpeningsView{Items: []api.OpeningView{}, Totals: []api.OpeningTotalView{}}
	err := s.Ledger.Read(ctx, api.CommandOpenings, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		now := kernel.Instant(s.Clock.Now())
		out.CheckedAt = now
		list, err := postgres.Openings(ctx, exec, scope(ctx), cutoff)
		if err != nil {
			return err
		}
		type sums struct {
			n                                     int
			source, before, amount, applied, open kernel.Money
		}
		totals := map[totalKey]*sums{}
		var order []totalKey
		for _, o := range list {
			if !d.Sees(o.OwnerActor, o.OwnerTeam) {
				continue
			}
			v, err := openingView(ctx, exec, o, d, now)
			if err != nil {
				return err
			}
			out.Items = append(out.Items, v)
			if o.Status != settlement.OpeningRecorded {
				continue
			}
			k := totalKey{cutoff: o.CutoffAt, kind: o.Kind, currency: v.Currency}
			t, ok := totals[k]
			if !ok {
				z := zeroIn(o.Amount.Currency())
				t = &sums{source: z, before: z, amount: z, applied: z, open: z}
				totals[k] = t
				order = append(order, k)
			}
			t.n++
			for _, x := range []struct {
				into *kernel.Money
				s    string
			}{{&t.source, v.SourceAmountMinor}, {&t.before, v.SettledBeforeMinor}, {&t.amount, v.AmountMinor}, {&t.applied, v.AppliedSinceMinor},
				{&t.open, v.OpenMinor}} {
				m, err := kernel.ParseMinor(o.Amount.Currency(), x.s)
				if err != nil {
					return err
				}
				if *x.into, err = x.into.Add(m); err != nil {
					return err
				}
			}
		}
		for _, k := range order {
			t := totals[k]
			out.Totals = append(out.Totals, api.OpeningTotalView{CutoffAt: k.cutoff, OpeningKind: k.kind, Currency: k.currency, Items: t.n,
				SourceAmountMinor: t.source.MinorString(), SettledBeforeMinor: t.before.MinorString(), AmountMinor: t.amount.MinorString(),
				AppliedSinceMinor: t.applied.MinorString(), OpenMinor: t.open.MinorString()})
		}
		return nil
	})
	return out, err
}
