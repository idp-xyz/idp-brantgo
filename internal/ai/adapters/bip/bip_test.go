package bip

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"go.idp.xyz/idp-brant/internal/ai/channel"
	tskapi "go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

var scope = kernel.Scope{Tenant: "tenant.1", Company: "company.A"}

func as(t *testing.T, kind kernel.ActorKind, id string) context.Context {
	t.Helper()
	ctx, err := identity.WithPrincipal(context.Background(), identity.Principal{Actor: kernel.ActorRef{Kind: kind, ID: id}, Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func authorization(task string) channel.Authorize {
	return channel.Authorize{TaskID: task, Channel: Name,
		Content:   channel.Content{Kind: "quote_document", ID: "quote.1", Version: 1, SHA256: strings.Repeat("a", 64)},
		Recipient: channel.Recipient{PartyID: "customer.1", SnapshotSHA256: strings.Repeat("b", 64), Address: "buyer@example.invalid"}}
}

var evidence = channel.Evidence{Kind: "evd_source", ID: "src.sent.1", Version: 1}

func code(err error) string {
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code
	}
	return ""
}

func kinds(op channel.Operation) string {
	var out []string
	for _, r := range op.Records {
		out = append(out, string(r.Kind)+":"+r.Outcome)
	}
	return strings.Join(out, " ")
}

// IG-11, baseline 8.2 and 18.2 ("没有连接器则使用显式人工发送节点，不能返回虚假发送成功"): BIP is not connected,
// so a send becomes a manual send node (EXTERNAL_CHANNEL_NOT_CONFIGURED); the person who sends it
// themselves confirms it with evidence; the operation is manually sent — no provider receipt, no
// delivery, and the confirmation is its own record. The task waits for the operation (task.wait
// external, awaiting external_send) and resumes on its arrival.
func TestUnconnectedBIPMakesAManualSendNode(t *testing.T) {
	s := channel.Sender{Channel: New(), Store: &channel.MemoryStore{}}
	clerk, sys := as(t, kernel.ActorHuman, "clerk.1"), as(t, kernel.ActorSystem, "system:ai.runner")
	op, err := s.Authorize(clerk, authorization("task.1"))
	if err != nil || op.State != channel.Authorized || op.Version != 1 || !strings.HasPrefix(op.OperationID, "send-") {
		t.Fatalf("authorize %+v %v", op, err)
	}
	if again, err := s.Authorize(clerk, authorization("task.1")); err != nil || again.OperationID != op.OperationID || again.Version != 1 {
		t.Fatalf("the same authorization is the same operation: %+v %v", again, err)
	}
	op, err = s.Send(sys, op.OperationID)
	if err != nil || op.State != channel.ManualRequired {
		t.Fatalf("send %+v %v", op, err)
	}
	if last := op.Records[len(op.Records)-1]; last.Outcome != channel.OutcomeNotConfigured || last.Code != channel.CodeNotConfigured {
		t.Fatalf("records %s", kinds(op))
	}
	if aw := op.Awaiting(); aw.Kind != channel.ArrivalExternalSend || aw.ID != op.OperationID {
		t.Fatalf("awaiting %+v", aw)
	}
	wait := tskapi.WaitTask{IdempotencyKey: "k.1", TaskID: "task.1", ExpectedVersion: 3, Wait: tskapi.WaitExternal, Reason: "manual send node",
		ResponsibleID: "clerk.1", Awaiting: ptr(op.Awaiting()), DueAt: "2026-11-20T00:00:00Z"}
	if err = wait.Validate(); err != nil {
		t.Fatalf("TSK refuses the wait: %v", err)
	}
	// Sending again leaves the manual node as it is: nothing goes to a channel.
	if again, err := s.Send(sys, op.OperationID); err != nil || again.State != channel.ManualRequired || again.Version != op.Version {
		t.Fatalf("send again %+v %v", again, err)
	}
	// The AI agent never confirms; a confirmation needs means and evidence.
	if _, err = s.ConfirmManualSend(as(t, kernel.ActorAI, "agent.q01"), op.OperationID, channel.ManualSend{Means: "mail", Evidence: evidence}); code(err) != kernel.CodePermissionDenied {
		t.Fatalf("AI confirmation: %v", err)
	}
	if _, err = s.ConfirmManualSend(clerk, op.OperationID, channel.ManualSend{Means: "mail"}); code(err) != kernel.CodeSchemaInvalid {
		t.Fatalf("no evidence: %v", err)
	}
	m := channel.ManualSend{Means: "sent from the clerk's own mailbox", Evidence: evidence}
	op, err = s.ConfirmManualSend(clerk, op.OperationID, m)
	if err != nil || op.State != channel.ManuallySent || !op.State.Sent() || op.ProviderAccepted() || op.Delivered() ||
		len(op.ProviderReceipts()) != 0 || len(op.ManualConfirmations()) != 1 || op.ManualConfirmations()[0].Actor.ID != "clerk.1" {
		t.Fatalf("confirmed %+v %v", op, err)
	}
	if got := kinds(op); got != "send_authorization:authorized submission:submitted submission:not_configured manual_send_confirmation:sent" {
		t.Fatalf("records %s", got)
	}
	if again, err := s.ConfirmManualSend(clerk, op.OperationID, m); err != nil || again.Version != op.Version {
		t.Fatalf("the same confirmation again %+v %v", again, err)
	}
	if _, err = s.ConfirmManualSend(clerk, op.OperationID, channel.ManualSend{Means: "fax", Evidence: evidence}); code(err) != kernel.CodeInvalidState {
		t.Fatalf("another confirmation: %v", err)
	}
	if arr := op.Arrival(); arr.Kind != channel.ArrivalExternalSend || arr.Version != int64(len(op.Records)) {
		t.Fatalf("arrival %+v", arr)
	}
	// Reconciling a manual send asks no provider and changes nothing.
	if again, err := s.Reconcile(sys, op.OperationID); err != nil || again.Version != op.Version {
		t.Fatalf("reconcile %+v %v", again, err)
	}
}

func ptr[T any](v T) *T { return &v }

// A connected provider (the fake): the send goes once and the provider's receipt is recorded; its
// later delivery report is a separate receipt; a rejection is final for the operation. The AI agent
// neither authorizes nor sends (D-7).
func TestProviderReceiptsAreTheProvidersOwn(t *testing.T) {
	f := &Fake{}
	s := channel.Sender{Channel: f, Store: &channel.MemoryStore{}}
	clerk, sys, agent := as(t, kernel.ActorHuman, "clerk.1"), as(t, kernel.ActorSystem, "system:ai.runner"), as(t, kernel.ActorAI, "agent.q01")
	if _, err := s.Authorize(agent, authorization("task.1")); code(err) != kernel.CodePermissionDenied {
		t.Fatalf("AI authorization: %v", err)
	}
	op, err := s.Authorize(clerk, authorization("task.1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Send(agent, op.OperationID); code(err) != kernel.CodePermissionDenied {
		t.Fatalf("AI send: %v", err)
	}
	op, err = s.Send(sys, op.OperationID)
	if err != nil || op.State != channel.ProviderAccepted || !op.ProviderAccepted() || op.Delivered() || len(op.ProviderReceipts()) != 1 ||
		op.ProviderReceipts()[0].ProviderRef == "" || len(op.ManualConfirmations()) != 0 {
		t.Fatalf("accepted %+v %v", op, err)
	}
	if again, err := s.Send(sys, op.OperationID); err != nil || again.Version != op.Version {
		t.Fatalf("send again %+v %v", again, err)
	}
	f.Deliver(op.OperationID)
	op, err = s.Reconcile(sys, op.OperationID)
	if err != nil || !op.Delivered() || len(op.ProviderReceipts()) != 2 {
		t.Fatalf("delivery report %s %v", kinds(op), err)
	}
	if again, err := s.Reconcile(sys, op.OperationID); err != nil || again.Version != op.Version {
		t.Fatalf("nothing more to learn %+v %v", again, err)
	}
	// A bounce of another accepted message is the provider's own later receipt; its acceptance stands
	// and nothing is delivered.
	ob, _ := s.Authorize(clerk, authorization("task.3"))
	if ob, err = s.Send(sys, ob.OperationID); err != nil {
		t.Fatal(err)
	}
	f.Bounce(ob.OperationID)
	ob, err = s.Reconcile(sys, ob.OperationID)
	if err != nil || ob.State != channel.ProviderAccepted || ob.Delivered() || len(ob.ProviderReceipts()) != 2 ||
		ob.ProviderReceipts()[1].Outcome != string(channel.Rejected) {
		t.Fatalf("bounce %s %v", kinds(ob), err)
	}
	if again, err := s.Reconcile(sys, ob.OperationID); err != nil || again.Version != ob.Version {
		t.Fatalf("the bounce recorded twice: %s %v", kinds(again), err)
	}
	f.Script = []Behaviour{Reject}
	a2 := authorization("task.2")
	op2, _ := s.Authorize(clerk, a2)
	op2, err = s.Send(sys, op2.OperationID)
	if err != nil || op2.State != channel.ProviderRejected || op2.State.Sent() || !op2.State.Settled() || op2.ProviderReceipts()[0].Code == "" {
		t.Fatalf("rejected %+v %v", op2, err)
	}
	if msgs, sends := f.Held(); msgs != 3 || sends != 3 {
		t.Fatalf("provider holds %d messages from %d sends", msgs, sends)
	}
}

// Unknown reconciliation (IG-11 unknown 对账; baseline 17.2 超时进入 unknown 并查询／人工对账): a send
// whose answer is lost is unknown (EXTERNAL_RESULT_UNKNOWN), cannot be sent again before it is
// reconciled, and the provider's lookup under the same operation ID finds it accepted — one message
// at the provider. A request lost on the way is not found by the lookup, so the operation is
// authorized again and goes under the same ID, still one message. An unreachable provider leaves
// the operation authorized: nothing went.
func TestUnknownSendsAreReconciledByLookup(t *testing.T) {
	f := &Fake{Script: []Behaviour{LoseAnswer, LoseRequest, Accept, Unreachable, Accept}}
	s := channel.Sender{Channel: f, Store: &channel.MemoryStore{}}
	clerk, sys := as(t, kernel.ActorHuman, "clerk.1"), as(t, kernel.ActorSystem, "system:ai.runner")

	op, _ := s.Authorize(clerk, authorization("task.1"))
	op, err := s.Send(sys, op.OperationID)
	if !errors.Is(err, channel.ErrUnknown) || code(err) != tskapi.CodeExternalResultUnknown || op.State != channel.Unknown {
		t.Fatalf("lost answer %+v %v", op, err)
	}
	if _, err = s.Send(sys, op.OperationID); code(err) != tskapi.CodeExternalResultUnknown {
		t.Fatalf("send before reconciliation: %v", err)
	}
	op, err = s.Reconcile(sys, op.OperationID)
	if err != nil || op.State != channel.ProviderAccepted || len(op.ProviderReceipts()) != 1 {
		t.Fatalf("reconciled %s %v", kinds(op), err)
	}

	op2, _ := s.Authorize(clerk, authorization("task.2"))
	if op2, err = s.Send(sys, op2.OperationID); op2.State != channel.Unknown {
		t.Fatalf("lost request %+v %v", op2, err)
	}
	op2, err = s.Reconcile(sys, op2.OperationID)
	if err != nil || op2.State != channel.Authorized {
		t.Fatalf("not received %s %v", kinds(op2), err)
	}
	if op2, err = s.Send(sys, op2.OperationID); err != nil || op2.State != channel.ProviderAccepted {
		t.Fatalf("sent again %s %v", kinds(op2), err)
	}

	op3, _ := s.Authorize(clerk, authorization("task.3"))
	op3, err = s.Send(sys, op3.OperationID)
	if !errors.Is(err, channel.ErrUnavailable) || op3.State != channel.Authorized {
		t.Fatalf("unreachable %+v %v", op3, err)
	}
	if op3, err = s.Send(sys, op3.OperationID); err != nil || op3.State != channel.ProviderAccepted {
		t.Fatalf("after the outage %+v %v", op3, err)
	}
	if msgs, sends := f.Held(); msgs != 3 || sends != 3 {
		t.Fatalf("provider holds %d messages from %d sends", msgs, sends)
	}
	want := "send_authorization:authorized submission:submitted submission:unknown lookup:not_received submission:submitted provider_receipt:accepted"
	if got := kinds(op2); got != want {
		t.Fatalf("records %s", got)
	}
}

// When the lookup cannot settle an unknown send (the provider cannot be asked), a person reconciles
// it with evidence: sent → reconciled_sent, recorded as a manual reconciliation, not a receipt; not
// sent → authorized again. Only an unknown send is reconciled by hand, only by a person.
func TestAPersonReconcilesWhatTheLookupCannot(t *testing.T) {
	f := &Fake{Script: []Behaviour{LoseAnswer, LoseAnswer}, LookupFails: true}
	verified := []channel.Evidence{}
	s := channel.Sender{Channel: f, Store: &channel.MemoryStore{}, Verify: func(_ context.Context, e channel.Evidence) error {
		if e.ID == "src.missing" {
			return kernel.Reject(kernel.CodeNotFound, "/source_id", "")
		}
		verified = append(verified, e)
		return nil
	}}
	clerk, sys := as(t, kernel.ActorHuman, "clerk.1"), as(t, kernel.ActorSystem, "system:ai.runner")
	op, _ := s.Authorize(clerk, authorization("task.1"))
	op, _ = s.Send(sys, op.OperationID)
	op, err := s.Reconcile(sys, op.OperationID)
	if !errors.Is(err, channel.ErrUnavailable) || op.State != channel.Unknown {
		t.Fatalf("failed lookup %+v %v", op, err)
	}
	if _, err = s.ReconcileManually(sys, op.OperationID, channel.ManualReconciliation{Sent: true, Note: "x", Evidence: evidence}); code(err) != kernel.CodePermissionDenied {
		t.Fatalf("system reconciliation by hand: %v", err)
	}
	if _, err = s.ReconcileManually(clerk, op.OperationID, channel.ManualReconciliation{Sent: true, Note: "the provider confirmed by phone",
		Evidence: channel.Evidence{Kind: "evd_source", ID: "src.missing", Version: 1}}); code(err) != kernel.CodeReferenceInvalid {
		t.Fatalf("unreadable evidence: %v", err)
	}
	op, err = s.ReconcileManually(clerk, op.OperationID, channel.ManualReconciliation{Sent: true, Note: "the provider's statement lists the message",
		Evidence: evidence})
	if err != nil || op.State != channel.ReconciledSent || !op.State.Sent() || op.ProviderAccepted() || len(verified) != 1 {
		t.Fatalf("reconciled sent %s %v", kinds(op), err)
	}
	if _, err = s.ReconcileManually(clerk, op.OperationID, channel.ManualReconciliation{Note: "again", Evidence: evidence}); code(err) != kernel.CodeInvalidState {
		t.Fatalf("a settled send reconciled by hand: %v", err)
	}
	op2, _ := s.Authorize(clerk, authorization("task.2"))
	op2, _ = s.Send(sys, op2.OperationID)
	op2, err = s.ReconcileManually(clerk, op2.OperationID, channel.ManualReconciliation{Sent: false, Note: "the recipient never got it", Evidence: evidence})
	if err != nil || op2.State != channel.Authorized {
		t.Fatalf("reconciled not sent %s %v", kinds(op2), err)
	}
	last := op2.Records[len(op2.Records)-1]
	if last.Kind != channel.RecordManualRecon || last.Outcome != channel.OutcomeNotSent || last.Actor.ID != "clerk.1" || last.Evidence == nil {
		t.Fatalf("record %+v", last)
	}
}

// Of two concurrent sends of one operation, one reaches the channel; the other is refused
// (VERSION_CONFLICT) or finds the operation settled.
func TestConcurrentSendsReachTheChannelOnce(t *testing.T) {
	for round := range 20 {
		f := &Fake{}
		s := channel.Sender{Channel: f, Store: &channel.MemoryStore{}}
		clerk, sys := as(t, kernel.ActorHuman, "clerk.1"), as(t, kernel.ActorSystem, "system:ai.runner")
		op, _ := s.Authorize(clerk, authorization("task.1"))
		var wg sync.WaitGroup
		errs := make([]error, 8)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = s.Send(sys, op.OperationID)
			}()
		}
		wg.Wait()
		if _, sends := f.Held(); sends != 1 {
			t.Fatalf("round %d: %d sends reached the provider", round, sends)
		}
		for _, err := range errs {
			if err != nil && code(err) != kernel.CodeVersionConflict && code(err) != tskapi.CodeExternalResultUnknown {
				t.Fatalf("round %d: %v", round, err)
			}
		}
		if got, _ := s.Get(sys, op.OperationID); got.State != channel.ProviderAccepted {
			t.Fatalf("round %d: %s", round, got)
		}
	}
}
