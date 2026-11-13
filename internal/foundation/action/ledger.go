// Package action runs the write commands and audited reads of a bounded context in the order the
// engineering contracts fix (Python Core.execute): contract and structure, current identity and
// permission, a stored result of the same idempotency key, then the context's own checks. One
// successful command commits its business change, its action result, its audit entry and its
// Outbox events in one transaction; a rejected command leaves only a rejection audit entry.
//
// Every context owns the tables the ledger writes, in its own schema: action_result and
// audit_event, created by the context's migrations with FORCE RLS (see ResultsDDL).
package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.idp.xyz/idp-bento-go/application"
	"go.idp.xyz/idp-bento-go/eventing"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Command is one write command as the ledger records it.
type Command struct {
	Name           string // registered command, such as "rfq.save_draft"
	IdempotencyKey string
	RequestHash    string // RequestHash of the business arguments
	ObjectID       string // the object the request names, for the audit trail
	Reason         string // the reason a cancelling or closing command records in the audit trail
}

// Event is an integration event a command publishes through the Outbox. Its payload carries
// identifiers and versions only (contract C4).
type Event struct {
	Type             string // registered type with major version, such as "idp-brant.inq.rfq_closed.v1"
	AggregateType    string
	AggregateID      string
	AggregateVersion int64
	Fields           map[string]any // event-specific identifiers and versions
}

// Outcome is what a handler changed.
type Outcome struct {
	ObjectID   string
	RowVersion int64
	Status     string
	Events     []Event
}

// Result is the closed success answer of a command. A replay returns the stored result with
// Replayed set.
type Result struct {
	ActionID   string
	ObjectID   string
	RowVersion int64
	Status     string
	Replayed   bool
}

// Handler performs one command inside the write transaction.
type Handler interface {
	// Lock locks the command's own aggregate root FOR UPDATE when it exists, before the ledger
	// locks the authority guard rows FOR SHARE (lock order of ADR-BR-011). It decides nothing and
	// must reset whatever an earlier attempt left in the handler.
	Lock(ctx context.Context, exec bentopg.Executor) error
	// Apply decides and writes after authorization. It returns a *kernel.Rejection to refuse.
	Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (Outcome, error)
}

// Preparer is a Handler that reads upstream contexts before the write transaction begins (Context
// Map relationships without a guard lock). Prepare runs outside any transaction, after the
// command is authorized and found not to be a replay. It only fetches; Apply decides, so the order
// of rejections stays the one the contracts fix.
type Preparer interface {
	Prepare(ctx context.Context) error
}

// Visibility reports whether the actor of d may see the object a stored result names. A replay of
// an object the actor no longer sees is NOT_FOUND.
type Visibility func(ctx context.Context, exec bentopg.Executor, d authorization.Decision, command, objectID string) (bool, error)

// Ledger runs the commands and reads of one bounded context.
type Ledger struct {
	DB         *bentopg.DB
	Schema     string // the context's schema, brant_<bc>
	Source     string // the context's event source, idp-brant.<bc>
	Authorizer authorization.Authorizer
	Outbox     eventing.OutboxWriter
	Clock      application.Clock
	// ReplayRead is the read command whose permission a replay needs besides the command's own
	// (Python authorizes "read" before returning a stored result).
	ReplayRead string
	Visible    Visibility
	// ReconcileAudit is the name reconciliations have in the audit trail; empty means
	// ReconcileQuery. A context whose own command is named action.reconcile (TSK's ActionExecution,
	// S4 unit B1) names its reconciliations otherwise, so its audit trail tells them apart.
	ReconcileAudit string
}

