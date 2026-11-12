//go:build integration

package bootstrap_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The POL suite ports tests/test_foundation.py of the Python V0.50 foundation package. Python runs
// against a fixed clock inside [START, END); POL publishes at the system clock, so each test's
// synthetic configurations are valid from an hour ago for a day (window).

type window struct{ from, until time.Time }

func newWindow() window {
	from := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	return window{from: from, until: from.Add(24 * time.Hour)}
}

// polActors provisions the Python FoundationTests editor and reviewer: humans with every
// permission and company data scope.
func (w *world) polActors() (editor, reviewer context.Context) {
	grant := func(id string) context.Context {
		return w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: id},
			Permissions: allPermissions, DataScope: "company", Team: "trade"})
	}
	editor, reviewer = grant("editor"), grant("reviewer")
	// The verification evidence every publication of the suite cites (Python FoundationTests
	// registers it in setUp); both actors read it through their company data scope.
	w.source(editor, "evidence", "合成配置核实依据")
	return editor, reviewer
}

func (w *world) saveConfig(ctx context.Context, kind, id string, expected int64, from, until time.Time, data polapi.ConfigData) (polapi.Receipt, error) {
	cmd := polapi.SaveDraft{IdempotencyKey: w.key(), Kind: kind, ConfigID: id, ExpectedVersion: expected, ValidFrom: kernel.FormatTimestamp(from), Data: data}
	if !until.IsZero() {
		cmd.ValidUntil = kernel.FormatTimestamp(until)
	}
	return w.c.POL.SaveDraft(ctx, cmd)
}

func (w *world) publish(ctx context.Context, kind, id string, expected, revision int64) (polapi.Receipt, error) {
	return w.c.POL.Publish(ctx, polapi.Publish{IdempotencyKey: w.key(), Kind: kind, ConfigID: id, ExpectedVersion: expected,
		Revision: revision, EvidenceIDs: []string{"evidence"}, Reason: "核实合成测试配置"})
}

// configure saves a configuration as editor and, if publish, publishes it as reviewer; it returns
// the reference of the saved revision (Python FoundationTests.config).
func (w *world) configure(editor, reviewer context.Context, kind, id string, expected int64, from, until time.Time, data polapi.ConfigData, publish bool) polapi.ConfigRef {
	w.t.Helper()
	saved, err := w.saveConfig(editor, kind, id, expected, from, until, data)
	w.must(err)
	v, err := w.c.POL.GetConfig(editor, kind, id)
	w.must(err)
	ref := polapi.ConfigRef{Kind: kind, ConfigID: id, Revision: v.WorkingRevision}
	if publish {
		_, err = w.publish(reviewer, kind, id, saved.RowVersion, ref.Revision)
		w.must(err)
	}
	return ref
}

func currencyData(code string, minorUnits int, name string) polapi.ConfigData {
	return polapi.ConfigData{Currency: &polapi.Currency{Code: code, Name: name, MinorUnits: minorUnits, Rounding: polapi.RoundingHalfUp}}
}

func fxData(rate, purpose string, observed time.Time) polapi.ConfigData {
	return polapi.ConfigData{FXRate: &polapi.FXRate{FromCurrency: "USD", ToCurrency: "CNY", Rate: rate, Purpose: purpose,
		SourceName: "SYNTHETIC RATE", ObservedAt: kernel.FormatTimestamp(observed)}}
}

// currency publishes a currency with two minor units (Python FoundationTests.currency).
func (w *world) currency(editor, reviewer context.Context, code string, win window) polapi.ConfigRef {
	return w.configure(editor, reviewer, polapi.KindCurrency, code, 0, win.from, time.Time{}, currencyData(code, 2, code), true)
}

func taxData(recovery string) polapi.ConfigData {
	return polapi.ConfigData{TaxProfile: &polapi.TaxProfile{Name: "SYNTHETIC TAX", Rate: "0.13", PriceBasis: polapi.PriceBasisInclusive,
		RecoveryTreatment: recovery, PolicyBasis: "test only, not a legal determination"}}
}

// tax publishes the synthetic 13 % inclusive tax profile id for the window (Python
// FoundationTests.tax).
func (w *world) tax(editor, reviewer context.Context, recovery, id string, win window) polapi.ConfigRef {
	return w.configure(editor, reviewer, polapi.KindTaxProfile, id, 0, win.from, win.until, taxData(recovery), true)
}

func policyData(tax polapi.ConfigRef, maxFXAge int64) polapi.ConfigData {
	return polapi.ConfigData{PricingPolicy: &polapi.PricingPolicy{Name: "test", CalculationCurrency: "CNY", MinimumMargin: "0.25",
		QuoteValidDays: 7, MaxFXAgeSeconds: maxFXAge, TaxProfile: tax}}
}

