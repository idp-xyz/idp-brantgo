package application

import (
	"context"
	"slices"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/acceptance"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/response"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Customer responses and acceptance (WBS S3-06, S3-13; api/acceptance.go): quote.response.record
// (human or AI), quote.response.verify, acceptance.invalidate and acceptance.mark_disputed (human
// only). The response, its acceptances and their effectivity ledger live under the quote root they
// answer, which is their conflict key (ADR-BR-002): every command locks that root FOR UPDATE first
// — the row quote.replace, quote.cancel, quote.approval.revoke (risk hold) and the object control
// lock too — then, through the ledger's Authorize, the company's authority guard row FOR SHARE;
// acceptance.invalidate and acceptance.mark_disputed lock the acceptance row FOR UPDATE after the
// root (tactical lockOrder: the root, then AcceptanceEffectivity). The root's version does not move:
// none of them changes the QuoteRoot aggregate (one existing aggregate per transaction; verify
// creates the acceptance, the registered creating exception).
//
// Before the transaction, each reads from EVD which of the sources it cites the actor reads
// (SourceRef v1, Context Map R16): the customer's message and evidence of a response, the response's
// sources for its verifier, the new evidence of an effectivity change. Inside it: the quote's
// visibility (NOT_FOUND), its object control, then the aggregate's own rules (package response and
// package acceptance) and the writes, with the action result, the audit entry and the event.

const (
	CommandGetResponse   = "quote.response.get"
	CommandGetAcceptance = "acceptance.get"
)

// acceptanceWrites are the commands of this file; a replay of one passes the root's object control
// as the write would (Visible).
var acceptanceWrites = []string{api.CommandRecordResponse, api.CommandVerifyResponse, api.CommandInvalidateAcceptance,
	api.CommandDisputeAcceptance}

// rootCommand locks the quote root a response or an acceptance belongs to and applies one command:
// quote.response.record names the root; the others resolve it (resolve) from the response or the
// acceptance, whose binding to the root never changes, and then load their own rows (load).
type rootCommand struct {
	quoteID string
	q       *quote.Quote
	resolve func(ctx context.Context, exec bentopg.Executor) (string, error)
	load    func(ctx context.Context, exec bentopg.Executor) error
	prepare func(ctx context.Context) error
	apply   func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error)
	replay  bool // the command found its own effect stored: the same response or verification under another key
}

func (h *rootCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.q, h.replay = nil, false
	if h.resolve != nil {
		id, err := h.resolve(ctx, exec)
		if err != nil {
			return err
		}
		h.quoteID = id
	}
	if h.quoteID != "" {
		q, err := postgres.LoadQuote(ctx, exec, scope(ctx), h.quoteID, postgres.ForUpdate)
		if err != nil {
			return err
		}
		h.q = q
	}
	if h.load != nil {
		return h.load(ctx, exec)
	}
	return nil
}

func (h *rootCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	return h.apply(ctx, exec, d)
}

func (h *rootCommand) Prepare(ctx context.Context) error {
	if h.prepare == nil {
		return nil
	}
	return h.prepare(ctx)
}

// admit runs the checks every write under a root starts with: the root exists and the caller sees
// it (NOT_FOUND at path), then its object control.
func (h *rootCommand) admit(ctx context.Context, d authorization.Decision, path string) error {
	if h.q == nil || !d.Sees(h.q.OwnerActor, h.q.OwnerTeam) {
		return kernel.Reject(kernel.CodeNotFound, path, "")
	}
	return h.q.Control.Admit(claim(ctx), d.Epoch)
}

// readEvidence reads, before the transaction, which of ids the actor reads.
func (s Service) readEvidence(ctx context.Context, ids []string) (map[string]bool, error) {
	found, err := s.sources(ctx, ids)
	return readableSet(found), err
}

func evidenceIDs(es []quote.Evidence) []string {
	ids := make([]string, len(es))
	for i, e := range es {
		ids[i] = e.SourceID
	}
	return ids
}

