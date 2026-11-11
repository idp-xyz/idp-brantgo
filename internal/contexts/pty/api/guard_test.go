package api

import (
	"errors"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// guardRow scans values into the destinations of a guard's SQL: the known flag first, then nullable
// strings and integers (nil leaves NULL).
func guardRow(values ...any) func(dest ...any) error {
	return func(dest ...any) error {
		if len(dest) != len(values) {
			return errors.New("one destination per column")
		}
		*dest[0].(*bool) = values[0].(bool)
		for i := 1; i < len(values); i++ {
			switch v := values[i].(type) {
			case string:
				*dest[i].(**string) = &v
			case int64:
				*dest[i].(**int64) = &v
			}
		}
		return nil
	}
}

func TestTradingProfileGuardVerdict(t *testing.T) {
	ref := TradingProfileRef{PartyID: "customer", Revision: 2}
	if args := TradingProfileGuardArgs(ref); len(args) != 2 || args[0] != "customer" || args[1] != int64(2) {
		t.Fatalf("args %v", args)
	}
	for _, c := range []struct {
		scan  func(dest ...any) error
		want  string
		stale string
	}{
		{guardRow(true, StatusActive, int64(1), "hash", nil, nil), "OK", ""},
		{guardRow(true, StatusSuperseded, int64(1), "hash", CodeReferenceStale, StaleSuperseded), "REFERENCE_STALE /args/trading_profile", StaleSuperseded},
		{guardRow(true, StatusActive, int64(1), "hash", CodeReferenceStale, StalePartyUnavailable), "REFERENCE_STALE /args/trading_profile", StalePartyUnavailable},
		{guardRow(true, StatusActive, int64(1), "hash", CodeReferenceStale, StalePartyChanged), "REFERENCE_STALE /args/trading_profile", StalePartyChanged},
		{guardRow(true, StatusActive, int64(1), "hash", CodeReferenceStale, StaleContactChanged), "REFERENCE_STALE /args/trading_profile", StaleContactChanged},
		{guardRow(false, nil, nil, nil, kernel.CodeReferenceInvalid, nil), "REFERENCE_INVALID /args/trading_profile", ""},
	} {
		v, err := ScanTradingProfileGuard(ref, c.scan)
		if err != nil {
			t.Fatal(err)
		}
		err = v.Check("/args/trading_profile")
		if got := code(err); got != c.want || v.Current() != (c.want == "OK") || v.Ref != ref || v.Stale != c.stale {
			t.Errorf("%+v: %s, want %s", v, got, c.want)
		}
		if rej, ok := kernel.AsRejection(err); ok && rej.Code == CodeReferenceStale &&
			(rej.Category != kernel.CategoryConflict || rej.Fields[0].Reason != "Reference is "+c.stale) {
			t.Errorf("stale rejection %+v", rej)
		}
		if c.want == "OK" && (v.Status != StatusActive || v.PartyRevision != 1 || v.ContentHash != "hash") {
			t.Errorf("facts %+v", v)
		}
	}
	if (TradingProfileGuardVerdict{}).Check("/x") == nil {
		t.Fatal("a verdict the guard did not give is REFERENCE_INVALID")
	}
	broken := errors.New("connection lost")
	if _, err := ScanTradingProfileGuard(ref, func(...any) error { return broken }); !errors.Is(err, broken) {
		t.Fatal(err)
	}
}

func TestAddressRoleGuardVerdict(t *testing.T) {
	ref := AddressRoleRef{PartyID: "customer", PartyRevision: 1, AddressID: "hq"}
	if args := AddressRoleGuardArgs(ref); len(args) != 3 || args[0] != "customer" || args[1] != int64(1) || args[2] != "hq" {
		t.Fatalf("args %v", args)
	}
	for _, c := range []struct {
		scan  func(dest ...any) error
		want  string
		stale string
	}{
		{guardRow(true, AddressBuyer, StatusConfirmed, int64(1), nil, nil), "OK", ""},
		{guardRow(true, AddressBuyer, StatusDraft, int64(1), CodeReferenceStale, StalePartyUnavailable), "REFERENCE_STALE /args/buyer_address", StalePartyUnavailable},
		{guardRow(true, AddressBuyer, StatusConfirmed, int64(2), CodeReferenceStale, StalePartyRevised), "REFERENCE_STALE /args/buyer_address", StalePartyRevised},
		{guardRow(false, nil, nil, nil, kernel.CodeReferenceInvalid, nil), "REFERENCE_INVALID /args/buyer_address", ""},
	} {
		v, err := ScanAddressRoleGuard(ref, c.scan)
		if err != nil {
			t.Fatal(err)
		}
		if got := code(v.Check("/args/buyer_address")); got != c.want || v.Current() != (c.want == "OK") || v.Ref != ref || v.Stale != c.stale {
			t.Errorf("%+v: %s, want %s", v, got, c.want)
		}
		if c.want == "OK" && (v.Role != AddressBuyer || v.PartyStatus != StatusConfirmed || v.CurrentRevision != 1) {
			t.Errorf("facts %+v", v)
		}
	}
	if (AddressRoleGuardVerdict{}).Check("/x") == nil {
		t.Fatal("a verdict the guard did not give is REFERENCE_INVALID")
	}
	broken := errors.New("connection lost")
	if _, err := ScanAddressRoleGuard(ref, func(...any) error { return broken }); !errors.Is(err, broken) {
		t.Fatal(err)
	}
}
