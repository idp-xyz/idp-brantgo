//go:build integration

package bootstrap_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
	refapi "go.idp.xyz/idp-brant/internal/reference/api"
)

// The party screening suite (WBS S1-09, S1-11; PTY-INV-002, TD-AT15, ADR-BR-017). Python V0.50 has
// no party screening ("—（Python 未实现）"), so no Python test is ported; the vectors come from the
// tactical model's TD-AT15 and PTY-INV-002.

// compliancePermissions is what the compliance role template holds for screening and the list
// import, with the reads the suite needs.
var compliancePermissions = []string{"party_screening.record", "party_screening.decide", "party_screening.block",
	"screening_list.import", "pty.read", "pty.audit", "ref.read", "ref.audit", "evd.read", "pol.read"}

type screeningWorld struct {
	*world
	editor, reviewer, compliance, sales context.Context
	app                                 *bentopg.DB
}

func newScreeningWorld(t *testing.T) *screeningWorld {
	w := newWorld(t)
	s := &screeningWorld{world: w}
	s.editor, s.reviewer = w.polActors()
	s.compliance = w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "compliance"},
		Permissions: compliancePermissions, DataScope: "company"})
	// A sales clerk confirms customers and may screen them, but holds no compliance conclusion.
	s.sales = w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "sales"},
		Permissions: []string{"party.save_draft", "party.confirm", "party.revise", "pty.read", "evd.read", "party_screening.record"},
		DataScope:   "company"})
	db, err := bentopg.NewDB(pgtest.AppPool(t))
	if err != nil {
		t.Fatal(err)
	}
	s.app = db
	return s
}

func listEntry(id, name string, aliases ...string) refapi.ScreeningEntry {
	return refapi.ScreeningEntry{EntryID: id, PrimaryName: name, Aliases: aliases, EntityType: refapi.EntityTypeEntity}
}

// importList imports a synthetic list version as compliance.
func (s *screeningWorld) importList(ctx context.Context, code, jurisdiction, version, published string, entries ...refapi.ScreeningEntry) (refapi.Receipt, error) {
	sum := sha256.Sum256([]byte("synthetic source file " + code + " " + version))
	return s.c.REF.ImportScreeningList(ctx, refapi.ImportScreeningList{IdempotencyKey: s.key(), ListCode: code, Jurisdiction: jurisdiction,
		Version: version, SourceName: "Synthetic list " + code, SourceURL: "https://lists.example.org/" + code, PublishedOn: published,
		FileSHA256: hex.EncodeToString(sum[:]), EntryCount: len(entries), Entries: entries})
}

func (s *screeningWorld) mustImport(code, jurisdiction, version, published string, entries ...refapi.ScreeningEntry) {
	s.t.Helper()
	_, err := s.importList(s.compliance, code, jurisdiction, version, published, entries...)
	s.must(err)
}

// regulation saves a screening_regulation as editor and publishes it as reviewer, citing the
// suite's evidence and the legal confirmations.
func (s *screeningWorld) regulation(id string, expected int64, days int, lists ...polapi.ScreeningList) (polapi.ConfigRef, error) {
	s.t.Helper()
	saved, err := s.saveConfig(s.editor, polapi.KindScreeningRegulation, id, expected, newWindow().from, time.Time{},
		polapi.ConfigData{ScreeningRegulation: &polapi.ScreeningRegulation{Lists: lists, RescreenIntervalDays: days}})
	if err != nil {
		return polapi.ConfigRef{}, err
	}
	v, err := s.c.POL.GetConfig(s.editor, polapi.KindScreeningRegulation, id)
	s.must(err)
	evidence := []string{"evidence"}
	for _, l := range lists {
		if l.LegalConfirmationEvidenceID != "" {
			evidence = append(evidence, l.LegalConfirmationEvidenceID)
		}
	}
	_, err = s.c.POL.Publish(s.reviewer, polapi.Publish{IdempotencyKey: s.key(), Kind: polapi.KindScreeningRegulation, ConfigID: id,
		ExpectedVersion: saved.RowVersion, Revision: v.WorkingRevision, EvidenceIDs: evidence, Reason: "合规负责人确认名单来源"})
	return polapi.ConfigRef{Kind: polapi.KindScreeningRegulation, ConfigID: id, Revision: v.WorkingRevision}, err
}

