//go:build integration

package bootstrap_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// readyArgs registers src.1 and confirms customer.1, contact.1 and sku.1, then returns a complete
// draft (Python CoreTests.ready_args).
func (w *world) readyArgs(ctx context.Context) inqapi.SaveRfq {
	w.source(ctx, "src.1", "合成询价120件")
	w.party(ctx, "customer.1", true, "src.1")
	w.contact(ctx, "contact.1", "customer.1", true, "src.1")
	w.sku(ctx, "sku.1", true, "src.1")
	return inqapi.SaveRfq{RfqID: "rfq.1", CustomerID: "customer.1", ContactID: "contact.1",
		SourceIDs: []string{"src.1"}, UnresolvedFields: []string{},
		Lines: []inqapi.RfqLine{{LineID: "L1", SkuID: "sku.1", Quantity: "120", UOM: "PCS", SourceID: "src.1", Locator: "第1段"}}}
}

func (w *world) save(ctx context.Context, cmd inqapi.SaveRfq) (inqapi.Receipt, error) {
	if cmd.IdempotencyKey == "" {
		cmd.IdempotencyKey = w.key()
	}
	return w.c.INQ.SaveRfq(ctx, cmd)
}

func (w *world) confirm(ctx context.Context, id string, version int64, evidence ...string) (inqapi.Receipt, error) {
	return w.c.INQ.ConfirmRfqScope(ctx, inqapi.ConfirmRfqScope{IdempotencyKey: w.key(), RfqID: id, ExpectedVersion: version, EvidenceIDs: evidence})
}

// Python test_core.py test_incomplete_rfq_can_be_saved_without_invented_facts.
func TestIncompleteRfqIsSavedWithoutInventedFacts(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	r, err := w.save(ctx, inqapi.SaveRfq{RfqID: "rfq.1", Lines: []inqapi.RfqLine{{LineID: "L1", RequestedItem: "待核实商品"}}})
	w.must(err)
	if r.Status != inqapi.StatusDraft || r.RowVersion != 1 {
		t.Fatalf("save %+v", r)
	}
	v, err := w.c.INQ.GetRfq(ctx, "rfq.1")
	w.must(err)
	if v.CustomerID != "" || v.Lines[0].Quantity != "" || v.Lines[0].RequestedItem != "待核实商品" {
		t.Fatalf("draft %+v", v)
	}
	_, err = w.confirm(ctx, "rfq.1", 1, "source.unknown")
	w.want(err, kernel.CodeReferenceInvalid)
	if v, _ = w.c.INQ.GetRfq(ctx, "rfq.1"); v.Status != inqapi.StatusDraft {
		t.Fatalf("status %s", v.Status)
	}
}

// Python test_core.py test_complete_rfq_becomes_ready_and_closes_with_reason.
func TestCompleteRfqBecomesReadyAndClosesWithReason(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	_, err := w.save(ctx, w.readyArgs(ctx))
	w.must(err)
	r, err := w.confirm(ctx, "rfq.1", 1, "src.1")
	w.must(err)
	if r.Status != inqapi.StatusReady || r.RowVersion != 2 {
		t.Fatalf("confirm %+v", r)
	}
	closed, err := w.c.INQ.CloseRfq(ctx, inqapi.CloseRfq{IdempotencyKey: w.key(), RfqID: "rfq.1", ExpectedVersion: 2, Reason: "客户不再推进"})
	w.must(err)
	if closed.Status != inqapi.StatusClosed || closed.RowVersion != 3 {
		t.Fatalf("close %+v", closed)
	}
	v, err := w.c.INQ.GetRfq(ctx, "rfq.1")
	w.must(err)
	if v.Lines[0].Quantity != "120" || v.Lines[0].SkuSpecRevision != 1 || v.Confirmation.CustomerRowVersion != 2 ||
		v.Confirmation.ConfirmedBy.ID != "human.1" || v.Confirmation.AuthorityEpoch < 1 {
		t.Fatalf("closed RFQ %+v %+v", v, v.Confirmation)
	}
	audit, err := w.c.INQ.Audit(ctx)
	w.must(err)
	var reason string
	for _, e := range audit {
		if e.Command == inqapi.CommandCloseRfq {
			reason = e.Reason
		}
	}
	if reason != "客户不再推进" {
		t.Fatalf("close reason %q", reason)
	}
	if err = w.exec("company.A", `UPDATE brant_inq.rfq_line SET quantity = '121'`); err == nil {
		t.Fatal("the lines of a closed RFQ must not change")
	}
}

