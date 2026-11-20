package eer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeEER is a synthetic stand-in for EER's Runtime API v1, written by hand from its documented
// behaviour (nothing of EER's code, contract files or fixtures): bearer tokens map to an actor of
// tenant "pilot"; intents need their originating event; an execution ID is bound to its request's
// content; T2 waits for approval; a cancellation is a signal the execution takes at its next read;
// approvals and cancellations only from the token's mapped actor.
type fakeEER struct {
	mu         sync.Mutex
	t          *testing.T
	tokens     map[string]string // token -> mapped actor
	events     map[string]string // event_id -> canonical content
	intents    map[string]string // execution_id -> canonical request
	execs      map[string]*executionDoc
	cancelling map[string]*pendingCancel
	tasks      map[string]*HumanTask
	taskEvents map[string][]HumanTaskEvent
	calls      map[string]int // "METHOD /path-prefix" -> count
	failNext   map[string]int // "METHOD prefix" -> status to answer once
	stateOver  map[string]string
}

// pendingCancel is a cancellation signal the execution has not yet taken: it ends the execution at
// the read after the next one (EER's workflow takes the signal at its next safe point).
type pendingCancel struct {
	reason string
	reads  int
}

func newFakeEER(t *testing.T) (*fakeEER, *httptest.Server) {
	f := &fakeEER{t: t, tokens: map[string]string{"tok-operator": "operator", "tok-bob": "bob"}, events: map[string]string{},
		intents: map[string]string{}, execs: map[string]*executionDoc{}, cancelling: map[string]*pendingCancel{}, tasks: map[string]*HumanTask{},
		taskEvents: map[string][]HumanTaskEvent{}, calls: map[string]int{}, failNext: map[string]int{}, stateOver: map[string]string{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeEER) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[key]
}

func fakeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": msg}})
}

func fakeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func ptr[T any](v T) *T { return &v }

