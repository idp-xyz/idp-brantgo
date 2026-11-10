// Package postgres stores POL configurations, document number sequences and foundation snapshots
// in schema brant_pol, on the executor of POL's own transaction.
package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pol/internal/domain/config"
	"go.idp.xyz/idp-brant/internal/contexts/pol/internal/domain/numbering"
	"go.idp.xyz/idp-brant/internal/contexts/pol/internal/domain/snapshot"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Schema is POL's schema.
const Schema = "brant_pol"

// ErrConflict means a compare-and-set update found another version.
var ErrConflict = errors.New("pol: version changed")

const revisionColumns = `kind, config_id, revision, status, data, content_hash, valid_from, valid_until, saved_by_kind, saved_by_id,
	saved_epoch, saved_at, published_by_kind, published_by_id, publish_epoch, published_at, evidence_ids, ended_at`

// Key names one revision.
type Key struct {
	Kind     string
	ConfigID string
	Revision int64
}

// Stored is a revision with the key it is stored under.
type Stored struct {
	Key Key
	Rev *config.Revision
}

func scanRevision(row pgx.Row) (Stored, error) {
	var s Stored
	var r config.Revision
	var status, savedKind string
	var data []byte
	var until, publishedAt, endedAt *time.Time
	var publishedKind, publishedID *string
	var publishEpoch *int64
	var evidence []string
	err := row.Scan(&s.Key.Kind, &s.Key.ConfigID, &s.Key.Revision, &status, &data, &r.Hash, &r.Content.ValidFrom, &until, &savedKind,
		&r.SavedBy.ID, &r.SavedEpoch, &r.SavedAt, &publishedKind, &publishedID, &publishEpoch, &publishedAt, &evidence, &endedAt)
	if err != nil {
		return Stored{}, err
	}
	r.Number, r.Status, r.SavedBy.Kind = s.Key.Revision, config.Status(status), kernel.ActorKind(savedKind)
	r.Content.Kind, r.Content.ValidFrom, r.SavedAt = config.Kind(s.Key.Kind), kernel.Instant(r.Content.ValidFrom), kernel.Instant(r.SavedAt)
	if until != nil {
		r.Content.ValidUntil = kernel.Instant(*until)
	}
	if endedAt != nil {
		r.EndedAt = kernel.Instant(*endedAt)
	}
	if publishedAt != nil {
		r.Publication = &config.Publication{By: kernel.ActorRef{Kind: kernel.ActorKind(*publishedKind), ID: *publishedID},
			Epoch: *publishEpoch, At: kernel.Instant(*publishedAt), EvidenceIDs: evidence}
	}
	if err = r.Content.DecodeData(data); err != nil {
		return Stored{}, err
	}
	s.Rev = &r
	return s, nil
}

func collectRevisions(rows pgx.Rows, err error) ([]Stored, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Stored, error) { return scanRevision(row) })
}

// LoadRoot returns the configuration (kind, id) with its working, active and latest published
// revisions, or nil; forUpdate locks the root row.
func LoadRoot(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, kind, id string, forUpdate bool) (*config.Root, error) {
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE"
	}
	t, c := string(scope.Tenant), string(scope.Company)
	r := config.New(config.Kind(kind), id)
	err := exec.QueryRow(ctx, `SELECT root_version, last_revision FROM brant_pol.config_root
		WHERE tenant_id = $1 AND company_id = $2 AND kind = $3 AND config_id = $4`+lock, t, c, kind, id).Scan(&r.Version, &r.LastRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	revisions, err := collectRevisions(exec.Query(ctx, `SELECT `+revisionColumns+` FROM brant_pol.config_revision
		WHERE tenant_id = $1 AND company_id = $2 AND kind = $3 AND config_id = $4
		AND (status IN ('working', 'active') OR revision = (SELECT max(revision) FROM brant_pol.config_revision
			WHERE tenant_id = $1 AND company_id = $2 AND kind = $3 AND config_id = $4 AND published_at IS NOT NULL))
		ORDER BY revision`, t, c, kind, id))
	for _, s := range revisions {
		switch s.Rev.Status {
		case config.Working:
			r.Working = s.Rev
		case config.Active:
			r.Active = s.Rev
		}
		if s.Rev.Publication != nil {
			r.Definition = s.Rev
		}
	}
	return r, err
}

