package quote

import (
	"errors"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// grants is a fake of the authority guard: the grants of actors by ID, as Grants reads them.
type grants map[string]Grant

func (g grants) read(id string) (Grant, error) { return g[id], nil }

// delegated is the fixture's delegation: the author (human) for the task's initiator, agent.1 (AI)
// for its agent, each at the changed epoch the fixture's grants give it.
var delegated = Binding{Initiator: "author", Agent: "agent.1"}

func fixtureGrants() grants {
	return grants{"author": {Kind: kernel.ActorHuman, Active: true, ChangedEpoch: 3},
		"agent.1": {Kind: kernel.ActorAI, Active: true, ChangedEpoch: 5},
		"lead":    {Kind: kernel.ActorHuman, Active: true, ChangedEpoch: 4}}
}

func agentWrite(c *Claim, g grants) Write {
	return Write{Claim: c, Actor: kernel.ActorAI, Grants: g.read}
}
func humanWrite(c *Claim, g grants) Write {
	return Write{Claim: c, Actor: kernel.ActorHuman, Grants: g.read}
}

// TRD-INV-015 (Python test_t03_a24_write_control and t03_storage.guard): who may write while the
// root is unbound, bound or stopping, and with which claim. S3's test, changed for S4: the stop and
// the release need the bound task's claim (decision D-4 replaces S3-D11's direct release), and the
// binding's fence is its delegation's grants, not the company's authority epoch (decision D-5
// replaces S3-D12).
func TestControlFence(t *testing.T) {
	q := New("quote.1", "author", "")
	now := issuedAt
	g := fixtureGrants()
	task := &Claim{TaskID: "task.1", Epoch: 1}
	if err := q.Control.Admit(agentWrite(nil, g)); err != nil {
		t.Fatalf("an unbound root takes writes without a claim: %v", err)
	}
	if got := code(q.Control.Admit(agentWrite(task, g))); got != "CONTROL_EPOCH_STALE at /" {
		t.Fatalf("a claim on an unbound root: %s", got)
	}
	if got := code(q.Bind("task.1", delegated, 1, human("author"), 7, g.read, now)); got != "VERSION_CONFLICT at /args/expected_control_epoch" {
		t.Fatalf("bind at another epoch: %s", got)
	}
	if err := q.Bind("task.1", delegated, 0, human("author"), 7, g.read, now); err != nil || q.Control.Epoch != 1 || q.Control.State != Bound ||
		q.Control.Delegation != (Delegation{Initiator: "author", InitiatorEpoch: 3, Agent: "agent.1", AgentEpoch: 5}) ||
		q.Control.AuthorityEpoch != 7 {
		t.Fatalf("bind: %v %+v", err, q.Control)
	}
	if got := code(q.Bind("task.2", delegated, 1, human("author"), 7, g.read, now)); got != "OBJECT_CONTROL_CONFLICT at /args/quote_id" {
		t.Fatalf("a second binding: %s", got)
	}
	for name, c := range map[string]struct {
		claim *Claim
		want  string
	}{
		"no claim":          {nil, "OBJECT_CONTROL_CONFLICT at /"},
		"another task":      {&Claim{TaskID: "task.2", Epoch: 1}, "OBJECT_CONTROL_CONFLICT at /"},
		"an earlier epoch":  {&Claim{TaskID: "task.1", Epoch: 0}, "CONTROL_EPOCH_STALE at /"},
		"the bound task":    {task, "OK"},
		"a future epoch":    {&Claim{TaskID: "task.1", Epoch: 2}, "CONTROL_EPOCH_STALE at /"},
		"guessed and wrong": {&Claim{TaskID: "other", Epoch: 2}, "CONTROL_EPOCH_STALE at /"},
	} {
		for _, w := range []Write{agentWrite(c.claim, g), humanWrite(c.claim, g)} {
			if got := code(q.Control.Admit(w)); got != c.want {
				t.Errorf("bound, %s by %s: %s, want %s", name, w.Actor, got, c.want)
			}
		}
	}
	if got := code(q.Release(task, 0, human("author"), now)); got != "VERSION_CONFLICT at /args/expected_control_epoch" {
		t.Fatalf("release at an old epoch: %s", got)
	}
	if err := q.Stop(task, 1, human("lead"), now); err != nil || q.Control.State != Stopping || q.Control.Epoch != 2 || q.Control.TaskID != "task.1" {
		t.Fatalf("stop: %v %+v", err, q.Control)
	}
	if got := code(q.Control.Admit(agentWrite(task, g))); got != "CONTROL_EPOCH_STALE at /" {
		t.Fatalf("the stopped task: %s", got)
	}
	if got := code(q.Control.Admit(humanWrite(nil, g))); got != "OBJECT_CONTROL_CONFLICT at /" {
		t.Fatalf("a write without a claim while stopping: %s", got)
	}
	stopped := &Claim{TaskID: "task.1", Epoch: 2}
	if got := code(q.Control.Admit(humanWrite(stopped, g))); got != "OBJECT_CONTROL_CONFLICT at /" {
		t.Fatalf("a claim of the stop's epoch: %s", got)
	}
	if got := code(q.Stop(stopped, 2, human("lead"), now)); got != "INVALID_STATE at /args/quote_id" {
		t.Fatalf("stop twice: %s", got)
	}
	if err := q.Release(stopped, 2, human("lead"), now); err != nil || q.Control.State != Unbound || q.Control.Epoch != 3 ||
		q.Control.TaskID != "" || q.Control.Delegation != (Delegation{}) {
		t.Fatalf("release: %v %+v", err, q.Control)
	}
	if err := q.Control.Admit(agentWrite(nil, g)); err != nil {
		t.Fatalf("released: %v", err)
	}
	if got := code(q.Release(nil, 3, human("lead"), now)); got != "INVALID_STATE at /args/quote_id" {
		t.Fatalf("release an unbound root: %s", got)
	}
	if err := q.Bind("task.2", delegated, 3, kernel.ActorRef{Kind: kernel.ActorSystem, ID: "tsk"}, 9, g.read, now); err != nil || q.Control.Epoch != 4 {
		t.Fatalf("rebind: %v", err)
	}
	if got := code(q.Control.Admit(agentWrite(task, g))); got != "CONTROL_EPOCH_STALE at /" {
		t.Fatalf("the first task after a rebinding: %s", got)
	}
	if err := q.Control.Admit(agentWrite(&Claim{TaskID: "task.2", Epoch: 4}, g)); err != nil {
		t.Fatalf("the new task: %v", err)
	}
	if q.RootVersion != 0 {
		t.Fatal("control never moves the root version")
	}
}

// SM-TRD-CONTROL as A0 committed it, every transition once: bind, stop, enter_manual, stop (from
// manual), handoff, stop, enter_manual, release. Each moves the control epoch by exactly one and keeps
// the task from the bind to the release; the root version never moves (WBS S4-01b).
func TestControlStateMachine(t *testing.T) {
	q := New("quote.1", "author", "")
	g := fixtureGrants()
	if err := q.Bind("task.1", delegated, 0, kernel.ActorRef{Kind: kernel.ActorSystem, ID: "tsk"}, 7, g.read, issuedAt); err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		name string
		do   func(c *Claim, epoch int64) error
		want ControlState
	}{
		{"stop", func(c *Claim, e int64) error { return q.Stop(c, e, human("lead"), issuedAt) }, Stopping},
		{"enter_manual", func(c *Claim, e int64) error { return q.EnterManual(c, e, human("lead"), issuedAt) }, Manual},
		{"stop from manual", func(c *Claim, e int64) error { return q.Stop(c, e, human("lead"), issuedAt) }, Stopping},
		{"handoff", func(c *Claim, e int64) error { return q.Handoff(c, delegated, e, human("lead"), 8, g.read, issuedAt) }, Bound},
		{"stop again", func(c *Claim, e int64) error { return q.Stop(c, e, human("lead"), issuedAt) }, Stopping},
		{"enter_manual again", func(c *Claim, e int64) error {
			return q.EnterManual(c, e, kernel.ActorRef{Kind: kernel.ActorSystem, ID: "tsk"}, issuedAt)
		}, Manual},
		{"release from manual", func(c *Claim, e int64) error { return q.Release(c, e, human("lead"), issuedAt) }, Unbound},
	}
	for i, s := range steps {
		epoch := int64(i + 1)
		if err := s.do(&Claim{TaskID: "task.1", Epoch: epoch}, epoch); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if c := q.Control; c.State != s.want || c.Epoch != epoch+1 || (c.State != Unbound && c.TaskID != "task.1") || !c.Changed {
			t.Fatalf("after %s: %+v", s.name, c)
		}
	}
	if q.RootVersion != 0 {
		t.Fatal("control never moves the root version")
	}
	// release from bound and from stopping (the cancelled task's stopping release, S3-D11's kept case)
	for _, from := range []ControlState{Bound, Stopping} {
		r := New("quote.2", "author", "")
		if err := r.Bind("task.9", delegated, 0, human("author"), 7, g.read, issuedAt); err != nil {
			t.Fatal(err)
		}
		if from == Stopping {
			if err := r.Stop(&Claim{TaskID: "task.9", Epoch: 1}, 1, human("lead"), issuedAt); err != nil {
				t.Fatal(err)
			}
		}
		e := r.Control.Epoch
		if err := r.Release(&Claim{TaskID: "task.9", Epoch: e}, e, human("lead"), issuedAt); err != nil || r.Control.State != Unbound ||
			r.Control.Epoch != e+1 {
			t.Fatalf("release from %s: %v %+v", from, err, r.Control)
		}
	}
}

