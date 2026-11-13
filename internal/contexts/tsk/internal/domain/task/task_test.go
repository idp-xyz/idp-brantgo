package task

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

var (
	t0      = time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC)
	kinds   = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem}
	alice   = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "alice"}
	tskSys  = kernel.ActorRef{Kind: kernel.ActorSystem, ID: "tsk"}
	agentAI = kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.1"}
)

func act(kind kernel.ActorKind) Act {
	ids := map[kernel.ActorKind]string{kernel.ActorHuman: "alice", kernel.ActorAI: "agent.1", kernel.ActorSystem: "tsk"}
	return Act{By: kernel.ActorRef{Kind: kind, ID: ids[kind]}, Epoch: 7, At: t0}
}

func code(err error) string {
	if rej, ok := kernel.AsRejection(err); ok {
		return rej.Code
	}
	if err != nil {
		return "ERROR " + err.Error()
	}
	return "OK"
}

// The tactical model is the contract: SM-TSK-TASK as committed in A0 (owner decision D-1) has 13
// states and 67 transitions, each with its command and actor; the Go table is the same, row for row.
func TestTransitionTableIsSMTSKTASK(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..", "..", "docs", "05_TACTICAL_DDD",
		"02_IDP_Brant_Tactical_DDD_Candidate_V0.1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		StateMachines []struct {
			ID          string   `json:"id"`
			States      []string `json:"states"`
			Initial     string   `json:"initial"`
			Terminal    []string `json:"terminal"`
			Transitions []struct {
				From, Command, To, Actor string
			} `json:"transitions"`
		} `json:"stateMachines"`
	}
	if err = json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, sm := range doc.StateMachines {
		if sm.ID != "SM-TSK-TASK" {
			continue
		}
		var states []string
		for _, s := range States {
			states = append(states, string(s))
		}
		if !slices.Equal(sm.States, states) || sm.Initial != string(Created) {
			t.Fatalf("states %v initial %s, Go %v", sm.States, sm.Initial, states)
		}
		var terminal []string
		for _, s := range States {
			if s.Terminal() {
				terminal = append(terminal, string(s))
			}
		}
		if !slices.Equal(sm.Terminal, terminal) {
			t.Fatalf("terminal %v, Go %v", sm.Terminal, terminal)
		}
		var want []string
		for _, tr := range sm.Transitions {
			want = append(want, tr.From+" "+tr.Command+" "+tr.To+" "+tr.Actor)
		}
		var got []string
		for _, tr := range Transitions() {
			var a []string
			for _, k := range tr.Actors {
				a = append(a, string(k))
			}
			got = append(got, string(tr.From)+" "+string(tr.Command)+" "+string(tr.To)+" "+strings.Join(a, "|"))
		}
		if len(want) != 67 || !slices.Equal(got, want) {
			t.Fatalf("SM-TSK-TASK has %d transitions:\n%s\nGo has %d:\n%s", len(want), strings.Join(want, "\n"), len(got), strings.Join(got, "\n"))
		}
		return
	}
	t.Fatal("SM-TSK-TASK is missing from the tactical model")
}

// setup describes the task a command runs on, besides its state.
type setup struct {
	mode       Mode
	bound      bool
	settled    bool
	pausedFrom State
	purpose    string
	variant    int // the command's input variant: wait kind, verdict
}

func (s setup) task(state State) *Task {
	tk := &Task{ID: "task.1", Goal: "order_active", GoalVersion: 1, State: state, Mode: s.mode, Version: 5, Initiator: alice,
		Owner: "alice", Agent: "agent.1", CreatedAt: t0, UpdatedAt: t0}
	if s.bound {
		tk.ControlEpoch = 3
		tk.Binding = &Binding{QuoteID: "quote.1", State: BindingBound, Epoch: 3, BoundAt: t0}
		tk.Target.QuoteID = "quote.1"
	}
	if s.settled {
		tk.ScopeSettledAt = t0
	}
	if state == Paused {
		tk.PausedFrom = s.pausedFrom
	}
	if state == Stopping {
		tk.Stop = &Stop{Purpose: s.purpose, From: Running, Reason: "r", By: alice, At: t0}
	}
	switch state {
	case Completed:
		tk.CompletionActor = CompletionAIWithRequiredReview
	case ClosedUnfulfilled, Cancelled, Failed:
		tk.Termination = &Termination{Cause: EndFailed, Reason: "r", Responsible: "alice", By: alice, At: t0}
	}
	if p := tk.resumePoint(); p.Waiting() {
		tk.WaitCond = &WaitCondition{Kind: string(p)[len("waiting_"):], Reason: "r", Responsible: "alice", Since: t0}
	}
	return tk
}

