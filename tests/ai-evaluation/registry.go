package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// EvalRun is the registry entry of one evaluation run (product baseline 19.1: EvalRun 登记
// dataset_version、真实／合成标记、客户／会话分组、输入 profile、目标、真值及评审人、模型与完整执行版本、
// trial 数、预算、截止条件、评分器、失败分类及运行环境; 所有试次保留). Every trial is kept, failed and
// timed-out ones included; nothing is selected.
type EvalRun struct {
	RunID  string `json:"run_id"`
	RunTag string `json:"run_tag"` // in every tenant ID and idempotency key of the run

	Tool            string          `json:"tool"`
	Suite           string          `json:"suite"`
	DatasetVersion  string          `json:"dataset_version"`
	Synthetic       bool            `json:"synthetic"`
	DataNote        string          `json:"data_note"`
	Grouping        string          `json:"grouping"`
	InputProfile    string          `json:"input_profile"`
	Goals           []string        `json:"goals"`
	Truth           string          `json:"truth"`
	Reviewer        string          `json:"reviewer"`
	Model           ModelInfo       `json:"model"`
	ExecutionBundle json.RawMessage `json:"execution_bundle,omitempty"`
	BundleRef       BundleRef       `json:"bundle_ref"`
	TrialsPerFamily int             `json:"trials_per_family"`
	Budget          Budget          `json:"budget"`
	StopCondition   string          `json:"stop_condition"`
	Graders         []string        `json:"graders"`
	FailureTaxonomy []string        `json:"failure_taxonomy"`
	Environment     Environment     `json:"environment"`
	Tenant          string          `json:"tenant"`
	StartedAt       string          `json:"started_at"`
	FinishedAt      string          `json:"finished_at"`
	StartupSeconds  float64         `json:"startup_seconds"` // migrate, brant-api, the relay and the bundle probe
	Probes          []Probe         `json:"probes"`
	Families        []FamilySummary `json:"families"`
	Totals          Totals          `json:"totals"`
	Trials          []*TrialRecord  `json:"trials"`
	NotExecuted     []string        `json:"not_executed"`
}

// ModelInfo is the model the run used, as the model-call ledger recorded it.
type ModelInfo struct {
	Kind          string   `json:"kind"` // scripted or openaicompat
	ProfileID     string   `json:"profile_id"`
	Provider      string   `json:"provider"`
	Requested     string   `json:"model_requested"`
	Returned      []string `json:"model_returned"`
	BaseURL       string   `json:"base_url,omitempty"`
	TokensNote    string   `json:"tokens_note"`
	CostNote      string   `json:"cost_note"`
	KeyFromEnvVar string   `json:"key_from_env_var,omitempty"`
}

// BundleRef names the ExecutionBundle every task of the run names (baseline 17.3).
type BundleRef struct {
	BundleID string `json:"bundle_id"`
	SHA256   string `json:"sha256"`
}

// Budget is the per-task budget of the run's tasks (decision E-4: 40 model calls, 3 retries, USD 3.00),
// unless a family sets its own (recorded on the trial).
type Budget struct {
	MaxModelCalls int    `json:"max_model_calls"`
	MaxRetries    int    `json:"max_retries"`
	MaxCost       string `json:"max_cost"`
	Currency      string `json:"currency"`
}

// Environment is where the run ran.
type Environment struct {
	Host         string `json:"host"`
	GoVersion    string `json:"go_version"`
	OS           string `json:"os"`
	CPUs         int    `json:"cpus"`
	Database     string `json:"database"`
	Migrate      string `json:"migrate"`
	Processes    string `json:"processes"`
	CI           bool   `json:"ci"`
	GitHead      string `json:"git_head,omitempty"`
	Parallel     int    `json:"parallel"`
	PhaseTimeout string `json:"phase_timeout"` // the timeout used: -phase-timeout, or the suite's default
	// AgentTrace says whether the agent workers wrote their local diagnostic trace (brant-worker
	// -agent-trace-dir; S4 G2) and where. A trace is a diagnostic file of synthetic trials — what
	// the model was told and what it sent, in clear — not evidence and not a result of the run.
	AgentTrace string  `json:"agent_trace"`
	Load1Start float64 `json:"load1_start"`
	Load1End   float64 `json:"load1_end"`
	Error      string  `json:"error,omitempty"`
}

