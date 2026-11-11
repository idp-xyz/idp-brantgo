//go:build integration

package bootstrap_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The S1 suite drives the composed contexts through their api packages against the migrated
// PostgreSQL 16 database. Each test runs in a fresh tenant with company.A and company.B, so tests
// never see each other's data. Test names cite the Python V0.50 test they port.

// every S1 permission, SUP's, CST's and TRD's, Python's "*".
var allPermissions = []string{
	"source.register", "file.register", "evd.read", "evd.audit",
	"party.save_draft", "party.confirm", "party.revise", "party.deactivate", "contact.save_draft", "contact.confirm", "contact.revise",
	"service_role.verify", "trading_profile.save_draft", "trading_profile.publish",
	"pty.read", "pty.audit",
	"sku.save_draft", "sku.confirm", "sku.revise_spec", "sku.deactivate", "item_code_mapping.save_draft", "item_code_mapping.publish",
	"cat.read", "cat.audit",
	"config.save_draft", "config.discard_draft", "config.publish", "config.retire", "number.issue", "foundation.snapshot",
	"pol.read", "pol.audit", "pol.read_sensitive",
	"rfq.save_draft", "rfq.confirm_scope", "rfq.cancel", "rfq.close", "inq.read", "inq.audit",
	"party_screening.record", "party_screening.decide", "party_screening.block",
	"screening_list.import", "ref.read", "ref.audit",
	"costing_context.save_draft", "costing_context.revise", "costing_context.discard_draft", "costing_context.verify",
	"costing_context.withdraw", "measurement.record_verified", "measurement.supersede", "measurement.withdraw", "cst.read", "cst.audit",
	"cost_scenario.save_draft", "cost_scenario.evaluate", "cost_snapshot.freeze", "costing.attest_source",
	"supplier_rfq.save_draft", "supplier_rfq.record_issued", "supplier_rfq.close",
	"supplier_offer.save_draft", "supplier_offer.revise", "supplier_offer.discard_draft", "supplier_offer.verify", "supplier_offer.withdraw",
	"purchase_terms.save_draft", "purchase_terms.revise", "purchase_terms.discard_draft", "purchase_terms.publish", "purchase_terms.retire",
	"logistics_offer.save_draft", "logistics_offer.revise", "logistics_offer.discard_draft", "logistics_offer.verify",
	"logistics_offer.withdraw", "sup.read", "sup.audit",
	"quote.save_draft", "quote.revise", "quote.discard_draft", "quote.freeze", "quote.metadata.update",
	"object_control.bind", "object_control.stop", "object_control.release", "trd.read", "trd.audit",
	"quote.submit_review", "quote.review.decide", "quote.approval.revoke", "quote.margin_exception.approve",
	"quote.activate", "quote.replace", "quote.withdraw", "quote.cancel",
	"quote.response.record", "quote.response.verify", "acceptance.invalidate", "acceptance.mark_disputed",
}

type world struct {
	t        *testing.T
	platform *bootstrap.Platform
	c        *bootstrap.Contexts
	tenant   string
	serial   int
}

func newWorld(t *testing.T) *world {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	platform, err := bootstrap.Open(ctx, bootstrap.Config{PostgresDSN: pgtest.AppDSN(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(platform.Close)
	c, err := platform.Contexts()
	if err != nil {
		t.Fatal(err)
	}
	return &world{t: t, platform: platform, c: c, tenant: pgtest.UniqueID(t, "tenant.")}
}

func (w *world) scope(company string) kernel.Scope {
	return kernel.Scope{Tenant: kernel.TenantID(w.tenant), Company: kernel.CompanyID(company)}
}

// actor provisions a human actor in company.A with permissions and returns its request context.
func (w *world) actor(id string, permissions ...string) context.Context {
	return w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: id}, Permissions: permissions})
}

func (w *world) provision(g bootstrap.Grant) context.Context {
	w.t.Helper()
	if g.Permissions == nil {
		g.Permissions = []string{}
	}
	if _, err := w.platform.Provision(context.Background(), g); err != nil {
		w.t.Fatal(err)
	}
	ctx, err := identity.WithPrincipal(context.Background(), identity.Principal{Actor: g.Actor, Scope: g.Scope})
	if err != nil {
		w.t.Fatal(err)
	}
	return action.WithCorrelation(ctx, pgtest.UniqueID(w.t, "corr."))
}

// key returns a fresh idempotency key, like the Python tests' k1, k2, ...
func (w *world) key() string {
	w.serial++
	return "k" + string(rune('0'+w.serial/100%10)) + string(rune('0'+w.serial/10%10)) + string(rune('0'+w.serial%10))
}

func code(err error) string {
	if rej, ok := kernel.AsRejection(err); ok {
		return rej.Code
	}
	if err != nil {
		return "ERROR: " + err.Error()
	}
	return "OK"
}

func path(err error) string {
	if rej, ok := kernel.AsRejection(err); ok && len(rej.Fields) > 0 {
		return rej.Fields[0].Path
	}
	return ""
}

func (w *world) must(err error) {
	w.t.Helper()
	if err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) want(err error, wantCode string) {
	w.t.Helper()
	if got := code(err); got != wantCode {
		w.t.Fatalf("want %s, got %s", wantCode, got)
	}
}

func authorizationScope(s string) authorization.DataScope { return authorization.DataScope(s) }

func actorRef(kind, id string) kernel.ActorRef {
	return kernel.ActorRef{Kind: kernel.ActorKind(kind), ID: id}
}

// exec runs sql as the application role in company's scope.
func (w *world) exec(company, sql string) error {
	db, err := bentopg.NewDB(pgtest.AppPool(w.t))
	if err != nil {
		w.t.Fatal(err)
	}
	return bento.ScopeTransactor{DB: db, Scope: w.scope(company)}.WithinTransaction(context.Background(), func(ctx context.Context) error {
		exec, err := db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		_, err = exec.Exec(ctx, sql)
		return err
	})
}

type event struct {
	Type    string
	Payload map[string]any
}

// events returns the Outbox events of source in company, oldest first.
func (w *world) events(company, source string) []event {
	w.t.Helper()
	db, err := bentopg.NewDB(pgtest.AppPool(w.t))
	if err != nil {
		w.t.Fatal(err)
	}
	q, err := db.ReadExecutor(context.Background())
	if err != nil {
		w.t.Fatal(err)
	}
	rows, err := q.Query(context.Background(), `SELECT event_type, payload FROM bento.outbox WHERE source = $1 AND scope = $2
		ORDER BY enqueued_seq`, source, w.scope(company).String())
	if err != nil {
		w.t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (event, error) {
		var e event
		return e, row.Scan(&e.Type, &e.Payload)
	})
	if err != nil {
		w.t.Fatal(err)
	}
	return out
}

// count returns the rows of table in company, read as the application role through the scope's
// RLS.
func (w *world) count(company, table string) int {
	w.t.Helper()
	db, err := bentopg.NewDB(pgtest.AppPool(w.t))
	if err != nil {
		w.t.Fatal(err)
	}
	var n int
	err = bento.ScopeTransactor{DB: db, Scope: w.scope(company)}.WithinTransaction(context.Background(), func(ctx context.Context) error {
		exec, err := db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		return exec.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n)
	})
	if err != nil {
		w.t.Fatal(err)
	}
	return n
}
