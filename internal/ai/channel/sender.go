package channel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	tskapi "go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Store keeps send operations by scope and operation ID.
type Store interface {
	// Get returns the operation, or false when the scope has none under the ID.
	Get(ctx context.Context, scope kernel.Scope, operationID string) (Operation, bool, error)
	// Put stores op when the stored version is expected (0: none is stored), and ErrStale otherwise.
	Put(ctx context.Context, op Operation, expected int64) error
}

// ErrStale is a Put over an operation that changed meanwhile.
var ErrStale = errors.New("channel: the operation changed meanwhile")

// MemoryStore is an in-process Store (tests; a process without a persisted send node — brant_ai has
// no table for it in S4).
type MemoryStore struct {
	mu  sync.Mutex
	ops map[string]Operation
}

func storeKey(scope kernel.Scope, id string) string {
	return string(scope.Tenant) + "\x1f" + string(scope.Company) + "\x1f" + id
}

// Get returns a copy of the operation.
func (m *MemoryStore) Get(_ context.Context, scope kernel.Scope, id string) (Operation, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	op, ok := m.ops[storeKey(scope, id)]
	return clone(op), ok, nil
}

// Put stores a copy of op at the next version.
func (m *MemoryStore) Put(_ context.Context, op Operation, expected int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ops == nil {
		m.ops = map[string]Operation{}
	}
	k := storeKey(op.Scope, op.OperationID)
	if cur, ok := m.ops[k]; (ok && cur.Version != expected) || (!ok && expected != 0) {
		return ErrStale
	}
	op = clone(op)
	op.Version = expected + 1
	m.ops[k] = op
	return nil
}

func clone(op Operation) Operation {
	op.Records = append([]Record(nil), op.Records...)
	for i := range op.Records {
		if e := op.Records[i].Evidence; e != nil {
			c := *e
			op.Records[i].Evidence = &c
		}
	}
	return op
}

// Sender runs send operations over a channel: the person's authorization, the single send, the
// provider's receipts, the lookup of an unknown send, and the people's manual records.
type Sender struct {
	Channel Port
	Store   Store
	Clock   func() time.Time
	// Verify, when set, checks a person's evidence before it is recorded (the EVD source or file
	// exists and the person may read it).
	Verify func(ctx context.Context, e Evidence) error
}

// Limits of a person's text.
const (
	MaxMeans = 200
	MaxNote  = 2000
)

func (s Sender) now() time.Time {
	if s.Clock != nil {
		return s.Clock().UTC()
	}
	return time.Now().UTC()
}

func caller(ctx context.Context, kinds ...kernel.ActorKind) (identity.Principal, error) {
	p, err := identity.FromContext(ctx)
	if err != nil {
		return p, kernel.Reject(kernel.CodeUnauthenticated, "/", "")
	}
	for _, k := range kinds {
		if p.Actor.Kind == k {
			return p, nil
		}
	}
	return p, kernel.Reject(kernel.CodePermissionDenied, "/actor", "Only "+kindNames(kinds)+" may do this")
}

func kindNames(kinds []kernel.ActorKind) string {
	n := make([]string, len(kinds))
	for i, k := range kinds {
		n[i] = string(k)
	}
	return strings.Join(n, " or ")
}

func schema(path string) error { return kernel.Reject(kernel.CodeSchemaInvalid, path, "") }

func invalidState(op Operation, why string) error {
	return kernel.Reject(kernel.CodeInvalidState, "/operation_id", "The send is "+string(op.State)+": "+why)
}

func unknownResult(op Operation) error {
	return errors.Join(ErrUnknown, kernel.RejectAs(tskapi.CodeExternalResultUnknown, kernel.CategoryConflict, "/operation_id",
		"The send "+op.OperationID+" may have reached the provider; reconcile it (lookup or a person) before anything else"))
}

// Authorize is a person's authorization to send Content to Recipient over Channel for TaskID.
type Authorize struct {
	TaskID    string
	Channel   string
	Content   Content
	Recipient Recipient
}