// Python test_core.py test_duplicate_line_and_zero_quantity_have_exact_codes and
// test_unknown_source_points_to_actual_request_field.
func TestDraftRejectionsHaveExactCodesAndPaths(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	for _, c := range []struct {
		cmd  inqapi.SaveRfq
		code string
		path string
	}{
		{inqapi.SaveRfq{RfqID: "r", Lines: []inqapi.RfqLine{{LineID: "L1"}, {LineID: "L1"}}}, kernel.CodeDuplicateLineID, "/args/lines"},
		{inqapi.SaveRfq{RfqID: "r", Lines: []inqapi.RfqLine{{LineID: "L1", Quantity: "0.000"}}}, kernel.CodeInvalidKnownValue, "/args/lines/0/quantity"},
		{inqapi.SaveRfq{RfqID: "r", SourceIDs: []string{"missing"}}, kernel.CodeReferenceInvalid, "/args/source_ids/0"},
		{inqapi.SaveRfq{RfqID: "r", Lines: []inqapi.RfqLine{{LineID: "L1", SourceID: "missing"}}}, kernel.CodeReferenceInvalid, "/args/lines/0/source_id"},
		{inqapi.SaveRfq{RfqID: "r", Lines: []inqapi.RfqLine{{LineID: "L1", Quantity: "120\n"}}}, kernel.CodeSchemaInvalid, "/args/lines/0/quantity"},
		{inqapi.SaveRfq{RfqID: "r\n"}, kernel.CodeSchemaInvalid, "/args/id"},
	} {
		_, err := w.save(ctx, c.cmd)
		if code(err) != c.code || path(err) != c.path {
			t.Errorf("%+v: %s at %s, want %s at %s", c.cmd, code(err), path(err), c.code, c.path)
		}
	}
}

// Python test_core.py test_omission_replaces_draft_and_old_version_does_not_overwrite.
func TestOmissionReplacesDraftAndStaleVersionDoesNotOverwrite(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	_, err := w.save(ctx, inqapi.SaveRfq{RfqID: "r", Lines: []inqapi.RfqLine{{LineID: "L1"}}})
	w.must(err)
	r, err := w.save(ctx, inqapi.SaveRfq{RfqID: "r", ExpectedVersion: 1})
	w.must(err)
	if r.RowVersion != 2 {
		t.Fatalf("save %+v", r)
	}
	_, err = w.save(ctx, inqapi.SaveRfq{RfqID: "r", ExpectedVersion: 1, Lines: []inqapi.RfqLine{{LineID: "L2"}}})
	w.want(err, kernel.CodeVersionConflict)
	v, err := w.c.INQ.GetRfq(ctx, "r")
	w.must(err)
	if len(v.Lines) != 0 {
		t.Fatalf("lines %+v", v.Lines)
	}
}

// Python test_core.py test_same_content_different_origins_can_support_different_rfqs (RFQ half).
func TestSameContentDifferentOriginsSupportDifferentRfqs(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	w.source(ctx, "s1", "合成询价120件")
	w.source(ctx, "s2", "合成询价120件")
	for _, id := range []string{"1", "2"} {
		_, err := w.save(ctx, inqapi.SaveRfq{RfqID: "r" + id, SourceIDs: []string{"s" + id}})
		w.must(err)
		v, err := w.c.INQ.GetRfq(ctx, "r"+id)
		w.must(err)
		if !slices.Equal(v.SourceIDs, []string{"s" + id}) {
			t.Fatalf("r%s sources %v", id, v.SourceIDs)
		}
	}
}

