package application

import (
	"context"
	"errors"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/order"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The checks of an AI agent's order commands before a person is asked to approve them (S4 G7b;
// api.OrderRequestChecks). Each runs, in a read transaction of its own, the judgement its command
// makes on the stored records — the domain's own functions on unlocked copies, which are dropped —
// and writes only the audit entry. They are audited as their own commands and need the permission
// of the command they check, so nobody learns more than a person who could send it would.
const (
	CommandCheckPrepareOrder       = "order.prepare.check"
	CommandCheckRequestOrderReview = "order.review.request.check"
)

// checkKey stands in for the idempotency key of the command checked: a check uses no key (the key
// is the send's own, derived from the request's intent), but the commands' validation requires one,
// and the rest of the request is validated as the command validates it.
const checkKey = "check.order"

// checkRules are the authorization of the two checks: the permission of the command checked, for a
// person or an AI agent. A check is no write, so it needs no approval grant (and an AI agent's
// check proves none).
func checkRules() []authorization.Rule {
	return []authorization.Rule{
		{Command: CommandCheckPrepareOrder, Permission: api.CommandPrepareOrder, ActorKinds: humanOrAgent},
		{Command: CommandCheckRequestOrderReview, Permission: api.CommandRequestOrderReview, ActorKinds: humanOrAgent},
	}
}

// rejectChecked refuses a check whose request failed its validation, audited as the check.
func (s Service) rejectChecked(ctx context.Context, command, objectID string, validation error) error {
	if rej, ok := kernel.AsRejection(validation); ok {
		if err := s.Ledger.AuditRejected(ctx, command, objectID, rej.Code); err != nil {
			return errors.Join(validation, err)
		}
	}
	return validation
}

// authorizeCheck refuses a caller without the check's permission, before anything is read for it
// (the order commands authorize before their preparation too). A refusal is audited.
func (s Service) authorizeCheck(ctx context.Context, command, objectID string) error {
	err := s.Ledger.Lookup(ctx, command, func(context.Context, bentopg.Executor, authorization.Decision) error { return nil })
	if rej, ok := kernel.AsRejection(err); ok {
		if auditErr := s.Ledger.AuditRejected(ctx, command, objectID, rej.Code); auditErr != nil {
			return errors.Join(err, auditErr)
		}
	}
	return err
}

// CheckPrepareOrder judges order.prepare as PrepareOrder does, in this order: the request's
// validation, the facts the order seals (prepareSeal, outside any transaction, as the command reads
// them), then on the unlocked records the quote (NOT_FOUND unless the caller sees it), the order
// ID (BUSINESS_KEY_CONFLICT), the revision at its hash, the acceptance, the seal and the
// conversion slot (order.Prepare on a copy), and last the notify party's address as the
// transaction's guard would see it (REFERENCE_STALE). It does not pass the quote root's object
// control, whose claim exists only on the send, and it takes no lock, so a slot taken after it is
// the command's to refuse.
func (s Service) CheckPrepareOrder(ctx context.Context, cmd api.PrepareOrder) error {
	cmd.IdempotencyKey = checkKey
	if err := s.rejectChecked(ctx, CommandCheckPrepareOrder, cmd.OrderID, cmd.Validate()); err != nil {
		return err
	}
	if err := s.authorizeCheck(ctx, CommandCheckPrepareOrder, cmd.OrderID); err != nil {
		return err
	}
	var notify *quote.AddressRef
	if n := cmd.NotifyPartyRef; n != nil {
		notify = &quote.AddressRef{PartyID: n.PartyID, PartyRevision: n.PartyRevision, AddressID: n.AddressID}
	}
	seal, err := s.prepareSeal(ctx, cmd.QuoteID, cmd.BusinessRevision, cmd.BusinessHash, notify)
	if err != nil {
		return s.rejectedAs(ctx, CommandCheckPrepareOrder, cmd.OrderID, err)
	}
	// What PTY says of the notify address now is read before the transaction, as prepareSeal reads
	// it: no context's transaction is opened inside another's.
	var resolved ptyapi.ResolvedAddressRole
	var known bool
	if notify != nil {
		got, err := s.resolveAddresses(ctx, []ptyapi.AddressRoleRef{ptyRef(*notify)})
		if err != nil {
			return s.rejectedAs(ctx, CommandCheckPrepareOrder, cmd.OrderID, err)
		}
		resolved, known = got[ptyRef(*notify)]
	}
	return s.Ledger.Read(ctx, CommandCheckPrepareOrder, cmd.OrderID, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		q, err := postgres.LoadQuote(ctx, exec, scope(ctx), cmd.QuoteID, postgres.NoLock)
		if err != nil {
			return err
		}
		if q == nil || !d.Sees(q.OwnerActor, q.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/args/quote_id", "")
		}
		o, err := postgres.LoadOrder(ctx, exec, scope(ctx), cmd.OrderID, postgres.NoLock)
		if err != nil {
			return err
		}
		if o != nil && o.Source.QuoteID != q.ID {
			return api.Reject(api.CodeBusinessKeyConflict, "/args/order_id")
		}
		a, err := postgres.LoadAcceptance(ctx, exec, scope(ctx), cmd.AcceptanceID, postgres.NoLock)
		if err != nil {
			return err
		}
		if _, err = order.Prepare(order.Preparation{OrderID: cmd.OrderID, QuoteID: cmd.QuoteID, Revision: cmd.BusinessRevision,
			BusinessHash: cmd.BusinessHash, AcceptanceID: cmd.AcceptanceID, NotifyParty: notify, Seal: seal}, q, a, scope(ctx), d.Actor, d.Epoch,
			s.Clock.Now()); err != nil {
			return err
		}
		if notify == nil {
			return nil
		}
		// The transaction guards the notify address (guardAddress); the check applies the guard's
		// verdict to what PTY said of it, without the lock.
		return ptyapi.AddressRoleGuardVerdict{Ref: resolved.Ref, Known: known, Role: resolved.Role, Code: resolved.Code, Stale: resolved.Stale}.
			Check("/args/notify_party_ref")
	})
}

