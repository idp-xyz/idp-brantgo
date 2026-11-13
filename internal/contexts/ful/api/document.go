package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The shipment document set (WBS S5-08; tactical model FUL-AGG-ShipmentDocumentSet with
// CommercialInvoice, PackingList, BillOfLadingRecord, OriginCertificateRef, DocumentReconciliation and
// SignedCopy; SM-FUL-DOCUMENT; FUL-INV-004; UL FUL-ShipmentDocumentSet; master data MD-39; baseline
// BT12, CN-AT17, INT-05; Context Map R49 POL->FUL ConfigRef, ConfigGuard and NumberIssue, R44
// PTY->FUL, R74 EVD->FUL; SAP reference O2C-12; S5 decisions D-12, D-17). A set belongs to one
// shipment and holds, per document kind, versions that are only appended: the commercial invoice
// (CI) and the packing list (PL), which FUL renders, and the records of the external bill of lading
// (B/L) and certificate of origin (CO). A version is a draft, issued or superseded.
//
//	shipment_document.prepare        human or AI (documentation): a new draft version of one kind.
//	                                 expected_version is the set's row version (0 creates the set,
//	                                 one per shipment). A CI or PL is rendered by shipment-text/0.1.0
//	                                 from 1–2 POL shipment_document_template revisions of its kind in
//	                                 distinct languages (en, zh-CN: one language-neutral content, so
//	                                 amounts, currency, names and terms correspond, CN-AT17), the
//	                                 seller's POL company_profile revision and the shipment's facts,
//	                                 and numbered by POL NumberIssue from a number_sequence revision
//	                                 (document type invoice for a CI, packing_list for a PL; business
//	                                 reference the set ID, so every version of a kind keeps its
//	                                 number). The CI's lines are the shipment lines at the prices the
//	                                 shipment keeps; the PL's packages are the shipment's actual
//	                                 packing unless stated (the factory's list, say). The
//	                                 particulars — consignee (default: the order's consignee address,
//	                                 else the buyer), notify, marks, ports (default: the booking's)
//	                                 and the goods' names per SKU and language — are stated per
//	                                 version. A B/L or CO is a record of an external document: its
//	                                 data and an EVD file. The draft the kind had before is
//	                                 superseded; the issued one stays.
//	shipment_document.issue          human only: issues every draft of the set when the set has a CI
//	                                 and a PL and every difference of the reconciliation is resolved
//	                                 or acknowledged with a reason (SHIPMENT_DOCUMENTS_UNRECONCILED,
//	                                 BT12) — an earlier issue's acknowledgement of the very same
//	                                 difference is carried; the templates, number sequences and seller profiles of the
//	                                 drafts are re-checked in force by POL's ConfigGuard at the content
//	                                 hashes they were rendered from. The version a kind had issued
//	                                 before is superseded; an issued version never changes.
//	shipment_document.record_signed  human only: registers the signed or stamped copy of one language
//	                                 of an issued CI or PL version, an EVD file the caller reads; the
//	                                 generated, unsigned bytes are never the signed copy.
//
// The reconciliation compares, item by item, the current version of every kind (the draft, else the
// issued one) with the shipment lines, the order lines' prices the shipment keeps, the actual
// packing, the booking and each other: SKU, quantity, unit, unit price and amount of every line,
// quantity totals, cartons, weights and volume, consignee, notify, marks, ports, vessel and voyage,
// and the sources a version was prepared from. Every difference has a path into the document
// (/packing_list/totals/quantity/PCS) and names what it was compared with.
//
// Request members in snake_case: shipment_document.prepare {document_set_id, shipment_id,
// expected_version, document_kind, templates [{kind, id, revision}], sequence_ref, seller_ref,
// particulars {consignee {name, address}, notify {name, address}, marks, port_of_loading,
// port_of_discharge, descriptions [{sku_id, language, text}]}, packages (as packing.record_actual),
// bill_of_lading {number, file_id, vessel, voyage, port_of_loading, port_of_discharge,
// shipped_on_board, consignee, notify, marks, packages, gross_weight_kg, volume_cbm, container_numbers},
// certificate {number, file_id, issuer, issued_on, certificate_ref {certificate_id, version}},
// evidence_ids}; shipment_document.issue {document_set_id, expected_version, acknowledgements [{path,
// reason}]}; shipment_document.record_signed {document_set_id, document_kind, version, language,
// file_id, evidence_ids}.
const (
	CommandPrepareShipmentDocument      = "shipment_document.prepare"
	CommandIssueShipmentDocuments       = "shipment_document.issue"
	CommandRecordSignedShipmentDocument = "shipment_document.record_signed"
	CommandGetDocumentSet               = "shipment_document_set.get"
	CommandGetShipmentDocument          = "shipment_document.get"

	EventShipmentDocumentIssued = "idp-brant.ful.shipment_document_issued.v1"

	// The document kinds of a set.
	DocumentCommercialInvoice   = "commercial_invoice"
	DocumentPackingList         = "packing_list"
	DocumentBillOfLading        = "bill_of_lading"
	DocumentCertificateOfOrigin = "certificate_of_origin"

	// States of a document version (SM-FUL-DOCUMENT) and the receipt status of record_signed.
	DocumentDraft      = "draft"
	DocumentIssued     = "issued"
	DocumentSuperseded = "superseded"
	StatusSigned       = "signed"

	// The renderer of CIs and PLs, its languages and the POL kinds a rendering names.
	ShipmentRendererVersion      = "shipment-text/0.1.0"
	LanguageEn                   = "en"
	LanguageZhCN                 = "zh-CN"
	KindShipmentDocumentTemplate = "shipment_document_template"
	KindNumberSequence           = "number_sequence"
	KindCompanyProfile           = "company_profile"
	DocumentTypeInvoice          = "invoice"
	DocumentTypePackingList      = "packing_list"

	// Reconciliation states of a set.
	Reconciled   = "reconciled"
	Unreconciled = "unreconciled"

	MaxTemplates        = 2
	MaxDescriptions     = 400
	MaxDescription      = 500
	MaxPartyName        = 200
	MaxPartyAddress     = 1000
	MaxMarks            = 2000
	MaxDocumentNumber   = 64
	MaxContainerNumbers = 100
	MaxAcknowledgements = 200
	MaxPath             = 300
)

// DocumentKind reports whether s is a document kind of a set.
func DocumentKind(s string) bool {
	return s == DocumentCommercialInvoice || s == DocumentPackingList || s == DocumentBillOfLading || s == DocumentCertificateOfOrigin
}

// Rendered reports whether FUL renders documents of kind s (a CI or a PL).
func Rendered(s string) bool { return s == DocumentCommercialInvoice || s == DocumentPackingList }

// Language reports whether s is a language shipment-text/0.1.0 has a layout for.
func Language(s string) bool { return s == LanguageEn || s == LanguageZhCN }

// ConfigRef names a POL configuration revision (ConfigRef v1): {kind, id, revision}.
type ConfigRef struct {
	Kind     string
	ID       string
	Revision int64
}

func (r ConfigRef) validate(kind, path string) error {
	switch {
	case r.Kind != kind:
		return schema(path + "/kind")
	case !kernel.ValidIdentifier(r.ID):
		return schema(path + "/id")
	case r.Revision < 1 || r.Revision > maxVersion:
		return schema(path + "/revision")
	}
	return nil
}

// DocumentParty is a party as a document prints it: a name and an address.
type DocumentParty struct {
	Name    string
	Address string
}

func (p DocumentParty) validate(path string) error {
	switch {
	case !Line(p.Name, MaxPartyName):
		return schema(path + "/name")
	case !Block(p.Address, MaxPartyAddress):
		return schema(path + "/address")
	}
	return nil
}

// Description names the goods of a SKU in one language.
type Description struct {
	SkuID    string
	Language string
	Text     string
}

// Particulars are what a CI or PL version prints besides FUL's facts (INT-05: an explicit snapshot
// per shipment, master data as defaults): the consignee (nil: the order's consignee address, else
// the buyer), the notify party (nil: none), the shipping marks ("": none, printed N/M), the ports of
// loading and discharge (UN/LOCODE; "": the current booking's) and the goods' name of every SKU of
// the shipment in every language rendered.
type Particulars struct {
	Consignee       *DocumentParty
	Notify          *DocumentParty
	Marks           string
	PortOfLoading   string
	PortOfDischarge string
	Descriptions    []Description
}

// BillOfLading is the record of an external bill of lading: its number and EVD file, and what it
// states.
type BillOfLading struct {
	Number           string
	FileID           string
	Vessel           string
	Voyage           string
	PortOfLoading    string
	PortOfDischarge  string
	ShippedOnBoard   string // YYYY-MM-DD
	Consignee        DocumentParty
	Notify           *DocumentParty
	Marks            string
	Packages         int64  // the number of cartons
	GrossWeightKg    string // in total
	VolumeCbm        string // in total, "" when not stated
	ContainerNumbers []string
}

// CertificateRef names a certificate result in CNX at a revision (CertificateResultRef v1, R56). The
// prepare is refused unless CNX holds that revision as current and it applies to the shipment now:
// REFERENCE_INVALID (no such certificate), REFERENCE_STALE (a correction superseded the revision),
// CERTIFICATE_NOT_APPLICABLE (another kind, market or specification, or outside its validity), and
// INVALID_KNOWN_VALUE at /args/certificate/number when the record's number is not the certificate's
// (S5 C5).
type CertificateRef struct {
	CertificateID string
	Version       int64
}

// CertificateOfOrigin is the record of an external certificate of origin.
type CertificateOfOrigin struct {
	Number         string
	FileID         string
	Issuer         string
	IssuedOn       string // YYYY-MM-DD
	CertificateRef *CertificateRef
}

// PrepareShipmentDocument prepares a new draft version of kind DocumentKind in set DocumentSetID of
// shipment ShipmentID at the set's row version ExpectedVersion (0: the set is created). Human or AI.
type PrepareShipmentDocument struct {
	IdempotencyKey  string
	DocumentSetID   string
	ShipmentID      string
	ExpectedVersion int64
	DocumentKind    string
	Templates       []ConfigRef          // CI, PL: 1–2 shipment_document_template revisions of distinct languages
	SequenceRef     *ConfigRef           // CI, PL: a number_sequence revision
	SellerRef       *ConfigRef           // CI, PL: the seller's company_profile revision
	Particulars     *Particulars         // CI, PL
	Packages        []Package            // PL, optional: the packages as stated
	BillOfLading    *BillOfLading        // bill_of_lading
	Certificate     *CertificateOfOrigin // certificate_of_origin
	EvidenceIDs     []string
}

// Acknowledgement accepts the difference at Path for Reason.
type Acknowledgement struct {
	Path   string
	Reason string
}

// IssueShipmentDocuments issues every draft of set DocumentSetID at its row version
// ExpectedVersion. Human only.
type IssueShipmentDocuments struct {
	IdempotencyKey   string
	DocumentSetID    string
	ExpectedVersion  int64
	Acknowledgements []Acknowledgement
}

// RecordSignedShipmentDocument registers EVD file FileID as the signed or stamped copy of the
// Language rendition of version Version of the DocumentKind document of set DocumentSetID. Human
// only.
type RecordSignedShipmentDocument struct {
	IdempotencyKey string
	DocumentSetID  string
	DocumentKind   string
	Version        int64
	Language       string
	FileID         string
	EvidenceIDs    []string
}

func (PrepareShipmentDocument) fulCommand()      {}
func (IssueShipmentDocuments) fulCommand()       {}
func (RecordSignedShipmentDocument) fulCommand() {}

func documentNumber(s string) bool { return Line(s, MaxDocumentNumber) }

func optionalMeasure(s, path string) error {
	if s == "" {
		return nil
	}
	if q, ok := Quantity(s); !ok || q.Sign() <= 0 {
		return schema(path)
	}
	return nil
}

func (p *Particulars) validate() error {
	const at = "/args/particulars"
	if p.Consignee != nil {
		if err := p.Consignee.validate(at + "/consignee"); err != nil {
			return err
		}
	}
	if p.Notify != nil {
		if err := p.Notify.validate(at + "/notify"); err != nil {
			return err
		}
	}
	switch {
	case p.Marks != "" && !Block(p.Marks, MaxMarks):
		return schema(at + "/marks")
	case p.PortOfLoading != "" && !Location(p.PortOfLoading):
		return schema(at + "/port_of_loading")
	case p.PortOfDischarge != "" && !Location(p.PortOfDischarge):
		return schema(at + "/port_of_discharge")
	case len(p.Descriptions) < 1 || len(p.Descriptions) > MaxDescriptions:
		return schema(at + "/descriptions")
	}
	seen := map[[2]string]bool{}
	for i, d := range p.Descriptions {
		dp := index(at+"/descriptions", i)
		switch {
		case !kernel.ValidIdentifier(d.SkuID):
			return schema(dp + "/sku_id")
		case !Language(d.Language):
			return schema(dp + "/language")
		case !Line(d.Text, MaxDescription):
			return schema(dp + "/text")
		case seen[[2]string{d.SkuID, d.Language}]:
			return kernel.Reject(kernel.CodeDuplicateLineID, dp+"/sku_id", "")
		}
		seen[[2]string{d.SkuID, d.Language}] = true
	}
	return nil
}

func (b *BillOfLading) validate() error {
	const at = "/args/bill_of_lading"
	switch {
	case !documentNumber(b.Number):
		return schema(at + "/number")
	case !kernel.ValidIdentifier(b.FileID):
		return schema(at + "/file_id")
	case !Line(b.Vessel, MaxVessel):
		return schema(at + "/vessel")
	case !Line(b.Voyage, MaxVoyage):
		return schema(at + "/voyage")
	case !Location(b.PortOfLoading):
		return schema(at + "/port_of_loading")
	case !Location(b.PortOfDischarge):
		return schema(at + "/port_of_discharge")
	case b.Marks != "" && !Block(b.Marks, MaxMarks):
		return schema(at + "/marks")
	case b.Packages < 1 || b.Packages > MaxCartons*MaxPackages:
		return schema(at + "/packages")
	case len(b.ContainerNumbers) > MaxContainerNumbers:
		return schema(at + "/container_numbers")
	}
	if _, ok := PlannedOn(b.ShippedOnBoard); !ok {
		return schema(at + "/shipped_on_board")
	}
	if err := b.Consignee.validate(at + "/consignee"); err != nil {
		return err
	}
	if b.Notify != nil {
		if err := b.Notify.validate(at + "/notify"); err != nil {
			return err
		}
	}
	if q, ok := Quantity(b.GrossWeightKg); !ok || q.Sign() <= 0 {
		return schema(at + "/gross_weight_kg")
	}
	if err := optionalMeasure(b.VolumeCbm, at+"/volume_cbm"); err != nil {
		return err
	}
	seen := map[string]bool{}
	for i, n := range b.ContainerNumbers {
		if !ContainerNumber(n) || seen[n] {
			return schema(index(at+"/container_numbers", i))
		}
		seen[n] = true
	}
	return nil
}

// ContainerNumber reports whether s is a container number in ISO 6346 form: four upper-case
// letters and seven digits ("MSCU1234565"); the check digit is not computed.
func ContainerNumber(s string) bool {
	if len(s) != 11 {
		return false
	}
	for i := 0; i < 11; i++ {
		c := s[i]
		if (i < 4 && (c < 'A' || c > 'Z')) || (i >= 4 && (c < '0' || c > '9')) {
			return false
		}
	}
	return true
}

func (c *CertificateOfOrigin) validate() error {
	const at = "/args/certificate"
	switch {
	case !documentNumber(c.Number):
		return schema(at + "/number")
	case !kernel.ValidIdentifier(c.FileID):
		return schema(at + "/file_id")
	case !Line(c.Issuer, MaxPartyName):
		return schema(at + "/issuer")
	}
	if _, ok := PlannedOn(c.IssuedOn); !ok {
		return schema(at + "/issued_on")
	}
	if r := c.CertificateRef; r != nil && (!kernel.ValidIdentifier(r.CertificateID) || r.Version < 1 || r.Version > maxVersion) {
		return schema(at + "/certificate_ref")
	}
	return nil
}

// Validate checks the closed contract: the set, shipment and kind; for a CI or PL 1–MaxTemplates
// templates of distinct languages, a number sequence, a seller profile and the particulars — and
// packages only for a PL, in the form of an actual packing (whether they account for the shipment's
// lines is the reconciliation's question); for a B/L or CO its record and nothing else.
func (c PrepareShipmentDocument) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.DocumentSetID):
		return schema("/args/document_set_id")
	case !kernel.ValidIdentifier(c.ShipmentID):
		return schema("/args/shipment_id")
	case c.ExpectedVersion < 0 || c.ExpectedVersion > maxVersion:
		return schema("/args/expected_version")
	case !DocumentKind(c.DocumentKind):
		return schema("/args/document_kind")
	}
	rendered := Rendered(c.DocumentKind)
	switch {
	case rendered && c.BillOfLading != nil:
		return schema("/args/bill_of_lading")
	case rendered && c.Certificate != nil:
		return schema("/args/certificate")
	case !rendered && len(c.Templates) > 0:
		return schema("/args/templates")
	case !rendered && c.SequenceRef != nil:
		return schema("/args/sequence_ref")
	case !rendered && c.SellerRef != nil:
		return schema("/args/seller_ref")
	case !rendered && c.Particulars != nil:
		return schema("/args/particulars")
	case c.DocumentKind != DocumentPackingList && len(c.Packages) > 0:
		return schema("/args/packages")
	case c.DocumentKind == DocumentBillOfLading && c.Certificate != nil:
		return schema("/args/certificate")
	case c.DocumentKind == DocumentCertificateOfOrigin && c.BillOfLading != nil:
		return schema("/args/bill_of_lading")
	}
	switch c.DocumentKind {
	case DocumentBillOfLading:
		if c.BillOfLading == nil {
			return schema("/args/bill_of_lading")
		}
		if err := c.BillOfLading.validate(); err != nil {
			return err
		}
	case DocumentCertificateOfOrigin:
		if c.Certificate == nil {
			return schema("/args/certificate")
		}
		if err := c.Certificate.validate(); err != nil {
			return err
		}
	default:
		if len(c.Templates) < 1 || len(c.Templates) > MaxTemplates {
			return schema("/args/templates")
		}
		for i, t := range c.Templates {
			if err := t.validate(KindShipmentDocumentTemplate, index("/args/templates", i)); err != nil {
				return err
			}
			for _, o := range c.Templates[:i] {
				if o == t {
					return schema(index("/args/templates", i))
				}
			}
		}
		if c.SequenceRef == nil {
			return schema("/args/sequence_ref")
		}
		if err := c.SequenceRef.validate(KindNumberSequence, "/args/sequence_ref"); err != nil {
			return err
		}
		if c.SellerRef == nil {
			return schema("/args/seller_ref")
		}
		if err := c.SellerRef.validate(KindCompanyProfile, "/args/seller_ref"); err != nil {
			return err
		}
		if c.Particulars == nil {
			return schema("/args/particulars")
		}
		if err := c.Particulars.validate(); err != nil {
			return err
		}
		if len(c.Packages) > 0 {
			if err := packages(c.Packages, true); err != nil {
				return err
			}
		}
	}
	return evidence(c.EvidenceIDs, "/args/evidence_ids")
}

