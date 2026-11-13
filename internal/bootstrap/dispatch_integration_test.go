//go:build integration

package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.idp.xyz/idp-bento-go/eventing"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// throwawayBento provisions a Bento schema of its own for a test, so the relay claims only the
// test's events.
func throwawayBento(t *testing.T, migrator, app *pgxpool.Pool) string {
	t.Helper()
	return pgtest.SchemaFunc(t, migrator, app, "brant_t_dispatch_", func(name string) string {
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
}

// outboxFixture enqueues events into and reads their status from a throwaway Bento schema.
type outboxFixture struct {
	t     *testing.T
	db    *bentopg.DB
	store *bentooutbox.Store
}

func newOutboxFixture(t *testing.T, app *pgxpool.Pool, schema string) outboxFixture {
	t.Helper()
	db, err := bentopg.NewDB(app, bentopg.WithSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	store, err := bentooutbox.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	return outboxFixture{t: t, db: db, store: store}
}

func (f outboxFixture) enqueue(e eventing.Envelope) {
	f.t.Helper()
	if err := f.db.Transactor().WithinTransaction(context.Background(), func(ctx context.Context) error { return f.store.Enqueue(ctx, e) }); err != nil {
		f.t.Fatal(err)
	}
}

func (f outboxFixture) status(id eventing.EventID) string {
	f.t.Helper()
	q, err := f.db.ReadExecutor(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	var status string
	if err = q.QueryRow(context.Background(), `SELECT status FROM `+pgx.Identifier{f.db.Schema()}.Sanitize()+`.outbox WHERE event_id = $1`,
		string(id)).Scan(&status); err != nil {
		f.t.Fatal(err)
	}
	return status
}

func partyConfirmed(scope kernel.Scope, id string) eventing.Envelope {
	at := kernel.Instant(time.Now().Add(-time.Minute))
	return eventing.Envelope{
		SpecVersion: eventing.SpecVersion, ID: eventing.EventID(id), Source: ptyapi.Source, Type: ptyapi.EventPartyConfirmed, Version: 1,
		Scope: scope.String(), Subject: "party.1", PartitionKey: scope.String() + "/Party:party.1-" + id,
		OccurredAt: at, RecordedAt: at, CorrelationID: "corr.dispatch", ContentType: eventing.JSONContentType,
		Payload: json.RawMessage(`{"aggregate_type":"Party","aggregate_id":"party.1","aggregate_version":2}`),
	}
}

// The assembled relay and replayer dispatch in process to a consumer on a transaction owner of its
// own. The consumer's command is denied until the operator provisions its system actor
// with what its subscription declares (Platform.ProvisionInbox, brant-admin inbox); then it runs
// once, as system:inbox.<consumer> with data scope company, and a redelivery runs nothing. A
// delivery dead-lettered while the consumer was broken is replayed through the dispatcher and
// reaches it once.
func TestAssembledDispatchRunsProvisionedConsumerOnceAndReplays(t *testing.T) {
	migrator, app := pgtest.MigratorPool(t), pgtest.AppPool(t)
	schema := throwawayBento(t, migrator, app)
	ctx := context.Background()
	platform, err := Open(ctx, Config{PostgresDSN: os.Getenv(pgtest.AppDSNEnv), BentoSchema: schema})
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	c, err := platform.Contexts()
	if err != nil {
		t.Fatal(err)
	}
	scope := kernel.Scope{Tenant: kernel.TenantID(pgtest.UniqueID(t, "tenant.")), Company: kernel.CompanyID(pgtest.UniqueID(t, "company."))}
	operator := kernel.ActorRef{Kind: kernel.ActorHuman, ID: "operator"}
	if _, err = platform.Provision(ctx, Grant{Scope: scope, Actor: operator, Permissions: []string{messaging.ReplayPermission}}); err != nil {
		t.Fatal(err)
	}

	policy, err := authorization.NewPolicy(authorization.Rule{Command: "probe.apply", Permission: "probe.apply", ActorKinds: []kernel.ActorKind{kernel.ActorSystem}})
	if err != nil {
		t.Fatal(err)
	}
	auth := authorization.Authorizer{Policy: policy}
	probeDB, err := platform.newDB() // the consuming context's own transaction owner
	if err != nil {
		t.Fatal(err)
	}
	var broken atomic.Bool
	var mu sync.Mutex
	applied := map[eventing.EventID]int{}
	var decisions []authorization.Decision
	probe := messaging.Subscription{
		Consumer: "inq.dispatch_probe", Source: ptyapi.Source, EventTypes: []eventing.EventType{ptyapi.EventPartyConfirmed},
		DB: probeDB, Permissions: []string{"probe.apply"},
		Handler: func(ctx context.Context, e messaging.Event) error {
			d, err := auth.Authorize(ctx, e.Exec, "probe.apply")
			if err != nil {
				return err
			}
			if broken.Load() {
				return errors.New("probe broken")
			}
			mu.Lock()
			defer mu.Unlock()
			applied[e.Envelope.ID]++
			decisions = append(decisions, d)
			return nil
		},
	}
	dispatcher, err := messaging.NewDispatcher(probe) // only the probe: the contexts' own consumers never see its synthetic events
	if err != nil {
		t.Fatal(err)
	}
	outbox := newOutboxFixture(t, app, schema)
	recorded := &alerts{}
	relay := func(maxFailures uint32) messaging.Relay {
		r, err := platform.Relay(RelayConfig{Publisher: dispatcher, Batch: 1, LeaseFor: 30 * time.Second, PublishTimeout: 5 * time.Second,
			MaxAttempts: 10, MaxFailures: maxFailures, RetryBase: time.Millisecond, RetryMax: time.Millisecond, IdleWait: time.Second,
			Alerter: recorded})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	runOnce := func(r messaging.Relay) {
		t.Helper()
		time.Sleep(5 * time.Millisecond) // past the retry delay
		if n, err := r.RunOnce(ctx); err != nil || n != 1 {
			t.Fatalf("RunOnce = %d, %v", n, err)
		}
	}

	first := partyConfirmed(scope, "evt-dispatch-1")
	outbox.enqueue(first)
	runOnce(relay(5))
	if s := outbox.status(first.ID); s == "PUBLISHED" || len(applied) != 0 {
		t.Fatalf("before provisioning: outbox %s, applied %v; want the consumer denied and the delivery retried", s, applied)
	}
	epoch, err := platform.ProvisionInbox(ctx, scope, dispatcher.Subscriptions())
	if err != nil || epoch < 1 {
		t.Fatalf("ProvisionInbox = %d, %v", epoch, err)
	}
	if again, err := platform.ProvisionInbox(ctx, scope, dispatcher.Subscriptions()); err != nil || again != epoch {
		t.Fatalf("second ProvisionInbox = %d, %v; want epoch %d unchanged", again, err, epoch)
	}
	runOnce(relay(5))
	if s := outbox.status(first.ID); s != "PUBLISHED" {
		t.Fatalf("after provisioning: outbox %s", s)
	}
	if err = dispatcher.Publish(ctx, first); err != nil { // a redelivery
		t.Fatal(err)
	}
	actor := kernel.ActorRef{Kind: kernel.ActorSystem, ID: "inbox.inq.dispatch_probe"}
	if applied[first.ID] != 1 || len(decisions) != 1 || decisions[0].Actor != actor || decisions[0].DataScope != authorization.DataScopeCompany ||
		decisions[0].Epoch != epoch {
		t.Fatalf("applied %v with decisions %+v; want once as %s with data scope company at epoch %d", applied, decisions, actor, epoch)
	}

	broken.Store(true)
	second := partyConfirmed(scope, "evt-dispatch-2")
	outbox.enqueue(second)
	runOnce(relay(1))
	if s := outbox.status(second.ID); s != "ABANDONED" || len(recorded.list) != 1 || !recorded.list[0].Stored ||
		recorded.list[0].Code != messaging.FailurePublishFailed {
		t.Fatalf("broken consumer: outbox %s, alerts %+v; want one stored dead letter", s, recorded.list)
	}
	broken.Store(false)
	replayer, err := platform.Replayer(ReplayConfig{Publisher: dispatcher, PublishTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	opCtx, err := identity.WithPrincipal(ctx, identity.Principal{Actor: operator, Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = replayer.Replay(opCtx, second.Source, string(second.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err = replayer.Replay(opCtx, second.Source, string(second.ID)); !errors.Is(err, messaging.ErrAlreadyReplayed) {
		t.Fatalf("second replay: %v", err)
	}
	if applied[second.ID] != 1 || len(applied) != 2 {
		t.Fatalf("applied %v; want the replayed event once", applied)
	}

	// brant-admin inbox provisions the consumers the composed contexts declare.
	want, err := messaging.InboxGrants(c.Subscriptions())
	if err != nil {
		t.Fatal(err)
	}
	if grants, _, err := platform.ProvisionInboxFor(ctx, string(scope.Tenant), string(scope.Company)); err != nil || len(grants) != len(want) {
		t.Fatalf("ProvisionInboxFor = %+v, %v; want %d consumers", grants, err, len(want))
	}
}

// brant-worker -dispatch relays through the dispatcher of every composed context: an event
// nobody subscribes to is settled as delivered, and SIGTERM stops it. -sink and -dispatch
// together are refused.
func TestWorkerDispatchModeRelaysInProcess(t *testing.T) {
	migrator, app := pgtest.MigratorPool(t), pgtest.AppPool(t)
	schema := throwawayBento(t, migrator, app)
	outbox := newOutboxFixture(t, app, schema)
	scope := kernel.Scope{Tenant: kernel.TenantID(pgtest.UniqueID(t, "tenant.")), Company: kernel.CompanyID(pgtest.UniqueID(t, "company."))}
	event := partyConfirmed(scope, "evt-worker-dispatch")
	event.Type = "idp-brant.pty.worker_dispatch_probe.v1" // no context subscribes to it
	outbox.enqueue(event)

	bin := filepath.Join(t.TempDir(), "brant-worker")
	if out, err := exec.Command("go", "build", "-o", bin, "go.idp.xyz/idp-brant/cmd/brant-worker").CombinedOutput(); err != nil {
		t.Fatalf("build brant-worker: %v\n%s", err, out)
	}
	both := exec.Command(bin, "-schema", schema, "-sink", "http://127.0.0.1:1/sink", "-dispatch")
	both.Env = os.Environ()
	if out, err := both.CombinedOutput(); err == nil || !strings.Contains(string(out), "one of -sink and -dispatch") {
		t.Fatalf("-sink with -dispatch: %v\n%s", err, out)
	}

	var logs bytes.Buffer
	worker := exec.Command(bin, "-schema", schema, "-dispatch", "-batch", "1", "-lease", "3s", "-sink-timeout", "2s", "-poll", "100ms")
	worker.Env = os.Environ()
	worker.Stdout, worker.Stderr = &logs, &logs
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for outbox.status(event.ID) != "PUBLISHED" {
		if time.Now().After(deadline) {
			_ = worker.Process.Kill()
			t.Fatalf("the dispatching worker did not settle the event:\n%s", logs.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := worker.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := worker.Wait(); err != nil {
		t.Fatalf("graceful shutdown: %v\n%s", err, logs.String())
	}
	if !strings.Contains(logs.String(), "dispatch=true") {
		t.Fatalf("worker logs lack the dispatch mode:\n%s", logs.String())
	}
	t.Log(fmt.Sprint(strings.TrimSpace(logs.String())))
}
