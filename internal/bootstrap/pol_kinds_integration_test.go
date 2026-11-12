//go:build integration

package bootstrap_test

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The second batch of configuration kinds (S1-04) and the Python tests it unlocks.

func taxRef(id string) polapi.ConfigRef {
	return polapi.ConfigRef{Kind: polapi.KindTaxProfile, ConfigID: id, Revision: 1}
}

func entryRefs(s polapi.SnapshotView) []polapi.ConfigRef {
	var refs []polapi.ConfigRef
	for _, e := range s.Entries {
		refs = append(refs, e.Ref)
	}
	return refs
}

// Python test_foundation.py test_reference_deactivation_blocks_new_work_but_preserves_snapshot: a
// snapshot keeps the exact revisions and hashes it adopted, with every dependency, after the rate
// is retired, which blocks new use.
func TestReferenceDeactivationBlocksNewWorkButPreservesSnapshot(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	win := newWindow()
	usd, cny, policy, fx := w.fxSetup(editor, reviewer, win)
	company := w.company(editor, reviewer, win)
	r, err := w.c.POL.CaptureSnapshot(editor, polapi.CaptureSnapshot{IdempotencyKey: w.key(), SnapshotID: "old", Purpose: polapi.PurposeCostEstimate,
		Refs: []polapi.ConfigRef{company, policy, fx}})
	w.must(err)
	if r.Status != polapi.StatusCaptured || r.ObjectID != "old" {
		t.Fatalf("capture %+v", r)
	}
	_, err = w.c.POL.CaptureSnapshot(editor, polapi.CaptureSnapshot{IdempotencyKey: w.key(), SnapshotID: "old", Purpose: polapi.PurposeCostEstimate,
		Refs: []polapi.ConfigRef{usd}})
	w.want(err, polapi.CodeBusinessKeyConflict)
	_, err = w.c.POL.CaptureSnapshot(editor, polapi.CaptureSnapshot{IdempotencyKey: w.key(), SnapshotID: "quote", Purpose: polapi.PurposeQuotation,
		Refs: []polapi.ConfigRef{company, policy, fx}})
	if code(err) != polapi.CodeFXScopeMismatch || path(err) != "/args/refs/2" {
		t.Fatalf("a cost estimate rate in a quotation snapshot: %v", err)
	}
	_, err = w.c.POL.Retire(reviewer, polapi.Retire{IdempotencyKey: w.key(), Kind: polapi.KindFXRate, ConfigID: fx.ConfigID, ExpectedVersion: 2, Reason: "撤销来源"})
	w.must(err)
	_, err = w.c.POL.LookupFX(editor, withPolicy(fxQuery("USD", "CNY", polapi.PurposeCostEstimate, &fx), policy))
	w.want(err, polapi.CodeConfigNotActive)
	s, err := w.c.POL.GetSnapshot(editor, "old")
	w.must(err)
	for _, e := range s.Entries {
		if len(e.ContentHash) != 64 {
			t.Fatalf("entry %+v", e)
		}
	}
	if want := []polapi.ConfigRef{company, cny, usd, fx, policy, taxRef("tax")}; s.Purpose != polapi.PurposeCostEstimate || len(s.ContentHash) != 64 ||
		!slices.Equal(entryRefs(s), want) {
		t.Fatalf("snapshot %+v, want entries %v", s, want)
	}
	if err = w.exec("company.A", `DELETE FROM brant_pol.snapshot_entry`); err == nil {
		t.Fatal("snapshots are append-only")
	}
}

