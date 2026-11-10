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
	"go.idp.xyz/idp-brant/internal/contexts/evd/internal/domain/source"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Schema is EVD's schema.
const Schema = "brant_evd"

const originColumns = `o.origin_id, o.content_id, c.sha256, o.source_kind, coalesce(o.provider, ''), coalesce(o.account_ref, ''),
	coalesce(o.message_id, ''), coalesce(o.author_claim, ''), o.owner_actor_id, coalesce(o.owner_team_id, '')`

const fromOrigin = ` FROM brant_evd.source_origin o JOIN brant_evd.source_content c
	ON c.tenant_id = o.tenant_id AND c.company_id = o.company_id AND c.content_id = o.content_id`

func scanOrigin(row pgx.Row) (*source.Origin, error) {
	var o source.Origin
	var kind string
	err := row.Scan(&o.ID, &o.ContentID, &o.ContentHash, &kind, &o.Channel.Provider, &o.Channel.AccountRef,
		&o.Channel.ExternalMessageID, &o.Channel.AuthorClaim, &o.OwnerActor, &o.OwnerTeam)
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

// InsertOrigin stores a new origin owned by its first registrant.
func InsertOrigin(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, o source.Origin, at time.Time) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_evd.source_origin (tenant_id, company_id, origin_id, content_id, source_kind,
		provider, account_ref, message_id, author_claim, owner_actor_id, owner_team_id, registered_at)
		VALUES ($1, $2, $3, $4, $5, nullif($6, ''), nullif($7, ''), nullif($8, ''), nullif($9, ''), $10, nullif($11, ''), $12)`,
		string(scope.Tenant), string(scope.Company), o.ID, o.ContentID, string(o.Kind), o.Channel.Provider, o.Channel.AccountRef,
		o.Channel.ExternalMessageID, o.Channel.AuthorClaim, o.OwnerActor, o.OwnerTeam, at)
	return err
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
		Scan(&o.ID, &o.ContentID, &o.ContentHash, &kind, &o.Channel.Provider, &o.Channel.AccountRef, &o.Channel.ExternalMessageID,
			&o.Channel.AuthorClaim, &o.OwnerActor, &o.OwnerTeam, &s.Text, &s.RegisteredAt)
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