// invoke runs command on tk with the inputs of s: the epoch TRD would return when the root is bound
// and the call reaches TRD, otherwise 0.
func invoke(tk *Task, command Command, s setup, a Act) (Change, error) {
	epoch := int64(0)
	if tk.Bound() {
		epoch = tk.ControlEpoch + 1
	}
	switch command {
	case CmdScope:
		return tk.Scope("", a)
	case CmdWait:
		w := []string{WaitInput, WaitApproval, WaitExternal}[s.variant%3]
		return tk.Wait(WaitRequest{Wait: w, Reason: "r", Responsible: "alice", Items: []string{"i"}, Awaiting: &Ref{Kind: "k", ID: "x"},
			DueAt: t0.Add(time.Hour)}, a)
	case CmdResume:
		return tk.Resume("handled", &Ref{Kind: "k", ID: "x"}, 0, a)
	case CmdVerify:
		return tk.Verify(0, a)
	case CmdVerifyGoal:
		v := []string{Satisfied, Unsatisfied, Unknown, Error}[s.variant%4]
		return tk.VerifyGoal(Verdict{Outcome: v}, 0, a)
	case CmdPause:
		return tk.Pause(PauseRequest{Cause: "operator", Reason: "r", ResumeCondition: "c"}, a)
	case CmdTakeControl:
		return tk.TakeControl("r", epoch, a)
	case CmdHandoff:
		return tk.Handoff("r", epoch, a)
	case CmdCancel:
		return tk.Cancel("r", epoch, a)
	case CmdFinishStop:
		if tk.Stop != nil && tk.Stop.Purpose == StopCancel {
			epoch = 0
		}
		return tk.FinishStop(0, epoch, a)
	case CmdCloseUnfulfilled:
		return tk.CloseUnfulfilled("customer_declined", "r", 0, a)
	case CmdFail:
		return tk.Fail("r", "alice", 0, a)
	case CmdBindObject:
		return tk.BindObject("quote.1", true, tk.ControlEpoch+1, a)
	case CmdRelease:
		return tk.Release("r", epoch, 0, a)
	}
	panic("unknown command " + command)
}

// consistent checks what brant_tsk.business_task's constraints require of a stored task.
func consistent(t *Task) string {
	switch {
	case (t.State == Paused) != (t.PausedFrom != ""):
		return "paused_from"
	case t.WaitCond != nil && string(t.resumePoint()) != "waiting_"+t.WaitCond.Kind:
		return "wait condition of another state"
	case t.State.Waiting() && t.WaitCond == nil:
		return "waiting without its condition"
	case (t.State == Paused || t.State == Stopping) && t.WaitCond == nil && t.resumePoint().Waiting():
		return "the wait to resume to is lost"
	case t.PauseCond != nil && t.State != Paused:
		return "pause cause"
	case t.State == Stopping && t.Stop == nil:
		return "stop request"
	case (t.CompletionActor != "") != (t.State == Completed):
		return "completion actor"
	case (t.Termination != nil) != (t.State == ClosedUnfulfilled || t.State == Cancelled || t.State == Failed):
		return "termination"
	case t.Binding == nil && t.ControlEpoch != 0, t.Binding != nil && t.Binding.Epoch != t.ControlEpoch:
		return "control epoch"
	case t.Binding != nil && t.Binding.QuoteID != t.Target.QuoteID:
		return "target quote"
	case t.Stop != nil && (t.Stop.From == Paused || t.Stop.From == Stopping || t.Stop.From.Terminal() || t.Stop.From == ""):
		return "stop from"
	}
	return ""
}

// resumePoint is the state a paused or stopping task returns to.
func (t *Task) resumePoint() State {
	switch {
	case t.State == Paused:
		return t.PausedFrom
	case t.State == Stopping && t.Stop != nil:
		return t.Stop.From
	}
	return t.State
}

func setups() []setup {
	var out []setup
	for _, mode := range []Mode{ModeAI, ModeManual} {
		for _, bound := range []bool{false, true} {
			for _, settled := range []bool{false, true} {
				for _, from := range []State{Running, Created, WaitingInput, WaitingApproval, WaitingExternal} {
					for _, purpose := range []string{StopTakeControl, StopHandoff, StopCancel} {
						for v := range 4 {
							out = append(out, setup{mode: mode, bound: bound, settled: settled, pausedFrom: from, purpose: purpose, variant: v})
						}
					}
				}
			}
		}
	}
	return out
}

