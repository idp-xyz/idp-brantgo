package reservation

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func TestReleaseIsNotAnIssue(t *testing.T) {
	r := Reservation{Status: StatusActive, Remaining: kernel.MustDecimal("60"), Quantity: kernel.MustDecimal("60"), RowVersion: 1}
	back, ok := r.Release()
	if !ok || back.Cmp(kernel.MustDecimal("60")) != 0 || r.Status != StatusReleased || r.Remaining.Sign() != 0 || r.RowVersion != 2 {
		t.Fatalf("%+v %s", r, back)
	}
	if _, ok = r.Release(); ok {
		t.Fatal("a released hold released again")
	}
	used := Reservation{Status: StatusConsumed, RowVersion: 2}
	if _, ok = used.Release(); ok {
		t.Fatal("a consumed hold was released")
	}
}

func TestConsumeStopsAtZero(t *testing.T) {
	r := Reservation{Status: StatusActive, Remaining: kernel.MustDecimal("60"), RowVersion: 1}
	if !r.Consume(kernel.MustDecimal("20")) || r.Status != StatusActive || r.Remaining.Cmp(kernel.MustDecimal("40")) != 0 {
		t.Fatalf("partial %+v", r)
	}
	if !r.Consume(kernel.MustDecimal("40")) || r.Status != StatusConsumed || r.Remaining.Sign() != 0 {
		t.Fatalf("final %+v", r)
	}
	if r.Consume(kernel.MustDecimal("1")) {
		t.Fatal("consumed again")
	}
}
