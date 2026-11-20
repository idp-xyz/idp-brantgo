package api

import (
	"context"
	"slices"
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Export licences and their usage ledger (WBS S5-14; tactical model CNX-AGG-ExportLicense with its
// LicenseAllocation entries, CNX-INV-002, MD-44, UL CNX-ExportLicense and CNX-LicenseAllocation,
// UL-INV-31, ES-12 step 3, TD-AT13; S5 decisions D-3, E-4: every licence in S5 is synthetic,
// apparel needs none in reality). The licence is the external authority's; CNX registers it and
// allocates its quantity and value to FUL shipments or export lots. The licence root is locked FOR
// UPDATE by every allocation and release, and its usage is derived from immutable allocation and
// release entries: Σ outstanding allocated quantity and value never exceed the licensed ones.
//
//	export_license.register  human only (documentation): license_number and issuer (unique in the
//	                         company), holder, goods — the SKU specifications (sku_specs) or the
//	                         classification (code_table_version, commodity_codes) it covers —
//	                         licensed_quantity in unit, licensed_value in currency (a POL currency
//	                         revision {id, revision}), destination country, end_user {party_id, name},
//	                         valid_from, valid_until and file_id, the EVD file of the licence. S5R-D04:
//	                         usage_mode single_use (一批一证, the legal mode of dual-use export licences
//	                         and the default) or multi_use with usage_evidence_ids, the EVD sources of
//	                         the licence face that says so; customs_office, the customs office it is
//	                         used at (一证一关, required), which a declaration it covers must name.
//	export_license.allocate  human or AI (documentation): allocation_id of license_id to target
//	                         {kind shipment|export_lot, id}: every line of
//	                         a prepared shipment or a registered export lot (FUL, through CNX's port),
//	                         its destination and end user the licence's (the end user the shipment
//	                         states, else the consignee, else the customer, S5R-D40), inside the
//	                         licence's validity (EXPORT_LICENSE_EXPIRED otherwise), its goods and units
//	                         the licence's (EXPORT_LICENSE_GOODS_MISMATCH), and Σ allocated quantity and
//	                         value with it never above the licensed ones (EXPORT_LICENSE_EXCEEDED). A
//	                         target holds one allocation at a time. S5R-D04: one licence per
//	                         declaration — a shipment needing two licences is split into export lots,
//	                         each allocated its own; a shipment never holds its own allocation beside
//	                         its lots' (INVALID_STATE); a single_use licence holds one live allocation
//	                         and none once declared (INVALID_STATE); a declaration of the target that
//	                         names another customs office refuses it (INVALID_KNOWN_VALUE).
//	export_license.release   human only (documentation): release_id releases allocation_id of
//	                         license_id at expected_version on an external result only — an amended (quantity and
//	                         value released) or withdrawn (all that is outstanding) declaration
//	                         receipt, stated with its declaration_number, receipt_text, received_at
//	                         and evidence_ids (EVD sources of the receipt) — basis external_result,
//	                         the default — or, S5R-D03, basis local_cancellation: everything
//	                         outstanding, with a reason, of an allocation whose target FUL cancelled
//	                         while no declaration case covering it is submitted or later (INVALID_STATE
//	                         otherwise: declared usage is never restored by a local cancellation,
//	                         MD-44).
//
// Request members in snake_case: export_license.register {license_number, issuer, holder, goods
// {sku_specs [{sku_id, spec_revision}], classification {code_table_version, commodity_codes}},
// licensed_quantity, unit, currency {id, revision}, licensed_value, destination, end_user {party_id,
// name}, valid_from, valid_until, file_id, usage_mode, usage_evidence_ids, customs_office};
// export_license.allocate {license_id, allocation_id, target {kind, id}}; export_license.release
// {license_id, expected_version, allocation_id, release_id, basis, external_result {kind, receipt_id,
// declaration_number, receipt_text, received_at, evidence_ids}, quantity, value, reason}.

// Commands, reads and events of export licences (role template documentation holds the three
// commands; humanOnlyCommands name register and release).
const (
	CommandRegisterLicense = "export_license.register"
	CommandAllocateLicense = "export_license.allocate"
	CommandReleaseLicense  = "export_license.release"

	// Reads: a licence with its usage ledger, the company's licences, a shipment's allocation.
	CommandGetLicense       = "export_license.get"
	CommandLicenses         = "export_license.list"
	CommandShipmentLicense  = "export_license.shipment"
	EventLicenseRegistered  = "idp-brant.cnx.export_license_registered.v1"
	EventLicenseAllocated   = "idp-brant.cnx.export_license_allocated.v1"
	LicenseStatusRegistered = "registered"
)

// Targets of an allocation (UL CNX-LicenseAllocation: "具体出运或出口批次").
const (
	TargetShipment  = "shipment"
	TargetExportLot = "export_lot"
)

// Kinds of the external result a release rests on (SM-CNX-DECLARATION amended and withdrawn), and
// the kind a release on a local cancellation records (S5R-D03).
const (
	ExternalAmended   = "amended"
	ExternalWithdrawn = "withdrawn"
	ReleaseCancelled  = "cancelled"
)

// Bases of a release (S5R-D03): an external result (the default when omitted), or the local
// cancellation of a target never declared.
const (
	BasisExternalResult    = "external_result"
	BasisLocalCancellation = "local_cancellation"
)

// Usage modes of a licence (S5R-D04): 一批一证 (one live allocation, none once declared) or not.
const (
	UsageSingleUse = "single_use"
	UsageMultiUse  = "multi_use"
)

// Where an allocation's end user comes from (S5R-D40): stated on the shipment, or derived from its
// consignee, then its customer.
const (
	EndUserStated    = "stated"
	EndUserConsignee = "consignee"
	EndUserCustomer  = "customer"
)

// Sources of an external result: a human states the receipt with the EVD sources it rests on, for
// a target no declaration in CNX covers; or the declaration receipt CNX recorded (S5 C3a), named by
// receipt_id, which is required once a declaration in CNX covers the target.
const (
	ResultStated             = "stated"
	ResultDeclarationReceipt = "declaration_receipt"
	// ResultLocalCancellation is the source of a release on a local cancellation (S5R-D03).
	ResultLocalCancellation = "local_cancellation"
)

// A licence's state at an instant, as the reads derive it from its validity.
const (
	LicenseValid       = "valid"
	LicenseExpired     = "expired"
	LicenseNotYetValid = "not_yet_valid"
)

// An allocation's state, derived from its release entries.
const (
	AllocationAllocated = "allocated"
	AllocationReleased  = "released"
)

// Limits of the licence contract.
const (
	MaxLicenseNumber  = 64
	MaxLicenseName    = 200
	MaxLicenseGoods   = 100
	MaxCommodityCode  = 32
	MaxReceiptText    = 4000
	MaxDeclarationRef = 64
	MaxCustomsOffice  = 32 // S5R-D04: a customs office code (关区代码), an identifier
)

// CustomsOffice reports whether s is a customs office code: an identifier of at most
// MaxCustomsOffice characters (S5R-D04).
func CustomsOffice(s string) bool { return len(s) <= MaxCustomsOffice && kernel.ValidIdentifier(s) }

// valueFormat bounds a decimal amount before the currency's minor units are known.
var valueFormat = kernel.DecimalFormat{IntDigits: kernel.MaxMinorDigits - 4, Scale: 4}

func positive(s string, f kernel.DecimalFormat, zero bool) bool {
	d, err := kernel.ParseDecimal(s, f)
	return err == nil && (d.Sign() > 0 || (zero && d.Sign() == 0))
}

func timestamp(s string) bool {
	_, err := kernel.ParseTimestamp(s)
	return err == nil
}

// SpecRef is a SKU specification revision a licence covers (CAT).
type SpecRef struct {
	SkuID        string
	SpecRevision int64
}

// Classification is the commodity codes under a customs code table version a licence covers; a
// line is covered when its verified declaration profile (CAT, R39) has one of them.
type Classification struct {
	CodeTableVersion string
	CommodityCodes   []string
}

// LicensedGoods is what a licence covers: SKU specifications or a classification, exactly one.
type LicensedGoods struct {
	SkuSpecs       []SpecRef
	Classification *Classification
}

// EndUser is the end user a licence names: the PTY party and the name on the licence.
type EndUser struct {
	PartyID string
	Name    string
}

// CurrencyRef is a POL currency revision.
type CurrencyRef struct {
	ID       string
	Revision int64
}

// RegisterLicense registers an export licence the issuing authority granted (human only).
type RegisterLicense struct {
	IdempotencyKey   string
	LicenseNumber    string
	Issuer           string
	Holder           string
	Goods            LicensedGoods
	LicensedQuantity string
	Unit             string
	Currency         CurrencyRef
	LicensedValue    string
	Destination      string
	EndUser          EndUser
	ValidFrom        string
	ValidUntil       string
	FileID           string
	// S5R-D04: single_use ("" is single_use) or multi_use with the EVD sources of the licence face
	// (1 to MaxEvidence), and the customs office (required).
	UsageMode        string   `json:",omitempty"`
	UsageEvidenceIDs []string `json:",omitempty"`
	CustomsOffice    string   `json:",omitempty"`
}

// Validate checks the closed contract.
func (c RegisterLicense) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !Text(c.LicenseNumber, MaxLicenseNumber):
		return schema("/args/license_number")
	case !Text(c.Issuer, MaxLicenseName):
		return schema("/args/issuer")
	case !Text(c.Holder, MaxLicenseName):
		return schema("/args/holder")
	}
	if err := c.Goods.validate("/args/goods"); err != nil {
		return err
	}
	switch {
	case !positive(c.LicensedQuantity, kernel.QuantityFormat, false):
		return schema("/args/licensed_quantity")
	case !kernel.UnitCode(c.Unit).Valid():
		return schema("/args/unit")
	case !kernel.ValidIdentifier(c.Currency.ID):
		return schema("/args/currency/id")
	case c.Currency.Revision < 1 || c.Currency.Revision > maxVersion:
		return schema("/args/currency/revision")
	case !positive(c.LicensedValue, valueFormat, false):
		return schema("/args/licensed_value")
	case !Country(c.Destination):
		return schema("/args/destination")
	case !kernel.ValidIdentifier(c.EndUser.PartyID):
		return schema("/args/end_user/party_id")
	case !Text(c.EndUser.Name, MaxLicenseName):
		return schema("/args/end_user/name")
	case !timestamp(c.ValidFrom):
		return schema("/args/valid_from")
	case !timestamp(c.ValidUntil):
		return schema("/args/valid_until")
	case !kernel.ValidIdentifier(c.FileID):
		return schema("/args/file_id")
	case c.UsageMode != "" && c.UsageMode != UsageSingleUse && c.UsageMode != UsageMultiUse:
		return schema("/args/usage_mode")
	case !CustomsOffice(c.CustomsOffice):
		return schema("/args/customs_office")
	}
	if err := evidence(c.UsageEvidenceIDs, "/args/usage_evidence_ids", 0); err != nil {
		return err
	}
	if (c.UsageMode == UsageMultiUse) != (len(c.UsageEvidenceIDs) > 0) {
		return schema("/args/usage_evidence_ids")
	}
	from, _ := kernel.ParseTimestamp(c.ValidFrom)
	until, _ := kernel.ParseTimestamp(c.ValidUntil)
	if !until.After(from) {
		return schema("/args/valid_until")
	}
	return nil
}