// customer saves and confirms a customer named name as sales.
func (s *screeningWorld) customer(id, name string) {
	s.t.Helper()
	_, err := s.c.PTY.SaveParty(s.sales, ptyapi.SaveParty{IdempotencyKey: s.key(), PartyID: id, Name: name, Roles: []string{ptyapi.RoleCustomer}})
	s.must(err)
	_, err = s.c.PTY.ConfirmParty(s.sales, ptyapi.ConfirmParty{IdempotencyKey: s.key(), PartyID: id, ExpectedVersion: 1, EvidenceIDs: []string{"evidence"}})
	s.must(err)
}

func (s *screeningWorld) record(ctx context.Context, partyID string, expected int64, reg polapi.ConfigRef) (ptyapi.Receipt, error) {
	return s.c.PTY.RecordScreening(ctx, ptyapi.RecordScreening{IdempotencyKey: s.key(), PartyID: partyID, ExpectedVersion: expected,
		RegulationID: reg.ConfigID, RegulationRevision: reg.Revision})
}

func (s *screeningWorld) screening(partyID string) ptyapi.ScreeningView {
	s.t.Helper()
	v, err := s.c.PTY.GetScreening(s.compliance, partyID)
	s.must(err)
	return v
}

// guard runs PartyScreeningGuard v1 as a downstream would: in a transaction of its own owner, with
// its own executor, in company.A.
func (s *screeningWorld) guard(partyID string, at time.Time) ptyapi.ScreeningGuardResult {
	s.t.Helper()
	var g ptyapi.ScreeningGuardResult
	s.must(bento.ScopeTransactor{DB: s.app, Scope: s.scope("company.A")}.WithinTransaction(context.Background(), func(ctx context.Context) error {
		exec, err := s.app.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		return exec.QueryRow(ctx, ptyapi.ScreeningGuardSQL, partyID, at).Scan(g.Targets()...)
	}))
	return g
}

func (s *screeningWorld) wantGuard(partyID, status, reason string) ptyapi.ScreeningGuardResult {
	s.t.Helper()
	g := s.guard(partyID, time.Now())
	if g.Status != status || g.Reason != reason || g.Clear != (status == ptyapi.ScreeningClear) {
		s.t.Fatalf("guard of %s: %+v, want %s/%s", partyID, g, status, reason)
	}
	return g
}

func (s *screeningWorld) due(at time.Time) map[string]ptyapi.ScreeningDue {
	s.t.Helper()
	list, err := s.c.PTY.ScreeningsDue(s.compliance, at)
	s.must(err)
	out := map[string]ptyapi.ScreeningDue{}
	for _, d := range list {
		out[d.PartyID] = d
	}
	return out
}

var (
	cnList = polapi.ScreeningList{ListCode: "cn.uel", Jurisdiction: polapi.JurisdictionCN}
	unList = polapi.ScreeningList{ListCode: "un.consolidated", Jurisdiction: polapi.JurisdictionUN}
)

