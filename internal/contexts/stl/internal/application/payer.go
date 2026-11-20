package application

import (
	"context"
	"slices"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/adapters/postgres"
	fr "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/fundrecord"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Third-party payers (S5R-D21; STL-INV-006, SM-STL-FUND; Context Map R43 PTY→STL: PartyRef v1 and
// TradingProfileRef v1, guardLock false). A receipt from another party than the customer is the
// customer's when the customer's active PTY trading profile lists the payer as a permitted payer and
// PTY shows the payer confirmed — read before STL's transaction, the profile revision recorded on the
// application — or when a credit manager other than the recorder verified the payment for the
// customer with fund.verify_payer (verifications read under the fund record's lock).

// Parties is STL's port to PTY (R43): PartyRef v1 (ResolveParties) and, from S5R-D21,
// TradingProfileRef v1 (ResolveTradingProfiles) for a customer's permitted payers. Both are lookups
// before STL's transaction; STL locks no PTY row.
type Parties interface {
	ptyapi.Queries
	ResolveTradingProfiles(ctx context.Context, refs []ptyapi.TradingProfileRef) (map[ptyapi.TradingProfileRef]ptyapi.ResolvedTradingProfile, error)
}

// permittedPayer reads, before the transaction, whether fund record fundID's payer is a permitted payer
// of customer (permittedBy); 0 when the fund is unknown or paid by the customer itself.
func (s Service) permittedPayer(ctx context.Context, fundID, customer string) (int64, error) {
	var payer string
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		f, err := postgres.LoadFund(ctx, exec, scope(ctx), fundID, false)
		if err == nil && f != nil {
			payer = f.CounterpartyID
		}
		return err
	})
	if err != nil || payer == "" || payer == customer {
		return 0, err
	}
	return s.permittedBy(ctx, payer, customer)
}

// permittedBy returns the revision of customer's active PTY trading profile when it serves new business
// and lists payer as a permitted payer, and PTY shows payer confirmed (PartyRef v1: the payer's own
// state, which never makes the profile stale); 0 otherwise.
func (s Service) permittedBy(ctx context.Context, payer, customer string) (int64, error) {
	ref := ptyapi.TradingProfileRef{PartyID: customer}
	profiles, err := s.Upstream.Parties.ResolveTradingProfiles(ctx, []ptyapi.TradingProfileRef{ref})
	if err != nil {
		return 0, err
	}
	p, ok := profiles[ref]
	if !ok || p.Code != "" || !slices.Contains(p.PermittedPayers, payer) { // a profile that does not serve new business permits no one
		return 0, nil
	}
	parties, err := s.Upstream.Parties.ResolveParties(ctx, ptyapi.Resolve{PartyIDs: []string{payer}})
	if err != nil {
		return 0, err
	}
	if x, ok := parties.Parties[payer]; !ok || x.Status != ptyapi.StatusConfirmed {
		return 0, nil
	}
	return p.Ref.Revision, nil
}

// verifyPayer runs fund.verify_payer (S5R-D21; human only, credit manager, independent of the
// recorder): before the transaction it reads the evidence (EVD) and the customer (PTY: a confirmed
// party with role customer); inside it, it locks the fund record FOR UPDATE, checks its version and
// appends the verification, moving the fund record's version by one.
type verifyPayer struct {
	s        Service
	cmd      api.VerifyPayer
	sources  map[string]bool
	customer *ptyapi.PartyRef
	fund     *fr.FundRecord
	prior    []fr.PayerVerification
}

func (h *verifyPayer) Prepare(ctx context.Context) error {
	var err error
	if h.sources, err = h.s.resolveEvidence(ctx, h.cmd.EvidenceIDs); err != nil {
		return err
	}
	parties, err := h.s.Upstream.Parties.ResolveParties(ctx, ptyapi.Resolve{PartyIDs: []string{h.cmd.CustomerID}})
	if err != nil {
		return err
	}
	if p, ok := parties.Parties[h.cmd.CustomerID]; ok {
		h.customer = &p
	}
	return nil
}

func (h *verifyPayer) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.fund, h.prior = nil, nil
	var err error
	if h.fund, err = postgres.LoadFund(ctx, exec, scope(ctx), h.cmd.FundRecordID, true); err != nil || h.fund == nil {
		return err
	}
	h.prior, err = postgres.PayerVerifications(ctx, exec, scope(ctx), h.fund.ID)
	return err
}

func (h *verifyPayer) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	c, f, now := h.cmd, h.fund, kernel.Instant(h.s.Clock.Now())
	switch {
	case f == nil || !d.Sees(f.OwnerActor, f.OwnerTeam):
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/fund_record_id", "")
	case f.Version != c.ExpectedVersion:
		return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	case h.customer == nil || h.customer.Status != ptyapi.StatusConfirmed || !slices.Contains(h.customer.Roles, ptyapi.RoleCustomer):
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/customer_id", "PTY shows no such confirmed customer")
	}
	if err := evidenceFound(h.sources, c.EvidenceIDs, "/args/evidence_ids"); err != nil {
		return action.Outcome{}, err
	}
	from := f.Version
	v, err := f.VerifyPayer(c.CustomerID, c.EvidenceIDs, h.prior, fundAct(d, now, c.Reason))
	if err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.SavePayerVerification(ctx, exec, scope(ctx), f, from, v); err != nil {
		return action.Outcome{}, err
	}
	e := fundEvent(api.EventFundPayerVerified, f, map[string]any{"payer_party_id": v.PayerID, "customer_party_id": v.CustomerID,
		"evidence_ids": v.EvidenceIDs})
	return action.Outcome{ObjectID: f.ID, RowVersion: f.Version, Status: f.Status, Events: []action.Event{e}}, nil
}

// VerifyPayer verifies that a receipt from a third party pays for a customer (fund.verify_payer).
// Human only, not the recorder.
func (s Service) VerifyPayer(ctx context.Context, cmd api.VerifyPayer) (api.FundRecordReceipt, error) {
	h := &verifyPayer{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandVerifyPayer, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.FundRecordID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
	if err != nil {
		return api.FundRecordReceipt{}, err
	}
	return s.fundReceipt(ctx, res)
}
