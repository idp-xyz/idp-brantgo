package draft

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// DocumentContract names the draft body. Python A46's is brant.purchase-draft/0.1.0; this one adds
// the allocated quantity, the overbuy and the cost split of each line and drops trade scopes, which
// POL does not have.
const DocumentContract = "brant.purchase-draft/0.2.0"

// CommitmentNotCommitted is the commitment state every draft is prepared and stored in: a draft is
// never an order to the supplier (UL-INV-20). Whether a purchase commitment issued on it since made it
// committed is the commitment's fact (domain/commitment), derived when read.
const CommitmentNotCommitted = "not_committed"

// Document is the immutable body of a purchase draft: what was bound, decided and calculated, at
// checked_at. Its canonical JSON is stored as it was hashed; draft_hash is its SHA-256 (Python
// t04_purchase _calculate).
type Document struct {
	Contract         string       `json:"contract"`
	Scope            DocScope     `json:"scope"`
	DraftID          string       `json:"draft_id"`
	CommitmentState  string       `json:"commitment_state"`
	Order            DocOrder     `json:"order"`
	Supplier         DocParty     `json:"supplier"`
	CurrencyRef      DocRef       `json:"currency_ref"`
	PaymentTermsRef  DocRef       `json:"payment_terms_ref"`
	TradeTermsRef    DocRef       `json:"trade_terms_ref"`
	Currency         string       `json:"currency"`
	MinorUnits       int          `json:"minor_units"`
	Rounding         string       `json:"rounding"`
	Sources          []DocSource  `json:"sources"`
	PriceGroups      []DocGroup   `json:"price_groups"`
	Lines            []DocLine    `json:"lines"`
	Totals           DocTotals    `json:"totals"`
	CostingSnapshot  DocCosting   `json:"costing_snapshot"`
	SupplyValidUntil string       `json:"supply_valid_until"`
	SelectionReason  string       `json:"selection_reason"`
	PreparedBy       DocActor     `json:"prepared_by"`
	AuthorityEpoch   int64        `json:"authority_epoch"`
	CheckedAt        string       `json:"checked_at"`
	Revision         *DocRevision `json:"revision,omitempty"`
}

// RevisionContract names the body of a purchase commitment revision a commitment change proposes
// (S5-19): a draft's document prepared again with the change's quantities, sources and supplier
// dates (Change), contract RevisionContract instead of DocumentContract, and Revision set. A draft's
// document has no Revision.
const RevisionContract = "brant.purchase-commitment-revision/0.1.0"

// DocRevision is what a revision's document binds: the commitment, the change that proposes it, the
// commitment revision the change is bound to and the revision it makes.
type DocRevision struct {
	CommitmentID string `json:"commitment_id"`
	ChangeID     string `json:"change_id"`
	BaseRevision int64  `json:"base_revision"`
	Revision     int64  `json:"revision"`
}

// DocScope is the tenant and company the draft belongs to.
type DocScope struct {
	Tenant  string `json:"tenant"`
	Company string `json:"company"`
}

// DocOrder is the order the draft was prepared from, as bound.
type DocOrder struct {
	OrderID          string      `json:"order_id"`
	OrderRevision    int64       `json:"order_revision"`
	OrderHash        string      `json:"order_hash"`
	QuoteID          string      `json:"quote_id"`
	QuoteRevision    int64       `json:"quote_revision"`
	QuoteHash        string      `json:"quote_hash"`
	OrderReviewID    string      `json:"order_review_id"`
	CustomerID       string      `json:"customer_id"`
	Rfq              DocRfq      `json:"rfq"`
	RequiredDelivery DocRequired `json:"required_delivery"`
}

// DocRfq is the order's RFQ.
type DocRfq struct {
	RfqID      string `json:"rfq_id"`
	RowVersion int64  `json:"row_version"`
}

// DocRequired is the order's delivery requirement.
type DocRequired struct {
	Kind      string `json:"kind"`
	Date      string `json:"date"`
	Milestone string `json:"milestone"`
	Place     string `json:"place"`
	TimeZone  string `json:"time_zone"`
}

// DocParty is a party at a row version.
type DocParty struct {
	PartyID    string `json:"party_id"`
	RowVersion int64  `json:"row_version"`
}

// DocRef is a POL configuration revision.
type DocRef struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
}

func docRef(k ConfigKey) DocRef { return DocRef(k) }