// Every state, command, input and actor kind is tried: the moves that succeed are exactly the rows of
// SM-TSK-TASK, each only by an actor kind its row names; every other combination is refused, an
// unlisted transition as INVALID_STATE (Q01 V0.1 TRANSITION_NOT_ALLOWED) and a listed one by
// another actor as PERMISSION_DENIED. No AI actor moves a task (AI-INV-004), and each success moves
// the task version by exactly one.
func TestOnlyTheListedTransitionsByTheirActors(t *testing.T) {
	reached := map[string]bool{}
	for _, from := range States {
		for _, command := range Commands() {
			if command == CmdCreate {
				continue
			}
			for _, s := range setups() {
				for _, kind := range kinds {
					tk := s.task(from)
					before := tk.Version
					ch, err := invoke(tk, command, s, act(kind))
					if err != nil {
						if c := code(err); c == "OK" || strings.HasPrefix(c, "ERROR") {
							t.Fatalf("%s %s: %v", from, command, err)
						}
						if tk.Version != before || tk.State != from {
							t.Fatalf("a refused %s changed the task: %+v", command, tk)
						}
						continue
					}
					if err := consistent(tk); err != "" {
						t.Fatalf("%s %s → %s: %s", from, command, tk.State, err)
					}
					tr, ok := lookup(from, command, tk.State)
					if !ok || !slices.Contains(tr.Actors, kind) {
						t.Fatalf("%s %s → %s by %s is not in SM-TSK-TASK", from, command, tk.State, kind)
					}
					if tk.Version != before+1 || ch.Version != tk.Version || ch.From != from || ch.To != tk.State || ch.Command != command {
						t.Fatalf("%s %s: version %d → %d, change %+v", from, command, before, tk.Version, ch)
					}
					reached[string(from)+" "+string(command)+" "+string(tk.State)+" "+string(kind)] = true
				}
			}
			// A state without any row for the command refuses it as INVALID_STATE, whoever asks.
			if !allows(from, command) {
				for _, kind := range kinds {
					s := setup{mode: ModeAI, bound: true, settled: true, pausedFrom: Running, purpose: StopTakeControl}
					if _, err := invoke(s.task(from), command, s, act(kind)); code(err) != kernel.CodeInvalidState {
						t.Fatalf("%s from %s by %s: %s, want INVALID_STATE", command, from, kind, code(err))
					}
				}
			}
		}
	}
	for _, tr := range Transitions() {
		for _, kind := range tr.Actors {
			if kind == kernel.ActorAI {
				t.Fatalf("SM-TSK-TASK row %+v admits an AI actor", tr)
			}
			if !reached[string(tr.From)+" "+string(tr.Command)+" "+string(tr.To)+" "+string(kind)] {
				t.Errorf("transition %s %s → %s by %s was never reached", tr.From, tr.Command, tr.To, kind)
			}
		}
	}
	for _, kind := range kinds {
		for _, tr := range Transitions() {
			if slices.Contains(tr.Actors, kind) {
				continue
			}
			s := setup{mode: ModeAI, bound: false, settled: true, pausedFrom: Running, purpose: StopTakeControl}
			if tr.Command == CmdHandoff || (tr.Command == CmdRelease && tr.From == Paused) {
				s.mode = ModeManual
			}
			if tr.Command == CmdRelease {
				s.bound = true
			}
			if tr.To == Cancelled {
				s.purpose = StopCancel
			}
			if tr.To == WaitingInput && tr.Command == CmdVerifyGoal {
				s.variant = 1
			}
			if tr.To == WaitingExternal && tr.Command == CmdVerifyGoal {
				s.variant = 2
			}
			if tr.Command == CmdWait {
				s.variant = map[State]int{WaitingInput: 0, WaitingApproval: 1, WaitingExternal: 2}[tr.To]
			}
			if tr.Command == CmdResume && tr.From == Paused {
				s.pausedFrom = map[State]State{Running: Running, WaitingInput: WaitingInput, WaitingApproval: WaitingApproval,
					WaitingExternal: WaitingExternal}[tr.To]
			}
			if _, err := invoke(s.task(tr.From), tr.Command, s, act(kind)); code(err) != kernel.CodePermissionDenied {
				t.Errorf("%s %s → %s by %s: %s, want PERMISSION_DENIED", tr.From, tr.Command, tr.To, kind, code(err))
			}
		}
	}
}

func created(t *testing.T, mode Mode) *Task {
	t.Helper()
	tk, ch, err := Create(CreateRequest{ID: "task.1", Goal: "order_active", GoalVersion: 1, Mode: mode, Agent: "agent.1", Owner: "alice",
		RequestHash: strings.Repeat("a", 64)}, Act{By: alice, Epoch: 1, At: t0})
	if err != nil {
		t.Fatal(err)
	}
	if tk.State != Created || tk.Version != 1 || ch.To != Created || ch.Command != CmdCreate || tk.ControlEpoch != 0 {
		t.Fatalf("%+v %+v", tk, ch)
	}
	return tk
}

