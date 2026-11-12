// Package application runs STL's commands and queries on the command ledger. The customer credit
// rules live in domain/customercredit; SQL lives in adapters/postgres.
//
// STL's upstream contexts for customer credit are TRD (Context Map R32, OrderCommercialSnapshot v1,
// through STL's own port Orders), POL (R33, ConfigRef v1 and ConfigGuard v1: credit_policy, the
// currency, fx_rate of purpose credit), PTY (R43, the customer) and EVD (R75, the evidence of a
// limit). A command reads them through their api packages before STL's transaction begins (the
// ledger's Preparer step); inside it, after STL locked the customer's CustomerCredit root FOR UPDATE
// and the ledger locked the authority guard FOR SHARE, POL's ConfigGuard v1 re-checks every
// configuration revision the command decides by, locking it FOR SHARE until STL commits (R33 guard
// lock, ADR-BR-004 option B, ADR-BR-011). STL locks no TRD, PTY or EVD row and never calls another
// context's transaction owner. STL publishes CreditGuard v1 (R57) for TRD (api/guard.go).
package application

import (
	"context"
	"errors"

	"go.idp.xyz/idp-bento-go/application"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/api"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/adapters/postgres"
	cc "go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/customercredit"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/kernel"
)

var (
	anyActor  = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem}
	humanOrAI = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI}
)

// Rules is STL's authorization policy for customer credit. The tactical model's humanOnlyCommands
// name credit_limit.publish and credit_exception.decide; the command catalog lets humans and AI
// request limits and reserve and release credit (人工或 AI（按动作权限）), and the state machine has the
// system consume (PL-12) and release (PL-13) reservations, so reserve and release admit the system
// as well; credit.consume is the system's alone (SM-STL-CREDIT-RESERVATION). Reads need stl.read,
// the audit trail stl.audit.
func Rules() []authorization.Rule {
	return []authorization.Rule{
		{Command: api.CommandRequestLimit, Permission: api.CommandRequestLimit, ActorKinds: humanOrAI},
		authorization.HumanOnly(api.CommandPublishLimit, api.CommandPublishLimit),
		{Command: api.CommandReserve, Permission: api.CommandReserve, ActorKinds: anyActor},
		authorization.HumanOnly(api.CommandDecideException, api.CommandDecideException),
		{Command: api.CommandRelease, Permission: api.CommandRelease, ActorKinds: anyActor},
		{Command: api.CommandConsume, Permission: api.CommandConsume, ActorKinds: []kernel.ActorKind{kernel.ActorSystem}},
		{Command: api.CommandGetCredit, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandGetReservation, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandGetDecision, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandAudit, Permission: api.PermissionAudit, ActorKinds: anyActor},
		// Funds (B2a): fund.record, fund.resolve_difference, application.apply and application.unapply
		// are humanOnlyCommands (D-16, D-17); order_account.open adopts an order revision's stages with
		// the permission of payment_plan_review.open, by a person or the system (SM-STL-PAYMENT-PLAN-REVIEW).
		authorization.HumanOnly(api.CommandRecordFund, api.CommandRecordFund),
		authorization.HumanOnly(api.CommandResolveDifference, api.CommandResolveDifference),
		{Command: api.CommandOpenOrderAccount, Permission: api.PermissionOpenOrderAccount,
			ActorKinds: []kernel.ActorKind{kernel.ActorHuman, kernel.ActorSystem}},
		authorization.HumanOnly(api.CommandApply, api.CommandApply),
		authorization.HumanOnly(api.CommandUnapply, api.CommandUnapply),
		{Command: api.CommandGetFundRecord, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandGetOrderAccount, Permission: api.PermissionRead, ActorKinds: anyActor},
		{Command: api.CommandGetApplication, Permission: api.PermissionRead, ActorKinds: anyActor},
	}
}

// Upstream are the contexts STL reads references from.
type Upstream struct {
	Orders  Orders         // TRD OrderCommercialSnapshot v1 (R32), STL's port
	Configs polapi.Queries // POL ConfigRef v1 (R33)
	Parties ptyapi.Queries // PTY PartyRef v1 (R43)
	Sources evdapi.Queries // EVD SourceRef v1 (R75)
}

// Complete reports whether every upstream is wired.
func (u Upstream) Complete() bool {
	return u.Orders != nil && u.Configs != nil && u.Parties != nil && u.Sources != nil
}

// Service runs STL's use cases.
type Service struct {
	Ledger   *action.Ledger
	Clock    application.Clock
	Upstream Upstream
	Grants   authorization.Grants
}

func scope(ctx context.Context) kernel.Scope {
	s, _ := bento.ScopeFromContext(ctx)
	return s
}

// Visible reports whether d's actor sees the object a stored result names: a limit request, a
// reservation or a decision (credit.reserve names a decision when it opened one).
func Visible(ctx context.Context, exec bentopg.Executor, d authorization.Decision, command, id string) (bool, error) {
	if visible, handled, err := visibleFund(ctx, exec, d, command, id); handled {
		return visible, err
	}
	sc := scope(ctx)
	var subjects []string
	switch command {
	case api.CommandRequestLimit, api.CommandPublishLimit:
		subjects = []string{postgres.SubjectLimitRequest}
	case api.CommandDecideException:
		subjects = []string{postgres.SubjectDecision}
	case api.CommandReserve:
		subjects = []string{postgres.SubjectReservation, postgres.SubjectDecision}
	default:
		subjects = []string{postgres.SubjectReservation}
	}
	for _, subject := range subjects {
		owner, team, found, err := ownerOf(ctx, exec, sc, subject, id)
		if err != nil || found {
			return found && d.Sees(owner, team), err
		}
	}
	return false, nil
}

