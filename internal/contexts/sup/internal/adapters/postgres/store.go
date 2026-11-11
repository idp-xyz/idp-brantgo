// Package postgres stores SUP's supplier RFQs and supply conditions in schema brant_sup, on the
// executor of SUP's own transaction.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/sup/internal/domain/revision"
	"go.idp.xyz/idp-brant/internal/contexts/sup/internal/domain/supplierrfq"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Schema is SUP's schema.
const Schema = "brant_sup"

// ErrConflict means a compare-and-set update found another version.
var ErrConflict = errors.New("sup: version changed")

type evidenceJSON struct {
	SourceID string `json:"source_id"`
	Locator  string `json:"locator"`
}

func encodeEvidence(ev []revision.Evidence) ([]byte, error) {
	out := make([]evidenceJSON, len(ev))
	for i, e := range ev {
		out[i] = evidenceJSON(e)
	}
	return json.Marshal(out)
}

func decodeEvidence(raw []byte) ([]revision.Evidence, error) {
	var in []evidenceJSON
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	out := make([]revision.Evidence, len(in))
	for i, e := range in {
		out[i] = revision.Evidence(e)
	}
	return out, nil
}

// act is the columns of a recorded act, nullable.
type act struct {
	kind, id, reason *string
	epoch            *int64
	at               *time.Time
	evidence         []byte
}

