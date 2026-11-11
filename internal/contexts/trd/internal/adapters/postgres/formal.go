package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Formal documents, commit evidence, closures, risk holds and the conversion slot of a quote root
// (migrations/trd/000003), loaded with the root and written in its transaction: documents and
// commit evidence before the revision becomes active, closures before the revision is withdrawn or
// cancelled, holds after the approval revocation that registers them. A document's bytes are read
// only by LoadDocumentContent.

func loadFormal(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, q *quote.Quote) error {
	t, c := string(scope.Tenant), string(scope.Company)
	rows, err := exec.Query(ctx, `SELECT d.revision, d.business_hash, d.number, d.sequence_kind, d.sequence_id, d.sequence_revision, d.business_ref,
		d.template_kind, d.template_id, d.template_revision, d.template_content_hash, d.renderer_version, d.file_name, d.mime_type,
		d.content_sha256, d.size_bytes, e.command, e.idempotency_key, e.approval_id, e.approval_effectivity_version,
		coalesce(e.replaced_revision, 0), e.cost_snapshot_id, e.authority_epoch, e.control_epoch, e.root_version, e.committed_by_kind,
		e.committed_by_id, e.committed_at, e.entries
		FROM brant_trd.quote_document d JOIN brant_trd.quote_commit_evidence e ON e.tenant_id = d.tenant_id AND e.company_id = d.company_id
			AND e.quote_id = d.quote_id AND e.revision = d.revision
		WHERE d.tenant_id = $1 AND d.company_id = $2 AND d.quote_id = $3 ORDER BY d.revision`, t, c, q.ID)
	if err != nil {
		return err
	}
	formals, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (revisionPart[quote.Formalization], error) {
		out := revisionPart[quote.Formalization]{part: &quote.Formalization{}}
		d, m := &out.part.Document, &out.part.Commit
		var kind, entries string
		err := row.Scan(&out.revision, &d.BusinessHash, &d.Number, &d.Sequence.Kind, &d.Sequence.ID, &d.Sequence.Revision, &d.BusinessRef, &d.Template.Kind,
			&d.Template.ID, &d.Template.Revision, &d.TemplateHash, &d.RendererVersion, &d.FileName, &d.MimeType, &d.ContentSHA256,
			&d.SizeBytes, &m.Command, &m.IdempotencyKey, &m.ApprovalID, &m.EffectivityVersion, &m.Replaced, &m.CostSnapshotID,
			&m.AuthorityEpoch, &m.ControlEpoch, &m.RootVersion, &kind, &m.By.ID, &m.At, &entries)
		if err != nil {
			return out, err
		}
		m.By.Kind, m.At = kernel.ActorKind(kind), kernel.Instant(m.At)
		return out, json.Unmarshal([]byte(entries), &m.Entries)
	})
	if err != nil {
		return err
	}
	for _, f := range formals {
		if r := q.Revision(f.revision); r != nil {
			r.Formal = f.part
		}
	}
	rows, err = exec.Query(ctx, `SELECT revision, status, closed_by_kind, closed_by_id, close_epoch, closed_at, reason, evidence, root_version
		FROM brant_trd.quote_revision_closure WHERE tenant_id = $1 AND company_id = $2 AND quote_id = $3 ORDER BY revision`, t, c, q.ID)
	if err != nil {
		return err
	}
	closures, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (revisionPart[quote.Closure], error) {
		out := revisionPart[quote.Closure]{part: &quote.Closure{}}
		var epoch int64
		var status, kind, id, reason, proofs string
		var at time.Time
		if err := row.Scan(&out.revision, &status, &kind, &id, &epoch, &at, &reason, &proofs, &out.part.RootVersion); err != nil {
			return out, err
		}
		a, err := act(kind, id, epoch, at, &reason, proofs)
		out.part.Status, out.part.Closed = quote.Status(status), a
		return out, err
	})
	if err != nil {
		return err
	}
	for _, cl := range closures {
		if r := q.Revision(cl.revision); r != nil {
			r.Closed = cl.part
		}
	}
	rows, err = exec.Query(ctx, `SELECT hold_id, revision, cause, approval_id, code, registered_by_kind, registered_by_id, register_epoch,
		registered_at, root_version FROM brant_trd.quote_risk_hold WHERE tenant_id = $1 AND company_id = $2 AND quote_id = $3
		ORDER BY root_version, hold_id`, t, c, q.ID)
	if err != nil {
		return err
	}
	q.Holds, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (*quote.Hold, error) {
		h := &quote.Hold{}
		var kind string
		err := row.Scan(&h.ID, &h.Revision, &h.Cause, &h.ApprovalID, &h.Code, &kind, &h.Registered.By.ID, &h.Registered.Epoch,
			&h.Registered.At, &h.RootVersion)
		h.Registered.By.Kind, h.Registered.At = kernel.ActorKind(kind), kernel.Instant(h.Registered.At)
		return h, err
	})
	if err != nil {
		return err
	}
	s := &quote.Slot{}
	var kind string
	err = exec.QueryRow(ctx, `SELECT slot, revision, business_hash, order_id, taken_by_kind, taken_by_id, take_epoch, taken_at, root_version
		FROM brant_trd.quote_conversion_slot WHERE tenant_id = $1 AND company_id = $2 AND quote_id = $3`, t, c, q.ID).
		Scan(&s.Slot, &s.Revision, &s.BusinessHash, &s.OrderID, &kind, &s.Taken.By.ID, &s.Taken.Epoch, &s.Taken.At, &s.RootVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	s.Taken.By.Kind, s.Taken.At = kernel.ActorKind(kind), kernel.Instant(s.Taken.At)
	q.Slot = s
	return nil
}

