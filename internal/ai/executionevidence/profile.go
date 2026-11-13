package executionevidence

import (
	"errors"
	"fmt"
	"regexp"

	tskapi "go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// ModelProfile is the model a runner calls and what a call costs (baseline 19.3 ModelProfile; decision
// E-4: the local OpenAI-compatible service costs 0). Prices are exact decimals in Currency per
// million tokens; ReservePerCall is what the execution entry reserves before each call and settles
// against the actual cost after it (baseline 17.4). The endpoint, key and timeouts are deployment,
// not part of the profile.
type ModelProfile struct {
	ProfileID       string `json:"profile_id"`
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	MaxOutputTokens int    `json:"max_output_tokens"`
	Currency        string `json:"currency"`
	MinorUnits      int    `json:"minor_units"`
	InputPerMTok    string `json:"input_per_mtok"`
	OutputPerMTok   string `json:"output_per_mtok"`
	ReservePerCall  string `json:"reserve_per_call"`
}

var (
	provider = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	currency = regexp.MustCompile(`^[A-Z]{3}$`)

	// ErrCurrency is a cost that cannot be measured in the budget's currency.
	ErrCurrency = errors.New("executionevidence: the model's prices are in another currency than the budget")

	priceFormat = kernel.PriceFormat
	million     = kernel.DecimalFromInt(1_000_000)
)

// Validate checks the profile.
func (p ModelProfile) Validate() error {
	bad := func(what string) error { return fmt.Errorf("%w: model profile %s: %s", ErrInvalid, p.ProfileID, what) }
	switch {
	case !kernel.ValidIdentifier(p.ProfileID):
		return bad("profile id")
	case !provider.MatchString(p.Provider):
		return bad("provider")
	case len(p.Model) == 0 || len(p.Model) > 200:
		return bad("model")
	case p.MaxOutputTokens < 1:
		return bad("max output tokens")
	case !currency.MatchString(p.Currency) || p.MinorUnits < 0 || p.MinorUnits > tskapi.MaxMinorUnits:
		return bad("currency")
	}
	for _, s := range []string{p.InputPerMTok, p.OutputPerMTok, p.ReservePerCall} {
		if _, err := kernel.ParseDecimal(s, priceFormat); err != nil {
			return bad("price " + s)
		}
	}
	return nil
}

func (p ModelProfile) prices() (in, out, reserve kernel.Decimal, err error) {
	if in, err = kernel.ParseDecimal(p.InputPerMTok, priceFormat); err != nil {
		return
	}
	if out, err = kernel.ParseDecimal(p.OutputPerMTok, priceFormat); err != nil {
		return
	}
	reserve, err = kernel.ParseDecimal(p.ReservePerCall, priceFormat)
	return
}

// Free reports whether every price of the profile is zero, so its cost is zero in any currency.
func (p ModelProfile) Free() bool {
	in, out, reserve, err := p.prices()
	return err == nil && in.IsZero() && out.IsZero() && reserve.IsZero()
}

func (p ModelProfile) currencyFits(c kernel.Currency) bool {
	return p.Free() || (p.Currency == c.Code() && p.MinorUnits == c.MinorUnits())
}

// Reservation returns what a call reserves in c before it is made.
func (p ModelProfile) Reservation(c kernel.Currency) (kernel.Money, error) {
	if !p.currencyFits(c) {
		return kernel.Money{}, ErrCurrency
	}
	_, _, reserve, err := p.prices()
	if err != nil {
		return kernel.Money{}, err
	}
	return kernel.RoundMoney(c, reserve)
}

// Cost returns the cost of a call that used in input and out output tokens, in c: exact until the
// one HALF_UP rounding to c's minor units.
func (p ModelProfile) Cost(c kernel.Currency, in, out int64) (kernel.Money, error) {
	if in < 0 || out < 0 {
		return kernel.Money{}, fmt.Errorf("%w: negative token count", ErrInvalid)
	}
	if !p.currencyFits(c) {
		return kernel.Money{}, ErrCurrency
	}
	inPrice, outPrice, _, err := p.prices()
	if err != nil {
		return kernel.Money{}, err
	}
	total := kernel.DecimalFromInt(in).Mul(inPrice).Add(kernel.DecimalFromInt(out).Mul(outPrice))
	perToken, err := total.Quo(million)
	if err != nil {
		return kernel.Money{}, err
	}
	return kernel.RoundMoney(c, perToken)
}

// Limits is the budget an AI task runs under: model calls, retries of unavailable calls, and the
// cost cap in Currency (nil: none). Source says whether the task named it ("task") or the runner's
// default applied ("runner").
type Limits struct {
	MaxModelCalls int
	MaxRetries    int
	Currency      kernel.Currency
	MaxCost       *kernel.Money
	Source        string
}

// Limits returns the limits a bundle's default budget states, with source.
func (s BudgetSpec) Limits(source string) (Limits, error) {
	bad := func(what string) error { return fmt.Errorf("%w: budget: %s", ErrInvalid, what) }
	if s.MaxModelCalls < 1 || s.MaxModelCalls > tskapi.MaxModelCalls {
		return Limits{}, bad("max model calls")
	}
	if s.MaxRetries < 0 || s.MaxRetries > tskapi.MaxRetries {
		return Limits{}, bad("max retries")
	}
	if !currency.MatchString(s.Currency) || s.MinorUnits < 0 || s.MinorUnits > tskapi.MaxMinorUnits {
		return Limits{}, bad("currency")
	}
	cur, err := kernel.NewCurrency(s.Currency, s.MinorUnits)
	if err != nil {
		return Limits{}, bad("currency")
	}
	out := Limits{MaxModelCalls: s.MaxModelCalls, MaxRetries: s.MaxRetries, Currency: cur, Source: source}
	if s.MaxCost != "" {
		m, err := kernel.ParseAmount(cur, s.MaxCost)
		if err != nil || m.Sign() < 0 {
			return Limits{}, bad("max cost")
		}
		out.MaxCost = &m
	}
	return out, nil
}

// LimitsOf returns the limits of a task's budget (task.create budget), or fallback when the task
// names none. A task's budget without a cost cap keeps fallback's currency for the cost it records.
func LimitsOf(b *tskapi.Budget, fallback Limits) (Limits, error) {
	if b == nil {
		return fallback, nil
	}
	out := Limits{MaxModelCalls: b.MaxModelCalls, MaxRetries: b.MaxRetries, Currency: fallback.Currency, Source: "task"}
	if out.MaxModelCalls < 1 || out.MaxModelCalls > tskapi.MaxModelCalls || out.MaxRetries < 0 || out.MaxRetries > tskapi.MaxRetries {
		return Limits{}, fmt.Errorf("%w: the task's budget %+v", ErrInvalid, *b)
	}
	if b.MaxCost != nil {
		m, err := b.MaxCost.Cost()
		if err != nil {
			return Limits{}, err
		}
		out.Currency, out.MaxCost = m.Currency(), &m
	}
	return out, nil
}