// Revisions returns every revision of the configuration (kind, id), oldest first.
func Revisions(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, kind, id string) ([]Stored, error) {
	return collectRevisions(exec.Query(ctx, `SELECT `+revisionColumns+` FROM brant_pol.config_revision
		WHERE tenant_id = $1 AND company_id = $2 AND kind = $3 AND config_id = $4 ORDER BY revision`,
		string(scope.Tenant), string(scope.Company), kind, id))
}

// RevisionsByKey returns the revisions among keys that exist, without locks.
func RevisionsByKey(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, keys []Key) (map[Key]*config.Revision, error) {
	kinds, ids, numbers := make([]string, len(keys)), make([]string, len(keys)), make([]int64, len(keys))
	for i, k := range keys {
		kinds[i], ids[i], numbers[i] = k.Kind, k.ConfigID, k.Revision
	}
	stored, err := collectRevisions(exec.Query(ctx, `SELECT `+revisionColumns+` FROM brant_pol.config_revision
		WHERE tenant_id = $1 AND company_id = $2
		AND (kind, config_id, revision) IN (SELECT * FROM unnest($3::text[], $4::text[], $5::bigint[]))`,
		string(scope.Tenant), string(scope.Company), kinds, ids, numbers))
	out := make(map[Key]*config.Revision, len(stored))
	for _, s := range stored {
		out[s.Key] = s.Rev
	}
	return out, err
}

// LoadRevision returns one revision, or nil.
func LoadRevision(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, key Key) (*config.Revision, error) {
	found, err := RevisionsByKey(ctx, exec, scope, []Key{key})
	return found[key], err
}

// PublishedAt returns the revision of (kind, id) that was published and not yet superseded or
// retired at instant at, if any, and whether the configuration exists at all.
func PublishedAt(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, kind, id string, at time.Time) (*config.Revision, bool, error) {
	t, c := string(scope.Tenant), string(scope.Company)
	var exists bool
	err := exec.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM brant_pol.config_root WHERE tenant_id = $1 AND company_id = $2
		AND kind = $3 AND config_id = $4)`, t, c, kind, id).Scan(&exists)
	if err != nil || !exists {
		return nil, exists, err
	}
	stored, err := collectRevisions(exec.Query(ctx, `SELECT `+revisionColumns+` FROM brant_pol.config_revision
		WHERE tenant_id = $1 AND company_id = $2 AND kind = $3 AND config_id = $4
		AND published_at <= $5 AND (ended_at IS NULL OR ended_at > $5) ORDER BY revision DESC LIMIT 1`, t, c, kind, id, at))
	if err != nil || len(stored) == 0 {
		return nil, true, err
	}
	return stored[0].Rev, true, nil
}

// InsertRoot stores a new configuration.
func InsertRoot(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *config.Root, at time.Time) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_pol.config_root (tenant_id, company_id, kind, config_id, root_version, last_revision,
		created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $7)`,
		string(scope.Tenant), string(scope.Company), string(r.Kind), r.ID, r.Version, r.LastRevision, at)
	return err
}

