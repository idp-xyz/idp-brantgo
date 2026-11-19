package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Failure taxonomy of a trial (baseline 19.1 失败分类).
const (
	failSetup      = "setup"       // the tenant, world or task could not be built
	failAgentPhase = "agent_phase" // an agent phase ended in an unexpected state or its worker failed
	failHumanStep  = "human_step"  // a scripted person's step was refused where the family expects none
	failAssertion  = "assertion"   // the authoritative records differ from the family's expectation
	failTimeout    = "timeout"     // an agent phase exceeded -phase-timeout
	failHarness    = "harness"     // the harness itself failed
)

// IDs of a trial's objects; each trial has a tenant of its own (D1-D09), so they repeat.
//
// The task names what exists when it is created: the uploaded inquiry file (source kind evd_file,
// S4 G2) and, as its target, the RFQ and the quote the agent's drafts will create. What the agent
// creates under an ID of its own choosing — the source it registers from the file, the customer
// response it records, the order it prepares — the scripted operator and the graders learn from the
// task's records (trial.sourceID, response, order), so the live arm works whatever the model names
// them. orderID and responseID are what the scripted model names them, and what a person names them
// who does the agent's part by hand (AN08).
const (
	taskID       = "task.q01"
	rfqID        = "rfq.q01"
	quoteID      = "quote.q01"
	orderID      = "order.q01"
	fileID       = "file.inquiry"
	mailID       = "mail.reply"
	responseID   = "response.q01"
	acceptanceID = "acceptance.q01"
	delegationID = "del.q01"
	costTag      = "q01"
)

// trial is one Q01 task of a family, in a tenant of its own.
type trial struct {
	env      *env
	fam      *family
	n        int
	tenant   string
	dir      string
	agentID  string
	api      *api
	ops      map[string]*actor
	w        *world
	in       inquiry
	claimed  *inquiry // the places the scripted agent claims for the fields, when not the file's own (family feedback)
	rec      *TrialRecord
	ownAPI   *proc // AN06: the trial's own brant-api
	issued   map[string]bool
	budget   *Budget
	approval int // approvals issued so far

	// What the agent created under IDs of its own choosing, as the task's records show it (learn.go):
	// the source registered from the inquiry file, the customer response and the order.
	sourceID     string
	learnedResp  string
	learnedOrder string

	rfqVersion   int64
	snap         costed
	quoteRoot    int64 // the quote root's version after the last step
	revision     int64 // the business revision under review or active
	businessHash string
	approvalID   string
	orderVersion int64
	orderHash    string
	orderReview  string
}

func newTrial(e *env, f *family, n int) *trial {
	id := fmt.Sprintf("%s-%d", f.name, n)
	t := &trial{env: e, fam: f, n: n, agentID: actorAgent, issued: map[string]bool{},
		tenant: "eval." + e.run.RunTag + "." + f.name + "." + strconv.Itoa(n),
		dir:    filepath.Join(e.cfg.out, "trials", id)}
	t.rec = &TrialRecord{Family: f.name, Trial: n, TrialID: id, Design: f.design, Goal: f.goal, Expected: f.expected, TaskID: taskID,
		extra: map[string]float64{}, Authority: map[string]any{}}
	return t
}

