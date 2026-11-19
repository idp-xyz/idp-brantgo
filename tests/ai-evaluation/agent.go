package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The scripted model (decision E-4/E-5: the only model in CI) answers the turns of a script file
// (brant-worker -agent -model scripted -model-script <file>; internal/ai/adapters/model ScriptStep).
// The harness plays the model's part from the trial's fixed truth: each step is what a model given
// the task's facts proposes. A tool call's arguments are the api command's fields under their Go
// names — the tool schema of internal/ai/capability (strictjson decodes them into the command) — and
// carry no empty string or null, which strict JSON refuses. Usage is scripted (synthetic tokens).

// scriptStep is one turn of the scripted model (the JSON of model.ScriptStep).
type scriptStep struct {
	Content     string     `json:"content,omitempty"`
	ToolCalls   []toolCall `json:"tool_calls,omitempty"`
	Usage       *usage     `json:"usage,omitempty"`
	Unavailable bool       `json:"unavailable,omitempty"`
}

type toolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type usage struct {
	InputTokens  int64 `json:"InputTokens"`
	OutputTokens int64 `json:"OutputTokens"`
}

// call is a tool call request; args are marshalled as they are (Go field names).
type call struct {
	name string
	args map[string]any
}

// turn is a model turn proposing calls.
func turn(calls ...call) scriptStep {
	s := scriptStep{Usage: &usage{InputTokens: 1800, OutputTokens: 160}}
	for i, c := range calls {
		raw, err := json.Marshal(c.args)
		if err != nil {
			panic(err)
		}
		s.ToolCalls = append(s.ToolCalls, toolCall{ID: "call_" + strconv.Itoa(i+1), Name: c.name, Arguments: raw})
	}
	return s
}

// done is a model turn without a tool call: the agent says it has nothing left to do, which only
// starts the goal's verification (AI-INV-004).
func done(text string) scriptStep {
	return scriptStep{Content: text, Usage: &usage{InputTokens: 1500, OutputTokens: 40}}
}

// unavailable is a turn the model service does not answer (TOOL_UNAVAILABLE, BC-AT26).
func unavailable() scriptStep { return scriptStep{Unavailable: true} }

// Tool calls of the Q01 capabilities (decision D-7) in the api commands' Go field names.

func registerExtract(source, file string) call {
	return call{"source.register", map[string]any{"SourceID": source, "SourceKind": "file_extract", "FileID": file}}
}

// recordWord is the word a record of the located form starts with ("segment<TAB>5:1<TAB>text",
// brant-extract/1). The live model of 2026-11-19 copied it in front of every place it wrote.
const recordWord = "segment"

// agentLine is an RFQ line candidate the agent proposes. noPlace leaves the line's own Locator out,
// as the live model did on 2026-11-18 (it placed every field and not the line): the gateway's
// locating then gives the line the row its fields stand in. withWord writes every place of the line,
// the line's own included, with the located form's record word and a space in front, as the live
// model did on 2026-11-19 ("segment 6:1"): the gateway reads each as the place of the extract's
// notation it names and INQ stores it without the word.
type agentLine struct {
	id, sku, item, qty, uom string
	pending                 []map[string]any
	noPlace                 bool
	withWord                bool
}

// saveRFQ is the agent's rfq.save_draft from the inquiry's extract: each field with a value quotes
// the file at its locator (B4: the gateway verifies the locators before the call is hashed).
func saveRFQ(rfq, source string, in inquiry, lines []agentLine) call {
	var out []any
	for _, l := range lines {
		m := map[string]any{"LineID": l.id, "SourceID": source, "Locator": "inquiry " + in.kind + " row of " + l.id}
		if at, ok := in.at[l.id][fieldItem]; ok {
			m["Locator"] = "inquiry " + in.kind + " " + at.Locator
			if l.withWord {
				m["Locator"] = recordWord + " " + at.Locator
			}
		}
		if l.noPlace {
			delete(m, "Locator")
		}
		var locs []any
		for _, f := range []struct{ field, key, value string }{{fieldSKU, "SkuID", l.sku}, {fieldItem, "RequestedItem", l.item},
			{fieldQty, "Quantity", l.qty}, {fieldUOM, "UOM", l.uom}} {
			if f.value == "" {
				continue
			}
			m[f.key] = f.value
			if at, ok := in.at[l.id][f.field]; ok {
				place := at.Locator
				if l.withWord {
					place = recordWord + " " + place
				}
				locs = append(locs, map[string]any{"Field": f.field, "Locator": place, "Quote": at.Quote})
			}
		}
		if len(locs) > 0 {
			m["Locators"] = locs
		}
		if len(l.pending) > 0 {
			m["Pending"] = l.pending
		}
		out = append(out, m)
	}
	return call{"rfq.save_draft", map[string]any{"RfqID": rfq, "ExpectedVersion": 0, "CustomerID": "customer.1", "ContactID": "contact.1",
		"SourceIDs": []string{source}, "Lines": out}}
}

