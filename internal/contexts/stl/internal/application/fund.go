package application

import (
	"context"
	"strconv"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/adapters/postgres"
	fr "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/fundrecord"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Funds, applications and order accounts (unit B2a). Their upstreams are POL (R33: the currency and
// the company's bank_account, ConfigGuard v1 inside the transaction), PTY (R43: the counterparty),
// EVD (R75: the bank evidence) and TRD (R32: the order's payment stages, through Orders), all read
// before STL's transaction. Inside it STL locks its own roots FOR UPDATE — the fund record and the
// order account in ID order (tactical lockOrder), or the source a refund, return or conversion draws
// on — then the ledger locks the authority guard, then POL's ConfigGuard locks the revisions
// fund.record and order_account.open decide by FOR SHARE. STL publishes PaymentConditionGuard v1
// (R45, api/payment.go) for FUL and FundRecordRef v1 (R38) for CNX.

// fundEvent is an Outbox event of a FundRecord aggregate at its version.
func fundEvent(typ string, f *fr.FundRecord, fields map[string]any) action.Event {
	fields["fund_record_id"], fields["fund_record_version"] = f.ID, f.Version
	return action.Event{Type: typ, AggregateType: "FundRecord", AggregateID: f.ID, AggregateVersion: f.Version, Fields: fields}
}

func fundAct(d authorization.Decision, at time.Time, reason string) fr.Act {
	return fr.Act{By: d.Actor, Epoch: d.Epoch, At: at, Reason: reason}
}

// evidenceFound checks the EVD sources at path.
func evidenceFound(found map[string]bool, ids []string, path string) error {
	for j, e := range ids {
		if !found[e] {
			return kernel.Reject(kernel.CodeReferenceInvalid, path+"/"+strconv.Itoa(j), "EVD shows no such source")
		}
	}
	return nil
}

func (s Service) resolveEvidence(ctx context.Context, ids []string) (map[string]bool, error) {
	found, err := s.Upstream.Sources.ResolveSources(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for id := range found {
		out[id] = true
	}
	return out, nil
}

// currencyOf checks a resolved currency revision at path and returns its currency.
func currencyOf(rc polapi.ResolvedConfig, path string) (kernel.Currency, error) {
	if err := rc.Check(path); err != nil {
		return kernel.Currency{}, err
	}
	if rc.Data.Currency == nil {
		return kernel.Currency{}, kernel.Reject(kernel.CodeReferenceInvalid, path, "Not a currency")
	}
	return kernel.NewCurrency(rc.Data.Currency.Code, rc.Data.Currency.MinorUnits)
}

func amountIn(c kernel.Currency, s, path string) (kernel.Money, error) {
	m, err := kernel.ParseAmount(c, s)
	if err != nil {
		return kernel.Money{}, kernel.Reject(kernel.CodeInvalidKnownValue, path, "More fractional digits than "+c.Code()+" has")
	}
	return m, nil
}

// recordFund runs fund.record (D-16: human only, bank evidence). Before the transaction it reads the
// counterparty (PTY), the evidence (EVD), the currency and the bank account (POL); inside it, it
// locks the source a refund, return or conversion draws on FOR UPDATE, re-checks both POL revisions
// with ConfigGuard and writes the record.
type recordFund struct {
	s        Service
	cmd      api.RecordFund
	party    *ptyapi.PartyRef
	sources  map[string]bool
	currency polapi.ResolvedConfig
	account  polapi.ResolvedConfig
	source   *fr.FundRecord
	existing *fr.FundRecord
}

func (h *recordFund) Prepare(ctx context.Context) error {
	c := h.cmd
	if c.CounterpartyID != "" {
		parties, err := h.s.Upstream.Parties.ResolveParties(ctx, ptyapi.Resolve{PartyIDs: []string{c.CounterpartyID}})
		if err != nil {
			return err
		}
		if p, ok := parties.Parties[c.CounterpartyID]; ok {
			h.party = &p
		}
	}
	var err error
	if h.sources, err = h.s.resolveEvidence(ctx, c.EvidenceIDs); err != nil {
		return err
	}
	configs, err := h.s.resolveConfigs(ctx, h.s.Clock.Now(), map[string]polapi.ConfigRef{"/args/currency": polRef(c.Currency),
		"/args/bank_account": polRef(c.BankAccount)})
	if err == nil {
		h.currency, h.account = configs["/args/currency"], configs["/args/bank_account"]
	}
	return err
}

func (h *recordFund) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.source, h.existing = nil, nil
	sc := scope(ctx)
	var err error
	if h.cmd.SourceFundRecordID != "" {
		if h.source, err = postgres.LoadFund(ctx, exec, sc, h.cmd.SourceFundRecordID, true); err != nil {
			return err
		}
	}
	h.existing, err = postgres.LoadFund(ctx, exec, sc, h.cmd.FundRecordID, false)
	return err
}

func (h *recordFund) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, c, now := scope(ctx), h.cmd, kernel.Instant(h.s.Clock.Now())
	switch {
	case h.existing != nil:
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/fund_record_id", "The fund record ID is taken")
	case c.CounterpartyID != "" && (h.party == nil || h.party.Status != ptyapi.StatusConfirmed):
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/counterparty_id", "PTY shows no such confirmed party")
	case c.SourceFundRecordID != "" && (h.source == nil || !d.Sees(h.source.OwnerActor, h.source.OwnerTeam)):
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/source_fund_record_id", "STL has no such fund record")
	}
	if err := evidenceFound(h.sources, c.EvidenceIDs, "/args/evidence_ids"); err != nil {
		return action.Outcome{}, err
	}
	cur, err := currencyOf(h.currency, "/args/currency")
	if err != nil {
		return action.Outcome{}, err
	}
	if err = h.account.Check("/args/bank_account"); err != nil {
		return action.Outcome{}, err
	}
	bank := h.account.Data.BankAccount
	if bank == nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/bank_account", "Not a bank account")
	}
	if err = guardConfig(ctx, exec, polRef(c.Currency), h.currency.ContentHash, now, "/args/currency"); err != nil {
		return action.Outcome{}, err
	}
	if err = guardConfig(ctx, exec, polRef(c.BankAccount), h.account.ContentHash, now, "/args/bank_account"); err != nil {
		return action.Outcome{}, err
	}
	if taken, err := postgres.FundByReference(ctx, exec, sc, c.BankAccount.ConfigID, c.BankReference); err != nil || taken != "" {
		if err == nil {
			err = api.Reject(api.CodeBusinessKeyConflict, "/args/bank_reference", "The bank reference is recorded on the account as "+taken)
		}
		return action.Outcome{}, err
	}
	in := fr.Input{ID: c.FundRecordID, Kind: c.FundKind, CounterpartyID: c.CounterpartyID, Currency: cur, CurrencyRevision: c.Currency.Revision,
		Account: fr.BankAccount{ID: c.BankAccount.ConfigID, Revision: c.BankAccount.Revision, OwnerKind: bank.OwnerKind, OwnerID: bank.OwnerID,
			Currency: bank.Currency, Purpose: bank.Purpose}, Company: string(sc.Company), BankReference: c.BankReference, EvidenceIDs: c.EvidenceIDs,
		By: fundAct(d, now, ""), Team: d.Team}
	in.ValueDate, _ = kernel.ParseTimestamp(c.ValueDate)
	if in.Received, err = amountIn(cur, c.ReceivedAmount, "/args/received_amount"); err != nil {
		return action.Outcome{}, err
	}
	for _, m := range []struct {
		s, path string
		to      **kernel.Money
	}{{c.RemittedAmount, "/args/remitted_amount", &in.Remitted}, {c.BankCharges, "/args/bank_charges", &in.BankCharges}} {
		if m.s != "" {
			v, err := amountIn(cur, m.s, m.path)
			if err != nil {
				return action.Outcome{}, err
			}
			*m.to = &v
		}
	}
	if h.source != nil {
		entries, err := postgres.FundEntries(ctx, exec, sc, h.source)
		if err != nil {
			return action.Outcome{}, err
		}
		in.Source = &fr.Source{Record: h.source, Entries: entries}
	}
	if x := c.FX; x != nil && h.source != nil {
		fx := &fr.FX{}
		if fx.Sold, err = amountIn(h.source.Currency(), x.SoldAmount, "/args/fx/sold_amount"); err != nil {
			return action.Outcome{}, err
		}
		fee := x.Fee
		if fee == "" {
			fee = "0"
		}
		if fx.Fee, err = amountIn(cur, fee, "/args/fx/fee"); err != nil {
			return action.Outcome{}, err
		}
		fx.Rate, _ = kernel.ParseDecimal(x.BankRate, api.RateFormat)
		in.FX = fx
	}
	f, err := fr.Record(in)
	if err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.InsertFund(ctx, exec, sc, f); err != nil {
		return action.Outcome{}, err
	}
	fields := map[string]any{"kind": f.Kind, "direction": f.Direction}
	if f.SourceID != "" {
		fields["source_fund_record_id"] = f.SourceID
	}
	return action.Outcome{ObjectID: f.ID, RowVersion: f.Version, Status: f.Status, Events: []action.Event{fundEvent(api.EventFundRecorded, f, fields)}}, nil
}

