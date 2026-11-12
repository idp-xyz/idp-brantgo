// Package declaration holds CNX's DeclarationCase (tactical model CNX-AGG-DeclarationCase with its
// DeclarationVersion, DeclarationItemMapping, DeclarationSubmission and DeclarationReceipt entities,
// CNX-INV-003, CNX-INV-006, SM-CNX-DECLARATION; baseline 22.1 DeclarationCase/DeclarationVersion/
// Receipt, 22.2 "出口行 ↔ 报关商品项", 23.3, CN-R05, CN-R07; UL CNX-DeclarationCase; WBS S5-05a; S5
// decisions E-1, D-17): the customs declaration of one FUL export lot, organised by Brant and filed
// outside it. S5 has manual paths only: a person submits through a broker or our own clerk and
// records each external result with its original text, the version of the external status mapping
// applied and the external version or business order (sequence).
//
// The case is the root, one per export lot and attempt (a new attempt only after the last was
// rejected or withdrawn). Its versions, confirmations, submissions and receipts are immutable
// entries; each command moves the root's row version by one under its FOR UPDATE lock. Internal
// states keep apart a local draft, the confirmed (exported) data, a manual submission, an unknown
// result, and customs' received, accepted, released, rejected, amended and withdrawn results. A
// customs release is not a departure nor a delivery (CN-R07): those are FUL's milestones.
package declaration

