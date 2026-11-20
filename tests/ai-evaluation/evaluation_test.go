package main

import (
	"archive/zip"
	"bytes"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// S4 G2: a failed agent phase is explained from the model-call ledger — each call by kind, state,
// code and, for a model call, finish reason, tokens and latency; never content, which the ledger
// does not hold. The summary shows the explanation for a trial that missed its outcome. The source
// ID the scripted model chooses differs per trial and input kind, so nothing can know it but through
// the task's records, and it is an identifier as the prompt states the rule.
func TestPhaseDiagnosisAndTheScriptedSourceID(t *testing.T) {
	for want, call := range map[string]map[string]any{
		"#7 model succeeded finish=stop in=5800 out=40 21000ms": {"seq": float64(7), "kind": "model", "state": "succeeded", "finish_reason": "stop",
			"input_tokens": float64(5800), "output_tokens": float64(40), "latency_ms": float64(21000)},
		"#8 tool rfq.get failed NOT_FOUND":            {"seq": float64(8), "kind": "tool", "tool_command": "rfq.get", "state": "failed", "code": "NOT_FOUND"},
		"#9 model unavailable TOOL_UNAVAILABLE retry": {"seq": float64(9), "kind": "model", "state": "unavailable", "code": "TOOL_UNAVAILABLE", "retry": true},
		"#10 tool order.prepare waiting WAITING_APPROVAL": {"seq": float64(10), "kind": "tool", "tool_command": "order.prepare", "state": "waiting",
			"code": "WAITING_APPROVAL"},
		"#11 tool source.register succeeded": {"seq": float64(11), "kind": "tool", "tool_command": "source.register", "state": "succeeded"},
	} {
		if got := describeCall(call); got != want {
			t.Errorf("describeCall = %q, want %q", got, want)
		}
	}
	phase := PhaseRecord{Name: "rfq", ModelCalls: 5, ToolCalls: 10, Timeout: "20m0s",
		Diagnosis: "no write: the phase ended with the task waiting_input after 5 model calls and 10 tool calls, and committed no action",
		LastCalls: []string{"#14 tool rfq.get failed NOT_FOUND", "#15 model succeeded finish=stop in=5800 out=40 21000ms"}}
	failed := &TrialRecord{Family: "normal", Trial: 1, TrialID: "normal-1", Outcome: "waiting_input", Failure: failAgentPhase, Phases: []PhaseRecord{phase}}
	passed := &TrialRecord{Family: "normal", Trial: 2, TrialID: "normal-2", Pass: true, Outcome: "completed", Phases: []PhaseRecord{phase}}
	md := (&EvalRun{Trials: []*TrialRecord{failed, passed}, Environment: Environment{PhaseTimeout: "20m0s", AgentTrace: "off"}}).markdown()
	if !strings.Contains(md, "- normal-1: phase rfq: no write: the phase ended with the task waiting_input after 5 model calls and 10 tool calls, "+
		"and committed no action [last calls: #14 tool rfq.get failed NOT_FOUND; #15 model succeeded finish=stop in=5800 out=40 21000ms]") ||
		strings.Contains(md, "- normal-2:") || !strings.Contains(md, "- phase timeout: 20m0s\n- agent trace: off\n") {
		t.Fatalf("%s", md)
	}
	identifier := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)
	seen := map[string]bool{}
	for _, kind := range []string{kindText, kindXLSX, kindPDF} {
		for n := 1; n <= 5; n++ {
			id := (&trial{n: n, in: inquiry{kind: kind}}).agentSourceID()
			if seen[id] || !identifier.MatchString(id) || id == fileID {
				t.Fatalf("scripted source ID %q", id)
			}
			seen[id] = true
		}
	}
	// What a trial learned of the agent's own objects wins over the scripted names; without it the
	// scripted names stand (a person doing the agent's part by hand uses them).
	tr := &trial{}
	if tr.order() != orderID || tr.response() != responseID {
		t.Fatal(tr.order(), tr.response())
	}
	tr.learnedOrder, tr.learnedResp = "so-2026-17", "resp:a"
	if tr.order() != "so-2026-17" || tr.response() != "resp:a" {
		t.Fatal(tr.order(), tr.response())
	}
}

