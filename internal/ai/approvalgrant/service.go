package approvalgrant

import (
	"context"
	"errors"
	"reflect"
	"time"

	"go.idp.xyz/idp-bento-go/application"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/ai/capability"
	"go.idp.xyz/idp-brant/internal/ai/codes"
	"go.idp.xyz/idp-brant/internal/ai/policy"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Service runs the approval grant commands and uses on the AI layer's ledger (brant_ai).
type Service struct {
	Ledger   *action.Ledger
	Clock    application.Clock
	Registry *capability.Registry
}

// IssueGrant issues an approval grant. The checks after the ledger's (structure, human-only rule
// and permission, idempotency): the ID is new; the delegation exists and the issuer sees it
// (REFERENCE_INVALID), and it is active and unexpired (INVALID_STATE); the command is a registered
// capability (PERMISSION_DENIED for a human-only one, UNSUPPORTED_CONTRACT otherwise) that needs a
// grant and that the delegation allows (INVALID_KNOWN_VALUE); the expiry is in the future and at
// most MaxLifetime away (INVALID_KNOWN_VALUE); the issuer holds the command's permission
// (PERMISSION_DENIED): nobody approves for the AI what they could not do themselves.
func (s Service) IssueGrant(ctx context.Context, cmd IssueGrant) (Receipt, error) {
	cmd.ExpiresAt = kernel.Instant(cmd.ExpiresAt)
	if err := cmd.Validate(); err != nil {
		return Receipt{}, s.rejectEarly(ctx, CommandIssue, cmd.GrantID, err)
	}
	args := cmd
	args.IdempotencyKey = ""
	hash, err := action.RequestHash(args)
	if err != nil {
		return Receipt{}, err
	}
	res, err := s.Ledger.Execute(ctx, action.Command{Name: CommandIssue, IdempotencyKey: cmd.IdempotencyKey, RequestHash: hash,
		ObjectID: cmd.GrantID}, &issueHandler{s: s, cmd: cmd})
	return Receipt(res), err
}

// RevokeGrant revokes an issued grant the revoker sees, at its current version; a used or revoked
// grant is INVALID_STATE.
func (s Service) RevokeGrant(ctx context.Context, cmd RevokeGrant) (Receipt, error) {
	if err := cmd.Validate(); err != nil {
		return Receipt{}, s.rejectEarly(ctx, CommandRevoke, cmd.GrantID, err)
	}
	args := cmd
	args.IdempotencyKey, args.ExpectedVersion = "", 0
	hash, err := action.RequestHash(args)
	if err != nil {
		return Receipt{}, err
	}
	res, err := s.Ledger.Execute(ctx, action.Command{Name: CommandRevoke, IdempotencyKey: cmd.IdempotencyKey, RequestHash: hash,
		ObjectID: cmd.GrantID, Reason: cmd.Reason}, &revokeHandler{s: s, cmd: cmd})
	return Receipt(res), err
}

// GetGrant returns a grant the caller sees (its agent, or a reader whose data scope covers the
// issuer), audited as ai.approval_grant.get (ai.read); NOT_FOUND otherwise.
func (s Service) GetGrant(ctx context.Context, id string) (Grant, error) {
	var out Grant
	err := s.Ledger.Read(ctx, CommandGet, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		g, err := Load(ctx, exec, scopeOf(ctx), id, policy.NoLock)
		if err != nil {
			return err
		}
		if g == nil || !g.SeenBy(d) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		out = *g
		return nil
	})
	return out, err
}

// GrantsOf returns the grants of a delegation the caller sees, in issue order, audited as
// ai.approval_grant.get (ai.read) of the delegation. The execution entry finds here the grant a
// human issued for a call it proposed (same command and request hash, status issued).
func (s Service) GrantsOf(ctx context.Context, delegationID string) ([]Grant, error) {
	var out []Grant
	err := s.Ledger.Read(ctx, CommandGet, delegationID, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		del, err := policy.Load(ctx, exec, scopeOf(ctx), delegationID, policy.NoLock)
		if err != nil {
			return err
		}
		if del == nil || !del.SeenBy(d) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		all, err := OfDelegation(ctx, exec, scopeOf(ctx), delegationID)
		for _, g := range all {
			if g.SeenBy(d) {
				out = append(out, g)
			}
		}
		return err
	})
	return out, err
}

// Visible reports whether the actor of d sees grant id, for replays and reconciliations of the
// grant commands.
func Visible(ctx context.Context, exec bentopg.Executor, d authorization.Decision, id string) (bool, error) {
	g, err := Load(ctx, exec, scopeOf(ctx), id, policy.NoLock)
	return err == nil && g != nil && g.SeenBy(d), err
}

