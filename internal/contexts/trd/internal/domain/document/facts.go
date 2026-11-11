package document

import "go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"

// Facts are what a formal document prints that the commercial document names by reference only.
// The command resolves each from the exact revision the frozen document binds, before its
// transaction; Prepare refuses a fact for another reference (REFERENCE_INVALID) and a POL revision
// whose content hash is not the one the dependency manifest pins (REFERENCE_STALE). Unit and
// currency codes need no fact: a POL unit's and currency's config_id is its code.
type Facts struct {
	// Customer is the customer's name at the bound party revision (PTY PartyRevision.Name; Python
	// party_snapshots.customer.name).
	Customer Party
	// Contact is the contact's name at the bound row version (PTY ContactView.Name; Python
	// party_snapshots.contact_name).
	Contact Contact
	// Items name each line's SKU specification revision (CAT; Python sku_profile item_name and
	// specification_version).
	Items map[quote.SkuRef]Item
	// Currency is the quote currency's revision (POL currency).
	Currency Currency
	// TradeTerms is the trade terms revision (POL trade_terms); it also plays Python's trade scope
	// and named place location.
	TradeTerms TradeTerms
	// Delivery is the delivery the revision binds: the trade terms' delivery (POL
	// TradeTerms.Delivery) with the milestone and time zone of the customer's delivery requirement
	// where one is known (Python data.delivery; calc.Delivery). POL trade terms carry neither.
	Delivery Delivery
}

// Party is a party at a confirmed revision and its name.
type Party struct {
	Ref  quote.PartyRef
	Name string
}

// Contact is a contact at a row version and its name.
type Contact struct {
	Ref  quote.ContactRef
	Name string
}

// Item is how a document names a SKU specification revision: the item's name and the
// specification's label.
type Item struct {
	Name          string
	Specification string
}

// Currency is a POL currency revision with its content hash and minor units (0–4).
type Currency struct {
	Ref         quote.Ref
	ContentHash string
	MinorUnits  int
}

// TradeTerms is a POL trade_terms revision with its content hash and what the document prints of
// it.
type TradeTerms struct {
	Ref                quote.Ref
	ContentHash        string
	Incoterm           string
	Edition            string
	ResponsibilityNote string
	NamedPlace         string
}

// Delivery kinds (POL Delivery).
const (
	DeliveryFixedDate = "fixed_date"
	DeliveryLeadTime  = "lead_time"
)

// Delivery is a fixed date (Kind fixed_date with Date), or a lead time (Kind lead_time) of Days
// after AnchorEvent counted on DayBasis, in Calendar when one is named; Milestone and TimeZone are
// optional.
type Delivery struct {
	Kind        string
	Date        string // fixed_date: YYYY-MM-DD
	AnchorEvent string // lead_time
	Days        int    // lead_time: 0–3650
	DayBasis    string // lead_time
	Calendar    *quote.Ref
	Milestone   string
	TimeZone    string
}