var (
	schemaName  = regexp.MustCompile(`^brant_[a-z][a-z0-9_]{1,40}$`)
	hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`) // the action_result CHECK
)

// ResultsDDL returns the action-result and audit tables of schema, with FORCE RLS on the scope.
// A context's first migration contains this DDL verbatim; the ledger's tests create it with this
// function.
func ResultsDDL(schema string) string {
	s := schema
	return `CREATE TABLE ` + s + `.action_result (
    tenant_id       text        NOT NULL,
    company_id      text        NOT NULL,
    command         text        NOT NULL CHECK (command ~ '^[a-z][a-z0-9_]*\.[a-z][a-z0-9_.]*$'),
    idempotency_key text        NOT NULL CHECK (idempotency_key ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    request_hash    text        NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    action_id       text        NOT NULL,
    object_id       text        NOT NULL,
    row_version     bigint      NOT NULL CHECK (row_version > 0),
    status          text        NOT NULL,
    actor_kind      text        NOT NULL CHECK (actor_kind IN ('human', 'ai', 'system')),
    actor_id        text        NOT NULL,
    authority_epoch bigint      NOT NULL CHECK (authority_epoch > 0),
    correlation_id  text        NOT NULL,
    recorded_at     timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, command, idempotency_key),
    UNIQUE (tenant_id, company_id, action_id)
);

CREATE TABLE ` + s + `.audit_event (
    seq             bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id       text        NOT NULL,
    company_id      text        NOT NULL,
    event_id        text        NOT NULL UNIQUE,
    actor_kind      text        NOT NULL CHECK (actor_kind IN ('human', 'ai', 'system')),
    actor_id        text        NOT NULL,
    command         text        NOT NULL,
    object_id       text,
    outcome         text        NOT NULL CHECK (outcome IN ('succeeded', 'replayed', 'rejected', 'read')),
    code            text,
    correlation_id  text        NOT NULL,
    recorded_at     timestamptz NOT NULL,
    reason          text,
    authority_epoch bigint,
    CHECK ((outcome = 'rejected') = (code IS NOT NULL))
);

ALTER TABLE ` + s + `.action_result ENABLE ROW LEVEL SECURITY;
ALTER TABLE ` + s + `.action_result FORCE ROW LEVEL SECURITY;
ALTER TABLE ` + s + `.audit_event ENABLE ROW LEVEL SECURITY;
ALTER TABLE ` + s + `.audit_event FORCE ROW LEVEL SECURITY;
CREATE POLICY action_result_scope ON ` + s + `.action_result
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
CREATE POLICY audit_event_scope ON ` + s + `.audit_event
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
`
}

func (l *Ledger) check() error {
	if l.DB == nil || !schemaName.MatchString(l.Schema) || l.Source == "" || l.Outbox == nil || l.Clock == nil ||
		l.ReplayRead == "" || l.Visible == nil {
		return errors.New("action: ledger misconfigured")
	}
	return nil
}

// Execute runs cmd through h. It returns the committed or replayed result, a *kernel.Rejection, or
// an infrastructure error. Every rejection is audited.
//
// When the database cannot tell whether the commit happened, the error is
// application.ErrCommitUncertain as Bento reports it: not a rejection, not audited, with a zero
// Result. The caller does not execute the command again blindly; it reconciles (Reconcile) or
// replays the same idempotency key, which returns the stored result if the commit happened.
func (l *Ledger) Execute(ctx context.Context, cmd Command, h Handler) (Result, error) {
	if err := l.check(); err != nil {
		return Result{}, err
	}
	p, err := identity.FromContext(ctx)
	if err != nil {
		return Result{}, err
	}
	ctx = ensureCorrelation(ctx)
	res, err := l.execute(ctx, p, cmd, h)
	if err == nil {
		return res, nil
	}
	if rej, ok := kernel.AsRejection(err); ok {
		if auditErr := l.auditRejected(ctx, p, cmd.Name, cmd.ObjectID, rej.Code); auditErr != nil {
			return Result{}, errors.Join(rej, auditErr)
		}
	}
	// A failed command has no result; with ErrCommitUncertain, the one prepared before the commit
	// may or may not exist, so it is not handed out either.
	return Result{}, err
}

func (l *Ledger) execute(ctx context.Context, p identity.Principal, cmd Command, h Handler) (Result, error) {
	if !kernel.ValidIdentifier(cmd.IdempotencyKey) || !hashPattern.MatchString(cmd.RequestHash) || cmd.Name == "" {
		return Result{}, kernel.Reject(kernel.CodeSchemaInvalid, "/idempotency_key", "")
	}
	if _, err := l.Authorizer.Policy.Admit(p, cmd.Name); err != nil {
		return Result{}, denied(err)
	}
	if prep, ok := h.(Preparer); ok {
		var res Result
		var replayed bool
		err := l.inScope(ctx, p, func(ctx context.Context, exec bentopg.Executor) error {
			if _, err := l.Authorizer.Authorize(ctx, exec, cmd.Name); err != nil {
				return denied(err)
			}
			var err error
			res, replayed, err = l.replay(ctx, exec, p, cmd)
			return err
		})
		if err != nil || replayed {
			return res, err
		}
		if err = prep.Prepare(ctx); err != nil {
			return Result{}, err
		}
	}
	res, err := l.write(ctx, p, cmd, h)
	if uniqueViolation(err) {
		// A concurrent command with the same key or the same new identity committed first; the
		// second attempt sees its result or its row.
		res, err = l.write(ctx, p, cmd, h)
		if uniqueViolation(err) {
			err = kernel.Reject(kernel.CodeVersionConflict, "/args/id", "")
		}
	}
	if rej, ok := kernel.AsRejection(err); ok && rej.Code == kernel.CodeVersionConflict {
		// The stored result is looked up before the root is locked, so a command with the same key
		// that commits in between shows here as a version conflict. Python checks both under one
		// lock: the stored result answers, or the payload conflicts.
		var stored Result
		var replayed bool
		if rerr := l.inScope(ctx, p, func(ctx context.Context, exec bentopg.Executor) error {
			var err error
			stored, replayed, err = l.replay(ctx, exec, p, cmd)
			return err
		}); rerr != nil || replayed {
			return stored, rerr
		}
		return Result{}, rej
	}
	return res, err
}

func (l *Ledger) write(ctx context.Context, p identity.Principal, cmd Command, h Handler) (Result, error) {
	var res Result
	err := l.inScope(ctx, p, func(ctx context.Context, exec bentopg.Executor) error {
		var replayed bool
		var err error
		if res, replayed, err = l.replay(ctx, exec, p, cmd); err != nil || replayed {
			return err
		}
		if err = h.Lock(ctx, exec); err != nil {
			return err
		}
		d, err := l.Authorizer.Authorize(ctx, exec, cmd.Name)
		if err != nil {
			return denied(err)
		}
		out, err := h.Apply(ctx, exec, d)
		if err != nil {
			return err
		}
		if out.ObjectID == "" || out.RowVersion < 1 || out.Status == "" {
			return fmt.Errorf("action: %s returned an incomplete outcome %+v", cmd.Name, out)
		}
		now := l.Clock.Now()
		res = Result{ActionID: NewID("act", now), ObjectID: out.ObjectID, RowVersion: out.RowVersion, Status: out.Status}
		_, err = exec.Exec(ctx, `INSERT INTO `+l.Schema+`.action_result (tenant_id, company_id, command, idempotency_key,
			request_hash, action_id, object_id, row_version, status, actor_kind, actor_id, authority_epoch, correlation_id,
			recorded_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
			string(p.Scope.Tenant), string(p.Scope.Company), cmd.Name, cmd.IdempotencyKey, cmd.RequestHash, res.ActionID,
			res.ObjectID, res.RowVersion, res.Status, string(p.Actor.Kind), p.Actor.ID, d.Epoch, Correlation(ctx), now)
		if err != nil {
			return err
		}
		if err = l.audit(ctx, exec, p, cmd.Name, res.ObjectID, "succeeded", "", cmd.Reason, d.Epoch); err != nil {
			return err
		}
		for _, e := range out.Events {
			if err = l.enqueue(ctx, p, d, res.ActionID, now, e); err != nil {
				return err
			}
		}
		return nil
	})
	return res, err
}