// fxSetup publishes USD, CNY, a nonrecoverable tax profile, the pricing policy "policy" calculating
// in CNY with rates up to a day old, and the cost_estimate rate usd.cny 7.123456 observed at the
// window's start (Python FoundationTests.fx_setup).
func (w *world) fxSetup(editor, reviewer context.Context, win window) (usd, cny, policy, fx polapi.ConfigRef) {
	usd, cny = w.currency(editor, reviewer, "USD", win), w.currency(editor, reviewer, "CNY", win)
	tax := w.tax(editor, reviewer, polapi.RecoveryNonrecoverable, "tax", win)
	policy = w.configure(editor, reviewer, polapi.KindPricingPolicy, "policy", 0, win.from, time.Time{}, policyData(tax, 86400), true)
	fx = w.configure(editor, reviewer, polapi.KindFXRate, "usd.cny", 0, win.from, win.until, fxData("7.123456", polapi.PurposeCostEstimate, win.from), true)
	return usd, cny, policy, fx
}

func companyData(baseCurrency string) polapi.ConfigData {
	return polapi.ConfigData{CompanyProfile: &polapi.CompanyProfile{LegalName: "合成企业", RegistrationNo: "SYNTHETIC-NOT-REAL", Country: "CN",
		Address: "合成地址", BaseCurrency: baseCurrency, TimeZone: "Asia/Shanghai", PolicyScope: "synthetic general trade test"}}
}

// company publishes the profile of company.A with base currency CNY (Python FoundationTests.company).
func (w *world) company(editor, reviewer context.Context, win window) polapi.ConfigRef {
	return w.configure(editor, reviewer, polapi.KindCompanyProfile, "company.A", 0, win.from, time.Time{}, companyData("CNY"), true)
}

func fxQuery(from, to, purpose string, rate *polapi.ConfigRef) polapi.FXQuery {
	return polapi.FXQuery{FromCurrency: from, ToCurrency: to, Purpose: purpose, At: time.Now(), Rate: rate}
}

// withPolicy is q for the calculation of pricing policy p (Python fx.convert's policy argument).
func withPolicy(q polapi.FXQuery, p polapi.ConfigRef) polapi.FXQuery {
	q.Policy = &p
	return q
}

// Python test_foundation.py test_independent_publication_and_ai_guard.
func TestIndependentPublicationAndAIGuard(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	win := newWindow()
	draft := w.configure(editor, reviewer, polapi.KindCurrency, "USD", 0, win.from, time.Time{}, currencyData("USD", 2, "USD"), false)
	_, err := w.publish(editor, polapi.KindCurrency, "USD", 1, draft.Revision)
	w.want(err, polapi.CodeIndependentReviewRequired)
	ai := w.provision(bootstrap.Grant{Scope: w.scope("company.A"), Actor: kernel.ActorRef{Kind: kernel.ActorAI, ID: "ai"},
		Permissions: allPermissions, DataScope: "company"})
	_, err = w.publish(ai, polapi.KindCurrency, "USD", 1, draft.Revision)
	w.want(err, kernel.CodePermissionDenied)
	r, err := w.publish(reviewer, polapi.KindCurrency, "USD", 1, draft.Revision)
	w.must(err)
	if r.Status != polapi.StatusActive || r.RowVersion != 2 || r.ObjectID != "currency:USD" {
		t.Fatalf("publish %+v", r)
	}
	v, err := w.c.POL.GetConfig(editor, polapi.KindCurrency, "USD")
	w.must(err)
	rev := v.Revisions[0]
	if v.ActiveRevision != 1 || rev.Data.Currency.Code != "USD" || rev.SavedBy.ID != "editor" || rev.PublishedBy.ID != "reviewer" ||
		!slices.Equal(rev.EvidenceIDs, []string{"evidence"}) || len(rev.ContentHash) != 64 {
		t.Fatalf("config %+v", v)
	}
	// An agent prepares drafts through the same command path; only humans publish, retire or discard.
	_, err = w.saveConfig(ai, polapi.KindCurrency, "CNY", 0, win.from, time.Time{}, currencyData("CNY", 2, "CNY"))
	w.must(err)
	_, err = w.c.POL.DiscardDraft(ai, polapi.DiscardDraft{IdempotencyKey: w.key(), Kind: polapi.KindCurrency, ConfigID: "CNY", ExpectedVersion: 1, Reason: "x"})
	w.want(err, kernel.CodePermissionDenied)
	_, err = w.c.POL.Retire(ai, polapi.Retire{IdempotencyKey: w.key(), Kind: polapi.KindCurrency, ConfigID: "USD", ExpectedVersion: 2, Reason: "x"})
	w.want(err, kernel.CodePermissionDenied)
	_, err = w.publish(editor, polapi.KindCurrency, "CNY", 1, 1)
	w.must(err) // the agent was the last editor, so the human editor reviews independently
}