// RecordResponse records the customer's reply to an active formal revision (quote.response.record).
func (s Service) RecordResponse(ctx context.Context, cmd api.RecordResponse) (api.ResponseReceipt, error) {
	draft := response.Draft{ID: cmd.ResponseID, QuoteID: cmd.QuoteID, Revision: cmd.BusinessRevision, BusinessHash: cmd.BusinessHash,
		Classification: response.Classification(cmd.Classification), Summary: cmd.Summary,
		Message: quote.Evidence{SourceID: cmd.SourceRef.SourceID, Locator: cmd.SourceRef.Locator}, Evidence: evidence(cmd.Evidence),
		CustomerPO: cmd.CustomerPONumber}
	if t, err := kernel.ParseTimestamp(cmd.ReceivedAt); err == nil {
		draft.Arrival = &response.Arrival{At: kernel.Instant(t), Basis: response.Basis(cmd.ReceivedAtBasis)}
	}
	h := &rootCommand{quoteID: cmd.QuoteID}
	var readable map[string]bool
	var stored *response.Response
	h.prepare = func(ctx context.Context) error {
		var err error
		readable, err = s.readEvidence(ctx, append([]string{draft.Message.SourceID}, evidenceIDs(draft.Evidence)...))
		return err
	}
	h.load = func(ctx context.Context, exec bentopg.Executor) error {
		var err error
		stored, err = postgres.LoadResponse(ctx, exec, scope(ctx), cmd.ResponseID)
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := h.admit(ctx, d, "/args/quote_id"); err != nil {
			return action.Outcome{}, err
		}
		r, replay, err := response.Record(h.q, stored, draft, readable, d.Actor, d.Epoch, s.Clock.Now())
		if err != nil {
			return action.Outcome{}, err
		}
		out := action.Outcome{ObjectID: r.ID, RowVersion: 1, Status: api.StatusRecorded}
		if h.replay = replay; replay {
			return out, nil
		}
		if err = postgres.InsertResponse(ctx, exec, scope(ctx), r); err != nil {
			return action.Outcome{}, err
		}
		fields := map[string]any{"response_id": r.ID, "quote_id": r.QuoteID, "business_revision": r.Revision, "business_hash": r.BusinessHash,
			"number": r.Number, "classification": string(r.Classification), "source_id": r.Message.SourceID,
			"expires_at": kernel.FormatTimestamp(r.ExpiresAt), "late": r.Late(r.Recorded.At)}
		if a := r.Arrival; a != nil {
			fields["received_at"], fields["received_at_basis"] = kernel.FormatTimestamp(a.At), string(a.Basis)
		}
		if r.CustomerPO != "" {
			fields["customer_po_number"] = r.CustomerPO
		}
		out.Events = []action.Event{{Type: api.EventCustomerResponseRecorded, AggregateType: "CustomerResponse", AggregateID: r.ID,
			AggregateVersion: 1, Fields: fields}}
		return out, nil
	}
	res, err := s.run(ctx, action.Command{Name: api.CommandRecordResponse, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ResponseID},
		cmd, cmd.Validate(), h)
	if err != nil {
		return api.ResponseReceipt{}, err
	}
	out := api.ResponseReceipt{Receipt: res}
	out.Replayed = res.Replayed || h.replay
	err = s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		r, err := postgres.LoadResponse(ctx, exec, scope(ctx), res.ObjectID)
		if err == nil && r != nil {
			out.QuoteID, out.BusinessRevision, out.BusinessHash, out.Number = r.QuoteID, r.Revision, r.BusinessHash, r.Number
			out.Classification, out.ExpiresAt, out.RecordedAt, out.Late = string(r.Classification), r.ExpiresAt, r.Recorded.At, r.Late(r.Recorded.At)
		}
		return err
	})
	return out, err
}

