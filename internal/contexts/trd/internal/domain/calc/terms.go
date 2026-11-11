package calc

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// PriceBasis says whether an amount includes its tax.
type PriceBasis string

const (
	TaxInclusive     PriceBasis = "inclusive"      // the amount is the payable
	TaxExclusive     PriceBasis = "exclusive"      // the amount is the net
	TaxNotApplicable PriceBasis = "not_applicable" // no tax applies; the rate is 0
)

// Closed values of the POL configurations the sales terms read.
const (
	SideSales    = "sales"    // a tax application for a sale
	SidePurchase = "purchase" // a tax application for a purchase; it never answers for a sale

	SubjectSku         = "sku"          // a tax application's subject: goods of one SKU
	SubjectFeeCategory = "fee_category" // a tax application's subject: charges of one fee category

	FeeSalesCharge  = "sales_charge"  // a fee category of income the exporter charges the customer
	FeePurchaseCost = "purchase_cost" // a fee category of cost; never seller income

	InclusionSeparate = "separate"         // charged in addition to the lines
	InclusionIncluded = "included_in_line" // part of one line's price

	RecoveryUnknown = "unknown" // blocks every calculation: a tax treatment is never assumed
	Recoverable     = "recoverable"
	Nonrecoverable  = "nonrecoverable"
)

// TaxProfile is a POL tax_profile revision: rate (0–1), the price basis amounts are stated on and
// the recovery treatment.
type TaxProfile struct {
	Ref        Ref
	Rate       kernel.Decimal
	PriceBasis PriceBasis
	Recovery   string
}

// TaxApplication is a verified POL tax_application revision: the side of trade, the subject (a SKU
// or a fee category), the party it is for ("" for every party) and the tax profile revision it
// applies. Nothing here is inferred from a product name or from a rate of 0.
type TaxApplication struct {
	Ref         Ref
	Side        string
	SubjectKind string
	SubjectID   string
	PartyID     string
	Profile     TaxProfile
}

// FeeCategory is a POL fee_category revision as a sales charge reads it: its side of trade and
// whether it is charged separately or included in a line.
type FeeCategory struct {
	Ref       Ref
	Side      string
	Inclusion string
}

// TermsBinding is what BindTerms established (Python brant.quote-sales-terms/0.1.0): the tax
// binding of every line and charge, by role, and the binding of every charge, by charge ID.
type TermsBinding struct {
	Taxes   []TaxBinding
	Charges []ChargeBinding
}

// TaxBinding is the tax application and profile that one line ("line:<line_id>") or one charge
// ("charge:<obligation_key>") is taxed by.
type TaxBinding struct {
	Role        string
	Application Ref
	Profile     Ref
	PriceBasis  PriceBasis
	Rate        kernel.Decimal
}

// ChargeBinding is one sales charge as bound.
type ChargeBinding struct {
	ChargeID       string
	ObligationKey  string
	FeeCategory    Ref
	Amount         kernel.Decimal
	Inclusion      string
	IncludedLineID string
	Evidence       []Evidence
}

// Equal reports whether two bindings are the same.
func (b TermsBinding) Equal(o TermsBinding) bool {
	return slices.EqualFunc(b.Taxes, o.Taxes, func(x, y TaxBinding) bool {
		return x.Role == y.Role && x.Application == y.Application && x.Profile == y.Profile && x.PriceBasis == y.PriceBasis && x.Rate.Equal(y.Rate)
	}) && slices.EqualFunc(b.Charges, o.Charges, func(x, y ChargeBinding) bool {
		return x.ChargeID == y.ChargeID && x.ObligationKey == y.ObligationKey && x.FeeCategory == y.FeeCategory && x.Amount.Equal(y.Amount) &&
			x.Inclusion == y.Inclusion && x.IncludedLineID == y.IncludedLineID && slices.Equal(x.Evidence, y.Evidence)
	})
}

// Check applies the tax profile's own rules: a confirmed recovery treatment, a known price basis
// and a rate between 0 and 1 that is 0 where no tax applies. Each failure is TAX_POLICY_UNCONFIRMED
// at path.
func (t TaxProfile) Check(path string) error {
	switch {
	case t.Recovery != Recoverable && t.Recovery != Nonrecoverable:
		return fail(CodeTaxPolicyUnconfirmed, path, "The tax recovery treatment is not confirmed; it is never assumed")
	case t.PriceBasis != TaxInclusive && t.PriceBasis != TaxExclusive && t.PriceBasis != TaxNotApplicable:
		return fail(CodeTaxPolicyUnconfirmed, path, fmt.Sprintf("Price basis %q is not supported", t.PriceBasis))
	case !fits(t.Rate, FactorScale) || t.Rate.Sign() < 0 || t.Rate.Cmp(one) > 0:
		return fail(CodeTaxPolicyUnconfirmed, path, "A tax rate is between 0 and 1 with at most 12 fractional digits")
	case t.PriceBasis == TaxNotApplicable && !t.Rate.IsZero():
		return fail(CodeTaxPolicyUnconfirmed, path, "Where no tax applies the rate is 0")
	}
	return nil
}

