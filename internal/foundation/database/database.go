// Package database opens Brant's PostgreSQL 16 connection pool and checks the server and the role
// the application runs as.
package database

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// SupportedMajor is the only PostgreSQL major version Brant runs on.
	SupportedMajor = 16
	// MigratorRole owns the schemas and runs migrations; the application role must not inherit it.
	MigratorRole = "brant_migrator"
)

var (
	ErrUnsupportedServer = errors.New("database: unsupported PostgreSQL server")
	ErrPrivilegedRole    = errors.New("database: application role is privileged")
)

// Querier is the part of pgxpool.Pool, pgx.Conn and pgx.Tx that the checks use.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Open connects with dsn and returns the pool once CheckServer and CheckApplicationRole pass.
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err = CheckServer(ctx, pool); err == nil {
		err = CheckApplicationRole(ctx, pool)
	}
	if err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// CheckServer requires PostgreSQL 16.
func CheckServer(ctx context.Context, q Querier) error {
	var major int
	if err := q.QueryRow(ctx, "SELECT current_setting('server_version_num')::int / 10000").Scan(&major); err != nil {
		return fmt.Errorf("database: read server version: %w", err)
	}
	if major != SupportedMajor {
		return fmt.Errorf("%w: major version %d, want %d", ErrUnsupportedServer, major, SupportedMajor)
	}
	return nil
}

// CheckApplicationRole refuses a role that is superuser, bypasses RLS, can create roles or
// databases, belongs to the migrator role or owns a schema. Each of these would let application
// queries escape FORCE RLS or change the schema.
func CheckApplicationRole(ctx context.Context, q Querier) error {
	var name string
	var super, bypass, createRole, createDB, migrator, owner bool
	err := q.QueryRow(ctx, `SELECT r.rolname, r.rolsuper, r.rolbypassrls, r.rolcreaterole, r.rolcreatedb,
		COALESCE(pg_catalog.pg_has_role(r.oid, (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = $1), 'MEMBER'), false),
		EXISTS (SELECT 1 FROM pg_catalog.pg_namespace n WHERE n.nspowner = r.oid)
		FROM pg_catalog.pg_roles r WHERE r.rolname = current_user`, MigratorRole).
		Scan(&name, &super, &bypass, &createRole, &createDB, &migrator, &owner)
	if err != nil {
		return fmt.Errorf("database: read role attributes: %w", err)
	}
	switch {
	case super, bypass:
		return fmt.Errorf("%w: %s is superuser or bypasses RLS", ErrPrivilegedRole, name)
	case createRole, createDB:
		return fmt.Errorf("%w: %s can create roles or databases", ErrPrivilegedRole, name)
	case migrator:
		return fmt.Errorf("%w: %s is a member of %s", ErrPrivilegedRole, name, MigratorRole)
	case owner:
		return fmt.Errorf("%w: %s owns a schema", ErrPrivilegedRole, name)
	}
	return nil
}