// Python test_core.py test_cross_company_reference_is_rejected_without_existence_leak.
func TestCrossCompanyReferenceIsRejectedWithoutExistenceLeak(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	w.party(ctx, "c", false)
	other := w.provision(bootstrap.Grant{Scope: w.scope("company.B"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "human.1"}, Permissions: allPermissions})
	for _, ref := range []string{"c", "not.existing"} {
		_, err := w.save(other, inqapi.SaveRfq{RfqID: "r", CustomerID: ref})
		rej, _ := kernel.AsRejection(err)
		if rej == nil || rej.Code != kernel.CodeReferenceInvalid || len(rej.Fields) != 1 ||
			rej.Fields[0] != (kernel.FieldError{Path: "/args/customer_id", Reason: kernel.CodeReferenceInvalid}) {
			t.Fatalf("%s: %v", ref, err)
		}
	}
}

// Python test_core.py test_revocation_is_current_and_blocks_replay.
func TestRevokedActorCannotReplayAnRfqSave(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	cmd := inqapi.SaveRfq{IdempotencyKey: "k", RfqID: "r"}
	_, err := w.save(ctx, cmd)
	w.must(err)
	w.actor("human.1", "inq.read", "inq.audit")
	_, err = w.save(ctx, cmd)
	w.want(err, kernel.CodePermissionDenied)
	audit, err := w.c.INQ.Audit(ctx)
	w.must(err)
	if audit[len(audit)-1].Code != kernel.CodePermissionDenied {
		t.Fatalf("last audit %+v", audit[len(audit)-1])
	}
}

// Python test_core.py test_unconfirmed_and_inactive_masters_block_ready.
func TestInactiveSkuBlocksReady(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	_, err := w.save(ctx, w.readyArgs(ctx))
	w.must(err)
	_, err = w.c.CAT.DeactivateSku(ctx, catapi.DeactivateSku{IdempotencyKey: w.key(), SkuID: "sku.1", ExpectedVersion: 2, Reason: "停用"})
	w.must(err)
	_, err = w.confirm(ctx, "rfq.1", 1, "src.1")
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/lines/0/sku_id" {
		t.Fatalf("inactive SKU: %v at %s", err, path(err))
	}
	if v, _ := w.c.INQ.GetRfq(ctx, "rfq.1"); v.RowVersion != 1 {
		t.Fatalf("row version %d", v.RowVersion)
	}
}

// A revised customer is not current until confirmed again (PTY-INV-001 seen from INQ).
func TestRevisedCustomerBlocksReadyUntilReconfirmed(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	_, err := w.save(ctx, w.readyArgs(ctx))
	w.must(err)
	_, err = w.c.PTY.ReviseParty(ctx, ptyapi.ReviseParty{IdempotencyKey: w.key(), PartyID: "customer.1", ExpectedVersion: 2, Reason: "更名"})
	w.must(err)
	_, err = w.confirm(ctx, "rfq.1", 1, "src.1")
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/customer_id" {
		t.Fatalf("revised customer: %v", err)
	}
	_, err = w.c.PTY.ConfirmParty(ctx, ptyapi.ConfirmParty{IdempotencyKey: w.key(), PartyID: "customer.1", ExpectedVersion: 3, EvidenceIDs: []string{"src.1"}})
	w.must(err)
	r, err := w.confirm(ctx, "rfq.1", 1, "src.1")
	w.must(err)
	v, err := w.c.INQ.GetRfq(ctx, "rfq.1")
	w.must(err)
	if r.Status != inqapi.StatusReady || v.Confirmation.CustomerRowVersion != 4 {
		t.Fatalf("ready against customer row version %d", v.Confirmation.CustomerRowVersion)
	}
}

// INQ-INV-001 and Python test_ai_cannot_verify_sources_even_with_broad_command_grants: an agent
// prepares the draft, only a human confirms it.
func TestAgentPreparesAndOnlyAHumanConfirms(t *testing.T) {
	w := newWorld(t)
	human := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "human.1"},
		Permissions: allPermissions, DataScope: "company"})
	ai := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorAI, ID: "ai.1"},
		Permissions: allPermissions, DataScope: "company"})
	args := w.readyArgs(human)
	_, err := w.save(ai, args)
	w.must(err)
	_, err = w.confirm(ai, "rfq.1", 1, "src.1")
	w.want(err, kernel.CodePermissionDenied)
	_, err = w.confirm(human, "rfq.1", 1, "src.1")
	w.must(err)
}

