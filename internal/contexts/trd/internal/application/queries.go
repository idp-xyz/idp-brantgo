package application

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/calc"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The read side of WBS S3-09 (api/queries.go): readiness, dispatch preflight and target here, the
// histories in history.go. Each query is an audited read of the ledger under trd.read, run in a TRD
// transaction of its own after the caller is authorized now (X-INV-003); a quote the caller does
// not see is NOT_FOUND, a malformed query SCHEMA_INVALID, both audited. What a query needs of the
// upstream contexts it reads before that transaction through their lookups (Context Map R11–R16;
// no other context's transaction owner is ever called inside TRD's); inside it, readiness re-checks
// what the revision binds through the upstream guard functions with TRD's executor, as the
// formalization did (ADR-BR-004 option B). Nothing is written but the audit entry.

// readRules are the read side's authorization: trd.read, any actor kind (an AI agent or TSK's
// system actor verifies a goal as a human reader does).
func readRules() []authorization.Rule {
	var out []authorization.Rule
	for _, q := range []string{api.QueryQuoteReady, api.QueryQuoteHistory, api.QueryApprovalHistory, api.QueryDocumentHistory,
		api.QueryCommitHistory, api.QueryOutboxHistory, api.QueryDispatchPreflight, api.QueryDispatchTarget, api.QueryGoalReadiness} {
		out = append(out, authorization.Rule{Command: q, Permission: PermissionRead, ActorKinds: anyActor})
	}
	return out
}

// readQuote runs the audited read command of quote id. A query that failed validation is refused
// and audited; otherwise the ledger authorizes the caller, the quote is loaded unlocked
// (NOT_FOUND unless the caller sees it) and fn reads it.
func (s Service) readQuote(ctx context.Context, command, id string, validation error,
	fn func(ctx context.Context, exec bentopg.Executor, d authorization.Decision, q *quote.Quote) error) error {
	if validation != nil {
		if rej, ok := kernel.AsRejection(validation); ok {
			if err := s.Ledger.AuditRejected(ctx, command, id, rej.Code); err != nil {
				return errors.Join(validation, err)
			}
		}
		return validation
	}
	return s.Ledger.Read(ctx, command, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		q, err := postgres.LoadQuote(ctx, exec, scope(ctx), id, postgres.NoLock)
		if err != nil {
			return err
		}
		if q == nil || !d.Sees(q.OwnerActor, q.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/quote_id", "")
		}
		return fn(ctx, exec, d, q)
	})
}

// preRead returns quote id as the caller sees it, read before a query's transaction in a TRD lookup
// of its own (nil when the caller does not see it). A refusal is left to the audited read, which
// decides and records it.
func (s Service) preRead(ctx context.Context, id string) (*quote.Quote, error) {
	var out *quote.Quote
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		q, err := postgres.LoadQuote(ctx, exec, scope(ctx), id, postgres.NoLock)
		if q != nil && d.Sees(q.OwnerActor, q.OwnerTeam) {
			out = q
		}
		return err
	})
	if _, refused := kernel.AsRejection(err); refused {
		return nil, nil
	}
	return out, err
}

// reasons collects the refusals of a query's checks.
type reasons []api.Reason

// add records err's refusal under check and returns nil; any other error is returned.
func (r *reasons) add(check string, err error) error {
	rej, ok := kernel.AsRejection(err)
	if !ok {
		return err
	}
	path := ""
	if len(rej.Fields) > 0 {
		path = rej.Fields[0].Path
	}
	*r = append(*r, api.Reason{Check: check, Code: rej.Code, Path: path})
	return nil
}

// refusal is a rejection with a code TRD or the kernel declares, at path.
func refusal(code, path string) error {
	if c, ok := api.Category(code); ok {
		return kernel.RejectAs(code, c, path, "")
	}
	return kernel.Reject(code, path, "")
}

// judged is the revision readiness judges on q: the one named, or the active one (nil for none).
func judged(q *quote.Quote, named int64) *quote.Revision {
	if named != 0 {
		return q.Revision(named)
	}
	return q.Active()
}

