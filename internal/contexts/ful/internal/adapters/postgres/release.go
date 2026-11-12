package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/release"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipment"
	"go.idp.xyz/idp-brant/internal/kernel"
)

const releaseColumns = `release_id, shipment_id, scope, status, row_version, order_id, order_revision, order_hash, opened_by_kind, opened_by_id,
	opened_at, consumed_command, consumed_by_kind, consumed_by_id, consume_epoch, consumed_at, consume_evidence_ids, consume_entries,
	consume_entries_sha256, coalesce(document_release_kind, ''), consumed_document_set_id, consumed_bl_version, consumed_bl_number,
	consumed_bl_sha256`

func entries(doc string) ([]shipment.Entry, error) {
	var out []shipment.Entry
	if err := json.Unmarshal([]byte(doc), &out); err != nil {
		return nil, fmt.Errorf("ful: stored guard entries: %w", err)
	}
	if out == nil {
		out = []shipment.Entry{}
	}
	return out, nil
}

func scanRelease(row pgx.Row) (*release.Release, error) {
	var r release.Release
	var opened string
	var cmd, cKind, cID, doc, sum *string
	var epoch *int64
	var at *time.Time
	var evidence []string
	var kind string
	var blSet, blNumber, blSHA *string
	var blVersion *int64
	if err := row.Scan(&r.ID, &r.ShipmentID, &r.Scope, &r.Status, &r.RowVersion, &r.Order.OrderID, &r.Order.OrderRevision, &r.Order.OrderHash,
		&opened, &r.Opened.By.ID, &r.Opened.At, &cmd, &cKind, &cID, &epoch, &at, &evidence, &doc, &sum, &kind, &blSet, &blVersion, &blNumber,
		&blSHA); err != nil {
		return nil, err
	}
	r.Opened.By.Kind, r.Opened.At = kernel.ActorKind(opened), kernel.Instant(r.Opened.At)
	if at != nil && cmd != nil && cKind != nil && cID != nil && epoch != nil && doc != nil && sum != nil {
		e, err := entries(*doc)
		if err != nil {
			return nil, err
		}
		r.Consumption = &release.Consumption{Command: *cmd, ReleaseKind: kind, Evidence: strs(evidence), Entries: e, EntriesJSON: *doc,
			EntriesSHA256: *sum, Consumed: shipment.Act{By: kernel.ActorRef{Kind: kernel.ActorKind(*cKind), ID: *cID}, Epoch: *epoch,
				At: kernel.Instant(*at)}}
		if blSet != nil && blVersion != nil && blNumber != nil && blSHA != nil {
			r.Consumption.BillOfLading = &release.BillOfLadingRef{DocumentSetID: *blSet, Version: *blVersion, Number: *blNumber, ContentSHA256: *blSHA}
		}
	}
	return &r, nil
}

func loadRelease(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, where string, lock Lock, args ...any) (*release.Release, error) {
	t, c := scope2(scope)
	r, err := scanRelease(exec.QueryRow(ctx, `SELECT `+releaseColumns+` FROM brant_ful.commercial_release WHERE tenant_id = $1 AND company_id = $2
		AND `+where+string(lock), append([]any{t, c}, args...)...))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return r, loadReleaseParts(ctx, exec, scope, r)
}

// LoadRelease returns the release of shipment shipmentID for releaseScope with its evaluations and
// exceptions, or nil; lock locks the release row.
func LoadRelease(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, shipmentID, releaseScope string, lock Lock) (*release.Release, error) {
	return loadRelease(ctx, exec, scope, `shipment_id = $3 AND scope = $4`, lock, shipmentID, releaseScope)
}

// LoadReleaseByID returns release id with its evaluations and exceptions, or nil.
func LoadReleaseByID(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) (*release.Release, error) {
	return loadRelease(ctx, exec, scope, `release_id = $3`, NoLock, id)
}

// LoadShipmentReleases returns the releases of shipment shipmentID, shipment scope first.
func LoadShipmentReleases(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, shipmentID string) ([]*release.Release, error) {
	var out []*release.Release
	for _, s := range []string{release.ScopeShipment, release.ScopeDocuments} {
		r, err := LoadRelease(ctx, exec, scope, shipmentID, s, NoLock)
		if err != nil {
			return nil, err
		}
		if r != nil {
			out = append(out, r)
		}
	}
	return out, nil
}

