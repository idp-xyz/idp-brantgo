package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The Q01 steps of a trial (event storming ES-01 with the S4 task: BRC-001's inquiry to an active
// order). Agent phases are brant-worker -agent processes (agent.go); every other step is a person's,
// played by the scripted human operator with the fixed truth, or the evaluation platform's TSK
// transition where no consumer drives one (D1-D06).

// brcAgentLines are BRC-001's lines as the agent reads them from the inquiry: every field the file
// gives, each placed at its words in the extract.
func brcAgentLines() []agentLine {
	var out []agentLine
	for _, l := range brcLines {
		out = append(out, agentLine{id: l.id, sku: l.sku, item: l.item, qty: l.qty, uom: "PCS"})
	}
	return out
}

// draftRFQ is the agent's first phase: register the extract of the inquiry file the task names (B4,
// file_extract; S4 G2: the task's facts name the file, the source ID is the agent's own choice) and
// draft the RFQ from it with field locators; the model then says it is done, which only starts
// verification (AI-INV-004): the goal is not met and the task waits for input. Which source the
// agent registered is then read from the task's records, and a phase that registered none or saved
// no RFQ draft is the agent's failure (agent_phase), not a person's refused step.
func (t *trial) draftRFQ(ctx context.Context, lines []agentLine, more ...scriptStep) error {
	src := t.agentSourceID()
	in := t.in
	if t.claimed != nil {
		in = *t.claimed // where the agent says the fields stand, when that is not where they stand
	}
	steps := []scriptStep{turn(registerExtract(src, fileID), saveRFQ(rfqID, src, in, lines))}
	steps = append(steps, more...)
	steps = append(steps, done("RFQ 草稿已按附件逐行保存，等待负责人逐行核实。"))
	if _, err := t.runAgent(ctx, agentPhase{name: "rfq", steps: steps, until: lifecycleIs("waiting_input", "paused", "waiting_approval")}); err != nil {
		return err
	}
	if err := t.learnSource(ctx); err != nil {
		return err
	}
	if t.countActions(ctx, "rfq.save_draft") < 1 {
		return &phaseError{err: errors.New("the task's records show no committed rfq.save_draft")}
	}
	return nil
}

// confirmRFQ is the owner's line-by-line verification (BC-AT27, B4): each line of the agent's draft
// is compared with the fixed truth; a field that differs or is pending is settled from the truth
// (ownerVerification), the others are verified as they stand. Every field the owner settles is an
// owner resolution of the trial, synthetic, and INQ records it as the owner's correction: a
// person's work, never the agent's. A draft the truth cannot repair is the agent's failure
// (agent_phase), not a person's refused step. Through the task-step route.
func (t *trial) confirmRFQ(ctx context.Context, truth map[string]map[string]string) error {
	r, err := t.ops[actorClerk].get(ctx, "/api/brant/v1/inq/rfqs/"+rfqID)
	if err != nil {
		return err
	}
	t.save("rfq-draft.json", r)
	if err = t.checkLocators(ctx, r); err != nil {
		return err
	}
	verified, resolutions, err := ownerVerification(r, truth, t.in)
	if err != nil {
		return &phaseError{err: err}
	}
	out, err := t.humanStep(ctx, actorClerk, "inq", contractINQ, "rfq.confirm_scope", "h.rfq.confirm", true, map[string]any{"id": rfqID,
		"expected_version": r["row_version"], "evidence_ids": []string{t.sourceID}, "verified_lines": verified})
	if err != nil {
		return err
	}
	// Counted once the confirmation that carried them is recorded.
	t.rec.OwnerResolutions = append(t.rec.OwnerResolutions, resolutions...)
	t.rfqVersion = stepVersion(out)
	return nil
}

// Kinds of an owner resolution.
const (
	resolvedPlace     = "place_named"     // a field whose words stand at several places: the owner names the place
	resolvedValue     = "value_stated"    // a field the draft left pending: the owner states the value
	resolvedCorrected = "value_corrected" // a field the draft stated otherwise: the owner states the true value
)

// ownerVerification is what a person who knows the inquiry sends as verified_lines for the agent's
// RFQ draft (S4 G3): each line of the draft, and for a field the draft leaves pending or states
// otherwise than the truth, the true value. Where INQ holds a field ambiguous between several
// places of the source (the agent quoted the right words and did not place them: the unit "PCS"
// stands in every row), the owner names the place the truth knows the value at, which INQ requires
// (SOURCE_AMBIGUOUS otherwise) and admits only when it is one of the places it kept. Every field so
// settled is returned as an owner resolution.
//
// What the truth cannot repair is an error: a draft whose lines are not the inquiry's (one missing,
// one the truth does not have), and an ambiguous field whose true place is not among the places INQ
// kept (it keeps four; a person would save the line with the place, which the scripted owner does
// not do).
func ownerVerification(rfq map[string]any, truth map[string]map[string]string, in inquiry) ([]any, []OwnerResolution, error) {
	drafted := map[string]bool{}
	var unknown, missing []string
	for _, l := range list(rfq["lines"]) {
		id := str(dig(l, "line_id"))
		drafted[id] = true
		if _, ok := truth[id]; !ok {
			unknown = append(unknown, id)
		}
	}
	for id := range truth {
		if !drafted[id] {
			missing = append(missing, id)
		}
	}
	if len(unknown) > 0 || len(missing) > 0 {
		sort.Strings(missing)
		return nil, nil, fmt.Errorf("the agent's RFQ draft is not the inquiry's line set (missing %v, not in the truth %v): the owner cannot "+
			"repair it from the truth", missing, unknown)
	}
	var verified []any
	var resolutions []OwnerResolution
	for _, l := range list(rfq["lines"]) {
		id := str(dig(l, "line_id"))
		reasons, places := map[string]string{}, map[string][]string{}
		for _, p := range list(dig(l, "pending")) {
			reasons[str(dig(p, "field"))] = str(dig(p, "reason"))
		}
		for _, x := range list(dig(l, "locators")) {
			f := str(dig(x, "field"))
			places[f] = append(places[f], str(dig(x, "locator")))
		}
		var fields []any
		for _, f := range []string{fieldSKU, fieldQty, fieldUOM, fieldItem} {
			current := str(dig(l, f))
			reason, pending := reasons[f]
			want, known := truth[id][f]
			if !known {
				// The truth states the SKU, the quantity and the unit. The customer's words for the
				// item are verified as the draft has them, unless the draft left them pending.
				if !pending {
					continue
				}
				want = current
				if want == "" {
					want = in.at[id][f].Quote
				}
			}
			if !pending && current == want {
				continue
			}
			field := map[string]any{"field": f, "value": want}
			res := OwnerResolution{LineID: id, Field: f, Kind: resolvedValue, PendingReason: reason, From: current, To: want, Synthetic: true}
			switch {
			case !pending:
				res.Kind = resolvedCorrected
			case reason == pendingAmbiguous && len(places[f]) > 1:
				at, ok := in.at[id][f]
				if !ok || !slices.Contains(places[f], at.Locator) {
					return nil, nil, fmt.Errorf("line %s field %s is ambiguous between %v and the truth's place %q is not among them: the owner "+
						"cannot name it", id, f, places[f], at.Locator)
				}
				field["locator"] = at.Locator
				res.Kind, res.Locator = resolvedPlace, at.Locator
			}
			fields = append(fields, field)
			resolutions = append(resolutions, res)
		}
		v := map[string]any{"line_id": id}
		if len(fields) > 0 {
			v["fields"] = fields
		}
		verified = append(verified, v)
	}
	return verified, resolutions, nil
}

// pendingAmbiguous is INQ's pending reason of a field that fits several values or places.
const pendingAmbiguous = "ambiguous"

// checkLocators is the reviewer's reading of the file's extract (BC-AT27 字段定位; S4 B4): the owner
// reads the located form EVD registered for the inquiry over HTTP and checks that every locator of
// the agent's lines names a place in it holding the quoted words. The source is the one the task's
// records show the agent registered from the inquiry file (learnSource).
func (t *trial) checkLocators(ctx context.Context, rfq map[string]any) error {
	x, err := t.ops[actorClerk].get(ctx, "/api/brant/v1/evd/sources/"+t.sourceID+"/extract")
	if err != nil {
		return err
	}
	t.rec.Authority["extract"] = map[string]any{"kind": dig(x, "extract", "kind"), "extractor": dig(x, "extract", "extractor"),
		"extract_sha256": dig(x, "extract", "extract_sha256"), "file_sha256": dig(x, "extract", "file_sha256"),
		"characters": dig(x, "extract", "characters")}
	segments := locatedSegments(str(x["located_form"]))
	placed, bad := 0, []string{}
	for _, l := range list(rfq["lines"]) {
		for _, loc := range list(dig(l, "locators")) {
			placed++
			at, quote := str(dig(loc, "locator")), str(dig(loc, "quote"))
			if !standsAt(segments, t.in.kind, at, quote) {
				bad = append(bad, str(dig(l, "line_id"))+" "+str(dig(loc, "field"))+" "+at)
			}
		}
	}
	t.assert("BC-AT27: the agent's locators stand in the extract", "true", fmt.Sprint(placed > 0 && len(bad) == 0))
	if len(bad) > 0 {
		t.note("locators not found in the extract: %v", bad)
	}
	return nil
}

// locatedSegments reads the segments of a located form (brant-extract/1: a header line, then
// tab-separated records "segment <locator> <text>" and "note <kind> <locator>", with the escapes
// \\ \t \n \r).
func locatedSegments(form string) map[string]string {
	unescape := strings.NewReplacer(`\\`, `\`, `\t`, "\t", `\n`, "\n", `\r`, "\r")
	out := map[string]string{}
	for _, line := range strings.Split(form, "\n") {
		f := strings.SplitN(line, "\t", 3)
		if len(f) == 3 && f[0] == "segment" {
			out[unescape.Replace(f[1])] = unescape.Replace(f[2])
		}
	}
	return out
}

// standsAt reports whether quote stands at locator: a text line:col is a column of the segment of
// line n (n:1); a PDF page:line or an XLSX cell is a segment of its own that holds the words.
func standsAt(segments map[string]string, kind, locator, quote string) bool {
	if kind != kindText {
		return strings.Contains(segments[locator], quote)
	}
	n, c, ok := strings.Cut(locator, ":")
	col, err := strconv.Atoi(c)
	if !ok || err != nil || col < 1 {
		return false
	}
	runes := []rune(segments[n+":1"])
	return col-1 <= len(runes) && strings.HasPrefix(string(runes[col-1:]), quote)
}

// brcTruth is BRC-001's lines as the owner knows them.
func brcTruth() map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, l := range brcLines {
		out[l.id] = map[string]string{fieldSKU: l.sku, fieldQty: l.qty, fieldUOM: "PCS"}
	}
	return out
}

// costQuote has the buyer and the cost checker cost the ready RFQ (D1-D08) and resumes the task:
// the RFQ is ready and the cost snapshot frozen (no consumer tells TSK, D1-D06).
func (t *trial) costQuote(ctx context.Context) error {
	var err error
	t.snap, err = costing(ctx, t.ops, t.w, costTag, t.rfqVersion, t.sourceID)
	t.human(actorBuyer, "costing (supplier RFQ, offers, costing context, scenario)", "plain", true, err)
	t.human(actorCostChecker, "cost_snapshot.freeze", "plain", true, err)
	if err != nil {
		return err
	}
	return t.resume(ctx, "rfq", rfqID, t.rfqVersion, "询价已逐行核实并完成核价，成本快照 "+t.snap.snapshotID+" 已冻结")
}

// draftQuote is the agent's quote phase. It reads quote.price_proposal and drafts that price; price
// is the family's truth, and a proposal that is not that price fails the phase. The runner binds
// the root the draft created (task.bind_object, MD-31).
func (t *trial) draftQuote(ctx context.Context, price string, more ...scriptStep) error {
	if err := t.proposalMatches(ctx, price); err != nil {
		return err
	}
	data := quoteData(t.w, rfqID, t.rfqVersion, t.snap, t.sourceID, price, time.Now().Add(72*time.Hour))
	steps := append([]scriptStep{turn(readProposal(t.w, t.snap)), turn(saveQuote(quoteID, data))}, more...)
	steps = append(steps, done("报价草稿已保存，等待人工冻结、审核与生效。"))
	if _, err := t.runAgent(ctx, agentPhase{name: "quote", steps: steps, until: lifecycleIs("waiting_input", "paused")}); err != nil {
		return err
	}
	if t.quoteRoot = t.actionVersion(ctx, "quote.save_draft"); t.quoteRoot == 0 {
		return &phaseError{err: errors.New("the task's records show no committed quote.save_draft")}
	}
	v, err := t.task(ctx)
	if err != nil {
		return err
	}
	t.assert("quote root bound to the task", "bound "+quoteID, str(dig(v, "binding", "state"))+" "+str(dig(v, "binding", "quote_id")))
	return nil
}

// proposalMatches reads the price proposal over HTTP and requires every BRC line at price per piece.
func (t *trial) proposalMatches(ctx context.Context, price string) error {
	q := url.Values{}
	q.Set("currency", "USD")
	q.Set("pricing_policy_id", str(t.w.policy["id"]))
	q.Set("pricing_policy_revision", fmt.Sprint(t.w.policy["revision"]))
	q.Set("cost_snapshot_id", t.snap.snapshotID)
	q.Set("cost_snapshot_result_hash", t.snap.resultHash)
	v, err := t.ops[actorClerk].get(ctx, "/api/brant/v1/trd/rfqs/"+rfqID+"/price_proposal?"+q.Encode())
	if err != nil {
		return err
	}
	// The proposal also states how long the quote may be valid: the policy's seven days from the read.
	checked, errC := time.Parse(time.RFC3339Nano, str(v["checked_at"]))
	latest, errL := time.Parse(time.RFC3339Nano, str(v["expires_at_latest"]))
	if errC != nil || errL != nil || num(v["quote_valid_days"]) != 7 || !latest.Equal(checked.Add(7*24*time.Hour)) {
		return &phaseError{err: fmt.Errorf("quote.price_proposal states validity %v days, checked at %q, latest expiry %q; the policy allows 7 days",
			v["quote_valid_days"], str(v["checked_at"]), str(v["expires_at_latest"]))}
	}
	got := map[string]map[string]any{}
	for _, line := range list(v["lines"]) {
		m, _ := line.(map[string]any)
		got[str(m["rfq_line_id"])] = m
	}
	for _, l := range brcLines {
		m := got[l.id]
		if str(m["source"]) == "" || str(m["source"]) == "none" || str(m["unit_price"]) != price || str(m["price_basis_quantity"]) != "1" {
			return &phaseError{err: fmt.Errorf("quote.price_proposal for %s is source %q price %q basis %q; the truth drafts %s per piece",
				l.id, str(m["source"]), str(m["unit_price"]), str(m["price_basis_quantity"]), price)}
		}
	}
	return nil
}

// actionVersion is the row version the last committed action of command left on its object.
func (t *trial) actionVersion(ctx context.Context, command string) int64 {
	a, err := t.ops[actorClerk].get(ctx, "/api/brant/v1/tsk/tasks/"+taskID+"/actions")
	if err != nil {
		return 0
	}
	var v int64
	for _, x := range list(a["actions"]) {
		if str(dig(x, "target", "command")) == command && str(dig(x, "state")) == "committed" {
			v = num(dig(x, "result", "row_version"))
		}
	}
	return v
}

// freezeAndReview has the owner freeze working revision rev and submit it for review, and the quote
// reviewer approve it (independent of the agent who drafted it), through the task-step route.
func (t *trial) freezeAndReview(ctx context.Context, rev int64) error {
	clerk := t.ops[actorClerk]
	out, err := t.humanStep(ctx, actorClerk, "trd", contractTRD, "quote.freeze", fmt.Sprintf("h.quote.freeze.%d", rev), true,
		map[string]any{"quote_id": quoteID, "expected_version": t.quoteRoot, "working_revision": rev})
	if err != nil {
		return err
	}
	t.quoteRoot, t.revision = stepVersion(out), rev
	q, err := clerk.get(ctx, "/api/brant/v1/trd/quotes/"+quoteID)
	if err != nil {
		return err
	}
	for _, r := range list(q["revisions"]) {
		if num(dig(r, "revision")) == rev {
			t.businessHash = str(dig(r, "business_hash"))
			t.rec.Authority[fmt.Sprintf("quote_revision_%d_payable_minor", rev)] = dig(r, "calculation", "payable_minor")
		}
	}
	review := fmt.Sprintf("review.quote.%d", rev)
	if out, err = t.humanStep(ctx, actorClerk, "trd", contractTRD, "quote.submit_review", "h.quote.submit."+review, true,
		map[string]any{"quote_id": quoteID, "expected_version": t.quoteRoot, "business_revision": rev, "review_request_id": review,
			"target_command": "quote.activate"}); err != nil {
		return err
	}
	t.quoteRoot = stepVersion(out)
	t.approvalID = fmt.Sprintf("approval.quote.%d", rev)
	out, err = t.humanStep(ctx, actorQuoteReviewer, "trd", contractTRD, "quote.review.decide", "h.quote.decide."+review, true,
		map[string]any{"quote_id": quoteID, "expected_version": t.quoteRoot, "review_request_id": review, "business_hash": t.businessHash,
			"decision": "approve", "approval_id": t.approvalID, "expires_at": stamp(time.Now().Add(24 * time.Hour)),
			"reason": "内容核对无误：毛利率不低于 25%（合成评测）", "evidence": []any{map[string]any{"source_id": t.w.evidence, "locator": "报价审核依据"}}})
	if err != nil {
		return err
	}
	t.quoteRoot = stepVersion(out)
	return nil
}

// activateQuote has the owner activate the reviewed revision with its approval (formal document,
// quote number).
func (t *trial) activateQuote(ctx context.Context) error {
	out, err := t.humanStep(ctx, actorClerk, "trd", contractTRD, "quote.activate", fmt.Sprintf("h.quote.activate.%d", t.revision), true,
		map[string]any{"quote_id": quoteID, "expected_version": t.quoteRoot, "business_revision": t.revision, "approval_id": t.approvalID,
			"expected_approval_effectivity_version": 1})
	if err != nil {
		return err
	}
	t.quoteRoot = stepVersion(out)
	return nil
}

// customerReply files the customer's synthetic acceptance mail (the owner receives it) and resumes
// the task on it.
func (t *trial) customerReply(ctx context.Context) error {
	if _, err := t.humanCmd(ctx, actorClerk, "evd", contractEVD, "source.register", true, map[string]any{"id": mailID, "source_kind": "manual_text",
		"content_text": "合成客户邮件：确认接受报价 " + quoteID + " 全部条款与全部四行（合计 120 PCS，960.00 USD）。（AI 评测合成数据）"}); err != nil {
		return err
	}
	return t.resume(ctx, "evd_source", mailID, 0, "客户回复已到达："+mailID)
}

// recordResponse is the agent's phase recording the customer's response to the active revision; the
// response it recorded is then read from the task's records.
func (t *trial) recordResponse(ctx context.Context) error {
	steps := []scriptStep{turn(recordResponse(responseID, quoteID, t.revision, t.businessHash, mailID, time.Now())),
		done("客户回复已登记，等待人工核实接受。")}
	if _, err := t.runAgent(ctx, agentPhase{name: "response", steps: steps, until: lifecycleIs("waiting_input", "paused")}); err != nil {
		return err
	}
	return t.learnResponse(ctx)
}

// verifyAcceptance has the owner verify the response into an acceptance (human only) and resumes
// the task on it.
func (t *trial) verifyAcceptance(ctx context.Context) error {
	if _, err := t.humanStep(ctx, actorClerk, "trd", contractTRD, "quote.response.verify", "h.response.verify", true, map[string]any{
		"response_id": t.response(), "acceptance_id": acceptanceID, "business_revision": t.revision, "business_hash": t.businessHash,
		"customer_ref": map[string]any{"party_id": "customer.1", "revision": t.w.customerRevision},
		"contact_ref":  map[string]any{"contact_id": "contact.1", "row_version": 2}, "line_ids": lineIDs()}); err != nil {
		return err
	}
	return t.resume(ctx, "acceptance", acceptanceID, 1, "客户接受已核实："+acceptanceID)
}

// orderSteps are the agent's order phase in the scripted model: order.prepare needs a person's
// approval grant (D-7), so the first proposal makes the task wait; once the owner issued it the
// runner resumes the task and the model proposes it again; the same for order.review.request.
func (t *trial) orderSteps() []scriptStep {
	prepare := prepareOrder(orderID, quoteID, t.revision, t.businessHash, acceptanceID)
	request := requestOrderReview(orderID, 1, "review.order.1")
	return []scriptStep{turn(prepare), turn(prepare), turn(request), turn(request), done("订单草稿已准备并提交审核，等待人工审核与生效。")}
}

// prepareOrder is the agent's order phase; the owner approves each proposal as it waits.
func (t *trial) prepareOrderPhase(ctx context.Context, p agentPhase) error {
	if p.steps == nil {
		p.steps = t.orderSteps()
	}
	if p.name == "" {
		p.name = "order"
	}
	if p.until == nil {
		p.until = lifecycleIs("waiting_input", "paused")
	}
	if p.react == nil {
		p.react = t.approveWaits
	}
	_, err := t.runAgent(ctx, p)
	return err
}

// activateOrder has the order reviewer approve the order, compliance clear it, the owner reserve
// the customer's credit and activate it (S5's activation guards), through the task-step route
// where TRD is written.
func (t *trial) activateOrder(ctx context.Context) error {
	clerk := t.ops[actorClerk]
	// The order and its open review are the ones the records name: the task's committed
	// order.prepare, and the order's own last review.
	if err := t.learnOrder(ctx); err != nil {
		return err
	}
	order := t.order()
	o, err := clerk.get(ctx, "/api/brant/v1/trd/orders/"+order)
	if err != nil {
		return err
	}
	t.orderHash, t.orderVersion, t.orderReview = str(o["order_hash"]), num(o["row_version"]), "review.order.1"
	if reviews := list(o["reviews"]); len(reviews) > 0 {
		if id := str(dig(reviews[len(reviews)-1], "review_id")); id != "" {
			t.orderReview = id
		}
	}
	out, err := t.humanStep(ctx, actorOrderReviewer, "trd", contractTRD, "order.review.decide", "h.order.decide", true, map[string]any{
		"order_id": order, "expected_version": t.orderVersion, "review_id": t.orderReview, "order_hash": t.orderHash, "decision": "approve",
		"expires_at": stamp(time.Now().Add(24 * time.Hour)), "reason": "订单与已接受的报价一致（合成评测）"})
	if err != nil {
		return err
	}
	t.orderVersion = stepVersion(out)
	evidence := []string{t.w.evidence}
	e, err := t.humanCmd(ctx, actorCompliance, "cnx", contractCNX, "trade_compliance.evaluate", true, map[string]any{"order_id": order,
		"order_revision": 1, "expected_version": 0, "regulation_id": "screening", "regulation_revision": 1,
		"programme_lists": []string{cnProgrammes, unProgrammes}, "code_table_version": codeTable,
		"destination":       map[string]any{"country": clearCountry, "reason": "客户 PO 载明的卸货国（合成）", "evidence_ids": evidence},
		"address_countries": []any{map[string]any{"party_id": "customer.1", "address_id": "hq", "country": clearCountry, "evidence_ids": evidence}},
		"end_use":           map[string]any{"statement": "服装零售（合成）", "evidence_ids": evidence}})
	if err != nil {
		return err
	}
	check := str(e["object_id"])
	view, err := t.ops[actorCompliance].get(ctx, "/api/brant/v1/cnx/trade_compliance_checks/"+check)
	if err != nil {
		return err
	}
	if _, err = t.humanCmd(ctx, actorCompliance, "cnx", contractCNX, "trade_compliance.decide", true, map[string]any{"check_id": check,
		"expected_version": 1, "evaluation_id": dig(view, "evaluations", 0, "evaluation_id"), "conclusion": "clear",
		"reason": "目的国、交易方所在国与商品核查完成（合成）", "license_required": false, "evidence_ids": evidence}); err != nil {
		return err
	}
	if _, err = t.humanCmd(ctx, actorClerk, "stl", contractSTL, "credit.reserve", true, map[string]any{"reservation_id": "credit.q01",
		"order_id": order, "order_revision": 1, "order_hash": t.orderHash, "credit_policy": t.w.credit}); err != nil {
		return err
	}
	out, err = t.humanStep(ctx, actorClerk, "trd", contractTRD, "order.activate", "h.order.activate", true, map[string]any{"order_id": order,
		"expected_version": t.orderVersion, "review_id": t.orderReview})
	if err != nil {
		return err
	}
	t.orderVersion = stepVersion(out)
	return nil
}

// verifyPhase is the agent runner's last pass: TSK's consumer of OrderActivated (or QuoteActivated
// for quote_ready) resumes the task and starts its verification; the runner verifies the goal
// (goal.verify) against TRD's records. The model is asked only if the task is running again.
func (t *trial) verifyPhase(ctx context.Context) error {
	_, err := t.runAgent(ctx, agentPhase{name: "verify", steps: []scriptStep{done("目标已由平台核验。")},
		until: lifecycleIs("completed", "paused", "closed_unfulfilled", "failed", "cancelled"), anyVersion: true})
	return err
}

// assertOrderActive asserts the authoritative result of a completed order_active task: TSK
// completed with the completion actor, the last verification satisfied, every action settled; TRD's
// order active at 960.00 USD from the quote's active revision at 960.00 USD, prepared by the agent;
// TRD's goal readiness satisfied.
func (t *trial) assertCompleted(ctx context.Context, goal, actor string) {
	clerk := t.ops[actorClerk]
	v, err := t.task(ctx)
	if err == nil {
		t.assert("TSK lifecycle", "completed", str(v["lifecycle"]))
		t.assert("TSK completion actor", actor, str(v["completion_actor"]))
	}
	if vs, err := clerk.get(ctx, "/api/brant/v1/tsk/tasks/"+taskID+"/verifications"); err == nil {
		all := list(vs["verifications"])
		if len(all) > 0 {
			t.assert("TSK last verification", "satisfied", str(dig(all[len(all)-1], "outcome")))
		}
	}
	t.assert("TSK actions settled", "0", fmt.Sprint(t.openActions(ctx)))
	g, err := clerk.get(ctx, "/api/brant/v1/trd/quotes/"+quoteID+"/goal_readiness?goal="+goal)
	t.assert("TRD goal readiness "+goal, "satisfied", str(g["outcome"])+codeOf(err))
	if goal == "order_active" {
		o, err := clerk.get(ctx, "/api/brant/v1/trd/orders/"+t.order())
		t.assert("TRD order status", "active", str(o["status"])+codeOf(err))
		if actor == "ai_with_required_review" {
			t.assert("TRD order prepared by", "ai:"+t.agentID, str(dig(o, "prepared_by", "kind"))+":"+str(dig(o, "prepared_by", "id")))
		}
	}
}

// openActions counts the task's submitted or unknown actions (TSK-INV-003).
func (t *trial) openActions(ctx context.Context) int {
	a, err := t.ops[actorClerk].get(ctx, "/api/brant/v1/tsk/tasks/"+taskID+"/actions")
	if err != nil {
		return -1
	}
	n := 0
	for _, x := range list(a["actions"]) {
		if s := str(dig(x, "state")); s == "submitted" || s == "unknown" {
			n++
		}
	}
	return n
}

// assertPayable asserts the active quote revision's payable amount in minor units.
func (t *trial) assertPayable(ctx context.Context, want string) {
	clerk := t.ops[actorClerk]
	a, err := clerk.get(ctx, "/api/brant/v1/trd/quotes/"+quoteID+"/active")
	t.save("quote-active.json", a)
	rev := num(dig(a, "ref", "business_revision"))
	got := "no active revision " + codeOf(err)
	if q, err := clerk.get(ctx, "/api/brant/v1/trd/quotes/"+quoteID); err == nil && rev > 0 {
		for _, r := range list(q["revisions"]) {
			if num(dig(r, "revision")) == rev {
				got = str(dig(r, "calculation", "payable_minor"))
			}
		}
	}
	t.assert(fmt.Sprintf("TRD active revision %d payable (minor units)", rev), want, got)
}

// countActions counts the task's committed actions of command.
func (t *trial) countActions(ctx context.Context, command string) int {
	a, err := t.ops[actorClerk].get(ctx, "/api/brant/v1/tsk/tasks/"+taskID+"/actions")
	if err != nil {
		return -1
	}
	n := 0
	for _, x := range list(a["actions"]) {
		if str(dig(x, "target", "command")) == command && str(dig(x, "state")) == "committed" {
			n++
		}
	}
	return n
}