// Decision D-4 (TSK-INV-004 negative 绕过 TSK 释放根返回 OBJECT_CONTROL_CONFLICT; SM-TRD-CONTROL: every
// change with the bound task's claim): stop, enter_manual, handoff and release refuse a direct call
// (no claim) and another task's claim with OBJECT_CONTROL_CONFLICT, an earlier epoch's claim with
// CONTROL_EPOCH_STALE, then a stale expected epoch with VERSION_CONFLICT and a transition the state
// does not have with INVALID_STATE; a refused change leaves the control as it was.
func TestControlChangesNeedTheBoundTasksClaim(t *testing.T) {
	g := fixtureGrants()
	changes := map[string]func(q *Quote, c *Claim, expected int64) error{
		"stop":         func(q *Quote, c *Claim, e int64) error { return q.Stop(c, e, human("lead"), issuedAt) },
		"enter_manual": func(q *Quote, c *Claim, e int64) error { return q.EnterManual(c, e, human("lead"), issuedAt) },
		"handoff": func(q *Quote, c *Claim, e int64) error {
			return q.Handoff(c, delegated, e, human("lead"), 7, g.read, issuedAt)
		},
		"release": func(q *Quote, c *Claim, e int64) error { return q.Release(c, e, human("lead"), issuedAt) },
	}
	// bound at epoch 1 or stopping at epoch 2, task.1
	root := func(state ControlState) *Quote {
		q := New("quote.1", "author", "")
		if err := q.Bind("task.1", delegated, 0, human("author"), 7, g.read, issuedAt); err != nil {
			t.Fatal(err)
		}
		if state == Stopping {
			if err := q.Stop(&Claim{TaskID: "task.1", Epoch: 1}, 1, human("lead"), issuedAt); err != nil {
				t.Fatal(err)
			}
		}
		q.Control.Changed = false
		return q
	}
	for name, change := range changes {
		for _, state := range []ControlState{Bound, Stopping} {
			e := root(state).Control.Epoch
			for what, c := range map[string]struct {
				claim    *Claim
				expected int64
				want     string
			}{
				"no claim":         {nil, e, "OBJECT_CONTROL_CONFLICT at /"},
				"another task":     {&Claim{TaskID: "task.2", Epoch: e}, e, "OBJECT_CONTROL_CONFLICT at /"},
				"an earlier epoch": {&Claim{TaskID: "task.1", Epoch: e - 1}, e, "CONTROL_EPOCH_STALE at /"},
				"a stale expected": {&Claim{TaskID: "task.1", Epoch: e}, e - 1, "VERSION_CONFLICT at /args/expected_control_epoch"},
			} {
				q := root(state)
				before := q.Control
				if got := code(change(q, c.claim, c.expected)); got != c.want || q.Control != before {
					t.Errorf("%s on %s, %s: %s, want %s (%+v)", name, state, what, got, c.want, q.Control)
				}
			}
		}
		q := New("quote.1", "author", "")
		if got := code(change(q, nil, 0)); got != "INVALID_STATE at /args/quote_id" {
			t.Errorf("%s on a root never bound: %s", name, got)
		}
		if got := code(change(q, &Claim{TaskID: "task.1", Epoch: 1}, 0)); got != "CONTROL_EPOCH_STALE at /" {
			t.Errorf("%s with a claim on a root never bound: %s", name, got)
		}
	}
	for name, c := range map[string]struct {
		state  ControlState
		change string
	}{
		"enter_manual from bound": {Bound, "enter_manual"}, "handoff from bound": {Bound, "handoff"}, "stop while stopping": {Stopping, "stop"},
	} {
		q := root(c.state)
		e := q.Control.Epoch
		if got := code(changes[c.change](q, &Claim{TaskID: "task.1", Epoch: e}, e)); got != "INVALID_STATE at /args/quote_id" {
			t.Errorf("%s: %s", name, got)
		}
	}
	q := root(Stopping)
	if err := q.EnterManual(&Claim{TaskID: "task.1", Epoch: 2}, 2, human("lead"), issuedAt); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]string{"enter_manual from manual": "enter_manual", "handoff from manual": "handoff"} {
		if got := code(changes[change](q, &Claim{TaskID: "task.1", Epoch: 3}, 3)); got != "INVALID_STATE at /args/quote_id" {
			t.Errorf("%s: %s", name, got)
		}
	}
}

