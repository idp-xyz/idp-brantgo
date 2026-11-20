// Package postgres stores INQ RFQs in schema brant_inq, on the executor of INQ's own transaction.
package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/inq/internal/domain/rfq"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Schema is INQ's schema.
const Schema = "brant_inq"

// ErrConflict means a compare-and-set update found another row version.
var ErrConflict = errors.New("inq: row version changed")

// Load returns the RFQ id with its lines and sources, or nil; forUpdate locks the root row.
func Load(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string, forUpdate bool) (*rfq.Rfq, error) {
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE"
	}
	t, c := string(scope.Tenant), string(scope.Company)
	var r rfq.Rfq
	var status, confirmedKind, confirmedID string
	var customerVersion, contactVersion, epoch *int64
	var evidence []string
	err := exec.QueryRow(ctx, `SELECT rfq_id, status, row_version, coalesce(customer_party_id, ''), coalesce(contact_id, ''),
		unresolved_fields, owner_actor_id, coalesce(owner_team_id, ''), customer_row_version, contact_row_version, evidence_ids,
		coalesce(confirmed_by_kind, ''), coalesce(confirmed_by_id, ''), authority_epoch
		FROM brant_inq.customer_rfq WHERE tenant_id = $1 AND company_id = $2 AND rfq_id = $3`+lock, t, c, id).
		Scan(&r.ID, &status, &r.RowVersion, &r.CustomerID, &r.ContactID, &r.Unresolved, &r.OwnerActor, &r.OwnerTeam,
			&customerVersion, &contactVersion, &evidence, &confirmedKind, &confirmedID, &epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.Status = rfq.Status(status)
	if epoch != nil {
		r.Confirmation = &rfq.Confirmation{CustomerVersion: *customerVersion, ContactVersion: *contactVersion, Evidence: evidence,
			By: kernel.ActorRef{Kind: kernel.ActorKind(confirmedKind), ID: confirmedID}, Epoch: *epoch}
	}
	rows, err := exec.Query(ctx, `SELECT line_id, coalesce(sku_id, ''), coalesce(quantity, ''), coalesce(uom, ''),
		coalesce(requested_item, ''), coalesce(source_id, ''), coalesce(locator, ''), coalesce(sku_spec_revision, 0),
		locators, pending, coalesce(proposed_by_kind, ''), coalesce(proposed_by_id, '')
		FROM brant_inq.rfq_line WHERE tenant_id = $1 AND company_id = $2 AND rfq_id = $3 ORDER BY position`, t, c, id)
	if err != nil {
		return nil, err
	}
	r.Lines, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (rfq.Line, error) {
		var l rfq.Line
		var locators []locatorJSON
		var pending []pendingJSON
		var kind string
		err := row.Scan(&l.ID, &l.SkuID, &l.Quantity, &l.UOM, &l.RequestedItem, &l.SourceID, &l.Locator, &l.SkuSpecRevision,
			&locators, &pending, &kind, &l.ProposedBy.ID)
		l.ProposedBy.Kind = kernel.ActorKind(kind)
		for _, x := range locators {
			l.Locators = append(l.Locators, rfq.FieldLocator(x))
		}
		for _, x := range pending {
			l.Pending = append(l.Pending, rfq.PendingField(x))
		}
		return l, err
	})
	if err != nil {
		return nil, err
	}
	rows, err = exec.Query(ctx, `SELECT source_id FROM brant_inq.rfq_source WHERE tenant_id = $1 AND company_id = $2 AND rfq_id = $3
		ORDER BY source_id`, t, c, id)
	if err != nil {
		return nil, err
	}
	r.Sources, err = pgx.CollectRows(rows, pgx.RowTo[string])
	return &r, err
}