// TD-AT15 (PTY-INV-002): a new customer is pending before screening; a suspected hit decided by
// compliance as a false positive becomes clear and registers a positive-list entry; the entry
// stops applying when the party is confirmed in a new revision and when the list entry changes in
// a newly imported version, and the party is pending again; a party on the negative list stays
// blocked after a list update. A list import and a new party revision make screenings due without
// writing to them.
func TestTDAT15PartyScreeningLifecycle(t *testing.T) {
	s := newScreeningWorld(t)
	s.mustImport("cn.uel", refapi.JurisdictionCN, "v1", "2026-11-01",
		listEntry("E1", "Acme Trading Co., Ltd.", "ACME TRADING"), listEntry("E2", "Globex Corporation"))
	s.mustImport("un.consolidated", refapi.JurisdictionUN, "2026-11", "2026-11-02", listEntry("QDe.001", "Initech Holdings"))
	reg, err := s.regulation("screening", 0, 30, cnList, unList)
	s.must(err)

	// A new customer is pending before screening.
	s.customer("acme", "Acme Trading")
	s.wantGuard("acme", ptyapi.ScreeningPending, ptyapi.ReasonNotScreened)
	if v := s.screening("acme"); v.Status != ptyapi.ScreeningPending || v.RowVersion != 0 || len(v.Screenings) != 0 {
		t.Fatalf("before screening %+v", v)
	}
	if d := s.due(time.Now())["acme"]; d.Reason != ptyapi.ReasonNotScreened || d.RowVersion != 0 || d.PartyRevision != 1 {
		t.Fatalf("a new customer is due: %+v", d)
	}

	// An AI agent screens it: the alias "ACME TRADING" matches "Acme Trading" (name-key-v1).
	ai := s.provision(bootstrap.Grant{Scope: s.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorAI, ID: "screener"},
		Permissions: allPermissions, DataScope: "company"})
	r, err := s.record(ai, "acme", 0, reg)
	s.must(err)
	if r.Status != ptyapi.ScreeningMatch || r.RowVersion != 1 || r.ObjectID != "acme" {
		t.Fatalf("record %+v", r)
	}
	g := s.wantGuard("acme", ptyapi.ScreeningMatch, ptyapi.ReasonSuspectedMatch)
	v := s.screening("acme")
	first := v.Screenings[0]
	if v.CurrentScreeningID != g.ScreeningID || first.Outcome != ptyapi.OutcomeMatch || len(first.Hits) != 1 ||
		first.Hits[0].EntryID != "E1" || first.Hits[0].MatchedName != "ACME TRADING" || first.Hits[0].NameKey != "acme trading" ||
		first.Hits[0].Disposition != ptyapi.HitSuspected || first.Normalization != refapi.Normalization || first.ScreenedBy.ID != "screener" ||
		len(first.Lists) != 2 || first.Lists[0].Version != "v1" || first.RegulationRevision != reg.Revision ||
		!first.ValidUntil.Equal(first.ScreenedAt.Add(30*24*time.Hour)) {
		t.Fatalf("conclusion %+v", v)
	}

	// Only a compliance human decides: AI and system actors are refused whatever their grant, and
	// a human without the compliance permission is refused through permissions.
	decide := ptyapi.DecideScreening{PartyID: "acme", ExpectedVersion: 1, ScreeningID: g.ScreeningID, Decision: ptyapi.DecisionFalsePositive,
		Reason: "注册地址与名单条目不同", RegisterPositive: true}
	system := s.provision(bootstrap.Grant{Scope: s.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorSystem, ID: "worker"},
		Permissions: allPermissions, DataScope: "company"})
	for _, ctx := range []context.Context{ai, system, s.sales} {
		decide.IdempotencyKey = s.key()
		_, err = s.c.PTY.DecideScreening(ctx, decide)
		s.want(err, kernel.CodePermissionDenied)
	}
	_, err = s.c.PTY.BlockParty(ai, ptyapi.BlockParty{IdempotencyKey: s.key(), PartyID: "acme", ExpectedVersion: 1, Reason: "x"})
	s.want(err, kernel.CodePermissionDenied)

	// Compliance decides a false positive: clear, with a positive-list entry for revision 1 and
	// the entry's content.
	decide.IdempotencyKey = s.key()
	r, err = s.c.PTY.DecideScreening(s.compliance, decide)
	s.must(err)
	if r.Status != ptyapi.ScreeningClear || r.RowVersion != 2 {
		t.Fatalf("decide %+v", r)
	}
	s.wantGuard("acme", ptyapi.ScreeningClear, ptyapi.ReasonScreened)
	v = s.screening("acme")
	if len(v.PositiveEntries) != 1 || v.PositiveEntries[0].PartyRevision != 1 || v.PositiveEntries[0].EntryContentHash != first.Hits[0].EntryContentHash ||
		!v.PositiveEntries[0].AppliesToRevision || v.Screenings[0].Decision == nil || v.Screenings[0].Decision.DecidedBy.ID != "compliance" {
		t.Fatalf("positive list %+v", v)
	}
	if _, ok := s.due(time.Now())["acme"]; ok {
		t.Fatal("a clear party is not due")
	}

	// A new list version with the entry unchanged: the conclusion is pending until re-screened,
	// and the positive-list entry still applies.
	s.mustImport("cn.uel", refapi.JurisdictionCN, "v2", "2026-11-05",
		listEntry("E1", "Acme Trading Co., Ltd.", "ACME TRADING"), listEntry("E2", "Globex Corporation"), listEntry("E3", "Umbrella Ltd"))
	s.wantGuard("acme", ptyapi.ScreeningPending, ptyapi.ReasonListUpdated)
	if d := s.due(time.Now())["acme"]; d.Reason != ptyapi.ReasonListUpdated || d.RowVersion != 2 {
		t.Fatalf("a list update makes the screening due: %+v", d)
	}
	r, err = s.record(s.compliance, "acme", 2, reg)
	s.must(err)
	if r.Status != ptyapi.ScreeningClear {
		t.Fatalf("unchanged entry: %+v", r)
	}
	if hit := s.screening("acme").Screenings[1].Hits[0]; hit.Disposition != ptyapi.HitPositiveList || hit.PositiveID != v.PositiveEntries[0].PositiveID {
		t.Fatalf("covered hit %+v", hit)
	}

	// The party is confirmed in a new revision: pending, and its positive-list entry no longer
	// applies.
	_, err = s.c.PTY.ReviseParty(s.sales, ptyapi.ReviseParty{IdempotencyKey: s.key(), PartyID: "acme", ExpectedVersion: 2, Reason: "地址更正"})
	s.must(err)
	s.wantGuard("acme", ptyapi.ScreeningPending, ptyapi.ReasonPartyChanged)
	_, err = s.record(s.compliance, "acme", 3, reg)
	s.want(err, kernel.CodeInvalidState) // a draft is not screened
	_, err = s.c.PTY.ConfirmParty(s.sales, ptyapi.ConfirmParty{IdempotencyKey: s.key(), PartyID: "acme", ExpectedVersion: 3, EvidenceIDs: []string{"evidence"}})
	s.must(err)
	s.wantGuard("acme", ptyapi.ScreeningPending, ptyapi.ReasonPartyChanged)
	if d := s.due(time.Now())["acme"]; d.Reason != ptyapi.ReasonPartyChanged || d.PartyRevision != 2 {
		t.Fatalf("a new revision makes the screening due: %+v", d)
	}
	r, err = s.record(s.compliance, "acme", 3, reg)
	s.must(err)
	if r.Status != ptyapi.ScreeningMatch {
		t.Fatalf("the positive-list entry of revision 1 must not apply to revision 2: %+v", r)
	}
	g = s.wantGuard("acme", ptyapi.ScreeningMatch, ptyapi.ReasonSuspectedMatch)
	_, err = s.c.PTY.DecideScreening(s.compliance, ptyapi.DecideScreening{IdempotencyKey: s.key(), PartyID: "acme", ExpectedVersion: 4,
		ScreeningID: g.ScreeningID, Decision: ptyapi.DecisionFalsePositive, Reason: "修订后仍非名单主体", RegisterPositive: true})
	s.must(err)
	s.wantGuard("acme", ptyapi.ScreeningClear, ptyapi.ReasonScreened)

	// The list entry changes in a newly imported version: the party is pending again, and
	// re-screening finds the positive-list entry no longer applies, so the hit is suspected again.
	s.mustImport("cn.uel", refapi.JurisdictionCN, "v3", "2026-11-08",
		listEntry("E1", "Acme Trading Co., Ltd.", "ACME TRADING", "Acme Trading HK"), listEntry("E2", "Globex Corporation"))
	s.wantGuard("acme", ptyapi.ScreeningPending, ptyapi.ReasonListUpdated)
	r, err = s.record(s.compliance, "acme", 5, reg)
	s.must(err)
	if r.Status != ptyapi.ScreeningMatch {
		t.Fatalf("changed entry: %+v", r)
	}
	v = s.screening("acme")
	last := v.Screenings[len(v.Screenings)-1]
	if last.Hits[0].Disposition != ptyapi.HitSuspected || last.Hits[0].Version != "v3" || len(v.PositiveEntries) != 2 {
		t.Fatalf("after the entry changed %+v", v)
	}
	s.wantGuard("acme", ptyapi.ScreeningMatch, ptyapi.ReasonSuspectedMatch)

	// A party on the negative list stays blocked after a list update.
	s.customer("rogue", "Rogue Shipping")
	_, err = s.c.PTY.BlockParty(s.compliance, ptyapi.BlockParty{IdempotencyKey: s.key(), PartyID: "rogue", ExpectedVersion: 0, Reason: "实际控制人被制裁"})
	s.must(err)
	s.wantGuard("rogue", ptyapi.ScreeningBlocked, ptyapi.ReasonNegativeList)
	s.mustImport("cn.uel", refapi.JurisdictionCN, "v4", "2026-11-09", listEntry("E2", "Globex Corporation"))
	r, err = s.record(s.compliance, "rogue", 1, reg)
	s.must(err)
	if r.Status != ptyapi.ScreeningBlocked {
		t.Fatalf("re-screening does not lift the negative list: %+v", r)
	}
	s.wantGuard("rogue", ptyapi.ScreeningBlocked, ptyapi.ReasonNegativeList)
	_, err = s.c.PTY.BlockParty(s.compliance, ptyapi.BlockParty{IdempotencyKey: s.key(), PartyID: "rogue", ExpectedVersion: 2, Reason: "again"})
	s.want(err, kernel.CodeInvalidState)
	if err = s.exec("company.A", `UPDATE brant_pty.party_screening SET negative = false, row_version = row_version + 1 WHERE party_id = 'rogue'`); err == nil {
		t.Fatal("the database must refuse lifting the negative list")
	}
	if err = s.exec("company.A", `DELETE FROM brant_pty.negative_list_entry`); err == nil {
		t.Fatal("negative-list entries are never deleted")
	}
	if err = s.exec("company.A", `UPDATE brant_pty.positive_list_entry SET party_revision = 9`); err == nil {
		t.Fatal("positive-list entries never change")
	}
	if d, ok := s.due(time.Now())["rogue"]; ok {
		t.Fatalf("a party on the negative list is never due: %+v", d)
	}

	var types []string
	for _, e := range s.events("company.A", ptyapi.Source) {
		if e.Payload["aggregate_type"] == "PartyScreening" {
			types = append(types, e.Type)
		}
	}
	want := []string{ptyapi.EventPartyScreeningRecorded, ptyapi.EventPartyScreeningDecided, ptyapi.EventPartyScreeningRecorded,
		ptyapi.EventPartyScreeningRecorded, ptyapi.EventPartyScreeningDecided, ptyapi.EventPartyScreeningRecorded,
		ptyapi.EventPartyScreeningBlocked, ptyapi.EventPartyScreeningRecorded}
	if !slices.Equal(types, want) {
		t.Fatalf("events %v", types)
	}
	imports := s.events("company.A", refapi.Source)
	if len(imports) != 5 || imports[2].Payload["previous_version"] != "v1" || imports[2].Payload["version"] != "v2" {
		t.Fatalf("import events %v", imports)
	}
}