// replay returns the stored result of cmd's idempotency key, if there is one, after re-checking
// the current permission for the command, the payload, the read permission and the visibility of
// the object: a revoked or narrowed grant never receives a stored success.
func (l *Ledger) replay(ctx context.Context, exec bentopg.Executor, p identity.Principal, cmd Command) (Result, bool, error) {
	hash, res, found, err := l.stored(ctx, exec, p, cmd.Name, cmd.IdempotencyKey)
	if err != nil || !found {
		return Result{}, false, err
	}
	if _, err = l.Authorizer.Authorize(ctx, exec, cmd.Name); err != nil {
		return Result{}, false, denied(err)
	}
	if hash != cmd.RequestHash {
		return Result{}, false, kernel.Reject(kernel.CodeIdempotencyPayloadConflict, "/idempotency_key", "")
	}
	d, err := l.Authorizer.Authorize(ctx, exec, l.ReplayRead)
	if err != nil {
		return Result{}, false, denied(err)
	}
	visible, err := l.Visible(ctx, exec, d, cmd.Name, res.ObjectID)
	if err != nil {
		return Result{}, false, err
	}
	if !visible {
		return Result{}, false, kernel.Reject(kernel.CodeNotFound, "/args/id", "")
	}
	res.Replayed = true
	return res, true, l.audit(ctx, exec, p, cmd.Name, res.ObjectID, "replayed", "", "", d.Epoch)
}