func (a Authorize) validate() error {
	switch {
	case !kernel.ValidIdentifier(a.TaskID):
		return schema("/task_id")
	case !kernel.ValidIdentifier(a.Channel):
		return schema("/channel")
	case !kernel.ValidIdentifier(a.Content.Kind) || !kernel.ValidIdentifier(a.Content.ID) || a.Content.Version < 1 || len(a.Content.SHA256) != 64:
		return schema("/content")
	case !kernel.ValidIdentifier(a.Recipient.PartyID) || len(a.Recipient.SnapshotSHA256) != 64 || strings.TrimSpace(a.Recipient.Address) == "":
		return schema("/recipient")
	}
	return nil
}

// Authorize records a person's authorization of a send (human only: "发送需独立授权"; an AI agent never
// authorizes or sends, decision D-7). The same send authorized again is the same operation and
// answers it as it stands.
func (s Sender) Authorize(ctx context.Context, a Authorize) (Operation, error) {
	p, err := caller(ctx, kernel.ActorHuman)
	if err != nil {
		return Operation{}, err
	}
	if err = a.validate(); err != nil {
		return Operation{}, err
	}
	id := OperationOf(p.Scope, a.TaskID, a.Channel, a.Content, a.Recipient)
	if op, ok, err := s.Store.Get(ctx, p.Scope, id); err != nil || ok {
		return op, err
	}
	op := Operation{OperationID: id, Scope: p.Scope, TaskID: a.TaskID, Channel: a.Channel, Content: a.Content, Recipient: a.Recipient}
	op.add(Record{Kind: RecordAuthorization, Actor: p.Actor, At: s.now(), Outcome: OutcomeAuthorized}, Authorized)
	return s.put(ctx, op, 0)
}

func (s Sender) put(ctx context.Context, op Operation, expected int64) (Operation, error) {
	if err := s.Store.Put(ctx, op, expected); err != nil {
		if errors.Is(err, ErrStale) {
			return Operation{}, errors.Join(err, kernel.Reject(kernel.CodeVersionConflict, "/operation_id", "The send changed meanwhile"))
		}
		return Operation{}, err
	}
	op.Version = expected + 1
	return op, nil
}

func (s Sender) load(ctx context.Context, scope kernel.Scope, id string) (Operation, error) {
	op, ok, err := s.Store.Get(ctx, scope, id)
	if err != nil {
		return Operation{}, err
	}
	if !ok {
		return Operation{}, kernel.Reject(kernel.CodeNotFound, "/operation_id", "")
	}
	return op, nil
}

// Send sends an authorized operation once (a person or the system; never an AI agent). The send is
// recorded as submitted before the channel is asked, so of two concurrent sends only one reaches
// the channel (VERSION_CONFLICT for the other), and a process that dies after the record leaves the
// operation for reconciliation. Without a channel the operation becomes a manual send node; a lost
// answer makes it unknown (EXTERNAL_RESULT_UNKNOWN); a channel that cannot be reached leaves it
// authorized (ErrUnavailable, nothing went). An operation that has settled, or waits for a person,
// answers as it stands; one that is submitted or unknown must be reconciled first.
func (s Sender) Send(ctx context.Context, operationID string) (Operation, error) {
	p, err := caller(ctx, kernel.ActorHuman, kernel.ActorSystem)
	if err != nil {
		return Operation{}, err
	}
	op, err := s.load(ctx, p.Scope, operationID)
	if err != nil {
		return Operation{}, err
	}
	switch op.State {
	case Authorized:
	case Submitted, Unknown:
		return op, unknownResult(op)
	default:
		return op, nil
	}
	op.add(Record{Kind: RecordSubmission, Actor: p.Actor, At: s.now(), Outcome: OutcomeSubmitted}, Submitted)
	if op, err = s.put(ctx, op, op.Version); err != nil {
		return Operation{}, err
	}
	rc, sendErr := s.Channel.Send(ctx, op.Message())
	at := s.now()
	var result error
	switch {
	case errors.Is(sendErr, ErrNotConfigured):
		op.add(Record{Kind: RecordSubmission, Actor: p.Actor, At: at, Outcome: OutcomeNotConfigured, Code: CodeNotConfigured}, ManualRequired)
	case errors.Is(sendErr, ErrUnavailable):
		op.add(Record{Kind: RecordSubmission, Actor: p.Actor, At: at, Outcome: OutcomeUnavailable}, Authorized)
		result = sendErr
	case sendErr == nil && rc.valid(op.OperationID) && rc.Status != NotReceived:
		op.add(receiptRecord(rc, at), stateOf(rc.Status))
	default:
		// Lost, or an answer Brant cannot read: the provider may have it.
		op.add(Record{Kind: RecordSubmission, Actor: p.Actor, At: at, Outcome: OutcomeUnknown}, Unknown)
		result = unknownResult(op)
		if sendErr != nil {
			result = errors.Join(result, sendErr)
		}
	}
	saved, err := s.put(ctx, op, op.Version)
	if err != nil {
		return Operation{}, errors.Join(err, result)
	}
	return saved, result
}