// goRef turns a configuration or business reference of the HTTP shape {"kind","id","revision"} into
// the api's ConfigRef / BusinessRef {Kind, ID, Revision}.
func goRef(r map[string]any) map[string]any {
	return map[string]any{"Kind": r["kind"], "ID": r["id"], "Revision": r["revision"]}
}

// quoteData is the agent's quote draft (trdapi.QuoteData) on the trial's RFQ and cost snapshot at
// price per piece: BRC-001's four lines at 8.00 USD = 960.00.
func quoteData(w *world, rfq string, rfqVersion int64, snap costed, source, price string, expires time.Time) map[string]any {
	var lines []any
	for _, l := range brcLines {
		lines = append(lines, map[string]any{"LineID": l.id, "RfqLineID": l.id, "sku_ref": map[string]any{"SkuID": l.sku, "SpecRevision": 1},
			"Quantity": l.qty, "UnitRef": goRef(w.pcs), "UnitPrice": price, "PriceBasisQuantity": "1", "DiscountRatio": "0",
			"TaxApplicationRef": goRef(w.sale[l.sku]), "Evidence": []any{map[string]any{"SourceID": source, "Locator": "line " + l.id}}})
	}
	return map[string]any{
		"IssuerRef":           goRef(w.issuer),
		"CustomerRef":         map[string]any{"PartyID": "customer.1", "Revision": w.customerRevision},
		"ContactRef":          map[string]any{"ContactID": "contact.1", "RowVersion": 2},
		"TradingProfileRef":   map[string]any{"PartyID": "customer.1", "Revision": 1},
		"CustomerAddressRefs": []any{map[string]any{"PartyID": "customer.1", "PartyRevision": w.customerRevision, "AddressID": "hq"}},
		"RfqRef":              map[string]any{"RfqID": rfq, "RowVersion": rfqVersion},
		"CostSnapshotRef":     map[string]any{"SnapshotID": snap.snapshotID, "ResultHash": snap.resultHash},
		"Lines":               lines,
		"CurrencyRef":         goRef(w.usd),
		"PricingPolicyRef":    goRef(w.policy),
		"TradeTermsRef":       goRef(w.trade),
		"NamedPlaceRef":       goRef(w.place),
		"PaymentTermsRef":     goRef(w.payment),
		"Delivery": map[string]any{"Kind": "fixed", "Date": "2027-01-31", "Milestone": "handover", "PlaceRef": goRef(w.place),
			"TimeZone": "Asia/Shanghai"},
		"DocumentTemplateRef": goRef(w.template),
		"SequenceRef":         goRef(w.seq),
		"ExpiresAt":           stamp(expires),
		"CustomerNotes":       "客户可见说明：AI 评测合成样例，ST01 红/蓝 × S/M。",
		"Evidence":            []any{map[string]any{"SourceID": source, "Locator": "inquiry"}},
	}
}