// Validate checks the closed contract: at most MaxAcknowledgements acknowledgements of distinct
// paths, each with a reason.
func (c IssueShipmentDocuments) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.DocumentSetID):
		return schema("/args/document_set_id")
	case version(c.ExpectedVersion) != nil:
		return schema("/args/expected_version")
	case len(c.Acknowledgements) > MaxAcknowledgements:
		return schema("/args/acknowledgements")
	}
	seen := map[string]bool{}
	for i, a := range c.Acknowledgements {
		p := index("/args/acknowledgements", i)
		switch {
		case !Line(a.Path, MaxPath) || a.Path[0] != '/':
			return schema(p + "/path")
		case seen[a.Path]:
			return schema(p + "/path")
		case !Reason(a.Reason):
			return schema(p + "/reason")
		}
		seen[a.Path] = true
	}
	return nil
}

// Validate checks the closed contract.
func (c RecordSignedShipmentDocument) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.DocumentSetID):
		return schema("/args/document_set_id")
	case !DocumentKind(c.DocumentKind):
		return schema("/args/document_kind")
	case c.Version < 1 || c.Version > maxVersion:
		return schema("/args/version")
	case !Language(c.Language):
		return schema("/args/language")
	case !kernel.ValidIdentifier(c.FileID):
		return schema("/args/file_id")
	}
	return evidence(c.EvidenceIDs, "/args/evidence_ids")
}

