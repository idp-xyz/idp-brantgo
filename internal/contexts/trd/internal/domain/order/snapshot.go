package order

import (
	"bytes"
	"encoding/json"
	"slices"
	"strconv"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/acceptance"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// SnapshotContract names the order's commercial snapshot (Python
// t03_order_snapshot.CONTRACT).
const SnapshotContract = "brant.order-commercial-snapshot/0.1.0"

// termKeys are the members of the revision's data the snapshot's terms copy (Python TERM_KEYS on
// TRD's commercial document: trade_scope_ref is not one of its members; issued_at is the
// document's).
var termKeys = []string{"trade_terms_ref", "named_place_ref", "payment_terms_ref", "delivery", "expires_at"}

// BuildSnapshot builds the commercial snapshot of order id in scope, converting the revision of src
// whose sealed commercial document is commercial (checked against src.BusinessHash) on acceptance a
// (Python t03_order_snapshot.build, T03 A25: copy the entire frozen input and result; do not
// reprice and do not fill in missing terms). It is canonical JSON of:
//
//	snapshot_contract, scope {tenant, company}, order_id, conversion_slot,
//	customer_ref, contact_ref, currency_ref, terms {trade_terms_ref, named_place_ref,
//	payment_terms_ref, delivery, issued_at, expires_at}, lines, charges, calculation,
//	source_quote_id, source_quote_revision, source_quote_hash, source_quote_number,
//	source_document_sha256,
//	acceptance_id, acceptance {acceptance_id, effectivity_version, arrival_at, arrival_basis,
//	expires_at, customer_ref, contact_ref, line_ids},
//	source_quote_snapshot (the sealed commercial document, whole)
//
// with every copied value exactly as the commercial document holds it. A document that does not
// hash to its business hash is BUSINESS_HASH_MISMATCH; one without a calculation, lines or the terms
// DRAFT_NOT_READY (the freeze seals neither).
func BuildSnapshot(scope kernel.Scope, id string, src Source, commercial []byte, a *acceptance.Acceptance) ([]byte, error) {
	if _, err := quote.ParseCommercial(commercial, src.BusinessHash); err != nil {
		return nil, err
	}
	tree, err := quote.Tree(commercial)
	if err != nil {
		return nil, err
	}
	doc, _ := tree.(map[string]any)
	data, _ := doc["data"].(map[string]any)
	if doc == nil || data == nil || doc["calculation"] == nil || data["lines"] == nil {
		return nil, kernel.Reject(kernel.CodeDraftNotReady, "/args/business_revision", "The revision has no sealed calculation")
	}
	terms := map[string]any{"issued_at": doc["issued_at"]}
	for _, k := range termKeys {
		v, ok := data[k]
		if !ok {
			return nil, kernel.Reject(kernel.CodeDraftNotReady, "/args/business_revision", "The revision has no "+k)
		}
		terms[k] = v
	}
	lines := make([]any, len(a.LineIDs))
	for i, l := range a.LineIDs {
		lines[i] = l
	}
	body := map[string]any{
		"snapshot_contract": SnapshotContract,
		"scope":             map[string]any{"tenant": string(scope.Tenant), "company": string(scope.Company)},
		"order_id":          id,
		"conversion_slot":   quote.SlotFullV1,
		"customer_ref":      data["customer_ref"],
		"contact_ref":       data["contact_ref"],
		"currency_ref":      data["currency_ref"],
		"terms":             terms,
		"lines":             data["lines"],
		"charges":           data["charges"],
		"calculation":       doc["calculation"],
		"source_quote_id":   src.QuoteID, "source_quote_revision": src.Revision, "source_quote_hash": src.BusinessHash,
		"source_quote_number": src.Number, "source_document_sha256": src.DocumentSHA256,
		"acceptance_id": src.AcceptanceID,
		"acceptance": map[string]any{"acceptance_id": src.AcceptanceID, "effectivity_version": src.AcceptanceVersion,
			"arrival_at": kernel.FormatTimestamp(src.ArrivalAt), "arrival_basis": src.ArrivalBasis,
			"expires_at":   kernel.FormatTimestamp(src.ExpiresAt),
			"customer_ref": map[string]any{"party_id": a.Customer.PartyID, "revision": a.Customer.Revision},
			"contact_ref":  map[string]any{"contact_id": a.Contact.ContactID, "row_version": a.Contact.RowVersion},
			"line_ids":     lines},
		"source_quote_snapshot": tree,
	}
	return quote.Canonical(body)
}

// Snapshot is what an order's snapshot says of the order and its source (reads, ActiveOrderRef v1).
type Snapshot struct {
	Contract             string            `json:"snapshot_contract"`
	OrderID              string            `json:"order_id"`
	Slot                 string            `json:"conversion_slot"`
	SourceQuoteID        string            `json:"source_quote_id"`
	SourceQuoteRevision  int64             `json:"source_quote_revision"`
	SourceQuoteHash      string            `json:"source_quote_hash"`
	SourceQuoteNumber    string            `json:"source_quote_number"`
	SourceDocumentSHA256 string            `json:"source_document_sha256"`
	AcceptanceID         string            `json:"acceptance_id"`
	Scope                map[string]string `json:"scope"`
	Commercial           quote.Commercial  `json:"source_quote_snapshot"`
}

// Verify checks the stored order against its snapshot before the order is used (Python
// validate_order_snapshot): the snapshot hashes to the order hash, names the order, its slot and its
// source as the order's columns do, and its source document hashes to the business hash it names
// (REFERENCE_INVALID at path otherwise). It returns the parsed snapshot.
func (o *Order) Verify(scope kernel.Scope, path string) (Snapshot, error) {
	bad := func(why string) (Snapshot, error) {
		return Snapshot{}, kernel.Reject(kernel.CodeReferenceInvalid, path, "The order's snapshot "+why)
	}
	if quote.Hash(o.Snapshot) != o.SnapshotHash || (o.OrderRevision == 1 && o.OrderHash != o.SnapshotHash) {
		return bad("does not hash to its order hash")
	}
	var s Snapshot
	if err := json.Unmarshal(o.Snapshot, &s); err != nil {
		return bad("is not readable")
	}
	src := o.Source
	switch {
	case s.Contract != SnapshotContract || s.OrderID != o.ID || s.Slot != o.Slot:
		return bad("names another order")
	case s.Scope["tenant"] != string(scope.Tenant) || s.Scope["company"] != string(scope.Company):
		return bad("names another company")
	case s.SourceQuoteID != src.QuoteID || s.SourceQuoteRevision != src.Revision || s.SourceQuoteHash != src.BusinessHash ||
		s.SourceQuoteNumber != src.Number || s.SourceDocumentSHA256 != src.DocumentSHA256 || s.AcceptanceID != src.AcceptanceID:
		return bad("names another source")
	case s.Commercial.QuoteID != src.QuoteID || s.Commercial.BusinessRevision != src.Revision:
		return bad("holds another revision")
	}
	tree, err := quote.Tree(o.Snapshot)
	if err != nil {
		return bad("is not readable")
	}
	commercial, err := quote.Canonical(tree.(map[string]any)["source_quote_snapshot"])
	if err != nil || quote.Hash(commercial) != src.BusinessHash {
		return bad("holds a commercial document that is not its source's")
	}
	// Every other member is what the source and the acceptance give (the acceptance restated the
	// revision's customer, contact and complete line set): rebuilt, the snapshot is the same bytes.
	d := s.Commercial.Data
	lines := make([]string, len(d.Lines))
	for i, l := range d.Lines {
		lines[i] = l.LineID
	}
	slices.Sort(lines)
	rebuilt, err := BuildSnapshot(scope, o.ID, src, commercial, &acceptance.Acceptance{Customer: d.Customer, Contact: d.Contact, LineIDs: lines})
	if err != nil || !bytes.Equal(rebuilt, o.Snapshot) {
		return bad("is not the copy of its source")
	}
	return s, nil
}

// Lines are the order lines a snapshot holds, with their amounts (ActiveOrderRef v1).
type Line struct {
	LineID             string
	RfqLineID          string
	Sku                quote.SkuRef
	Quantity           string
	Unit               quote.Ref
	NormalizedQuantity string
	CostUnit           string
	UnitPrice          string
	NetMinor           string
	TaxMinor           string
	PayableMinor       string
}

// Lines returns the order lines of snapshot s: each quoted line with its normalized quantity and
// cost unit (the calculation's cost scope binding) and its amounts.
func (s Snapshot) Lines() []Line {
	c := s.Commercial
	out := make([]Line, 0, len(c.Data.Lines))
	for _, l := range c.Data.Lines {
		line := Line{LineID: l.LineID, RfqLineID: l.RfqLineID, Sku: l.Sku, Quantity: l.Quantity, Unit: l.Unit, UnitPrice: l.UnitPrice}
		if calc := c.Calculation; calc != nil {
			if i := slices.IndexFunc(calc.CostScope.Lines, func(b quote.LineBinding) bool { return b.LineID == l.LineID }); i >= 0 {
				line.NormalizedQuantity, line.CostUnit = calc.CostScope.Lines[i].NormalizedQuantity, calc.CostScope.Lines[i].CostUnit
			}
			if i := slices.IndexFunc(calc.Lines, func(a quote.AmountLine) bool { return a.ID == l.LineID }); i >= 0 {
				line.NetMinor, line.TaxMinor, line.PayableMinor = calc.Lines[i].NetMinor, calc.Lines[i].TaxMinor, calc.Lines[i].PayableMinor
			}
		}
		out = append(out, line)
	}
	return out
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
