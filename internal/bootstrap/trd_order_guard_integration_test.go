//go:build integration

package bootstrap_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The owner's decision of 2026-11-11: a revision a live (valid or disputed) acceptance accepts — a
// formed contract — is neither replaced, withdrawn nor cancelled (ACCEPTANCE_ALREADY_EXISTS) until a
// human invalidates the acceptance; a disputed acceptance is invalidated too. The database refuses
// it as well (the revision's status trigger). In commit order: while an invalidation holds the quote
// root uncommitted, a replacement is refused and a cancellation waits for it, then cancels; a
// replacement first is refused at once; unsynchronized, a replacement succeeds only when the
// invalidation committed first, and the invalidation always succeeds. BC-AT16 and BC-AT17 (SC-03): once the order took the slot, the replacement is
// ROOT_CONVERSION_LOCKED, and the replacing revision cannot take the slot either.
func TestAcceptedRevisionIsNotReplacedWithdrawnOrCancelled(t *testing.T) {
	q := newOrderWorld(t)
	d := newDownstream(q.world)
	_, a1 := q.accepted("quote.1")
	n, _, version := q.revisedAndApproved("quote.1", q.getQuote("quote.1").RootVersion, trdapi.TargetReplace, "review.quote.1.2", "approval.quote.1.2")
	_, err := q.c.TRD.Replace(q.author, q.replaceCmd("quote.1", version, n, "approval.quote.1.2"))
	if code(err) != trdapi.CodeAcceptanceAlreadyExists || path(err) != "/args/expected_active_revision" {
		t.Fatalf("replacing an accepted revision: %s at %s", code(err), path(err))
	}
	cancel := func(id string) error {
		_, err := q.c.TRD.Cancel(q.author, trdapi.Cancel{IdempotencyKey: q.key(), QuoteID: id, ExpectedVersion: q.getQuote(id).RootVersion,
			BusinessRevision: 1, Reason: "客户撤回", Evidence: q.proof})
		return err
	}
	q.want(cancel("quote.1"), trdapi.CodeAcceptanceAlreadyExists)
	if formal, _, _ := q.formalRows(); formal != 1 || !slices.Equal(q.statuses("quote.1"), []string{trdapi.StatusActive, trdapi.StatusPendingApproval}) {
		t.Fatalf("after the refusals %v", q.statuses("quote.1"))
	}

	// Disputed: still a formed contract until invalidated.
	_, a2 := q.accepted("quote.2")
	_, err = q.c.TRD.DisputeAcceptance(q.author, q.dispute(a2.ObjectID, 1))
	q.must(err)
	q.want(cancel("quote.2"), trdapi.CodeAcceptanceAlreadyExists)
	_, err = q.c.TRD.InvalidateAcceptance(q.reviewer, q.invalidation(a2.ObjectID, 2))
	q.must(err)
	q.must(cancel("quote.2"))

	// The database refuses it too, whatever the command checked.
	err = q.inTRD(func(ctx context.Context, exec bentopg.Executor) error {
		s := q.scope("company.A")
		if _, err := exec.Exec(ctx, `INSERT INTO brant_trd.quote_revision_closure (tenant_id, company_id, quote_id, revision, status, closed_by_kind,
			closed_by_id, close_epoch, closed_at, reason, evidence, root_version) VALUES ($1, $2, 'quote.1', 1, 'cancelled', 'human', 'editor', 1,
			now(), 'r', '[]', 99)`, string(s.Tenant), string(s.Company)); err != nil {
			return err
		}
		_, err := exec.Exec(ctx, `UPDATE brant_trd.quote_revision SET status = 'cancelled' WHERE tenant_id = $1 AND company_id = $2
			AND quote_id = 'quote.1' AND revision = 1`, string(s.Tenant), string(s.Company))
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "ACCEPTANCE_ALREADY_EXISTS") {
		t.Fatalf("the database cancelling an accepted revision: %v", err)
	}

	// The invalidation first, holding the quote root at its action result: a replacement is refused
	// at once (its trial before the transaction still finds the acceptance live), and a cancellation
	// waits for the root and then cancels the revision, the acceptance invalidated.
	invalidate := q.invalidation(a1.ObjectID, 1)
	blocker, release := d.keyBlocker(t, "brant_trd", trdapi.CommandInvalidateAcceptance, invalidate.IdempotencyKey)
	done := make(chan error, 1)
	go func() {
		_, err := q.c.TRD.InvalidateAcceptance(q.reviewer, invalidate)
		done <- err
	}()
	if !awaitInHold(t, "the invalidation holds the quote root", func() bool { return d.blocks(blocker) }, done) {
		release()
		return
	}
	_, err = q.c.TRD.Replace(q.author, q.replaceCmd("quote.1", version, n, "approval.quote.1.2"))
	if code(err) != trdapi.CodeAcceptanceAlreadyExists {
		release()
		t.Fatalf("a replacement while the invalidation is uncommitted: %s", code(err))
	}
	cancelled := make(chan error, 1)
	go func() { cancelled <- cancel("quote.1") }()
	ok := awaitInHold(t, "the cancellation waits for the invalidating root", func() bool { return lockWaiter(t, "brant_trd.quote_root") }, cancelled)
	release()
	if !ok {
		return
	}
	if err = <-done; err != nil {
		t.Fatalf("the invalidation: %v", err)
	}
	if err = <-cancelled; err != nil {
		t.Fatalf("the cancellation after the invalidation: %v", err)
	}
	if s := q.statuses("quote.1"); s[0] != trdapi.StatusCancelled {
		t.Fatalf("after the cancellation %v", s)
	}

	// Unsynchronized: an invalidation and a replacement at once; the replacement succeeds only if the
	// invalidation committed first, and the invalidation always does.
	for round := range 3 {
		id := "quote.race." + string(rune('a'+round))
		_, a := q.accepted(id)
		rn, _, rv := q.revisedAndApproved(id, q.getQuote(id).RootVersion, trdapi.TargetReplace, "review."+id+".2", "approval."+id+".2")
		var wg sync.WaitGroup
		var invErr, repErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, invErr = q.c.TRD.InvalidateAcceptance(q.reviewer, q.invalidation(a.ObjectID, 1))
		}()
		go func() {
			defer wg.Done()
			_, repErr = q.c.TRD.Replace(q.author, q.replaceCmd(id, rv, rn, "approval."+id+".2"))
		}()
		wg.Wait()
		if invErr != nil {
			t.Fatalf("round %d: the invalidation: %v", round, invErr)
		}
		statuses := q.statuses(id)
		switch code(repErr) {
		case "OK":
			if !slices.Equal(statuses, []string{trdapi.StatusSuperseded, trdapi.StatusActive}) {
				t.Fatalf("round %d: replaced %v", round, statuses)
			}
		case trdapi.CodeAcceptanceAlreadyExists, kernel.CodeVersionConflict:
			if !slices.Equal(statuses, []string{trdapi.StatusActive, trdapi.StatusPendingApproval}) {
				t.Fatalf("round %d: refused %v", round, statuses)
			}
		default:
			t.Fatalf("round %d: the replacement %s", round, code(repErr))
		}
	}

	// BC-AT16, BC-AT17: the order took the slot; the replacement is ROOT_CONVERSION_LOCKED and the
	// replacing revision converts nothing.
	a3, o3 := q.prepared("quote.3")
	n3, h3, v3 := q.revisedAndApproved("quote.3", q.getQuote("quote.3").RootVersion, trdapi.TargetReplace, "review.quote.3.2", "approval.quote.3.2")
	_, err = q.c.TRD.Replace(q.author, q.replaceCmd("quote.3", v3, n3, "approval.quote.3.2"))
	q.want(err, trdapi.CodeRootConversionLocked)
	_, err = q.c.TRD.PrepareOrder(q.author, trdapi.PrepareOrder{IdempotencyKey: q.key(), OrderID: "order.quote.3.r2", QuoteID: "quote.3",
		BusinessRevision: n3, BusinessHash: h3, AcceptanceID: a3.ObjectID})
	q.want(err, kernel.CodeReferenceInvalid) // r2 was never formalized: it serves no new business
	if v := q.getQuote("quote.3"); v.Conversion == nil || v.Conversion.OrderID != o3.ObjectID {
		t.Fatalf("the slot %+v", v.Conversion)
	}
}

