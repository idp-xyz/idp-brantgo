// Package postgres stores CST costing contexts and measurement snapshots in schema brant_cst, on the
// executor of CST's own transaction.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/costing"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/measurement"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Schema is CST's schema.
const Schema = "brant_cst"

// ErrConflict means a compare-and-set update found another root version.
var ErrConflict = errors.New("cst: root version changed")

// Lock modes of a load: none, the root FOR UPDATE (a command on it), or the root FOR SHARE (a
// command of another aggregate that reads it, such as a freeze).
type Lock string

const (
	NoLock    Lock = ""
	ForUpdate Lock = " FOR UPDATE"
	ForShare  Lock = " FOR SHARE"
)

// rfqLine is an RFQ line as a verification stores it.
type rfqLine struct {
	LineID          string `json:"line_id"`
	SkuID           string `json:"sku_id"`
	SkuSpecRevision int64  `json:"sku_spec_revision"`
	Quantity        string `json:"quantity"`
	UOM             string `json:"uom"`
}

type proposal struct {
	ByKind string    `json:"proposed_by_kind"`
	ByID   string    `json:"proposed_by_id"`
	At     time.Time `json:"proposed_at"`
}

// LoadContext returns the costing context id with all its revisions, or nil, locking the root as
// lock says.
func LoadContext(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, lock Lock) (*costing.Context, error) {
	t, c := string(scope.Tenant), string(scope.Company)
	cc := costing.New(id, "", "")
	err := exec.QueryRow(ctx, `SELECT root_version, last_revision, owner_actor_id, coalesce(owner_team_id, '')
		FROM brant_cst.costing_context WHERE tenant_id = $1 AND company_id = $2 AND context_id = $3`+string(lock), t, c, id).
		Scan(&cc.RootVersion, &cc.LastRevision, &cc.OwnerActor, &cc.OwnerTeam)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := exec.Query(ctx, `SELECT revision, status, coalesce(base_revision, 0), data, content_hash, contributors, provenance,
		saved_by_kind, saved_by_id, saved_epoch, saved_at, verified_by_kind, verified_by_id, verify_epoch, verified_at,
		verification_evidence, rfq_id, rfq_row_version, rfq_lines_hash, rfq_lines, superseded_at, ended_by_kind, ended_by_id,
		end_epoch, ended_at, end_reason, end_evidence
		FROM brant_cst.costing_context_revision WHERE tenant_id = $1 AND company_id = $2 AND context_id = $3 ORDER BY revision`, t, c, id)
	if err != nil {
		return nil, err
	}
	cc.Revisions, err = pgx.CollectRows(rows, scanContextRevision)
	return cc, err
}

func scanContextRevision(row pgx.CollectableRow) (*costing.Revision, error) {
	var r costing.Revision
	var status, savedKind string
	var data, provenance, verification, rfqLines, endEvidence []byte
	var verifiedKind, verifiedID, rfqID, rfqHash, endedKind, endedID, endReason *string
	var verifyEpoch, rfqVersion, endEpoch *int64
	var verifiedAt, supersededAt, endedAt *time.Time
	err := row.Scan(&r.Number, &status, &r.Base, &data, &r.Hash, &r.Contributors, &provenance, &savedKind, &r.SavedBy.ID,
		&r.SavedEpoch, &r.SavedAt, &verifiedKind, &verifiedID, &verifyEpoch, &verifiedAt, &verification, &rfqID, &rfqVersion,
		&rfqHash, &rfqLines, &supersededAt, &endedKind, &endedID, &endEpoch, &endedAt, &endReason, &endEvidence)
	if err != nil {
		return nil, err
	}
	r.Status, r.SavedBy.Kind = costing.Status(status), kernel.ActorKind(savedKind)
	r.SavedAt = kernel.Instant(r.SavedAt)
	if err = json.Unmarshal(data, &r.Data); err != nil {
		return nil, err
	}
	var proposals map[string]proposal
	if err = json.Unmarshal(provenance, &proposals); err != nil {
		return nil, err
	}
	r.Proposals = map[string]costing.Proposal{}
	for id, p := range proposals {
		r.Proposals[id] = costing.Proposal{By: kernel.ActorRef{Kind: kernel.ActorKind(p.ByKind), ID: p.ByID}, At: kernel.Instant(p.At)}
	}
	if verifiedID != nil {
		s := &costing.Seal{By: kernel.ActorRef{Kind: kernel.ActorKind(*verifiedKind), ID: *verifiedID}, Epoch: *verifyEpoch,
			At: kernel.Instant(*verifiedAt), Rfq: costing.Binding{RfqID: *rfqID, RowVersion: *rfqVersion, LinesHash: *rfqHash}}
		if err = json.Unmarshal(verification, &s.Evidence); err != nil {
			return nil, err
		}
		var lines []rfqLine
		if err = json.Unmarshal(rfqLines, &lines); err != nil {
			return nil, err
		}
		for _, l := range lines {
			s.RfqLines = append(s.RfqLines, costing.RfqLine(l))
		}
		r.Seal = s
	}
	if supersededAt != nil {
		r.SupersededAt = kernel.Instant(*supersededAt)
	}
	if endedAt != nil {
		e := &costing.End{By: kernel.ActorRef{Kind: kernel.ActorKind(*endedKind), ID: *endedID}, Epoch: *endEpoch,
			At: kernel.Instant(*endedAt), Reason: *endReason}
		if endEvidence != nil {
			if err = json.Unmarshal(endEvidence, &e.Evidence); err != nil {
				return nil, err
			}
		}
		r.End = e
	}
	return &r, nil
}