// withoutSources is draft d without the EVD sources it cites: readiness reads every upstream the
// calculation binds (PTY, INQ, CAT, POL, CST, through their lookups) but not EVD, whose sources the
// freeze judged and the formalization does not re-check; a reader of the quote needs no read of
// them to learn whether it is ready.
func withoutSources(d quote.Data) quote.Data {
	d.Evidence = nil
	d.Lines = slices.Clone(d.Lines)
	for i := range d.Lines {
		d.Lines[i].Evidence = nil
	}
	d.Charges = slices.Clone(d.Charges)
	for i := range d.Charges {
		d.Charges[i].Evidence = nil
	}
	return d
}

// errMoved means the revision a readiness judges moved between the read of its inputs and the
// read's transaction: the inputs are read again.
var errMoved = errors.New("trd: the judged revision moved")

// QuoteReady says whether the quote's active revision (or the revision named, which must be it)
// serves the next step now, audited as quote.ready (api.ReadQueries).
func (s Service) QuoteReady(ctx context.Context, rq api.ReadyQuery) (api.QuoteReadiness, error) {
	var out api.QuoteReadiness
	const attempts = 3
	for attempt := 1; ; attempt++ {
		now := s.Clock.Now()
		var read int64 // the sealed revision whose inputs were read; 0: none
		var f facts
		var inputs error
		if rq.Validate() == nil {
			q, err := s.preRead(ctx, rq.QuoteID)
			if err != nil {
				return api.QuoteReadiness{}, err
			}
			if q != nil {
				if rev := judged(q, rq.BusinessRevision); rev != nil && rev.Seal != nil {
					read = rev.Number
					if f, inputs = s.facts(ctx, withoutSources(rev.Data), now); inputs != nil {
						if _, refused := kernel.AsRejection(inputs); !refused {
							return api.QuoteReadiness{}, inputs
						}
					}
				}
			}
		}
		err := s.readQuote(ctx, api.QueryQuoteReady, rq.QuoteID, rq.Validate(),
			func(ctx context.Context, exec bentopg.Executor, d authorization.Decision, q *quote.Quote) error {
				if rev := judged(q, rq.BusinessRevision); rev != nil && rev.Seal != nil && rev.Number != read {
					if attempt < attempts {
						return errMoved
					}
					return kernel.Reject(kernel.CodeVersionConflict, "/quote_id", "The quote changed while it was judged")
				}
				var err error
				out, err = s.readiness(ctx, exec, d, q, rq.BusinessRevision, f, inputs, now)
				return err
			})
		if errors.Is(err, errMoved) {
			continue
		}
		return out, err
	}
}

