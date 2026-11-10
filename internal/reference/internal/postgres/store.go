// Package postgres stores screening list reference data in schema brant_ref, on the executor of the
// reference data's own transaction.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/kernel"
	"go.idp.xyz/idp-brant/internal/reference/internal/lists"
)

// Schema is the schema of reference data.
const Schema = "brant_ref"

// ErrConflict means a compare-and-set update found another row version.
var ErrConflict = errors.New("ref: row version changed")

func args(scope kernel.Scope, more ...any) []any {
	return append([]any{string(scope.Tenant), string(scope.Company)}, more...)
}

// LoadList returns the list code, or nil, locking it FOR UPDATE when forUpdate.
func LoadList(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, code string, forUpdate bool) (*lists.List, error) {
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE"
	}
	var l lists.List
	err := exec.QueryRow(ctx, `SELECT list_code, jurisdiction, current_version, row_version FROM brant_ref.screening_list
		WHERE tenant_id = $1 AND company_id = $2 AND list_code = $3`+lock, args(scope, code)...).
		Scan(&l.Code, &l.Jurisdiction, &l.Current, &l.RowVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &l, err
}

// SaveList inserts the list at row version 1 or moves it from row version from.
func SaveList(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, l *lists.List, from int64, at time.Time) error {
	if from == 0 {
		_, err := exec.Exec(ctx, `INSERT INTO brant_ref.screening_list (tenant_id, company_id, list_code, jurisdiction, current_version,
			row_version, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $7)`,
			args(scope, l.Code, l.Jurisdiction, l.Current, l.RowVersion, at)...)
		return err
	}
	tag, err := exec.Exec(ctx, `UPDATE brant_ref.screening_list SET current_version = $4, row_version = $5, updated_at = $6
		WHERE tenant_id = $1 AND company_id = $2 AND list_code = $3 AND row_version = $7`,
		args(scope, l.Code, l.Current, l.RowVersion, at, from)...)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

const versionColumns = `v.list_code, v.version, v.source_name, v.source_url, v.published_on, v.file_sha256, v.content_hash,
	v.entry_count, v.import_seq, v.imported_by_kind, v.imported_by_id, v.authority_epoch, v.imported_at`

func scanVersion(row pgx.Row) (*lists.Version, error) {
	var v lists.Version
	var kind string
	err := row.Scan(&v.ListCode, &v.Version, &v.SourceName, &v.SourceURL, &v.PublishedOn, &v.FileSHA256, &v.ContentHash,
		&v.EntryCount, &v.ImportSeq, &kind, &v.ImportedBy.ID, &v.Epoch, &v.ImportedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	v.ImportedBy.Kind = kernel.ActorKind(kind)
	v.PublishedOn, v.ImportedAt = v.PublishedOn.UTC(), kernel.Instant(v.ImportedAt)
	return &v, err
}

// LoadVersion returns one version of a list, or nil.
func LoadVersion(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, code, version string) (*lists.Version, error) {
	return scanVersion(exec.QueryRow(ctx, `SELECT `+versionColumns+` FROM brant_ref.screening_list_version v
		WHERE v.tenant_id = $1 AND v.company_id = $2 AND v.list_code = $3 AND v.version = $4`, args(scope, code, version)...))
}

// Versions returns every version of a list, oldest import first.
func Versions(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, code string) ([]*lists.Version, error) {
	rows, err := exec.Query(ctx, `SELECT `+versionColumns+` FROM brant_ref.screening_list_version v
		WHERE v.tenant_id = $1 AND v.company_id = $2 AND v.list_code = $3 ORDER BY v.import_seq`, args(scope, code)...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*lists.Version, error) { return scanVersion(row) })
}

// Current is a list's current version with the list's jurisdiction.
type Current struct {
	Jurisdiction string
	Version      *lists.Version
}

// CurrentVersions returns the current version of each of codes the company imported.
func CurrentVersions(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, codes []string) ([]Current, error) {
	rows, err := exec.Query(ctx, `SELECT l.jurisdiction, `+versionColumns+` FROM brant_ref.screening_list l
		JOIN brant_ref.screening_list_version v ON v.tenant_id = l.tenant_id AND v.company_id = l.company_id
		 AND v.list_code = l.list_code AND v.version = l.current_version
		WHERE l.tenant_id = $1 AND l.company_id = $2 AND l.list_code = ANY ($3) ORDER BY l.list_code`, args(scope, codes)...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Current, error) {
		var c Current
		var v lists.Version
		var kind string
		err := row.Scan(&c.Jurisdiction, &v.ListCode, &v.Version, &v.SourceName, &v.SourceURL, &v.PublishedOn, &v.FileSHA256,
			&v.ContentHash, &v.EntryCount, &v.ImportSeq, &kind, &v.ImportedBy.ID, &v.Epoch, &v.ImportedAt)
		v.ImportedBy.Kind = kernel.ActorKind(kind)
		v.PublishedOn, v.ImportedAt = v.PublishedOn.UTC(), kernel.Instant(v.ImportedAt)
		c.Version = &v
		return c, err
	})
}