// Insert stores a new RFQ with its lines and sources.
func Insert(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *rfq.Rfq, at time.Time) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_inq.customer_rfq (tenant_id, company_id, rfq_id, status, row_version, customer_party_id,
		contact_id, unresolved_fields, owner_actor_id, owner_team_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, nullif($6, ''), nullif($7, ''), $8, $9, nullif($10, ''), $11, $11)`,
		string(scope.Tenant), string(scope.Company), r.ID, string(r.Status), r.RowVersion, r.CustomerID, r.ContactID,
		nonNil(r.Unresolved), r.OwnerActor, r.OwnerTeam, at)
	if err != nil {
		return err
	}
	return writeContent(ctx, exec, scope, r, false)
}

// Update writes the RFQ if the stored row is still at version from. With content, it first replaces
// the lines and sources, while the stored RFQ is still a draft (a trigger refuses line changes of
// any other RFQ).
func Update(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *rfq.Rfq, from int64, content bool, at time.Time) error {
	if content {
		if err := writeContent(ctx, exec, scope, r, true); err != nil {
			return err
		}
	}
	var customerVersion, contactVersion, epoch any
	var evidence []string
	var kind, id any
	if c := r.Confirmation; c != nil {
		customerVersion, contactVersion, epoch, evidence, kind, id = c.CustomerVersion, c.ContactVersion, c.Epoch, c.Evidence, string(c.By.Kind), c.By.ID
	}
	tag, err := exec.Exec(ctx, `UPDATE brant_inq.customer_rfq SET status = $4, row_version = $5, customer_party_id = nullif($6, ''),
		contact_id = nullif($7, ''), unresolved_fields = $8, customer_row_version = $9, contact_row_version = $10, evidence_ids = $11,
		confirmed_by_kind = $12, confirmed_by_id = $13, authority_epoch = $14,
		confirmed_at = CASE WHEN $14::bigint IS NULL THEN NULL ELSE coalesce(confirmed_at, $15) END, updated_at = $15
		WHERE tenant_id = $1 AND company_id = $2 AND rfq_id = $3 AND row_version = $16`,
		string(scope.Tenant), string(scope.Company), r.ID, string(r.Status), r.RowVersion, r.CustomerID, r.ContactID, nonNil(r.Unresolved),
		customerVersion, contactVersion, evidence, kind, id, epoch, at, from)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

func writeContent(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *rfq.Rfq, replace bool) error {
	t, c := string(scope.Tenant), string(scope.Company)
	if replace {
		if _, err := exec.Exec(ctx, `DELETE FROM brant_inq.rfq_line WHERE tenant_id = $1 AND company_id = $2 AND rfq_id = $3`, t, c, r.ID); err != nil {
			return err
		}
		if _, err := exec.Exec(ctx, `DELETE FROM brant_inq.rfq_source WHERE tenant_id = $1 AND company_id = $2 AND rfq_id = $3`, t, c, r.ID); err != nil {
			return err
		}
	}
	for i, l := range r.Lines {
		var revision any
		if l.SkuSpecRevision > 0 {
			revision = l.SkuSpecRevision
		}
		locators, pending := []locatorJSON{}, []pendingJSON{}
		for _, x := range l.Locators {
			locators = append(locators, locatorJSON(x))
		}
		for _, x := range l.Pending {
			pending = append(pending, pendingJSON(x))
		}
		_, err := exec.Exec(ctx, `INSERT INTO brant_inq.rfq_line (tenant_id, company_id, rfq_id, line_id, position, sku_id, quantity, uom,
			requested_item, source_id, locator, sku_spec_revision, locators, pending, proposed_by_kind, proposed_by_id)
			VALUES ($1, $2, $3, $4, $5, nullif($6, ''), nullif($7, ''), nullif($8, ''), nullif($9, ''), nullif($10, ''), nullif($11, ''), $12,
			$13, $14, nullif($15, ''), nullif($16, ''))`,
			t, c, r.ID, l.ID, i, l.SkuID, l.Quantity, l.UOM, l.RequestedItem, l.SourceID, l.Locator, revision, locators, pending,
			string(l.ProposedBy.Kind), l.ProposedBy.ID)
		if err != nil {
			return err
		}
	}
	for _, s := range r.Sources {
		if _, err := exec.Exec(ctx, `INSERT INTO brant_inq.rfq_source (tenant_id, company_id, rfq_id, source_id) VALUES ($1, $2, $3, $4)`,
			t, c, r.ID, s); err != nil {
			return err
		}
	}
	return nil
}

// locatorJSON and pendingJSON are the stored forms of a line's field locators and pending fields.
type locatorJSON struct {
	Field   string `json:"field"`
	Locator string `json:"locator"`
	Quote   string `json:"quote"`
}

type pendingJSON struct {
	Field      string   `json:"field"`
	Reason     string   `json:"reason"`
	Candidates []string `json:"candidates,omitempty"`
}

// Correction is a stored correction.
type Correction struct {
	rfq.Correction
	Seq        int64
	RecordedAt time.Time
}

// InsertCorrections appends corrections to the RFQ's, numbered after the last one. The caller
// holds the RFQ root FOR UPDATE.
func InsertCorrections(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, rfqID string, cs []rfq.Correction, at time.Time) error {
	if len(cs) == 0 {
		return nil
	}
	t, c := string(scope.Tenant), string(scope.Company)
	var last int64
	if err := exec.QueryRow(ctx, `SELECT coalesce(max(seq), 0) FROM brant_inq.rfq_correction WHERE tenant_id = $1 AND company_id = $2
		AND rfq_id = $3`, t, c, rfqID).Scan(&last); err != nil {
		return err
	}
	for i, x := range cs {
		_, err := exec.Exec(ctx, `INSERT INTO brant_inq.rfq_correction (tenant_id, company_id, rfq_id, seq, row_version, command, line_id,
			field, kind, from_value, to_value, locator, reason, candidates, proposed_by_kind, proposed_by_id, corrected_by_kind,
			corrected_by_id, recorded_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, nullif($15, ''), nullif($16, ''), $17, $18, $19)`,
			t, c, rfqID, last+int64(i)+1, x.RowVersion, x.Command, x.LineID, x.Field, x.Kind, x.From, x.To, x.Locator, x.Reason,
			nonNil(x.Candidates), string(x.ProposedBy.Kind), x.ProposedBy.ID, string(x.CorrectedBy.Kind), x.CorrectedBy.ID, at)
		if err != nil {
			return err
		}
	}
	return nil
}

// Corrections returns the RFQ's corrections in order.
func Corrections(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, rfqID string) ([]Correction, error) {
	rows, err := exec.Query(ctx, `SELECT seq, row_version, command, line_id, field, kind, from_value, to_value, locator, reason, candidates,
		coalesce(proposed_by_kind, ''), coalesce(proposed_by_id, ''), corrected_by_kind, corrected_by_id, recorded_at
		FROM brant_inq.rfq_correction WHERE tenant_id = $1 AND company_id = $2 AND rfq_id = $3 ORDER BY seq`,
		string(scope.Tenant), string(scope.Company), rfqID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Correction, error) {
		var x Correction
		var proposed, corrected string
		err := row.Scan(&x.Seq, &x.RowVersion, &x.Command, &x.LineID, &x.Field, &x.Kind, &x.From, &x.To, &x.Locator, &x.Reason,
			&x.Candidates, &proposed, &x.ProposedBy.ID, &corrected, &x.CorrectedBy.ID, &x.RecordedAt)
		x.ProposedBy.Kind, x.CorrectedBy.Kind = kernel.ActorKind(proposed), kernel.ActorKind(corrected)
		return x, err
	})
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
