package application

import (
	"context"

	tcc "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/tradecompliancecheck"
)

// CNX's ports to the upstream contracts a trade compliance evaluation reads before CNX's
// transaction. They name what CNX needs, in CNX's terms; adapters implement them on the upstream
// api packages (adapters/upstream, module.go): TRD's order snapshots (S5 A2), PTY's address
// countries and reference data's country programmes (S5 A3b) and CAT's declaration profiles (S5
// A5). Tests may implement the ports to steer the upstreams' answers.

// Orders is CNX's port to TRD (Context Map R46 TRD→CNX, "SalesOrderRef v1（订单修订、行、交易方与地址）",
// no guard lock; risk 报关资料引用旧订单修订). ResolveOrders says what TRD says of each order now:
// its current revision at its order hash, its state, its parties' addresses and its lines (trdapi
// OrderSnapshotLookups.ResolveOrderSnapshots, drafts and active orders alike; data scope applied: an
// order the caller does not see is absent). Code is REFERENCE_INVALID when TRD's sealed revision is
// not intact.
type Orders interface {
	ResolveOrders(ctx context.Context, orderIDs []string) (map[string]Order, error)
}

// TRD's order states as CNX distinguishes them: an evaluation and an order_amended reopening need a
// draft or an active order.
const (
	OrderDraft  = "draft"
	OrderActive = "active"
)

// Order is an order at its current revision.
type Order struct {
	Binding    tcc.Binding
	Status     string
	Code       string
	CustomerID string
	Addresses  []OrderAddress
	Lines      []OrderLine
}

// OrderAddress is an address role of the order's parties as the order binds it: the party revision
// and address (PTY AddressRoleRef v1) and its trade role (buyer, payer, consignee, ...).
type OrderAddress struct {
	Role          string
	PartyID       string
	PartyRevision int64
	AddressID     string
}

// OrderLine is an order line and its SKU specification revision.
type OrderLine struct {
	LineID       string
	SkuID        string
	SpecRevision int64
}

// Countries is CNX's port to PTY (R77 PTY→CNX, "PartyRef v1、AddressRoleRef v1（含国家代码）"; risk
// 以自由文本地址推定所在国): the country PTY records for an address of a party revision (ISO
// 3166-1 alpha-2; "" for an address PTY records none for, S5 D-8). An address PTY does not resolve
// for the caller is absent.
type Countries interface {
	ResolveAddressCountries(ctx context.Context, refs []AddressRef) (map[AddressRef]AddressCountry, error)
}

// AddressRef is PTY's AddressRoleRef v1.
type AddressRef struct {
	PartyID       string
	PartyRevision int64
	AddressID     string
}

// AddressCountry is what PTY says of an address: its role and country ("" unknown).
type AddressCountry struct {
	Role    string
	Country string
}

// Profiles is CNX's port to CAT (R39 CAT→CNX, DeclarationProfileRef v1: the verified profile's
// commodity code and export control classification; risk AI 商品编码候选被当作合规结论; no guard
// lock): for each SKU specification revision under a customs code table version, the profile
// revision verified now (catapi DeclarationProfileLookups.ResolveDeclarationProfiles with revision
// 0), or the profile unverified when it has only a draft. A key no profile is kept for is absent.
//
// R39 carries no guard, so nothing holds a profile between the lookup and CNX's commit: when a
// newer revision is verified in between, the evaluation stays on the revision it read — verified
// when read and never changed since, superseded for new use — which it records with its revision
// and content hash. A later evaluation reads the newer one; superseding a profile reopens nothing.
type Profiles interface {
	ResolveProfiles(ctx context.Context, refs []ProfileRef) (map[ProfileRef]tcc.Profile, error)
}

// ProfileRef is the key CAT keeps a declaration profile for (catapi DeclarationProfileKey): a SKU
// specification revision under a customs commodity code table version.
type ProfileRef struct {
	SkuID            string
	SpecRevision     int64
	CodeTableVersion string
}

// Programmes is CNX's port to reference data (country programme lists imported under control,
// ADR-BR-017; refapi ProgrammeQueries.ResolveCountryProgrammes): the current version of each list
// and every programme of those versions naming one of the countries. A list the company never
// imported is absent from Versions and contributes no hit. Inside its transaction CNX re-checks
// each list with reference data's list guard (refapi.CountryProgrammeListGuardSQL).
type Programmes interface {
	ResolveProgrammes(ctx context.Context, lists []string, countries []string) (ProgrammeAnswer, error)
}

// ProgrammeAnswer is what reference data says of the lists and countries asked.
type ProgrammeAnswer struct {
	Versions map[string]tcc.ListVersion
	Hits     []tcc.ProgrammeHit
}