func loadReleaseParts(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *release.Release) error {
	t, c := scope2(scope)
	rows, err := exec.Query(ctx, `SELECT seq, release_version, outcome, coalesce(guard, ''), coalesce(code, ''), coalesce(reason, ''),
		coalesce(exception_seq, 0), guard_entries, guard_entries_sha256, bindings::text, evaluated_by_kind, evaluated_by_id, evaluate_epoch, evaluated_at
		FROM brant_ful.commercial_release_evaluation WHERE tenant_id = $1 AND company_id = $2 AND release_id = $3 ORDER BY seq`, t, c, r.ID)
	if err != nil {
		return err
	}
	r.Evaluations, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (release.Evaluation, error) {
		var e release.Evaluation
		var kind, bindings string
		if err := row.Scan(&e.Seq, &e.ReleaseVersion, &e.Outcome, &e.Guard, &e.Code, &e.Reason, &e.ExceptionSeq, &e.EntriesJSON, &e.EntriesSHA256,
			&bindings, &kind, &e.Evaluated.By.ID, &e.Evaluated.Epoch, &e.Evaluated.At); err != nil {
			return e, err
		}
		e.Evaluated.By.Kind, e.Evaluated.At = kernel.ActorKind(kind), kernel.Instant(e.Evaluated.At)
		var err error
		if e.Entries, err = entries(e.EntriesJSON); err != nil {
			return e, err
		}
		if err = json.Unmarshal([]byte(bindings), &e.Bindings); err != nil {
			return e, fmt.Errorf("ful: stored bindings of %s: %w", r.ID, err)
		}
		return e, nil
	})
	if err != nil {
		return err
	}
	rows, err = exec.Query(ctx, `SELECT seq, release_version, order_id, order_revision, coalesce(order_hash, ''), stage_id, currency,
		shortfall_minor::text, valid_until, reason, evidence_ids, guard_entries, guard_entries_sha256, granted_by_kind, granted_by_id, grant_epoch,
		granted_at
		FROM brant_ful.commercial_release_exception WHERE tenant_id = $1 AND company_id = $2 AND release_id = $3 ORDER BY seq`, t, c, r.ID)
	if err != nil {
		return err
	}
	r.Exceptions, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (release.Exception, error) {
		var x release.Exception
		var kind string
		if err := row.Scan(&x.Seq, &x.ReleaseVersion, &x.Order.OrderID, &x.Order.OrderRevision, &x.Order.OrderHash, &x.StageID, &x.Currency,
			&x.ShortfallMinor, &x.ValidUntil, &x.Reason, &x.Evidence, &x.EntriesJSON, &x.EntriesSHA256, &kind, &x.Granted.By.ID, &x.Granted.Epoch,
			&x.Granted.At); err != nil {
			return x, err
		}
		if x.Order.OrderHash == "" {
			// Exceptions before ful 000007 bind the release's order (its hash is not repeated).
			x.Order.OrderHash = r.Order.OrderHash
		}
		x.Granted.By.Kind, x.Granted.At, x.ValidUntil, x.Evidence = kernel.ActorKind(kind), kernel.Instant(x.Granted.At), kernel.Instant(x.ValidUntil),
			strs(x.Evidence)
		var err error
		x.Entries, err = entries(x.EntriesJSON)
		return x, err
	})
	return err
}

func insertEvaluation(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *release.Release) error {
	t, c := scope2(scope)
	e := r.Latest()
	bindings, err := json.Marshal(e.Bindings)
	if err != nil {
		return err
	}
	var exception *int64
	if e.ExceptionSeq > 0 {
		exception = &e.ExceptionSeq
	}
	_, err = exec.Exec(ctx, `INSERT INTO brant_ful.commercial_release_evaluation (tenant_id, company_id, release_id, seq, release_version, outcome,
		guard, code, reason, exception_seq, guard_entries, guard_entries_sha256, bindings, evaluated_by_kind, evaluated_by_id, evaluate_epoch,
		evaluated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13::jsonb, $14, $15, $16, $17)`,
		t, c, r.ID, e.Seq, e.ReleaseVersion, e.Outcome, nullable(e.Guard), nullable(e.Code), nullable(e.Reason), exception, e.EntriesJSON,
		e.EntriesSHA256, string(bindings), string(e.Evaluated.By.Kind), e.Evaluated.By.ID, e.Evaluated.Epoch, e.Evaluated.At)
	return err
}