func receiptRecord(rc Receipt, at time.Time) Record {
	if !rc.At.IsZero() {
		at = rc.At.UTC()
	}
	return Record{Kind: RecordProviderReceipt, Actor: kernel.ActorRef{Kind: kernel.ActorSystem, ID: "system:channel"}, At: at,
		Outcome: string(rc.Status), ProviderRef: rc.ProviderRef, Code: rc.Code}
}

func stateOf(st ReceiptStatus) State {
	if st == Rejected {
		return ProviderRejected
	}
	return ProviderAccepted
}

// Reconcile asks the provider about an operation (a person or the system): a submitted or unknown
// send becomes provider_accepted or provider_rejected on the provider's receipt, or authorized again
// when the provider has nothing under the ID (it may then be sent again under the same ID); an
// accepted one learns a delivery report. A lookup that fails, or a channel that is not configured,
// leaves an unknown send unknown — a person reconciles it (ReconcileManually).
func (s Sender) Reconcile(ctx context.Context, operationID string) (Operation, error) {
	p, err := caller(ctx, kernel.ActorHuman, kernel.ActorSystem)
	if err != nil {
		return Operation{}, err
	}
	op, err := s.load(ctx, p.Scope, operationID)
	if err != nil {
		return Operation{}, err
	}
	open := op.State == Submitted || op.State == Unknown
	if !open && (op.State != ProviderAccepted || op.Delivered()) {
		return op, nil
	}
	rc, lookErr := s.Channel.Lookup(ctx, op.OperationID)
	at := s.now()
	var result error
	switch {
	case lookErr != nil || !rc.valid(op.OperationID):
		if !open {
			return op, lookErr
		}
		outcome := OutcomeUnavailable
		if errors.Is(lookErr, ErrNotConfigured) {
			outcome = OutcomeNotConfigured
		}
		op.add(Record{Kind: RecordLookup, Actor: p.Actor, At: at, Outcome: outcome}, Unknown)
		result = unknownResult(op)
		if lookErr != nil {
			result = errors.Join(result, lookErr)
		}
	case rc.Status == NotReceived:
		if !open {
			// The provider forgot an accepted message: recorded once, and the receipt stands.
			if op.last(RecordLookup, string(NotReceived)) {
				return op, nil
			}
			op.add(Record{Kind: RecordLookup, Actor: p.Actor, At: at, Outcome: string(NotReceived)}, op.State)
			break
		}
		op.add(Record{Kind: RecordLookup, Actor: p.Actor, At: at, Outcome: string(NotReceived)}, Authorized)
	case !open && rc.Status == Accepted:
		return op, nil
	case !open && rc.Status == Rejected:
		// A later refusal of an accepted message (a bounce): the provider's own receipt, recorded;
		// the acceptance stands as a fact and nothing is delivered. Recorded once.
		if op.last(RecordProviderReceipt, string(Rejected)) {
			return op, nil
		}
		op.add(receiptRecord(rc, at), op.State)
	default:
		op.add(receiptRecord(rc, at), stateOf(rc.Status))
	}
	saved, err := s.put(ctx, op, op.Version)
	if err != nil {
		return Operation{}, errors.Join(err, result)
	}
	return saved, result
}

// ManualSend is a person's confirmation that they sent the content themselves: how, and the
// evidence (the sent mail registered in EVD).
type ManualSend struct {
	Means    string
	Note     string
	Evidence Evidence
}

func (m ManualSend) validate() error {
	switch {
	case strings.TrimSpace(m.Means) == "" || utf8.RuneCountInString(m.Means) > MaxMeans:
		return schema("/means")
	case utf8.RuneCountInString(m.Note) > MaxNote:
		return schema("/note")
	case !m.Evidence.valid():
		return schema("/evidence")
	}
	return nil
}