// Python test_foundation.py test_historical_snapshot_survives_rate_revision.
func TestHistoricalSnapshotSurvivesRateRevision(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	win := newWindow()
	_, _, policy, fx := w.fxSetup(editor, reviewer, win)
	company := w.company(editor, reviewer, win)
	_, err := w.c.POL.CaptureSnapshot(editor, polapi.CaptureSnapshot{IdempotencyKey: w.key(), SnapshotID: "snapshot.1",
		Purpose: polapi.PurposeCostEstimate, Refs: []polapi.ConfigRef{fx, policy, company}})
	w.must(err)
	before, err := w.c.POL.GetSnapshot(editor, "snapshot.1")
	w.must(err)
	old, err := w.c.POL.GetConfig(editor, polapi.KindFXRate, fx.ConfigID)
	w.must(err)
	data := old.Revisions[0].Data
	data.FXRate.Rate = "7.500000"
	revised := w.configure(editor, reviewer, polapi.KindFXRate, fx.ConfigID, 2, win.from, win.until, data, true)
	after, err := w.c.POL.GetSnapshot(editor, "snapshot.1")
	w.must(err)
	if !reflect.DeepEqual(before, after) || revised.Revision != 2 {
		t.Fatalf("before %+v, after %+v, revised %+v", before, after, revised)
	}
	_, err = w.c.POL.CaptureSnapshot(editor, polapi.CaptureSnapshot{IdempotencyKey: w.key(), SnapshotID: "snapshot.2",
		Purpose: polapi.PurposeCostEstimate, Refs: []polapi.ConfigRef{fx}})
	w.want(err, polapi.CodeConfigNotActive)
	v, err := w.c.POL.GetConfig(editor, polapi.KindFXRate, fx.ConfigID)
	w.must(err)
	if v.Revisions[0].Data.FXRate.Rate != "7.123456" || v.Revisions[0].Status != polapi.StatusSuperseded || v.ActiveRevision != 2 {
		t.Fatalf("history %+v", v)
	}
}

// Python test_foundation.py test_snapshot_requires_company_policy_and_unknown_tax_blocks. A bank
// account is never adopted (Python SENSITIVE).
func TestSnapshotRequiresCompanyPolicyAndUnknownTaxBlocks(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	win := newWindow()
	_, _, policy, fx := w.fxSetup(editor, reviewer, win)
	_, err := w.c.POL.CaptureSnapshot(editor, polapi.CaptureSnapshot{IdempotencyKey: w.key(), SnapshotID: "incomplete",
		Purpose: polapi.PurposeCostEstimate, Refs: []polapi.ConfigRef{fx}})
	w.want(err, kernel.CodeDraftNotReady)
	company := w.company(editor, reviewer, win)
	unknown := w.tax(editor, reviewer, polapi.RecoveryUnknown, "unknown.tax", win)
	_, err = w.c.POL.CaptureSnapshot(editor, polapi.CaptureSnapshot{IdempotencyKey: w.key(), SnapshotID: "unknown",
		Purpose: polapi.PurposeCostEstimate, Refs: []polapi.ConfigRef{company, policy, unknown}})
	if code(err) != polapi.CodeTaxPolicyUnconfirmed || path(err) != "/args/refs/2" {
		t.Fatalf("unknown recovery: %v", err)
	}
	bank := w.configure(editor, reviewer, polapi.KindBankAccount, "bank", 0, win.from, time.Time{}, bankData(polapi.OwnerCompany, "company.A", "1234567890"), true)
	_, err = w.c.POL.CaptureSnapshot(editor, polapi.CaptureSnapshot{IdempotencyKey: w.key(), SnapshotID: "bank",
		Purpose: polapi.PurposeCostEstimate, Refs: []polapi.ConfigRef{company, policy, bank}})
	if code(err) != kernel.CodePermissionDenied || path(err) != "/args/refs/2" {
		t.Fatalf("bank account: %v", err)
	}
}

// Python test_foundation.py test_snapshot_read_rechecks_sources_without_revalidating_expiry: a
// snapshot reads back unchanged after a rate it adopted expired. Snapshots belong to the company
// (S1-D15), so a reader whose data scope narrows to its own records still reads it, and evidence is
// not re-checked on reads (S1-D16): those halves are not ported.
func TestSnapshotReadDoesNotRevalidateExpiry(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	win := newWindow()
	w.currency(editor, reviewer, "USD", win)
	w.currency(editor, reviewer, "CNY", win)
	tax := w.tax(editor, reviewer, polapi.RecoveryNonrecoverable, "tax", win)
	policy := w.configure(editor, reviewer, polapi.KindPricingPolicy, "policy", 0, win.from, time.Time{}, policyData(tax, 86400), true)
	expires := time.Now().Add(3 * time.Second).Truncate(time.Millisecond)
	fx := w.configure(editor, reviewer, polapi.KindFXRate, "usd.cny", 0, win.from, expires, fxData("7.123456", polapi.PurposeCostEstimate, win.from), true)
	company := w.company(editor, reviewer, win)
	_, err := w.c.POL.CaptureSnapshot(editor, polapi.CaptureSnapshot{IdempotencyKey: w.key(), SnapshotID: "history",
		Purpose: polapi.PurposeCostEstimate, Refs: []polapi.ConfigRef{company, policy, fx}})
	w.must(err)
	peer := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: actorRef("human", "peer"), Permissions: allPermissions,
		DataScope: "company"})
	captured, err := w.c.POL.GetSnapshot(peer, "history")
	w.must(err)
	time.Sleep(time.Until(expires) + 10*time.Millisecond)
	_, err = w.c.POL.LookupFX(editor, fxQuery("USD", "CNY", polapi.PurposeCostEstimate, &fx))
	w.want(err, polapi.CodeFXStale)
	later, err := w.c.POL.GetSnapshot(editor, "history")
	w.must(err)
	if !reflect.DeepEqual(captured, later) || later.Purpose != polapi.PurposeCostEstimate {
		t.Fatalf("captured %+v, read after expiry %+v", captured, later)
	}
}

