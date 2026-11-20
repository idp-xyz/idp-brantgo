//go:build integration

package bootstrap_test

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/kernel"
	refapi "go.idp.xyz/idp-brant/internal/reference/api"
)

// The POL and PTY additions of S5R (unit R2; simulated sign-off E0 of 2026-11-13) against
// PostgreSQL 16 and migrations pol/000006, pty/000007 and ref/000003: publication by kind
// (S5R-D30), ConfigAt v1 (S5R-D23), the new-revision rules and the revisions stored before them
// (S5R-D06, D14, D15, D38), service roles without a route (S5R-D36), the notify address (S5R-D40),
// permitted payers (S5R-D21) and the regions of a country programme (S5R-D15). Python V0.50 has
// none of them, so the tests cite the design IDs.

// S5R-D30, POL-INV-001 (the lead's decision: each kind names who may publish it): with the role
// templates, a credit policy is published by the credit manager alone, an export tax policy by the
// tax specialist, a screening regulation by compliance, and the other kinds by the administrator;
// config.publish lets none of them publish another's kind. A tenant role that merges administrator
// with credit_manager publishes both. A credit policy's tiers name role templates that decide credit.
func TestS5RPublicationByKind(t *testing.T) {
	w := newWorld(t)
	editor, _ := w.polActors()
	admin, creditManager := w.assign("admin.1", "administrator"), w.assign("credit.manager.1", "credit_manager")
	taxSpecialist, compliance := w.assign("tax.1", "tax_specialist"), w.assign("compliance.1", "compliance")
	w.source(editor, "tax.review", "合成专业复核结论：免退（仅供测试）")
	win := newWindow()
	publish := func(ctx context.Context, kind, id string, expected, revision int64, evidence ...string) error {
		_, err := w.c.POL.Publish(ctx, polapi.Publish{IdempotencyKey: w.key(), Kind: kind, ConfigID: id, ExpectedVersion: expected,
			Revision: revision, EvidenceIDs: append([]string{"evidence"}, evidence...), Reason: "按种类发布（S5R-D30）"})
		return err
	}
	denied := func(err error, who string) {
		t.Helper()
		if code(err) != kernel.CodePermissionDenied || path(err) != "/args/kind" {
			t.Fatalf("%s: %v", who, err)
		}
	}

	_, err := w.saveConfig(editor, polapi.KindCurrency, "USD", 0, win.from, time.Time{}, currencyData("USD", 2, "USD"))
	w.must(err)
	denied(publish(creditManager, polapi.KindCurrency, "USD", 1, 1), "a credit manager publishing a currency")
	denied(publish(taxSpecialist, polapi.KindCurrency, "USD", 1, 1), "a tax specialist publishing a currency")
	denied(publish(compliance, polapi.KindCurrency, "USD", 1, 1), "compliance publishing a currency")
	w.must(publish(admin, polapi.KindCurrency, "USD", 1, 1))

	credit := polapi.CreditPolicy{Name: "E-2 synthetic", Currency: "USD", DefaultLimit: "0", LimitSources: []string{polapi.LimitSourceInternalAssessment},
		ApprovalTiers:        []polapi.CreditApprovalTier{{Level: "credit_officer", MaxAmount: "1000.00"}, {Level: "credit_manager", MaxAmount: "5000.00"}},
		OverdueToleranceDays: 30, ReservationValidityDays: 30, DecisionValidityDays: 7}
	_, err = w.saveConfig(editor, polapi.KindCreditPolicy, "credit", 0, win.from, time.Time{}, polapi.ConfigData{CreditPolicy: &credit})
	w.must(err)
	denied(publish(admin, polapi.KindCreditPolicy, "credit", 1, 1), "an administrator publishing a credit policy")
	denied(publish(taxSpecialist, polapi.KindCreditPolicy, "credit", 1, 1), "a tax specialist publishing a credit policy")
	w.must(publish(creditManager, polapi.KindCreditPolicy, "credit", 1, 1))
	unknown := credit
	unknown.ApprovalTiers = []polapi.CreditApprovalTier{{Level: "administrator", MaxAmount: "1000.00"}}
	_, err = w.saveConfig(editor, polapi.KindCreditPolicy, "credit.unknown", 0, win.from, time.Time{}, polapi.ConfigData{CreditPolicy: &unknown})
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/data/approval_tiers/0/level" {
		t.Fatalf("a tier level that is no credit template: %v", err)
	}

	_, err = w.saveConfig(taxSpecialist, polapi.KindExportTaxPolicy, "export.tax", 0, win.from, time.Time{}, exportTax(polapi.RefundExemptRefund, "tax.review"))
	w.must(err)
	denied(publish(creditManager, polapi.KindExportTaxPolicy, "export.tax", 1, 1, "tax.review"), "a credit manager publishing export tax")
	denied(publish(admin, polapi.KindExportTaxPolicy, "export.tax", 1, 1, "tax.review"), "an administrator publishing export tax")
	if err = publish(taxSpecialist, polapi.KindExportTaxPolicy, "export.tax", 1, 1, "tax.review"); code(err) != polapi.CodeIndependentReviewRequired {
		t.Fatalf("the tax specialist who edited it: %v", err)
	}
	other := w.assign("tax.2", "tax_specialist")
	w.must(publish(other, polapi.KindExportTaxPolicy, "export.tax", 1, 1, "tax.review"))

	_, err = w.saveConfig(editor, polapi.KindScreeningRegulation, "screening", 0, win.from, time.Time{}, polapi.ConfigData{
		ScreeningRegulation: &polapi.ScreeningRegulation{Lists: []polapi.ScreeningList{{ListCode: "cn.uel", Jurisdiction: polapi.JurisdictionCN}},
			RescreenIntervalDays: 30, Programmes: []polapi.ScreeningList{{ListCode: cnProgrammeList, Jurisdiction: polapi.JurisdictionCN}}}})
	w.must(err)
	denied(publish(admin, polapi.KindScreeningRegulation, "screening", 1, 1), "an administrator publishing a screening regulation")
	w.must(publish(compliance, polapi.KindScreeningRegulation, "screening", 1, 1))

	// A tenant role merging the administrator and the credit manager publishes both kinds.
	_, err = w.platform.DefineRole(context.Background(), w.scope("company.A"), "config.admin.credit", "配置管理兼信用经理",
		[]string{"administrator", "credit_manager"})
	w.must(err)
	merged := w.assign("merged.1", "config.admin.credit")
	_, err = w.saveConfig(editor, polapi.KindCurrency, "CNY", 0, win.from, time.Time{}, currencyData("CNY", 2, "CNY"))
	w.must(err)
	w.must(publish(merged, polapi.KindCurrency, "CNY", 1, 1))
	_, err = w.saveConfig(editor, polapi.KindCreditPolicy, "credit", 2, win.from, time.Time{}, polapi.ConfigData{CreditPolicy: &credit})
	w.must(err)
	w.must(publish(merged, polapi.KindCreditPolicy, "credit", 3, 2))

	audit, err := w.c.POL.Audit(editor)
	w.must(err)
	refused := 0
	for _, e := range audit {
		if e.Command == polapi.CommandPublish && e.Outcome == "rejected" && e.Code == kernel.CodePermissionDenied {
			refused++
		}
	}
	if refused != 8 {
		t.Fatalf("%d refused publications audited, want 8", refused)
	}
}

