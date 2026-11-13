package execution

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"go.idp.xyz/idp-brant/internal/ai/adapters/model"
	"go.idp.xyz/idp-brant/internal/ai/capability"
	"go.idp.xyz/idp-brant/internal/ai/executionevidence"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	tskapi "go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

const hash = "0000000000000000000000000000000000000000000000000000000000000001"

// fakeTSK plays TSK's ActionExecution (B1's contract) over a fake target ledger: committed holds the
// keys the target committed.
type fakeTSK struct {
	claim     tskapi.Claim
	claimErr  error
	actions   map[string]*tskapi.ActionView
	committed map[string]bool
	keys      []string // TSK idempotency keys of the records, in order
	log       []string // "outcome@attempt"
}

func newFake(quote string, epoch int64) *fakeTSK {
	return &fakeTSK{claim: tskapi.Claim{TaskID: "task.1", QuoteID: quote, ControlEpoch: epoch, Lifecycle: tskapi.StateRunning},
		actions: map[string]*tskapi.ActionView{}, committed: map[string]bool{}}
}

func (f *fakeTSK) GetTask(context.Context, string) (tskapi.TaskView, error) {
	return tskapi.TaskView{}, nil
}
func (f *fakeTSK) Audit(context.Context) ([]kernel.AuditEntry, error)  { return nil, nil }
func (f *fakeTSK) Claim(context.Context, string) (tskapi.Claim, error) { return f.claim, f.claimErr }
func (f *fakeTSK) TaskActions(context.Context, string) ([]tskapi.ActionView, error) {
	var out []tskapi.ActionView
	for _, a := range f.actions {
		out = append(out, *a)
	}
	return out, nil
}

func (f *fakeTSK) GetAction(_ context.Context, id string) (tskapi.ActionView, error) {
	if a, ok := f.actions[id]; ok {
		return *a, nil
	}
	return tskapi.ActionView{}, kernel.Reject(kernel.CodeNotFound, "/action_id", "")
}

func (f *fakeTSK) receipt(a *tskapi.ActionView) tskapi.ActionReceipt {
	return tskapi.ActionReceipt{Receipt: tskapi.Receipt{ObjectID: a.ActionID, RowVersion: a.Version, Status: a.State},
		IdempotencyKey: a.ActionID, Attempt: a.Attempts, State: a.State, Result: a.Result}
}

func (f *fakeTSK) settle(a *tskapi.ActionView) bool {
	if f.committed[a.ActionID] {
		a.State, a.Result = tskapi.ActionCommitted, &tskapi.TargetReceipt{ActionID: "evd.act." + a.ActionID, ObjectID: "src.1", RowVersion: 1}
		a.Version++
		return true
	}
	return false
}

func (f *fakeTSK) RecordAttempt(_ context.Context, c tskapi.RecordAttempt) (tskapi.ActionReceipt, error) {
	f.keys = append(f.keys, c.IdempotencyKey)
	id := tskapi.ActionIDOf(c.BusinessIntentID)
	a, ok := f.actions[id]
	if !ok {
		a = &tskapi.ActionView{ActionID: id, BusinessIntentID: c.BusinessIntentID, Target: c.Target, IdempotencyKey: id}
		f.actions[id] = a
	}
	if a.Target != c.Target {
		return tskapi.ActionReceipt{}, kernel.Reject(kernel.CodeIdempotencyPayloadConflict, "/args/business_intent_id", "")
	}
	f.log = append(f.log, fmt.Sprintf("%s@%d", c.Outcome, a.Attempts))
	if a.State == tskapi.ActionCommitted || a.State == tskapi.ActionConflict || f.settle(a) {
		return f.receipt(a), nil
	}
	switch c.Outcome {
	case tskapi.ActionSubmitted:
		if a.State == tskapi.ActionSubmitted || a.State == tskapi.ActionUnknown {
			return tskapi.ActionReceipt{}, kernel.RejectAs(tskapi.CodeExternalResultUnknown, kernel.CategoryConflict, "/args/business_intent_id", "")
		}
		a.Attempts++
		a.State = tskapi.ActionSubmitted
	default:
		a.State = c.Outcome
	}
	a.Version++
	return f.receipt(a), nil
}

func (f *fakeTSK) ReconcileAction(_ context.Context, c tskapi.ReconcileAction) (tskapi.ActionReceipt, error) {
	f.keys = append(f.keys, c.IdempotencyKey)
	a := f.actions[c.ActionID]
	f.log = append(f.log, "reconcile")
	if !f.settle(a) {
		a.State = tskapi.ActionNotCommitted
		a.Version++
	}
	return f.receipt(a), nil
}

type fakeEVD struct{}

