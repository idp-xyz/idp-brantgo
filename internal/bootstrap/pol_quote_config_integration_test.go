//go:build integration

package bootstrap_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/foundation/database/pgtest"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Python T03's quotation configuration runtime (test_t03_configuration_runtime.py: quote document
// templates and sales tax applications kept by the quote core) is POL's configuration lifecycle
// in Brant: the kinds quote_document_template and tax_application (side sales) go through the same
// save, independent publication, supersession, retirement and discard as every POL kind. These
// tests run the T03 scenarios on those kinds; test names cite the Python V0.50 test they port.

// feqvTemplate is a quote document template POL publishes for quote-text/0.1.0.
func feqvTemplate(footer string) polapi.ConfigData {
	return polapi.ConfigData{QuoteDocumentTemplate: &polapi.QuoteDocumentTemplate{Name: "formal", Language: polapi.LanguageEn,
		RendererVersion: polapi.QuoteRendererVersion, Title: "Quotation", Sections: polapi.QuoteSections(), FooterNote: footer}}
}

// feqvFailLedger makes POL's action result insert of command fail in w's tenant until the returned
// function drops the trigger: a late failure of the command ledger inside POL's transaction.
func feqvFailLedger(w *world, command string) func() {
	w.t.Helper()
	migrator := pgtest.MigratorPool(w.t)
	trigger := strings.ReplaceAll(pgtest.UniqueID(w.t, "fail_"), ".", "_")
	pgtest.Schema(w.t, migrator, pgtest.AppPool(w.t), "brant_t_fail_", fmt.Sprintf(`
CREATE FUNCTION {{schema}}.fail() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'test storage failure'; END $$;
CREATE TRIGGER %s BEFORE INSERT ON brant_pol.action_result FOR EACH ROW
WHEN (NEW.tenant_id = '%s' AND NEW.command = '%s') EXECUTE FUNCTION {{schema}}.fail();`, trigger, w.tenant, command))
	return func() {
		if _, err := migrator.Exec(context.Background(), "DROP TRIGGER "+trigger+" ON brant_pol.action_result"); err != nil {
			w.t.Fatal(err)
		}
	}
}