// ConfigAt v1 (S5R-D23, POL-INV-007; Context Map R50): the revision of a configuration whose
// validity contains an instant, published before or after it, the latest published of several,
// answered alike by POL's query before a transaction and by brant_pol.config_at_v1 inside one;
// ConfigGuard and ResolveConfigs, which decide the current state, answer differently. From a
// retirement on nothing published before it applies; a later publication applies again. The SQL
// function takes no lock: a publication proceeds while a transaction that asked holds open.
func TestS5RConfigAtV1(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	d := newDownstream(w)
	w.source(editor, "tax.review", "合成专业复核结论（仅供测试）")
	now := time.Now().UTC().Truncate(time.Second)
	day := func(n int) time.Time { return now.Add(time.Duration(n) * 24 * time.Hour) }
	publish := func(expected, revision int64) {
		t.Helper()
		_, err := w.c.POL.Publish(reviewer, polapi.Publish{IdempotencyKey: w.key(), Kind: polapi.KindExportTaxPolicy, ConfigID: "export.tax",
			ExpectedVersion: expected, Revision: revision, EvidenceIDs: []string{"evidence", "tax.review"}, Reason: "合成退税政策"})
		w.must(err)
	}
	// Revision 1 is valid from day -30 on; revision 2, valid from day -20 on, is published after it
	// and covers exports of the last twenty days, which it applies to although they happened before.
	_, err := w.saveConfig(editor, polapi.KindExportTaxPolicy, "export.tax", 0, day(-30), time.Time{}, exportTax(polapi.RefundExemptRefund, "tax.review"))
	w.must(err)
	publish(1, 1)
	_, err = w.saveConfig(editor, polapi.KindExportTaxPolicy, "export.tax", 2, day(-20), time.Time{}, exportTax(polapi.RefundExempt, "tax.review"))
	w.must(err)
	publish(3, 2)
	view, err := w.c.POL.GetConfig(editor, polapi.KindExportTaxPolicy, "export.tax")
	w.must(err)

	type answer struct {
		revision int64
		code     string
	}
	ask := func(id string, at time.Time) answer {
		t.Helper()
		q := polapi.ConfigAtQuery{Kind: polapi.KindExportTaxPolicy, ConfigID: id, At: at}
		r, err := w.c.POL.ConfigAt(editor, q)
		w.must(err)
		var v polapi.ConfigAtVerdict
		w.must(d.in(d.scope, func(ctx context.Context, exec bentopg.Executor) error {
			var err error
			v, err = polapi.ScanConfigAt(q, exec.QueryRow(ctx, polapi.ConfigAtSQL, polapi.ConfigAtArgs(q)...).Scan)
			return err
		}))
		if v.Ref.Revision != r.Ref.Revision || v.Code != r.Code || v.Status != r.Status || v.ContentHash != r.ContentHash {
			t.Fatalf("%s at %s: POL says %+v, the SQL function %+v", id, at, r, v)
		}
		if r.Code == "" {
			rev := view.Revisions[r.Ref.Revision-1]
			if r.ContentHash != rev.ContentHash || !reflect.DeepEqual(r.Data, rev.Data) || !r.PublishedAt.Equal(rev.PublishedAt) ||
				r.Check("/args/policy_id") != nil {
				t.Fatalf("%+v is not revision %+v", r, rev)
			}
		}
		return answer{r.Ref.Revision, r.Code}
	}
	for _, c := range []struct {
		id   string
		at   time.Time
		want answer
	}{
		{"export.tax", day(-40), answer{0, polapi.CodeConfigNotActive}},
		{"export.tax", day(-25), answer{1, ""}},
		{"export.tax", day(-10), answer{2, ""}},
		{"export.tax", now, answer{2, ""}},
		{"export.tax", day(400), answer{2, ""}},
		{"export.unknown", now, answer{0, kernel.CodeReferenceInvalid}},
	} {
		if got := ask(c.id, c.at); got != c.want {
			t.Errorf("%s at %s: %+v, want %+v", c.id, c.at, got, c.want)
		}
	}
	// ResolveConfigs (and ConfigGuard) decide whether a revision was in force: revision 2 was not
	// at day -10, published only now.
	second := polapi.ConfigRef{Kind: polapi.KindExportTaxPolicy, ConfigID: "export.tax", Revision: 2}
	resolved, err := w.c.POL.ResolveConfigs(editor, []polapi.ConfigRef{second}, day(-10))
	w.must(err)
	if resolved[second].Code != polapi.CodeConfigNotActive {
		t.Fatalf("revision 2 at day -10 resolves %+v", resolved[second])
	}
	// Another company does not know the configuration.
	q := polapi.ConfigAtQuery{Kind: polapi.KindExportTaxPolicy, ConfigID: "export.tax", At: now}
	var v polapi.ConfigAtVerdict
	w.must(d.in(w.scope("company.B"), func(ctx context.Context, exec bentopg.Executor) error {
		var err error
		v, err = polapi.ScanConfigAt(q, exec.QueryRow(ctx, polapi.ConfigAtSQL, polapi.ConfigAtArgs(q)...).Scan)
		return err
	}))
	if v.Known || code(v.Check("/x")) != kernel.CodeReferenceInvalid {
		t.Fatalf("company.B: %+v", v)
	}

	// No lock: while a transaction that asked is open, the next revision is published.
	_, err = w.saveConfig(editor, polapi.KindExportTaxPolicy, "export.tax", 4, day(-30), day(300), exportTax(polapi.RefundExemptRefund, "tax.review"))
	w.must(err)
	w.must(d.in(d.scope, func(ctx context.Context, exec bentopg.Executor) error {
		v, err := polapi.ScanConfigAt(q, exec.QueryRow(ctx, polapi.ConfigAtSQL, polapi.ConfigAtArgs(q)...).Scan)
		if err != nil || v.Ref.Revision != 2 {
			t.Errorf("before: %+v %v", v, err)
		}
		bounded, cancel := context.WithTimeout(reviewer, 20*time.Second)
		defer cancel()
		_, err = w.c.POL.Publish(bounded, polapi.Publish{IdempotencyKey: w.key(), Kind: polapi.KindExportTaxPolicy, ConfigID: "export.tax",
			ExpectedVersion: 5, Revision: 3, EvidenceIDs: []string{"evidence", "tax.review"}, Reason: "合成退税政策"})
		return err
	}))
	view, err = w.c.POL.GetConfig(editor, polapi.KindExportTaxPolicy, "export.tax")
	w.must(err)
	if got := ask("export.tax", day(-25)); got != (answer{3, ""}) {
		t.Fatalf("revision 3 is the latest published covering day -25: %+v", got)
	}
	if got := ask("export.tax", day(350)); got != (answer{2, ""}) {
		t.Fatalf("revision 3 ends at day 300: %+v", got)
	}

	// Retiring the active revision ends the configuration from then on.
	_, err = w.c.POL.Retire(reviewer, polapi.Retire{IdempotencyKey: w.key(), Kind: polapi.KindExportTaxPolicy, ConfigID: "export.tax",
		ExpectedVersion: 6, Reason: "政策停止适用（合成）"})
	w.must(err)
	view, err = w.c.POL.GetConfig(editor, polapi.KindExportTaxPolicy, "export.tax")
	w.must(err)
	if got := ask("export.tax", day(-25)); got != (answer{3, ""}) {
		t.Fatalf("before the retirement: %+v", got)
	}
	if got := ask("export.tax", day(1)); got != (answer{0, polapi.CodeConfigNotActive}) {
		t.Fatalf("after the retirement: %+v", got)
	}
}