func (f *fakeEER) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	route := r.Method + " " + r.URL.Path
	for prefix, status := range f.failNext {
		if strings.HasPrefix(route, prefix) {
			delete(f.failNext, prefix)
			fakeError(w, status, "execution_unavailable", "injected")
			return
		}
	}
	actor, ok := f.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	if !ok {
		fakeError(w, http.StatusUnauthorized, "invalid_token", "the bearer token is invalid")
		return
	}
	var body map[string]any
	if r.Method == http.MethodPost {
		if r.Header.Get("Content-Type") != "application/json" {
			fakeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "json")
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			fakeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if body["tenant_id"] != nil && body["tenant_id"] != "pilot" {
			fakeError(w, http.StatusForbidden, "access_denied", "request is not authorized")
			return
		}
	}
	path := r.URL.Path
	switch {
	case route == "POST /v1/events":
		f.calls["POST /v1/events"]++
		raw, _ := json.Marshal(body)
		id, _ := body["event_id"].(string)
		if prev, ok := f.events[id]; ok && prev != string(raw) {
			fakeError(w, http.StatusConflict, "event_identity_conflict", "event_id is already bound to different immutable content")
			return
		}
		f.events[id] = string(raw)
		fakeJSON(w, http.StatusCreated, map[string]any{"tenant_id": "pilot", "event_id": id, "sequence": len(f.events), "recorded_at": time.Now().UTC()})
	case route == "POST /v1/intents":
		f.calls["POST /v1/intents"]++
		f.accept(w, body)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/executions/") && strings.HasSuffix(path, "/invocations"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/executions/"), "/invocations")
		if _, ok := f.execs[id]; !ok {
			fakeError(w, http.StatusNotFound, "execution_not_found", "no execution")
			return
		}
		fakeJSON(w, http.StatusOK, map[string]any{"execution_id": id, "items": []map[string]any{{"tenant_id": "pilot", "schema_version": 1,
			"idempotency_key": id + "/step-1/1", "execution_id": id, "step_id": "step-1", "attempt": 1, "delivery_attempt": 1, "actor_id": "oms-connector",
			"capability": "shipment.address.update", "result": map[string]any{"verified": true}}}})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/executions/"):
		f.calls["GET /v1/executions"]++
		id := strings.TrimPrefix(path, "/v1/executions/")
		e, ok := f.execs[id]
		if !ok {
			fakeError(w, http.StatusNotFound, "execution_not_found", "an Execution does not exist")
			return
		}
		if p, ok := f.cancelling[id]; ok {
			if p.reads > 0 {
				p.reads--
			} else {
				delete(f.cancelling, id)
				f.move(e, StateCompleted, "cancellation_completed", p.reason, OutcomeCancelled)
				e.Audit = append(e.Audit, auditDoc{Sequence: int64(len(e.Audit) + 1), Action: "cancellation.requested", ActorID: "operator",
					At: time.Now().UTC().Format(time.RFC3339Nano), Reason: p.reason})
			}
		}
		doc := *e
		if s, ok := f.stateOver[id]; ok {
			doc.State = s
		}
		fakeJSON(w, http.StatusOK, doc)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/cancellation"):
		f.calls["POST cancellation"]++
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/executions/"), "/cancellation")
		if body["actor_id"] != actor {
			fakeError(w, http.StatusForbidden, "principal_actor_mismatch", "request is not authorized")
			return
		}
		e, ok := f.execs[id]
		switch {
		case !ok:
			fakeError(w, http.StatusNotFound, "execution_not_found", "an Execution does not exist")
		case e.State == StateCompleted:
			fakeError(w, http.StatusConflict, "cancellation_state_conflict", "Execution is already terminal")
		default:
			if _, ok := f.cancelling[id]; !ok {
				reason, _ := body["reason"].(string)
				f.cancelling[id] = &pendingCancel{reason: reason, reads: 1}
			}
			fakeJSON(w, http.StatusAccepted, map[string]any{"tenant_id": "pilot", "execution_id": id, "accepted": true})
		}
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/approval"):
		f.calls["POST approval"]++
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/executions/"), "/approval")
		if body["actor_id"] != actor {
			fakeError(w, http.StatusForbidden, "principal_actor_mismatch", "request is not authorized")
			return
		}
		e, ok := f.execs[id]
		if !ok {
			fakeError(w, http.StatusNotFound, "execution_not_found", "an Execution does not exist")
			return
		}
		if e.State != StateWaiting || e.Transitions[len(e.Transitions)-1].Trigger != TriggerApprovalRequired {
			fakeError(w, http.StatusConflict, "approval_state_conflict", "Execution is not waiting for T2 approval")
			return
		}
		decision, _ := body["decision"].(string)
		e.Audit = append(e.Audit, auditDoc{Sequence: int64(len(e.Audit) + 1), Action: "approval." + decision, ActorID: actor,
			At: time.Now().UTC().Format(time.RFC3339Nano), Reason: "human attestation captured"})
		if decision == "approved" {
			f.move(e, StateExecuting, "dispatch_started", "", "")
			f.move(e, StateCompleted, "execution_completed", "", "completed")
		} else {
			f.move(e, StateCompleted, "approval_rejected", "human approval was rejected", "rejected")
		}
		fakeJSON(w, http.StatusAccepted, map[string]any{"tenant_id": "pilot", "execution_id": id, "decision": decision, "accepted": true})
	case route == "POST /v1/human-tasks":
		raw, _ := json.Marshal(body)
		id, _ := body["task_id"].(string)
		if t, ok := f.tasks[id]; ok {
			prev, _ := json.Marshal(map[string]any{"tenant_id": t.TenantID, "task_id": t.TaskID, "schema_version": 1, "execution_id": t.ExecutionID,
				"actor_id": t.ActorID, "due_at": t.DueAt, "payload": t.Payload})
			if string(prev) != string(raw) {
				fakeError(w, http.StatusConflict, "human_task_conflict", "different content")
				return
			}
			fakeJSON(w, http.StatusOK, t)
			return
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		t := &HumanTask{TenantID: "pilot", TaskID: id, SchemaVersion: 1, ExecutionID: body["execution_id"].(string), ActorID: body["actor_id"].(string),
			Status: TaskOpen, DueAt: body["due_at"].(string), Payload: body["payload"].(map[string]any), CreatedAt: now, UpdatedAt: now}
		f.tasks[id] = t
		f.taskEvent(id, "task.created", actor)
		fakeJSON(w, http.StatusCreated, t)
	case route == "GET /v1/human-tasks":
		items := []*HumanTask{}
		for _, t := range f.tasks {
			items = append(items, t)
		}
		fakeJSON(w, http.StatusOK, map[string]any{"items": items})
	case strings.HasPrefix(path, "/v1/human-tasks/"):
		rest := strings.TrimPrefix(path, "/v1/human-tasks/")
		id, verb, _ := strings.Cut(rest, "/")
		t, ok := f.tasks[id]
		if !ok {
			fakeError(w, http.StatusNotFound, "human_task_not_found", "no task")
			return
		}
		switch {
		case r.Method == http.MethodGet && verb == "":
			fakeJSON(w, http.StatusOK, t)
		case r.Method == http.MethodGet && verb == "events":
			fakeJSON(w, http.StatusOK, map[string]any{"task_id": id, "items": f.taskEvents[id]})
		case verb == "claim" && t.Status == TaskOpen:
			t.Status, t.Assignee = TaskClaimed, ptr(body["worker_id"].(string))
			f.taskEvent(id, "task.claimed", actor)
			fakeJSON(w, http.StatusOK, t)
		case (verb == "complete" || verb == "reject") && t.Status == TaskClaimed && t.Assignee != nil && *t.Assignee == body["worker_id"]:
			t.Status, t.Result = map[string]string{"complete": TaskCompleted, "reject": TaskRejected}[verb], body["result"].(map[string]any)
			f.taskEvent(id, "task."+t.Status, actor)
			fakeJSON(w, http.StatusOK, t)
		default:
			fakeError(w, http.StatusConflict, "human_task_conflict", "state")
		}
	default:
		fakeError(w, http.StatusNotFound, "not_found", "no route")
	}
}

