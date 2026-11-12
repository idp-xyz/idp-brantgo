//go:build integration

package bootstrap_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// TradingProfileGuard v1 and AddressRoleGuard v1 (Context Map R13 PTY->TRD, guardLock; ADR-BR-004
// option B, ADR-BR-011) as TRD will use them: the throwaway quote schema of
// pol_guard_integration_test.go, whose activation locks the quote root FOR UPDATE first and then
// calls PTY's guard functions with the downstream's own executor, against the real PTY commands.
// What Python checks inside its quote transactions (test_t03_a29_trade_parties.py
// test_retired_profile_blocks_activation_and_ready, test_order_activation_rechecks_profile;
// test_t03_snapshot.py test_stale_master_row_version_rolls_back_formalization;
// test_t03_a30_contact_profiles.py test_retired_contact_profile_blocks_formalization and
// test_new_contact_profile_revision_blocks_old_reference, on the contact the profile pins: Brant
// has no contact profile) TRD ports on top of these guards; here is PTY's half: the verdicts
// (contact revised: contact_changed) and every commit order. TRD's half is
// TestLapsedInputsBlockActivation.

var (
	errReferenceRefused = errors.New("downstream: reference does not serve new business")
	errBlockerRollsBack = errors.New("blocker rolls back")
)

// adoption is the row a downstream activation records for a reference it guarded.
type adoption struct {
	kind, id string
	revision int64
	hash     string
}

// guardStep runs one upstream guard with the downstream's executor and returns what the
// activation records, or the guard's refusal joined with errReferenceRefused.
type guardStep func(ctx context.Context, exec bentopg.Executor) (adoption, error)

// activateWith is a downstream command: lock the quote root FOR UPDATE, run guard, then activate
// the quote and record the adoption, in one transaction.
func (d *downstream) activateWith(quoteID string, s stage, guard guardStep) error {
	return d.in(d.scope, func(ctx context.Context, exec bentopg.Executor) error {
		if s.pid != nil {
			var pid int32
			if err := exec.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				return err
			}
			s.pid <- pid
		}
		t, c := string(d.scope.Tenant), string(d.scope.Company)
		var status string
		if err := exec.QueryRow(ctx, "SELECT status FROM "+d.quotes+" WHERE tenant_id = $1 AND company_id = $2 AND quote_id = $3 FOR UPDATE",
			t, c, quoteID).Scan(&status); err != nil {
			return err
		}
		a, err := guard(ctx, exec)
		if err != nil {
			return err
		}
		if s.hold != nil {
			s.hold()
		}
		if _, err = exec.Exec(ctx, "UPDATE "+d.quotes+" SET status = 'active', root_version = root_version + 1 WHERE tenant_id = $1 AND company_id = $2 AND quote_id = $3",
			t, c, quoteID); err != nil {
			return err
		}
		_, err = exec.Exec(ctx, "INSERT INTO "+d.adopted+" (tenant_id, company_id, quote_id, kind, config_id, revision, content_hash) VALUES ($1, $2, $3, $4, $5, $6, $7)",
			t, c, quoteID, a.kind, a.id, a.revision, a.hash)
		return err
	})
}

// keyBlocker holds the idempotency key of command in the ledger of schema in an open transaction:
// the command with that key does all its writes and then waits to insert its result, holding its
// row locks, until release; the blocker then rolls back and the command commits. A test that fails
// before it releases the blocker releases it when it ends, so the command and the test finish.
func (d *downstream) keyBlocker(t *testing.T, schema, command, key string) (pid int32, release func()) {
	t.Helper()
	hold, pids, done := make(chan struct{}), make(chan int32, 1), make(chan error, 1)
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
			<-hold
			return errBlockerRollsBack
		})
	}()
	select {
	case pid = <-pids:
	case err := <-done:
		t.Fatalf("blocker: %v", err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			close(hold)
			if err := <-done; !errors.Is(err, errBlockerRollsBack) {
				t.Errorf("the blocker must roll back: %v", err)
			}
		})
	}
	t.Cleanup(release)
	return pid, release
}

