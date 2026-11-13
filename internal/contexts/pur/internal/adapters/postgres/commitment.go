package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/commitment"
	"go.idp.xyz/idp-brant/internal/kernel"
)

const commitmentColumns = `commitment_id, status, row_version, draft_id, draft_hash, order_id, order_revision, order_hash, supplier_id,
	currency, supply_valid_until, owner_actor_id, coalesce(owner_team_id, ''), requested_by_kind, requested_by_id, request_epoch,
	requested_at, decided_by_kind, decided_by_id, decide_epoch, decided_at, decision, valid_until, decision_reason, acknowledgements,
	revoked_by_kind, revoked_by_id, revoke_epoch, revoked_at, revoke_reason, issued_by_kind, issued_by_id, issue_epoch, issued_at,
	po_number, sequence_id, sequence_revision, sent_at, issue_evidence, issue_file_id, issue_file_sha256, revision, cancelled_by_kind,
	cancelled_by_id, cancel_epoch, cancelled_at, cancel_reason, cancel_evidence`

type ackDoc struct {
	Kind        string `json:"kind"`
	OrderLineID string `json:"order_line_id"`
	Reason      string `json:"reason"`
}

type evidenceDoc struct {
	Locator  string `json:"locator"`
	SourceID string `json:"source_id"`
}

func acksText(as []commitment.Acknowledgement) string {
	out := make([]ackDoc, len(as))
	for i, a := range as {
		out[i] = ackDoc{Kind: a.Kind, OrderLineID: a.OrderLineID, Reason: a.Reason}
	}
	b, _ := json.Marshal(out)
	return string(b)
}

func evidenceText(es []commitment.Evidence) string {
	out := make([]evidenceDoc, len(es))
	for i, e := range es {
		out[i] = evidenceDoc{Locator: e.Locator, SourceID: e.SourceID}
	}
	b, _ := json.Marshal(out)
	return string(b)
}

func evidenceOf(s string) ([]commitment.Evidence, error) {
	var docs []evidenceDoc
	if err := json.Unmarshal([]byte(s), &docs); err != nil {
		return nil, fmt.Errorf("pur: stored evidence: %w", err)
	}
	out := make([]commitment.Evidence, len(docs))
	for i, d := range docs {
		out[i] = commitment.Evidence{SourceID: d.SourceID, Locator: d.Locator}
	}
	return out, nil
}

type row interface{ Scan(dest ...any) error }