// readiness judges the revision named (or the active one) of q at instant at, for the caller d, on
// the inputs f read before the transaction (inputs: their refusal). In order: the object control
// as a write by the caller would meet it; the revision (NOT_FOUND for a revision the root does not
// have, NO_ACTIVE_REVISION, QUOTE_NOT_CURRENT for one that is not active); its commit evidence;
// its formal document's bytes; a risk hold; the validity; the conversion slot (taken:
// CONVERSION_SLOT_CONFLICT); the approval its commit evidence names (quote.ReadyApproval); the upstream guards (guardUpstream); then the cost scope and sales terms
// bound again on the current inputs, and the sales exchange rate's age.
func (s Service) readiness(ctx context.Context, exec bentopg.Executor, d authorization.Decision, q *quote.Quote, named int64, f facts,
	inputs error, at time.Time) (api.QuoteReadiness, error) {
	out := api.QuoteReadiness{QuoteID: q.ID, RootVersion: q.RootVersion, ControlState: string(q.Control.State), CheckedAt: kernel.Instant(at)}
	var r reasons
	finish := func() (api.QuoteReadiness, error) {
		out.Reasons, out.Ready = r, len(r) == 0
		if out.Reasons == nil {
			out.Reasons = []api.Reason{}
		}
		return out, nil
	}
	if err := r.add(api.CheckObjectControl, q.Control.Admit(fence(ctx, exec, d))); err != nil {
		return out, err
	}
	active, rev := q.Active(), judged(q, named)
	switch {
	case named != 0 && rev == nil:
		return out, kernel.Reject(kernel.CodeNotFound, "/business_revision", "")
	case rev == nil:
		_ = r.add(api.CheckRevision, refusal(api.CodeNoActiveRevision, "/quote_id"))
		return finish()
	case rev != active:
		_ = r.add(api.CheckRevision, refusal(api.CodeQuoteNotCurrent, "/business_revision"))
	}
	out.BusinessRevision, out.Status = rev.Number, string(rev.Status)
	if rev.Seal == nil || rev.Formal == nil {
		_ = r.add(api.CheckCommitEvidence, refusal(api.CodeCommitEvidenceMissing, "/business_revision"))
		return finish()
	}
	out.BusinessHash, out.ExpiresAt, out.ApprovalID = rev.Seal.BusinessHash, rev.Seal.ExpiresAt, rev.Formal.Commit.ApprovalID
	if _, err := s.verifiedContent(ctx, exec, q, rev); r.add(api.CheckDocument, err) != nil {
		return out, err
	}
	if h := q.Hold(rev.Number); h != nil {
		_ = r.add(api.CheckRiskHold, refusal(h.Code, "/holds/"+h.ID))
	}
	if !kernel.Instant(at).Before(rev.Seal.ExpiresAt) {
		_ = r.add(api.CheckValidity, refusal(api.CodeQuoteExpired, "/expires_at"))
	}
	if q.Slot != nil {
		// The root's one full_v1 conversion is taken: the revision serves no conversion any more (S3
		// gap closed in S4 unit A3; baseline 15.4: 已占用时返回已有订单或明确冲突 — the order is the
		// root's GetQuote conversion and GoalReadiness's evidence).
		_ = r.add(api.CheckConversion, api.Reject(api.CodeConversionSlotConflict, "/quote_id"))
	}
	if _, err := q.ReadyApproval(rev, at, s.standings(ctx, exec, q)); r.add(api.CheckApproval, err) != nil {
		return out, err
	}
	c, err := quote.ParseCommercial(rev.Seal.Commercial, rev.Seal.BusinessHash)
	if err != nil {
		if err = r.add(api.CheckDocument, err); err != nil {
			return out, err
		}
		return finish()
	}
	if _, err = s.guardUpstream(ctx, exec, sealedGuards(rev, c), at); r.add(api.CheckUpstream, err) != nil {
		return out, err
	}
	if inputs != nil {
		_ = r.add(api.CheckInputs, inputs)
		return finish()
	}
	if snap := f.snapshot; snap != nil {
		out.CostSnapshot = &api.CostQualification{SnapshotID: snap.Ref.SnapshotID, ResultHash: snap.Ref.ResultHash, EvidenceHash: snap.EvidenceHash,
			Status: snap.Adoptability.Status, Adoptable: snap.Adoptability.Adoptable()}
	}
	check, err := bindAgain(c, rev.Data, f, at)
	if r.add(check, err) != nil {
		return out, err
	}
	if err = r.add(api.CheckSalesFX, salesFXAge(rev.Data, f, at)); err != nil {
		return out, err
	}
	return finish()
}

// bindAgain binds the cost scope and the sales terms of draft d again on the current inputs f at
// instant at (calc.BindScope, calc.BindTerms, as the freeze's calculation bound them) and compares
// them with the bindings commercial document c froze (quote.Calculation.Bound): the check and the
// refusal of the first that does not hold, at the commercial document's paths.
func bindAgain(c quote.Commercial, d quote.Data, f facts, at time.Time) (string, error) {
	const from, to = dataPath, "/data"
	switch {
	case f.rfq == nil:
		return api.CheckCostScope, kernel.Reject(kernel.CodeReferenceInvalid, to+"/rfq_ref", "")
	case f.snapshot == nil:
		return api.CheckCostScope, kernel.Reject(kernel.CodeReferenceInvalid, to+"/cost_snapshot_ref", "")
	case f.Profile == nil:
		return api.CheckCostScope, kernel.Reject(kernel.CodeReferenceInvalid, to+"/trading_profile_ref", "")
	}
	b := builder{in: CalculationInput{Data: d, Configs: f.configs, Rfq: *f.rfq, Profile: *f.Profile, Snapshot: *f.snapshot, Context: f.context,
		At: at}}
	q, cf, err := b.inputs()
	if err != nil {
		return api.CheckCostScope, moved(err, from, to)
	}
	scope, err := calc.BindScope(q, cf)
	if err != nil {
		return api.CheckCostScope, moved(err, from, to)
	}
	terms, err := calc.BindTerms(q)
	if err != nil {
		return api.CheckSalesTerms, moved(err, from, to)
	}
	err = c.Calculation.Bound(scope, terms)
	if rej, ok := kernel.AsRejection(err); ok && len(rej.Fields) > 0 && rej.Fields[0].Path == quote.PathSalesTermsBound {
		return api.CheckSalesTerms, err
	}
	return api.CheckCostScope, err
}

