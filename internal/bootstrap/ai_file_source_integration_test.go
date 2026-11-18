//go:build integration

package bootstrap_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"go.idp.xyz/idp-brant/internal/ai"
	"go.idp.xyz/idp-brant/internal/ai/adapters/model"
	"go.idp.xyz/idp-brant/internal/ai/execution"
	"go.idp.xyz/idp-brant/internal/ai/policy"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	tskapi "go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// A task that starts from an uploaded file (S4 G2; owner decision 2026-11-18; WBS S4-01, S4-04,
// S4-07): the task's source names the EVD business file (kind evd_file), the agent registers the
// file's extract itself (source.register, file_extract), and the runner's facts let the model see
// the inquiry: first the file, then — once the task has a committed source.register of that file —
// the source it created and its bounded text, exactly as for an evd_source task.

// g2Inquiry is a synthetic inquiry text (decision E-6). Its third line places sku.1 at 3:6, the
// quantity at 3:16 and the unit at 3:25 (text locators line:col, baseline 14.2). pad appends that
// many filler lines, so the text can exceed what the facts carry.
func g2Inquiry(tag string, pad int) string {
	var b strings.Builder
	b.WriteString("SYNTHETIC INQUIRY " + tag + " - not a real customer\nFrom: customer.1 (contact.1)\nCode sku.1 Qty 120 Unit PCS\n")
	for i := range pad {
		fmt.Fprintf(&b, "filler line %03d of the synthetic inquiry, nothing to quote here\n", i+1)
	}
	return b.String()
}

// factsDoc is what a test reads of a turn's facts (the user message the runner sends the model).
type factsDoc struct {
	TaskID string `json:"task_id"`
	Source *struct {
		Kind string
		ID   string
	} `json:"source"`
	SourceFileID       string `json:"source_file_id"`
	RegisteredSourceID string `json:"registered_source_id"`
	SourceText         string `json:"source_text"`
	SourceTruncated    bool   `json:"source_truncated"`
	SourceSHA256       string `json:"source_sha256"`
	Actions            []struct {
		Command  string `json:"command"`
		State    string `json:"state"`
		ObjectID string `json:"object_id"`
	} `json:"actions"`
}

// factsOf decodes the facts of a model request: the second message, after the system prompt.
func factsOf(req model.Request) (factsDoc, error) {
	var f factsDoc
	if len(req.Messages) < 2 || req.Messages[0].Role != model.RoleSystem || req.Messages[0].Content != execution.Q01Prompt ||
		req.Messages[1].Role != model.RoleUser {
		return f, fmt.Errorf("the request does not open with the Q01 prompt and the facts: %d messages", len(req.Messages))
	}
	return f, json.Unmarshal([]byte(req.Messages[1].Content), &f)
}

func (f factsDoc) committed(command, object string) bool {
	for _, a := range f.Actions {
		if a.Command == command && a.State == tskapi.ActionCommitted && a.ObjectID == object {
			return true
		}
	}
	return false
}

// sourcedTask creates an AI task of the agent that serves source for quote_ready, settles its scope
// and delegates caps to the agent as the clerk.
func (w *agentWorld) sourcedTask(id string, target tskapi.Target, source tskapi.SourceRef, bundle *tskapi.BundleRef, caps ...string) {
	w.t.Helper()
	r, err := w.c.TSK.CreateTask(w.clerk, tskapi.CreateTask{IdempotencyKey: w.key(), TaskID: id, GoalCode: tskapi.GoalQuoteReady,
		ControlMode: tskapi.ModeAI, AgentID: agentID, Target: target, Source: &source, ExecutionBundle: bundle})
	w.must(err)
	r, err = w.c.TSK.ScopeTask(w.sys, tskapi.ScopeTask{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: r.RowVersion})
	w.must(err)
	_, err = w.c.TSK.ScopeTask(w.clerk, tskapi.ScopeTask{IdempotencyKey: w.key(), TaskID: id, ExpectedVersion: r.RowVersion, Note: "范围确认"})
	w.must(err)
	_, err = w.c.AI.GrantDelegation(w.clerk, policy.GrantDelegation{IdempotencyKey: w.key(), DelegationID: "del." + id, AgentID: agentID, TaskID: id,
		Capabilities: caps, Autonomy: "draft", ExpiresAt: time.Now().Add(24 * time.Hour)})
	w.must(err)
}