// runTrial runs trial n of family f and returns its record, whatever happened.
func runTrial(ctx context.Context, e *env, f *family, n int) (rec *TrialRecord) {
	start := time.Now()
	t := newTrial(e, f, n)
	rec = t.rec
	defer func() {
		if p := recover(); p != nil {
			rec.Failure, rec.Error = failHarness, fmt.Sprintf("panic: %v\n%s", p, debug.Stack())
		}
		_ = t.ownAPI.stop()
		rec.Seconds = round(time.Since(start).Seconds(), 2)
		if raw, err := json.MarshalIndent(rec, "", "  "); err == nil {
			_ = os.WriteFile(filepath.Join(t.dir, "trial.json"), raw, 0o644)
		}
	}()
	if err := os.MkdirAll(t.dir, 0o755); err != nil {
		rec.Failure, rec.Error = failHarness, err.Error()
		return rec
	}
	err := t.setup(ctx)
	if err != nil {
		rec.Failure = failSetup
	} else if err = f.run(ctx, t); err != nil && rec.Failure == "" {
		rec.Failure = classify(err)
	}
	if err != nil {
		rec.Error = err.Error()
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	t.collect(cctx)
	if err == nil {
		f.grade(cctx, t)
	}
	rec.Pass = err == nil && len(rec.Assertions) > 0
	for _, a := range rec.Assertions {
		if !a.Pass {
			rec.Pass = false
			if rec.Failure == "" {
				rec.Failure = failAssertion
			}
		}
	}
	rec.Outcome = outcomeOf(rec)
	return rec
}

// phaseError is an agent phase that did not reach what it waits for.
type phaseError struct {
	timeout bool
	err     error
}

func (p *phaseError) Error() string { return p.err.Error() }

func classify(err error) string {
	var pe *phaseError
	var r *refusal
	switch {
	case errors.As(err, &pe) && pe.timeout:
		return failTimeout
	case errors.As(err, &pe):
		return failAgentPhase
	case errors.As(err, &r):
		return failHumanStep
	}
	return failHarness
}

func outcomeOf(r *TrialRecord) string {
	s := r.Lifecycle
	if s == "" {
		s = "none"
	}
	if r.CompletionActor != "" {
		s += "/" + r.CompletionActor
	}
	if p, ok := r.Authority["pause_cause"].(string); ok && p != "" && r.Lifecycle == "paused" {
		s += "/" + p
	}
	return s
}

// setup provisions the trial's tenant, builds the world, files the customer's inquiry, creates the
// AI task, settles its scope and delegates the agent's capabilities for it (the owner's steps).
func (t *trial) setup(ctx context.Context) error {
	if err := t.env.provision(ctx, t.tenant); err != nil {
		return err
	}
	t.api = t.env.client(t.tenant)
	if t.fam.ownAPI {
		p, base, err := t.env.startAPI(ctx, filepath.Join(t.dir, "brant-api.log"))
		if err != nil {
			return err
		}
		t.ownAPI, t.api.base = p, base
	}
	t.ops = map[string]*actor{}
	for _, a := range []string{actorSales, actorAdmin, actorController, actorCompliance, actorBuyer, actorCostChecker, actorDocumentation,
		actorDocVerifier, actorCreditManager, actorCreditSecond, actorClerk, actorQuoteReviewer, actorOrderReviewer} {
		t.ops[a] = t.api.as(a, kindHuman)
	}
	t.ops[actorAgent] = t.api.as(actorAgent, kindAI)
	t.ops[actorPlatform] = t.api.as(actorPlatform, kindSystem)
	var err error
	if t.w, err = setupWorld(ctx, t.ops); err != nil {
		return fmt.Errorf("world: %w", err)
	}
	rows, extra := brcRows(), []string(nil)
	if t.fam.rows != nil {
		rows = t.fam.rows()
	}
	if t.fam.extra != nil {
		extra = t.fam.extra
	}
	kind := t.fam.input
	if kind == "" {
		kind = []string{kindText, kindXLSX, kindPDF}[(t.n-1)%3]
	}
	if t.in, err = makeInquiry(kind, "inquiry", rows, extra); err != nil {
		return err
	}
	sum := sha256.Sum256(t.in.data)
	t.rec.Input = InputRecord{Kind: kind, File: t.in.name, MIME: t.in.mime, Bytes: len(t.in.data), SHA256: hex.EncodeToString(sum[:])}
	if err = os.WriteFile(filepath.Join(t.dir, t.in.name), t.in.data, 0o644); err != nil {
		return err
	}
	if _, err = t.humanCmd(ctx, actorClerk, "evd", contractEVD, "file.register", true, map[string]any{"id": fileID, "file_name": t.in.name,
		"mime_type": t.in.mime, "content_base64": base64.StdEncoding.EncodeToString(t.in.data)}); err != nil {
		return err
	}
	b := Budget{MaxModelCalls: 40, MaxRetries: 3, MaxCost: "3.00", Currency: "USD"}
	if t.fam.budget != nil {
		b = *t.fam.budget
	}
	t.budget, t.rec.Budget = &b, &b
	bundle := t.env.bundle
	if t.fam.bundleSHA != "" {
		bundle.SHA256 = t.fam.bundleSHA
	}
	// The task starts from the uploaded file (S4 G2, owner decision 2026-11-18): its source is the
	// file itself; the agent registers the extract as its first step, in both arms.
	create := map[string]any{"task_id": taskID, "goal_code": t.fam.goalCode, "control_mode": "ai", "agent_id": t.agentID, "owner_id": actorClerk,
		"target": map[string]any{"rfq_id": rfqID, "quote_id": quoteID, "customer_id": "customer.1"},
		"source": map[string]any{"kind": "evd_file", "id": fileID},
		"budget": map[string]any{"max_model_calls": b.MaxModelCalls, "max_retries": b.MaxRetries,
			"max_cost": map[string]any{"currency": b.Currency, "minor_units": 2, "amount": b.MaxCost}},
		"execution_bundle": map[string]any{"bundle_id": bundle.BundleID, "sha256": bundle.SHA256}}
	r, err := t.humanCmd(ctx, actorClerk, "tsk", contractTSK, "task.create", true, create)
	if err != nil {
		return err
	}
	r, err = t.platformCmd(ctx, "task.scope", map[string]any{"task_id": taskID, "expected_version": r["row_version"]})
	if err != nil {
		return err
	}
	if _, err = t.humanCmd(ctx, actorClerk, "tsk", contractTSK, "task.scope", true, map[string]any{"task_id": taskID,
		"expected_version": r["row_version"], "note": "范围确认：合成询价 " + t.in.name + "（AI 评测）"}); err != nil {
		return err
	}
	caps := clerkCapabilities
	if t.fam.capabilities != nil {
		caps = t.fam.capabilities
	}
	_, err = t.humanCmd(ctx, actorClerk, "ai", contractAI, "ai.delegation.grant", true, map[string]any{"id": delegationID, "agent_id": t.agentID,
		"task_id": taskID, "capabilities": caps, "autonomy": "submit", "expires_at": stamp(time.Now().Add(24 * time.Hour))})
	return err
}

// clerkCapabilities are the Q01 capabilities (decision D-7) the owner, a sales clerk, holds and so
// may delegate: the costing drafts are a costing person's (D1-D08: people cost on their pages).
var clerkCapabilities = []string{"source.register", "file.register", "rfq.save_draft", "quote.save_draft", "quote.revise",
	"quote.metadata.update", "quote.response.record", "order.prepare", "order.review.request",
	"rfq.get", "cost_snapshot.list", "costing_context.get", "quote.price_proposal", "config.lookup", "sku.get", "party.get",
	"quote.get", "quote.response.get", "order.get", "source.get"}

// readDelegation is the reads family's delegation: every Q01 read except order.get, so that call
// is outside the delegation. The bundle still lists it.
var readDelegation = without(clerkCapabilities, "order.get")

func without(in []string, drop string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}

// humanCmd is a person's command on a plain route, recorded as a synthetic human step.
func (t *trial) humanCmd(ctx context.Context, who, bc, contract, command string, required bool, args map[string]any) (map[string]any, error) {
	out, err := t.ops[who].cmd(ctx, bc, contract, command, args)
	t.human(who, command, "plain", required, err)
	return out, err
}

// humanStep is a person's step on the task through the task-step route (D-2): the business intent
// names the action.
func (t *trial) humanStep(ctx context.Context, who, bc, contract, command, intent string, required bool, args map[string]any) (map[string]any, error) {
	out, err := t.ops[who].step(ctx, taskID, bc, contract, command, intent, args)
	t.human(who, command, "step", required, err)
	return out, err
}

func (t *trial) human(who, command, route string, required bool, err error) {
	outcome := "ok"
	if err != nil {
		outcome = codeOf(err)
	}
	t.rec.HumanSteps = append(t.rec.HumanSteps, HumanStep{Actor: who, Command: command, Route: route, Required: required, Synthetic: true, Outcome: outcome})
}

// platformCmd is a TSK system transition the evaluation's platform actor takes (D1-D06).
func (t *trial) platformCmd(ctx context.Context, command string, args map[string]any) (map[string]any, error) {
	return t.ops[actorPlatform].cmd(ctx, "tsk", contractTSK, command, args)
}

func (t *trial) assert(name, want, got string) {
	t.rec.Assertions = append(t.rec.Assertions, Assertion{Name: name, Want: want, Got: got, Pass: want == got})
}

func (t *trial) note(format string, a ...any) {
	t.rec.Notes = append(t.rec.Notes, fmt.Sprintf(format, a...))
}

// task reads the task as its owner.
func (t *trial) task(ctx context.Context) (map[string]any, error) {
	return t.ops[actorClerk].get(ctx, "/api/brant/v1/tsk/tasks/"+taskID)
}

type callCount struct{ model, tool int }

func (t *trial) modelCalls(ctx context.Context) (callCount, error) {
	out, err := t.ops[actorClerk].get(ctx, "/api/brant/v1/ai/tasks/"+taskID+"/model_calls")
	var c callCount
	for _, x := range list(out["calls"]) {
		if dig(x, "kind") == "model" {
			c.model++
		} else {
			c.tool++
		}
	}
	return c, err
}

// resume resumes the waiting task on what arrived where no consumer tells TSK (D1-D06).
func (t *trial) resume(ctx context.Context, kind, id string, version int64, reason string) error {
	v, err := t.task(ctx)
	if err != nil {
		return err
	}
	_, err = t.platformCmd(ctx, "task.resume", map[string]any{"task_id": taskID, "expected_version": v["task_version"], "reason": reason,
		"arrival": map[string]any{"kind": kind, "id": id, "version": version}})
	return err
}

// stepVersion is the row version a step's receipt reports.
func stepVersion(out map[string]any) int64 {
	if v := num(dig(out, "result", "row_version")); v > 0 {
		return v
	}
	return num(dig(out, "stored", "row_version"))
}

// approveWaits issues the approval grant a task waiting for one names (AI-INV-002, D-7): the owner
// approves exactly the command, request hash and object the wait's items name. It acts once per
// proposal.
func (t *trial) approveWaits(ctx context.Context, v map[string]any) (bool, error) {
	if str(v["lifecycle"]) != "waiting_approval" || str(dig(v, "wait", "awaiting", "kind")) != "ai_approval_grant" {
		return false, nil
	}
	proposal := str(dig(v, "wait", "awaiting", "id")) + "@" + fmt.Sprint(num(v["task_version"]))
	if t.issued[proposal] {
		return false, nil
	}
	var command, hash, object string
	var version int64
	for _, it := range list(dig(v, "wait", "items")) {
		s := str(it)
		switch {
		case strings.HasPrefix(s, "command "):
			command = strings.TrimPrefix(s, "command ")
		case strings.HasPrefix(s, "request_hash "):
			hash = strings.TrimPrefix(s, "request_hash ")
		case strings.HasPrefix(s, "object "):
			f := strings.Fields(s) // object <type> <id> version <v>
			if len(f) == 5 {
				object = f[2]
				version, _ = strconv.ParseInt(f[4], 10, 64)
			}
		}
	}
	if command == "" || hash == "" || object == "" {
		return false, fmt.Errorf("the approval wait names no call: %v", dig(v, "wait"))
	}
	if t.fam.approve != nil && !t.fam.approve(t, command) {
		return false, nil
	}
	if err := t.readProposal(ctx, str(dig(v, "wait", "awaiting", "id")), command, hash); err != nil {
		return false, err
	}
	t.approval++
	_, err := t.humanCmd(ctx, actorClerk, "ai", contractAI, "ai.approval_grant.issue", true, map[string]any{"id": fmt.Sprintf("grant.%d", t.approval),
		"delegation_id": delegationID, "command": command, "request_hash": hash, "object_id": object, "object_version": version,
		"expires_at": stamp(time.Now().Add(2 * time.Hour))})
	if err != nil {
		return false, err
	}
	t.issued[proposal] = true
	return true, nil
}

// readProposal is the approver reading what the grant will approve (S4 G7b): the proposal the wait
// names is readable over HTTP by the owner, is still pending, and is the command and request hash the
// wait's items carry, with the arguments the agent proposed.
func (t *trial) readProposal(ctx context.Context, id, command, hash string) error {
	p, err := t.ops[actorClerk].get(ctx, "/api/brant/v1/ai/proposals/"+id)
	if err != nil {
		return fmt.Errorf("the approver cannot read the proposal %s: %w", id, err)
	}
	if str(p["proposal_id"]) != id || str(p["command"]) != command || str(p["request_hash"]) != hash || str(p["status"]) != "pending" ||
		str(dig(p, "arguments", "OrderID")) == "" {
		return fmt.Errorf("the proposal %s is not the call the wait names (%s, %s): %v", id, command, hash, p)
	}
	n, _ := t.rec.Authority["proposals_read"].(int)
	t.rec.Authority["proposals_read"] = n + 1
	t.note("the approver read the proposal %s before the grant: %s on %s", id, command, str(dig(p, "arguments", "OrderID")))
	return nil
}

// collect reads the trial's authoritative records back: the task, its actions and verifications,
// the model-call ledger, the RFQ's corrections, the quote and the order.
func (t *trial) collect(ctx context.Context) {
	if t.ops == nil {
		return
	}
	clerk := t.ops[actorClerk]
	if v, err := t.task(ctx); err == nil {
		t.rec.Lifecycle, t.rec.CompletionActor = str(v["lifecycle"]), str(v["completion_actor"])
		t.rec.GoalCompleted = t.rec.Lifecycle == "completed"
		t.rec.Authority["task_version"] = num(v["task_version"])
		t.rec.Authority["task_source"] = v["source"] // the uploaded file the task names (kind evd_file)
		t.rec.Authority["control_mode"] = str(v["control_mode"])
		t.rec.Authority["control_epoch"] = num(v["control_epoch"])
		t.rec.Authority["binding"] = v["binding"]
		t.rec.Authority["pause_cause"] = str(dig(v, "pause", "cause"))
		t.rec.Authority["pause_reason"] = str(dig(v, "pause", "reason"))
		t.rec.Authority["wait"] = v["wait"]
		var history []string
		for _, h := range list(v["history"]) {
			history = append(history, fmt.Sprintf("%s %s→%s %s:%s", str(dig(h, "command")), str(dig(h, "from")), str(dig(h, "to")),
				str(dig(h, "actor", "kind")), str(dig(h, "actor", "id"))))
		}
		t.rec.Authority["history"] = history
		t.save("task.json", v)
	}
	if a, err := clerk.get(ctx, "/api/brant/v1/tsk/tasks/"+taskID+"/actions"); err == nil {
		var actions []string
		open := 0
		for _, x := range list(a["actions"]) {
			s := str(dig(x, "state"))
			if s == "submitted" || s == "unknown" {
				open++
			}
			actions = append(actions, fmt.Sprintf("%s/%s %s %s attempts=%d by %s", str(dig(x, "target", "context")), str(dig(x, "target", "command")),
				s, str(dig(x, "result", "object_id")), num(dig(x, "attempts")), str(dig(x, "registered_by", "id"))))
		}
		t.rec.Authority["actions"] = actions
		t.rec.Authority["open_actions"] = open
		t.save("actions.json", a)
	}
	if vs, err := clerk.get(ctx, "/api/brant/v1/tsk/tasks/"+taskID+"/verifications"); err == nil {
		for _, x := range list(vs["verifications"]) {
			var unmet []string
			for _, u := range list(dig(x, "unmet")) {
				unmet = append(unmet, str(u))
			}
			m := map[string]any{"outcome": dig(x, "outcome"), "lifecycle": dig(x, "lifecycle"), "unmet": unmet, "code": dig(x, "code")}
			var codes []string
			for _, as := range list(dig(x, "assertions")) {
				if str(dig(as, "answer")) != "satisfied" {
					codes = append(codes, str(dig(as, "assertion"))+" "+str(dig(as, "answer"))+" "+str(dig(as, "code")))
				}
			}
			m["unmet_codes"] = codes
			t.rec.Verifications = append(t.rec.Verifications, m)
		}
		t.save("verifications.json", vs)
	}
	if mc, err := clerk.get(ctx, "/api/brant/v1/ai/tasks/"+taskID+"/model_calls"); err == nil {
		c := CostRecord{Currency: "USD"}
		var cost int64
		for _, x := range list(mc["calls"]) {
			switch str(dig(x, "kind")) {
			case "model":
				c.ModelCalls++
				c.InputTokens += num(dig(x, "input_tokens"))
				c.OutputTokens += num(dig(x, "output_tokens"))
				if p := str(dig(x, "profile_id")); p != "" && !slices.Contains(c.Profiles, p) {
					c.Profiles = append(c.Profiles, p)
				}
				if m := str(dig(x, "model_returned")); m != "" && !slices.Contains(c.Returned, m) {
					c.Returned = append(c.Returned, m)
				}
			default:
				c.ToolCalls++
			}
			if dig(x, "retry") == true {
				c.Retries++
			}
			switch str(dig(x, "state")) {
			case "failed":
				c.Failed++
			case "unavailable":
				c.Unavailable++
			}
			if cur := str(dig(x, "cost", "currency")); cur != "" {
				c.Currency = cur
			}
			cost += minorOf(str(dig(x, "cost", "amount")))
		}
		c.Cost = formatMinor(cost)
		t.rec.Cost = c
		t.save("model_calls.json", mc)
	}
	if g, err := clerk.get(ctx, "/api/brant/v1/ai/delegations/"+delegationID+"/approval_grants"); err == nil {
		var grants []string
		for _, x := range list(g["approval_grants"]) {
			grants = append(grants, fmt.Sprintf("%s %s %s@%d %s", str(dig(x, "grant_id")), str(dig(x, "command")), str(dig(x, "object_id")),
				num(dig(x, "object_version")), str(dig(x, "status"))))
		}
		t.rec.Authority["approval_grants"] = grants
	}
	if d, err := clerk.get(ctx, "/api/brant/v1/ai/delegations/"+delegationID); err == nil {
		t.rec.Authority["delegation_status"] = str(d["status"])
	}
	if r, err := clerk.get(ctx, "/api/brant/v1/inq/rfqs/"+rfqID); err == nil {
		for _, x := range list(r["corrections"]) {
			m, _ := x.(map[string]any)
			m["synthetic"] = true
			t.rec.Corrections = append(t.rec.Corrections, m)
		}
		t.rec.Authority["rfq_status"] = str(r["status"])
		var proposers []string
		for _, l := range list(r["lines"]) {
			proposers = append(proposers, str(dig(l, "line_id"))+":"+str(dig(l, "proposed_by", "kind"))+":"+str(dig(l, "proposed_by", "id")))
		}
		t.rec.Authority["rfq_lines"] = proposers
		t.save("rfq.json", r)
	}
	if q, err := clerk.get(ctx, "/api/brant/v1/trd/quotes/"+quoteID); err == nil {
		t.rec.Authority["quote_control"] = q["control"]
		t.save("quote.json", q)
	}
	if o, err := clerk.get(ctx, "/api/brant/v1/trd/orders/"+t.order()); err == nil {
		t.rec.Authority["order_status"] = str(o["status"])
		t.rec.Authority["order_prepared_by"] = str(dig(o, "prepared_by", "kind")) + ":" + str(dig(o, "prepared_by", "id"))
		t.save("order.json", o)
	} else {
		t.rec.Authority["order_status"] = "absent (" + codeOf(err) + ")"
	}
	t.rec.Automation = automationOf(t.rec)
}

// automationOf is the automation level of baseline 19.1 (自动化水平): only the required reviews,
// extra revisions by people, or a take-over. P0 never completes a Q01 task without people.
func automationOf(r *TrialRecord) string {
	for _, h := range r.HumanSteps {
		if h.Command == "task.take_control" && h.Outcome == "ok" {
			return "human_takeover"
		}
	}
	for _, c := range r.Corrections {
		if k := str(c["kind"]); k != "verified" {
			return "extra_human_work"
		}
	}
	// A field of the agent's draft that a person had to settle — a place named for an ambiguous
	// field is recorded by INQ as verified — is extra human work too (S4 G3).
	if len(r.OwnerResolutions) > 0 {
		return "extra_human_work"
	}
	for _, h := range r.HumanSteps {
		if !h.Required && h.Outcome == "ok" {
			return "extra_human_work"
		}
	}
	return "required_review_only"
}

func (t *trial) save(name string, v any) {
	if raw, err := json.MarshalIndent(v, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(t.dir, name), raw, 0o644)
	}
}