// stored reads the action result of command and key in p's scope, with the request hash it was
// stored for.
func (l *Ledger) stored(ctx context.Context, exec bentopg.Executor, p identity.Principal, command, key string) (string, Result, bool, error) {
	var hash string
	var res Result
	err := exec.QueryRow(ctx, `SELECT request_hash, action_id, object_id, row_version, status FROM `+l.Schema+`.action_result
		WHERE tenant_id = $1 AND company_id = $2 AND command = $3 AND idempotency_key = $4`,
		string(p.Scope.Tenant), string(p.Scope.Company), command, key).
		Scan(&hash, &res.ActionID, &res.ObjectID, &res.RowVersion, &res.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", Result{}, false, nil
	}
	if err != nil {
		return "", Result{}, false, err
	}
	return hash, res, true, nil
}

// ReconcileQuery is the name a reconciliation has in the audit trail (Python audits
// quote.query.action_reconciliation), unless the ledger names it otherwise (ReconcileAudit).
const ReconcileQuery = "action.reconcile"

// Reconciliation is what became of a command whose caller lost the answer.
type Reconciliation struct {
	State    kernel.ReconcileState
	ActionID string  // committed and payload_conflict
	ObjectID string  // committed and payload_conflict
	Result   *Result // the stored result; committed only
}

// Reconcile tells a caller that lost the answer to a command, for instance after
// application.ErrCommitUncertain, what became of command under idempotencyKey: committed (with the
// stored result), payload_conflict (the key committed another request) or not_committed. It
// executes nothing (contract 06, 动作对账; Python t03_reconciliation.reconcile). After committed,
// a replay of the same key returns the same result; after not_committed, the caller sends the
// command again with the same key, and it executes once.
//
// Reconciliation is an audited read. It re-checks, in the caller's own scope and now, the
// business read permission a replay needs (ReplayRead) and the visibility of the object; a
// revoked grant is PERMISSION_DENIED and an object outside the data scope NOT_FOUND, whether
// the hash matches or not. It does not require the command's own permission: historical results
// need read grants, never a grant to repeat the write (Python READ_PERMISSIONS, test
// test_readers_can_recover_receipts_without_write_grants). The ledger of another tenant or company
// is never read, so a key committed there is not_committed here.
func (l *Ledger) Reconcile(ctx context.Context, command, idempotencyKey, requestHash string) (Reconciliation, error) {
	if err := l.check(); err != nil {
		return Reconciliation{}, err
	}
	p, err := identity.FromContext(ctx)
	if err != nil {
		return Reconciliation{}, err
	}
	ctx = ensureCorrelation(ctx)
	rec, err := l.reconcile(ctx, p, command, idempotencyKey, requestHash)
	if err == nil {
		return rec, nil
	}
	if rej, ok := kernel.AsRejection(err); ok {
		if auditErr := l.auditRejected(ctx, p, l.reconcileAudit(), "", rej.Code); auditErr != nil {
			return Reconciliation{}, errors.Join(rej, auditErr)
		}
	}
	return Reconciliation{}, err
}

func (l *Ledger) reconcileAudit() string {
	if l.ReconcileAudit != "" {
		return l.ReconcileAudit
	}
	return ReconcileQuery
}

