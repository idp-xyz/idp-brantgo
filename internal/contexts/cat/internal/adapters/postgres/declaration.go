package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cat/internal/domain/declaration"
	"go.idp.xyz/idp-brant/internal/kernel"
)

const profileColumns = `profile_id, sku_id, spec_revision, code_table_version, root_version, last_revision`

const profileRevisionColumns = `revision, status, content, content_hash, saved_by_kind, saved_by_id, saved_epoch, saved_at,
	verified_by_kind, verified_by_id, verify_epoch, verified_at, evidence_ids, superseded_at`

func scanProfile(row pgx.Row) (*declaration.Root, error) {
	r := &declaration.Root{}
	err := row.Scan(&r.ID, &r.Key.SkuID, &r.Key.SpecRevision, &r.Key.CodeTableVersion, &r.Version, &r.LastRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

func collectProfiles(rows pgx.Rows, err error) ([]*declaration.Root, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*declaration.Root, error) { return scanProfile(row) })
}

// scanProfileRevision reads a stored revision, after the columns lead, and checks its content
// against its hash: a revision whose content does not hash to what was stored is an error, never
// an answer.
func scanProfileRevision(row pgx.Row, lead ...any) (*declaration.Revision, error) {
	var r declaration.Revision
	var status, savedKind string
	var content []byte
	var verifiedKind, verifiedID *string
	var verifyEpoch *int64
	var verifiedAt, supersededAt *time.Time
	var evidence []string
	err := row.Scan(append(lead, &r.Number, &status, &content, &r.Hash, &savedKind, &r.SavedBy.ID, &r.SavedEpoch, &r.SavedAt,
		&verifiedKind, &verifiedID, &verifyEpoch, &verifiedAt, &evidence, &supersededAt)...)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(content, &r.Content); err != nil {
		return nil, fmt.Errorf("cat: declaration profile revision %d: %w", r.Number, err)
	}
	r.Content = declaration.Normalize(r.Content)
	if r.Content.Hash() != r.Hash {
		return nil, fmt.Errorf("cat: declaration profile revision %d does not match its content hash", r.Number)
	}
	r.Status, r.SavedBy.Kind, r.SavedAt = declaration.Status(status), kernel.ActorKind(savedKind), kernel.Instant(r.SavedAt)
	if verifiedAt != nil {
		r.Verification = &declaration.Verification{By: kernel.ActorRef{Kind: kernel.ActorKind(*verifiedKind), ID: *verifiedID},
			Epoch: *verifyEpoch, At: kernel.Instant(*verifiedAt), EvidenceIDs: evidence}
	}
	if supersededAt != nil {
		r.SupersededAt = kernel.Instant(*supersededAt)
	}
	return &r, nil
}

// ownedRevision is a revision with the ID of its profile.
type ownedRevision struct {
	ProfileID string
	Revision  *declaration.Revision
}

func collectOwnedRevisions(rows pgx.Rows, err error) ([]ownedRevision, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (ownedRevision, error) {
		var o ownedRevision
		var err error
		o.Revision, err = scanProfileRevision(row, &o.ProfileID)
		return o, err
	})
}

// attach sets the draft and verified revisions of roots.
func attach(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, roots []*declaration.Root) error {
	if len(roots) == 0 {
		return nil
	}
	byID := make(map[string]*declaration.Root, len(roots))
	ids := make([]string, len(roots))
	for i, r := range roots {
		byID[r.ID], ids[i] = r, r.ID
	}
	list, err := collectOwnedRevisions(exec.Query(ctx, `SELECT profile_id, `+profileRevisionColumns+`
		FROM brant_cat.declaration_profile_revision WHERE tenant_id = $1 AND company_id = $2 AND profile_id = ANY ($3)
		AND status IN ('draft', 'verified') ORDER BY profile_id, revision`, string(scope.Tenant), string(scope.Company), ids))
	for _, o := range list {
		switch o.Revision.Status {
		case declaration.Draft:
			byID[o.ProfileID].Draft = o.Revision
		case declaration.Verified:
			byID[o.ProfileID].Verified = o.Revision
		}
	}
	return err
}