// VerifyResponse verifies a recorded full_unconditional response into an acceptance and judges the
// contract's formation (quote.response.verify).
func (s Service) VerifyResponse(ctx context.Context, cmd api.VerifyResponse) (api.AcceptanceReceipt, error) {
	v := acceptance.Verification{AcceptanceID: cmd.AcceptanceID, Revision: cmd.BusinessRevision, BusinessHash: cmd.BusinessHash,
		Customer: quote.PartyRef{PartyID: cmd.CustomerRef.PartyID, Revision: cmd.CustomerRef.Revision},
		Contact:  quote.ContactRef{ContactID: cmd.ContactRef.ContactID, RowVersion: cmd.ContactRef.RowVersion}, LineIDs: cmd.LineIDs}
	h := &rootCommand{}
	var r *response.Response
	var known acceptance.Known
	var readable map[string]bool
	h.prepare = func(ctx context.Context) error {
		// The response's sources, read in a TRD transaction of its own (the response never changes),
		// then asked of EVD for the verifier.
		var ids []string
		if err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
			found, err := postgres.LoadResponse(ctx, exec, scope(ctx), cmd.ResponseID)
			if found != nil {
				ids = found.Sources()
			}
			return err
		}); err != nil {
			return err
		}
		var err error
		readable, err = s.readEvidence(ctx, ids)
		return err
	}
	h.resolve = func(ctx context.Context, exec bentopg.Executor) (string, error) {
		r, known = nil, acceptance.Known{}
		found, err := postgres.LoadResponse(ctx, exec, scope(ctx), cmd.ResponseID)
		if err != nil || found == nil {
			return "", err
		}
		r = found
		return r.QuoteID, nil
	}
	h.load = func(ctx context.Context, exec bentopg.Executor) error {
		if r == nil {
			return nil
		}
		sc := scope(ctx)
		var err error
		if known.OfQuote, err = postgres.LoadAcceptances(ctx, exec, sc, postgres.AcceptanceFilter{QuoteID: r.QuoteID}, postgres.NoLock); err != nil {
			return err
		}
		if known.Taken, err = postgres.LoadAcceptance(ctx, exec, sc, cmd.AcceptanceID, postgres.NoLock); err != nil {
			return err
		}
		message, err := postgres.LoadAcceptances(ctx, exec, sc, postgres.AcceptanceFilter{MessageSourceID: r.Message.SourceID,
			MessageLocator: r.Message.Locator}, postgres.NoLock)
		if len(message) > 0 {
			known.Message = message[0]
		}
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if r == nil {
			return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/response_id", "")
		}
		if err := h.admit(ctx, d, "/args/response_id"); err != nil {
			return action.Outcome{}, err
		}
		a, replay, err := acceptance.Verify(r, h.q, v, known, readable, d.Actor, d.Epoch, s.Clock.Now())
		if err != nil {
			return action.Outcome{}, err
		}
		out := action.Outcome{ObjectID: a.ID, RowVersion: a.Version, Status: string(a.Effectivity)}
		if h.replay = replay; replay {
			return out, nil
		}
		if err = postgres.SaveAcceptance(ctx, exec, scope(ctx), a); err != nil {
			return action.Outcome{}, err
		}
		out.Events = []action.Event{acceptanceEvent(api.EventAcceptanceVerified, a, map[string]any{"arrival_at": kernel.FormatTimestamp(a.ArrivalAt),
			"arrival_basis": a.ArrivalBasis, "expires_at": kernel.FormatTimestamp(a.ExpiresAt), "formed": a.Formed(), "number": a.Number,
			"customer_party_id": a.Customer.PartyID, "customer_revision": a.Customer.Revision, "contact_id": a.Contact.ContactID,
			"contact_row_version": a.Contact.RowVersion, "formation_rule": a.FormedBy().Rule, "formation_basis": a.FormedBy().Basis})}
		return out, nil
	}
	res, err := s.run(ctx, action.Command{Name: api.CommandVerifyResponse, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ResponseID},
		cmd, cmd.Validate(), h)
	if err != nil {
		return api.AcceptanceReceipt{}, err
	}
	return s.acceptanceReceipt(ctx, res, h.replay)
}

