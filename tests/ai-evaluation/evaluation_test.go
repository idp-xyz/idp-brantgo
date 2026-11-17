package main

import (
	"archive/zip"
	"bytes"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

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

// The fixed set is fixed before the run: every family names its design IDs, its goal, the task's
// goal code, its expected outcome, a flow and a grader; names are unique.
func TestTheFixedSet(t *testing.T) {
	names := familyNames()
	if len(names) != 12 || names[0] != "normal" {
		t.Fatal(names)
	}
	seen := map[string]bool{}
	for _, f := range fixedSet {
		if f.run == nil || f.grade == nil || f.expected == "" || f.goal == "" || len(f.design) == 0 ||
			(f.goalCode != "quote_ready" && f.goalCode != "order_active") {
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
	base := []string{"-dsn", "postgres://a", "-migrator-dsn", "postgres://m"}
	cfg, err := parseFlags(base)
	if err != nil || cfg.suite != suiteScripted || cfg.trials != 5 || len(cfg.families) != 12 {
		t.Fatal(cfg, err)
	}
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