// Python test_t03_configuration_runtime.py test_template_persisted_lifecycle_history_and_replay
// (PYT-0404): a quote document template is saved as a working revision; its editor cannot publish
// it (INDEPENDENT_REVIEW_REQUIRED); the reviewer publishes it; the publication replays under its
// key, also on a platform opened again (Python reopens the core), and the key with another reason
// is IDEMPOTENCY_PAYLOAD_CONFLICT; a revised revision published supersedes the first, which stays
// readable; the lifecycle's events are kept. Python then retires the superseded revision by
// target_revision while the new one stays active; in POL a superseded revision has already ended
// (SM-POL-REVISION), so retirement ends the active revision instead.
func TestQuoteTemplateLifecycleHistoryAndReplay(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	win := newWindow()
	kind, id := polapi.KindQuoteDocumentTemplate, "template.synthetic"
	saved, err := w.saveConfig(editor, kind, id, 0, win.from, time.Time{}, feqvTemplate("合成模板"))
	w.must(err)
	v, err := w.c.POL.GetConfig(editor, kind, id)
	w.must(err)
	if saved.RowVersion != 1 || v.WorkingRevision != 1 || v.ActiveRevision != 0 || v.Revisions[0].Status != polapi.StatusWorking {
		t.Fatalf("the draft %+v %+v", saved, v)
	}
	pub := polapi.Publish{IdempotencyKey: w.key(), Kind: kind, ConfigID: id, ExpectedVersion: 1, Revision: 1, EvidenceIDs: []string{"evidence"},
		Reason: "Synthetic independent template review"}
	self := pub
	self.IdempotencyKey = w.key()
	_, err = w.c.POL.Publish(editor, self)
	w.want(err, polapi.CodeIndependentReviewRequired)
	published, err := w.c.POL.Publish(reviewer, pub)
	if err != nil || published.Status != polapi.StatusActive || published.RowVersion != 2 || published.Replayed {
		t.Fatalf("publish %+v, %v", published, err)
	}
	replay, err := w.c.POL.Publish(reviewer, pub)
	if err != nil || !replay.Replayed || replay.ActionID != published.ActionID {
		t.Fatalf("replay %+v, %v", replay, err)
	}
	platform, err := bootstrap.Open(context.Background(), bootstrap.Config{PostgresDSN: pgtest.AppDSN(t)})
	w.must(err)
	defer platform.Close()
	reopened, err := platform.Contexts()
	w.must(err)
	replay, err = reopened.POL.Publish(reviewer, pub)
	if err != nil || !replay.Replayed || replay.ActionID != published.ActionID {
		t.Fatalf("replay on a reopened platform %+v, %v", replay, err)
	}
	changed := pub
	changed.Reason = "Different payload"
	_, err = w.c.POL.Publish(reviewer, changed)
	w.want(err, kernel.CodeIdempotencyPayloadConflict)

	revised, err := w.saveConfig(editor, kind, id, 2, win.from, time.Time{}, feqvTemplate("合成模板第二版"))
	w.must(err)
	v, err = w.c.POL.GetConfig(editor, kind, id)
	w.must(err)
	if revised.RowVersion != 3 || v.WorkingRevision != 2 || v.ActiveRevision != 1 {
		t.Fatalf("the revision %+v %+v", revised, v)
	}
	_, err = w.publish(reviewer, kind, id, 3, 2)
	w.must(err)
	v, err = w.c.POL.GetConfig(reviewer, kind, id)
	w.must(err)
	if v.ActiveRevision != 2 || len(v.Revisions) != 2 || v.Revisions[0].Status != polapi.StatusSuperseded || v.Revisions[0].EndedAt.IsZero() ||
		v.Revisions[0].Data.QuoteDocumentTemplate.FooterNote != "合成模板" || v.Revisions[1].Status != polapi.StatusActive {
		t.Fatalf("after the second publication %+v", v)
	}
	_, err = w.c.POL.Retire(reviewer, polapi.Retire{IdempotencyKey: w.key(), Kind: kind, ConfigID: id, ExpectedVersion: 4,
		Reason: "Synthetic obsolete template retirement"})
	w.must(err)
	v, err = w.c.POL.GetConfig(reviewer, kind, id)
	w.must(err)
	if v.ActiveRevision != 0 || v.Revisions[0].Status != polapi.StatusSuperseded || v.Revisions[1].Status != polapi.StatusRetired {
		t.Fatalf("after the retirement %+v", v)
	}
	var types []string
	for _, e := range w.events("company.A", polapi.Source) {
		if e.Payload["config_id"] == id {
			types = append(types, e.Type)
		}
	}
	if want := []string{polapi.EventConfigPublished, polapi.EventConfigPublished, polapi.EventConfigSuperseded, polapi.EventConfigRetired}; !slices.Equal(types, want) {
		t.Fatalf("events %v, want %v", types, want)
	}
}