// RecordFund records a fund record (fund.record). Human only.
func (s Service) RecordFund(ctx context.Context, cmd api.RecordFund) (api.FundRecordReceipt, error) {
	h := &recordFund{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandRecordFund, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.FundRecordID}, cmd,
		cmd.Validate(), h)
	if err != nil {
		return api.FundRecordReceipt{}, err
	}
	return s.fundReceipt(ctx, res)
}

// resolveDifference runs fund.resolve_difference (SM-STL-FUND; D-16: human only, credit manager,
// independent of the recorder): before the transaction it reads the evidence (EVD); inside it, it
// locks the fund record FOR UPDATE, checks its version and appends the resolution.
type resolveDifference struct {
	s       Service
	cmd     api.ResolveFundDifference
	sources map[string]bool
	fund    *fr.FundRecord
}

func (h *resolveDifference) Prepare(ctx context.Context) error {
	var err error
	h.sources, err = h.s.resolveEvidence(ctx, h.cmd.EvidenceIDs)
	return err
}

func (h *resolveDifference) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.fund, err = postgres.LoadFund(ctx, exec, scope(ctx), h.cmd.FundRecordID, true)
	return err
}

func (h *resolveDifference) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	c, f, now := h.cmd, h.fund, kernel.Instant(h.s.Clock.Now())
	if f == nil || !d.Sees(f.OwnerActor, f.OwnerTeam) {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/fund_record_id", "")
	}
	if f.Version != c.ExpectedVersion {
		return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	if err := evidenceFound(h.sources, c.EvidenceIDs, "/args/evidence_ids"); err != nil {
		return action.Outcome{}, err
	}
	amount, err := amountIn(f.Currency(), c.Amount, "/args/amount")
	if err != nil {
		return action.Outcome{}, err
	}
	from := f.Version
	if err = f.Resolve(c.Resolution, amount, c.EvidenceIDs, fundAct(d, now, c.Reason)); err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.SaveResolution(ctx, exec, scope(ctx), f, from); err != nil {
		return action.Outcome{}, err
	}
	e := fundEvent(api.EventFundDifferenceResolved, f, map[string]any{"resolution": c.Resolution, "seq": len(f.Resolutions)})
	return action.Outcome{ObjectID: f.ID, RowVersion: f.Version, Status: f.Status, Events: []action.Event{e}}, nil
}