// InsertVersion stores a version, its entries and the keys of their names. Entries and keys go
// in one statement each, as arrays.
func InsertVersion(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, v *lists.Version, entries []lists.Entry) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_ref.screening_list_version (tenant_id, company_id, list_code, version, source_name,
		source_url, published_on, file_sha256, content_hash, entry_count, import_seq, imported_by_kind, imported_by_id,
		authority_epoch, imported_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
		args(scope, v.ListCode, v.Version, v.SourceName, v.SourceURL, v.PublishedOn, v.FileSHA256, v.ContentHash, v.EntryCount,
			v.ImportSeq, string(v.ImportedBy.Kind), v.ImportedBy.ID, v.Epoch, v.ImportedAt)...)
	if err != nil {
		return err
	}
	n := len(entries)
	ids, names, aliases, types, countries, hashes := make([]string, n), make([]string, n), make([]string, n), make([]string, n),
		make([]string, n), make([]string, n)
	var keyEntries, keys, keyNames []string
	for i, e := range entries {
		raw, err := json.Marshal(e.Aliases)
		if err != nil {
			return err
		}
		ids[i], names[i], aliases[i], types[i], countries[i], hashes[i] = e.ID, e.PrimaryName, string(raw), e.EntityType, e.Country, e.Hash
		for _, k := range e.Keys {
			keyEntries, keys, keyNames = append(keyEntries, e.ID), append(keys, k.Key), append(keyNames, k.Name)
		}
	}
	_, err = exec.Exec(ctx, `INSERT INTO brant_ref.screening_list_entry (tenant_id, company_id, list_code, version, entry_id,
		primary_name, aliases, entity_type, country, content_hash)
		SELECT $1, $2, $3, $4, u.id, u.name, ARRAY(SELECT jsonb_array_elements_text(u.aliases::jsonb)), u.type, nullif(u.country, ''), u.hash
		FROM unnest($5::text[], $6::text[], $7::text[], $8::text[], $9::text[], $10::text[]) AS u(id, name, aliases, type, country, hash)`,
		args(scope, v.ListCode, v.Version, ids, names, aliases, types, countries, hashes)...)
	if err != nil {
		return err
	}
	_, err = exec.Exec(ctx, `INSERT INTO brant_ref.screening_entry_name (tenant_id, company_id, list_code, version, entry_id, name_key, name)
		SELECT $1, $2, $3, $4, u.id, u.key, u.name FROM unnest($5::text[], $6::text[], $7::text[]) AS u(id, key, name)`,
		args(scope, v.ListCode, v.Version, keyEntries, keys, keyNames)...)
	return err
}

const entryColumns = `e.entry_id, e.primary_name, e.aliases, e.entity_type, coalesce(e.country, ''), e.content_hash`

func scanEntry(row pgx.Row, e *lists.Entry) error {
	return row.Scan(&e.ID, &e.PrimaryName, &e.Aliases, &e.EntityType, &e.Country, &e.Hash)
}

// Entries returns the entries of one version, ordered by entry ID.
func Entries(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, code, version string) ([]lists.Entry, error) {
	rows, err := exec.Query(ctx, `SELECT `+entryColumns+` FROM brant_ref.screening_list_entry e
		WHERE e.tenant_id = $1 AND e.company_id = $2 AND e.list_code = $3 AND e.version = $4 ORDER BY e.entry_id`,
		args(scope, code, version)...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (lists.Entry, error) {
		var e lists.Entry
		return e, scanEntry(row, &e)
	})
}

// Match is one entry a key matched.
type Match struct {
	ListCode, Version string
	Entry             lists.Entry
	Key, Name         string
}

// MatchKeys returns the entries of the versions (code, version pairs) whose names have one of keys,
// ordered by list, entry and key.
func MatchKeys(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, codes, versions, keys []string) ([]Match, error) {
	rows, err := exec.Query(ctx, `SELECT n.list_code, n.version, n.name_key, n.name, `+entryColumns+`
		FROM brant_ref.screening_entry_name n
		JOIN brant_ref.screening_list_entry e ON e.tenant_id = n.tenant_id AND e.company_id = n.company_id AND e.list_code = n.list_code
		 AND e.version = n.version AND e.entry_id = n.entry_id
		WHERE n.tenant_id = $1 AND n.company_id = $2
		  AND (n.list_code, n.version) IN (SELECT * FROM unnest($3::text[], $4::text[]))
		  AND n.name_key = ANY ($5)
		ORDER BY n.list_code, n.entry_id, n.name_key`, args(scope, codes, versions, keys)...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Match, error) {
		var m Match
		err := row.Scan(&m.ListCode, &m.Version, &m.Key, &m.Name, &m.Entry.ID, &m.Entry.PrimaryName, &m.Entry.Aliases,
			&m.Entry.EntityType, &m.Entry.Country, &m.Entry.Hash)
		return m, err
	})
}
