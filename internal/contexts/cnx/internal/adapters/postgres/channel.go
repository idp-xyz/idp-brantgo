package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Channel attempts and the ingested receipt chain (migrations/cnx/000010_cnx_interop_receipts.sql).

// ChannelAttempt is one connector filing. The row stays when it reaches succeeded, failed or
// not_received; the next filing is another row.
type ChannelAttempt struct {
	ID          string
	Channel     string
	SubjectID   string
	AttemptNo   int
	OperationID string
	State       string
	Outcome     string
	Reason      string
	RowVersion  int64
	Actor       kernel.ActorRef
	Epoch       int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ChannelReceipt is one ingested external receipt. Authoritative is false when the receipt was
// kept without advancing the declaration, and RefusalCode says why.
type ChannelReceipt struct {
	ID             string
	Channel        string
	SubjectID      string
	OperationID    string
	IngressNo      int
	Sequence       int64
	Outcome        string
	StatusCode     string
	OriginalText   string
	MappingVersion string
	Authoritative  bool
	RefusalCode    string
	RecordedAt     time.Time
	Actor          kernel.ActorRef
	Epoch          int64
}

func scanAttempt(row pgx.Row) (*ChannelAttempt, error) {
	var a ChannelAttempt
	var kind string
	err := row.Scan(&a.ID, &a.Channel, &a.SubjectID, &a.AttemptNo, &a.OperationID, &a.State, &a.Outcome, &a.Reason,
		&a.RowVersion, &kind, &a.Actor.ID, &a.Epoch, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a.Actor.Kind = kernel.ActorKind(kind)
	return &a, nil
}

const attemptColumns = `attempt_id, channel, subject_id, attempt_no, operation_id, state, coalesce(outcome, ''), reason,
	row_version, actor_kind, actor_id, authority_epoch, created_at, updated_at`

// LoadChannelAttempt returns the latest attempt of subject on channel, or nil.
func LoadChannelAttempt(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, channel, subject string, lock Lock) (*ChannelAttempt, error) {
	return scanAttempt(exec.QueryRow(ctx, `SELECT `+attemptColumns+` FROM brant_cnx.channel_attempt
		WHERE tenant_id = $1 AND company_id = $2 AND channel = $3 AND subject_id = $4
		ORDER BY attempt_no DESC LIMIT 1`+string(lock), scoped(scope, channel, subject)...))
}

// InsertChannelAttempt opens an attempt in state planned.
func InsertChannelAttempt(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, a *ChannelAttempt) error {
	_, err := exec.Exec(ctx, `INSERT INTO brant_cnx.channel_attempt (tenant_id, company_id, attempt_id, channel, subject_id,
		attempt_no, operation_id, state, outcome, reason, row_version, actor_kind, actor_id, authority_epoch, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, nullif($9, ''), $10, $11, $12, $13, $14, $15, $16)`,
		scoped(scope, a.ID, a.Channel, a.SubjectID, a.AttemptNo, a.OperationID, a.State, a.Outcome, a.Reason, a.RowVersion,
			string(a.Actor.Kind), a.Actor.ID, a.Epoch, kernel.Instant(a.CreatedAt), kernel.Instant(a.UpdatedAt))...)
	return err
}

// SaveChannelAttempt moves attempt a from row version from.
func SaveChannelAttempt(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, a *ChannelAttempt, from int64, at time.Time) error {
	tag, err := exec.Exec(ctx, `UPDATE brant_cnx.channel_attempt SET state = $4, outcome = nullif($5, ''), reason = $6,
		row_version = $7, updated_at = $8 WHERE tenant_id = $1 AND company_id = $2 AND attempt_id = $3 AND row_version = $9`,
		scoped(scope, a.ID, a.State, a.Outcome, a.Reason, a.RowVersion, kernel.Instant(at), from)...)
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrConflict
	}
	return err
}

func scanChainReceipt(row pgx.Row) (*ChannelReceipt, error) {
	var r ChannelReceipt
	var kind string
	var sequence *int64
	err := row.Scan(&r.ID, &r.Channel, &r.SubjectID, &r.OperationID, &r.IngressNo, &sequence, &r.Outcome, &r.StatusCode,
		&r.OriginalText, &r.MappingVersion, &r.Authoritative, &r.RefusalCode, &r.RecordedAt, &kind, &r.Actor.ID, &r.Epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if sequence != nil {
		r.Sequence = *sequence
	}
	r.Actor.Kind = kernel.ActorKind(kind)
	return &r, nil
}

const chainColumns = `receipt_id, channel, subject_id, operation_id, ingress_no, external_sequence, outcome, status_code,
	original_text, mapping_version, authoritative, refusal_code, recorded_at, actor_kind, actor_id, authority_epoch`

// LoadChannelReceipt returns the chained receipt id, or nil.
func LoadChannelReceipt(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) (*ChannelReceipt, error) {
	return scanChainReceipt(exec.QueryRow(ctx, `SELECT `+chainColumns+` FROM brant_cnx.channel_receipt
		WHERE tenant_id = $1 AND company_id = $2 AND receipt_id = $3`, scoped(scope, id)...))
}

// ChannelReceipts returns the chain of subject on channel in arrival order.
func ChannelReceipts(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, channel, subject string) ([]*ChannelReceipt, error) {
	rows, err := exec.Query(ctx, `SELECT `+chainColumns+` FROM brant_cnx.channel_receipt
		WHERE tenant_id = $1 AND company_id = $2 AND channel = $3 AND subject_id = $4 ORDER BY ingress_no`,
		scoped(scope, channel, subject)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ChannelReceipt
	for rows.Next() {
		r, err := scanChainReceipt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountChannelReceipts returns how many receipts are chained for subject.
func CountChannelReceipts(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, channel, subject string) (int, error) {
	var n int
	err := exec.QueryRow(ctx, `SELECT count(*) FROM brant_cnx.channel_receipt WHERE tenant_id = $1 AND company_id = $2
		AND channel = $3 AND subject_id = $4`, scoped(scope, channel, subject)...).Scan(&n)
	return n, err
}

// InsertChannelReceipt appends one receipt. The chain is insert-only.
func InsertChannelReceipt(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, r *ChannelReceipt) error {
	var sequence *int64
	if r.Sequence > 0 {
		sequence = &r.Sequence
	}
	_, err := exec.Exec(ctx, `INSERT INTO brant_cnx.channel_receipt (tenant_id, company_id, receipt_id, channel, subject_id,
		operation_id, ingress_no, external_sequence, outcome, status_code, original_text, mapping_version, authoritative,
		refusal_code, recorded_at, actor_kind, actor_id, authority_epoch)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, nullif($12, ''), $13, $14, $15, $16, $17, $18)`,
		scoped(scope, r.ID, r.Channel, r.SubjectID, r.OperationID, r.IngressNo, sequence, r.Outcome, r.StatusCode, r.OriginalText,
			r.MappingVersion, r.Authoritative, r.RefusalCode, kernel.Instant(r.RecordedAt), string(r.Actor.Kind), r.Actor.ID, r.Epoch)...)
	return err
}