// salesFXAge checks, at instant at, that the sales exchange rate draft d was calculated with is not
// older than its pricing policy allows (FX_STALE; Python _current_sales_fx, T03 A32, PYT-0383),
// on the revisions POL answered before the transaction (ConfigGuard keeps them in force in it).
func salesFXAge(d quote.Data, f facts, at time.Time) error {
	if d.SalesFX == nil || d.PricingPolicy == nil {
		return nil
	}
	cfg := func(r quote.Ref) polapi.ConfigData {
		return f.configs[polapi.ConfigRef{Kind: r.Kind, ConfigID: r.ID, Revision: r.Revision}].Data
	}
	rate, policy := cfg(*d.SalesFX).FXRate, cfg(*d.PricingPolicy).PricingPolicy
	switch {
	case rate == nil:
		return kernel.Reject(kernel.CodeReferenceInvalid, "/data/sales_fx_ref", "")
	case policy == nil:
		return kernel.Reject(kernel.CodeReferenceInvalid, "/data/pricing_policy_ref", "")
	}
	observed, err := kernel.ParseTimestamp(rate.ObservedAt)
	if err != nil {
		return kernel.Reject(kernel.CodeReferenceInvalid, "/data/sales_fx_ref", "The rate has no observation instant")
	}
	return quote.FreshFX(observed, time.Duration(policy.MaxFXAgeSeconds)*time.Second, at, "/data/sales_fx_ref")
}

// recipientFacts reads, before a dispatch query's transaction, what PTY says now of the customer,
// the contact and the addresses revision rev of quote id binds; nil facts when the caller does not
// see the quote or the revision is not sealed. A refusal of a lookup is returned as inputs.
func (s Service) recipientFacts(ctx context.Context, id string, rev int64) (f *quote.Facts, inputs, err error) {
	q, err := s.preRead(ctx, id)
	if err != nil || q == nil {
		return nil, nil, err
	}
	r := q.Revision(rev)
	if r == nil || r.Seal == nil {
		return nil, nil, nil
	}
	f = &quote.Facts{Addresses: map[quote.AddressRef]quote.Address{}}
	if inputs = s.parties(ctx, r.Data, f); inputs != nil {
		if _, refused := kernel.AsRejection(inputs); !refused {
			return nil, nil, inputs
		}
	}
	return f, inputs, nil
}

// DispatchPreflight says what holds before a formal revision is sent to the customer, audited as
// quote.dispatch.preflight (api.ReadQueries).
func (s Service) DispatchPreflight(ctx context.Context, dq api.DispatchQuery) (api.DispatchPreflight, error) {
	now := s.Clock.Now()
	var f *quote.Facts
	var inputs error
	if dq.Validate() == nil {
		var err error
		if f, inputs, err = s.recipientFacts(ctx, dq.QuoteID, dq.BusinessRevision); err != nil {
			return api.DispatchPreflight{}, err
		}
	}
	var out api.DispatchPreflight
	err := s.readQuote(ctx, api.QueryDispatchPreflight, dq.QuoteID, dq.Validate(),
		func(ctx context.Context, exec bentopg.Executor, d authorization.Decision, q *quote.Quote) error {
			var err error
			out, err = s.preflight(ctx, exec, q, dq.BusinessRevision, f, inputs, now)
			return err
		})
	return out, err
}