// revisionPart is a row that belongs to one revision of the root.
type revisionPart[T any] struct {
	revision int64
	part     *T
}

// LoadDocumentContent returns the stored bytes of the formal document of revision rev of quote id,
// and false when there is none.
func LoadDocumentContent(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, rev int64) ([]byte, bool, error) {
	var content []byte
	err := exec.QueryRow(ctx, `SELECT content FROM brant_trd.quote_document WHERE tenant_id = $1 AND company_id = $2 AND quote_id = $3
		AND revision = $4`, string(scope.Tenant), string(scope.Company), id, rev).Scan(&content)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	return content, err == nil, err
}

// writeFormal stores a fresh formalization: the document with its bytes, then its commit evidence.
func writeFormal(ctx context.Context, exec bentopg.Executor, t, c, id string, r *quote.Revision) error {
	f := r.Formal
	d, m := f.Document, f.Commit
	_, err := exec.Exec(ctx, `INSERT INTO brant_trd.quote_document (tenant_id, company_id, quote_id, revision, business_hash, number,
		sequence_kind, sequence_id, sequence_revision, business_ref, template_kind, template_id, template_revision, template_content_hash,
		renderer_version, file_name, mime_type, content_sha256, size_bytes, content, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21)`,
		t, c, id, r.Number, d.BusinessHash, d.Number, d.Sequence.Kind, d.Sequence.ID, d.Sequence.Revision, d.BusinessRef, d.Template.Kind,
		d.Template.ID, d.Template.Revision, d.TemplateHash, d.RendererVersion, d.FileName, d.MimeType, d.ContentSHA256, d.SizeBytes, d.Content,
		m.At)
	if err != nil {
		return err
	}
	entries, err := m.EntriesJSON()
	if err != nil {
		return err
	}
	var replaced any
	if m.Replaced > 0 {
		replaced = m.Replaced
	}
	_, err = exec.Exec(ctx, `INSERT INTO brant_trd.quote_commit_evidence (tenant_id, company_id, quote_id, revision, command,
		idempotency_key, business_hash, approval_id, approval_effectivity_version, replaced_revision, number, content_sha256,
		cost_snapshot_id, authority_epoch, control_epoch, root_version, committed_by_kind, committed_by_id, committed_at, entries,
		entries_sha256) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21)`,
		t, c, id, r.Number, m.Command, m.IdempotencyKey, r.Seal.BusinessHash, m.ApprovalID, m.EffectivityVersion, replaced, d.Number,
		d.ContentSHA256, m.CostSnapshotID, m.AuthorityEpoch, m.ControlEpoch, m.RootVersion, string(m.By.Kind), m.By.ID, m.At,
		string(entries), quote.Hash(entries))
	return err
}

func writeClosure(ctx context.Context, exec bentopg.Executor, t, c, id string, r *quote.Revision) error {
	cl := r.Closed
	proofs, err := proofsOf(cl.Closed.Evidence)
	if err != nil {
		return err
	}
	_, err = exec.Exec(ctx, `INSERT INTO brant_trd.quote_revision_closure (tenant_id, company_id, quote_id, revision, status, closed_by_kind,
		closed_by_id, close_epoch, closed_at, reason, evidence, root_version) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		t, c, id, r.Number, string(cl.Status), string(cl.Closed.By.Kind), cl.Closed.By.ID, cl.Closed.Epoch, cl.Closed.At, cl.Closed.Reason,
		proofs, cl.RootVersion)
	return err
}

func writeHold(ctx context.Context, exec bentopg.Executor, t, c, id string, h *quote.Hold) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_trd.quote_risk_hold (tenant_id, company_id, quote_id, hold_id, revision, cause, approval_id,
		code, registered_by_kind, registered_by_id, register_epoch, registered_at, root_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		t, c, id, h.ID, h.Revision, h.Cause, h.ApprovalID, h.Code, string(h.Registered.By.Kind), h.Registered.By.ID, h.Registered.Epoch,
		h.Registered.At, h.RootVersion)
	return err
}

func writeSlot(ctx context.Context, exec bentopg.Executor, t, c, id string, s *quote.Slot) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_trd.quote_conversion_slot (tenant_id, company_id, quote_id, slot, revision, business_hash,
		order_id, taken_by_kind, taken_by_id, take_epoch, taken_at, root_version) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		t, c, id, s.Slot, s.Revision, s.BusinessHash, s.OrderID, string(s.Taken.By.Kind), s.Taken.By.ID, s.Taken.Epoch, s.Taken.At,
		s.RootVersion)
	return err
}