// acceptanceEvent is an Acceptance event at the acceptance's effectivity version: every event of
// one acceptance (verified, invalidated, disputed) shares its partition, so it is delivered in
// order.
func acceptanceEvent(eventType string, a *acceptance.Acceptance, more map[string]any) action.Event {
	fields := map[string]any{"acceptance_id": a.ID, "response_id": a.ResponseID, "quote_id": a.QuoteID, "business_revision": a.Revision,
		"business_hash": a.BusinessHash, "effectivity": string(a.Effectivity), "effectivity_version": a.Version}
	for k, v := range more {
		fields[k] = v
	}
	return action.Event{Type: eventType, AggregateType: "Acceptance", AggregateID: a.ID, AggregateVersion: a.Version, Fields: fields}
}

// InvalidateAcceptance invalidates a valid acceptance (acceptance.invalidate).
func (s Service) InvalidateAcceptance(ctx context.Context, cmd api.InvalidateAcceptance) (api.AcceptanceReceipt, error) {
	return s.changeAcceptance(ctx, api.CommandInvalidateAcceptance, api.EventAcceptanceInvalidated, cmd.IdempotencyKey, cmd.AcceptanceID,
		cmd.ExpectedEffectivityVersion, cmd.Reason, cmd.Evidence, cmd, cmd.Validate(), (*acceptance.Acceptance).Invalidate)
}

// DisputeAcceptance marks a valid acceptance disputed (acceptance.mark_disputed).
func (s Service) DisputeAcceptance(ctx context.Context, cmd api.DisputeAcceptance) (api.AcceptanceReceipt, error) {
	return s.changeAcceptance(ctx, api.CommandDisputeAcceptance, api.EventAcceptanceDisputed, cmd.IdempotencyKey, cmd.AcceptanceID,
		cmd.ExpectedEffectivityVersion, cmd.Reason, cmd.Evidence, cmd, cmd.Validate(), (*acceptance.Acceptance).MarkDisputed)
}

// changeAcceptance runs an effectivity change: the evidence's sources are read before the
// transaction; in it, the quote root FOR UPDATE, the acceptance FOR UPDATE, visibility and the
// object control, then the change (effectivity version, state) and the evidence.
func (s Service) changeAcceptance(ctx context.Context, name, eventType, key, id string, expected int64, reason string, proofs []api.Evidence,
	req api.Command, validation error, change func(*acceptance.Acceptance, int64, quote.Act) error) (api.AcceptanceReceipt, error) {
	evidenced := evidence(proofs)
	h := &rootCommand{}
	var a *acceptance.Acceptance
	var readable map[string]bool
	h.prepare = func(ctx context.Context) error {
		var err error
		readable, err = s.readEvidence(ctx, evidenceIDs(evidenced))
		return err
	}
	h.resolve = func(ctx context.Context, exec bentopg.Executor) (string, error) {
		a = nil
		found, err := postgres.LoadAcceptance(ctx, exec, scope(ctx), id, postgres.NoLock)
		if err != nil || found == nil {
			return "", err
		}
		return found.QuoteID, nil
	}
	h.load = func(ctx context.Context, exec bentopg.Executor) error {
		if h.quoteID == "" {
			return nil
		}
		var err error
		a, err = postgres.LoadAcceptance(ctx, exec, scope(ctx), id, postgres.ForUpdate)
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if a == nil {
			return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/acceptance_id", "")
		}
		if err := h.admit(ctx, d, "/args/acceptance_id"); err != nil {
			return action.Outcome{}, err
		}
		if err := change(a, expected, quote.Act{By: d.Actor, Epoch: d.Epoch, At: s.Clock.Now(), Reason: reason, Evidence: evidenced}); err != nil {
			return action.Outcome{}, err
		}
		if err := quote.CheckDraft(quote.Data{Evidence: evidenced}, readable); err != nil {
			return action.Outcome{}, moved(err, "/args/data/evidence", "/args/evidence")
		}
		if err := postgres.SaveAcceptance(ctx, exec, scope(ctx), a); err != nil {
			return action.Outcome{}, err
		}
		return action.Outcome{ObjectID: a.ID, RowVersion: a.Version, Status: string(a.Effectivity), Events: []action.Event{
			acceptanceEvent(eventType, a, nil)}}, nil
	}
	res, err := s.run(ctx, action.Command{Name: name, IdempotencyKey: key, ObjectID: id, Reason: reason}, req, validation, h)
	if err != nil {
		return api.AcceptanceReceipt{}, err
	}
	return s.acceptanceReceipt(ctx, res, false)
}