// awaitInHold is await for a transaction the test holds open: it reports a failure with Errorf
// and returns false instead of ending the test, so that the transaction still ends.
func awaitInHold[T any](t *testing.T, what string, cond func() bool, done <-chan T) bool {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for !cond() {
		select {
		case v := <-done:
			t.Errorf("finished before %s: %v", what, v)
			return false
		case <-deadline:
			t.Errorf("timed out waiting until %s", what)
			return false
		case <-time.After(10 * time.Millisecond):
		}
	}
	return true
}

// profileStep guards ref with TradingProfileGuard v1 and keeps the verdict in v.
func profileStep(ref ptyapi.TradingProfileRef, v *ptyapi.TradingProfileGuardVerdict) guardStep {
	return func(ctx context.Context, exec bentopg.Executor) (adoption, error) {
		var err error
		if *v, err = ptyapi.ScanTradingProfileGuard(ref, exec.QueryRow(ctx, ptyapi.TradingProfileGuardSQL, ptyapi.TradingProfileGuardArgs(ref)...).Scan); err != nil {
			return adoption{}, err
		}
		if err = v.Check("/args/trading_profile"); err != nil {
			return adoption{}, errors.Join(errReferenceRefused, err)
		}
		return adoption{kind: "trading_profile", id: ref.PartyID, revision: ref.Revision, hash: v.ContentHash}, nil
	}
}

// addressStep guards ref with AddressRoleGuard v1 and keeps the verdict in v.
func addressStep(ref ptyapi.AddressRoleRef, v *ptyapi.AddressRoleGuardVerdict) guardStep {
	return func(ctx context.Context, exec bentopg.Executor) (adoption, error) {
		var err error
		if *v, err = ptyapi.ScanAddressRoleGuard(ref, exec.QueryRow(ctx, ptyapi.AddressRoleGuardSQL, ptyapi.AddressRoleGuardArgs(ref)...).Scan); err != nil {
			return adoption{}, err
		}
		if err = v.Check("/args/buyer_address"); err != nil {
			return adoption{}, errors.Join(errReferenceRefused, err)
		}
		return adoption{kind: "address_role", id: ref.PartyID + "/" + ref.AddressID, revision: ref.PartyRevision, hash: v.Role}, nil
	}
}

// profileGuard calls TradingProfileGuard v1 for ref in a transaction of scope.
func (d *downstream) profileGuard(scope kernel.Scope, ref ptyapi.TradingProfileRef) ptyapi.TradingProfileGuardVerdict {
	d.t.Helper()
	var v ptyapi.TradingProfileGuardVerdict
	err := d.in(scope, func(ctx context.Context, exec bentopg.Executor) error {
		var err error
		v, err = ptyapi.ScanTradingProfileGuard(ref, exec.QueryRow(ctx, ptyapi.TradingProfileGuardSQL, ptyapi.TradingProfileGuardArgs(ref)...).Scan)
		return err
	})
	if err != nil {
		d.t.Fatal(err)
	}
	return v
}

// addressGuard calls AddressRoleGuard v1 for ref in a transaction of scope.
func (d *downstream) addressGuard(scope kernel.Scope, ref ptyapi.AddressRoleRef) ptyapi.AddressRoleGuardVerdict {
	d.t.Helper()
	var v ptyapi.AddressRoleGuardVerdict
	err := d.in(scope, func(ctx context.Context, exec bentopg.Executor) error {
		var err error
		v, err = ptyapi.ScanAddressRoleGuard(ref, exec.QueryRow(ctx, ptyapi.AddressRoleGuardSQL, ptyapi.AddressRoleGuardArgs(ref)...).Scan)
		return err
	})
	if err != nil {
		d.t.Fatal(err)
	}
	return v
}

// guardAttributes checks that fn (a regprocedure) is SECURITY DEFINER with a fixed search_path and
// that brant_app, and not PUBLIC, may execute it.
func (d *downstream) guardAttributes(fn string) {
	d.t.Helper()
	var definer, executable, public bool
	var config []string
	if err := d.app.QueryRow(context.Background(), `SELECT p.prosecdef, p.proconfig,
		has_function_privilege('brant_app', p.oid, 'EXECUTE'), has_function_privilege('public', p.oid, 'EXECUTE')
		FROM pg_proc p WHERE p.oid = $1::regprocedure`, fn).Scan(&definer, &config, &executable, &public); err != nil {
		d.t.Fatal(err)
	}
	if !definer || len(config) != 1 || config[0] != "search_path=pg_catalog, pg_temp" || !executable || public {
		d.t.Fatalf("%s: security definer %v, config %v, brant_app %v, public %v", fn, definer, config, executable, public)
	}
}