func (g LicensedGoods) validate(path string) error {
	if (len(g.SkuSpecs) == 0) == (g.Classification == nil) {
		return schema(path)
	}
	if len(g.SkuSpecs) > MaxLicenseGoods {
		return schema(path + "/sku_specs")
	}
	for i, s := range g.SkuSpecs {
		p := path + "/sku_specs/" + strconv.Itoa(i)
		switch {
		case !kernel.ValidIdentifier(s.SkuID):
			return schema(p + "/sku_id")
		case s.SpecRevision < 1 || s.SpecRevision > maxVersion || slices.Index(g.SkuSpecs, s) != i:
			return schema(p + "/spec_revision")
		}
	}
	if c := g.Classification; c != nil {
		if !kernel.ValidIdentifier(c.CodeTableVersion) {
			return schema(path + "/classification/code_table_version")
		}
		if len(c.CommodityCodes) < 1 || len(c.CommodityCodes) > MaxLicenseGoods {
			return schema(path + "/classification/commodity_codes")
		}
		for i, code := range c.CommodityCodes {
			if len(code) > MaxCommodityCode || !kernel.ValidIdentifier(code) || slices.Index(c.CommodityCodes, code) != i {
				return schema(path + "/classification/commodity_codes/" + strconv.Itoa(i))
			}
		}
	}
	return nil
}

