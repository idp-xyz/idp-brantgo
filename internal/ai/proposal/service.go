package proposal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.idp.xyz/idp-bento-go/application"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/ai/policy"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Service is the audited read of proposals, on the AI layer's ledger (brant_ai).
type Service struct {
	Ledger *action.Ledger
}

// Get returns a proposal the caller sees (the agent that proposed it, or a reader whose data scope
// covers the initiator of its delegation), audited as ai.proposal.get (ai.read); NOT_FOUND otherwise.
func (s Service) Get(ctx context.Context, id string) (Proposal, error) {
	var out Proposal
	err := s.Ledger.Read(ctx, CommandGet, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		scope, err := scopeOf(ctx)
		if err != nil {
			return err
		}
		p, err := Load(ctx, exec, scope, id, policy.NoLock)
		if err != nil {
			return err
		}
		if p == nil || !p.SeenBy(d) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		out = *p
		return nil
	})
	return out, err
}

func scopeOf(ctx context.Context) (kernel.Scope, error) {
	p, err := identity.FromContext(ctx)
	return p.Scope, err
}

// Store is what the agent runner does with proposals: make one, claim it for a grant, settle it. It
// runs in brant_ai in the scope of the Principal of each call, in transactions of its own, and decides
// nothing about authority: the approval grant and the delegation are the gateway's.
type Store struct {
	DB    *bentopg.DB
	Clock application.Clock
}

func (s Store) within(ctx context.Context, fn func(context.Context, bentopg.Executor, kernel.Scope) error) error {
	scope, err := scopeOf(ctx)
	if err != nil {
		return err
	}
	return policy.WithinScope(ctx, s.DB, scope, func(ctx context.Context, exec bentopg.Executor) error { return fn(ctx, exec, scope) })
}

// New is a request the agent proposes under a delegation: the proposal's ID, the command, the request
// hash and the object an approval grant binds, and the arguments as a tool call's.
type New struct {
	ProposalID    string
	TaskID        string
	Delegation    policy.Delegation
	Command       string
	RequestHash   string
	ObjectType    string
	ObjectID      string
	ObjectVersion int64
	Arguments     json.RawMessage
}

// ErrInvalid is a proposal that cannot be stored.
var ErrInvalid = errors.New("proposal: invalid")

// ErrNotFound is a proposal the scope does not have, and ErrSettled one that is not in the state the
// step needs (another runner settled or released it first).
var (
	ErrTooLong  = errors.New("proposal: the request is longer than a proposal keeps")
	ErrNotFound = errors.New("proposal: not found")
	ErrSettled  = errors.New("proposal: not in the state the step needs")
)

func (n New) check() error {
	bad := func(what string) error { return fmt.Errorf("%w: %s", ErrInvalid, what) }
	var members map[string]json.RawMessage
	switch {
	case !kernel.ValidIdentifier(n.ProposalID) || !kernel.ValidIdentifier(n.TaskID):
		return bad("proposal or task")
	case n.Delegation.DelegationID == "" || n.Delegation.TaskID != n.TaskID:
		return bad("the delegation is not the task's")
	case n.Command == "" || len(n.RequestHash) != 64 || n.ObjectType == "" || !kernel.ValidIdentifier(n.ObjectID) || n.ObjectVersion < 0:
		return bad("request or object")
	case len(n.Arguments) == 0 || len(n.Arguments) > MaxArguments:
		if len(n.Arguments) > MaxArguments {
			return fmt.Errorf("%w: %d bytes", ErrTooLong, len(n.Arguments))
		}
		return bad("arguments are empty")
	case json.Unmarshal(n.Arguments, &members) != nil || members == nil:
		return bad("arguments are not one JSON object")
	}
	if _, has := members["IdempotencyKey"]; has {
		return bad("a proposal carries no idempotency key")
	}
	return nil
}