func (a act) decode() (*revision.Act, error) {
	if a.at == nil {
		return nil, nil
	}
	out := &revision.Act{At: kernel.Instant(*a.at)}
	if a.kind != nil && a.id != nil {
		out.By = kernel.ActorRef{Kind: kernel.ActorKind(*a.kind), ID: *a.id}
	}
	if a.epoch != nil {
		out.Epoch = *a.epoch
	}
	if a.reason != nil {
		out.Reason = *a.reason
	}
	if a.evidence != nil {
		var err error
		if out.Evidence, err = decodeEvidence(a.evidence); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// actArgs returns the column values of a: kind, id, epoch, at, reason and evidence, all NULL for a
// nil act; evidence NULL for an act without it (a discard).
func actArgs(a *revision.Act, withEvidence bool) ([]any, error) {
	if a == nil {
		return []any{nil, nil, nil, nil, nil, nil}, nil
	}
	var reason, evidence any
	if a.Reason != "" {
		reason = a.Reason
	}
	if withEvidence {
		raw, err := encodeEvidence(a.Evidence)
		if err != nil {
			return nil, err
		}
		evidence = raw
	}
	return []any{string(a.By.Kind), a.By.ID, a.Epoch, a.At, reason, evidence}, nil
}

const rfqColumns = `supplier_rfq_id, status, row_version, data, content_hash, owner_actor_id, coalesce(owner_team_id, ''), saved_by_kind,
	saved_by_id, saved_epoch, saved_at, issued_version, issued_by_kind, issued_by_id, issue_epoch, issued_at, issue_reason, issue_evidence,
	closed_by_kind, closed_by_id, close_epoch, closed_at, close_reason, close_evidence`

// LoadSupplierRfq returns the supplier RFQ id, or nil; forUpdate locks its row.
func LoadSupplierRfq(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, forUpdate bool) (*supplierrfq.SupplierRfq, error) {
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE"
	}
	var r supplierrfq.SupplierRfq
	var status, savedKind string
	var issuedVersion *int64
	var issue, closing act
	err := exec.QueryRow(ctx, `SELECT `+rfqColumns+` FROM brant_sup.supplier_rfq WHERE tenant_id = $1 AND company_id = $2
		AND supplier_rfq_id = $3`+lock, string(scope.Tenant), string(scope.Company), id).
		Scan(&r.ID, &status, &r.RowVersion, &r.Data, &r.Hash, &r.OwnerActor, &r.OwnerTeam, &savedKind, &r.SavedBy.ID, &r.SavedEpoch, &r.SavedAt,
			&issuedVersion, &issue.kind, &issue.id, &issue.epoch, &issue.at, &issue.reason, &issue.evidence,
			&closing.kind, &closing.id, &closing.epoch, &closing.at, &closing.reason, &closing.evidence)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.Status, r.SavedBy.Kind, r.SavedAt = supplierrfq.Status(status), kernel.ActorKind(savedKind), kernel.Instant(r.SavedAt)
	if issuedVersion != nil {
		r.IssuedVersion = *issuedVersion
	}
	if r.Issue, err = issue.decode(); err != nil {
		return nil, err
	}
	r.Closing, err = closing.decode()
	return &r, err
}

// InsertSupplierRfq stores a new draft supplier RFQ.
func InsertSupplierRfq(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *supplierrfq.SupplierRfq, at time.Time) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_sup.supplier_rfq (tenant_id, company_id, supplier_rfq_id, status, row_version, data, content_hash,
		owner_actor_id, owner_team_id, saved_by_kind, saved_by_id, saved_epoch, saved_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, nullif($9, ''), $10, $11, $12, $13, $14, $14)`,
		string(scope.Tenant), string(scope.Company), r.ID, string(r.Status), r.RowVersion, r.Data, r.Hash, r.OwnerActor, r.OwnerTeam,
		string(r.SavedBy.Kind), r.SavedBy.ID, r.SavedEpoch, r.SavedAt, at)
	return err
}

// UpdateSupplierRfq writes r if the stored row is still at version from; the trigger refuses every
// change of an issued request but its closing.
func UpdateSupplierRfq(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *supplierrfq.SupplierRfq, from int64, at time.Time) error {
	issue, err := actArgs(r.Issue, true)
	if err != nil {
		return err
	}
	closing, err := actArgs(r.Closing, true)
	if err != nil {
		return err
	}
	var issuedVersion any
	if r.IssuedVersion > 0 {
		issuedVersion = r.IssuedVersion
	}
	args := []any{string(scope.Tenant), string(scope.Company), r.ID, from, string(r.Status), r.RowVersion, r.Data, r.Hash,
		string(r.SavedBy.Kind), r.SavedBy.ID, r.SavedEpoch, r.SavedAt, issuedVersion, at}
	args = append(append(args, issue...), closing...)
	tag, err := exec.Exec(ctx, `UPDATE brant_sup.supplier_rfq SET status = $5, row_version = $6, data = $7, content_hash = $8,
		saved_by_kind = $9, saved_by_id = $10, saved_epoch = $11, saved_at = $12, issued_version = $13, updated_at = $14,
		issued_by_kind = $15, issued_by_id = $16, issue_epoch = $17, issued_at = $18, issue_reason = $19, issue_evidence = $20,
		closed_by_kind = $21, closed_by_id = $22, close_epoch = $23, closed_at = $24, close_reason = $25, close_evidence = $26
		WHERE tenant_id = $1 AND company_id = $2 AND supplier_rfq_id = $3 AND row_version = $4`, args...)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

const revisionColumns = `r.kind, r.condition_id, r.revision, r.status, coalesce(r.base_revision, 0), r.data, r.content_hash, r.valid_from,
	r.valid_until, coalesce(r.validity_mode, ''), coalesce(r.source_offer_id, ''), coalesce(r.source_offer_revision, 0), r.saved_by_kind,
	r.saved_by_id, r.saved_epoch, r.saved_at, r.contributors, r.verified_by_kind, r.verified_by_id, r.verify_epoch, r.verified_at,
	r.verification_evidence, r.superseded_at, r.ended_by_kind, r.ended_by_id, r.end_epoch, r.ended_at, r.end_reason, r.end_evidence`

// Key names one revision.
type Key struct {
	Kind     string
	ID       string
	Revision int64
}

// Stored is a revision with its key and the owner of its root.
type Stored struct {
	Key        Key
	Rev        *revision.Revision
	OwnerActor string
	OwnerTeam  string
}

func scanRevision(row pgx.Row, owner bool) (Stored, error) {
	var s Stored
	var rev revision.Revision
	var status, savedKind string
	var from, until, supersededAt *time.Time
	var verification, end act
	dest := []any{&s.Key.Kind, &s.Key.ID, &s.Key.Revision, &status, &rev.Base, &rev.Content.Data, &rev.Content.Hash, &from, &until,
		&rev.Content.Header.ValidityMode, &rev.Content.Header.SourceID, &rev.Content.Header.SourceRevision, &savedKind, &rev.SavedBy.ID,
		&rev.SavedEpoch, &rev.SavedAt, &rev.Contributors, &verification.kind, &verification.id, &verification.epoch, &verification.at,
		&verification.evidence, &supersededAt, &end.kind, &end.id, &end.epoch, &end.at, &end.reason, &end.evidence}
	if owner {
		dest = append(dest, &s.OwnerActor, &s.OwnerTeam)
	}
	if err := row.Scan(dest...); err != nil {
		return Stored{}, err
	}
	rev.Number, rev.Status, rev.SavedBy.Kind, rev.SavedAt = s.Key.Revision, revision.Status(status), kernel.ActorKind(savedKind), kernel.Instant(rev.SavedAt)
	if from != nil {
		rev.Content.Header.ValidFrom = kernel.Instant(*from)
	}
	if until != nil {
		rev.Content.Header.ValidUntil = kernel.Instant(*until)
	}
	if supersededAt != nil {
		rev.SupersededAt = kernel.Instant(*supersededAt)
	}
	var err error
	if rev.Verification, err = verification.decode(); err != nil {
		return Stored{}, err
	}
	if rev.End, err = end.decode(); err != nil {
		return Stored{}, err
	}
	s.Rev = &rev
	return s, nil
}

func collectRevisions(rows pgx.Rows, err error) ([]Stored, error) { return collect(rows, err, false) }

// collectOwned collects revisions selected with their roots' owner columns.
func collectOwned(rows pgx.Rows, err error) ([]Stored, error) { return collect(rows, err, true) }

func collect(rows pgx.Rows, err error, owner bool) ([]Stored, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Stored, error) { return scanRevision(row, owner) })
}

// LoadRoot returns the supply condition (kind, id) with its draft and active revisions and, when
// named is positive, the revision of that number; nil when it does not exist. forUpdate locks the
// root row.
func LoadRoot(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, kind, id string, named int64, forUpdate bool) (*revision.Root, error) {
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE"
	}
	t, c := string(scope.Tenant), string(scope.Company)
	r := revision.Root{Kind: revision.Kind(kind), ID: id}
	var team *string
	err := exec.QueryRow(ctx, `SELECT root_version, last_revision, owner_actor_id, owner_team_id FROM brant_sup.condition_root
		WHERE tenant_id = $1 AND company_id = $2 AND kind = $3 AND condition_id = $4`+lock, t, c, kind, id).
		Scan(&r.Version, &r.LastRevision, &r.OwnerActor, &team)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if team != nil {
		r.OwnerTeam = *team
	}
	if err = exec.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM brant_sup.condition_revision WHERE tenant_id = $1 AND company_id = $2
		AND kind = $3 AND condition_id = $4 AND verified_at IS NOT NULL)`, t, c, kind, id).Scan(&r.EverSealed); err != nil {
		return nil, err
	}
	revisions, err := collectRevisions(exec.Query(ctx, `SELECT `+revisionColumns+` FROM brant_sup.condition_revision r
		WHERE r.tenant_id = $1 AND r.company_id = $2 AND r.kind = $3 AND r.condition_id = $4
		AND (r.status IN ('draft', 'active') OR r.revision = $5) ORDER BY r.revision`, t, c, kind, id, named))
	if err != nil {
		return nil, err
	}
	for _, s := range revisions {
		switch s.Rev.Status {
		case revision.Draft:
			r.Working = s.Rev
		case revision.Active:
			r.Active = s.Rev
		default:
			r.Named = s.Rev
		}
	}
	return &r, nil
}

