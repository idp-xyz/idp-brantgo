//go:build integration

package bootstrap_test

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	supapi "go.idp.xyz/idp-brant/internal/contexts/sup/api"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// supState is a digest of SUP's business tables, its action results and its Outbox events in
// company.A: everything a refused command must leave as it was (Python test_costing_failures.py
// store_state, which leaves out the audit trail).
func (s *supWorld) supState() string {
	s.t.Helper()
	if s.db == nil {
		db, err := bentopg.NewDB(pgtest.AppPool(s.t))
		s.must(err)
		s.db = db
	}
	db := s.db
	var digest string
	err := bento.ScopeTransactor{DB: db, Scope: s.scope("company.A")}.WithinTransaction(context.Background(), func(ctx context.Context) error {
		exec, err := db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		return exec.QueryRow(ctx, `SELECT md5(concat_ws('|',
			(SELECT string_agg(t::text, ',' ORDER BY t::text) FROM brant_sup.condition_root t),
			(SELECT string_agg(t::text, ',' ORDER BY t::text) FROM brant_sup.condition_revision t),
			(SELECT string_agg(t::text, ',' ORDER BY t::text) FROM brant_sup.supplier_rfq t),
			(SELECT string_agg(t::text, ',' ORDER BY t::text) FROM brant_sup.action_result t),
			(SELECT count(*)::text FROM bento.outbox WHERE source = $1 AND scope = $2)))`, supapi.Source, s.scope("company.A").String()).Scan(&digest)
	})
	s.must(err)
	return digest
}

type supCommand func(ctx context.Context, key, id string, expected int64) (supapi.Receipt, error)

// matrix drives every SUP command of a lifecycle through the checks of Python
// test_costing_failures.py PersistentFailureMatrixTests around its success: before it, an actor
// without the permission, an AI agent for a human-only command and the draft's own editor for a
// verification or publication are refused and change nothing; the command succeeds and replays
// with the same result; after it, the same key with another object is IDEMPOTENCY_PAYLOAD_CONFLICT,
// a new key at the old version VERSION_CONFLICT and, for state transitions, a new key at the new
// version INVALID_STATE, all changing nothing.
type matrix struct {
	*supWorld
	blocked, ai context.Context
	commands    map[string]bool
}

func newMatrix(s *supWorld) *matrix {
	return &matrix{supWorld: s, blocked: s.other("blocked", kernel.ActorHuman, "sup.read", "evd.read"),
		ai: s.other("matrix.ai", kernel.ActorAI, allPermissions...), commands: map[string]bool{}}
}

var humanOnlySup = []string{supapi.CommandIssueSupplierRfq, supapi.CommandCloseSupplierRfq, supapi.CommandVerifySupplierOffer,
	supapi.CommandWithdrawSupplierOffer, supapi.CommandPublishPurchaseTerms, supapi.CommandRetirePurchaseTerms,
	supapi.CommandVerifyLogisticsOffer, supapi.CommandWithdrawLogisticsOffer}

func (m *matrix) call(who context.Context, command, id string, expected int64, run supCommand) supapi.Receipt {
	m.t.Helper()
	before := m.supState()
	refuse := func(ctx context.Context, want, check string) {
		m.t.Helper()
		if _, err := run(ctx, m.key(), id, expected); code(err) != want {
			m.t.Fatalf("%s %s: %s, want %s", command, check, code(err), want)
		}
		if m.supState() != before {
			m.t.Fatalf("%s %s changed the store", command, check)
		}
	}
	refuse(m.blocked, kernel.CodePermissionDenied, "without the permission")
	if slices.Contains(humanOnlySup, command) {
		refuse(m.ai, kernel.CodePermissionDenied, "by an AI agent")
	}
	_, action, _ := strings.Cut(command, ".")
	if action == supapi.ActionVerify || action == supapi.ActionPublish {
		refuse(m.author, supapi.CodeIndependentReviewRequired, "by the draft's editor")
	}
	key := m.key()
	r, err := run(who, key, id, expected)
	if err != nil || r.Replayed {
		m.t.Fatalf("%s: %+v %v", command, r, err)
	}
	replay, err := run(who, key, id, expected)
	if r.Replayed = true; err != nil || !reflect.DeepEqual(replay, r) {
		m.t.Fatalf("%s replay: %+v %v, want %+v", command, replay, err, r)
	}
	after := m.supState()
	if _, err = run(who, key, id+".changed", expected); code(err) != kernel.CodeIdempotencyPayloadConflict {
		m.t.Fatalf("%s with the same key for another object: %s", command, code(err))
	}
	if _, err = run(who, m.key(), id, expected); code(err) != kernel.CodeVersionConflict {
		m.t.Fatalf("%s at the old version: %s", command, code(err))
	}
	switch action {
	case supapi.ActionVerify, supapi.ActionPublish, supapi.ActionRevise, supapi.ActionDiscardDraft, "record_issued", "close":
		if _, err = run(who, m.key(), id, r.RowVersion); code(err) != kernel.CodeInvalidState {
			m.t.Fatalf("%s again at the new version: %s", command, code(err))
		}
	}
	if m.supState() != after {
		m.t.Fatalf("a refused repeat of %s changed the store", command)
	}
	m.commands[command] = true
	r.Replayed = false
	return r
}

