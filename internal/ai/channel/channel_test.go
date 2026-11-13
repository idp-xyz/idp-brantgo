package channel

import (
	"context"
	"errors"
	"strings"
	"testing"

	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

var scopeA = kernel.Scope{Tenant: "tenant.1", Company: "company.A"}

// Q01 V0.1 q01/ports.py UnconnectedChannel: an unconnected channel refuses send and lookup with
// EXTERNAL_CHANNEL_NOT_CONFIGURED (422, the catalog's class), never a fake success.
func TestUnconnectedChannelRefuses(t *testing.T) {
	u := Unconnected{Name: "bip"}
	_, e1 := u.Send(context.Background(), Message{OperationID: "send-1"})
	_, e2 := u.Lookup(context.Background(), "send-1")
	for _, err := range []error{e1, e2} {
		r, ok := kernel.AsRejection(err)
		if !errors.Is(err, ErrNotConfigured) || !ok || r.Code != "EXTERNAL_CHANNEL_NOT_CONFIGURED" || r.Category != kernel.CategoryInvalid ||
			!strings.Contains(r.Fields[0].Reason, "bip") {
			t.Fatalf("%v", err)
		}
	}
	if _, err := (Unconnected{}).Send(context.Background(), Message{}); !strings.Contains(err.Error(), "EXTERNAL_CHANNEL_NOT_CONFIGURED") {
		t.Fatalf("%v", err)
	}
}

// The operation of a send is stable for the same content to the same recipient over the same channel
// for the same task in the same scope, and different when any of them differs.
func TestOperationOfIsTheSend(t *testing.T) {
	c := Content{Kind: "quote_document", ID: "quote.1", Version: 1, SHA256: strings.Repeat("a", 64)}
	r := Recipient{PartyID: "customer.1", SnapshotSHA256: strings.Repeat("b", 64), Address: "x@example.invalid"}
	base := OperationOf(scopeA, "task.1", "bip", c, r)
	if base != OperationOf(scopeA, "task.1", "bip", c, r) || !strings.HasPrefix(base, "send-") {
		t.Fatal(base)
	}
	c2, r2 := c, r
	c2.Version = 2
	r2.SnapshotSHA256 = strings.Repeat("c", 64)
	for _, other := range []string{OperationOf(scopeA, "task.2", "bip", c, r), OperationOf(scopeA, "task.1", "mail", c, r),
		OperationOf(scopeA, "task.1", "bip", c2, r), OperationOf(scopeA, "task.1", "bip", c, r2),
		OperationOf(kernel.Scope{Tenant: "tenant.1", Company: "company.B"}, "task.1", "bip", c, r)} {
		if other == base {
			t.Fatal("two sends share an operation")
		}
	}
}

// The store refuses a stale write and keeps its own copies.
func TestMemoryStoreIsCompareAndSwap(t *testing.T) {
	m := &MemoryStore{}
	ctx := context.Background()
	op := Operation{OperationID: "send-1", Scope: scopeA}
	op.add(Record{Kind: RecordAuthorization, Evidence: &Evidence{Kind: "evd_source", ID: "s", Version: 1}}, Authorized)
	if err := m.Put(ctx, op, 1); !errors.Is(err, ErrStale) {
		t.Fatalf("put over nothing at version 1: %v", err)
	}
	if err := m.Put(ctx, op, 0); err != nil {
		t.Fatal(err)
	}
	if err := m.Put(ctx, op, 0); !errors.Is(err, ErrStale) {
		t.Fatalf("second create: %v", err)
	}
	got, ok, _ := m.Get(ctx, scopeA, "send-1")
	got.Records[0].Evidence.ID = "changed"
	again, _, _ := m.Get(ctx, scopeA, "send-1")
	if !ok || got.Version != 1 || again.Records[0].Evidence.ID != "s" {
		t.Fatalf("store %+v", again)
	}
	if _, ok, _ = m.Get(ctx, kernel.Scope{Tenant: "tenant.1", Company: "company.B"}, "send-1"); ok {
		t.Fatal("another company's operation")
	}
	if !ProviderAccepted.Sent() || !ManuallySent.Sent() || !ReconciledSent.Sent() || Unknown.Sent() || !ProviderRejected.Settled() || ManualRequired.Settled() {
		t.Fatal("states")
	}
}

// Without a Principal nothing happens; malformed authorizations are refused at their member.
func TestSenderChecksTheCallerAndTheRequest(t *testing.T) {
	s := Sender{Channel: Unconnected{}, Store: &MemoryStore{}}
	if _, err := s.Authorize(context.Background(), Authorize{}); err == nil {
		t.Fatal("no principal")
	}
	if _, err := s.Send(context.Background(), "send-1"); err == nil {
		t.Fatal("no principal")
	}
	if _, err := s.Get(context.Background(), "send-1"); err == nil {
		t.Fatal("no principal")
	}
	for path, a := range map[string]Authorize{
		"/task_id":   {Channel: "bip"},
		"/channel":   {TaskID: "task.1"},
		"/content":   {TaskID: "task.1", Channel: "bip", Content: Content{Kind: "quote_document", ID: "q", Version: 1, SHA256: "short"}},
		"/recipient": {TaskID: "task.1", Channel: "bip", Content: Content{Kind: "quote_document", ID: "q", Version: 1, SHA256: strings.Repeat("a", 64)}},
	} {
		if r, ok := kernel.AsRejection(a.validate()); !ok || r.Fields[0].Path != path {
			t.Fatalf("%s: %v", path, a.validate())
		}
	}
	for _, e := range []Evidence{{}, {Kind: "url", ID: "x", Version: 1}, {Kind: "evd_source", ID: "x"}, {Kind: "evd_file", ID: "x", SHA256: "short"}} {
		if e.valid() {
			t.Fatalf("evidence %+v accepted", e)
		}
	}
}

// FromDispatch sends only what TRD's dispatch read side says is ready: the preflight ready and the
// missing channel the target's only reason; the content is the formal document, the recipient the
// sealed snapshot.
func TestFromDispatchTakesOnlyWhatTRDWouldSend(t *testing.T) {
	sum := func(c string) string { return strings.Repeat(c, 64) }
	p := trdapi.DispatchPreflight{QuoteID: "quote.1", BusinessRevision: 1, BusinessHash: sum("h"), DocumentSHA256: sum("d"), Ready: true}
	tg := trdapi.DispatchTarget{QuoteID: "quote.1", BusinessRevision: 1, BusinessHash: sum("h"), CustomerRef: trdapi.PartyRef{PartyID: "customer.1", Revision: 2},
		Addresses: []trdapi.RecipientAddress{{Role: "buyer", Address: "合成买方地址"}}, RecipientSnapshotSHA256: sum("r"), PreflightReady: true,
		ChannelStatus: trdapi.ChannelNotConfigured, Reasons: []trdapi.Reason{{Check: trdapi.CheckChannel, Code: trdapi.CodeExternalChannelNotConfigured, Path: "/channel"}}}
	c, r, err := FromDispatch(p, tg)
	if err != nil || c != (Content{Kind: ContentQuoteDocument, ID: "quote.1", Version: 1, SHA256: sum("d")}) ||
		r != (Recipient{PartyID: "customer.1", SnapshotSHA256: sum("r"), Address: "合成买方地址"}) {
		t.Fatalf("%+v %+v %v", c, r, err)
	}
	notReady := p
	notReady.Ready, notReady.Reasons = false, []trdapi.Reason{{Check: "validity", Code: "QUOTE_EXPIRED"}}
	held := tg
	held.Reasons = append([]trdapi.Reason{{Check: "risk_hold", Code: "COST_SOURCE_DENIED"}}, tg.Reasons...)
	other := tg
	other.BusinessRevision = 2
	bare := tg
	bare.Addresses = nil
	for name, c := range map[string]struct {
		p    trdapi.DispatchPreflight
		t    trdapi.DispatchTarget
		code string
	}{"not ready": {notReady, tg, kernel.CodeInvalidState}, "held": {p, held, kernel.CodeInvalidState},
		"other revision": {p, other, kernel.CodeReferenceInvalid}, "no address": {p, bare, kernel.CodeReferenceInvalid}} {
		if _, _, err := FromDispatch(c.p, c.t); err == nil || !strings.Contains(err.Error(), c.code) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
