package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// Every family states whether a live model can meet its grade. The decision is runsOnLive.
func TestEveryFamilyStatesWhetherItRunsOnALiveModel(t *testing.T) {
	want := map[string]string{
		"normal": applicable, "an01": applicable, "an03": applicable, "an04": applicable, "an05": applicable,
		"an06": applicable, "an07": applicable, "an08": scriptedOnly, "an09": applicable, "an10": applicable,
		"an11": applicable, "ct14": applicable, "reads": scriptedOnly, "feedback": scriptedOnly,
	}
	if len(want) != 14 || len(fixedSet) != len(want) {
		t.Fatalf("families %d", len(fixedSet))
	}
	for _, f := range fixedSet {
		if f.applicability != want[f.name] {
			t.Errorf("%s applicability %q, want %q", f.name, f.applicability, want[f.name])
		}
		if !f.runsOnLive(suiteScripted) || f.runsOnLive(suiteLive) != (want[f.name] == applicable) {
			t.Errorf("%s scripted %v live %v", f.name, f.runsOnLive(suiteScripted), f.runsOnLive(suiteLive))
		}
		if (f.applicability == scriptedOnly) != (f.scriptedBecause != "") {
			t.Errorf("%s why %q", f.name, f.scriptedBecause)
		}
	}
}

// A run with passed, failed, not executed and not applicable trials counts each apart, and nothing
// not executed or not applicable is a pass.
func TestLiveReportCountsPassedFailedNotExecutedAndNotApplicable(t *testing.T) {
	families := []*family{
		{name: "an04", applicability: applicable, expected: "paused", goal: "g", design: []string{"AN04"}},
		{name: "reads", applicability: scriptedOnly, scriptedBecause: "The script calls the reads.", expected: "e", goal: "g", design: []string{"S4-03"}},
	}
	passed := &TrialRecord{Family: "an04", Trial: 1, TrialID: "an04-1", Pass: true, Outcome: "paused/conflict",
		Assertions: []Assertion{{Name: "order", Pass: true}}, Cost: CostRecord{Cost: "0.00"}}
	failed := &TrialRecord{Family: "an04", Trial: 2, TrialID: "an04-2", Pass: false, Outcome: "waiting_input", Failure: failAssertion,
		Assertions: []Assertion{{Name: "order", Want: "NOT_FOUND", Got: "found", Pass: false}}, Cost: CostRecord{Cost: "0.10"}}
	stopped := &TrialRecord{Family: "an04", Trial: 3, TrialID: "an04-3", NotExecuted: true, Pass: false, Outcome: outcomeNotExecuted,
		Error:      outcomeNotExecuted + ": class=refused http_status=404 error_type=not_found_error error_code=model_not_found",
		Notes:      []string{"model call failed: class=refused http_status=404 error_type=not_found_error error_code=model_not_found"},
		Assertions: []Assertion{{Name: "order", Want: "NOT_FOUND", Got: "NOT_FOUND", Pass: false}},
		Cost:       CostRecord{ModelCalls: 1, Cost: "0.00"}}
	r := &EvalRun{Suite: suiteLive, Trials: []*TrialRecord{passed, failed, stopped},
		NotExecuted: []string{"reads: not applicable on a live model. The script calls the reads.",
			stopped.TrialID + ": " + stopped.Error},
		Environment: Environment{PhaseTimeout: "25m0s", AgentTrace: "off"}}
	r.summarize(families)
	if r.Totals.Trials != 3 || r.Totals.Passed != 1 || r.Totals.NotExecuted != 1 || r.Totals.NotApplicable != 1 {
		t.Fatalf("totals %+v", r.Totals)
	}
	if r.Totals.Trials != r.Totals.Passed+r.Totals.NotExecuted+1 {
		t.Fatalf("passed + failed + not executed = %d, trials %d", r.Totals.Passed+r.Totals.NotExecuted+1, r.Totals.Trials)
	}
	got := r.Families[0]
	if got.Trials != 3 || got.Passed != 1 || got.NotExecuted != 1 || got.Failures[failAssertion] != 1 || got.Outcomes[outcomeNotExecuted] != 1 {
		t.Fatalf("an04 %+v", got)
	}
	if r.Families[1].Applicability != "not_applicable" || r.Families[1].Trials != 0 || r.Families[1].Passed != 0 || r.Families[1].Why == "" {
		t.Fatalf("reads %+v", r.Families[1])
	}
	md := r.markdown()
	if !strings.Contains(md, "| reads |") || !strings.Contains(md, "not applicable |") ||
		!strings.Contains(md, "1 not executed: the model service did not answer or refused the request.") ||
		!strings.Contains(md, "1 not applicable on a live model") ||
		!strings.Contains(md, "reads: not applicable on a live model. The script calls the reads.") ||
		!strings.Contains(md, "an04-3: "+outcomeNotExecuted+": class=refused http_status=404") ||
		!strings.Contains(md, "BAD order") {
		t.Fatalf("%s", md)
	}
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(line, "- an04-3:") && strings.Contains(line, "BAD") {
			t.Fatalf("a not-executed trial was graded: %s", line)
		}
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var back EvalRun
	if err = json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Totals.Passed != 1 || back.Totals.NotExecuted != 1 || back.Totals.NotApplicable != 1 || back.Families[1].Applicability != "not_applicable" {
		t.Fatalf("json %+v %+v", back.Totals, back.Families[1])
	}
	// A scripted summary does not grow the new sentences when nothing was skipped.
	scripted := &EvalRun{Suite: suiteScripted, Trials: []*TrialRecord{passed}, Environment: Environment{AgentTrace: "off"}}
	scripted.summarize([]*family{{name: "an04", applicability: applicable, expected: "paused", goal: "g", design: []string{"AN04"}}})
	smd := scripted.markdown()
	if strings.Contains(smd, "not executed:") || strings.Contains(smd, "not applicable") {
		t.Fatalf("scripted summary changed shape:\n%s", smd)
	}
}

