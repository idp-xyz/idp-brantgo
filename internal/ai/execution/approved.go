package execution

import (
	"context"
	"errors"
	"strconv"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/ai/adapters/model"
	"go.idp.xyz/idp-brant/internal/ai/approvalgrant"
	"go.idp.xyz/idp-brant/internal/ai/capability"
	"go.idp.xyz/idp-brant/internal/ai/executionevidence"
	"go.idp.xyz/idp-brant/internal/ai/gateway"
	"go.idp.xyz/idp-brant/internal/ai/policy"
	"go.idp.xyz/idp-brant/internal/ai/proposal"
	tskapi "go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The proposal flow of a command that needs an approval grant (S4 G7b).
//
//	call ─ settled? ─ judged by the owning context? ─ proposal stored ─ the task waits ─ a person's grant
//	   ─ the runner resumes the task ─ the platform sends the stored request, as the model made it
//
// A call whose intent the owning context committed already replays its result. Otherwise the owning
// context judges the request without writing (gateway.Judge): a refusal is the call's, in the same
// turn, with no proposal, no wait and no grant asked. A request that passes is stored (proposal), and
// the task waits for a person's grant for exactly that request. When the grant is there the runner
// resumes the task and, before it asks the model, sends every approved proposal through the path a
// tool call takes from the grant on: the delegation and the task are checked again, the intent is
// recorded before the side effect, the grant is used with the action's key and the owning context
// decides. The model is not asked; what the owning context answered reaches its next turn as the
// task's actions and refusals.

// errInFlight is a proposal another runner is sending: the call neither waits nor sends.
var errInFlight = errors.New("execution: another runner is sending this proposal")

// ProposalID names a proposal: one per task, command, request and object version. The expected
// version is no part of the request hash (a version is the caller's view, not the request), but an
// approval grant binds it, so the same request at another version is another proposal.
func ProposalID(taskID, command, requestHash string, objectVersion int64) string {
	return "agp." + IntentOf(taskID, command, "v"+strconv.FormatInt(objectVersion, 10), requestHash)[3:]
}

// proposals is the runner's store of proposals, in brant_ai.
func (r *Runner) proposals() proposal.Store { return proposal.Store{DB: r.DB, Clock: r.Clock} }

// grantMatch is what an approval grant binds to be the grant of a request.
type grantMatch struct {
	Command       string
	RequestHash   string
	ObjectType    string
	ObjectID      string
	ObjectVersion int64
	// ResendKey, when set, also accepts the grant a send of this request used with that key: the
	// answer of an earlier send was lost, and the same key is the same use (approvalgrant.Service.Use).
	// It is set only for a request that was sent and whose outcome was not settled; a refused
	// request needs a new grant.
	ResendKey string
}

// matchOf is the grant of call c; resend when the request was sent before and its outcome was not
// settled.
func (t *turn) matchOf(c gateway.Call, resend bool) grantMatch {
	m := grantMatch{Command: c.Capability.Command, RequestHash: c.RequestHash, ObjectType: c.Object.Type, ObjectID: c.Object.ID,
		ObjectVersion: c.Object.Version}
	if resend {
		m.ResendKey = tskapi.ActionIDOf(IntentOf(t.view.TaskID, c.Capability.Command, c.Object.ID, c.RequestHash))
	}
	return m
}

// grantMatching returns the unexpired approval grant of the delegation for exactly the request m
// describes, issued, or used with m.ResendKey; nil when there is none.
func (r *Runner) grantMatching(ctx context.Context, scope kernel.Scope, delegationID string, m grantMatch) (*approvalgrant.Grant, error) {
	var out *approvalgrant.Grant
	now := r.Clock.Now()
	err := policy.WithinScope(ctx, r.DB, scope, func(ctx context.Context, exec bentopg.Executor) error {
		grants, err := approvalgrant.OfDelegation(ctx, exec, scope, delegationID)
		for _, g := range grants {
			open := g.Status == approvalgrant.StatusIssued || (g.Status == approvalgrant.StatusUsed && m.ResendKey != "" && g.UsedKey == m.ResendKey)
			if open && now.Before(g.ExpiresAt) && g.Command == m.Command && g.RequestHash == m.RequestHash && g.ObjectType == m.ObjectType &&
				g.ObjectID == m.ObjectID && g.ObjectVersion == m.ObjectVersion {
				g := g
				out = &g
				return err
			}
		}
		return err
	})
	return out, err
}

// delegation returns the active delegation of the turn with id.
func (t *turn) delegation(id string) (policy.Delegation, bool) {
	for _, d := range t.dels {
		if d.DelegationID == id {
			return d, true
		}
	}
	return policy.Delegation{}, false
}

// propose stores the request of call as a proposal pending a person's grant, or returns the one that
// is stored. The arguments are the tool call's, as the capability reads them back from the decoded
// command: what the owning context judged and what the grant's hash covers.
func (t *turn) propose(d policy.Delegation, c gateway.Call) (proposal.Proposal, error) {
	args, err := c.Capability.Arguments(c.Command)
	if err != nil {
		return proposal.Proposal{}, err
	}
	p, _, err := t.r.proposals().Propose(t.agent, proposal.New{
		ProposalID: ProposalID(t.view.TaskID, c.Capability.Command, c.RequestHash, c.Object.Version), TaskID: t.view.TaskID,
		Delegation: d, Command: c.Capability.Command, RequestHash: c.RequestHash, ObjectType: c.Object.Type, ObjectID: c.Object.ID,
		ObjectVersion: c.Object.Version, Arguments: args})
	if errors.Is(err, proposal.ErrTooLong) {
		return proposal.Proposal{}, kernel.Reject(kernel.CodeSchemaInvalid, "/args", "the request is longer than a proposal keeps")
	}
	return p, err
}

// closeSent records that the intent of a proposal is committed, when the proposal is still open: the
// target's ledger held it before this runner sent it (a send whose runner went away, or a call the
// model repeated).
func (t *turn) closeSent(id string, rcpt Receipt) error {
	err := t.r.proposals().Settle(t.agent, id, proposal.Outcome{Status: proposal.StatusSent, ActionID: rcpt.ActionID})
	if errors.Is(err, proposal.ErrSettled) || errors.Is(err, proposal.ErrNotFound) {
		return nil
	}
	return err
}

// sendApproved sends, without asking the model, the proposals of the task that a person approved.
// It reports whether it sent anything (the task's actions have changed then).
func (t *turn) sendApproved() (bool, error) {
	ps, err := t.r.proposals().OfTask(t.agent, t.view.TaskID)
	if err != nil {
		return false, err
	}
	sentAny := false
	for _, p := range ps {
		if !p.Open() {
			continue
		}
		did, err := t.sendProposal(p)
		sentAny = sentAny || did
		if err != nil || t.view.Lifecycle != tskapi.StateRunning {
			return sentAny, err
		}
	}
	return sentAny, nil
}

// sendProposal sends proposal p when a person approved exactly it, through the executor and gateway
// as a tool call is sent from its grant on, and records what became of it in the proposal and in the
// ledger of the task's calls (a tool call of the agent: the next turn's facts list its refusal).
func (t *turn) sendProposal(p proposal.Proposal) (bool, error) {
	task := t.view.TaskID
	intent := IntentOf(task, p.Command, p.ObjectID, p.RequestHash)
	if rcpt, ok, err := t.r.Executor.Settled(t.agent, task, intent); err != nil {
		return false, err
	} else if ok {
		t.res.Receipts = append(t.res.Receipts, rcpt)
		return false, t.closeSent(p.ProposalID, rcpt)
	}
	d, ok := t.delegation(p.DelegationID)
	if !ok {
		return false, t.voidProposal(p.ProposalID, kernel.CodePermissionDenied, "/delegation")
	}
	m := grantMatch{Command: p.Command, RequestHash: p.RequestHash, ObjectType: p.ObjectType, ObjectID: p.ObjectID, ObjectVersion: p.ObjectVersion}
	if p.Sends > 0 {
		m.ResendKey = tskapi.ActionIDOf(intent)
	}
	g, err := t.r.grantMatching(t.agent, t.scope, d.DelegationID, m)
	if err != nil || g == nil {
		return false, err
	}
	b := gateway.Binding{TaskID: task, DelegationID: d.DelegationID, GrantID: g.GrantID}
	c, err := t.r.Gateway.Reload(t.agent, b, p.Command, p.Arguments, p.RequestHash,
		capability.ObjectRef{Type: p.ObjectType, ID: p.ObjectID, Version: p.ObjectVersion})
	if err != nil {
		code, path := codeAndPath(err)
		return false, t.voidProposal(p.ProposalID, code, path)
	}
	for retry := false; ; retry = true {
		var reserved executionevidence.Call
		var reserveErr error
		res, err := t.dispatch(p.ProposalID, d, *g, c, func() error {
			reserved, reserveErr = t.r.Evidence.Reserve(t.agent, t.limits, executionevidence.Reservation{TaskID: task, TaskVersion: t.view.TaskVersion,
				Kind: executionevidence.KindTool, Retry: retry || p.Sends > 0, Bundle: t.r.ref, ToolCommand: p.Command})
			return reserveErr
		})
		if reserveErr != nil {
			return false, t.exhausted(reserveErr, retry, tskapi.PauseToolUnavailable)
		}
		if !res.Claimed {
			if res.Held {
				return false, errInFlight
			}
			return false, err
		}
		t.res.ToolCalls++
		zero, _ := kernel.NewMoney(t.limits.Currency, 0)
		o := executionevidence.Outcome{State: executionevidence.StateSucceeded, Cost: &zero, ActionID: res.Receipt.ActionID}
		switch {
		case res.Err == nil:
		case errors.Is(res.Err, model.ErrUnavailable):
			o.State, o.Code = executionevidence.StateUnavailable, "TOOL_UNAVAILABLE"
		default:
			o.State, o.Code = executionevidence.StateFailed, errorCode(res.Err)
			if _, ok := kernel.AsRejection(res.Err); ok {
				_, o.RefusalPath, o.RefusalReason = t.refusedAs(p.Command, p.Arguments, res.Err)
			}
		}
		if o.ActionID != "" && !kernel.ValidIdentifier(o.ActionID) {
			o.ActionID = ""
		}
		if _, serr := t.r.Evidence.Settle(t.agent, reserved.CallID, o); serr != nil {
			return true, errors.Join(serr, err)
		}
		switch {
		case res.Err == nil:
			t.res.Receipts = append(t.res.Receipts, res.Receipt)
			return true, errors.Join(err, t.refresh())
		case errors.Is(res.Err, model.ErrUnavailable):
			if err != nil {
				return true, err
			}
			continue
		}
		if _, ok := kernel.AsRejection(res.Err); !ok {
			return true, errors.Join(res.Err, err)
		}
		t.res.Refusals = append(t.res.Refusals, p.Command+" "+errorCode(res.Err))
		return true, errors.Join(err, t.refresh())
	}
}

// sent is what dispatching a proposal came to: whether this runner claimed it (Held: it did not,
// because another runner is sending it), the executor's receipt and the send's own answer (the owning
// context's refusal, an unavailable target).
type sent struct {
	Claimed bool
	Held    bool
	Receipt Receipt
	Err     error
}

// dispatch claims proposal id for the grant, runs call c through the executor and settles the
// proposal with what came of it: the one place a proposal is sent. The claim is a row lock and a state
// check, so of two runners that resume the same task one sends and the other finds the proposal
// claimed. reserve, when set, runs once the claim is held (the ledger's reservation of the call); its
// failure releases the claim. The error is the store's or the reservation's, never the send's.
func (t *turn) dispatch(id string, d policy.Delegation, g approvalgrant.Grant, c gateway.Call, reserve func() error) (sent, error) {
	store := t.r.proposals()
	cur, claimed, err := store.Claim(t.agent, id, g.GrantID)
	if err != nil || !claimed {
		return sent{Held: err == nil && cur.Status == proposal.StatusSending}, err
	}
	if reserve != nil {
		if err = reserve(); err != nil {
			return sent{Claimed: true}, errors.Join(err, store.Release(t.agent, id))
		}
	}
	b := gateway.Binding{TaskID: t.view.TaskID, DelegationID: d.DelegationID, GrantID: g.GrantID}
	var admitted gateway.Admitted
	rcpt, runErr := t.r.Executor.Run(t.agent, t.stepOf(b, c, &admitted))
	return sent{Claimed: true, Receipt: rcpt, Err: runErr}, t.settleProposal(id, rcpt, runErr, admitted.Key != "")
}

// stepOf is the executor's step of call c: the intent derived from the request, the admission that
// re-checks the delegation and uses the grant b names, and the send with the grant's proof.
func (t *turn) stepOf(b gateway.Binding, c gateway.Call, admitted *gateway.Admitted) Step {
	return Step{TaskID: t.view.TaskID, BusinessIntentID: IntentOf(t.view.TaskID, c.Capability.Command, c.Object.ID, c.RequestHash),
		Context: c.Capability.Context, Command: c.Command, RequestHash: c.RequestHash, DelegationID: b.DelegationID, GrantID: b.GrantID,
		Admit: func(ctx context.Context, key string) error {
			var err error
			*admitted, err = t.r.Gateway.Admit(ctx, b, c, key)
			return err
		},
		Send: func(ctx context.Context, _ any) (capability.Result, error) { return admitted.Run(ctx) }}
}

// settleProposal records what became of a claimed proposal: sent; released, pending again, when the
// target did not answer (the same request is sent again with the same key); refused when the owning
// context refused it after the admission; void when the admission or the task did not allow it.
func (t *turn) settleProposal(id string, rcpt Receipt, runErr error, admitted bool) error {
	store := t.r.proposals()
	o, release := outcomeOf(rcpt, runErr, admitted)
	var err error
	if release {
		err = store.Release(t.agent, id)
	} else {
		err = store.Settle(t.agent, id, o)
	}
	if errors.Is(err, proposal.ErrSettled) {
		return nil
	}
	return err
}

// outcomeOf is what a send's result means for its proposal: sent when it committed; released (the
// proposal is pending again, to be sent again with the same key) when the target did not answer or
// the error is no rejection; refused when the owning context rejected a request that was admitted
// (its code and the request member it points at); void when the admission or the task rejected it
// before anything was sent.
func outcomeOf(rcpt Receipt, runErr error, admitted bool) (o proposal.Outcome, release bool) {
	rej, isRejection := kernel.AsRejection(runErr)
	switch {
	case runErr == nil:
		return proposal.Outcome{Status: proposal.StatusSent, ActionID: rcpt.ActionID}, false
	case !isRejection || errors.Is(runErr, model.ErrUnavailable):
		return proposal.Outcome{}, true
	}
	status := proposal.StatusRefused
	if !admitted {
		status = proposal.StatusVoid
	}
	_, path := codeAndPath(runErr)
	return proposal.Outcome{Status: status, Code: rej.Code, Path: path}, false
}

// voidProposal records that a proposal can no longer be sent: nothing was sent, no grant was used.
func (t *turn) voidProposal(id, code, path string) error {
	err := t.r.proposals().Settle(t.agent, id, proposal.Outcome{Status: proposal.StatusVoid, Code: code, Path: path})
	if errors.Is(err, proposal.ErrSettled) || errors.Is(err, proposal.ErrNotFound) {
		return nil
	}
	return err
}

// codeAndPath is the code and the request member a refusal points at; a proposal that no longer
// decodes is SCHEMA_INVALID at its arguments.
func codeAndPath(err error) (code, path string) {
	if rej, ok := kernel.AsRejection(err); ok {
		code = rej.Code
		if len(rej.Fields) > 0 && executionevidence.RefusalPathOK(rej.Fields[0].Path) {
			path = rej.Fields[0].Path
		}
		return code, path
	}
	if errors.Is(err, gateway.ErrProposalChanged) {
		return kernel.CodeSchemaInvalid, "/args"
	}
	return "INTERNAL", ""
}