// httpQuoteData is the same draft as a person sends it on the HTTP routes (snake_case members, as
// tests/load's order cycle).
func httpQuoteData(w *world, rfq string, rfqVersion int64, snap costed, source, price string, expires time.Time) map[string]any {
	var lines []any
	for _, l := range brcLines {
		lines = append(lines, map[string]any{"line_id": l.id, "rfq_line_id": l.id, "sku_ref": map[string]any{"sku_id": l.sku, "spec_revision": 1},
			"quantity": l.qty, "unit_ref": w.pcs, "unit_price": price, "price_basis_quantity": "1", "discount_ratio": "0",
			"tax_application_ref": w.sale[l.sku], "evidence": []any{map[string]any{"source_id": source, "locator": "line " + l.id}}})
	}
	return map[string]any{"issuer_ref": w.issuer,
		"customer_ref":          map[string]any{"party_id": "customer.1", "revision": w.customerRevision},
		"contact_ref":           map[string]any{"contact_id": "contact.1", "row_version": 2},
		"trading_profile_ref":   map[string]any{"party_id": "customer.1", "revision": 1},
		"customer_address_refs": []any{map[string]any{"party_id": "customer.1", "party_revision": w.customerRevision, "address_id": "hq"}},
		"rfq_ref":               map[string]any{"rfq_id": rfq, "row_version": rfqVersion},
		"cost_snapshot_ref":     map[string]any{"snapshot_id": snap.snapshotID, "result_hash": snap.resultHash},
		"lines":                 lines, "currency_ref": w.usd, "pricing_policy_ref": w.policy, "trade_terms_ref": w.trade,
		"named_place_ref": w.place, "payment_terms_ref": w.payment,
		"delivery": map[string]any{"kind": "fixed", "date": "2027-01-31", "milestone": "handover", "place_ref": w.place,
			"time_zone": "Asia/Shanghai"},
		"document_template_ref": w.template, "sequence_ref": w.seq, "expires_at": stamp(expires),
		"customer_notes": "客户可见说明：AI 评测合成样例，ST01 红/蓝 × S/M。",
		"evidence":       []any{map[string]any{"source_id": source, "locator": "inquiry"}}}
}

// readProposal is the agent's quote.price_proposal for the trial's RFQ, snapshot and pricing policy.
func readProposal(w *world, snap costed) call {
	return call{"quote.price_proposal", map[string]any{"RfqID": rfqID, "Currency": "USD",
		"CostSnapshotRef":  map[string]any{"SnapshotID": snap.snapshotID, "ResultHash": snap.resultHash},
		"PricingPolicyRef": goRef(w.policy)}}
}

func saveQuote(quote string, data map[string]any) call {
	return call{"quote.save_draft", map[string]any{"QuoteID": quote, "ExpectedVersion": 0, "Data": data}}
}

func reviseQuote(quote string, expected, base int64) call {
	return call{"quote.revise", map[string]any{"QuoteID": quote, "ExpectedVersion": expected, "BaseRevision": base}}
}

func replaceDraft(quote string, expected int64, data map[string]any) call {
	return call{"quote.save_draft", map[string]any{"QuoteID": quote, "ExpectedVersion": expected, "Data": data}}
}

func recordResponse(response, quote string, revision int64, hash, mail string, received time.Time) call {
	return call{"quote.response.record", map[string]any{"ResponseID": response, "QuoteID": quote, "BusinessRevision": revision,
		"BusinessHash": hash, "Classification": "full_unconditional", "Summary": "客户回复：全部接受（合成）",
		"SourceRef": map[string]any{"SourceID": mail, "Locator": "body"}, "ReceivedAt": stamp(received), "ReceivedAtBasis": "channel_server"}}
}

func prepareOrder(order, quote string, revision int64, hash, acceptance string) call {
	return call{"order.prepare", map[string]any{"OrderID": order, "QuoteID": quote, "BusinessRevision": revision, "BusinessHash": hash,
		"AcceptanceID": acceptance}}
}

func requestOrderReview(order string, expected int64, review string) call {
	return call{"order.review.request", map[string]any{"OrderID": order, "ExpectedVersion": expected, "ReviewID": review}}
}

// agentPhase is one stretch of the agent's work: a brant-worker -agent process with the phase's
// script (scripted suite) or the live model (live suite), running until the task reaches what the
// phase waits for. react is called with every task view read meanwhile (a person issuing an
// approval grant the task waits for, AI-INV-002); it returns true once it acted on the view.
type agentPhase struct {
	name  string
	steps []scriptStep
	until func(task map[string]any) bool
	react func(ctx context.Context, task map[string]any) (bool, error)
	// kill ends the worker with SIGKILL as soon as kill returns true for a task view (AN05); the phase
	// then ends.
	kill    func(ctx context.Context, task map[string]any) (bool, error)
	timeout time.Duration
	// maxSteps are the model turns per task and pass (brant-worker -agent-steps; 8 when 0) and poll
	// the wait between passes (150 ms when 0).
	maxSteps int
	poll     time.Duration
	// anyVersion lets until end the phase on the task as it is at the start; otherwise until is
	// asked only once the task moved (its version changed), so a state the task was already in
	// does not end the phase.
	anyVersion bool
}