// ExportTarget is what an allocation covers: a FUL shipment or export lot.
type ExportTarget struct {
	Kind string
	ID   string
}

// AllocateLicense allocates licence LicenseID, as AllocationID, to Target. It names no expected
// version: the licence root's FOR UPDATE lock serializes allocations, and the usage derived under
// it is the condition ("许可证根 FOR UPDATE；用量条件更新"), so allocations of other targets do not
// conflict unless together they exceed the licence.
type AllocateLicense struct {
	IdempotencyKey string
	LicenseID      string
	AllocationID   string
	Target         ExportTarget
}

// Validate checks the closed contract.
func (c AllocateLicense) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.LicenseID):
		return schema("/args/license_id")
	case !kernel.ValidIdentifier(c.AllocationID):
		return schema("/args/allocation_id")
	case c.Target.Kind != TargetShipment && c.Target.Kind != TargetExportLot:
		return schema("/args/target/kind")
	case !kernel.ValidIdentifier(c.Target.ID):
		return schema("/args/target/id")
	}
	return nil
}

// ExternalResult is the external result a release rests on: the declaration receipt (amended or
// withdrawn) with the declaration's number, the receipt's original text, when it was received and
// the EVD sources it rests on (1 to MaxEvidence).
type ExternalResult struct {
	Kind              string
	ReceiptID         string
	DeclarationNumber string
	ReceiptText       string
	ReceivedAt        string
	EvidenceIDs       []string
}