// Revisions as POL stored them before S5R (computed by the code of 90925f3 and pinned in the
// domain test TestS5RRevisionsStoredBeforeKeepTheirDataAndHash): an export tax policy of
// not_applicable and a company profile whose country is the unassigned UK.
var s5rRows = []struct{ kind, id, data, hash, from, until string }{
	{polapi.KindExportTaxPolicy, "export.tax.s5", `{"name":"synthetic","refund_method":"not_applicable","policy_basis":"synthetic",` +
		`"professional_review_evidence_id":"tax.review"}`, "27c89f383e0cc69d93d0ddd1119ea7e4acebbbefd6ec4408ee0985febd3ce265",
		"2026-11-13T00:00:00Z", "'2026-11-14T00:00:00Z'"},
	{polapi.KindCompanyProfile, "company.A", `{"legal_name":"合成企业","registration_no":"SYN","country":"UK","address":"合成地址",` +
		`"base_currency":"CNY","time_zone":"Asia/Shanghai","policy_scope":"synthetic"}`, "772a0a1c549530267a9774793cea438344a0aa089d5a0f59a4401f758a13a9f1",
		"2026-11-13T00:00:00Z", "NULL"},
}

// POL-INV-006 (S5R-D06, D14, D15, D38): a new revision states a contract insurance percentage of at
// least 110, one of the four refund methods, officially assigned countries and programme lists
// under the list rules; revisions stored before, which a new revision could not state, read back,
// resolve, guard and apply as stored.
func TestS5RNewRevisionRulesAndStoredRevisions(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	d := newDownstream(w)
	for _, r := range s5rRows {
		w.must(w.exec("company.A", `INSERT INTO brant_pol.config_root (tenant_id, company_id, kind, config_id, root_version, last_revision,
			created_at, updated_at) VALUES (current_setting('app.tenant_id'), 'company.A', '`+r.kind+`', '`+r.id+`', 2, 1,
			'2026-11-13T07:00:00Z', '2026-11-13T08:00:00Z')`))
		w.must(w.exec("company.A", `INSERT INTO brant_pol.config_revision (tenant_id, company_id, kind, config_id, revision, status, data,
			content_hash, valid_from, valid_until, saved_by_kind, saved_by_id, saved_epoch, saved_at, published_by_kind, published_by_id,
			publish_epoch, published_at, evidence_ids) VALUES (current_setting('app.tenant_id'), 'company.A', '`+r.kind+`', '`+r.id+`', 1,
			'active', '`+r.data+`', '`+r.hash+`', '`+r.from+`', `+r.until+`, 'human', 'editor', 1, '2026-11-13T07:00:00Z', 'human',
			'reviewer', 1, '2026-11-13T08:00:00Z', ARRAY['evidence'])`))
	}
	at := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	for _, r := range s5rRows {
		ref := polapi.ConfigRef{Kind: r.kind, ConfigID: r.id, Revision: 1}
		v, err := w.c.POL.GetConfig(editor, r.kind, r.id)
		w.must(err)
		resolved, err := w.c.POL.ResolveConfigs(editor, []polapi.ConfigRef{ref}, at)
		w.must(err)
		applies, err := w.c.POL.ConfigAt(editor, polapi.ConfigAtQuery{Kind: r.kind, ConfigID: r.id, At: at})
		w.must(err)
		g, err := d.guard(w.scope("company.A"), ref, at)
		if err != nil || v.Revisions[0].ContentHash != r.hash || resolved[ref].Code != "" || resolved[ref].ContentHash != r.hash ||
			applies.Ref != ref || applies.ContentHash != r.hash || !g.InForce() || g.ContentHash != r.hash {
			t.Fatalf("%s reads back as %+v, %+v, %+v, %+v %v", r.id, v, resolved[ref], applies, g, err)
		}
		// The same data is refused as a new revision.
		_, err = w.saveConfig(editor, r.kind, r.id, 2, time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC), time.Time{}, v.Revisions[0].Data)
		want := map[string]string{polapi.KindExportTaxPolicy: "/args/data/refund_method", polapi.KindCompanyProfile: "/args/data/country"}[r.kind]
		if code(err) != kernel.CodeInvalidKnownValue || path(err) != want {
			t.Fatalf("%s saved again: %v", r.id, err)
		}
	}
	if v, _ := w.c.POL.GetConfig(editor, polapi.KindExportTaxPolicy, "export.tax.s5"); v.Revisions[0].Data.ExportTaxPolicy.RefundMethod != polapi.RefundNotApplicable {
		t.Fatalf("not_applicable reads back: %+v", v)
	}

	win := newWindow()
	cif := func(percent string) polapi.ConfigData {
		return polapi.ConfigData{TradeTerms: &polapi.TradeTerms{Incoterm: "CIF", Edition: polapi.IncotermsEdition2020, NamedPlace: "DEHAM",
			ResponsibilityNote: "synthetic CIF", Delivery: polapi.Delivery{Kind: polapi.DeliveryFixedDate, Date: "2027-01-31"}, InsurancePercent: percent}}
	}
	_, err := w.saveConfig(editor, polapi.KindTradeTerms, "cif.low", 0, win.from, time.Time{}, cif("105"))
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/data/insurance_percent" {
		t.Fatalf("105 %%: %v", err)
	}
	trade := w.configure(editor, reviewer, polapi.KindTradeTerms, "cif", 0, win.from, time.Time{}, cif("120"), true)
	plain := w.configure(editor, reviewer, polapi.KindTradeTerms, "cif.plain", 0, win.from, time.Time{}, cif(""), true)
	resolved, err := w.c.POL.ResolveConfigs(editor, []polapi.ConfigRef{trade, plain}, time.Now())
	w.must(err)
	if p, err := resolved[trade].Data.TradeTerms.ContractInsurancePercent(); err != nil || p.String() != "120" {
		t.Fatalf("contract insurance %v %v", p, err)
	}
	if p, err := resolved[plain].Data.TradeTerms.ContractInsurancePercent(); err != nil || p.String() != "110" ||
		resolved[plain].Data.TradeTerms.InsurancePercent != "" {
		t.Fatalf("default insurance %v %v", p, err)
	}

	w.source(editor, "tax.review", "合成专业复核结论（仅供测试）")
	for _, method := range []string{polapi.RefundExempt, polapi.RefundTaxable} {
		ref := w.configure(editor, reviewer, polapi.KindExportTaxPolicy, "export."+method, 0, win.from, time.Time{}, exportTax(method, "tax.review"), false)
		_, err = w.c.POL.Publish(reviewer, polapi.Publish{IdempotencyKey: w.key(), Kind: ref.Kind, ConfigID: ref.ConfigID, ExpectedVersion: 1,
			Revision: 1, EvidenceIDs: []string{"evidence", "tax.review"}, Reason: "合成退税方式"})
		w.must(err)
	}
	_, err = w.saveConfig(editor, polapi.KindExportTaxPolicy, "export.na", 0, win.from, time.Time{}, exportTax(polapi.RefundNotApplicable, "tax.review"))
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/data/refund_method" {
		t.Fatalf("not_applicable on a new revision: %v", err)
	}

	w.source(editor, "legal.eu", "合成法律确认（仅供测试）")
	screening := w.configure(editor, reviewer, polapi.KindScreeningRegulation, "screening", 0, win.from, time.Time{}, polapi.ConfigData{
		ScreeningRegulation: &polapi.ScreeningRegulation{Lists: []polapi.ScreeningList{{ListCode: "cn.uel", Jurisdiction: polapi.JurisdictionCN}},
			RescreenIntervalDays: 30, Programmes: []polapi.ScreeningList{{ListCode: cnProgrammeList, Jurisdiction: polapi.JurisdictionCN},
				{ListCode: "eu.programmes", Jurisdiction: polapi.JurisdictionForeign, LegalConfirmationEvidenceID: "legal.eu"}}}}, false)
	_, err = w.c.POL.Publish(reviewer, polapi.Publish{IdempotencyKey: w.key(), Kind: screening.Kind, ConfigID: screening.ConfigID, ExpectedVersion: 1,
		Revision: 1, EvidenceIDs: []string{"evidence"}, Reason: "合规法规"})
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/evidence_ids" {
		t.Fatalf("an uncited legal confirmation of a programme list: %v", err)
	}
	_, err = w.c.POL.Publish(reviewer, polapi.Publish{IdempotencyKey: w.key(), Kind: screening.Kind, ConfigID: screening.ConfigID, ExpectedVersion: 1,
		Revision: 1, EvidenceIDs: []string{"evidence", "legal.eu"}, Reason: "合规法规"})
	w.must(err)
	resolved, err = w.c.POL.ResolveConfigs(editor, []polapi.ConfigRef{screening}, time.Now())
	w.must(err)
	if p := resolved[screening].Data.ScreeningRegulation.Programmes; len(p) != 2 || p[0].ListCode != cnProgrammeList ||
		p[1].LegalConfirmationEvidenceID != "legal.eu" {
		t.Fatalf("programme lists %+v", p)
	}
}