// Python test_foundation.py test_unit_conversion_is_sku_scoped_and_exact, and the conversion half
// of test_master_revision_invalidates_old_sku_profile_for_new_conversion: a conversion verified for
// one SKU specification revision does not apply to another (the caller reads the current one from
// CAT). Python answers that case REFERENCE_INVALID through its sku_profile kind; here it is
// UNIT_SCOPE_MISMATCH.
func TestUnitConversionIsSkuScopedAndExact(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	win := newWindow()
	unit := func(code string) polapi.ConfigData {
		return polapi.ConfigData{Unit: &polapi.Unit{Code: code, Name: code, Dimension: polapi.DimensionCount, Precision: 0}}
	}
	w.configure(editor, reviewer, polapi.KindUnit, "PCS", 0, win.from, time.Time{}, unit("PCS"), true)
	w.configure(editor, reviewer, polapi.KindUnit, "DOZEN", 0, win.from, time.Time{}, unit("DOZEN"), true)
	w.configure(editor, reviewer, polapi.KindUnit, "KG", 0, win.from, time.Time{},
		polapi.ConfigData{Unit: &polapi.Unit{Code: "KG", Name: "千克", Dimension: polapi.DimensionMass, Precision: 3}}, true)
	conversionData := func(to string) polapi.ConfigData {
		return polapi.ConfigData{UnitConversion: &polapi.UnitConversion{SkuID: "sku", SpecRevision: 1, FromUnit: "DOZEN", ToUnit: to, Factor: "12",
			Basis: "test SKU TECH.r1 packaging evidence"}}
	}
	_, err := w.saveConfig(editor, polapi.KindUnitConversion, "dozen.kg", 0, win.from, time.Time{}, conversionData("KG"))
	if code(err) != polapi.CodeUnitScopeMismatch || path(err) != "/args/data/to_unit" {
		t.Fatalf("count into mass: %v", err)
	}
	conversion := w.configure(editor, reviewer, polapi.KindUnitConversion, "dozen.pcs", 0, win.from, win.until, conversionData("PCS"), true)
	q := polapi.UnitQuery{Conversion: conversion, SkuID: "sku", SpecRevision: 1, At: time.Now()}
	c, err := w.c.POL.LookupUnitConversion(editor, q)
	w.must(err)
	two, _ := kernel.ParseQuantity("2", "DOZEN")
	pcs, err := c.Convert(two)
	w.must(err)
	if pcs.String() != "24 PCS" || c.Conversion != conversion || len(c.ContentHash) != 64 {
		t.Fatalf("2 DOZEN = %s by %+v", pcs, c)
	}
	other := q
	other.SkuID = "other"
	_, err = w.c.POL.LookupUnitConversion(editor, other)
	if code(err) != polapi.CodeUnitScopeMismatch || path(err) != "/sku_id" {
		t.Fatalf("another SKU: %v", err)
	}
	revised := q
	revised.SpecRevision = 2
	_, err = w.c.POL.LookupUnitConversion(editor, revised)
	if code(err) != polapi.CodeUnitScopeMismatch || path(err) != "/spec_revision" {
		t.Fatalf("a revised specification: %v", err)
	}
	tenth, _ := kernel.ParseQuantity("0.1", "DOZEN")
	_, err = c.Convert(tenth)
	w.want(err, kernel.CodeInvalidKnownValue)
}