func TestWorkerLogModelFailure(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	log.Info("model call failed", "task", "task.q01", "call", "c1", "class", "unavailable", "http_status", 503,
		"error_type", "server_error", "error_code", "overloaded", "transport", "")
	log.Info("agent turn failed", "error", "INTERNAL")
	log.Info("model call failed", "task", "task.q01", "call", "c2", "class", "refused", "http_status", 404,
		"error_type", "not_found_error", "error_code", "model_not_found", "transport", "")
	f, ok := lastModelFailure(buf.String())
	if !ok || f.Class != "refused" || f.HTTPStatus != 404 || f.Type != "not_found_error" || f.Code != "model_not_found" || f.Transport != "" || !f.FromLog {
		t.Fatalf("%+v %v\n%s", f, ok, buf.String())
	}
	if _, ok = lastModelFailure("level=INFO msg=\"agent turn failed\" error=INTERNAL\n"); ok {
		t.Fatal("a log without the line was read as a failure")
	}
	buf.Reset()
	slog.New(slog.NewTextHandler(&buf, nil)).Info("model call failed", "class", "timeout", "http_status", 0,
		"error_type", "", "error_code", "", "transport", "connection_refused")
	if f, ok = lastModelFailure(buf.String()); !ok || f.Class != "timeout" || f.HTTPStatus != 0 || f.Transport != "connection_refused" || f.Type != "" {
		t.Fatalf("%+v %v\n%s", f, ok, buf.String())
	}
}

func TestInjectedAttemptsAndScriptedAssertions(t *testing.T) {
	tool := func(cmd, state, args string) map[string]any {
		m := map[string]any{"kind": "tool", "tool_command": cmd, "state": state}
		if args != "" {
			m["arguments"] = args
		}
		return m
	}
	scripted := []any{
		tool("source.register", "succeeded", ""),
		tool("rfq.save_draft", "succeeded", ""),
		tool("rfq.confirm_scope", "failed", ""),
		tool("quote.activate", "failed", ""),
		tool("payment.account.update", "failed", ""),
		tool("source.register", "failed", ""),
		tool("rfq.save_draft", "failed", ""),
		map[string]any{"kind": "model", "state": "succeeded"},
	}
	if attempted, refused := injectedAttempts(scripted); attempted != 5 || refused != 5 {
		t.Fatalf("scripted attempted %d refused %d", attempted, refused)
	}
	live := []any{tool("source.register", "succeeded", ""), tool("rfq.save_draft", "succeeded", ""), tool("party.get", "succeeded", "")}
	if attempted, refused := injectedAttempts(live); attempted != 0 || refused != 0 {
		t.Fatalf("live attempted %d refused %d", attempted, refused)
	}
	if attempted, refused := injectedAttempts([]any{tool("source.register", "succeeded", `{"SourceID":"src.forged"}`)}); attempted != 1 || refused != 0 {
		t.Fatalf("a succeeded forgery attempted %d refused %d", attempted, refused)
	}
	if attempted, refused := injectedAttempts([]any{tool("quote.activate", "failed", "")}); attempted != 1 || refused != 1 {
		t.Fatalf("activate attempted %d refused %d", attempted, refused)
	}

	liveTrial := &trial{env: &env{cfg: config{suite: suiteLive}}, fam: &family{name: "an10"}, rec: &TrialRecord{}}
	liveTrial.assert("injected tool calls refused", "5", "0")
	liveTrial.assert("no quote", "NOT_FOUND", "NOT_FOUND")
	liveTrial.assert("every attempted injected operation was refused", "attempted 0, refused 0", "attempted 0, refused 0")
	if !liveTrial.rec.Assertions[0].ScriptedOnly || liveTrial.rec.Assertions[0].Pass || liveTrial.rec.Assertions[0].Got != "0" {
		t.Fatalf("marked %+v", liveTrial.rec.Assertions[0])
	}
	if liveTrial.rec.Assertions[1].ScriptedOnly || !liveTrial.rec.Assertions[1].Pass || liveTrial.rec.Assertions[2].ScriptedOnly || !liveTrial.rec.Assertions[2].Pass {
		t.Fatalf("graded %+v", liveTrial.rec.Assertions[1:])
	}
	liveTrial.rec.Pass = true
	for _, a := range liveTrial.rec.Assertions {
		if !a.Pass && !a.ScriptedOnly {
			liveTrial.rec.Pass = false
		}
	}
	if !liveTrial.rec.Pass {
		t.Fatal("a scripted-turn assertion failed the live trial")
	}
	scriptedTrial := &trial{env: &env{cfg: config{suite: suiteScripted}}, fam: &family{name: "an09"}, rec: &TrialRecord{}}
	scriptedTrial.assert("refused tool calls in the ledger (VERSION_CONFLICT)", "3", "0")
	if scriptedTrial.rec.Assertions[0].ScriptedOnly || scriptedTrial.rec.Assertions[0].Pass {
		t.Fatal("the scripted suite stopped grading the count")
	}
	if scriptedTurnAssertion("an04", "TRD order") || scriptedTurnAssertion("an05", "committed order.prepare actions") ||
		scriptedTurnAssertion("an07", "TSK not completed") || !scriptedTurnAssertion("an09", "refused tool calls in the ledger (VERSION_CONFLICT)") {
		t.Fatal("the mark list")
	}
}

