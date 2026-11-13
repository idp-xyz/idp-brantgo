package policy

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"time"

	"go.idp.xyz/idp-bento-go/application"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/ai/capability"
	"go.idp.xyz/idp-brant/internal/ai/codes"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Service runs the delegation commands and checks on the AI layer's ledger (brant_ai).
type Service struct {
	Ledger   *action.Ledger
	Clock    application.Clock
	Registry *capability.Registry
}

// GrantDelegation grants a delegation. The checks after the ledger's (structure, human-only rule
// and permission, idempotency): the ID is new; every capability is registered and none is
// human-only (PERMISSION_DENIED; X-INV-002) or unknown (UNSUPPORTED_CONTRACT), and each fits the
// autonomy (INVALID_KNOWN_VALUE); the expiry is in the future and at most MaxLifetime away; the
// agent is an AI actor with an active grant in the scope (REFERENCE_INVALID); the initiator holds
// the permission of every capability (PERMISSION_DENIED).
func (s Service) GrantDelegation(ctx context.Context, cmd GrantDelegation) (Receipt, error) {
	cmd.ExpiresAt = kernel.Instant(cmd.ExpiresAt)
	if err := cmd.Validate(); err != nil {
		return Receipt{}, s.rejectEarly(ctx, CommandGrant, cmd.DelegationID, err)
	}
	args := cmd
	args.IdempotencyKey = ""
	hash, err := action.RequestHash(args)
	if err != nil {
		return Receipt{}, err
	}
	res, err := s.Ledger.Execute(ctx, action.Command{Name: CommandGrant, IdempotencyKey: cmd.IdempotencyKey, RequestHash: hash,
		ObjectID: cmd.DelegationID}, &grantHandler{s: s, cmd: cmd})
	return Receipt(res), err
}

// RevokeDelegation revokes an active delegation the revoker sees, at its current version.
func (s Service) RevokeDelegation(ctx context.Context, cmd RevokeDelegation) (Receipt, error) {
	if err := cmd.Validate(); err != nil {
		return Receipt{}, s.rejectEarly(ctx, CommandRevoke, cmd.DelegationID, err)
	}
	args := cmd
	args.IdempotencyKey, args.ExpectedVersion = "", 0
	hash, err := action.RequestHash(args)
	if err != nil {
		return Receipt{}, err
	}
	res, err := s.Ledger.Execute(ctx, action.Command{Name: CommandRevoke, IdempotencyKey: cmd.IdempotencyKey, RequestHash: hash,
		ObjectID: cmd.DelegationID, Reason: cmd.Reason}, &revokeHandler{s: s, cmd: cmd})
	return Receipt(res), err
}

// GetDelegation returns a delegation the caller sees (its agent, or a reader whose data scope
// covers the initiator), audited as ai.delegation.get (ai.read); NOT_FOUND otherwise.
func (s Service) GetDelegation(ctx context.Context, id string) (Delegation, error) {
	var out Delegation
	err := s.Ledger.Read(ctx, CommandGet, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		del, err := Load(ctx, exec, scopeOf(ctx), id, NoLock)
		if err != nil {
			return err
		}
		if del == nil || !del.SeenBy(d) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		out = *del
		return nil
	})
	return out, err
}

// Visible reports whether the actor of d sees delegation id, for replays and reconciliations of the
// delegation commands.
func Visible(ctx context.Context, exec bentopg.Executor, d authorization.Decision, id string) (bool, error) {
	del, err := Load(ctx, exec, scopeOf(ctx), id, NoLock)
	return err == nil && del != nil && del.SeenBy(d), err
}

func (s Service) rejectEarly(ctx context.Context, command, objectID string, rejection error) error {
	if rej, ok := kernel.AsRejection(rejection); ok {
		if err := s.Ledger.AuditRejected(ctx, command, objectID, rej.Code); err != nil {
			return errors.Join(rejection, err)
		}
	}
	return rejection
}

type grantHandler struct {
	s      Service
	cmd    GrantDelegation
	exists bool
}

func (h *grantHandler) Lock(ctx context.Context, exec bentopg.Executor) error {
	d, err := Load(ctx, exec, scopeOf(ctx), h.cmd.DelegationID, ForUpdate)
	h.exists = d != nil
	return err
}