// runAgent runs phase p on the trial's task and returns the last task view it read. The scripted
// suite writes p.steps, which name server values the harness already knows. The live suite does
// not: the worker is the live model, and the phase uses the read tools (rfq.get, cost_snapshot.list,
// config.lookup, and the others) to find those references. The live arm is not executed here when
// IDP_BRANT_MODEL_API_KEY is unset.
func (t *trial) runAgent(ctx context.Context, p agentPhase) (map[string]any, error) {
	rec := PhaseRecord{Name: p.name, ScriptSteps: len(p.steps), StartedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	before, err := t.modelCalls(ctx)
	if err != nil {
		return nil, err
	}
	writesBefore, err := t.committedActions(ctx)
	if err != nil {
		return nil, err
	}
	first, err := t.task(ctx)
	if err != nil {
		return nil, err
	}
	startVersion := num(first["task_version"])
	steps, poll := p.maxSteps, p.poll
	if steps == 0 {
		steps = 8
	}
	if poll == 0 {
		poll = 150 * time.Millisecond
	}
	args := []string{"-agent", "-agent-scope", t.tenant + "/" + company, "-agent-id", t.agentID, "-agent-poll", poll.String(),
		"-agent-steps", strconv.Itoa(steps)}
	if t.env.cfg.trace {
		// The worker's local diagnostic trace of the phase, under the trial's directory (S4 G2):
		// what the model was told and what it sent. Not evidence; the registry says where it is.
		rec.Trace = filepath.Join("trace", fmt.Sprintf("%02d-%s", len(t.rec.Phases)+1, p.name))
		args = append(args, "-agent-trace-dir", filepath.Join(t.dir, rec.Trace))
	}
	var env []string
	if t.env.cfg.suite == suiteLive {
		args = append(args, t.env.cfg.live.workerArgs()...)
		env = append(env, t.env.cfg.live.workerEnv()...)
	} else {
		raw, err := json.MarshalIndent(p.steps, "", "  ")
		if err != nil {
			return nil, err
		}
		if p.steps == nil {
			raw = []byte("[]")
		}
		script := filepath.Join(t.dir, fmt.Sprintf("script-%02d-%s.json", len(t.rec.Phases)+1, p.name))
		if err = os.WriteFile(script, raw, 0o600); err != nil {
			return nil, err
		}
		rec.Script = filepath.Base(script)
		args = append(args, "-model", "scripted", "-model-script", script)
	}
	logPath := filepath.Join(t.dir, fmt.Sprintf("worker-%02d-%s.log", len(t.rec.Phases)+1, p.name))
	start := time.Now()
	w, err := startProc(t.env.cfg.bin, "brant-worker", logPath, append([]string{"IDP_BRANT_POSTGRES_DSN=" + t.env.cfg.dsn}, env...), args...)
	if err != nil {
		return nil, err
	}
	timeout := p.timeout
	if timeout == 0 {
		timeout = t.env.cfg.phaseTimeout
	}
	rec.Timeout = timeout.String()
	deadline := time.Now().Add(timeout)
	var view map[string]any
	killed := false
	for {
		if ended, werr := w.exited(); ended {
			err = &phaseError{err: fmt.Errorf("brant-worker ended during phase %s: %w (see %s)", p.name, werr, logPath)}
			break
		}
		view, err = t.task(ctx)
		if err != nil {
			break
		}
		if p.kill != nil {
			var hit bool
			if hit, err = p.kill(ctx, view); err != nil {
				break
			}
			if hit {
				w.kill()
				killed = true
				rec.Killed = true
				view, err = t.task(ctx)
				break
			}
		}
		if (p.anyVersion || num(view["task_version"]) != startVersion) && p.until(view) {
			break
		}
		if p.react != nil {
			var acted bool
			if acted, err = p.react(ctx, view); err != nil {
				break
			}
			if acted {
				continue
			}
		}
		if time.Now().After(deadline) {
			err = &phaseError{timeout: true, err: fmt.Errorf("phase %s: the task is %s %v after %s (see %s)", p.name, str(view["lifecycle"]),
				view["wait"], timeout, logPath)}
			break
		}
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		if err != nil {
			break
		}
	}
	if !killed {
		_ = w.stop()
	}
	rec.Seconds = round(time.Since(start).Seconds(), 2)
	// What the phase did is read after it ended, whatever ended it: the records outlive ctx's
	// phase and a failed phase is the one that has to be explained.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	after, cerr := t.modelCalls(rctx)
	if cerr == nil {
		rec.ModelCalls, rec.ToolCalls = after.model-before.model, after.tool-before.tool
	}
	if writes, werr := t.committedActions(rctx); werr == nil {
		rec.Writes = writes - writesBefore
	}
	if view != nil {
		rec.Lifecycle = str(view["lifecycle"])
	}
	if err != nil {
		rec.Error = err.Error()
		if tail := tailOf(logPath, 12); tail != "" {
			rec.WorkerLogTail = tail
		}
	}
	var pe *phaseError
	switch {
	case killed:
		// AN05: the harness ended the worker on purpose.
	case errors.As(err, &pe) && pe.timeout:
		rec.Diagnosis = fmt.Sprintf("timeout: the task was %s after %s; the phase made %d model calls and %d tool calls and committed %d actions",
			rec.Lifecycle, timeout, rec.ModelCalls, rec.ToolCalls, rec.Writes)
	case err != nil:
		rec.Diagnosis = fmt.Sprintf("failed: the phase made %d model calls and %d tool calls and committed %d actions before it ended",
			rec.ModelCalls, rec.ToolCalls, rec.Writes)
	case rec.ModelCalls > 0 && rec.Writes == 0:
		rec.Diagnosis = fmt.Sprintf("no write: the phase ended with the task %s after %d model calls and %d tool calls, and committed no action",
			rec.Lifecycle, rec.ModelCalls, rec.ToolCalls)
	}
	if rec.Diagnosis != "" {
		rec.LastCalls = t.lastCalls(rctx, before.model+before.tool, 12)
	}
	t.rec.Phases = append(t.rec.Phases, rec)
	return view, err
}

// committedActions counts the task's committed actions (its writes).
func (t *trial) committedActions(ctx context.Context) (int, error) {
	a, err := t.ops[actorClerk].get(ctx, "/api/brant/v1/tsk/tasks/"+taskID+"/actions")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, x := range list(a["actions"]) {
		if str(dig(x, "state")) == "committed" {
			n++
		}
	}
	return n, nil
}

// lastCalls describes the last n calls of the task's model-call ledger after the first skip: for a
// model call its state, finish reason, tokens and latency; for a tool call the command, its state
// and the code of a refusal or wait. The ledger holds no content (decision E-6), so neither does
// this.
func (t *trial) lastCalls(ctx context.Context, skip, n int) []string {
	mc, err := t.ops[actorClerk].get(ctx, "/api/brant/v1/ai/tasks/"+taskID+"/model_calls")
	if err != nil {
		return []string{"the model-call ledger could not be read: " + codeOf(err)}
	}
	calls := list(mc["calls"])
	if skip > len(calls) {
		skip = len(calls)
	}
	calls = calls[skip:]
	if len(calls) > n {
		calls = calls[len(calls)-n:]
	}
	out := make([]string, 0, len(calls))
	for _, x := range calls {
		out = append(out, describeCall(x))
	}
	return out
}

// describeCall is one ledger call in words.
func describeCall(x any) string {
	s := fmt.Sprintf("#%d %s", num(dig(x, "seq")), str(dig(x, "kind")))
	if str(dig(x, "kind")) == "model" {
		s += " " + str(dig(x, "state"))
		if f := str(dig(x, "finish_reason")); f != "" {
			s += " finish=" + f
		}
		if dig(x, "input_tokens") != nil {
			s += fmt.Sprintf(" in=%d out=%d", num(dig(x, "input_tokens")), num(dig(x, "output_tokens")))
		}
		if dig(x, "latency_ms") != nil {
			s += fmt.Sprintf(" %dms", num(dig(x, "latency_ms")))
		}
	} else {
		s += " " + str(dig(x, "tool_command")) + " " + str(dig(x, "state"))
	}
	if c := str(dig(x, "code")); c != "" {
		s += " " + c
	}
	if dig(x, "retry") == true {
		s += " retry"
	}
	return s
}

// tailOf returns the last n lines of a log file.
func tailOf(path string, n int) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// lifecycleIs is a phase end condition: the task is in one of states.
func lifecycleIs(states ...string) func(map[string]any) bool {
	return func(v map[string]any) bool {
		for _, s := range states {
			if str(v["lifecycle"]) == s {
				return true
			}
		}
		return false
	}
}