// DocActor is an actor.
type DocActor struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// DocSourceRef is a supply condition revision.
type DocSourceRef struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
}

// DocLineSource is a condition line of a supply condition revision.
type DocLineSource struct {
	Kind            string `json:"kind"`
	ID              string `json:"id"`
	Revision        int64  `json:"revision"`
	ConditionLineID string `json:"condition_line_id"`
}

func docSource(s Source) DocLineSource {
	return DocLineSource{Kind: s.Kind, ID: s.ID, Revision: s.Revision, ConditionLineID: s.ConditionLineID}
}

// DocSource is a supply condition revision the draft adopted, with its content hash, verification
// and validity; Origin is the source offer of purchase terms.
type DocSource struct {
	Kind        string     `json:"kind"`
	ID          string     `json:"id"`
	Revision    int64      `json:"revision"`
	ContentHash string     `json:"content_hash"`
	VerifiedBy  DocActor   `json:"verified_by"`
	VerifiedAt  string     `json:"verified_at"`
	ValidFrom   string     `json:"valid_from"`
	ValidUntil  string     `json:"valid_until"`
	Origin      *DocOrigin `json:"origin"`
}

// DocOrigin is the source offer of purchase terms.
type DocOrigin struct {
	Kind         string `json:"kind"`
	ID           string `json:"id"`
	Revision     int64  `json:"revision"`
	ValidityMode string `json:"validity_mode"`
	ValidFrom    string `json:"valid_from"`
	ValidUntil   string `json:"valid_until"`
}

// DocTier is a tier a price group applied, with the quantity it priced.
type DocTier struct {
	LowerInclusive string `json:"lower_inclusive"`
	UpperExclusive string `json:"upper_exclusive,omitempty"`
	UnitPrice      string `json:"unit_price"`
	Quantity       string `json:"quantity"`
}

// DocGroup is one condition line priced once over the draft lines it serves (T02 §13.3).
type DocGroup struct {
	GroupID            string        `json:"group_id"`
	Source             DocLineSource `json:"source"`
	OrderLineIDs       []string      `json:"order_line_ids"`
	Quantity           string        `json:"quantity"`
	UnitRef            DocRef        `json:"unit_ref"`
	PricingMode        string        `json:"pricing_mode"`
	PriceBasisQuantity string        `json:"price_basis_quantity"`
	AppliedTiers       []DocTier     `json:"applied_tiers"`
	TaxProfileRef      DocRef        `json:"tax_profile_ref"`
	TaxPriceBasis      string        `json:"tax_price_basis"`
	TaxRate            string        `json:"tax_rate"`
	TaxRecovery        string        `json:"tax_recovery_treatment"`
	RawAmount          string        `json:"raw_amount"`
	NetMinor           string        `json:"net_minor"`
	TaxMinor           string        `json:"tax_minor"`
	GrossMinor         string        `json:"gross_minor"`
}

// DocSku is a SKU specification revision.
type DocSku struct {
	SkuID        string `json:"sku_id"`
	SpecRevision int64  `json:"spec_revision"`
}

// DocOverbuy is a line's overbuy, marked with its quantity and reason. It is approved by the purchase
// commitment decision (S5 decision D-14, PUR-INV-005), not in the draft: a draft prepared in S3,
// when the preparer approved it with purchase.overbuy.approve, also names that preparer and epoch
// (ApprovedBy, AuthorityEpoch); a later draft omits both.
type DocOverbuy struct {
	Quantity       string    `json:"quantity"`
	Reason         string    `json:"reason"`
	ApprovedBy     *DocActor `json:"approved_by,omitempty"`
	AuthorityEpoch int64     `json:"authority_epoch,omitempty"`
}

// DocMOQ is the condition line's MOQ as applied.
type DocMOQ struct {
	Quantity      string   `json:"quantity"`
	UnitRef       DocRef   `json:"unit_ref"`
	Scope         string   `json:"scope"`
	GroupID       string   `json:"group_id,omitempty"`
	MemberLineIDs []string `json:"member_line_ids,omitempty"`
	Purchased     string   `json:"purchased"`
}