// S5R-D36 (UL PTY-ServicePartyRole; R44, R73, R77): an inspection agency, a customs broker and an
// insurer are verified without a route and resolve through ServiceRoleRef v1; a forwarder needs
// one, which the database requires as well. S5R-D40: a notify address is kept with the confirmed
// revision and resolves through AddressRoleRef v1 and AddressRoleGuard v1.
func TestS5RServiceRolesWithoutRouteAndNotifyAddress(t *testing.T) {
	w := newWorld(t)
	editor, _ := w.polActors()
	w.staffParty(editor, "service.1", []string{ptyapi.RoleSupplier})
	from, until := time.Now().Add(-time.Hour), time.Now().Add(30*24*time.Hour)
	verify := func(expected int64, id, role, origin, destination string) (ptyapi.Receipt, error) {
		return w.c.PTY.VerifyServiceRole(editor, ptyapi.VerifyServiceRole{IdempotencyKey: w.key(), PartyID: "service.1", ExpectedVersion: expected,
			ServiceRoleID: id, Role: role, ServiceCode: id + ".service", Origin: origin, Destination: destination,
			ValidFrom: kernel.FormatTimestamp(from), ValidUntil: kernel.FormatTimestamp(until), EvidenceIDs: []string{"evidence"}})
	}
	var refs []ptyapi.ServiceRoleRef
	for i, role := range []string{ptyapi.ServiceInspectionAgency, ptyapi.ServiceCustomsBroker, ptyapi.ServiceInsurer} {
		r, err := verify(int64(2+i), role, role, "", "")
		w.must(err)
		if r.RowVersion != int64(3+i) || r.Status != ptyapi.StatusVerified {
			t.Fatalf("%s: %+v", role, r)
		}
		refs = append(refs, ptyapi.ServiceRoleRef{PartyID: "service.1", ServiceRoleID: role, Revision: 1})
	}
	if _, err := verify(5, "sea", ptyapi.ServiceForwarder, "", "DEHAM"); code(err) != kernel.CodeSchemaInvalid || path(err) != "/args/origin" {
		t.Fatalf("a forwarder without a route: %v", err)
	}
	r, err := verify(5, "sea", ptyapi.ServiceForwarder, "CNSHA", "DEHAM")
	w.must(err)
	refs = append(refs, ptyapi.ServiceRoleRef{PartyID: "service.1", ServiceRoleID: "sea", Revision: 1})
	resolved, err := w.c.PTY.ResolveServiceRoles(editor, refs, time.Now())
	w.must(err)
	for i, role := range []string{ptyapi.ServiceInspectionAgency, ptyapi.ServiceCustomsBroker, ptyapi.ServiceInsurer, ptyapi.ServiceForwarder} {
		got := resolved[refs[i]]
		route := role == ptyapi.ServiceForwarder
		if got.Role != role || got.Code != "" || got.Check("/args/agency") != nil || (got.Origin != "") != route || (got.Destination != "") != route {
			t.Errorf("%s resolves as %+v", role, got)
		}
	}
	party, err := w.c.PTY.GetParty(editor, "service.1")
	w.must(err)
	if len(party.ServiceRoles) != 4 || party.RowVersion != r.RowVersion {
		t.Fatalf("party %+v", party)
	}
	err = w.exec("company.A", `INSERT INTO brant_pty.service_role (tenant_id, company_id, party_id, service_role_id, revision, party_revision,
		role, service_code, origin, destination, valid_from, valid_until, evidence_ids, verified_by_kind, verified_by_id, authority_epoch,
		verified_at) VALUES (current_setting('app.tenant_id'), 'company.A', 'service.1', 'air', 1, 1, 'carrier', 'air', NULL, NULL,
		now(), now() + interval '1 day', ARRAY['evidence'], 'human', 'editor', 1, now())`)
	if err == nil || !strings.Contains(err.Error(), "service_role_route_check") {
		t.Fatalf("a carrier without a route in the database: %v", err)
	}

	buyer := ptyapi.Address{AddressID: "hq", Role: ptyapi.AddressBuyer, Address: "合成买方地址", Country: "DE"}
	notify := ptyapi.Address{AddressID: "notify", Role: ptyapi.AddressNotify, Address: "合成通知方地址", Country: "NL"}
	w.staffParty(editor, "customer.1", []string{ptyapi.RoleCustomer}, buyer, notify)
	ref := ptyapi.AddressRoleRef{PartyID: "customer.1", PartyRevision: 1, AddressID: "notify"}
	got, err := w.c.PTY.ResolveAddressRoles(editor, []ptyapi.AddressRoleRef{ref})
	w.must(err)
	if a := got[ref]; a.Role != ptyapi.AddressNotify || a.Country != "NL" || a.Code != "" {
		t.Fatalf("notify address %+v", a)
	}
	d := newDownstream(w)
	w.must(d.in(d.scope, func(ctx context.Context, exec bentopg.Executor) error {
		v, err := ptyapi.ScanAddressRoleGuard(ref, exec.QueryRow(ctx, ptyapi.AddressRoleGuardSQL, ptyapi.AddressRoleGuardArgs(ref)...).Scan)
		if err == nil && (!v.Current() || v.Role != ptyapi.AddressNotify) {
			t.Errorf("guarded notify address %+v", v)
		}
		return err
	}))
}

