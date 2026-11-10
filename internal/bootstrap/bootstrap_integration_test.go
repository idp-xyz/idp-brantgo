//go:build integration

package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.idp.xyz/idp-bento-go/eventing"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
	"go.idp.xyz/idp-brant/internal/kernel"
)

type alerts struct {
	mu   sync.Mutex
	list []messaging.DeadLetterAlert
}

func (a *alerts) Alert(_ context.Context, alert messaging.DeadLetterAlert) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.list = append(a.list, alert)
}

// The assembled relay abandons a rejected delivery and stores its dead letter through one
// transaction owner; the assembled replayer republishes it for a human operator.
func TestAssembledRelayDeadLettersAndReplayerReplays(t *testing.T) {
	migrator, app := pgtest.MigratorPool(t), pgtest.AppPool(t)
	schema := pgtest.SchemaFunc(t, migrator, app, "brant_t_boot_", func(name string) string {
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
	ctx := context.Background()
	platform, err := Open(ctx, Config{PostgresDSN: os.Getenv(pgtest.AppDSNEnv), BentoSchema: schema})
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()

	scope := kernel.Scope{Tenant: kernel.TenantID(pgtest.UniqueID(t, "tenant.")), Company: kernel.CompanyID(pgtest.UniqueID(t, "company."))}
	operator := kernel.ActorRef{Kind: kernel.ActorHuman, ID: "operator"}
	db, err := bentopg.NewDB(app, bentopg.WithSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	err = bento.ScopeTransactor{DB: db, Scope: scope}.WithinTransaction(ctx, func(txCtx context.Context) error {
		exec, err := db.RequireExecutor(txCtx)
		if err != nil {
			return err
		}
		g := authorization.Grants{}
		if err = g.Provision(txCtx, exec, scope); err != nil {
			return err
		}
		_, err = g.Put(txCtx, exec, scope, operator, []string{messaging.ReplayPermission}, time.Now())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := bentooutbox.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	at := kernel.Instant(time.Now().Add(-time.Minute))
	event := eventing.Envelope{
		SpecVersion: eventing.SpecVersion, ID: "boot-1", Source: "idp-brant.boot-probe", Type: "brant.boot_probe.qualification_revoked",
		Version: 1, Scope: scope.String(), Subject: "probe/boot-1", PartitionKey: "boot-1",
		OccurredAt: at, RecordedAt: at, ContentType: eventing.JSONContentType, Payload: json.RawMessage(`{"probe":true}`),
	}
	if err = db.Transactor().WithinTransaction(ctx, func(txCtx context.Context) error { return store.Enqueue(txCtx, event) }); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var keys []string
	accept := false
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		if accept {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
	}))
	defer sink.Close()

	recorded := &alerts{}
	relay, err := platform.Relay(RelayConfig{
		SinkURL: sink.URL, Batch: 1, LeaseFor: 30 * time.Second, PublishTimeout: 5 * time.Second,
		MaxAttempts: 5, MaxFailures: 3, RetryBase: time.Second, RetryMax: time.Minute, IdleWait: time.Second, Alerter: recorded,
	})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := relay.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("RunOnce = %d, %v", n, err)
	}
	if len(recorded.list) != 1 || !recorded.list[0].Stored || !recorded.list[0].Critical || recorded.list[0].Code != messaging.FailurePublishRejected {
		t.Fatalf("alerts %+v, want one stored critical rejection", recorded.list)
	}

	replayer, err := platform.Replayer(ReplayConfig{SinkURL: sink.URL, PublishTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	accept = true
	mu.Unlock()
	opCtx, err := identity.WithPrincipal(ctx, identity.Principal{Actor: operator, Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	result, err := replayer.Replay(opCtx, event.Source, string(event.ID))
	if err != nil || result.EventID != event.ID || result.ReplayedBy != operator || result.AuthorityEpoch < 1 {
		t.Fatalf("Replay = %+v, %v", result, err)
	}
	if _, err = replayer.Replay(opCtx, event.Source, string(event.ID)); !errors.Is(err, messaging.ErrAlreadyReplayed) {
		t.Fatalf("second Replay = %v, want ErrAlreadyReplayed", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 2 || keys[0] != string(event.ID) || keys[1] != string(event.ID) {
		t.Fatalf("Idempotency-Keys %v, want the event ID on delivery and replay", keys)
	}
}
