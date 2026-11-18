//go:build integration

package bootstrap_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/ai/adapters/model"
	"go.idp.xyz/idp-brant/internal/ai/capability"
	"go.idp.xyz/idp-brant/internal/ai/execution"
	"go.idp.xyz/idp-brant/internal/ai/executionevidence"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	tskapi "go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// What an agent learns from a refused call and from the next turn's facts, on the composed runner
// and PostgreSQL 16 (S4 unit G3, WBS S4-03; the single live trials of 2026-11-18,
// bin/ai-eval/live-20261118T041505Z: thirteen refused quote.save_draft calls, 40 model calls, no
// draft).
//
// The model here is a learner: it answers from the request alone — the tools on offer, the facts
// and this turn's tool results — and keeps nothing between calls, as a model has nothing between
// turns (decision E-6). It makes the live model's mistakes: its quote draft has no UnitPrice,
// PriceBasisQuantity or DiscountRatio, and a delivery of Kind "date" without Milestone or TimeZone.
// A refusal teaches it one member: it gives that member what the refusal says it needs, and when a
// refusal says nothing but the code and the path it guesses, as the live model did (a member that
// was absent is added; a Milestone becomes "delivery"). The references of a draft it has from the
// fixture, as the evaluation's scripted model has them from the trial's truth; the unit price is
// that truth's 8.00 (no price source exists, and none is built here). Like the live model it reads
// before it drafts, three model calls a turn, and it copies the delivery members it is told to
// correct from costing_context.get.

// g3Feedback is what of the platform's feedback reaches the learner. The control runs withhold
// what the platform did not give before this unit: detail is a refusal's member, needs, allowed
// values and reason (false: code and path, the tool result of 3185097); facts is the facts'
// refusals, arrivals and clock (false: the facts of 3185097).
type g3Feedback struct{ detail, facts bool }

type g3Note struct {
	OK      *bool    `json:"ok"`
	Tool    string   `json:"tool"`
	Code    string   `json:"code"`
	Path    string   `json:"path"`
	Member  string   `json:"member"`
	Missing bool     `json:"missing"`
	Reason  string   `json:"reason"`
	Needs   string   `json:"needs"`
	Allowed []string `json:"allowed"`
	Times   int      `json:"times"`
}

type g3Facts struct {
	TaskID   string        `json:"task_id"`
	Now      string        `json:"now"`
	Target   tskapi.Target `json:"target"`
	Arrivals []struct {
		Kind    string `json:"kind"`
		ID      string `json:"id"`
		Version int64  `json:"version"`
	} `json:"arrivals"`
	Actions []struct {
		Command string `json:"command"`
		State   string `json:"state"`
	} `json:"actions"`
	Refusals []g3Note `json:"refusals"`
}

func (f g3Facts) committed(command string) bool {
	for _, a := range f.Actions {
		if a.Command == command && a.State == tskapi.ActionCommitted {
			return true
		}
	}
	return false
}

// g3Result is a tool call of this turn with what came back.
type g3Result struct {
	name    string
	content string
}

type g3Learner struct {
	sees  g3Feedback
	draft func() map[string]any // the fixture's quote draft as tool arguments (QuoteData)
	price string

	mu      sync.Mutex
	facts   []string // the facts of every request, as sent
	refused []string // every refused tool result, as it came back
}

func newG3Learner(w *agentWorld, sees g3Feedback) *g3Learner {
	return &g3Learner{sees: sees, price: "8.00", draft: func() map[string]any {
		var d map[string]any
		w.must(json.Unmarshal(toolArgs(w.data()), &d))
		return d
	}}
}

func (l *g3Learner) Complete(ctx context.Context, req model.Request) (model.Response, error) {
	if err := ctx.Err(); err != nil {
		return model.Response{}, err
	}
	if err := req.Validate(); err != nil {
		return model.Response{}, err
	}
	if len(req.Messages) < 2 || req.Messages[0].Content != execution.Q01Prompt {
		return model.Response{}, fmt.Errorf("the request does not open with the Q01 prompt and the facts")
	}
	var f g3Facts
	if err := json.Unmarshal([]byte(req.Messages[1].Content), &f); err != nil {
		return model.Response{}, err
	}
	// This turn so far: the calls the learner made and what came back.
	steps, asked := 0, map[string]string{}
	var results []g3Result
	for _, m := range req.Messages[2:] {
		switch m.Role {
		case model.RoleAssistant:
			steps++
			for _, c := range m.ToolCalls {
				asked[c.ID] = c.Name
			}
		case model.RoleTool:
			results = append(results, g3Result{name: asked[m.ToolCallID], content: m.Content})
		}
	}
	// For the test's own assertions (never for the learner's answer): the facts of each turn and
	// the refused results that are new in this request, the tool messages at its end.
	l.mu.Lock()
	if steps == 0 {
		l.facts = append(l.facts, req.Messages[1].Content)
	}
	for _, r := range results[len(results)-toolTail(req.Messages):] {
		if strings.HasPrefix(r.content, `{"ok":false`) {
			l.refused = append(l.refused, r.name+" "+r.content)
		}
	}
	l.mu.Unlock()

	offered := func(name string) bool {
		return slices.ContainsFunc(req.Tools, func(t model.Tool) bool { return t.Name == name })
	}
	var reply model.Message
	if offered(trdapi.CommandRecordResponse) {
		reply = l.respond(f, steps, results)
	} else {
		reply = l.quote(f, steps, results)
	}
	reply.Role = model.RoleAssistant
	finish := "stop"
	for i := range reply.ToolCalls {
		reply.ToolCalls[i].ID = fmt.Sprintf("m%d.%d", len(req.Messages), i+1)
		finish = "tool_calls"
	}
	return model.Response{Model: "scripted-q01-v1", Message: reply, FinishReason: finish, Usage: model.Usage{InputTokens: 1000, OutputTokens: 100}}, nil
}