// LoadProfile returns the profile id with its draft and verified revisions, or nil; forUpdate locks
// the root row.
func LoadProfile(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, forUpdate bool) (*declaration.Root, error) {
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE"
	}
	r, err := scanProfile(exec.QueryRow(ctx, `SELECT `+profileColumns+` FROM brant_cat.declaration_profile
		WHERE tenant_id = $1 AND company_id = $2 AND profile_id = $3`+lock, string(scope.Tenant), string(scope.Company), id))
	if err != nil || r == nil {
		return nil, err
	}
	return r, attach(ctx, exec, scope, []*declaration.Root{r})
}

// ProfileRevisions returns every revision of the profile id, oldest first.
func ProfileRevisions(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) ([]*declaration.Revision, error) {
	rows, err := exec.Query(ctx, `SELECT `+profileRevisionColumns+` FROM brant_cat.declaration_profile_revision
		WHERE tenant_id = $1 AND company_id = $2 AND profile_id = $3 ORDER BY revision`, string(scope.Tenant), string(scope.Company), id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*declaration.Revision, error) { return scanProfileRevision(row) })
}

// ProfilesOfSku returns the profiles kept for a SKU with their draft and verified revisions, by
// specification revision and code table version, without locks.
func ProfilesOfSku(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, skuID string) ([]*declaration.Root, error) {
	roots, err := collectProfiles(exec.Query(ctx, `SELECT `+profileColumns+` FROM brant_cat.declaration_profile
		WHERE tenant_id = $1 AND company_id = $2 AND sku_id = $3 ORDER BY spec_revision, code_table_version`,
		string(scope.Tenant), string(scope.Company), skuID))
	if err != nil {
		return nil, err
	}
	return roots, attach(ctx, exec, scope, roots)
}

// ProfilesFor returns the profiles among keys with their draft and verified revisions, without
// locks.
func ProfilesFor(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, keys []declaration.Key) (map[declaration.Key]*declaration.Root, error) {
	skus, revs, tables := make([]string, len(keys)), make([]int64, len(keys)), make([]string, len(keys))
	for i, k := range keys {
		skus[i], revs[i], tables[i] = k.SkuID, k.SpecRevision, k.CodeTableVersion
	}
	roots, err := collectProfiles(exec.Query(ctx, `SELECT `+profileColumns+` FROM brant_cat.declaration_profile
		WHERE tenant_id = $1 AND company_id = $2
		AND (sku_id, spec_revision, code_table_version) IN (SELECT * FROM unnest($3::text[], $4::bigint[], $5::text[]))`,
		string(scope.Tenant), string(scope.Company), skus, revs, tables))
	if err == nil {
		err = attach(ctx, exec, scope, roots)
	}
	out := make(map[declaration.Key]*declaration.Root, len(roots))
	for _, r := range roots {
		out[r.Key] = r
	}
	return out, err
}

// RevisionRef names one revision of a profile.
type RevisionRef struct {
	ProfileID string
	Revision  int64
}

// ProfileRevisionsAt returns the revisions among refs that exist, without locks.
func ProfileRevisionsAt(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, refs []RevisionRef) (map[RevisionRef]*declaration.Revision, error) {
	out := make(map[RevisionRef]*declaration.Revision, len(refs))
	if len(refs) == 0 {
		return out, nil
	}
	ids, numbers := make([]string, len(refs)), make([]int64, len(refs))
	for i, r := range refs {
		ids[i], numbers[i] = r.ProfileID, r.Revision
	}
	list, err := collectOwnedRevisions(exec.Query(ctx, `SELECT profile_id, `+profileRevisionColumns+`
		FROM brant_cat.declaration_profile_revision WHERE tenant_id = $1 AND company_id = $2
		AND (profile_id, revision) IN (SELECT * FROM unnest($3::text[], $4::bigint[]))`,
		string(scope.Tenant), string(scope.Company), ids, numbers))
	for _, o := range list {
		out[RevisionRef{ProfileID: o.ProfileID, Revision: o.Revision.Number}] = o.Revision
	}
	return out, err
}