// Applies checks, as TAX_POLICY_UNCONFIRMED at path, that the application answers for the sale of
// subject (kind sku or fee_category, and its ID) to customer: a sales application for exactly that
// subject, for every party or for this customer, with a confirmed tax profile. A missing
// application is never zero tax, and a purchase application never answers for a sale, not even at
// a rate of 0.
func (a *TaxApplication) Applies(kind, subject, customer, path string) error {
	switch {
	case a == nil:
		return fail(CodeTaxPolicyUnconfirmed, path, "No sales tax application is named; a missing one is never zero tax")
	case a.Side != SideSales:
		return fail(CodeTaxPolicyUnconfirmed, path, "A purchase tax application never authorizes a sale")
	case a.SubjectKind != kind || a.SubjectID != subject:
		return fail(CodeTaxPolicyUnconfirmed, path, fmt.Sprintf("The tax application is not for %s %s", kind, subject))
	case a.PartyID != "" && a.PartyID != customer:
		return fail(CodeTaxPolicyUnconfirmed, path, "The tax application is for another party")
	}
	return a.Profile.Check(path)
}

// BindTerms applies the sales terms of T03 §5.1 (Python t03_sales_terms.bind) to the quote's lines
// and charges, in document order.
//
// Every line is taxed by a sales application for its SKU and the customer (TaxApplication.Applies).
// Every charge:
//
//   - has a charge ID and an obligation key of its own: a repeated ID, or a missing key, is
//     CHARGE_SCOPE_UNRESOLVED; a repeated key is one obligation counted twice, CHARGE_DOUBLE_COUNT;
//   - names a fee category of the sales side (a purchase cost is never seller income) that no other
//     charge of the quote names, since one fee category is one fixed obligation here;
//   - states its inclusion, which is the fee category's: a separately charged fee is never claimed
//     as included (CHARGE_SCOPE_UNRESOLVED), and a fee the line price already includes is never
//     charged again (CHARGE_DOUBLE_COUNT);
//   - has a positive amount (INVALID_KNOWN_VALUE) and at least one piece of evidence;
//   - is taxed by a sales application for its fee category and the customer;
//   - when included, names an existing line taxed by the same tax profile, and the included charges
//     of a line together do not exceed its exact, unrounded base B; when separate, names no line.
//
// Every other failure of a charge is CHARGE_SCOPE_UNRESOLVED.
func BindTerms(q Quote) (TermsBinding, error) {
	if len(q.Lines) == 0 {
		return TermsBinding{}, fail(kernel.CodeDraftNotReady, q.Path+"/lines", "A quote has at least one line")
	}
	if len(q.Lines) > MaxLines {
		return TermsBinding{}, fail(CodeCalculationLimitExceeded, q.Path+"/lines", fmt.Sprintf("A quote has at most %d lines", MaxLines))
	}
	if len(q.Charges) > MaxCharges {
		return TermsBinding{}, fail(CodeCalculationLimitExceeded, q.Path+"/charges", fmt.Sprintf("A quote has at most %d sales charges", MaxCharges))
	}
	lines := make(map[string]Line, len(q.Lines))
	var taxes []TaxBinding
	for _, l := range q.Lines {
		if l.LineID == "" {
			return TermsBinding{}, fail(kernel.CodeDraftNotReady, l.Path+"/line_id", "")
		}
		if _, dup := lines[l.LineID]; dup {
			return TermsBinding{}, fail(kernel.CodeDuplicateLineID, l.Path+"/line_id", "")
		}
		lines[l.LineID] = l
		if err := l.Tax.Applies(SubjectSku, l.SkuID, q.Customer, l.Path+"/tax_application_ref"); err != nil {
			return TermsBinding{}, err
		}
		taxes = append(taxes, taxBinding("line:"+l.LineID, l.Tax))
	}

	ids := map[string]bool{}
	keys := map[string]bool{}
	fees := map[string]bool{}
	included := map[string]kernel.Decimal{}
	charges := make([]ChargeBinding, 0, len(q.Charges))
	for _, c := range q.Charges {
		scope := func(field, reason string) error { return fail(CodeChargeScopeUnresolved, c.Path+"/"+field, reason) }
		switch {
		case c.ChargeID == "" || ids[c.ChargeID]:
			return TermsBinding{}, scope("charge_id", "A sales charge has an ID of its own")
		case c.ObligationKey == "":
			return TermsBinding{}, scope("obligation_key", "A sales charge names its obligation; the charge ID never stands in for it")
		case keys[c.ObligationKey]:
			return TermsBinding{}, fail(CodeChargeDoubleCount, c.Path+"/obligation_key", "The obligation is already charged under another ID")
		case c.Fee == nil:
			return TermsBinding{}, scope("fee_category_ref", "A sales charge names its fee category")
		case fees[c.Fee.Ref.ID]:
			return TermsBinding{}, scope("fee_category_ref", "A fee category is one fixed obligation of a quote; several legs are not supported")
		case c.Fee.Side != FeeSalesCharge:
			return TermsBinding{}, scope("fee_category_ref", "Only a sales_charge fee category is seller income")
		case c.Inclusion != InclusionSeparate && c.Inclusion != InclusionIncluded:
			return TermsBinding{}, scope("inclusion", "A sales charge states whether it is separate or included; it is never assumed")
		case c.Fee.Inclusion == InclusionIncluded && c.Inclusion == InclusionSeparate:
			return TermsBinding{}, fail(CodeChargeDoubleCount, c.Path+"/inclusion", "The line price already includes this fee; it is not charged again")
		case c.Inclusion != c.Fee.Inclusion:
			return TermsBinding{}, scope("inclusion", "The fee category is charged separately; it is never claimed as included")
		}
		ids[c.ChargeID], keys[c.ObligationKey], fees[c.Fee.Ref.ID] = true, true, true
		if err := chargeAmount(c.Amount, c.Path+"/amount"); err != nil {
			return TermsBinding{}, err
		}
		if len(c.Evidence) == 0 || len(c.Evidence) > MaxEvidence {
			return TermsBinding{}, scope("evidence", fmt.Sprintf("A sales charge rests on 1 to %d pieces of evidence", MaxEvidence))
		}
		for i, e := range c.Evidence {
			if e.SourceID == "" || strings.TrimSpace(e.Locator) == "" {
				return TermsBinding{}, fail(kernel.CodeReferenceInvalid, index(c.Path, "evidence", i), "Evidence names a source and a locator")
			}
		}
		if err := c.Tax.Applies(SubjectFeeCategory, c.Fee.Ref.ID, q.Customer, c.Path+"/tax_application_ref"); err != nil {
			return TermsBinding{}, err
		}
		if c.Inclusion == InclusionIncluded {
			line, ok := lines[c.IncludedLineID]
			if !ok {
				return TermsBinding{}, scope("included_line_id", "An included charge names a line of the quote")
			}
			if line.Tax.Profile.Ref != c.Tax.Profile.Ref {
				return TermsBinding{}, scope("tax_application_ref", "An included charge is taxed like the line that includes it")
			}
			base, err := line.Base()
			if err != nil {
				return TermsBinding{}, err
			}
			sum := included[line.LineID].Add(c.Amount)
			if sum.Cmp(base) > 0 {
				return TermsBinding{}, scope("amount", fmt.Sprintf("The charges included in line %s exceed its amount %s", line.LineID, base))
			}
			included[line.LineID] = sum
		} else if c.IncludedLineID != "" {
			return TermsBinding{}, scope("included_line_id", "A separate charge is included in no line")
		}
		taxes = append(taxes, taxBinding("charge:"+c.ObligationKey, c.Tax))
		charges = append(charges, ChargeBinding{
			ChargeID: c.ChargeID, ObligationKey: c.ObligationKey, FeeCategory: c.Fee.Ref, Amount: c.Amount,
			Inclusion: c.Inclusion, IncludedLineID: c.IncludedLineID, Evidence: slices.Clone(c.Evidence),
		})
	}
	slices.SortFunc(taxes, func(a, b TaxBinding) int { return cmp.Compare(a.Role, b.Role) })
	slices.SortFunc(charges, func(a, b ChargeBinding) int { return cmp.Compare(a.ChargeID, b.ChargeID) })
	return TermsBinding{Taxes: taxes, Charges: charges}, nil
}

func taxBinding(role string, a *TaxApplication) TaxBinding {
	return TaxBinding{Role: role, Application: a.Ref, Profile: a.Profile.Ref, PriceBasis: a.Profile.PriceBasis, Rate: a.Profile.Rate}
}

// chargeAmount admits a fixed charge amount: positive, of the price format, below 10^12. Zero is
// not a charge and a negative amount is no credit note.
func chargeAmount(d kernel.Decimal, path string) error {
	if d.Sign() <= 0 {
		return fail(kernel.CodeInvalidKnownValue, path, "A sales charge is positive; zero is no charge and a negative amount no credit")
	}
	return positive(d, PriceScale, path, "A charge amount")
}
