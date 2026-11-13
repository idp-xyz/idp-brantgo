package declaration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// DocumentContract names the version document's layout.
const DocumentContract = "brant.china-export.declaration/0.1.0"

// Document is a DeclarationVersion as it is stored: what was declared (the lot, the order and its
// sales documents as TRD showed them, the code table, the items with their lines, profiles,
// conversions, quantities and values, the mapping), by whom and when; an amendment version also
// names the receipt and the items it amended with their base quantities before and after. It is
// stored as canonical JSON with its SHA-256 and never changes.
type Document struct {
	Contract         string         `json:"contract"`
	DeclarationID    string         `json:"declaration_id"`
	Version          int            `json:"version"`
	Kind             string         `json:"kind"`
	ExportLot        DocLot         `json:"export_lot"`
	Order            DocOrder       `json:"order"`
	CodeTableVersion string         `json:"code_table_version"`
	Mapping          string         `json:"mapping"`
	Items            []DocItem      `json:"items"`
	Amendments       []DocAmendment `json:"amendments"`
	ReceiptID        string         `json:"receipt_id"`
	PreparedBy       DocActor       `json:"prepared_by"`
	AuthorityEpoch   int64          `json:"authority_epoch"`
	PreparedAt       string         `json:"prepared_at"`
	// CustomsOffice is the customs office declared at (申报地海关; S5R-D04 一证一关), "" when not
	// stated; omitted then, so versions stored before keep their bytes.
	CustomsOffice string `json:"customs_office,omitempty"`
}

// DocLot is the export lot as FUL showed it.
type DocLot struct {
	ExportLotID   string `json:"export_lot_id"`
	Version       int64  `json:"version"`
	Status        string `json:"status"`
	ShipmentID    string `json:"shipment_id"`
	OrderID       string `json:"order_id"`
	OrderRevision int64  `json:"order_revision"`
	Currency      string `json:"currency"`
	ValueMinor    string `json:"value_minor"`
}

// DocOrder is the sales order as TRD showed it (SalesOrderRef v1 and SalesContractDocumentRef v1,
// R46): its current revision at its hash, its state and its S/C and PI documents.
type DocOrder struct {
	OrderID       string           `json:"order_id"`
	OrderRevision int64            `json:"order_revision"`
	OrderHash     string           `json:"order_hash"`
	Status        string           `json:"status"`
	Documents     []DocSalesRecord `json:"documents"`
}

// DocSalesRecord is a sales contract or proforma invoice of the order.
type DocSalesRecord struct {
	Kind           string `json:"kind"`
	Number         string `json:"number"`
	OrderRevision  int64  `json:"order_revision"`
	DocumentSHA256 string `json:"document_sha256"`
	Current        bool   `json:"current"`
	Signatures     int    `json:"signatures"`
}

// DocItem is a declaration item.
type DocItem struct {
	ItemNo          int        `json:"item_no"`
	CommodityCode   string     `json:"commodity_code"`
	DeclarationName string     `json:"declaration_name"`
	Elements        []Element  `json:"elements"`
	BaseUnit        string     `json:"base_unit"`
	BaseQuantity    string     `json:"base_quantity"`
	LegalQuantities []DocLegal `json:"legal_quantities"`
	Currency        string     `json:"currency"`
	ValueMinor      string     `json:"value_minor"`
	Mapping         string     `json:"mapping"`
	Lines           []DocLine  `json:"lines"`
}

// DocLegal is a quantity in a legal unit; for a line, with the conversion factor and its basis.
type DocLegal struct {
	Unit        string `json:"unit"`
	Quantity    string `json:"quantity"`
	PerBaseUnit string `json:"per_base_unit"`
	Basis       string `json:"basis"`
}

// DocLine is a lot line of an item.
type DocLine struct {
	LineID          string        `json:"line_id"`
	ShipmentID      string        `json:"shipment_id"`
	OrderLineID     string        `json:"order_line_id"`
	SkuID           string        `json:"sku_id"`
	SpecRevision    int64         `json:"spec_revision"`
	Quantity        string        `json:"quantity"`
	Unit            string        `json:"unit"`
	AmountMinor     string        `json:"amount_minor"`
	Profile         DocProfile    `json:"profile"`
	Conversion      DocConversion `json:"conversion"`
	BaseQuantity    string        `json:"base_quantity"`
	LegalQuantities []DocLegal    `json:"legal_quantities"`
	Mapping         string        `json:"mapping"`
	Reason          string        `json:"reason"`
}

// DocProfile is the declaration profile revision a line rests on.
type DocProfile struct {
	ProfileID   string `json:"profile_id"`
	Revision    int64  `json:"revision"`
	ContentHash string `json:"content_hash"`
}

// DocConversion is how a line's quantity reached the base unit.
type DocConversion struct {
	Source      string `json:"source"`
	ConfigID    string `json:"config_id"`
	Revision    int64  `json:"revision"`
	ContentHash string `json:"content_hash"`
	Factor      string `json:"factor"`
}

