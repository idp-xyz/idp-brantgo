// Package proposal holds the requests an AI agent proposed and a person is asked to approve (S4
// G7b; owner decision 2026-11-18 20:15: the platform keeps the request awaiting approval, the owning
// context judges it without writing before a person is asked, the approver sees its content, and
// after the approval the platform sends exactly it and does not ask the model again).
//
// A command that needs an approval grant (order.prepare, order.review.request) used to wait with its
// request hash only. A person approved a digest, nothing kept the request, and after the grant the
// model rebuilt it: a slightly different request had another hash and needed another grant. A
// proposal is the request itself, as the arguments of the tool call the agent made without the
// idempotency key, stored once TRD has judged it. It is the same request the grant binds: its hash is
// the capability's RequestHash, its object and version the grant's.
//
// A proposal is never authority. The approval grant is a person's, bound to the tenant, company,
// command, request hash, object, version and expiry, used once (AI-INV-002); the delegation and the
// task are checked again when the proposal is sent, by the same gateway and executor a tool call goes
// through, and the owning context decides as it does for any caller. A proposal records what was
// asked and what became of it.
//
// A proposal has one row for the task, the command, the request hash and the object version the grant
// binds (execution.ProposalID; an expected version is no part of the hash, so a request proposed
// again at another version is another proposal). Its request never changes. Its status:
//
//	pending  waiting for a person's approval grant
//	sending  a runner claimed it for a grant and is sending it (a claim that goes stale is taken over)
//	sent     the owning context committed it
//	refused  the owning context refused it after the grant (what only its transaction can judge)
//	void     the admission refused it: the delegation, the grant or the task no longer allowed it
//
// A refused or void proposal that the agent proposes again, and that the owning context then accepts,
// is pending again with the next attempt: a request a person approved and the context refused does
// not need to be rebuilt to be asked again, and no grant is asked for it before the context accepts.
// A proposal is kept for good, like the grants and the model calls: it is the record of what a person
// approved. It holds the request's own arguments and nothing else of the run.
package proposal

import (
	"encoding/json"
	"time"

	"go.idp.xyz/idp-brant/internal/ai/policy"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

const (
	// CommandGet is the audited read of proposals (permission ai.read).
	CommandGet = "ai.proposal.get"

	StatusPending = "pending"
	StatusSending = "sending"
	StatusSent    = "sent"
	StatusRefused = "refused"
	StatusVoid    = "void"

	// ClaimLease is how long a runner's claim holds. A runner that claimed a proposal and went away
	// leaves it sending; once the lease has passed another runner takes it over. Sending is
	// idempotent at every step (the action's key, the grant's use, the target's ledger), so a taken
	// over claim never sends twice; a send takes seconds, so a crashed runner's request waits half a
	// minute at most.
	ClaimLease = 30 * time.Second

	// MaxArguments bounds the stored arguments (the table's check): the request of a command that
	// needs a grant is a handful of identifiers.
	MaxArguments = 8192
)

// Proposal is one proposed request and what became of it.
type Proposal struct {
	ProposalID    string
	TaskID        string
	DelegationID  string // the delegation the agent proposed under
	AgentID       string
	InitiatorID   string // the delegation's initiator; whoever sees it sees the proposal
	OwnerTeamID   string
	Command       string
	RequestHash   string // capability.RequestHash of the stored arguments: what the approval grant binds
	ObjectType    string
	ObjectID      string
	ObjectVersion int64
	// Arguments are the request as the arguments of the agent's tool call, in the Go names of the
	// tool's schema and without the idempotency key.
	Arguments  json.RawMessage
	Status     string
	RowVersion int64
	Attempts   int // how often the request was proposed: 1, and one more for each time it was proposed again after a refusal
	Sends      int // how often it was claimed for sending in this attempt
	ProposedAt time.Time
	ClaimedAt  time.Time // when the current or last claim began
	GrantID    string    // the approval grant it was claimed for
	SettledAt  time.Time
	// OutcomeCode and OutcomePath are the refusal's of a refused or void proposal: the code and the
	// request member it points at. ActionID is the TSK action of a sent one.
	OutcomeCode string
	OutcomePath string
	ActionID    string
}

// SeenBy reports whether the actor of decision sees the proposal, as it sees the delegation the agent
// proposed under: the agent itself, or a reader whose data scope covers the delegation's initiator
// (BC-D02).
func (p Proposal) SeenBy(decision authorization.Decision) bool {
	return decision.Actor.ID == p.AgentID || decision.Sees(p.InitiatorID, p.OwnerTeamID)
}

// Open reports whether the proposal can still be sent for a grant: waiting for one, or claimed by a
// runner that did not settle it.
func (p Proposal) Open() bool { return p.Status == StatusPending || p.Status == StatusSending }

// Stale reports whether the claim of a sending proposal has passed its lease at now.
func (p Proposal) Stale(now time.Time) bool {
	return p.Status == StatusSending && !now.Before(p.ClaimedAt.Add(ClaimLease))
}

var anyActor = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem}

// Rules are the authorization rules of the proposal read: ai.read, as the delegation's and the
// grant's reads are.
func Rules() []authorization.Rule {
	return []authorization.Rule{{Command: CommandGet, Permission: policy.PermissionRead, ActorKinds: anyActor}}
}
