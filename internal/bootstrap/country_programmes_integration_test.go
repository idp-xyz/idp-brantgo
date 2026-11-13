//go:build integration

package bootstrap_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"testing"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
	refapi "go.idp.xyz/idp-brant/internal/reference/api"
)

// Country programme reference data (S5 A3b; owner decision E-3 of 2026-11-12, ADR-BR-017; UL
// CNX-EmbargoCheck; role template country_programme.import). Python V0.50 has no country
// programmes ("—（Python 未实现）"), so no Python test is ported; the vectors come from the design.
//
// The fixture is synthetic, as E-3 decided: one CN and one UN list whose programmes name only
// uninhabited territories (AQ Antarctica, BV Bouvet Island, HM Heard Island and McDonald Islands),
// so no test datum reads as a statement about a real country's legal position. It is not a legal
// list and must never be imported outside tests.

// Synthetic programme list codes; later units (CNX, the S5 chain) import the same fixture.
const (
	cnProgrammeList = "cn.country_programmes"
	unProgrammeList = "un.country_programmes"
	// syntheticEmbargoed is the destination the synthetic CN embargo programme names (TD-AT12's
	// "目的国处于禁运项目"); syntheticClear is named by no synthetic programme.
	syntheticEmbargoed = "AQ"
	syntheticClear     = "DE"
)

// syntheticCountryProgrammes is the synthetic import of list code (cnProgrammeList or
// unProgrammeList) at version, published on published (a date).
func syntheticCountryProgrammes(code, version, published string) refapi.ImportCountryProgrammes {
	cmd := refapi.ImportCountryProgrammes{ListCode: code, Version: version, PublishedOn: published,
		SourceName: "Synthetic country programmes " + code + " (test data, not a legal list)",
		SourceURL:  "https://lists.example.org/" + code}
	switch code {
	case cnProgrammeList:
		cmd.Jurisdiction = refapi.JurisdictionCN
		cmd.Programmes = []refapi.CountryProgramme{
			{ProgrammeID: "SYN-CN-EMB-1", Name: "合成全面禁运项目（测试数据）", Measure: refapi.MeasureEmbargo, Countries: []string{"AQ"},
				Scope: "全部货物与服务（合成）"},
			{ProgrammeID: "SYN-CN-EXP-1", Name: "合成出口管制项目（测试数据）", Measure: refapi.MeasureExportControl, Countries: []string{"BV", "AQ"},
				Scope: "合成两用物项清单所列物项"},
		}
	case unProgrammeList:
		cmd.Jurisdiction = refapi.JurisdictionUN
		cmd.Programmes = []refapi.CountryProgramme{
			{ProgrammeID: "SYN-UN-ARM-1", Name: "Synthetic arms embargo (test data)", Measure: refapi.MeasureArmsEmbargo,
				Countries: []string{"HM", "BV"}, Scope: "Arms and related materiel (synthetic)"},
		}
	}
	countries := map[string]bool{}
	for _, p := range cmd.Programmes {
		for _, c := range p.Countries {
			countries[c] = true
		}
	}
	sum := sha256.Sum256([]byte("synthetic programme file " + code + " " + version))
	cmd.FileSHA256, cmd.ProgrammeCount, cmd.CountryCount = hex.EncodeToString(sum[:]), len(cmd.Programmes), len(countries)
	return cmd
}

// complianceProgrammePermissions is what the compliance role template holds for the programme
// import, with the reads the suite needs.
var complianceProgrammePermissions = []string{"country_programme.import", "screening_list.import", "ref.read", "ref.audit"}

type programmeWorld struct {
	*world
	compliance, screener, reader context.Context
}

func newProgrammeWorld(t *testing.T) *programmeWorld {
	w := newWorld(t)
	p := &programmeWorld{world: w}
	p.compliance = w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "compliance"},
		Permissions: complianceProgrammePermissions, DataScope: "company"})
	// An actor who imports screening lists but holds no programme import.
	p.screener = w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "screener"},
		Permissions: []string{"screening_list.import", "ref.read"}, DataScope: "company"})
	// A downstream reader holding an active grant only.
	p.reader = w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "reader"},
		Permissions: []string{"pty.read"}, DataScope: "company"})
	return p
}