// UpdateRoot writes r if the stored root is still at version from.
func UpdateRoot(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *config.Root, from int64, at time.Time) error {
	tag, err := exec.Exec(ctx, `UPDATE brant_pol.config_root SET root_version = $5, last_revision = $6, updated_at = $7
		WHERE tenant_id = $1 AND company_id = $2 AND kind = $3 AND config_id = $4 AND root_version = $8`,
		string(scope.Tenant), string(scope.Company), string(r.Kind), r.ID, r.Version, r.LastRevision, at, from)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

type revisionRow struct {
	data                        []byte
	until, publishedAt, endedAt *time.Time
	publishedKind, publishedID  *string
	publishEpoch                *int64
	evidence                    []string
}

func row(rev *config.Revision) (revisionRow, error) {
	var w revisionRow
	var err error
	if w.data, err = rev.Content.DataJSON(); err != nil {
		return w, err
	}
	if !rev.Content.ValidUntil.IsZero() {
		w.until = &rev.Content.ValidUntil
	}
	if !rev.EndedAt.IsZero() {
		w.endedAt = &rev.EndedAt
	}
	if p := rev.Publication; p != nil {
		kind := string(p.By.Kind)
		w.publishedKind, w.publishedID, w.publishEpoch, w.publishedAt, w.evidence = &kind, &p.By.ID, &p.Epoch, &p.At, p.EvidenceIDs
	}
	return w, nil
}

// InsertRevision stores a new revision of r.
func InsertRevision(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *config.Root, rev *config.Revision) error {
	w, err := row(rev)
	if err != nil {
		return err
	}
	_, err = exec.Exec(ctx, `INSERT INTO brant_pol.config_revision (tenant_id, company_id, kind, config_id, revision, status, data,
		content_hash, valid_from, valid_until, saved_by_kind, saved_by_id, saved_epoch, saved_at, published_by_kind, published_by_id,
		publish_epoch, published_at, evidence_ids, ended_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)`,
		string(scope.Tenant), string(scope.Company), string(r.Kind), r.ID, rev.Number, string(rev.Status), w.data, rev.Hash,
		rev.Content.ValidFrom, w.until, string(rev.SavedBy.Kind), rev.SavedBy.ID, rev.SavedEpoch, rev.SavedAt, w.publishedKind,
		w.publishedID, w.publishEpoch, w.publishedAt, w.evidence, w.endedAt)
	return err
}

// UpdateRevision writes the state of a stored revision of r; the trigger refuses every change that
// is not a revision transition.
func UpdateRevision(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *config.Root, rev *config.Revision) error {
	w, err := row(rev)
	if err != nil {
		return err
	}
	tag, err := exec.Exec(ctx, `UPDATE brant_pol.config_revision SET status = $6, data = $7, content_hash = $8, valid_from = $9,
		valid_until = $10, saved_by_kind = $11, saved_by_id = $12, saved_epoch = $13, saved_at = $14, published_by_kind = $15,
		published_by_id = $16, publish_epoch = $17, published_at = $18, evidence_ids = $19, ended_at = $20
		WHERE tenant_id = $1 AND company_id = $2 AND kind = $3 AND config_id = $4 AND revision = $5`,
		string(scope.Tenant), string(scope.Company), string(r.Kind), r.ID, rev.Number, string(rev.Status), w.data, rev.Hash,
		rev.Content.ValidFrom, w.until, string(rev.SavedBy.Kind), rev.SavedBy.ID, rev.SavedEpoch, rev.SavedAt, w.publishedKind,
		w.publishedID, w.publishEpoch, w.publishedAt, w.evidence, w.endedAt)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// LockCounter locks the counter of a document type and year FOR UPDATE; a year without numbers
// yet returns an unstored counter at 0.
func LockCounter(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, documentType string, year int) (*numbering.Counter, error) {
	c := &numbering.Counter{DocumentType: documentType, Year: year}
	err := exec.QueryRow(ctx, `SELECT last_value FROM brant_pol.number_counter
		WHERE tenant_id = $1 AND company_id = $2 AND document_type = $3 AND year = $4 FOR UPDATE`,
		string(scope.Tenant), string(scope.Company), documentType, year).Scan(&c.Last)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, nil
	}
	c.Stored = err == nil
	return c, err
}

// SaveCounter stores c, which was at value from when it was locked. Two first numbers of a year
// racing to insert the counter collide on its key; the ledger retries the loser, which then locks
// the stored counter.
func SaveCounter(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, c *numbering.Counter, from int64, at time.Time) error {
	t, co := string(scope.Tenant), string(scope.Company)
	if !c.Stored {
		_, err := exec.Exec(ctx, `INSERT INTO brant_pol.number_counter (tenant_id, company_id, document_type, year, last_value, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6)`, t, co, c.DocumentType, c.Year, c.Last, at)
		c.Stored = err == nil
		return err
	}
	tag, err := exec.Exec(ctx, `UPDATE brant_pol.number_counter SET last_value = $5, updated_at = $6
		WHERE tenant_id = $1 AND company_id = $2 AND document_type = $3 AND year = $4 AND last_value = $7`,
		t, co, c.DocumentType, c.Year, c.Last, at, from)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

const issuedColumns = `document_number, document_type, year, sequence_value, business_ref, sequence_config_id, sequence_revision`

func scanIssued(row pgx.Row) (*numbering.Issued, error) {
	var n numbering.Issued
	err := row.Scan(&n.Number, &n.DocumentType, &n.Year, &n.Value, &n.BusinessRef, &n.SequenceID, &n.SequenceRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &n, err
}

// IssuedFor returns the number issued for a business reference of a document type, or nil.
func IssuedFor(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, documentType, businessRef string) (*numbering.Issued, error) {
	return scanIssued(exec.QueryRow(ctx, `SELECT `+issuedColumns+` FROM brant_pol.issued_number
		WHERE tenant_id = $1 AND company_id = $2 AND document_type = $3 AND business_ref = $4`,
		string(scope.Tenant), string(scope.Company), documentType, businessRef))
}

// IssuedNumber returns an issued number, or nil.
func IssuedNumber(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, number string) (*numbering.Issued, error) {
	return scanIssued(exec.QueryRow(ctx, `SELECT `+issuedColumns+` FROM brant_pol.issued_number
		WHERE tenant_id = $1 AND company_id = $2 AND document_number = $3`, string(scope.Tenant), string(scope.Company), number))
}

// InsertIssued stores an issued number for good.
func InsertIssued(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, n numbering.Issued, by kernel.ActorRef, epoch int64, at time.Time) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_pol.issued_number (tenant_id, company_id, document_number, document_type, year,
		sequence_value, business_ref, sequence_kind, sequence_config_id, sequence_revision, issued_by_kind, issued_by_id,
		authority_epoch, issued_at) VALUES ($1, $2, $3, $4, $5, $6, $7, 'number_sequence', $8, $9, $10, $11, $12, $13)`,
		string(scope.Tenant), string(scope.Company), n.Number, n.DocumentType, n.Year, n.Value, n.BusinessRef, n.SequenceID,
		n.SequenceRevision, string(by.Kind), by.ID, epoch, at)
	return err
}

// SnapshotExists reports whether a snapshot ID is taken.
func SnapshotExists(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) (bool, error) {
	var exists bool
	err := exec.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM brant_pol.foundation_snapshot WHERE tenant_id = $1 AND company_id = $2
		AND snapshot_id = $3)`, string(scope.Tenant), string(scope.Company), id).Scan(&exists)
	return exists, err
}

// InsertSnapshot stores a snapshot and its entries for good.
func InsertSnapshot(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, s *snapshot.Snapshot) error {
	t, c := string(scope.Tenant), string(scope.Company)
	_, err := exec.Exec(ctx, `INSERT INTO brant_pol.foundation_snapshot (tenant_id, company_id, snapshot_id, purpose, content_hash,
		captured_by_kind, captured_by_id, authority_epoch, captured_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		t, c, s.ID, s.Purpose, s.Hash, string(s.CapturedBy.Kind), s.CapturedBy.ID, s.Epoch, s.CapturedAt)
	for _, e := range s.Entries {
		if err != nil {
			return err
		}
		_, err = exec.Exec(ctx, `INSERT INTO brant_pol.snapshot_entry (tenant_id, company_id, snapshot_id, kind, config_id, revision,
			content_hash) VALUES ($1, $2, $3, $4, $5, $6, $7)`, t, c, s.ID, e.Kind, e.ConfigID, e.Revision, e.ContentHash)
	}
	return err
}

// LoadSnapshot returns a snapshot with its entries, or nil.
func LoadSnapshot(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) (*snapshot.Snapshot, error) {
	t, c := string(scope.Tenant), string(scope.Company)
	s := snapshot.Snapshot{ID: id}
	var kind string
	err := exec.QueryRow(ctx, `SELECT purpose, content_hash, captured_by_kind, captured_by_id, authority_epoch, captured_at
		FROM brant_pol.foundation_snapshot WHERE tenant_id = $1 AND company_id = $2 AND snapshot_id = $3`, t, c, id).
		Scan(&s.Purpose, &s.Hash, &kind, &s.CapturedBy.ID, &s.Epoch, &s.CapturedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.CapturedBy.Kind, s.CapturedAt = kernel.ActorKind(kind), kernel.Instant(s.CapturedAt)
	rows, err := exec.Query(ctx, `SELECT kind, config_id, revision, content_hash FROM brant_pol.snapshot_entry
		WHERE tenant_id = $1 AND company_id = $2 AND snapshot_id = $3 ORDER BY kind COLLATE "C", config_id COLLATE "C", revision`, t, c, id)
	if err != nil {
		return nil, err
	}
	s.Entries, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (snapshot.Entry, error) {
		var e snapshot.Entry
		return e, row.Scan(&e.Kind, &e.ConfigID, &e.Revision, &e.ContentHash)
	})
	return &s, err
}