// RenditionReceipt is one language of a rendered version: its file name, SHA-256 and size.
type RenditionReceipt struct {
	Language  string
	FileName  string
	SHA256    string
	SizeBytes int
}

// DocumentReceipt answers shipment_document.prepare: the receipt (ObjectID the set, RowVersion the
// set's, Status draft) and the version prepared — its kind, version, number and renditions.
type DocumentReceipt struct {
	Receipt
	DocumentKind string
	Version      int64
	Number       string
	Renditions   []RenditionReceipt
}

// DocumentRef names a version of a kind.
type DocumentRef struct {
	DocumentKind string
	Version      int64
}

// IssueReceipt answers shipment_document.issue: the receipt (Status issued) and the versions it
// issued, with the SHA-256 of the reconciliation it issued on.
type IssueReceipt struct {
	Receipt
	Issued               []DocumentRef
	ReconciliationSHA256 string
}

// Entry is one named value of a version's or an issue's commit evidence.
type Entry struct {
	Name  string
	Value string
}

// RenditionView is one language of a rendered version: the template revision and content hash it
// was rendered from and the file.
type RenditionView struct {
	Language     string
	Template     ConfigRef
	TemplateHash string
	FileName     string
	MimeType     string
	SHA256       string
	SizeBytes    int
}

