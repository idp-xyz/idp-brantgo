package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"go.idp.xyz/idp-bento-go/eventing"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Dead letters live in brant_msg.dead_letter, which foundation/messaging owns (migrations/msg).
// The table is FORCE RLS on app.tenant_id and app.company_id, so every statement runs in a
// ScopeTransactor transaction for the letter's scope.
const (
	insertDeadLetter = `INSERT INTO brant_msg.dead_letter
		(tenant_id, company_id, source, event_id, event_type, envelope, attempt, failures, failure_code,
		 last_error, critical, dead_at)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (tenant_id, company_id, source, event_id) DO NOTHING`
	readDeadLetter = `SELECT state, envelope FROM brant_msg.dead_letter
		WHERE tenant_id = $1 AND company_id = $2 AND source = $3 AND event_id = $4`
	markReplayed = `UPDATE brant_msg.dead_letter SET state = 'replayed', replayed_at = $5,
		replayed_by_kind = $6, replayed_by_id = $7, replay_epoch = $8,
		replay_attempts = replay_attempts + 1, last_replay_at = $5
		WHERE tenant_id = $1 AND company_id = $2 AND source = $3 AND event_id = $4 AND state = 'dead'
		RETURNING replay_attempts`
	recordReplayFailure = `UPDATE brant_msg.dead_letter SET replay_attempts = replay_attempts + 1,
		last_replay_at = $5, last_replay_error = $6
		WHERE tenant_id = $1 AND company_id = $2 AND source = $3 AND event_id = $4 AND state = 'dead'`
)

// stateDead is a dead letter not yet replayed; a replay moves it to 'replayed'.
const stateDead = "dead"

// maxErrorBytes bounds the error texts a dead letter keeps.
const maxErrorBytes = 1024

// DeadLetter is a delivery the relay abandoned, as it is stored.
type DeadLetter struct {
	Scope     kernel.Scope
	Source    string
	EventID   eventing.EventID
	Envelope  eventing.Envelope
	Attempt   uint32
	Failures  uint32 // after the failure that abandoned it, if one did
	Code      eventing.FailureCode
	LastError string
	Critical  bool
	DeadAt    time.Time
}

// DeadLetterWriter stores dead letters. Bury runs settle, which abandons the delivery in the
// Outbox, and stores letter in the same transaction, so neither happens without the other. An
// error from settle, such as eventing.ErrLeaseLost, comes back unchanged and nothing is stored.
type DeadLetterWriter interface {
	Bury(ctx context.Context, letter DeadLetter, settle func(context.Context) error) error
}

// DeadLetterStore is the PostgreSQL DeadLetterWriter. DB must be the Bento DB of the Outbox store
// that settle calls: Bento's operations then join Bury's transaction. With another DB they fail
// with application.ErrTransactionConflict instead.
//
// A letter that is already stored keeps its first record; Bento abandons a delivery only once.
type DeadLetterStore struct {
	DB *bentopg.DB
}

var _ DeadLetterWriter = DeadLetterStore{}

func (s DeadLetterStore) Bury(ctx context.Context, letter DeadLetter, settle func(context.Context) error) error {
	if s.DB == nil || settle == nil {
		return errors.New("messaging: dead letter store misconfigured")
	}
	envelope, err := encodeEnvelope(letter.Envelope)
	if err != nil {
		return err
	}
	var lastError *string
	if letter.LastError != "" {
		text := cutText(letter.LastError, maxErrorBytes)
		lastError = &text
	}
	return bento.ScopeTransactor{DB: s.DB, Scope: letter.Scope}.WithinTransaction(ctx, func(txCtx context.Context) error {
		if err := settle(txCtx); err != nil {
			return err
		}
		exec, err := s.DB.RequireExecutor(txCtx)
		if err != nil {
			return err
		}
		_, err = exec.Exec(txCtx, insertDeadLetter, string(letter.Scope.Tenant), string(letter.Scope.Company),
			letter.Source, string(letter.EventID), cutText(string(letter.Envelope.Type), eventing.MaxEventTypeLength), string(envelope),
			int64(letter.Attempt), int64(letter.Failures), string(letter.Code), lastError, letter.Critical,
			kernel.Instant(letter.DeadAt))
		return err
	})
}

// envelopeWire mirrors the field names of Bento's envelope encoding, for envelopes Bento refuses
// to encode.
type envelopeWire struct {
	SpecVersion   string             `json:"specversion"`
	ID            eventing.EventID   `json:"id"`
	Source        string             `json:"source"`
	Type          eventing.EventType `json:"type"`
	Version       uint32             `json:"version"`
	Scope         string             `json:"scope"`
	Subject       string             `json:"subject"`
	PartitionKey  string             `json:"partition_key"`
	OccurredAt    string             `json:"occurred_at"`
	RecordedAt    string             `json:"recorded_at"`
	CorrelationID string             `json:"correlation_id,omitempty"`
	CausationID   string             `json:"causation_id,omitempty"`
	TraceParent   string             `json:"traceparent,omitempty"`
	TraceState    string             `json:"tracestate,omitempty"`
	ContentType   string             `json:"content_type"`
	Payload       any                `json:"payload"`
}

// encodeEnvelope returns Bento's JSON encoding of e. Bento encodes valid envelopes only; an
// invalid one is kept under the same field names, with a payload that is not JSON as a string, so
// the dead letter still holds everything the outbox delivered.
func encodeEnvelope(e eventing.Envelope) ([]byte, error) {
	if b, err := json.Marshal(e); err == nil {
		return b, nil
	}
	var payload any = e.Payload
	if !json.Valid(e.Payload) {
		payload = cleanText(string(e.Payload))
	}
	return json.Marshal(envelopeWire{
		SpecVersion: e.SpecVersion, ID: e.ID, Source: e.Source, Type: e.Type, Version: uint32(e.Version),
		Scope: e.Scope, Subject: e.Subject, PartitionKey: e.PartitionKey,
		OccurredAt: e.OccurredAt.UTC().Format(time.RFC3339Nano), RecordedAt: e.RecordedAt.UTC().Format(time.RFC3339Nano),
		CorrelationID: e.CorrelationID, CausationID: e.CausationID, TraceParent: e.TraceParent, TraceState: e.TraceState,
		ContentType: e.ContentType, Payload: payload,
	})
}

// cleanText makes s valid UTF-8 without NUL, which PostgreSQL text and jsonb refuse.
func cleanText(s string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(s, "\uFFFD"), "\x00", "")
}

// cutText cleans s and cuts it to at most max bytes on a character boundary.
func cutText(s string, max int) string {
	s = cleanText(s)
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
