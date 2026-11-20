// Package postgres stores EVD sources in schema brant_evd. Every statement runs on the executor of
// EVD's own transaction; FORCE RLS limits it to the transaction's tenant and company, and the
// statements name the scope as well.
package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/evd/api"
	"go.idp.xyz/idp-brant/internal/contexts/evd/internal/domain/source"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Schema is EVD's schema.
const Schema = "brant_evd"

const originColumns = `o.origin_id, o.content_id, c.sha256, o.source_kind, coalesce(o.provider, ''), coalesce(o.account_ref, ''),
	coalesce(o.message_id, ''), coalesce(o.author_claim, ''), o.owner_actor_id, coalesce(o.owner_team_id, ''),
	coalesce(x.file_id, ''), coalesce(x.sha256, ''), coalesce(f.owner_actor_id, ''), coalesce(f.owner_team_id, '')`

// fromOrigin joins an origin's content and, for a file extract, the extract and the file's owner.
const fromOrigin = ` FROM brant_evd.source_origin o JOIN brant_evd.source_content c
	ON c.tenant_id = o.tenant_id AND c.company_id = o.company_id AND c.content_id = o.content_id
	LEFT JOIN brant_evd.file_extract x ON x.tenant_id = o.tenant_id AND x.company_id = o.company_id AND x.extract_id = o.extract_id
	LEFT JOIN brant_evd.business_file f ON f.tenant_id = x.tenant_id AND f.company_id = x.company_id AND f.file_id = x.file_id`

func originTargets(o *source.Origin, kind *string) []any {
	return []any{&o.ID, &o.ContentID, &o.ContentHash, kind, &o.Channel.Provider, &o.Channel.AccountRef,
		&o.Channel.ExternalMessageID, &o.Channel.AuthorClaim, &o.OwnerActor, &o.OwnerTeam,
		&o.Extract.FileID, &o.Extract.SHA256, &o.File.Actor, &o.File.Team}
}

func scanOrigin(row pgx.Row) (*source.Origin, error) {
	var o source.Origin
	var kind string
	err := row.Scan(originTargets(&o, &kind)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	o.Kind = source.Kind(kind)
	return &o, err
}

// OriginByID returns the origin with id, or nil.
func OriginByID(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) (*source.Origin, error) {
	return scanOrigin(exec.QueryRow(ctx, `SELECT `+originColumns+fromOrigin+`
		WHERE o.tenant_id = $1 AND o.company_id = $2 AND o.origin_id = $3`, string(scope.Tenant), string(scope.Company), id))
}

// OriginByChannel returns the origin of the email ch, or nil.
func OriginByChannel(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, ch source.Channel) (*source.Origin, error) {
	return scanOrigin(exec.QueryRow(ctx, `SELECT `+originColumns+fromOrigin+`
		WHERE o.tenant_id = $1 AND o.company_id = $2 AND o.provider = $3 AND o.account_ref = $4 AND o.message_id = $5`,
		string(scope.Tenant), string(scope.Company), ch.Provider, ch.AccountRef, ch.ExternalMessageID))
}

// ContentByHash returns the ID of the content with hash, or "".
func ContentByHash(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, hash string) (string, error) {
	var id string
	err := exec.QueryRow(ctx, `SELECT content_id FROM brant_evd.source_content
		WHERE tenant_id = $1 AND company_id = $2 AND sha256 = $3`, string(scope.Tenant), string(scope.Company), hash).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// InsertContent stores text under contentID.
func InsertContent(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, contentID, hash, text string, at time.Time) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_evd.source_content (tenant_id, company_id, content_id, sha256, content_text, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)`, string(scope.Tenant), string(scope.Company), contentID, hash, text, at)
	return err
}

// InsertOrigin stores a new origin owned by its first registrant; extractID names the file extract
// of a file_extract origin ("" for other kinds).
func InsertOrigin(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, o source.Origin, extractID string, at time.Time) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_evd.source_origin (tenant_id, company_id, origin_id, content_id, source_kind,
		provider, account_ref, message_id, author_claim, owner_actor_id, owner_team_id, registered_at, extract_id)
		VALUES ($1, $2, $3, $4, $5, nullif($6, ''), nullif($7, ''), nullif($8, ''), nullif($9, ''), $10, nullif($11, ''), $12, nullif($13, ''))`,
		string(scope.Tenant), string(scope.Company), o.ID, o.ContentID, string(o.Kind), o.Channel.Provider, o.Channel.AccountRef,
		o.Channel.ExternalMessageID, o.Channel.AuthorClaim, o.OwnerActor, o.OwnerTeam, at, extractID)
	return err
}