func (f *fakeEER) taskEvent(id, typ, actor string) {
	f.taskEvents[id] = append(f.taskEvents[id], HumanTaskEvent{TaskID: id, Sequence: int64(len(f.taskEvents[id]) + 1), EventType: typ, ActorID: actor,
		OccurredAt: time.Now().UTC().Format(time.RFC3339Nano), Detail: map[string]any{}})
}

func (f *fakeEER) move(e *executionDoc, to, trigger, reason, outcome string) {
	from := e.State
	t := transitionDoc{Sequence: int64(len(e.Transitions) + 1), FromState: &from, ToState: to, Trigger: trigger, At: time.Now().UTC().Format(time.RFC3339Nano)}
	if reason != "" {
		t.Reason = &reason
	}
	e.Transitions = append(e.Transitions, t)
	e.State = to
	if outcome != "" {
		e.Outcome = &outcome
	}
	e.UpdatedAt = t.At
}

func (f *fakeEER) accept(w http.ResponseWriter, body map[string]any) {
	raw, _ := json.Marshal(body)
	id, _ := body["execution_id"].(string)
	intent, _ := body["intent"].(map[string]any)
	if !strings.HasPrefix(id, "execution:") || intent == nil {
		fakeError(w, http.StatusBadRequest, "invalid_intent", "execution_id is invalid")
		return
	}
	for _, ev := range intent["originating_events"].([]any) {
		if _, ok := f.events[ev.(string)]; !ok {
			fakeError(w, http.StatusNotFound, "originating_event_not_found", "an originating Event does not exist")
			return
		}
	}
	ack := map[string]any{"tenant_id": "pilot", "execution_id": id, "intent_id": intent["intent_id"], "workflow_id": "wf-" + id,
		"plan_id": body["plan_id"], "request_status": "accepted", "status_url": "/v1/executions/" + id}
	if prev, ok := f.intents[id]; ok {
		if prev != string(raw) {
			fakeError(w, http.StatusConflict, "execution_identity_conflict", "Intent or Execution identity is already bound to different content")
			return
		}
		ack["state"], ack["idempotent_replay"] = f.execs[id].State, true
		fakeJSON(w, http.StatusAccepted, ack)
		return
	}
	f.intents[id] = string(raw)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	e := &executionDoc{TenantID: "pilot", ExecutionID: id, SchemaVersion: 1, IntentID: intent["intent_id"].(string), CorrelationID: intent["correlation_id"].(string),
		State: StateCreated, Transitions: []transitionDoc{{Sequence: 1, ToState: StateCreated, Trigger: "intent_accepted", At: now}}, Audit: []auditDoc{},
		ParticipatingActors: []string{}, UpdatedAt: now}
	f.execs[id] = e
	f.move(e, StatePlanned, "plan_compiled", "", "")
	if intent["constraints"].(map[string]any)["disposition_tier"] == "T2" {
		f.move(e, StateWaiting, TriggerApprovalRequired, "", "")
	} else {
		f.move(e, StateExecuting, "dispatch_started", "", "")
		f.move(e, StateCompleted, "execution_completed", "", "completed")
	}
	ack["state"], ack["idempotent_replay"] = StateCreated, false
	fakeJSON(w, http.StatusAccepted, ack)
}
