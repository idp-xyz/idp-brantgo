//go:build integration

package action_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.idp.xyz/idp-bento-go/eventing"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

var (
	human = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "human.1"}
	other = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "human.2"}
	robot = kernel.ActorRef{Kind: kernel.ActorAI, ID: "ai.1"}
)

// rig is a throwaway context: schema <brant_tnote...> with the ledger tables and one aggregate
// table, a Bento outbox schema, and a fresh tenant and company in brant_auth.
type rig struct {
	t      *testing.T
	pool   *pgxpool.Pool
	db     *bentopg.DB
	schema string
	scope  kernel.Scope
	ledger *action.Ledger
	outbox *bentooutbox.Store
}

func newRig(t *testing.T) *rig {
	t.Helper()
	migrator, app := pgtest.MigratorPool(t), pgtest.AppPool(t)
	bentoSchema := pgtest.SchemaFunc(t, migrator, app, "brant_t_bento_", func(name string) string {
		var b strings.Builder
		for _, m := range bentopg.Migrations() {
			sql, err := bentopg.RenderMigration(m.ID, name)
			if err != nil {
				t.Fatal(err)
			}
			b.WriteString(sql + ";\n")
		}
		b.WriteString("GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA {{schema}} TO {{app}};\n")
		b.WriteString("GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA {{schema}} TO {{app}}")
		return b.String()
	})
	schema := pgtest.SchemaFunc(t, migrator, app, "brant_tnote", func(name string) string {
		return action.ResultsDDL(name) + `
CREATE TABLE {{schema}}.note (
    tenant_id text NOT NULL, company_id text NOT NULL, note_id text NOT NULL, body text NOT NULL,
    row_version bigint NOT NULL, owner_actor_id text NOT NULL, owner_team_id text,
    PRIMARY KEY (tenant_id, company_id, note_id));
ALTER TABLE {{schema}}.note ENABLE ROW LEVEL SECURITY;
ALTER TABLE {{schema}}.note FORCE ROW LEVEL SECURITY;
CREATE POLICY note_scope ON {{schema}}.note
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
GRANT SELECT, INSERT ON {{schema}}.action_result, {{schema}}.audit_event TO {{app}};
GRANT SELECT, INSERT, UPDATE ON {{schema}}.note TO {{app}}`
	})
	db, err := bentopg.NewDB(app, bentopg.WithSchema(bentoSchema))
	if err != nil {
		t.Fatal(err)
	}
	store, err := bentooutbox.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := authorization.NewPolicy(
		authorization.Rule{Command: "note.save", Permission: "note.save", ActorKinds: []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI}},
		authorization.HumanOnly("note.approve", "note.approve"),
		authorization.Rule{Command: "note.get", Permission: "note.read", ActorKinds: []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI}},
	)
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, pool: app, db: db, schema: schema, outbox: store,
		scope: kernel.Scope{Tenant: kernel.TenantID(pgtest.UniqueID(t, "tenant.")), Company: kernel.CompanyID(pgtest.UniqueID(t, "company."))}}
	r.ledger = &action.Ledger{
		DB: db, Schema: schema, Source: "idp-brant.note", Authorizer: authorization.Authorizer{Policy: policy},
		Outbox: store, Clock: bento.SystemClock{}, ReplayRead: "note.get", Visible: r.visible,
	}
	r.grant(human, "note.save", "note.approve", "note.read")
	r.grant(robot, "note.save", "note.approve", "note.read")
	return r
}

func (r *rig) admin(fn func(context.Context, bentopg.Executor) error) {
	r.t.Helper()
	err := bento.ScopeTransactor{DB: r.db, Scope: r.scope}.WithinTransaction(context.Background(), func(ctx context.Context) error {
		exec, err := r.db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		return fn(ctx, exec)
	})
	if err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) grant(a kernel.ActorRef, permissions ...string) {
	r.admin(func(ctx context.Context, exec bentopg.Executor) error {
		g := authorization.Grants{}
		if err := g.Provision(ctx, exec, r.scope); err != nil {
			return err
		}
		_, err := g.Put(ctx, exec, r.scope, a, permissions, time.Now())
		return err
	})
}