// PTY-INV-002: a screening uses only the lists the active regulation enables, in their current
// versions; an enabled list without an imported version leaves it pending until one is imported.
func TestScreeningUsesOnlyEnabledLists(t *testing.T) {
	s := newScreeningWorld(t)
	s.mustImport("cn.uel", refapi.JurisdictionCN, "v1", "2026-11-01", listEntry("E2", "Globex Corporation"))
	s.mustImport("us.ofac.sdn", refapi.JurisdictionForeign, "2026-11-03", "2026-11-03", listEntry("S1", "Acme Trading"))
	reg, err := s.regulation("screening", 0, 30, cnList, unList)
	s.must(err)
	s.customer("acme", "Acme Trading")
	r, err := s.record(s.compliance, "acme", 0, reg)
	s.must(err)
	if r.Status != ptyapi.ScreeningPending {
		t.Fatalf("the UN list is enabled but not imported: %+v", r)
	}
	s.wantGuard("acme", ptyapi.ScreeningPending, ptyapi.ReasonIncomplete)
	c := s.screening("acme").Screenings[0]
	if len(c.Hits) != 0 || len(c.Lists) != 1 || c.Lists[0].ListCode != "cn.uel" || !slices.Equal(c.MissingLists, []string{"un.consolidated"}) {
		t.Fatalf("a hit on a list the regulation does not enable counts: %+v", c)
	}
	s.mustImport("un.consolidated", refapi.JurisdictionUN, "2026-11", "2026-11-02", listEntry("QDe.001", "Initech Holdings"))
	s.wantGuard("acme", ptyapi.ScreeningPending, ptyapi.ReasonListUpdated)
	r, err = s.record(s.compliance, "acme", 1, reg)
	s.must(err)
	if r.Status != ptyapi.ScreeningClear {
		t.Fatalf("complete screening: %+v", r)
	}
	s.wantGuard("acme", ptyapi.ScreeningClear, ptyapi.ReasonScreened)

	// A regulation revision that is no longer in force cannot be screened under, and the parties
	// screened under it become due.
	next, err := s.regulation("screening", 2, 30, cnList, unList)
	s.must(err)
	_, err = s.record(s.compliance, "acme", 2, reg)
	s.want(err, polapi.CodeConfigNotActive)
	if d := s.due(time.Now())["acme"]; d.Reason != ptyapi.ReasonRegulationChanged || d.RegulationRevision != reg.Revision {
		t.Fatalf("superseded regulation: %+v", d)
	}
	_, err = s.record(s.compliance, "acme", 2, next)
	s.must(err)
	// A list imported under another jurisdiction than the regulation enables it with is refused.
	wrong, err := s.regulation("wrong", 0, 30, polapi.ScreeningList{ListCode: "us.ofac.sdn", Jurisdiction: polapi.JurisdictionCN})
	s.must(err)
	_, err = s.record(s.compliance, "acme", 3, wrong)
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/regulation_id" {
		t.Fatalf("jurisdiction mismatch: %v", err)
	}
	_, err = s.record(s.compliance, "acme", 3, polapi.ConfigRef{Kind: polapi.KindScreeningRegulation, ConfigID: "missing", Revision: 1})
	s.want(err, kernel.CodeReferenceInvalid)
	_, err = s.record(s.compliance, "acme", 2, next)
	s.want(err, kernel.CodeVersionConflict)
	// A name without a letter or digit has no key and is never reported clear.
	s.customer("dashes", "---")
	_, err = s.record(s.compliance, "dashes", 0, next)
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/id" {
		t.Fatalf("unmatchable name: %v", err)
	}
}