// Revisions returns every revision of the supply condition (kind, id), oldest first.
func Revisions(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, kind, id string) ([]*revision.Revision, error) {
	stored, err := collectRevisions(exec.Query(ctx, `SELECT `+revisionColumns+` FROM brant_sup.condition_revision r
		WHERE r.tenant_id = $1 AND r.company_id = $2 AND r.kind = $3 AND r.condition_id = $4 ORDER BY r.revision`,
		string(scope.Tenant), string(scope.Company), kind, id))
	out := make([]*revision.Revision, len(stored))
	for i, s := range stored {
		out[i] = s.Rev
	}
	return out, err
}

// RevisionsByKey returns the revisions among keys that exist, with their roots' owners, without
// locks.
func RevisionsByKey(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, keys []Key) (map[Key]Stored, error) {
	kinds, ids, numbers := make([]string, len(keys)), make([]string, len(keys)), make([]int64, len(keys))
	for i, k := range keys {
		kinds[i], ids[i], numbers[i] = k.Kind, k.ID, k.Revision
	}
	stored, err := collectOwned(exec.Query(ctx, `SELECT `+revisionColumns+`, o.owner_actor_id, coalesce(o.owner_team_id, '')
		FROM brant_sup.condition_revision r JOIN brant_sup.condition_root o ON o.tenant_id = r.tenant_id AND o.company_id = r.company_id
		AND o.kind = r.kind AND o.condition_id = r.condition_id
		WHERE r.tenant_id = $1 AND r.company_id = $2
		AND (r.kind, r.condition_id, r.revision) IN (SELECT * FROM unnest($3::text[], $4::text[], $5::bigint[]))`,
		string(scope.Tenant), string(scope.Company), kinds, ids, numbers))
	out := make(map[Key]Stored, len(stored))
	for _, s := range stored {
		out[s.Key] = s
	}
	return out, err
}

