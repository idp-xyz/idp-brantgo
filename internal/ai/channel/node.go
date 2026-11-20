package channel

import (
	"slices"
	"time"

	tskapi "go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// State is where a send operation stands.
type State string

const (
	// Authorized: a person authorized the send of the content to the recipient; nothing went.
	Authorized State = "authorized"
	// Submitted: the send is recorded as handed to the channel and its answer is pending; a process
	// that dies here leaves it for reconciliation (the record comes before the send, as TSK-INV-003).
	Submitted State = "submitted"
	// Unknown: the channel's answer was lost; the provider's lookup or a person settles it.
	Unknown State = "unknown"
	// ManualRequired: no channel is configured; a person sends it themselves and confirms it (the
	// manual send node, baseline 8.2).
	ManualRequired State = "manual_required"
	// ProviderAccepted: the provider took the message (its receipt); delivery is a later, separate
	// report.
	ProviderAccepted State = "provider_accepted"
	// ProviderRejected: the provider refused the message; nothing went. A new send is a new
	// authorization.
	ProviderRejected State = "provider_rejected"
	// ManuallySent: a person confirmed sending it outside Brant, with evidence. Not a provider
	// receipt, not delivery.
	ManuallySent State = "manually_sent"
	// ReconciledSent: an unknown send that a person established, with evidence, as sent.
	ReconciledSent State = "reconciled_sent"
)

// Sent reports whether the operation went out by any path (provider, by hand, or as reconciled).
func (s State) Sent() bool { return s == ProviderAccepted || s == ManuallySent || s == ReconciledSent }

// Settled reports whether the operation has an outcome a task can go on with (sent, or refused by
// the provider).
func (s State) Settled() bool { return s.Sent() || s == ProviderRejected }

// RecordKind is the kind of an operation's record. Each fact has its own kind: a manual send
// confirmation is never a provider receipt, and neither is delivery.
type RecordKind string

const (
	RecordAuthorization      RecordKind = "send_authorization"
	RecordSubmission         RecordKind = "submission"
	RecordProviderReceipt    RecordKind = "provider_receipt"
	RecordLookup             RecordKind = "lookup"
	RecordManualConfirmation RecordKind = "manual_send_confirmation"
	RecordManualRecon        RecordKind = "manual_reconciliation"
)

// Record outcomes besides the ReceiptStatus values.
const (
	OutcomeAuthorized    = "authorized"
	OutcomeSubmitted     = "submitted"
	OutcomeNotConfigured = "not_configured"
	OutcomeUnknown       = "unknown"
	OutcomeUnavailable   = "unavailable"
	OutcomeSent          = "sent"
	OutcomeNotSent       = "not_sent"
)

// Evidence names what a person's record rests on: an EVD source or file (the sent mail, the
// provider's statement) at its version or SHA-256.
type Evidence struct {
	Kind    string // evd_source or evd_file
	ID      string
	Version int64
	SHA256  string
}

func (e *Evidence) valid() bool {
	return e != nil && (e.Kind == "evd_source" || e.Kind == "evd_file") && kernel.ValidIdentifier(e.ID) && (e.Version > 0 || len(e.SHA256) == 64)
}

// Record is one append-only record of an operation.
type Record struct {
	Seq     int
	Kind    RecordKind
	Actor   kernel.ActorRef
	At      time.Time
	Outcome string
	// ProviderRef and Code come only from a provider's receipt.
	ProviderRef string
	Code        string
	// Means (how a person sent it), Note and Evidence come only from a person's record.
	Means    string
	Note     string
	Evidence *Evidence
}

// Operation is a send operation: the content, the recipient, the person's authorization and every
// record since.
type Operation struct {
	OperationID string
	Scope       kernel.Scope
	TaskID      string
	Channel     string
	Content     Content
	Recipient   Recipient
	State       State
	Records     []Record
	Version     int64
}

// Message is what the operation sends.
func (o Operation) Message() Message {
	return Message{OperationID: o.OperationID, Channel: o.Channel, Content: o.Content, Recipient: o.Recipient}
}

// ProviderReceipts returns the provider's receipts of the operation (never a person's record).
func (o Operation) ProviderReceipts() []Record {
	return o.records(RecordProviderReceipt)
}

// ManualConfirmations returns the people's confirmations of a manual send (never a receipt).
func (o Operation) ManualConfirmations() []Record {
	return o.records(RecordManualConfirmation)
}

func (o Operation) records(k RecordKind) []Record {
	var out []Record
	for _, r := range o.Records {
		if r.Kind == k {
			out = append(out, r)
		}
	}
	return out
}

// ProviderAccepted reports whether the provider gave a receipt that it took the message.
func (o Operation) ProviderAccepted() bool {
	return slices.ContainsFunc(o.ProviderReceipts(), func(r Record) bool {
		return r.Outcome == string(Accepted) || r.Outcome == string(Delivered)
	})
}

// Delivered reports whether the provider reported delivery. A person's confirmation never does.
func (o Operation) Delivered() bool {
	return slices.ContainsFunc(o.ProviderReceipts(), func(r Record) bool { return r.Outcome == string(Delivered) })
}

// ArrivalExternalSend is the kind of what a task waiting for a send awaits (task.wait external,
// awaiting {external_send, operation ID}) and of the arrival its resume records once the operation
// has settled.
const ArrivalExternalSend = "external_send"

// Awaiting is what a task waits for while the operation is open (task.wait external).
func (o Operation) Awaiting() tskapi.ObjectRef {
	return tskapi.ObjectRef{Kind: ArrivalExternalSend, ID: o.OperationID}
}

// Arrival is what arrived for a waiting task once the operation has settled (task.resume), at the
// number of records it settled with.
func (o Operation) Arrival() tskapi.ObjectRef {
	return tskapi.ObjectRef{Kind: ArrivalExternalSend, ID: o.OperationID, Version: int64(len(o.Records))}
}

// last reports whether the operation's latest record is of kind k with outcome.
func (o Operation) last(k RecordKind, outcome string) bool {
	n := len(o.Records)
	return n > 0 && o.Records[n-1].Kind == k && o.Records[n-1].Outcome == outcome
}

func (o *Operation) add(r Record, to State) {
	r.Seq = len(o.Records) + 1
	o.Records = append(o.Records, r)
	o.State = to
}
