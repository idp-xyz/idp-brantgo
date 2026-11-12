// Package orderdoc renders an order revision into its export sales contract (S/C) or proforma
// invoice (PI), binds the document to the revision, and keeps the signed copies (WBS S3-15;
// TRD-INV-020; tactical model TRD-AGG-SalesOrder OrderContractDocument, order.document.generate and
// order.document.record_signed; UL TRD-SalesContractDocument; Context Map R46). Python has none of
// it. It reads no store and keeps no state; it follows package document (quote-text/0.1.0) in form.
//
// A document is generated in three steps, so the number and the file either both bind to the
// revision or neither does:
//
//  1. Before its transaction the command reads the order revision, the POL order_document_template
//     revision, the number sequence and the Facts, and calls Prepare at the generation instant.
//     Prepare checks everything but the number and returns a Plan naming the number to request.
//  2. The command asks POL for the number (number.issue with Plan.Number). Its business reference is
//     the order ID, so every revision's S/C carries the order's one contract number, and its PI the
//     order's one PI number (document types order and proforma_invoice); the file name tells the
//     revisions apart.
//  3. Plan.Generate renders the document with that number: the file and the Binding of number,
//     file and template to the order revision and its hash. The command stores the binding, the
//     bytes and its commit evidence in one transaction. Verify checks stored bytes on every read.
//
// A document binds one order revision (TRD-INV-020). Once the order is amended, Current refuses the
// old revision's documents (ORDER_DOCUMENT_STALE): they stay readable, read-only, and the new
// revision needs documents of its own. Sign records a signed or countersigned copy — an EVD file,
// immutable — of a current document; the generated, unsigned bytes are never a signed copy
// (草稿不能冒充已签署件).
//
// # order-text/0.1.0
//
// UTF-8 without a byte order mark, "\n" line breaks, the template's title, a blank line, the
// template's sections in its order separated by blank lines, and a final "\n" (at most 1 MiB):
//
//	seller      Seller: <legal name>
//	            <address>
//	            Registration: <registration no> | Country: <country>
//	buyer       Buyer: <customer name>
//	            <buyer address, else billing address>
//	            Contact: <contact name>
//	identity    Number: <number>
//	            Order: <order_id> r<order_revision>
//	            Order SHA-256: <order hash>
//	            Quote: <quote number> (<quote_id> r<business_revision>) | Acceptance: <acceptance_id>
//	            Amendment: <amendment_id> of r<previous revision>          (a revision after the first)
//	            Date: <generation instant, UTC>
//	            Not a commercial invoice or a tax invoice.               (a PI)
//	lines       Lines (amounts in <currency code>):
//	            <line_id> | <item> | <specification> | <quantity> <unit> | price <unit_price> per <price_basis_quantity> <unit> | discount <discount_ratio> | net <amount> | tax <amount> | total <amount>
//	            charge <charge_id> | <obligation_key> | <inclusion> | net <amount> | tax <amount> | total <amount>
//	totals      Currency: <code>
//	            Net: <amount> | Tax: <amount> | Total: <amount>
//	terms       Terms: <incoterm> <edition> | <responsibility note>
//	            Named place: <place> | Trade terms: <id> r<revision>
//	delivery    Delivery: <date> | <milestone> | <time zone>
//	            (a lead time: <days> <day basis> days after <anchor id> r<revision> | calendar <id> r<revision> | <milestone> | <time zone>)
//	payments    Payment plan (planned amounts, not receipts):
//	            <stage_id> | <percent>% | <amount> | <trigger> | <days> <day basis> days[ | calendar <id> r<revision>]
//	clauses     Clauses:
//	            1. <clause>
//	law         Legal terms: <id> r<revision>
//	            Governing law: <governing_law> | CISG: <applies or excluded>
//	            Contract formation: acceptance effective on arrival | <CISG Art. 18(2), 23 or PRC Civil Code Art. 137, 483, 484>
//	            Disputes: arbitration | <institution> | seat <seat> | language <language>
//	            (or) Disputes: litigation | <court>
//	signatures  Signatures (this generated copy is unsigned; signed copies are recorded as evidence):
//	            Seller: <legal name>
//	            Buyer: <customer name>
//	notes       <footer note>
//
// Lines in line_id order and charges in charge_id order (the revision's), payment stages in the
// plan's order. Amounts are the revision's minor-unit integers printed in major units with the
// currency's minor digits ("928.00"); quantities, prices and ratios as the revision holds them
// (shortest exact decimals); instants in UTC as stored.
//
// Sections: an S/C lists seller, buyer, identity, lines, totals, terms, delivery, payments, clauses,
// signatures and notes once each, and may list law; a PI lists seller, buyer, identity, lines,
// totals, terms, delivery, payments and notes, and may list clauses and law. A template that lists
// clauses has 1–50 of them; one that does not has none.
//
// The law section prints the legal terms the order revision carries (WBS S3-17, F-02: 适用法律与争议解决
// 条款) — the source quote revision's sealed legal terms, copied by the order's snapshot and never
// read from POL's current revision — in the lines the quote's formal document printed them
// (document.LegalLines, quote-text/0.2.0). A template prints the law section exactly when the
// revision carries legal terms (REFERENCE_INVALID at /template/sections otherwise): a contract never
// leaves out the legal terms its order binds, and never prints terms it does not bind.
//
// Only language en (DEPENDENCY_NOT_SUPPORTED otherwise), as quote-text/0.1.0. Refusals point into the
// inputs: the revision at /revision, the template at /template/<member>, the facts at
// /facts/<member>, the number at /number.
package orderdoc