// Python test_foundation.py test_tax_split_is_exact_and_unknown_recovery_blocks.
func TestTaxSplitIsExactAndUnknownRecoveryBlocks(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	win := newWindow()
	w.currency(editor, reviewer, "CNY", win)
	tax := w.tax(editor, reviewer, polapi.RecoveryNonrecoverable, "tax", win)
	rate, err := w.c.POL.LookupTax(editor, polapi.TaxQuery{Profile: tax, Currency: "CNY", At: time.Now()})
	w.must(err)
	split, err := rate.Split(kernel.MustDecimal("113"))
	w.must(err)
	if split.Net.String() != "100.00 CNY" || split.Tax.String() != "13.00 CNY" || split.Gross.String() != "113.00 CNY" || rate.Profile != tax {
		t.Fatalf("split %+v by %+v", split, rate)
	}
	unknown := w.tax(editor, reviewer, polapi.RecoveryUnknown, "tax.unknown", win)
	_, err = w.c.POL.LookupTax(editor, polapi.TaxQuery{Profile: unknown, Currency: "CNY", At: time.Now()})
	if code(err) != polapi.CodeTaxPolicyUnconfirmed || path(err) != "/profile" {
		t.Fatalf("unknown recovery: %v", err)
	}
	_, err = w.c.POL.LookupTax(editor, polapi.TaxQuery{Profile: tax, Currency: "USD", At: time.Now()})
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/currency" {
		t.Fatalf("unconfigured currency: %v", err)
	}
}

// Python test_foundation.py test_number_and_plaintext_template_bind_snapshot (template half; the
// number half is TestSameBusinessReferenceKeepsItsNumber, rendering into a file is not POL's).
func TestPlainTextTemplateNamesFixedFieldsOnly(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	win := newWindow()
	text := "报价 {document.number}：{customer.name}，{quote.total} {quote.currency}"
	template := w.configure(editor, reviewer, polapi.KindDocumentTemplate, "quote.template", 0, win.from, time.Time{},
		polapi.ConfigData{DocumentTemplate: &polapi.DocumentTemplate{Name: "quote test", DocumentType: polapi.DocumentTypeQuote, TemplateText: text,
			FieldNames: []string{"document.number", "customer.name", "quote.total", "quote.currency"}}}, true)
	v, err := w.c.POL.GetConfig(editor, polapi.KindDocumentTemplate, template.ConfigID)
	w.must(err)
	if v.Revisions[0].Data.DocumentTemplate.TemplateText != text || v.ActiveRevision != 1 {
		t.Fatalf("template %+v", v)
	}
	_, err = w.saveConfig(editor, polapi.KindDocumentTemplate, "unsafe", 0, win.from, time.Time{},
		polapi.ConfigData{DocumentTemplate: &polapi.DocumentTemplate{Name: "unsafe", DocumentType: polapi.DocumentTypeQuote,
			TemplateText: "{customer.name.__class__}", FieldNames: []string{"customer.name"}}})
	w.want(err, polapi.CodeTemplateInvalid)
	sections := polapi.QuoteSections()
	slices.Reverse(sections)
	quote := w.configure(editor, reviewer, polapi.KindQuoteDocumentTemplate, "formal.quote", 0, win.from, time.Time{},
		polapi.ConfigData{QuoteDocumentTemplate: &polapi.QuoteDocumentTemplate{Name: "formal", Language: polapi.LanguageZhCN,
			RendererVersion: polapi.QuoteRendererVersion, Title: "报价单", Sections: sections, FooterNote: "合成测试，不构成要约"}}, true)
	q, err := w.c.POL.GetConfig(editor, polapi.KindQuoteDocumentTemplate, quote.ConfigID)
	w.must(err)
	if !slices.Equal(q.Revisions[0].Data.QuoteDocumentTemplate.Sections, sections) {
		t.Fatalf("the section order is the rendering order: %+v", q.Revisions[0].Data.QuoteDocumentTemplate)
	}
}

func bankData(ownerKind, ownerID, number string) polapi.ConfigData {
	return polapi.ConfigData{BankAccount: &polapi.BankAccount{OwnerKind: ownerKind, OwnerID: ownerID, BankName: "synthetic bank",
		AccountNumber: number, Currency: "CNY", Purpose: polapi.AccountReceipt}}
}