// acceptanceReceipt completes the receipt of an acceptance command, executed or replayed, from the
// stored acceptance.
func (s Service) acceptanceReceipt(ctx context.Context, r api.Receipt, replay bool) (api.AcceptanceReceipt, error) {
	out := api.AcceptanceReceipt{Receipt: r}
	out.Replayed = r.Replayed || replay
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		a, err := postgres.LoadAcceptance(ctx, exec, scope(ctx), r.ObjectID, postgres.NoLock)
		if err == nil && a != nil {
			out.ResponseID, out.QuoteID, out.BusinessRevision, out.BusinessHash, out.Number = a.ResponseID, a.QuoteID, a.Revision, a.BusinessHash,
				a.Number
			out.ArrivalAt, out.ArrivalBasis, out.ExpiresAt, out.Formed = a.ArrivalAt, a.ArrivalBasis, a.ExpiresAt, a.Formed()
			out.FormationRule, out.FormationBasis = a.FormedBy().Rule, a.FormedBy().Basis
		}
		return err
	})
	return out, err
}

// GetCustomerResponse returns a response with its acceptances, audited as quote.response.get
// (trd.read): NOT_FOUND for a response of a quote the caller does not see; the caller reads its
// sources (EVD, before the read's transaction), REFERENCE_INVALID otherwise.
func (s Service) GetCustomerResponse(ctx context.Context, id string) (api.CustomerResponseView, error) {
	var ids []string
	pre := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		r, err := postgres.LoadResponse(ctx, exec, scope(ctx), id)
		if r != nil {
			ids = r.Sources()
		}
		return err
	})
	if _, refused := kernel.AsRejection(pre); pre != nil && !refused {
		return api.CustomerResponseView{}, pre
	}
	var readable map[string]bool
	var sourceErr error
	if len(ids) > 0 {
		readable, sourceErr = s.readEvidence(ctx, ids)
	}
	var v api.CustomerResponseView
	err := s.Ledger.Read(ctx, CommandGetResponse, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		r, err := postgres.LoadResponse(ctx, exec, scope(ctx), id)
		if err != nil {
			return err
		}
		if r == nil {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		root, err := postgres.LoadRoot(ctx, exec, scope(ctx), r.QuoteID)
		if err != nil {
			return err
		}
		if root == nil || !d.Sees(root.OwnerActor, root.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		if sourceErr != nil {
			return sourceErr
		}
		for _, sid := range r.Sources() {
			if !readable[sid] {
				return kernel.Reject(kernel.CodeReferenceInvalid, "/response/source_id", "The reader does not read the response's sources")
			}
		}
		accs, err := postgres.LoadAcceptances(ctx, exec, scope(ctx), postgres.AcceptanceFilter{ResponseID: r.ID}, postgres.NoLock)
		if err != nil {
			return err
		}
		v = responseView(r, accs, s.Clock.Now())
		return nil
	})
	return v, err
}