func (p *programmeWorld) importProgrammes(ctx context.Context, cmd refapi.ImportCountryProgrammes) (refapi.Receipt, error) {
	if cmd.IdempotencyKey == "" {
		cmd.IdempotencyKey = p.key()
	}
	return p.c.REF.ImportCountryProgrammes(ctx, cmd)
}

// S5 E-3, ADR-BR-017: a programme list version is imported by compliance alone, a human, under
// control and never changes afterwards: no command edits a programme, brant_app holds no UPDATE or
// DELETE, and triggers refuse both even for the schema owner. Each company imports the versions it
// uses; an import is replayed by its idempotency key.
func TestCountryProgrammeImportIsControlledAndImmutable(t *testing.T) {
	p := newProgrammeWorld(t)
	first := syntheticCountryProgrammes(cnProgrammeList, "v1", "2026-11-01")
	first.IdempotencyKey = "import.cn.v1"
	r, err := p.importProgrammes(p.compliance, first)
	p.must(err)
	if r.ObjectID != cnProgrammeList+":v1" || r.RowVersion != 1 || r.Status != refapi.StatusImported || r.Replayed {
		t.Fatalf("import %+v", r)
	}
	// The same request again, its programmes and countries in another order, is the same request.
	again := syntheticCountryProgrammes(cnProgrammeList, "v1", "2026-11-01")
	again.IdempotencyKey = "import.cn.v1"
	again.Programmes = []refapi.CountryProgramme{again.Programmes[1], again.Programmes[0]}
	again.Programmes[0].Countries = []string{"AQ", "BV"}
	replay, err := p.importProgrammes(p.compliance, again)
	p.must(err)
	if !replay.Replayed || replay.ActionID != r.ActionID || replay.RowVersion != 1 {
		t.Fatalf("replay %+v, first %+v", replay, r)
	}
	changed := syntheticCountryProgrammes(cnProgrammeList, "v1", "2026-11-01")
	changed.IdempotencyKey = "import.cn.v1"
	changed.Programmes[0].Measure = refapi.MeasureSanctions
	_, err = p.importProgrammes(p.compliance, changed)
	p.want(err, kernel.CodeIdempotencyPayloadConflict)

	ai := p.provision(bootstrap.Grant{Scope: p.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorAI, ID: "ai"},
		Permissions: append(slices.Clone(allPermissions), "country_programme.import")})
	_, err = p.importProgrammes(ai, syntheticCountryProgrammes(cnProgrammeList, "v2", "2026-11-02"))
	p.want(err, kernel.CodePermissionDenied)
	_, err = p.importProgrammes(p.screener, syntheticCountryProgrammes(cnProgrammeList, "v2", "2026-11-02"))
	p.want(err, kernel.CodePermissionDenied)
	_, err = p.importProgrammes(p.compliance, syntheticCountryProgrammes(cnProgrammeList, "v1", "2026-11-01"))
	p.want(err, refapi.CodeBusinessKeyConflict)
	foreign := syntheticCountryProgrammes(cnProgrammeList, "v2", "2026-11-02")
	foreign.Jurisdiction = refapi.JurisdictionForeign
	_, err = p.importProgrammes(p.compliance, foreign)
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/jurisdiction" {
		t.Fatalf("a list keeps its jurisdiction: %v", err)
	}
	_, err = p.importProgrammes(p.compliance, syntheticCountryProgrammes(cnProgrammeList, "v0", "2026-10-01"))
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/published_on" {
		t.Fatalf("no older version replaces the current one: %v", err)
	}
	miscounted := syntheticCountryProgrammes(cnProgrammeList, "v2", "2026-11-02")
	miscounted.CountryCount = 3
	_, err = p.importProgrammes(p.compliance, miscounted)
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/country_count" {
		t.Fatalf("the declared country count must match: %v", err)
	}
	misspelt := syntheticCountryProgrammes(cnProgrammeList, "v2", "2026-11-02")
	misspelt.Programmes[0].Countries = []string{"UK"}
	_, err = p.importProgrammes(p.compliance, misspelt)
	if code(err) != kernel.CodeSchemaInvalid || path(err) != "/args/programmes/0/countries/0" {
		t.Fatalf("an unassigned code is refused: %v", err)
	}

	v, err := p.c.REF.GetProgrammeListVersion(p.compliance, cnProgrammeList, "v1")
	p.must(err)
	if v.ProgrammeCount != 2 || v.CountryCount != 2 || len(v.Programmes) != 2 || v.Programmes[0].ProgrammeID != "SYN-CN-EMB-1" ||
		!slices.Equal(v.Programmes[1].Countries, []string{"AQ", "BV"}) || len(v.Programmes[1].ContentHash) != 64 || len(v.ContentHash) != 64 ||
		v.ImportedBy != (kernel.ActorRef{Kind: kernel.ActorHuman, ID: "compliance"}) || v.PublishedOn != "2026-11-01" || !v.Current ||
		v.ImportSeq != 1 || v.FileSHA256 != first.FileSHA256 || v.Jurisdiction != refapi.JurisdictionCN || v.ImportedAt.IsZero() ||
		v.Programmes[0].Scope != "全部货物与服务（合成）" {
		t.Fatalf("version %+v", v)
	}
	l, err := p.c.REF.GetProgrammeList(p.compliance, cnProgrammeList)
	p.must(err)
	if l.CurrentVersion != "v1" || l.Jurisdiction != refapi.JurisdictionCN || l.RowVersion != 1 || len(l.Versions) != 1 {
		t.Fatalf("list %+v", l)
	}
	if _, err = p.c.REF.GetProgrammeList(p.reader, cnProgrammeList); code(err) != kernel.CodePermissionDenied {
		t.Fatalf("reading needs ref.read: %v", err)
	}
	if _, err = p.c.REF.GetProgrammeListVersion(p.compliance, cnProgrammeList, "v9"); code(err) != kernel.CodeNotFound {
		t.Fatalf("an unknown version: %v", err)
	}
	if _, err = p.c.REF.GetProgrammeList(p.compliance, "cn.uel"); code(err) != kernel.CodeNotFound {
		t.Fatalf("screening lists are not programme lists: %v", err)
	}

	for _, sql := range []string{
		`UPDATE brant_ref.country_programme SET name = 'edited'`,
		`DELETE FROM brant_ref.country_programme`,
		`UPDATE brant_ref.country_programme_list_version SET programme_count = 3`,
		`DELETE FROM brant_ref.country_programme_list_version`,
		`DELETE FROM brant_ref.country_programme_list`,
		`UPDATE brant_ref.country_programme_list SET jurisdiction = 'foreign', row_version = row_version + 1`,
	} {
		if err := p.exec("company.A", sql); err == nil {
			t.Errorf("brant_app must not run %s", sql)
		}
	}
	// The schema owner bypasses no trigger: programmes and versions refuse UPDATE and DELETE; the
	// database refuses a programme whose countries are not canonical.
	migrator := pgtest.MigratorPool(t)
	for _, c := range []struct{ sql, want string }{
		{`UPDATE brant_ref.country_programme SET countries = ARRAY['AQ', 'HM']`, "immutable"},
		{`DELETE FROM brant_ref.country_programme`, "immutable"},
		{`UPDATE brant_ref.country_programme_list_version SET country_count = 3`, "immutable"},
		{`INSERT INTO brant_ref.country_programme (tenant_id, company_id, list_code, version, programme_id, name, measure, countries,
			content_hash) SELECT tenant_id, company_id, list_code, version, 'X', 'x', 'embargo', ARRAY['BV', 'AQ'], content_hash
			FROM brant_ref.country_programme_list_version`, "check constraint"},
	} {
		tx, err := migrator.Begin(context.Background())
		p.must(err)
		_, err = tx.Exec(context.Background(), `SELECT set_config('app.tenant_id', $1, true), set_config('app.company_id', 'company.A', true)`, p.tenant)
		p.must(err)
		_, err = tx.Exec(context.Background(), c.sql)
		_ = tx.Rollback(context.Background())
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("must refuse %s: %v", c.sql, err)
		}
	}
	if p.count("company.A", "brant_ref.country_programme") != 2 || p.count("company.B", "brant_ref.country_programme") != 0 ||
		p.count("company.A", "brant_ref.screening_list") != 0 {
		t.Fatal("each company imports the programme versions it uses; programme lists are not screening lists")
	}

	events := p.events("company.A", refapi.Source)
	if len(events) != 1 || events[0].Type != refapi.EventCountryProgrammesImported || events[0].Payload["list_code"] != cnProgrammeList {
		t.Fatalf("events %+v", events)
	}
	audit, err := p.c.REF.Audit(p.compliance)
	p.must(err)
	var imported, replayed, rejected int
	for _, e := range audit {
		if e.Command != refapi.CommandImportCountryProgrammes {
			continue
		}
		switch e.Outcome {
		case "succeeded":
			imported++
		case "replayed":
			replayed++
		case "rejected":
			rejected++
		}
	}
	if imported != 1 || replayed != 1 || rejected != 8 {
		t.Fatalf("audit: %d imported, %d replayed, %d rejected", imported, replayed, rejected)
	}
}