// Python test_foundation.py test_sensitive_account_field_requires_separate_grant.
func TestSensitiveAccountFieldRequiresSeparateGrant(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	win := newWindow()
	w.currency(editor, reviewer, "CNY", win)
	bank := w.configure(editor, reviewer, polapi.KindBankAccount, "bank", 0, win.from, time.Time{}, bankData(polapi.OwnerCompany, "company.A", "1234567890"), true)
	reader := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: actorRef("human", "reader"), Permissions: []string{"pol.read"},
		DataScope: "company"})
	masked, err := w.c.POL.GetConfig(reader, polapi.KindBankAccount, "bank")
	w.must(err)
	whole, err := w.c.POL.GetConfig(editor, polapi.KindBankAccount, "bank")
	w.must(err)
	if masked.Revisions[0].Data.BankAccount.AccountNumber != "******7890" || whole.Revisions[0].Data.BankAccount.AccountNumber != "1234567890" {
		t.Fatalf("reader %+v, editor %+v", masked.Revisions[0].Data.BankAccount, whole.Revisions[0].Data.BankAccount)
	}
	resolved, err := w.c.POL.ResolveConfigs(reader, []polapi.ConfigRef{bank}, time.Now())
	w.must(err)
	if resolved[bank].Data.BankAccount.AccountNumber != "******7890" || resolved[bank].Code != "" {
		t.Fatalf("resolved %+v", resolved[bank])
	}
	ai := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: actorRef("ai", "agent"), Permissions: allPermissions, DataScope: "company"})
	agent, err := w.c.POL.GetConfig(ai, polapi.KindBankAccount, "bank")
	w.must(err)
	if agent.Revisions[0].Data.BankAccount.AccountNumber != "******7890" {
		t.Fatal("an AI agent never reads a whole account number")
	}
	_, err = w.saveConfig(editor, polapi.KindBankAccount, "foreign", 0, win.from, time.Time{}, bankData(polapi.OwnerCompany, "company.B", "1234567890"))
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/data/owner_id" {
		t.Fatalf("another company's account: %v", err)
	}
}

// Python test_foundation.py test_account_sensitive_permission_does_not_replace_owner_scope. POL
// cannot ask PTY who owns a party (PTY is not upstream of POL), so a party's account is visible only
// to readers whose data scope is the whole company.
func TestAccountSensitivePermissionDoesNotReplaceOwnerScope(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	win := newWindow()
	w.currency(editor, reviewer, "CNY", win)
	account := w.configure(editor, reviewer, polapi.KindBankAccount, "private.bank", 0, win.from, time.Time{},
		bankData(polapi.OwnerParty, "customer", "SECRET12345678"), true)
	grant := bootstrap.Grant{Scope: w.scope("company.A"), Actor: actorRef("human", "private.reader"), Permissions: allPermissions, DataScope: "company"}
	reader := w.provision(grant)
	v, err := w.c.POL.GetConfig(reader, polapi.KindBankAccount, account.ConfigID)
	w.must(err)
	if v.Revisions[0].Data.BankAccount.AccountNumber != "SECRET12345678" {
		t.Fatalf("%+v", v.Revisions[0].Data.BankAccount)
	}
	grant.DataScope = "own"
	reader = w.provision(grant)
	_, err = w.c.POL.GetConfig(reader, polapi.KindBankAccount, account.ConfigID)
	w.want(err, kernel.CodeNotFound)
	resolved, err := w.c.POL.ResolveConfigs(reader, []polapi.ConfigRef{account}, time.Now())
	w.must(err)
	if len(resolved) != 0 {
		t.Fatalf("resolved %+v", resolved)
	}
}

// Python test_foundation.py test_fee_classification_preserves_inclusion_and_side.
func TestFeeClassificationPreservesInclusionAndSide(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	win := newWindow()
	fee := w.configure(editor, reviewer, polapi.KindFeeCategory, "packaging", 0, win.from, time.Time{},
		polapi.ConfigData{FeeCategory: &polapi.FeeCategory{Name: "包装", Category: "packaging", Side: polapi.FeeSidePurchaseCost,
			Inclusion: polapi.InclusionIncludedInLine, AllocationBasis: polapi.AllocationQuantity, PolicyBasis: "synthetic cost scope"}}, true)
	v, err := w.c.POL.GetConfig(editor, polapi.KindFeeCategory, fee.ConfigID)
	w.must(err)
	if f := v.Revisions[0].Data.FeeCategory; f.Inclusion != polapi.InclusionIncludedInLine || f.Side != polapi.FeeSidePurchaseCost {
		t.Fatalf("fee %+v", f)
	}
}

