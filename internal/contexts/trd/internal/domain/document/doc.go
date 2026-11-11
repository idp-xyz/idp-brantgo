// Package document renders a frozen quote revision into its formal document and plans the
// formalization that seals it (WBS S3-04, TRD-INV-009 and TRD-INV-016; Python V0.50
// t03_candidate/document.py, renderer quote-text/0.1.0, and brant_core/foundation.py
// document_render for plain-text templates). It reads no store and keeps no state.
//
// A formalization (quote.activate, quote.replace) runs in three steps, so the number and the file
// either both bind to the revision or neither does:
//
//  1. Before its transaction the command reads the frozen revision, the POL template revision and
//     the Facts, and calls Prepare. Prepare checks everything that does not depend on the number,
//     the size of the document included, and returns a Plan naming the number to request.
//  2. The command asks POL for the number (number.issue with Plan.Number). POL keeps the number of a
//     business reference, so a retry after a rollback gets the same number back; a revision that is
//     never formalized leaves a gap.
//  3. Plan.Formalize renders the document with that number: the file (name, MIME type, bytes,
//     SHA-256) and the Binding of number, file and template to the revision. The command stores
//     the binding, the bytes and its commit evidence (Binding.Entries plus its own entries) in one
//     transaction; any failure rolls all of it back. On every later read Verify checks the stored
//     bytes against their binding.
//
// The document is a function of the business-hashed commercial document, the template revision
// (pinned by its content hash in the dependency manifest), the number and the Facts: Prepare
// refuses a stored document that is not its own normalized canonical form, so two revisions with
// one business hash render the same bytes.
//
// # quote-text/0.1.0
//
// UTF-8 without a byte order mark, "\n" line breaks, the template's title, a blank line, the ten
// sections in the template's order separated by blank lines, and a final "\n" (at most 1 MiB):
//
//	issuer     Issuer: <legal name>
//	           <address>
//	recipient  Customer: <name>
//	           <address>
//	           Contact: <name>
//	identity   Number: <number>
//	           Quote: <quote_id> r<business_revision>
//	           Business SHA-256: <business_hash>
//	lines      Lines (all amounts in currency minor units):
//	           <line_id> | <item> | <specification> | <quantity> <unit> | price <unit_price> per <price_basis_quantity> | discount <discount_ratio> | net <minor> | tax <minor> | payable <minor>
//	           charge <charge_id> | <obligation_key> | <inclusion> | net <minor> | tax <minor> | payable <minor>
//	totals     Currency: <code> | minor digits: <n>
//	           Net: <minor> | Tax: <minor> | Payable: <minor>
//	terms      Terms: <incoterm> <edition> | <responsibility note>
//	           Named place: <place> | Scope: <id> r<revision>
//	delivery   Delivery: <date> | <milestone> | <time zone>
//	payments   Payment plan (minor units; not receipts):
//	           <stage_id> | <percent>% | <amount minor> | <trigger> | <days> <day basis> days | calendar <id> r<revision>
//	validity   Commercial issue: <issued_at>
//	           Expires exclusively: <expires_at>
//	notes      <customer notes>
//	           <footer note>
//
// Lines come in line_id order (code points), charges and payment stages in the document's order.
// Amounts are the calculation's minor-unit integers, decimals the document's strings and instants
// its UTC strings, all printed as stored: nothing is converted to another time zone, as in Python.
// The bytes are Python's for the same values (golden files in testdata).
//
// Where TRD's model differs from Python's (documented at each Facts member):
//   - Decimals print as the business-hashed document holds them, normalized ("120", not
//     "120.000000"); Python prints the commercial document it is given, which before
//     normalization keeps the author's form.
//   - The customer address is the buyer address, else the billing address, of the party snapshot;
//     Python's quote names exactly one address of either role.
//   - Scope is the trade terms revision: TRD's trade terms carry the Incoterm, the named place and
//     the delivery that Python spread over trade_terms_ref, trade_scope_ref and named_place_ref.
//   - Beyond Python's forms: a payment stage without a day basis counts calendar days (POL), one
//     without a calendar prints no calendar part, a lead-time delivery prints
//     "<days> <day basis> days after <anchor event>" and its calendar, and a delivery without a
//     milestone or time zone leaves that part out.
//   - Only language en: Python's renderer has no zh-CN layout (DEPENDENCY_NOT_SUPPORTED).
//
// Refusals point into the inputs: the stored commercial document by its own JSON Pointers
// ("/calculation", "/data/currency_ref", "/party_snapshots/customer/addresses"), the template at
// "/template/<member>", the number at "/number" and plain-text values at "/values/<field>".
package document