// toolTail counts the tool messages at the end of a conversation.
func toolTail(ms []model.Message) int {
	n := 0
	for i := len(ms) - 1; i >= 0 && ms[i].Role == model.RoleTool; i-- {
		n++
	}
	return n
}

func g3Call(name string, args map[string]any) model.ToolCall {
	return model.ToolCall{Name: name, Arguments: toolArgs(args)}
}

// quote is the learner's quote step: read, then draft with what it has learned.
func (l *g3Learner) quote(f g3Facts, steps int, results []g3Result) model.Message {
	if f.committed(trdapi.CommandSaveDraft) {
		return model.Message{Content: "The quote draft is saved; people freeze, review and activate it."}
	}
	var costing map[string]any // the delivery costing_context.get returned in this turn
	var notes []g3Note
	if l.sees.facts {
		for _, n := range f.Refusals {
			if n.Tool == trdapi.CommandSaveDraft {
				notes = append(notes, n)
			}
		}
	}
	for _, r := range results {
		switch r.name {
		case capability.ReadContext:
			var doc struct {
				Result struct {
					Result struct {
						Delivery map[string]any `json:"delivery"`
					} `json:"result"`
				} `json:"result"`
			}
			if json.Unmarshal([]byte(r.content), &doc) == nil {
				costing = doc.Result.Result.Delivery
			}
		case trdapi.CommandSaveDraft:
			var n g3Note
			if json.Unmarshal([]byte(r.content), &n) != nil || n.OK == nil {
				continue
			}
			if *n.OK {
				return model.Message{Content: "The quote draft is saved; people freeze, review and activate it."}
			}
			notes = append(notes, n)
		}
	}
	switch steps {
	case 0:
		return model.Message{ToolCalls: []model.ToolCall{g3Call(capability.ReadRfq, map[string]any{"RfqID": f.Target.RfqID}),
			g3Call(capability.ReadSnapshots, map[string]any{"RfqID": f.Target.RfqID})}}
	case 1:
		return model.Message{ToolCalls: []model.ToolCall{g3Call(capability.ReadParty, map[string]any{"PartyID": f.Target.CustomerID}),
			g3Call(capability.ReadSku, map[string]any{"SkuID": "sku.1"}), g3Call(capability.ReadContext, map[string]any{"ContextID": "context.1"})}}
	case 2:
		var lookups []model.ToolCall
		for _, kind := range []string{"tax_application", "quote_document_template", "number_sequence", "company_profile"} {
			lookups = append(lookups, g3Call(capability.ReadConfig, map[string]any{"Kind": kind}))
		}
		return model.Message{ToolCalls: lookups}
	}
	// The draft as the live model first sent it, then every lesson.
	d := l.draft()
	line := d["Lines"].([]any)[0].(map[string]any)
	for _, member := range []string{"UnitPrice", "PriceBasisQuantity", "DiscountRatio"} {
		delete(line, member)
	}
	d["Delivery"] = map[string]any{"Kind": "date", "Date": "2027-01-31", "PlaceRef": d["Delivery"].(map[string]any)["PlaceRef"]}
	// Every draft states an expiry, as the live model's did: since S4 G6 the gateway refuses a draft
	// a person could not freeze (a draft without ExpiresAt is DRAFT_NOT_READY there, which would end
	// the controls at a member this test is not about). A learner that sees the facts counts it from
	// the platform's clock, the others from their own idea of today.
	now := time.Now()
	if t, err := kernel.ParseTimestamp(f.Now); l.sees.facts && err == nil {
		now = t
	}
	d["ExpiresAt"] = kernel.FormatTimestamp(now.Add(72 * time.Hour))
	args := map[string]any{"QuoteID": f.Target.QuoteID, "ExpectedVersion": 0, "Data": d}
	for _, n := range notes {
		l.learn(args, n, costing)
	}
	return model.Message{ToolCalls: []model.ToolCall{{Name: trdapi.CommandSaveDraft, Arguments: toolArgs(args)}}}
}

// learn corrects the member a refusal is about.
func (l *g3Learner) learn(args map[string]any, n g3Note, costing map[string]any) {
	if !l.sees.detail {
		n = g3Note{Code: n.Code, Path: n.Path} // what a refusal carried before this unit
	}
	member := n.Member
	if member == "" {
		member = g3GuessMember(n.Path) // the live model read the member off the path too
	}
	if member == "" {
		return
	}
	name := member[strings.LastIndexByte(member, '.')+1:]
	known := map[string]string{"UnitPrice": l.price, "PriceBasisQuantity": "1", "DiscountRatio": "0"}
	var value string
	switch {
	case len(n.Allowed) > 0:
		// One of the values the refusal allows: the costing's, which the quote must repeat.
		value = n.Allowed[0]
		if v, ok := costing[name].(string); ok && slices.Contains(n.Allowed, v) {
			value = v
		}
	case n.Needs != "":
		// Told what the member needs: the learner's own value for it, or the costing's.
		value = known[name]
		if v, ok := costing[name].(string); ok && value == "" {
			value = v
		}
	default:
		// Code and path only: the live model's guesses.
		value = known[name]
		switch name {
		case "Milestone":
			value = "delivery"
		case "Kind":
			value = "date"
		case "TimeZone":
			value = "UTC+8"
		}
	}
	if value != "" {
		g3Set(args, strings.Split(member, "."), value)
	}
}