// ProfileKeyTaken reports whether a profile has key. Two creations racing past this check collide
// on declaration_profile_key; the ledger retries the loser, which then sees the winner here.
func ProfileKeyTaken(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, key declaration.Key) (bool, error) {
	var taken bool
	err := exec.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM brant_cat.declaration_profile WHERE tenant_id = $1 AND company_id = $2
		AND sku_id = $3 AND spec_revision = $4 AND code_table_version = $5)`,
		string(scope.Tenant), string(scope.Company), key.SkuID, key.SpecRevision, key.CodeTableVersion).Scan(&taken)
	return taken, err
}

// InsertProfile stores a new profile root.
func InsertProfile(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *declaration.Root, at time.Time) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_cat.declaration_profile (tenant_id, company_id, profile_id, sku_id, spec_revision,
		code_table_version, root_version, last_revision, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9)`,
		string(scope.Tenant), string(scope.Company), r.ID, r.Key.SkuID, r.Key.SpecRevision, r.Key.CodeTableVersion, r.Version,
		r.LastRevision, at)
	return err
}

// UpdateProfile writes r if the stored root is still at version from.
func UpdateProfile(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *declaration.Root, from int64, at time.Time) error {
	tag, err := exec.Exec(ctx, `UPDATE brant_cat.declaration_profile SET root_version = $4, last_revision = $5, updated_at = $6
		WHERE tenant_id = $1 AND company_id = $2 AND profile_id = $3 AND root_version = $7`,
		string(scope.Tenant), string(scope.Company), r.ID, r.Version, r.LastRevision, at, from)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

type profileRevisionRow struct {
	verifiedKind, verifiedID *string
	verifyEpoch              *int64
	verifiedAt, supersededAt *time.Time
	evidence                 []string
}

func profileRow(rev *declaration.Revision) profileRevisionRow {
	var w profileRevisionRow
	if v := rev.Verification; v != nil {
		kind := string(v.By.Kind)
		w.verifiedKind, w.verifiedID, w.verifyEpoch, w.verifiedAt, w.evidence = &kind, &v.By.ID, &v.Epoch, &v.At, v.EvidenceIDs
	}
	if !rev.SupersededAt.IsZero() {
		w.supersededAt = &rev.SupersededAt
	}
	return w
}

// InsertProfileRevision stores a new revision of r.
func InsertProfileRevision(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *declaration.Root, rev *declaration.Revision) error {
	w := profileRow(rev)
	_, err := exec.Exec(ctx, `INSERT INTO brant_cat.declaration_profile_revision (tenant_id, company_id, profile_id, revision, status,
		content, content_hash, saved_by_kind, saved_by_id, saved_epoch, saved_at, verified_by_kind, verified_by_id, verify_epoch,
		verified_at, evidence_ids, superseded_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`,
		string(scope.Tenant), string(scope.Company), r.ID, rev.Number, string(rev.Status), rev.Content.Canonical(), rev.Hash,
		string(rev.SavedBy.Kind), rev.SavedBy.ID, rev.SavedEpoch, rev.SavedAt, w.verifiedKind, w.verifiedID, w.verifyEpoch, w.verifiedAt,
		w.evidence, w.supersededAt)
	return err
}

// UpdateProfileRevision writes the state of a stored revision of r; the trigger refuses every change
// that is not a revision transition or an edit of a draft.
func UpdateProfileRevision(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *declaration.Root, rev *declaration.Revision) error {
	w := profileRow(rev)
	tag, err := exec.Exec(ctx, `UPDATE brant_cat.declaration_profile_revision SET status = $5, content = $6, content_hash = $7,
		saved_by_kind = $8, saved_by_id = $9, saved_epoch = $10, saved_at = $11, verified_by_kind = $12, verified_by_id = $13,
		verify_epoch = $14, verified_at = $15, evidence_ids = $16, superseded_at = $17
		WHERE tenant_id = $1 AND company_id = $2 AND profile_id = $3 AND revision = $4`,
		string(scope.Tenant), string(scope.Company), r.ID, rev.Number, string(rev.Status), rev.Content.Canonical(), rev.Hash,
		string(rev.SavedBy.Kind), rev.SavedBy.ID, rev.SavedEpoch, rev.SavedAt, w.verifiedKind, w.verifiedID, w.verifyEpoch, w.verifiedAt,
		w.evidence, w.supersededAt)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}