// DispatchTarget returns a formal revision's recipient and whether it can be sent, audited as
// quote.dispatch.target (api.ReadQueries).
func (s Service) DispatchTarget(ctx context.Context, dq api.DispatchQuery) (api.DispatchTarget, error) {
	now := s.Clock.Now()
	var f *quote.Facts
	var inputs error
	if dq.Validate() == nil {
		var err error
		if f, inputs, err = s.recipientFacts(ctx, dq.QuoteID, dq.BusinessRevision); err != nil {
			return api.DispatchTarget{}, err
		}
	}
	var out api.DispatchTarget
	err := s.readQuote(ctx, api.QueryDispatchTarget, dq.QuoteID, dq.Validate(),
		func(ctx context.Context, exec bentopg.Executor, d authorization.Decision, q *quote.Quote) error {
			p, err := s.preflight(ctx, exec, q, dq.BusinessRevision, f, inputs, now)
			if err != nil {
				return err
			}
			out = api.DispatchTarget{QuoteID: q.ID, BusinessRevision: p.BusinessRevision, BusinessHash: p.BusinessHash, Addresses: []api.RecipientAddress{},
				PreflightReady: p.Ready, ChannelStatus: api.ChannelNotConfigured, CheckedAt: p.CheckedAt, ExternalDelivery: api.ExternalDeliveryNotStarted}
			r := reasons(p.Reasons)
			if rev := q.Revision(dq.BusinessRevision); rev.Seal != nil {
				recipient, sum, err := quote.RecipientOf(rev)
				if r.add(api.CheckRecipientSnapshot, err) != nil {
					return err
				}
				if err == nil {
					out.CustomerRef = api.PartyRef{PartyID: recipient.Customer.PartyID, Revision: recipient.Customer.Revision}
					out.ContactRef = api.ContactRef{ContactID: recipient.Contact.ContactID, RowVersion: recipient.Contact.RowVersion}
					out.RecipientSnapshotSHA256 = sum
					for _, a := range recipient.Addresses {
						out.Addresses = append(out.Addresses, api.RecipientAddress{Ref: api.AddressRoleRef{PartyID: a.Ref.PartyID,
							PartyRevision: a.Ref.PartyRevision, AddressID: a.Ref.AddressID}, Role: a.Role, Address: a.Address})
					}
				}
			}
			// Brant has no dispatch channel (T03 A15): a target is never ready, and nothing is sent.
			_ = r.add(api.CheckChannel, refusal(api.CodeExternalChannelNotConfigured, "/channel"))
			out.Reasons = r
			return nil
		})
	return out, err
}