// Python test_foundation.py test_fx_direction_purpose_rounding_and_expiry. An expired rate is
// FX_STALE here (POL-INV-003); Python's fx.convert returns CONFIG_EXPIRED.
func TestFXDirectionPurposeRoundingAndExpiry(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	win := newWindow()
	_, _, policy, fx := w.fxSetup(editor, reviewer, win)
	rate, err := w.c.POL.LookupFX(editor, withPolicy(fxQuery("USD", "CNY", polapi.PurposeCostEstimate, &fx), policy))
	w.must(err)
	ten, _ := kernel.ParseAmount(rate.Value.From(), "10")
	cny, err := rate.Value.Convert(ten)
	w.must(err)
	if cny.String() != "71.23 CNY" || rate.Rate != fx || len(rate.ContentHash) != 64 || !rate.ObservedAt.Equal(win.from) {
		t.Fatalf("10 USD = %s at %+v", cny, rate)
	}
	_, err = w.c.POL.LookupFX(editor, withPolicy(fxQuery("USD", "CNY", polapi.PurposeQuotation, &fx), policy))
	w.want(err, polapi.CodeFXScopeMismatch)
	_, err = w.c.POL.LookupFX(editor, withPolicy(fxQuery("CNY", "USD", polapi.PurposeCostEstimate, &fx), policy))
	if code(err) != polapi.CodeFXScopeMismatch || path(err) != "/policy" {
		t.Fatalf("the policy calculates in CNY: %v", err)
	}
	_, err = w.c.POL.LookupFX(editor, fxQuery("CNY", "USD", polapi.PurposeCostEstimate, &fx))
	w.want(err, polapi.CodeFXScopeMismatch)
	expired := withPolicy(fxQuery("USD", "CNY", polapi.PurposeCostEstimate, &fx), policy)
	expired.At = win.until
	_, err = w.c.POL.LookupFX(editor, expired)
	if code(err) != polapi.CodeConfigExpired || path(err) != "/policy" {
		// The tax profile of the policy expires with the window, before the rate is looked at.
		t.Fatalf("expired policy dependency: %v", err)
	}
	expired.Policy = nil
	_, err = w.c.POL.LookupFX(editor, expired)
	w.want(err, polapi.CodeFXStale)
}

// Python test_foundation.py test_payment_stage_total_and_declared_calendar. The calendar is declared
// once for the payment terms, not per stage (the stage shape stays as published before).
func TestPaymentStageTotalAndDeclaredCalendar(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	win := newWindow()
	cal := w.configure(editor, reviewer, polapi.KindCalendar, "calendar", 0, win.from, time.Time{}, polapi.ConfigData{Calendar: &polapi.Calendar{
		TimeZone: "Asia/Shanghai", WorkingWeekdays: []int{1, 2, 3, 4, 5}, Holidays: []string{"2026-10-01"}}}, true)
	stage := polapi.PaymentStage{StageID: "deposit", Percent: "30", Trigger: polapi.TriggerOrderConfirmed}
	_, err := w.saveConfig(editor, polapi.KindPaymentTerms, "payment", 0, win.from, time.Time{},
		polapi.ConfigData{PaymentTerms: &polapi.PaymentTerms{Name: "test", Stages: []polapi.PaymentStage{stage}, DayBasis: polapi.DayBasisCalendar,
			Calendar: &cal}})
	if code(err) != polapi.CodePaymentTotalMismatch || path(err) != "/args/data/stages" {
		t.Fatalf("30 alone: %v", err)
	}
	balance := polapi.PaymentStage{StageID: "balance", Percent: "70", Trigger: polapi.TriggerBeforeShipment, Days: 0}
	full := w.configure(editor, reviewer, polapi.KindPaymentTerms, "payment", 0, win.from, time.Time{},
		polapi.ConfigData{PaymentTerms: &polapi.PaymentTerms{Name: "30/70", Stages: []polapi.PaymentStage{stage, balance},
			DayBasis: polapi.DayBasisCalendar, Calendar: &cal}}, true)
	v, err := w.c.POL.GetConfig(editor, polapi.KindPaymentTerms, full.ConfigID)
	w.must(err)
	if p := v.Revisions[0].Data.PaymentTerms; p.Stages[1].Percent != "70" || *p.Calendar != cal {
		t.Fatalf("stages %+v", v.Revisions[0].Data.PaymentTerms)
	}
	c, err := w.c.POL.GetConfig(editor, polapi.KindCalendar, "calendar")
	w.must(err)
	if d := c.Revisions[0].Data.Calendar; !slices.Equal(d.WorkingWeekdays, []int{1, 2, 3, 4, 5}) || !slices.Equal(d.Holidays, []string{"2026-10-01"}) {
		t.Fatalf("calendar %+v", d)
	}
	// A declared calendar must be in force: a calendar draft that nobody published is not.
	draft := w.configure(editor, reviewer, polapi.KindCalendar, "draft.cal", 0, win.from, time.Time{}, polapi.ConfigData{Calendar: &polapi.Calendar{
		TimeZone: "Asia/Shanghai", WorkingWeekdays: []int{1, 2, 3, 4, 5, 6}}}, false)
	_, err = w.saveConfig(editor, polapi.KindPaymentTerms, "payment.2", 0, win.from, time.Time{},
		polapi.ConfigData{PaymentTerms: &polapi.PaymentTerms{Name: "100", Stages: []polapi.PaymentStage{{StageID: "all", Percent: "100",
			Trigger: polapi.TriggerBeforeShipment}}, DayBasis: polapi.DayBasisWorking, Calendar: &draft}})
	if code(err) != polapi.CodeConfigNotActive || path(err) != "/args/data/calendar" {
		t.Fatalf("unpublished calendar: %v", err)
	}
}

