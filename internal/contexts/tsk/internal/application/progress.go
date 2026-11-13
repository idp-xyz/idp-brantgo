package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/internal/domain/task"
)

// TSK's consumer of TRD's integration events (WBS S4-06b; Context Map R18 "TRD 集成事件 v1"; event
// storming PL-04, PL-06; SM-TSK-TASK task.resume and task.verify, both system): the Inbox consumer
// tsk.task_progress (adapters/events) reads each event as a task.Fact and calls ProgressOn in a
// transaction on TSK's own transaction owner, as the consumer's system actor. ProgressOn locks the
// tasks the fact may concern, asks each what the fact means to it (task.WakeOn) and runs task.resume
// and task.verify through TSK's command ledger, which joins the transaction, so the moves, their
// history, action results, audit entries and the Inbox record commit together.
//
// Each command is keyed by the consumer, the event, the task and the command: a delivery moves a
// task once. A verification starts only when no action of the task is submitted or unknown
// (TSK-INV-003; task.verify's guard); otherwise the task stays running and whoever settles the
// actions moves it on. Lock order (ADR-BR-011): the task rows FOR UPDATE in ID order, then, in the
// commands, the company's authority guard FOR SHARE.

// Progress is what a fact did: the tasks it resumed and those whose verification it started, in ID
// order.
type Progress struct {
	Resumed  []string
	Verified []string
}

// progressKey is the idempotency key of command on task for event of consumer.
func progressKey(consumer, event, taskID, command string) string {
	sum := sha256.Sum256([]byte(consumer + "\x1f" + event + "\x1f" + taskID + "\x1f" + command))
	return "tsk.in." + hex.EncodeToString(sum[:20])
}

func objectRef(r task.Ref) *api.ObjectRef {
	return &api.ObjectRef{Kind: r.Kind, ID: r.ID, Version: r.Version}
}

// ArrivalReason is the reason a resume on arrival r records: the object that arrived, at its
// version, and origin, the event that carried it.
func ArrivalReason(r task.Ref, origin string) string {
	return "arrived: " + r.Kind + " " + r.ID + " at version " + strconv.FormatInt(r.Version, 10) + " (" + origin + ")"
}

// ProgressOn applies fact f, carried by event eventID of consumer, in the consumer's transaction
// exec (ctx carries its scope and the consumer's system principal). origin names the event in the
// reason each resume records (ArrivalReason).
func (s Service) ProgressOn(ctx context.Context, exec bentopg.Executor, consumer, eventID string, f task.Fact, origin string) (Progress, error) {
	var out Progress
	sc := scope(ctx)
	ids, err := postgres.TasksConcerned(ctx, exec, sc, f.QuoteID, f.Answers)
	if err != nil {
		return out, err
	}
	for _, id := range ids {
		t, err := postgres.LoadTask(ctx, exec, sc, id, postgres.NoLock)
		if err != nil {
			return out, err
		}
		if t == nil || !t.Concerns(f) {
			continue
		}
		w := t.WakeOn(f)
		version := t.Version
		if w.Resume {
			r, err := s.ResumeTask(ctx, api.ResumeTask{IdempotencyKey: progressKey(consumer, eventID, id, api.CommandResume), TaskID: id,
				ExpectedVersion: version, Reason: ArrivalReason(w.Arrival, origin), Arrival: objectRef(w.Arrival)})
			if err != nil {
				return out, err
			}
			version = r.RowVersion
			out.Resumed = append(out.Resumed, id)
		}
		if !w.Verify {
			continue
		}
		n, err := s.Actions.Unsettled(ctx, exec, sc, id)
		if err != nil {
			return out, err
		}
		if n > 0 {
			continue
		}
		if _, err = s.VerifyTask(ctx, api.VerifyTask{IdempotencyKey: progressKey(consumer, eventID, id, api.CommandVerify), TaskID: id,
			ExpectedVersion: version}); err != nil {
			return out, err
		}
		out.Verified = append(out.Verified, id)
	}
	return out, nil
}