// ReleaseLicense releases, as ReleaseID, allocation AllocationID of licence LicenseID at
// ExpectedVersion on ExternalResult (human only): amended releases Quantity (in the licence's unit)
// and Value (in its currency, may be 0); withdrawn releases all that is outstanding and names
// neither. Basis local_cancellation (S5R-D03) names no external result, quantity or value: it
// releases all that is outstanding of an allocation whose target FUL cancelled before any
// declaration of it was submitted.
type ReleaseLicense struct {
	IdempotencyKey  string
	LicenseID       string
	ExpectedVersion int64
	AllocationID    string
	ReleaseID       string
	ExternalResult  *ExternalResult
	Quantity        string
	Value           string
	Reason          string
	Basis           string `json:",omitempty"` // "" is external_result (S5R-D03)
}

// LocalCancellation reports whether the release rests on a local cancellation (S5R-D03).
func (c ReleaseLicense) LocalCancellation() bool { return c.Basis == BasisLocalCancellation }

// Validate checks the closed contract: a release without an external result is not a release,
// unless it rests on a local cancellation, which names nothing but its reason.
func (c ReleaseLicense) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.LicenseID):
		return schema("/args/license_id")
	case c.ExpectedVersion < 1 || c.ExpectedVersion > maxVersion:
		return schema("/args/expected_version")
	case !kernel.ValidIdentifier(c.AllocationID):
		return schema("/args/allocation_id")
	case !kernel.ValidIdentifier(c.ReleaseID):
		return schema("/args/release_id")
	case c.Basis != "" && c.Basis != BasisExternalResult && c.Basis != BasisLocalCancellation:
		return schema("/args/basis")
	}
	if c.LocalCancellation() {
		switch {
		case c.ExternalResult != nil:
			return schema("/args/external_result")
		case c.Quantity != "":
			return schema("/args/quantity")
		case c.Value != "":
			return schema("/args/value")
		case !Text(c.Reason, MaxReason):
			return schema("/args/reason")
		}
		return nil
	}
	if c.ExternalResult == nil {
		return schema("/args/external_result")
	}
	r := c.ExternalResult
	switch {
	case r.Kind != ExternalAmended && r.Kind != ExternalWithdrawn:
		return schema("/args/external_result/kind")
	case r.ReceiptID != "" && !kernel.ValidIdentifier(r.ReceiptID):
		return schema("/args/external_result/receipt_id")
	case r.ReceiptID != "" && r.DeclarationNumber != "":
		return schema("/args/external_result/declaration_number")
	case r.ReceiptID != "" && r.ReceiptText != "":
		return schema("/args/external_result/receipt_text")
	case r.ReceiptID != "" && r.ReceivedAt != "":
		return schema("/args/external_result/received_at")
	case r.ReceiptID != "" && len(r.EvidenceIDs) > 0:
		return schema("/args/external_result/evidence_ids")
	}
	if r.ReceiptID == "" {
		if err := r.stated(); err != nil {
			return err
		}
	}
	amended := r.Kind == ExternalAmended
	switch {
	case amended && !positive(c.Quantity, kernel.QuantityFormat, false), !amended && c.Quantity != "":
		return schema("/args/quantity")
	case amended && !positive(c.Value, valueFormat, true), !amended && c.Value != "":
		return schema("/args/value")
	case !Text(c.Reason, MaxReason):
		return schema("/args/reason")
	}
	return nil
}

