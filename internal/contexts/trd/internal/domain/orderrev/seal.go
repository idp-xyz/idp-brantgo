package orderrev

import (
	"cmp"
	"slices"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// What a new order revision seals beyond its references (S5R-D35, S5R-D40; simulated sign-off E0;
// tactical model TRD-AGG-SalesOrder, UL TRD-SalesOrder; Context Map R27, R46).
//
// A revision binds the POL trade_terms revision and the customer's addresses at a confirmed PTY
// party revision; the Incoterm, its edition and the addresses' countries are what those revisions
// state. Since S5R a new revision — revision 1 written by order.prepare, a later one by
// order.amendment.apply — seals them too, in one member "seal" both document forms carry:
//
//	seal {incoterm, incoterm_edition,
//	      addresses [{address_id, country?}]   every customer address the source binds, address ID
//	                                           order; no country when PTY's revision states none
//	      notify_party? {address, country?}}   the notify party terms.notify_party_ref names
//
// and the optional notify party (S5R-D40) is a term: terms.notify_party_ref, a PTY AddressRoleRef
// v1 whose address has the role notify, of any party the company knows (an amendment may bind,
// change or drop it). Both members are omitted when empty, so a revision written before S5R has
// neither and keeps its bytes and order hash; OrderRevisionRef v1 derives what it would have sealed
// from the bound POL and PTY revisions (which never change) and says so (derived).

// RoleNotify is the PTY address role of a notify party (ptyapi.AddressNotify).
const RoleNotify = "notify"

// Seal is what a revision sealed of its trade terms and parties (see above).
type Seal struct {
	Incoterm  string          `json:"incoterm"`
	Edition   string          `json:"incoterm_edition"`
	Addresses []SealedCountry `json:"addresses"`
	Notify    *SealedAddress  `json:"notify_party,omitempty"`
}

// SealedCountry is the country of one customer address the revision binds: the ISO 3166-1 alpha-2
// code PTY's confirmed revision gives it, empty when it states none.
type SealedCountry struct {
	AddressID string `json:"address_id"`
	Country   string `json:"country,omitempty"`
}

// SealedAddress is the notify party's address as PTY's confirmed revision gives it: the text and
// the country (empty when it states none).
type SealedAddress struct {
	Address string `json:"address"`
	Country string `json:"country,omitempty"`
}

// Country returns the country sealed for customer address id, and whether the seal names the
// address.
func (s *Seal) Country(id string) (string, bool) {
	if s == nil {
		return "", false
	}
	for _, a := range s.Addresses {
		if a.AddressID == id {
			return a.Country, true
		}
	}
	return "", false
}

// SealFacts are what POL and PTY say, before the transaction, of what a new revision seals: the
// trade_terms revision named (TradeTerms) with its Incoterm and edition, the country of every
// customer address the order binds (address ID → country, "" when PTY states none), and the notify
// party the terms name, as PTY resolved it (nil when they name none).
type SealFacts struct {
	TradeTerms quote.Ref
	Incoterm   string
	Edition    string
	Countries  map[string]string
	Notify     *NotifyFact
}

// NotifyFact is an AddressRoleRef v1 as PTY resolved it: the reference, the address's role, text
// and country.
type NotifyFact struct {
	Ref     quote.AddressRef
	Role    string
	Address string
	Country string
}

// NewSeal returns the seal of a revision with terms t whose customer addresses are addresses, from
// the facts f. A fact it needs and does not find is REFERENCE_INVALID — the Incoterm of t's trade
// terms at tradePath, an address's country at /facts/seal/addresses — never a neutral value; a
// notify party whose address is not of the role notify is INVALID_KNOWN_VALUE at notifyPath.
func NewSeal(t Terms, addresses []quote.AddressSnapshot, f SealFacts, tradePath, notifyPath string) (*Seal, error) {
	if f.TradeTerms != t.TradeTerms || f.Incoterm == "" {
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, tradePath, "The Incoterm of the trade terms is needed")
	}
	s := &Seal{Incoterm: f.Incoterm, Edition: f.Edition, Addresses: []SealedCountry{}}
	for _, a := range addresses {
		country, ok := f.Countries[a.AddressID]
		if !ok {
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/facts/seal/addresses", "The country of address "+a.AddressID+" is needed")
		}
		s.Addresses = append(s.Addresses, SealedCountry{AddressID: a.AddressID, Country: country})
	}
	slices.SortFunc(s.Addresses, func(a, b SealedCountry) int { return cmp.Compare(a.AddressID, b.AddressID) })
	if n := t.NotifyParty; n != nil {
		switch x := f.Notify; {
		case x == nil || x.Ref != *n:
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, notifyPath, "The notify party's address is needed")
		case x.Role != RoleNotify:
			return nil, kernel.Reject(kernel.CodeInvalidKnownValue, notifyPath, "The address's role is "+x.Role+", not notify")
		default:
			s.Notify = &SealedAddress{Address: x.Address, Country: x.Country}
		}
	}
	return s, nil
}

// checkSeal checks the seal of a stored revision r against what it binds: none, and then no notify
// party, for a revision written before S5R; otherwise an Incoterm, the countries of exactly the
// customer addresses its parties hold in address ID order, and a notify party exactly when its
// terms name one. It returns why not, or "".
func checkSeal(r *Revision) string {
	s := r.Seal
	if s == nil {
		if r.Terms.NotifyParty != nil {
			return "names a notify party it does not seal"
		}
		return ""
	}
	ids := make([]string, 0, len(r.Parties.Customer.Addresses))
	for _, a := range r.Parties.Customer.Addresses {
		ids = append(ids, a.AddressID)
	}
	slices.Sort(ids)
	got := make([]string, 0, len(s.Addresses))
	for _, a := range s.Addresses {
		got = append(got, a.AddressID)
	}
	switch {
	case s.Incoterm == "":
		return "seals no Incoterm"
	case !slices.Equal(ids, got):
		return "seals the countries of other addresses than its customer's"
	case (s.Notify == nil) != (r.Terms.NotifyParty == nil):
		return "seals a notify party its terms do not name, or names one it does not seal"
	}
	return ""
}

// validAddressRef reports whether a is an AddressRoleRef v1 in form.
func validAddressRef(a quote.AddressRef) bool {
	return kernel.ValidIdentifier(a.PartyID) && a.PartyRevision > 0 && kernel.ValidIdentifier(a.AddressID)
}