// Extract is a stored file extract.
type Extract struct {
	ID string
	api.ExtractRef
	LocatedForm string
	CreatedAt   time.Time
}

const extractColumns = `x.extract_id, x.file_id, x.file_sha256, x.extract_kind, x.extractor, x.profile, x.characters, x.pages, x.sheets, x.sha256`

func extractTargets(x *Extract) []any {
	return []any{&x.ID, &x.FileID, &x.FileSHA256, &x.Kind, &x.Extractor, &x.Profile, &x.Characters, &x.Pages, &x.Sheets, &x.SHA256}
}

// ExtractByFile returns the extract of fileID by extractor under profile, without its located form,
// or nil.
func ExtractByFile(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, fileID, extractor, profile string) (*Extract, error) {
	var x Extract
	err := exec.QueryRow(ctx, `SELECT `+extractColumns+` FROM brant_evd.file_extract x
		WHERE x.tenant_id = $1 AND x.company_id = $2 AND x.file_id = $3 AND x.extractor = $4 AND x.profile = $5`,
		string(scope.Tenant), string(scope.Company), fileID, extractor, profile).Scan(extractTargets(&x)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &x, err
}

// InsertExtract stores a file extract.
func InsertExtract(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, x Extract) error {
	sheets := x.Sheets
	if sheets == nil {
		sheets = []string{}
	}
	_, err := exec.Exec(ctx, `INSERT INTO brant_evd.file_extract (tenant_id, company_id, extract_id, file_id, file_sha256, extract_kind,
		extractor, profile, characters, pages, sheets, sha256, located_form, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		string(scope.Tenant), string(scope.Company), x.ID, x.FileID, x.FileSHA256, x.Kind, x.Extractor, x.Profile, x.Characters, x.Pages,
		sheets, x.SHA256, x.LocatedForm, x.CreatedAt)
	return err
}

// OriginExtract returns the extract of the origin id with its located form, or nil when the origin
// is no file extract.
func OriginExtract(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) (*Extract, error) {
	var x Extract
	targets := append(extractTargets(&x), &x.LocatedForm, &x.CreatedAt)
	err := exec.QueryRow(ctx, `SELECT `+extractColumns+`, x.located_form, x.created_at FROM brant_evd.source_origin o
		JOIN brant_evd.file_extract x ON x.tenant_id = o.tenant_id AND x.company_id = o.company_id AND x.extract_id = o.extract_id
		WHERE o.tenant_id = $1 AND o.company_id = $2 AND o.origin_id = $3`, string(scope.Tenant), string(scope.Company), id).Scan(targets...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &x, err
}

// InsertImport records one accepted registration of originID by actor.
func InsertImport(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, importID, originID string, actor kernel.ActorRef, at time.Time) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_evd.source_import (tenant_id, company_id, import_id, origin_id, actor_kind, actor_id, imported_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, string(scope.Tenant), string(scope.Company), importID, originID, string(actor.Kind), actor.ID, at)
	return err
}

// Stored is an origin with its content and imports.
type Stored struct {
	Origin       source.Origin
	Text         string
	RegisteredAt time.Time
	Imports      []Import
}

// Import is one stored import.
type Import struct {
	ID         string
	Actor      kernel.ActorRef
	ImportedAt time.Time
}

// Load returns the origin id with content and imports, or nil.
func Load(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) (*Stored, error) {
	var s Stored
	var kind string
	o := &s.Origin
	err := exec.QueryRow(ctx, `SELECT `+originColumns+`, c.content_text, o.registered_at`+fromOrigin+`
		WHERE o.tenant_id = $1 AND o.company_id = $2 AND o.origin_id = $3`, string(scope.Tenant), string(scope.Company), id).
		Scan(append(originTargets(o, &kind), &s.Text, &s.RegisteredAt)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	o.Kind = source.Kind(kind)
	rows, err := exec.Query(ctx, `SELECT import_id, actor_kind, actor_id, imported_at FROM brant_evd.source_import
		WHERE tenant_id = $1 AND company_id = $2 AND origin_id = $3 ORDER BY imported_at, import_id`,
		string(scope.Tenant), string(scope.Company), id)
	if err != nil {
		return nil, err
	}
	s.Imports, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Import, error) {
		var i Import
		var k string
		err := row.Scan(&i.ID, &k, &i.Actor.ID, &i.ImportedAt)
		i.Actor.Kind = kernel.ActorKind(k)
		return i, err
	})
	return &s, err
}

// Origins returns the origins among ids.
func Origins(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, ids []string) ([]source.Origin, error) {
	rows, err := exec.Query(ctx, `SELECT `+originColumns+fromOrigin+`
		WHERE o.tenant_id = $1 AND o.company_id = $2 AND o.origin_id = ANY ($3)`, string(scope.Tenant), string(scope.Company), ids)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (source.Origin, error) {
		o, err := scanOrigin(row)
		if err != nil {
			return source.Origin{}, err
		}
		return *o, nil
	})
}

// File is a stored business file.
type File struct {
	ID               string
	Name             string
	MimeType         string
	Content          []byte
	SHA256           string
	SupersedesFileID string
	OwnerActor       string
	OwnerTeam        string
	RegisteredAt     time.Time
}

// FileExists reports whether a file id exists in the scope, whoever owns it (Python
// file_register: BUSINESS_KEY_CONFLICT before any access check).
func FileExists(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) (bool, error) {
	var ok bool
	err := exec.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM brant_evd.business_file WHERE tenant_id = $1 AND company_id = $2 AND file_id = $3)`,
		string(scope.Tenant), string(scope.Company), id).Scan(&ok)
	return ok, err
}

// HasSuccessor reports whether a file already has a successor.
func HasSuccessor(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) (bool, error) {
	var ok bool
	err := exec.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM brant_evd.business_file WHERE tenant_id = $1 AND company_id = $2
		AND supersedes_file_id = $3)`, string(scope.Tenant), string(scope.Company), id).Scan(&ok)
	return ok, err
}

// LoadFile returns the file id, or nil; withContent also reads its bytes.
func LoadFile(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, withContent bool) (*File, error) {
	content := "NULL::bytea"
	if withContent {
		content = "content"
	}
	var f File
	err := exec.QueryRow(ctx, `SELECT file_id, file_name, mime_type, `+content+`, sha256, coalesce(supersedes_file_id, ''),
		owner_actor_id, coalesce(owner_team_id, ''), registered_at FROM brant_evd.business_file
		WHERE tenant_id = $1 AND company_id = $2 AND file_id = $3`, string(scope.Tenant), string(scope.Company), id).
		Scan(&f.ID, &f.Name, &f.MimeType, &f.Content, &f.SHA256, &f.SupersedesFileID, &f.OwnerActor, &f.OwnerTeam, &f.RegisteredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &f, err
}

// InsertFile stores a business file.
func InsertFile(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, f File) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_evd.business_file (tenant_id, company_id, file_id, file_name, mime_type, content, sha256,
		byte_size, supersedes_file_id, owner_actor_id, owner_team_id, registered_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, nullif($9, ''), $10, nullif($11, ''), $12)`,
		string(scope.Tenant), string(scope.Company), f.ID, f.Name, f.MimeType, f.Content, f.SHA256, len(f.Content), f.SupersedesFileID,
		f.OwnerActor, f.OwnerTeam, f.RegisteredAt)
	return err
}