func (s Service) rejectEarly(ctx context.Context, command, objectID string, rejection error) error {
	if rej, ok := kernel.AsRejection(rejection); ok {
		if err := s.Ledger.AuditRejected(ctx, command, objectID, rej.Code); err != nil {
			return errors.Join(rejection, err)
		}
	}
	return rejection
}

type issueHandler struct {
	s      Service
	cmd    IssueGrant
	exists bool
	del    *policy.Delegation
}

// Lock locks the grant's row (when the ID is taken) FOR UPDATE and its delegation FOR SHARE.
func (h *issueHandler) Lock(ctx context.Context, exec bentopg.Executor) error {
	g, err := Load(ctx, exec, scopeOf(ctx), h.cmd.GrantID, policy.ForUpdate)
	if err != nil {
		return err
	}
	h.exists = g != nil
	h.del, err = policy.Load(ctx, exec, scopeOf(ctx), h.cmd.DelegationID, policy.ForShare)
	return err
}

func (h *issueHandler) Apply(ctx context.Context, exec bentopg.Executor, dec authorization.Decision) (action.Outcome, error) {
	scope, now := scopeOf(ctx), h.s.Clock.Now()
	switch {
	case h.exists:
		return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/id", "")
	case h.del == nil || !h.del.SeenBy(dec):
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/delegation_id", "")
	case h.del.Status != policy.StatusActive || h.del.Expired(now):
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/delegation_id", "")
	}
	c, err := h.s.Registry.Lookup(scope.Tenant, h.cmd.Command, "/args/command")
	if err != nil {
		return action.Outcome{}, err
	}
	switch {
	case c.Approval != capability.ApprovalGrant || !h.del.Allows(c):
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/command", "")
	case !h.cmd.ExpiresAt.After(now) || h.cmd.ExpiresAt.Sub(now) > MaxLifetime:
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/expires_at", "")
	}
	issuer, err := authorization.Authorizer{}.Qualify(ctx, exec, dec.Actor.ID)
	if err != nil {
		return action.Outcome{}, err
	}
	if !issuer.Holds(c.Permission()) {
		return action.Outcome{}, kernel.Reject(kernel.CodePermissionDenied, "/args/command", "")
	}
	g := Grant{GrantID: h.cmd.GrantID, DelegationID: h.del.DelegationID, AgentID: h.del.AgentID, TaskID: h.del.TaskID,
		Command: c.Command, RequestHash: h.cmd.RequestHash, ObjectType: c.Object(reflect.Zero(c.Input()).Interface()).Type,
		ObjectID: h.cmd.ObjectID, ObjectVersion: h.cmd.ObjectVersion, ExpiresAt: h.cmd.ExpiresAt, Status: StatusIssued, RowVersion: 1,
		IssuerID: dec.Actor.ID, OwnerTeamID: dec.Team, IssuedEpoch: dec.Epoch, IssuedAt: now}
	if err = insert(ctx, exec, scope, g); err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: g.GrantID, RowVersion: 1, Status: StatusIssued}, nil
}

type revokeHandler struct {
	s   Service
	cmd RevokeGrant
	g   *Grant
}

func (h *revokeHandler) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.g, err = Load(ctx, exec, scopeOf(ctx), h.cmd.GrantID, policy.ForUpdate)
	return err
}

func (h *revokeHandler) Apply(ctx context.Context, exec bentopg.Executor, dec authorization.Decision) (action.Outcome, error) {
	switch {
	case h.g == nil || !h.g.SeenBy(dec):
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/id", "")
	case h.g.RowVersion != h.cmd.ExpectedVersion:
		return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	case h.g.Status != StatusIssued:
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/id", "")
	}
	g := *h.g
	g.Status, g.RowVersion = StatusRevoked, g.RowVersion+1
	g.RevokedByID, g.RevokedAt, g.RevokeReason = dec.Actor.ID, h.s.Clock.Now(), h.cmd.Reason
	if err := revoke(ctx, exec, scopeOf(ctx), g, dec.Epoch); err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: g.GrantID, RowVersion: g.RowVersion, Status: StatusRevoked}, nil
}

// Use is one call that needs an approval grant, as the gateway admits it: everything in it comes
// from the platform (the grant reference from TSK, the rest from the decoded call), never from
// model output (AI-INV-004).
type Use struct {
	GrantID     string
	Step        policy.Step
	RequestHash string
	Object      capability.ObjectRef
	Key         string // the idempotency key the call is sent with
}