func (r *rig) as(a kernel.ActorRef) context.Context {
	ctx, err := identity.WithPrincipal(context.Background(), identity.Principal{Actor: a, Scope: r.scope})
	if err != nil {
		r.t.Fatal(err)
	}
	return ctx
}

func (r *rig) visible(ctx context.Context, exec bentopg.Executor, d authorization.Decision, _, id string) (bool, error) {
	var owner, team string
	err := exec.QueryRow(ctx, `SELECT owner_actor_id, coalesce(owner_team_id, '') FROM `+r.schema+`.note
		WHERE tenant_id = $1 AND company_id = $2 AND note_id = $3`, string(r.scope.Tenant), string(r.scope.Company), id).Scan(&owner, &team)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil && d.Sees(owner, team), err
}

// saveNote is a minimal handler: create at expected version 0, replace at the current version.
type saveNote struct {
	r        *rig
	id, body string
	expected int64
	found    bool
	version  int64
}

func (h *saveNote) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.found, h.version = false, 0
	err := exec.QueryRow(ctx, `SELECT row_version FROM `+h.r.schema+`.note WHERE tenant_id = $1 AND company_id = $2
		AND note_id = $3 FOR UPDATE`, string(h.r.scope.Tenant), string(h.r.scope.Company), h.id).Scan(&h.version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	h.found = err == nil
	return err
}

func (h *saveNote) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	if h.version != h.expected {
		return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	var err error
	if h.found {
		_, err = exec.Exec(ctx, `UPDATE `+h.r.schema+`.note SET body = $4, row_version = row_version + 1
			WHERE tenant_id = $1 AND company_id = $2 AND note_id = $3`, string(h.r.scope.Tenant), string(h.r.scope.Company), h.id, h.body)
	} else {
		_, err = exec.Exec(ctx, `INSERT INTO `+h.r.schema+`.note VALUES ($1, $2, $3, $4, 1, $5, nullif($6, ''))`,
			string(h.r.scope.Tenant), string(h.r.scope.Company), h.id, h.body, d.Actor.ID, d.Team)
	}
	v := h.version + 1
	return action.Outcome{ObjectID: h.id, RowVersion: v, Status: "draft", Events: []action.Event{{
		Type: "idp-brant.note.note_saved.v1", AggregateType: "Note", AggregateID: h.id, AggregateVersion: v,
	}}}, err
}

func (r *rig) save(ctx context.Context, key, id, body string, expected int64) (action.Result, error) {
	hash, err := action.RequestHash(struct{ ID, Body string }{id, body})
	if err != nil {
		r.t.Fatal(err)
	}
	return r.ledger.Execute(ctx, action.Command{Name: "note.save", IdempotencyKey: key, RequestHash: hash, ObjectID: id},
		&saveNote{r: r, id: id, body: body, expected: expected})
}

func code(err error) string {
	if rej, ok := kernel.AsRejection(err); ok {
		return rej.Code
	}
	if err != nil {
		return "ERROR: " + err.Error()
	}
	return ""
}

func (r *rig) audit() []action.AuditEntry {
	r.t.Helper()
	var out []action.AuditEntry
	r.admin(func(ctx context.Context, exec bentopg.Executor) error {
		rows, err := exec.Query(ctx, `SELECT actor_id, command, coalesce(object_id, ''), outcome, coalesce(code, ''),
			coalesce(authority_epoch, 0) FROM `+r.schema+`.audit_event ORDER BY seq`)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (action.AuditEntry, error) {
			var e action.AuditEntry
			err := row.Scan(&e.Actor.ID, &e.Command, &e.ObjectID, &e.Outcome, &e.Code, &e.AuthorityEpoch)
			return e, err
		})
		return err
	})
	return out
}

func (r *rig) count(table string) int {
	r.t.Helper()
	var n int
	r.admin(func(ctx context.Context, exec bentopg.Executor) error {
		return exec.QueryRow(ctx, `SELECT count(*) FROM `+r.schema+`.`+table).Scan(&n)
	})
	return n
}

func (r *rig) events() int {
	r.t.Helper()
	var n int
	r.admin(func(ctx context.Context, exec bentopg.Executor) error {
		return exec.QueryRow(ctx, `SELECT count(*) FROM `+r.db.Schema()+`.outbox WHERE source = 'idp-brant.note'
			AND scope = $1`, r.scope.String()).Scan(&n)
	})
	return n
}