// lifecycle is Python test_costing_lifecycle.py PersistentLifecycleTests.lifecycle for one supply
// condition kind: save, seal, revise, discard the revision, revise again, save, seal superseding
// revision 1, end revision 1. The root ends at version 8 with revision 3 active and its content.
func (m *matrix) lifecycle(kind string, content any) {
	m.t.Helper()
	id := "matrix." + kind
	s := m.supWorld
	saveCmd := func(ctx context.Context, key, id string, expected int64) (supapi.Receipt, error) {
		switch kind {
		case supapi.KindSupplierOffer:
			return s.c.SUP.SaveSupplierOffer(ctx, supapi.SaveSupplierOffer{IdempotencyKey: key, OfferID: id, ExpectedVersion: expected,
				Data: content.(supapi.SupplierOffer)})
		case supapi.KindPurchaseTerms:
			return s.c.SUP.SavePurchaseTerms(ctx, supapi.SavePurchaseTerms{IdempotencyKey: key, TermsID: id, ExpectedVersion: expected,
				Data: content.(supapi.PurchaseTerms)})
		}
		return s.c.SUP.SaveLogisticsOffer(ctx, supapi.SaveLogisticsOffer{IdempotencyKey: key, OfferID: id, ExpectedVersion: expected,
			Data: content.(supapi.LogisticsOffer)})
	}
	sealCmd := func(working, supersedes int64) supCommand {
		return func(ctx context.Context, key, id string, expected int64) (supapi.Receipt, error) {
			return s.c.SUP.Seal(ctx, supapi.Seal{IdempotencyKey: key, Kind: kind, ID: id, ExpectedVersion: expected, WorkingRevision: working,
				SupersedesRevision: supersedes, Evidence: evidenceOf("合成生命周期依据")})
		}
	}
	reviseCmd := func(ctx context.Context, key, id string, expected int64) (supapi.Receipt, error) {
		return s.c.SUP.Revise(ctx, supapi.Revise{IdempotencyKey: key, Kind: kind, ID: id, ExpectedVersion: expected, BaseRevision: 1, Reason: "修订测试"})
	}
	discardCmd := func(ctx context.Context, key, id string, expected int64) (supapi.Receipt, error) {
		return s.c.SUP.DiscardDraft(ctx, supapi.DiscardDraft{IdempotencyKey: key, Kind: kind, ID: id, ExpectedVersion: expected,
			WorkingRevision: 2, Reason: "丢弃未完成修订"})
	}
	endCmd := func(ctx context.Context, key, id string, expected int64) (supapi.Receipt, error) {
		return s.c.SUP.End(ctx, supapi.End{IdempotencyKey: key, Kind: kind, ID: id, ExpectedVersion: expected, TargetRevision: 1,
			Reason: "撤销旧修订", Evidence: evidenceOf("合成生命周期依据")})
	}
	seal, end := supapi.Command(kind, supapi.SealAction(kind)), supapi.Command(kind, supapi.EndAction(kind))
	m.call(s.author, supapi.Command(kind, supapi.ActionSaveDraft), id, 0, saveCmd)
	m.call(s.reviewer, seal, id, 1, sealCmd(1, 0))
	m.call(s.author, supapi.Command(kind, supapi.ActionRevise), id, 2, reviseCmd)
	m.call(s.author, supapi.Command(kind, supapi.ActionDiscardDraft), id, 3, discardCmd)
	m.call(s.author, supapi.Command(kind, supapi.ActionRevise), id, 4, reviseCmd)
	m.call(s.author, supapi.Command(kind, supapi.ActionSaveDraft), id, 5, saveCmd)
	latest := m.call(s.reviewer, seal, id, 6, sealCmd(3, 1))
	old := m.call(s.reviewer, end, id, 7, endCmd)
	v := s.condition(kind, id)
	if latest.RowVersion != 7 || old.RowVersion != 8 || v.RootVersion != 8 || v.ActiveRevision != 3 || v.WorkingRevision != 0 {
		m.t.Fatalf("%s: seal %+v, end %+v, view %+v", kind, latest, old, v)
	}
	statuses := []string{}
	for _, r := range v.Revisions {
		statuses = append(statuses, r.Status)
	}
	if want := []string{supapi.StatusSuperseded, supapi.StatusDiscarded, supapi.StatusActive}; old.Status != v.Revisions[0].Status ||
		!slices.Equal(statuses[1:], want[1:]) {
		m.t.Fatalf("%s: revisions %v, end %+v", kind, statuses, old)
	}
	r3 := v.Revisions[2]
	var got any
	switch kind {
	case supapi.KindSupplierOffer:
		got = *r3.Offer
	case supapi.KindPurchaseTerms:
		got = *r3.Terms
	default:
		got = *r3.Logistics
	}
	if !reflect.DeepEqual(got, content) || r3.BaseRevision != 1 || r3.Verification == nil || r3.Verification.By.ID != "reviewer" {
		m.t.Fatalf("%s: revision 3 %+v, want content %+v", kind, r3, content)
	}
}