// tradingCustomer confirms the customer id with a buyer address hq and a consignee address dock, a
// confirmed contact id.c, and publishes its trading profile: revision 1, binding party revision 1
// and the contact at row version 2.
func (w *world) tradingCustomer(editor, reviewer context.Context, id string, payment, trade ptyapi.TermsRef) (ptyapi.TradingProfileRef, ptyapi.AddressRoleRef) {
	w.t.Helper()
	w.staffParty(editor, id, []string{ptyapi.RoleCustomer}, ptyapi.Address{AddressID: "hq", Role: ptyapi.AddressBuyer, Address: "合成买方地址 " + id},
		ptyapi.Address{AddressID: "dock", Role: ptyapi.AddressConsignee, Address: "合成收货地址 " + id})
	w.contact(editor, id+".c", id, true, "evidence")
	saved, err := w.saveProfile(editor, id, 0, 1, payment, trade, ptyapi.PinnedContact{ContactID: id + ".c", RowVersion: 2})
	w.must(err)
	_, err = w.publishProfile(reviewer, id, saved.RowVersion, 1)
	w.must(err)
	return ptyapi.TradingProfileRef{PartyID: id, Revision: 1}, ptyapi.AddressRoleRef{PartyID: id, PartyRevision: 1, AddressID: "hq"}
}

func (w *world) reviseParty(ctx context.Context, id string, expected int64, key string) error {
	_, err := w.c.PTY.ReviseParty(ctx, ptyapi.ReviseParty{IdempotencyKey: key, PartyID: id, ExpectedVersion: expected, Reason: "更名"})
	return err
}

func (w *world) deactivateParty(ctx context.Context, id string, expected int64, key string) error {
	_, err := w.c.PTY.DeactivateParty(ctx, ptyapi.DeactivateParty{IdempotencyKey: key, PartyID: id, ExpectedVersion: expected, Reason: "停止合作"})
	return err
}

func (w *world) reconfirmParty(ctx context.Context, id string, expected int64) {
	w.t.Helper()
	_, err := w.c.PTY.ConfirmParty(ctx, ptyapi.ConfirmParty{IdempotencyKey: w.key(), PartyID: id, ExpectedVersion: expected, EvidenceIDs: []string{"evidence"}})
	w.must(err)
}

func (w *world) reviseContact(ctx context.Context, id string, expected int64, key string) error {
	_, err := w.c.PTY.ReviseContact(ctx, ptyapi.ReviseContact{IdempotencyKey: key, ContactID: id, ExpectedVersion: expected, Reason: "联系人变更"})
	return err
}

