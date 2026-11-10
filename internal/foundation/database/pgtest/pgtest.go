// Package pgtest connects integration tests to PostgreSQL 16. A test that needs the database
// skips when IDP_BRANT_POSTGRES_DSN is unset, except in CI, where a missing database fails it.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.idp.xyz/idp-brant/internal/foundation/database"
)

const (
	AppDSNEnv      = "IDP_BRANT_POSTGRES_DSN"
	MigratorDSNEnv = "IDP_BRANT_MIGRATOR_DSN"
)

func dsn(t testing.TB, env string) string {
	t.Helper()
	v := os.Getenv(env)
	if v == "" {
		if os.Getenv("CI") != "" {
			t.Fatalf("%s is required in CI; skipping is forbidden", env)
		}
		t.Skipf("set %s to run against PostgreSQL 16", env)
	}
	return v
}

// AppDSN returns the DSN of the application role, for tests that open Brant through bootstrap.
func AppDSN(t testing.TB) string {
	t.Helper()
	return dsn(t, AppDSNEnv)
}

// AppPool returns a pool for the application role after database.Open's server and role checks.
func AppPool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := database.Open(ctx, dsn(t, AppDSNEnv))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// MigratorPool returns a pool for the migrator role, which provisions throwaway schemas.
func MigratorPool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn(t, MigratorDSNEnv))
	if err == nil {
		err = database.CheckServer(ctx, pool)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// UniqueID returns prefix followed by 12 random hex digits, for identifiers and schema names
// that must not collide between test runs.
func UniqueID(t testing.TB, prefix string) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return prefix + hex.EncodeToString(b)
}

// Schema creates a throwaway schema as the migrator, runs ddl with search_path set to it, grants
// the application role USAGE on it, and drops it with everything in it when the test ends. ddl
// grants the table privileges the test needs; in ddl, {{app}} is replaced by the quoted
// application role and {{schema}} by the quoted schema name.
func Schema(t testing.TB, migrator *pgxpool.Pool, app *pgxpool.Pool, prefix, ddl string) string {
	t.Helper()
	return SchemaFunc(t, migrator, app, prefix, func(string) string { return ddl })
}

// SchemaFunc is Schema with the DDL built from the unquoted schema name, for DDL that a library
// renders for a given schema.
func SchemaFunc(t testing.TB, migrator *pgxpool.Pool, app *pgxpool.Pool, prefix string, render func(name string) string) string {
	t.Helper()
	ctx := context.Background()
	name := UniqueID(t, prefix)
	ddl := render(name)
	var role string
	if err := app.QueryRow(ctx, "SELECT current_user").Scan(&role); err != nil {
		t.Fatal(err)
	}
	schema, quotedRole := pgx.Identifier{name}.Sanitize(), pgx.Identifier{role}.Sanitize()
	conn, err := migrator.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	setup := "CREATE SCHEMA " + schema + "; SET LOCAL search_path TO " + schema + "; " +
		strings.NewReplacer("{{app}}", quotedRole, "{{schema}}", schema).Replace(ddl) +
		"; GRANT USAGE ON SCHEMA " + schema + " TO " + quotedRole
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit
	if _, err = tx.Exec(ctx, setup, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatalf("provision schema %s: %v", name, err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := migrator.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop schema %s: %v", name, err)
		}
	})
	return name
}