func TestCommandCommitsResultAuditAndEventTogether(t *testing.T) {
	r := newRig(t)
	res, err := r.save(r.as(human), "k1", "n1", "hello", 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Replayed || res.RowVersion != 1 || res.Status != "draft" || !strings.HasPrefix(res.ActionID, "act-") {
		t.Fatalf("result %+v", res)
	}
	if r.count("action_result") != 1 || r.events() != 1 {
		t.Fatalf("want one action result and one event, got %d and %d", r.count("action_result"), r.events())
	}
	a := r.audit()
	if len(a) != 1 || a[0].Outcome != "succeeded" || a[0].AuthorityEpoch < 1 || a[0].ObjectID != "n1" {
		t.Fatalf("audit %+v", a)
	}
}

func TestSameKeyReplaysAndDifferentPayloadConflicts(t *testing.T) {
	r := newRig(t)
	ctx := r.as(human)
	first, err := r.save(ctx, "same", "n1", "hello", 0)
	if err != nil {
		t.Fatal(err)
	}
	// A different expected version is not part of the request hash.
	second, err := r.save(ctx, "same", "n1", "hello", 7)
	if err != nil || !second.Replayed || second.ActionID != first.ActionID || second.RowVersion != 1 {
		t.Fatalf("replay %+v, %v", second, err)
	}
	if _, err = r.save(ctx, "same", "n1", "changed", 0); code(err) != kernel.CodeIdempotencyPayloadConflict {
		t.Fatalf("want IDEMPOTENCY_PAYLOAD_CONFLICT, got %v", err)
	}
	if r.count("note") != 1 || r.events() != 1 {
		t.Fatal("a replay must not repeat the change or the event")
	}
	a := r.audit()
	if got := []string{a[0].Outcome, a[1].Outcome, a[2].Code}; got[0] != "succeeded" || got[1] != "replayed" || got[2] != kernel.CodeIdempotencyPayloadConflict {
		t.Fatalf("audit outcomes %v", got)
	}
}

// Python test_revocation_is_current_and_blocks_replay.
func TestRevocationBlocksReplayAndIsAudited(t *testing.T) {
	r := newRig(t)
	if _, err := r.save(r.as(human), "k", "n1", "hello", 0); err != nil {
		t.Fatal(err)
	}
	r.grant(human, "note.read")
	if _, err := r.save(r.as(human), "k", "n1", "hello", 0); code(err) != kernel.CodePermissionDenied {
		t.Fatalf("want PERMISSION_DENIED, got %v", err)
	}
	if a := r.audit(); a[len(a)-1].Code != kernel.CodePermissionDenied {
		t.Fatalf("last audit %+v", a[len(a)-1])
	}
}

func TestHumanOnlyRuleRefusesAIBeforeTheDatabase(t *testing.T) {
	r := newRig(t)
	hash, _ := action.RequestHash("x")
	_, err := r.ledger.Execute(r.as(robot), action.Command{Name: "note.approve", IdempotencyKey: "k", RequestHash: hash, ObjectID: "n1"},
		&saveNote{r: r, id: "n1"})
	if code(err) != kernel.CodePermissionDenied || !errors.Is(err, authorization.ErrActorKindDenied) {
		t.Fatalf("want PERMISSION_DENIED for an AI actor, got %v", err)
	}
}

// Another actor holding the same key gets the stored result only for an object it sees.
func TestReplayRechecksVisibility(t *testing.T) {
	r := newRig(t)
	r.grant(other, "note.save", "note.read")
	if _, err := r.save(r.as(human), "k", "n1", "hello", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := r.save(r.as(other), "k", "n1", "hello", 0); code(err) != kernel.CodeNotFound {
		t.Fatalf("want NOT_FOUND for an object outside the data scope, got %v", err)
	}
	r.admin(func(ctx context.Context, exec bentopg.Executor) error {
		_, err := authorization.Grants{}.SetDataScope(ctx, exec, r.scope, other.ID, authorization.DataScopeCompany, "", time.Now())
		return err
	})
	if res, err := r.save(r.as(other), "k", "n1", "hello", 0); err != nil || !res.Replayed {
		t.Fatalf("company data scope sees the note: %+v, %v", res, err)
	}
}

type failingOutbox struct{}

func (failingOutbox) Enqueue(context.Context, eventing.Envelope) error {
	return errors.New("injected outbox failure")
}

// Python test_rollback_when_storage_rejects_outbox_write.
func TestOutboxFailureRollsBackEverything(t *testing.T) {
	r := newRig(t)
	working := r.ledger.Outbox
	r.ledger.Outbox = failingOutbox{}
	_, err := r.save(r.as(human), "atomic", "n1", "hello", 0)
	if err == nil || !strings.Contains(err.Error(), "injected outbox failure") {
		t.Fatalf("want the injected failure, got %v", err)
	}
	if r.count("note") != 0 || r.count("action_result") != 0 {
		t.Fatal("a failed command left business data or an action result behind")
	}
	for _, e := range r.audit() {
		if e.Outcome == "succeeded" {
			t.Fatalf("a failed command was audited as succeeded: %+v", e)
		}
	}
	r.ledger.Outbox = working
	res, err := r.save(r.as(human), "atomic", "n1", "hello", 0)
	if err != nil || res.Replayed {
		t.Fatalf("the same key after a rollback is a new command: %+v, %v", res, err)
	}
}

// Python test_concurrent_duplicate_import_is_single_action and
// test_concurrent_edit_has_one_winner_and_one_version_conflict.
func TestConcurrentCommands(t *testing.T) {
	r := newRig(t)
	ctx := r.as(human)
	run := func(n int, f func(i int) (action.Result, error)) ([]action.Result, []error) {
		res, errs := make([]action.Result, n), make([]error, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() { defer wg.Done(); res[i], errs[i] = f(i) }()
		}
		wg.Wait()
		return res, errs
	}
	res, errs := run(2, func(int) (action.Result, error) { return r.save(ctx, "dup", "n1", "hello", 0) })
	if errs[0] != nil || errs[1] != nil || res[0].ActionID != res[1].ActionID || res[0].Replayed == res[1].Replayed {
		t.Fatalf("duplicate key: %+v %v", res, errs)
	}
	res, errs = run(2, func(i int) (action.Result, error) {
		return r.save(ctx, "edit."+string(rune('a'+i)), "n1", "edit "+string(rune('a'+i)), 1)
	})
	won := 0
	for i := range errs {
		switch {
		case errs[i] == nil && res[i].RowVersion == 2:
			won++
		case code(errs[i]) != kernel.CodeVersionConflict:
			t.Fatalf("loser got %v", errs[i])
		}
	}
	if won != 1 {
		t.Fatalf("want one winner, got %d: %+v %v", won, res, errs)
	}
	res, errs = run(2, func(i int) (action.Result, error) {
		return r.save(ctx, "create."+string(rune('a'+i)), "n2", "new", 0)
	})
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("concurrent creation: want one success and one VERSION_CONFLICT, got %v", errs)
	}
	for _, err := range errs {
		if err != nil && code(err) != kernel.CodeVersionConflict {
			t.Fatalf("loser got %v", err)
		}
	}
}

func TestReadIsAuditedAndDeniedReadToo(t *testing.T) {
	r := newRig(t)
	r.grant(other, "note.save")
	if _, err := r.save(r.as(human), "k", "n1", "hello", 0); err != nil {
		t.Fatal(err)
	}
	read := func(ctx context.Context) error {
		return r.ledger.Read(ctx, "note.get", "n1", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
			ok, err := r.visible(ctx, exec, d, "note.get", "n1")
			if err == nil && !ok {
				err = kernel.Reject(kernel.CodeNotFound, "/id", "")
			}
			return err
		})
	}
	if err := read(r.as(human)); err != nil {
		t.Fatal(err)
	}
	if err := read(r.as(other)); code(err) != kernel.CodePermissionDenied {
		t.Fatalf("want PERMISSION_DENIED, got %v", err)
	}
	a := r.audit()
	if a[1].Outcome != "read" || a[2].Code != kernel.CodePermissionDenied || a[2].Actor.ID != other.ID {
		t.Fatalf("audit %+v", a)
	}
	entries, err := r.ledger.Audit(r.as(human), "note.get")
	if err != nil || len(entries) != 3 {
		t.Fatalf("audit read: %d entries, %v", len(entries), err)
	}
}