// SignedCopyView is the signed or stamped copy of one language of an issued version.
type SignedCopyView struct {
	Language       string
	FileID         string
	FileSHA256     string
	MimeType       string
	DocumentSHA256 string
	EvidenceIDs    []string
	RecordedBy     kernel.ActorRef
	RecordedAt     time.Time
	AuthorityEpoch int64
}

// DocumentVersionView is one version of a kind: its state, number, what it was prepared from — the
// POL revisions with their content hashes, the shipment, packing, booking and order revisions
// (Sources) and the content's SHA-256 — its renditions or, for a B/L or CO, its EVD file, who
// prepared it and its commit evidence, the issue that issued it and its signed copies.
type DocumentVersionView struct {
	DocumentKind    string
	Version         int64
	Status          string
	Number          string
	SetVersion      int64
	Sequence        *ConfigRef
	SequenceHash    string
	Seller          *ConfigRef
	SellerHash      string
	RendererVersion string
	ContentSHA256   string
	Sources         []Entry
	Renditions      []RenditionView
	FileID          string
	FileSHA256      string
	FileMimeType    string
	EvidenceIDs     []string
	PreparedBy      kernel.ActorRef
	PreparedAt      time.Time
	AuthorityEpoch  int64
	Entries         []Entry
	EntriesSHA256   string
	IssueSeq        int64
	IssuedAt        time.Time
	SupersededAt    time.Time
	SignedCopies    []SignedCopyView
}

