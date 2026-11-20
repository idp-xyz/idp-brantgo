package postgres

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/kernel"
	"go.idp.xyz/idp-brant/internal/reference/internal/programmes"
)

// LoadProgrammeList returns the country programme list code, or nil, locking it FOR UPDATE when
// forUpdate.
func LoadProgrammeList(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, code string, forUpdate bool) (*programmes.List, error) {
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE"
	}
	var l programmes.List
	err := exec.QueryRow(ctx, `SELECT list_code, jurisdiction, current_version, row_version FROM brant_ref.country_programme_list
		WHERE tenant_id = $1 AND company_id = $2 AND list_code = $3`+lock, args(scope, code)...).
		Scan(&l.Code, &l.Jurisdiction, &l.Current, &l.RowVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &l, err
}

// SaveProgrammeList inserts the list at row version 1 or moves it from row version from.
func SaveProgrammeList(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, l *programmes.List, from int64, at time.Time) error {
	if from == 0 {
		_, err := exec.Exec(ctx, `INSERT INTO brant_ref.country_programme_list (tenant_id, company_id, list_code, jurisdiction,
			current_version, row_version, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $7)`,
			args(scope, l.Code, l.Jurisdiction, l.Current, l.RowVersion, at)...)
		return err
	}
	tag, err := exec.Exec(ctx, `UPDATE brant_ref.country_programme_list SET current_version = $4, row_version = $5, updated_at = $6
		WHERE tenant_id = $1 AND company_id = $2 AND list_code = $3 AND row_version = $7`,
		args(scope, l.Code, l.Current, l.RowVersion, at, from)...)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

const programmeVersionColumns = `v.list_code, v.version, v.source_name, v.source_url, v.published_on, v.file_sha256, v.content_hash,
	v.programme_count, v.country_count, v.import_seq, v.imported_by_kind, v.imported_by_id, v.authority_epoch, v.imported_at`

func programmeVersionTargets(v *programmes.Version, kind *string) []any {
	return []any{&v.ListCode, &v.Version, &v.SourceName, &v.SourceURL, &v.PublishedOn, &v.FileSHA256, &v.ContentHash,
		&v.ProgrammeCount, &v.CountryCount, &v.ImportSeq, kind, &v.ImportedBy.ID, &v.Epoch, &v.ImportedAt}
}

func normalizeProgrammeVersion(v *programmes.Version, kind string) {
	v.ImportedBy.Kind = kernel.ActorKind(kind)
	v.PublishedOn, v.ImportedAt = v.PublishedOn.UTC(), kernel.Instant(v.ImportedAt)
}

func scanProgrammeVersion(row pgx.Row) (*programmes.Version, error) {
	var v programmes.Version
	var kind string
	err := row.Scan(programmeVersionTargets(&v, &kind)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	normalizeProgrammeVersion(&v, kind)
	return &v, err
}

// LoadProgrammeVersion returns one version of a programme list, or nil.
func LoadProgrammeVersion(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, code, version string) (*programmes.Version, error) {
	return scanProgrammeVersion(exec.QueryRow(ctx, `SELECT `+programmeVersionColumns+` FROM brant_ref.country_programme_list_version v
		WHERE v.tenant_id = $1 AND v.company_id = $2 AND v.list_code = $3 AND v.version = $4`, args(scope, code, version)...))
}

// ProgrammeVersions returns every version of a programme list, oldest import first.
func ProgrammeVersions(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, code string) ([]*programmes.Version, error) {
	rows, err := exec.Query(ctx, `SELECT `+programmeVersionColumns+` FROM brant_ref.country_programme_list_version v
		WHERE v.tenant_id = $1 AND v.company_id = $2 AND v.list_code = $3 ORDER BY v.import_seq`, args(scope, code)...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*programmes.Version, error) { return scanProgrammeVersion(row) })
}

// InsertProgrammeVersion stores a version and its programmes; the programmes go in one statement,
// as arrays (a programme's countries, and its regions, as one comma-separated element; no regions
// are NULL).
func InsertProgrammeVersion(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, v *programmes.Version, list []programmes.Programme) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_ref.country_programme_list_version (tenant_id, company_id, list_code, version,
		source_name, source_url, published_on, file_sha256, content_hash, programme_count, country_count, import_seq, imported_by_kind,
		imported_by_id, authority_epoch, imported_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
		args(scope, v.ListCode, v.Version, v.SourceName, v.SourceURL, v.PublishedOn, v.FileSHA256, v.ContentHash, v.ProgrammeCount,
			v.CountryCount, v.ImportSeq, string(v.ImportedBy.Kind), v.ImportedBy.ID, v.Epoch, v.ImportedAt)...)
	if err != nil {
		return err
	}
	n := len(list)
	ids, names, measures, countries, scopes, regions, hashes := make([]string, n), make([]string, n), make([]string, n), make([]string, n),
		make([]string, n), make([]string, n), make([]string, n)
	for i, p := range list {
		ids[i], names[i], measures[i], scopes[i], hashes[i] = p.ID, p.Name, p.Measure, p.Scope, p.Hash
		countries[i], regions[i] = strings.Join(p.Countries, ","), strings.Join(p.Regions, ",")
	}
	_, err = exec.Exec(ctx, `INSERT INTO brant_ref.country_programme (tenant_id, company_id, list_code, version, programme_id, name,
		measure, countries, scope, regions, content_hash)
		SELECT $1, $2, $3, $4, u.id, u.name, u.measure, string_to_array(u.countries, ','), nullif(u.scope, ''),
		string_to_array(nullif(u.regions, ''), ','), u.hash
		FROM unnest($5::text[], $6::text[], $7::text[], $8::text[], $9::text[], $10::text[], $11::text[])
		AS u(id, name, measure, countries, scope, regions, hash)`,
		args(scope, v.ListCode, v.Version, ids, names, measures, countries, scopes, regions, hashes)...)
	return err
}