// g3GuessMember turns a rejection's path into the argument it seems to name:
// /args/data/lines/0/unit_price is Data.Lines[0].UnitPrice.
func g3GuessMember(path string) string {
	rest, ok := strings.CutPrefix(path, "/args/")
	if !ok {
		return ""
	}
	var out []string
	for _, seg := range strings.Split(rest, "/") {
		if _, err := strconv.Atoi(seg); err == nil && len(out) > 0 {
			out[len(out)-1] += "[" + seg + "]"
			continue
		}
		var b strings.Builder
		for _, word := range strings.Split(seg, "_") {
			if word != "" {
				b.WriteString(strings.ToUpper(word[:1]) + word[1:])
			}
		}
		out = append(out, b.String())
	}
	return strings.Join(out, ".")
}

// g3Set sets the member tokens name in node, in every item of a list on the way.
func g3Set(node map[string]any, tokens []string, value any) {
	name, indexed := tokens[0], false
	if i := strings.IndexByte(name, '['); i >= 0 {
		name, indexed = name[:i], true
	}
	if len(tokens) == 1 {
		node[name] = value
		return
	}
	if indexed {
		items, _ := node[name].([]any)
		for _, item := range items {
			if m, ok := item.(map[string]any); ok {
				g3Set(m, tokens[1:], value)
			}
		}
		return
	}
	m, ok := node[name].(map[string]any)
	if !ok {
		m = map[string]any{}
		node[name] = m
	}
	g3Set(m, tokens[1:], value)
}

// respond is the learner's response step: it records the customer's reply the task was resumed
// with, which only the facts' arrivals name.
func (l *g3Learner) respond(f g3Facts, steps int, results []g3Result) model.Message {
	reply := ""
	if l.sees.facts {
		for _, a := range f.Arrivals {
			if a.Kind == tskapi.SourceEVD {
				reply = a.ID
			}
		}
	}
	switch {
	case f.committed(trdapi.CommandRecordResponse):
		return model.Message{Content: "The customer's response is recorded; a person verifies the acceptance."}
	case reply == "":
		return model.Message{Content: "The facts name no source of the customer's reply; I cannot record a response."}
	case steps == 0:
		return model.Message{ToolCalls: []model.ToolCall{g3Call(capability.ReadQuote, map[string]any{"QuoteID": f.Target.QuoteID})}}
	case steps > 1:
		return model.Message{Content: "The customer's response is recorded; a person verifies the acceptance."}
	}
	var doc struct {
		Result struct {
			Result struct {
				Active    int64 `json:"active_revision"`
				Revisions []struct {
					Revision int64  `json:"revision"`
					Hash     string `json:"business_hash"`
				} `json:"revisions"`
			} `json:"result"`
		} `json:"result"`
	}
	hash := ""
	if len(results) > 0 && json.Unmarshal([]byte(results[len(results)-1].content), &doc) == nil {
		for _, r := range doc.Result.Result.Revisions {
			if r.Revision == doc.Result.Result.Active {
				hash = r.Hash
			}
		}
	}
	return model.Message{ToolCalls: []model.ToolCall{g3Call(trdapi.CommandRecordResponse, map[string]any{"ResponseID": "response." + f.TaskID,
		"QuoteID": f.Target.QuoteID, "BusinessRevision": doc.Result.Result.Active, "BusinessHash": hash,
		"Classification": trdapi.ClassificationFullUnconditional, "Summary": "The customer accepts every line (synthetic).",
		"SourceRef": map[string]any{"SourceID": reply, "Locator": "body"}})}}
}

var g3QuoteTools = []string{trdapi.CommandSaveDraft, capability.ReadRfq, capability.ReadSnapshots, capability.ReadContext, capability.ReadConfig,
	capability.ReadSku, capability.ReadParty, capability.ReadQuote}

// g3Run has a learner that sees `sees` work on a quote_ready task until the task leaves running.
func g3Run(w *agentWorld, task, quote string, sees g3Feedback) (*g3Learner, []execution.TurnResult) {
	w.t.Helper()
	l := newG3Learner(w, sees)
	r := w.runner(l)
	w.aiTask(task, tskapi.GoalQuoteReady, tskapi.Target{RfqID: "rfq.1", QuoteID: quote, CustomerID: "customer.1"}, nil, bundleRef(r), g3QuoteTools...)
	var turns []execution.TurnResult
	for range 8 {
		res := w.turnOn(r, task)
		turns = append(turns, res)
		if res.Lifecycle != tskapi.StateRunning {
			break
		}
	}
	return l, turns
}

// g3Refusals returns the paths of a task's refused quote.save_draft calls, in order, and its
// model calls.
func g3Refusals(w *agentWorld, task string) (paths []string, modelCalls int) {
	for _, c := range w.ledger(task) {
		switch {
		case c.Kind == executionevidence.KindModel:
			modelCalls++
		case c.ToolCommand == trdapi.CommandSaveDraft && c.State == executionevidence.StateFailed:
			paths = append(paths, c.Code+" "+c.RefusalPath)
		}
	}
	return paths, modelCalls
}