func (l *Ledger) reconcile(ctx context.Context, p identity.Principal, command, key, hash string) (Reconciliation, error) {
	switch {
	case !kernel.ValidIdentifier(key):
		return Reconciliation{}, kernel.Reject(kernel.CodeSchemaInvalid, "/args/idempotency_key", "")
	case !hashPattern.MatchString(hash):
		return Reconciliation{}, kernel.Reject(kernel.CodeSchemaInvalid, "/args/request_hash", "")
	}
	if _, err := l.Authorizer.Policy.Rule(command); err != nil {
		return Reconciliation{}, kernel.Reject(kernel.CodeSchemaInvalid, "/args/command", "")
	}
	var rec Reconciliation
	err := l.inScope(ctx, p, func(ctx context.Context, exec bentopg.Executor) error {
		d, err := l.Authorizer.Authorize(ctx, exec, l.ReplayRead)
		if err != nil {
			return denied(err)
		}
		storedHash, res, found, err := l.stored(ctx, exec, p, command, key)
		if err != nil {
			return err
		}
		rec = Reconciliation{State: kernel.ReconcileNotCommitted}
		if found {
			visible, err := l.Visible(ctx, exec, d, command, res.ObjectID)
			if err != nil {
				return err
			}
			if !visible {
				// The same answer for a matching and a conflicting hash: a conflict must not reveal
				// another owner's action either.
				return kernel.Reject(kernel.CodeNotFound, "/args/idempotency_key", "")
			}
			rec = Reconciliation{State: kernel.ReconcilePayloadConflict, ActionID: res.ActionID, ObjectID: res.ObjectID}
			if storedHash == hash {
				rec.State, rec.Result = kernel.ReconcileCommitted, &res
			}
		}
		return l.audit(ctx, exec, p, l.reconcileAudit(), rec.ObjectID, "read", "", "", d.Epoch)
	})
	if err != nil {
		return Reconciliation{}, err
	}
	return rec, nil
}

// Read runs an audited read: it authorizes command, runs fn and records the read. fn returns a
// *kernel.Rejection (normally NOT_FOUND) for an object the actor does not see.
func (l *Ledger) Read(ctx context.Context, command, objectID string, fn func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error) error {
	if err := l.check(); err != nil {
		return err
	}
	p, err := identity.FromContext(ctx)
	if err != nil {
		return err
	}
	ctx = ensureCorrelation(ctx)
	err = l.inScope(ctx, p, func(ctx context.Context, exec bentopg.Executor) error {
		d, err := l.Authorizer.Authorize(ctx, exec, command)
		if err != nil {
			return denied(err)
		}
		if err = fn(ctx, exec, d); err != nil {
			return err
		}
		return l.audit(ctx, exec, p, command, objectID, "read", "", "", d.Epoch)
	})
	if rej, ok := kernel.AsRejection(err); ok {
		if auditErr := l.auditRejected(ctx, p, command, objectID, rej.Code); auditErr != nil {
			return errors.Join(rej, auditErr)
		}
	}
	return err
}

// Lookup answers a reference lookup from a downstream context in a transaction of this context.
// An empty command asks only for an active grant (Authorizer.Access); otherwise the command's
// permission is required. Lookups are not audited: the downstream command that asked records its
// own outcome.
func (l *Ledger) Lookup(ctx context.Context, command string, fn func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error) error {
	if err := l.check(); err != nil {
		return err
	}
	p, err := identity.FromContext(ctx)
	if err != nil {
		return err
	}
	return l.inScope(ctx, p, func(ctx context.Context, exec bentopg.Executor) error {
		var d authorization.Decision
		var err error
		if command == "" {
			d, err = l.Authorizer.Access(ctx, exec)
		} else {
			d, err = l.Authorizer.Authorize(ctx, exec, command)
		}
		if err != nil {
			return denied(err)
		}
		return fn(ctx, exec, d)
	})
}

// AuditRejected records a rejection an inbound adapter decided before the context saw the command,
// such as a request that is not valid JSON.
func (l *Ledger) AuditRejected(ctx context.Context, command, objectID, code string) error {
	if err := l.check(); err != nil {
		return err
	}
	p, err := identity.FromContext(ctx)
	if err != nil {
		return err
	}
	return l.auditRejected(ensureCorrelation(ctx), p, command, objectID, code)
}

// AuditEntry is one line of a context's audit trail.
type AuditEntry = kernel.AuditEntry

// Audit returns the scope's audit trail in order; command is the read command that guards it.
func (l *Ledger) Audit(ctx context.Context, command string) ([]AuditEntry, error) {
	var out []AuditEntry
	err := l.Lookup(ctx, command, func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		p, _ := identity.FromContext(ctx)
		rows, err := exec.Query(ctx, `SELECT seq, actor_kind, actor_id, command, coalesce(object_id, ''), outcome,
			coalesce(code, ''), correlation_id, recorded_at, coalesce(reason, ''), coalesce(authority_epoch, 0)
			FROM `+l.Schema+`.audit_event WHERE tenant_id = $1 AND company_id = $2 ORDER BY seq`,
			string(p.Scope.Tenant), string(p.Scope.Company))
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (AuditEntry, error) {
			var e AuditEntry
			var kind string
			err := row.Scan(&e.Seq, &kind, &e.Actor.ID, &e.Command, &e.ObjectID, &e.Outcome, &e.Code, &e.CorrelationID,
				&e.RecordedAt, &e.Reason, &e.AuthorityEpoch)
			e.Actor.Kind = kernel.ActorKind(kind)
			return e, err
		})
		return err
	})
	return out, err
}