// ADR-BR-017, SR-19: a foreign list cannot be enabled without the tenant's legal confirmation,
// which the publication cites and the publisher must be able to read (R63).
func TestForeignListNeedsLegalConfirmation(t *testing.T) {
	s := newScreeningWorld(t)
	ofac := polapi.ScreeningList{ListCode: "us.ofac.sdn", Jurisdiction: polapi.JurisdictionForeign}
	_, err := s.regulation("screening", 0, 30, cnList, ofac)
	if code(err) != kernel.CodeSchemaInvalid || path(err) != "/args/data/lists/1/legal_confirmation_evidence_id" {
		t.Fatalf("a foreign list without legal confirmation: %v at %s", err, path(err))
	}
	ofac.LegalConfirmationEvidenceID = "legal.ofac"
	saved, err := s.saveConfig(s.editor, polapi.KindScreeningRegulation, "screening", 0, newWindow().from, time.Time{},
		polapi.ConfigData{ScreeningRegulation: &polapi.ScreeningRegulation{Lists: []polapi.ScreeningList{cnList, ofac}, RescreenIntervalDays: 30}})
	s.must(err)
	publish := polapi.Publish{IdempotencyKey: s.key(), Kind: polapi.KindScreeningRegulation, ConfigID: "screening", ExpectedVersion: saved.RowVersion,
		Revision: 1, EvidenceIDs: []string{"evidence"}, Reason: "启用外国名单"}
	_, err = s.c.POL.Publish(s.reviewer, publish)
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/evidence_ids" {
		t.Fatalf("the legal confirmation must be cited: %v at %s", err, path(err))
	}
	publish.IdempotencyKey, publish.EvidenceIDs = s.key(), []string{"evidence", "legal.ofac"}
	_, err = s.c.POL.Publish(s.reviewer, publish)
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/evidence_ids/1" {
		t.Fatalf("the legal confirmation must be readable through EVD: %v at %s", err, path(err))
	}
	s.source(s.editor, "legal.ofac", "法务确认：对美出口业务启用 OFAC SDN 名单（合成）")
	publish.IdempotencyKey = s.key()
	_, err = s.c.POL.Publish(s.reviewer, publish)
	s.must(err)
	cfg, err := s.c.POL.GetConfig(s.compliance, polapi.KindScreeningRegulation, "screening")
	s.must(err)
	rev := cfg.Revisions[0]
	if rev.Data.ScreeningRegulation.Lists[1].LegalConfirmationEvidenceID != "legal.ofac" || !slices.Contains(rev.EvidenceIDs, "legal.ofac") {
		t.Fatalf("published %+v", rev)
	}
	s.mustImport("us.ofac.sdn", refapi.JurisdictionForeign, "2026-11-03", "2026-11-03", listEntry("S1", "Acme Trading"))
	s.mustImport("cn.uel", refapi.JurisdictionCN, "v1", "2026-11-01", listEntry("E2", "Globex Corporation"))
	s.customer("acme", "Acme Trading")
	r, err := s.record(s.compliance, "acme", 0, polapi.ConfigRef{Kind: polapi.KindScreeningRegulation, ConfigID: "screening", Revision: 1})
	s.must(err)
	if r.Status != ptyapi.ScreeningMatch {
		t.Fatalf("the enabled foreign list is used: %+v", r)
	}
}