// Python test_core.py test_concurrent_edit_has_one_winner_and_one_version_conflict.
func TestConcurrentRfqEditsHaveOneWinner(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	_, err := w.save(ctx, inqapi.SaveRfq{RfqID: "r"})
	w.must(err)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = w.c.INQ.SaveRfq(ctx, inqapi.SaveRfq{IdempotencyKey: "parallel." + string(rune('1'+i)), RfqID: "r", ExpectedVersion: 1,
				Lines: []inqapi.RfqLine{{LineID: "L" + string(rune('1'+i))}}})
		}()
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("want one winner: %v", errs)
	}
	for _, err := range errs {
		if err != nil && code(err) != kernel.CodeVersionConflict {
			t.Fatalf("loser %v", err)
		}
	}
	if v, _ := w.c.INQ.GetRfq(ctx, "r"); v.RowVersion != 2 {
		t.Fatalf("row version %d", v.RowVersion)
	}
}

// Python test_core.py test_reopen_preserves_source_and_rfq and test_storage.py
// test_parallel_startup_uses_one_atomic_schema: another process opening the same database sees the
// committed RFQ and source.
func TestAnotherProcessSeesCommittedData(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	w.source(ctx, "src.1", "合成询价120件")
	_, err := w.save(ctx, inqapi.SaveRfq{RfqID: "r", SourceIDs: []string{"src.1"}})
	w.must(err)
	octx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	other, err := bootstrap.Open(octx, bootstrap.Config{PostgresDSN: pgtest.AppDSN(t)})
	w.must(err)
	defer other.Close()
	c, err := other.Contexts()
	w.must(err)
	v, err := c.INQ.GetRfq(ctx, "r")
	w.must(err)
	s, err := c.EVD.GetSource(ctx, "src.1")
	w.must(err)
	if !slices.Equal(v.SourceIDs, []string{"src.1"}) || s.ContentText != "合成询价120件" {
		t.Fatalf("reopened %+v %+v", v, s)
	}
}

// Python test_core.py test_contact_must_match_customer_and_wrong_reference_kind_is_rejected.
func TestContactMustMatchCustomerAndReferenceKindsAreChecked(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	args := w.readyArgs(ctx)
	w.party(ctx, "customer.2", true, "src.1")
	wrongCustomer := args
	wrongCustomer.CustomerID = "customer.2"
	_, err := w.save(ctx, wrongCustomer)
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/contact_id" {
		t.Fatalf("contact of another customer: %v", err)
	}
	skuAsCustomer := args
	skuAsCustomer.CustomerID = "sku.1"
	_, err = w.save(ctx, skuAsCustomer)
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/customer_id" {
		t.Fatalf("a SKU as customer: %v", err)
	}
}

// Python test_core.py test_confirmation_requires_all_sources_and_no_unresolved_fields.
func TestConfirmationRequiresAllSourcesAndNoUnresolvedFields(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	args := w.readyArgs(ctx)
	w.source(ctx, "src.2", "补充")
	args.SourceIDs = []string{"src.1", "src.2"}
	_, err := w.save(ctx, args)
	w.must(err)
	_, err = w.confirm(ctx, "rfq.1", 1, "src.1")
	w.want(err, kernel.CodeDraftNotReady)
	args.ExpectedVersion, args.UnresolvedFields = 1, []string{"交期尚未澄清"}
	r, err := w.save(ctx, args)
	w.must(err)
	if r.RowVersion != 2 {
		t.Fatalf("save %+v", r)
	}
	_, err = w.confirm(ctx, "rfq.1", 2, "src.1", "src.2")
	if path(err) != "/args/unresolved_fields" {
		t.Fatalf("unresolved: %v at %s", err, path(err))
	}
}

