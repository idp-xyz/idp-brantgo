// Package bip is the adapter to BIP, the exchange and channel platform (mail and external exchange;
// WBS S4-05; decision E-1; Context Map externalBoundaries "BIP（交换与渠道）"). No BIP interface,
// environment or failure semantics exist for S4 (IG-11 stays open): New answers the unconnected
// channel, so every send becomes a manual send node (EXTERNAL_CHANNEL_NOT_CONFIGURED), never a fake
// success.
//
// Fake is a synthetic provider with the semantics Brant's send node must survive — an idempotency
// key per operation, a lost answer after the provider took the message, a request lost on the way,
// a provider that cannot be reached, a lookup by operation ID, a later delivery report — so the
// unknown → lookup / manual reconciliation paths are tested now (internal/ai/channel). It is not a
// model of BIP: when BIP's interface is known, its adapter replaces New and these semantics are
// checked against it.
package bip

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.idp.xyz/idp-brant/internal/ai/channel"
)

// Name is the channel name of BIP.
const Name = "bip"

// New returns BIP's channel: unconnected until BIP's interface exists.
func New() channel.Port { return channel.Unconnected{Name: Name} }

// Behaviour is what Fake does with the next send.
type Behaviour int

const (
	// Accept takes the message and answers its receipt.
	Accept Behaviour = iota
	// Reject refuses the message.
	Reject
	// LoseAnswer takes the message and loses the answer (the caller learns nothing).
	LoseAnswer
	// LoseRequest loses the request on the way: the provider has nothing, the caller learns nothing.
	LoseRequest
	// Unreachable cannot be reached: nothing leaves (channel.ErrUnavailable).
	Unreachable
)

// Fake is a synthetic channel provider (tests).
type Fake struct {
	mu sync.Mutex
	// Script is consumed one behaviour per send; Accept when empty.
	Script []Behaviour
	// LookupFails makes lookups unavailable.
	LookupFails bool
	messages    map[string]*fakeMessage
	sends       int
}

type fakeMessage struct {
	msg       channel.Message
	status    channel.ReceiptStatus
	ref       string
	takenAt   time.Time
	delivered bool
}

var _ channel.Port = (*Fake)(nil)

// Send applies the next behaviour. A repeat of an operation the provider holds answers its first
// receipt and stores nothing new (idempotency key = operation ID).
func (f *Fake) Send(_ context.Context, m channel.Message) (channel.Receipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.messages == nil {
		f.messages = map[string]*fakeMessage{}
	}
	b := Accept
	if len(f.Script) > 0 {
		b, f.Script = f.Script[0], f.Script[1:]
	}
	switch b {
	case Unreachable:
		return channel.Receipt{}, fmt.Errorf("%w: fake provider unreachable", channel.ErrUnavailable)
	case LoseRequest:
		return channel.Receipt{}, fmt.Errorf("%w: fake provider: request lost", channel.ErrUnknown)
	}
	f.sends++
	held, ok := f.messages[m.OperationID]
	if !ok {
		held = &fakeMessage{msg: m, status: channel.Accepted, ref: fmt.Sprintf("fake-msg-%d", len(f.messages)+1), takenAt: time.Now().UTC()}
		if b == Reject {
			held.status, held.ref = channel.Rejected, ""
		}
		f.messages[m.OperationID] = held
	}
	if b == LoseAnswer {
		return channel.Receipt{}, fmt.Errorf("%w: fake provider: answer lost", channel.ErrUnknown)
	}
	return f.receipt(m.OperationID, held), nil
}

func (f *Fake) receipt(op string, h *fakeMessage) channel.Receipt {
	rc := channel.Receipt{OperationID: op, Status: h.status, ProviderRef: h.ref, At: h.takenAt}
	if h.status == channel.Rejected {
		rc.Code = "recipient_refused"
	}
	if h.delivered {
		rc.Status = channel.Delivered
	}
	return rc
}

// Lookup answers what the provider holds under the operation ID.
func (f *Fake) Lookup(_ context.Context, op string) (channel.Receipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.LookupFails {
		return channel.Receipt{}, fmt.Errorf("%w: fake provider lookup failed", channel.ErrUnavailable)
	}
	h, ok := f.messages[op]
	if !ok {
		return channel.Receipt{OperationID: op, Status: channel.NotReceived, At: time.Now().UTC()}, nil
	}
	return f.receipt(op, h), nil
}

// Deliver records the recipient system's delivery of an accepted message (a later report).
func (f *Fake) Deliver(op string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if h, ok := f.messages[op]; ok && h.status == channel.Accepted {
		h.delivered = true
	}
}

// Bounce records the provider's later refusal of an accepted message (a bounce).
func (f *Fake) Bounce(op string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if h, ok := f.messages[op]; ok && h.status == channel.Accepted && !h.delivered {
		h.status = channel.Rejected
	}
}

// Held reports how many distinct messages the provider holds and how many sends reached it.
func (f *Fake) Held() (messages, sends int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.messages), f.sends
}