func scanCommitment(r row) (*commitment.Commitment, error) {
	c := &commitment.Commitment{}
	var reqKind string
	var dKind, dID, decision, dReason, acks *string
	var dEpoch *int64
	var dAt, validUntil *time.Time
	var rKind, rID, rReason *string
	var rEpoch *int64
	var rAt *time.Time
	var iKind, iID, number, seqID, evidence, fileID, fileSHA *string
	var iEpoch, seqRev *int64
	var iAt, sentAt *time.Time
	var cKind, cID, cReason, cEvidence *string
	var cEpoch *int64
	var cAt *time.Time
	if err := r.Scan(&c.ID, &c.Status, &c.RowVersion, &c.DraftID, &c.DraftHash, &c.OrderID, &c.OrderRevision, &c.OrderHash, &c.SupplierID,
		&c.Currency, &c.SupplyValidUntil, &c.OwnerActor, &c.OwnerTeam, &reqKind, &c.Requested.By.ID, &c.Requested.Epoch, &c.Requested.At,
		&dKind, &dID, &dEpoch, &dAt, &decision, &validUntil, &dReason, &acks, &rKind, &rID, &rEpoch, &rAt, &rReason,
		&iKind, &iID, &iEpoch, &iAt, &number, &seqID, &seqRev, &sentAt, &evidence, &fileID, &fileSHA, &c.Revision, &cKind, &cID, &cEpoch, &cAt,
		&cReason, &cEvidence); err != nil {
		return nil, err
	}
	if cKind != nil && cID != nil && cEpoch != nil && cAt != nil && cReason != nil && cEvidence != nil {
		es, err := evidenceOf(*cEvidence)
		if err != nil {
			return nil, err
		}
		c.Cancellation = &commitment.Cancellation{Act: commitment.Act{By: kernel.ActorRef{Kind: kernel.ActorKind(*cKind), ID: *cID}, Epoch: *cEpoch,
			At: kernel.Instant(*cAt), Reason: *cReason}, Evidence: es}
	}
	c.Stored = c.RowVersion
	c.Requested.By.Kind = kernel.ActorKind(reqKind)
	c.Requested.At, c.SupplyValidUntil = kernel.Instant(c.Requested.At), kernel.Instant(c.SupplyValidUntil)
	if dID != nil && dKind != nil && dEpoch != nil && dAt != nil && decision != nil {
		d := &commitment.Decision{Act: commitment.Act{By: kernel.ActorRef{Kind: kernel.ActorKind(*dKind), ID: *dID}, Epoch: *dEpoch,
			At: kernel.Instant(*dAt)}, Approve: *decision == commitment.DecisionApprove}
		if dReason != nil {
			d.Reason = *dReason
		}
		if validUntil != nil {
			d.ValidUntil = kernel.Instant(*validUntil)
		}
		if acks != nil {
			var docs []ackDoc
			if err := json.Unmarshal([]byte(*acks), &docs); err != nil {
				return nil, fmt.Errorf("pur: stored acknowledgements of %s: %w", c.ID, err)
			}
			d.Acknowledgements = []commitment.Acknowledgement{}
			for _, a := range docs {
				d.Acknowledgements = append(d.Acknowledgements, commitment.Acknowledgement{OrderLineID: a.OrderLineID, Kind: a.Kind, Reason: a.Reason})
			}
		}
		c.Decision = d
	}
	if rID != nil && rKind != nil && rEpoch != nil && rAt != nil && rReason != nil {
		c.Revocation = &commitment.Act{By: kernel.ActorRef{Kind: kernel.ActorKind(*rKind), ID: *rID}, Epoch: *rEpoch, At: kernel.Instant(*rAt),
			Reason: *rReason}
	}
	if iID != nil && iKind != nil && iEpoch != nil && iAt != nil && number != nil && seqID != nil && seqRev != nil && sentAt != nil && evidence != nil {
		is := &commitment.Issue{Act: commitment.Act{By: kernel.ActorRef{Kind: kernel.ActorKind(*iKind), ID: *iID}, Epoch: *iEpoch,
			At: kernel.Instant(*iAt)}, Number: *number, Sequence: commitment.Sequence{ID: *seqID, Revision: *seqRev}, SentAt: kernel.Instant(*sentAt)}
		var err error
		if is.Evidence, err = evidenceOf(*evidence); err != nil {
			return nil, err
		}
		if fileID != nil && fileSHA != nil {
			is.FileID, is.FileSHA256 = *fileID, *fileSHA
		}
		c.Issue = is
	}
	return c, nil
}