// Python test_foundation.py test_company_identity_and_immutable_currency_semantics.
func TestCompanyIdentityAndImmutableCurrencySemantics(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	win := newWindow()
	w.currency(editor, reviewer, "CNY", win)
	w.company(editor, reviewer, win)
	profile, err := w.c.POL.GetConfig(editor, polapi.KindCompanyProfile, "company.A")
	w.must(err)
	_, err = w.saveConfig(editor, polapi.KindCompanyProfile, "company.B", 0, win.from, time.Time{}, profile.Revisions[0].Data)
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/id" {
		t.Fatalf("a profile identifies the current company: %v", err)
	}
	if err = w.exec("company.A", `INSERT INTO brant_pol.config_root (tenant_id, company_id, kind, config_id, root_version, last_revision,
		created_at, updated_at) VALUES (current_setting('app.tenant_id'), 'company.A', 'company_profile', 'company.B', 1, 1, now(), now())`); err == nil {
		t.Fatal("the database refuses a profile of another company")
	}
	draft := w.configure(editor, reviewer, polapi.KindCurrency, "CNY", 2, win.from, time.Time{}, currencyData("CNY", 3, "changed"), false)
	_, err = w.publish(reviewer, polapi.KindCurrency, "CNY", 3, draft.Revision)
	w.want(err, polapi.CodeImmutableDefinition)
	_, err = w.saveConfig(editor, polapi.KindCurrency, "RMB", 0, win.from, time.Time{}, currencyData("CNY", 2, "人民币"))
	if code(err) != kernel.CodeInvalidKnownValue || path(err) != "/args/id" {
		t.Fatalf("a currency's id is its code: %v", err)
	}
}

// Python test_foundation.py test_configuration_cas_and_cross_company_reference.
func TestConfigurationCASAndCrossCompanyReference(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	win := newWindow()
	cny := w.currency(editor, reviewer, "CNY", win)
	_, err := w.saveConfig(editor, polapi.KindCurrency, "CNY", 1, win.from, time.Time{}, currencyData("CNY", 2, "new"))
	w.want(err, kernel.CodeVersionConflict)
	other := w.provision(bootstrap.Grant{Scope: w.scope("company.B"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "operator"},
		Permissions: allPermissions, DataScope: "company"})
	_, err = w.c.POL.GetConfig(other, polapi.KindCurrency, "CNY")
	w.want(err, kernel.CodeNotFound)
	resolved, err := w.c.POL.ResolveConfigs(other, []polapi.ConfigRef{cny}, time.Now())
	w.must(err)
	if len(resolved) != 0 {
		t.Fatalf("company.B resolved %+v", resolved)
	}
	_, err = w.c.POL.LookupFX(other, fxQuery("CNY", "CNY", polapi.PurposeCostEstimate, nil))
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/from_currency" {
		t.Fatalf("company.B currency: %v", err)
	}
}

// Python test_foundation.py test_stale_fx_is_not_silently_reused_and_no_reciprocal: a policy
// revision allowing rates a minute old makes the rate observed an hour ago stale, in a lookup and
// in a snapshot.
func TestStaleFXIsNotSilentlyReusedAndNoReciprocal(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	win := newWindow()
	_, _, policy, fx := w.fxSetup(editor, reviewer, win)
	old, err := w.c.POL.GetConfig(editor, polapi.KindPricingPolicy, policy.ConfigID)
	w.must(err)
	data := old.Revisions[0].Data
	data.PricingPolicy.MaxFXAgeSeconds = 60
	newPolicy := w.configure(editor, reviewer, polapi.KindPricingPolicy, policy.ConfigID, 2, win.from, time.Time{}, data, true)
	_, err = w.c.POL.LookupFX(editor, withPolicy(fxQuery("USD", "CNY", polapi.PurposeCostEstimate, &fx), newPolicy))
	w.want(err, polapi.CodeFXStale)
	q := fxQuery("USD", "CNY", polapi.PurposeCostEstimate, &fx)
	q.MaxObservationAge = time.Minute
	_, err = w.c.POL.LookupFX(editor, q)
	w.want(err, polapi.CodeFXStale)
	_, err = w.c.POL.LookupFX(editor, fxQuery("CNY", "USD", polapi.PurposeCostEstimate, nil))
	w.want(err, polapi.CodeFXScopeMismatch)
	company := w.company(editor, reviewer, win)
	_, err = w.c.POL.CaptureSnapshot(editor, polapi.CaptureSnapshot{IdempotencyKey: w.key(), SnapshotID: "stale.snapshot",
		Purpose: polapi.PurposeCostEstimate, Refs: []polapi.ConfigRef{company, newPolicy, fx}})
	if code(err) != polapi.CodeFXStale || path(err) != "/args/refs/2" {
		t.Fatalf("stale rate in a snapshot: %v", err)
	}
}