func ok(t *testing.T) func(Change, error) {
	return func(_ Change, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
}

func must(t *testing.T, _ Change, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// running drives a new task through task.scope twice: created → scoping (system) → running.
func running(t *testing.T, mode Mode) *Task {
	t.Helper()
	tk := created(t, mode)
	sys := Act{By: tskSys, Epoch: 1, At: t0}
	ch, err := tk.Scope("", sys)
	must(t, ch, err)
	ch, err = tk.Scope("scope settled", Act{By: alice, Epoch: 1, At: t0})
	must(t, ch, err)
	if tk.State != Running || tk.ScopeSettledAt.IsZero() {
		t.Fatalf("%+v", tk)
	}
	return tk
}

// completed drives a task to completed through goal.verify, the only way there.
func completed(t *testing.T, mode Mode) *Task {
	t.Helper()
	tk := running(t, mode)
	sys := Act{By: tskSys, Epoch: 1, At: t0}
	ch, err := tk.Verify(0, sys)
	must(t, ch, err)
	ch, err = tk.VerifyGoal(Verdict{Outcome: Satisfied}, 0, sys)
	must(t, ch, err)
	if tk.State != Completed {
		t.Fatalf("%+v", tk)
	}
	return tk
}

// Q01 V0.1 tests/test_execution.py::test_generic_transition_cannot_set_completed: there is no
// transition that sets a state; completed is reached only by goal.verify from verifying with every
// assertion satisfied (TSK-INV-001), whatever the command, the inputs or the actor.
func TestNoCommandSetsCompletedButASatisfiedGoalVerification(t *testing.T) {
	for _, tr := range Transitions() {
		if tr.To == Completed && tr.From != Completed && (tr.Command != CmdVerifyGoal || tr.From != Verifying) {
			t.Fatalf("%+v reaches completed", tr)
		}
	}
	for _, v := range []Verdict{{Outcome: Unsatisfied, Unmet: []string{"Q01.order"}}, {Outcome: Unknown}, {Outcome: Error},
		{Outcome: Satisfied, Unmet: []string{"Q01.document"}}} {
		tk := running(t, ModeAI)
		sys := Act{By: tskSys, Epoch: 1, At: t0}
		ch, err := tk.Verify(0, sys)
		must(t, ch, err)
		if _, err = tk.VerifyGoal(v, 0, sys); err != nil || tk.State == Completed || tk.CompletionActor != "" || tk.WaitCond == nil {
			t.Fatalf("%+v: %v %+v", v, err, tk)
		}
		if !slices.Equal(tk.WaitCond.Items, v.Unmet) || tk.WaitCond.Responsible != "alice" {
			t.Fatalf("unmet items %+v", tk.WaitCond)
		}
	}
	// A satisfied verdict with an action still submitted or unknown waits for the external result
	// (TSK-INV-003).
	tk := running(t, ModeAI)
	sys := Act{By: tskSys, Epoch: 1, At: t0}
	ch, err := tk.Verify(0, sys)
	must(t, ch, err)
	if _, err = tk.VerifyGoal(Verdict{Outcome: Satisfied}, 1, sys); err != nil || tk.State != WaitingExternal {
		t.Fatalf("%v %+v", err, tk)
	}
	// An unknown outcome is not a pass.
	tk = running(t, ModeAI)
	ok(t)(tk.Verify(0, sys))
	if _, err = tk.VerifyGoal(Verdict{Outcome: "done"}, 0, sys); code(err) != kernel.CodeInvalidKnownValue || tk.State != Verifying {
		t.Fatalf("%v %+v", err, tk)
	}
}

// Q01 V0.1 tests/test_execution.py::test_terminal_task_rejects_new_forward_action and BC-AT25: a
// completed task keeps its completion and takes no forward action — every command but task.release is
// INVALID_STATE, and no claim is issued; the same holds for the other terminal states.
func TestTerminalTaskRejectsEveryForwardAction(t *testing.T) {
	tk := completed(t, ModeAI)
	if tk.CompletionActor != CompletionAIWithRequiredReview {
		t.Fatalf("completion actor %s", tk.CompletionActor)
	}
	for _, s := range []State{Completed, ClosedUnfulfilled, Cancelled, Failed} {
		for _, command := range Commands() {
			if command == CmdCreate || command == CmdRelease {
				continue
			}
			for _, kind := range kinds {
				st := setup{mode: ModeAI, bound: true, settled: true, purpose: StopTakeControl}
				x := st.task(s)
				if _, err := invoke(x, command, st, act(kind)); code(err) != kernel.CodeInvalidState || x.State != s {
					t.Fatalf("%s on %s by %s: %s", command, s, kind, code(err))
				}
			}
		}
		x := setup{mode: ModeAI}.task(s)
		for _, a := range []kernel.ActorRef{alice, agentAI, tskSys} {
			if err := x.Claim(a); code(err) != kernel.CodeInvalidState {
				t.Fatalf("claim of a %s task by %s: %s", s, a, code(err))
			}
		}
	}
}

// Completion actor (baseline 16.1): manual for a task under manual control when it completes, else
// ai_with_required_review; P0 never completes ai_unassisted.
func TestCompletionActor(t *testing.T) {
	if a := completed(t, ModeManual).CompletionActor; a != CompletionManual {
		t.Fatalf("manual task: %s", a)
	}
	if a := completed(t, ModeAI).CompletionActor; a != CompletionAIWithRequiredReview {
		t.Fatalf("AI task: %s", a)
	}
}

// TSK-INV-002 and SM-TSK-TASK take_control: a bound root is stopped at TRD first, and the task
// records the epoch TRD returned — never an earlier one; an unbound task records only the control
// change. The barrier waits for every submitted or unknown action (TSK-INV-003); a take-over pauses
// the task in manual mode, a hand-back returns it to ai, a cancellation ends it cancelled.
func TestTakeOverRecordsTRDsEpochAndTheBarrierWaits(t *testing.T) {
	sys := Act{By: tskSys, Epoch: 1, At: t0}
	hum := Act{By: alice, Epoch: 1, At: t0}
	tk := running(t, ModeAI)
	ok(t)(tk.BindObject("quote.1", true, 1, sys))
	if !tk.Bound() || tk.ControlEpoch != 1 || tk.Binding.Epoch != 1 {
		t.Fatalf("%+v", tk)
	}
	if _, err := tk.TakeControl("model down", 1, hum); code(err) != CodeControlEpochStale || tk.State != Running {
		t.Fatalf("a stale epoch: %s", code(err))
	}
	if _, err := tk.TakeControl("model down", 0, hum); code(err) != CodeControlEpochStale {
		t.Fatalf("no epoch for a bound root: %s", code(err))
	}
	ch, err := tk.TakeControl("model down", 2, hum)
	must(t, ch, err)
	if tk.State != Stopping || tk.ControlEpoch != 2 || tk.Mode != ModeAI || ch.ControlChange != ChangeTakeControl || ch.ControlEpoch != 2 ||
		tk.Stop.From != Running || tk.Stop.By != alice {
		t.Fatalf("%+v %+v", tk, ch)
	}
	if err = tk.Claim(alice); code(err) != kernel.CodeInvalidState {
		t.Fatalf("claim while stopping: %s", code(err))
	}
	if _, err = tk.FinishStop(1, 3, sys); code(err) != CodeExternalResultUnknown || tk.State != Stopping {
		t.Fatalf("an unknown action must block the barrier: %s", code(err))
	}
	ch, err = tk.FinishStop(0, 3, sys)
	must(t, ch, err)
	if tk.State != Paused || tk.Mode != ModeManual || tk.ControlEpoch != 3 || tk.PausedFrom != Running || ch.ControlChange != ChangeFinishStop {
		t.Fatalf("%+v", tk)
	}
	// Under manual control the AI agent gets no claim; people do.
	if err = tk.Claim(agentAI); code(err) != CodeControlModeMismatch {
		t.Fatalf("agent claim in manual mode: %s", code(err))
	}
	if err = tk.Claim(alice); err != nil {
		t.Fatal(err)
	}
	if _, err = tk.TakeControl("again", 4, hum); code(err) != kernel.CodeInvalidState {
		t.Fatalf("a manual task is not taken over again: %s", code(err))
	}
	// Hand it back: manual → stopping → paused in mode ai, TRD's epochs recorded.
	ok(t)(tk.Handoff("model back", 4, hum))
	ok(t)(tk.FinishStop(0, 5, sys))
	if tk.Mode != ModeAI || tk.State != Paused || tk.ControlEpoch != 5 {
		t.Fatalf("%+v", tk)
	}
	// Cancel: stopping → cancelled without TRD; the root stays bound until task.release.
	ok(t)(tk.Cancel("customer gone", 6, hum))
	if _, err = tk.FinishStop(0, 7, sys); code(err) != kernel.CodeInvalidState {
		t.Fatalf("a cancellation does not change TRD: %s", code(err))
	}
	ok(t)(tk.FinishStop(0, 0, sys))
	if tk.State != Cancelled || tk.Termination.Cause != EndCancelled || tk.Termination.By != alice || !tk.Bound() {
		t.Fatalf("%+v", tk)
	}
	if err = tk.CheckRelease(1, hum); code(err) != CodeExternalResultUnknown {
		t.Fatalf("release with an unknown action: %s", code(err))
	}
	ch, err = tk.Release("done", 7, 0, hum)
	must(t, ch, err)
	if tk.State != Cancelled || tk.Bound() || tk.Binding.State != BindingReleased || tk.ControlEpoch != 7 || ch.ControlChange != ChangeRelease {
		t.Fatalf("%+v", tk)
	}
	if err = tk.CheckRelease(0, hum); code(err) != kernel.CodeInvalidState {
		t.Fatalf("nothing is bound any more: %s", code(err))
	}
}

// An unbound task is taken over without TRD: no epoch is recorded, and passing one is refused.
func TestUnboundTakeOverRecordsOnlyTheControlChange(t *testing.T) {
	hum := Act{By: alice, Epoch: 1, At: t0}
	tk := running(t, ModeAI)
	if _, err := tk.TakeControl("x", 1, hum); code(err) != kernel.CodeInvalidState {
		t.Fatalf("an epoch without a root: %s", code(err))
	}
	ok(t)(tk.TakeControl("x", 0, hum))
	ok(t)(tk.FinishStop(0, 0, Act{By: tskSys, Epoch: 1, At: t0}))
	if tk.ControlEpoch != 0 || tk.Mode != ModeManual || tk.State != Paused {
		t.Fatalf("%+v", tk)
	}
	// A manual paused task without a root has nothing to release.
	if err := tk.CheckRelease(0, hum); code(err) != kernel.CodeInvalidState {
		t.Fatalf("%s", code(err))
	}
}

// task.bind_object: a running AI task binds one root and records TRD's epoch: the root its target
// names, or, when the target names none, a root an action of the task created (SM-TSK-TASK
// "本任务的动作已创建报价根"; B1-D09); a manual task, a second binding, another target's root and a root
// the task did not create are refused.
func TestBindObject(t *testing.T) {
	sys := Act{By: tskSys, Epoch: 1, At: t0}
	tk := running(t, ModeAI)
	tk.Target.QuoteID = "quote.1"
	if err := tk.CheckBind("quote.2", true, sys); code(err) != kernel.CodeReferenceInvalid {
		t.Fatalf("another root: %s", code(err))
	}
	ok(t)(tk.BindObject("quote.1", false, 4, sys)) // the target names it: the task need not have created it
	if err := tk.CheckBind("quote.1", true, sys); code(err) != kernel.CodeInvalidState {
		t.Fatalf("second binding: %s", code(err))
	}
	free := running(t, ModeAI)
	if _, err := free.BindObject("quote.9", false, 1, sys); code(err) != kernel.CodeReferenceInvalid || free.Bound() || free.Version != 3 {
		t.Fatalf("a root the task did not create: %s %+v", code(err), free)
	}
	ok(t)(free.BindObject("quote.9", true, 1, sys))
	if !free.Bound() || free.Target.QuoteID != "quote.9" {
		t.Fatalf("a root the task created: %+v", free)
	}
	m := running(t, ModeManual)
	if err := m.CheckBind("quote.1", true, sys); code(err) != kernel.CodeInvalidState {
		t.Fatalf("manual task: %s", code(err))
	}
	if err := running(t, ModeAI).CheckBind("quote.1", true, Act{By: alice, Epoch: 1, At: t0}); code(err) != kernel.CodePermissionDenied {
		t.Fatalf("a human binds: %s", code(err))
	}
}

// D-2 and TSK-INV-004: in mode ai the task's agent (scoping or running) and humans get a claim; in
// manual mode only humans; another AI actor and system actors never; nobody while stopping or after
// the end.
func TestClaimRules(t *testing.T) {
	other := kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.2"}
	for _, s := range States {
		for _, mode := range []Mode{ModeAI, ModeManual} {
			tk := setup{mode: mode}.task(s)
			stopped := s.Terminal() || s == Stopping
			want := map[kernel.ActorRef]string{alice: "OK", agentAI: "OK", other: kernel.CodePermissionDenied, tskSys: kernel.CodePermissionDenied}
			switch {
			case stopped:
				for a := range want {
					want[a] = kernel.CodeInvalidState
				}
			case mode == ModeManual:
				want[agentAI], want[other] = CodeControlModeMismatch, CodeControlModeMismatch
			case s != Scoping && s != Running:
				want[agentAI] = kernel.CodeInvalidState
			}
			for a, w := range want {
				if got := code(tk.Claim(a)); got != w {
					t.Errorf("%s %s claim by %s: %s, want %s", s, mode, a, got, w)
				}
			}
		}
	}
}

// task.resume: a waiting task resumes on what arrived (system); a paused one returns to the waiting
// state it was paused in, or to running, once its cause is dealt with (human), and not while an
// action is submitted or unknown; a human resuming a task paused before its scope was settled
// settles it.
func TestResume(t *testing.T) {
	sys := Act{By: tskSys, Epoch: 1, At: t0}
	hum := Act{By: alice, Epoch: 1, At: t0}
	tk := running(t, ModeAI)
	ok(t)(tk.Wait(WaitRequest{Wait: WaitExternal, Reason: "customer reply", Responsible: "alice", Awaiting: &Ref{Kind: "customer_response", ID: "quote.1"},
		DueAt: t0.Add(48 * time.Hour)}, sys))
	if tk.State != WaitingExternal || tk.WaitCond.Awaiting.Kind != "customer_response" || !tk.WaitCond.DueAt.Equal(t0.Add(48*time.Hour)) {
		t.Fatalf("%+v", tk.WaitCond)
	}
	ok(t)(tk.Pause(PauseRequest{Cause: "model_unavailable", Reason: "model down", ResumeCondition: "model back"}, sys))
	if tk.PausedFrom != WaitingExternal || tk.PauseCond.Responsible != "alice" || tk.WaitCond.Awaiting.Kind != "customer_response" {
		t.Fatalf("%+v", tk)
	}
	if _, err := tk.Resume("", nil, 0, hum); code(err) != kernel.CodeSchemaInvalid {
		t.Fatalf("a paused task resumes with a reason: %s", code(err))
	}
	if _, err := tk.Resume("model back", nil, 1, hum); code(err) != CodeExternalResultUnknown {
		t.Fatalf("an unknown action: %s", code(err))
	}
	if _, err := tk.Resume("model back", nil, 0, sys); code(err) != kernel.CodePermissionDenied {
		t.Fatalf("a system actor resumes a paused task: %s", code(err))
	}
	ok(t)(tk.Resume("model back", nil, 0, hum))
	if tk.State != WaitingExternal || tk.PauseCond != nil || tk.PausedFrom != "" || tk.WaitCond.Awaiting.Kind != "customer_response" {
		t.Fatalf("%+v", tk)
	}
	if _, err := tk.Resume("", nil, 0, sys); code(err) != kernel.CodeSchemaInvalid {
		t.Fatalf("a waiting task resumes on an arrival: %s", code(err))
	}
	if _, err := tk.Resume("", &Ref{Kind: "acceptance", ID: "acc.1", Version: 1}, 0, hum); code(err) != kernel.CodePermissionDenied {
		t.Fatalf("a human resumes a waiting task: %s", code(err))
	}
	ch, err := tk.Resume("", &Ref{Kind: "acceptance", ID: "acc.1", Version: 1}, 0, sys)
	must(t, ch, err)
	if tk.State != Running || ch.Reason != "arrived: acceptance acc.1" {
		t.Fatalf("%+v %+v", tk, ch)
	}

	// Paused before the scope was settled: the human who resumes it settles the scope.
	early := created(t, ModeAI)
	ok(t)(early.Pause(PauseRequest{Cause: "operator", Reason: "hold", ResumeCondition: "go"}, hum))
	ok(t)(early.Resume("scope confirmed", nil, 0, hum))
	if early.State != Running || early.ScopeSettledAt.IsZero() {
		t.Fatalf("%+v", early)
	}
}

// task.wait records what each wait needs; a due instant in the past is refused.
func TestWait(t *testing.T) {
	sys := Act{By: tskSys, Epoch: 1, At: t0}
	tk := running(t, ModeAI)
	if _, err := tk.Wait(WaitRequest{Wait: WaitExternal, Reason: "r", Responsible: "alice", Awaiting: &Ref{Kind: "k", ID: "x"}, DueAt: t0}, sys); code(err) != kernel.CodeInvalidKnownValue {
		t.Fatalf("due now: %s", code(err))
	}
	ok(t)(tk.Wait(WaitRequest{Wait: WaitApproval, Reason: "review", Responsible: "bob", Awaiting: &Ref{Kind: "quote_review", ID: "rev.1", Version: 2}}, sys))
	if tk.State != WaitingApproval || tk.WaitCond.Responsible != "bob" || tk.WaitCond.Awaiting.Version != 2 {
		t.Fatalf("%+v", tk.WaitCond)
	}
}

// task.create: the initiator is human or system; a system initiator names the owner; an AI task
// names its agent; a deadline lies ahead.
func TestCreate(t *testing.T) {
	r := CreateRequest{ID: "task.1", Goal: "quote_ready", GoalVersion: 1, Mode: ModeAI, Agent: "agent.1", Owner: "alice"}
	if _, _, err := Create(r, Act{By: agentAI, Epoch: 1, At: t0}); code(err) != kernel.CodePermissionDenied {
		t.Fatalf("AI creates: %s", code(err))
	}
	noOwner := r
	noOwner.Owner = ""
	if _, _, err := Create(noOwner, Act{By: tskSys, Epoch: 1, At: t0}); code(err) != kernel.CodeSchemaInvalid {
		t.Fatalf("no owner: %s", code(err))
	}
	noAgent := r
	noAgent.Agent = ""
	if _, _, err := Create(noAgent, Act{By: alice, Epoch: 1, At: t0}); code(err) != kernel.CodeSchemaInvalid {
		t.Fatalf("no agent: %s", code(err))
	}
	late := r
	late.Deadline = t0.Add(-time.Minute)
	if _, _, err := Create(late, Act{By: alice, Epoch: 1, At: t0}); code(err) != kernel.CodeInvalidKnownValue {
		t.Fatalf("past deadline: %s", code(err))
	}
	tk, _, err := Create(r, Act{By: tskSys, Epoch: 1, At: t0})
	if err != nil || tk.Initiator != tskSys || tk.State != Created || tk.Mode != ModeAI {
		t.Fatalf("%v %+v", err, tk)
	}
}

// close_unfulfilled and fail end the task with their cause, never as success, and only once no
// action is submitted or unknown.
func TestUnfulfilledAndFailedEnds(t *testing.T) {
	hum := Act{By: alice, Epoch: 1, At: t0}
	tk := running(t, ModeAI)
	if _, err := tk.CloseUnfulfilled("quote_expired", "expired", 1, hum); code(err) != CodeExternalResultUnknown {
		t.Fatalf("%s", code(err))
	}
	ok(t)(tk.CloseUnfulfilled("quote_expired", "expired", 0, hum))
	if tk.State != ClosedUnfulfilled || tk.CompletionActor != "" || tk.Termination.Cause != "quote_expired" {
		t.Fatalf("%+v", tk)
	}
	f := running(t, ModeAI)
	if _, err := f.Fail("broken", "bob", 2, hum); code(err) != CodeExternalResultUnknown {
		t.Fatalf("%s", code(err))
	}
	ok(t)(f.Fail("broken", "bob", 0, hum))
	if f.State != Failed || f.Termination.Responsible != "bob" || f.Termination.Cause != EndFailed {
		t.Fatalf("%+v", f)
	}
}

func TestExpectAndVisibility(t *testing.T) {
	tk := running(t, ModeAI)
	if err := tk.Expect(2); code(err) != kernel.CodeVersionConflict {
		t.Fatalf("%s", code(err))
	}
	if err := tk.Expect(tk.Version); err != nil {
		t.Fatal(err)
	}
	none := func(string, string) bool { return false }
	if !tk.Visible(alice, none) || !tk.Visible(agentAI, none) || tk.Visible(kernel.ActorRef{Kind: kernel.ActorHuman, ID: "eve"}, none) {
		t.Fatal("visibility")
	}
}

func TestActorKindsOfCommands(t *testing.T) {
	want := map[Command][]kernel.ActorKind{
		CmdCreate: humanSystem, CmdScope: {kernel.ActorSystem, kernel.ActorHuman}, CmdWait: system, CmdVerify: system, CmdVerifyGoal: system,
		CmdPause: humanSystem, CmdResume: {kernel.ActorSystem, kernel.ActorHuman}, CmdTakeControl: human, CmdHandoff: human, CmdCancel: human,
		CmdFinishStop: system, CmdCloseUnfulfilled: humanSystem, CmdFail: human, CmdBindObject: system, CmdRelease: human,
	}
	if len(Commands()) != len(want) {
		t.Fatalf("commands %v", Commands())
	}
	for c, k := range want {
		if got := ActorKinds(c); !slices.Equal(got, k) {
			t.Errorf("%s: %v, want %v", c, got, k)
		}
	}
}

// SM-TSK-TASK paused → waiting_* ("回到暂停前的等待状态"): what a task waits for survives a pause and a
// take-over, and the task resumes to that wait; a resume to running or an end clears it.
func TestTheWaitSurvivesPauseAndTakeOver(t *testing.T) {
	sys := Act{By: tskSys, Epoch: 1, At: t0}
	hum := Act{By: alice, Epoch: 1, At: t0}
	tk := running(t, ModeAI)
	awaited := &Ref{Kind: "quote_review", ID: "rev.1", Version: 3}
	ok(t)(tk.Wait(WaitRequest{Wait: WaitApproval, Reason: "需要审核", Responsible: "bob", Awaiting: awaited}, sys))
	ok(t)(tk.Pause(PauseRequest{Cause: "tool_unavailable", Reason: "工具故障", ResumeCondition: "工具恢复"}, sys))
	ok(t)(tk.TakeControl("接管", 0, hum))
	if tk.WaitCond == nil || tk.Stop.From != WaitingApproval || tk.PauseCond != nil || tk.PausedFrom != "" {
		t.Fatalf("%+v", tk)
	}
	ok(t)(tk.FinishStop(0, 0, sys))
	if tk.PausedFrom != WaitingApproval || tk.WaitCond == nil || tk.Mode != ModeManual {
		t.Fatalf("%+v", tk)
	}
	ok(t)(tk.Resume("接管后继续等待审核", nil, 0, hum))
	if tk.State != WaitingApproval || *tk.WaitCond.Awaiting != *awaited || tk.WaitCond.Responsible != "bob" || consistent(tk) != "" {
		t.Fatalf("%+v %+v", tk, tk.WaitCond)
	}
	ok(t)(tk.Resume("", &Ref{Kind: "quote_review", ID: "rev.1", Version: 4}, 0, sys))
	if tk.State != Running || tk.WaitCond != nil {
		t.Fatalf("%+v", tk)
	}
	ok(t)(tk.Wait(WaitRequest{Wait: WaitInput, Reason: "缺项", Responsible: "alice", Items: []string{"数量"}}, sys))
	ok(t)(tk.Cancel("撤销", 0, hum))
	ok(t)(tk.FinishStop(0, 0, sys))
	if tk.State != Cancelled || tk.WaitCond != nil || consistent(tk) != "" {
		t.Fatalf("%+v", tk)
	}
}
