//go:build integration

package bootstrap_test

import (
	"context"
	"errors"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
)

// hold opens a transaction in company.A that inserts command's action result under key in schema,
// so the command sent with that key commits only after the transaction rolls back, when release is
// closed. It returns the blocker's backend pid and its outcome.
func (c *costingWorld) hold(d *downstream, schema, command, key string, release <-chan struct{}) (int32, <-chan error) {
	c.t.Helper()
	pids, done := make(chan int32, 1), make(chan error, 1)
	go func() {
		done <- d.in(d.scope, func(ctx context.Context, exec bentopg.Executor) error {
			var pid int32
			if err := exec.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				return err
			}
			if _, err := exec.Exec(ctx, `INSERT INTO `+schema+`.action_result (tenant_id, company_id, command, idempotency_key, request_hash,
				action_id, object_id, row_version, status, actor_kind, actor_id, authority_epoch, correlation_id, recorded_at)
				VALUES ($1, $2, $3, $4, repeat('0', 64), 'act.blocker', 'blocker', 1, 'blocked', 'human', 'blocker', 1, 'corr.blocker', now())`,
				string(d.scope.Tenant), string(d.scope.Company), command, key); err != nil {
				return err
			}
			pids <- pid
			<-release
			return errors.New("blocker rolls back")
		})
	}()
	select {
	case pid := <-pids:
		return pid, done
	case err := <-done:
		c.t.Fatalf("blocker: %v", err)
	}
	return 0, nil
}

