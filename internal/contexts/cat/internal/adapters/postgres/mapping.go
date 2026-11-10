package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cat/internal/domain/mapping"
	"go.idp.xyz/idp-brant/internal/contexts/cat/internal/domain/sku"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// LoadShared returns the SKU id, or nil, and holds its row FOR SHARE until the transaction ends: a
// mapping that references the SKU's specification revision commits before any change to the SKU
// or after it, never across it. A command locks its own root first.
func LoadShared(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) (*sku.Sku, error) {
	return scan(exec.QueryRow(ctx, `SELECT `+columns+` FROM brant_cat.sku WHERE tenant_id = $1 AND company_id = $2 AND sku_id = $3 FOR SHARE`,
		string(scope.Tenant), string(scope.Company), id))
}

const revisionColumns = `revision, status, sku_id, spec_revision, saved_by_kind, saved_by_id, saved_epoch, saved_at,
	published_by_kind, published_by_id, publish_epoch, published_at, evidence_ids, superseded_at`

func scanRevision(row pgx.Row) (*mapping.Revision, error) {
	var r mapping.Revision
	var status, savedKind string
	var publishedKind, publishedID *string
	var publishEpoch *int64
	var publishedAt, supersededAt *time.Time
	var evidence []string
	err := row.Scan(&r.Number, &status, &r.Target.SkuID, &r.Target.SpecRevision, &savedKind, &r.SavedBy.ID, &r.SavedEpoch, &r.SavedAt,
		&publishedKind, &publishedID, &publishEpoch, &publishedAt, &evidence, &supersededAt)
	if err != nil {
		return nil, err
	}
	r.Status, r.SavedBy.Kind, r.SavedAt = mapping.Status(status), kernel.ActorKind(savedKind), kernel.Instant(r.SavedAt)
	if publishedAt != nil {
		r.Publication = &mapping.Publication{By: kernel.ActorRef{Kind: kernel.ActorKind(*publishedKind), ID: *publishedID},
			Epoch: *publishEpoch, At: kernel.Instant(*publishedAt), EvidenceIDs: evidence}
	}
	if supersededAt != nil {
		r.SupersededAt = kernel.Instant(*supersededAt)
	}
	return &r, nil
}

func collectRevisions(rows pgx.Rows, err error) ([]*mapping.Revision, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*mapping.Revision, error) { return scanRevision(row) })
}

// LoadMapping returns the mapping id with its draft and active revisions, or nil; forUpdate locks
// the root row.
func LoadMapping(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, forUpdate bool) (*mapping.Root, error) {
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE"
	}
	t, c := string(scope.Tenant), string(scope.Company)
	r := &mapping.Root{ID: id}
	var role string
	err := exec.QueryRow(ctx, `SELECT party_id, party_role, external_code, root_version, last_revision FROM brant_cat.item_code_mapping
		WHERE tenant_id = $1 AND company_id = $2 AND mapping_id = $3`+lock, t, c, id).
		Scan(&r.Key.PartyID, &role, &r.Key.ExternalCode, &r.Version, &r.LastRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.Key.PartyRole = mapping.Role(role)
	revisions, err := collectRevisions(exec.Query(ctx, `SELECT `+revisionColumns+` FROM brant_cat.item_code_mapping_revision
		WHERE tenant_id = $1 AND company_id = $2 AND mapping_id = $3 AND status IN ('draft', 'active') ORDER BY revision`, t, c, id))
	for _, rev := range revisions {
		switch rev.Status {
		case mapping.Draft:
			r.Draft = rev
		case mapping.Active:
			r.Active = rev
		}
	}
	return r, err
}

// MappingRevisions returns every revision of the mapping id, oldest first.
func MappingRevisions(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) ([]*mapping.Revision, error) {
	return collectRevisions(exec.Query(ctx, `SELECT `+revisionColumns+` FROM brant_cat.item_code_mapping_revision
		WHERE tenant_id = $1 AND company_id = $2 AND mapping_id = $3 ORDER BY revision`, string(scope.Tenant), string(scope.Company), id))
}

