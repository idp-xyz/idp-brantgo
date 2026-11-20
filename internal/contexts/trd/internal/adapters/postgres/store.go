// Package postgres stores TRD quote roots and their business revisions in schema brant_trd, on the
// executor of TRD's own transaction.
package postgres

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Schema is TRD's schema.
const Schema = "brant_trd"

// ErrConflict means a compare-and-set update found another root version or control epoch.
var ErrConflict = errors.New("trd: root changed")

// Lock modes of a load: none, or the root FOR UPDATE (a command on it).
type Lock string

const (
	NoLock    Lock = ""
	ForUpdate Lock = " FOR UPDATE"
)

const rootColumns = `root_version, last_revision, owner_actor_id, coalesce(owner_team_id, ''), labels, internal_note, control_state,
	control_epoch, coalesce(control_task_id, ''), coalesce(control_authority_epoch, 0), coalesce(control_by_kind, ''),
	coalesce(control_by_id, ''), control_changed_at, coalesce(control_initiator_id, ''), coalesce(control_initiator_epoch, 0),
	coalesce(control_agent_id, ''), coalesce(control_agent_epoch, 0)`

// LoadRoot returns the root id without its revisions, or nil, unlocked: what visibility and the
// object control fence of a replay need.
func LoadRoot(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) (*quote.Quote, error) {
	return loadRoot(ctx, exec, scope, id, NoLock)
}