func (fakeEVD) RequestOf(cmd evdapi.Command) (kernel.ActionRequest, error) {
	if _, ok := cmd.(evdapi.RegisterSource); !ok {
		return kernel.ActionRequest{}, kernel.Reject(kernel.CodeSchemaInvalid, "/command", "")
	}
	return kernel.ActionRequest{Command: evdapi.CommandRegisterSource, RequestHash: hash}, nil
}

func executor(f *fakeTSK) Executor { return Executor{Tasks: f, Requests: Requests{EVD: fakeEVD{}}} }

func step(send func(ctx context.Context, cmd any) (capability.Result, error)) Step {
	return Step{TaskID: "task.1", BusinessIntentID: "intent.1", Context: tskapi.TargetEVD,
		Command: evdapi.RegisterSource{SourceID: "src.1", SourceKind: evdapi.KindManualText, ContentText: "x"}, Send: send}
}

func ok(r capability.Receipt) (capability.Result, error) { return capability.Result{Receipt: r}, nil }

// TSK-INV-003, D-2, UL AI-Receipt: the attempt is in TSK before the command leaves; the command
// carries the action's key and, on a bound root, the task's claim; the outcome is reconciled with
// the target's ledger (VERIFIED). A step on an unbound task carries no claim.
func TestStepIsRecordedBeforeItIsSent(t *testing.T) {
	for _, quote := range []string{"quote.1", ""} {
		f := newFake(quote, 3)
		e := executor(f)
		r, err := e.Run(context.Background(), step(func(ctx context.Context, cmd any) (capability.Result, error) {
			a := f.actions[tskapi.ActionIDOf("intent.1")]
			if a == nil || a.State != tskapi.ActionSubmitted || a.Attempts != 1 {
				return capability.Result{}, errors.New("sent before the intent was recorded")
			}
			if cmd.(evdapi.RegisterSource).IdempotencyKey != tskapi.ActionIDOf("intent.1") {
				return capability.Result{}, errors.New("the key is not the action's")
			}
			claim, has := trdapi.ControlFromContext(ctx)
			if has != (quote != "") || (has && (claim.TaskID != "task.1" || claim.ControlEpoch != 3)) {
				return capability.Result{}, fmt.Errorf("claim %+v %t", claim, has)
			}
			f.committed[a.ActionID] = true
			return ok(capability.Receipt{ActionID: "evd.act." + a.ActionID, ObjectID: "src.1", RowVersion: 1})
		}))
		if err != nil || r.Stage != Verified || r.Attempt != 1 || r.Stored == nil || r.Result == nil || r.Replayed || r.ControlEpoch != 3 ||
			r.Target != (tskapi.TargetCommand{Context: tskapi.TargetEVD, Command: evdapi.CommandRegisterSource, RequestHash: hash}) {
			t.Fatalf("quote %q: %+v %v", quote, r, err)
		}
		if fmt.Sprint(f.log) != "[submitted@0 reconcile]" {
			t.Fatalf("log %v", f.log)
		}
	}
}

// CT11: an intent that committed is answered with its result and not sent again.
func TestCommittedIntentIsNotSentAgain(t *testing.T) {
	f := newFake("", 0)
	e := executor(f)
	send := func(ctx context.Context, cmd any) (capability.Result, error) {
		f.committed[tskapi.ActionIDOf("intent.1")] = true
		return ok(capability.Receipt{ActionID: "evd.act", ObjectID: "src.1", RowVersion: 1})
	}
	if _, err := e.Run(context.Background(), step(send)); err != nil {
		t.Fatal(err)
	}
	r, err := e.Run(context.Background(), step(func(context.Context, any) (capability.Result, error) {
		return capability.Result{}, errors.New("sent again")
	}))
	if err != nil || r.Stage != Verified || !r.Replayed || r.Stored == nil || r.Result != nil {
		t.Fatalf("%+v %v", r, err)
	}
}

// Baseline 17.2: an attempt left open by an earlier run is reconciled before a new one is opened —
// committed answers its result without a send, not committed opens the next attempt with the same
// key.
func TestOpenAttemptIsReconciledFirst(t *testing.T) {
	for _, committed := range []bool{true, false} {
		f := newFake("", 0)
		id := tskapi.ActionIDOf("intent.1")
		f.actions[id] = &tskapi.ActionView{ActionID: id, BusinessIntentID: "intent.1", IdempotencyKey: id, State: tskapi.ActionSubmitted, Attempts: 1,
			Version: 1, Target: tskapi.TargetCommand{Context: tskapi.TargetEVD, Command: evdapi.CommandRegisterSource, RequestHash: hash}}
		f.committed[id] = committed
		sent := 0
		r, err := executor(f).Run(context.Background(), step(func(ctx context.Context, cmd any) (capability.Result, error) {
			sent++
			f.committed[id] = true
			return ok(capability.Receipt{ActionID: "evd.act." + id, ObjectID: "src.1", RowVersion: 1})
		}))
		if err != nil || r.Stage != Verified || (sent == 1) == committed || (r.Attempt == 2) == committed {
			t.Fatalf("committed %t: %+v %v, sent %d", committed, r, err, sent)
		}
	}
}

