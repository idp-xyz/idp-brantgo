package execution

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.idp.xyz/idp-brant/internal/ai/adapters/model"
	"go.idp.xyz/idp-brant/internal/ai/executionevidence"
	tskapi "go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// TraceFormat names the record format of a trace file.
const TraceFormat = "brant.agent.trace/1"

// TraceNote is the first thing a trace file says about itself.
const TraceNote = "Local diagnostic trace of an agent turn: what the model was told and what it sent. It is not evidence and not a ledger: " +
	"the model-call ledger (brant_ai.model_call) keeps hashes by design, and the authoritative records are the contexts'. " +
	"It holds source text, tool arguments and tool results in clear, so it is for synthetic trials on a developer's machine only."

// traceRedacted replaces a secret that would have been written.
const traceRedacted = "[REDACTED]"

// TraceDir is the agent runner's opt-in, local, off-by-default diagnostic trace (S4 G2; brant-worker
// -agent-trace-dir). The model-call ledger keeps hashes of what a turn sent and received (decision
// E-6); to classify why a live model failed a trial, a person has to see the words. With a TraceDir
// the runner writes, for every turn in which it asks the model, one JSON Lines file
//
//	<Dir>/<tenant>/<company>/<task>/turn-<UTC time>-<n>.jsonl
//
// (a ':' of an identifier is written '_'), and once per distinct list of tools on offer
//
//	<Dir>/tools-<first 12 digits of its SHA-256>.json
//
// with each tool's name, description and the JSON Schema of its arguments as the model is sent
// them. A turn file holds a record per line, in the order things happened:
//
//	turn         the task, the bundle, the model profile, the system prompt, and the names of the
//	             tools on offer with the tools file that describes them
//	facts        the user message of the turn — the task's facts — as sent; replan true when the
//	             runner rebuilt it inside the turn (a file source was registered)
//	model_reply  one model call's answer: text, tool calls with their arguments, finish reason,
//	             tokens, the model ID as returned
//	model_error  a model call that did not answer: its class (unavailable, timeout, refused), no text
//	tool_result  what a tool call returned to the model, exactly: the receipt, the read or the
//	             refusal's code and path
//	tool_wait    a tool call that made the task wait for an approval grant (nothing went back)
//	tool_error   a tool call that ended the turn with an infrastructure error: its class, no text
//	end          how the turn ended: lifecycle, calls, refusals, pause, wait, verification
//
// Each record is written when it happens, so a turn cut short still shows how far it came.
//
// A trace never holds a credential. The model's key is not part of a request or an answer: the
// adapter puts it in the HTTP Authorization header and nowhere else, and the runner never sees it.
// As a second line, NewTraceDir takes the values that must not be written whatever a record holds
// (the worker passes the key); a record that would hold one is written with it replaced. Errors are
// recorded by class, never by text.
//
// It is never on by default, and brant-worker refuses it under CI=true. It records synthetic trials
// for diagnosis; it is not evidence.
type TraceDir struct {
	dir     string
	secrets [][]byte
	mu      sync.Mutex
	seq     int
	tools   map[string]bool // SHA-256s of the tool lists already written
}

// NewTraceDir prepares dir (created with mode 0700 when missing) for traces that never hold secrets.
func NewTraceDir(dir string, secrets ...string) (*TraceDir, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("execution: the trace directory is empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("execution: the trace directory: %w", err)
	}
	d := &TraceDir{dir: dir}
	for _, s := range secrets {
		if s == "" {
			continue
		}
		d.secrets = append(d.secrets, []byte(s))
		// The same value as it stands inside a JSON string, when escaping changes it.
		if raw, err := json.Marshal(s); err == nil && len(raw) > 2 && string(raw[1:len(raw)-1]) != s {
			d.secrets = append(d.secrets, raw[1:len(raw)-1])
		}
	}
	return d, nil
}

// Dir returns the directory the traces are written under.
func (d *TraceDir) Dir() string { return d.dir }

