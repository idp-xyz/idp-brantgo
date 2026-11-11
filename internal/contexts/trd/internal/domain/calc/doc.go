// Package calc holds TRD's server-side quotation calculation as pure functions (WBS S3-02, T03
// §5): the cost scope a quote adopts (TRD-INV-006; Python T03 A27), the sales terms it binds, that
// is tax applicability and sales charges (A28), the line, charge and tax amounts, the net sales in
// the comparison currency through a quotation exchange rate (A32), the margin verdict (TRD-INV-007)
// and the payment plan (TRD-INV-008). It reads no store, keeps no state and never takes an amount,
// a total or a verdict from the client: the QuoteRoot freeze calls Calculate with the inputs the
// application layer resolved and checked for access and effect (references in force, sources
// verified, guards held), and stores the Calculation it returns. A later decision (activation,
// quote_ready, order preparation) calls BindScope and BindTerms again on the current inputs and
// compares the bindings with Equal.
//
// The order of Calculate, which callers of the parts follow as well:
//
//  1. BindScope: the quote covers the complete RFQ line set of the adopted cost snapshot, each RFQ
//     line once, in SKU, specification and converted quantity, on the snapshot's pricing policy,
//     calculation currency, trade and payment terms, named place and delivery, which are also the
//     customer's trading profile terms.
//  2. BindTerms: every line and every sales charge has a verified sales-side tax application for
//     its subject and the customer; every charge is one fixed obligation of a sales fee category,
//     charged separately or included in one line, never counted twice.
//  3. PriceLine and PriceCharge: B = quantity × unit_price ÷ price_basis_quantity × (1 −
//     discount_ratio) exactly, split by its tax profile and rounded HALF_UP to the quote
//     currency's minor units per line and per charge; an included charge adds nothing.
//  4. Total: net, tax and payable are sums of the rounded amounts (TRD-INV-008).
//  5. Compare: the net sales in the comparison currency (the pricing policy's calculation
//     currency); the same currency as is, another one converted once, HALF_UP, by the quotation
//     rate the quote names.
//  6. CheckMargin: R − C ≥ R × minimum_margin compared exactly (TRD-INV-007).
//  7. AllocatePayments: the payable over the payment stages by largest remainder (TRD-INV-008).
//
// Arithmetic is exact (kernel.Decimal) between rounding points; amounts are kernel.Money in
// integer minor units below 10^24, each and in total. Inputs carry the scale of their contract
// format, as kernel.ParseDecimal parsed them: quantities 6 fractional digits, unit prices, price
// basis quantities, charge amounts and discount ratios 8, tax and exchange rates, conversion
// factors and the minimum margin 12, payment percentages 6, and at most 12 integer digits.
//
// Every input a rejection can point at carries a Path, the JSON Pointer of that element in the
// caller's request or stored document, such as "/args/data/lines/0". A rejection's path is that
// Path followed by the contract field, named as in Python's T03 quote data with references
// suffixed _ref. Off Quote.Path: rfq_ref, customer_ref, contact_ref, currency_ref,
// pricing_policy_ref, cost_snapshot_ref, trade_terms_ref, payment_terms_ref, named_place_ref,
// delivery, sales_fx_ref, lines, charges. Off a Line's Path: line_id, rfq_line_id, sku_id,
// unit_ref, quantity, base_quantity, conversion_ref, unit_price, price_basis_quantity,
// discount_ratio, tax_application_ref. Off a Charge's Path: charge_id, obligation_key,
// fee_category_ref, amount, inclusion, included_line_id, evidence, tax_application_ref. A caller
// whose payload names a field otherwise maps the path.
package calc