// Python test_foundation.py test_retired_currency_cannot_be_redefined.
func TestRetiredCurrencyCannotBeRedefined(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	win := newWindow()
	w.currency(editor, reviewer, "USD", win)
	r, err := w.c.POL.Retire(reviewer, polapi.Retire{IdempotencyKey: w.key(), Kind: polapi.KindCurrency, ConfigID: "USD", ExpectedVersion: 2, Reason: "停用"})
	w.must(err)
	if r.Status != polapi.StatusRetired || r.RowVersion != 3 {
		t.Fatalf("retire %+v", r)
	}
	draft := w.configure(editor, reviewer, polapi.KindCurrency, "USD", 3, win.from, time.Time{}, currencyData("USD", 0, "USD"), false)
	if draft.Revision != 2 {
		t.Fatalf("draft %+v", draft)
	}
	_, err = w.publish(reviewer, polapi.KindCurrency, "USD", 4, 2)
	w.want(err, polapi.CodeImmutableDefinition)
	v, err := w.c.POL.GetConfig(editor, polapi.KindCurrency, "USD")
	w.must(err)
	if len(v.Revisions) != 2 || v.Revisions[0].Status != polapi.StatusRetired || v.Revisions[0].EndedAt.IsZero() || v.ActiveRevision != 0 {
		t.Fatalf("history %+v", v)
	}
}

// Python test_foundation.py test_concurrent_number_issue_has_unique_numbers, with eight issues
// racing for the first number of the year instead of two.
func TestConcurrentNumberIssueHasUniqueNumbers(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	seq := w.sequence(editor, reviewer, "quote.seq", "QT-")
	const n = 8
	numbers := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := w.c.POL.IssueNumber(editor, polapi.IssueNumber{IdempotencyKey: "parallel." + string(rune('a'+i)), Sequence: seq,
				BusinessRef: "q" + string(rune('a'+i))})
			numbers[i], errs[i] = r.ObjectID, err
		}()
	}
	wg.Wait()
	for _, err := range errs {
		w.must(err)
	}
	year := quoteYear(t)
	var want []string
	for i := 1; i <= n; i++ {
		want = append(want, "QT-"+year+"-0000"+string(rune('0'+i)))
	}
	slices.Sort(numbers)
	if !slices.Equal(numbers, want) {
		t.Fatalf("numbers %v, want %v", numbers, want)
	}
}

func quoteYear(t *testing.T) string {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	y := time.Now().In(loc).Year()
	return string(rune('0'+y/1000)) + string(rune('0'+y/100%10)) + string(rune('0'+y/10%10)) + string(rune('0'+y%10))
}

// sequence publishes a quote number sequence of width 5 counting years in Asia/Shanghai.
func (w *world) sequence(editor, reviewer context.Context, id, prefix string) polapi.ConfigRef {
	return w.configure(editor, reviewer, polapi.KindNumberSequence, id, 0, newWindow().from, time.Time{},
		polapi.ConfigData{NumberSequence: &polapi.NumberSequence{DocumentType: polapi.DocumentTypeQuote, Prefix: prefix, Width: 5,
			Reset: polapi.ResetAnnual, TimeZone: "Asia/Shanghai"}}, true)
}

// Python test_foundation.py test_same_currency_explicit_one_and_missing_cross_rate_blocks. A
// missing cross rate is FX_SCOPE_MISMATCH (error catalog: the rate for the purpose is missing);
// Python returns DRAFT_NOT_READY.
func TestSameCurrencyExplicitOneAndMissingCrossRateBlocks(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	win := newWindow()
	_, _, policy, fx := w.fxSetup(editor, reviewer, win)
	same, err := w.c.POL.LookupFX(editor, withPolicy(fxQuery("CNY", "CNY", polapi.PurposeCostEstimate, nil), policy))
	w.must(err)
	amount, err := kernel.RoundMoney(same.Value.To(), kernel.MustDecimal("10.005").Mul(same.Value.Rate()))
	w.must(err)
	if amount.Amount() != "10.01" || !same.Value.Rate().Equal(kernel.DecimalFromInt(1)) || same.Rate != (polapi.ConfigRef{}) {
		t.Fatalf("same currency %+v: %s", same, amount)
	}
	_, err = w.c.POL.LookupFX(editor, withPolicy(fxQuery("USD", "CNY", polapi.PurposeCostEstimate, nil), policy))
	w.want(err, polapi.CodeFXScopeMismatch)
	_, err = w.c.POL.LookupFX(editor, withPolicy(fxQuery("CNY", "CNY", polapi.PurposeCostEstimate, &fx), policy))
	w.want(err, polapi.CodeFXScopeMismatch)
}