// The quote step converges with what a refusal and the next turn's facts now say, and with either
// withheld it does not (unit items 2, 3 and 5).
//
// With both: the first turn is three reads and five drafts, each refused about the next member
// (UnitPrice, PriceBasisQuantity, DiscountRatio, Milestone, TimeZone) and each telling what that
// member needs and, for the milestone, the values allowed; the second turn starts from the facts,
// which list the five refusals, so its first draft carries all five corrections and is refused only
// about the delivery's Kind; the next draft commits. Fourteen model calls, seven drafts.
//
// Without the detail (code and path only, with or without the refusals in the facts) the learner
// never finds a milestone the contract takes; with the detail but without the refusals in the
// facts every turn learns the same five members again and never reaches the sixth. Each control
// ends as the live trial did: the budget's 40 model calls used, the task paused, no draft.
func TestAgentLearnsAQuoteDraftFromRefusalsAndFacts(t *testing.T) {
	w := newAgentWorld(t, false)
	const draft = trdapi.CommandSaveDraft
	p := "SCHEMA_INVALID /args/data/"
	firstFive := []string{p + "lines/0/unit_price", p + "lines/0/price_basis_quantity", p + "lines/0/discount_ratio", p + "delivery/milestone",
		p + "delivery/time_zone"}

	l, turns := g3Run(w, "task.learn", "quote.learn", g3Feedback{detail: true, facts: true})
	paths, modelCalls := g3Refusals(w, "task.learn")
	if len(turns) != 2 || turns[0].ModelCalls != 8 || len(turns[0].Refusals) != 5 || turns[0].Lifecycle != tskapi.StateRunning ||
		turns[1].ModelCalls != 6 || len(turns[1].Refusals) != 1 || len(turns[1].Receipts) != 1 || !turns[1].Verified ||
		turns[1].Lifecycle != tskapi.StateWaitingInput {
		t.Fatalf("the learner's turns %+v", turns)
	}
	if modelCalls != 14 || !slices.Equal(paths, append(slices.Clone(firstFive), p+"delivery/kind")) {
		t.Fatalf("%d model calls, refusals %v", modelCalls, paths)
	}
	// The draft is the fixture's: the learner's corrections were the right ones, and its expiry is
	// counted from the facts' clock.
	q := w.getQuote("quote.learn")
	want := w.data()
	got := q.Revisions[0].Data
	if q.WorkingRevision != 1 || q.Revisions[0].Status != trdapi.StatusWorking || len(got.Lines) != 1 || got.Lines[0].UnitPrice != "8.00" ||
		got.Lines[0].PriceBasisQuantity != "1" || got.Lines[0].DiscountRatio != "0" || *got.Delivery != *want.Delivery || got.ExpiresAt == "" {
		t.Fatalf("the committed draft %+v, delivery %+v", got.Lines, got.Delivery)
	}
	if v := w.taskView("task.learn"); v.Binding == nil || v.Binding.QuoteID != "quote.learn" || v.Binding.State != tskapi.BindingBound {
		t.Fatalf("the draft's root is not bound to the task: %+v", v.Binding)
	}
	// What a refused call answered (item 2), on the two refusals the live model never got past.
	for _, wantAnswer := range []string{
		draft + ` {"ok":false,"code":"SCHEMA_INVALID","path":"/args/data/lines/0/unit_price","member":"Data.Lines[0].UnitPrice","missing":true,` +
			`"needs":"required; decimal, up to 8 decimals; for PriceBasisQuantity units, before discount"}`,
		draft + ` {"ok":false,"code":"SCHEMA_INVALID","path":"/args/data/delivery/milestone","member":"Data.Delivery.Milestone","missing":true,` +
			`"needs":"required; one of the allowed values; what is due at PlaceRef","allowed":["goods_ready","handover","arrival"]}`,
		draft + ` {"ok":false,"code":"SCHEMA_INVALID","path":"/args/data/delivery/kind","member":"Data.Delivery.Kind",` +
			`"needs":"required; one of the allowed values; fixed: on Date; lead_time: Days after the event AnchorRef names","allowed":["fixed","lead_time"]}`,
	} {
		if !slices.Contains(l.refused, wantAnswer) {
			t.Errorf("no refused call answered %s\n got %s", wantAnswer, strings.Join(l.refused, "\n     "))
		}
	}
	// The second turn's facts (item 3): the five refusals with what each member needs, no rejected
	// action among the actions, the platform's clock.
	if len(l.facts) != 2 || strings.Contains(l.facts[0], `"refusals"`) || strings.Contains(l.facts[1], `"state":"rejected"`) {
		t.Fatalf("the facts of the two turns:\n%s", strings.Join(l.facts, "\n"))
	}
	var second g3Facts
	w.must(json.Unmarshal([]byte(l.facts[1]), &second))
	var told []string
	for _, n := range second.Refusals {
		told = append(told, n.Code+" "+n.Path)
		if n.Tool != draft || n.Member == "" || n.Needs == "" || n.Times != 1 {
			t.Errorf("a refusal in the facts: %+v", n)
		}
	}
	if !slices.Equal(told, firstFive) || !slices.Equal(second.Refusals[3].Allowed, []string{"goods_ready", "handover", "arrival"}) {
		t.Fatalf("the refusals in the second turn's facts: %s", l.facts[1])
	}
	if now, err := kernel.ParseTimestamp(second.Now); err != nil || time.Since(now) > time.Minute || time.Until(now) > time.Minute {
		t.Fatalf("the facts' clock %q: %v", second.Now, err)
	}
	t.Logf("with the refusal's detail and the facts' refusals: %d model calls, %d drafts, committed in turn %d", modelCalls, len(paths)+1, len(turns))
	t.Logf("a refused call answered: %s", l.refused[0])
	t.Logf("the second turn's facts: %s", l.facts[1])

	// The ledger keeps where a call was refused; the stored outcome never changes (ai 000004).
	for _, c := range w.ledger("task.learn") {
		if c.State != executionevidence.StateFailed && (c.RefusalPath != "" || c.RefusalReason != "") {
			t.Fatalf("a call that was not refused has a refusal: %+v", c)
		}
	}
	if err := w.exec("company.A", `UPDATE brant_ai.model_call SET refusal_reason = 'changed' WHERE task_id = 'task.learn' AND state = 'failed'`); err == nil {
		t.Fatal("the refusal of a settled call was changed")
	}

	// The controls: the same learner, shown less.
	milestone := p + "delivery/milestone"
	for _, c := range []struct {
		id, name string
		sees     g3Feedback
		turn1    []string // the refusals of the first turn
		later    []string // the refusals of every later turn
	}{
		{"ctl1", "code and path, no refusals in the facts (3185097)", g3Feedback{},
			append(slices.Clone(firstFive[:4]), milestone), append(slices.Clone(firstFive[:4]), milestone)},
		{"ctl2", "the refusal's detail, no refusals in the facts", g3Feedback{detail: true}, firstFive, firstFive},
		{"ctl3", "refusals in the facts, code and path only", g3Feedback{facts: true},
			append(slices.Clone(firstFive[:4]), milestone), slices.Repeat([]string{milestone}, 5)},
	} {
		id := c.id
		_, turns := g3Run(w, "task."+id, "quote."+id, c.sees)
		paths, modelCalls := g3Refusals(w, "task."+id)
		last := turns[len(turns)-1]
		wantPaths := slices.Clone(c.turn1)
		for range 4 {
			wantPaths = append(wantPaths, c.later...)
		}
		if len(turns) != 6 || modelCalls != 40 || last.Paused != tskapi.PauseBudgetExhausted || last.Lifecycle != tskapi.StatePaused ||
			!slices.Equal(paths, wantPaths) {
			t.Errorf("%s: %d turns, %d model calls, last %+v, refusals %v", c.name, len(turns), modelCalls, last, paths)
		}
		if _, err := w.c.TRD.GetQuote(w.clerk, "quote."+id); code(err) != kernel.CodeNotFound {
			t.Errorf("%s: a quote exists: %v", c.name, err)
		}
		t.Logf("%s: %d model calls, %d refused drafts, paused %s, no draft", c.name, modelCalls, len(paths), last.Paused)
	}
}