// Trade terms, tax applications and credit policies publish with their references in force; a
// pricing policy whose tax profile was superseded no longer serves a calculation (Python active,
// recursively).
func TestTradeTaxApplicationAndCreditKinds(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	win := newWindow()
	_, _, policy, fx := w.fxSetup(editor, reviewer, win)
	cal := w.configure(editor, reviewer, polapi.KindCalendar, "cal", 0, win.from, time.Time{}, polapi.ConfigData{Calendar: &polapi.Calendar{
		TimeZone: "Asia/Shanghai", WorkingWeekdays: []int{1, 2, 3, 4, 5}}}, true)
	terms := w.configure(editor, reviewer, polapi.KindTradeTerms, "FCA.test", 0, win.from, time.Time{}, polapi.ConfigData{TradeTerms: &polapi.TradeTerms{
		Incoterm: "FCA", Edition: polapi.IncotermsEdition2020, NamedPlace: "TEST NAMED PLACE", ResponsibilityNote: "synthetic evidence only",
		Delivery: polapi.Delivery{Kind: polapi.DeliveryLeadTime, AnchorEvent: polapi.AnchorTechnicalApproved, Days: 30, DayBasis: polapi.DayBasisWorking,
			Calendar: &cal}}}, true)
	resolved, err := w.c.POL.ResolveConfigs(editor, []polapi.ConfigRef{terms}, time.Now())
	w.must(err)
	if d := resolved[terms].Data.TradeTerms.Delivery; d.Days != 30 || *d.Calendar != cal || resolved[terms].Code != "" {
		t.Fatalf("trade terms %+v", resolved[terms])
	}
	freight := w.configure(editor, reviewer, polapi.KindFeeCategory, "freight", 0, win.from, time.Time{},
		polapi.ConfigData{FeeCategory: &polapi.FeeCategory{Name: "运费", Category: "freight", Side: polapi.FeeSideSalesCharge,
			Inclusion: polapi.InclusionSeparate, AllocationBasis: polapi.AllocationValue, PolicyBasis: "synthetic"}}, true)
	application := func(subject string) polapi.ConfigData {
		return polapi.ConfigData{TaxApplication: &polapi.TaxApplication{Name: "sales freight", Side: polapi.TaxSideSales,
			SubjectKind: polapi.SubjectFeeCategory, SubjectID: subject, TaxProfile: taxRef("tax"), Jurisdiction: "CN", BasisNote: "synthetic"}}
	}
	_, err = w.saveConfig(editor, polapi.KindTaxApplication, "app.missing", 0, win.from, time.Time{}, application("insurance"))
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/data/subject_id" {
		t.Fatalf("unconfigured fee category: %v", err)
	}
	w.configure(editor, reviewer, polapi.KindTaxApplication, "sales.freight", 0, win.from, time.Time{}, application(freight.ConfigID), true)
	credit := func(maxAmount string) polapi.ConfigData {
		return polapi.ConfigData{CreditPolicy: &polapi.CreditPolicy{Name: "credit", Currency: "CNY", DefaultLimit: "0",
			LimitSources:         []string{polapi.LimitSourceInternalAssessment, polapi.LimitSourceSinosureBuyerLimit},
			ApprovalTiers:        []polapi.CreditApprovalTier{{Level: "credit_officer", MaxAmount: "1000"}, {Level: "credit_manager", MaxAmount: maxAmount}},
			OverdueToleranceDays: 30, ReservationValidityDays: 30, DecisionValidityDays: 7}}
	}
	_, err = w.saveConfig(editor, polapi.KindCreditPolicy, "credit", 0, win.from, time.Time{}, credit("10000.001"))
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/data/approval_tiers/1/max_amount" {
		t.Fatalf("an amount finer than the currency: %v", err)
	}
	w.configure(editor, reviewer, polapi.KindCreditPolicy, "credit", 0, win.from, time.Time{}, credit("10000.00"), true)

	// The tax profile the policy names is superseded: the policy stays published but no calculation
	// adopts it any more until a reviewer publishes a policy revision naming the new tax revision.
	w.configure(editor, reviewer, polapi.KindTaxProfile, "tax", 2, win.from, win.until, taxData(polapi.RecoveryRecoverable), true)
	_, err = w.c.POL.LookupFX(editor, withPolicy(fxQuery("USD", "CNY", polapi.PurposeCostEstimate, &fx), policy))
	if code(err) != polapi.CodeConfigNotActive || path(err) != "/policy" {
		t.Fatalf("superseded tax profile of the policy: %v", err)
	}
	resolved, err = w.c.POL.ResolveConfigs(editor, []polapi.ConfigRef{policy, taxRef("tax")}, time.Now())
	w.must(err)
	if resolved[policy].Code != "" || resolved[taxRef("tax")].Code != polapi.CodeConfigNotActive {
		t.Fatalf("resolved %+v", resolved)
	}
}