// GetAcceptance returns an acceptance with its effectivity ledger, audited as acceptance.get
// (trd.read): NOT_FOUND for an acceptance of a quote the caller does not see.
func (s Service) GetAcceptance(ctx context.Context, id string) (api.AcceptanceView, error) {
	var v api.AcceptanceView
	err := s.Ledger.Read(ctx, CommandGetAcceptance, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		a, err := postgres.LoadAcceptance(ctx, exec, scope(ctx), id, postgres.NoLock)
		if err != nil {
			return err
		}
		if a == nil {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		root, err := postgres.LoadRoot(ctx, exec, scope(ctx), a.QuoteID)
		if err != nil {
			return err
		}
		if root == nil || !d.Sees(root.OwnerActor, root.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v = acceptanceView(a)
		return nil
	})
	return v, err
}

// ResolveAcceptances answers acceptance lookups (api.AcceptanceLookups): only an active grant is
// needed; acceptances of quotes outside the caller's data scope are absent.
func (s Service) ResolveAcceptances(ctx context.Context, ids []string) (map[string]api.AcceptanceFact, error) {
	if err := api.ValidateAcceptanceIDs(ids); err != nil {
		return nil, err
	}
	out := map[string]api.AcceptanceFact{}
	err := s.lookupAcceptances(ctx, func(ctx context.Context, exec bentopg.Executor, sees func(quoteID string) (bool, error)) error {
		for _, id := range ids {
			a, err := postgres.LoadAcceptance(ctx, exec, scope(ctx), id, postgres.NoLock)
			if err != nil {
				return err
			}
			if a == nil {
				continue
			}
			ok, err := sees(a.QuoteID)
			if err != nil {
				return err
			}
			if ok {
				out[id] = acceptanceFact(a)
			}
		}
		return nil
	})
	return out, err
}

// ResolveRevisionAcceptances returns each revision's current acceptance: its valid one, or else the
// one verified last (api.AcceptanceLookups).
func (s Service) ResolveRevisionAcceptances(ctx context.Context, refs []api.QuoteRevisionRef) (map[api.QuoteRevisionRef]api.AcceptanceFact, error) {
	if err := api.ValidateRevisionRefs(refs); err != nil {
		return nil, err
	}
	out := map[api.QuoteRevisionRef]api.AcceptanceFact{}
	err := s.lookupAcceptances(ctx, func(ctx context.Context, exec bentopg.Executor, sees func(quoteID string) (bool, error)) error {
		for _, ref := range refs {
			ok, err := sees(ref.QuoteID)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			accs, err := postgres.LoadAcceptances(ctx, exec, scope(ctx), postgres.AcceptanceFilter{QuoteID: ref.QuoteID,
				Revision: ref.BusinessRevision}, postgres.NoLock)
			if err != nil {
				return err
			}
			if len(accs) == 0 {
				continue
			}
			current := accs[len(accs)-1]
			for _, a := range accs {
				if a.Effectivity == acceptance.Valid {
					current = a
				}
			}
			out[ref] = acceptanceFact(current)
		}
		return nil
	})
	return out, err
}

// lookupAcceptances runs fn in a lookup transaction with a test of the quotes the caller sees, each
// read once.
func (s Service) lookupAcceptances(ctx context.Context, fn func(ctx context.Context, exec bentopg.Executor,
	sees func(quoteID string) (bool, error)) error) error {
	return s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		seen := map[string]bool{}
		sees := func(id string) (bool, error) {
			if ok, done := seen[id]; done {
				return ok, nil
			}
			q, err := postgres.LoadRoot(ctx, exec, scope(ctx), id)
			if err != nil {
				return false, err
			}
			seen[id] = q != nil && d.Sees(q.OwnerActor, q.OwnerTeam)
			return seen[id], nil
		}
		return fn(ctx, exec, sees)
	})
}

