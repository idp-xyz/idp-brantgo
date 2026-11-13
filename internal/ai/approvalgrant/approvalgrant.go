// Package approvalgrant holds the AI layer's approval grants (UL AI-ApprovalGrant; AI-INV-002): a
// human lets the agent of one delegation make exactly one call of a capability that needs a grant
// (D-7: order.prepare, order.review.request). The grant binds the tenant and company (its scope),
// the delegation and so the agent and the task, the command, the request hash of the call, the
// object and its version, and an expiry; it is used once. It is not an approval of a quote, an
// order or a purchase commitment: those stay the owning contexts' human decisions.
//
// ai.approval_grant.issue and ai.approval_grant.revoke are human-only (Context Map platformCommands
// AI). A use is not a command: the gateway uses a grant for a call it admits, recording the call's
// idempotency key, so a resend of the same call (same key, BC-AT19) is the same use and any other
// call is refused.
package approvalgrant

import (
	"time"

	"go.idp.xyz/idp-brant/internal/ai/policy"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

const (
	// CommandIssue issues an approval grant (human-only).
	CommandIssue = "ai.approval_grant.issue"
	// CommandRevoke revokes an unused approval grant (human-only).
	CommandRevoke = "ai.approval_grant.revoke"
	// CommandGet is the audited read of approval grants (permission ai.read).
	CommandGet = "ai.approval_grant.get"
	// CommandUse is the name a use of a grant has in the audit trail.
	CommandUse = "ai.approval_grant.use"

	StatusIssued  = "issued"
	StatusUsed    = "used"
	StatusRevoked = "revoked"

	// MaxLifetime bounds an approval grant.
	MaxLifetime = 7 * 24 * time.Hour
)

// IssueGrant lets the agent of delegation DelegationID make one call of Command with request hash
// RequestHash on object ObjectID at ObjectVersion until ExpiresAt. The request hash is the
// capability's (capability.Capability.RequestHash), which the execution entry shows the issuer
// with the call it proposes.
type IssueGrant struct {
	IdempotencyKey string
	GrantID        string // args.id
	DelegationID   string
	Command        string
	RequestHash    string
	ObjectID       string
	ObjectVersion  int64
	ExpiresAt      time.Time
}

// Validate checks the closed contract.
func (c IssueGrant) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.GrantID):
		return schema("/args/id")
	case !kernel.ValidIdentifier(c.DelegationID):
		return schema("/args/delegation_id")
	case !kernel.ValidIdentifier(c.Command):
		return schema("/args/command")
	case !hash(c.RequestHash):
		return schema("/args/request_hash")
	case !kernel.ValidIdentifier(c.ObjectID):
		return schema("/args/object_id")
	case c.ObjectVersion < 0:
		return schema("/args/object_version")
	case c.ExpiresAt.IsZero():
		return schema("/args/expires_at")
	}
	return nil
}

// RevokeGrant revokes unused grant GrantID at ExpectedVersion for Reason.
type RevokeGrant struct {
	IdempotencyKey  string
	GrantID         string // args.id
	ExpectedVersion int64
	Reason          string
}

// Validate checks the closed contract.
func (c RevokeGrant) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.GrantID):
		return schema("/args/id")
	case c.ExpectedVersion < 1:
		return schema("/args/expected_version")
	case !policy.Reason(c.Reason):
		return schema("/args/reason")
	}
	return nil
}

func hash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func schema(path string) error { return kernel.Reject(kernel.CodeSchemaInvalid, path, "") }

// Receipt is the closed success answer of a command.
type Receipt = policy.Receipt

// Grant is an approval grant as recorded.
type Grant struct {
	GrantID       string
	DelegationID  string
	AgentID       string
	TaskID        string
	Command       string
	RequestHash   string
	ObjectType    string
	ObjectID      string
	ObjectVersion int64
	ExpiresAt     time.Time
	Status        string
	RowVersion    int64
	IssuerID      string
	OwnerTeamID   string
	IssuedEpoch   int64
	IssuedAt      time.Time
	UsedKey       string // the idempotency key of the call that used it
	UsedByID      string
	UsedEpoch     int64
	UsedAt        time.Time
	RevokedByID   string
	RevokedAt     time.Time
	RevokeReason  string
}

// SeenBy reports whether the actor of decision sees the grant: its agent, or a reader whose data
// scope covers the issuer's records (BC-D02).
func (g Grant) SeenBy(decision authorization.Decision) bool {
	return decision.Actor.ID == g.AgentID || decision.Sees(g.IssuerID, g.OwnerTeamID)
}

var anyActor = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem}

// Rules are the authorization rules of the approval grant commands and reads.
func Rules() []authorization.Rule {
	return []authorization.Rule{
		authorization.HumanOnly(CommandIssue, CommandIssue),
		authorization.HumanOnly(CommandRevoke, CommandRevoke),
		{Command: CommandGet, Permission: policy.PermissionRead, ActorKinds: anyActor},
	}
}
