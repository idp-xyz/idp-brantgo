// Package api is the contract of the CST bounded context (costing: costing contexts, measurement
// snapshots, cost scenarios, their evaluation and frozen cost snapshots) for transports, the AI
// layer and the contexts downstream of CST. It exposes no CST domain type.
//
// A costing context (CostingContext, T02 §4) binds a ready INQ RFQ with its complete line set
// (RfqRef v1), the POL configurations it costs under (ConfigRef v1) and the costing assumptions;
// its content lives in revisions with monotonic numbers like a POL configuration: a working
// revision is saved whole, a human who did not contribute to it verifies it, and verifying
// supersedes the active revision atomically (T02 §13.2). A measurement snapshot (MeasurementSnapshot,
// T02 §13.8) is a human-recorded, immutable revision of package measurements with an effect head.
// Lookups (lookups.go) give the scenario evaluation and the freeze what a verified costing context
// and an active measurement say.
package api

import (
	"context"
	"strconv"
	"unicode/utf8"

	"go.idp.xyz/idp-brant/internal/kernel"
)

const (
	// Contract names the request and response shapes of CST commands (Python brant.costing/0.1.0).
	Contract = "brant.costing/0.1.0"
	// Source identifies CST as the producer of integration events.
	Source = "idp-brant.cst"

	// Events of the tactical model (CostingContextVerified, MeasurementRecorded); the event catalog
	// V0.1 does not list them yet.
	EventCostingContextVerified = "idp-brant.cst.costing_context_verified.v1"
	EventMeasurementRecorded    = "idp-brant.cst.measurement_recorded.v1"

	CommandSaveContext          = "costing_context.save_draft"
	CommandReviseContext        = "costing_context.revise"
	CommandDiscardContextDraft  = "costing_context.discard_draft"
	CommandVerifyContext        = "costing_context.verify"
	CommandWithdrawContext      = "costing_context.withdraw"
	CommandRecordMeasurement    = "measurement.record_verified"
	CommandSupersedeMeasurement = "measurement.supersede"
	CommandWithdrawMeasurement  = "measurement.withdraw"
)

// Statuses of a costing context revision and of a measurement revision. A costing context
// revision is working until it is verified (active) or discarded; an active revision is
// superseded by the next verification or withdrawn; a superseded revision may still be withdrawn.
// A measurement revision is active when recorded, until superseded or withdrawn.
const (
	StatusWorking    = "working"
	StatusActive     = "active"
	StatusSuperseded = "superseded"
	StatusWithdrawn  = "withdrawn"
	StatusDiscarded  = "discarded"
)

// Limits of the contract (Python commands.schema.json).
const (
	MaxLines            = 200
	MaxConversions      = 8
	MaxRequiredFees     = 50
	MaxAssumptions      = 100
	MaxAssumptionProofs = 20
	MaxEvidence         = 100
	MaxPackages         = 1000
	MaxPackageLines     = 200
	MaxPackageCount     = 1_000_000

	maxVersion  = 2147483647
	maxText     = 1000
	maxTimeZone = 100
	maxLeadDays = 3650
)

// ConfigRef is a POL ConfigRef v1 as CST's contract carries it: a configuration kind, its ID and
// the exact revision adopted. Each field of a costing context names the kind it takes.
type ConfigRef struct {
	Kind     string
	ID       string
	Revision int64
}

// Validate checks the form of the reference; path is where it sits in the request. Whether the
// kind fits the field and the revision exists is decided against POL (REFERENCE_INVALID).
func (r ConfigRef) Validate(path string) error { return ref(r.Kind, r.ID, r.Revision, path) }

// Evidence is a source a human relied on, with where in it: EVD source ID and locator. The actor of
// the command reads the source in EVD (Context Map R64, SourceRef v1): REFERENCE_INVALID at the
// entry's source_id otherwise, alike for a source that does not exist, and PERMISSION_DENIED
// without EVD's read permission.
type Evidence struct {
	SourceID string
	Locator  string
}

// Auditor records a request an inbound adapter refused before it became a command, such as a body
// that is not valid JSON, in the context's audit trail. The principal in ctx is the requester.
type Auditor interface {
	AuditRejected(ctx context.Context, command, objectID, code string) error
}

// Receipt is the closed success answer of a command. ObjectID is the costing context or
// measurement ID, RowVersion its root version and Status the status of the revision the command
// wrote; GetContext and GetMeasurement show the revision numbers.
type Receipt struct {
	ActionID   string
	ObjectID   string
	RowVersion int64
	Status     string
	Replayed   bool
}

// Commands is CST's command entry point. Errors are *kernel.Rejection for refusals. An error that
// is application.ErrCommitUncertain from IDP-Bento-Go means the outcome is unknown: the caller
// sends the same command with the same idempotency key again, which returns the stored receipt if
// the command committed.
type Commands interface {
	SaveContext(ctx context.Context, cmd SaveContext) (Receipt, error)
	ReviseContext(ctx context.Context, cmd ReviseContext) (Receipt, error)
	DiscardContextDraft(ctx context.Context, cmd DiscardContextDraft) (Receipt, error)
	VerifyContext(ctx context.Context, cmd VerifyContext) (Receipt, error)
	WithdrawContext(ctx context.Context, cmd WithdrawContext) (Receipt, error)
	RecordMeasurement(ctx context.Context, cmd RecordMeasurement) (Receipt, error)
	SupersedeMeasurement(ctx context.Context, cmd SupersedeMeasurement) (Receipt, error)
	WithdrawMeasurement(ctx context.Context, cmd WithdrawMeasurement) (Receipt, error)
}

