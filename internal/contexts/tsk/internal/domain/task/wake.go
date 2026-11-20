package task

// What an integration event of TRD asks of a task (WBS S4-06b; Context Map R18 "TRD 集成事件 v1";
// event storming PL-04 "QuoteActivated → quote_ready 可核验", PL-06 "OrderActivated → order_active
// 可核验"; SM-TSK-TASK waiting_* → running by task.resume "输入、审批或外部结果到达：去重、版本及内容仍适用；
// 先重查上下文", running → verifying by task.verify). TSK's Inbox consumer reads each event as a Fact and
// asks every task it may concern (WakeOn); the commands then move the task as SM-TSK-TASK allows,
// and only so: a fact never completes a task (goal.verify does, TSK-INV-001), never touches a
// paused, stopping or ended one (BC-AT25: completed is history), and never another task's.
//
// A fact concerns a task whose target quote is the fact's quote, or whose wait awaits an object the
// fact answers. Then:
//
//	task running, the fact makes its goal verifiable         task.verify
//	task waiting, the fact makes its goal verifiable         task.resume on the fact's object, then task.verify
//	task waiting, the fact answers what it awaits            task.resume on that object
//	task waiting, the fact revokes an approval of its quote  task.resume on the revocation (the context changed)
//	anything else                                            nothing
//
// The wait's object matches an answered object of the same kind and identifier at the awaited
// version or a later one (an awaited version 0 takes any).

// Fact is an integration event of TRD as TSK reads it: the quote root it concerns, the objects it
// answers (most specific first; the first is the arrival a resume records when the task awaits none
// of them), the goal it makes verifiable (quote_ready, order_active, or none) and whether it
// revokes an approval of the quote.
type Fact struct {
	QuoteID    string
	Answers    []Ref
	Goal       string
	Revocation bool
}

// Wake is what a fact asks of a task: resume it on Arrival, start its verification, both or nothing.
type Wake struct {
	Resume  bool
	Arrival Ref
	Verify  bool
}

// Concerns reports whether fact f may concern the task: the task's target quote is f's quote, or
// its wait awaits an object f answers.
func (t *Task) Concerns(f Fact) bool {
	if t.Target.QuoteID != "" && t.Target.QuoteID == f.QuoteID {
		return true
	}
	_, ok := t.awaited(f)
	return ok
}

// awaited returns the object of f the task's wait awaits.
func (t *Task) awaited(f Fact) (Ref, bool) {
	if t.WaitCond == nil || t.WaitCond.Awaiting == nil {
		return Ref{}, false
	}
	a := *t.WaitCond.Awaiting
	for _, r := range f.Answers {
		if r.Kind == a.Kind && r.ID == a.ID && (a.Version == 0 || r.Version >= a.Version) {
			return r, true
		}
	}
	return Ref{}, false
}

// WakeOn decides what fact f asks of the task in its state now (the table above). It changes
// nothing; the commands decide again on the locked task.
func (t *Task) WakeOn(f Fact) Wake {
	if len(f.Answers) == 0 {
		return Wake{}
	}
	ours := t.Target.QuoteID != "" && t.Target.QuoteID == f.QuoteID
	goal := ours && f.Goal != "" && f.Goal == t.Goal
	switch {
	case t.State == Running:
		return Wake{Verify: goal}
	case t.State.Waiting():
		if r, ok := t.awaited(f); ok {
			return Wake{Resume: true, Arrival: r, Verify: goal}
		}
		if goal || (ours && f.Revocation) {
			return Wake{Resume: true, Arrival: f.Answers[0], Verify: goal}
		}
	}
	return Wake{}
}