// PTY-INV-002: an expired conclusion is not clear, and the party is due again.
func TestExpiredConclusionIsNotClear(t *testing.T) {
	s := newScreeningWorld(t)
	s.mustImport("cn.uel", refapi.JurisdictionCN, "v1", "2026-11-01", listEntry("E2", "Globex Corporation"))
	reg, err := s.regulation("screening", 0, 7, cnList)
	s.must(err)
	s.customer("acme", "Acme Trading")
	_, err = s.record(s.compliance, "acme", 0, reg)
	s.must(err)
	now := time.Now()
	s.wantGuard("acme", ptyapi.ScreeningClear, ptyapi.ReasonScreened)
	later := now.Add(8 * 24 * time.Hour)
	if g := s.guard("acme", later); g.Clear || g.Status != ptyapi.ScreeningExpired || g.Reason != ptyapi.ReasonExpired || g.PartyRevision != 1 {
		t.Fatalf("expired conclusion %+v", g)
	}
	if err = (ptyapi.ScreeningGuardResult{Status: ptyapi.ScreeningExpired, Reason: ptyapi.ReasonExpired}).Check("/args/buyer"); code(err) != ptyapi.CodePartyScreeningNotClear {
		t.Fatalf("expired is not clear: %v", err)
	}
	if _, ok := s.due(now)["acme"]; ok {
		t.Fatal("a valid conclusion is not due")
	}
	if d := s.due(later)["acme"]; d.Status != ptyapi.ScreeningExpired || d.Reason != ptyapi.ReasonExpired || d.RowVersion != 1 {
		t.Fatalf("due after expiry: %+v", d)
	}
	// Screenings are scoped to their company like every PTY record.
	other := s.provision(bootstrap.Grant{Scope: s.scope("company.B"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "compliance"},
		Permissions: compliancePermissions, DataScope: "company"})
	_, err = s.c.PTY.GetScreening(other, "acme")
	s.want(err, kernel.CodeNotFound)
	if s.count("company.B", "brant_pty.screening_result") != 0 || s.count("company.A", "brant_pty.screening_result") != 1 {
		t.Fatal("screenings are scoped to their company")
	}
}

