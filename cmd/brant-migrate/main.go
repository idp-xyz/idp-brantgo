// brant-migrate is an operator-only migration tool; the API and the worker never link it.
//
// It applies the IDP-Bento-Go framework migrations to the bento schema first, then the migrations
// of each directory that migrations/manifest/contexts.txt lists, in order. Every applied
// migration's SHA-256 is recorded in brant_migrations.applied; applied SQL is never rewritten, and
// a changed checksum stops the run.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
)

const (
	ledgerSchema = "brant_migrations"
	// Advisory lock that serializes migration runs: the ASCII of "bran" and "t".
	lockKey1, lockKey2 = 0x6272616e, 0x74
)

type migration struct {
	ID       string // ledger ID: framework:<Bento ID> or brant:<file stem>
	SQL      string
	Checksum string // hex SHA-256
}

func checksum(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "migration blocked:", err)
		os.Exit(1)
	}
}

func run() error {
	manifest := flag.String("manifest", "migrations/manifest/contexts.txt", "file listing the migration directories in order")
	apply := flag.Bool("apply", false, "apply to PostgreSQL 16 as the migrator role in IDP_BRANT_MIGRATOR_DSN")
	flag.Parse()
	dirs, err := readManifest(*manifest)
	if err != nil {
		return err
	}
	plan, err := frameworkMigrations()
	if err != nil {
		return err
	}
	for _, d := range dirs {
		items, err := loadDirectory(d)
		if err != nil {
			return err
		}
		plan = append(plan, items...)
	}
	for _, m := range plan {
		fmt.Printf("PLAN %-45s sha256:%s\n", m.ID, m.Checksum)
	}
	if !*apply {
		fmt.Println("DRY-RUN: no database connection or DDL executed")
		return nil
	}
	dsn := os.Getenv("IDP_BRANT_MIGRATOR_DSN")
	if dsn == "" {
		return errors.New("IDP_BRANT_MIGRATOR_DSN required")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return err
	}
	// The simple protocol runs versioned multi-statement DDL. No value is interpolated into it.
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	var major int
	var role string
	if err = pool.QueryRow(ctx, "SELECT current_setting('server_version_num')::int / 10000, current_user").Scan(&major, &role); err != nil {
		return err
	}
	if major != 16 {
		return fmt.Errorf("PostgreSQL major %d is not supported; Brant runs on 16", major)
	}
	fmt.Printf("CONNECTED PostgreSQL %d as %s\n", major, role)
	// Bento migrations assume their schema exists; creating it is the consumer's job.
	if _, err = pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+pgx.Identifier{bentopg.DefaultSchema}.Sanitize()); err != nil {
		return err
	}
	applied, err := applyAll(ctx, pool, ledgerSchema, plan)
	if err != nil {
		return err
	}
	db, err := bentopg.NewDB(pool)
	if err != nil {
		return err
	}
	if err = db.CheckSchema(ctx); err != nil {
		return fmt.Errorf("bento schema check: %w", err)
	}
	fmt.Printf("DONE: %d applied, %d already recorded; Bento schema check passed\n", applied, len(plan)-applied)
	return nil
}

// frameworkMigrations renders the Bento migrations for the bento schema. Their ledger checksum is
// the one Bento publishes, so a framework upgrade that changes an applied migration stops the run.
func frameworkMigrations() ([]migration, error) {
	var out []migration
	for _, m := range bentopg.Migrations() {
		sql, err := bentopg.RenderMigration(m.ID, bentopg.DefaultSchema)
		if err != nil {
			return nil, err
		}
		out = append(out, migration{ID: "framework:" + m.ID, SQL: sql, Checksum: strings.TrimPrefix(m.Checksum, "sha256:")})
	}
	return out, nil
}

// readManifest returns the directories a manifest lists, in order. Entries name directories under
// the migrations root, which is the parent of the manifest's directory.
func readManifest(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	root := filepath.Dir(filepath.Dir(path))
	seen := map[string]bool{}
	var dirs []string
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		name := strings.TrimSpace(line)
		if name == "" || strings.HasPrefix(name, "#") {
			continue
		}
		if seen[name] || strings.ContainsAny(name, `/\. `) {
			return nil, fmt.Errorf("manifest %s: invalid or repeated entry %q", path, name)
		}
		seen[name] = true
		dirs = append(dirs, filepath.Join(root, name))
	}
	if len(dirs) == 0 {
		return nil, fmt.Errorf("manifest %s lists no migration directories", path)
	}
	return dirs, nil
}

// loadDirectory reads one directory's migrations. Files are numbered from 000001 without gaps and
// named NNNNNN_<directory>_<topic>.sql, which keeps ledger IDs unique across directories.
func loadDirectory(dir string) ([]migration, error) {
	name := filepath.Base(dir)
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no migrations in %s", dir)
	}
	sort.Strings(files)
	var items []migration
	for i, file := range files {
		id := strings.TrimSuffix(filepath.Base(file), ".sql")
		if !strings.HasPrefix(id, fmt.Sprintf("%06d_%s_", i+1, name)) {
			return nil, fmt.Errorf("noncontiguous or misnamed migration: %s", id)
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		body := strings.ReplaceAll(string(raw), "\r\n", "\n")
		if strings.TrimSpace(body) == "" {
			return nil, fmt.Errorf("empty migration %s", id)
		}
		items = append(items, migration{ID: "brant:" + id, SQL: body, Checksum: checksum(body)})
	}
	return items, nil
}

// applyAll records the ledger in schema ledger and applies each migration that it does not hold
// yet, in order. It returns how many it applied.
func applyAll(ctx context.Context, pool *pgxpool.Pool, ledger string, plan []migration) (int, error) {
	table := pgx.Identifier{ledger, "applied"}.Sanitize()
	if _, err := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+pgx.Identifier{ledger}.Sanitize()); err != nil {
		return 0, err
	}
	if _, err := pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+table+
		" (id text PRIMARY KEY, sha256 text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())"); err != nil {
		return 0, err
	}
	applied := 0
	for _, m := range plan {
		done, err := applyMigration(ctx, pool, table, m)
		if err != nil {
			return applied, err
		}
		if done {
			applied++
		}
	}
	return applied, nil
}

func applyMigration(ctx context.Context, pool *pgxpool.Pool, table string, m migration) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit
	if _, err = tx.Exec(ctx, fmt.Sprintf("SELECT pg_advisory_xact_lock(%d, %d)", lockKey1, lockKey2)); err != nil {
		return false, err
	}
	var recorded string
	err = tx.QueryRow(ctx, "SELECT sha256 FROM "+table+" WHERE id = $1", m.ID).Scan(&recorded)
	if err == nil {
		if recorded != m.Checksum {
			return false, fmt.Errorf("migration tampering %s: recorded %s, current %s", m.ID, recorded, m.Checksum)
		}
		return false, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	if _, err = tx.Exec(ctx, m.SQL); err != nil {
		return false, fmt.Errorf("%s DDL: %w", m.ID, err)
	}
	if _, err = tx.Exec(ctx, "INSERT INTO "+table+" (id, sha256) VALUES ($1, $2)", m.ID, m.Checksum); err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	fmt.Println("APPLIED", m.ID)
	return true, nil
}
