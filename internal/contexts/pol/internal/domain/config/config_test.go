package config

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The Python V0.50 vectors run against a fixed clock: 2026-10-22T09:00Z inside [START, END).
var (
	start = time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	clock = start.Add(9 * time.Hour)
	end   = start.Add(24 * time.Hour)

	editor   = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "editor"}
	reviewer = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "reviewer"}

	// both currencies of the exchange-rate vectors are configured and in force
	currenciesInForce = facts(currencyFact("USD", 2), currencyFact("CNY", 2))
)

// fact is a referenced configuration and what the facts say about it.
type fact struct {
	target Target
	Fact
}

// facts returns the facts of company.A with entries.
func facts(entries ...fact) Facts {
	f := Facts{Company: "company.A", Configs: map[Target]Fact{}}
	for _, e := range entries {
		f.Configs[e.target] = e.Fact
	}
	return f
}

// currencyFact is a currency in force with minor units.
func currencyFact(code string, minor int) fact {
	return fact{Target{Kind: KindCurrency, ID: code}, Fact{Rev: &Revision{Number: 1, Status: Active, Content: currency(code, minor, code)}}}
}

func code(err error) string {
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code + " " + r.Fields[0].Path
	}
	if err != nil {
		return "ERROR " + err.Error()
	}
	return "OK"
}

func currency(code string, minor int, name string) Content {
	return Content{Kind: KindCurrency, ValidFrom: start, Currency: &Currency{Code: code, Name: name, MinorUnits: minor, Rounding: "HALF_UP"}}
}

func fx(from, to, rate, purpose string) Content {
	return Content{Kind: KindFXRate, ValidFrom: start, ValidUntil: end, FXRate: &FXRate{FromCurrency: from, ToCurrency: to, Rate: rate,
		Purpose: purpose, SourceName: "SYNTHETIC RATE", ObservedAt: kernel.FormatTimestamp(start)}}
}

func payment(percents ...string) Content {
	c := Content{Kind: KindPaymentTerms, ValidFrom: start, PaymentTerms: &PaymentTerms{Name: "terms"}}
	for i, p := range percents {
		c.PaymentTerms.Stages = append(c.PaymentTerms.Stages, PaymentStage{StageID: "s" + string(rune('1'+i)), Percent: p, Trigger: "order_confirmed"})
	}
	return c
}

func mustSave(t *testing.T, r *Root, c Content, by kernel.ActorRef, epoch int64, at time.Time) {
	t.Helper()
	if _, _, err := r.Save(c, by, epoch, at); err != nil {
		t.Fatal(err)
	}
}

// publishedRoot returns a configuration whose revision 1 holds c, saved by editor and published by
// reviewer at clock.
func publishedRoot(t *testing.T, id string, c Content) *Root {
	t.Helper()
	r := New(c.Kind, id)
	if _, _, err := r.Save(c, editor, 1, start); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Publish(1, currenciesInForce, reviewer, 1, []string{"evidence"}, clock); err != nil {
		t.Fatal(err)
	}
	return r
}

// Python test_foundation.py test_payment_stage_total_and_declared_calendar (payment-stage half) and
// POL-INV-004: stages are positive and sum to exactly 100.
func TestPaymentStagesArePositiveAndSumToExactly100(t *testing.T) {
	for _, c := range []struct {
		content Content
		want    string
	}{
		{payment("30"), "PAYMENT_TOTAL_MISMATCH /args/data/stages"},
		{payment("30", "70"), "OK"},
		{payment("99"), "PAYMENT_TOTAL_MISMATCH /args/data/stages"},
		{payment("100", "0"), "PAYMENT_TOTAL_MISMATCH /args/data/stages"},
		{payment("33.333333", "33.333333", "33.333334"), "OK"},
		{payment("33.333333", "33.333333", "33.333333"), "PAYMENT_TOTAL_MISMATCH /args/data/stages"},
	} {
		if got := code(Validate("payment", c.content)); got != c.want {
			t.Errorf("%v: %s, want %s", c.content.PaymentTerms.Stages, got, c.want)
		}
	}
	dup := payment("30", "70")
	dup.PaymentTerms.Stages[1].StageID = dup.PaymentTerms.Stages[0].StageID
	if got := code(Validate("payment", dup)); got != "INVALID_KNOWN_VALUE /args/data/stages" {
		t.Fatal(got)
	}
}

