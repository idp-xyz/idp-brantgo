//go:build integration

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
)

// An empty ledger applies everything, a second run applies nothing, and an edited migration that
// was already applied stops the run.
func TestApplyIsIdempotentAndRefusesTampering(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.MigratorPool(t)
	ledger := pgtest.UniqueID(t, "brant_t_ledger_")
	target := pgx.Identifier{pgtest.UniqueID(t, "brant_t_target_")}.Sanitize()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{ledger}.Sanitize()+" CASCADE")
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+target+" CASCADE")
	})
	plan := []migration{
		{ID: "brant:000001_t_schema", SQL: "CREATE SCHEMA " + target},
		{ID: "brant:000002_t_table", SQL: "CREATE TABLE " + target + ".item (id text PRIMARY KEY)"},
	}
	for i := range plan {
		plan[i].Checksum = checksum(plan[i].SQL)
	}

	if n, err := applyAll(ctx, pool, ledger, plan); err != nil || n != 2 {
		t.Fatalf("first run: %d applied, %v; want 2", n, err)
	}
	if n, err := applyAll(ctx, pool, ledger, plan); err != nil || n != 0 {
		t.Fatalf("second run: %d applied, %v; want 0", n, err)
	}
	var recorded int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{ledger, "applied"}.Sanitize()).Scan(&recorded); err != nil || recorded != 2 {
		t.Fatalf("ledger holds %d rows, %v; want 2", recorded, err)
	}

	edited := append([]migration(nil), plan...)
	edited[1].SQL += " -- edited"
	edited[1].Checksum = checksum(edited[1].SQL)
	if _, err := applyAll(ctx, pool, ledger, edited); err == nil || !strings.Contains(err.Error(), "migration tampering brant:000002_t_table") {
		t.Fatalf("edited migration: %v; want a tampering error", err)
	}

	failing := append(append([]migration(nil), plan...), migration{ID: "brant:000003_t_broken", SQL: "CREATE TABLE " + target + ".item (id text)"})
	failing[2].Checksum = checksum(failing[2].SQL)
	if _, err := applyAll(ctx, pool, ledger, failing); err == nil {
		t.Fatal("a failing migration must stop the run")
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{ledger, "applied"}.Sanitize()).Scan(&recorded); err != nil || recorded != 2 {
		t.Fatalf("a failed migration must not be recorded: %d rows, %v", recorded, err)
	}
}