// responseView is a response as GetCustomerResponse shows it at now, with its acceptances.
func responseView(r *response.Response, accs []*acceptance.Acceptance, now time.Time) api.CustomerResponseView {
	v := api.CustomerResponseView{ResponseID: r.ID, QuoteID: r.QuoteID, BusinessRevision: r.Revision, BusinessHash: r.BusinessHash,
		Number: r.Number, ExpiresAt: r.ExpiresAt, Classification: string(r.Classification), Summary: r.Summary,
		SourceRef: api.Evidence{SourceID: r.Message.SourceID, Locator: r.Message.Locator}, Evidence: apiEvidence(r.Evidence),
		CustomerPONumber: r.CustomerPO, RecordedBy: r.Recorded.By, RecordedAt: r.Recorded.At, Late: r.Late(now)}
	if a := r.Arrival; a != nil {
		v.ReceivedAt, v.ReceivedAtBasis = a.At, string(a.Basis)
	}
	for _, a := range accs {
		if a.Live() && a.Formed() {
			v.Late = false // a contract formed from it and stands
		}
		v.Acceptances = append(v.Acceptances, acceptanceView(a))
	}
	return v
}

// acceptanceView is an acceptance as the reads show it.
func acceptanceView(a *acceptance.Acceptance) api.AcceptanceView {
	v := api.AcceptanceView{AcceptanceID: a.ID, ResponseID: a.ResponseID, QuoteID: a.QuoteID, BusinessRevision: a.Revision,
		BusinessHash: a.BusinessHash, Number: a.Number, CustomerRef: api.PartyRef{PartyID: a.Customer.PartyID, Revision: a.Customer.Revision},
		ContactRef: api.ContactRef{ContactID: a.Contact.ContactID, RowVersion: a.Contact.RowVersion}, LineIDs: slices.Clone(a.LineIDs),
		SourceRef: api.Evidence{SourceID: a.Message.SourceID, Locator: a.Message.Locator}, ArrivalAt: a.ArrivalAt, ArrivalBasis: a.ArrivalBasis,
		ExpiresAt: a.ExpiresAt, Formed: a.Formed(), FormationRule: a.FormedBy().Rule, FormationBasis: a.FormedBy().Basis, VerifiedBy: a.Verified.By,
		VerifiedAt: a.Verified.At, Effectivity: string(a.Effectivity), EffectivityVersion: a.Version}
	for _, c := range a.History {
		v.History = append(v.History, api.EffectivityView{EffectivityVersion: c.Version, Effectivity: string(c.Effectivity),
			ChangedBy: c.Act.By, ChangedAt: c.Act.At, Reason: c.Act.Reason, Evidence: apiEvidence(c.Act.Evidence)})
	}
	return v
}

// acceptanceFact is what the lookups say of an acceptance now.
func acceptanceFact(a *acceptance.Acceptance) api.AcceptanceFact {
	return api.AcceptanceFact{AcceptanceID: a.ID, ResponseID: a.ResponseID, Ref: api.QuoteRevisionRef{QuoteID: a.QuoteID,
		BusinessRevision: a.Revision}, BusinessHash: a.BusinessHash, Number: a.Number,
		CustomerRef: api.PartyRef{PartyID: a.Customer.PartyID, Revision: a.Customer.Revision},
		ContactRef:  api.ContactRef{ContactID: a.Contact.ContactID, RowVersion: a.Contact.RowVersion}, LineIDs: slices.Clone(a.LineIDs),
		Effectivity: string(a.Effectivity), EffectivityVersion: a.Version, ArrivalAt: a.ArrivalAt, ArrivalBasis: a.ArrivalBasis,
		ExpiresAt: a.ExpiresAt, Formed: a.Formed(), FormationRule: a.FormedBy().Rule, FormationBasis: a.FormedBy().Basis, VerifiedBy: a.Verified.By,
		VerifiedAt: a.Verified.At, Code: a.Code()}
}
