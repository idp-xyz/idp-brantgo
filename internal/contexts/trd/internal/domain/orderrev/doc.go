// Package orderrev holds the revisions of an active sales order as values, and the rules that judge
// them (WBS S3-15; tactical model TRD-AGG-SalesOrder: OrderRevision, OrderHash, OrderCommercialSnapshot;
// SM-TRD-ORDER after activation; TRD-INV-018, 019; UL TRD-OrderRevision, UL-INV-21, UL-INV-27).
// Python has none of this (Python 未实现); the design is the only source. It reads no store and
// keeps no state.
//
// # Order revisions
//
// An order revision is an immutable commercial version of a sales order. Revision 1 is the
// commercial snapshot order.prepare built (brant.order-commercial-snapshot/0.1.0: the sealed quote
// revision copied whole with the acceptance it binds); its SHA-256 is the order hash. Every later
// revision is written by order.amendment.apply as a document of its own,
// brant.order-revision/0.1.0, whose SHA-256 is that revision's order hash:
//
//	revision_contract   "brant.order-revision/0.1.0"
//	scope               {tenant, company}
//	order_id, order_revision
//	previous            {order_revision, order_hash}      the revision it amends
//	amendment           {amendment_id, amendment_hash}    the approved amendment that produced it
//	source              {quote_id, business_revision, business_hash, number, document_sha256,
//	                     acceptance_id}                   the contract's origin, never changed
//	parties             {issuer, customer}                the source quote's party snapshots
//	customer_ref, contact_ref, currency_ref               never changed by an amendment
//	legal_terms_ref?, legal_terms?                        the source quote's sealed legal terms (WBS
//	                                                      S3-17), when it sealed any; never changed
//	terms               {trade_terms_ref, named_place_ref, payment_terms_ref, delivery,
//	                     notify_party_ref?}                the notify party (S5R-D40), when bound
//	lines               [{line_id, rfq_line_id?, sku_ref, quantity, unit_ref, conversion_ref?,
//	                      normalized_quantity, cost_unit, unit_price, price_basis_quantity,
//	                      discount_ratio, tax {tax_application_ref, tax_profile_ref, price_basis,
//	                      rate}}]                         in line_id order
//	charges             [{charge_id, fee_category_ref, obligation_key, amount, inclusion,
//	                      included_line_id?, tax {...}}]  in charge_id order
//	amounts             {lines, charges, net_minor, tax_minor, payable_minor, payments}
//	seal?               {incoterm, incoterm_edition, addresses [{address_id, country?}],
//	                     notify_party? {address, country?}}  what POL and PTY stated (S5R-D35;
//	                                                      seal.go); absent before S5R
//
// in the canonical JSON of the quote's business hash (package quote: members in code point order,
// no whitespace, strings escaped as Python's json.dumps(ensure_ascii=False) does), with every
// decimal in its shortest exact form ("44", not "44.000000") and every amount in integer minor
// units of the order currency. Revision (revision.go) is the same value for both forms:
// FromSnapshot reads revision 1 from the snapshot bytes, Parse a later one from its document. The
// chain previous → order_hash makes the history tamper evident: each revision names the hash of
// the one it amends.
//
// # What an amendment may change
//
// An amendment proposes the complete commercial content of the next revision (Proposal): every
// line, every charge, and the terms. Next derives the revision from the current one, the proposal,
// the shipments FUL confirmed and the upstream facts; nothing is taken from the client but the
// proposal, and every amount is recalculated by the quote calculation's own rules (package calc:
// B = quantity × unit_price ÷ price_basis_quantity × (1 − discount_ratio), split by the line's tax
// profile and rounded HALF_UP per line; totals as sums; the payment plan by largest remainder).
//
//   - Lines: the quantity, the price (unit_price, price_basis_quantity, discount_ratio), the SKU
//     specification (another revision or another SKU) and its tax application change; lines are
//     added (no RFQ line: they were never quoted) and removed. A line keeps its unit (remove it and
//     add another).
//   - A line with confirmed shipments keeps at least the shipped quantity (QUANTITY_BELOW_SHIPPED,
//     also when it is removed; UL-INV-21) and keeps its SKU and price (INVALID_STATE): the shipped
//     goods were of that SKU at that price, and an amendment never rewrites what happened (UL
//     TRD-OrderAmendment: 不覆盖已发生的采购、出运与结算事实). To reprice the rest, reduce the line to
//     the shipped quantity and add a line for the remainder.
//   - Charges: an existing charge keeps or changes its amount or is removed. A new charge, or
//     another fee category, inclusion or tax for one, is DEPENDENCY_NOT_SUPPORTED in this first
//     version.
//   - Terms: the trade terms, the named place, the payment terms (the plan follows the new terms'
//     stages), the delivery and the notify party (S5R-D40: bound, changed or dropped) change. The
//     next revision seals the Incoterm of its trade terms, its customer addresses' countries and
//     its notify party's address as POL and PTY state them (Facts.Seal; S5R-D35).
//   - Never: the order, its scope, its source quote revision and acceptance, the issuer, the
//     customer, the contact, the currency and the legal terms. A proposal has no member for them; another customer
//     or currency is another contract.
//
// # Ending an active order (TRD-INV-019)
//
// end.go decides SM-TRD-ORDER's transitions out of active: order.cancel (no confirmed shipment —
// ORDER_HAS_SHIPMENTS — and no open downstream commitment without an approved disposition —
// DOWNSTREAM_COMMITMENT_OPEN), order.close_remainder (short closing: something shipped, something
// left, a reason) and order.close (every line shipped as ordered within its approved tolerance —
// ORDER_NOT_FULFILLED). Each keeps the conversion slot and the history. Shipments are FUL's facts
// (Fulfilment) and commitments PUR's and STL's (Downstream); this package judges what it is given and
// records where it came from, so a caller that cannot ask FUL or PUR yet says so (NotWired) rather
// than passing an empty answer off as a checked one.
package orderrev