func TestContentRules(t *testing.T) {
	observedLate := fx("USD", "CNY", "7.123456", "cost_estimate")
	observedLate.FXRate.ObservedAt = kernel.FormatTimestamp(start.Add(time.Second))
	openEnded := fx("USD", "CNY", "7.123456", "cost_estimate")
	openEnded.ValidUntil = time.Time{}
	reversed := currency("USD", 2, "USD")
	reversed.ValidUntil = start
	badZone := Content{Kind: KindNumberSequence, ValidFrom: start, NumberSequence: &NumberSequence{DocumentType: "quote", Prefix: "QT-",
		Width: 5, Reset: "annual", TimeZone: "Mars/Olympus_Mons"}}
	goodZone := badZone
	goodZone.NumberSequence = &NumberSequence{DocumentType: "quote", Prefix: "QT-", Width: 5, Reset: "annual", TimeZone: "Asia/Shanghai"}
	for _, c := range []struct {
		id      string
		content Content
		want    string
	}{
		{"USD", currency("USD", 2, "USD"), "OK"},
		{"usd", currency("USD", 2, "USD"), "INVALID_KNOWN_VALUE /args/id"},
		{"USD", reversed, "INVALID_KNOWN_VALUE /args/valid_until"},
		{"r", fx("USD", "CNY", "7.123456", "cost_estimate"), "OK"},
		{"r", fx("USD", "USD", "1", "cost_estimate"), "INVALID_KNOWN_VALUE /args/data/to_currency"},
		{"r", fx("USD", "CNY", "0", "cost_estimate"), "INVALID_KNOWN_VALUE /args/data/rate"},
		{"r", observedLate, "INVALID_KNOWN_VALUE /args/data/observed_at"},
		{"r", openEnded, "INVALID_KNOWN_VALUE /args/valid_until"},
		{"seq", badZone, "INVALID_KNOWN_VALUE /args/data/time_zone"},
		{"seq", goodZone, "OK"},
	} {
		if got := code(Validate(c.id, c.content)); got != c.want {
			t.Errorf("%s %+v: %s, want %s", c.id, c.content, got, c.want)
		}
	}
}

func TestContentHashIsCanonical(t *testing.T) {
	a, err := fx("USD", "CNY", "7.123456", "cost_estimate").Hash()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := fx("USD", "CNY", "7.123456", "cost_estimate").Hash()
	quote, _ := fx("USD", "CNY", "7.123456", "quotation").Hash()
	later := fx("USD", "CNY", "7.123456", "cost_estimate")
	later.ValidUntil = end.Add(time.Microsecond)
	longer, _ := later.Hash()
	if len(a) != 64 || a != b || a == quote || a == longer {
		t.Fatalf("hashes %s %s %s %s", a, b, quote, longer)
	}
	raw, _ := fx("USD", "CNY", "7.123456", "cost_estimate").DataJSON()
	back := Content{Kind: KindFXRate, ValidFrom: start, ValidUntil: end}
	if err = back.DecodeData(raw); err != nil {
		t.Fatal(err)
	}
	if h, _ := back.Hash(); h != a {
		t.Fatal("stored data must hash as it was saved")
	}
}