// ResolveFundDifference resolves part or all of a receipt's unexplained difference
// (fund.resolve_difference). Human only, not the recorder.
func (s Service) ResolveFundDifference(ctx context.Context, cmd api.ResolveFundDifference) (api.FundRecordReceipt, error) {
	h := &resolveDifference{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandResolveDifference, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.FundRecordID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
	if err != nil {
		return api.FundRecordReceipt{}, err
	}
	return s.fundReceipt(ctx, res)
}

// fundReceipt completes res with the stored fund record's balances.
func (s Service) fundReceipt(ctx context.Context, res api.Receipt) (api.FundRecordReceipt, error) {
	out := api.FundRecordReceipt{Receipt: res}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		sc := scope(ctx)
		f, err := postgres.LoadFund(ctx, exec, sc, res.ObjectID, false)
		if err != nil || f == nil {
			return err
		}
		e, err := postgres.FundEntries(ctx, exec, sc, f)
		if err != nil {
			return err
		}
		out.FundKind, out.Currency, out.ReceivedMinor = f.Kind, f.Currency().Code(), f.Received.MinorString()
		out.DifferenceMinor, out.UnexplainedMinor, out.AvailableMinor = f.Difference().MinorString(), f.Unexplained().MinorString(),
			f.Available(e).MinorString()
		return nil
	})
	return out, err
}