// BC-AT27's synthetic inputs (decision E-6): each generated inquiry places every field it gives at
// the locator the harness quotes, in the notation of baseline 14.2 (text line:col, PDF page:line,
// XLSX Sheet!A1), and is the same bytes every time (the dataset is versioned, not drawn).
func TestInquiriesPlaceEveryFieldAtItsLocator(t *testing.T) {
	for _, kind := range []string{kindText, kindXLSX, kindPDF} {
		in, err := makeInquiry(kind, "inquiry", brcRows(), []string{"extra line"})
		if err != nil {
			t.Fatal(kind, err)
		}
		again, err := makeInquiry(kind, "inquiry", brcRows(), []string{"extra line"})
		if err != nil || !bytes.Equal(in.data, again.data) {
			t.Fatalf("%s: not deterministic (%v)", kind, err)
		}
		if len(in.at) != len(brcLines) {
			t.Fatalf("%s: %d lines placed", kind, len(in.at))
		}
		for _, l := range brcLines {
			for _, f := range rowFields {
				at, ok := in.at[l.id][f]
				if !ok || at.Quote == "" {
					t.Fatalf("%s: %s %s not placed", kind, l.id, f)
				}
				switch kind {
				case kindText:
					checkText(t, in.data, at)
				case kindXLSX:
					checkCell(t, in.data, at)
				case kindPDF:
					if !strings.HasPrefix(at.Locator, "1:") || !bytes.Contains(in.data, []byte(pdfEscape(at.Quote))) {
						t.Fatalf("pdf: %+v", at)
					}
				}
			}
		}
	}
}

func checkText(t *testing.T, data []byte, at fieldAt) {
	t.Helper()
	ln, col, ok := strings.Cut(at.Locator, ":")
	n, err1 := strconv.Atoi(ln)
	c, err2 := strconv.Atoi(col)
	if !ok || err1 != nil || err2 != nil {
		t.Fatalf("text locator %q", at.Locator)
	}
	line := strings.Split(string(data), "\n")[n-1]
	runes := []rune(line)
	if c-1+utf8.RuneCountInString(at.Quote) > len(runes) || string(runes[c-1:c-1+utf8.RuneCountInString(at.Quote)]) != at.Quote {
		t.Fatalf("text %s: %q is not at %q", at.Locator, at.Quote, line)
	}
}

func checkCell(t *testing.T, data []byte, at fieldAt) {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var sheet string
	for _, f := range zr.File {
		if f.Name == "xl/worksheets/sheet1.xml" {
			r, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(r)
			_ = r.Close()
			sheet = string(raw)
		}
	}
	ref := strings.TrimPrefix(at.Locator, sheetName+"!")
	if ref == at.Locator {
		t.Fatalf("xlsx locator %q names another sheet", at.Locator)
	}
	_, cell, ok := strings.Cut(sheet, `<c r="`+ref+`"`)
	if !ok {
		t.Fatalf("xlsx: no cell %s", ref)
	}
	cell, _, _ = strings.Cut(cell, "</c>")
	if !strings.Contains(cell, ">"+xmlEscape(at.Quote)+"<") {
		t.Fatalf("xlsx %s: %q not in %q", ref, at.Quote, cell)
	}
}

// The reviewer's reading of a located form (S4 B4's brant-extract/1, escapes \\ \t \n \r).
func TestLocatedFormReading(t *testing.T) {
	form := "brant-extract/1\nkind\ttext\nsegment\t5:1\tST01.red.S | knitted\\tT | 24\nsegment\t1:3\tA\\\\B\nnote\thidden_row\tInquiry!5:5\n" +
		"segment\tInquiry!C5\t24\n"
	s := locatedSegments(form)
	if len(s) != 3 || s["5:1"] != "ST01.red.S | knitted\tT | 24" || s["1:3"] != `A\B` {
		t.Fatalf("%q", s)
	}
	if !standsAt(s, kindText, "5:1", "ST01.red.S") || !standsAt(s, kindText, "5:26", "24") || standsAt(s, kindText, "5:2", "ST01") ||
		standsAt(s, kindText, "9:1", "x") || standsAt(s, kindText, "5", "S") {
		t.Fatal("text")
	}
	if !standsAt(s, kindXLSX, "Inquiry!C5", "24") || standsAt(s, kindXLSX, "Inquiry!C6", "24") {
		t.Fatal("cell")
	}
}