// Use uses grant u.GrantID for the call u describes, in a brant_ai transaction of its own, and
// returns the grant as used and the authority the step saw. In order:
//
//   - the actor of ctx is an AI actor (PERMISSION_DENIED);
//   - the grant exists in the scope and is for the actor's delegation and task (GRANT_INVALID);
//   - the delegation allows the step now (policy.CheckIn: GRANT_INVALID, GRANT_EXPIRED,
//     PERMISSION_DENIED for a revoked initiator or agent);
//   - the grant binds exactly this command, request hash, object and object version, and it is
//     neither revoked nor used by another call (GRANT_INVALID), nor expired (GRANT_EXPIRED);
//   - the issuer is an active human who still holds the command's permission (PERMISSION_DENIED).
//
// An issued grant becomes used with the call's key; a grant this call (the same key) used already
// is answered again without change, so a resend after an uncertain outcome is the same use. The
// grant row is locked FOR UPDATE first, then the delegation and the authority guard rows FOR SHARE:
// of two concurrent uses only one succeeds. Every outcome is audited: a refusal under the call's
// command, a use as ai.approval_grant.use.
func (s Service) Use(ctx context.Context, u Use) (Grant, policy.Authority, error) {
	p, err := identity.FromContext(ctx)
	if err != nil {
		return Grant{}, policy.Authority{}, err
	}
	if !kernel.ValidIdentifier(u.Key) {
		return Grant{}, policy.Authority{}, errors.New("approvalgrant: a use needs the call's idempotency key")
	}
	ctx = action.WithCorrelation(ctx, action.Correlation(ctx))
	var g Grant
	var auth policy.Authority
	err = policy.WithinScope(ctx, s.Ledger.DB, p.Scope, func(ctx context.Context, exec bentopg.Executor) error {
		now := s.Clock.Now()
		if p.Actor.Kind != kernel.ActorAI {
			return kernel.Reject(kernel.CodePermissionDenied, "/", "approval grants are for AI actors")
		}
		cur, err := Load(ctx, exec, p.Scope, u.GrantID, policy.ForUpdate)
		if err != nil {
			return err
		}
		if cur == nil || cur.AgentID != p.Actor.ID || cur.DelegationID != u.Step.DelegationID || cur.TaskID != u.Step.TaskID {
			return codes.Reject(codes.GrantInvalid, "/grant")
		}
		if auth, err = policy.CheckIn(ctx, exec, p, u.Step, now); err != nil {
			return err
		}
		switch {
		case cur.Command != u.Step.Capability.Command || cur.RequestHash != u.RequestHash || cur.ObjectType != u.Object.Type ||
			cur.ObjectID != u.Object.ID || cur.ObjectVersion != u.Object.Version:
			return codes.Reject(codes.GrantInvalid, "/grant")
		case cur.Status == StatusRevoked, cur.Status == StatusUsed && cur.UsedKey != u.Key:
			return codes.Reject(codes.GrantInvalid, "/grant")
		case !now.Before(cur.ExpiresAt):
			return codes.Reject(codes.GrantExpired, "/grant")
		}
		issuer, err := authorization.Authorizer{}.Qualify(ctx, exec, cur.IssuerID)
		if err != nil {
			return err
		}
		if !issuer.Holds(u.Step.Capability.Permission()) || issuer.Actor.Kind != kernel.ActorHuman {
			return kernel.Reject(kernel.CodePermissionDenied, "/grant", "the issuer's grant")
		}
		outcome := "replayed"
		if cur.Status == StatusIssued {
			cur.Status, cur.RowVersion = StatusUsed, cur.RowVersion+1
			cur.UsedKey, cur.UsedByID, cur.UsedEpoch, cur.UsedAt = u.Key, p.Actor.ID, auth.Epoch, kernel.Instant(now)
			if err = markUsed(ctx, exec, p.Scope, *cur); err != nil {
				return err
			}
			outcome = "succeeded"
		}
		g = *cur
		return audit(ctx, exec, p, CommandUse, cur.GrantID, outcome, auth.Epoch, now)
	})
	if err != nil {
		return Grant{}, policy.Authority{}, policy.Service{Ledger: s.Ledger}.Audited(ctx, u.Step.Capability.Command, u.GrantID, err)
	}
	return g, auth, nil
}

// audit records an outcome in the AI layer's audit trail inside the caller's transaction.
func audit(ctx context.Context, exec bentopg.Executor, p identity.Principal, command, objectID, outcome string, epoch int64, now time.Time) error {
	at := kernel.Instant(now)
	_, err := exec.Exec(ctx, `INSERT INTO brant_ai.audit_event (tenant_id, company_id, event_id, actor_kind, actor_id, command,
		object_id, outcome, correlation_id, recorded_at, authority_epoch) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		string(p.Scope.Tenant), string(p.Scope.Company), action.NewID("aud", at), string(p.Actor.Kind), p.Actor.ID, command,
		objectID, outcome, action.Correlation(ctx), at, epoch)
	return err
}

func scopeOf(ctx context.Context) kernel.Scope {
	s, _ := bento.ScopeFromContext(ctx)
	return s
}
