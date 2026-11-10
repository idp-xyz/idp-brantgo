//go:build integration

package database_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"go.idp.xyz/idp-brant/internal/foundation/database"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
)

func TestApplicationRoleIsUnprivileged(t *testing.T) {
	ctx := context.Background()
	app := pgtest.AppPool(t) // database.Open runs both checks
	if err := database.CheckApplicationRole(ctx, app); err != nil {
		t.Fatal(err)
	}
	migrator := pgtest.MigratorPool(t)
	if err := database.CheckServer(ctx, migrator); err != nil {
		t.Fatal(err)
	}
	if err := database.CheckApplicationRole(ctx, migrator); !errors.Is(err, database.ErrPrivilegedRole) {
		t.Fatalf("the migrator owns the schemas and must be refused as the application role: %v", err)
	}
}

// The application role cannot change the schema or switch off row-level security.
func TestApplicationRoleCannotRunDDL(t *testing.T) {
	app := pgtest.AppPool(t)
	for _, ddl := range []string{
		"CREATE SCHEMA brant_t_forbidden",
		"CREATE TABLE brant_auth.forbidden (id text)",
		"ALTER TABLE brant_auth.actor_grant DISABLE ROW LEVEL SECURITY",
		"ALTER TABLE brant_auth.actor_grant NO FORCE ROW LEVEL SECURITY",
		"DROP POLICY actor_grant_scope ON brant_auth.actor_grant",
		"DROP TABLE brant_auth.actor_grant",
		"TRUNCATE brant_auth.actor_grant",
		"DELETE FROM brant_auth.actor_grant",
		"SELECT count(*) FROM brant_migrations.applied",
	} {
		_, err := app.Exec(context.Background(), ddl)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("%s: %v, want SQLSTATE 42501 (insufficient privilege)", ddl, err)
		}
	}
}