// UL CNX-EmbargoCheck, D-8: ResolveCountryProgrammes gives a downstream (CNX's trade compliance
// check) the versions it asked for and the programmes of them that name one of the queried
// countries, in one read and with an active grant only. A list the company has not imported is
// absent, never an empty clear answer; a stated version is resolved exactly, an empty one to the
// current version.
func TestResolveCountryProgrammes(t *testing.T) {
	p := newProgrammeWorld(t)
	_, err := p.importProgrammes(p.compliance, syntheticCountryProgrammes(cnProgrammeList, "v1", "2026-11-01"))
	p.must(err)
	_, err = p.importProgrammes(p.compliance, syntheticCountryProgrammes(unProgrammeList, "2026-11", "2026-11-01"))
	p.must(err)

	lists := []refapi.ListVersionRef{{ListCode: unProgrammeList}, {ListCode: cnProgrammeList}, {ListCode: "foreign.programmes"}}
	got, err := p.c.REF.ResolveCountryProgrammes(p.reader, refapi.ResolveProgrammes{Lists: lists,
		Countries: []string{syntheticClear, syntheticEmbargoed, "BV", syntheticEmbargoed}})
	p.must(err)
	if len(got.Versions) != 2 || got.Versions[cnProgrammeList].Version != "v1" || !got.Versions[cnProgrammeList].Current ||
		got.Versions[unProgrammeList].Jurisdiction != refapi.JurisdictionUN || len(got.Versions[unProgrammeList].ContentHash) != 64 {
		t.Fatalf("versions %+v", got.Versions)
	}
	if _, ok := got.Versions["foreign.programmes"]; ok {
		t.Fatal("a list the company never imported is absent")
	}
	var hits []string
	for _, h := range got.Hits {
		hits = append(hits, h.ListCode+"/"+h.Version+"/"+h.Programme.ProgrammeID+"/"+h.Country+"/"+h.Jurisdiction+"/"+h.Programme.Measure)
	}
	want := []string{
		"cn.country_programmes/v1/SYN-CN-EMB-1/AQ/CN/embargo",
		"cn.country_programmes/v1/SYN-CN-EXP-1/AQ/CN/export_control",
		"cn.country_programmes/v1/SYN-CN-EXP-1/BV/CN/export_control",
		"un.country_programmes/2026-11/SYN-UN-ARM-1/BV/UN/arms_embargo",
	}
	if !slices.Equal(hits, want) {
		t.Fatalf("hits %v", hits)
	}
	clear, err := p.c.REF.ResolveCountryProgrammes(p.reader, refapi.ResolveProgrammes{Lists: lists[:2], Countries: []string{syntheticClear}})
	p.must(err)
	if len(clear.Versions) != 2 || len(clear.Hits) != 0 {
		t.Fatalf("a country no programme names: %+v", clear)
	}

	// A new CN version without the embargo becomes current; v1 stays readable when asked for.
	next := syntheticCountryProgrammes(cnProgrammeList, "v2", "2026-11-05")
	next.Programmes, next.ProgrammeCount, next.CountryCount = next.Programmes[1:], 1, 2
	_, err = p.importProgrammes(p.compliance, next)
	p.must(err)
	now, err := p.c.REF.ResolveCountryProgrammes(p.reader, refapi.ResolveProgrammes{Lists: []refapi.ListVersionRef{{ListCode: cnProgrammeList}},
		Countries: []string{syntheticEmbargoed}})
	p.must(err)
	if now.Versions[cnProgrammeList].Version != "v2" || len(now.Hits) != 1 || now.Hits[0].Programme.ProgrammeID != "SYN-CN-EXP-1" {
		t.Fatalf("current %+v", now)
	}
	pinned, err := p.c.REF.ResolveCountryProgrammes(p.reader, refapi.ResolveProgrammes{
		Lists: []refapi.ListVersionRef{{ListCode: cnProgrammeList, Version: "v1"}}, Countries: []string{syntheticEmbargoed}})
	p.must(err)
	if v := pinned.Versions[cnProgrammeList]; v.Version != "v1" || v.Current || len(pinned.Hits) != 2 {
		t.Fatalf("pinned %+v", pinned)
	}
	missing, err := p.c.REF.ResolveCountryProgrammes(p.reader, refapi.ResolveProgrammes{
		Lists: []refapi.ListVersionRef{{ListCode: cnProgrammeList, Version: "v9"}}, Countries: []string{syntheticEmbargoed}})
	p.must(err)
	if len(missing.Versions) != 0 || len(missing.Hits) != 0 {
		t.Fatalf("an unknown version is absent: %+v", missing)
	}
	if _, err = p.c.REF.ResolveCountryProgrammes(p.reader, refapi.ResolveProgrammes{Lists: lists, Countries: []string{"UK"}}); code(err) != kernel.CodeSchemaInvalid {
		t.Fatalf("a misspelt country: %v", err)
	}
	other := p.provision(bootstrap.Grant{Scope: p.scope("company.B"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "reader"},
		Permissions: []string{"pty.read"}, DataScope: "company"})
	elsewhere, err := p.c.REF.ResolveCountryProgrammes(other, refapi.ResolveProgrammes{Lists: lists, Countries: []string{syntheticEmbargoed}})
	p.must(err)
	if len(elsewhere.Versions) != 0 || len(elsewhere.Hits) != 0 {
		t.Fatalf("company.B imported nothing: %+v", elsewhere)
	}
}