// stated checks a result the human states: the declaration number, the receipt's original text,
// when it was received and at least one EVD source.
func (r *ExternalResult) stated() error {
	switch {
	case !Text(r.DeclarationNumber, MaxDeclarationRef):
		return schema("/args/external_result/declaration_number")
	case !Text(r.ReceiptText, MaxReceiptText):
		return schema("/args/external_result/receipt_text")
	case !timestamp(r.ReceivedAt):
		return schema("/args/external_result/received_at")
	}
	return evidence(r.EvidenceIDs, "/args/external_result/evidence_ids", 1)
}

func (RegisterLicense) cnxCommand() {}
func (AllocateLicense) cnxCommand() {}
func (ReleaseLicense) cnxCommand()  {}

// Usage is a licence's usage ledger as derived from its entries: licensed, outstanding allocated
// and available quantity (decimal strings in Unit) and value (minor units of Currency).
type Usage struct {
	Unit                string
	LicensedQuantity    string
	AllocatedQuantity   string
	AvailableQuantity   string
	Currency            string
	LicensedValueMinor  string
	AllocatedValueMinor string
	AvailableValueMinor string
}

// LicenseReceipt is the answer of every licence command: the receipt (ObjectID the licence, its row
// version and status) with the allocation and release the command wrote ("" when none) and the
// usage after it, rebuilt from the stored licence.
type LicenseReceipt struct {
	Receipt
	LicenseID    string
	AllocationID string
	ReleaseID    string
	Usage        Usage
}

// LicenseCommands is the licence half of CNX's command entry point.
type LicenseCommands interface {
	RegisterLicense(ctx context.Context, cmd RegisterLicense) (LicenseReceipt, error)
	AllocateLicense(ctx context.Context, cmd AllocateLicense) (LicenseReceipt, error)
	ReleaseLicense(ctx context.Context, cmd ReleaseLicense) (LicenseReceipt, error)
}

// LicenseCurrencyView is the currency of a licence's value: the POL revision and what it said.
type LicenseCurrencyView struct {
	ID         string
	Revision   int64
	Code       string
	MinorUnits int
}

// LicenseFileView is the EVD file of a licence.
type LicenseFileView struct {
	FileID        string
	ContentSHA256 string
}