import (
	"slices"
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Codes the domain decides with besides the kernel's, and their categories (error catalog V0.1).
const (
	CodeDeclarationProfileUnverified = "DECLARATION_PROFILE_UNVERIFIED"
	CodeDeclarationMappingUnverified = "DECLARATION_MAPPING_UNVERIFIED"
	CodeDeclarationReceiptStale      = "DECLARATION_RECEIPT_STALE"
	CodeExternalResultUnknown        = "EXTERNAL_RESULT_UNKNOWN"
	CodeReferenceStale               = "REFERENCE_STALE"
)

// Categories are the categories of the domain's own codes.
var Categories = map[string]kernel.Category{
	CodeDeclarationProfileUnverified: kernel.CategoryInvalid,
	CodeDeclarationMappingUnverified: kernel.CategoryInvalid,
	CodeDeclarationReceiptStale:      kernel.CategoryConflict,
	CodeExternalResultUnknown:        kernel.CategoryConflict,
	CodeReferenceStale:               kernel.CategoryConflict,
}

func reject(code, path, reason string) *kernel.Rejection {
	return kernel.RejectAs(code, Categories[code], path, reason)
}

// States of a case (SM-CNX-DECLARATION). Confirmed is the plan's "exported": the confirmed version
// is the data handed over for the manual submission.
const (
	Draft     = "draft"
	Confirmed = "confirmed"
	Submitted = "submitted"
	Unknown   = "unknown"
	Received  = "received"
	Accepted  = "accepted"
	Released  = "released"
	Rejected  = "rejected"
	Amended   = "amended"
	Withdrawn = "withdrawn"
)

// Kinds of a version, mapping states, submitters and conversion sources.
const (
	VersionPrepared = "prepared"
	VersionAmended  = "amended"

	MappingVerified   = "verified"
	MappingUnverified = "unverified"

	SubmitterBroker   = "broker"
	SubmitterOwnClerk = "own_clerk"

	// The conversion of a line's quantity into the SKU's base unit: none needed (the line is in the
	// base unit), a POL unit_conversion revision (受控换算), or none available (the mapping is
	// unverified).
	ConversionBaseUnit       = "base_unit"
	ConversionUnitConversion = "unit_conversion"
	ConversionNone           = "none"

	// ReasonUnitNotConverted: no controlled conversion from the line's unit into the base unit.
	ReasonUnitNotConverted = "unit_not_converted"
)

// BaseUnit is the base unit of every SKU (CAT's UnitPCS): CAT's legal unit conversions convert
// from it.
const BaseUnit kernel.UnitCode = "PCS"

// Terminal reports whether a case in status s takes no further receipt.
func Terminal(s string) bool { return s == Rejected || s == Withdrawn }

// next lists the outcomes a receipt may record in each state. SM-CNX-DECLARATION draws the main
// path one step at a time; receipts arrive out of order (CN-AT06, baseline 23.3), so a receipt may
// move the case forward past results whose receipts are still missing, never back. The SM draws
// nothing after amended: another amendment and a withdrawal after an amendment are admitted (a gap
// reported for the design).
var next = map[string][]string{
	Submitted: {Unknown, Received, Accepted, Released, Rejected},
	Unknown:   {Received, Accepted, Released, Rejected},
	Received:  {Accepted, Released, Rejected},
	Accepted:  {Released, Rejected},
	Released:  {Amended, Withdrawn},
	Amended:   {Amended, Withdrawn},
}

// Outcomes are the outcomes a receipt records.
var Outcomes = []string{Unknown, Received, Accepted, Released, Rejected, Amended, Withdrawn}

// Exported reports whether a case in status s stands as exported by customs: released, or amended
// after its release. Its export date is the one the receipts state.
func Exported(s string) bool { return s == Released || s == Amended }

// Case is the DeclarationCase root.
type Case struct {
	ID                string
	ExportLotID       string
	Attempt           int
	ShipmentID        string
	OrderID           string
	OrderRevision     int64
	OrderHash         string
	CodeTableVersion  string
	Status            string
	Version           int
	Mapping           string
	DeclarationNumber string
	ExportedAt        time.Time // zero: no export date
	LastSequence      int64     // 0: no external result yet
	Submissions       int
	Receipts          int
	RowVersion        int64
	OpenedBy          kernel.ActorRef
	Epoch             int64
	OpenedAt          time.Time
}

// CheckVersion compares the caller's expected row version with c's.
func CheckVersion(c *Case, expected int64) error {
	if c.RowVersion != expected {
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	return nil
}

// LotLine is a line of the export lot as FUL states it (ExportLotRef v1, R36).
type LotLine struct {
	LineID       string
	ShipmentID   string
	OrderLineID  string
	SkuID        string
	SpecRevision int64
	Quantity     kernel.Decimal
	Unit         kernel.UnitCode
	Amount       kernel.Money
}

// Element is a declaration element (申报要素).
type Element struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// LegalUnit is a legal unit of quantity and its conversion from the base unit, as CAT's verified
// profile states it.
type LegalUnit struct {
	Unit        string
	PerBaseUnit kernel.Decimal
	Basis       string
}

// Profile is CAT's declaration profile of a line's SKU specification under the code table
// (DeclarationProfileRef v1, R39): the revision verified now and its content; Verified false when
// the profile has only a draft.
type Profile struct {
	ProfileID       string
	Revision        int64
	ContentHash     string
	Verified        bool
	CommodityCode   string
	DeclarationName string
	Elements        []Element
	LegalUnits      []LegalUnit
}

// Conversion is a POL unit_conversion revision for a line's SKU specification (one From is Factor
// To), as POL adopts it for the line.
type Conversion struct {
	ConfigID    string
	Revision    int64
	ContentHash string
	From, To    kernel.UnitCode
	Factor      kernel.Decimal
}

// LineInput maps one lot line into an item, with the unit conversion the request names for it
// (Conversion, or the refusal POL gave for it in Err).
type LineInput struct {
	LineID     string
	Conversion *Conversion
	Err        error
}

// ItemInput is one declaration item and the lot lines it declares.
type ItemInput struct {
	ItemNo int
	Lines  []LineInput
}

// PrepareInput is what a preparation maps: the lot's lines, the profiles by line, the items.
type PrepareInput struct {
	Lines    []LotLine
	Profiles map[string]Profile // by lot line ID; absent: no profile
	Items    []ItemInput
}

// Prepare maps the export lot's lines into declaration items (CNX-INV-006; baseline 22.2 "出口行 ↔
// 报关商品项": combinations registered explicitly, never matched by name). Refusals in this order, for
// each line of each item: a line the lot does not have (REFERENCE_INVALID), a line without a
// profile verified for the code table (DECLARATION_PROFILE_UNVERIFIED, CAT-INV-002), a line whose
// commodity code, declaration name, elements or legal units differ from its item's first line
// (INVALID_KNOWN_VALUE: "不同编码或申报要素不得合并"), a unit conversion POL refused, one for another
// unit or not into the base unit, or one named for a line already in the base unit
// (INVALID_KNOWN_VALUE); then a lot line no item declares (INVALID_KNOWN_VALUE /args/items).
//
// A line's mapping is verified when its quantity reaches the base unit on a professional basis: it
// is in the base unit, or a POL unit_conversion converts it (受控换算); the legal quantities follow
// from the verified profile's conversions, exactly. A line in another unit without a conversion is
// unverified, and so are its item and the version: confirmation refuses it
// (DECLARATION_MAPPING_UNVERIFIED, CN-AT03).
func Prepare(in PrepareInput) ([]Item, string, error) {
	byID := map[string]LotLine{}
	for _, l := range in.Lines {
		byID[l.LineID] = l
	}
	declared := map[string]bool{}
	mapping := MappingVerified
	var items []Item
	for i, it := range in.Items {
		item := Item{ItemNo: it.ItemNo, Mapping: MappingVerified}
		ipath := "/args/items/" + strconv.Itoa(i)
		var first *Profile
		for j, li := range it.Lines {
			path := ipath + "/lines/" + strconv.Itoa(j)
			l, ok := byID[li.LineID]
			if !ok {
				return nil, "", kernel.Reject(kernel.CodeReferenceInvalid, path+"/line_id", "The export lot has no such line")
			}
			p, ok := in.Profiles[li.LineID]
			if !ok || !p.Verified {
				return nil, "", reject(CodeDeclarationProfileUnverified, path+"/line_id", "No declaration profile of "+l.SkuID+" r"+
					strconv.FormatInt(l.SpecRevision, 10)+" is verified for the code table")
			}
			if first == nil {
				first = &p
				item.CommodityCode, item.DeclarationName, item.Elements = p.CommodityCode, p.DeclarationName, p.Elements
				for _, u := range p.LegalUnits {
					item.LegalQuantities = append(item.LegalQuantities, LegalQuantity{Unit: u.Unit})
				}
			} else if !sameClassification(*first, p) {
				return nil, "", kernel.Reject(kernel.CodeInvalidKnownValue, path+"/line_id",
					"Lines of different commodity codes, declaration names, elements or legal units are never merged into one item")
			}
			line, err := mapLine(l, p, li, path)
			if err != nil {
				return nil, "", err
			}
			declared[li.LineID] = true
			item.Lines = append(item.Lines, line)
		}
		if err := item.total(); err != nil {
			return nil, "", err
		}
		if item.Mapping != MappingVerified {
			mapping = MappingUnverified
		}
		items = append(items, item)
	}
	for _, l := range in.Lines {
		if !declared[l.LineID] {
			return nil, "", kernel.Reject(kernel.CodeInvalidKnownValue, "/args/items", "Line "+l.LineID+" of the export lot is in no item")
		}
	}
	return items, mapping, nil
}

func sameClassification(a, b Profile) bool {
	if a.CommodityCode != b.CommodityCode || a.DeclarationName != b.DeclarationName || !slices.Equal(a.Elements, b.Elements) ||
		len(a.LegalUnits) != len(b.LegalUnits) {
		return false
	}
	for i := range a.LegalUnits {
		if a.LegalUnits[i].Unit != b.LegalUnits[i].Unit {
			return false
		}
	}
	return true
}

func mapLine(l LotLine, p Profile, in LineInput, path string) (ItemLine, error) {
	out := ItemLine{LineID: l.LineID, ShipmentID: l.ShipmentID, OrderLineID: l.OrderLineID, SkuID: l.SkuID, SpecRevision: l.SpecRevision,
		Quantity: l.Quantity, Unit: l.Unit, Amount: l.Amount, ProfileID: p.ProfileID, ProfileRevision: p.Revision, ProfileHash: p.ContentHash,
		Mapping: MappingVerified}
	cpath := path + "/unit_conversion"
	switch {
	case in.Err != nil:
		if rej, ok := kernel.AsRejection(in.Err); ok {
			reason := ""
			if len(rej.Fields) > 0 {
				reason = rej.Fields[0].Reason
			}
			return out, kernel.RejectAs(rej.Code, rej.Category, cpath, reason)
		}
		return out, in.Err
	case in.Conversion != nil && l.Unit == BaseUnit:
		return out, kernel.Reject(kernel.CodeInvalidKnownValue, cpath, "The line is in the base unit "+string(BaseUnit)+" already")
	case in.Conversion != nil:
		c := in.Conversion
		if c.From != l.Unit || c.To != BaseUnit {
			return out, kernel.Reject(kernel.CodeInvalidKnownValue, cpath, "The conversion is from "+string(c.From)+" to "+string(c.To)+
				", not from "+string(l.Unit)+" to "+string(BaseUnit))
		}
		out.Conversion = Applied{Source: ConversionUnitConversion, ConfigID: c.ConfigID, Revision: c.Revision, ContentHash: c.ContentHash, Factor: c.Factor}
		out.BaseQuantity = l.Quantity.Mul(c.Factor)
	case l.Unit == BaseUnit:
		out.Conversion = Applied{Source: ConversionBaseUnit, Factor: kernel.DecimalFromInt(1)}
		out.BaseQuantity = l.Quantity
	default:
		out.Conversion = Applied{Source: ConversionNone}
		out.Mapping, out.Reason = MappingUnverified, ReasonUnitNotConverted
		return out, nil
	}
	for _, u := range p.LegalUnits {
		out.LegalQuantities = append(out.LegalQuantities, LegalQuantity{Unit: u.Unit, Quantity: out.BaseQuantity.Mul(u.PerBaseUnit),
			PerBaseUnit: u.PerBaseUnit, Basis: u.Basis})
	}
	return out, nil
}

// Applied is how a line's quantity reached the base unit.
type Applied struct {
	Source      string
	ConfigID    string
	Revision    int64
	ContentHash string
	Factor      kernel.Decimal
}

// LegalQuantity is a quantity in a legal unit: of a line with the conversion it rests on, or an
// item's sum.
type LegalQuantity struct {
	Unit        string
	Quantity    kernel.Decimal
	PerBaseUnit kernel.Decimal
	Basis       string
}

// ItemLine is a lot line as an item declares it.
type ItemLine struct {
	LineID          string
	ShipmentID      string
	OrderLineID     string
	SkuID           string
	SpecRevision    int64
	Quantity        kernel.Decimal
	Unit            kernel.UnitCode
	Amount          kernel.Money
	ProfileID       string
	ProfileRevision int64
	ProfileHash     string
	Conversion      Applied
	BaseQuantity    kernel.Decimal // zero when unverified
	LegalQuantities []LegalQuantity
	Mapping         string
	Reason          string
}

// Item is a declaration item (报关商品项): its classification from the verified profile, its base
// and legal quantities, its value, its lines and whether its mapping is verified.
type Item struct {
	ItemNo          int
	CommodityCode   string
	DeclarationName string
	Elements        []Element
	BaseQuantity    kernel.Decimal
	LegalQuantities []LegalQuantity
	Value           kernel.Money
	Mapping         string
	Lines           []ItemLine
}

// total sums the item's lines: its value always, its base and legal quantities when every line is
// verified.
func (it *Item) total() error {
	for i, l := range it.Lines {
		switch {
		case i == 0 || l.Amount.Currency().IsZero():
			if i == 0 {
				it.Value = l.Amount
			}
		default:
			v, err := it.Value.Add(l.Amount)
			if err != nil {
				return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/items", "The lot's lines are in different currencies")
			}
			it.Value = v
		}
		if l.Mapping != MappingVerified {
			it.Mapping = MappingUnverified
		}
	}
	if it.Mapping != MappingVerified {
		it.LegalQuantities = nil
		return nil
	}
	for _, l := range it.Lines {
		it.BaseQuantity = it.BaseQuantity.Add(l.BaseQuantity)
		for k := range it.LegalQuantities {
			it.LegalQuantities[k].Quantity = it.LegalQuantities[k].Quantity.Add(l.LegalQuantities[k].Quantity)
		}
	}
	return nil
}

// CheckDraft refuses a change of a case that is not a draft (INVALID_STATE).
func CheckDraft(c *Case) error {
	if c.Status != Draft {
		return kernel.Reject(kernel.CodeInvalidState, "/args/declaration_id", "The declaration is "+c.Status+", not a draft")
	}
	return nil
}

// CheckConfirm decides a confirmation of version of c: a draft (INVALID_STATE), its current version
// (REFERENCE_STALE), whose mapping is verified (DECLARATION_MAPPING_UNVERIFIED, CNX-INV-006,
// CN-AT03: the unverified items' numbers in the reason).
func CheckConfirm(c *Case, version int, items []Item) error {
	if err := CheckDraft(c); err != nil {
		return err
	}
	if version != c.Version {
		return reject(CodeReferenceStale, "/args/version", "The declaration's current version is "+strconv.Itoa(c.Version))
	}
	var open []string
	for _, it := range items {
		if it.Mapping != MappingVerified {
			open = append(open, strconv.Itoa(it.ItemNo))
		}
	}
	if len(open) > 0 {
		return reject(CodeDeclarationMappingUnverified, "/args/version", "Items without a verified mapping: "+join(open))
	}
	return nil
}

func join(s []string) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += ", "
		}
		out += x
	}
	return out
}