// ADR-BR-004 option B: a downstream transaction that holds a programme list through its guard
// function FOR SHARE makes an import of the next version wait until it ends, and then sees the new
// version in its next transaction; an import that committed first is what the guard returns.
func TestCountryProgrammeListGuardDecidesEveryCommitOrder(t *testing.T) {
	p := newProgrammeWorld(t)
	_, err := p.importProgrammes(p.compliance, syntheticCountryProgrammes(cnProgrammeList, "v1", "2026-11-01"))
	p.must(err)
	db, err := bentopg.NewDB(pgtest.AppPool(t))
	p.must(err)
	guard := func(hold func(pid int32)) string {
		var version *string
		p.must(bento.ScopeTransactor{DB: db, Scope: p.scope("company.A")}.WithinTransaction(context.Background(), func(ctx context.Context) error {
			exec, err := db.RequireExecutor(ctx)
			if err != nil {
				return err
			}
			if err = exec.QueryRow(ctx, refapi.CountryProgrammeListGuardSQL, cnProgrammeList).Scan(&version); err != nil {
				return err
			}
			if hold != nil {
				var pid int32
				if err = exec.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
					return err
				}
				hold(pid)
			}
			return nil
		}))
		if version == nil {
			return ""
		}
		return *version
	}
	if v := guard(nil); v != "v1" {
		t.Fatalf("guard %q", v)
	}
	var unknown *string
	p.must(bento.ScopeTransactor{DB: db, Scope: p.scope("company.A")}.WithinTransaction(context.Background(), func(ctx context.Context) error {
		exec, err := db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		return exec.QueryRow(ctx, refapi.CountryProgrammeListGuardSQL, unProgrammeList).Scan(&unknown)
	}))
	if unknown != nil {
		t.Fatalf("a list never imported has no current version: %q", *unknown)
	}

	done := make(chan error, 1)
	if v := guard(func(pid int32) {
		go func() {
			_, err := p.importProgrammes(p.compliance, syntheticCountryProgrammes(cnProgrammeList, "v2", "2026-11-02"))
			done <- err
		}()
		waitBlockedBy(t, pgtest.AppPool(t), pid, done)
	}); v != "v1" {
		t.Fatalf("the guard holding the list saw %q", v)
	}
	p.must(<-done)
	if v := guard(nil); v != "v2" {
		t.Fatalf("after the import the guard sees %q", v)
	}
}
