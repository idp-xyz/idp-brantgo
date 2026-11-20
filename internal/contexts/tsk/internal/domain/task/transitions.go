package task

import (
	"slices"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// State is a lifecycle state of SM-TSK-TASK (baseline 16.1).
type State string

const (
	Created           State = "created"
	Scoping           State = "scoping"
	Running           State = "running"
	WaitingInput      State = "waiting_input"
	WaitingApproval   State = "waiting_approval"
	WaitingExternal   State = "waiting_external"
	Verifying         State = "verifying"
	Paused            State = "paused"
	Stopping          State = "stopping"
	Completed         State = "completed"
	ClosedUnfulfilled State = "closed_unfulfilled"
	Cancelled         State = "cancelled"
	Failed            State = "failed"
)

// States lists the 13 states in SM-TSK-TASK's order.
var States = []State{Created, Scoping, Running, WaitingInput, WaitingApproval, WaitingExternal, Verifying, Paused, Stopping,
	Completed, ClosedUnfulfilled, Cancelled, Failed}

// Terminal reports whether s is one of SM-TSK-TASK's terminal states: completed, closed_unfulfilled,
// cancelled, failed. A terminal task takes no forward action; only task.release, which leaves the
// lifecycle unchanged, applies to it.
func (s State) Terminal() bool {
	return s == Completed || s == ClosedUnfulfilled || s == Cancelled || s == Failed
}

// Waiting reports whether s is one of the three waiting states.
func (s State) Waiting() bool {
	return s == WaitingInput || s == WaitingApproval || s == WaitingExternal
}

// Command is a registered TSK command (Context Map TSK.commands).
type Command string

const (
	CmdCreate           Command = "task.create"
	CmdScope            Command = "task.scope"
	CmdWait             Command = "task.wait"
	CmdVerify           Command = "task.verify"
	CmdVerifyGoal       Command = "goal.verify"
	CmdPause            Command = "task.pause"
	CmdResume           Command = "task.resume"
	CmdTakeControl      Command = "task.take_control"
	CmdHandoff          Command = "task.handoff"
	CmdCancel           Command = "task.cancel"
	CmdFinishStop       Command = "task.finish_stop"
	CmdCloseUnfulfilled Command = "task.close_unfulfilled"
	CmdFail             Command = "task.fail"
	CmdBindObject       Command = "task.bind_object"
	CmdRelease          Command = "task.release"
)

// Transition is one row of SM-TSK-TASK: Command moves a task from From to To when its actor is of
// one of Actors.
type Transition struct {
	From    State
	Command Command
	To      State
	Actors  []kernel.ActorKind
}

var (
	human       = []kernel.ActorKind{kernel.ActorHuman}
	system      = []kernel.ActorKind{kernel.ActorSystem}
	humanSystem = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorSystem}
)