// BC-AT17 and SC-03 under the owner's decision: a preparation and a replacement of an accepted
// revision submitted at once have one deterministic result — the order converts the revision and the
// replacement is refused, by the slot (ROOT_CONVERSION_LOCKED) when the preparation committed first,
// by the acceptance (ACCEPTANCE_ALREADY_EXISTS) otherwise.
func TestPrepareAndReplaceOfAnAcceptedRevision(t *testing.T) {
	q := newOrderWorld(t)
	for round := range 3 {
		id := "quote." + string(rune('a'+round))
		_, a := q.accepted(id)
		n, _, v := q.revisedAndApproved(id, q.getQuote(id).RootVersion, trdapi.TargetReplace, "review."+id+".2", "approval."+id+".2")
		var wg sync.WaitGroup
		var prepErr, repErr error
		wg.Add(2)
		go func() { defer wg.Done(); _, prepErr = q.c.TRD.PrepareOrder(q.author, q.prepareCmd("order."+id, a)) }()
		go func() {
			defer wg.Done()
			_, repErr = q.c.TRD.Replace(q.author, q.replaceCmd(id, v, n, "approval."+id+".2"))
		}()
		wg.Wait()
		if prepErr != nil {
			t.Fatalf("round %d: the preparation: %v", round, prepErr)
		}
		if c := code(repErr); c != trdapi.CodeRootConversionLocked && c != trdapi.CodeAcceptanceAlreadyExists && c != kernel.CodeVersionConflict {
			t.Fatalf("round %d: the replacement: %s", round, c)
		}
		if s := q.statuses(id); !slices.Equal(s, []string{trdapi.StatusActive, trdapi.StatusPendingApproval}) {
			t.Fatalf("round %d: %v", round, s)
		}
	}
}