// The guards' verdicts are the lookups' (the same facts, the same stale reasons), confined to the
// caller's company; how the database publishes them.
func TestTradingProfileGuardVerdicts(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	payment, trade := w.settlement(editor, reviewer)
	d := newDownstream(w)

	current, _ := w.tradingCustomer(editor, reviewer, "current", payment, trade)
	superseded, _ := w.tradingCustomer(editor, reviewer, "superseded", payment, trade)
	saved, err := w.saveProfile(editor, "superseded", 2, 1, payment, trade, ptyapi.PinnedContact{ContactID: "superseded.c", RowVersion: 2})
	w.must(err)
	_, err = w.publishProfile(reviewer, "superseded", saved.RowVersion, 2)
	w.must(err)
	working, _ := w.tradingCustomer(editor, reviewer, "working", payment, trade)
	_, err = w.saveProfile(editor, "working", 2, 1, payment, trade)
	w.must(err)
	draft, _ := w.tradingCustomer(editor, reviewer, "draft", payment, trade)
	w.must(w.reviseParty(editor, "draft", 2, w.key()))
	inactive, _ := w.tradingCustomer(editor, reviewer, "inactive", payment, trade)
	w.must(w.deactivateParty(editor, "inactive", 2, w.key()))
	reconfirmed, _ := w.tradingCustomer(editor, reviewer, "reconfirmed", payment, trade)
	w.must(w.reviseParty(editor, "reconfirmed", 2, w.key()))
	w.reconfirmParty(editor, "reconfirmed", 3)
	contact, _ := w.tradingCustomer(editor, reviewer, "contact", payment, trade)
	w.must(w.reviseContact(editor, "contact.c", 2, w.key()))

	a, b := d.scope, w.scope("company.B")
	for _, c := range []struct {
		name   string
		ref    ptyapi.TradingProfileRef
		scope  kernel.Scope
		status string
		want   string
		stale  string
	}{
		{"current", current, a, ptyapi.StatusActive, "OK", ""},
		{"superseded", superseded, a, ptyapi.StatusSuperseded, ptyapi.CodeReferenceStale, ptyapi.StaleSuperseded},
		{"the superseding revision", ptyapi.TradingProfileRef{PartyID: "superseded", Revision: 2}, a, ptyapi.StatusActive, "OK", ""},
		{"working revision", ptyapi.TradingProfileRef{PartyID: "working", Revision: 2}, a, "", kernel.CodeReferenceInvalid, ""},
		{"the active revision beside a working one", working, a, ptyapi.StatusActive, "OK", ""},
		{"revision 0", ptyapi.TradingProfileRef{PartyID: "current"}, a, "", kernel.CodeReferenceInvalid, ""},
		{"unknown revision", ptyapi.TradingProfileRef{PartyID: "current", Revision: 9}, a, "", kernel.CodeReferenceInvalid, ""},
		{"unknown party", ptyapi.TradingProfileRef{PartyID: "nobody", Revision: 1}, a, "", kernel.CodeReferenceInvalid, ""},
		{"party revised into a draft", draft, a, ptyapi.StatusActive, ptyapi.CodeReferenceStale, ptyapi.StalePartyUnavailable},
		{"party deactivated", inactive, a, ptyapi.StatusActive, ptyapi.CodeReferenceStale, ptyapi.StalePartyUnavailable},
		{"party confirmed in a later revision", reconfirmed, a, ptyapi.StatusActive, ptyapi.CodeReferenceStale, ptyapi.StalePartyChanged},
		{"contact revised", contact, a, ptyapi.StatusActive, ptyapi.CodeReferenceStale, ptyapi.StaleContactChanged},
		{"another company", current, b, "", kernel.CodeReferenceInvalid, ""}, // never sees it
	} {
		v := d.profileGuard(c.scope, c.ref)
		got := code(v.Check("/args/trading_profile"))
		if got != c.want || v.Status != c.status || v.Stale != c.stale || v.Current() != (c.want == "OK") || v.Known != (c.status != "") {
			t.Errorf("%s: %+v (%s), want %s %s", c.name, v, got, c.want, c.stale)
		}
		if c.scope != a || c.ref.Revision == 0 {
			continue // the lookup reads revision 0 as the active one; the guard needs the exact revision
		}
		resolved, err := w.c.PTY.ResolveTradingProfiles(editor, []ptyapi.TradingProfileRef{c.ref})
		w.must(err)
		r, ok := resolved[c.ref]
		if ok != v.Known || (ok && (r.Code != v.Code || r.Stale != v.Stale || r.Status != v.Status || r.ContentHash != v.ContentHash ||
			r.PartyRevision != v.PartyRevision)) {
			t.Errorf("%s: the lookup says %+v (%v), the guard %+v", c.name, r, ok, v)
		}
	}
	other := w.provision(bootstrap.Grant{Scope: w.scope("company.B"), Actor: actorRef("human", "editor"), Permissions: allPermissions, DataScope: "company"})
	if resolved, err := w.c.PTY.ResolveTradingProfiles(other, []ptyapi.TradingProfileRef{current}); err != nil || len(resolved) != 0 {
		t.Fatalf("another company resolves %+v, %v", resolved, err)
	}
	for _, args := range [][]any{{nil, int64(1)}, {"current", nil}} {
		if err = d.in(a, func(ctx context.Context, exec bentopg.Executor) error {
			_, err := exec.Exec(ctx, ptyapi.TradingProfileGuardSQL, args...)
			return err
		}); err == nil {
			t.Fatalf("the guard needs every argument: %v", args)
		}
	}
	d.guardAttributes("brant_pty.guard_trading_profile_current_v1(text, bigint)")
}