// Python test_foundation.py test_expired_and_future_publication_cannot_replace_active_rate. The
// second save replaces the working revision 2 instead of adding revision 3.
func TestExpiredAndFuturePublicationCannotReplaceActiveRate(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	win := newWindow()
	_, _, _, fx := w.fxSetup(editor, reviewer, win)
	for i, shift := range []time.Duration{-72 * time.Hour, 72 * time.Hour} {
		from := win.from.Add(shift)
		expected := int64(2 + i)
		draft := w.configure(editor, reviewer, polapi.KindFXRate, fx.ConfigID, expected, from, from.Add(24*time.Hour),
			fxData("7.2", polapi.PurposeCostEstimate, from), false)
		_, err := w.publish(reviewer, polapi.KindFXRate, fx.ConfigID, expected+1, draft.Revision)
		w.want(err, polapi.CodeConfigExpired)
		v, err := w.c.POL.GetConfig(editor, polapi.KindFXRate, fx.ConfigID)
		w.must(err)
		if v.ActiveRevision != 1 || v.WorkingRevision != 2 {
			t.Fatalf("%v: %+v", shift, v)
		}
	}
}

// Python test_foundation.py test_configuration_save_race_has_one_winner.
func TestConfigurationSaveRaceHasOneWinner(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	win := newWindow()
	w.currency(editor, reviewer, "CNY", win)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = w.c.POL.SaveDraft(editor, polapi.SaveDraft{IdempotencyKey: "currency.race." + string(rune('1'+i)),
				Kind: polapi.KindCurrency, ConfigID: "CNY", ExpectedVersion: 2, ValidFrom: kernel.FormatTimestamp(win.from),
				Data: currencyData("CNY", 2, "name"+string(rune('1'+i)))})
		}()
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("want one winner: %v", errs)
	}
	for _, err := range errs {
		if err != nil {
			w.want(err, kernel.CodeVersionConflict)
		}
	}
	v, err := w.c.POL.GetConfig(editor, polapi.KindCurrency, "CNY")
	w.must(err)
	if v.ActiveRevision != 1 || v.WorkingRevision != 2 || v.RootVersion != 3 {
		t.Fatalf("config %+v", v)
	}
}

// SM-POL-REVISION: publishing supersedes the active revision in the same transaction; retiring
// ends it; history stays readable; a ConfigRef is in force only between its publication and its
// end. Events carry identifiers, versions and hashes only.
func TestPublicationSupersedesAndResolvesAtAnInstant(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	win := newWindow()
	first := w.currency(editor, reviewer, "USD", win)
	between := time.Now()
	second := w.configure(editor, reviewer, polapi.KindCurrency, "USD", 2, win.from, time.Time{}, currencyData("USD", 2, "US dollar"), true)
	unknown := polapi.ConfigRef{Kind: polapi.KindCurrency, ConfigID: "EUR", Revision: 1}
	now := time.Now()
	resolved, err := w.c.POL.ResolveConfigs(editor, []polapi.ConfigRef{first, second, unknown}, now)
	w.must(err)
	if len(resolved) != 2 || resolved[first].Code != polapi.CodeConfigNotActive || resolved[first].Status != polapi.StatusSuperseded ||
		resolved[second].Code != "" || resolved[second].Data.Currency.Name != "US dollar" || resolved[second].Check("/x") != nil {
		t.Fatalf("resolved %+v", resolved)
	}
	if rej, ok := kernel.AsRejection(resolved[first].Check("/args/currency")); !ok || rej.Fields[0].Path != "/args/currency" {
		t.Fatalf("check %v", rej)
	}
	past, err := w.c.POL.ResolveConfigs(editor, []polapi.ConfigRef{first, second}, between)
	w.must(err)
	if past[first].Code != "" || past[second].Code != polapi.CodeConfigNotActive {
		t.Fatalf("at %s: %+v", between, past)
	}
	_, err = w.c.POL.Retire(reviewer, polapi.Retire{IdempotencyKey: w.key(), Kind: polapi.KindCurrency, ConfigID: "USD", ExpectedVersion: 4, Reason: "停用"})
	w.must(err)
	after, err := w.c.POL.ResolveConfigs(editor, []polapi.ConfigRef{second}, time.Now())
	w.must(err)
	if after[second].Code != polapi.CodeConfigNotActive || after[second].Status != polapi.StatusRetired {
		t.Fatalf("retired %+v", after)
	}
	events := w.events("company.A", polapi.Source)
	var types []string
	for _, e := range events {
		types = append(types, e.Type)
	}
	want := []string{polapi.EventConfigPublished, polapi.EventConfigPublished, polapi.EventConfigSuperseded, polapi.EventConfigRetired}
	if !slices.Equal(types, want) {
		t.Fatalf("events %v", types)
	}
	if p := events[2].Payload; p["revision"] != float64(1) || p["superseded_by_revision"] != float64(2) || p["root_version"] != float64(4) ||
		p["config_id"] != "USD" || p["name"] != nil || len(p["content_hash"].(string)) != 64 {
		t.Fatalf("superseded payload %v", p)
	}
}