func loadRoot(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, lock Lock) (*quote.Quote, error) {
	q := quote.New(id, "", "")
	var state, byKind string
	var changed *time.Time
	d := &q.Control.Delegation
	err := exec.QueryRow(ctx, `SELECT `+rootColumns+` FROM brant_trd.quote_root
		WHERE tenant_id = $1 AND company_id = $2 AND quote_id = $3`+string(lock), string(scope.Tenant), string(scope.Company), id).
		Scan(&q.RootVersion, &q.LastRevision, &q.OwnerActor, &q.OwnerTeam, &q.Labels, &q.InternalNote, &state, &q.Control.Epoch,
			&q.Control.TaskID, &q.Control.AuthorityEpoch, &byKind, &q.Control.By.ID, &changed, &d.Initiator, &d.InitiatorEpoch, &d.Agent,
			&d.AgentEpoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	q.Control.State, q.Control.By.Kind = quote.ControlState(state), kernel.ActorKind(byKind)
	if changed != nil {
		q.Control.At = kernel.Instant(*changed)
	}
	return q, nil
}

// LoadQuote returns the quote id with all its revisions, or nil, locking the root as lock says.
func LoadQuote(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, lock Lock) (*quote.Quote, error) {
	q, err := loadRoot(ctx, exec, scope, id, lock)
	if err != nil || q == nil {
		return nil, err
	}
	rows, err := exec.Query(ctx, `SELECT revision, status, coalesce(base_revision, 0), data, content_hash, contributors, saved_by_kind,
		saved_by_id, saved_epoch, saved_at, business_hash, commercial, issued_at, expires_at, frozen_by_kind, frozen_by_id, freeze_epoch,
		frozen_at, frozen_root_version, ended_by_kind, ended_by_id, end_epoch, ended_at, end_reason, end_evidence
		FROM brant_trd.quote_revision WHERE tenant_id = $1 AND company_id = $2 AND quote_id = $3 ORDER BY revision`,
		string(scope.Tenant), string(scope.Company), id)
	if err != nil {
		return nil, err
	}
	if q.Revisions, err = pgx.CollectRows(rows, scanRevision); err != nil {
		return nil, err
	}
	if q.Reviews, err = loadReviews(ctx, exec, scope, id); err != nil {
		return nil, err
	}
	if q.Approvals, err = loadApprovals(ctx, exec, scope, id); err != nil {
		return nil, err
	}
	if err = loadFormal(ctx, exec, scope, q); err != nil {
		return nil, err
	}
	if err = loadContracts(ctx, exec, scope, q); err != nil {
		return nil, err
	}
	return q, nil
}

func scanRevision(row pgx.CollectableRow) (*quote.Revision, error) {
	var r quote.Revision
	var status, savedKind, data string
	var hash, commercial, frozenKind, frozenID, endedKind, endedID, endReason, endEvidence *string
	var freezeEpoch, frozenVersion, endEpoch *int64
	var issued, expires, frozenAt, endedAt *time.Time
	err := row.Scan(&r.Number, &status, &r.Base, &data, &r.Hash, &r.Contributors, &savedKind, &r.SavedBy.ID, &r.SavedEpoch, &r.SavedAt,
		&hash, &commercial, &issued, &expires, &frozenKind, &frozenID, &freezeEpoch, &frozenAt, &frozenVersion, &endedKind, &endedID,
		&endEpoch, &endedAt, &endReason, &endEvidence)
	if err != nil {
		return nil, err
	}
	r.Status, r.SavedBy.Kind, r.SavedAt = quote.Status(status), kernel.ActorKind(savedKind), kernel.Instant(r.SavedAt)
	if err = json.Unmarshal([]byte(data), &r.Data); err != nil {
		return nil, err
	}
	if hash != nil {
		r.Seal = &quote.Seal{Commercial: []byte(*commercial), BusinessHash: *hash, IssuedAt: kernel.Instant(*issued),
			ExpiresAt: kernel.Instant(*expires), By: kernel.ActorRef{Kind: kernel.ActorKind(*frozenKind), ID: *frozenID}, Epoch: *freezeEpoch,
			At: kernel.Instant(*frozenAt), RootVersion: *frozenVersion}
	}
	if endedAt != nil {
		e := &quote.End{By: kernel.ActorRef{Kind: kernel.ActorKind(*endedKind), ID: *endedID}, Epoch: *endEpoch, At: kernel.Instant(*endedAt),
			Reason: *endReason}
		if err = json.Unmarshal([]byte(*endEvidence), &e.Evidence); err != nil {
			return nil, err
		}
		r.End = e
	}
	return &r, nil
}

// SaveQuote writes what a command changed: the root, inserted when created and otherwise updated
// if it is still at root version from and control epoch fromEpoch, then its fresh and changed
// revisions.
func SaveQuote(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, q *quote.Quote, created bool, from, fromEpoch int64, at time.Time) error {
	t, c := string(scope.Tenant), string(scope.Company)
	ctl := q.Control
	var task, byKind, byID any
	var authority, changed any
	var initiator, initiatorEpoch, agent, agentEpoch any // the delegation (S4 D-5): NULL when unbound or bound before TRD 000009
	if ctl.State != quote.Unbound {
		task, authority = ctl.TaskID, ctl.AuthorityEpoch
		if d := ctl.Delegation; d.Initiator != "" {
			initiator, initiatorEpoch, agent, agentEpoch = d.Initiator, d.InitiatorEpoch, d.Agent, d.AgentEpoch
		}
	}
	if ctl.Epoch > 0 {
		byKind, byID, changed = string(ctl.By.Kind), ctl.By.ID, ctl.At
	}
	labels := q.Labels
	if labels == nil {
		labels = []string{}
	}
	if created {
		_, err := exec.Exec(ctx, `INSERT INTO brant_trd.quote_root (tenant_id, company_id, quote_id, root_version, last_revision,
			owner_actor_id, owner_team_id, labels, internal_note, control_state, control_epoch, control_task_id, control_authority_epoch,
			control_by_kind, control_by_id, control_changed_at, created_at, updated_at, control_initiator_id, control_initiator_epoch,
			control_agent_id, control_agent_epoch)
			VALUES ($1, $2, $3, $4, $5, $6, nullif($7, ''), $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $17, $18, $19, $20, $21)`,
			t, c, q.ID, q.RootVersion, q.LastRevision, q.OwnerActor, q.OwnerTeam, labels, q.InternalNote, string(ctl.State), ctl.Epoch,
			task, authority, byKind, byID, changed, at, initiator, initiatorEpoch, agent, agentEpoch)
		if err != nil {
			return err
		}
	} else {
		tag, err := exec.Exec(ctx, `UPDATE brant_trd.quote_root SET root_version = $4, last_revision = $5, labels = $6, internal_note = $7,
			control_state = $8, control_epoch = $9, control_task_id = $10, control_authority_epoch = $11, control_by_kind = $12,
			control_by_id = $13, control_changed_at = $14, updated_at = $15, control_initiator_id = $18, control_initiator_epoch = $19,
			control_agent_id = $20, control_agent_epoch = $21
			WHERE tenant_id = $1 AND company_id = $2 AND quote_id = $3 AND root_version = $16 AND control_epoch = $17`,
			t, c, q.ID, q.RootVersion, q.LastRevision, labels, q.InternalNote, string(ctl.State), ctl.Epoch, task, authority, byKind, byID,
			changed, at, from, fromEpoch, initiator, initiatorEpoch, agent, agentEpoch)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrConflict
		}
	}
	// New revisions first; then what a status change needs to exist before it (the triggers check):
	// a formal document and its commit evidence before the revision becomes active, a closure before
	// it is withdrawn or cancelled; then the changed revisions, the revision becoming active last
	// (one active revision per root at every statement).
	for _, r := range q.Revisions {
		if r.Fresh {
			if err := writeRevision(ctx, exec, t, c, q.ID, r); err != nil {
				return err
			}
		}
	}
	for _, r := range q.Revisions {
		if r.Formal != nil && r.Formal.Fresh {
			if err := writeFormal(ctx, exec, t, c, q.ID, r); err != nil {
				return err
			}
		}
		if r.Closed != nil && r.Closed.Fresh {
			if err := writeClosure(ctx, exec, t, c, q.ID, r); err != nil {
				return err
			}
		}
	}
	moved := slices.DeleteFunc(slices.Clone(q.Revisions), func(r *quote.Revision) bool { return r.Fresh || !r.Changed })
	slices.SortStableFunc(moved, func(a, b *quote.Revision) int {
		return cmp.Compare(boolInt(a.Status == quote.Active), boolInt(b.Status == quote.Active))
	})
	for _, r := range moved {
		if err := writeRevision(ctx, exec, t, c, q.ID, r); err != nil {
			return err
		}
	}
	// Reviews after the revisions they move, approvals after the reviews that approved them,
	// exceptions after their approvals, risk holds after the revocation that registers them: the
	// triggers check each against what it binds.
	for _, r := range q.Reviews {
		if err := writeReview(ctx, exec, t, c, q.ID, r); err != nil {
			return err
		}
	}
	for _, a := range q.Approvals {
		if err := writeApproval(ctx, exec, t, c, q.ID, a); err != nil {
			return err
		}
	}
	for _, h := range q.Holds {
		if h.Fresh {
			if err := writeHold(ctx, exec, t, c, q.ID, h); err != nil {
				return err
			}
		}
	}
	if s := q.Slot; s != nil && s.Fresh {
		return writeSlot(ctx, exec, t, c, q.ID, s)
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func writeRevision(ctx context.Context, exec bentopg.Executor, t, c, id string, r *quote.Revision) error {
	if r.StatusOnly && !r.Fresh {
		tag, err := exec.Exec(ctx, `UPDATE brant_trd.quote_revision SET status = $5
			WHERE tenant_id = $1 AND company_id = $2 AND quote_id = $3 AND revision = $4`, t, c, id, r.Number, string(r.Status))
		if err == nil && tag.RowsAffected() != 1 {
			err = ErrConflict
		}
		return err
	}
	tree, err := quote.TreeOf(r.Data)
	if err != nil {
		return err
	}
	data, err := quote.Canonical(tree)
	if err != nil {
		return err
	}
	var base any
	if r.Base > 0 {
		base = r.Base
	}
	var hash, commercial, issued, expires, frozenKind, frozenID, freezeEpoch, frozenAt, frozenVersion any
	if s := r.Seal; s != nil {
		hash, commercial, issued, expires = s.BusinessHash, string(s.Commercial), s.IssuedAt, s.ExpiresAt
		frozenKind, frozenID, freezeEpoch, frozenAt, frozenVersion = string(s.By.Kind), s.By.ID, s.Epoch, s.At, s.RootVersion
	}
	var endedKind, endedID, endEpoch, endedAt, endReason, endEvidence any
	if e := r.End; e != nil {
		proofs := e.Evidence
		if proofs == nil {
			proofs = []quote.Evidence{}
		}
		doc, err := json.Marshal(proofs)
		if err != nil {
			return err
		}
		endedKind, endedID, endEpoch, endedAt, endReason, endEvidence = string(e.By.Kind), e.By.ID, e.Epoch, e.At, e.Reason, string(doc)
	}
	args := []any{t, c, id, r.Number, string(r.Status), base, string(data), r.Hash, r.Contributors, string(r.SavedBy.Kind), r.SavedBy.ID,
		r.SavedEpoch, r.SavedAt, hash, commercial, issued, expires, frozenKind, frozenID, freezeEpoch, frozenAt, frozenVersion,
		endedKind, endedID, endEpoch, endedAt, endReason, endEvidence}
	if r.Fresh {
		_, err = exec.Exec(ctx, `INSERT INTO brant_trd.quote_revision (tenant_id, company_id, quote_id, revision, status, base_revision,
			data, content_hash, contributors, saved_by_kind, saved_by_id, saved_epoch, saved_at, business_hash, commercial, issued_at,
			expires_at, frozen_by_kind, frozen_by_id, freeze_epoch, frozen_at, frozen_root_version, ended_by_kind, ended_by_id, end_epoch,
			ended_at, end_reason, end_evidence)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26,
			$27, $28)`, args...)
		return err
	}
	_, err = exec.Exec(ctx, `UPDATE brant_trd.quote_revision SET status = $5, data = $7, content_hash = $8, contributors = $9,
		saved_by_kind = $10, saved_by_id = $11, saved_epoch = $12, saved_at = $13, business_hash = $14, commercial = $15, issued_at = $16,
		expires_at = $17, frozen_by_kind = $18, frozen_by_id = $19, freeze_epoch = $20, frozen_at = $21, frozen_root_version = $22,
		ended_by_kind = $23, ended_by_id = $24, end_epoch = $25, ended_at = $26, end_reason = $27, end_evidence = $28
		WHERE tenant_id = $1 AND company_id = $2 AND quote_id = $3 AND revision = $4 AND coalesce(base_revision, 0) = coalesce($6::bigint, 0)`,
		args...)
	return err
}