// InsertRelease stores a new release with its first evaluation.
func InsertRelease(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *release.Release) error {
	t, c := scope2(scope)
	if _, err := exec.Exec(ctx, `INSERT INTO brant_ful.commercial_release (tenant_id, company_id, release_id, shipment_id, scope, status,
		row_version, order_id, order_revision, order_hash, evaluations, exceptions, opened_by_kind, opened_by_id, opened_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
		t, c, r.ID, r.ShipmentID, r.Scope, r.Status, r.RowVersion, r.Order.OrderID, r.Order.OrderRevision, r.Order.OrderHash, len(r.Evaluations),
		len(r.Exceptions), string(r.Opened.By.Kind), r.Opened.By.ID, r.Opened.At); err != nil {
		return err
	}
	return insertEvaluation(ctx, exec, scope, r)
}

func moveRelease(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *release.Release, from int64) error {
	t, c := scope2(scope)
	tag, err := exec.Exec(ctx, `UPDATE brant_ful.commercial_release SET status = $4, row_version = $5, evaluations = $6, exceptions = $7
		WHERE tenant_id = $1 AND company_id = $2 AND release_id = $3 AND row_version = $8`,
		t, c, r.ID, r.Status, r.RowVersion, len(r.Evaluations), len(r.Exceptions), from)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

// SaveEvaluation writes r's latest evaluation if the stored release is still at version from.
func SaveEvaluation(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *release.Release, from int64) error {
	if err := moveRelease(ctx, exec, scope, r, from); err != nil {
		return err
	}
	return insertEvaluation(ctx, exec, scope, r)
}

// SaveException writes r's latest exception if the stored release is still at version from.
func SaveException(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *release.Release, from int64) error {
	if err := moveRelease(ctx, exec, scope, r, from); err != nil {
		return err
	}
	t, c := scope2(scope)
	x := r.Exceptions[len(r.Exceptions)-1]
	_, err := exec.Exec(ctx, `INSERT INTO brant_ful.commercial_release_exception (tenant_id, company_id, release_id, seq, release_version,
		order_id, order_revision, order_hash, stage_id, currency, shortfall_minor, valid_until, reason, evidence_ids, guard_entries,
		guard_entries_sha256, granted_by_kind, granted_by_id, grant_epoch, granted_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::numeric, $12, $13, $14, $15, $16, $17, $18, $19, $20)`,
		t, c, r.ID, x.Seq, x.ReleaseVersion, x.Order.OrderID, x.Order.OrderRevision, x.Order.OrderHash, x.StageID, x.Currency, x.ShortfallMinor,
		x.ValidUntil, x.Reason, strs(x.Evidence), x.EntriesJSON, x.EntriesSHA256, string(x.Granted.By.Kind), x.Granted.By.ID, x.Granted.Epoch,
		x.Granted.At)
	return err
}

// ConsumeRelease writes r's consumption if the stored release is still at version from.
func ConsumeRelease(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *release.Release, from int64) error {
	t, c := scope2(scope)
	x := r.Consumption
	var blSet, blNumber, blSHA *string
	var blVersion *int64
	if b := x.BillOfLading; b != nil {
		blSet, blVersion, blNumber, blSHA = &b.DocumentSetID, &b.Version, &b.Number, &b.ContentSHA256
	}
	tag, err := exec.Exec(ctx, `UPDATE brant_ful.commercial_release SET status = $4, row_version = $5, consumed_command = $6, consumed_by_kind = $7,
		consumed_by_id = $8, consume_epoch = $9, consumed_at = $10, consume_evidence_ids = $11, consume_entries = $12, consume_entries_sha256 = $13,
		document_release_kind = $14, consumed_document_set_id = $16, consumed_bl_version = $17, consumed_bl_number = $18, consumed_bl_sha256 = $19
		WHERE tenant_id = $1 AND company_id = $2 AND release_id = $3 AND row_version = $15`,
		t, c, r.ID, r.Status, r.RowVersion, x.Command, string(x.Consumed.By.Kind), x.Consumed.By.ID, x.Consumed.Epoch, x.Consumed.At, strs(x.Evidence),
		x.EntriesJSON, x.EntriesSHA256, nullable(x.ReleaseKind), from, blSet, blVersion, blNumber, blSHA)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}