// LoadCommitment returns commitment id with its supplier confirmations, or nil; lock locks the
// commitment row.
func LoadCommitment(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, lock Lock) (*commitment.Commitment, error) {
	c, err := scanCommitment(exec.QueryRow(ctx, `SELECT `+commitmentColumns+` FROM brant_pur.purchase_commitment WHERE tenant_id = $1
		AND company_id = $2 AND commitment_id = $3`+string(lock), string(scope.Tenant), string(scope.Company), id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = confirmations(ctx, exec, scope, c); err != nil {
		return nil, err
	}
	return c, revisions(ctx, exec, scope, c)
}

// revisions loads c's revisions after the first, in order, with their lines in line order.
func revisions(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, c *commitment.Commitment) error {
	if c.Revision <= 1 {
		return nil
	}
	rows, err := exec.Query(ctx, `SELECT revision, kind, coalesce(change_id, ''), order_revision, order_hash, coalesce(document, ''),
		coalesce(document_hash, ''), created_by_kind, created_by_id, created_epoch, created_at FROM brant_pur.purchase_commitment_revision
		WHERE tenant_id = $1 AND company_id = $2 AND commitment_id = $3 ORDER BY revision`, string(scope.Tenant), string(scope.Company), c.ID)
	if err != nil {
		return err
	}
	c.Revisions, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (commitment.Revision, error) {
		var x commitment.Revision
		var doc, kind string
		err := r.Scan(&x.Number, &x.Kind, &x.ChangeID, &x.OrderRevision, &x.OrderHash, &doc, &x.DocumentHash, &kind, &x.By.ID, &x.Epoch, &x.At)
		x.By.Kind, x.At = kernel.ActorKind(kind), kernel.Instant(x.At)
		if doc != "" {
			x.Document = []byte(doc)
		}
		return x, err
	})
	if err != nil {
		return err
	}
	lines, err := exec.Query(ctx, `SELECT l.revision, l.order_line_id, l.allocated_quantity::text, l.overbuy_quantity::text,
		coalesce(to_char(l.supplier_date, 'YYYY-MM-DD'), '') FROM brant_pur.purchase_commitment_revision_line l
		JOIN brant_pur.purchase_commitment c ON c.tenant_id = l.tenant_id AND c.company_id = l.company_id AND c.commitment_id = l.commitment_id
		JOIN brant_pur.purchase_draft_line dl ON dl.tenant_id = c.tenant_id AND dl.company_id = c.company_id AND dl.draft_id = c.draft_id
			AND dl.order_line_id = l.order_line_id
		WHERE l.tenant_id = $1 AND l.company_id = $2 AND l.commitment_id = $3 ORDER BY l.revision, dl.line_no`,
		string(scope.Tenant), string(scope.Company), c.ID)
	if err != nil {
		return err
	}
	defer lines.Close()
	for lines.Next() {
		var n int64
		var q commitment.LineQuantity
		var a, o string
		if err = lines.Scan(&n, &q.OrderLineID, &a, &o, &q.SupplierDate); err != nil {
			return err
		}
		if q.Allocated, err = quantity(a); err != nil {
			return err
		}
		if q.Overbuy, err = quantity(o); err != nil {
			return err
		}
		for i := range c.Revisions {
			if c.Revisions[i].Number == n {
				c.Revisions[i].Lines = append(c.Revisions[i].Lines, q)
			}
		}
	}
	return lines.Err()
}

func optDate(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// insertRevision stores revision r of commitment c with its lines.
func insertRevision(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, c *commitment.Commitment, r *commitment.Revision) error {
	var doc *string
	if r.Document != nil {
		s := string(r.Document)
		doc = &s
	}
	if _, err := exec.Exec(ctx, `INSERT INTO brant_pur.purchase_commitment_revision (tenant_id, company_id, commitment_id, revision, kind,
		change_id, order_revision, order_hash, document, document_hash, line_count, created_by_kind, created_by_id, created_epoch, created_at)
		VALUES ($1, $2, $3, $4, $5, nullif($6, ''), $7, $8, $9, nullif($10, ''), $11, $12, $13, $14, $15)`,
		string(scope.Tenant), string(scope.Company), c.ID, r.Number, r.Kind, r.ChangeID, r.OrderRevision, r.OrderHash, doc, r.DocumentHash,
		len(r.Lines), string(r.By.Kind), r.By.ID, r.Epoch, r.At); err != nil {
		return err
	}
	for _, l := range r.Lines {
		if _, err := exec.Exec(ctx, `INSERT INTO brant_pur.purchase_commitment_revision_line (tenant_id, company_id, commitment_id, revision,
			order_line_id, allocated_quantity, overbuy_quantity, purchase_quantity, supplier_date)
			VALUES ($1, $2, $3, $4, $5, $6::numeric, $7::numeric, $8::numeric, $9::date)`,
			string(scope.Tenant), string(scope.Company), c.ID, r.Number, l.OrderLineID, decimalText(l.Allocated), decimalText(l.Overbuy),
			decimalText(l.Purchased()), optDate(l.SupplierDate)); err != nil {
			return err
		}
	}
	return nil
}

func confirmations(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, c *commitment.Commitment) error {
	rows, err := exec.Query(ctx, `SELECT commitment_version, commitment_revision, result, received_at, coalesce(deviation, ''),
		coalesce(supplier_reference, ''), evidence, recorded_by_kind, recorded_by_id, record_epoch, recorded_at FROM brant_pur.supplier_confirmation
		WHERE tenant_id = $1 AND company_id = $2 AND commitment_id = $3 ORDER BY commitment_version`,
		string(scope.Tenant), string(scope.Company), c.ID)
	if err != nil {
		return err
	}
	c.Confirmations, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (commitment.Confirmation, error) {
		var f commitment.Confirmation
		var evidence, kind string
		if err := r.Scan(&f.Version, &f.Revision, &f.Result, &f.ReceivedAt, &f.Deviation, &f.SupplierReference, &evidence, &kind, &f.By.ID,
			&f.Epoch, &f.At); err != nil {
			return f, err
		}
		f.By.Kind = kernel.ActorKind(kind)
		f.ReceivedAt, f.At = kernel.Instant(f.ReceivedAt), kernel.Instant(f.At)
		var err error
		f.Evidence, err = evidenceOf(evidence)
		return f, err
	})
	return err
}

// InsertCommitment stores a requested commitment.
func InsertCommitment(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, c *commitment.Commitment) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_pur.purchase_commitment (tenant_id, company_id, commitment_id, status, row_version, draft_id,
		draft_hash, order_id, order_revision, order_hash, supplier_id, currency, supply_valid_until, owner_actor_id, owner_team_id,
		requested_by_kind, requested_by_id, request_epoch, requested_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, nullif($15, ''), $16, $17, $18, $19, $19)`,
		string(scope.Tenant), string(scope.Company), c.ID, c.Status, c.RowVersion, c.DraftID, c.DraftHash, c.OrderID, c.OrderRevision,
		c.OrderHash, c.SupplierID, c.Currency, c.SupplyValidUntil, c.OwnerActor, c.OwnerTeam, string(c.Requested.By.Kind), c.Requested.By.ID,
		c.Requested.Epoch, c.Requested.At)
	return err
}

func optText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// SaveCommitment writes c's state, decision, revocation, issue, revision in force and cancellation if
// the stored row is still at the version it was loaded at (ErrConflict otherwise), after inserting
// the confirmation recorded since or the revision made since.
func SaveCommitment(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, c *commitment.Commitment, at time.Time) error {
	if f := c.Fresh(); f != nil {
		if _, err := exec.Exec(ctx, `INSERT INTO brant_pur.supplier_confirmation (tenant_id, company_id, commitment_id, commitment_version,
			commitment_revision, result, received_at, deviation, supplier_reference, evidence, recorded_by_kind, recorded_by_id, record_epoch,
			recorded_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
			string(scope.Tenant), string(scope.Company), c.ID, f.Version, f.Revision, f.Result, f.ReceivedAt, optText(f.Deviation),
			optText(f.SupplierReference), evidenceText(f.Evidence), string(f.By.Kind), f.By.ID, f.Epoch, f.At); err != nil {
			return err
		}
	}
	if r := c.FreshRevision(); r != nil {
		if err := insertRevision(ctx, exec, scope, c, r); err != nil {
			return err
		}
	}
	var cKind, cID, cReason, cEvidence *string
	var cEpoch *int64
	var cAt *time.Time
	if x := c.Cancellation; x != nil {
		kind, e := string(x.By.Kind), evidenceText(x.Evidence)
		cKind, cID, cEpoch, cAt, cReason, cEvidence = &kind, &x.By.ID, &x.Epoch, &x.At, &x.Reason, &e
	}
	var dKind, dID, decision, dReason, acks *string
	var dEpoch *int64
	var dAt, validUntil *time.Time
	if d := c.Decision; d != nil {
		kind, decided := string(d.By.Kind), commitment.DecisionReject
		if d.Approve {
			decided = commitment.DecisionApprove
			a, until := acksText(d.Acknowledgements), d.ValidUntil
			acks, validUntil = &a, &until
		}
		dKind, dID, dEpoch, dAt, decision, dReason = &kind, &d.By.ID, &d.Epoch, &d.At, &decided, optText(d.Reason)
	}
	var rKind, rID, rReason *string
	var rEpoch *int64
	var rAt *time.Time
	if r := c.Revocation; r != nil {
		kind := string(r.By.Kind)
		rKind, rID, rEpoch, rAt, rReason = &kind, &r.By.ID, &r.Epoch, &r.At, &r.Reason
	}
	var iKind, iID, number, seqID, evidence, fileID, fileSHA *string
	var iEpoch, seqRev *int64
	var iAt, sentAt *time.Time
	if is := c.Issue; is != nil {
		kind, e := string(is.By.Kind), evidenceText(is.Evidence)
		iKind, iID, iEpoch, iAt, number, seqID, seqRev, sentAt, evidence = &kind, &is.By.ID, &is.Epoch, &is.At, &is.Number, &is.Sequence.ID,
			&is.Sequence.Revision, &is.SentAt, &e
		fileID, fileSHA = optText(is.FileID), optText(is.FileSHA256)
	}
	tag, err := exec.Exec(ctx, `UPDATE brant_pur.purchase_commitment SET status = $4, row_version = $5, decided_by_kind = $6, decided_by_id = $7,
		decide_epoch = $8, decided_at = $9, decision = $10, valid_until = $11, decision_reason = $12, acknowledgements = $13,
		revoked_by_kind = $14, revoked_by_id = $15, revoke_epoch = $16, revoked_at = $17, revoke_reason = $18, issued_by_kind = $19,
		issued_by_id = $20, issue_epoch = $21, issued_at = $22, po_number = $23, sequence_id = $24, sequence_revision = $25, sent_at = $26,
		issue_evidence = $27, issue_file_id = $28, issue_file_sha256 = $29, updated_at = $30, revision = $32, cancelled_by_kind = $33,
		cancelled_by_id = $34, cancel_epoch = $35, cancelled_at = $36, cancel_reason = $37, cancel_evidence = $38
		WHERE tenant_id = $1 AND company_id = $2 AND commitment_id = $3 AND row_version = $31`,
		string(scope.Tenant), string(scope.Company), c.ID, c.Status, c.RowVersion, dKind, dID, dEpoch, dAt, decision, validUntil, dReason, acks,
		rKind, rID, rEpoch, rAt, rReason, iKind, iID, iEpoch, iAt, number, seqID, seqRev, sentAt, evidence, fileID, fileSHA, at, c.Stored,
		c.Revision, cKind, cID, cEpoch, cAt, cReason, cEvidence)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// CommitmentOwner is a commitment of a draft or an order as the rules and the reads list it.
