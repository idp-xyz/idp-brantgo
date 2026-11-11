//go:build integration

package bootstrap_test

import (
	"context"
	"errors"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// SkuGuard v1 (Context Map R14 CAT->TRD, guardLock; ADR-BR-004 option B, ADR-BR-011) as TRD will
// use it, with the throwaway quote schema and the helpers of pty_profile_guard_integration_test.go,
// against the real CAT commands. Python re-checks the SKU revision inside its quote transactions
// (test_t03_a27_cost_scope.py test_sku_profile_revision_must_match_cost_manifest, test_t03_snapshot.py
// test_stale_master_row_version_rolls_back_formalization); TRD ports those on top of this guard.

// skuStep guards ref with SkuGuard v1 and keeps the verdict in v.
func skuStep(ref catapi.SpecRef, v *catapi.SkuGuardVerdict) guardStep {
	return func(ctx context.Context, exec bentopg.Executor) (adoption, error) {
		var err error
		if *v, err = catapi.ScanSkuGuard(ref, exec.QueryRow(ctx, catapi.SkuGuardSQL, catapi.SkuGuardArgs(ref)...).Scan); err != nil {
			return adoption{}, err
		}
		if err = v.Check("/args/lines/0/sku"); err != nil {
			return adoption{}, errors.Join(errReferenceRefused, err)
		}
		return adoption{kind: "sku", id: ref.SkuID, revision: ref.SpecRevision}, nil
	}
}

// skuGuard calls SkuGuard v1 for ref in a transaction of scope.
func (d *downstream) skuGuard(scope kernel.Scope, ref catapi.SpecRef) catapi.SkuGuardVerdict {
	d.t.Helper()
	var v catapi.SkuGuardVerdict
	err := d.in(scope, func(ctx context.Context, exec bentopg.Executor) error {
		var err error
		v, err = catapi.ScanSkuGuard(ref, exec.QueryRow(ctx, catapi.SkuGuardSQL, catapi.SkuGuardArgs(ref)...).Scan)
		return err
	})
	if err != nil {
		d.t.Fatal(err)
	}
	return v
}

func (w *world) reviseSpec(ctx context.Context, id string, expected int64, key string) error {
	_, err := w.c.CAT.ReviseSpec(ctx, catapi.ReviseSpec{IdempotencyKey: key, SkuID: id, ExpectedVersion: expected, Reason: "面料变更"})
	return err
}

func (w *world) deactivateSku(ctx context.Context, id string, expected int64, key string) error {
	_, err := w.c.CAT.DeactivateSku(ctx, catapi.DeactivateSku{IdempotencyKey: key, SkuID: id, ExpectedVersion: expected, Reason: "停用"})
	return err
}

func (w *world) reconfirmSku(ctx context.Context, id string, expected int64) {
	w.t.Helper()
	_, err := w.c.CAT.ConfirmSku(ctx, catapi.ConfirmSku{IdempotencyKey: w.key(), SkuID: id, ExpectedVersion: expected, EvidenceIDs: []string{"src.1"}})
	w.must(err)
}

// The guard's verdicts are ResolveSpecs' (CAT-INV-001), confined to the caller's company; how the
// database publishes it.
func TestSkuGuardVerdicts(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	w.source(ctx, "src.1", "text")
	d := newDownstream(w)
	w.sku(ctx, "current", true, "src.1")
	w.sku(ctx, "draft", false)
	w.sku(ctx, "reopened", true, "src.1")
	w.must(w.reviseSpec(ctx, "reopened", 2, w.key()))
	w.sku(ctx, "revised", true, "src.1")
	w.must(w.reviseSpec(ctx, "revised", 2, w.key()))
	w.reconfirmSku(ctx, "revised", 3)
	w.sku(ctx, "inactive", true, "src.1")
	w.must(w.deactivateSku(ctx, "inactive", 2, w.key()))

	ref := func(id string, revision int64) catapi.SpecRef {
		return catapi.SpecRef{SkuID: id, SpecRevision: revision}
	}
	a, b := d.scope, w.scope("company.B")
	for _, c := range []struct {
		name    string
		ref     catapi.SpecRef
		scope   kernel.Scope
		known   bool
		status  string
		current int64
		want    string
	}{
		{"current revision", ref("current", 1), a, true, catapi.StatusConfirmed, 1, "OK"},
		{"revision never confirmed", ref("current", 2), a, false, catapi.StatusConfirmed, 1, kernel.CodeReferenceInvalid},
		{"revision 0", ref("current", 0), a, false, catapi.StatusConfirmed, 1, kernel.CodeReferenceInvalid},
		{"draft never confirmed", ref("draft", 1), a, false, catapi.StatusDraft, 0, kernel.CodeReferenceInvalid},
		{"unknown SKU", ref("nothing", 1), a, false, "", 0, kernel.CodeReferenceInvalid},
		{"reopened for revision", ref("reopened", 1), a, true, catapi.StatusDraft, 0, catapi.CodeReferenceStale},
		{"confirmed in a later revision", ref("revised", 1), a, true, catapi.StatusConfirmed, 2, catapi.CodeReferenceStale},
		{"the later revision", ref("revised", 2), a, true, catapi.StatusConfirmed, 2, "OK"},
		{"inactive SKU", ref("inactive", 1), a, true, catapi.StatusInactive, 0, kernel.CodeReferenceInvalid},
		{"another company", ref("current", 1), b, false, "", 0, kernel.CodeReferenceInvalid}, // never sees it
	} {
		v := d.skuGuard(c.scope, c.ref)
		got := code(v.Check("/args/lines/0/sku"))
		if got != c.want || v.Known != c.known || v.Status != c.status || v.CurrentRevision != c.current || v.Current() != (c.want == "OK") {
			t.Errorf("%s: %+v (%s), want %s", c.name, v, got, c.want)
		}
		if c.scope != a || c.ref.SpecRevision == 0 {
			continue // ResolveSpecs refuses revision 0 as malformed
		}
		standings, err := w.c.CAT.ResolveSpecs(ctx, []catapi.SpecRef{c.ref})
		w.must(err)
		s := standings[c.ref]
		if s.Current != v.Current() || s.Code != v.Code || s.Status != v.Status || s.CurrentRevision != v.CurrentRevision {
			t.Errorf("%s: ResolveSpecs says %+v, the guard %+v", c.name, s, v)
		}
	}
	for _, args := range [][]any{{nil, int64(1)}, {"current", nil}} {
		if err := d.in(a, func(ctx context.Context, exec bentopg.Executor) error {
			_, err := exec.Exec(ctx, catapi.SkuGuardSQL, args...)
			return err
		}); err == nil {
			t.Fatalf("the guard needs every argument: %v", args)
		}
	}
	d.guardAttributes("brant_cat.guard_sku_spec_current_v1(text, bigint)")
}

// ADR-BR-004 option B against CAT: whichever transaction locks the SKU row first decides.
func TestSkuGuardDecidesEveryCommitOrder(t *testing.T) {
	w := newWorld(t)
	ctx := w.actor("human.1", allPermissions...)
	w.source(ctx, "src.1", "text")
	d := newDownstream(w)
	changes := []struct {
		name, command string
		run           func(id, key string) error
		code, status  string
	}{
		{"specification reopened", catapi.CommandReviseSpec, func(id, key string) error { return w.reviseSpec(ctx, id, 2, key) },
			catapi.CodeReferenceStale, catapi.StatusDraft},
		{"SKU deactivated", catapi.CommandDeactivateSku, func(id, key string) error { return w.deactivateSku(ctx, id, 2, key) },
			kernel.CodeReferenceInvalid, catapi.StatusInactive},
	}
	serial := 0
	confirmed := func() (string, catapi.SpecRef) {
		serial++
		id := "sku" + string(rune('a'+serial))
		w.sku(ctx, id, true, "src.1")
		d.createQuote("q." + id)
		return id, catapi.SpecRef{SkuID: id, SpecRevision: 1}
	}

	// CAT commits first: the downstream guard then refuses the revision, and so it does once the
	// SKU is confirmed in a later revision.
	t.Run("CAT commits first", func(t *testing.T) {
		for _, c := range changes {
			id, ref := confirmed()
			w.must(c.run(id, w.key()))
			var v catapi.SkuGuardVerdict
			err := d.activateWith("q."+id, stage{}, skuStep(ref, &v))
			if !errors.Is(err, errReferenceRefused) || code(err) != c.code || v.Status != c.status {
				t.Errorf("%s: activation %v, verdict %+v; want refused %s", c.name, err, v, c.code)
			}
			if status, adopted := d.quote("q." + id); status != "draft" || adopted != 0 {
				t.Errorf("%s: quote %s with %d adoptions", c.name, status, adopted)
			}
		}
		id, ref := confirmed()
		w.must(w.reviseSpec(ctx, id, 2, w.key()))
		w.reconfirmSku(ctx, id, 3)
		var v catapi.SkuGuardVerdict
		err := d.activateWith("q."+id, stage{}, skuStep(ref, &v))
		if !errors.Is(err, errReferenceRefused) || code(err) != catapi.CodeReferenceStale || v.CurrentRevision != 2 {
			t.Errorf("confirmed in a later revision: activation %v, verdict %+v", err, v)
		}
	})

	// The downstream guard passes and holds the SKU row FOR SHARE: sku.revise_spec and
	// sku.deactivate wait until the activation commits, then succeed.
	t.Run("downstream holds the guard first", func(t *testing.T) {
		for _, c := range changes {
			id, ref := confirmed()
			type outcome struct {
				at  time.Time
				err error
			}
			done := make(chan outcome, 1)
			pids := make(chan int32, 1)
			key := w.key()
			var released time.Time
			var v catapi.SkuGuardVerdict
			err := d.activateWith("q."+id, stage{pid: pids, hold: func() {
				pid := <-pids
				go func() {
					err := c.run(id, key)
					done <- outcome{time.Now(), err}
				}()
				if !awaitInHold(t, c.name+" waits on the guard", func() bool { return d.blocks(pid) }, done) {
					return // the activation still ends its transaction
				}
				released = time.Now() // the activation writes and commits after this
			}}, skuStep(ref, &v))
			if err != nil || !v.Current() {
				t.Fatalf("%s: activation %v, verdict %+v", c.name, err, v)
			}
			if t.Failed() {
				return
			}
			r := <-done
			if r.err != nil || !r.at.After(released) {
				t.Fatalf("%s: %v, returned after the activation moved on: %v", c.name, r.err, r.at.After(released))
			}
			if status, adopted := d.quote("q." + id); status != "active" || adopted != 1 {
				t.Fatalf("%s: quote %s with %d adoptions", c.name, status, adopted)
			}
			if after := d.skuGuard(d.scope, ref); after.Code != c.code || after.Status != c.status {
				t.Fatalf("%s: after it %+v", c.name, after)
			}
		}
	})

	// The CAT command updated the SKU row and waits on a transaction holding its idempotency key;
	// the downstream guard waits on the row; when the command commits, the guard re-reads the row and
	// the activation is refused.
	t.Run("CAT holds the row first", func(t *testing.T) {
		for _, c := range changes {
			id, ref := confirmed()
			key := w.key()
			pid, release := d.keyBlocker(t, "brant_cat", c.command, key)
			done := make(chan error, 1)
			go func() { done <- c.run(id, key) }()
			await(t, c.name+" holds the SKU row and waits on the blocker", func() bool { return d.blocks(pid) }, done)
			activated := make(chan error, 1)
			pids := make(chan int32, 1)
			var v catapi.SkuGuardVerdict
			go func() { activated <- d.activateWith("q."+id, stage{pid: pids}, skuStep(ref, &v)) }()
			downstreamPID := <-pids
			await(t, "the guard waits on "+c.name, func() bool { return d.waitsForLock(downstreamPID) }, activated)
			release()
			if err := <-done; err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			err := <-activated
			if !errors.Is(err, errReferenceRefused) || code(err) != c.code || v.Status != c.status {
				t.Fatalf("%s: activation %v, verdict %+v; want refused %s", c.name, err, v, c.code)
			}
			if status, adopted := d.quote("q." + id); status != "draft" || adopted != 0 {
				t.Fatalf("%s: quote %s with %d adoptions", c.name, status, adopted)
			}
		}
	})
}