// DifferenceView is one difference of a reconciliation: the path into the document, the document
// kind and version, the item, the value the document states (Found, "" for none), the value it was
// compared with (Expected) and where that value comes from (Against: shipment_line, order_line,
// actual_packing, booking, order, or another document's kind).
type DifferenceView struct {
	Path     string
	Document string
	Version  int64
	Item     string
	Found    string
	Expected string
	Against  string
}

// ReconciliationView is the reconciliation of a set's current versions, judged at CheckedAt: State
// reconciled when it has no difference, its differences in path order and their SHA-256.
type ReconciliationView struct {
	State       string
	Differences []DifferenceView
	SHA256      string
	CheckedAt   time.Time
}

// AcknowledgementView is a difference an issue accepted, with its reason; FromIssue names the
// earlier issue whose acknowledgement of the very same difference it carried (0: stated).
type AcknowledgementView struct {
	Difference DifferenceView
	Reason     string
	FromIssue  int64
}

// IssueView is one issue of a set: the versions it issued, the differences it accepted, the
// reconciliation's SHA-256, who issued when and its commit evidence.
type IssueView struct {
	Seq                  int64
	SetVersion           int64
	Issued               []DocumentRef
	Acknowledgements     []AcknowledgementView
	ReconciliationSHA256 string
	IssuedBy             kernel.ActorRef
	IssuedAt             time.Time
	AuthorityEpoch       int64
	Entries              []Entry
	EntriesSHA256        string
}

