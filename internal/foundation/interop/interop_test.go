package interop

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func TestOperationIDIsStable(t *testing.T) {
	scope := kernel.Scope{Tenant: "tenant.a", Company: "company.a"}
	a := OperationIDFor(scope, ChannelCustomsDeclaration, "dcl.1", 1)
	b := OperationIDFor(scope, ChannelCustomsDeclaration, "dcl.1", 1)
	c := OperationIDFor(scope, ChannelCustomsDeclaration, "dcl.1", 2)
	if a != b || a == c || !kernel.ValidIdentifier(string(a)) {
		t.Fatalf("ids %s %s %s", a, b, c)
	}
	if OperationIDFor(scope, ChannelTaxRefund, "dcl.1", 1) == a {
		t.Fatal("channel is part of the id")
	}
}

func TestUnconnectedNeverSucceeds(t *testing.T) {
	var u Unconnected
	res, err := u.Submit(context.Background(), Submission{ID: "op-1"})
	if !errors.Is(err, ErrNotConfigured) || res != (Result{}) {
		t.Fatalf("submit %+v %v", res, err)
	}
	res, err = u.Lookup(context.Background(), "op-1")
	if !errors.Is(err, ErrNotConfigured) || res.State == Succeeded {
		t.Fatalf("lookup %+v %v", res, err)
	}
}

func TestFakeTimeoutIsNotSuccess(t *testing.T) {
	op := Submission{ID: "op-held", Channel: ChannelCustomsDeclaration, Subject: "dcl.1"}
	f := &Fake{Script: []Step{{Behaviour: BehaviourTimeoutHeld, Outcome: "accepted", Sequence: 2, StatusCode: "A",
		OriginalText: "held", MappingVersion: "map.v1", DeclarationNumber: "SYN-1"}}}
	res, err := f.Submit(context.Background(), op)
	if !errors.Is(err, ErrUnknown) || res != (Result{}) || res.State == Succeeded {
		t.Fatalf("timeout must not be a result: %+v %v", res, err)
	}
	got, err := f.Lookup(context.Background(), op.ID)
	if err != nil || got.State != Succeeded || got.Outcome != "accepted" || got.Sequence != 2 || got.OperationID != op.ID {
		t.Fatalf("lookup %+v %v", got, err)
	}
	again, err := f.Submit(context.Background(), op)
	if err != nil || again != got || f.Held() != 1 || f.Submits != 1 {
		t.Fatalf("repeat %+v %v held %d submits %d", again, err, f.Held(), f.Submits)
	}
}

func TestFakeTimeoutLostIsNotFound(t *testing.T) {
	op := Submission{ID: "op-lost", Channel: ChannelTaxRefund, Subject: "case.1"}
	f := &Fake{Script: []Step{{Behaviour: BehaviourTimeoutLost}, {Behaviour: BehaviourAccept, Outcome: "approved"}}}
	if _, err := f.Submit(context.Background(), op); !errors.Is(err, ErrUnknown) {
		t.Fatal(err)
	}
	if _, err := f.Lookup(context.Background(), op.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("lookup %v", err)
	}
	if _, err := f.Submit(context.Background(), op); !errors.Is(err, ErrUnknown) || f.Held() != 0 {
		t.Fatalf("repeat consumed the next step: held %d", f.Held())
	}
}

func TestFakeUnavailableStoresNothing(t *testing.T) {
	f := &Fake{Script: []Step{{Behaviour: BehaviourUnavailable}}, LookupUnavailable: true}
	op := Submission{ID: "op-down"}
	res, err := f.Submit(context.Background(), op)
	if !errors.Is(err, ErrUnavailable) || res != (Result{}) || f.Held() != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	f.LookupUnavailable = true
	f.held = map[OperationID]Result{op.ID: {OperationID: op.ID, State: Succeeded}}
	if _, err := f.Lookup(context.Background(), op.ID); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}

func TestFakeRejectIsAuthoritative(t *testing.T) {
	f := &Fake{Now: func() time.Time { return time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) },
		Script: []Step{{Behaviour: BehaviourReject, Outcome: "rejected", Sequence: 4, OriginalText: "no"}}}
	res, err := f.Submit(context.Background(), Submission{ID: "op-no"})
	if err != nil || res.State != Failed || res.Outcome != "rejected" || !res.At.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("%+v %v", res, err)
	}
}