// Decision D-3 (SM-TRD-CONTROL stopping → manual: 此后只接受携带当前声明的人工写入（AI 返回
// CONTROL_MODE_MISMATCH）; TSK-INV-004; Q01 V0.1 store.py execute: CONTROL_EPOCH_STALE before
// CONTROL_MODE_MISMATCH): a manual root admits the bound task's current claim from a human only;
// without the claim or with another task's it is OBJECT_CONTROL_CONFLICT, an AI agent's or the
// system's write with the claim CONTROL_MODE_MISMATCH, the AI's claim of the bound epoch before the
// takeover CONTROL_EPOCH_STALE. Manual writes are each under the human's own grant, so the binding's
// delegation is not consulted.
func TestManualControlAdmitsHumansWithTheClaim(t *testing.T) {
	g := fixtureGrants()
	q := New("quote.1", "author", "")
	if err := q.Bind("task.1", delegated, 0, human("author"), 7, g.read, issuedAt); err != nil {
		t.Fatal(err)
	}
	if err := q.Stop(&Claim{TaskID: "task.1", Epoch: 1}, 1, human("lead"), issuedAt); err != nil {
		t.Fatal(err)
	}
	if err := q.EnterManual(&Claim{TaskID: "task.1", Epoch: 2}, 2, human("lead"), issuedAt); err != nil {
		t.Fatal(err)
	}
	manual := &Claim{TaskID: "task.1", Epoch: 3}
	changed := grants{"author": {Kind: kernel.ActorHuman, Active: false, ChangedEpoch: 9}}
	for name, c := range map[string]struct {
		w    Write
		want string
	}{
		"a human with the claim":                {humanWrite(manual, g), "OK"},
		"a human after the delegation changed":  {humanWrite(manual, changed), "OK"},
		"an AI agent with the claim":            {agentWrite(manual, g), "CONTROL_MODE_MISMATCH at /"},
		"the system with the claim":             {Write{Claim: manual, Actor: kernel.ActorSystem, Grants: g.read}, "CONTROL_MODE_MISMATCH at /"},
		"a human without the claim":             {humanWrite(nil, g), "OBJECT_CONTROL_CONFLICT at /"},
		"an AI agent without the claim":         {agentWrite(nil, g), "OBJECT_CONTROL_CONFLICT at /"},
		"a human with another task's claim":     {humanWrite(&Claim{TaskID: "task.2", Epoch: 3}, g), "OBJECT_CONTROL_CONFLICT at /"},
		"the AI's claim of the bound epoch":     {agentWrite(&Claim{TaskID: "task.1", Epoch: 1}, g), "CONTROL_EPOCH_STALE at /"},
		"a human with the stop's epoch's claim": {humanWrite(&Claim{TaskID: "task.1", Epoch: 2}, g), "CONTROL_EPOCH_STALE at /"},
	} {
		if got := code(q.Control.Admit(c.w)); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
}

// Decision D-5 (replaces S3-D12): a binding holds while the grants of the task's initiator and agent
// are active at the changed epochs the bind recorded. A change of either — new permissions, a data
// scope, a revocation — fences the bound task's writes with OBJECT_CONTROL_CONFLICT, whoever writes; a
// change of anyone else's grant does not. The bind names an active human initiator and an active AI
// agent (REFERENCE_INVALID otherwise); the handoff records the delegation anew, so a task stopped
// after a change writes again once handed back. A binding without a delegation (bound before TRD
// 000009) admits no write.
func TestBindingHoldsWhileTheDelegationIsUnchanged(t *testing.T) {
	g := fixtureGrants()
	for name, c := range map[string]struct {
		b    Binding
		g    func(grants)
		want string
	}{
		"an initiator without a grant": {Binding{Initiator: "nobody", Agent: "agent.1"}, nil, "REFERENCE_INVALID at /args/initiator_id"},
		"an inactive initiator": {delegated, func(g grants) { g["author"] = Grant{Kind: kernel.ActorHuman, ChangedEpoch: 6} },
			"REFERENCE_INVALID at /args/initiator_id"},
		"an AI initiator":          {Binding{Initiator: "agent.1", Agent: "agent.1"}, nil, "REFERENCE_INVALID at /args/initiator_id"},
		"a human agent":            {Binding{Initiator: "author", Agent: "lead"}, nil, "REFERENCE_INVALID at /args/agent_id"},
		"an agent without a grant": {Binding{Initiator: "author", Agent: "agent.x"}, nil, "REFERENCE_INVALID at /args/agent_id"},
		"an active delegation":     {delegated, nil, "OK"},
	} {
		h := fixtureGrants()
		if c.g != nil {
			c.g(h)
		}
		q := New("quote.1", "author", "")
		if got := code(q.Bind("task.1", c.b, 0, human("author"), 7, h.read, issuedAt)); got != c.want || (got != "OK") != (q.Control.State == Unbound) {
			t.Errorf("bind, %s: %s, want %s (%+v)", name, got, c.want, q.Control)
		}
	}

	q := New("quote.1", "author", "")
	if err := q.Bind("task.1", delegated, 0, human("author"), 7, g.read, issuedAt); err != nil {
		t.Fatal(err)
	}
	task := &Claim{TaskID: "task.1", Epoch: 1}
	for name, c := range map[string]struct {
		change func(grants)
		want   string
	}{
		"another actor's grant changed":  {func(g grants) { g["lead"] = Grant{Kind: kernel.ActorHuman, Active: true, ChangedEpoch: 12} }, "OK"},
		"the initiator's grant changed":  {func(g grants) { g["author"] = Grant{Kind: kernel.ActorHuman, Active: true, ChangedEpoch: 12} }, "OBJECT_CONTROL_CONFLICT at /"},
		"the initiator's grant revoked":  {func(g grants) { g["author"] = Grant{Kind: kernel.ActorHuman, ChangedEpoch: 12} }, "OBJECT_CONTROL_CONFLICT at /"},
		"the agent's grant changed":      {func(g grants) { g["agent.1"] = Grant{Kind: kernel.ActorAI, Active: true, ChangedEpoch: 12} }, "OBJECT_CONTROL_CONFLICT at /"},
		"the agent's grant gone":         {func(g grants) { delete(g, "agent.1") }, "OBJECT_CONTROL_CONFLICT at /"},
		"an inactive grant at the epoch": {func(g grants) { g["agent.1"] = Grant{Kind: kernel.ActorAI, ChangedEpoch: 5} }, "OBJECT_CONTROL_CONFLICT at /"},
	} {
		h := fixtureGrants()
		c.change(h)
		for _, w := range []Write{agentWrite(task, h), humanWrite(task, h)} {
			if got := code(q.Control.Admit(w)); got != c.want {
				t.Errorf("%s, a write by %s: %s, want %s", name, w.Actor, got, c.want)
			}
		}
	}
	broken := errors.New("guard read failed")
	if err := q.Control.Admit(Write{Claim: task, Actor: kernel.ActorAI, Grants: func(string) (Grant, error) { return Grant{}, broken }}); !errors.Is(err, broken) {
		t.Fatalf("a failing read of the grants: %v", err)
	}

	// The initiator's grant changed: the task is stopped and handed back under the grants as they are.
	h := fixtureGrants()
	h["author"] = Grant{Kind: kernel.ActorHuman, Active: true, ChangedEpoch: 12}
	if err := q.Stop(task, 1, human("lead"), issuedAt); err != nil {
		t.Fatal(err)
	}
	if err := q.Handoff(&Claim{TaskID: "task.1", Epoch: 2}, delegated, 2, human("lead"), 9, h.read, issuedAt); err != nil {
		t.Fatal(err)
	}
	if d := q.Control.Delegation; d.InitiatorEpoch != 12 || d.AgentEpoch != 5 || q.Control.AuthorityEpoch != 9 || q.Control.TaskID != "task.1" {
		t.Fatalf("after the handoff %+v", q.Control)
	}
	if err := q.Control.Admit(agentWrite(&Claim{TaskID: "task.1", Epoch: 3}, h)); err != nil {
		t.Fatalf("the task handed back: %v", err)
	}
	if err := q.Stop(&Claim{TaskID: "task.1", Epoch: 3}, 3, human("lead"), issuedAt); err != nil {
		t.Fatal(err)
	}
	if got := code(q.Handoff(&Claim{TaskID: "task.1", Epoch: 4}, Binding{Initiator: "author", Agent: "lead"}, 4, human("lead"), 9, h.read,
		issuedAt)); got != "REFERENCE_INVALID at /args/agent_id" || q.Control.State != Stopping {
		t.Fatalf("a handoff to a human agent: %s %+v", got, q.Control)
	}

	legacy := Control{State: Bound, Epoch: 1, TaskID: "task.1", AuthorityEpoch: 7}
	if got := code(legacy.Admit(agentWrite(task, g))); got != "OBJECT_CONTROL_CONFLICT at /" {
		t.Fatalf("a binding without a delegation: %s", got)
	}
}