// Python test_foundation.py test_independent_publication_and_ai_guard (domain half) and
// POL-INV-001: the last editor of a revision never publishes it.
func TestPublicationIsIndependentAndSupersedesAtomically(t *testing.T) {
	r := New(KindCurrency, "USD")
	if err := CheckSave(nil, 1); code(err) != "NOT_FOUND /args/id" {
		t.Fatal(code(err))
	}
	if _, fresh, _ := r.Save(currency("USD", 2, "US dollar"), reviewer, 1, start); !fresh {
		t.Fatal("the first save starts revision 1")
	}
	if w, fresh, _ := r.Save(currency("USD", 2, "USD"), editor, 1, start); fresh || w.Number != 1 || r.Version != 2 {
		t.Fatalf("a save replaces the working revision whole: %+v, version %d", w, r.Version)
	}
	if _, err := r.Publish(1, Facts{}, editor, 1, []string{"e"}, clock); code(err) != "INDEPENDENT_REVIEW_REQUIRED /args/revision" {
		t.Fatal(code(err))
	}
	if _, err := r.Publish(2, Facts{}, reviewer, 1, []string{"e"}, clock); code(err) != "NOT_FOUND /args/revision" {
		t.Fatal(code(err))
	}
	if superseded, err := r.Publish(1, Facts{}, reviewer, 1, []string{"e"}, clock); err != nil || superseded != nil {
		t.Fatal(err, superseded)
	}
	if _, err := r.Publish(1, Facts{}, reviewer, 1, []string{"e"}, clock); code(err) != "INVALID_STATE /args/revision" {
		t.Fatal(code(err))
	}
	first := r.Active
	mustSave(t, r, currency("USD", 2, "United States dollar"), editor, 1, clock)
	later := clock.Add(time.Hour)
	superseded, err := r.Publish(2, Facts{}, reviewer, 1, []string{"e"}, later)
	if err != nil || superseded != first || first.Status != Superseded || !first.EndedAt.Equal(later) || r.Active.Number != 2 || r.Version != 5 {
		t.Fatalf("supersede: %v %+v %+v version %d", err, superseded, r.Active, r.Version)
	}
	if code(InForce(first, clock)) != "OK" || code(InForce(first, later)) != "CONFIG_NOT_ACTIVE /" || code(InForce(r.Active, later)) != "OK" {
		t.Fatal("each revision is in force from its publication until the next one")
	}
	if code(InForce(r.Active, clock)) != "CONFIG_NOT_ACTIVE /" || code(InForce(nil, clock)) != "CONFIG_NOT_ACTIVE /" {
		t.Fatal("a revision is not in force before its publication")
	}
}

// Python test_foundation.py test_company_identity_and_immutable_currency_semantics (currency half)
// and test_retired_currency_cannot_be_redefined: POL-INV-002.
func TestCurrencyMeaningIsImmutableEvenAfterRetirement(t *testing.T) {
	r := publishedRoot(t, "CNY", currency("CNY", 2, "CNY"))
	mustSave(t, r, currency("CNY", 3, "changed"), editor, 1, clock)
	if _, err := r.Publish(2, Facts{}, reviewer, 1, []string{"e"}, clock); code(err) != "IMMUTABLE_DEFINITION /args/revision" {
		t.Fatal(code(err))
	}
	mustSave(t, r, currency("CNY", 2, "人民币"), editor, 1, clock)
	if _, err := r.Publish(2, Facts{}, reviewer, 1, []string{"e"}, clock); err != nil {
		t.Fatal("the name is not part of the meaning:", err)
	}

	usd := publishedRoot(t, "USD", currency("USD", 2, "USD"))
	retired, err := usd.Retire(clock)
	if err != nil || retired.Status != Retired || usd.Active != nil {
		t.Fatal(err, retired)
	}
	if _, err = usd.Retire(clock); code(err) != "INVALID_STATE /args/id" {
		t.Fatal(code(err))
	}
	mustSave(t, usd, currency("USD", 0, "USD"), editor, 1, clock)
	if _, err = usd.Publish(2, Facts{}, reviewer, 1, []string{"e"}, clock); code(err) != "IMMUTABLE_DEFINITION /args/revision" {
		t.Fatal(code(err))
	}
	discarded, err := usd.Discard(clock)
	if err != nil || discarded.Status != Discarded || usd.Working != nil {
		t.Fatal(err, discarded)
	}
	if w, _, _ := usd.Save(currency("USD", 2, "USD"), editor, 1, clock); w.Number != 3 {
		t.Fatalf("a discarded revision number is never reused: %d", w.Number)
	}
	if _, err = usd.Publish(3, Facts{}, reviewer, 1, []string{"e"}, clock); err != nil {
		t.Fatal("the same meaning may be published again:", err)
	}
}

// Python test_foundation.py test_expired_and_future_publication_cannot_replace_active_rate.
func TestExpiredAndFutureRevisionsCannotReplaceTheActiveRate(t *testing.T) {
	r := publishedRoot(t, "usd.cny", fx("USD", "CNY", "7.123456", "cost_estimate"))
	for _, offset := range []time.Duration{-48 * time.Hour, 48 * time.Hour} {
		c := fx("USD", "CNY", "7.2", "cost_estimate")
		c.ValidFrom, c.ValidUntil = start.Add(offset), end.Add(offset)
		c.FXRate.ObservedAt = kernel.FormatTimestamp(c.ValidFrom)
		mustSave(t, r, c, editor, 1, clock)
		if _, err := r.Publish(2, currenciesInForce, reviewer, 1, []string{"e"}, clock); code(err) != "CONFIG_EXPIRED /args/revision" {
			t.Fatalf("%v: %s", offset, code(err))
		}
		if r.Active.Number != 1 || r.Active.Status != Active {
			t.Fatalf("active %+v", r.Active)
		}
	}
}

