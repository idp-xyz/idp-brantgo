// Package policy holds the AI layer's delegations (UL AI-Policy; AI-INV-002; plan S4-03 policy): a
// human initiator lets one AI execution identity call listed capabilities for one task, up to an
// autonomy level, until an expiry. Without a delegation nothing is allowed (default deny), and every
// step re-checks, at that moment, the delegation and the current grants of both the initiator and
// the agent (baseline 8.7: 长任务继续执行时重新检查必要权限；用户离职、角色撤销或委托过期不能沿用旧授权;
// AN04). A delegation never replaces the owning context's authorization and guards.
//
// The delegation commands ai.delegation.grant and ai.delegation.revoke are human-only (Context Map
// platformCommands AI; tactical humanOnlyCommands); they run on the AI layer's ledger in brant_ai.
package policy

import (
	"strconv"
	"time"
	"unicode/utf8"

	"go.idp.xyz/idp-brant/internal/ai/capability"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

const (
	// CommandGrant grants a delegation (human-only).
	CommandGrant = "ai.delegation.grant"
	// CommandRevoke revokes a delegation (human-only).
	CommandRevoke = "ai.delegation.revoke"
	// CommandGet is the audited read of a delegation and the read a replay of the AI layer's
	// commands needs (permission ai.read).
	CommandGet = "ai.delegation.get"
	// PermissionRead reads delegations, approval grants and execution evidence (role templates).
	PermissionRead = "ai.read"

	StatusActive  = "active"
	StatusRevoked = "revoked"

	// MaxLifetime bounds a delegation: no permanent super-authority (baseline 17.1, 不能借用…永久超级权限).
	MaxLifetime = 30 * 24 * time.Hour
	// MaxCapabilities bounds the capabilities of one delegation.
	MaxCapabilities = 64
	// MaxReasonCharacters bounds a revocation reason.
	MaxReasonCharacters = 500
)

// GrantDelegation lets AgentID, an AI execution identity, call Capabilities for task TaskID up to
// Autonomy (read, draft or submit) until ExpiresAt. The initiator is the human who issues the
// command; the delegation never allows more than the initiator holds.
type GrantDelegation struct {
	IdempotencyKey string
	DelegationID   string // args.id
	AgentID        string
	TaskID         string
	Capabilities   []string
	Autonomy       string
	ExpiresAt      time.Time
}

// Validate checks the closed contract.
func (c GrantDelegation) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.DelegationID):
		return schema("/args/id")
	case !kernel.ValidIdentifier(c.AgentID):
		return schema("/args/agent_id")
	case !kernel.ValidIdentifier(c.TaskID):
		return schema("/args/task_id")
	case len(c.Capabilities) == 0 || len(c.Capabilities) > MaxCapabilities:
		return schema("/args/capabilities")
	case !capability.SideEffect(c.Autonomy).Valid():
		return schema("/args/autonomy")
	case c.ExpiresAt.IsZero():
		return schema("/args/expires_at")
	}
	seen := map[string]bool{}
	for i, name := range c.Capabilities {
		if !kernel.ValidIdentifier(name) || seen[name] {
			return schema("/args/capabilities/" + strconv.Itoa(i))
		}
		seen[name] = true
	}
	return nil
}

// RevokeDelegation ends delegation DelegationID at ExpectedVersion for Reason. The agent's next
// step under it is refused; what it committed before stays.
type RevokeDelegation struct {
	IdempotencyKey  string
	DelegationID    string // args.id
	ExpectedVersion int64
	Reason          string
}

// Validate checks the closed contract.
func (c RevokeDelegation) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.DelegationID):
		return schema("/args/id")
	case c.ExpectedVersion < 1:
		return schema("/args/expected_version")
	case !Reason(c.Reason):
		return schema("/args/reason")
	}
	return nil
}

// Reason reports whether s is a recorded reason: 1 to MaxReasonCharacters characters of valid UTF-8.
func Reason(s string) bool {
	n := utf8.RuneCountInString(s)
	return utf8.ValidString(s) && n >= 1 && n <= MaxReasonCharacters
}

func schema(path string) error { return kernel.Reject(kernel.CodeSchemaInvalid, path, "") }

// Receipt is the closed success answer of a command.
type Receipt struct {
	ActionID   string
	ObjectID   string
	RowVersion int64
	Status     string
	Replayed   bool
}

// Delegation is a delegation as recorded.
type Delegation struct {
	DelegationID string
	InitiatorID  string
	AgentID      string
	TaskID       string
	Autonomy     capability.SideEffect
	Capabilities []string
	ExpiresAt    time.Time
	Status       string
	RowVersion   int64
	OwnerTeamID  string
	GrantedEpoch int64
	GrantedAt    time.Time
	RevokedByID  string
	RevokedAt    time.Time
	RevokeReason string
}

// Allows reports whether the delegation lists capability c within its autonomy.
func (d Delegation) Allows(c capability.Capability) bool {
	for _, name := range d.Capabilities {
		if name == c.Command {
			return c.SideEffect.Within(d.Autonomy)
		}
	}
	return false
}

// Expired reports whether the delegation has expired at now.
func (d Delegation) Expired(now time.Time) bool { return !now.Before(d.ExpiresAt) }

// SeenBy reports whether the actor of decision sees the delegation: its agent, or a reader whose
// data scope covers the initiator's records (BC-D02).
func (d Delegation) SeenBy(decision authorization.Decision) bool {
	return decision.Actor.ID == d.AgentID || decision.Sees(d.InitiatorID, d.OwnerTeamID)
}

var anyActor = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem}

// Rules are the authorization rules of the delegation commands and reads.
func Rules() []authorization.Rule {
	return []authorization.Rule{
		authorization.HumanOnly(CommandGrant, CommandGrant),
		authorization.HumanOnly(CommandRevoke, CommandRevoke),
		{Command: CommandGet, Permission: PermissionRead, ActorKinds: anyActor},
	}
}