// CheckSubmission decides a manual submission of c (SM-CNX-DECLARATION confirmed → submitted): an
// unknown result is verified first (EXTERNAL_RESULT_UNKNOWN, CNX-INV-003, CN-AT13: the original
// business operation stays the only one); only a confirmed case is submitted (INVALID_STATE).
func CheckSubmission(c *Case) error {
	switch c.Status {
	case Confirmed:
		return nil
	case Unknown:
		return reject(CodeExternalResultUnknown, "/args/declaration_id",
			"The submission's result is unknown: verify it and record the result before anything is submitted again")
	}
	return kernel.Reject(kernel.CodeInvalidState, "/args/declaration_id", "The declaration is "+c.Status+", not confirmed")
}

// ReceiptInput is an external result as a person records it.
type ReceiptInput struct {
	Outcome           string
	Sequence          int64 // 0 for unknown
	DeclarationNumber string
	ExportedAt        time.Time
	ReceivedAt        time.Time
	AmendedItems      map[int]bool
	At                time.Time
}

// CheckReceipt decides a receipt of c (CNX-INV-003, CN-R05; SM-CNX-DECLARATION), in this order:
// nothing submitted yet (INVALID_STATE); a result not after the one recorded by its external
// sequence (DECLARATION_RECEIPT_STALE: an older or repeated receipt never overrides a newer result,
// CN-AT06); an outcome that does not follow from the state (INVALID_STATE; nothing follows a
// rejection or a withdrawal); a declaration number other than the one recorded, an export date after
// the receipt was received or recorded, an amended item the version does not have
// (INVALID_KNOWN_VALUE). An unknown result follows a submission only and carries no sequence.
func CheckReceipt(c *Case, items []Item, in ReceiptInput) error {
	const path = "/args/outcome"
	if c.Status == Draft || c.Status == Confirmed {
		return kernel.Reject(kernel.CodeInvalidState, "/args/declaration_id", "Nothing was submitted")
	}
	if in.Outcome != Unknown && in.Sequence <= c.LastSequence {
		return reject(CodeDeclarationReceiptStale, "/args/external/sequence", "The receipt is not after the result recorded at sequence "+
			strconv.FormatInt(c.LastSequence, 10))
	}
	if !slices.Contains(next[c.Status], in.Outcome) {
		return kernel.Reject(kernel.CodeInvalidState, path, "A "+in.Outcome+" result does not follow a "+c.Status+" declaration")
	}
	if c.DeclarationNumber != "" && in.DeclarationNumber != "" && in.DeclarationNumber != c.DeclarationNumber {
		return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/declaration_number", "The declaration's number is "+c.DeclarationNumber)
	}
	if !in.ExportedAt.IsZero() && (in.ExportedAt.After(in.ReceivedAt) || in.ExportedAt.After(in.At)) {
		return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/exported_at", "The export date is after the receipt")
	}
	for no := range in.AmendedItems {
		if !slices.ContainsFunc(items, func(it Item) bool { return it.ItemNo == no }) {
			return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/amended_items", "The declaration has no item "+strconv.Itoa(no))
		}
	}
	return nil
}

// Apply moves c by a receipt that CheckReceipt admitted: its state, the external sequence, the
// declaration number once stated, and the export date — set by a release, changed by an amendment
// that states one, cleared by a withdrawal (the goods were not exported under it).
func (c *Case) Apply(in ReceiptInput) {
	c.Status = in.Outcome
	c.Receipts++
	if in.Outcome != Unknown {
		c.LastSequence = in.Sequence
	}
	if c.DeclarationNumber == "" {
		c.DeclarationNumber = in.DeclarationNumber
	}
	switch {
	case in.Outcome == Withdrawn:
		c.ExportedAt = time.Time{}
	case !in.ExportedAt.IsZero():
		c.ExportedAt = in.ExportedAt
	}
}
