// Package party is the Party aggregate: a trading party with the roles it plays for the company,
// its addresses with their trade roles (AddressRole) and the service-provider roles verified for it
// (ServicePartyRole). One party may be customer and supplier at once (Python kept one master.kind).
package party

import (
	"cmp"
	"slices"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/pty/internal/domain/lifecycle"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Role a party plays.
type Role string

const (
	Customer Role = "customer"
	Supplier Role = "supplier"
)

// AddressRole is what an address of the party is for in trade (Python party_profile.addresses[].role;
// T03 A31). It is declared with the address, never derived from trade terms.
type AddressRole string

const (
	AddressBuyer     AddressRole = "buyer"
	AddressSeller    AddressRole = "seller"
	AddressPayer     AddressRole = "payer"
	AddressPayee     AddressRole = "payee"
	AddressConsignee AddressRole = "consignee"
	AddressShipper   AddressRole = "shipper"
	AddressBilling   AddressRole = "billing"
	// AddressNotify is the notify party's address (S5R-D40).
	AddressNotify AddressRole = "notify"
)

// Address is an address of the party with the one trade role it serves (PTY-AGG-Party entity
// AddressRole). Its ID is unique within the party and stays the same across revisions. Country is
// its AddressCountry value: an ISO 3166-1 alpha-2 code, or empty when unknown (R77, S5 D-8).
//
// party.save_draft hashes its addresses as this structure's JSON: Country is omitted when empty, so
// an address without a country hashes exactly as it did before countries existed and a stored
// request replays unchanged.
type Address struct {
	ID      string
	Role    AddressRole
	Text    string
	Country string `json:",omitempty"`
}

// Party is the aggregate root. Addresses are part of its draft content, sorted by ID; a
// confirmation keeps them with the confirmed revision.
type Party struct {
	lifecycle.Record
	Name      string
	Roles     []Role
	Addresses []Address
}

// ReplaceDraft replaces the whole draft; an empty name is unknown and no addresses are none.
func (p *Party) ReplaceDraft(name string, roles []Role, addresses []Address) {
	p.Name, p.Roles, p.Addresses = name, slices.Clone(roles), SortAddresses(addresses)
	slices.Sort(p.Roles)
	p.Move(lifecycle.Draft)
}

// SortAddresses returns a copy of addresses ordered by ID.
func SortAddresses(addresses []Address) []Address {
	out := slices.Clone(addresses)
	slices.SortFunc(out, func(a, b Address) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// Has reports whether the party plays role.
func (p *Party) Has(role Role) bool { return slices.Contains(p.Roles, role) }

// ReadyToConfirm reports the first part a confirmation still lacks (Python _confirm_master).
func (p *Party) ReadyToConfirm() error {
	if p.Name == "" {
		return kernel.Reject(kernel.CodeDraftNotReady, "/args/name", "")
	}
	return nil
}

// ServiceKind is the service a party provides as a service party (T02 spec 13.1; S5R-D36).
type ServiceKind string

const (
	ServiceForwarder        ServiceKind = "forwarder"
	ServiceCarrier          ServiceKind = "carrier"
	ServiceInsurer          ServiceKind = "insurer"
	ServiceInspectionAgency ServiceKind = "inspection_agency"
	ServiceCustomsBroker    ServiceKind = "customs_broker"
	// The S1 names of the customs broker and the inspection agency, which serve a route (S5R-D36
	// keeps them until QLT and CNX resolve the new ones).
	ServiceBroker     ServiceKind = "broker"
	ServiceInspection ServiceKind = "inspection"
)

// NeedsRoute reports whether a role of kind k serves a route, an exact origin and destination: a
// forwarder or a carrier, and the S1 kinds (S5R-D36). An insurer, an inspection agency and a customs
// broker are verified without one.
func (k ServiceKind) NeedsRoute() bool {
	return k == ServiceForwarder || k == ServiceCarrier || k == ServiceBroker || k == ServiceInspection
}

// ServiceRole is one verified revision of a service-provider role of the party (PTY-AGG-Party
// entity PartyRole, UL PTY-ServicePartyRole): the service it applies to, between an exact origin
// and an exact destination (UN/LOCODE) when its kind serves a route (both empty otherwise, unless
// the verifier states them), within a validity, verified by a human on evidence against the party's
// confirmed revision. Revisions of one role ID count from 1; the latest is current. It does not
// change what customer or supplier mean.
type ServiceRole struct {
	ID            string
	Revision      int64
	PartyRevision int64
	Kind          ServiceKind
	ServiceCode   string
	Origin        string
	Destination   string
	Validity      kernel.ValidityInterval
	EvidenceIDs   []string
	VerifiedBy    kernel.ActorRef
	Epoch         int64
	At            time.Time
}

// VerifyServiceRole checks that the party, a confirmed revision the caller already admitted, may
// hold role r now and returns r as the next revision after latest (0: the role ID is new) of the
// confirmed revision. A service role belongs to a supplier (T02 binds it to a supplier
// PartyRef), and a validity that has already ended at instant at verifies nothing.
func (p *Party) VerifyServiceRole(r ServiceRole, latest int64, at time.Time) (ServiceRole, error) {
	switch {
	case p.Status != lifecycle.Confirmed:
		return ServiceRole{}, kernel.Reject(kernel.CodeInvalidState, "/args/id", "")
	case !p.Has(Supplier):
		return ServiceRole{}, kernel.Reject(kernel.CodeInvalidState, "/args/id", "A service role belongs to a supplier")
	case !r.Validity.Until.After(r.Validity.From):
		return ServiceRole{}, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/valid_until", "")
	case !r.Validity.Until.After(at):
		return ServiceRole{}, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/valid_until", "The validity has already ended")
	case r.Kind.NeedsRoute() && (r.Origin == "" || r.Destination == ""):
		return ServiceRole{}, kernel.Reject(kernel.CodeSchemaInvalid, "/args/origin", "A "+string(r.Kind)+" serves a route")
	}
	r.Revision, r.PartyRevision, r.EvidenceIDs = latest+1, p.Revision, slices.Clone(r.EvidenceIDs)
	return r, nil
}