func registerExtractCall(id, source, file string) model.ToolCall {
	return tc(id, evdapi.CommandRegisterSource, map[string]any{"SourceID": source, "SourceKind": evdapi.KindFileExtract, "FileID": file})
}

// rfqDraftCall is the agent's rfq.save_draft of the inquiry's one line, each field placed in the
// extract of source.
func rfqDraftCall(id, rfq, source string) model.ToolCall {
	return tc(id, inqapi.CommandSaveRfq, map[string]any{"RfqID": rfq, "CustomerID": "customer.1", "ContactID": "contact.1",
		"SourceIDs": []string{source}, "Lines": []map[string]any{{"LineID": "L1", "SkuID": "sku.1", "Quantity": "120", "UOM": "PCS",
			"SourceID": source, "Locator": "line 3", "Locators": []map[string]string{at("sku_id", "3:6", "sku.1"), at("quantity", "3:16", "120"),
				at("uom", "3:25", "PCS")}}}})
}

// The composed runner with the scripted model on PostgreSQL 16.
//
// Turn by turn (one model call per turn): the first turn's facts name the file and carry no text —
// the source does not exist yet; the agent registers the extract under a source ID of its choosing;
// the next turn's facts name that source and carry its text, cut at 2000 characters with the
// content's SHA-256, as for an evd_source; the agent drafts the RFQ from it, its field locators
// verified against the extract (BC-AT27); its claim that it is done only starts verification.
//
// Within one turn: a source the agent registers from another file is not the task's (EVD says which
// file a source was read from, not the model), and the conversation goes on; once the task's file is
// registered the runner rebuilds the facts and the model's next call starts from them — it sees the
// inquiry in the call after it registered it.
//
// A task whose source is an EVD source gets that source's text as before, and no file.
func TestAgentRunnerStartsFromAnUploadedFile(t *testing.T) {
	w := newAgentWorld(t, false)
	long, short := g2Inquiry("G2-A", 40), g2Inquiry("G2-B", 0)
	if utf8.RuneCountInString(long) <= 2000 {
		t.Fatal("the long inquiry fits the facts")
	}
	w.rawFile(w.clerk, "file.g2a", "inquiry-a.txt", evdapi.MimeText, []byte(long))
	w.rawFile(w.clerk, "file.g2b", "inquiry-b.txt", evdapi.MimeText, []byte(short))
	w.rawFile(w.clerk, "file.other", "other.txt", evdapi.MimeText, []byte("SYNTHETIC other file, not the task's\n"))
	caps := []string{evdapi.CommandRegisterSource, inqapi.CommandSaveRfq}

	// Turn by turn.
	m := model.NewScripted("scripted-q01-v1",
		model.Step{Reply: model.Message{ToolCalls: []model.ToolCall{registerExtractCall("c1", "src.agent.a", "file.g2a")}},
			Usage: model.Usage{InputTokens: 900, OutputTokens: 60}, Check: func(req model.Request) error {
				f, err := factsOf(req)
				if err != nil {
					return err
				}
				if f.Source == nil || f.Source.Kind != tskapi.SourceFile || f.Source.ID != "file.g2a" || f.SourceFileID != "file.g2a" ||
					f.RegisteredSourceID != "" || f.SourceText != "" || len(f.Actions) != 0 {
					return fmt.Errorf("the first facts of a file task: %s", req.Messages[1].Content)
				}
				return nil
			}},
		model.Step{Reply: model.Message{ToolCalls: []model.ToolCall{rfqDraftCall("c2", "rfq.g2a", "src.agent.a")}},
			Usage: model.Usage{InputTokens: 1400, OutputTokens: 200}, Check: func(req model.Request) error {
				f, err := factsOf(req)
				if err != nil {
					return err
				}
				src, err := w.c.EVD.GetSource(w.clerk, "src.agent.a")
				if err != nil {
					return err
				}
				if len(req.Messages) != 2 || f.SourceFileID != "file.g2a" || f.RegisteredSourceID != "src.agent.a" ||
					!f.committed(evdapi.CommandRegisterSource, "src.agent.a") {
					return fmt.Errorf("the facts after the registration: %s", req.Messages[1].Content)
				}
				if !f.SourceTruncated || f.SourceSHA256 != src.ContentSHA256 || utf8.RuneCountInString(f.SourceText) != 2000 ||
					!strings.HasPrefix(src.ContentText, f.SourceText) || !strings.Contains(f.SourceText, "Code sku.1 Qty 120 Unit PCS") {
					return fmt.Errorf("the bounded text: truncated %v, sha %s, %d characters", f.SourceTruncated, f.SourceSHA256,
						utf8.RuneCountInString(f.SourceText))
				}
				return nil
			}},
		model.Step{Reply: model.Message{Content: "询价已登记并起草，等待负责人核实。"}, Usage: model.Usage{InputTokens: 1500, OutputTokens: 20},
			Check: func(req model.Request) error {
				f, err := factsOf(req)
				if err != nil {
					return err
				}
				if !f.committed(inqapi.CommandSaveRfq, "rfq.g2a") || f.RegisteredSourceID != "src.agent.a" {
					return fmt.Errorf("the facts after the draft: %s", req.Messages[1].Content)
				}
				return nil
			}})
	r := w.runner(m)
	r.MaxSteps = 1
	w.sourcedTask("task.file.a", tskapi.Target{RfqID: "rfq.g2a", CustomerID: "customer.1"}, tskapi.SourceRef{Kind: tskapi.SourceFile, ID: "file.g2a"},
		bundleRef(r), caps...)
	if res := w.turnOn(r, "task.file.a"); res.ModelCalls != 1 || len(res.Receipts) != 1 || res.Receipts[0].Stage != execution.Verified ||
		res.Receipts[0].Stored.ObjectID != "src.agent.a" || res.Lifecycle != tskapi.StateRunning {
		t.Fatalf("the registering turn %+v", res)
	}
	if res := w.turnOn(r, "task.file.a"); res.ModelCalls != 1 || len(res.Receipts) != 1 || res.Receipts[0].Stored.ObjectID != "rfq.g2a" ||
		len(res.Refusals) != 0 || res.Lifecycle != tskapi.StateRunning {
		t.Fatalf("the drafting turn %+v", res)
	}
	if res := w.turnOn(r, "task.file.a"); !res.Verified || res.Lifecycle != tskapi.StateWaitingInput || m.Remaining() != 0 {
		t.Fatalf("the last turn %+v, %d steps left", res, m.Remaining())
	}
	src, err := w.c.EVD.GetSource(w.clerk, "src.agent.a")
	w.must(err)
	if src.SourceKind != evdapi.KindFileExtract || src.Extract == nil || src.Extract.FileID != "file.g2a" || src.Imports[0].Actor.ID != agentID {
		t.Fatalf("the registered source %+v", src)
	}
	rfq, err := w.c.INQ.GetRfq(w.clerk, "rfq.g2a")
	w.must(err)
	wantLocators := []inqapi.FieldLocator{{Field: "sku_id", Locator: "3:6", Quote: "sku.1"}, {Field: "quantity", Locator: "3:16", Quote: "120"},
		{Field: "uom", Locator: "3:25", Quote: "PCS"}}
	if len(rfq.Lines) != 1 || rfq.Lines[0].ProposedBy.ID != agentID || !slices.Equal(rfq.Lines[0].Locators, wantLocators) ||
		len(rfq.Lines[0].Pending) != 0 || !slices.Equal(rfq.SourceIDs, []string{"src.agent.a"}) {
		t.Fatalf("the RFQ draft %+v", rfq)
	}
	// The task's own records name the source the agent registered: the committed action's object.
	actions, err := w.c.TSK.TaskActions(w.clerk, "task.file.a")
	w.must(err)
	if len(actions) != 2 || actions[0].Target.Command != evdapi.CommandRegisterSource || actions[0].State != tskapi.ActionCommitted ||
		actions[0].Result.ObjectID != "src.agent.a" || actions[0].RegisteredBy.ID != agentID || actions[1].Result.ObjectID != "rfq.g2a" {
		t.Fatalf("actions %+v", actions)
	}
	if v := w.taskView("task.file.a"); v.Source == nil || *v.Source != (tskapi.SourceRef{Kind: tskapi.SourceFile, ID: "file.g2a"}) {
		t.Fatalf("the task's source %+v", v.Source)
	}

	// Within one turn, after a source of another file.
	whole := strings.TrimSuffix(short, "\n")
	m = model.NewScripted("scripted-q01-v1",
		calls(registerExtractCall("c1", "src.agent.other", "file.other")),
		model.Step{Reply: model.Message{ToolCalls: []model.ToolCall{registerExtractCall("c2", "src.agent.b", "file.g2b")}},
			Usage: model.Usage{InputTokens: 1000, OutputTokens: 60}, Check: func(req model.Request) error {
				f, err := factsOf(req)
				if err != nil {
					return err
				}
				if len(req.Messages) != 4 || req.Messages[3].Role != model.RoleTool || f.RegisteredSourceID != "" || f.SourceText != "" {
					return fmt.Errorf("another file's source became the task's: %d messages, %s", len(req.Messages), req.Messages[1].Content)
				}
				return nil
			}},
		model.Step{Reply: model.Message{ToolCalls: []model.ToolCall{rfqDraftCall("c3", "rfq.g2b", "src.agent.b")}},
			Usage: model.Usage{InputTokens: 1200, OutputTokens: 200}, Check: func(req model.Request) error {
				f, err := factsOf(req)
				if err != nil {
					return err
				}
				if len(req.Messages) != 2 || f.RegisteredSourceID != "src.agent.b" || f.SourceText != whole || f.SourceTruncated ||
					f.SourceSHA256 != "" || !f.committed(evdapi.CommandRegisterSource, "src.agent.b") {
					return fmt.Errorf("the call after the registration does not start from the rebuilt facts: %d messages, %s",
						len(req.Messages), req.Messages[1].Content)
				}
				return nil
			}},
		model.Step{Reply: model.Message{Content: "已起草。"}, Usage: model.Usage{InputTokens: 1300, OutputTokens: 10}, Check: func(req model.Request) error {
			if len(req.Messages) != 4 {
				return fmt.Errorf("the facts were rebuilt again: %d messages", len(req.Messages))
			}
			return nil
		}})
	r = w.runner(m)
	w.sourcedTask("task.file.b", tskapi.Target{RfqID: "rfq.g2b", CustomerID: "customer.1"}, tskapi.SourceRef{Kind: tskapi.SourceFile, ID: "file.g2b"},
		bundleRef(r), caps...)
	res := w.turnOn(r, "task.file.b")
	if res.ModelCalls != 4 || res.ToolCalls != 3 || len(res.Receipts) != 3 || len(res.Refusals) != 0 || !res.Verified ||
		res.Lifecycle != tskapi.StateWaitingInput || m.Remaining() != 0 {
		t.Fatalf("the turn %+v, %d steps left", res, m.Remaining())
	}
	if rfq, err = w.c.INQ.GetRfq(w.clerk, "rfq.g2b"); err != nil || !slices.Equal(rfq.Lines[0].Locators, wantLocators) {
		t.Fatalf("the RFQ draft %+v %v", rfq, err)
	}

	// An EVD source as before.
	w.source(w.clerk, "src.c", "合成询价：120 件 sku.1，请报价。")
	m = model.NewScripted("scripted-q01-v1", model.Step{Reply: model.Message{Content: "没有可做的事。"}, Usage: model.Usage{InputTokens: 700, OutputTokens: 10},
		Check: func(req model.Request) error {
			f, err := factsOf(req)
			if err != nil {
				return err
			}
			if f.Source == nil || f.Source.Kind != tskapi.SourceEVD || f.Source.ID != "src.c" || f.SourceFileID != "" || f.RegisteredSourceID != "" ||
				f.SourceText != "合成询价：120 件 sku.1，请报价。" || f.SourceTruncated {
				return fmt.Errorf("the facts of an evd_source task: %s", req.Messages[1].Content)
			}
			return nil
		}})
	r = w.runner(m)
	w.sourcedTask("task.src.c", tskapi.Target{CustomerID: "customer.1"}, tskapi.SourceRef{Kind: tskapi.SourceEVD, ID: "src.c"}, bundleRef(r), caps...)
	if res = w.turnOn(r, "task.src.c"); res.ModelCalls != 1 || !res.Verified || m.Remaining() != 0 {
		t.Fatalf("the evd_source turn %+v", res)
	}
}

