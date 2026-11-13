// Package exportlicense holds CNX's ExportLicense (tactical model CNX-AGG-ExportLicense with its
// LicenseAllocation entries, CNX-INV-002; MD-44; UL CNX-ExportLicense, CNX-LicenseAllocation,
// UL-INV-31; WBS S5-14): an export licence the issuing authority granted — number, issuer, holder,
// goods, licensed quantity and value, destination, end user and validity — and the usage ledger of
// its allocations to FUL shipments or export lots.
//
// The licence is the root. Allocations and releases are immutable entries of it; what is allocated
// is derived from them (S5 D-3): an allocation's outstanding quantity and value are its own less
// its releases', and Σ outstanding never exceeds the licensed quantity or value. Every allocation
// and release moves the root's row version by one under its FOR UPDATE lock, so two allocations
// that exceed the licence together are serialized and the second is refused. A release rests on an
// external result only (an amended or withdrawn declaration receipt): there is no release on a
// local cancellation (MD-44: "凭本地取消恢复已申报用量" is forbidden).
package exportlicense

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Codes the domain decides with besides the kernel's, and their categories (error catalog V0.1).
const (
	CodeExportLicenseExceeded = "EXPORT_LICENSE_EXCEEDED"
	CodeExportLicenseExpired  = "EXPORT_LICENSE_EXPIRED"
)

// Categories are the categories of the domain's own codes.
var Categories = map[string]kernel.Category{
	CodeExportLicenseExceeded: kernel.CategoryConflict,
	CodeExportLicenseExpired:  kernel.CategoryConflict,
}

func reject(code, path, reason string) *kernel.Rejection {
	return kernel.RejectAs(code, Categories[code], path, reason)
}

// Targets, external result kinds and sources, licence and allocation states.
const (
	TargetShipment  = "shipment"
	TargetExportLot = "export_lot"

	Amended   = "amended"
	Withdrawn = "withdrawn"
	Stated    = "stated"
	// DeclarationReceipt: the result is a declaration receipt CNX recorded (S5 C3a).
	DeclarationReceipt = "declaration_receipt"

	Registered = "registered"

	Valid       = "valid"
	Expired     = "expired"
	NotYetValid = "not_yet_valid"

	Allocated = "allocated"
	Released  = "released"
)

// Spec is a SKU specification revision.
type Spec struct {
	SkuID        string `json:"sku_id"`
	SpecRevision int64  `json:"spec_revision"`
}

// Goods is what a licence covers: Specs, or the commodity codes CommodityCodes under code table
// CodeTable.
type Goods struct {
	Specs          []Spec   `json:"sku_specs,omitempty"`
	CodeTable      string   `json:"code_table_version,omitempty"`
	CommodityCodes []string `json:"commodity_codes,omitempty"`
}

// ByClassification reports whether the goods are a classification rather than specifications.
func (g Goods) ByClassification() bool { return len(g.Specs) == 0 }

// Currency is the POL currency revision of a licence's value and what it said.
type Currency struct {
	ConfigID   string
	Revision   int64
	Code       string
	MinorUnits int
}

// Kernel returns the kernel currency.
func (c Currency) Kernel() (kernel.Currency, error) { return kernel.NewCurrency(c.Code, c.MinorUnits) }

// License is the ExportLicense root.
type License struct {
	ID             string
	Number         string
	Issuer         string
	Holder         string
	Goods          Goods
	Unit           kernel.UnitCode
	Quantity       kernel.Decimal
	Currency       Currency
	Value          kernel.Money
	Destination    string
	EndUserPartyID string
	EndUserName    string
	Validity       kernel.ValidityInterval
	FileID         string
	FileSHA256     string
	RowVersion     int64
	RegisteredBy   kernel.ActorRef
	Epoch          int64
	RegisteredAt   time.Time
}

// State is the licence's state at at, from its validity.
func (l *License) State(at time.Time) string {
	switch {
	case at.Before(l.Validity.From):
		return NotYetValid
	case l.Validity.Contains(at):
		return Valid
	}
	return Expired
}

// CheckVersion compares the caller's expected row version with l's.
func CheckVersion(l *License, expected int64) error {
	if l.RowVersion != expected {
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	return nil
}

// Line is a line of an allocation's target: its SKU specification revision, quantity and unit, and,
// for a licence by classification, the commodity code of the line's verified declaration profile
// ("" when none is verified).
type Line struct {
	LineID        string
	SkuID         string
	SpecRevision  int64
	Quantity      kernel.Decimal
	Unit          kernel.UnitCode
	CommodityCode string
}

// LinesHash is the hash an allocation records of the lines it covers (the published LinesHash of
// ExportLicenseGuard v1: line-ID order, "line_id|sku_id|spec_revision|quantity|unit" per line with
// six fractional digits, joined by "\n", SHA-256 hex).
func LinesHash(lines []Line) string {
	sorted := slices.Clone(lines)
	slices.SortFunc(sorted, func(a, b Line) int { return strings.Compare(a.LineID, b.LineID) })
	parts := make([]string, 0, len(sorted))
	for _, l := range sorted {
		parts = append(parts, l.LineID+"|"+l.SkuID+"|"+strconv.FormatInt(l.SpecRevision, 10)+"|"+l.Quantity.StringFixed(6)+"|"+string(l.Unit))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])
}

