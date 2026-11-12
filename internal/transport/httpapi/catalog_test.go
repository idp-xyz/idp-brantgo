package httpapi

import (
	"context"
	"testing"
	"time"

	"go.idp.xyz/idp-bento-go/application"
	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// fakeCAT records the declaration profile commands it receives.
type fakeCAT struct {
	CAT
	saved    catapi.SaveDeclarationProfile
	verified catapi.VerifyDeclarationProfile
}

func (f *fakeCAT) SaveDeclarationProfile(_ context.Context, cmd catapi.SaveDeclarationProfile) (catapi.Receipt, error) {
	f.saved = cmd
	return catapi.Receipt{ActionID: "act-1", ObjectID: cmd.ProfileID, RowVersion: 1, Status: catapi.StatusDraft}, nil
}

func (f *fakeCAT) VerifyDeclarationProfile(_ context.Context, cmd catapi.VerifyDeclarationProfile) (catapi.Receipt, error) {
	f.verified = cmd
	return catapi.Receipt{ActionID: "act-2", ObjectID: cmd.ProfileID, RowVersion: 2, Status: catapi.StatusVerified}, nil
}

func (f *fakeCAT) GetDeclarationProfile(_ context.Context, id string) (catapi.DeclarationProfileView, error) {
	if id != "profile.1" {
		return catapi.DeclarationProfileView{}, kernel.Reject(kernel.CodeNotFound, "/id", "")
	}
	return catapi.DeclarationProfileView{ProfileID: id, RootVersion: 2, VerifiedRevision: 1,
		Key: catapi.DeclarationProfileKey{SkuID: "sku", SpecRevision: 1, CodeTableVersion: "CN-2026"},
		Revisions: []catapi.DeclarationProfileRevision{{Revision: 1, Status: catapi.StatusVerified, ContentHash: "h",
			DeclarationProfile: f.saved.Profile(), SavedBy: kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent.1"},
			VerifiedBy: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "doc.2"}, VerifiedAt: time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC),
			EvidenceIDs: []string{"ruling.1"}}}}, nil
}

func (f *fakeCAT) ListDeclarationProfiles(_ context.Context, skuID string) (catapi.DeclarationProfileList, error) {
	return catapi.DeclarationProfileList{SkuID: skuID, Profiles: []catapi.DeclarationProfileSummary{{ProfileID: "profile.1", RootVersion: 2,
		VerifiedRevision: 1, Key: catapi.DeclarationProfileKey{SkuID: skuID, SpecRevision: 1, CodeTableVersion: "CN-2026"}}}}, nil
}

func (f *fakeCAT) AuditRejected(context.Context, string, string, string) error { return nil }

// The declaration profile commands decode by reflection into catapi's structures: the profile ID
// is "id", the classification parts are nested members, a Go field name is refused; the reads
// return the content of a revision inline.
func TestDeclarationProfileCommandsAndReads(t *testing.T) {
	cat := &fakeCAT{}
	h, err := New(Config{EVD: struct{ EVD }{}, PTY: struct{ PTY }{}, CAT: cat, INQ: &fakeINQ{}, POL: struct{ POL }{}, REF: struct{ REF }{},
		SUP: struct{ SUP }{}, CST: struct{ CST }{}, TRD: struct{ TRD }{}, PUR: struct{ PUR }{}, Verifier: tokens{"Bearer good": alice},
		Clock: application.Clock(fixedClock{})})
	if err != nil {
		t.Fatal(err)
	}
	post := func(command, args string) answer {
		return call(t, h, "POST", "/api/brant/v1/cat/commands/"+command, "Bearer good",
			`{"contract":"`+catapi.Contract+`","command":"`+command+`","idempotency_key":"k1","args":`+args+`}`)
	}
	a := post(catapi.CommandSaveDeclarationProfile, `{"id":"profile.1","expected_version":0,"sku_id":"sku","spec_revision":1,`+
		`"code_table_version":"CN-2026","commodity":{"code":"6109100021","basis":"归类依据","source_ids":["spec.1"]},`+
		`"declaration_name":"棉制针织T恤衫","elements":[{"name":"品牌类型","value":"无品牌"}],`+
		`"legal_units":[{"unit":"件","per_base_unit":"1","basis":"一件即一 PCS"}],`+
		`"origin":{"country":"CN","criterion":"wholly_obtained","basis":"境内生产"},`+
		`"export_control":{"status":"controlled","list_reference":"合成清单 1C999","basis":"合成"}}`)
	s := cat.saved
	if a.status != 200 || a.body["status"] != "draft" || s.ProfileID != "profile.1" || s.IdempotencyKey != "k1" || s.CodeTableVersion != "CN-2026" ||
		s.Commodity.SourceIDs[0] != "spec.1" || s.Elements[0].Value != "无品牌" || s.LegalUnits[0].PerBaseUnit != "1" ||
		s.Origin.Criterion != catapi.OriginWhollyObtained || s.ExportControl.ListReference != "合成清单 1C999" {
		t.Fatalf("%d %v %+v", a.status, a.body, s)
	}
	a = post(catapi.CommandVerifyDeclarationProfile, `{"id":"profile.1","expected_version":1,"revision":1,"evidence_ids":["ruling.1"],"reason":"专业核实"}`)
	if v := cat.verified; a.status != 200 || a.body["status"] != "verified" || v.ProfileID != "profile.1" || v.ExpectedVersion != 1 ||
		v.Revision != 1 || v.EvidenceIDs[0] != "ruling.1" || v.Reason != "专业核实" {
		t.Fatalf("%d %v %+v", a.status, a.body, cat.verified)
	}
	if a = post(catapi.CommandVerifyDeclarationProfile, `{"id":"profile.1","revision":1,"evidence_ids":["e"],"reason":"r"}`); a.status != 422 ||
		a.body["field_errors"].([]any)[0].(map[string]any)["path"] != "/args/expected_version" {
		t.Fatalf("missing expected_version: %d %v", a.status, a.body)
	}
	if a = post(catapi.CommandSaveDeclarationProfile, `{"id":"p","expected_version":0,"commodity":{"Code":"6109100021"}}`); a.status != 422 {
		t.Fatalf("Go field name accepted: %d %v", a.status, a.body)
	}

	a = call(t, h, "GET", "/api/brant/v1/cat/declaration_profiles/profile.1", "Bearer good", "")
	rev := a.body["revisions"].([]any)[0].(map[string]any)
	if a.status != 200 || a.body["ok"] != true || a.body["key"].(map[string]any)["code_table_version"] != "CN-2026" ||
		rev["commodity"].(map[string]any)["code"] != "6109100021" || rev["declaration_name"] != "棉制针织T恤衫" ||
		rev["export_control"].(map[string]any)["status"] != "controlled" || rev["verified_by"].(map[string]any)["id"] != "doc.2" ||
		rev["verified_at"] != "2026-11-12T00:00:00.000000Z" {
		t.Fatalf("%d %v", a.status, a.body)
	}
	if a = call(t, h, "GET", "/api/brant/v1/cat/declaration_profiles/other", "Bearer good", ""); a.status != 404 {
		t.Fatalf("%d %v", a.status, a.body)
	}
	a = call(t, h, "GET", "/api/brant/v1/cat/skus/sku/declaration_profiles", "Bearer good", "")
	if profiles := a.body["profiles"].([]any); a.status != 200 || a.body["sku_id"] != "sku" || len(profiles) != 1 ||
		profiles[0].(map[string]any)["verified_revision"] != float64(1) {
		t.Fatalf("%d %v", a.status, a.body)
	}
}