// S5R-D21 (UL PTY-TradingProfile; R43 TradingProfileRef v1): a trading profile lists permitted
// payers, confirmed other parties the editor sees; the publication verifies them; they resolve with
// the profile and never make it stale; they change only with a working revision; a profile without
// payers stores none.
func TestS5RTradingProfilePermittedPayers(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	payment, trade := w.settlement(editor, reviewer)
	w.staffParty(editor, "customer.1", []string{ptyapi.RoleCustomer})
	w.staffParty(editor, "payer.1", []string{ptyapi.RoleCustomer})
	_, err := w.c.PTY.SaveParty(editor, ptyapi.SaveParty{IdempotencyKey: w.key(), PartyID: "payer.draft", Name: "payer.draft",
		Roles: []string{ptyapi.RoleCustomer}})
	w.must(err)
	save := func(expected int64, payers ...string) (ptyapi.Receipt, error) {
		return w.c.PTY.SaveTradingProfile(editor, ptyapi.SaveTradingProfile{IdempotencyKey: w.key(), PartyID: "customer.1", ExpectedVersion: expected,
			PartyRevision: 1, PaymentTerms: payment, TradeTerms: trade, Owner: ptyapi.OwnerRef{ActorID: "sales.owner", TeamID: "trade"},
			PermittedPayers: payers})
	}
	for _, c := range []struct {
		payers []string
		want   string
	}{
		{[]string{"payer.1", "customer.1"}, kernel.CodeInvalidKnownValue + " /args/permitted_payers/1"},
		{[]string{"payer.draft"}, kernel.CodeReferenceInvalid + " /args/permitted_payers/0"},
		{[]string{"payer.1", "nobody"}, kernel.CodeReferenceInvalid + " /args/permitted_payers/1"},
	} {
		if _, err := save(0, c.payers...); code(err)+" "+path(err) != c.want {
			t.Errorf("%v: %v, want %s", c.payers, err, c.want)
		}
	}
	saved, err := save(0, "payer.1")
	w.must(err)
	_, err = w.publishProfile(reviewer, "customer.1", saved.RowVersion, 1)
	w.must(err)
	current := ptyapi.TradingProfileRef{PartyID: "customer.1"}
	profiles, err := w.c.PTY.ResolveTradingProfiles(editor, []ptyapi.TradingProfileRef{current})
	w.must(err)
	if p := profiles[current]; p.Code != "" || !slices.Equal(p.PermittedPayers, []string{"payer.1"}) || p.Ref.Revision != 1 {
		t.Fatalf("resolved %+v", p)
	}
	// A payer that changes does not make the profile stale; STL reads the payer's PartyRef itself.
	_, err = w.c.PTY.ReviseParty(editor, ptyapi.ReviseParty{IdempotencyKey: w.key(), PartyID: "payer.1", ExpectedVersion: 2, Reason: "地址变更"})
	w.must(err)
	profiles, err = w.c.PTY.ResolveTradingProfiles(editor, []ptyapi.TradingProfileRef{current})
	w.must(err)
	if p := profiles[current]; p.Code != "" || p.Check("/args/profile") != nil {
		t.Fatalf("a revised payer: %+v", p)
	}
	err = w.exec("company.A", `INSERT INTO brant_pty.trading_profile_payer VALUES (current_setting('app.tenant_id'), 'company.A', 'customer.1', 1, 'payer.draft')`)
	if err == nil || !strings.Contains(err.Error(), "only while the revision is working") {
		t.Fatalf("a payer added to a published revision: %v", err)
	}
	// The next revision without payers lists none; its view and the database hold none.
	saved, err = save(2)
	w.must(err)
	view, err := w.c.PTY.GetTradingProfile(editor, "customer.1")
	w.must(err)
	if len(view.Revisions) != 2 || !slices.Equal(view.Revisions[0].PermittedPayers, []string{"payer.1"}) || view.Revisions[1].PermittedPayers != nil ||
		view.Revisions[0].ContentHash == view.Revisions[1].ContentHash {
		t.Fatalf("view %+v", view)
	}
	// A payer that is no longer confirmed is not saved again.
	_, err = save(saved.RowVersion, "payer.1")
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/permitted_payers/0" {
		t.Fatalf("a payer revised into a draft: %v", err)
	}
}