// DocumentSetView is a shipment's document set: its shipment and order binding, its row version,
// every version of every kind (kind order, then version), its issues and its reconciliation now.
type DocumentSetView struct {
	DocumentSetID  string
	ShipmentID     string
	Order          OrderBinding
	RowVersion     int64
	Documents      []DocumentVersionView
	Issues         []IssueView
	Reconciliation ReconciliationView
	CreatedBy      kernel.ActorRef
	CreatedAt      time.Time
	AuthorityEpoch int64
}

// DocumentFile is the rendition of one language of a version with its bytes, verified against the
// stored SHA-256 and size on every read.
type DocumentFile struct {
	DocumentSetID string
	Document      DocumentVersionView
	Language      string
	Content       []byte
}

// DocumentCommands is FUL's command entry point for shipment documents. Errors are
// *kernel.Rejection for refusals.
type DocumentCommands interface {
	PrepareShipmentDocument(ctx context.Context, cmd PrepareShipmentDocument) (DocumentReceipt, error)
	IssueShipmentDocuments(ctx context.Context, cmd IssueShipmentDocuments) (IssueReceipt, error)
	RecordSignedShipmentDocument(ctx context.Context, cmd RecordSignedShipmentDocument) (Receipt, error)
}

// DocumentQueries reads shipment document sets.
type DocumentQueries interface {
	// GetDocumentSet returns a set the caller sees, with its reconciliation now, audited as
	// shipment_document_set.get (ful.read): NOT_FOUND otherwise.
	GetDocumentSet(ctx context.Context, setID string) (DocumentSetView, error)
	// GetShipmentDocument returns the rendition of language of version version of kind, audited as
	// shipment_document.get (ful.read): NOT_FOUND for none; DOCUMENT_INTEGRITY_FAILURE for bytes that
	// are not the stored ones.
	GetShipmentDocument(ctx context.Context, setID, kind string, version int64, language string) (DocumentFile, error)
}

// BillOfLadingRef names the issued bill of lading record of a shipment's document set that
// document_release.record released the documents on: the set, the record's version, its number and
// the SHA-256 of its content.
type BillOfLadingRef struct {
	DocumentSetID string
	Version       int64
	Number        string
	ContentSHA256 string
}
