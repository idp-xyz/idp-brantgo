package api

import (
	"context"
	"slices"
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Service roles, address roles and trading profiles (WBS S1-02).
//
// A service role (UL PTY-ServicePartyRole, T02 spec 13.1) is what a confirmed supplier provides as a
// service party: a role, the service it applies to, an exact origin and destination, a validity and
// the evidence a human verified it on. service_role.verify records a new revision of it in the
// Party aggregate; ServiceRoleRef v1 names (party, service role, revision) for SUP (Context Map
// R05).
//
// Address roles are the addresses of a party with their trade role (UL PTY-AddressRole), part of
// its draft and kept with each confirmed revision; AddressRoleRef v1 names (party, confirmed
// revision, address) for TRD (R13).
//
// A trading profile (UL PTY-TradingProfile) is the relationship of a party with the company:
// settlement terms as exact POL revisions (payment_terms and trade_terms, ConfigRef v1, read from
// POL under R60), the business owner and the contacts at the row versions the editor saw. It is
// saved in working revisions and published by a human other than the revision's last editor;
// TradingProfileRef v1 names (party, revision) for TRD and STL (R13, R43).
const (
	CommandVerifyServiceRole     = "service_role.verify"        // human only
	CommandSaveTradingProfile    = "trading_profile.save_draft" // human or AI
	CommandPublishTradingProfile = "trading_profile.publish"    // human only, independent of the last editor

	EventServiceRoleVerified     = "idp-brant.pty.service_role_verified.v1"
	EventTradingProfilePublished = "idp-brant.pty.trading_profile_published.v1"

	// StatusVerified is the receipt status of service_role.verify.
	StatusVerified = "verified"
	// Statuses of a trading profile revision.
	StatusWorking    = "working"
	StatusActive     = "active"
	StatusSuperseded = "superseded"

	// MaxRefs bounds the references of one lookup.
	MaxRefs = 200
	// MaxContactRefs bounds the contacts a trading profile binds (Python party_profile.contact_refs).
	MaxContactRefs = 50
	// MaxConfigIDLength is POL's longest config_id.
	MaxConfigIDLength = 40

	// KindPaymentTerms and KindTradeTerms are the POL configuration kinds of the settlement terms.
	KindPaymentTerms = "payment_terms"
	KindTradeTerms   = "trade_terms"
)

// Service roles a party verifies (T02 service_role.role).
const (
	ServiceForwarder  = "forwarder"
	ServiceCarrier    = "carrier"
	ServiceBroker     = "broker"
	ServiceInspection = "inspection"
	ServiceInsurer    = "insurer"
)

var serviceKinds = []string{ServiceForwarder, ServiceCarrier, ServiceBroker, ServiceInspection, ServiceInsurer}

// ServiceKindKnown reports whether role is one of the service roles.
func ServiceKindKnown(role string) bool { return slices.Contains(serviceKinds, role) }

// Location reports whether s is an exact location code: a UN/LOCODE of a two-letter country and a
// three-character place ("CNSHA"). Zones and routes are not locations (T02 spec 13.1: exact origin
// and destination only).
func Location(s string) bool {
	if len(s) != 5 {
		return false
	}
	for i := 0; i < 5; i++ {
		c := s[i]
		upper := c >= 'A' && c <= 'Z'
		if !upper && (i < 2 || c < '2' || c > '9') {
			return false
		}
	}
	return true
}

// VerifyServiceRole records a new verified revision of the service role ServiceRoleID of a confirmed
// supplier: the service ServiceCode of kind Role between Origin and Destination (UN/LOCODE),
// valid in [ValidFrom, ValidUntil) (UTC timestamps), on evidence the verifying human may read.
// ExpectedVersion is the party's row version, which the verification increments. Only a human
// verifies.
type VerifyServiceRole struct {
	IdempotencyKey  string
	PartyID         string
	ExpectedVersion int64
	ServiceRoleID   string
	Role            string
	ServiceCode     string
	Origin          string
	Destination     string
	ValidFrom       string
	ValidUntil      string
	EvidenceIDs     []string
}

// Validate checks the closed contract.
func (c VerifyServiceRole) Validate() error {
	if err := head(c.IdempotencyKey, c.PartyID, c.ExpectedVersion, 1); err != nil {
		return err
	}
	switch {
	case !kernel.ValidIdentifier(c.ServiceRoleID):
		return schema("/args/service_role_id")
	case !ServiceKindKnown(c.Role):
		return schema("/args/role")
	case !kernel.ValidIdentifier(c.ServiceCode):
		return schema("/args/service_code")
	case !Location(c.Origin):
		return schema("/args/origin")
	case !Location(c.Destination):
		return schema("/args/destination")
	}
	from, err := kernel.ParseTimestamp(c.ValidFrom)
	if err != nil {
		return schema("/args/valid_from")
	}
	until, err := kernel.ParseTimestamp(c.ValidUntil)
	if err != nil {
		return schema("/args/valid_until")
	}
	if !kernel.Instant(until).After(kernel.Instant(from)) {
		return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/valid_until", "")
	}
	return evidence(c.EvidenceIDs)
}

// TermsRef is ConfigRef v1 of POL (kind, id, revision) as PTY's contract carries it.
type TermsRef struct {
	Kind     string
	ConfigID string
	Revision int64
}

// OwnerRef is the business owner of a trading relationship: an actor of the company and,
// optionally, the actor's team.
type OwnerRef struct {
	ActorID string
	TeamID  string
}

// PinnedContact is a contact at the row version a trading profile binds (Python
// party_profile.contact_refs: id and row_version).
type PinnedContact struct {
	ContactID  string
	RowVersion int64
}

// SaveTradingProfile creates the trading profile of the party PartyID at ExpectedVersion 0, or
// replaces its working revision whole at the current root version; without a working revision it
// starts one with the next revision number. The content binds the party's confirmed revision
// PartyRevision, the settlement terms (exact POL revisions of payment_terms and trade_terms in force
// now), the business owner and 0 to MaxContactRefs contacts of the party, each confirmed at its row
// version.
type SaveTradingProfile struct {
	IdempotencyKey  string
	PartyID         string
	ExpectedVersion int64
	PartyRevision   int64
	PaymentTerms    TermsRef
	TradeTerms      TermsRef
	Owner           OwnerRef
	ContactRefs     []PinnedContact
}

// Validate checks the closed contract.
func (c SaveTradingProfile) Validate() error {
	if err := head(c.IdempotencyKey, c.PartyID, c.ExpectedVersion, 0); err != nil {
		return err
	}
	if c.PartyRevision < 1 || c.PartyRevision > 2147483647 {
		return schema("/args/party_revision")
	}
	if err := c.PaymentTerms.validate(KindPaymentTerms, "/args/payment_terms"); err != nil {
		return err
	}
	if err := c.TradeTerms.validate(KindTradeTerms, "/args/trade_terms"); err != nil {
		return err
	}
	switch {
	case !kernel.ValidIdentifier(c.Owner.ActorID):
		return schema("/args/owner/actor_id")
	case c.Owner.TeamID != "" && !kernel.ValidIdentifier(c.Owner.TeamID):
		return schema("/args/owner/team_id")
	case len(c.ContactRefs) > MaxContactRefs:
		return schema("/args/contact_refs")
	}
	for i, ref := range c.ContactRefs {
		path := "/args/contact_refs/" + strconv.Itoa(i)
		switch {
		case !kernel.ValidIdentifier(ref.ContactID):
			return schema(path + "/id")
		case ref.RowVersion < 1 || ref.RowVersion > 2147483647:
			return schema(path + "/row_version")
		case slices.IndexFunc(c.ContactRefs, func(o PinnedContact) bool { return o.ContactID == ref.ContactID }) != i:
			return schema("/args/contact_refs")
		}
	}
	return nil
}

func (r TermsRef) validate(kind, path string) error {
	switch {
	case r.Kind != kind:
		return schema(path + "/kind")
	case len(r.ConfigID) > MaxConfigIDLength || !kernel.ValidIdentifier(r.ConfigID):
		return schema(path + "/id")
	case r.Revision < 1 || r.Revision > 2147483647:
		return schema(path + "/revision")
	}
	return nil
}

// PublishTradingProfile makes the working revision Revision the active one. Only a human other
// than the revision's last editor publishes, citing the evidence verified and a reason; the
// references must still be current (the party's confirmed revision, the contacts' row versions)
// and the settlement terms in force. The previously active revision is superseded in the same
// transaction.
type PublishTradingProfile struct {
	IdempotencyKey  string
	PartyID         string
	ExpectedVersion int64
	Revision        int64
	EvidenceIDs     []string
	Reason          string
}

// Validate checks the closed contract.
func (c PublishTradingProfile) Validate() error {
	if err := head(c.IdempotencyKey, c.PartyID, c.ExpectedVersion, 1); err != nil {
		return err
	}
	if c.Revision < 1 || c.Revision > 2147483647 {
		return schema("/args/revision")
	}
	if err := evidence(c.EvidenceIDs); err != nil {
		return err
	}
	if !Text(c.Reason, 1000) {
		return schema("/args/reason")
	}
	return nil
}

// ProfileCommands is PTY's command entry point for service roles and trading profiles. A
// service_role.verify receipt names the party (ObjectID), its row version and StatusVerified; a
// trading profile receipt names the party, the profile's root version and the status of the
// revision the command changed (working or active).
type ProfileCommands interface {
	VerifyServiceRole(ctx context.Context, cmd VerifyServiceRole) (Receipt, error)
	SaveTradingProfile(ctx context.Context, cmd SaveTradingProfile) (Receipt, error)
	PublishTradingProfile(ctx context.Context, cmd PublishTradingProfile) (Receipt, error)
}

// ServiceRoleView is one verified revision of a service role.
type ServiceRoleView struct {
	ServiceRoleID  string
	Revision       int64
	PartyRevision  int64 // the party's confirmed revision it was verified against
	Role           string
	ServiceCode    string
	Origin         string
	Destination    string
	ValidFrom      time.Time
	ValidUntil     time.Time
	EvidenceIDs    []string
	VerifiedBy     kernel.ActorRef
	VerifiedAt     time.Time
	AuthorityEpoch int64
}

// TradingProfileRevisionView is one revision of a trading profile. Published revisions never
// change.
type TradingProfileRevisionView struct {
	Revision      int64
	Status        string
	ContentHash   string // SHA-256 of the revision's canonical content
	PartyRevision int64
	PaymentTerms  TermsRef
	TradeTerms    TermsRef
	Owner         OwnerRef
	ContactRefs   []PinnedContact
	SavedBy       kernel.ActorRef // the last editor
	SavedAt       time.Time
	PublishedBy   kernel.ActorRef // zero before publication
	PublishedAt   time.Time
	EvidenceIDs   []string  // the evidence the publisher verified
	EndedAt       time.Time // when it was superseded
}

// TradingProfileView is a party's trading profile in the caller's company and all its revisions,
// oldest first.
type TradingProfileView struct {
	PartyID         string
	RootVersion     int64
	WorkingRevision int64 // 0: none
	ActiveRevision  int64 // 0: none
	Revisions       []TradingProfileRevisionView
}

// AddressRoleRef is AddressRoleRef v1: an address of a party as a confirmed revision of the party
// keeps it. The confirmed revision is the content identity (it never changes); the party's row
// version only counts commands.
type AddressRoleRef struct {
	PartyID       string
	PartyRevision int64
	AddressID     string
}

// ServiceRoleRef is ServiceRoleRef v1: one verified revision of a service role of a party.
type ServiceRoleRef struct {
	PartyID       string
	ServiceRoleID string
	Revision      int64
}

// TradingProfileRef is TradingProfileRef v1: one published revision of the trading profile of a
// party in the caller's company. Revision 0 asks a lookup for the active revision.
type TradingProfileRef struct {
	PartyID  string
	Revision int64
}

// Why a resolved reference does not serve new business now; empty when it does. Every such
// reference resolves with CodeReferenceStale: refresh it and resubmit.
const (
	StaleSuperseded       = "superseded"        // a later revision of the service role or profile exists
	StalePartyRevised     = "party_revised"     // the party is confirmed in a later revision
	StalePartyChanged     = "party_changed"     // the party is not confirmed in the revision the role or profile binds
	StalePartyUnavailable = "party_unavailable" // the party is not confirmed now (revised into a draft or deactivated)
	StaleContactChanged   = "contact_changed"   // a bound contact is not confirmed at the bound row version
	StaleOutsideValidity  = "outside_validity"  // the service role's validity does not contain the instant
)

// ResolvedAddressRole is what PTY says about one AddressRoleRef now. Code is empty when the party
// is confirmed in PartyRevision now; otherwise it is REFERENCE_STALE with Stale saying why.
type ResolvedAddressRole struct {
	Ref             AddressRoleRef
	Role            string
	Address         string
	PartyStatus     string
	CurrentRevision int64 // the party's latest confirmed revision
	Code            string
	Stale           string
}

// ResolvedServiceRole is what PTY says about one ServiceRoleRef at an instant. Code is empty when
// the revision is the latest of its service role, the party is confirmed in the revision it was
// verified against, and its validity contains the instant; otherwise it is REFERENCE_STALE.
type ResolvedServiceRole struct {
	Ref            ServiceRoleRef
	Role           string
	ServiceCode    string
	Origin         string
	Destination    string
	ValidFrom      time.Time
	ValidUntil     time.Time
	PartyRevision  int64
	LatestRevision int64
	Code           string
	Stale          string
}

// ResolvedTradingProfile is what PTY says about one published trading profile revision now. Code is
// empty when it is the active revision, its party is confirmed in PartyRevision and every bound
// contact is confirmed at its bound row version; otherwise it is REFERENCE_STALE. The settlement
// terms are POL revisions: a downstream context checks them with POL (ResolveConfigs before its
// transaction, ConfigGuard inside it), under its own relationship with POL.
type ResolvedTradingProfile struct {
	Ref            TradingProfileRef // with the exact revision
	Status         string
	ActiveRevision int64
	ContentHash    string
	PartyRevision  int64
	PaymentTerms   TermsRef
	TradeTerms     TermsRef
	Owner          OwnerRef
	ContactRefs    []PinnedContact
	Code           string
	Stale          string
}

// Check returns nil when the address serves new business and REFERENCE_STALE at path otherwise.
func (r ResolvedAddressRole) Check(path string) error { return check(r.Code, r.Stale, path) }

// Check returns nil when the service role serves new business and REFERENCE_STALE at path otherwise.
func (r ResolvedServiceRole) Check(path string) error { return check(r.Code, r.Stale, path) }

// Check returns nil when the profile serves new business and REFERENCE_STALE at path otherwise.
func (r ResolvedTradingProfile) Check(path string) error { return check(r.Code, r.Stale, path) }

func check(code, stale, path string) error {
	if code == "" {
		return nil
	}
	return kernel.RejectAs(code, codes[code], path, "Reference is "+stale)
}

// ValidateLookup checks the size of a lookup.
func ValidateLookup(n int) error {
	if n > MaxRefs {
		return schema("/refs")
	}
	return nil
}

// ProfileQueries is PTY's read side for trading profiles, address roles and service roles.
type ProfileQueries interface {
	// GetTradingProfile returns the party's trading profile in the caller's company, audited as
	// trading_profile.get (pty.read). It is NOT_FOUND when the caller does not see the party.
	GetTradingProfile(ctx context.Context, partyID string) (TradingProfileView, error)
	// ResolveAddressRoles answers AddressRoleRef v1 lookups. Like ResolveParties it needs no
	// permission beyond an active grant; references to a party the caller does not see, or to a
	// revision or address that does not exist, are absent. Ask before opening your own transaction.
	ResolveAddressRoles(ctx context.Context, refs []AddressRoleRef) (map[AddressRoleRef]ResolvedAddressRole, error)
	// ResolveServiceRoles answers ServiceRoleRef v1 lookups at instant at, as ResolveAddressRoles.
	ResolveServiceRoles(ctx context.Context, refs []ServiceRoleRef, at time.Time) (map[ServiceRoleRef]ResolvedServiceRole, error)
	// ResolveTradingProfiles answers TradingProfileRef v1 lookups of published revisions, keyed by
	// the reference asked (Revision 0: the active revision, absent without one), as
	// ResolveAddressRoles. A working revision is not a TradingProfileRef and is absent.
	ResolveTradingProfiles(ctx context.Context, refs []TradingProfileRef) (map[TradingProfileRef]ResolvedTradingProfile, error)
}
