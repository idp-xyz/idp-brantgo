package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/amendment"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/order"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/orderrev"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The active order's revisions, amendments, amendment commit evidence and end
// (migrations/trd/000006), written in the transaction of the quote root and the order: a revision,
// commit evidence and an end once; an amendment once, then along SM-TRD-ORDER-AMENDMENT (the trigger
// checks every step). Nothing is deleted.

// preparation is one preparation of an amendment as stored.
type preparation struct {
	At     string `json:"at"`
	ByID   string `json:"by_id"`
	ByKind string `json:"by_kind"`
	Epoch  int64  `json:"epoch"`
}

// loadActive reads o's revisions after the first (with the current one's document), amendments,
// amendment commit evidence, end, and its S/C and PI with their signed copies (orderdoc.go).
func loadActive(ctx context.Context, exec bentopg.Executor, t, c string, o *order.Order) error {
	rows, err := exec.Query(ctx, `SELECT order_revision, order_hash, previous_hash, amendment_id, amendment_hash, row_version, created_at,
		CASE WHEN order_revision = $4 THEN document ELSE '' END FROM brant_trd.sales_order_revision
		WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3 ORDER BY order_revision`, t, c, o.ID, o.OrderRevision)
	if err != nil {
		return err
	}
	var current string
	o.Revisions, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (order.RevisionRecord, error) {
		var r order.RevisionRecord
		var doc string
		if err := row.Scan(&r.Revision, &r.Hash, &r.PreviousHash, &r.AmendmentID, &r.AmendmentHash, &r.RowVersion, &r.At, &doc); err != nil {
			return r, err
		}
		r.At = kernel.Instant(r.At)
		if doc != "" {
			current = doc
		}
		return r, nil
	})
	if err != nil {
		return err
	}
	if o.OrderRevision > 1 {
		r, err := orderrev.Parse([]byte(current), o.OrderHash)
		if err != nil {
			return err
		}
		o.SetCurrent(r)
	}
	if o.Amendments, err = loadAmendments(ctx, exec, t, c, o); err != nil {
		return err
	}
	if o.AmendmentCommits, err = loadAmendmentCommits(ctx, exec, t, c, o.ID); err != nil {
		return err
	}
	if o.End, err = loadEnd(ctx, exec, t, c, o.ID); err != nil {
		return err
	}
	return loadDocuments(ctx, exec, t, c, o)
}

func loadAmendments(ctx context.Context, exec bentopg.Executor, t, c string, o *order.Order) ([]*amendment.Amendment, error) {
	rows, err := exec.Query(ctx, `SELECT amendment_id, content, amendment_hash, status, version, preparations, submitted_by_kind,
		submitted_by_id, submit_epoch, submitted_at, decided_by_kind, decided_by_id, decide_epoch, decided_at, decision_reason,
		decision_evidence, applied_by_kind, applied_by_id, apply_epoch, applied_at, applied_revision, withdrawn_by_kind, withdrawn_by_id,
		withdraw_epoch, withdrawn_at, withdraw_reason, withdraw_evidence
		FROM brant_trd.sales_order_amendment WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3 ORDER BY created_at, amendment_id`,
		t, c, o.ID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*amendment.Amendment, error) {
		a := &amendment.Amendment{OrderID: o.ID}
		var content, status, preps string
		var sKind, sID, dKind, dID, dReason, dProofs, pKind, pID, wKind, wID, wReason, wProofs *string
		var sEpoch, dEpoch, pEpoch, pRevision, wEpoch *int64
		var sAt, dAt, pAt, wAt *time.Time
		if err := row.Scan(&a.ID, &content, &a.Hash, &status, &a.Version, &preps, &sKind, &sID, &sEpoch, &sAt, &dKind, &dID, &dEpoch, &dAt,
			&dReason, &dProofs, &pKind, &pID, &pEpoch, &pAt, &pRevision, &wKind, &wID, &wEpoch, &wAt, &wReason, &wProofs); err != nil {
			return nil, err
		}
		a.Status = amendment.Status(status)
		if err := json.Unmarshal([]byte(content), &a.Content); err != nil {
			return nil, err
		}
		if doc, err := a.Document(); err != nil || quote.Hash(doc) != a.Hash {
			return nil, errors.New("trd: an amendment's content is not its canonical, hashed document")
		}
		var ps []preparation
		if err := json.Unmarshal([]byte(preps), &ps); err != nil {
			return nil, err
		}
		for _, p := range ps {
			at, err := kernel.ParseTimestamp(p.At)
			if err != nil {
				return nil, err
			}
			a.Prepared = append(a.Prepared, quote.Act{By: kernel.ActorRef{Kind: kernel.ActorKind(p.ByKind), ID: p.ByID}, Epoch: p.Epoch, At: at})
		}
		if sAt != nil {
			s := quote.Act{By: kernel.ActorRef{Kind: kernel.ActorKind(*sKind), ID: *sID}, Epoch: *sEpoch, At: kernel.Instant(*sAt)}
			a.Submitted = &s
		}
		if dAt != nil {
			d, err := act(*dKind, *dID, *dEpoch, *dAt, dReason, *dProofs)
			if err != nil {
				return nil, err
			}
			a.Decided = &d
		}
		if pAt != nil {
			to := orderrev.Link{Revision: *pRevision}
			for _, r := range o.Revisions {
				if r.Revision == *pRevision {
					to.Hash = r.Hash
				}
			}
			a.Applied = &amendment.Application{Act: quote.Act{By: kernel.ActorRef{Kind: kernel.ActorKind(*pKind), ID: *pID}, Epoch: *pEpoch,
				At: kernel.Instant(*pAt)}, From: a.Content.Base, To: to}
		}
		if wAt != nil {
			w, err := act(*wKind, *wID, *wEpoch, *wAt, wReason, *wProofs)
			if err != nil {
				return nil, err
			}
			a.Withdrawn = &w
		}
		return a, nil
	})
}