// Probe is a one-off check of the run outside the families (a role-template gap, for instance).
type Probe struct {
	Name     string `json:"name"`
	Design   string `json:"design"`
	Expected string `json:"expected"`
	Got      string `json:"got"`
	Pass     bool   `json:"pass"`
	Note     string `json:"note,omitempty"`
}

// TrialRecord is one trial, kept whatever its outcome.
type TrialRecord struct {
	Family          string           `json:"family"`
	Trial           int              `json:"trial"`
	TrialID         string           `json:"trial_id"`
	Design          []string         `json:"design"`
	Goal            string           `json:"goal"`
	Input           InputRecord      `json:"input"`
	Expected        string           `json:"expected"`
	Outcome         string           `json:"outcome"`
	Pass            bool             `json:"pass"`
	NotExecuted     bool             `json:"not_executed,omitempty"` // the model service did not let the trial run; not a pass, not a failure
	Failure         string           `json:"failure,omitempty"`
	Error           string           `json:"error,omitempty"`
	TaskID          string           `json:"task_id"`
	Lifecycle       string           `json:"lifecycle"`
	CompletionActor string           `json:"completion_actor,omitempty"`
	GoalCompleted   bool             `json:"goal_completed"`
	Automation      string           `json:"automation"`
	Assertions      []Assertion      `json:"assertions"`
	Authority       map[string]any   `json:"authority"`
	Corrections     []map[string]any `json:"corrections"`
	// OwnerResolutions are the fields of the agent's RFQ draft the scripted owner settled from the
	// trial's fixed truth at the confirmation (S4 G3): a place named, a value stated or corrected.
	// Each is a person's work, synthetic, never the agent's; INQ's record of it is among Corrections.
	OwnerResolutions []OwnerResolution  `json:"owner_resolutions"`
	HumanSteps       []HumanStep        `json:"human_steps"`
	Refusals         []string           `json:"refusals,omitempty"`
	Cost             CostRecord         `json:"cost"`
	Phases           []PhaseRecord      `json:"phases"`
	Verifications    []map[string]any   `json:"verifications,omitempty"`
	Budget           *Budget            `json:"budget,omitempty"`
	Seconds          float64            `json:"seconds"`
	Notes            []string           `json:"notes,omitempty"`
	ModelFailure     *ModelCallFailure  `json:"model_failure,omitempty"` // the phase's last failed model call, live suite
	extra            map[string]float64 // per-trial values the family's grader reads
}