// waiting reports whether a backend waits for a lock while running a statement containing text.
func waiting(d *downstream, text string) bool {
	var found bool
	if err := d.app.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE wait_event_type = 'Lock'
		AND position($1 in query) > 0)`, text).Scan(&found); err != nil {
		d.t.Fatal(err)
	}
	return found
}

type timed struct {
	at  time.Time
	err error
}

// costing_context.verify calls RfqGuard v1 and ConfigGuard v1 in its own transaction after locking
// the context root (ADR-BR-004 option B, ADR-BR-011): whichever of the verification and the
// upstream command locks the upstream row first decides. The verification is held open by a
// transaction holding its idempotency key, so no test hook enters the command.
func TestVerifyGuardDecidesCommitOrder(t *testing.T) {
	c := newCostingWorld(t)
	d := newDownstream(c.world)
	cancel := func(id, key string) error {
		_, err := c.c.INQ.CancelRfq(c.editor, inqapi.CancelRfq{IdempotencyKey: key, RfqID: id, ExpectedVersion: 2, Reason: "客户撤回询价"})
		return err
	}
	draft := func(id, rfq string) {
		data := c.data()
		data.RfqRef = &cstapi.RfqRef{RfqID: rfq, RowVersion: 2}
		_, err := c.saveContext(c.author, id, 0, data)
		c.must(err)
	}

	// The verification holds the guard first: it sealed the context and waits to commit; the
	// cancellation waits on its FOR SHARE lock and commits after it.
	t.Run("verification holds the RFQ guard first", func(t *testing.T) {
		c.readyRfq(c.editor, "rfq.v1", line("L1", "sku.1", "120"))
		draft("ctx.v1", "rfq.v1")
		key, release := c.key(), make(chan struct{})
		blocker, blocked := c.hold(d, "brant_cst", cstapi.CommandVerifyContext, key, release)
		verified := make(chan timed, 1)
		go func() {
			_, err := c.c.CST.VerifyContext(c.checker, cstapi.VerifyContext{IdempotencyKey: key, ContextID: "ctx.v1", ExpectedVersion: 1,
				WorkingRevision: 1, Evidence: proof})
			verified <- timed{time.Now(), err}
		}()
		await(t, "the verification guarded the RFQ and waits to commit", func() bool { return d.blocks(blocker) }, verified)
		cancelled, cancelKey := make(chan timed, 1), c.key()
		go func() {
			err := cancel("rfq.v1", cancelKey)
			cancelled <- timed{time.Now(), err}
		}()
		await(t, "the cancellation waits on the guard", func() bool { return waiting(d, "brant_inq.customer_rfq") }, cancelled)
		close(release)
		if err := <-blocked; err == nil {
			t.Fatal("the blocker must roll back")
		}
		v, x := <-verified, <-cancelled
		if v.err != nil || x.err != nil || !x.at.After(v.at) {
			t.Fatalf("verification %v at %v, cancellation %v at %v", v.err, v.at, x.err, x.at)
		}
		if ctx := c.context("ctx.v1"); ctx.ActiveRevision != 1 || ctx.Revisions[0].Rfq.RowVersion != 2 {
			t.Fatalf("context %+v", ctx)
		}
	})

	// INQ holds the row first: the cancellation updated the RFQ and waits to commit; the
	// verification's guard waits on the row, re-reads it cancelled and refuses.
	t.Run("INQ holds the row first", func(t *testing.T) {
		c.readyRfq(c.editor, "rfq.v2", line("L1", "sku.1", "120"))
		draft("ctx.v2", "rfq.v2")
		key, release := c.key(), make(chan struct{})
		blocker, blocked := c.hold(d, "brant_inq", inqapi.CommandCancelRfq, key, release)
		cancelled := make(chan error, 1)
		go func() { cancelled <- cancel("rfq.v2", key) }()
		await(t, "the cancellation holds the RFQ row and waits to commit", func() bool { return d.blocks(blocker) }, cancelled)
		verified, verify := make(chan error, 1), c.verifyCommand("ctx.v2")
		go func() {
			_, err := c.c.CST.VerifyContext(c.checker, verify)
			verified <- err
		}()
		await(t, "the verification's guard waits on the RFQ row", func() bool { return waiting(d, inqapi.RfqGuardFunction) }, verified)
		close(release)
		if err := <-blocked; err == nil {
			t.Fatal("the blocker must roll back")
		}
		if err := <-cancelled; err != nil {
			t.Fatalf("cancellation: %v", err)
		}
		err := <-verified
		if code(err) != cstapi.CodeReferenceStale || path(err) != "/args/data/rfq_ref" {
			t.Fatalf("verification after the cancellation: %v", err)
		}
		if ctx := c.context("ctx.v2"); ctx.ActiveRevision != 0 || ctx.WorkingRevision != 1 {
			t.Fatalf("context %+v", ctx)
		}
	})

	// POL holds the row first: retiring the pricing policy waits to commit; the verification's
	// ConfigGuard waits on the revision row, re-reads it retired and refuses.
	t.Run("POL holds the configuration row first", func(t *testing.T) {
		c.readyRfq(c.editor, "rfq.v3", line("L1", "sku.1", "120"))
		draft("ctx.v3", "rfq.v3")
		key, release := c.key(), make(chan struct{})
		blocker, blocked := c.hold(d, "brant_pol", polapi.CommandRetire, key, release)
		retired := make(chan error, 1)
		go func() {
			_, err := c.c.POL.Retire(c.reviewer, polapi.Retire{IdempotencyKey: key, Kind: polapi.KindPricingPolicy, ConfigID: "policy.1",
				ExpectedVersion: 2, Reason: "规则停止采用"})
			retired <- err
		}()
		await(t, "the retirement holds the revision row and waits to commit", func() bool { return d.blocks(blocker) }, retired)
		verified, verify := make(chan error, 1), c.verifyCommand("ctx.v3")
		go func() {
			_, err := c.c.CST.VerifyContext(c.checker, verify)
			verified <- err
		}()
		await(t, "the verification's guard waits on the revision row", func() bool { return waiting(d, polapi.ConfigGuardFunction) }, verified)
		close(release)
		if err := <-blocked; err == nil {
			t.Fatal("the blocker must roll back")
		}
		if err := <-retired; err != nil {
			t.Fatalf("retirement: %v", err)
		}
		err := <-verified
		if code(err) != polapi.CodeConfigNotActive || path(err) != "/args/data/pricing_policy_ref" {
			t.Fatalf("verification after the retirement: %v", err)
		}
	})
}