func TestAddressRoleGuardVerdicts(t *testing.T) {
	w := newWorld(t)
	editor, _ := w.polActors()
	d := newDownstream(w)
	buyer := ptyapi.Address{AddressID: "hq", Role: ptyapi.AddressBuyer, Address: "合成买方地址"}
	consignee := ptyapi.Address{AddressID: "dock", Role: ptyapi.AddressConsignee, Address: "合成收货地址"}
	w.staffParty(editor, "current", []string{ptyapi.RoleCustomer}, buyer, consignee)
	w.staffParty(editor, "draft", []string{ptyapi.RoleCustomer}, buyer)
	w.must(w.reviseParty(editor, "draft", 2, w.key()))
	w.staffParty(editor, "inactive", []string{ptyapi.RoleCustomer}, buyer)
	w.must(w.deactivateParty(editor, "inactive", 2, w.key()))
	w.staffParty(editor, "reconfirmed", []string{ptyapi.RoleCustomer}, buyer, consignee)
	w.must(w.reviseParty(editor, "reconfirmed", 2, w.key()))
	w.reconfirmParty(editor, "reconfirmed", 3)

	ref := func(party string, revision int64, address string) ptyapi.AddressRoleRef {
		return ptyapi.AddressRoleRef{PartyID: party, PartyRevision: revision, AddressID: address}
	}
	a, b := d.scope, w.scope("company.B")
	for _, c := range []struct {
		name    string
		ref     ptyapi.AddressRoleRef
		scope   kernel.Scope
		role    string
		status  string
		current int64
		want    string
		stale   string
	}{
		{"buyer address", ref("current", 1, "hq"), a, ptyapi.AddressBuyer, ptyapi.StatusConfirmed, 1, "OK", ""},
		{"consignee address", ref("current", 1, "dock"), a, ptyapi.AddressConsignee, ptyapi.StatusConfirmed, 1, "OK", ""},
		{"unknown address", ref("current", 1, "nowhere"), a, "", "", 0, kernel.CodeReferenceInvalid, ""},
		{"unknown revision", ref("current", 2, "hq"), a, "", "", 0, kernel.CodeReferenceInvalid, ""},
		{"unknown party", ref("nobody", 1, "hq"), a, "", "", 0, kernel.CodeReferenceInvalid, ""},
		{"party revised into a draft", ref("draft", 1, "hq"), a, ptyapi.AddressBuyer, ptyapi.StatusDraft, 1, ptyapi.CodeReferenceStale, ptyapi.StalePartyUnavailable},
		{"party deactivated", ref("inactive", 1, "hq"), a, ptyapi.AddressBuyer, ptyapi.StatusInactive, 1, ptyapi.CodeReferenceStale, ptyapi.StalePartyUnavailable},
		{"party confirmed in a later revision", ref("reconfirmed", 1, "hq"), a, ptyapi.AddressBuyer, ptyapi.StatusConfirmed, 2, ptyapi.CodeReferenceStale, ptyapi.StalePartyRevised},
		{"the later revision", ref("reconfirmed", 2, "dock"), a, ptyapi.AddressConsignee, ptyapi.StatusConfirmed, 2, "OK", ""},
		{"another company", ref("current", 1, "hq"), b, "", "", 0, kernel.CodeReferenceInvalid, ""}, // never sees it
	} {
		v := d.addressGuard(c.scope, c.ref)
		got := code(v.Check("/args/buyer_address"))
		if got != c.want || v.Role != c.role || v.PartyStatus != c.status || v.CurrentRevision != c.current || v.Stale != c.stale ||
			v.Current() != (c.want == "OK") || v.Known != (c.role != "") {
			t.Errorf("%s: %+v (%s), want %s %s", c.name, v, got, c.want, c.stale)
		}
		if c.scope != a {
			continue
		}
		resolved, err := w.c.PTY.ResolveAddressRoles(editor, []ptyapi.AddressRoleRef{c.ref})
		w.must(err)
		r, ok := resolved[c.ref]
		if ok != v.Known || (ok && (r.Code != v.Code || r.Stale != v.Stale || r.Role != v.Role || r.PartyStatus != v.PartyStatus ||
			r.CurrentRevision != v.CurrentRevision)) {
			t.Errorf("%s: the lookup says %+v (%v), the guard %+v", c.name, r, ok, v)
		}
	}
	for _, args := range [][]any{{nil, int64(1), "hq"}, {"current", nil, "hq"}, {"current", int64(1), nil}} {
		if err := d.in(a, func(ctx context.Context, exec bentopg.Executor) error {
			_, err := exec.Exec(ctx, ptyapi.AddressRoleGuardSQL, args...)
			return err
		}); err == nil {
			t.Fatalf("the guard needs every argument: %v", args)
		}
	}
	d.guardAttributes("brant_pty.guard_address_role_current_v1(text, bigint, text)")
}