// transitions is SM-TSK-TASK as committed in the tactical model (S4 A0, owner decision D-1): 13
// states, 67 transitions, each with its registered command and actor. No other transition exists
// ("未列出的转换拒绝"), and none sets a state a command does not name.
var transitions = []Transition{
	{Created, CmdScope, Scoping, system},
	{Scoping, CmdScope, Running, humanSystem},

	{Scoping, CmdWait, WaitingInput, system},
	{Running, CmdWait, WaitingInput, system},
	{Verifying, CmdWait, WaitingInput, system},
	{Running, CmdWait, WaitingApproval, system},
	{Verifying, CmdWait, WaitingApproval, system},
	{Running, CmdWait, WaitingExternal, system},

	{WaitingInput, CmdResume, Running, system},
	{WaitingApproval, CmdResume, Running, system},
	{WaitingExternal, CmdResume, Running, system},

	{Running, CmdBindObject, Running, system},
	{Running, CmdVerify, Verifying, system},

	{Verifying, CmdVerifyGoal, Completed, system},
	{Verifying, CmdVerifyGoal, WaitingInput, system},
	{Verifying, CmdVerifyGoal, WaitingExternal, system},

	{Created, CmdPause, Paused, humanSystem},
	{Scoping, CmdPause, Paused, humanSystem},
	{Running, CmdPause, Paused, humanSystem},
	{WaitingInput, CmdPause, Paused, humanSystem},
	{WaitingApproval, CmdPause, Paused, humanSystem},
	{WaitingExternal, CmdPause, Paused, humanSystem},
	{Verifying, CmdPause, Paused, humanSystem},

	{Paused, CmdResume, Running, human},
	{Paused, CmdResume, WaitingInput, human},
	{Paused, CmdResume, WaitingApproval, human},
	{Paused, CmdResume, WaitingExternal, human},

	{Created, CmdTakeControl, Stopping, human},
	{Scoping, CmdTakeControl, Stopping, human},
	{Running, CmdTakeControl, Stopping, human},
	{WaitingInput, CmdTakeControl, Stopping, human},
	{WaitingApproval, CmdTakeControl, Stopping, human},
	{WaitingExternal, CmdTakeControl, Stopping, human},
	{Verifying, CmdTakeControl, Stopping, human},
	{Paused, CmdTakeControl, Stopping, human},

	{Running, CmdHandoff, Stopping, human},
	{WaitingInput, CmdHandoff, Stopping, human},
	{WaitingApproval, CmdHandoff, Stopping, human},
	{WaitingExternal, CmdHandoff, Stopping, human},
	{Paused, CmdHandoff, Stopping, human},

	{Created, CmdCancel, Stopping, human},
	{Scoping, CmdCancel, Stopping, human},
	{Running, CmdCancel, Stopping, human},
	{WaitingInput, CmdCancel, Stopping, human},
	{WaitingApproval, CmdCancel, Stopping, human},
	{WaitingExternal, CmdCancel, Stopping, human},
	{Verifying, CmdCancel, Stopping, human},
	{Paused, CmdCancel, Stopping, human},

	{Stopping, CmdFinishStop, Paused, system},
	{Stopping, CmdFinishStop, Cancelled, system},

	{Running, CmdCloseUnfulfilled, ClosedUnfulfilled, humanSystem},
	{WaitingInput, CmdCloseUnfulfilled, ClosedUnfulfilled, humanSystem},
	{WaitingApproval, CmdCloseUnfulfilled, ClosedUnfulfilled, humanSystem},
	{WaitingExternal, CmdCloseUnfulfilled, ClosedUnfulfilled, humanSystem},
	{Verifying, CmdCloseUnfulfilled, ClosedUnfulfilled, humanSystem},
	{Paused, CmdCloseUnfulfilled, ClosedUnfulfilled, humanSystem},

	{Running, CmdFail, Failed, human},
	{WaitingInput, CmdFail, Failed, human},
	{WaitingApproval, CmdFail, Failed, human},
	{WaitingExternal, CmdFail, Failed, human},
	{Verifying, CmdFail, Failed, human},
	{Paused, CmdFail, Failed, human},

	{Completed, CmdRelease, Completed, human},
	{ClosedUnfulfilled, CmdRelease, ClosedUnfulfilled, human},
	{Cancelled, CmdRelease, Cancelled, human},
	{Failed, CmdRelease, Failed, human},
	{Paused, CmdRelease, Paused, human},
}

// Transitions returns a copy of SM-TSK-TASK's transitions.
func Transitions() []Transition {
	out := make([]Transition, len(transitions))
	for i, t := range transitions {
		t.Actors = slices.Clone(t.Actors)
		out[i] = t
	}
	return out
}

// Commands returns every command that labels a transition, and task.create, which creates a task in
// created.
func Commands() []Command {
	out := []Command{CmdCreate}
	for _, t := range transitions {
		if !slices.Contains(out, t.Command) {
			out = append(out, t.Command)
		}
	}
	return out
}

// ActorKinds returns the actor kinds any transition of command admits: the union an authorization
// rule of the command admits before the transition's own rule is checked. task.create admits humans
// and system actors (the Context Map's task.create; no AI actor creates or moves a task,
// AI-INV-004).
func ActorKinds(command Command) []kernel.ActorKind {
	if command == CmdCreate {
		return slices.Clone(humanSystem)
	}
	var out []kernel.ActorKind
	for _, t := range transitions {
		if t.Command != command {
			continue
		}
		for _, k := range t.Actors {
			if !slices.Contains(out, k) {
				out = append(out, k)
			}
		}
	}
	return out
}

// lookup returns the transition of command from from to to.
func lookup(from State, command Command, to State) (Transition, bool) {
	for _, t := range transitions {
		if t.From == from && t.Command == command && t.To == to {
			return t, true
		}
	}
	return Transition{}, false
}

// allows reports whether command has any transition from from.
func allows(from State, command Command) bool {
	for _, t := range transitions {
		if t.From == from && t.Command == command {
			return true
		}
	}
	return false
}