// preflight judges, at instant at, what holds before revision n of q is sent (T03 A14), on the
// recipient facts f PTY answered before the transaction (inputs: their refusal): the commit
// evidence; the revision active; no risk hold; the validity; the document's bytes; the approval
// the commit evidence names, in effect; the activation event in the Outbox; the customer confirmed
// at the bound revision, the contact confirmed at the bound row version and the address roles in
// force. NOT_FOUND for a revision the root does not have.
func (s Service) preflight(ctx context.Context, exec bentopg.Executor, q *quote.Quote, n int64, f *quote.Facts, inputs error,
	at time.Time) (api.DispatchPreflight, error) {
	rev := q.Revision(n)
	if rev == nil {
		return api.DispatchPreflight{}, kernel.Reject(kernel.CodeNotFound, "/business_revision", "")
	}
	out := api.DispatchPreflight{QuoteID: q.ID, BusinessRevision: n, Status: string(rev.Status), CheckedAt: kernel.Instant(at),
		ExternalDelivery: api.ExternalDeliveryNotStarted}
	var r reasons
	finish := func() (api.DispatchPreflight, error) {
		out.Reasons, out.Ready = r, len(r) == 0
		if out.Reasons == nil {
			out.Reasons = []api.Reason{}
		}
		return out, nil
	}
	if rev.Seal != nil {
		out.BusinessHash = rev.Seal.BusinessHash
	}
	if rev.Seal == nil || rev.Formal == nil {
		_ = r.add(api.CheckCommitEvidence, refusal(api.CodeCommitEvidenceMissing, "/business_revision"))
		return finish()
	}
	fm := rev.Formal
	out.Number, out.DocumentSHA256, out.ApprovalID = fm.Document.Number, fm.Document.ContentSHA256, fm.Commit.ApprovalID
	act, err := postgres.LoadAction(ctx, exec, scope(ctx), fm.Commit.Command, fm.Commit.IdempotencyKey)
	if err != nil {
		return out, err
	}
	if act != nil {
		out.ActionID = act.ActionID
	}
	if rev.Status != quote.Active {
		_ = r.add(api.CheckRevision, refusal(api.CodeQuoteNotCurrent, "/business_revision"))
	}
	if h := q.Hold(n); h != nil {
		_ = r.add(api.CheckRiskHold, refusal(h.Code, "/holds/"+h.ID))
	}
	if !kernel.Instant(at).Before(rev.Seal.ExpiresAt) {
		_ = r.add(api.CheckValidity, refusal(api.CodeQuoteExpired, "/expires_at"))
	}
	if _, err = s.verifiedContent(ctx, exec, q, rev); r.add(api.CheckDocument, err) != nil {
		return out, err
	}
	if _, err = q.CommittedApproval(rev); r.add(api.CheckApproval, err) != nil {
		return out, err
	}
	key := scope(ctx).String() + "/QuoteRoot:" + q.ID
	if out.ActivationEventID, err = postgres.LoadActivationEvent(ctx, exec, s.Ledger.DB.Schema(), api.Source, key,
		[]string{api.EventQuoteActivated, api.EventQuoteReplaced}, n); err != nil {
		return out, err
	}
	if out.ActivationEventID == "" {
		_ = r.add(api.CheckActivationEvent, refusal(api.CodeCommitEvidenceMissing, "/outbox"))
	}
	if inputs != nil || f == nil {
		if inputs == nil {
			inputs = kernel.Reject(kernel.CodeReferenceInvalid, "/data/customer_ref", "")
		}
		_ = r.add(api.CheckInputs, inputs)
		return finish()
	}
	for _, err := range recipientChecks(rev.Data, f) {
		if err.err != nil {
			_ = r.add(err.check, err.err)
		}
	}
	return finish()
}

type checked struct {
	check string
	err   error
}

// recipientChecks judges the recipient draft d binds against what PTY answered now: the customer
// confirmed, a customer, at the bound revision; the contact confirmed, the customer's, at the
// bound row version (REFERENCE_STALE otherwise, REFERENCE_INVALID for one PTY does not show the
// caller); each address role in force (its owner's verdict).
func recipientChecks(d quote.Data, f *quote.Facts) []checked {
	var out []checked
	switch p := f.Party; {
	case d.Customer == nil || p == nil:
		out = append(out, checked{api.CheckCustomer, kernel.Reject(kernel.CodeReferenceInvalid, "/data/customer_ref", "")})
	case !p.Confirmed || !p.Customer || p.Revision != d.Customer.Revision:
		out = append(out, checked{api.CheckCustomer, refusal(api.CodeReferenceStale, "/data/customer_ref")})
	}
	switch c := f.Contact; {
	case d.Contact == nil || c == nil:
		out = append(out, checked{api.CheckContact, kernel.Reject(kernel.CodeReferenceInvalid, "/data/contact_ref", "")})
	case !c.Confirmed || d.Customer == nil || c.PartyID != d.Customer.PartyID || c.RowVersion != d.Contact.RowVersion:
		out = append(out, checked{api.CheckContact, refusal(api.CodeReferenceStale, "/data/contact_ref")})
	}
	for i, a := range d.Addresses {
		path := "/data/customer_address_refs/" + strconv.Itoa(i)
		if got, ok := f.Addresses[a]; !ok {
			out = append(out, checked{api.CheckAddresses, kernel.Reject(kernel.CodeReferenceInvalid, path, "")})
		} else if got.Check != nil {
			if err := got.Check(path); err != nil {
				out = append(out, checked{api.CheckAddresses, err})
			}
		}
	}
	return out
}
