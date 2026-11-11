package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Reads of TRD's read side (WBS S3-09) beyond the aggregates: TRD's command ledger, the ids of a
// quote's customer responses, and the events TRD enqueued in the Outbox of IDP-Bento-Go (schema
// bentoSchema, shared by every context's transaction owner, read here by source and partition key
// only; TRD writes it only through the ledger).

// Action is a stored action result of TRD's command ledger.
type Action struct {
	ActionID string
	ObjectID string
}

// LoadAction returns the stored action result of command under idempotency key in scope, or nil.
func LoadAction(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, command, key string) (*Action, error) {
	var a Action
	err := exec.QueryRow(ctx, `SELECT action_id, object_id FROM brant_trd.action_result
		WHERE tenant_id = $1 AND company_id = $2 AND command = $3 AND idempotency_key = $4`,
		string(scope.Tenant), string(scope.Company), command, key).Scan(&a.ActionID, &a.ObjectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// LoadResponseIDs returns the ids of the customer responses to quote id, in recording order.
func LoadResponseIDs(ctx context.Context, exec bentopg.Executor, scope kernel.Scope, id string) ([]string, error) {
	rows, err := exec.Query(ctx, `SELECT response_id FROM brant_trd.customer_response
		WHERE tenant_id = $1 AND company_id = $2 AND quote_id = $3 ORDER BY recorded_at, response_id`,
		string(scope.Tenant), string(scope.Company), id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// OutboxEvent is an event in the Outbox without its payload: its position (enqueued_seq), the
// aggregate and business revision the payload names, the action that enqueued it (causation), the
// SHA-256 of the stored payload's text and the relay's state.
type OutboxEvent struct {
	Seq              int64
	EventID          string
	EventType        string
	AggregateType    string
	AggregateID      string
	AggregateVersion int64
	BusinessRevision int64
	ActionID         string
	OccurredAt       time.Time
	PayloadSHA256    string
	Status           string // PENDING, LEASED, PUBLISHED or ABANDONED
	Attempts         int64
	PublishedAt      *time.Time
	AbandonedAt      *time.Time
}

// LoadOutbox returns at most limit events source enqueued in the partitions keys after position
// after, in enqueue order. The Outbox's (partition_key, enqueued_seq) index answers it.
func LoadOutbox(ctx context.Context, exec bentopg.Executor, bentoSchema, source string, keys []string, after int64, limit int) ([]OutboxEvent, error) {
	table := pgx.Identifier{bentoSchema, "outbox"}.Sanitize()
	rows, err := exec.Query(ctx, `SELECT enqueued_seq, event_id, event_type, coalesce(payload->>'aggregate_type', ''), subject,
		coalesce((payload->>'aggregate_version')::bigint, 0),
		CASE WHEN jsonb_typeof(payload->'business_revision') = 'number' THEN (payload->>'business_revision')::bigint ELSE 0 END,
		coalesce(causation_id, ''), occurred_at, encode(sha256(convert_to(payload::text, 'UTF8')), 'hex'), status, attempt,
		published_at, abandoned_at
		FROM `+table+` WHERE source = $1 AND partition_key = ANY($2) AND enqueued_seq > $3 ORDER BY enqueued_seq LIMIT $4`,
		source, keys, after, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (OutboxEvent, error) {
		var e OutboxEvent
		err := row.Scan(&e.Seq, &e.EventID, &e.EventType, &e.AggregateType, &e.AggregateID, &e.AggregateVersion, &e.BusinessRevision,
			&e.ActionID, &e.OccurredAt, &e.PayloadSHA256, &e.Status, &e.Attempts, &e.PublishedAt, &e.AbandonedAt)
		e.OccurredAt = kernel.Instant(e.OccurredAt)
		return e, err
	})
}

// LoadActivationEvent returns the id of the first event of one of types source enqueued in
// partition key whose payload names business revision rev, "" when there is none.
func LoadActivationEvent(ctx context.Context, exec bentopg.Executor, bentoSchema, source, key string, types []string, rev int64) (string, error) {
	table := pgx.Identifier{bentoSchema, "outbox"}.Sanitize()
	var id string
	err := exec.QueryRow(ctx, `SELECT event_id FROM `+table+` WHERE source = $1 AND partition_key = $2 AND event_type = ANY($3)
		AND jsonb_typeof(payload->'business_revision') = 'number' AND (payload->>'business_revision')::bigint = $4
		ORDER BY enqueued_seq LIMIT 1`, source, key, types, rev).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}