func TestExchangeRateReferencesMustBeConfiguredAndInForce(t *testing.T) {
	c := fx("USD", "CNY", "7.123456", "cost_estimate")
	if got := code(CheckReferences("r", c, facts(currencyFact("USD", 2)))); got != "REFERENCE_INVALID /args/data/to_currency" {
		t.Fatal(got)
	}
	retired := currencyFact("CNY", 2)
	retired.Refused = reject(CodeConfigNotActive, "/")
	if got := code(CheckReferences("r", c, facts(currencyFact("USD", 2), retired))); got != "CONFIG_NOT_ACTIVE /args/data/to_currency" {
		t.Fatal(got)
	}
	if got := code(CheckReferences("r", c, currenciesInForce)); got != "OK" {
		t.Fatal(got)
	}
}

// Python test_foundation.py test_fx_direction_purpose_rounding_and_expiry,
// test_same_currency_explicit_one_and_missing_cross_rate_blocks and
// test_stale_fx_is_not_silently_reused_and_no_reciprocal: POL-INV-003.
func TestExchangeRateDirectionPurposeAndExpiry(t *testing.T) {
	rate := publishedRoot(t, "usd.cny", fx("USD", "CNY", "7.123456", "cost_estimate")).Active
	use := FXUse{From: "USD", To: "CNY", Purpose: "cost_estimate", At: clock}
	value, err := DecideFX(use, rate)
	if err != nil {
		t.Fatal(err)
	}
	usd, cny := kernel.MustCurrency("USD", 2), kernel.MustCurrency("CNY", 2)
	x, _ := kernel.NewExchangeRate(usd, cny, value)
	ten, _ := kernel.ParseAmount(usd, "10")
	if got, _ := x.Convert(ten); got.Amount() != "71.23" {
		t.Fatalf("10 USD at 7.123456 = %s CNY, want 71.23", got.Amount())
	}
	same, err := DecideFX(FXUse{From: "CNY", To: "CNY", Purpose: "cost_estimate", At: clock}, nil)
	one, _ := kernel.NewExchangeRate(cny, cny, same)
	if rounded, _ := kernel.RoundMoney(cny, kernel.MustDecimal("10.005").Mul(one.Rate())); err != nil || rounded.Amount() != "10.01" {
		t.Fatalf("same currency: %v %s", err, rounded.Amount())
	}
	for _, c := range []struct {
		use  FXUse
		rate *Revision
		want string
	}{
		{FXUse{From: "CNY", To: "CNY", Purpose: "cost_estimate", At: clock}, rate, "FX_SCOPE_MISMATCH /rate"},
		{use, nil, "FX_SCOPE_MISMATCH /rate"},
		{FXUse{From: "USD", To: "CNY", Purpose: "quotation", At: clock}, rate, "FX_SCOPE_MISMATCH /purpose"},
		{FXUse{From: "CNY", To: "USD", Purpose: "cost_estimate", At: clock}, rate, "FX_SCOPE_MISMATCH /rate"},
		{FXUse{From: "USD", To: "CNY", Purpose: "cost_estimate", At: end}, rate, "FX_STALE /rate"},
		{FXUse{From: "USD", To: "CNY", Purpose: "cost_estimate", At: clock, MaxAge: time.Minute}, rate, "FX_STALE /rate"},
		{FXUse{From: "USD", To: "CNY", Purpose: "cost_estimate", At: clock, MaxAge: 24 * time.Hour}, rate, "OK"},
		{FXUse{From: "USD", To: "CNY", Purpose: "cost_estimate", At: clock.Add(-time.Second)}, rate, "CONFIG_NOT_ACTIVE /rate"},
	} {
		_, err := DecideFX(c.use, c.rate)
		if got := code(err); got != c.want {
			t.Errorf("%+v: %s, want %s", c.use, got, c.want)
		}
	}
	r := publishedRoot(t, "usd.cny", fx("USD", "CNY", "7.123456", "cost_estimate"))
	if _, err = r.Retire(clock.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err = DecideFX(FXUse{From: "USD", To: "CNY", Purpose: "cost_estimate", At: clock.Add(time.Hour)}, r.Definition); code(err) != "CONFIG_NOT_ACTIVE /rate" {
		t.Fatal(code(err))
	}
}