// Python test_core.py test_terminal_rfq_cannot_be_reopened_by_save.
func TestTerminalRfqCannotBeReopenedBySave(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	_, err := w.save(ctx, inqapi.SaveRfq{RfqID: "r"})
	w.must(err)
	r, err := w.c.INQ.CancelRfq(ctx, inqapi.CancelRfq{IdempotencyKey: w.key(), RfqID: "r", ExpectedVersion: 1, Reason: "撤回"})
	w.must(err)
	if r.Status != inqapi.StatusCancelled {
		t.Fatalf("cancel %+v", r)
	}
	_, err = w.save(ctx, inqapi.SaveRfq{RfqID: "r", ExpectedVersion: 2})
	w.want(err, kernel.CodeInvalidState)
	_, err = w.c.INQ.CloseRfq(ctx, inqapi.CloseRfq{IdempotencyKey: w.key(), RfqID: "r", ExpectedVersion: 2, Reason: "x"})
	w.want(err, kernel.CodeInvalidState)
}

// Python test_core.py test_replay_ignores_changed_cas_but_not_changed_business_fields.
func TestReplayIgnoresCASButNotBusinessFields(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	original, err := w.save(ctx, inqapi.SaveRfq{IdempotencyKey: "once", RfqID: "r"})
	w.must(err)
	replay, err := w.save(ctx, inqapi.SaveRfq{IdempotencyKey: "once", RfqID: "r", ExpectedVersion: 999})
	w.must(err)
	if replay.ActionID != original.ActionID || !replay.Replayed {
		t.Fatalf("replay %+v", replay)
	}
	_, err = w.save(ctx, inqapi.SaveRfq{IdempotencyKey: "once", RfqID: "r", Lines: []inqapi.RfqLine{}})
	w.want(err, kernel.CodeIdempotencyPayloadConflict)
}

// Python test_core.py test_supplemental_confirmation_evidence_is_retained.
func TestSupplementalConfirmationEvidenceIsRetained(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	args := w.readyArgs(ctx)
	w.source(ctx, "clarification", "客户澄清")
	_, err := w.save(ctx, args)
	w.must(err)
	r, err := w.confirm(ctx, "rfq.1", 1, "src.1", "clarification")
	w.must(err)
	v, err := w.c.INQ.GetRfq(ctx, "rfq.1")
	w.must(err)
	if r.Status != inqapi.StatusReady || !slices.Equal(v.SourceIDs, []string{"clarification", "src.1"}) {
		t.Fatalf("sources %v", v.SourceIDs)
	}
	ev := w.events("company.A", inqapi.Source)
	if len(ev) != 1 || ev[0].Type != inqapi.EventRfqScopeConfirmed || ev[0].Payload["customer_row_version"] != float64(2) {
		t.Fatalf("events %+v", ev)
	}
}

// Python test_foundation.py test_own_team_scope_and_replay_do_not_leak.
func TestTeamScopeAndReplayDoNotLeak(t *testing.T) {
	w := newWorld(t)
	grant := func(id, team string, scope string) context.Context {
		return w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: id},
			Permissions: allPermissions, DataScope: authorizationScope(scope), Team: team})
	}
	a, b, outsider := grant("a", "team1", "team"), grant("b", "team1", "team"), grant("outside", "team2", "team")
	cmd := inqapi.SaveRfq{IdempotencyKey: "shared.key", RfqID: "private"}
	_, err := w.save(a, cmd)
	w.must(err)
	if _, err = w.c.INQ.GetRfq(b, "private"); err != nil {
		t.Fatalf("team member: %v", err)
	}
	_, err = w.c.INQ.GetRfq(outsider, "private")
	w.want(err, kernel.CodeNotFound)
	_, err = w.save(outsider, cmd)
	w.want(err, kernel.CodeNotFound)
	b = grant("b", "team1", "own")
	_, err = w.c.INQ.GetRfq(b, "private")
	w.want(err, kernel.CodeNotFound)
}