// AllocationLineView is a target line an allocation covers; ShipmentLineID is an export lot line's
// shipment line (S5R-D04).
type AllocationLineView struct {
	LineID         string
	SkuID          string
	SpecRevision   int64
	Quantity       string
	ShipmentLineID string `json:",omitempty"`
}

// ReleaseView is a release entry and the external result it rests on, or (Basis
// local_cancellation, Kind cancelled, S5R-D03) the cancelled target's version.
type ReleaseView struct {
	ReleaseID         string
	Basis             string
	TargetVersion     int64
	Kind              string
	Source            string
	ReceiptID         string
	DeclarationNumber string
	ReceiptText       string
	ReceivedAt        time.Time
	Evidence          []EvidenceView
	Quantity          string
	ValueMinor        string
	Reason            string
	ReleasedBy        kernel.ActorRef
	ReleasedAt        time.Time
	AuthorityEpoch    int64
}

// AllocationView is an allocation entry: the target it covers (with the version, order revision,
// destination and end user FUL gave, the shipment it ships and where the end user came from,
// S5R-D04, D40 — "" for an allocation made before), its lines, quantity and value, what remains
// outstanding after its releases and its state.
type AllocationView struct {
	AllocationID          string
	TargetKind            string
	TargetID              string
	ShipmentID            string
	EndUserSource         string
	TargetVersion         int64
	Attempt               int
	OrderID               string
	OrderRevision         int64
	Destination           string
	EndUserPartyID        string
	Lines                 []AllocationLineView
	LinesHash             string
	Quantity              string
	ValueMinor            string
	OutstandingQuantity   string
	OutstandingValueMinor string
	Status                string
	AllocatedBy           kernel.ActorRef
	AllocatedAt           time.Time
	AuthorityEpoch        int64
	Releases              []ReleaseView
}

// LicenseView is a licence with its usage ledger, its state as of CheckedAt and every allocation
// with its releases, oldest first.
type LicenseView struct {
	LicenseID        string
	LicenseNumber    string
	Issuer           string
	Holder           string
	Goods            LicensedGoods
	Unit             string
	LicensedQuantity string
	Currency         LicenseCurrencyView
	LicensedValue    string
	Destination      string
	EndUser          EndUser
	ValidFrom        time.Time
	ValidUntil       time.Time
	File             LicenseFileView
	UsageMode        string
	UsageEvidence    []EvidenceView
	CustomsOffice    string // "" for a licence registered before S5R-D04
	Status           string
	State            string
	RowVersion       int64
	Usage            Usage
	Allocations      []AllocationView
	RegisteredBy     kernel.ActorRef
	RegisteredAt     time.Time
	AuthorityEpoch   int64
	CheckedAt        time.Time
}

// LicenseSummary is one licence of the company as of CheckedAt.
type LicenseSummary struct {
	LicenseID     string
	LicenseNumber string
	Issuer        string
	Destination   string
	EndUserID     string
	ValidFrom     time.Time
	ValidUntil    time.Time
	UsageMode     string
	CustomsOffice string
	State         string
	RowVersion    int64
	Usage         Usage
	CheckedAt     time.Time
}

// ShipmentLicenseView is what ExportLicenseGuard v1 decides now for a shipment, read without locks.
type ShipmentLicenseView struct {
	ExportLicenseVerdict
	CheckedAt time.Time
}

// LicenseQueries is the licence half of CNX's audited read side (cnx.read).
type LicenseQueries interface {
	// GetLicense returns a licence of the company, audited as export_license.get; NOT_FOUND otherwise.
	GetLicense(ctx context.Context, licenseID string) (LicenseView, error)
	// Licenses returns the company's licences, newest first, audited as export_license.list.
	Licenses(ctx context.Context) ([]LicenseSummary, error)
	// ShipmentLicense returns ExportLicenseGuard v1's verdict for a shipment now without locks,
	// audited as export_license.shipment.
	ShipmentLicense(ctx context.Context, shipmentID string) (ShipmentLicenseView, error)
}