func loadAmendmentCommits(ctx context.Context, exec bentopg.Executor, t, c, id string) ([]*order.AmendmentCommit, error) {
	rows, err := exec.Query(ctx, `SELECT idempotency_key, order_revision, order_hash, from_order_hash, amendment_id, amendment_hash,
		authority_epoch, control_epoch, row_version, committed_by_kind, committed_by_id, committed_at, entries
		FROM brant_trd.sales_order_amendment_commit WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3 ORDER BY order_revision`,
		t, c, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*order.AmendmentCommit, error) {
		m := &order.AmendmentCommit{}
		var kind, entries string
		if err := row.Scan(&m.IdempotencyKey, &m.OrderRevision, &m.OrderHash, &m.FromHash, &m.AmendmentID, &m.AmendmentHash, &m.AuthorityEpoch,
			&m.ControlEpoch, &m.RowVersion, &kind, &m.By.ID, &m.At, &entries); err != nil {
			return nil, err
		}
		m.By.Kind, m.At = kernel.ActorKind(kind), kernel.Instant(m.At)
		return m, json.Unmarshal([]byte(entries), &m.Entries)
	})
}

func loadEnd(ctx context.Context, exec bentopg.Executor, t, c, id string) (*order.End, error) {
	e := &order.End{}
	var status, kind, lines, proofs string
	var reason *string
	var at time.Time
	err := exec.QueryRow(ctx, `SELECT command, status, order_revision, order_hash, lines, fulfilment_source, downstream_source, ended_by_kind,
		ended_by_id, end_epoch, ended_at, reason, evidence, row_version FROM brant_trd.sales_order_end
		WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3`, t, c, id).
		Scan(&e.Command, &status, &e.OrderRevision, &e.OrderHash, &lines, &e.Fulfilment, &e.Downstream, &kind, &e.Act.By.ID, &e.Act.Epoch,
			&at, &reason, &proofs, &e.RowVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	e.Status = orderrev.Status(status)
	if e.Act, err = act(kind, e.Act.By.ID, e.Act.Epoch, at, reason, proofs); err != nil {
		return nil, err
	}
	var ls []endLine
	if err = json.Unmarshal([]byte(lines), &ls); err != nil {
		return nil, err
	}
	for _, l := range ls {
		e.Lines = append(e.Lines, orderrev.LineEnd(l))
	}
	return e, nil
}

// endLine is one line of an order's end as stored.
type endLine struct {
	LineID    string `json:"line_id"`
	Unit      string `json:"unit"`
	Ordered   string `json:"ordered"`
	Shipped   string `json:"shipped"`
	Remainder string `json:"remainder"`
}

// saveActive writes what an active-order command did, in the order the triggers check: the revision
// an apply produced, the amendments (fresh, then changed: the applied one names that revision), the
// apply's commit evidence and the order's end; and the documents and signed copies of the document
// commands, which bind the order's revision as it stands.
func saveActive(ctx context.Context, exec bentopg.Executor, t, c string, o *order.Order, at time.Time) error {
	if r := o.NewRevision; r != nil {
		rec := o.Revisions[len(o.Revisions)-1]
		if _, err := exec.Exec(ctx, `INSERT INTO brant_trd.sales_order_revision (tenant_id, company_id, order_id, order_revision, document,
			order_hash, previous_revision, previous_hash, amendment_id, amendment_hash, row_version, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
			t, c, o.ID, r.Number, string(r.Document), r.Hash, r.Previous.Revision, r.Previous.Hash, r.Amendment.ID, r.Amendment.Hash,
			rec.RowVersion, rec.At); err != nil {
			return err
		}
		o.NewRevision = nil
	}
	for _, a := range o.Amendments {
		if err := writeAmendment(ctx, exec, t, c, a, at); err != nil {
			return err
		}
	}
	for _, m := range o.AmendmentCommits {
		if !m.Fresh {
			continue
		}
		entries, err := m.EntriesJSON()
		if err != nil {
			return err
		}
		if _, err = exec.Exec(ctx, `INSERT INTO brant_trd.sales_order_amendment_commit (tenant_id, company_id, order_id, order_revision,
			command, idempotency_key, amendment_id, amendment_hash, from_order_hash, order_hash, authority_epoch, control_epoch, row_version,
			committed_by_kind, committed_by_id, committed_at, entries, entries_sha256)
			VALUES ($1, $2, $3, $4, 'order.amendment.apply', $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`,
			t, c, o.ID, m.OrderRevision, m.IdempotencyKey, m.AmendmentID, m.AmendmentHash, m.FromHash, m.OrderHash, m.AuthorityEpoch,
			m.ControlEpoch, m.RowVersion, string(m.By.Kind), m.By.ID, m.At, string(entries), quote.Hash(entries)); err != nil {
			return err
		}
	}
	if e := o.End; e != nil && e.Fresh {
		ls := make([]endLine, len(e.Lines))
		for i, l := range e.Lines {
			ls[i] = endLine(l)
		}
		lines, err := json.Marshal(ls)
		if err != nil {
			return err
		}
		proofs, err := proofsOf(e.Act.Evidence)
		if err != nil {
			return err
		}
		if _, err = exec.Exec(ctx, `INSERT INTO brant_trd.sales_order_end (tenant_id, company_id, order_id, command, status, order_revision,
			order_hash, lines, fulfilment_source, downstream_source, ended_by_kind, ended_by_id, end_epoch, ended_at, reason, evidence, row_version)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`,
			t, c, o.ID, e.Command, string(e.Status), e.OrderRevision, e.OrderHash, string(lines), e.Fulfilment, e.Downstream,
			string(e.Act.By.Kind), e.Act.By.ID, e.Act.Epoch, e.Act.At, reasonOf(e.Act.Reason), proofs, e.RowVersion); err != nil {
			return err
		}
	}
	return saveDocuments(ctx, exec, t, c, o)
}

// writeAmendment inserts a fresh amendment or writes a changed one at its previous version.
func writeAmendment(ctx context.Context, exec bentopg.Executor, t, c string, a *amendment.Amendment, at time.Time) error {
	if !a.Fresh && !a.Changed {
		return nil
	}
	content, err := a.Document()
	if err != nil {
		return err
	}
	ps := make([]preparation, len(a.Prepared))
	for i, p := range a.Prepared {
		ps[i] = preparation{At: kernel.FormatTimestamp(p.At), ByID: p.By.ID, ByKind: string(p.By.Kind), Epoch: p.Epoch}
	}
	preps, err := json.Marshal(ps)
	if err != nil {
		return err
	}
	base := a.Content.Base
	if a.Fresh {
		_, err = exec.Exec(ctx, `INSERT INTO brant_trd.sales_order_amendment (tenant_id, company_id, order_id, amendment_id, base_revision,
			base_hash, content, amendment_hash, status, version, preparations, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $12)`,
			t, c, a.OrderID, a.ID, base.Revision, base.Hash, string(content), a.Hash, string(a.Status), a.Version, string(preps), at)
		if err == nil {
			a.Fresh, a.Changed = false, false
		}
		return err
	}
	cols := []any{t, c, a.OrderID, a.ID, a.Version - 1, base.Revision, base.Hash, string(content), a.Hash, string(a.Status), a.Version,
		string(preps), at}
	add := func(x *quote.Act, reason bool) error {
		if x == nil {
			cols = append(cols, nil, nil, nil, nil)
			if reason {
				cols = append(cols, nil, nil)
			}
			return nil
		}
		cols = append(cols, string(x.By.Kind), x.By.ID, x.Epoch, x.At)
		if reason {
			proofs, err := proofsOf(x.Evidence)
			if err != nil {
				return err
			}
			cols = append(cols, reasonOf(x.Reason), proofs)
		}
		return nil
	}
	if err = add(a.Submitted, false); err != nil {
		return err
	}
	if err = add(a.Decided, true); err != nil {
		return err
	}
	var decision any
	switch a.Status {
	case amendment.Approved, amendment.Applied:
		decision = "approve"
	case amendment.Rejected:
		decision = "reject"
	}
	cols = append(cols, decision)
	var applied *quote.Act
	var appliedRevision any
	if a.Applied != nil {
		applied, appliedRevision = &a.Applied.Act, a.Applied.To.Revision
	}
	if err = add(applied, false); err != nil {
		return err
	}
	cols = append(cols, appliedRevision)
	if err = add(a.Withdrawn, true); err != nil {
		return err
	}
	tag, err := exec.Exec(ctx, `UPDATE brant_trd.sales_order_amendment SET base_revision = $6, base_hash = $7, content = $8,
		amendment_hash = $9, status = $10, version = $11, preparations = $12, updated_at = $13,
		submitted_by_kind = $14, submitted_by_id = $15, submit_epoch = $16, submitted_at = $17,
		decided_by_kind = $18, decided_by_id = $19, decide_epoch = $20, decided_at = $21, decision_reason = $22, decision_evidence = $23,
		decision = $24, applied_by_kind = $25, applied_by_id = $26, apply_epoch = $27, applied_at = $28, applied_revision = $29,
		withdrawn_by_kind = $30, withdrawn_by_id = $31, withdraw_epoch = $32, withdrawn_at = $33, withdraw_reason = $34, withdraw_evidence = $35
		WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3 AND amendment_id = $4 AND version = $5`, cols...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	a.Changed = false
	return nil
}