// change is a PTY command that ends a reference: prepare runs before it (inside the downstream's
// hold when the downstream holds the guard first, to show it does not wait), run is the command
// with an idempotency key, command its name in the ledger, and stale the guard's verdict after it.
type change struct {
	name    string
	command string
	prepare func(id string)
	run     func(id, key string) error
	stale   string
}

// ADR-BR-004 option B against PTY: whichever transaction locks the row first decides, for every row
// TradingProfileGuard v1 locks (the revision, the party, the contacts).
func TestTradingProfileGuardDecidesEveryCommitOrder(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	payment, trade := w.settlement(editor, reviewer)
	d := newDownstream(w)
	changes := []change{
		{name: "next revision published", command: ptyapi.CommandPublishTradingProfile,
			prepare: func(id string) {
				_, err := w.saveProfile(editor, id, 2, 1, payment, trade, ptyapi.PinnedContact{ContactID: id + ".c", RowVersion: 2})
				w.must(err)
			},
			run: func(id, key string) error {
				_, err := w.c.PTY.PublishTradingProfile(reviewer, ptyapi.PublishTradingProfile{IdempotencyKey: key, PartyID: id, ExpectedVersion: 3,
					Revision: 2, EvidenceIDs: []string{"evidence"}, Reason: "核实交易档案修订"})
				return err
			},
			stale: ptyapi.StaleSuperseded},
		{name: "party revised", command: ptyapi.CommandReviseParty,
			run: func(id, key string) error { return w.reviseParty(editor, id, 2, key) }, stale: ptyapi.StalePartyUnavailable},
		{name: "party deactivated", command: ptyapi.CommandDeactivateParty,
			run: func(id, key string) error { return w.deactivateParty(editor, id, 2, key) }, stale: ptyapi.StalePartyUnavailable},
		{name: "contact revised", command: ptyapi.CommandReviseContact,
			run: func(id, key string) error { return w.reviseContact(editor, id+".c", 2, key) }, stale: ptyapi.StaleContactChanged},
	}
	serial := 0
	customer := func() (string, ptyapi.TradingProfileRef) {
		serial++
		id := "tp" + string(rune('a'+serial))
		ref, _ := w.tradingCustomer(editor, reviewer, id, payment, trade)
		d.createQuote("q." + id)
		return id, ref
	}

	// PTY commits first: the downstream guard then reports the profile stale and the activation is
	// refused. The party confirmed in a later revision is stale too.
	t.Run("PTY commits first", func(t *testing.T) {
		reconfirm := change{name: "party confirmed in a later revision", run: func(id, key string) error {
			if err := w.reviseParty(editor, id, 2, key); err != nil {
				return err
			}
			w.reconfirmParty(editor, id, 3)
			return nil
		}, stale: ptyapi.StalePartyChanged}
		for _, c := range append(changes, reconfirm) {
			id, ref := customer()
			if c.prepare != nil {
				c.prepare(id)
			}
			w.must(c.run(id, w.key()))
			var v ptyapi.TradingProfileGuardVerdict
			err := d.activateWith("q."+id, stage{}, profileStep(ref, &v))
			if !errors.Is(err, errReferenceRefused) || code(err) != ptyapi.CodeReferenceStale || v.Stale != c.stale {
				t.Errorf("%s: activation %v, verdict %+v; want refused REFERENCE_STALE %s", c.name, err, v, c.stale)
			}
			if status, adopted := d.quote("q." + id); status != "draft" || adopted != 0 {
				t.Errorf("%s: quote %s with %d adoptions", c.name, status, adopted)
			}
		}
	})

	// The downstream guard passes and holds its rows FOR SHARE: each PTY command waits until the
	// activation commits, then succeeds; the profile is stale afterwards. Saving the next working
	// revision does not wait.
	t.Run("downstream holds the guard first", func(t *testing.T) {
		for _, c := range changes {
			id, ref := customer()
			type outcome struct {
				at  time.Time
				err error
			}
			done := make(chan outcome, 1)
			pids := make(chan int32, 1)
			key := w.key()
			var released time.Time
			var v ptyapi.TradingProfileGuardVerdict
			err := d.activateWith("q."+id, stage{pid: pids, hold: func() {
				pid := <-pids
				if c.prepare != nil {
					prepared := make(chan struct{})
					go func() { defer close(prepared); c.prepare(id) }()
					select {
					case <-prepared:
					case <-time.After(10 * time.Second):
						t.Errorf("%s: preparing waited for the guard", c.name)
					}
				}
				go func() {
					err := c.run(id, key)
					done <- outcome{time.Now(), err}
				}()
				if !awaitInHold(t, c.name+" waits on the guard", func() bool { return d.blocks(pid) }, done) {
					return // the activation still ends its transaction
				}
				released = time.Now() // the activation writes and commits after this
			}}, profileStep(ref, &v))
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
			if after := d.profileGuard(d.scope, ref); after.Code != ptyapi.CodeReferenceStale || after.Stale != c.stale {
				t.Fatalf("%s: after it %+v", c.name, after)
			}
		}
	})

	// The PTY command updated its row and waits on a transaction holding its idempotency key; the
	// downstream guard waits on that row; when the command commits, the guard re-reads the row and
	// the activation is refused.
	t.Run("PTY holds the row first", func(t *testing.T) {
		for _, c := range changes {
			id, ref := customer()
			if c.prepare != nil {
				c.prepare(id)
			}
			key := w.key()
			pid, release := d.keyBlocker(t, "brant_pty", c.command, key)
			done := make(chan error, 1)
			go func() { done <- c.run(id, key) }()
			await(t, c.name+" holds its row and waits on the blocker", func() bool { return d.blocks(pid) }, done)
			activated := make(chan error, 1)
			pids := make(chan int32, 1)
			var v ptyapi.TradingProfileGuardVerdict
			go func() { activated <- d.activateWith("q."+id, stage{pid: pids}, profileStep(ref, &v)) }()
			downstreamPID := <-pids
			await(t, "the guard waits on "+c.name, func() bool { return d.waitsForLock(downstreamPID) }, activated)
			release()
			if err := <-done; err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			err := <-activated
			if !errors.Is(err, errReferenceRefused) || code(err) != ptyapi.CodeReferenceStale || v.Stale != c.stale {
				t.Fatalf("%s: activation %v, verdict %+v; want refused REFERENCE_STALE %s", c.name, err, v, c.stale)
			}
			if status, adopted := d.quote("q." + id); status != "draft" || adopted != 0 {
				t.Fatalf("%s: quote %s with %d adoptions", c.name, status, adopted)
			}
		}
	})
}

