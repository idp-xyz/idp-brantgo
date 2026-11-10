package kernel

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestIdentifierRule(t *testing.T) {
	for _, s := range []string{"a", "tenant.demo", "company_1", "quote:2026-001", "Z9", strings.Repeat("x", 64)} {
		if !ValidIdentifier(s) {
			t.Errorf("%q must be valid", s)
		}
	}
	for _, s := range []string{"", ".a", "-a", "_a", "a b", "a/b", "租户", strings.Repeat("x", 65)} {
		if ValidIdentifier(s) {
			t.Errorf("%q must be invalid", s)
		}
	}
}

func TestScopeAndActor(t *testing.T) {
	s, err := NewScope("tenant.demo", "company.demo")
	if err != nil || s.String() != "tenant.demo/company.demo" {
		t.Fatalf("scope = %v, %v", s, err)
	}
	if _, err = NewScope("tenant.demo", ""); !errors.Is(err, ErrInvalidScope) {
		t.Fatalf("empty company: %v", err)
	}
	a, err := NewActorRef(ActorAI, "agent.quote")
	if err != nil || a.IsHuman() || a.String() != "ai:agent.quote" {
		t.Fatalf("actor = %v, %v", a, err)
	}
	if _, err = NewActorRef("robot", "x"); !errors.Is(err, ErrInvalidActor) {
		t.Fatalf("unknown kind: %v", err)
	}
}

func TestParseScopeReadsStringForm(t *testing.T) {
	s, err := ParseScope("tenant.demo/company:1")
	if err != nil || s != (Scope{Tenant: "tenant.demo", Company: "company:1"}) {
		t.Fatalf("ParseScope = %v, %v", s, err)
	}
	if back, err := ParseScope(s.String()); err != nil || back != s {
		t.Fatalf("round trip = %v, %v", back, err)
	}
	for _, bad := range []string{"", "tenant.demo", "tenant.demo/", "/company.demo", "tenant.demo/company.demo/x",
		"tenant demo/company.demo", "test-scope", " tenant.demo/company.demo", "tenant.demo\\company.demo"} {
		if _, err := ParseScope(bad); !errors.Is(err, ErrInvalidScope) {
			t.Errorf("ParseScope(%q) = %v, want ErrInvalidScope", bad, err)
		}
	}
}

func TestValidityIsHalfOpen(t *testing.T) {
	at := func(h int) time.Time { return time.Date(2026, 10, 22, h, 0, 0, 0, time.UTC) }
	v, err := NewValidityInterval(at(0), at(12))
	if err != nil {
		t.Fatal(err)
	}
	if !v.Contains(at(0)) || !v.Contains(at(11)) || v.Contains(at(12)) {
		t.Fatal("[00:00, 12:00) must contain 00:00 and 11:00 but not 12:00")
	}
	next, _ := NewValidityInterval(at(12), time.Time{})
	if v.Overlaps(next) || !next.Contains(at(23)) {
		t.Fatal("[a, b) and [b, ∞) must not overlap")
	}
	if _, err = NewValidityInterval(at(12), at(12)); !errors.Is(err, ErrInvalidValidity) {
		t.Fatalf("empty interval: %v", err)
	}
}

func TestTimestampsHaveMicrosecondPrecision(t *testing.T) {
	ts, err := ParseTimestamp("2026-10-22T15:55:12.579416Z")
	if err != nil {
		t.Fatal(err)
	}
	if got := FormatTimestamp(ts); got != "2026-10-22T15:55:12.579416Z" {
		t.Fatalf("round trip = %s", got)
	}
	nano := time.Date(2026, 10, 22, 15, 55, 12, 579416999, time.FixedZone("CST", 8*3600))
	if got := FormatTimestamp(nano); got != "2026-10-22T07:55:12.579416Z" {
		t.Fatalf("Instant must truncate to microseconds in UTC: %s", got)
	}
	if ts, err = ParseTimestamp("2026-10-22T15:55:12Z"); err != nil || FormatTimestamp(ts) != "2026-10-22T15:55:12.000000Z" {
		t.Fatalf("whole seconds: %v, %v", ts, err)
	}
	for _, s := range []string{"2026-10-22T15:55:12+08:00", "2026-10-22T15:55:12.1234567Z", "2026-10-22 15:55:12Z", "2026-10-22T15:55:12.Z", "2026-13-22T15:55:12Z", "2026-10-22"} {
		if _, err := ParseTimestamp(s); !errors.Is(err, ErrInvalidTimestamp) {
			t.Errorf("ParseTimestamp(%q) = %v", s, err)
		}
	}
}
