//go:build integration

package bootstrap_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/ai/adapters/bip"
	"go.idp.xyz/idp-brant/internal/ai/adapters/model"
	"go.idp.xyz/idp-brant/internal/ai/channel"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	tskapi "go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The manual send node of a task on PostgreSQL 16 (WBS S4-05; decision E-1; IG-11 外发授权、
// operation_id; baseline 8.2 "未接入自动外发时，任务进入人工发送节点，人工发送确认与服务商回执分别记录"; 18.2
// "没有连接器则使用显式人工发送节点，不能返回虚假发送成功"). An activated quote is what TRD's dispatch read side
// says is ready to send, short of a channel. The clerk authorizes sending exactly that document to
// exactly that recipient snapshot; BIP is not connected, so the send becomes a manual node and the
// task waits for it in TSK (task.wait external, awaiting external_send <operation>). The clerk sends
// it from their own mailbox, registers the sent mail in EVD and confirms the send with it; evidence
// the clerk cannot read is refused. The system resumes the task on the operation's arrival. The
// operation shows a manual send confirmation and no provider receipt; TRD still reports no external
// delivery — nothing claims the customer received it.
func TestManualSendNodeOfATask(t *testing.T) {
	w := newAgentWorld(t, false)
	a := w.activated("quote.1")
	r := w.runner(model.NewScripted("scripted-q01-v1"))
	rec := w.aiTask("task.send.1", tskapi.GoalQuoteReady, tskapi.Target{CustomerID: "customer.1", QuoteID: "quote.1"}, budgetOf(10, 2), bundleRef(r),
		evdapi.CommandRegisterSource)

	p, err := w.c.TRD.DispatchPreflight(w.clerk, trdapi.DispatchQuery{QuoteID: "quote.1", BusinessRevision: 1})
	w.must(err)
	target, err := w.c.TRD.DispatchTarget(w.clerk, trdapi.DispatchQuery{QuoteID: "quote.1", BusinessRevision: 1})
	w.must(err)
	content, recipient, err := channel.FromDispatch(p, target)
	w.must(err)
	if content.SHA256 != a.DocumentSHA256 || recipient.PartyID != "customer.1" {
		t.Fatalf("content %+v recipient %+v", content, recipient)
	}
	verify := func(ctx context.Context, e channel.Evidence) error {
		src, err := w.c.EVD.GetSource(ctx, e.ID)
		if err != nil {
			return err
		}
		if e.SHA256 != "" && e.SHA256 != src.ContentSHA256 {
			return kernel.Reject(kernel.CodeReferenceInvalid, "/evidence", "")
		}
		return nil
	}
	s := channel.Sender{Channel: bip.New(), Store: &channel.MemoryStore{}, Verify: verify}
	if _, err = s.Authorize(w.agent, channel.Authorize{TaskID: "task.send.1", Channel: bip.Name, Content: content, Recipient: recipient}); code(err) != kernel.CodePermissionDenied {
		t.Fatalf("the agent authorized a send: %v", err)
	}
	op, err := s.Authorize(w.clerk, channel.Authorize{TaskID: "task.send.1", Channel: bip.Name, Content: content, Recipient: recipient})
	w.must(err)
	runner := w.actorOf(kernel.ActorSystem, "system:ai.runner")
	op, err = s.Send(runner, op.OperationID)
	if err != nil || op.State != channel.ManualRequired {
		t.Fatalf("send %+v %v", op, err)
	}

	// The task waits for the manual node.
	due := kernel.FormatTimestamp(time.Now().Add(48 * time.Hour))
	waited, err := w.c.TSK.WaitTask(w.sys, tskapi.WaitTask{IdempotencyKey: w.key(), TaskID: "task.send.1", ExpectedVersion: rec.RowVersion,
		Wait: tskapi.WaitExternal, Reason: "人工发送节点：BIP 未接入，正式报价需由业务员发送并确认", ResponsibleID: "clerk.ai",
		Awaiting: ptrTo(op.Awaiting()), DueAt: due})
	w.must(err)
	v := w.taskView("task.send.1")
	if v.Lifecycle != tskapi.StateWaitingExternal || v.Wait == nil || v.Wait.Awaiting == nil || v.Wait.Awaiting.Kind != channel.ArrivalExternalSend ||
		v.Wait.Awaiting.ID != op.OperationID {
		t.Fatalf("task %s %+v", v.Lifecycle, v.Wait)
	}

	// The clerk sent it from their mailbox and registers the sent mail as evidence.
	_, err = w.c.EVD.RegisterSource(w.clerk, evdapi.RegisterSource{IdempotencyKey: w.key(), SourceID: "src.sent.quote.1", SourceKind: evdapi.KindManualText,
		ContentText: "合成：已于本人邮箱发送 " + a.Number + " 至客户采购（附件 SHA-256 " + a.DocumentSHA256[:12] + "…）"})
	w.must(err)
	sent, err := w.c.EVD.GetSource(w.clerk, "src.sent.quote.1")
	w.must(err)
	if _, err = s.ConfirmManualSend(w.clerk, op.OperationID, channel.ManualSend{Means: "业务员本人邮箱",
		Evidence: channel.Evidence{Kind: "evd_source", ID: "src.missing", SHA256: sent.ContentSHA256}}); code(err) != kernel.CodeReferenceInvalid {
		t.Fatalf("evidence that does not exist: %v", err)
	}
	op, err = s.ConfirmManualSend(w.clerk, op.OperationID, channel.ManualSend{Means: "业务员本人邮箱",
		Evidence: channel.Evidence{Kind: "evd_source", ID: "src.sent.quote.1", SHA256: sent.ContentSHA256}})
	w.must(err)
	if op.State != channel.ManuallySent || op.ProviderAccepted() || op.Delivered() || len(op.ProviderReceipts()) != 0 ||
		len(op.ManualConfirmations()) != 1 || op.ManualConfirmations()[0].Actor.ID != "clerk.ai" {
		t.Fatalf("operation %+v", op)
	}

	// The system resumes the task on the operation's arrival.
	_, err = w.c.TSK.ResumeTask(w.sys, tskapi.ResumeTask{IdempotencyKey: w.key(), TaskID: "task.send.1", ExpectedVersion: waited.RowVersion,
		Arrival: ptrTo(op.Arrival())})
	w.must(err)
	if v = w.taskView("task.send.1"); v.Lifecycle != tskapi.StateRunning {
		t.Fatalf("after the arrival: %s", v.Lifecycle)
	}
	// TRD claims no delivery: the channel is still not configured and nothing reports delivery.
	target, err = w.c.TRD.DispatchTarget(w.clerk, trdapi.DispatchQuery{QuoteID: "quote.1", BusinessRevision: 1})
	w.must(err)
	if target.ExternalDelivery != trdapi.ExternalDeliveryNotStarted || target.ChannelStatus != trdapi.ChannelNotConfigured {
		t.Fatalf("TRD's dispatch target %+v", target)
	}
	want := "send_authorization:authorized submission:submitted submission:not_configured manual_send_confirmation:sent"
	var got []string
	for _, r := range op.Records {
		got = append(got, string(r.Kind)+":"+r.Outcome)
	}
	if strings.Join(got, " ") != want {
		t.Fatalf("records %v", got)
	}
}

func ptrTo[T any](v T) *T { return &v }