// S1-11: list versions are imported by a human under control and never change afterwards: no
// command edits an entry, brant_app holds no UPDATE or DELETE, and triggers refuse both even for
// the schema owner. Each company imports the versions it uses.
func TestScreeningListImportIsControlledAndImmutable(t *testing.T) {
	s := newScreeningWorld(t)
	r, err := s.importList(s.compliance, "cn.uel", refapi.JurisdictionCN, "v1", "2026-11-01",
		listEntry("E2", "Globex Corporation"), listEntry("E1", "Acme Trading Co., Ltd.", "ACME TRADING"))
	s.must(err)
	if r.ObjectID != "cn.uel:v1" || r.RowVersion != 1 || r.Status != refapi.StatusImported {
		t.Fatalf("import %+v", r)
	}
	ai := s.provision(bootstrap.Grant{Scope: s.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorAI, ID: "ai"}, Permissions: allPermissions})
	_, err = s.importList(ai, "cn.uel", refapi.JurisdictionCN, "v2", "2026-11-02", listEntry("E1", "Acme"))
	s.want(err, kernel.CodePermissionDenied)
	_, err = s.importList(s.sales, "cn.uel", refapi.JurisdictionCN, "v2", "2026-11-02", listEntry("E1", "Acme"))
	s.want(err, kernel.CodePermissionDenied)
	_, err = s.importList(s.compliance, "cn.uel", refapi.JurisdictionCN, "v1", "2026-11-01", listEntry("E1", "Acme"))
	s.want(err, refapi.CodeBusinessKeyConflict)
	_, err = s.importList(s.compliance, "cn.uel", refapi.JurisdictionForeign, "v2", "2026-11-02", listEntry("E1", "Acme"))
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/jurisdiction" {
		t.Fatalf("a list keeps its jurisdiction: %v", err)
	}
	_, err = s.importList(s.compliance, "cn.uel", refapi.JurisdictionCN, "v0", "2026-10-01", listEntry("E1", "Acme"))
	s.want(err, kernel.CodeInvalidKnownValue)

	v, err := s.c.REF.GetListVersion(s.compliance, "cn.uel", "v1")
	s.must(err)
	if v.EntryCount != 2 || len(v.Entries) != 2 || v.Entries[0].EntryID != "E1" || len(v.Entries[0].ContentHash) != 64 ||
		len(v.ContentHash) != 64 || v.ImportedBy.ID != "compliance" || v.PublishedOn != "2026-11-01" || !v.Current || v.ImportSeq != 1 ||
		v.SourceURL != "https://lists.example.org/cn.uel" || len(v.FileSHA256) != 64 || v.ImportedAt.IsZero() {
		t.Fatalf("version %+v", v)
	}
	l, err := s.c.REF.GetList(s.compliance, "cn.uel")
	s.must(err)
	if l.CurrentVersion != "v1" || l.Jurisdiction != refapi.JurisdictionCN || len(l.Versions) != 1 {
		t.Fatalf("list %+v", l)
	}
	current, err := s.c.REF.CurrentVersions(s.sales, []string{"cn.uel", "un.consolidated"})
	s.must(err)
	if len(current) != 1 || current["cn.uel"].Version != "v1" {
		t.Fatalf("current %+v", current)
	}
	hits, err := s.c.REF.Match(s.sales, []refapi.ListVersionRef{{ListCode: "cn.uel", Version: "v1"}}, []string{"ＡＣＭＥ Trading", "Nobody"})
	s.must(err)
	if len(hits) != 1 || hits[0].Entry.EntryID != "E1" || hits[0].QueryName != "ＡＣＭＥ Trading" || hits[0].MatchedName != "ACME TRADING" {
		t.Fatalf("hits %+v", hits)
	}

	for _, sql := range []string{
		`UPDATE brant_ref.screening_list_entry SET primary_name = 'edited'`,
		`DELETE FROM brant_ref.screening_list_entry`,
		`UPDATE brant_ref.screening_entry_name SET name_key = 'edited'`,
		`DELETE FROM brant_ref.screening_list_version`,
		`UPDATE brant_ref.screening_list_version SET entry_count = 3`,
		`DELETE FROM brant_ref.screening_list`,
		`UPDATE brant_ref.screening_list SET jurisdiction = 'foreign', row_version = row_version + 1`,
	} {
		if err := s.exec("company.A", sql); err == nil {
			t.Errorf("brant_app must not run %s", sql)
		}
	}
	// The schema owner bypasses no trigger: entries and versions refuse UPDATE and DELETE.
	migrator := pgtest.MigratorPool(t)
	for _, sql := range []string{
		`UPDATE brant_ref.screening_list_entry SET primary_name = 'edited'`,
		`DELETE FROM brant_ref.screening_entry_name`,
		`UPDATE brant_ref.screening_list_version SET entry_count = 3`,
	} {
		tx, err := migrator.Begin(context.Background())
		s.must(err)
		_, err = tx.Exec(context.Background(), `SELECT set_config('app.tenant_id', $1, true), set_config('app.company_id', 'company.A', true)`, s.tenant)
		s.must(err)
		_, err = tx.Exec(context.Background(), sql)
		_ = tx.Rollback(context.Background())
		if err == nil || !strings.Contains(err.Error(), "immutable") {
			t.Errorf("the trigger must refuse %s: %v", sql, err)
		}
	}
	if s.count("company.A", "brant_ref.screening_list_entry") != 2 || s.count("company.B", "brant_ref.screening_list_entry") != 0 {
		t.Fatal("each company imports the versions it uses")
	}
	audit, err := s.c.REF.Audit(s.compliance)
	s.must(err)
	var imported, rejected int
	for _, e := range audit {
		switch {
		case e.Command == refapi.CommandImportScreeningList && e.Outcome == "succeeded":
			imported++
		case e.Command == refapi.CommandImportScreeningList && e.Outcome == "rejected":
			rejected++
		}
	}
	if imported != 1 || rejected != 5 {
		t.Fatalf("audit: %d imported, %d rejected", imported, rejected)
	}
}