// S1-08: the front of BRC-001 as the Python demo runs it (python -m brant_core demo): one source,
// a confirmed customer and contact, four ST01 SKUs and a four-line RFQ of 24 + 36 + 12 + 48 = 120 PCS
// confirmed ready.
func TestBRC001FrontReachesReady(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.demo", allPermissions...)
	w.source(ctx, "source.1", "合成样例：ST01四个SKU，24、36、12、48件，合计120 PCS。")
	w.party(ctx, "customer.1", true, "source.1")
	w.contact(ctx, "contact.1", "customer.1", true, "source.1")
	skus := []string{"ST01.red.S", "ST01.red.M", "ST01.blue.S", "ST01.blue.M"}
	quantities := []string{"24", "36", "12", "48"}
	cmd := inqapi.SaveRfq{RfqID: "rfq.1", CustomerID: "customer.1", ContactID: "contact.1", SourceIDs: []string{"source.1"}}
	for i, s := range skus {
		w.sku(ctx, s, true, "source.1")
		n := string(rune('1' + i))
		cmd.Lines = append(cmd.Lines, inqapi.RfqLine{LineID: "L" + n, SkuID: s, Quantity: quantities[i], UOM: "PCS",
			SourceID: "source.1", Locator: "第1段，第" + n + "项"})
	}
	_, err := w.save(ctx, cmd)
	w.must(err)
	r, err := w.confirm(ctx, "rfq.1", 1, "source.1")
	w.must(err)
	v, err := w.c.INQ.GetRfq(ctx, "rfq.1")
	w.must(err)
	total := kernel.DecimalFromInt(0)
	var got []string
	for _, l := range v.Lines {
		got = append(got, l.Quantity)
		total = total.Add(kernel.MustDecimal(l.Quantity))
	}
	if r.Status != inqapi.StatusReady || r.RowVersion != 2 || !slices.Equal(got, quantities) || total.String() != "120" {
		t.Fatalf("BRC-001 front: %+v, quantities %v, total %s", r, got, total)
	}
}

// BC-AT02 (INQ half): a draft missing a SKU and the delivery date is saved; ready is refused until
// both are known.
func TestBCAT02DraftMissingSkuAndDeliverySavesButIsNotReady(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	args := w.readyArgs(ctx)
	args.Lines[0].SkuID = ""
	args.Lines[0].RequestedItem = "ST01 红色 S 码"
	args.UnresolvedFields = []string{"交期"}
	_, err := w.save(ctx, args)
	w.must(err)
	_, err = w.confirm(ctx, "rfq.1", 1, "src.1")
	if code(err) != kernel.CodeDraftNotReady || path(err) != "/args/unresolved_fields" {
		t.Fatalf("unresolved delivery: %v at %s", err, path(err))
	}
	args.ExpectedVersion, args.UnresolvedFields = 1, []string{}
	_, err = w.save(ctx, args)
	w.must(err)
	_, err = w.confirm(ctx, "rfq.1", 2, "src.1")
	if code(err) != kernel.CodeDraftNotReady || path(err) != "/args/lines/0/sku_id" {
		t.Fatalf("missing SKU: %v at %s", err, path(err))
	}
}

// BC-AT05: the matrix says 120, the original text 118. The conflict is recorded as unresolved and
// the lines are not adjusted to make the numbers agree; the RFQ is ready only after a clarification
// source settles it and the confirmation covers that source.
func TestBCAT05QuantityConflictIsNotPaperedOver(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	args := w.readyArgs(ctx)
	args.UnresolvedFields = []string{"数量冲突：矩阵 120、原文 118"}
	_, err := w.save(ctx, args)
	w.must(err)
	_, err = w.confirm(ctx, "rfq.1", 1, "src.1")
	w.want(err, kernel.CodeDraftNotReady)
	v, err := w.c.INQ.GetRfq(ctx, "rfq.1")
	w.must(err)
	if v.Status != inqapi.StatusDraft || v.Lines[0].Quantity != "120" {
		t.Fatalf("the refused confirmation changed the RFQ: %+v", v)
	}
	w.source(ctx, "clarification.1", "客户确认：数量以 120 件为准")
	args.ExpectedVersion, args.UnresolvedFields = 1, []string{}
	args.SourceIDs = []string{"src.1", "clarification.1"}
	_, err = w.save(ctx, args)
	w.must(err)
	_, err = w.confirm(ctx, "rfq.1", 2, "src.1")
	w.want(err, kernel.CodeDraftNotReady) // the clarification must be covered too
	r, err := w.confirm(ctx, "rfq.1", 2, "src.1", "clarification.1")
	w.must(err)
	if r.Status != inqapi.StatusReady {
		t.Fatalf("confirm %+v", r)
	}
}