// Propose records the request a delegation's agent proposed, pending a person's approval grant, and
// returns it with whether this call made it pending: the first proposal of the request, or a refused
// or void one proposed again (the next attempt, under the delegation now). A proposal that is
// pending, being sent or sent is returned as it is and not made again. The Principal of ctx is the
// agent.
func (s Store) Propose(ctx context.Context, n New) (Proposal, bool, error) {
	if err := n.check(); err != nil {
		return Proposal{}, false, err
	}
	var out Proposal
	var made bool
	err := s.within(ctx, func(ctx context.Context, exec bentopg.Executor, scope kernel.Scope) error {
		cur, err := Load(ctx, exec, scope, n.ProposalID, policy.ForUpdate)
		if err != nil {
			return err
		}
		now := kernel.Instant(s.Clock.Now())
		switch {
		case cur == nil:
			p := Proposal{ProposalID: n.ProposalID, TaskID: n.TaskID, DelegationID: n.Delegation.DelegationID, AgentID: n.Delegation.AgentID,
				InitiatorID: n.Delegation.InitiatorID, OwnerTeamID: n.Delegation.OwnerTeamID, Command: n.Command, RequestHash: n.RequestHash,
				ObjectType: n.ObjectType, ObjectID: n.ObjectID, ObjectVersion: n.ObjectVersion, Arguments: n.Arguments, Status: StatusPending,
				RowVersion: 1, Attempts: 1, ProposedAt: now}
			if err = insert(ctx, exec, scope, p); err != nil {
				return err
			}
			made = true
			out, err = reload(ctx, exec, scope, n.ProposalID)
			return err
		case cur.Command != n.Command || cur.RequestHash != n.RequestHash || cur.TaskID != n.TaskID || cur.ObjectID != n.ObjectID ||
			cur.ObjectVersion != n.ObjectVersion:
			return fmt.Errorf("%w: proposal %s is another request", ErrInvalid, n.ProposalID)
		case cur.Status == StatusRefused || cur.Status == StatusVoid:
			p := *cur
			p.Status, p.RowVersion, p.Attempts, p.Sends = StatusPending, cur.RowVersion+1, cur.Attempts+1, 0
			p.ClaimedAt, p.GrantID, p.SettledAt, p.OutcomeCode, p.OutcomePath, p.ActionID = time.Time{}, "", time.Time{}, "", "", ""
			p.DelegationID, p.InitiatorID, p.OwnerTeamID = n.Delegation.DelegationID, n.Delegation.InitiatorID, n.Delegation.OwnerTeamID
			if err = update(ctx, exec, scope, p); err != nil {
				return err
			}
			made = true
			out, err = reload(ctx, exec, scope, n.ProposalID)
			return err
		default:
			out = *cur
			return nil
		}
	})
	return out, made, err
}

func reload(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) (Proposal, error) {
	p, err := Load(ctx, exec, scope, id, policy.NoLock)
	if err != nil || p == nil {
		return Proposal{}, errors.Join(err, errors.New("proposal: not found after it was written"))
	}
	return *p, nil
}

// Get returns proposal id of the caller's scope, or nil, without an audit entry: the runner's own
// look at what it stored.
func (s Store) Get(ctx context.Context, id string) (*Proposal, error) {
	var out *Proposal
	err := s.within(ctx, func(ctx context.Context, exec bentopg.Executor, scope kernel.Scope) error {
		var err error
		out, err = Load(ctx, exec, scope, id, policy.NoLock)
		return err
	})
	return out, err
}

// OfTask returns the proposals of a task of the caller's scope in the order they were proposed.
func (s Store) OfTask(ctx context.Context, taskID string) ([]Proposal, error) {
	var out []Proposal
	err := s.within(ctx, func(ctx context.Context, exec bentopg.Executor, scope kernel.Scope) error {
		var err error
		out, err = OfTask(ctx, exec, scope, taskID)
		return err
	})
	return out, err
}