// Queries is CST's audited read side.
type Queries interface {
	// GetContext returns a costing context the caller sees with all its revisions, audited as
	// costing_context.get.
	GetContext(ctx context.Context, contextID string) (ContextView, error)
	// GetMeasurement returns a measurement snapshot the caller sees with all its revisions,
	// audited as measurement.get.
	GetMeasurement(ctx context.Context, measurementID string) (MeasurementView, error)
	Audit(ctx context.Context) ([]kernel.AuditEntry, error)
}

func head(key, id string, expected, minVersion int64) error {
	switch {
	case !kernel.ValidIdentifier(key):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(id):
		return schema("/args/id")
	case expected < minVersion || expected > maxVersion:
		return schema("/args/expected_version")
	}
	return nil
}

func revision(n int64, path string) error {
	if n < 1 || n > maxVersion {
		return schema(path)
	}
	return nil
}

func ref(kind, id string, rev int64, path string) error {
	switch {
	case !kernel.ValidIdentifier(kind):
		return schema(path + "/kind")
	case !kernel.ValidIdentifier(id):
		return schema(path + "/id")
	}
	return revision(rev, path+"/revision")
}

func evidence(es []Evidence, min, max int, path string) error {
	if len(es) < min || len(es) > max {
		return schema(path)
	}
	for i, e := range es {
		p := index(path, i)
		switch {
		case !kernel.ValidIdentifier(e.SourceID):
			return schema(p + "/source_id")
		case !text(e.Locator, maxText):
			return schema(p + "/locator")
		}
	}
	return nil
}

func identifiers(ids []string, min, max int, path string) error {
	if len(ids) < min || len(ids) > max {
		return schema(path)
	}
	for i, id := range ids {
		if !kernel.ValidIdentifier(id) {
			return schema(index(path, i))
		}
	}
	return nil
}

func reason(s string) error {
	if !text(s, maxText) {
		return schema("/args/reason")
	}
	return nil
}

func schema(path string) error { return kernel.Reject(kernel.CodeSchemaInvalid, path, "") }

func text(s string, max int) bool {
	n := utf8.RuneCountInString(s)
	return n >= 1 && n <= max && utf8.ValidString(s)
}

// positive reports whether s is a positive decimal of the quantity format (at most 6 fractional
// digits, no sign, exponent or leading zero).
func positive(s string) bool {
	d, err := kernel.ParseDecimal(s, kernel.QuantityFormat)
	return err == nil && d.Sign() > 0
}

func index(path string, i int) string { return path + "/" + strconv.Itoa(i) }

// Command is one of CST's commands.
type Command interface{ cstCommand() }

func (SaveContext) cstCommand()          {}
func (ReviseContext) cstCommand()        {}
func (DiscardContextDraft) cstCommand()  {}
func (VerifyContext) cstCommand()        {}
func (WithdrawContext) cstCommand()      {}
func (RecordMeasurement) cstCommand()    {}
func (SupersedeMeasurement) cstCommand() {}
func (WithdrawMeasurement) cstCommand()  {}

// Reconciliation is what became of a command whose answer the caller lost.
type Reconciliation struct {
	State    kernel.ReconcileState
	ActionID string   // committed and payload_conflict
	ObjectID string   // committed and payload_conflict
	Receipt  *Receipt // the stored receipt; committed only
}

// Reconciler recovers from an unknown outcome without executing anything (contract 06, 动作对账).
// The caller passes the command exactly as it sent it, idempotency key included. Reconciling needs
// the read permission cst.read, not the command's own, and re-checks it and the caller's data scope
// now: PERMISSION_DENIED and NOT_FOUND as for a read.
type Reconciler interface {
	Reconcile(ctx context.Context, cmd Command) (Reconciliation, error)
}

// ActionReconciler is CST's ActionReconciliation v1 (Context Map R69 CST→TSK, S4 decision D-11):
// reconciliation by the ledger identity of a command, for a caller that kept the request rather
// than the command (TSK's ActionExecution, which records it before the command is sent).
//
// RequestOf returns the command name, idempotency key and request hash CST's ledger records for cmd
// — hashed exactly as executing it hashes it — and executes and reads nothing; a command CST does
// not know is SCHEMA_INVALID. ReconcileHash answers what Reconcile answers for the command that
// hashes to requestHash: committed with the stored receipt, payload_conflict or not_committed,
// after the same checks (cst.read and the caller's data scope now, X-INV-003), audited the same way
// (action.reconcile), executing nothing. An unknown command name, an invalid key or a request hash
// that is not 64 lower-case hexadecimal digits is SCHEMA_INVALID.
type ActionReconciler interface {
	RequestOf(cmd Command) (kernel.ActionRequest, error)
	ReconcileHash(ctx context.Context, command, idempotencyKey, requestHash string) (Reconciliation, error)
}