// InsertMapping stores a new mapping root.
func InsertMapping(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *mapping.Root, at time.Time) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_cat.item_code_mapping (tenant_id, company_id, mapping_id, party_id, party_role,
		external_code, root_version, last_revision, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9)`,
		string(scope.Tenant), string(scope.Company), r.ID, r.Key.PartyID, string(r.Key.PartyRole), r.Key.ExternalCode, r.Version,
		r.LastRevision, at)
	return err
}

// UpdateMapping writes r if the stored root is still at version from.
func UpdateMapping(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *mapping.Root, from int64, at time.Time) error {
	tag, err := exec.Exec(ctx, `UPDATE brant_cat.item_code_mapping SET root_version = $4, last_revision = $5, updated_at = $6
		WHERE tenant_id = $1 AND company_id = $2 AND mapping_id = $3 AND root_version = $7`,
		string(scope.Tenant), string(scope.Company), r.ID, r.Version, r.LastRevision, at, from)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

type revisionRow struct {
	publishedKind, publishedID *string
	publishEpoch               *int64
	publishedAt, supersededAt  *time.Time
	evidence                   []string
}

func row(rev *mapping.Revision) revisionRow {
	var w revisionRow
	if p := rev.Publication; p != nil {
		kind := string(p.By.Kind)
		w.publishedKind, w.publishedID, w.publishEpoch, w.publishedAt, w.evidence = &kind, &p.By.ID, &p.Epoch, &p.At, p.EvidenceIDs
	}
	if !rev.SupersededAt.IsZero() {
		w.supersededAt = &rev.SupersededAt
	}
	return w
}

// InsertMappingRevision stores a new revision of r under r's key.
func InsertMappingRevision(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *mapping.Root, rev *mapping.Revision) error {
	w := row(rev)
	_, err := exec.Exec(ctx, `INSERT INTO brant_cat.item_code_mapping_revision (tenant_id, company_id, mapping_id, revision, party_id,
		party_role, external_code, status, sku_id, spec_revision, saved_by_kind, saved_by_id, saved_epoch, saved_at, published_by_kind,
		published_by_id, publish_epoch, published_at, evidence_ids, superseded_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)`,
		string(scope.Tenant), string(scope.Company), r.ID, rev.Number, r.Key.PartyID, string(r.Key.PartyRole), r.Key.ExternalCode,
		string(rev.Status), rev.Target.SkuID, rev.Target.SpecRevision, string(rev.SavedBy.Kind), rev.SavedBy.ID, rev.SavedEpoch, rev.SavedAt,
		w.publishedKind, w.publishedID, w.publishEpoch, w.publishedAt, w.evidence, w.supersededAt)
	return err
}

// UpdateMappingRevision writes the state of a stored revision of r; the trigger refuses every
// change that is not a revision transition or an edit of a draft.
func UpdateMappingRevision(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *mapping.Root, rev *mapping.Revision) error {
	w := row(rev)
	tag, err := exec.Exec(ctx, `UPDATE brant_cat.item_code_mapping_revision SET status = $5, sku_id = $6, spec_revision = $7,
		saved_by_kind = $8, saved_by_id = $9, saved_epoch = $10, saved_at = $11, published_by_kind = $12, published_by_id = $13,
		publish_epoch = $14, published_at = $15, evidence_ids = $16, superseded_at = $17
		WHERE tenant_id = $1 AND company_id = $2 AND mapping_id = $3 AND revision = $4`,
		string(scope.Tenant), string(scope.Company), r.ID, rev.Number, string(rev.Status), rev.Target.SkuID, rev.Target.SpecRevision,
		string(rev.SavedBy.Kind), rev.SavedBy.ID, rev.SavedEpoch, rev.SavedAt, w.publishedKind, w.publishedID, w.publishEpoch,
		w.publishedAt, w.evidence, w.supersededAt)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// KeyTaken reports whether a mapping other than except has an active revision for key. Two
// publications racing past this check collide on the unique index of active revisions; the ledger
// retries the loser, which then sees the winner here.
func KeyTaken(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, key mapping.Key, except string) (bool, error) {
	var taken bool
	err := exec.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM brant_cat.item_code_mapping_revision WHERE tenant_id = $1 AND company_id = $2
		AND party_id = $3 AND party_role = $4 AND external_code = $5 AND status = 'active' AND mapping_id <> $6)`,
		string(scope.Tenant), string(scope.Company), key.PartyID, string(key.PartyRole), key.ExternalCode, except).Scan(&taken)
	return taken, err
}

// Published is the active revision of the mapping published for a key.
type Published struct {
	MappingID string
	Revision  int64
	Target    mapping.Target
}

// PublishedFor returns the active revisions among keys, without locks.
func PublishedFor(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, keys []mapping.Key) (map[mapping.Key]Published, error) {
	parties, roles, codes := make([]string, len(keys)), make([]string, len(keys)), make([]string, len(keys))
	for i, k := range keys {
		parties[i], roles[i], codes[i] = k.PartyID, string(k.PartyRole), k.ExternalCode
	}
	rows, err := exec.Query(ctx, `SELECT party_id, party_role, external_code, mapping_id, revision, sku_id, spec_revision
		FROM brant_cat.item_code_mapping_revision WHERE tenant_id = $1 AND company_id = $2 AND status = 'active'
		AND (party_id, party_role, external_code) IN (SELECT * FROM unnest($3::text[], $4::text[], $5::text[]))`,
		string(scope.Tenant), string(scope.Company), parties, roles, codes)
	if err != nil {
		return nil, err
	}
	type found struct {
		key mapping.Key
		pub Published
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (found, error) {
		var f found
		var role string
		err := row.Scan(&f.key.PartyID, &role, &f.key.ExternalCode, &f.pub.MappingID, &f.pub.Revision, &f.pub.Target.SkuID,
			&f.pub.Target.SpecRevision)
		f.key.PartyRole = mapping.Role(role)
		return f, err
	})
	out := make(map[mapping.Key]Published, len(list))
	for _, f := range list {
		out[f.key] = f.pub
	}
	return out, err
}