// The facts say what a task was resumed with (unit item 3): the platform resumes a waiting task on
// the customer's reply, TSK keeps that arrival on the history row of the resume, and the agent's
// next turn finds the source its quote.response.record cites. A learner that is not shown the
// arrivals has nothing to cite and records nothing.
func TestAgentFindsTheCustomersReplyAmongTheArrivals(t *testing.T) {
	w := newAgentWorld(t, true)
	for i, c := range []struct {
		sees g3Feedback
	}{{g3Feedback{detail: true, facts: true}}, {g3Feedback{detail: true}}} {
		quote, task, mail := fmt.Sprintf("quote.reply%d", i), fmt.Sprintf("task.reply%d", i), fmt.Sprintf("mail.g3.%d", i)
		a := w.activated(quote)
		w.source(w.clerk, mail, "合成客户邮件：确认接受报价 "+quote+" 全部条款与全部行。")
		l := newG3Learner(w, c.sees)
		r := w.runner(l)
		rc := w.aiTask(task, tskapi.GoalOrderActive, tskapi.Target{QuoteID: quote, CustomerID: "customer.1"}, nil, bundleRef(r),
			trdapi.CommandRecordResponse, capability.ReadQuote)
		wait, err := w.c.TSK.WaitTask(w.sys, tskapi.WaitTask{IdempotencyKey: w.key(), TaskID: task, ExpectedVersion: rc.RowVersion,
			Wait: tskapi.WaitInput, Reason: "等待客户回复", ResponsibleID: "clerk.ai", Items: []string{"客户对报价的回复"}})
		w.must(err)
		_, err = w.c.TSK.ResumeTask(w.sys, tskapi.ResumeTask{IdempotencyKey: w.key(), TaskID: task, ExpectedVersion: wait.RowVersion,
			Reason: "客户回复已到达", Arrival: &tskapi.ObjectRef{Kind: tskapi.SourceEVD, ID: mail}})
		w.must(err)
		// TSK's record of the arrival: the history row of the resume, with the reason given.
		v := w.taskView(task)
		h := v.History[len(v.History)-1]
		if h.Command != tskapi.CommandResume || h.Reason != "客户回复已到达" || h.Arrival == nil ||
			*h.Arrival != (tskapi.ObjectRef{Kind: tskapi.SourceEVD, ID: mail}) {
			t.Fatalf("the resume in the task's history: %+v", h)
		}
		res := w.turnOn(r, task)
		if !strings.Contains(l.facts[0], `"arrivals":[{"kind":"evd_source","id":"`+mail+`"}]`) {
			t.Fatalf("the facts after the resume: %s", l.facts[0])
		}
		resp, err := w.c.TRD.GetCustomerResponse(w.clerk, "response."+task)
		if c.sees.facts {
			if err != nil || resp.SourceRef.SourceID != mail || resp.BusinessHash != a.BusinessHash || resp.RecordedBy.ID != agentID ||
				res.ModelCalls != 3 || len(res.Receipts) != 1 || len(res.Refusals) != 0 {
				t.Fatalf("the recorded response %+v %v, turn %+v", resp, err, res)
			}
			t.Logf("the facts after the resume: %s", l.facts[0])
			continue
		}
		if code(err) != kernel.CodeNotFound || res.ModelCalls != 1 || len(res.Receipts) != 0 {
			t.Fatalf("without the arrivals a response was recorded: %+v %v, turn %+v", resp, err, res)
		}
	}
}