// DocDelivery is the comparison of the supplier's date with the order's (PUR-INV-006). In a
// commitment revision, ConditionDate is the supply condition's date when the change states another
// supplier date, and SlackDays is negative for a date after the order's.
type DocDelivery struct {
	Milestone     string `json:"milestone"`
	Place         string `json:"place"`
	TimeZone      string `json:"time_zone"`
	SupplierDate  string `json:"supplier_date"`
	RequiredDate  string `json:"required_date"`
	SlackDays     int    `json:"slack_days"`
	ConditionDate string `json:"condition_date,omitempty"`
}

// DocCostingRef compares a line with the cost snapshot (R21: for review only, never blocking): the
// condition lines the snapshot bought the RFQ line under, its purchase cost of the whole line and
// of the allocated share, whether the draft buys under the same single source, and the variance of
// the draft's cost of the allocated quantity against the share (null across currencies: no
// exchange rate is inferred).
type DocCostingRef struct {
	Sources           []DocLineSource `json:"sources"`
	Currency          string          `json:"currency"`
	LineCostMinor     *string         `json:"line_purchase_cost_minor"`
	ShareCostMinor    *string         `json:"purchase_cost_minor"`
	SameSource        bool            `json:"same_source"`
	CostVarianceMinor *string         `json:"cost_variance_minor"`
}

// DocLine is one order line as the draft buys it.
type DocLine struct {
	OrderLineID       string        `json:"order_line_id"`
	RfqLineID         string        `json:"rfq_line_id"`
	Sku               DocSku        `json:"sku"`
	UnitRef           DocRef        `json:"unit_ref"`
	OrderQuantity     string        `json:"order_quantity"`
	AllocatedQuantity string        `json:"allocated_quantity"`
	OverbuyQuantity   string        `json:"overbuy_quantity"`
	PurchaseQuantity  string        `json:"purchase_quantity"`
	Overbuy           *DocOverbuy   `json:"overbuy"`
	Source            DocLineSource `json:"source"`
	SupplierItemCode  string        `json:"supplier_item_code,omitempty"`
	PriceGroupID      string        `json:"price_group_id"`
	NetMinor          string        `json:"net_minor"`
	TaxMinor          string        `json:"tax_minor"`
	GrossMinor        string        `json:"gross_minor"`
	CostMinor         string        `json:"cost_minor"`
	SoldCostMinor     string        `json:"sold_cost_minor"`
	OverbuyCostMinor  string        `json:"overbuy_cost_minor"`
	TaxProfileRef     DocRef        `json:"tax_profile_ref"`
	TaxApplicationRef DocRef        `json:"tax_application_ref"`
	MOQ               *DocMOQ       `json:"moq"`
	OrderMultiple     string        `json:"order_multiple,omitempty"`
	Delivery          DocDelivery   `json:"delivery"`
	CostingReference  DocCostingRef `json:"costing_reference"`
	Waivers           []string      `json:"waivers,omitempty"` // a commitment revision's: WaiverMOQNotMet, WaiverOrderMultipleInvalid
}

// DocTotals are the draft's totals in minor units of its currency.
type DocTotals struct {
	NetMinor   string `json:"net_minor"`
	TaxMinor   string `json:"tax_minor"`
	GrossMinor string `json:"gross_minor"`
	CostMinor  string `json:"cost_minor"`
}

// DocCosting is the cost snapshot compared with and its adoptability when the draft was prepared,
// as CST's AdoptabilityGuard v1 decided it inside the draft's transaction (recorded, never
// blocking: R21).
type DocCosting struct {
	SnapshotID   string `json:"snapshot_id"`
	ResultHash   string `json:"result_hash"`
	Currency     string `json:"currency"`
	Adoptability string `json:"adoptability"`
	Code         string `json:"code,omitempty"`
	Input        string `json:"input,omitempty"`
}

// canonical returns the canonical JSON of v: object members in byte order of their names at every
// level, no insignificant whitespace, no HTML escaping. Documents hold only strings, integers,
// booleans, nulls, arrays and objects, so equal content always encodes to the same bytes.
func canonical(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("draft: document does not encode: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree any
	if err = dec.Decode(&tree); err != nil {
		return nil, fmt.Errorf("draft: document does not decode: %w", err)
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err = enc.Encode(tree); err != nil {
		return nil, fmt.Errorf("draft: document does not re-encode: %w", err)
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// Canonical returns the canonical JSON of v, as documents are hashed.
func Canonical(v any) ([]byte, error) { return canonical(v) }

// Hash returns the SHA-256 of b in lowercase hex.
func Hash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
