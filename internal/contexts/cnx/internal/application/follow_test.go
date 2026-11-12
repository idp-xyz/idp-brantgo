package application

import (
	"context"
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Unit C4b-CNX (WBS S5-09b; PL-14, S5 D-18, MD-47; WF-06 idempotency "事件 ID 去重；各下游按（订单、
// 订单修订）只开启一次复核"): the keys and the retry rule of the consumer's reopenings.

// The order revision's reopening is keyed by the event ID alone; each check a publication reopens
// by the event ID and a short hash of the check, so one event never reopens a check twice and two
// checks never share a key. A key that would not be an identifier becomes a digest.
func TestFollowKeys(t *testing.T) {
	const id = "evt-0190a1b2-c3d4-7e5f-8a6b-0c1d2e3f4a5b"
	if k := followKey(id, ""); k != id {
		t.Fatalf("order key %q", k)
	}
	a, b := followKey(id, "tcc-1"), followKey(id, "tcc-2")
	if a == b || !strings.HasPrefix(a, id+":") || len(a) != len(id)+17 || !kernel.ValidIdentifier(a) || followKey(id, "tcc-1") != a {
		t.Fatalf("check keys %q %q", a, b)
	}
	long := strings.Repeat("e", 300)
	if k := followKey(long, "tcc-1"); len(k) != 64 || !kernel.ValidIdentifier(k) || k == followKey(long, "tcc-2") {
		t.Fatalf("digest key %q", k)
	}
}

// A refusal a later delivery can pass is retried — the consumer's actor not provisioned yet
// (PERMISSION_DENIED), a concurrent change (VERSION_CONFLICT), a revision changed or superseded
// since it was read (REFERENCE_STALE, CONFIG_NOT_ACTIVE, CONFIG_EXPIRED); any other refusal is for
// good.
func TestFollowRetries(t *testing.T) {
	for _, code := range []string{kernel.CodePermissionDenied, kernel.CodeVersionConflict, api.CodeReferenceStale, polapi.CodeConfigNotActive,
		polapi.CodeConfigExpired} {
		if !retried(code) {
			t.Errorf("%s is not retried", code)
		}
	}
	for _, code := range []string{kernel.CodeInvalidState, kernel.CodeNotFound, kernel.CodeReferenceInvalid, api.CodeOrderNotActive,
		kernel.CodeSchemaInvalid} {
		if retried(code) {
			t.Errorf("%s is retried", code)
		}
	}
}

// Another kind of configuration needs no read and reopens nothing; a screening_regulation revision
// POL does not resolve in force reopens nothing either (a later revision's publication does).
func TestRegulationOutsideItsForce(t *testing.T) {
	s := Service{}
	ctx := context.Background()
	other := PublishedRegulation{Kind: polapi.KindCreditPolicy, Ref: polapi.ConfigRef{Kind: polapi.KindCreditPolicy, ConfigID: "c", Revision: 2}}
	if b, err := s.PrepareRegulation(ctx, other); err != nil || b.Regulation != nil {
		t.Fatalf("another kind read POL: %+v %v", b, err)
	}
	if effect, reopened, err := s.ReopenOnRegulation(ctx, nil, other, RegulationBasis{}); effect != EffectNotScreeningRegulation ||
		reopened != nil || err != nil {
		t.Fatalf("another kind: %s %v %v", effect, reopened, err)
	}
	ref := polapi.ConfigRef{Kind: polapi.KindScreeningRegulation, ConfigID: "screening", Revision: 2}
	e := PublishedRegulation{Kind: polapi.KindScreeningRegulation, Ref: ref}
	reg := &polapi.ScreeningRegulation{RescreenIntervalDays: 30}
	for name, b := range map[string]RegulationBasis{
		"unresolved": {},
		"superseded": {Regulation: &polapi.ResolvedConfig{Ref: ref, Code: polapi.CodeConfigNotActive, Data: polapi.ConfigData{ScreeningRegulation: reg}}},
		"expired":    {Regulation: &polapi.ResolvedConfig{Ref: ref, Code: polapi.CodeConfigExpired, Data: polapi.ConfigData{ScreeningRegulation: reg}}},
		"another":    {Regulation: &polapi.ResolvedConfig{Ref: polapi.ConfigRef{Kind: ref.Kind, ConfigID: "screening", Revision: 3}, Data: polapi.ConfigData{ScreeningRegulation: reg}}},
	} {
		if effect, reopened, err := s.ReopenOnRegulation(ctx, nil, e, b); effect != EffectRegulationNotInForce || reopened != nil || err != nil {
			t.Errorf("%s: %s %v %v", name, effect, reopened, err)
		}
	}
}

// A refusal the consumer cannot cure names the reopening and its object.
func TestFollowMismatchText(t *testing.T) {
	m := &FollowMismatch{Code: FailureReopenRefused, Detail: "trade_compliance.reopen order_amended of order.1: INVALID_STATE"}
	if m.Error() != "cnx: cnx.reopen_refused: trade_compliance.reopen order_amended of order.1: INVALID_STATE" {
		t.Fatal(m.Error())
	}
}

// The reads refuse identifiers that cannot name a regulation, a list or a version before they read.
func TestFollowReadsValidateTheirArguments(t *testing.T) {
	s := Service{}
	ctx := context.Background()
	for name, err := range map[string]error{
		"regulation": func() error { _, err := s.ChecksByRegulation(ctx, "a b", 0); return err }(),
		"long regulation": func() error {
			_, err := s.ChecksByRegulation(ctx, strings.Repeat("r", api.MaxRegulationIDLen+1), 0)
			return err
		}(),
		"negative revision": func() error { _, err := s.ChecksByRegulation(ctx, "screening", -1); return err }(),
		"list":              func() error { _, err := s.ChecksByList(ctx, "a b", ""); return err }(),
		"long list": func() error {
			_, err := s.ChecksByList(ctx, strings.Repeat("l", api.MaxListCodeLength+1), "")
			return err
		}(),
		"version": func() error { _, err := s.ChecksByList(ctx, "cn.embargo", "v 1"); return err }(),
	} {
		if rej, ok := kernel.AsRejection(err); !ok || rej.Code != kernel.CodeSchemaInvalid {
			t.Errorf("%s: %v", name, err)
		}
	}
}