// SM-POL-REVISION working → discarded: human only; the number is not reused (also Python
// test_t03_configuration_runtime.py test_discarded_working_revision_keeps_history_and_allocates_a_new_number,
// PYT-0409, which discards a quote document template: the discarded revision stays in the history
// and the next draft is revision 2; POL's discard takes a reason, no evidence).
func TestDiscardedDraftKeepsItsNumber(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	win := newWindow()
	w.configure(editor, reviewer, polapi.KindUnit, "PCS", 0, win.from, time.Time{},
		polapi.ConfigData{Unit: &polapi.Unit{Code: "PCS", Name: "件", Dimension: polapi.DimensionCount, Precision: 0}}, false)
	r, err := w.c.POL.DiscardDraft(reviewer, polapi.DiscardDraft{IdempotencyKey: w.key(), Kind: polapi.KindUnit, ConfigID: "PCS", ExpectedVersion: 1, Reason: "重新编制"})
	w.must(err)
	if r.Status != polapi.StatusDiscarded || r.RowVersion != 2 {
		t.Fatalf("discard %+v", r)
	}
	_, err = w.c.POL.DiscardDraft(reviewer, polapi.DiscardDraft{IdempotencyKey: w.key(), Kind: polapi.KindUnit, ConfigID: "PCS", ExpectedVersion: 2, Reason: "x"})
	w.want(err, kernel.CodeInvalidState)
	again := w.configure(editor, reviewer, polapi.KindUnit, "PCS", 2, win.from, time.Time{},
		polapi.ConfigData{Unit: &polapi.Unit{Code: "PCS", Name: "件", Dimension: polapi.DimensionCount, Precision: 0}}, true)
	v, err := w.c.POL.GetConfig(editor, polapi.KindUnit, "PCS")
	w.must(err)
	if again.Revision != 2 || len(v.Revisions) != 2 || v.Revisions[0].Status != polapi.StatusDiscarded || v.ActiveRevision != 2 {
		t.Fatalf("config %+v", v)
	}
	audit, err := w.c.POL.Audit(editor)
	w.must(err)
	var reason string
	var reads int
	for _, e := range audit {
		if e.Command == polapi.CommandDiscardDraft && e.Outcome == "succeeded" {
			reason = e.Reason
		}
		if e.Command == "config.get" && e.Outcome == "read" && e.ObjectID == "unit:PCS" {
			reads++
		}
	}
	if reason != "重新编制" || reads == 0 {
		t.Fatalf("audit reason %q, reads %d", reason, reads)
	}
}

// Published revisions are immutable in the database too, and the database refuses a publisher who
// is the revision's last editor (POL-INV-001); issued numbers never change. Python
// test_t03_configuration_runtime.py test_runtime_schemas_match_candidate_contract_and_sealed_history_is_immutable
// (PYT-0408), its sealed-history half: no update or delete of a published revision passes, whatever
// the kind (one table holds every kind). Its schema-identity half has no Go counterpart (typed api),
// and POL's events are Outbox messages, not a history table.
func TestDatabaseKeepsPublishedRevisionsImmutable(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	win := newWindow()
	w.currency(editor, reviewer, "USD", win)
	w.configure(editor, reviewer, polapi.KindCurrency, "CNY", 0, win.from, time.Time{}, currencyData("CNY", 2, "CNY"), false)
	for _, sql := range []string{
		`UPDATE brant_pol.config_revision SET data = '{"code":"USD","name":"x","minor_units":0,"rounding":"HALF_UP"}' WHERE config_id = 'USD'`,
		`UPDATE brant_pol.config_revision SET status = 'working', published_by_kind = NULL, published_by_id = NULL, publish_epoch = NULL,
			published_at = NULL, evidence_ids = NULL WHERE config_id = 'USD'`,
		`DELETE FROM brant_pol.config_revision WHERE config_id = 'CNY'`,
		`UPDATE brant_pol.config_revision SET status = 'active', published_by_kind = 'human', published_by_id = saved_by_id,
			publish_epoch = 1, published_at = now(), evidence_ids = '{e}' WHERE config_id = 'CNY'`,
		`UPDATE brant_pol.config_root SET root_version = 1 WHERE config_id = 'USD'`,
	} {
		if err := w.exec("company.A", sql); err == nil {
			t.Errorf("the database must refuse %s", sql)
		}
	}
	if w.count("company.A", "brant_pol.config_revision") != 2 || w.count("company.B", "brant_pol.config_revision") != 0 {
		t.Fatal("revisions are scoped to their company")
	}
}