// The task over HTTP (S4 G2): the clerk uploads the inquiry (file.register) and creates the task
// naming the file; the read shows the source; a second task for the goal and the file is
// BUSINESS_KEY_CONFLICT (来源防重), a kind the contract does not know SCHEMA_INVALID. TSK does for a
// file what it does for an EVD source at task.create — it records the reference and reads nothing —
// so a task naming a file that does not exist is created too (decision G2-D02; the agent's
// registration is where EVD refuses). After the agent's turn the task's actions, read over HTTP, name
// the source it registered, and EVD shows that source as an extract of the task's file: what a
// reviewer or the evaluation's operator learns the source from.
func TestTaskFromAnUploadedFileOverHTTP(t *testing.T) {
	w := newHTTPTaskWorld(t)
	content := g2Inquiry("G2-HTTP", 0)
	w.clerk.command("evd", evdapi.Contract, evdapi.CommandRegisterFile, map[string]any{"id": "file.http", "file_name": "inquiry.txt",
		"mime_type": evdapi.MimeText, "content_base64": base64.StdEncoding.EncodeToString([]byte(content))})
	m := model.NewScripted("scripted-q01-v1", calls(registerExtractCall("c1", "src.http.agent", "file.http")), done("询价文件已登记。"))
	r := w.runner(m)
	ref := r.Ref()
	create := func(task, kind, file string) map[string]any {
		return map[string]any{"task_id": task, "goal_code": tskapi.GoalQuoteReady, "control_mode": tskapi.ModeAI, "agent_id": agentID,
			"target": map[string]any{"rfq_id": "rfq.http", "customer_id": "customer.1"}, "source": map[string]any{"kind": kind, "id": file},
			"execution_bundle": map[string]any{"bundle_id": ref.BundleID, "sha256": ref.SHA256}}
	}
	v := w.task(w.clerk, tskapi.CommandCreate, create("task.file.http", tskapi.SourceFile, "file.http"))
	tv := w.get(w.clerk, "/api/brant/v1/tsk/tasks/task.file.http")
	if src, _ := tv["source"].(map[string]any); len(src) != 2 || src["kind"] != "evd_file" || src["id"] != "file.http" {
		t.Fatalf("the task's source %v", tv["source"])
	}
	status, out := w.send(w.clerk, "tsk", tskapi.Contract, tskapi.CommandCreate, create("task.file.again", tskapi.SourceFile, "file.http"))
	refused(t, "a second task for the goal and the file", status, out, tskapi.CodeBusinessKeyConflict)
	if status != http.StatusConflict {
		t.Fatalf("来源防重 answers %d", status)
	}
	status, out = w.send(w.clerk, "tsk", tskapi.Contract, tskapi.CommandCreate, create("task.file.kind", "file", "file.http"))
	refused(t, "an unknown source kind", status, out, kernel.CodeSchemaInvalid)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("an unknown source kind answers %d", status)
	}
	w.task(w.clerk, tskapi.CommandCreate, create("task.file.absent", tskapi.SourceFile, "file.absent"))

	v = w.task(w.sys, tskapi.CommandScope, map[string]any{"task_id": "task.file.http", "expected_version": v})
	w.task(w.clerk, tskapi.CommandScope, map[string]any{"task_id": "task.file.http", "expected_version": v, "note": "范围确认"})
	status, out = w.send(w.clerk, "ai", ai.Contract, "ai.delegation.grant", map[string]any{"id": "del.file.http", "agent_id": agentID,
		"task_id": "task.file.http", "capabilities": []string{evdapi.CommandRegisterSource}, "autonomy": "draft",
		"expires_at": kernel.FormatTimestamp(time.Now().Add(24 * time.Hour))})
	if status != http.StatusOK {
		t.Fatalf("delegation: %d %v", status, out)
	}
	if res := w.turnOn(r, "task.file.http"); res.ModelCalls != 2 || len(res.Receipts) != 1 || !res.Verified || m.Remaining() != 0 {
		t.Fatalf("the turn %+v", res)
	}
	actions, _ := w.get(w.clerk, "/api/brant/v1/tsk/tasks/task.file.http/actions")["actions"].([]any)
	if len(actions) != 1 {
		t.Fatalf("actions %v", actions)
	}
	a := actions[0].(map[string]any)
	target, _ := a["target"].(map[string]any)
	result, _ := a["result"].(map[string]any)
	if target["context"] != "evd" || target["command"] != evdapi.CommandRegisterSource || a["state"] != tskapi.ActionCommitted ||
		result["object_id"] != "src.http.agent" {
		t.Fatalf("the registration in the task's records %v", a)
	}
	src := w.get(w.clerk, "/api/brant/v1/evd/sources/src.http.agent")
	if x, _ := src["extract"].(map[string]any); src["source_kind"] != evdapi.KindFileExtract || x["file_id"] != "file.http" {
		t.Fatalf("the registered source %v", src)
	}
}