// S5R-D15(a) (UL PLATFORM-CountryProgrammeVersion): a programme lists the regions of its countries
// it is limited to; they resolve with every hit, a programme without regions keeps the content hash
// it had, and the database keeps only the canonical form. The synthetic regions name uninhabited
// territories (test data, not a legal list).
func TestS5RCountryProgrammeRegions(t *testing.T) {
	p := newProgrammeWorld(t)
	_, err := p.importProgrammes(p.compliance, syntheticCountryProgrammes(cnProgrammeList, "v1", "2026-11-01"))
	p.must(err)
	second := syntheticCountryProgrammes(cnProgrammeList, "v2", "2026-11-13")
	second.Programmes[1].Regions = []string{"BV-01", "AQ-02"}
	bad := second
	bad.Programmes = slices.Clone(second.Programmes)
	bad.Programmes[1].Regions = []string{"HM-01"}
	if _, err = p.importProgrammes(p.compliance, bad); code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/programmes/1/regions/0" {
		t.Fatalf("a region of another country: %v", err)
	}
	_, err = p.importProgrammes(p.compliance, second)
	p.must(err)
	resolved, err := p.c.REF.ResolveCountryProgrammes(p.reader, refapi.ResolveProgrammes{Lists: []refapi.ListVersionRef{{ListCode: cnProgrammeList}},
		Countries: []string{"AQ", "BV"}})
	p.must(err)
	var regional, whole int
	for _, h := range resolved.Hits {
		switch h.Programme.ProgrammeID {
		case "SYN-CN-EXP-1":
			regional++
			if !slices.Equal(h.Programme.Regions, []string{"AQ-02", "BV-01"}) || !slices.Equal(h.Programme.RegionsOf(h.Country), []string{h.Country + "-0" +
				map[string]string{"AQ": "2", "BV": "1"}[h.Country]}) {
				t.Errorf("hit %+v", h)
			}
		case "SYN-CN-EMB-1":
			whole++
			if h.Programme.Regions != nil || h.Programme.RegionsOf(h.Country) != nil {
				t.Errorf("hit %+v", h)
			}
		}
	}
	if regional != 2 || whole != 1 || resolved.Versions[cnProgrammeList].Version != "v2" {
		t.Fatalf("hits %+v", resolved)
	}
	v1, err := p.c.REF.GetProgrammeListVersion(p.compliance, cnProgrammeList, "v1")
	p.must(err)
	v2, err := p.c.REF.GetProgrammeListVersion(p.compliance, cnProgrammeList, "v2")
	p.must(err)
	if v1.Programmes[0].ContentHash != v2.Programmes[0].ContentHash || v1.Programmes[1].ContentHash == v2.Programmes[1].ContentHash ||
		v1.Programmes[1].Regions != nil || v1.ContentHash == v2.ContentHash {
		t.Fatalf("v1 %+v, v2 %+v", v1.Programmes, v2.Programmes)
	}
	err = p.exec("company.A", `INSERT INTO brant_ref.country_programme (tenant_id, company_id, list_code, version, programme_id, name, measure,
		countries, regions, content_hash) VALUES (current_setting('app.tenant_id'), 'company.A', '`+cnProgrammeList+`', 'v2', 'SYN-X', 'x',
		'embargo', ARRAY['AQ', 'BV'], ARRAY['BV-01', 'AQ-02'], repeat('0', 64))`)
	if err == nil || !strings.Contains(err.Error(), "country_programme_regions_check") {
		t.Fatalf("regions out of order in the database: %v", err)
	}
}