// g3KindSites collects the POL references below struct type t whose kind the api type states
// (contract tag kind=…): the member in the tool's argument names → the kind.
func g3KindSites(t reflect.Type, prefix string, out map[string]string) {
	for i := range t.NumField() {
		f := t.Field(i)
		rule, err := capability.RuleOf(f)
		if err != nil || !f.IsExported() {
			continue
		}
		name := f.Name
		if tagged, _, _ := strings.Cut(f.Tag.Get("json"), ","); tagged != "" {
			name = tagged
		}
		ft := f.Type
		for ft.Kind() == reflect.Pointer || ft.Kind() == reflect.Slice {
			if ft.Kind() == reflect.Slice {
				name += "[0]"
			}
			ft = ft.Elem()
		}
		switch {
		case rule.Kind != "" && ft == reflect.TypeFor[trdapi.ConfigRef]():
			out[prefix+name] = rule.Kind
		case ft.Kind() == reflect.Struct:
			g3KindSites(ft, prefix+name+".", out)
		}
	}
}

// The kind a tool schema states for a POL reference of a quote draft is the kind TRD takes there
// (unit item 1; the api command's Validate checks a reference's form, TRD's quote.save_draft its
// kind). For each of the fifteen references: a draft that names another kind there is refused
// REFERENCE_INVALID at that reference's kind, with TRD's static reason, and the path resolves to
// the member whose schema allows exactly the stated kind. With every kind as stated the draft is
// saved. (The capability package's TestRulesAreWhatValidateEnforces pins the same fifteen as the
// kinds Validate does not check.)
func TestQuoteDraftReferencesTakeTheKindsTheSchemaStates(t *testing.T) {
	q := newTrdWorld(t)
	sites := map[string]string{}
	g3KindSites(reflect.TypeFor[trdapi.SaveDraft](), "", sites)
	if len(sites) != 15 {
		t.Fatalf("POL references with a stated kind: %v", sites)
	}
	// A draft that has every reference: the fixture's, with a conversion, a charge, a sales FX rate,
	// legal terms and a lead-time delivery. Whether they exist is the freeze's question.
	d := q.data()
	d.Lines[0].ConversionRef = &trdapi.ConfigRef{Kind: "unit_conversion", ID: "conversion.x", Revision: 1}
	d.Charges = []trdapi.Charge{{ChargeID: "C1", FeeCategoryRef: trdapi.ConfigRef{Kind: "fee_category", ID: "fee.x", Revision: 1},
		ObligationKey: "freight", Amount: "10", Inclusion: trdapi.InclusionSeparate, TaxApplicationRef: tref(q.sale), Evidence: q.proof}}
	d.SalesFXRef = &trdapi.ConfigRef{Kind: "fx_rate", ID: "fx.x", Revision: 1}
	d.LegalTermsRef = &trdapi.ConfigRef{Kind: "legal_terms", ID: "legal.x", Revision: 1}
	d.Delivery = &trdapi.Delivery{Kind: trdapi.DeliveryLeadTime, AnchorRef: &trdapi.BusinessRef{Kind: trdapi.AnchorKind, ID: "order", Revision: 1},
		Days: 30, DayBasis: trdapi.DayBasisCalendar, CalendarRef: &trdapi.ConfigRef{Kind: "calendar", ID: "calendar.x", Revision: 1},
		Milestone: trdapi.MilestoneHandover, PlaceRef: dehamPlace, TimeZone: "Asia/Shanghai"}
	raw := toolArgs(trdapi.SaveDraft{QuoteID: "quote.kinds", Data: d})
	save := func(change func(args map[string]any)) (trdapi.Receipt, error) {
		var args map[string]any
		q.must(json.Unmarshal(raw, &args))
		change(args)
		edited, err := json.Marshal(args)
		q.must(err)
		var cmd trdapi.SaveDraft
		q.must(json.Unmarshal(edited, &cmd))
		cmd.IdempotencyKey = q.key()
		return q.c.TRD.SaveDraft(q.author, cmd)
	}
	reg, err := capability.Q01Registry(capability.Q01Targets{})
	q.must(err)
	tool, err := reg.Lookup("tenant.1", trdapi.CommandSaveDraft, "/command")
	q.must(err)
	for member, kind := range sites {
		_, err := save(func(args map[string]any) { g3Set(args, strings.Split(member+".Kind", "."), "zz_other") })
		rej, ok := kernel.AsRejection(err)
		if !ok || rej.Code != kernel.CodeReferenceInvalid {
			t.Errorf("%s of another kind: %v", member, err)
			continue
		}
		m, found := tool.Member(rej.Fields[0].Path)
		if !found || m.Path != member+".Kind" || !slices.Equal(m.Rule.Allowed(), []string{kind}) ||
			capability.StatedReason(rej.Code, rej.Fields[0].Reason) != "The reference is not of the kind its field takes" {
			t.Errorf("%s of another kind: refused at %s (%q), which is %q allowing %v; the schema states %s", member, rej.Fields[0].Path,
				rej.Fields[0].Reason, m.Path, m.Rule.Allowed(), kind)
		}
	}
	if r, err := save(func(map[string]any) {}); err != nil || r.RowVersion != 1 || r.Status != trdapi.StatusWorking {
		t.Fatalf("the draft with every kind as the schema states it: %+v %v", r, err)
	}
}

