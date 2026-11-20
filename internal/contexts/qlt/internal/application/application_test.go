package application

import (
	"context"
	"errors"
	"maps"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/qlt/api"
	"go.idp.xyz/idp-brant/internal/contexts/qlt/internal/domain/inspection"
	"go.idp.xyz/idp-brant/internal/contexts/qlt/internal/domain/release"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The api declares exactly the codes the rules give and DEPENDENCY_NOT_SUPPORTED for an unwired
// port, with the same categories.
func TestAPICodesAreTheRulesCodes(t *testing.T) {
	rules := maps.Clone(inspection.Codes)
	maps.Copy(rules, release.Codes)
	rules[api.CodeDependencyNotSupported] = kernel.CategoryInvalid
	if !maps.Equal(rules, api.Codes()) {
		t.Fatalf("rules %v\napi %v", rules, api.Codes())
	}
	for _, c := range []string{api.CodeQualityReleaseExceedsConforming, api.CodeQualityReleaseRequired} {
		if _, ok := release.Codes[c]; !ok {
			t.Errorf("%s", c)
		}
	}
	if release.CodeQualityReleaseRequired != api.CodeQualityReleaseRequired || release.StatusRevoked != api.StatusRevoked ||
		release.StateExpired != api.StateExpired || inspection.StatusSuperseded != api.StatusSuperseded ||
		inspection.InspectorAgency != api.InspectorAgency || inspection.ActionConcession != api.DispositionConcession {
		t.Fatal("constants")
	}
}

// The tactical model's humanOnlyCommands name disposition.decide, quality_release.grant and
// quality_release.revoke (F-06: AI or system → PERMISSION_DENIED); SM-QLT-INSPECTION lets a human or
// an AI record an inspection, never a system actor.
func TestHumanOnlyCommands(t *testing.T) {
	policy, err := authorization.NewPolicy(Rules()...)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{api.CommandDecideDisposition, api.CommandGrantRelease, api.CommandRevokeRelease} {
		r, err := policy.Rule(c)
		if err != nil || len(r.ActorKinds) != 1 || r.ActorKinds[0] != kernel.ActorHuman || r.Permission != c {
			t.Errorf("%s: %+v, %v", c, r, err)
		}
	}
	r, err := policy.Rule(api.CommandRecordInspection)
	if err != nil || !r.Admits(kernel.ActorHuman) || !r.Admits(kernel.ActorAI) || r.Admits(kernel.ActorSystem) || r.Permission != api.CommandRecordInspection {
		t.Fatalf("record %+v, %v", r, err)
	}
	for _, c := range []string{api.CommandGetInspection, api.CommandGetRelease, api.CommandGetSubject} {
		if r, err := policy.Rule(c); err != nil || r.Permission != api.PermissionRead {
			t.Errorf("%s: %+v, %v", c, r, err)
		}
	}
}

// The request hash covers the business arguments, not the key or the CAS value.
func TestRequestHashIgnoresKeyAndVersion(t *testing.T) {
	a := api.DecideDisposition{IdempotencyKey: "k1", InspectionID: "i", ExpectedVersion: 1, Reason: "r",
		Items: []api.DispositionItem{{Action: api.DispositionScrap, Quantity: "1"}}}
	b := a
	b.IdempotencyKey, b.ExpectedVersion = "k2", 2
	_, _, x := request(a)
	_, _, y := request(b)
	if requestHash(a) != requestHash(b) || len(requestHash(a)) != 64 {
		t.Fatalf("hash %v %v", x, y)
	}
	g := api.GrantRelease{IdempotencyKey: "k1", ReleaseID: "r"}
	h := g
	h.IdempotencyKey = "k2"
	if requestHash(g) != requestHash(h) {
		t.Fatal("grant hash")
	}
	if name, _, _ := request(nil); name != "" {
		t.Fatal("unknown command")
	}
}

// Until PUR's InspectionSubject v1 is wired, the port answers DEPENDENCY_NOT_SUPPORTED.
func TestUnwiredSubjects(t *testing.T) {
	s := Service{Upstream: Upstream{Subjects: NotWired{}}}
	_, err := s.subjects(context.Background(), []api.SubjectRef{{Kind: api.SubjectCommitmentLine, ID: "c", LineID: "L"}}, "/args/subject")
	if rej, ok := kernel.AsRejection(err); !ok || rej.Code != api.CodeDependencyNotSupported || rej.Fields[0].Path != "/args/subject" {
		t.Fatalf("%v", err)
	}
	if _, err := (NotWired{}).ResolveSubjects(context.Background(), nil); !errors.Is(err, ErrSubjectsNotWired) {
		t.Fatal(err)
	}
	if got, err := s.subjects(context.Background(), nil, "/x"); err != nil || len(got) != 0 {
		t.Fatal("nothing asked")
	}
}

func TestSubjectOf(t *testing.T) {
	s, err := subjectOf(Subject{Ref: api.SubjectRef{Kind: api.SubjectCommitmentLine, ID: "c", LineID: "L"}, Quantity: "120.5", SkuID: "sku"})
	if err != nil || s.Quantity.String() != "120.5" || s.Ref.LineID != "L" || s.SkuID != "sku" {
		t.Fatalf("%+v, %v", s, err)
	}
	if _, err = subjectOf(Subject{Quantity: "0"}); err == nil {
		t.Fatal("PUR's subject without a quantity")
	}
	if rej, ok := kernel.AsRejection(factErr(kernel.Reject(kernel.CodeNotFound, "/", ""), "/args/report_file_id")); !ok || rej.Code != kernel.CodeReferenceInvalid {
		t.Fatal("not found")
	}
	if rej, ok := kernel.AsRejection(factErr(kernel.Reject(kernel.CodePermissionDenied, "/", ""), "/p")); !ok || rej.Code != kernel.CodePermissionDenied {
		t.Fatal("denied")
	}
}

// S5R-D18 (QLT-INV-002): FUL's answers for a release are summed in the release's unit; an answer in
// another unit or that is no quantity is an error, never a smaller frozen quantity.
func TestShippedOnRelease(t *testing.T) {
	r := &release.Release{ID: "rel.1", Subject: inspection.Subject{UnitID: "pcs", UnitRevision: 1}}
	if got, err := shippedOn(r, nil); err != nil || !got.IsZero() {
		t.Fatalf("nothing shipped %s %v", got, err)
	}
	got, err := shippedOn(r, []Consumed{{UnitID: "pcs", UnitRevision: 1, Quantity: "24", Shipments: 1}, {UnitID: "pcs", UnitRevision: 1, Quantity: "0.5"}})
	if err != nil || got.String() != "24.5" {
		t.Fatalf("%s %v", got, err)
	}
	for _, c := range []Consumed{{UnitID: "kg", UnitRevision: 1, Quantity: "1"}, {UnitID: "pcs", UnitRevision: 2, Quantity: "1"},
		{UnitID: "pcs", UnitRevision: 1, Quantity: "x"}} {
		if _, err := shippedOn(r, []Consumed{c}); err == nil {
			t.Errorf("%+v", c)
		}
	}
}

// S5R-D36: the port's verdict on an agency passes PTY's code on at the path, in QLT's category.
func TestAgencyCheck(t *testing.T) {
	if (Agency{InspectionAgency: true}).Check("/p") != nil {
		t.Fatal("serves")
	}
	rej, ok := kernel.AsRejection(Agency{Code: api.CodeReferenceStale, Stale: "superseded"}.Check("/args/inspector/agency"))
	if !ok || rej.Code != api.CodeReferenceStale || rej.Category != kernel.CategoryConflict || rej.Fields[0].Path != "/args/inspector/agency" {
		t.Fatalf("%+v", rej)
	}
	if rej, ok := kernel.AsRejection(Agency{Code: kernel.CodeReferenceInvalid}.Check("/p")); !ok || rej.Code != kernel.CodeReferenceInvalid {
		t.Fatal("another code")
	}
	if (Upstream{Subjects: NotWired{}, Agencies: noAgencies{}}).Complete() {
		t.Fatal("complete without FUL's ReleaseConsumption and EVD")
	}
}

type noAgencies struct{}

func (noAgencies) ResolveAgencies(context.Context, []api.AgencyRef, time.Time) (map[api.AgencyRef]Agency, error) {
	return nil, nil
}
