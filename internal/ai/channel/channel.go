// Package channel is the AI layer's port to external channels — mail and external exchange through
// BIP (ChannelPort; WBS S4-05; decision E-1; Context Map externalBoundaries "BIP（交换与渠道）→
// internal/ai／EVD, ACL": an exchange only produces sources and external result references, a send
// needs its own authorization and an operation_id; "S4 无可用接口：只交付端口与人工发送节点，unknown 以替身
// 测试"). It is the shape of Q01 V0.1 q01/ports.py ChannelPort (send(operation_id, approved_content),
// lookup(operation_id)).
//
// The send node (Operation, Sender) is what a task does when something approved is to leave the
// company (baseline 8.2, 18.2, 15.4 dispatch_sent): a person authorizes the send of exactly that
// content to exactly that recipient under a stable operation ID; the content goes to the channel
// once; without a channel (Unconnected, EXTERNAL_CHANNEL_NOT_CONFIGURED) the operation becomes a
// manual send node that a person completes by sending it themselves and confirming it with
// evidence. Manual send confirmation, the provider's receipt and the provider's delivery report are
// separate records and never stand in for each other ("人工发送确认与服务商回执分别记录，不能混称自动发送或
// 实际送达"); nothing is ever inferred delivered. A send whose answer is lost is unknown, settled by
// the provider's lookup under the same operation ID or by a person's reconciliation with evidence
// (baseline 17.2: 超时进入 unknown 并查询／人工对账), never by sending again under a new ID.
//
// No BIP interface exists (IG-11 stays open): internal/ai/adapters/bip is the unconnected BIP
// adapter and a fake provider for the unknown semantics. The send node has no table in brant_ai and
// no registered command yet (the Context Map names none): Store is a port with an in-process
// implementation, and the person-only steps check the actor kind, not a permission (S4 D2 report,
// design gaps).
package channel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Port is an external channel.
type Port interface {
	// Send hands m to the channel under m.OperationID. A channel that supports idempotency keys
	// answers a repeat of the same operation with its first receipt; one that does not still never
	// gets a new ID for the same send. An answer lost after the request may have reached the provider
	// is ErrUnknown.
	Send(ctx context.Context, m Message) (Receipt, error)
	// Lookup asks the provider what became of an operation: accepted, rejected, delivered, or not
	// received under that ID.
	Lookup(ctx context.Context, operationID string) (Receipt, error)
}

// Content is the approved content of a send, by reference and hash (a formal quote revision's
// document: kind quote_document, the quote, its business revision, the document's SHA-256).
type Content struct {
	Kind    string
	ID      string
	Version int64
	SHA256  string
}

// Recipient is who receives it: the party, the sealed recipient snapshot's SHA-256 (TRD's dispatch
// target) and the channel address taken from it.
type Recipient struct {
	PartyID        string
	SnapshotSHA256 string
	Address        string
}

// Message is what a channel is asked to send.
type Message struct {
	OperationID string
	Channel     string
	Content     Content
	Recipient   Recipient
}

// ReceiptStatus is a provider's answer about an operation.
type ReceiptStatus string

const (
	// Accepted: the provider took the message. Not delivery.
	Accepted ReceiptStatus = "accepted"
	// Rejected: the provider refused it; nothing went.
	Rejected ReceiptStatus = "rejected"
	// Delivered: the provider reports delivery to the recipient's system.
	Delivered ReceiptStatus = "delivered"
	// NotReceived: the provider has nothing under the operation ID (lookup only).
	NotReceived ReceiptStatus = "not_received"
)

// Receipt is a provider's answer.
type Receipt struct {
	OperationID string
	Status      ReceiptStatus
	// ProviderRef is the provider's ID of the message.
	ProviderRef string
	// Code is the provider's reason for a rejection.
	Code string
	At   time.Time
}

func (r Receipt) valid(op string) bool {
	switch r.Status {
	case Accepted, Rejected, Delivered, NotReceived:
	default:
		return false
	}
	return r.OperationID == op
}

var (
	// ErrNotConfigured is a process without a channel: EXTERNAL_CHANNEL_NOT_CONFIGURED.
	ErrNotConfigured = errors.New("channel: no external channel is configured")
	// ErrUnknown is a send whose answer did not arrive after the request may have reached the
	// provider (EXTERNAL_RESULT_UNKNOWN until reconciled).
	ErrUnknown = errors.New("channel: the outcome of the send is unknown")
	// ErrUnavailable is a channel that cannot answer now; the request did not leave, or a lookup
	// failed. Retryable.
	ErrUnavailable = errors.New("channel: unavailable")
)

// CodeNotConfigured is the rejection of Unconnected (error catalog EXTERNAL_CHANNEL_NOT_CONFIGURED,
// 422, not retryable; TRD's dispatch target reports the same code).
const CodeNotConfigured = trdapi.CodeExternalChannelNotConfigured

func notConfigured(name string) error {
	return errors.Join(ErrNotConfigured,
		kernel.RejectAs(CodeNotConfigured, kernel.CategoryInvalid, "/channel", "No "+name+" channel is configured; the send is a manual node"))
}

// Unconnected is a channel that is not configured: every call is refused, never a fake success
// (Q01 UnconnectedChannel). Name says which channel (default "external").
type Unconnected struct{ Name string }

var _ Port = Unconnected{}

func (u Unconnected) name() string {
	if u.Name == "" {
		return "external"
	}
	return u.Name
}

// Send refuses: nothing is sent.
func (u Unconnected) Send(context.Context, Message) (Receipt, error) {
	return Receipt{}, notConfigured(u.name())
}

// Lookup refuses: an unconnected channel knows nothing.
func (u Unconnected) Lookup(context.Context, string) (Receipt, error) {
	return Receipt{}, notConfigured(u.name())
}

// OperationOf is the stable operation ID of sending content to recipient over channel for a task
// in scope: the same send is always the same operation, so it is never sent twice under two IDs.
func OperationOf(scope kernel.Scope, taskID, channel string, c Content, r Recipient) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "brant.channel.send\x1f%s\x1f%s\x1f%s\x1f%s\x1f%s\x1f%s\x1f%d\x1f%s\x1f%s\x1f%s", scope.Tenant,
		scope.Company, taskID, channel, c.Kind, c.ID, c.Version, c.SHA256, r.PartyID, r.SnapshotSHA256))
	return "send-" + hex.EncodeToString(sum[:20])
}
