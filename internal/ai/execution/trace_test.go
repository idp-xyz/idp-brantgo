package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"go.idp.xyz/idp-brant/internal/ai/adapters/model"
	"go.idp.xyz/idp-brant/internal/ai/executionevidence"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	tskapi "go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

func readTrace(t *testing.T, path string) (string, []map[string]any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		var rec map[string]any
		if err = json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("%v in %q", err, line)
		}
		out = append(out, rec)
	}
	return string(raw), out
}

// The trace of a turn (S4 G2): one JSON Lines file per turn under <dir>/<tenant>/<company>/<task>/,
// a record per line in the order things happened, each written when it happens; what the model was
// told (prompt, tools, facts) and what it sent and got back (replies with tool calls and arguments,
// tool results) in clear, errors by class only. No record holds a secret the directory was given:
// neither as it is nor as it stands inside a JSON string. Files are private to their owner. A nil
// trace records nothing.
func TestTraceWritesATurnAndNeverASecret(t *testing.T) {
	const secret = `k3y-"quoted"-\value`
	root := filepath.Join(t.TempDir(), "trace")
	d, err := NewTraceDir(root, "", secret)
	if err != nil || d.Dir() != root {
		t.Fatal(d, err)
	}
	if st, err := os.Stat(root); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("the trace directory %v %v", st, err)
	}
	scope := kernel.Scope{Tenant: "tenant:1", Company: "company.A"}
	view := tskapi.TaskView{TaskID: "task.q01", TaskVersion: 3, Lifecycle: tskapi.StateRunning, GoalCode: tskapi.GoalQuoteReady}
	at := time.Date(2026, 11, 18, 3, 4, 5, 678901000, time.UTC)
	tools := []model.Tool{{Name: "source.register", Description: "Register a source", Parameters: json.RawMessage(`{"type":"object"}`)}}
	tr := d.begin(scope, "agent.q01", view, traceBundle{BundleID: "q01.agent", SHA256: strings.Repeat("a", 64), Profile: "strata.local.v1",
		Model: "m", PromptSHA256: strings.Repeat("b", 64)}, "the system prompt", tools, at)
	wantPath := filepath.Join(root, "tenant_1", "company.A", "task.q01", "turn-20261118T030405.678901Z-0001.jsonl")
	if tr.path != wantPath {
		t.Fatalf("trace file %s, want %s", tr.path, wantPath)
	}
	tr.facts(at, `{"task_id":"task.q01","source_file_id":"file.inquiry"}`, false)
	// The first records are on disk before the turn ends.
	if raw, recs := readTrace(t, wantPath); len(recs) != 2 || !strings.HasSuffix(raw, "\n") {
		t.Fatalf("%d records before the end", len(recs))
	}
	call := model.ToolCall{ID: "call_1", Name: "source.register", Arguments: json.RawMessage(`{"SourceID":"src.1","FileID":"file.inquiry"}`)}
	broken := model.ToolCall{ID: "call_2", Name: "rfq.save_draft", Arguments: json.RawMessage(`{"RfqID":`)}
	tr.reply(at, model.Response{Model: "m-as-returned", FinishReason: "tool_calls", Usage: model.Usage{InputTokens: 1200, OutputTokens: 40},
		Message: model.Message{Content: "the header was Bearer " + secret, ToolCalls: []model.ToolCall{call, broken}}}, 1500*time.Millisecond)
	tr.toolResult(at, call, `{"ok":true,"receipt":"VERIFIED","object_id":"src.1"}`, "")
	tr.toolResult(at, broken, `{"ok":false,"code":"SCHEMA_INVALID","path":"/args"}`, "SCHEMA_INVALID")
	tr.facts(at, `{"task_id":"task.q01","registered_source_id":"src.1","source_text":"`+strings.ReplaceAll(strings.ReplaceAll(secret, `\`, `\\`), `"`, `\"`)+`"}`, true)
	tr.modelError(at, "unavailable", false)
	tr.modelError(at, "timeout", true)
	tr.reply(at, model.Response{Model: "m-as-returned", FinishReason: "stop", Message: model.Message{Content: "done"}}, time.Second)
	tr.toolEnded(at, "tool_wait", call, "")
	tr.toolEnded(at, "tool_error", call, "internal")
	if err = tr.end(at, TurnResult{Lifecycle: tskapi.StateWaitingInput, ModelCalls: 4, ToolCalls: 2, Refusals: []string{"rfq.save_draft SCHEMA_INVALID"},
		Verified: true}, errors.New("connection reset: "+secret)); err != nil {
		t.Fatal(err)
	}
	raw, recs := readTrace(t, wantPath)
	var kinds []string
	for _, rec := range recs {
		kinds = append(kinds, rec["record"].(string))
		if rec["at"] != "2026-11-18T03:04:05.678901Z" {
			t.Fatalf("record time %v", rec["at"])
		}
	}
	if !slices.Equal(kinds, []string{"turn", "facts", "model_reply", "tool_result", "tool_result", "facts", "model_error", "model_error",
		"model_reply", "tool_wait", "tool_error", "end"}) {
		t.Fatalf("records %v", kinds)
	}
	// Never a secret: not raw, not as JSON escapes it; each place it stood says so.
	escaped, _ := json.Marshal(secret)
	if strings.Contains(raw, secret) || strings.Contains(raw, string(escaped[1:len(escaped)-1])) || strings.Count(raw, traceRedacted) != 2 {
		t.Fatalf("the trace holds the secret, or did not replace it twice:\n%s", raw)
	}
	head := recs[0]
	if head["trace"] != TraceFormat || head["note"] != TraceNote || head["tenant"] != "tenant:1" || head["company"] != "company.A" ||
		head["agent"] != "agent.q01" || head["task_id"] != "task.q01" || head["task_version"] != float64(3) || head["goal"] != tskapi.GoalQuoteReady ||
		head["bundle_sha256"] != strings.Repeat("a", 64) || head["prompt_sha256"] != strings.Repeat("b", 64) || head["system_prompt"] != "the system prompt" ||
		head["model_profile"] != "strata.local.v1" {
		t.Fatalf("turn %v", head)
	}
	// The tools on offer are named in the turn and described once, in a file of their own.
	toolsFile, _ := head["tools_file"].(string)
	toolsRaw, err := os.ReadFile(filepath.Join(root, toolsFile))
	if err != nil || len(head["tools"].([]any)) != 1 || head["tools"].([]any)[0] != "source.register" ||
		!strings.HasPrefix(toolsFile, "tools-") || !strings.HasSuffix(toolsFile, ".json") || len(toolsFile) != len("tools-.json")+12 ||
		head["tools_sha256"] != executionevidence.SHA256Hex(toolsRaw) || !strings.HasPrefix(head["tools_sha256"].(string), toolsFile[6:18]) {
		t.Fatalf("tools %v in %q: %v", head["tools"], toolsFile, err)
	}
	var described []map[string]any
	if err = json.Unmarshal(toolsRaw, &described); err != nil || len(described) != 1 || described[0]["name"] != "source.register" ||
		described[0]["description"] != "Register a source" || described[0]["parameters"].(map[string]any)["type"] != "object" {
		t.Fatalf("tools file %s: %v", toolsRaw, err)
	}
	if st, err := os.Stat(filepath.Join(root, toolsFile)); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("the tools file %v %v", st, err)
	}
	if f := recs[1]["facts"].(map[string]any); recs[1]["replan"] != false || f["source_file_id"] != "file.inquiry" {
		t.Fatalf("facts %v", recs[1])
	}
	reply := recs[2]
	calls := reply["tool_calls"].([]any)
	if reply["step"] != float64(1) || reply["model_returned"] != "m-as-returned" || reply["latency_ms"] != float64(1500) ||
		reply["input_tokens"] != float64(1200) || reply["content"] != "the header was Bearer "+traceRedacted ||
		calls[0].(map[string]any)["arguments"].(map[string]any)["SourceID"] != "src.1" ||
		// Arguments that are not JSON are kept as the text the model sent.
		calls[1].(map[string]any)["arguments"] != `{"RfqID":` {
		t.Fatalf("model reply %v", reply)
	}
	if ok, refused := recs[3], recs[4]; ok["ok"] != true || ok["tool_call_id"] != "call_1" || ok["content"].(map[string]any)["object_id"] != "src.1" ||
		ok["code"] != nil || refused["ok"] != false || refused["code"] != "SCHEMA_INVALID" || refused["step"] != float64(1) ||
		refused["content"].(map[string]any)["path"] != "/args" {
		t.Fatalf("tool results %v %v", ok, refused)
	}
	if recs[5]["replan"] != true || recs[5]["facts"].(map[string]any)["source_text"] != traceRedacted {
		t.Fatalf("rebuilt facts %v", recs[5])
	}
	// A model call that did not answer is recorded by class; it is a step of its own.
	if a, b := recs[6], recs[7]; a["class"] != "unavailable" || a["step"] != float64(2) || a["retry"] != false || b["class"] != "timeout" ||
		b["step"] != float64(3) || b["retry"] != true || len(a) != 5 {
		t.Fatalf("model errors %v %v", a, b)
	}
	if recs[9]["name"] != "source.register" || recs[9]["class"] != nil || recs[10]["class"] != "internal" || recs[10]["step"] != float64(4) {
		t.Fatalf("tool ends %v %v", recs[9], recs[10])
	}
	// The end records the error's class, never its text.
	end := recs[11]
	if end["lifecycle"] != tskapi.StateWaitingInput || end["model_calls"] != float64(4) || end["verified"] != true || end["error_class"] != "INTERNAL" ||
		end["refusals"].([]any)[0] != "rfq.save_draft SCHEMA_INVALID" || end["paused"] != nil {
		t.Fatalf("end %v", end)
	}
	if st, err := os.Stat(wantPath); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("the trace file %v %v", st, err)
	}
	if st, err := os.Stat(filepath.Dir(wantPath)); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("the task's trace directory %v %v", st, err)
	}
	// The next turn is the next file; the same tools are not described again, another list is.
	next := d.begin(scope, "agent.q01", view, traceBundle{}, "p", tools, at)
	if !strings.HasSuffix(next.path, "-0002.jsonl") || next.end(at, TurnResult{}, nil) != nil {
		t.Fatalf("next turn %s", next.path)
	}
	if files, _ := filepath.Glob(filepath.Join(root, "tools-*.json")); len(files) != 1 {
		t.Fatalf("tools files %v", files)
	}
	if other := d.begin(scope, "agent.q01", view, traceBundle{}, "p", nil, at); other.end(at, TurnResult{}, nil) != nil {
		t.Fatal("a turn without tools")
	}
	if files, _ := filepath.Glob(filepath.Join(root, "tools-*.json")); len(files) != 2 {
		t.Fatalf("tools files %v", files)
	}

	// Off: a nil directory gives a nil trace, and a nil trace records nothing.
	var off *TraceDir
	none := off.begin(scope, "agent.q01", view, traceBundle{}, "p", nil, at)
	none.facts(at, "{}", false)
	none.reply(at, model.Response{}, 0)
	none.modelError(at, "timeout", false)
	none.toolResult(at, call, "{}", "")
	none.toolEnded(at, "tool_wait", call, "")
	if none != nil || none.end(at, TurnResult{}, nil) != nil {
		t.Fatal("a nil trace")
	}
	if _, err = NewTraceDir("  "); err == nil {
		t.Fatal("an empty trace directory")
	}
	// A trace that cannot be written says so at the end and does not panic.
	blocked := filepath.Join(t.TempDir(), "file")
	if err = os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := (&TraceDir{dir: blocked}).begin(scope, "agent.q01", view, traceBundle{}, "p", nil, at)
	bad.facts(at, "{}", false)
	if bad.end(at, TurnResult{}, nil) == nil {
		t.Fatal("a trace under a file was written")
	}
	if _, err = NewTraceDir(filepath.Join(blocked, "sub")); err == nil {
		t.Fatal("a trace directory under a file")
	}
}

// fakeSources is EVD's read of a source as the agent.
type fakeSources struct {
	views map[string]evdapi.SourceView
	errs  map[string]error
	asked []string
}

func (f *fakeSources) GetSource(_ context.Context, id string) (evdapi.SourceView, error) {
	f.asked = append(f.asked, id)
	if err := f.errs[id]; err != nil {
		return evdapi.SourceView{}, err
	}
	if v, ok := f.views[id]; ok {
		return v, nil
	}
	return evdapi.SourceView{}, kernel.Reject(kernel.CodeNotFound, "/id", "")
}

func registered(action, source, state string) tskapi.ActionView {
	a := tskapi.ActionView{ActionID: action, State: state, Target: tskapi.TargetCommand{Context: tskapi.TargetEVD, Command: evdapi.CommandRegisterSource}}
	if state == tskapi.ActionCommitted {
		a.Result = &tskapi.TargetReceipt{ObjectID: source}
	}
	return a
}

// The source of a file task (S4 G2) is the object of the task's oldest committed source.register
// that EVD shows, to the agent now, as an extract of the task's file. The action names the source;
// EVD names the file it was read from — never the model. A source of another file, a text source,
// a source the agent cannot read and an action that has not committed are not it; each is looked
// at once per turn.
func TestRegisteredExtractIsWhatEVDSaysOfTheTasksFile(t *testing.T) {
	extract := func(id, file string) evdapi.SourceView {
		return evdapi.SourceView{SourceID: id, SourceKind: evdapi.KindFileExtract, ContentText: "text of " + id, Extract: &evdapi.ExtractRef{FileID: file}}
	}
	src := &fakeSources{views: map[string]evdapi.SourceView{
		"src.other": extract("src.other", "file.other"),
		"src.text":  {SourceID: "src.text", SourceKind: evdapi.KindManualText, ContentText: "a note"},
		"src.first": extract("src.first", "file.inquiry"),
		"src.again": extract("src.again", "file.inquiry"),
	}, errs: map[string]error{}}
	newTurn := func() *turn {
		return &turn{r: &Runner{Sources: src}, agent: context.Background(), others: map[string]bool{},
			view: tskapi.TaskView{TaskID: "task.1", Source: &tskapi.SourceRef{Kind: tskapi.SourceFile, ID: "file.inquiry"}}}
	}
	quote := tskapi.ActionView{ActionID: "a.quote", State: tskapi.ActionCommitted, Result: &tskapi.TargetReceipt{ObjectID: "quote.1"},
		Target: tskapi.TargetCommand{Context: tskapi.TargetTRD, Command: trdapi.CommandSaveDraft}}
	actions := []tskapi.ActionView{quote, registered("a.open", "src.first", tskapi.ActionSubmitted), registered("a.other", "src.other", tskapi.ActionCommitted),
		registered("a.text", "src.text", tskapi.ActionCommitted), registered("a.hidden", "src.hidden", tskapi.ActionCommitted)}

	tn := newTurn()
	if tn.awaitsExtract(nil) || tn.awaitsExtract([]tskapi.ActionView{quote, actions[1]}) || !tn.awaitsExtract(actions) {
		t.Fatal("a file task awaits its extract exactly while a committed source.register has not been looked at")
	}
	got, err := tn.registeredExtract(actions)
	if err != nil || got != nil || tn.source != "" || !slices.Equal(src.asked, []string{"src.other", "src.text", "src.hidden"}) {
		t.Fatalf("no extract of the file yet: %+v %v, asked %v", got, err, src.asked)
	}
	// Looked at once: the same actions ask nothing more, and nothing is awaited.
	if got, err = tn.registeredExtract(actions); err != nil || got != nil || len(src.asked) != 3 || tn.awaitsExtract(actions) {
		t.Fatalf("asked again: %v", src.asked)
	}
	// The oldest committed registration of the file is the task's source.
	actions = append(actions, registered("a.first", "src.first", tskapi.ActionCommitted), registered("a.again", "src.again", tskapi.ActionCommitted))
	if !tn.awaitsExtract(actions) {
		t.Fatal("a new committed registration is awaited")
	}
	if got, err = tn.registeredExtract(actions); err != nil || got == nil || got.SourceID != "src.first" || tn.source != "src.first" ||
		got.ContentText != "text of src.first" || tn.awaitsExtract(actions) {
		t.Fatalf("the file's extract: %+v %v", got, err)
	}
	// An infrastructure error is an error, not "no source".
	src.errs["src.other"] = errors.New("connection reset")
	if _, err = newTurn().registeredExtract(actions); err == nil {
		t.Fatal("an EVD failure was read as no source")
	}
	// Other kinds of task source never await an extract.
	for _, s := range []*tskapi.SourceRef{nil, {Kind: tskapi.SourceEVD, ID: "src.first"}, {Kind: tskapi.SourceRFQ, ID: "rfq.1"}} {
		other := newTurn()
		other.view.Source = s
		if other.awaitsExtract(actions) {
			t.Fatalf("%+v awaits an extract", s)
		}
	}
	without := newTurn()
	without.r.Sources = nil
	if without.awaitsExtract(actions) {
		t.Fatal("a runner without EVD reads awaits an extract")
	}
}

// The Q01 prompt (S4 G2, after the first live run): it states the order of work — a file source is
// registered first, with the fields of source.register it names; the target RFQ and quote are
// created by the agent's draft at expected version 0, and NOT_FOUND on a target is never a reason to
// stop; an ID the agent chooses follows the platform's identifier rule. Every field it names is a
// field of the command it names, and it stays short: every turn sends it.
func TestQ01PromptStatesTheOrderOfWork(t *testing.T) {
	for _, want := range []string{
		"source_file_id", "registered_source_id", "source_text", "source_truncated",
		`source.register with`, `SourceKind "file_extract"`, "FileID = source_file_id", "a new SourceID", "Send no ContentText",
		"rfq.save_draft with the target RfqID at ExpectedVersion 0", "quote.save_draft with the target QuoteID at ExpectedVersion 0",
		"LineID L1, L2, ... in the inquiry's order",
		`NOT_FOUND from a read of a target means "not created yet": your draft at ExpectedVersion 0 creates the target RFQ or quote.`,
		"It is never a reason to stop or to read again.",
		"1 to 64 characters: letters, digits and . _ : -", "starting with a letter or digit",
		"When the facts list the grant for it, send the same call again", "answer without a tool call",
		"Do the first step below that is not done yet.",
	} {
		if !strings.Contains(strings.Join(strings.Fields(Q01Prompt), " "), strings.Join(strings.Fields(want), " ")) {
			t.Errorf("the prompt does not say %q", want)
		}
	}
	fields := func(v any) []string {
		var out []string
		rt := reflect.TypeOf(v)
		for i := range rt.NumField() {
			out = append(out, rt.Field(i).Name)
		}
		return out
	}
	for _, f := range []string{"SourceID", "SourceKind", "FileID", "ContentText"} {
		if !slices.Contains(fields(evdapi.RegisterSource{}), f) {
			t.Errorf("source.register has no field %s", f)
		}
	}
	for _, f := range []string{"QuoteID", "ExpectedVersion"} {
		if !slices.Contains(fields(trdapi.SaveDraft{}), f) {
			t.Errorf("quote.save_draft has no field %s", f)
		}
	}
	if evdapi.KindFileExtract != "file_extract" {
		t.Fatal(evdapi.KindFileExtract)
	}
	// The identifier rule as the prompt states it is the kernel's.
	for id, valid := range map[string]bool{"src.task.q01": true, "a": true, "S_1:x-2.y": true, strings.Repeat("a", 64): true,
		strings.Repeat("a", 65): false, ".src": false, "src 1": false, "src/1": false, "询价": false, "": false} {
		if kernel.ValidIdentifier(id) != valid {
			t.Errorf("identifier %q: %v", id, kernel.ValidIdentifier(id))
		}
	}
	// Short: the first live run sent about 5,800 input tokens a turn, most of them tool schemas. The
	// prompt before this order of work had 2,816 characters.
	if n := utf8.RuneCountInString(Q01Prompt); n > 3000 {
		t.Errorf("the prompt has %d characters; keep it at 3000 or less", n)
	}
	if bytes.ContainsAny([]byte(Q01Prompt), "\t") {
		t.Error("the prompt holds a tab")
	}
}
