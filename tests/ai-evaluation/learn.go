package main

import (
	"context"
	"errors"
	"fmt"
)

// What the agent creates under IDs of its own choosing is learned from the task's records, never
// from a constant (S4 G2): the task's committed actions (GET /tsk/tasks/<id>/actions) name the
// object each one created, and the owning context says what that object is. The scripted model's
// IDs are fixed truth of the scripted arm only; a live model names things as it likes, within the
// identifier rule the prompt states.

// agentSourceID is the source ID the scripted model chooses for the extract it registers from the
// inquiry file. It differs per trial and input kind on purpose: nothing downstream may know it but
// through the task's records.
func (t *trial) agentSourceID() string {
	return fmt.Sprintf("src.inquiry.%s.%d", t.in.kind, t.n)
}

// committedObjects returns the objects the task's committed actions of bc's command created or
// changed, oldest first, with the actor who registered each action.
func (t *trial) committedObjects(ctx context.Context, bc, command string) ([]string, []any, error) {
	a, err := t.ops[actorClerk].get(ctx, "/api/brant/v1/tsk/tasks/"+taskID+"/actions")
	if err != nil {
		return nil, nil, err
	}
	var ids []string
	var by []any
	for _, x := range list(a["actions"]) {
		if str(dig(x, "target", "context")) == bc && str(dig(x, "target", "command")) == command && str(dig(x, "state")) == "committed" {
			if id := str(dig(x, "result", "object_id")); id != "" {
				ids = append(ids, id)
				by = append(by, dig(x, "registered_by"))
			}
		}
	}
	return ids, by, nil
}

// learnSource finds the source the agent registered from the trial's inquiry file: the object of the
// task's oldest committed source.register that EVD shows, to the owner, as an extract of that file
// (the rule the agent runner's facts follow). A phase that registered none is the agent's failure,
// not the operator's.
func (t *trial) learnSource(ctx context.Context) error {
	ids, by, err := t.committedObjects(ctx, "evd", "source.register")
	if err != nil {
		return err
	}
	for i, id := range ids {
		s, err := t.ops[actorClerk].get(ctx, "/api/brant/v1/evd/sources/"+id)
		var r *refusal
		if errors.As(err, &r) {
			continue
		}
		if err != nil {
			return err
		}
		if str(dig(s, "extract", "file_id")) == fileID {
			t.sourceID = id
			t.rec.Authority["registered_source"] = map[string]any{"source_id": id, "file_id": fileID, "source_kind": s["source_kind"],
				"registered_by": by[i], "learned_from": "the task's committed source.register action (TSK) and EVD's read of its source"}
			return nil
		}
	}
	return &phaseError{err: fmt.Errorf("the task's records show no committed source.register of the inquiry file %s (%d committed source.register)",
		fileID, len(ids))}
}

// learnResponse finds the customer response the agent (or, by hand, the owner) recorded for the task.
func (t *trial) learnResponse(ctx context.Context) error {
	ids, _, err := t.committedObjects(ctx, "trd", "quote.response.record")
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return &phaseError{err: errors.New("the task's records show no committed quote.response.record")}
	}
	t.learnedResp = ids[len(ids)-1]
	t.rec.Authority["recorded_response"] = t.learnedResp
	return nil
}

// learnOrder finds the order the agent (or, by hand, the owner) prepared for the task.
func (t *trial) learnOrder(ctx context.Context) error {
	ids, _, err := t.committedObjects(ctx, "trd", "order.prepare")
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return &phaseError{err: errors.New("the task's records show no committed order.prepare")}
	}
	t.learnedOrder = ids[len(ids)-1]
	t.rec.Authority["prepared_order"] = t.learnedOrder
	return nil
}

// response is the customer response of the trial: the one the task's records name, else the ID the
// scripted model and the scripted operator use.
func (t *trial) response() string {
	if t.learnedResp != "" {
		return t.learnedResp
	}
	return responseID
}

// order is the order of the trial: the one the task's records name, else the ID the scripted model
// and the scripted operator use.
func (t *trial) order() string {
	if t.learnedOrder != "" {
		return t.learnedOrder
	}
	return orderID
}