// ADR-BR-004 option B against PTY for AddressRoleGuard v1: the party row decides.
func TestAddressRoleGuardDecidesEveryCommitOrder(t *testing.T) {
	w := newWorld(t)
	editor, _ := w.polActors()
	d := newDownstream(w)
	changes := []change{
		{name: "party revised", command: ptyapi.CommandReviseParty,
			run: func(id, key string) error { return w.reviseParty(editor, id, 2, key) }, stale: ptyapi.StalePartyUnavailable},
		{name: "party deactivated", command: ptyapi.CommandDeactivateParty,
			run: func(id, key string) error { return w.deactivateParty(editor, id, 2, key) }, stale: ptyapi.StalePartyUnavailable},
	}
	serial := 0
	customer := func() (string, ptyapi.AddressRoleRef) {
		serial++
		id := "ar" + string(rune('a'+serial))
		w.staffParty(editor, id, []string{ptyapi.RoleCustomer}, ptyapi.Address{AddressID: "hq", Role: ptyapi.AddressBuyer, Address: "合成买方地址 " + id})
		d.createQuote("q." + id)
		return id, ptyapi.AddressRoleRef{PartyID: id, PartyRevision: 1, AddressID: "hq"}
	}

	t.Run("PTY commits first", func(t *testing.T) {
		reconfirm := change{name: "party confirmed in a later revision", run: func(id, key string) error {
			if err := w.reviseParty(editor, id, 2, key); err != nil {
				return err
			}
			w.reconfirmParty(editor, id, 3)
			return nil
		}, stale: ptyapi.StalePartyRevised}
		for _, c := range append(changes, reconfirm) {
			id, ref := customer()
			w.must(c.run(id, w.key()))
			var v ptyapi.AddressRoleGuardVerdict
			err := d.activateWith("q."+id, stage{}, addressStep(ref, &v))
			if !errors.Is(err, errReferenceRefused) || code(err) != ptyapi.CodeReferenceStale || v.Stale != c.stale || v.Role != ptyapi.AddressBuyer {
				t.Errorf("%s: activation %v, verdict %+v; want refused REFERENCE_STALE %s", c.name, err, v, c.stale)
			}
			if status, adopted := d.quote("q." + id); status != "draft" || adopted != 0 {
				t.Errorf("%s: quote %s with %d adoptions", c.name, status, adopted)
			}
		}
	})

	t.Run("downstream holds the guard first", func(t *testing.T) {
		for _, c := range changes {
			id, ref := customer()
			type outcome struct {
				at  time.Time
				err error
			}
			done := make(chan outcome, 1)
			pids := make(chan int32, 1)
			key := w.key()
			var released time.Time
			var v ptyapi.AddressRoleGuardVerdict
			err := d.activateWith("q."+id, stage{pid: pids, hold: func() {
				pid := <-pids
				go func() {
					err := c.run(id, key)
					done <- outcome{time.Now(), err}
				}()
				if !awaitInHold(t, c.name+" waits on the guard", func() bool { return d.blocks(pid) }, done) {
					return // the activation still ends its transaction
				}
				released = time.Now()
			}}, addressStep(ref, &v))
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
			if after := d.addressGuard(d.scope, ref); after.Code != ptyapi.CodeReferenceStale || after.Stale != c.stale {
				t.Fatalf("%s: after it %+v", c.name, after)
			}
		}
	})

	t.Run("PTY holds the row first", func(t *testing.T) {
		for _, c := range changes {
			id, ref := customer()
			key := w.key()
			pid, release := d.keyBlocker(t, "brant_pty", c.command, key)
			done := make(chan error, 1)
			go func() { done <- c.run(id, key) }()
			await(t, c.name+" holds the party row and waits on the blocker", func() bool { return d.blocks(pid) }, done)
			activated := make(chan error, 1)
			pids := make(chan int32, 1)
			var v ptyapi.AddressRoleGuardVerdict
			go func() { activated <- d.activateWith("q."+id, stage{pid: pids}, addressStep(ref, &v)) }()
			downstreamPID := <-pids
			await(t, "the guard waits on "+c.name, func() bool { return d.waitsForLock(downstreamPID) }, activated)
			release()
			if err := <-done; err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			err := <-activated
			if !errors.Is(err, errReferenceRefused) || code(err) != ptyapi.CodeReferenceStale || v.Stale != c.stale {
				t.Fatalf("%s: activation %v, verdict %+v; want refused REFERENCE_STALE %s", c.name, err, v, c.stale)
			}
			if status, adopted := d.quote("q." + id); status != "draft" || adopted != 0 {
				t.Fatalf("%s: quote %s with %d adoptions", c.name, status, adopted)
			}
		}
	})
}
