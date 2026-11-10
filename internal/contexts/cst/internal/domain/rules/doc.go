// Package rules holds the CST calculation rules as pure functions (WBS S2-04, S2-08): price tiers,
// purchase quantities with MOQ and order multiples, charge obligations and their inclusion graph,
// chargeable quantities and billing, tax cost treatment, conversion into the calculation currency,
// allocation, measurements and the comparison of cash flows. It reads no store and keeps no state;
// the CostScenario evaluation and the freeze of a CostSnapshot call it with inputs they resolved and
// checked for access and effect.
//
// The order of a scenario evaluation (T02 §6, §13.3), which the caller follows:
//
//  1. Purchase aggregates the scenario's allocations per condition line, checks MOQ per explicit
//     group, purchase quantity against demand and order multiples, and prices each condition line
//     once (Schedule.Amount) as an exact Decimal in the condition's currency.
//  2. PlanCharges decides which purchase components and fees are payable, included in another
//     component or borne by the customer, and that every obligation the seller bears is covered.
//  3. A payable fee's quantity comes from ChargeableQuantity and its amount from Billing.Amount.
//  4. Cost applies the tax treatment (HALF_UP to the original currency's minor units, the first
//     rounding point) and converts with the scenario's cost_estimate rate (the second).
//  5. Allocate splits each component over target lines in integer minor units; the amount basis
//     uses the purchase components' line amounts, fixed before any common fee is allocated.
//  6. Total sums the component amounts; the scenario total equals the sum of the line amounts.
//
// Arithmetic is exact (kernel.Decimal) between rounding points. Inputs carry the scale of their
// contract format (quantities 6, prices and amounts 8, factors 12 fractional digits) and the
// bounds of T02 §13.6; the rules refuse a value outside them, so every intermediate rational stays
// far below the 256-digit bound of T02 §13.9.
//
// Every input that a rejection can point at carries a Path: the JSON Pointer of that element in
// the caller's request or stored document, such as "/args/data/lines/0". A rejection's path is the
// element's Path followed by the contract field ("/args/data/lines/0/tiers/1/upper_exclusive");
// list indexes are positions in the slices the caller passed, in document order.
package rules