// Target is what FUL says of a shipment or export lot an allocation covers: its version, order
// revision, destination, end user, lines and value.
type Target struct {
	Kind           string
	ID             string
	Version        int64
	OrderID        string
	OrderRevision  int64
	Destination    string
	EndUserPartyID string
	Lines          []Line
	Value          kernel.Money
}

// Allocation is a LicenseAllocation entry: the target it covers and its quantity (Σ of its lines)
// and value, made at attempt Attempt of its target (1 + the target's allocations released before).
// Released and ReleasedValue are derived from its releases.
type Allocation struct {
	ID            string
	LicenseID     string
	Target        Target
	Attempt       int
	Quantity      kernel.Decimal
	Value         kernel.Money
	LinesHash     string
	By            kernel.ActorRef
	Epoch         int64
	At            time.Time
	Released      kernel.Decimal
	ReleasedValue kernel.Money
}

// Outstanding returns what the allocation still holds of the licence.
func (a *Allocation) Outstanding() (kernel.Decimal, kernel.Money, error) {
	v, err := a.Value.Sub(a.ReleasedValue)
	return a.Quantity.Sub(a.Released), v, err
}

// Status is allocated while quantity is outstanding, released after.
func (a *Allocation) Status() string {
	if q, _, _ := a.Outstanding(); q.Sign() > 0 {
		return Allocated
	}
	return Released
}

// Usage is a licence's usage derived from its allocations.
type Usage struct {
	Allocated      kernel.Decimal
	AllocatedValue kernel.Money
	Available      kernel.Decimal
	AvailableValue kernel.Money
}

// UsageOf derives l's usage from its allocations (with their releases).
func UsageOf(l *License, allocations []*Allocation) (Usage, error) {
	u := Usage{Allocated: kernel.DecimalFromInt(0)}
	var err error
	if u.AllocatedValue, err = kernel.NewMoney(l.Value.Currency(), 0); err != nil {
		return Usage{}, err
	}
	for _, a := range allocations {
		q, v, err := a.Outstanding()
		if err != nil {
			return Usage{}, err
		}
		u.Allocated = u.Allocated.Add(q)
		if u.AllocatedValue, err = u.AllocatedValue.Add(v); err != nil {
			return Usage{}, err
		}
	}
	u.Available = l.Quantity.Sub(u.Allocated)
	u.AvailableValue, err = l.Value.Sub(u.AllocatedValue)
	return u, err
}

// AllocateInput is an allocation as the command asks it.
type AllocateInput struct {
	ID      string
	Target  Target
	Attempt int
	By      kernel.ActorRef
	Epoch   int64
	At      time.Time
}

// Allocate allocates l to in.Target (CNX-INV-002): l is valid at in.At, its destination and end
// user are the target's and every line is goods it covers (EXPORT_LICENSE_EXPIRED otherwise), the
// lines are in its unit and the value in its currency (INVALID_KNOWN_VALUE), and the outstanding
// allocations with this one hold no more than the licensed quantity, then value
// (EXPORT_LICENSE_EXCEEDED). The caller has locked l FOR UPDATE and read all its allocations.
func Allocate(l *License, allocations []*Allocation, in AllocateInput) (*Allocation, error) {
	t := in.Target
	switch state := l.State(in.At); {
	case state == NotYetValid:
		return nil, reject(CodeExportLicenseExpired, "/args/license_id", "The licence is not valid yet")
	case state == Expired:
		return nil, reject(CodeExportLicenseExpired, "/args/license_id", "The licence has expired")
	case t.Destination != l.Destination:
		return nil, reject(CodeExportLicenseExpired, "/args/target/id", "The target's destination "+t.Destination+" is not the licence's "+l.Destination)
	case t.EndUserPartyID != l.EndUserPartyID:
		return nil, reject(CodeExportLicenseExpired, "/args/target/id", "The target's end user is not the licence's")
	case len(t.Lines) == 0:
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/target/id", "The target has no lines")
	}
	quantity := kernel.DecimalFromInt(0)
	for _, line := range t.Lines {
		if !l.covers(line) {
			return nil, reject(CodeExportLicenseExpired, "/args/target/id", "Line "+line.LineID+"'s goods are not the licence's")
		}
		if line.Unit != l.Unit {
			return nil, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/target/id", "Line "+line.LineID+" is in "+string(line.Unit)+", the licence in "+string(l.Unit))
		}
		quantity = quantity.Add(line.Quantity)
	}
	if t.Value.Currency() != l.Value.Currency() {
		return nil, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/target/id", "The target's value is in "+t.Value.Currency().Code()+", the licence's in "+l.Value.Currency().Code())
	}
	u, err := UsageOf(l, allocations)
	if err != nil {
		return nil, err
	}
	if quantity.Cmp(u.Available) > 0 {
		return nil, reject(CodeExportLicenseExceeded, "/args/target/id", "Quantity "+quantity.String()+" exceeds the "+u.Available.String()+" "+string(l.Unit)+" available")
	}
	if c, err := t.Value.Cmp(u.AvailableValue); err != nil || c > 0 {
		if err != nil {
			return nil, err
		}
		return nil, reject(CodeExportLicenseExceeded, "/args/target/id", "Value "+t.Value.String()+" exceeds the "+u.AvailableValue.String()+" available")
	}
	zero, err := kernel.NewMoney(l.Value.Currency(), 0)
	if err != nil {
		return nil, err
	}
	return &Allocation{ID: in.ID, LicenseID: l.ID, Target: t, Attempt: in.Attempt, Quantity: quantity, Value: t.Value,
		LinesHash: LinesHash(t.Lines), By: in.By, Epoch: in.Epoch, At: kernel.Instant(in.At), Released: kernel.DecimalFromInt(0),
		ReleasedValue: zero}, nil
}