const programmeColumns = `p.programme_id, p.name, p.measure, p.countries, coalesce(p.scope, ''), p.regions, p.content_hash`

func scanProgramme(row pgx.Row, p *programmes.Programme) error {
	return row.Scan(&p.ID, &p.Name, &p.Measure, &p.Countries, &p.Scope, &p.Regions, &p.Hash)
}

// ProgrammesOf returns the programmes of one version, ordered by programme ID.
func ProgrammesOf(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, code, version string) ([]programmes.Programme, error) {
	rows, err := exec.Query(ctx, `SELECT `+programmeColumns+` FROM brant_ref.country_programme p
		WHERE p.tenant_id = $1 AND p.company_id = $2 AND p.list_code = $3 AND p.version = $4 ORDER BY p.programme_id COLLATE "C"`,
		args(scope, code, version)...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (programmes.Programme, error) {
		var p programmes.Programme
		return p, scanProgramme(row, &p)
	})
}

// ResolvedVersion is a version a lookup resolved, with its list's jurisdiction and whether it is
// the list's current version.
type ResolvedVersion struct {
	Jurisdiction string
	Current      bool
	Version      *programmes.Version
}

// ResolveProgrammeVersions returns, for each (code, version) pair, the version it names: the
// list's current version for an empty version, else exactly that version; pairs the company has
// not imported are absent.
func ResolveProgrammeVersions(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, codes, versions []string) ([]ResolvedVersion, error) {
	rows, err := exec.Query(ctx, `SELECT l.jurisdiction, v.version = l.current_version, `+programmeVersionColumns+`
		FROM unnest($3::text[], $4::text[]) AS q(list_code, version)
		JOIN brant_ref.country_programme_list l ON l.tenant_id = $1 AND l.company_id = $2 AND l.list_code = q.list_code
		JOIN brant_ref.country_programme_list_version v ON v.tenant_id = l.tenant_id AND v.company_id = l.company_id
		 AND v.list_code = l.list_code AND v.version = coalesce(nullif(q.version, ''), l.current_version)
		ORDER BY l.list_code COLLATE "C"`, args(scope, codes, versions)...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (ResolvedVersion, error) {
		var r ResolvedVersion
		var v programmes.Version
		var kind string
		err := row.Scan(append([]any{&r.Jurisdiction, &r.Current}, programmeVersionTargets(&v, &kind)...)...)
		normalizeProgrammeVersion(&v, kind)
		r.Version = &v
		return r, err
	})
}

// ProgrammeHit is a programme that names one of the queried countries.
type ProgrammeHit struct {
	ListCode, Version, Country string
	Programme                  programmes.Programme
}

// ProgrammesNaming returns the programmes of the versions (code, version pairs) that name one of
// countries, once per (programme, country), ordered by list, programme and country.
func ProgrammesNaming(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, codes, versions, countries []string) ([]ProgrammeHit, error) {
	rows, err := exec.Query(ctx, `SELECT p.list_code, p.version, c.country, `+programmeColumns+`
		FROM brant_ref.country_programme p
		CROSS JOIN LATERAL unnest(p.countries) AS c(country)
		WHERE p.tenant_id = $1 AND p.company_id = $2
		  AND (p.list_code, p.version) IN (SELECT * FROM unnest($3::text[], $4::text[]))
		  AND c.country = ANY ($5)
		ORDER BY p.list_code COLLATE "C", p.programme_id COLLATE "C", c.country COLLATE "C"`, args(scope, codes, versions, countries)...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (ProgrammeHit, error) {
		var h ProgrammeHit
		err := row.Scan(&h.ListCode, &h.Version, &h.Country, &h.Programme.ID, &h.Programme.Name, &h.Programme.Measure,
			&h.Programme.Countries, &h.Programme.Scope, &h.Programme.Regions, &h.Programme.Hash)
		return h, err
	})
}