// POL-INV-005: a business reference keeps its number; another sequence for it conflicts; an
// exhausted sequence issues nothing; issued numbers are immutable.
func TestSameBusinessReferenceKeepsItsNumber(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	seq := w.sequence(editor, reviewer, "quote.seq", "QT-")
	first, err := w.c.POL.IssueNumber(editor, polapi.IssueNumber{IdempotencyKey: w.key(), Sequence: seq, BusinessRef: "quote1"})
	w.must(err)
	again, err := w.c.POL.IssueNumber(reviewer, polapi.IssueNumber{IdempotencyKey: w.key(), Sequence: seq, BusinessRef: "quote1"})
	w.must(err)
	year := quoteYear(t)
	if first.ObjectID != "QT-"+year+"-00001" || again.ObjectID != first.ObjectID || again.Replayed || first.Status != polapi.StatusIssued {
		t.Fatalf("first %+v, again %+v", first, again)
	}
	other := w.sequence(editor, reviewer, "quote.seq.2", "Q2-")
	_, err = w.c.POL.IssueNumber(editor, polapi.IssueNumber{IdempotencyKey: w.key(), Sequence: other, BusinessRef: "quote1"})
	if code(err) != polapi.CodeBusinessKeyConflict || path(err) != "/args/business_ref" {
		t.Fatalf("another sequence: %v", err)
	}
	w.must(w.exec("company.A", `UPDATE brant_pol.number_counter SET last_value = 99998`))
	last, err := w.c.POL.IssueNumber(editor, polapi.IssueNumber{IdempotencyKey: w.key(), Sequence: seq, BusinessRef: "quote2"})
	w.must(err)
	_, err = w.c.POL.IssueNumber(editor, polapi.IssueNumber{IdempotencyKey: w.key(), Sequence: seq, BusinessRef: "quote3"})
	w.want(err, polapi.CodeSequenceExhausted)
	if last.ObjectID != "QT-"+year+"-99999" || w.count("company.A", "brant_pol.issued_number") != 2 {
		t.Fatalf("last %+v", last)
	}
	if err = w.exec("company.A", `UPDATE brant_pol.issued_number SET business_ref = 'other'`); err == nil {
		t.Fatal("issued numbers are immutable")
	}
	if err = w.exec("company.A", `UPDATE brant_pol.number_counter SET last_value = 1`); err == nil {
		t.Fatal("a counter only counts up")
	}
	events := w.events("company.A", polapi.Source)
	var issued []map[string]any
	for _, e := range events {
		if e.Type == polapi.EventNumberIssued {
			issued = append(issued, e.Payload)
		}
	}
	if len(issued) != 2 || issued[0]["document_number"] != first.ObjectID || issued[0]["business_ref"] != "quote1" ||
		issued[0]["sequence_config_id"] != "quote.seq" || issued[0]["sequence_revision"] != float64(1) {
		t.Fatalf("issued events %v", issued)
	}
	_, err = w.c.POL.Retire(reviewer, polapi.Retire{IdempotencyKey: w.key(), Kind: polapi.KindNumberSequence, ConfigID: "quote.seq", ExpectedVersion: 2, Reason: "停用"})
	w.must(err)
	_, err = w.c.POL.IssueNumber(editor, polapi.IssueNumber{IdempotencyKey: w.key(), Sequence: seq, BusinessRef: "quote4"})
	w.want(err, polapi.CodeConfigNotActive)
	kept, err := w.c.POL.IssueNumber(editor, polapi.IssueNumber{IdempotencyKey: w.key(), Sequence: seq, BusinessRef: "quote1"})
	w.must(err)
	if kept.ObjectID != first.ObjectID {
		t.Fatalf("a retry after retirement keeps its number: %+v", kept)
	}
}

// Python test_foundation.py publish (core._sources): the publisher must be able to read every
// source it cites (Context Map R63 EVD->POL).
func TestPublicationEvidenceMustBeReadable(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	win := newWindow()
	w.configure(editor, reviewer, polapi.KindCurrency, "USD", 0, win.from, time.Time{}, currencyData("USD", 2, "USD"), false)
	pub := polapi.Publish{IdempotencyKey: w.key(), Kind: polapi.KindCurrency, ConfigID: "USD", ExpectedVersion: 1, Revision: 1,
		EvidenceIDs: []string{"evidence", "missing"}, Reason: "核实"}
	_, err := w.c.POL.Publish(reviewer, pub)
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/evidence_ids/1" {
		t.Fatalf("unreadable evidence: %v at %s", err, path(err))
	}
	// A publisher without the EVD read permission is refused before anything else.
	noRead := w.actor("no.read", "config.publish", "pol.read")
	pub.IdempotencyKey, pub.EvidenceIDs = w.key(), []string{"evidence"}
	_, err = w.c.POL.Publish(noRead, pub)
	w.want(err, kernel.CodePermissionDenied)
	pub.IdempotencyKey = w.key()
	r, err := w.c.POL.Publish(reviewer, pub)
	w.must(err)
	if r.Status != polapi.StatusActive {
		t.Fatalf("publish %+v", r)
	}
}