// Python test_t03_configuration_runtime.py test_sales_tax_application_requires_current_references_and_verified_evidence
// (PYT-0405): a sales tax application names configurations that must be in force — an
// unconfigured fee category is REFERENCE_INVALID when saved, and a tax profile superseded between
// the save and the publication refuses the publication (CONFIG_NOT_ACTIVE) — and it is published
// only on evidence the publisher can read. Python revokes the verification of the source and gets
// REFERENCE_STALE; EVD sources have no verification state in Brant and POL checks the evidence when
// it publishes (S1-D16), so unreadable evidence is the refusal (REFERENCE_INVALID). Published, the
// application is active and in force.
func TestTaxApplicationNeedsReferencesInForceAndReadableEvidence(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	win := newWindow()
	tax := w.tax(editor, reviewer, polapi.RecoveryNonrecoverable, "tax.synthetic", win)
	w.configure(editor, reviewer, polapi.KindFeeCategory, "charge.synthetic", 0, win.from, time.Time{},
		polapi.ConfigData{FeeCategory: &polapi.FeeCategory{Name: "合成费用", Category: "freight", Side: polapi.FeeSideSalesCharge,
			Inclusion: polapi.InclusionSeparate, AllocationBasis: polapi.AllocationValue, PolicyBasis: "synthetic"}}, true)
	application := func(subject string) polapi.ConfigData {
		return polapi.ConfigData{TaxApplication: &polapi.TaxApplication{Name: "Synthetic sales tax application", Side: polapi.TaxSideSales,
			SubjectKind: polapi.SubjectFeeCategory, SubjectID: subject, TaxProfile: tax, Jurisdiction: "CN-SYNTHETIC",
			BasisNote: "Synthetic policy basis; not a legal determination."}}
	}
	kind := polapi.KindTaxApplication
	_, err := w.saveConfig(editor, kind, "tax-application.unconfigured", 0, win.from, win.until, application("charge.unknown"))
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/data/subject_id" {
		t.Fatalf("an unconfigured fee category: %s at %s", code(err), path(err))
	}
	_, err = w.saveConfig(editor, kind, "tax-application.synthetic", 0, win.from, win.until, application("charge.synthetic"))
	w.must(err)
	_, err = w.saveConfig(editor, kind, "tax-application.later", 0, win.from, win.until, application("charge.synthetic"))
	w.must(err)
	pub := polapi.Publish{IdempotencyKey: w.key(), Kind: kind, ConfigID: "tax-application.synthetic", ExpectedVersion: 1, Revision: 1,
		EvidenceIDs: []string{"missing"}, Reason: "Synthetic tax-application review"}
	_, err = w.c.POL.Publish(reviewer, pub)
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/evidence_ids/0" {
		t.Fatalf("unreadable evidence: %s at %s", code(err), path(err))
	}
	pub.IdempotencyKey, pub.EvidenceIDs = w.key(), []string{"evidence"}
	r, err := w.c.POL.Publish(reviewer, pub)
	if err != nil || r.Status != polapi.StatusActive {
		t.Fatalf("publish %+v, %v", r, err)
	}
	ref := polapi.ConfigRef{Kind: kind, ConfigID: "tax-application.synthetic", Revision: 1}
	resolved, err := w.c.POL.ResolveConfigs(reviewer, []polapi.ConfigRef{ref}, time.Now())
	w.must(err)
	if got := resolved[ref]; got.Code != "" || got.Status != polapi.StatusActive || got.Data.TaxApplication.TaxProfile != tax {
		t.Fatalf("resolved %+v", got)
	}
	// The tax profile the other application names is superseded before it is published.
	w.configure(editor, reviewer, polapi.KindTaxProfile, "tax.synthetic", 2, win.from, win.until, taxData(polapi.RecoveryRecoverable), true)
	_, err = w.publish(reviewer, kind, "tax-application.later", 1, 1)
	if code(err) != polapi.CodeConfigNotActive {
		t.Fatalf("a superseded tax profile at the publication: %s at %s", code(err), path(err))
	}
	if v, err := w.c.POL.GetConfig(reviewer, kind, "tax-application.later"); err != nil || v.ActiveRevision != 0 ||
		v.Revisions[0].Status != polapi.StatusWorking {
		t.Fatalf("the refused application %+v, %v", v, err)
	}
}

// Python test_t03_configuration_runtime.py test_late_action_ledger_failure_rolls_back_config_event_root_and_outbox
// (PYT-0407): a failure writing the action result of a configuration save, after the save did its
// work, rolls the whole command back — no configuration root, no revision, no action result, no
// event — and the same key then saves once.
func TestConfigSaveLedgerFailureRollsBackEverything(t *testing.T) {
	w := newWorld(t)
	editor, _ := w.polActors()
	win := newWindow()
	cmd := polapi.SaveDraft{IdempotencyKey: w.key(), Kind: polapi.KindQuoteDocumentTemplate, ConfigID: "template.rollback", ExpectedVersion: 0,
		ValidFrom: kernel.FormatTimestamp(win.from), Data: feqvTemplate("合成回滚")}
	drop := feqvFailLedger(w, polapi.CommandSaveDraft)
	_, err := w.c.POL.SaveDraft(editor, cmd)
	if _, isRejection := kernel.AsRejection(err); err == nil || isRejection || !strings.Contains(err.Error(), "test storage failure") {
		t.Fatalf("the failing save: %v", err)
	}
	for _, table := range []string{"brant_pol.config_root", "brant_pol.config_revision", "brant_pol.action_result"} {
		if n := w.count("company.A", table); n != 0 {
			t.Fatalf("%s has %d rows after the rollback", table, n)
		}
	}
	if n := len(w.events("company.A", polapi.Source)); n != 0 {
		t.Fatalf("%d events after the rollback", n)
	}
	drop()
	r, err := w.c.POL.SaveDraft(editor, cmd)
	if err != nil || r.Replayed || r.RowVersion != 1 || w.count("company.A", "brant_pol.config_root") != 1 ||
		w.count("company.A", "brant_pol.action_result") != 1 {
		t.Fatalf("the retry %+v, %v", r, err)
	}
}