// ConfirmManualSend completes a manual send node (human only): the person sent it outside Brant and
// says how, with evidence. It is recorded as a manual send confirmation — never as a provider
// receipt or delivery. The same confirmation again answers the operation as it stands.
func (s Sender) ConfirmManualSend(ctx context.Context, operationID string, m ManualSend) (Operation, error) {
	p, err := caller(ctx, kernel.ActorHuman)
	if err != nil {
		return Operation{}, err
	}
	if err = m.validate(); err != nil {
		return Operation{}, err
	}
	op, err := s.load(ctx, p.Scope, operationID)
	if err != nil {
		return Operation{}, err
	}
	if op.State == ManuallySent {
		for _, r := range op.ManualConfirmations() {
			if r.Actor == p.Actor && r.Means == m.Means && *r.Evidence == m.Evidence {
				return op, nil
			}
		}
	}
	if op.State != ManualRequired {
		return op, invalidState(op, "only a manual send node is confirmed by hand")
	}
	if err = s.verify(ctx, m.Evidence); err != nil {
		return Operation{}, err
	}
	e := m.Evidence
	op.add(Record{Kind: RecordManualConfirmation, Actor: p.Actor, At: s.now(), Outcome: OutcomeSent, Means: m.Means, Note: m.Note, Evidence: &e}, ManuallySent)
	return s.put(ctx, op, op.Version)
}

func (s Sender) verify(ctx context.Context, e Evidence) error {
	if s.Verify == nil {
		return nil
	}
	if err := s.Verify(ctx, e); err != nil {
		if _, ok := kernel.AsRejection(err); ok {
			return errors.Join(kernel.Reject(kernel.CodeReferenceInvalid, "/evidence", "The evidence cannot be read"), err)
		}
		return err
	}
	return nil
}

// ManualReconciliation is a person's finding about an unknown send, with evidence (the provider's
// statement, the recipient's answer): it went, or it did not.
type ManualReconciliation struct {
	Sent     bool
	Note     string
	Evidence Evidence
}

// ReconcileManually settles a submitted or unknown send by a person's finding (human only; baseline
// 17.2 人工对账): sent → reconciled_sent; not sent → authorized again (it may be sent under the same
// ID). Recorded as a manual reconciliation, never as a provider receipt.
func (s Sender) ReconcileManually(ctx context.Context, operationID string, m ManualReconciliation) (Operation, error) {
	p, err := caller(ctx, kernel.ActorHuman)
	if err != nil {
		return Operation{}, err
	}
	switch {
	case strings.TrimSpace(m.Note) == "" || utf8.RuneCountInString(m.Note) > MaxNote:
		return Operation{}, schema("/note")
	case !m.Evidence.valid():
		return Operation{}, schema("/evidence")
	}
	op, err := s.load(ctx, p.Scope, operationID)
	if err != nil {
		return Operation{}, err
	}
	if op.State != Submitted && op.State != Unknown {
		return op, invalidState(op, "only a send whose outcome is unknown is reconciled by hand")
	}
	if err = s.verify(ctx, m.Evidence); err != nil {
		return Operation{}, err
	}
	e := m.Evidence
	to, outcome := Authorized, OutcomeNotSent
	if m.Sent {
		to, outcome = ReconciledSent, OutcomeSent
	}
	op.add(Record{Kind: RecordManualRecon, Actor: p.Actor, At: s.now(), Outcome: outcome, Note: m.Note, Evidence: &e}, to)
	return s.put(ctx, op, op.Version)
}

// Get reads an operation of the caller's scope.
func (s Sender) Get(ctx context.Context, operationID string) (Operation, error) {
	p, err := identity.FromContext(ctx)
	if err != nil {
		return Operation{}, kernel.Reject(kernel.CodeUnauthenticated, "/", "")
	}
	return s.load(ctx, p.Scope, operationID)
}

// String names the operation without its address.
func (o Operation) String() string {
	return fmt.Sprintf("send %s (%s %s v%d to %s) %s", o.OperationID, o.Content.Kind, o.Content.ID, o.Content.Version, o.Recipient.PartyID, o.State)
}