// orderGuard calls OrderGuard v1 in company.A as a downstream transaction would.
func (q *trdWorld) orderGuard(b trdapi.OrderBinding) trdapi.OrderVerdict {
	q.t.Helper()
	var v trdapi.OrderVerdict
	q.must(q.inTRD(func(ctx context.Context, exec bentopg.Executor) error {
		var err error
		v, err = trdapi.ScanOrderGuard(b, exec.QueryRow(ctx, trdapi.OrderGuardSQL, trdapi.OrderGuardArgs(b)...).Scan)
		return err
	}))
	return v
}

// orderStep guards b with OrderGuard v1 and keeps the verdict in v (a PUR draft's step).
func orderStep(b trdapi.OrderBinding, v *trdapi.OrderVerdict) guardStep {
	return func(ctx context.Context, exec bentopg.Executor) (adoption, error) {
		var err error
		if *v, err = trdapi.ScanOrderGuard(b, exec.QueryRow(ctx, trdapi.OrderGuardSQL, trdapi.OrderGuardArgs(b)...).Scan); err != nil {
			return adoption{}, err
		}
		if err = v.Check("/args/order_ref"); err != nil {
			return adoption{}, errors.Join(errReferenceRefused, err)
		}
		return adoption{kind: "sales_order", id: b.OrderID, revision: b.OrderRevision, hash: b.OrderHash}, nil
	}
}