// InsertRoot stores a new supply condition.
func InsertRoot(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *revision.Root, at time.Time) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_sup.condition_root (tenant_id, company_id, kind, condition_id, root_version, last_revision,
		owner_actor_id, owner_team_id, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, nullif($8, ''), $9, $9)`,
		string(scope.Tenant), string(scope.Company), string(r.Kind), r.ID, r.Version, r.LastRevision, r.OwnerActor, r.OwnerTeam, at)
	return err
}

// UpdateRoot writes r if the stored root is still at version from.
func UpdateRoot(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *revision.Root, from int64, at time.Time) error {
	tag, err := exec.Exec(ctx, `UPDATE brant_sup.condition_root SET root_version = $5, last_revision = $6, updated_at = $7
		WHERE tenant_id = $1 AND company_id = $2 AND kind = $3 AND condition_id = $4 AND root_version = $8`,
		string(scope.Tenant), string(scope.Company), string(r.Kind), r.ID, r.Version, r.LastRevision, at, from)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// revisionArgs returns the base revision of rev and the values of every column a transition may
// write, in the order of the columns InsertRevision and UpdateRevision name.
func revisionArgs(rev *revision.Revision) (any, []any, error) {
	h := rev.Content.Header
	var from, until, mode, source, sourceRevision, base, supersededAt any
	if !h.ValidFrom.IsZero() {
		from = h.ValidFrom
	}
	if !h.ValidUntil.IsZero() {
		until = h.ValidUntil
	}
	if h.ValidityMode != "" {
		mode = h.ValidityMode
	}
	if h.SourceID != "" {
		source, sourceRevision = h.SourceID, h.SourceRevision
	}
	if rev.Base > 0 {
		base = rev.Base
	}
	if !rev.SupersededAt.IsZero() {
		supersededAt = rev.SupersededAt
	}
	verification, err := actArgs(rev.Verification, true)
	if err != nil {
		return nil, nil, err
	}
	end, err := actArgs(rev.End, rev.Status != revision.Discarded)
	if err != nil {
		return nil, nil, err
	}
	args := []any{string(rev.Status), rev.Content.Data, rev.Content.Hash, from, until, mode, source, sourceRevision,
		string(rev.SavedBy.Kind), rev.SavedBy.ID, rev.SavedEpoch, rev.SavedAt, rev.Contributors, supersededAt}
	// verification: kind, id, epoch, at, (reason unused), evidence
	args = append(args, verification[0], verification[1], verification[2], verification[3], verification[5])
	return base, append(args, end...), nil
}

// InsertRevision stores a new revision of r.
func InsertRevision(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *revision.Root, rev *revision.Revision) error {
	base, values, err := revisionArgs(rev)
	if err != nil {
		return err
	}
	args := append([]any{string(scope.Tenant), string(scope.Company), string(r.Kind), r.ID, rev.Number, base}, values...)
	_, err = exec.Exec(ctx, `INSERT INTO brant_sup.condition_revision (tenant_id, company_id, kind, condition_id, revision, base_revision,
		status, data, content_hash, valid_from, valid_until, validity_mode, source_offer_id, source_offer_revision, saved_by_kind,
		saved_by_id, saved_epoch, saved_at, contributors, superseded_at, verified_by_kind, verified_by_id, verify_epoch, verified_at,
		verification_evidence, ended_by_kind, ended_by_id, end_epoch, ended_at, end_reason, end_evidence)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26,
		$27, $28, $29, $30, $31)`, args...)
	return err
}

// UpdateRevision writes the state of a stored revision of r; the trigger refuses every change that
// is not a revision transition.
func UpdateRevision(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *revision.Root, rev *revision.Revision) error {
	_, values, err := revisionArgs(rev)
	if err != nil {
		return err
	}
	args := append([]any{string(scope.Tenant), string(scope.Company), string(r.Kind), r.ID, rev.Number}, values...)
	tag, err := exec.Exec(ctx, `UPDATE brant_sup.condition_revision SET status = $6, data = $7, content_hash = $8,
		valid_from = $9, valid_until = $10, validity_mode = $11, source_offer_id = $12, source_offer_revision = $13, saved_by_kind = $14,
		saved_by_id = $15, saved_epoch = $16, saved_at = $17, contributors = $18, superseded_at = $19, verified_by_kind = $20,
		verified_by_id = $21, verify_epoch = $22, verified_at = $23, verification_evidence = $24, ended_by_kind = $25, ended_by_id = $26,
		end_epoch = $27, ended_at = $28, end_reason = $29, end_evidence = $30
		WHERE tenant_id = $1 AND company_id = $2 AND kind = $3 AND condition_id = $4 AND revision = $5`, args...)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}