func ownerOf(ctx context.Context, exec bentopg.Executor, sc kernel.Scope, subject, id string) (owner, team string, found bool, err error) {
	switch subject {
	case postgres.SubjectLimitRequest:
		r, err := postgres.LoadRequest(ctx, exec, sc, id)
		if err != nil || r == nil {
			return "", "", false, err
		}
		return r.OwnerActor, r.OwnerTeam, true, nil
	case postgres.SubjectDecision:
		x, err := postgres.LoadDecision(ctx, exec, sc, id)
		if err != nil || x == nil {
			return "", "", false, err
		}
		return x.OwnerActor, x.OwnerTeam, true, nil
	}
	r, err := postgres.LoadReservation(ctx, exec, sc, id)
	if err != nil || r == nil {
		return "", "", false, err
	}
	return r.OwnerActor, r.OwnerTeam, true, nil
}

// lockSubject locks FOR UPDATE the credit root of the customer subject id belongs to; nil when
// there is no such subject.
func lockSubject(ctx context.Context, exec bentopg.Executor, subject, id string) (*cc.Root, error) {
	sc := scope(ctx)
	party, err := postgres.PartyOf(ctx, exec, sc, subject, id)
	if err != nil || party == "" {
		return nil, err
	}
	return postgres.LoadRoot(ctx, exec, sc, party, true)
}

// lockOrCreate locks the customer's credit root FOR UPDATE, creating it at version 0 first when it
// is missing (the command moves it to 1 or rolls it back).
func (s Service) lockOrCreate(ctx context.Context, exec bentopg.Executor, party string) (*cc.Root, error) {
	sc := scope(ctx)
	if err := postgres.EnsureRoot(ctx, exec, sc, party, s.Clock.Now()); err != nil {
		return nil, err
	}
	return postgres.LoadRoot(ctx, exec, sc, party, true)
}

// bump moves the root to its next row version.
func (s Service) bump(ctx context.Context, exec bentopg.Executor, root *cc.Root) error {
	root.RowVersion++
	return postgres.SaveRoot(ctx, exec, scope(ctx), root, s.Clock.Now())
}

// event is an Outbox event of the customer's CustomerCredit aggregate at the root's row version.
func event(typ string, root *cc.Root, fields map[string]any) action.Event {
	fields["party_id"] = root.PartyID
	return action.Event{Type: typ, AggregateType: "CustomerCredit", AggregateID: root.PartyID, AggregateVersion: root.RowVersion, Fields: fields}
}

func (s Service) run(ctx context.Context, cmd action.Command, req api.Command, validation error, h action.Handler) (api.Receipt, error) {
	if validation != nil {
		if rej, ok := kernel.AsRejection(validation); ok {
			if err := s.Ledger.AuditRejected(ctx, cmd.Name, cmd.ObjectID, rej.Code); err != nil {
				return api.Receipt{}, errors.Join(validation, err)
			}
		}
		return api.Receipt{}, validation
	}
	_, _, args := request(req)
	hash, err := action.RequestHash(args)
	if err != nil {
		return api.Receipt{}, err
	}
	cmd.RequestHash = hash
	res, err := s.Ledger.Execute(ctx, cmd, h)
	return api.Receipt(res), err
}

// request returns the name, the idempotency key and the business arguments of cmd, which its
// request hash covers: everything but the key and the expected version.
func request(cmd api.Command) (name, key string, args any) {
	switch c := cmd.(type) {
	case api.RequestCreditLimit:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return api.CommandRequestLimit, key, c
	case api.PublishCreditLimit:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandPublishLimit, key, c
	case api.ReserveCredit:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return api.CommandReserve, key, c
	case api.DecideCreditException:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandDecideException, key, c
	case api.ReleaseCredit:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandRelease, key, c
	case api.ConsumeCredit:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return api.CommandConsume, key, c
	case api.RecordFund:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return api.CommandRecordFund, key, c
	case api.ResolveFundDifference:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandResolveDifference, key, c
	case api.OpenOrderAccount:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return api.CommandOpenOrderAccount, key, c
	case api.Apply:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return api.CommandApply, key, c
	case api.Unapply:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandUnapply, key, c
	}
	return "", "", nil
}

// Reconcile reports what became of cmd, sent earlier with its idempotency key, without executing
// it (api.Reconciler; contract 06, 动作对账).
func (s Service) Reconcile(ctx context.Context, cmd api.Command) (api.Reconciliation, error) {
	name, key, args := request(cmd)
	if name == "" {
		return api.Reconciliation{}, kernel.Reject(kernel.CodeSchemaInvalid, "/command", "")
	}
	hash, err := action.RequestHash(args)
	if err != nil {
		return api.Reconciliation{}, err
	}
	r, err := s.Ledger.Reconcile(ctx, name, key, hash)
	if err != nil {
		return api.Reconciliation{}, err
	}
	out := api.Reconciliation{State: r.State, ActionID: r.ActionID, ObjectID: r.ObjectID}
	if r.Result != nil {
		receipt := api.Receipt(*r.Result)
		out.Receipt = &receipt
	}
	return out, nil
}

// Audit returns STL's audit trail.
func (s Service) Audit(ctx context.Context) ([]kernel.AuditEntry, error) {
	return s.Ledger.Audit(ctx, api.CommandAudit)
}

// AuditRejected records a request an inbound adapter refused before it became a command.
func (s Service) AuditRejected(ctx context.Context, command, objectID, code string) error {
	return s.Ledger.AuditRejected(ctx, command, objectID, code)
}