// InputRecord is the trial's inquiry file.
type InputRecord struct {
	Kind   string `json:"kind"`
	File   string `json:"file"`
	MIME   string `json:"mime"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// Assertion is one authority assertion of a trial: what the authoritative records say, read back
// over HTTP (TSK, TRD, INQ, brant_ai), never what the model said.
type Assertion struct {
	Name         string `json:"name"`
	Want         string `json:"want"`
	Got          string `json:"got"`
	Pass         bool   `json:"pass"`
	ScriptedOnly bool   `json:"scripted_only,omitempty"` // live: the expected value is the script's own turns; it does not fail the trial
}

// OwnerResolution is one field of the agent's RFQ draft that the scripted owner settled from the
// trial's fixed truth when confirming the RFQ, as a person who knows the inquiry would (decision
// E-7 a): Kind place_named — the field's words stand at several places of the source and the owner
// named the one the value was read at —, value_stated — the draft left the field pending and the
// owner stated it —, or value_corrected — the draft stated another value. It is always synthetic.
type OwnerResolution struct {
	LineID        string `json:"line_id"`
	Field         string `json:"field"`
	Kind          string `json:"kind"`
	PendingReason string `json:"pending_reason,omitempty"` // INQ's reason the field was pending for
	From          string `json:"from,omitempty"`           // the draft's value
	To            string `json:"to"`                       // the value the owner verified
	Locator       string `json:"locator,omitempty"`        // the place the owner named
	Synthetic     bool   `json:"synthetic"`
}

// HumanStep is a step the scripted human operator took. Every one is synthetic (decision E-7):
// it is a person's part of the task, never an AI completion.
type HumanStep struct {
	Actor     string `json:"actor"`
	Command   string `json:"command"`
	Route     string `json:"route"` // step (task-step route), plain (command route), tsk, ai
	Required  bool   `json:"required"`
	Synthetic bool   `json:"synthetic"`
	Outcome   string `json:"outcome"`
}

// CostRecord is what the model-call ledger (brant_ai.model_call) holds for the trial's task.
type CostRecord struct {
	ModelCalls   int      `json:"model_calls"`
	ToolCalls    int      `json:"tool_calls"`
	Retries      int      `json:"retries"`
	Failed       int      `json:"failed_calls"`
	Unavailable  int      `json:"unavailable_calls"`
	InputTokens  int64    `json:"input_tokens"`
	OutputTokens int64    `json:"output_tokens"`
	Cost         string   `json:"cost"`
	Currency     string   `json:"currency"`
	Profiles     []string `json:"profiles"`
	Returned     []string `json:"model_returned"`
}

// PhaseRecord is one agent phase (one brant-worker -agent process).
//
// Writes are the task's actions that committed during the phase. A phase that failed — it timed
// out, its worker ended — or that asked the model and committed nothing records why (Diagnosis) and
// the last calls of the model-call ledger with their states and codes (LastCalls), so a failed live
// trial can be classified from the registry: the model never wrote, a tool kept refusing, the
// model service did not answer. Trace is the phase's trace directory under the trial's, when the
// agent trace is on.
type PhaseRecord struct {
	Name          string            `json:"name"`
	Script        string            `json:"script,omitempty"`
	ScriptSteps   int               `json:"script_steps"`
	ModelCalls    int               `json:"model_calls"`
	ToolCalls     int               `json:"tool_calls"`
	Writes        int               `json:"writes"`
	Lifecycle     string            `json:"lifecycle_after"`
	Killed        bool              `json:"killed,omitempty"`
	Seconds       float64           `json:"seconds"`
	StartedAt     string            `json:"started_at"`
	Timeout       string            `json:"timeout"`
	Error         string            `json:"error,omitempty"`
	Diagnosis     string            `json:"diagnosis,omitempty"`
	LastCalls     []string          `json:"last_calls,omitempty"`
	Trace         string            `json:"trace,omitempty"`
	WorkerLogTail string            `json:"worker_log_tail,omitempty"`
	ModelFailure  *ModelCallFailure `json:"model_failure,omitempty"`
}

// ModelCallFailure is the last failed model call of a phase, from the worker log: the runner's
// class, the HTTP status (0 when the service gave no answer) and, for an HTTP answer, the
// service's error type and code. Never the message, the URL or the key.
type ModelCallFailure struct {
	Phase      string `json:"phase,omitempty"`
	Class      string `json:"class"`
	HTTPStatus int    `json:"http_status"`
	ErrorType  string `json:"error_type,omitempty"`
	ErrorCode  string `json:"error_code,omitempty"`
	Transport  string `json:"transport,omitempty"`
}

// FamilySummary is a family's trials together: the fixed-set metrics of baseline 19.1 (goal
// completion over every planned trial, failures, timeouts, extra take-overs and safe stops apart),
// first success and repeat consistency.
type FamilySummary struct {
	Family           string         `json:"family"`
	Design           []string       `json:"design"`
	Goal             string         `json:"goal"`
	Expected         string         `json:"expected"`
	Trials           int            `json:"trials"`
	Passed           int            `json:"passed"`
	GoalCompleted    int            `json:"goal_completed"`
	AICompleted      int            `json:"ai_completed"` // completed with ai_with_required_review; a manual completion is not the AI's (AN08)
	Outcomes         map[string]int `json:"outcomes"`
	Failures         map[string]int `json:"failures,omitempty"`
	FirstTrialPassed bool           `json:"first_trial_passed"`
	Consistent       bool           `json:"consistent"`
	Signatures       []string       `json:"signatures"`
	ModelCalls       int            `json:"model_calls"`
	ToolCalls        int            `json:"tool_calls"`
	Tokens           int64          `json:"tokens"`
	Cost             string         `json:"cost"`
	HumanCorrections int            `json:"human_corrections"`
	OwnerResolutions int            `json:"owner_resolutions"` // fields of the agent's RFQ drafts the scripted owner settled; among the corrections
	HumanSteps       int            `json:"human_steps"`
	MeanSeconds      float64        `json:"mean_seconds"`
	NotExecuted      int            `json:"not_executed,omitempty"`
	Applicability    string         `json:"applicability,omitempty"` // not_applicable: a scripted-only family was not run
	Why              string         `json:"why,omitempty"`
}

// Totals are the run's totals over every trial.
type Totals struct {
	Trials           int     `json:"trials"`
	Passed           int     `json:"passed"`
	GoalCompleted    int     `json:"goal_completed"`
	AICompleted      int     `json:"ai_completed"`
	ModelCalls       int     `json:"model_calls"`
	ToolCalls        int     `json:"tool_calls"`
	Tokens           int64   `json:"tokens"`
	Cost             string  `json:"cost"`
	HumanCorrections int     `json:"human_corrections"`
	OwnerResolutions int     `json:"owner_resolutions"`
	HumanSteps       int     `json:"human_steps"`
	Seconds          float64 `json:"seconds"`
	NotExecuted      int     `json:"not_executed,omitempty"`
	NotApplicable    int     `json:"not_applicable,omitempty"`
}

// summarize computes the family summaries and totals from the trials.
func (r *EvalRun) summarize(families []*family) {
	r.Families = nil
	r.Totals = Totals{}
	var totalCost int64
	for _, f := range families {
		s := FamilySummary{Family: f.name, Design: f.design, Goal: f.goal, Expected: f.expected, Outcomes: map[string]int{},
			Failures: map[string]int{}}
		var seconds float64
		var cost int64
		sigs := map[string]bool{}
		for _, t := range r.Trials {
			if t.Family != f.name {
				continue
			}
			s.Trials++
			switch {
			case t.NotExecuted:
				s.NotExecuted++
			case t.Pass:
				s.Passed++
			}
			if !t.NotExecuted && t.GoalCompleted {
				s.GoalCompleted++
				if t.CompletionActor == "ai_with_required_review" {
					s.AICompleted++
				}
			}
			s.Outcomes[t.Outcome]++
			if !t.NotExecuted && t.Failure != "" {
				s.Failures[t.Failure]++
			}
			if t.Trial == 1 && !t.NotExecuted {
				s.FirstTrialPassed = t.Pass
			}
			sig := t.signature()
			if !sigs[sig] {
				sigs[sig] = true
				s.Signatures = append(s.Signatures, sig)
			}
			for _, m := range t.Cost.Returned {
				if !slices.Contains(r.Model.Returned, m) {
					r.Model.Returned = append(r.Model.Returned, m)
				}
			}
			s.ModelCalls += t.Cost.ModelCalls
			s.ToolCalls += t.Cost.ToolCalls
			s.Tokens += t.Cost.InputTokens + t.Cost.OutputTokens
			cost += minorOf(t.Cost.Cost)
			s.HumanCorrections += len(t.Corrections)
			s.OwnerResolutions += len(t.OwnerResolutions)
			s.HumanSteps += len(t.HumanSteps)
			seconds += t.Seconds
		}
		s.Consistent = len(s.Signatures) <= 1
		if s.Trials > 0 {
			s.MeanSeconds = round(seconds/float64(s.Trials), 2)
		}
		if len(s.Failures) == 0 {
			s.Failures = nil
		}
		if r.Suite == suiteLive && f.applicability == scriptedOnly {
			s.Applicability = "not_applicable"
			s.Why = f.scriptedBecause
		}
		s.Cost = formatMinor(cost)
		totalCost += cost
		r.Families = append(r.Families, s)
		r.Totals.Trials += s.Trials
		r.Totals.Passed += s.Passed
		r.Totals.GoalCompleted += s.GoalCompleted
		r.Totals.AICompleted += s.AICompleted
		r.Totals.ModelCalls += s.ModelCalls
		r.Totals.ToolCalls += s.ToolCalls
		r.Totals.Tokens += s.Tokens
		r.Totals.HumanCorrections += s.HumanCorrections
		r.Totals.OwnerResolutions += s.OwnerResolutions
		r.Totals.HumanSteps += s.HumanSteps
		r.Totals.NotExecuted += s.NotExecuted
		if s.Applicability == "not_applicable" {
			r.Totals.NotApplicable++
		}
		r.Totals.Seconds = round(r.Totals.Seconds+seconds, 2)
	}
	r.Totals.Cost = formatMinor(totalCost)
}

// signature is what repeat consistency compares between trials of a family: outcome, lifecycle,
// completion actor, the assertions' results, the phases' model and tool calls, and the corrections.
func (t *TrialRecord) signature() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s/%s/%s", t.Outcome, t.Lifecycle, t.CompletionActor)
	for _, a := range t.Assertions {
		fmt.Fprintf(&b, ";%s=%v", a.Name, a.Pass)
	}
	for _, p := range t.Phases {
		fmt.Fprintf(&b, ";%s:%d/%d", p.Name, p.ModelCalls, p.ToolCalls)
	}
	fmt.Fprintf(&b, ";corrections=%d;resolutions=%d", len(t.Corrections), len(t.OwnerResolutions))
	return b.String()
}

// minorOf reads a decimal cost "0.00" as minor units (two decimals); anything else is 0.
func minorOf(s string) int64 {
	whole, frac, _ := strings.Cut(s, ".")
	var n int64
	for _, c := range whole + (frac + "00")[:2] {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int64(c-'0')
	}
	return n
}

func formatMinor(n int64) string { return fmt.Sprintf("%d.%02d", n/100, n%100) }

func round(v float64, places int) float64 {
	p := math.Pow10(places)
	return math.Round(v*p) / p
}

// write stores the registry as report.json and summary.md in dir.
func (r *EvalRun) write(dir string) error {
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(dir, "report.json"), raw, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "summary.md"), []byte(r.markdown()), 0o644)
}

// markdown is the human-readable summary of the run.
func (r *EvalRun) markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Q01 evaluation run %s (%s suite)\n\n", r.RunID, r.Suite)
	fmt.Fprintf(&b, "- dataset %s, synthetic: %v (%s)\n- model: %s profile %s, returned %v; %s; %s\n", r.DatasetVersion, r.Synthetic,
		r.DataNote, r.Model.Kind, r.Model.ProfileID, r.Model.Returned, r.Model.TokensNote, r.Model.CostNote)
	fmt.Fprintf(&b, "- execution bundle %s sha256 %s\n- trials per family %d, budget per task %d model calls, %d retries, %s %s\n",
		r.BundleRef.BundleID, r.BundleRef.SHA256, r.TrialsPerFamily, r.Budget.MaxModelCalls, r.Budget.MaxRetries, r.Budget.MaxCost, r.Budget.Currency)
	fmt.Fprintf(&b, "- reviewer: %s\n- environment: %s, %s, %d CPUs, %s; %s\n", r.Reviewer, r.Environment.OS, r.Environment.GoVersion,
		r.Environment.CPUs, r.Environment.Database, r.Environment.Processes)
	fmt.Fprintf(&b, "- phase timeout: %s\n- agent trace: %s\n\n", r.Environment.PhaseTimeout, r.Environment.AgentTrace)
	b.WriteString("| family | design | goal | expected | trials | passed | goal completed (AI) | outcomes | first | consistent | model calls | tool calls | tokens | cost | human corrections (owner resolutions) | human steps | mean s |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, f := range r.Families {
		var outcomes []string
		for k, v := range f.Outcomes {
			outcomes = append(outcomes, fmt.Sprintf("%s %d", k, v))
		}
		sort.Strings(outcomes)
		outcomeText := strings.Join(outcomes, ", ")
		first, consistent := fmt.Sprint(f.FirstTrialPassed), fmt.Sprint(f.Consistent)
		if f.Applicability == "not_applicable" {
			outcomeText = "not applicable"
			first, consistent = "—", "—"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %d | %d | %d (%d) | %s | %s | %s | %d | %d | %d | %s | %d (%d) | %d | %.1f |\n", f.Family,
			strings.Join(f.Design, ", "), f.Goal, f.Expected, f.Trials, f.Passed, f.GoalCompleted, f.AICompleted, outcomeText,
			first, consistent, f.ModelCalls, f.ToolCalls, f.Tokens, f.Cost, f.HumanCorrections, f.OwnerResolutions, f.HumanSteps,
			f.MeanSeconds)
	}
	t := r.Totals
	fmt.Fprintf(&b, "\nTotals: %d trials, %d passed, %d goals completed (%d by the AI with the required reviews), %d model calls, %d tool calls, %d tokens, cost %s, %d human corrections (synthetic; %d of them fields of the agent's RFQ drafts that the scripted owner settled from the truth: a person's work, not the AI's), %d human steps (synthetic), %.1f s.\n",
		t.Trials, t.Passed, t.GoalCompleted, t.AICompleted, t.ModelCalls, t.ToolCalls, t.Tokens, t.Cost, t.HumanCorrections, t.OwnerResolutions,
		t.HumanSteps, t.Seconds)
	if t.NotExecuted > 0 {
		fmt.Fprintf(&b, "%d not executed: the model service did not answer or refused the request.\n", t.NotExecuted)
	}
	if t.NotApplicable > 0 {
		fmt.Fprintf(&b, "%d not applicable on a live model: a scripted-only family was not run, and is not a pass and not a failure.\n", t.NotApplicable)
	}
	if len(r.Probes) > 0 {
		b.WriteString("\n## Probes\n\n")
		for _, p := range r.Probes {
			fmt.Fprintf(&b, "- %s (%s): expected %s, got %s — %v. %s\n", p.Name, p.Design, p.Expected, p.Got, p.Pass, p.Note)
		}
	}
	b.WriteString("\n## Trials\n\n| trial | input | outcome | pass | lifecycle | completion actor | automation | model/tool calls | corrections | failure |\n|---|---|---|---|---|---|---|---|---|---|\n")
	for _, tr := range r.Trials {
		fmt.Fprintf(&b, "| %s | %s | %s | %v | %s | %s | %s | %d/%d | %d | %s |\n", tr.TrialID, tr.Input.Kind, tr.Outcome, tr.Pass, tr.Lifecycle,
			tr.CompletionActor, tr.Automation, tr.Cost.ModelCalls, tr.Cost.ToolCalls, len(tr.Corrections), strings.TrimSpace(tr.Failure+" "+tr.Error))
	}
	b.WriteString("\n## Assertions missed, refusals and notes per trial\n\n")
	for _, tr := range r.Trials {
		var lines []string
		if !tr.NotExecuted {
			for _, a := range tr.Assertions {
				switch {
				case a.ScriptedOnly:
					lines = append(lines, fmt.Sprintf("not applicable (scripted turns) %s: want %q, got %q", a.Name, a.Want, a.Got))
				case !a.Pass:
					lines = append(lines, fmt.Sprintf("BAD %s: want %q, got %q", a.Name, a.Want, a.Got))
				}
			}
		}
		if len(tr.Refusals) > 0 {
			lines = append(lines, "refusals: "+strings.Join(tr.Refusals, "; "))
		}
		if len(tr.OwnerResolutions) > 0 {
			var settled []string
			for _, o := range tr.OwnerResolutions {
				s := o.LineID + " " + o.Field + " " + o.Kind
				if o.Locator != "" {
					s += " " + o.Locator
				}
				settled = append(settled, s)
			}
			lines = append(lines, "settled by the scripted owner (synthetic, not the AI's work): "+strings.Join(settled, "; "))
		}
		// Why an agent phase of a trial that missed its outcome stopped (report.json has it for
		// every phase that asked the model and committed nothing).
		for _, p := range tr.Phases {
			if !tr.Pass && p.Diagnosis != "" {
				lines = append(lines, fmt.Sprintf("phase %s: %s [last calls: %s]", p.Name, p.Diagnosis, strings.Join(p.LastCalls, "; ")))
			}
		}
		lines = append(lines, tr.Notes...)
		if len(lines) > 0 {
			fmt.Fprintf(&b, "- %s: %s\n", tr.TrialID, strings.Join(lines, " | "))
		}
	}
	b.WriteString("\n## Not executed\n\n")
	for _, n := range r.NotExecuted {
		fmt.Fprintf(&b, "- %s\n", n)
	}
	return b.String()
}