// SaveContext writes what a command changed: the root, inserted when created and otherwise
// updated if it is still at root version from, then its fresh and changed revisions. Revisions
// that leave a status are written before those that enter it, so the one-working and one-active
// indexes hold at every statement.
func SaveContext(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, cc *costing.Context, created bool, from int64, at time.Time) error {
	t, c := string(scope.Tenant), string(scope.Company)
	var err error
	if created {
		_, err = exec.Exec(ctx, `INSERT INTO brant_cst.costing_context (tenant_id, company_id, context_id, root_version, last_revision,
			owner_actor_id, owner_team_id, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, nullif($7, ''), $8, $8)`,
			t, c, cc.ID, cc.RootVersion, cc.LastRevision, cc.OwnerActor, cc.OwnerTeam, at)
	} else {
		err = casRoot(ctx, exec, `UPDATE brant_cst.costing_context SET root_version = $4, last_revision = $5, updated_at = $6
			WHERE tenant_id = $1 AND company_id = $2 AND context_id = $3 AND root_version = $7`,
			t, c, cc.ID, cc.RootVersion, cc.LastRevision, at, from)
	}
	if err != nil {
		return err
	}
	// Leaving revisions first: superseded, withdrawn, discarded; then the one that became active or
	// working.
	for _, entering := range []bool{false, true} {
		for _, r := range cc.Revisions {
			if !r.Fresh && !r.Changed {
				continue
			}
			if (r.Status == costing.Active || r.Status == costing.Working) != entering {
				continue
			}
			if err = writeContextRevision(ctx, exec, t, c, cc.ID, r); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeContextRevision(ctx context.Context, exec bentopg.Executor, t, c, id string, r *costing.Revision) error {
	data, err := json.Marshal(r.Data)
	if err != nil {
		return err
	}
	proposals := map[string]proposal{}
	for aid, p := range r.Proposals {
		proposals[aid] = proposal{ByKind: string(p.By.Kind), ByID: p.By.ID, At: p.At}
	}
	provenance, err := json.Marshal(proposals)
	if err != nil {
		return err
	}
	var base any
	if r.Base > 0 {
		base = r.Base
	}
	var verifiedKind, verifiedID, rfqID, rfqHash, endedKind, endedID, endReason any
	var verifyEpoch, rfqVersion, endEpoch, verifiedAt, supersededAt, endedAt any
	var verification, rfqLines, endEvidence []byte
	if s := r.Seal; s != nil {
		verifiedKind, verifiedID, verifyEpoch, verifiedAt = string(s.By.Kind), s.By.ID, s.Epoch, s.At
		rfqID, rfqVersion, rfqHash = s.Rfq.RfqID, s.Rfq.RowVersion, s.Rfq.LinesHash
		if verification, err = json.Marshal(nonNil(s.Evidence)); err != nil {
			return err
		}
		lines := []rfqLine{}
		for _, l := range s.RfqLines {
			lines = append(lines, rfqLine(l))
		}
		if rfqLines, err = json.Marshal(lines); err != nil {
			return err
		}
	}
	if !r.SupersededAt.IsZero() {
		supersededAt = r.SupersededAt
	}
	if e := r.End; e != nil {
		endedKind, endedID, endEpoch, endedAt, endReason = string(e.By.Kind), e.By.ID, e.Epoch, e.At, e.Reason
		if r.Status == costing.Withdrawn {
			if endEvidence, err = json.Marshal(nonNil(e.Evidence)); err != nil {
				return err
			}
		}
	}
	args := []any{t, c, id, r.Number, string(r.Status), base, data, r.Hash, r.Contributors, provenance, string(r.SavedBy.Kind),
		r.SavedBy.ID, r.SavedEpoch, r.SavedAt, verifiedKind, verifiedID, verifyEpoch, verifiedAt, verification, rfqID, rfqVersion,
		rfqHash, rfqLines, supersededAt, endedKind, endedID, endEpoch, endedAt, endReason, endEvidence}
	if r.Fresh {
		_, err = exec.Exec(ctx, `INSERT INTO brant_cst.costing_context_revision (tenant_id, company_id, context_id, revision, status,
			base_revision, data, content_hash, contributors, provenance, saved_by_kind, saved_by_id, saved_epoch, saved_at,
			verified_by_kind, verified_by_id, verify_epoch, verified_at, verification_evidence, rfq_id, rfq_row_version, rfq_lines_hash,
			rfq_lines, superseded_at, ended_by_kind, ended_by_id, end_epoch, ended_at, end_reason, end_evidence)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24,
			$25, $26, $27, $28, $29, $30)`, args...)
		return err
	}
	_, err = exec.Exec(ctx, `UPDATE brant_cst.costing_context_revision SET status = $5, base_revision = $6, data = $7,
		content_hash = $8, contributors = $9, provenance = $10, saved_by_kind = $11, saved_by_id = $12, saved_epoch = $13,
		saved_at = $14, verified_by_kind = $15, verified_by_id = $16, verify_epoch = $17, verified_at = $18,
		verification_evidence = $19, rfq_id = $20, rfq_row_version = $21, rfq_lines_hash = $22, rfq_lines = $23,
		superseded_at = $24, ended_by_kind = $25, ended_by_id = $26, end_epoch = $27, ended_at = $28, end_reason = $29,
		end_evidence = $30
		WHERE tenant_id = $1 AND company_id = $2 AND context_id = $3 AND revision = $4`, args...)
	return err
}

// ContextRevisionKey names one revision of one costing context.
type ContextRevisionKey struct {
	ContextID string
	Revision  int64
}

// ContextRevisions returns the sealed revisions among keys with their roots' owners, keyed by key.
// With lock ForShare it locks the roots FOR SHARE in ID order first, so a freeze holds every context
// it uses against a concurrent verification or withdrawal.
func ContextRevisions(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, keys []ContextRevisionKey, lock Lock) (map[ContextRevisionKey]OwnedContextRevision, error) {
	out := map[ContextRevisionKey]OwnedContextRevision{}
	byID := map[string]bool{}
	var ids []string
	for _, k := range keys {
		if !byID[k.ContextID] {
			byID[k.ContextID] = true
			ids = append(ids, k.ContextID)
		}
	}
	slices.Sort(ids)
	for _, id := range ids {
		cc, err := LoadContext(ctx, exec, scope, id, lock)
		if err != nil {
			return nil, err
		}
		if cc == nil {
			continue
		}
		for _, k := range keys {
			if k.ContextID != id {
				continue
			}
			if r := cc.Revision(k.Revision); r != nil && r.Status.Sealed() {
				out[k] = OwnedContextRevision{Revision: r, OwnerActor: cc.OwnerActor, OwnerTeam: cc.OwnerTeam}
			}
		}
	}
	return out, nil
}

// OwnedContextRevision is a revision with its root's owner, for visibility.
type OwnedContextRevision struct {
	Revision   *costing.Revision
	OwnerActor string
	OwnerTeam  string
}

// LoadMeasurement returns the measurement snapshot id with all its revisions, or nil, locking the
// root as lock says.
func LoadMeasurement(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, lock Lock) (*measurement.Snapshot, error) {
	t, c := string(scope.Tenant), string(scope.Company)
	s := &measurement.Snapshot{ID: id}
	err := exec.QueryRow(ctx, `SELECT root_version, last_revision, owner_actor_id, coalesce(owner_team_id, '')
		FROM brant_cst.measurement WHERE tenant_id = $1 AND company_id = $2 AND measurement_id = $3`+string(lock), t, c, id).
		Scan(&s.RootVersion, &s.LastRevision, &s.OwnerActor, &s.OwnerTeam)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := exec.Query(ctx, `SELECT revision, status, data, content_hash, recorded_by_kind, recorded_by_id, record_epoch,
		recorded_at, coalesce(reason, ''), superseded_at, withdrawn_by_id, withdraw_epoch, withdrawn_at, withdraw_reason,
		withdraw_evidence FROM brant_cst.measurement_revision WHERE tenant_id = $1 AND company_id = $2 AND measurement_id = $3
		ORDER BY revision`, t, c, id)
	if err != nil {
		return nil, err
	}
	s.Revisions, err = pgx.CollectRows(rows, scanMeasurementRevision)
	return s, err
}

func scanMeasurementRevision(row pgx.CollectableRow) (*measurement.Revision, error) {
	var r measurement.Revision
	var status, kind string
	var data, evidence []byte
	var withdrawnBy, withdrawReason *string
	var withdrawEpoch *int64
	var supersededAt, withdrawnAt *time.Time
	err := row.Scan(&r.Number, &status, &data, &r.Hash, &kind, &r.RecordedBy.ID, &r.Epoch, &r.RecordedAt, &r.Reason, &supersededAt,
		&withdrawnBy, &withdrawEpoch, &withdrawnAt, &withdrawReason, &evidence)
	if err != nil {
		return nil, err
	}
	r.Status, r.RecordedBy.Kind, r.RecordedAt = measurement.Status(status), kernel.ActorKind(kind), kernel.Instant(r.RecordedAt)
	if err = json.Unmarshal(data, &r.Data); err != nil {
		return nil, err
	}
	if supersededAt != nil {
		r.SupersededAt = kernel.Instant(*supersededAt)
	}
	if withdrawnAt != nil {
		w := &measurement.Withdrawal{By: kernel.ActorRef{Kind: kernel.ActorHuman, ID: *withdrawnBy}, Epoch: *withdrawEpoch,
			At: kernel.Instant(*withdrawnAt), Reason: *withdrawReason}
		if err = json.Unmarshal(evidence, &w.Evidence); err != nil {
			return nil, err
		}
		r.Withdrawal = w
	}
	return &r, nil
}

// SaveMeasurement writes what a command changed, as SaveContext does.
func SaveMeasurement(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, s *measurement.Snapshot, created bool, from int64, at time.Time) error {
	t, c := string(scope.Tenant), string(scope.Company)
	var err error
	if created {
		_, err = exec.Exec(ctx, `INSERT INTO brant_cst.measurement (tenant_id, company_id, measurement_id, root_version, last_revision,
			owner_actor_id, owner_team_id, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, nullif($7, ''), $8, $8)`,
			t, c, s.ID, s.RootVersion, s.LastRevision, s.OwnerActor, s.OwnerTeam, at)
	} else {
		err = casRoot(ctx, exec, `UPDATE brant_cst.measurement SET root_version = $4, last_revision = $5, updated_at = $6
			WHERE tenant_id = $1 AND company_id = $2 AND measurement_id = $3 AND root_version = $7`,
			t, c, s.ID, s.RootVersion, s.LastRevision, at, from)
	}
	if err != nil {
		return err
	}
	for _, entering := range []bool{false, true} {
		for _, r := range s.Revisions {
			if (!r.Fresh && !r.Changed) || (r.Status == measurement.Active) != entering {
				continue
			}
			if err = writeMeasurementRevision(ctx, exec, t, c, s.ID, r); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeMeasurementRevision(ctx context.Context, exec bentopg.Executor, t, c, id string, r *measurement.Revision) error {
	if !r.Fresh {
		var withdrawnBy, withdrawEpoch, withdrawnAt, withdrawReason, supersededAt any
		var evidence []byte
		if !r.SupersededAt.IsZero() {
			supersededAt = r.SupersededAt
		}
		if w := r.Withdrawal; w != nil {
			var err error
			if evidence, err = json.Marshal(nonNil(w.Evidence)); err != nil {
				return err
			}
			withdrawnBy, withdrawEpoch, withdrawnAt, withdrawReason = w.By.ID, w.Epoch, w.At, w.Reason
		}
		_, err := exec.Exec(ctx, `UPDATE brant_cst.measurement_revision SET status = $5, superseded_at = $6, withdrawn_by_id = $7,
			withdraw_epoch = $8, withdrawn_at = $9, withdraw_reason = $10, withdraw_evidence = $11
			WHERE tenant_id = $1 AND company_id = $2 AND measurement_id = $3 AND revision = $4`,
			t, c, id, r.Number, string(r.Status), supersededAt, withdrawnBy, withdrawEpoch, withdrawnAt, withdrawReason, evidence)
		return err
	}
	data, err := json.Marshal(r.Data)
	if err != nil {
		return err
	}
	observed, err := kernel.ParseTimestamp(r.Data.ObservedAt)
	if err != nil {
		return err
	}
	var reason any
	if r.Reason != "" {
		reason = r.Reason
	}
	_, err = exec.Exec(ctx, `INSERT INTO brant_cst.measurement_revision (tenant_id, company_id, measurement_id, revision, status, data,
		content_hash, kind, observed_at, line_ids, recorded_by_kind, recorded_by_id, record_epoch, recorded_at, reason)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
		t, c, id, r.Number, string(r.Status), data, r.Hash, r.Data.Kind, observed, r.Data.LineIDs(), string(r.RecordedBy.Kind),
		r.RecordedBy.ID, r.Epoch, r.RecordedAt, reason)
	return err
}

// MeasurementRevisionKey names one revision of one measurement snapshot.
type MeasurementRevisionKey struct {
	MeasurementID string
	Revision      int64
}

// OwnedMeasurementRevision is a revision with its root's owner, for visibility.
type OwnedMeasurementRevision struct {
	Revision   *measurement.Revision
	OwnerActor string
	OwnerTeam  string
}

// MeasurementRevisions returns the revisions among keys with their roots' owners. With lock
// ForShare it locks the roots FOR SHARE in ID order first.
func MeasurementRevisions(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, keys []MeasurementRevisionKey, lock Lock) (map[MeasurementRevisionKey]OwnedMeasurementRevision, error) {
	out := map[MeasurementRevisionKey]OwnedMeasurementRevision{}
	byID := map[string]bool{}
	var ids []string
	for _, k := range keys {
		if !byID[k.MeasurementID] {
			byID[k.MeasurementID] = true
			ids = append(ids, k.MeasurementID)
		}
	}
	slices.Sort(ids)
	for _, id := range ids {
		s, err := LoadMeasurement(ctx, exec, scope, id, lock)
		if err != nil {
			return nil, err
		}
		if s == nil {
			continue
		}
		for _, k := range keys {
			if r := s.Revision(k.Revision); k.MeasurementID == id && r != nil {
				out[k] = OwnedMeasurementRevision{Revision: r, OwnerActor: s.OwnerActor, OwnerTeam: s.OwnerTeam}
			}
		}
	}
	return out, nil
}

// ActiveMeasurementsForLines returns the keys of the active measurement revisions whose packages
// hold any of lineIDs.
func ActiveMeasurementsForLines(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, lineIDs []string) ([]MeasurementRevisionKey, error) {
	rows, err := exec.Query(ctx, `SELECT measurement_id, revision FROM brant_cst.measurement_revision
		WHERE tenant_id = $1 AND company_id = $2 AND status = 'active' AND line_ids && $3::text[] ORDER BY measurement_id`,
		string(scope.Tenant), string(scope.Company), lineIDs)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (MeasurementRevisionKey, error) {
		var k MeasurementRevisionKey
		return k, row.Scan(&k.MeasurementID, &k.Revision)
	})
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func casRoot(ctx context.Context, exec bentopg.Executor, sql string, args ...any) error {
	tag, err := exec.Exec(ctx, sql, args...)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}