// turnTrace is the trace file of one turn. A nil turnTrace records nothing: the runner calls it
// unconditionally.
type turnTrace struct {
	d    *TraceDir
	path string
	f    *os.File
	err  error
	step int
}

func traceName(id string) string { return strings.ReplaceAll(id, ":", "_") }

// begin opens the trace of a turn on task v as agent in scope and writes its first record.
func (d *TraceDir) begin(scope kernel.Scope, agent string, v tskapi.TaskView, ref traceBundle, prompt string, tools []model.Tool, now time.Time) *turnTrace {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	d.seq++
	n := d.seq
	d.mu.Unlock()
	dir := filepath.Join(d.dir, traceName(string(scope.Tenant)), traceName(string(scope.Company)), traceName(v.TaskID))
	t := &turnTrace{d: d, path: filepath.Join(dir, fmt.Sprintf("turn-%s-%04d.jsonl", now.UTC().Format("20060102T150405.000000Z"), n))}
	if t.err = os.MkdirAll(dir, 0o700); t.err != nil {
		return t
	}
	if t.f, t.err = os.OpenFile(t.path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); t.err != nil {
		return t
	}
	names := make([]string, 0, len(tools))
	for _, x := range tools {
		names = append(names, x.Name)
	}
	file, sum, err := d.toolsFile(tools)
	t.write(now, "turn", map[string]any{"trace": TraceFormat, "note": TraceNote, "tenant": string(scope.Tenant), "company": string(scope.Company),
		"agent": agent, "task_id": v.TaskID, "task_version": v.TaskVersion, "lifecycle": v.Lifecycle, "goal": v.GoalCode,
		"bundle_id": ref.BundleID, "bundle_sha256": ref.SHA256, "model_profile": ref.Profile, "model_requested": ref.Model,
		"prompt_sha256": ref.PromptSHA256, "system_prompt": prompt, "tools": names, "tools_file": file, "tools_sha256": sum})
	if err != nil && t.err == nil {
		t.err = err
	}
	return t
}

// toolsFile writes the tools on offer — each tool's name, description and the JSON Schema of its
// arguments, as the model is sent them — once per distinct list, as tools-<12 digits>.json in the
// trace directory, and returns the file's name and the SHA-256 of its content.
func (d *TraceDir) toolsFile(tools []model.Tool) (string, string, error) {
	type tool struct {
		Name        string `json:"name"`
		Description string `json:"description,omitempty"`
		Parameters  any    `json:"parameters,omitempty"`
	}
	offered := make([]tool, 0, len(tools))
	for _, x := range tools {
		offered = append(offered, tool{Name: x.Name, Description: x.Description, Parameters: rawOrText(x.Parameters)})
	}
	raw, err := json.MarshalIndent(offered, "", "  ")
	if err != nil {
		return "", "", err
	}
	raw = append(d.scrub(raw), '\n')
	sum := executionevidence.SHA256Hex(raw)
	name := "tools-" + sum[:12] + ".json"
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.tools[sum] {
		return name, sum, nil
	}
	if err = os.WriteFile(filepath.Join(d.dir, name), raw, 0o600); err != nil {
		return "", "", err
	}
	if d.tools == nil {
		d.tools = map[string]bool{}
	}
	d.tools[sum] = true
	return name, sum, nil
}

// scrub replaces every secret in raw.
func (d *TraceDir) scrub(raw []byte) []byte {
	for _, s := range d.secrets {
		raw = bytes.ReplaceAll(raw, s, []byte(traceRedacted))
	}
	return raw
}

// traceBundle is what a trace says of the bundle a turn runs with.
type traceBundle struct {
	BundleID, SHA256, Profile, Model, PromptSHA256 string
}

// rawOrText is raw as JSON when it is JSON, else as text: a model's arguments may be anything.
func rawOrText(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	if json.Valid(raw) {
		return json.RawMessage(raw)
	}
	return string(raw)
}

