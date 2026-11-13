package httpapi

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-bento-go/application"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/kernel"
	refapi "go.idp.xyz/idp-brant/internal/reference/api"
)

// fakeProgrammes records the programme imports it receives and serves one list.
type fakeProgrammes struct {
	REF
	imported []refapi.ImportCountryProgrammes
}

func (f *fakeProgrammes) ImportCountryProgrammes(_ context.Context, cmd refapi.ImportCountryProgrammes) (refapi.Receipt, error) {
	f.imported = append(f.imported, cmd)
	return refapi.Receipt{ActionID: "act-1", ObjectID: refapi.ObjectID(cmd.ListCode, cmd.Version), RowVersion: 1, Status: refapi.StatusImported}, nil
}

func (f *fakeProgrammes) AuditRejected(context.Context, string, string, string) error { return nil }

func (f *fakeProgrammes) GetProgrammeListVersion(_ context.Context, code, version string) (refapi.ProgrammeListVersionView, error) {
	if code != "un.country_programmes" || version != "v1" {
		return refapi.ProgrammeListVersionView{}, kernel.Reject(kernel.CodeNotFound, "/id", "")
	}
	return refapi.ProgrammeListVersionView{ProgrammeListVersion: refapi.ProgrammeListVersion{ListCode: code, Jurisdiction: refapi.JurisdictionUN,
		Version: version, PublishedOn: "2026-11-01", ProgrammeCount: 1, CountryCount: 1, ImportSeq: 1, Current: true,
		ImportedAt: time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC), ImportedBy: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "compliance"}},
		Programmes: []refapi.Programme{{ProgrammeID: "SYN-UN-ARM-1", Name: "Synthetic arms embargo", Measure: refapi.MeasureArmsEmbargo,
			Countries: []string{"HM"}}}}, nil
}

// S5 E-3: country_programme.import is decoded reflectively from the api structure, with the
// screening list import's ingress limits; the version read answers with snake_case members and no
// absent scope.
func TestCountryProgrammeImportAndRead(t *testing.T) {
	ref := &fakeProgrammes{}
	h, err := New(Config{EVD: struct{ EVD }{}, PTY: struct{ PTY }{}, CAT: struct{ CAT }{}, INQ: &fakeINQ{}, POL: struct{ POL }{}, REF: ref, SUP: struct{ SUP }{}, CST: struct{ CST }{}, TRD: struct{ TRD }{}, PUR: struct{ PUR }{}, QLT: struct{ QLT }{}, CNX: struct{ CNX }{},
		Verifier: tokens{"Bearer good": alice}, Clock: application.Clock(fixedClock{})})
	if err != nil {
		t.Fatal(err)
	}
	args := `{"list_code":"un.country_programmes","jurisdiction":"UN","version":"v1","source_name":"Synthetic UN programmes",` +
		`"source_url":"https://lists.example.org/un","published_on":"2026-11-01","file_sha256":"` + strings.Repeat("e", 64) + `",` +
		`"programme_count":1,"country_count":2,"programmes":[{"programme_id":"SYN-UN-ARM-1","name":"Synthetic arms embargo",` +
		`"measure":"arms_embargo","countries":["HM","BV"],"scope":"Arms and related materiel"}]}`
	body := `{"contract":"brant.ref/0.1.0","command":"country_programme.import","idempotency_key":"k1","args":` + args + `}`
	a := call(t, h, "POST", "/api/brant/v1/ref/commands/country_programme.import", "Bearer good", body)
	if a.status != 200 || a.body["object_id"] != "un.country_programmes:v1" || len(ref.imported) != 1 {
		t.Fatalf("%d %v", a.status, a.body)
	}
	got := ref.imported[0]
	if got.IdempotencyKey != "k1" || got.FileSHA256 != strings.Repeat("e", 64) || got.ProgrammeCount != 1 || got.CountryCount != 2 ||
		got.Programmes[0].ProgrammeID != "SYN-UN-ARM-1" || !slices.Equal(got.Programmes[0].Countries, []string{"HM", "BV"}) ||
		got.Programmes[0].Scope != "Arms and related materiel" || got.Programmes[0].Measure != refapi.MeasureArmsEmbargo {
		t.Fatalf("decoded %+v", got)
	}
	for _, bad := range []string{
		strings.Replace(body, `"scope":"Arms and related materiel"`, `"scope":""`, 1),
		strings.Replace(body, `"countries":["HM","BV"]`, `"countries":["HM","BV"],"country":"HM"`, 1),
		strings.Replace(body, `"programme_count":1`, `"programme_count":1.0`, 1),
	} {
		if a := call(t, h, "POST", "/api/brant/v1/ref/commands/country_programme.import", "Bearer good", bad); a.status != 422 ||
			a.body["code"] != "SCHEMA_INVALID" {
			t.Fatalf("%d %v", a.status, a.body)
		}
	}
	a = call(t, h, "GET", "/api/brant/v1/ref/country_programme_lists/un.country_programmes/versions/v1", "Bearer good", "")
	programmes, _ := a.body["programmes"].([]any)
	if a.status != 200 || a.body["ok"] != true || a.body["programme_count"] != float64(1) || a.body["current"] != true || len(programmes) != 1 {
		t.Fatalf("%d %v", a.status, a.body)
	}
	p := programmes[0].(map[string]any)
	if _, present := p["scope"]; present || p["measure"] != "arms_embargo" || p["countries"].([]any)[0] != "HM" {
		t.Fatalf("programme %v", p)
	}
	if a := call(t, h, "GET", "/api/brant/v1/ref/country_programme_lists/un.country_programmes/versions/v2", "Bearer good", ""); a.status != 404 {
		t.Fatalf("%d %v", a.status, a.body)
	}
}

// S5 D-8: an address without a country reads back exactly as before countries existed, and a
// stated country is the member "country".
func TestPartyAddressCountryOnTheWire(t *testing.T) {
	view := partyView(ptyapi.PartyView{PartyID: "c", Status: "draft", Roles: []string{"customer"}, Addresses: []ptyapi.Address{
		{AddressID: "ap", Role: ptyapi.AddressBilling, Address: "合成账单地址"},
		{AddressID: "dock", Role: ptyapi.AddressConsignee, Address: "合成收货地址", Country: "DE"}}})
	b, err := json.Marshal(view.Addresses)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"address_id":"ap","role":"billing","address":"合成账单地址"},{"address_id":"dock","role":"consignee","address":"合成收货地址","country":"DE"}]`
	if string(b) != want {
		t.Fatalf("%s", b)
	}
	cmd, err := ptyCommands(struct{ PTY }{}).commands[ptyapi.CommandSaveParty].build("k1",
		json.RawMessage(`{"id":"c","expected_version":0,"roles":["customer"],"addresses":[{"address_id":"dock","role":"consignee","address":"x","country":"DE"},{"address_id":"ap","role":"billing","address":"y"}]}`), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if a := cmd.(ptyapi.SaveParty).Addresses; len(a) != 2 || a[0].Country != "DE" || a[1].Country != "" {
		t.Fatalf("%+v", a)
	}
}
