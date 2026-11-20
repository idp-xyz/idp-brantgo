package capability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

func grantSpec(command string) Spec {
	return Spec{Command: command, Context: "xx", Contract: "brant.x/0.1.0", Version: 1, SideEffect: Submit, Risk: RiskHigh,
		Approval: ApprovalGrant, Summary: "probe"}
}

// A judgement belongs to a capability that needs an approval grant and takes the capability's input
// type (S4 G7b): any other pairing is a definition error, and a Define error passes through.
func TestJudgedIsForAGrantCapabilityOfItsInputType(t *testing.T) {
	ok := func(context.Context, probe) error { return nil }
	grant := probeCapability(t, grantSpec("probe.prepare"))
	judged, err := Judged(grant, nil, ok)
	if err != nil || !judged.Judges() || grant.Judges() {
		t.Fatalf("judged %v, %v; the original judges %v", judged.Judges(), err, grant.Judges())
	}
	if _, err = Judged(probeCapability(t, draftSpec("probe.save_draft")), nil, ok); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a capability that needs no grant: %v", err)
	}
	if _, err = Judged(grant, nil, func(context.Context, trdapi.SaveDraft) error { return nil }); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a judgement of another type: %v", err)
	}
	if _, err = Judged[probe](grant, nil, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no judgement: %v", err)
	}
	failed := errors.New("define failed")
	if _, err = Judged(Capability{}, failed, ok); !errors.Is(err, failed) {
		t.Fatalf("%v", err)
	}
}

// Judge returns the owning context's rejection as it is, passes what it accepts, passes a capability
// without a judgement, and refuses a value of another type.
func TestJudgeAsksTheOwningContext(t *testing.T) {
	var seen []probe
	refusal := kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	judged, err := Judged(probeCapability(t, grantSpec("probe.prepare")), nil, func(_ context.Context, p probe) error {
		seen = append(seen, p)
		if p.ExpectedVersion != 1 {
			return refusal
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = judged.Judge(context.Background(), probe{ID: "p.1", ExpectedVersion: 1}); err != nil {
		t.Fatalf("an accepted request: %v", err)
	}
	if err = judged.Judge(context.Background(), probe{ID: "p.1", ExpectedVersion: 2}); !errors.Is(err, refusal) {
		t.Fatalf("a refused request: %v", err)
	}
	if len(seen) != 2 || seen[1].ID != "p.1" {
		t.Fatalf("judged %+v", seen)
	}
	if err = judged.Judge(context.Background(), "not a probe"); err == nil || errors.Is(err, refusal) {
		t.Fatalf("a value of another type: %v", err)
	}
	if err = probeCapability(t, draftSpec("probe.save_draft")).Judge(context.Background(), probe{}); err != nil {
		t.Fatalf("a capability without a judgement passes: %v", err)
	}
}

// Arguments is the request as a tool call carries it: the members under their Go names, the
// idempotency key left out; decoded strictly it is the same command and has the same request hash,
// so what a person approves and what is stored are one request.
func TestArgumentsAreTheRequestAsAToolCallCarriesIt(t *testing.T) {
	c := probeCapability(t, grantSpec("probe.prepare"))
	cmd := probe{IdempotencyKey: "k.1", ID: "p.1", ExpectedVersion: 4, Lines: []string{"a", "b"}}
	raw, err := c.Arguments(cmd)
	if err != nil {
		t.Fatal(err)
	}
	var members map[string]json.RawMessage
	if err = json.Unmarshal(raw, &members); err != nil {
		t.Fatal(err)
	}
	if _, ok := members[KeyField]; ok || len(members) != 3 || string(members["ID"]) != `"p.1"` {
		t.Fatalf("arguments %s", raw)
	}
	var back probe
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&back); err != nil {
		t.Fatal(err)
	}
	want, _ := c.RequestHash(cmd)
	got, _ := c.RequestHash(back)
	if got != want || back.IdempotencyKey != "" || !reflect.DeepEqual(back.Lines, cmd.Lines) || back.ExpectedVersion != 4 {
		t.Fatalf("decoded %+v: hash %s, want %s", back, got, want)
	}
	again, _ := c.Arguments(back)
	if !bytes.Equal(raw, again) {
		t.Fatalf("not stable: %s then %s", raw, again)
	}
	if _, err = c.Arguments("not a probe"); err == nil {
		t.Fatal("a value of another type has arguments")
	}
}

type orderChecks struct {
	prepared []trdapi.PrepareOrder
	reviews  []trdapi.RequestOrderReview
	err      error
}

func (o *orderChecks) CheckPrepareOrder(_ context.Context, c trdapi.PrepareOrder) error {
	o.prepared = append(o.prepared, c)
	return o.err
}

func (o *orderChecks) CheckRequestOrderReview(_ context.Context, c trdapi.RequestOrderReview) error {
	o.reviews = append(o.reviews, c)
	return o.err
}

// Q01's two approval-grant commands, and only they, are judged by TRD before a person is asked; the
// judgement is TRD's check of the same command, and without TRD's checks it refuses: a request nobody
// judged is not put to a person.
func TestQ01OrderCommandsAreJudgedByTRD(t *testing.T) {
	checks := &orderChecks{}
	caps, err := Q01(Q01Targets{TRDOrderChecks: checks})
	if err != nil {
		t.Fatal(err)
	}
	judged := map[string]Capability{}
	for _, c := range caps {
		if c.Judges() {
			judged[c.Command] = c
		}
		if c.Judges() != (c.Approval == ApprovalGrant) {
			t.Errorf("%s: judged %v, needs a grant %v", c.Command, c.Judges(), c.Approval == ApprovalGrant)
		}
	}
	if len(judged) != 2 || !judged[trdapi.CommandPrepareOrder].Judges() || !judged[trdapi.CommandRequestOrderReview].Judges() {
		t.Fatalf("judged: %v", judged)
	}
	prepare := trdapi.PrepareOrder{OrderID: "order.1", QuoteID: "quote.1", BusinessRevision: 2, BusinessHash: strings.Repeat("a", 64), AcceptanceID: "acc.1"}
	review := trdapi.RequestOrderReview{OrderID: "order.1", ExpectedVersion: 1, ReviewID: "review.1"}
	checks.err = kernel.Reject(kernel.CodeInvalidKnownValue, "/args/notify_party_ref", "")
	if err = judged[trdapi.CommandPrepareOrder].Judge(context.Background(), prepare); !errors.Is(err, checks.err) {
		t.Fatalf("prepare: %v", err)
	}
	checks.err = nil
	if err = judged[trdapi.CommandRequestOrderReview].Judge(context.Background(), review); err != nil {
		t.Fatalf("review: %v", err)
	}
	if len(checks.prepared) != 1 || checks.prepared[0] != prepare || len(checks.reviews) != 1 || checks.reviews[0] != review {
		t.Fatalf("TRD was asked %+v and %+v", checks.prepared, checks.reviews)
	}

	none, err := Q01(Q01Targets{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range none {
		if !c.Judges() {
			continue
		}
		var cmd any = prepare
		if c.Command == trdapi.CommandRequestOrderReview {
			cmd = review
		}
		if err = c.Judge(context.Background(), cmd); err == nil {
			t.Errorf("%s judged without TRD", c.Command)
		} else if _, ok := kernel.AsRejection(err); ok {
			t.Errorf("%s: a missing target is no rejection of the request: %v", c.Command, err)
		}
	}
}
