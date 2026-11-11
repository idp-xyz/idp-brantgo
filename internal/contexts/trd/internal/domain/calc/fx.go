package calc

import (
	"errors"
	"fmt"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Purposes of an exchange rate; only a quotation rate prices a sale.
const (
	PurposeQuotation    = "quotation"
	PurposeCostEstimate = "cost_estimate"
)

// FXRate is the POL fx_rate revision a quote names (sales_fx_ref): one From is Rate To, for
// Purpose, observed at ObservedAt. Whether the revision is published and in force is POL's verdict,
// which the caller checks (LookupFX before its transaction, ConfigGuard inside it).
type FXRate struct {
	Ref        Ref
	From, To   kernel.Currency
	Rate       kernel.Decimal
	Purpose    string
	ObservedAt time.Time
}

// Comparison is the quote's net sales in the comparison currency: the revenue R the margin is
// judged on, and the rate it was converted by (nil for the same currency, an explicit factor of 1).
// The customer still pays the payable in the quote currency.
type Comparison struct {
	Currency kernel.Currency
	FX       *FXRate
	Revenue  kernel.Money
}

// Compare converts net, the quote's net sales, into target, the pricing policy's calculation
// currency, at instant at (T03 §5.2, Python A32): the same currency is taken as is and names no
// rate; another currency needs the rate the quote names, exactly net's currency → target and of
// purpose quotation, observed no later than at and no longer than maxAge before it. The net sales
// are summed in minor units first and converted once, HALF_UP to target's minor units: 960.00 EUR
// at 1.1 is 1056.00 USD. A missing rate is never 1, a target → quote rate is never inverted and a
// cost_estimate rate never prices a sale: each is FX_SCOPE_MISMATCH, at path + "/sales_fx_ref",
// as is a rate named for the same currency; an observation outside the age window is FX_STALE.
func Compare(path string, net kernel.Money, target kernel.Currency, fx *FXRate, at time.Time, maxAge time.Duration) (Comparison, error) {
	ref := path + "/sales_fx_ref"
	from := net.Currency()
	if from.Code() == target.Code() {
		switch {
		case from != target:
			return Comparison{}, fail(kernel.CodeReferenceInvalid, path+"/currency_ref", fmt.Sprintf("Two definitions of %s disagree on minor units", from))
		case fx != nil:
			return Comparison{}, fail(CodeFXScopeMismatch, ref, "A quote in the calculation currency names no exchange rate; its factor is exactly 1")
		}
		return positiveRevenue(path, Comparison{Currency: target, Revenue: net})
	}
	switch {
	case fx == nil:
		return Comparison{}, fail(CodeFXScopeMismatch, ref, fmt.Sprintf("A %s quote needs a quotation rate into %s; a missing rate is never 1", from, target))
	case fx.Purpose != PurposeQuotation:
		return Comparison{}, fail(CodeFXScopeMismatch, ref, fmt.Sprintf("A %s rate never prices a sale; a quote needs a quotation rate", fx.Purpose))
	case fx.From.Code() == target.Code() && fx.To.Code() == from.Code():
		return Comparison{}, fail(CodeFXScopeMismatch, ref, fmt.Sprintf("The rate converts %s into %s; a rate is never inverted", target, from))
	case fx.From.Code() != from.Code() || fx.To.Code() != target.Code():
		return Comparison{}, fail(CodeFXScopeMismatch, ref, fmt.Sprintf("The rate does not convert %s into %s", from, target))
	case fx.From != from || fx.To != target:
		return Comparison{}, fail(kernel.CodeReferenceInvalid, ref, "The rate's currency definitions disagree on minor units")
	case !fits(fx.Rate, FactorScale) || fx.Rate.Sign() <= 0 || fx.Rate.Cmp(integerLimit) >= 0:
		return Comparison{}, fail(kernel.CodeInvalidKnownValue, ref, "An exchange rate is positive with at most 12 fractional digits")
	case maxAge <= 0:
		return Comparison{}, fail(kernel.CodeInvalidKnownValue, path+"/pricing_policy_ref", "The pricing policy sets no maximum exchange rate age")
	case at.IsZero():
		return Comparison{}, fail(kernel.CodeInvalidKnownValue, ref, "The calculation has no instant to judge the rate's age at")
	}
	now, observed := kernel.Instant(at), kernel.Instant(fx.ObservedAt)
	switch {
	case fx.ObservedAt.IsZero() || observed.After(now):
		return Comparison{}, fail(CodeFXStale, ref, "The exchange rate was observed after the calculation")
	case now.Sub(observed) > maxAge:
		return Comparison{}, fail(CodeFXStale, ref, fmt.Sprintf("The exchange rate was observed more than %s before the calculation", maxAge))
	}
	rate, err := kernel.NewExchangeRate(fx.From, fx.To, fx.Rate)
	if err != nil {
		return Comparison{}, fail(kernel.CodeInvalidKnownValue, ref, err.Error())
	}
	revenue, err := rate.Convert(net)
	if errors.Is(err, kernel.ErrAmountOutOfRange) {
		return Comparison{}, limited(ref, "The converted net sales")
	} else if err != nil {
		return Comparison{}, fail(kernel.CodeInvalidKnownValue, ref, err.Error())
	}
	used := *fx
	return positiveRevenue(path, Comparison{Currency: target, FX: &used, Revenue: revenue})
}

func positiveRevenue(path string, c Comparison) (Comparison, error) {
	if c.Revenue.Sign() <= 0 {
		return Comparison{}, fail(kernel.CodeInvalidKnownValue, path+"/lines", "The net sales in the comparison currency are positive")
	}
	return c, nil
}