type CommitmentOwner struct {
	ID      string
	DraftID string
	Status  string
	Actor   string
	Team    string
}

// CommitmentsOfDraft returns the commitments of draft draftID, by commitment ID.
func CommitmentsOfDraft(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, draftID string) ([]CommitmentOwner, error) {
	return owners(ctx, exec, `SELECT commitment_id, draft_id, status, owner_actor_id, coalesce(owner_team_id, '') FROM brant_pur.purchase_commitment
		WHERE tenant_id = $1 AND company_id = $2 AND draft_id = $3 ORDER BY commitment_id`, string(scope.Tenant), string(scope.Company), draftID)
}

// CommitmentsOfOrder returns the commitments of order orderID, by commitment ID.
func CommitmentsOfOrder(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, orderID string) ([]CommitmentOwner, error) {
	return owners(ctx, exec, `SELECT commitment_id, draft_id, status, owner_actor_id, coalesce(owner_team_id, '') FROM brant_pur.purchase_commitment
		WHERE tenant_id = $1 AND company_id = $2 AND order_id = $3 ORDER BY commitment_id`, string(scope.Tenant), string(scope.Company), orderID)
}

func owners(ctx context.Context, exec bentopg.Executor, sql string, args ...any) ([]CommitmentOwner, error) {
	rows, err := exec.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (CommitmentOwner, error) {
		var o CommitmentOwner
		err := r.Scan(&o.ID, &o.DraftID, &o.Status, &o.Actor, &o.Team)
		return o, err
	})
}