// A refusal — of the admission or of the target — is recorded as the attempt's rejection with its
// code and returned; the step stays ACCEPTED and nothing is sent after a refused admission.
func TestRefusalsAreRecorded(t *testing.T) {
	f := newFake("", 0)
	r, err := executor(f).Run(context.Background(), step(func(context.Context, any) (capability.Result, error) {
		return capability.Result{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}))
	if rej, ok := kernel.AsRejection(err); !ok || rej.Code != kernel.CodeVersionConflict || r.Stage != Accepted {
		t.Fatalf("%+v %v", r, err)
	}
	if a := f.actions[r.ActionID]; a.State != tskapi.ActionRejected || fmt.Sprint(f.log) != "[submitted@0 rejected@1]" {
		t.Fatalf("%+v %v", a, f.log)
	}

	f = newFake("", 0)
	s := step(func(context.Context, any) (capability.Result, error) { return capability.Result{}, errors.New("sent") })
	s.Admit = func(context.Context, string) error {
		return kernel.RejectAs("GRANT_INVALID", kernel.CategoryForbidden, "/grant", "")
	}
	r, err = executor(f).Run(context.Background(), s)
	if rej, ok := kernel.AsRejection(err); !ok || rej.Code != "GRANT_INVALID" || r.Stage != Accepted || f.actions[r.ActionID].State != tskapi.ActionRejected {
		t.Fatalf("%+v %v", r, err)
	}
}

// An answer lost on the way back is recorded unknown and reconciled at once: committed at the target
// is VERIFIED; nothing there is TOOL_UNAVAILABLE, and the action is not committed, ready for the
// next attempt with the same key.
func TestLostAnswersAreReconciled(t *testing.T) {
	f := newFake("", 0)
	r, err := executor(f).Run(context.Background(), step(func(context.Context, any) (capability.Result, error) {
		f.committed[tskapi.ActionIDOf("intent.1")] = true
		return capability.Result{}, errors.New("connection reset")
	}))
	if err != nil || r.Stage != Verified || r.Stored == nil {
		t.Fatalf("%+v %v", r, err)
	}
	f = newFake("", 0)
	r, err = executor(f).Run(context.Background(), step(func(context.Context, any) (capability.Result, error) {
		return capability.Result{}, errors.New("connection refused")
	}))
	if !errors.Is(err, model.ErrUnavailable) || r.Stage != Accepted || f.actions[r.ActionID].State != tskapi.ActionNotCommitted ||
		fmt.Sprint(f.log) != "[submitted@0 unknown@1 reconcile]" {
		t.Fatalf("%+v %v %v", r, err, f.log)
	}
}

// The step's refusals before anything is recorded: the claim's, the request's at its context, a
// hash other than the one the caller bound, an intent that is not an identifier.
func TestStepsAreCheckedBeforeTheRecord(t *testing.T) {
	f := newFake("", 0)
	f.claimErr = kernel.RejectAs(tskapi.CodeControlModeMismatch, kernel.CategoryConflict, "/", "")
	send := func(context.Context, any) (capability.Result, error) { return capability.Result{}, errors.New("sent") }
	if _, err := executor(f).Run(context.Background(), step(send)); !errors.Is(err, f.claimErr) {
		t.Fatal(err)
	}
	f = newFake("", 0)
	s := step(send)
	s.Command = evdapi.RegisterFile{FileID: "f.1"}
	if _, err := executor(f).Run(context.Background(), s); err == nil {
		t.Fatal("a command EVD refuses")
	}
	s = step(send)
	s.Context = tskapi.TargetINQ
	if _, err := executor(f).Run(context.Background(), s); !errors.Is(err, ErrNotACommand) {
		t.Fatal(err)
	}
	s = step(send)
	s.RequestHash = "ff" + hash[2:]
	if _, err := executor(f).Run(context.Background(), s); err == nil {
		t.Fatal("another hash")
	}
	s = step(send)
	s.BusinessIntentID = "has space"
	if _, err := executor(f).Run(context.Background(), s); err == nil {
		t.Fatal("intent")
	}
	if len(f.keys) != 0 {
		t.Fatalf("recorded %v", f.keys)
	}
}

// B1's contract: the executor's TSK keys are stable per action, attempt and outcome; intents and
// proposals are stable per request and differ for another one.
func TestKeysAndIntentsAreStable(t *testing.T) {
	a := tskapi.ActionIDOf("intent.1")
	first := recordKey(a, 1, "submitted")
	if first != recordKey(tskapi.ActionIDOf("intent.1"), 1, tskapi.ActionSubmitted) || first == recordKey(a, 2, "submitted") ||
		recordKey(a, 1, "submitted") == recordKey(a, 1, "rejected") || !kernel.ValidIdentifier(recordKey(a, 1, "x")) {
		t.Fatal("record keys")
	}
	i := IntentOf("task.1", "rfq.save_draft", "rfq.1", hash)
	if i != IntentOf("task.1", "rfq.save_draft", "rfq.1", hash) || i == IntentOf("task.2", "rfq.save_draft", "rfq.1", hash) ||
		i == IntentOf("task.1", "rfq.save_draft", "rfq.1", "ff"+hash[2:]) || !kernel.ValidIdentifier(i) ||
		!kernel.ValidIdentifier(ProposalID("task.1", "order.prepare", hash)) {
		t.Fatal("intents")
	}
	cmd, err := WithKey(evdapi.RegisterSource{SourceID: "s"}, "k.1")
	if err != nil || cmd.(evdapi.RegisterSource).IdempotencyKey != "k.1" {
		t.Fatal(err)
	}
	if _, err = WithKey("x", "k"); err == nil {
		t.Fatal("not a struct")
	}
	if _, err = WithKey(struct{ A string }{}, "k"); err == nil {
		t.Fatal("no key field")
	}
}

// The runner's system actor holds the system rows of SM-TSK-TASK it takes and the reads they need;
// a task without a bundle, or with another one, does not run.
func TestRunnerActorAndBundleCheck(t *testing.T) {
	if !kernel.ValidIdentifier(RunnerActor) {
		t.Fatal(RunnerActor)
	}
	// The stop barrier: task.finish_stop, the reads of every context a task step writes, TRD's
	// enter_manual and handoff; never a human-only command.
	for _, p := range []string{tskapi.CommandFinishStop, "evd.read", "inq.read", "sup.read", "cst.read", "trd.read",
		trdapi.CommandEnterManualControl, trdapi.CommandHandoffControl, trdapi.CommandBindControl, tskapi.CommandVerifyGoal} {
		if !slices.Contains(RunnerPermissions, p) {
			t.Errorf("the runner lacks %s", p)
		}
	}
	for _, p := range []string{tskapi.CommandTakeControl, tskapi.CommandHandoff, tskapi.CommandCancel, tskapi.CommandFail, tskapi.CommandRelease,
		trdapi.CommandStopControl, trdapi.CommandReleaseControl} {
		if slices.Contains(RunnerPermissions, p) {
			t.Errorf("the runner holds the human-only %s", p)
		}
	}
	r := &Runner{ref: executionevidenceRef("q01.agent", hash), Accepts: []string{"ff" + hash[2:]}}
	for b, want := range map[*tskapi.BundleRef]string{
		nil: "/execution_bundle",
		{BundleID: "q01.agent", SHA256: "ee" + hash[2:]}: "/execution_bundle/sha256",
		{BundleID: "other", SHA256: hash}:                "/execution_bundle/sha256",
		{BundleID: "q01.agent", SHA256: hash}:            "",
		{BundleID: "q01.agent", SHA256: "ff" + hash[2:]}: "",
	} {
		err := r.CheckBundle(tskapi.TaskView{ExecutionBundle: b})
		rej, isRej := kernel.AsRejection(err)
		if (want == "") != (err == nil) || (want != "" && (!isRej || rej.Code != "EXECUTION_BUNDLE_INCOMPATIBLE" || rej.Fields[0].Path != want)) {
			t.Errorf("%+v: %v", b, err)
		}
	}
}

func executionevidenceRef(id, sha string) executionevidence.Ref {
	return executionevidence.Ref{BundleID: id, SHA256: sha}
}

type fakeTRD struct{ command string }

func (f fakeTRD) RequestOf(trdapi.Command) (kernel.ActionRequest, error) {
	return kernel.ActionRequest{Command: f.command, RequestHash: hash}, nil
}

// Only TSK changes a root's control (R17, D-4): a step naming an object_control command is refused
// before anything is read or recorded.
func TestObjectControlIsNeverATaskStep(t *testing.T) {
	f := newFake("quote.1", 1)
	e := Executor{Tasks: f, Requests: Requests{TRD: fakeTRD{command: trdapi.CommandStopControl}}}
	_, err := e.Run(context.Background(), Step{TaskID: "task.1", BusinessIntentID: "intent.stop", Context: tskapi.TargetTRD,
		Command: trdapi.StopControl{QuoteID: "quote.1"}, Send: func(context.Context, any) (capability.Result, error) {
			return capability.Result{}, errors.New("sent")
		}})
	if rej, ok := kernel.AsRejection(err); !ok || rej.Code != kernel.CodePermissionDenied || rej.Fields[0].Path != "/command" || len(f.keys) != 0 {
		t.Fatalf("%v, recorded %v", err, f.keys)
	}
}