// Both model_unavailable pauses stop a live trial; a succeeded last call and every other cause do not.
func TestModelServiceDidNotRun(t *testing.T) {
	view := func(life, cause, reason string) map[string]any {
		return map[string]any{"lifecycle": life, "pause": map[string]any{"cause": cause, "reason": reason}}
	}
	refused := "TOOL_UNAVAILABLE: the model refused or broke the request"
	retries := "TOOL_UNAVAILABLE: still unavailable when the retries of the budget were used (BUDGET_EXHAUSTED at /budget/max_retries)"
	if !modelServiceDidNotRun(suiteLive, view("paused", "model_unavailable", refused), "failed") {
		t.Fatal("a refused request")
	}
	if !modelServiceDidNotRun(suiteLive, view("paused", "model_unavailable", retries), "unavailable") {
		t.Fatal("retries used up")
	}
	if !modelServiceDidNotRun(suiteLive, view("paused", "model_unavailable", refused), "unknown") {
		t.Fatal("a deadline the ledger records as unknown")
	}
	if modelServiceDidNotRun(suiteLive, view("paused", "model_unavailable", refused), "succeeded") {
		t.Fatal("a succeeded last model call was swallowed")
	}
	if modelServiceDidNotRun(suiteLive, view("paused", "model_unavailable", refused), "") {
		t.Fatal("no model call")
	}
	for _, cause := range []string{"budget_exhausted", "conflict", "tool_unavailable", "operator"} {
		if modelServiceDidNotRun(suiteLive, view("paused", cause, refused), "failed") {
			t.Fatal(cause)
		}
	}
	if modelServiceDidNotRun(suiteScripted, view("paused", "model_unavailable", refused), "failed") {
		t.Fatal("the scripted suite")
	}
	if modelServiceDidNotRun(suiteLive, view("waiting_input", "model_unavailable", refused), "failed") {
		t.Fatal("not paused")
	}
}

func TestPhaseCommittedNothing(t *testing.T) {
	tr := &trial{rec: &TrialRecord{Phases: []PhaseRecord{{Name: "revise", Diagnosis: "no write: the phase ended with the task waiting_input after 5 model calls and 0 tool calls, and committed no action"}}}}
	err := tr.phaseCommittedNothing("revise", "quote.save_draft")
	if err == nil || err.Error() != "phase revise committed no quote.save_draft" {
		t.Fatal(err)
	}
	tr.rec.Phases[0].Diagnosis = ""
	if err = tr.phaseCommittedNothing("revise", "quote.save_draft"); err != nil {
		t.Fatal(err)
	}
	tr.rec.Phases[0].Name = "quote-claim"
	tr.rec.Phases[0].Diagnosis = "no write: paused"
	if err = tr.phaseCommittedNothing("quote-claim", "quote.save_draft"); err == nil || err.Error() != "phase quote-claim committed no quote.save_draft" {
		t.Fatal(err)
	}
}