// AN01's inquiry leaves the unit out of every row: the agent's draft cannot place it.
func TestAN01InquiryLeavesTheUnitsOut(t *testing.T) {
	in, err := makeInquiry(kindText, "inquiry", an01Rows(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range brcLines {
		if _, ok := in.at[l.id][fieldUOM]; ok {
			t.Fatalf("%s has a unit", l.id)
		}
	}
	if in.at["L3"][fieldSKU].Quote != "ST01.blue" {
		t.Fatalf("L3 %+v", in.at["L3"])
	}
}

// The feedback family's inquiry (S4 G7a): the unit "PCS" stands in every row, once in the rows of
// L2 to L4 and twice in the row of L1, whose item carries it too. An agent that places every field
// at its row's start names, for each unit, a row the gateway can settle, except L1's: the owner's
// resolution stays exercised, for one line.
func TestFeedbackInquiryHoldsTheUnitTwiceInOneRow(t *testing.T) {
	in, err := makeInquiry(kindText, "inquiry", feedbackRows(), nil)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Split(string(in.data), "\n")
	claimed := in.atRowStart()
	for i, l := range brcLines {
		row, _, _ := strings.Cut(in.at[l.id][fieldUOM].Locator, ":")
		n, _ := strconv.Atoi(row)
		want := 1
		if i == 0 {
			want = 2
		}
		if got := strings.Count(text[n-1], "PCS"); got != want || claimed.at[l.id][fieldUOM].Locator != row+":1" {
			t.Errorf("%s: row %d holds PCS %d times, want %d; the agent names %q", l.id, n, got, want, claimed.at[l.id][fieldUOM].Locator)
		}
	}
	if in.at["L1"][fieldItem].Quote != feedbackItem || in.at["L2"][fieldItem].Quote != brcLines[1].item {
		t.Fatalf("%+v", in.at)
	}
	// Nothing else of the file changes: the other families' rows are BRC-001's.
	if rows := brcRows(); rows[0].item == feedbackItem || rows[1] != feedbackRows()[1] {
		t.Fatalf("%+v", rows)
	}
}

// The feedback family's agent writes every place of its second line, the line's own included, with
// the located form's record word, as the live model did on 2026-11-19 ("segment 6:1"); the other
// three lines name no place of their own and write plain places. The second line's unit is one the
// gateway can settle (TestFeedbackInquiryHoldsTheUnitTwiceInOneRow: its row holds the words once).
func TestFeedbackAgentWritesOneLinesPlacesWithTheRecordWord(t *testing.T) {
	in, err := makeInquiry(kindText, "inquiry", feedbackRows(), nil)
	if err != nil {
		t.Fatal(err)
	}
	lines := saveRFQ("rfq.x", "src.x", in.atRowStart(), feedbackLines()).args["Lines"].([]any)
	if len(lines) != len(brcLines) {
		t.Fatalf("%d lines", len(lines))
	}
	for i, l := range lines {
		m := l.(map[string]any)
		own, named := m["Locator"].(string)
		var places []string
		for _, x := range m["Locators"].([]any) {
			places = append(places, x.(map[string]any)["Locator"].(string))
		}
		start := strconv.Itoa(5+i) + ":1"
		wantOwn, wantPlace := "", start
		if i == 1 {
			wantOwn, wantPlace = recordWord+" "+start, recordWord+" "+start
		}
		if named != (wantOwn != "") || own != wantOwn || !slices.Equal(places, []string{wantPlace, wantPlace, wantPlace, wantPlace}) {
			t.Errorf("%s: own place %q (%v), places %q; want %q and %q", brcLines[i].id, own, named, places, wantOwn, wantPlace)
		}
	}
}

// Baseline 19.1: every trial is kept; the family reports first success and repeat consistency from
// all of them, and the totals count every trial, failed ones included.
func TestRegistryKeepsEveryTrial(t *testing.T) {
	f := &family{name: "normal", design: []string{"BRC-001"}, goal: "g", expected: "e"}
	ok := func(n int) *TrialRecord {
		return &TrialRecord{Family: "normal", Trial: n, Pass: true, Outcome: "completed/ai_with_required_review", Lifecycle: "completed",
			GoalCompleted: true, Assertions: []Assertion{{Name: "a", Pass: true}}, Cost: CostRecord{ModelCalls: 11, ToolCalls: 8, Cost: "0.00"},
			Phases: []PhaseRecord{{Name: "rfq", ModelCalls: 2, ToolCalls: 2}}}
	}
	r := &EvalRun{Trials: []*TrialRecord{ok(1), ok(2), ok(3)}}
	r.summarize([]*family{f})
	s := r.Families[0]
	if s.Trials != 3 || s.Passed != 3 || !s.FirstTrialPassed || !s.Consistent || s.ModelCalls != 33 || r.Totals.Trials != 3 {
		t.Fatalf("%+v %+v", s, r.Totals)
	}
	failed := ok(1)
	failed.Pass, failed.Outcome, failed.Failure = false, "waiting_input", failTimeout
	failed.Assertions[0].Pass = false
	failed.Cost.Cost = "0.25"
	r = &EvalRun{Trials: []*TrialRecord{failed, ok(2)}}
	r.summarize([]*family{f})
	s = r.Families[0]
	if s.Trials != 2 || s.Passed != 1 || s.FirstTrialPassed || s.Consistent || s.Failures[failTimeout] != 1 || s.Cost != "0.25" ||
		r.Totals.Cost != "0.25" || len(s.Signatures) != 2 {
		t.Fatalf("%+v", s)
	}
	if md := r.markdown(); !strings.Contains(md, "| normal |") || !strings.Contains(md, "BAD a") {
		t.Fatalf("%s", md)
	}
}

func TestMoneyAndOutcomes(t *testing.T) {
	for in, want := range map[string]int64{"0.00": 0, "3.00": 300, "0.1": 10, "12.345": 1234, "x": 0, "": 0} {
		if got := minorOf(in); got != want {
			t.Errorf("minorOf(%q) = %d, want %d", in, got, want)
		}
	}
	if formatMinor(1234) != "12.34" || formatMinor(5) != "0.05" {
		t.Fatal(formatMinor(1234), formatMinor(5))
	}
	r := &TrialRecord{Lifecycle: "paused", Authority: map[string]any{"pause_cause": "budget_exhausted"}}
	if outcomeOf(r) != "paused/budget_exhausted" {
		t.Fatal(outcomeOf(r))
	}
	// Baseline 19.1 自动化水平: a take-over, extra revisions and the required reviews only are apart.
	steps := []HumanStep{{Command: "quote.freeze", Required: true, Outcome: "ok"}}
	if automationOf(&TrialRecord{HumanSteps: steps}) != "required_review_only" {
		t.Fatal("required")
	}
	if automationOf(&TrialRecord{HumanSteps: steps, Corrections: []map[string]any{{"kind": "changed"}}}) != "extra_human_work" {
		t.Fatal("correction")
	}
	if automationOf(&TrialRecord{HumanSteps: append(steps, HumanStep{Command: "task.take_control", Outcome: "ok"})}) != "human_takeover" {
		t.Fatal("take-over")
	}
}

// The scripted owner settles what a person would (S4 G3, decision E-7 a): a field the draft leaves
// pending or states otherwise than the truth gets the true value, and a field INQ holds ambiguous
// between several places gets the place the truth knows — only when it is one of the places INQ
// kept. Every field so settled is an owner resolution, synthetic. What the truth cannot repair is
// an error: a line missing, a line the truth does not have, a true place INQ did not keep.
func TestTheScriptedOwnerSettlesWhatAPersonWould(t *testing.T) {
	in, err := makeInquiry(kindText, "inquiry", brcRows(), nil)
	if err != nil {
		t.Fatal(err)
	}
	truth := brcTruth()
	// The draft of the live trial: every value right, the unit placed at the four rows it stands in.
	draft := func() map[string]any {
		var lines []any
		for _, l := range brcLines {
			locs := []any{map[string]any{"field": fieldSKU, "locator": in.at[l.id][fieldSKU].Locator, "quote": l.sku}}
			for _, o := range brcLines {
				locs = append(locs, map[string]any{"field": fieldUOM, "locator": in.at[o.id][fieldUOM].Locator, "quote": "PCS"})
			}
			lines = append(lines, map[string]any{"line_id": l.id, "sku_id": l.sku, "quantity": l.qty, "uom": "PCS", "requested_item": l.item,
				"locators": locs, "pending": []any{map[string]any{"field": fieldUOM, "reason": "ambiguous"}}})
		}
		return map[string]any{"lines": lines}
	}
	line := func(d map[string]any, i int) map[string]any { return d["lines"].([]any)[i].(map[string]any) }
	verified, res, err := ownerVerification(draft(), truth, in)
	if err != nil || len(verified) != 4 || len(res) != 4 {
		t.Fatal(verified, res, err)
	}
	for i, l := range brcLines {
		place := in.at[l.id][fieldUOM].Locator
		want := OwnerResolution{LineID: l.id, Field: fieldUOM, Kind: resolvedPlace, PendingReason: "ambiguous", From: "PCS", To: "PCS",
			Locator: place, Synthetic: true}
		fields := verified[i].(map[string]any)["fields"].([]any)
		if res[i] != want || len(fields) != 1 || fields[0].(map[string]any)["locator"] != place || fields[0].(map[string]any)["value"] != "PCS" {
			t.Fatalf("line %s: %+v, sent %v", l.id, res[i], fields)
		}
	}
	// A draft as the truth has it: every line verified as it stands, nothing settled.
	clean := draft()
	for i := range brcLines {
		delete(line(clean, i), "pending")
	}
	if verified, res, err = ownerVerification(clean, truth, in); err != nil || len(res) != 0 || len(verified) != 4 ||
		len(verified[0].(map[string]any)) != 1 {
		t.Fatal(verified, res, err)
	}
	// A pending field with one place or none, and a value that differs: the value is stated, no place.
	other := draft()
	line(other, 0)["pending"] = []any{map[string]any{"field": fieldUOM, "reason": "missing"}}
	line(other, 0)["uom"] = ""
	line(other, 1)["quantity"], line(other, 1)["pending"] = "63", nil
	line(other, 2)["pending"] = []any{map[string]any{"field": fieldSKU, "reason": "ambiguous", "candidates": []any{"ST01.blue.S", "ST01.blue.M"}},
		map[string]any{"field": fieldItem, "reason": "noted"}}
	line(other, 3)["pending"] = nil
	if _, res, err = ownerVerification(other, truth, in); err != nil || len(res) != 4 {
		t.Fatal(res, err)
	}
	for i, want := range []OwnerResolution{
		{LineID: "L1", Field: fieldUOM, Kind: resolvedValue, PendingReason: "missing", To: "PCS", Synthetic: true},
		{LineID: "L2", Field: fieldQty, Kind: resolvedCorrected, From: "63", To: "36", Synthetic: true},
		{LineID: "L3", Field: fieldSKU, Kind: resolvedValue, PendingReason: "ambiguous", From: "ST01.blue.S", To: "ST01.blue.S", Synthetic: true},
		{LineID: "L3", Field: fieldItem, Kind: resolvedValue, PendingReason: "noted", From: brcLines[2].item, To: brcLines[2].item, Synthetic: true},
	} {
		if res[i] != want {
			t.Errorf("resolution %d: %+v, want %+v", i, res[i], want)
		}
	}
	// What the truth cannot repair.
	short := draft()
	short["lines"] = short["lines"].([]any)[:3]
	extra := draft()
	line(extra, 3)["line_id"] = "L9"
	cut := draft()
	line(cut, 0)["locators"] = line(cut, 0)["locators"].([]any)[2:] // INQ kept other places, not L1's own
	for name, d := range map[string]map[string]any{"a line missing": short, "a line the truth does not have": extra, "the true place not kept": cut} {
		if _, _, err := ownerVerification(d, truth, in); err == nil {
			t.Errorf("%s: repaired", name)
		}
	}
	// An agent that places every field at its row's start quotes the file's words at the wrong
	// place, in the text and the XLSX notation.
	claimed := in.atRowStart()
	if claimed.at["L1"][fieldUOM] != (fieldAt{Locator: "5:1", Quote: "PCS"}) || claimed.at["L4"][fieldQty] != (fieldAt{Locator: "8:1", Quote: "48"}) ||
		in.at["L1"][fieldUOM].Locator == "5:1" {
		t.Fatalf("%+v", claimed.at)
	}
	sheet, err := makeInquiry(kindXLSX, "inquiry", brcRows(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := sheet.atRowStart().at["L2"][fieldUOM].Locator; got != sheetName+"!A6" {
		t.Fatalf("xlsx row start %q", got)
	}
	// Counted in the registry, and extra human work whatever INQ's correction kind.
	rec := &TrialRecord{Family: "feedback", Trial: 1, Pass: true, OwnerResolutions: res, Corrections: []map[string]any{{"kind": "verified"}},
		HumanSteps: []HumanStep{{Command: "rfq.confirm_scope", Required: true, Outcome: "ok"}}}
	if automationOf(rec) != "extra_human_work" || automationOf(&TrialRecord{Corrections: rec.Corrections, HumanSteps: rec.HumanSteps}) != "required_review_only" {
		t.Fatal("automation level of a trial with owner resolutions")
	}
	run := &EvalRun{Trials: []*TrialRecord{rec}}
	run.summarize([]*family{{name: "feedback"}})
	if run.Families[0].OwnerResolutions != 4 || run.Totals.OwnerResolutions != 4 || !strings.Contains(rec.signature(), "resolutions=4") ||
		!strings.Contains(run.markdown(), "settled by the scripted owner (synthetic, not the AI's work): L1 uom value_stated") {
		t.Fatalf("%+v\n%s", run.Totals, run.markdown())
	}
}

// The fixed set is fixed before the run: every family names its design IDs, its goal, the task's
// goal code, its expected outcome, a flow and a grader; names are unique.
func TestTheFixedSet(t *testing.T) {
	names := familyNames()
	if len(names) != 14 || names[0] != "normal" || names[len(names)-1] != "feedback" {
		t.Fatal(names)
	}
	seen := map[string]bool{}
	for _, f := range fixedSet {
		if f.run == nil || f.grade == nil || f.expected == "" || f.goal == "" || len(f.design) == 0 ||
			(f.goalCode != "quote_ready" && f.goalCode != "order_active") ||
			(f.applicability != applicable && f.applicability != scriptedOnly) ||
			(f.applicability == scriptedOnly) != (f.scriptedBecause != "") {
			t.Fatalf("%+v", f)
		}
		if seen[f.name] {
			t.Fatal("duplicate", f.name)
		}
		seen[f.name] = true
	}
	if got, err := selectFamilies("normal,an09"); err != nil || !slices.Equal(got, []string{"normal", "an09"}) {
		t.Fatal(got, err)
	}
	if _, err := selectFamilies("an02"); err == nil {
		t.Fatal("an02 is not in the fixed set")
	}
}

func TestFlags(t *testing.T) {
	t.Setenv("IDP_BRANT_POSTGRES_DSN", "")
	t.Setenv("IDP_BRANT_MIGRATOR_DSN", "")
	t.Setenv("CI", "")
	base := []string{"-dsn", "postgres://a", "-migrator-dsn", "postgres://m"}
	cfg, err := parseFlags(base)
	if err != nil || cfg.suite != suiteScripted || cfg.trials != 5 || len(cfg.families) != 14 {
		t.Fatal(cfg, err)
	}
	// S4 G2: the scripted suite's phase timeout is 2 minutes unless -phase-timeout says otherwise (the
	// live suite's default is 25 minutes, for a local model that answers in 6–30 s per call under a
	// budget of 40 calls); the agent trace is off unless asked for, and never on under CI=true.
	if cfg.phaseTimeout != 2*time.Minute || cfg.trace || livePhaseTimeout != 25*time.Minute {
		t.Fatal(cfg.phaseTimeout, cfg.trace)
	}
	if cfg, err = parseFlags(append(base, "-phase-timeout", "7m", "-trace", "on")); err != nil || cfg.phaseTimeout != 7*time.Minute || !cfg.trace {
		t.Fatal(cfg, err)
	}
	if r := newEvalRun(cfg); r.Environment.PhaseTimeout != "7m0s" || !strings.Contains(r.StopCondition, "7m0s") ||
		!strings.Contains(r.Environment.AgentTrace, "not evidence") || !strings.HasPrefix(r.Environment.AgentTrace, "on:") {
		t.Fatalf("%+v", r.Environment)
	}
	if cfg, err = parseFlags(append(base, "-trace", "off")); err != nil || cfg.trace || newEvalRun(cfg).Environment.AgentTrace != "off" {
		t.Fatal(cfg, err)
	}
	if _, err = parseFlags(append(base, "-trace", "yes")); err == nil {
		t.Fatal("-trace yes")
	}
	if _, err = parseFlags(append(base, "-phase-timeout", "-1s")); err == nil {
		t.Fatal("a negative phase timeout")
	}
	t.Setenv("CI", "true")
	if _, err = parseFlags(append(base, "-trace", "on")); err == nil || !strings.Contains(err.Error(), "CI=true") {
		t.Fatalf("the trace under CI=true: %v", err)
	}
	if cfg, err = parseFlags(base); err != nil || cfg.trace {
		t.Fatal(cfg, err)
	}
	t.Setenv("CI", "")
	if _, err = parseFlags([]string{"-trials", "1"}); err == nil {
		t.Fatal("no DSN")
	}
	if _, err = parseFlags(append(base, "-suite", "live")); err == nil && !liveBuilt {
		t.Fatal("the live suite needs the build tag")
	}
	if _, err = parseFlags(append(base, "-suite", "other")); err == nil {
		t.Fatal("unknown suite")
	}
}