// A draft whose lines name no place of their own (the live trials of 2026-11-18,
// bin/ai-eval/live-20261118T100220Z and …100617Z normal-2): the model placed every field of every
// line where its words stand and left RfqLine.Locator out, and the owner's rfq.confirm_scope was
// DRAFT_NOT_READY at /args/lines/0/locator. By the owner's decision of that day the gateway's
// locating gives such a line the row its fields stand in, so the agent's draft is one a person can
// confirm as it stands. INQ is unchanged: the same lines saved without the gateway are refused.
func TestALineWithoutItsPlaceIsConfirmedAtTheRowOfItsFields(t *testing.T) {
	w := newAgentWorld(t, false)
	text := "SYNTHETIC INQUIRY G5 - not a real customer\nFrom: customer.1 (contact.1)\nCode | Item | Qty | Unit\n" +
		"sku.1 | synthetic shirt red | 120 | PCS\nsku.1 | synthetic shirt blue | 60 | PCS\n"
	w.rawFile(w.clerk, "file.g5", "inquiry.txt", evdapi.MimeText, []byte(text))
	// Each field at its own place in rows 4 and 5, as the live model placed them.
	fields := map[string][]inqapi.FieldLocator{
		"L1": {{Field: "sku_id", Locator: "4:1", Quote: "sku.1"}, {Field: "requested_item", Locator: "4:9", Quote: "synthetic shirt red"},
			{Field: "quantity", Locator: "4:31", Quote: "120"}, {Field: "uom", Locator: "4:37", Quote: "PCS"}},
		"L2": {{Field: "sku_id", Locator: "5:1", Quote: "sku.1"}, {Field: "requested_item", Locator: "5:9", Quote: "synthetic shirt blue"},
			{Field: "quantity", Locator: "5:32", Quote: "60"}, {Field: "uom", Locator: "5:37", Quote: "PCS"}},
	}
	line := func(id, item, qty string) map[string]any {
		var locators []map[string]string
		for _, x := range fields[id] {
			locators = append(locators, at(x.Field, x.Locator, x.Quote))
		}
		return map[string]any{"LineID": id, "SkuID": "sku.1", "RequestedItem": item, "Quantity": qty, "UOM": "PCS", "SourceID": "src.g5",
			"Locators": locators}
	}
	m := model.NewScripted("scripted-q01-v1", calls(registerExtractCall("c1", "src.g5", "file.g5")),
		calls(tc("c2", inqapi.CommandSaveRfq, map[string]any{"RfqID": "rfq.g5", "CustomerID": "customer.1", "ContactID": "contact.1",
			"SourceIDs": []string{"src.g5"}, "Lines": []map[string]any{line("L1", "synthetic shirt red", "120"),
				line("L2", "synthetic shirt blue", "60")}})), done("RFQ 草稿已保存。"))
	r := w.runner(m)
	w.sourcedTask("task.g5.rfq", tskapi.Target{RfqID: "rfq.g5", CustomerID: "customer.1"}, tskapi.SourceRef{Kind: tskapi.SourceFile, ID: "file.g5"},
		bundleRef(r), evdapi.CommandRegisterSource, inqapi.CommandSaveRfq)
	if res := w.turnOn(r, "task.g5.rfq"); len(res.Receipts) != 2 || len(res.Refusals) != 0 || m.Remaining() != 0 {
		t.Fatalf("the drafting turn %+v", res)
	}
	rfq, err := w.c.INQ.GetRfq(w.clerk, "rfq.g5")
	w.must(err)
	for i, row := range []string{"4:1", "5:1"} {
		if l := rfq.Lines[i]; l.Locator != row || len(l.Pending) != 0 || !slices.Equal(l.Locators, fields[l.LineID]) {
			t.Fatalf("line %d after the locating: place %q, pending %+v, locators %+v", i, l.Locator, l.Pending, l.Locators)
		}
	}
	// The owner verifies each line as it stands; nothing is left to settle.
	_, err = w.c.INQ.ConfirmRfqScope(w.clerk, inqapi.ConfirmRfqScope{IdempotencyKey: w.key(), RfqID: "rfq.g5", ExpectedVersion: rfq.RowVersion,
		EvidenceIDs: []string{"src.g5"}, VerifiedLines: []inqapi.LineVerification{{LineID: "L1"}, {LineID: "L2"}}})
	w.must(err)
	if rfq, err = w.c.INQ.GetRfq(w.clerk, "rfq.g5"); err != nil || rfq.Status != inqapi.StatusReady || len(rfq.Corrections) != 0 {
		t.Fatalf("after the confirmation: %s, corrections %+v, %v", rfq.Status, rfq.Corrections, err)
	}
	// The same lines saved by a person, past the gateway, keep no place and cannot be confirmed:
	// the rule is INQ's as before, and the place is the AI layer's candidate.
	var plain []inqapi.RfqLine
	for _, id := range []string{"L1", "L2"} {
		plain = append(plain, inqapi.RfqLine{LineID: id, SkuID: "sku.1", Quantity: "120", UOM: "PCS", SourceID: "src.g5", Locators: fields[id]})
	}
	saved, err := w.c.INQ.SaveRfq(w.clerk, inqapi.SaveRfq{IdempotencyKey: w.key(), RfqID: "rfq.g5.plain", CustomerID: "customer.1",
		ContactID: "contact.1", SourceIDs: []string{"src.g5"}, Lines: plain})
	w.must(err)
	_, err = w.c.INQ.ConfirmRfqScope(w.clerk, inqapi.ConfirmRfqScope{IdempotencyKey: w.key(), RfqID: "rfq.g5.plain", ExpectedVersion: saved.RowVersion,
		EvidenceIDs: []string{"src.g5"}, VerifiedLines: []inqapi.LineVerification{{LineID: "L1"}, {LineID: "L2"}}})
	if code(err) != kernel.CodeDraftNotReady || path(err) != "/args/lines/0/locator" {
		t.Fatalf("a draft without line places, confirmed past the gateway: %v", err)
	}
}