// Claim takes a proposal for sending it under approval grant grantID: a pending one, or a sending one
// whose claim went stale. It returns the claimed proposal, and false when another runner holds it, it
// is settled, or it is not the one the caller loaded (version). The claim is the one place two
// runners that resume the same task at once are told apart: the row is locked, so one of them claims
// and the other finds it claimed.
func (s Store) Claim(ctx context.Context, id, grantID string) (Proposal, bool, error) {
	var out Proposal
	var ok bool
	err := s.within(ctx, func(ctx context.Context, exec bentopg.Executor, scope kernel.Scope) error {
		cur, err := Load(ctx, exec, scope, id, policy.ForUpdate)
		if err != nil || cur == nil {
			return err
		}
		now := kernel.Instant(s.Clock.Now())
		if cur.Status != StatusPending && !cur.Stale(now) {
			out = *cur
			return nil
		}
		p := *cur
		p.Status, p.RowVersion, p.Sends, p.ClaimedAt, p.GrantID = StatusSending, cur.RowVersion+1, cur.Sends+1, now, grantID
		if !p.ClaimedAt.After(cur.ClaimedAt) {
			p.ClaimedAt = cur.ClaimedAt.Add(time.Microsecond)
		}
		if err = update(ctx, exec, scope, p); err != nil {
			return err
		}
		ok = true
		out, err = reload(ctx, exec, scope, id)
		return err
	})
	return out, ok, err
}

// Release gives a claimed proposal back, pending: the target did not answer and the same request is
// sent again by the next pass.
func (s Store) Release(ctx context.Context, id string) error {
	return s.step(ctx, id, []string{StatusSending}, func(p *Proposal, _ time.Time) error {
		p.Status, p.ClaimedAt, p.GrantID = StatusPending, time.Time{}, ""
		return nil
	})
}

// Outcome is what became of a claimed proposal: sent (the TSK action that committed it), refused (the
// owning context refused it) or void (the admission refused it). Code and Path are a refusal's.
type Outcome struct {
	Status   string
	Code     string
	Path     string
	ActionID string
}

func (o Outcome) check() error {
	switch {
	case o.Status == StatusSent && o.ActionID != "" && o.Code == "":
	case (o.Status == StatusRefused || o.Status == StatusVoid) && o.Code != "":
	default:
		return fmt.Errorf("%w: outcome %+v", ErrInvalid, o)
	}
	return nil
}

// Settle records what became of a proposal, once: a claimed one, or, for sent, a pending one whose
// request was committed without a claim (the target's ledger answered before the platform sent it).
func (s Store) Settle(ctx context.Context, id string, o Outcome) error {
	if err := o.check(); err != nil {
		return err
	}
	allowed := []string{StatusSending}
	if o.Status == StatusSent || o.Status == StatusVoid {
		allowed = append(allowed, StatusPending)
	}
	return s.step(ctx, id, allowed, func(p *Proposal, now time.Time) error {
		p.Status, p.SettledAt, p.OutcomeCode, p.OutcomePath, p.ActionID = o.Status, now, o.Code, o.Path, o.ActionID
		return nil
	})
}

// step changes a proposal that has one of the allowed statuses, under its row lock.
func (s Store) step(ctx context.Context, id string, allowed []string, change func(*Proposal, time.Time) error) error {
	return s.within(ctx, func(ctx context.Context, exec bentopg.Executor, scope kernel.Scope) error {
		cur, err := Load(ctx, exec, scope, id, policy.ForUpdate)
		if err != nil {
			return err
		}
		switch {
		case cur == nil:
			return fmt.Errorf("%w: %s", ErrNotFound, id)
		case !slices.Contains(allowed, cur.Status):
			return fmt.Errorf("%w: proposal %s is %s, not %s", ErrSettled, id, cur.Status, strings.Join(allowed, " or "))
		}
		p := *cur
		p.RowVersion = cur.RowVersion + 1
		if err = change(&p, kernel.Instant(s.Clock.Now())); err != nil {
			return err
		}
		return update(ctx, exec, scope, p)
	})
}