func (h *grantHandler) Apply(ctx context.Context, exec bentopg.Executor, dec authorization.Decision) (action.Outcome, error) {
	if h.exists {
		return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/id", "")
	}
	scope, now := scopeOf(ctx), h.s.Clock.Now()
	autonomy := capability.SideEffect(h.cmd.Autonomy)
	caps := make([]capability.Capability, len(h.cmd.Capabilities))
	for i, name := range h.cmd.Capabilities {
		path := "/args/capabilities/" + strconv.Itoa(i)
		c, err := h.s.Registry.Lookup(scope.Tenant, name, path)
		if err != nil {
			return action.Outcome{}, err
		}
		if !c.SideEffect.Within(autonomy) {
			return action.Outcome{}, kernel.Reject(kernel.CodeInvalidKnownValue, path, "above the autonomy")
		}
		caps[i] = c
	}
	if !h.cmd.ExpiresAt.After(now) || h.cmd.ExpiresAt.Sub(now) > MaxLifetime {
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/expires_at", "")
	}
	var authz authorization.Authorizer
	agent, err := authz.Qualify(ctx, exec, h.cmd.AgentID)
	if err != nil {
		return action.Outcome{}, err
	}
	if !agent.Active || agent.Actor.Kind != kernel.ActorAI {
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/agent_id", "not an active AI actor")
	}
	initiator, err := authz.Qualify(ctx, exec, dec.Actor.ID)
	if err != nil {
		return action.Outcome{}, err
	}
	for i, c := range caps {
		if !initiator.Holds(c.Permission()) {
			// A delegation never allows more than its initiator holds.
			return action.Outcome{}, kernel.Reject(kernel.CodePermissionDenied, "/args/capabilities/"+strconv.Itoa(i), "")
		}
	}
	names := slices.Clone(h.cmd.Capabilities)
	slices.Sort(names)
	d := Delegation{DelegationID: h.cmd.DelegationID, InitiatorID: dec.Actor.ID, AgentID: h.cmd.AgentID, TaskID: h.cmd.TaskID,
		Autonomy: autonomy, Capabilities: names, ExpiresAt: h.cmd.ExpiresAt, Status: StatusActive, RowVersion: 1,
		OwnerTeamID: dec.Team, GrantedEpoch: dec.Epoch, GrantedAt: now}
	if err = insert(ctx, exec, scope, d); err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: d.DelegationID, RowVersion: 1, Status: StatusActive}, nil
}

type revokeHandler struct {
	s   Service
	cmd RevokeDelegation
	d   *Delegation
}

func (h *revokeHandler) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.d, err = Load(ctx, exec, scopeOf(ctx), h.cmd.DelegationID, ForUpdate)
	return err
}

func (h *revokeHandler) Apply(ctx context.Context, exec bentopg.Executor, dec authorization.Decision) (action.Outcome, error) {
	switch {
	case h.d == nil || !h.d.SeenBy(dec):
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/id", "")
	case h.d.RowVersion != h.cmd.ExpectedVersion:
		return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	case h.d.Status != StatusActive:
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/id", "")
	}
	d := *h.d
	d.Status, d.RowVersion = StatusRevoked, d.RowVersion+1
	d.RevokedByID, d.RevokedAt, d.RevokeReason = dec.Actor.ID, h.s.Clock.Now(), h.cmd.Reason
	if err := revoke(ctx, exec, scopeOf(ctx), d, dec.Epoch); err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: d.DelegationID, RowVersion: d.RowVersion, Status: StatusRevoked}, nil
}

// Step is one call an AI agent makes under a delegation, as the trusted executor states it: the
// task and the delegation come from TSK and the platform, never from model output (AI-INV-004).
type Step struct {
	DelegationID string
	TaskID       string
	Capability   capability.Capability
}

// Authority is what a granted step saw: the delegation and the scope's authority epoch.
type Authority struct {
	Delegation Delegation
	Epoch      int64
}

// Check re-checks, now and in a brant_ai transaction of its own, that the AI actor of ctx may make
// step (CheckIn). A refusal is audited in brant_ai under the step's command.
func (s Service) Check(ctx context.Context, step Step) (Authority, error) {
	p, err := identity.FromContext(ctx)
	if err != nil {
		return Authority{}, err
	}
	ctx = action.WithCorrelation(ctx, action.Correlation(ctx))
	var out Authority
	err = WithinScope(ctx, s.Ledger.DB, p.Scope, func(ctx context.Context, exec bentopg.Executor) error {
		var err error
		out, err = CheckIn(ctx, exec, p, step, s.Clock.Now())
		return err
	})
	return out, s.Audited(ctx, step.Capability.Command, step.DelegationID, err)
}