// Field locators at the row's start (the live trial of 2026-11-18, live-20261118T045207Z): the
// model quotes each field's own words and places all of them where the row begins. The gateway's
// locating moves a quote that stands at one place and keeps every place of one that stands at
// several — the unit, "PCS", stands in every row — with the field pending ambiguous. A confirmation
// that states the value and names no place is SOURCE_AMBIGUOUS, as the scripted owner's was; naming
// the place a person read the value at settles it, and INQ records it as the owner's correction.
func TestRowStartLocatorsNeedThePlaceAPersonNames(t *testing.T) {
	w := newAgentWorld(t, false)
	text := "SYNTHETIC INQUIRY G3 - not a real customer\nFrom: customer.1 (contact.1)\nCode | Item | Qty | Unit\n" +
		"sku.1 | synthetic shirt red | 120 | PCS\nsku.1 | synthetic shirt blue | 60 | PCS\n"
	w.rawFile(w.clerk, "file.g3", "inquiry.txt", evdapi.MimeText, []byte(text))
	line := func(id, row, item, qty string) map[string]any {
		return map[string]any{"LineID": id, "SkuID": "sku.1", "RequestedItem": item, "Quantity": qty, "UOM": "PCS", "SourceID": "src.g3",
			"Locator": row + ":1", "Locators": []map[string]string{at("requested_item", row+":1", item), at("sku_id", row+":1", "sku.1"),
				at("quantity", row+":1", qty), at("uom", row+":1", "PCS")}}
	}
	m := model.NewScripted("scripted-q01-v1", calls(registerExtractCall("c1", "src.g3", "file.g3")),
		calls(tc("c2", inqapi.CommandSaveRfq, map[string]any{"RfqID": "rfq.g3", "CustomerID": "customer.1", "ContactID": "contact.1",
			"SourceIDs": []string{"src.g3"}, "Lines": []map[string]any{line("L1", "4", "synthetic shirt red", "120"),
				line("L2", "5", "synthetic shirt blue", "60")}})), done("RFQ 草稿已保存。"))
	r := w.runner(m)
	w.sourcedTask("task.g3.rfq", tskapi.Target{RfqID: "rfq.g3", CustomerID: "customer.1"}, tskapi.SourceRef{Kind: tskapi.SourceFile, ID: "file.g3"},
		bundleRef(r), evdapi.CommandRegisterSource, inqapi.CommandSaveRfq)
	if res := w.turnOn(r, "task.g3.rfq"); len(res.Receipts) != 2 || len(res.Refusals) != 0 || m.Remaining() != 0 {
		t.Fatalf("the drafting turn %+v", res)
	}
	rfq, err := w.c.INQ.GetRfq(w.clerk, "rfq.g3")
	w.must(err)
	for i, row := range []string{"4", "5"} {
		l := rfq.Lines[i]
		var places []string
		for _, x := range l.Locators {
			if x.Field == inqapi.FieldUOM {
				places = append(places, x.Locator)
			}
		}
		if len(l.Pending) != 1 || l.Pending[0].Field != inqapi.FieldUOM || l.Pending[0].Reason != inqapi.PendingAmbiguous ||
			!slices.Equal(places, []string{"4:37", "5:37"}) || !slices.Contains(l.Locators, inqapi.FieldLocator{Field: inqapi.FieldSkuID,
			Locator: row + ":1", Quote: "sku.1"}) {
			t.Fatalf("line %d after the locating: pending %+v, locators %+v", i, l.Pending, l.Locators)
		}
	}
	confirm := func(fields func(row string) []inqapi.VerifiedField) error {
		_, err := w.c.INQ.ConfirmRfqScope(w.clerk, inqapi.ConfirmRfqScope{IdempotencyKey: w.key(), RfqID: "rfq.g3", ExpectedVersion: rfq.RowVersion,
			EvidenceIDs:   []string{"src.g3"},
			VerifiedLines: []inqapi.LineVerification{{LineID: "L1", Fields: fields("4")}, {LineID: "L2", Fields: fields("5")}}})
		return err
	}
	// Each line verified as it stands: the pending field stays.
	err = confirm(func(string) []inqapi.VerifiedField { return nil })
	if code(err) != inqapi.CodeSourceAmbiguous || path(err) != "/args/lines/0/uom" {
		t.Fatalf("a confirmation that settles nothing: %v", err)
	}
	// The value stated, no place named: what the scripted owner sent in the live trial.
	err = confirm(func(string) []inqapi.VerifiedField {
		return []inqapi.VerifiedField{{Field: inqapi.FieldUOM, Value: "PCS"}}
	})
	if rej, _ := kernel.AsRejection(err); code(err) != inqapi.CodeSourceAmbiguous || path(err) != "/args/verified_lines/0/fields/0/locator" ||
		capability.StatedReason(rej.Code, rej.Fields[0].Reason) != "Name the place verified" {
		t.Fatalf("a confirmation without the place: %v", err)
	}
	// The place named: the row's own.
	w.must(confirm(func(row string) []inqapi.VerifiedField {
		return []inqapi.VerifiedField{{Field: inqapi.FieldUOM, Value: "PCS", Locator: row + ":37"}}
	}))
	rfq, err = w.c.INQ.GetRfq(w.clerk, "rfq.g3")
	w.must(err)
	if rfq.Status != inqapi.StatusReady || len(rfq.Corrections) != 2 {
		t.Fatalf("after the confirmation: %s, corrections %+v", rfq.Status, rfq.Corrections)
	}
	for i, c := range rfq.Corrections {
		if c.Kind != inqapi.CorrectionVerified || c.Field != inqapi.FieldUOM || c.Locator != []string{"4:37", "5:37"}[i] ||
			c.Reason != inqapi.PendingAmbiguous || c.ProposedBy.ID != agentID || c.CorrectedBy.ID != "clerk.ai" || c.To != "PCS" {
			t.Fatalf("correction %d: %+v", i, c)
		}
	}
}