// Python test_costing_lifecycle.py test_all_35_costing_commands_persist_and_replay (the supply
// condition and supplier RFQ part; SUP-INV-001, SUP-INV-003) and test_costing_failures.py
// PersistentFailureMatrixTests: every SUP command succeeds, replays its original result, and is
// refused without changing anything when the actor lacks the permission, is an AI agent for a
// human-only command, edited the draft it verifies or publishes, reuses a key, is stale or repeats a
// transition.
func TestAllSupCommandsPersistReplayAndRefuse(t *testing.T) {
	s := newSupWorld(t)
	m := newMatrix(s)
	m.lifecycle(supapi.KindSupplierOffer, s.offer())
	m.lifecycle(supapi.KindLogisticsOffer, s.logistics())
	s.verified(supapi.KindSupplierOffer, "offer.1", s.offer())
	m.lifecycle(supapi.KindPurchaseTerms, s.terms("offer.1", supapi.ValidityIndependent))

	o := s.offer()
	request := supapi.SupplierRfq{Supplier: o.Supplier, Rfq: o.Rfq, RfqLineIDs: []string{"L1"}, Evidence: evidenceOf("合成询价")}
	saved := m.call(s.author, supapi.CommandSaveSupplierRfq, "request.1", 0, func(ctx context.Context, key, id string, expected int64) (supapi.Receipt, error) {
		return s.c.SUP.SaveSupplierRfq(ctx, supapi.SaveSupplierRfq{IdempotencyKey: key, SupplierRfqID: id, ExpectedVersion: expected, Data: request})
	})
	issued := m.call(s.reviewer, supapi.CommandIssueSupplierRfq, "request.1", 1, func(ctx context.Context, key, id string, expected int64) (supapi.Receipt, error) {
		return s.c.SUP.RecordSupplierRfqIssued(ctx, supapi.RecordSupplierRfqIssued{IdempotencyKey: key, SupplierRfqID: id, ExpectedVersion: expected,
			Evidence: evidenceOf("人工发送记录"), Reason: "人工处理合成询价"})
	})
	closed := m.call(s.reviewer, supapi.CommandCloseSupplierRfq, "request.1", 2, func(ctx context.Context, key, id string, expected int64) (supapi.Receipt, error) {
		return s.c.SUP.CloseSupplierRfq(ctx, supapi.CloseSupplierRfq{IdempotencyKey: key, SupplierRfqID: id, ExpectedVersion: expected,
			Evidence: evidenceOf("人工关闭记录"), Reason: "人工处理合成询价"})
	})
	if saved.Status != supapi.StatusDraft || issued.RowVersion != 2 || issued.Status != supapi.StatusIssued || closed.RowVersion != 3 ||
		closed.Status != supapi.StatusClosed {
		t.Fatalf("supplier RFQ %+v %+v %+v", saved, issued, closed)
	}
	view, err := s.c.SUP.GetSupplierRfq(s.author, "request.1")
	s.must(err)
	if view.IssuedVersion != 2 || view.Issue == nil || view.Close == nil || !reflect.DeepEqual(view.Data, request) {
		t.Fatalf("supplier RFQ view %+v", view)
	}

	all := []string{supapi.CommandSaveSupplierRfq, supapi.CommandIssueSupplierRfq, supapi.CommandCloseSupplierRfq}
	for _, kind := range []string{supapi.KindSupplierOffer, supapi.KindPurchaseTerms, supapi.KindLogisticsOffer} {
		for _, a := range []string{supapi.ActionSaveDraft, supapi.ActionRevise, supapi.ActionDiscardDraft, supapi.SealAction(kind), supapi.EndAction(kind)} {
			all = append(all, supapi.Command(kind, a))
		}
	}
	for _, c := range all {
		if !m.commands[c] {
			t.Errorf("%s was not exercised", c)
		}
	}
	if len(m.commands) != 18 {
		t.Fatalf("%d commands exercised: %v", len(m.commands), m.commands)
	}
}
