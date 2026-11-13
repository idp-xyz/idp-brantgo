package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/evd/internal/domain/file"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// FormalFile is a stored formal file (brant_evd.formal_file, migration 000004): the business file
// that holds a formal document of TRD, the document as TRD sealed it, the event that registered it,
// and the file's owner.
type FormalFile struct {
	FileID       string
	Formal       file.Formal
	EventID      string
	EventType    string
	RegisteredAt time.Time
	OwnerActor   string
	OwnerTeam    string
}

const formalColumns = `ff.file_id, ff.document_kind, ff.quote_id, ff.business_revision, ff.business_hash, coalesce(ff.replaced_revision, 0),
	coalesce(ff.order_id, ''), coalesce(ff.order_revision, 0), coalesce(ff.order_hash, ''), ff.number, f.file_name, f.mime_type,
	ff.document_sha256, ff.size_bytes, ff.event_id, ff.event_type, ff.registered_at, f.owner_actor_id, coalesce(f.owner_team_id, '')`

const fromFormal = ` FROM brant_evd.formal_file ff JOIN brant_evd.business_file f
	ON f.tenant_id = ff.tenant_id AND f.company_id = ff.company_id AND f.file_id = ff.file_id`

func scanFormal(row pgx.Row) (*FormalFile, error) {
	var x FormalFile
	d := &x.Formal
	err := row.Scan(&x.FileID, &d.Kind, &d.QuoteID, &d.BusinessRevision, &d.BusinessHash, &d.ReplacedRevision, &d.OrderID, &d.OrderRevision,
		&d.OrderHash, &d.Number, &d.FileName, &d.MimeType, &d.SHA256, &d.Size, &x.EventID, &x.EventType, &x.RegisteredAt, &x.OwnerActor,
		&x.OwnerTeam)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &x, err
}

// FormalFileByDocument returns the formal file of the TRD document key, or nil.
func FormalFileByDocument(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, key file.DocumentKey) (*FormalFile, error) {
	if key.Kind == file.FormalQuote {
		return scanFormal(exec.QueryRow(ctx, `SELECT `+formalColumns+fromFormal+`
			WHERE ff.tenant_id = $1 AND ff.company_id = $2 AND ff.document_kind = 'quote' AND ff.quote_id = $3 AND ff.business_revision = $4`,
			string(scope.Tenant), string(scope.Company), key.QuoteID, key.BusinessRevision))
	}
	return scanFormal(exec.QueryRow(ctx, `SELECT `+formalColumns+fromFormal+`
		WHERE ff.tenant_id = $1 AND ff.company_id = $2 AND ff.document_kind = $3 AND ff.order_id = $4 AND ff.order_revision = $5`,
		string(scope.Tenant), string(scope.Company), key.Kind, key.OrderID, key.OrderRevision))
}

// FormalFileByID returns the formal file of the business file id, or nil when the file holds no
// formal document.
func FormalFileByID(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) (*FormalFile, error) {
	return scanFormal(exec.QueryRow(ctx, `SELECT `+formalColumns+fromFormal+`
		WHERE ff.tenant_id = $1 AND ff.company_id = $2 AND ff.file_id = $3`, string(scope.Tenant), string(scope.Company), id))
}

// InsertFormalFile records that the business file x.FileID, stored in the same transaction, holds
// the formal document x.Formal. The database checks that its bytes hash to the sealed SHA-256.
func InsertFormalFile(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, x FormalFile) error {
	d := x.Formal
	_, err := exec.Exec(ctx, `INSERT INTO brant_evd.formal_file (tenant_id, company_id, file_id, document_kind, quote_id, business_revision,
		business_hash, replaced_revision, order_id, order_revision, order_hash, number, document_sha256, size_bytes, event_id, event_type,
		registered_at) VALUES ($1, $2, $3, $4, $5, $6, $7, nullif($8::bigint, 0), nullif($9, ''), nullif($10::bigint, 0), nullif($11, ''), $12, $13, $14, $15,
		$16, $17)`,
		string(scope.Tenant), string(scope.Company), x.FileID, d.Kind, d.QuoteID, d.BusinessRevision, d.BusinessHash, d.ReplacedRevision,
		d.OrderID, d.OrderRevision, d.OrderHash, d.Number, d.SHA256, d.Size, x.EventID, x.EventType, x.RegisteredAt)
	return err
}