// rejectedAs audits the rejection err is, as the check, and returns it; any other error is returned
// as it is.
func (s Service) rejectedAs(ctx context.Context, command, objectID string, err error) error {
	if rej, ok := kernel.AsRejection(err); ok {
		if auditErr := s.Ledger.AuditRejected(ctx, command, objectID, rej.Code); auditErr != nil {
			return errors.Join(err, auditErr)
		}
	}
	return err
}

// CheckRequestOrderReview judges order.review.request as RequestOrderReview does, on the unlocked
// records: the order (NOT_FOUND unless the caller sees its quote), its row version, and the
// domain's RequestReview on a copy (ORDER_NOT_DRAFT, ORDER_REVIEW_IMMUTABLE, INVALID_STATE). It
// does not pass the quote root's object control (see CheckPrepareOrder).
func (s Service) CheckRequestOrderReview(ctx context.Context, cmd api.RequestOrderReview) error {
	cmd.IdempotencyKey = checkKey
	if err := s.rejectChecked(ctx, CommandCheckRequestOrderReview, cmd.OrderID, cmd.Validate()); err != nil {
		return err
	}
	return s.Ledger.Read(ctx, CommandCheckRequestOrderReview, cmd.OrderID, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		quoteID, err := postgres.OrderQuote(ctx, exec, scope(ctx), cmd.OrderID)
		if err != nil {
			return err
		}
		if quoteID == "" {
			return kernel.Reject(kernel.CodeNotFound, "/args/order_id", "")
		}
		q, err := postgres.LoadQuote(ctx, exec, scope(ctx), quoteID, postgres.NoLock)
		if err != nil {
			return err
		}
		o, err := postgres.LoadOrder(ctx, exec, scope(ctx), cmd.OrderID, postgres.NoLock)
		if err != nil {
			return err
		}
		if q == nil || o == nil || !d.Sees(q.OwnerActor, q.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/args/order_id", "")
		}
		if err = o.Check(cmd.ExpectedVersion); err != nil {
			return err
		}
		_, err = o.RequestReview(cmd.ReviewID, d.Actor, d.Epoch, s.Clock.Now())
		return err
	})
}