// DocAmendment is an item an amendment receipt changed: its base quantity before and after.
type DocAmendment struct {
	ItemNo int    `json:"item_no"`
	Before string `json:"base_quantity_before"`
	After  string `json:"base_quantity_after"`
}

// DocActor is who prepared the version.
type DocActor struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

func decimalText(d kernel.Decimal, ok bool) string {
	if !ok {
		return ""
	}
	return d.String()
}

// DocItems turns mapped items into their document form.
func DocItems(items []Item) []DocItem {
	out := []DocItem{}
	for _, it := range items {
		d := DocItem{ItemNo: it.ItemNo, CommodityCode: it.CommodityCode, DeclarationName: it.DeclarationName, Elements: it.Elements,
			BaseUnit: string(BaseUnit), BaseQuantity: decimalText(it.BaseQuantity, it.Mapping == MappingVerified), LegalQuantities: []DocLegal{},
			Currency: it.Value.Currency().Code(), ValueMinor: it.Value.MinorString(), Mapping: it.Mapping, Lines: []DocLine{}}
		if d.Elements == nil {
			d.Elements = []Element{}
		}
		for _, q := range it.LegalQuantities {
			d.LegalQuantities = append(d.LegalQuantities, DocLegal{Unit: q.Unit, Quantity: q.Quantity.String()})
		}
		for _, l := range it.Lines {
			verified := l.Mapping == MappingVerified
			dl := DocLine{LineID: l.LineID, ShipmentID: l.ShipmentID, OrderLineID: l.OrderLineID, SkuID: l.SkuID, SpecRevision: l.SpecRevision,
				Quantity: l.Quantity.String(), Unit: string(l.Unit), AmountMinor: l.Amount.MinorString(),
				Profile:      DocProfile{ProfileID: l.ProfileID, Revision: l.ProfileRevision, ContentHash: l.ProfileHash},
				Conversion:   DocConversion{Source: l.Conversion.Source, ConfigID: l.Conversion.ConfigID, Revision: l.Conversion.Revision, ContentHash: l.Conversion.ContentHash},
				BaseQuantity: decimalText(l.BaseQuantity, verified), LegalQuantities: []DocLegal{}, Mapping: l.Mapping, Reason: l.Reason}
			if l.Conversion.Source != ConversionNone {
				dl.Conversion.Factor = l.Conversion.Factor.String()
			}
			for _, q := range l.LegalQuantities {
				dl.LegalQuantities = append(dl.LegalQuantities, DocLegal{Unit: q.Unit, Quantity: q.Quantity.String(), PerBaseUnit: q.PerBaseUnit.String(),
					Basis: q.Basis})
			}
			d.Lines = append(d.Lines, dl)
		}
		out = append(out, d)
	}
	return out
}

// Version is a stored DeclarationVersion: its document, the bytes stored and their SHA-256.
type Version struct {
	Document
	Bytes  []byte
	SHA256 string
}

// NewVersion seals d as stored bytes and their SHA-256.
func NewVersion(d Document) (*Version, error) {
	d.Contract = DocumentContract
	if d.Items == nil {
		d.Items = []DocItem{}
	}
	if d.Amendments == nil {
		d.Amendments = []DocAmendment{}
	}
	if d.Order.Documents == nil {
		d.Order.Documents = []DocSalesRecord{}
	}
	b, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	return &Version{Document: d, Bytes: b, SHA256: hex.EncodeToString(sum[:])}, nil
}

// ErrDocumentNotIntact means a stored document does not match its stored SHA-256.
var ErrDocumentNotIntact = errors.New("cnx: declaration version document does not match its hash")

// ParseVersion reads a stored document, checking it against its SHA-256.
func ParseVersion(b []byte, hash string) (*Version, error) {
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != hash {
		return nil, ErrDocumentNotIntact
	}
	var d Document
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, err
	}
	return &Version{Document: d, Bytes: b, SHA256: hash}, nil
}

// Amend returns the items of d with the amended base quantities applied to the document form and
// the amendments they make: the receipt's quantities as customs stated them, never recomputed.
func Amend(items []DocItem, amended map[int]string) ([]DocItem, []DocAmendment) {
	var out []DocItem
	var changes []DocAmendment
	for _, it := range items {
		if q, ok := amended[it.ItemNo]; ok {
			changes = append(changes, DocAmendment{ItemNo: it.ItemNo, Before: it.BaseQuantity, After: q})
			it.BaseQuantity = q
		}
		out = append(out, it)
	}
	return out, changes
}

// Items returns the items of a stored version with their numbers and mappings, as CheckConfirm and
// CheckReceipt read them.
func Items(d Document) []Item {
	var out []Item
	for _, it := range d.Items {
		out = append(out, Item{ItemNo: it.ItemNo, Mapping: it.Mapping})
	}
	return out
}