// ActiveOrderRef v1 and OrderGuard v1 (Context Map R19 TRD->PUR, guardLock; SM-TRD-ORDER-REVIEW:
// 阻止新激活，不倒写已激活订单) against a throwaway downstream schema standing for PUR: the guard answers
// REFERENCE_INVALID for an unknown order, ORDER_NOT_ACTIVE for a draft, REFERENCE_STALE for another
// order hash, and usable for the active order; the lookup says the same with the order's lines. In
// commit order: a revocation of the activating approval committed first makes the guard answer
// ORDER_APPROVAL_REVOKED and the downstream refuse; a downstream holding the guard makes the
// revocation wait until it commits; a revocation holding its locks makes the guard wait, and the
// guard then reads the order revoked.
func TestOrderGuardDecidesCommitOrder(t *testing.T) {
	q := newOrderWorld(t)
	d := newDownstream(q.world)
	_, draft := q.prepared("quote.0")
	active := q.activeOrder("quote.1")
	b := trdapi.OrderBinding{OrderID: active.ObjectID, OrderRevision: 1, OrderHash: active.OrderHash}
	for name, c := range map[string]struct {
		b    trdapi.OrderBinding
		code string
	}{
		"unknown":      {trdapi.OrderBinding{OrderID: "order.none", OrderRevision: 1, OrderHash: active.OrderHash}, kernel.CodeReferenceInvalid},
		"a draft":      {trdapi.OrderBinding{OrderID: draft.ObjectID, OrderRevision: 1, OrderHash: draft.OrderHash}, trdapi.CodeOrderNotActive},
		"another hash": {trdapi.OrderBinding{OrderID: active.ObjectID, OrderRevision: 1, OrderHash: draft.OrderHash}, trdapi.CodeReferenceStale},
		"active":       {b, "OK"},
	} {
		if v := q.orderGuard(c.b); code(v.Check("/x")) != c.code {
			t.Errorf("%s: %s, want %s (%+v)", name, code(v.Check("/x")), c.code, v)
		}
	}
	facts, err := q.c.TRD.ResolveActiveOrders(q.author, []string{draft.ObjectID, active.ObjectID})
	q.must(err)
	if facts[draft.ObjectID].Code != trdapi.CodeOrderNotActive || facts[active.ObjectID].Check("/x") != nil {
		t.Fatalf("the lookups %+v", facts)
	}
	if outsider, err := q.c.TRD.ResolveActiveOrders(q.regrant("outsider", "own", allPermissions), []string{active.ObjectID}); err != nil ||
		len(outsider) != 0 {
		t.Fatalf("an outsider's lookup %+v %v", outsider, err)
	}

	// The downstream holds the guard: the revocation waits for it.
	d.createQuote("pur.1")
	holding, hold := make(chan struct{}), make(chan struct{})
	var v trdapi.OrderVerdict
	drafted := make(chan error, 1)
	go func() {
		drafted <- d.activateWith("pur.1", stage{hold: func() { close(holding); <-hold }}, orderStep(b, &v))
	}()
	<-holding
	revoked := make(chan timed, 1)
	revoke := trdapi.RevokeOrderApproval{IdempotencyKey: q.key(), OrderID: active.ObjectID, ReviewID: "review.quote.1", Reason: "客户条件变化"}
	go func() {
		_, err := q.c.TRD.RevokeOrderApproval(q.reviewer, revoke)
		revoked <- timed{at: time.Now(), err: err}
	}()
	ok := awaitInHold(t, "the revocation waits for the downstream's guard", func() bool { return lockWaiter(t, "brant_trd.sales_order") }, revoked)
	released := time.Now()
	close(hold)
	if !ok {
		return
	}
	if err = <-drafted; err != nil {
		t.Fatalf("the downstream draft: %v", err)
	}
	if r := <-revoked; r.err != nil || !r.at.After(released) {
		t.Fatalf("the revocation %v, after the downstream %v", r.err, r.at.After(released))
	}
	if status, adopted := d.quote("pur.1"); status != "active" || adopted != 1 {
		t.Fatalf("the downstream %s %d", status, adopted)
	}

	// The revocation committed first: the guard answers ORDER_APPROVAL_REVOKED; the order stays active.
	if g := q.orderGuard(b); g.Code != trdapi.CodeOrderApprovalRevoked || !g.ApprovalRevoked || g.Status != trdapi.OrderActive {
		t.Fatalf("after the revocation %+v", g)
	}
	d.createQuote("pur.2")
	if err = d.activateWith("pur.2", stage{}, orderStep(b, &v)); code(err) != trdapi.CodeOrderApprovalRevoked {
		t.Fatalf("a downstream draft after the revocation: %v", err)
	}
	if f := q.fact(active.AcceptanceID); f.AcceptanceID == "" {
		t.Fatal("the acceptance lookup")
	}

	// A revocation holding its locks: the guard waits and reads the order revoked.
	other := q.activeOrder("quote.2")
	b2 := trdapi.OrderBinding{OrderID: other.ObjectID, OrderRevision: 1, OrderHash: other.OrderHash}
	revoke2 := trdapi.RevokeOrderApproval{IdempotencyKey: q.key(), OrderID: other.ObjectID, ReviewID: "review.quote.2", Reason: "客户条件变化"}
	blocker, release := d.keyBlocker(t, "brant_trd", trdapi.CommandRevokeOrderApproval, revoke2.IdempotencyKey)
	done := make(chan error, 1)
	go func() {
		_, err := q.c.TRD.RevokeOrderApproval(q.reviewer, revoke2)
		done <- err
	}()
	if !awaitInHold(t, "the revocation holds the order", func() bool { return d.blocks(blocker) }, done) {
		release()
		return
	}
	verdict := make(chan trdapi.OrderVerdict, 1)
	go func() { verdict <- q.orderGuard(b2) }()
	ok = awaitInHold(t, "the guard waits for the revocation", func() bool { return lockWaiter(t, "guard_order_active_v1") }, verdict)
	release()
	if !ok {
		return
	}
	if err = <-done; err != nil {
		t.Fatalf("the revocation: %v", err)
	}
	if g := <-verdict; g.Code != trdapi.CodeOrderApprovalRevoked {
		t.Fatalf("the guard after waiting %+v", g)
	}
}