// CheckIn checks step for principal p inside the caller's brant_ai transaction exec at now:
//
//   - p is an AI actor (PERMISSION_DENIED otherwise: humans act on their own grants);
//   - the delegation exists in the scope, is p's, is for the task and is not revoked, and lists the
//     capability within its autonomy (GRANT_INVALID otherwise), and has not expired (GRANT_EXPIRED);
//   - the agent's grant is active and holds the capability's permission, and the initiator is an
//     active human who still holds it (PERMISSION_DENIED otherwise; AI-INV-002, AN04).
//
// It locks the delegation FOR SHARE and then the authority guard rows FOR SHARE, so a revocation of
// the delegation or a change of either grant either committed before and is seen, or waits until the
// caller's transaction ends.
func CheckIn(ctx context.Context, exec bentopg.Executor, p identity.Principal, step Step, now time.Time) (Authority, error) {
	if p.Actor.Kind != kernel.ActorAI {
		return Authority{}, kernel.Reject(kernel.CodePermissionDenied, "/", "delegations are for AI actors")
	}
	d, err := Load(ctx, exec, p.Scope, step.DelegationID, ForShare)
	if err != nil {
		return Authority{}, err
	}
	switch {
	case d == nil || d.AgentID != p.Actor.ID || d.TaskID != step.TaskID || d.Status != StatusActive || !d.Allows(step.Capability):
		return Authority{}, codes.Reject(codes.GrantInvalid, "/delegation")
	case d.Expired(now):
		return Authority{}, codes.Reject(codes.GrantExpired, "/delegation")
	}
	var authz authorization.Authorizer
	own, err := authz.Access(ctx, exec)
	if err != nil {
		return Authority{}, Denied(err)
	}
	permission := step.Capability.Permission()
	agent, err := authz.Qualify(ctx, exec, p.Actor.ID)
	if err != nil {
		return Authority{}, err
	}
	initiator, err := authz.Qualify(ctx, exec, d.InitiatorID)
	if err != nil {
		return Authority{}, err
	}
	if !agent.Holds(permission) || agent.Actor.Kind != kernel.ActorAI {
		return Authority{}, kernel.Reject(kernel.CodePermissionDenied, "/", "the agent's grant")
	}
	if !initiator.Holds(permission) || initiator.Actor.Kind != kernel.ActorHuman {
		return Authority{}, kernel.Reject(kernel.CodePermissionDenied, "/delegation", "the initiator's grant")
	}
	return Authority{Delegation: *d, Epoch: own.Epoch}, nil
}

// WithinScope runs fn in a transaction of db in scope, with db's executor.
func WithinScope(ctx context.Context, db *bentopg.DB, scope kernel.Scope, fn func(context.Context, bentopg.Executor) error) error {
	return bento.ScopeTransactor{DB: db, Scope: scope}.WithinTransaction(ctx, func(ctx context.Context) error {
		exec, err := db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		return fn(ctx, exec)
	})
}

// Audited records err, when it is a rejection, in the AI layer's audit trail under command and
// objectID, and returns it.
func (s Service) Audited(ctx context.Context, command, objectID string, err error) error {
	rej, ok := kernel.AsRejection(err)
	if !ok {
		return err
	}
	if command == "" {
		command = "invalid"
	}
	if auditErr := s.Ledger.AuditRejected(ctx, command, objectID, rej.Code); auditErr != nil {
		return errors.Join(err, auditErr)
	}
	return err
}

// Denied turns an authorization failure into PERMISSION_DENIED and passes other errors on.
func Denied(err error) error {
	switch {
	case errors.Is(err, authorization.ErrForbidden), errors.Is(err, authorization.ErrActorKindDenied),
		errors.Is(err, authorization.ErrUnknownCommand), errors.Is(err, authorization.ErrScopeMismatch),
		errors.Is(err, identity.ErrInvalidPrincipal):
		return errors.Join(kernel.Reject(kernel.CodePermissionDenied, "/", ""), err)
	}
	return err
}

func scopeOf(ctx context.Context) kernel.Scope {
	s, _ := bento.ScopeFromContext(ctx)
	return s
}