// write appends one record. The first failure stops the trace; Err reports it.
func (t *turnTrace) write(at time.Time, record string, fields map[string]any) {
	if t == nil || t.err != nil || t.f == nil {
		return
	}
	// The record's name and time first; the rest in the encoder's (sorted) order.
	head, err := json.Marshal(struct {
		Record string `json:"record"`
		At     string `json:"at"`
	}{record, at.UTC().Format("2006-01-02T15:04:05.000000Z")})
	if err != nil {
		t.err = err
		return
	}
	body, err := json.Marshal(fields)
	if err != nil {
		t.err = err
		return
	}
	line := head
	if len(body) > 2 {
		line = append(append(head[:len(head)-1:len(head)-1], ','), body[1:]...)
	}
	line = t.d.scrub(line)
	if _, t.err = t.f.Write(append(line, '\n')); t.err != nil {
		_ = t.f.Close()
		t.f = nil
	}
}

func (t *turnTrace) facts(at time.Time, facts string, replan bool) {
	if t == nil {
		return
	}
	t.write(at, "facts", map[string]any{"replan": replan, "facts": rawOrText([]byte(facts))})
}

// reply records one model call's answer; it opens the next step.
func (t *turnTrace) reply(at time.Time, resp model.Response, latency time.Duration) {
	if t == nil {
		return
	}
	t.step++
	type call struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Arguments any    `json:"arguments"`
	}
	calls := make([]call, 0, len(resp.Message.ToolCalls))
	for _, c := range resp.Message.ToolCalls {
		calls = append(calls, call{ID: c.ID, Name: c.Name, Arguments: rawOrText(c.Arguments)})
	}
	t.write(at, "model_reply", map[string]any{"step": t.step, "model_returned": resp.Model, "finish_reason": resp.FinishReason,
		"input_tokens": resp.Usage.InputTokens, "output_tokens": resp.Usage.OutputTokens, "latency_ms": latency.Milliseconds(),
		"content": resp.Message.Content, "tool_calls": calls})
}

// modelError records a model call that did not answer, by class only.
func (t *turnTrace) modelError(at time.Time, class string, retry bool) {
	if t == nil {
		return
	}
	t.step++
	t.write(at, "model_error", map[string]any{"step": t.step, "class": class, "retry": retry})
}

// toolResult records what a tool call returned to the model.
func (t *turnTrace) toolResult(at time.Time, call model.ToolCall, answer, code string) {
	if t == nil {
		return
	}
	f := map[string]any{"step": t.step, "tool_call_id": call.ID, "name": call.Name, "ok": code == "", "content": rawOrText([]byte(answer))}
	if code != "" {
		f["code"] = code
	}
	t.write(at, "tool_result", f)
}

// toolEnded records a tool call that returned nothing to the model: a wait, or an error by class.
func (t *turnTrace) toolEnded(at time.Time, record string, call model.ToolCall, class string) {
	if t == nil {
		return
	}
	f := map[string]any{"step": t.step, "tool_call_id": call.ID, "name": call.Name}
	if class != "" {
		f["class"] = class
	}
	t.write(at, record, f)
}

// end records how the turn ended and closes the file. It returns the trace's first write error.
func (t *turnTrace) end(at time.Time, res TurnResult, turnErr error) error {
	if t == nil {
		return nil
	}
	f := map[string]any{"lifecycle": res.Lifecycle, "model_calls": res.ModelCalls, "tool_calls": res.ToolCalls, "verified": res.Verified}
	if len(res.Refusals) > 0 {
		f["refusals"] = res.Refusals
	}
	for k, v := range map[string]string{"paused": res.Paused, "waiting": res.Waiting, "skipped": res.Skipped} {
		if v != "" {
			f[k] = v
		}
	}
	if turnErr != nil {
		f["error_class"] = errorCode(turnErr)
	}
	t.write(at, "end", f)
	if t.f != nil {
		if err := t.f.Close(); err != nil && t.err == nil {
			t.err = err
		}
		t.f = nil
	}
	return t.err
}