// traceRecords reads a trace file: one JSON record per line.
func traceRecords(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		var rec map[string]any
		if err = json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("%s: %v in %q", path, err, line)
		}
		out = append(out, rec)
	}
	return out
}

// filesUnder lists the regular files under dir.
func filesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			out = append(out, p)
		}
		return err
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return out
}

// The agent's trace (S4 G2): brant-worker -agent writes it only when asked, never under CI=true, and
// never with a credential in it.
//
// Without -agent-trace-dir a worker takes its turn and writes no trace. Under CI=true the flag is
// refused and nothing is created. With the flag, against an OpenAI-compatible service that demands
// the key of the environment, the worker writes one JSON Lines file for the turn under
// <dir>/<tenant>/<company>/<task>/ — the turn (the prompt, the tools' names), the facts, each model
// reply with its tool calls and arguments, each tool result as it went back to the model, the rebuilt
// facts after the registration, and the end — and one file describing the tools on offer
// (<dir>/tools-<hash>.json). The key reached the service in the Authorization header of
// every request and in no request body; it is in no trace file and in no log — also when the
// service's own answer repeats it, which the trace writes replaced.
func TestWorkerAgentTraceIsOptInAndHoldsNoCredential(t *testing.T) {
	w := newAgentWorld(t, false)
	const key = "g2-live-key-0123456789-never-written"
	const modelName = "fake-live-1"
	dir := t.TempDir()
	bin := filepath.Join(dir, "brant-worker")
	if out, err := exec.Command("go", "build", "-o", bin, "go.idp.xyz/idp-brant/cmd/brant-worker").CombinedOutput(); err != nil {
		t.Fatalf("build brant-worker: %v\n%s", err, out)
	}
	scope := w.scope("company.A")
	where := string(scope.Tenant) + "/" + string(scope.Company)
	// The worker's environment without CI (the suite itself runs under CI=true).
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "CI=") && !strings.HasPrefix(e, "IDP_BRANT_MODEL_API_KEY=") {
			env = append(env, e)
		}
	}
	run := func(task string, env []string, args ...string) string {
		t.Helper()
		var logs bytes.Buffer
		worker := exec.Command(bin, append([]string{"-agent", "-agent-scope", where, "-agent-id", agentID, "-agent-poll", "200ms"}, args...)...)
		worker.Env = env
		worker.Stdout, worker.Stderr = &logs, &logs
		w.must(worker.Start())
		deadline := time.Now().Add(60 * time.Second)
		for w.taskView(task).Lifecycle != tskapi.StateWaitingInput {
			if time.Now().After(deadline) {
				_ = worker.Process.Kill()
				t.Fatalf("the runner did not take its turn on %s:\n%s", task, logs.String())
			}
			time.Sleep(200 * time.Millisecond)
		}
		w.must(worker.Process.Signal(syscall.SIGTERM))
		if err := worker.Wait(); err != nil {
			t.Fatalf("graceful shutdown: %v\n%s", err, logs.String())
		}
		return logs.String()
	}
	bundleOf := func(profile ai.RunnerConfig) *tskapi.BundleRef {
		b, err := w.c.AI.Bundle(profile)
		w.must(err)
		ref, err := b.Ref()
		w.must(err)
		return &tskapi.BundleRef{BundleID: ref.BundleID, SHA256: ref.SHA256}
	}

	// Off by default: a turn, and no trace anywhere.
	w.sourcedTask("task.untraced", tskapi.Target{CustomerID: "customer.1"}, tskapi.SourceRef{Kind: tskapi.SourceFile, ID: "file.untraced"},
		bundleOf(ai.RunnerConfig{Profile: execution.ScriptedProfile("scripted-q01-v1"), Budget: execution.Q01DefaultBudget()}),
		evdapi.CommandRegisterSource)
	script := filepath.Join(dir, "script.json")
	w.must(os.WriteFile(script, []byte(`[{"content":"没有可做的事。","usage":{"InputTokens":5,"OutputTokens":3}}]`), 0o600))
	before := filesUnder(t, dir)
	logs := run("task.untraced", env, "-model-script", script)
	if !strings.Contains(logs, "trace=off") || !slices.Equal(filesUnder(t, dir), before) {
		t.Fatalf("a worker without -agent-trace-dir wrote files %v\n%s", filesUnder(t, dir), logs)
	}

	// Never under CI=true.
	traces := filepath.Join(dir, "traces")
	refusedCI := exec.Command(bin, "-agent", "-agent-scope", where, "-agent-id", agentID, "-model-script", script, "-agent-trace-dir", traces)
	refusedCI.Env = append(slices.Clone(env), "CI=true")
	if out, err := refusedCI.CombinedOutput(); err == nil || !strings.Contains(string(out), "never runs under CI=true") {
		t.Fatalf("the trace started under CI=true: %v\n%s", err, out)
	}
	if _, err := os.Stat(traces); !os.IsNotExist(err) {
		t.Fatalf("the refused worker created %s: %v", traces, err)
	}

	// On request, against a service that demands the key.
	content := g2Inquiry("G2-TRACE", 0)
	w.rawFile(w.clerk, "file.traced", "inquiry.txt", evdapi.MimeText, []byte(content))
	w.sourcedTask("task.traced", tskapi.Target{RfqID: "rfq.traced", CustomerID: "customer.1"},
		tskapi.SourceRef{Kind: tskapi.SourceFile, ID: "file.traced"},
		bundleOf(ai.RunnerConfig{Profile: execution.StrataProfile(modelName), Budget: execution.Q01DefaultBudget()}), evdapi.CommandRegisterSource)
	var mu sync.Mutex
	var headers []string
	var bodies [][]byte
	service := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		mu.Lock()
		headers = append(headers, req.Header.Get("Authorization"))
		bodies = append(bodies, body)
		n := len(headers)
		mu.Unlock()
		if req.Header.Get("Authorization") != "Bearer "+key {
			http.Error(rw, `{"error":{"type":"authentication_error"}}`, http.StatusUnauthorized)
			return
		}
		rw.Header().Set("Content-Type", "application/json")
		if n == 1 {
			args, _ := json.Marshal(`{"SourceID":"src.traced","SourceKind":"file_extract","FileID":"file.traced"}`)
			fmt.Fprintf(rw, `{"model":%q,"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_a","type":"function",`+
				`"function":{"name":"source__register","arguments":%s}}]},"finish_reason":"tool_calls"}],`+
				`"usage":{"prompt_tokens":1200,"completion_tokens":30}}`, modelName, args)
			return
		}
		// A service that repeats the credential it was sent: the trace must not keep it.
		said, _ := json.Marshal("registered; the service was called with " + req.Header.Get("Authorization"))
		fmt.Fprintf(rw, `{"model":%q,"choices":[{"message":{"role":"assistant","content":%s},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":1500,"completion_tokens":12}}`, modelName, said)
	}))
	defer service.Close()
	logs = run("task.traced", append(slices.Clone(env), "IDP_BRANT_MODEL_API_KEY="+key), "-model", "openaicompat", "-model-base-url", service.URL,
		"-model-name", modelName, "-agent-trace-dir", traces)
	if strings.Contains(logs, key) || !strings.Contains(logs, "trace="+traces) {
		t.Fatalf("the worker's log:\n%s", logs)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(headers) != 2 {
		t.Fatalf("%d model requests", len(headers))
	}
	for i := range headers {
		if headers[i] != "Bearer "+key || bytes.Contains(bodies[i], []byte(key)) {
			t.Fatalf("request %d: the key travels in the Authorization header only (header %q, in the body %v)", i, headers[i],
				bytes.Contains(bodies[i], []byte(key)))
		}
	}
	// One turn file under the task's directory, and one file describing the tools on offer.
	var turns, described []string
	for _, f := range filesUnder(t, traces) {
		if st, err := os.Stat(f); err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("the mode of %s: %v %v", f, st, err)
		}
		content, err := os.ReadFile(f)
		w.must(err)
		if bytes.Contains(content, []byte(key)) {
			t.Fatalf("%s holds the credential", f)
		}
		if strings.HasSuffix(f, ".jsonl") {
			turns = append(turns, f)
		} else {
			described = append(described, f)
		}
	}
	wantDir := filepath.Join(traces, string(scope.Tenant), "company.A", "task.traced")
	if len(turns) != 1 || filepath.Dir(turns[0]) != wantDir || !strings.HasPrefix(filepath.Base(turns[0]), "turn-") || len(described) != 1 ||
		filepath.Dir(described[0]) != traces || !strings.HasPrefix(filepath.Base(described[0]), "tools-") {
		t.Fatalf("trace files %v and %v, want one turn file in %s and one tools file in %s", turns, described, wantDir, traces)
	}
	raw, err := os.ReadFile(turns[0])
	w.must(err)
	if !bytes.Contains(raw, []byte("[REDACTED]")) {
		t.Fatalf("the trace did not replace the credential the service repeated:\n%s", raw)
	}
	recs := traceRecords(t, turns[0])
	var kinds []string
	for _, rec := range recs {
		kinds = append(kinds, rec["record"].(string))
		if _, err := time.Parse("2006-01-02T15:04:05.000000Z", rec["at"].(string)); err != nil {
			t.Fatalf("record time %v", rec["at"])
		}
	}
	if !slices.Equal(kinds, []string{"turn", "facts", "model_reply", "tool_result", "facts", "model_reply", "end"}) {
		t.Fatalf("records %v", kinds)
	}
	head := recs[0]
	tools, _ := head["tools"].([]any)
	if head["trace"] != execution.TraceFormat || head["system_prompt"] != execution.Q01Prompt || head["task_id"] != "task.traced" ||
		head["agent"] != agentID || head["tenant"] != string(scope.Tenant) || head["model_requested"] != modelName ||
		!strings.Contains(head["note"].(string), "not evidence") || len(tools) != 1 || tools[0] != evdapi.CommandRegisterSource ||
		head["tools_file"] != filepath.Base(described[0]) {
		t.Fatalf("the turn record %v", head)
	}
	var offered []map[string]any
	toolsRaw, err := os.ReadFile(described[0])
	w.must(err)
	w.must(json.Unmarshal(toolsRaw, &offered))
	if len(offered) != 1 || offered[0]["name"] != evdapi.CommandRegisterSource || offered[0]["description"] == nil || offered[0]["parameters"] == nil {
		t.Fatalf("the tools file %s", toolsRaw)
	}
	first, _ := recs[1]["facts"].(map[string]any)
	if recs[1]["replan"] != false || first["source_file_id"] != "file.traced" || first["registered_source_id"] != nil || first["source_text"] != nil {
		t.Fatalf("the first facts %v", recs[1])
	}
	call, _ := recs[2]["tool_calls"].([]any)
	if len(call) != 1 || recs[2]["step"] != float64(1) || recs[2]["model_returned"] != modelName || recs[2]["input_tokens"] != float64(1200) {
		t.Fatalf("the model's first reply %v", recs[2])
	}
	c := call[0].(map[string]any)
	if args, _ := c["arguments"].(map[string]any); c["name"] != evdapi.CommandRegisterSource || c["id"] != "call_a" ||
		args["SourceID"] != "src.traced" || args["FileID"] != "file.traced" || args["SourceKind"] != evdapi.KindFileExtract {
		t.Fatalf("the traced tool call %v", c)
	}
	result, _ := recs[3]["content"].(map[string]any)
	if recs[3]["name"] != evdapi.CommandRegisterSource || recs[3]["tool_call_id"] != "call_a" || recs[3]["ok"] != true ||
		result["ok"] != true || result["object_id"] != "src.traced" || result["receipt"] != string(execution.Verified) {
		t.Fatalf("the traced tool result %v", recs[3])
	}
	rebuilt, _ := recs[4]["facts"].(map[string]any)
	if recs[4]["replan"] != true || rebuilt["registered_source_id"] != "src.traced" || rebuilt["source_text"] != strings.TrimSuffix(content, "\n") {
		t.Fatalf("the rebuilt facts %v", recs[4])
	}
	if said, _ := recs[5]["content"].(string); recs[5]["step"] != float64(2) || recs[5]["finish_reason"] != "stop" ||
		said != "registered; the service was called with Bearer [REDACTED]" {
		t.Fatalf("the model's last reply %v", recs[5])
	}
	if end := recs[6]; end["lifecycle"] != tskapi.StateWaitingInput || end["model_calls"] != float64(2) || end["tool_calls"] != float64(1) ||
		end["verified"] != true {
		t.Fatalf("the end record %v", end)
	}
}