func (l *Ledger) inScope(ctx context.Context, p identity.Principal, fn func(context.Context, bentopg.Executor) error) error {
	return bento.ScopeTransactor{DB: l.DB, Scope: p.Scope}.WithinTransaction(ctx, func(ctx context.Context) error {
		exec, err := l.DB.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		return fn(ctx, exec)
	})
}

func (l *Ledger) audit(ctx context.Context, exec bentopg.Executor, p identity.Principal, command, objectID, outcome, code, reason string, epoch int64) error {
	now := l.Clock.Now()
	_, err := exec.Exec(ctx, `INSERT INTO `+l.Schema+`.audit_event (tenant_id, company_id, event_id, actor_kind, actor_id,
		command, object_id, outcome, code, correlation_id, recorded_at, reason, authority_epoch)
		VALUES ($1, $2, $3, $4, $5, $6, nullif($7, ''), $8, nullif($9, ''), $10, $11, nullif($12, ''), nullif($13, 0))`,
		string(p.Scope.Tenant), string(p.Scope.Company), NewID("aud", now), string(p.Actor.Kind), p.Actor.ID, command,
		objectID, outcome, code, Correlation(ctx), now, reason, epoch)
	return err
}

// auditRejected records a rejection in a transaction of its own, after the command's transaction
// rolled back.
func (l *Ledger) auditRejected(ctx context.Context, p identity.Principal, command, objectID, code string) error {
	if !kernel.ValidIdentifier(objectID) {
		objectID = "" // never echo malformed request content into the audit trail
	}
	return l.inScope(ctx, p, func(ctx context.Context, exec bentopg.Executor) error {
		return l.audit(ctx, exec, p, command, objectID, "rejected", code, "", 0)
	})
}

func (l *Ledger) enqueue(ctx context.Context, p identity.Principal, d authorization.Decision, actionID string, now time.Time, e Event) error {
	payload := map[string]any{
		"tenant_id": string(p.Scope.Tenant), "company_id": string(p.Scope.Company),
		"aggregate_type": e.AggregateType, "aggregate_id": e.AggregateID, "aggregate_version": e.AggregateVersion,
		"actor_kind": string(p.Actor.Kind), "actor_id": p.Actor.ID, "authority_epoch": d.Epoch, "action_id": actionID,
	}
	for k, v := range e.Fields {
		if _, clash := payload[k]; clash {
			return fmt.Errorf("action: event field %q shadows an envelope field", k)
		}
		payload[k] = v
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	envelope := eventing.Envelope{
		SpecVersion: eventing.SpecVersionV1, ID: eventing.EventID(NewID("evt", now)), Source: l.Source,
		Type: eventing.EventType(e.Type), Version: 1, Scope: p.Scope.String(), Subject: e.AggregateID,
		// Events of one aggregate are delivered in order; aggregate IDs are unique within a scope.
		PartitionKey: p.Scope.String() + "/" + e.AggregateType + ":" + e.AggregateID,
		OccurredAt:   now, RecordedAt: now, CorrelationID: Correlation(ctx), CausationID: actionID,
		ContentType: eventing.JSONContentType, Payload: body,
	}
	if err = envelope.Validate(); err != nil {
		return err
	}
	return l.Outbox.Enqueue(ctx, envelope)
}

// denied turns an authorization failure into PERMISSION_DENIED and passes infrastructure errors on.
func denied(err error) error {
	switch {
	case errors.Is(err, authorization.ErrForbidden), errors.Is(err, authorization.ErrActorKindDenied),
		errors.Is(err, authorization.ErrUnknownCommand), errors.Is(err, authorization.ErrScopeMismatch),
		errors.Is(err, identity.ErrInvalidPrincipal):
		return errors.Join(kernel.Reject(kernel.CodePermissionDenied, "/", ""), err)
	}
	return err
}

func uniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func ensureCorrelation(ctx context.Context) context.Context {
	return WithCorrelation(ctx, Correlation(ctx))
}