func (l *License) covers(line Line) bool {
	if !l.Goods.ByClassification() {
		return slices.Contains(l.Goods.Specs, Spec{SkuID: line.SkuID, SpecRevision: line.SpecRevision})
	}
	return line.CommodityCode != "" && slices.Contains(l.Goods.CommodityCodes, line.CommodityCode)
}

// Evidence is an EVD source an external result rests on (SourceRef v1).
type Evidence struct {
	SourceID      string `json:"source_id"`
	ContentSHA256 string `json:"content_sha256"`
}

// Release is a release entry: the external result it rests on and what it releases of its
// allocation.
type Release struct {
	ID                string
	LicenseID         string
	AllocationID      string
	Kind              string
	Source            string
	ReceiptID         string
	DeclarationNumber string
	ReceiptText       string
	ReceivedAt        time.Time
	Evidence          []Evidence
	Quantity          kernel.Decimal
	Value             kernel.Money
	Reason            string
	By                kernel.ActorRef
	Epoch             int64
	At                time.Time
}

// ReleaseInput is a release as the command asks it: Quantity and Value for amended only.
type ReleaseInput struct {
	ID                string
	Kind              string
	ReceiptID         string // the declaration receipt recorded in CNX; "" for a stated result
	DeclarationNumber string
	ReceiptText       string
	ReceivedAt        time.Time
	Evidence          []Evidence
	Quantity          kernel.Decimal
	Value             kernel.Money
	Reason            string
	By                kernel.ActorRef
	Epoch             int64
	At                time.Time
}

// ReleaseAllocation releases what the external result in says of a (CNX-INV-002 "报关改单或撤销按外部
// 结果释放用量"): withdrawn releases everything outstanding, amended the quantity and value it
// names, never more than is outstanding (INVALID_STATE); an allocation with nothing outstanding is
// not released again (INVALID_STATE). The result is a declaration receipt CNX recorded, or the
// human's statement of a receipt with its evidence where no declaration in CNX covers the target.
func ReleaseAllocation(a *Allocation, in ReleaseInput) (*Release, error) {
	q, v, err := a.Outstanding()
	if err != nil {
		return nil, err
	}
	if q.Sign() <= 0 {
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/allocation_id", "Nothing of the allocation is outstanding")
	}
	source := Stated
	if in.ReceiptID != "" {
		source = DeclarationReceipt
	}
	r := &Release{ID: in.ID, LicenseID: a.LicenseID, AllocationID: a.ID, Kind: in.Kind, Source: source, ReceiptID: in.ReceiptID,
		DeclarationNumber: in.DeclarationNumber,
		ReceiptText:       in.ReceiptText, ReceivedAt: kernel.Instant(in.ReceivedAt), Evidence: in.Evidence, Quantity: q, Value: v, Reason: in.Reason,
		By: in.By, Epoch: in.Epoch, At: kernel.Instant(in.At)}
	if in.Kind == Withdrawn {
		return r, nil
	}
	if in.Quantity.Sign() <= 0 || in.Quantity.Cmp(q) > 0 {
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/quantity", "Quantity "+in.Quantity.String()+" is more than the "+q.String()+" outstanding")
	}
	c, err := in.Value.Cmp(v)
	if err != nil {
		return nil, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/value", "")
	}
	if c > 0 || in.Value.Sign() < 0 {
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/value", "Value "+in.Value.String()+" is more than the "+v.String()+" outstanding")
	}
	r.Quantity, r.Value = in.Quantity, in.Value
	return r, nil
}

// Apply adds r to its allocation's released quantity and value.
func (a *Allocation) Apply(r *Release) error {
	v, err := a.ReleasedValue.Add(r.Value)
	if err != nil {
		return err
	}
	a.Released, a.ReleasedValue = a.Released.Add(r.Quantity), v
	return nil
}
