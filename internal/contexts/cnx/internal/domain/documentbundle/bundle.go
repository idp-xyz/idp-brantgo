// Package documentbundle holds CNX's DocumentBundle (tactical model CNX-AGG-DocumentBundle with its
// RequirementCheck and ExternalReconciliation entities, CNX-INV-008; baseline 22.1
// CNDocumentBundle/RequirementCheck and ExternalReconciliation/AdjustmentCase, CN-R11, CN-R13; UL
// CNX-DocumentBundle, CNX-ExternalReconciliation; WBS S5-05a; CN-AT14 bundle half): the catalogue of
// what an export lot's file must hold — each requirement's kind, version, status, owner and due date
// — and the reconciliations of external results with internal facts.
//
// The bundle is the root, one per export lot. Every check appends the whole checklist as stated
// then; nothing is overwritten. Presence is not validity (附件存在不等于真实性或法定效力已核实): a
// requirement with an attachment is present, and counts as met only once a human verified it on a
// stated basis; a bundle is complete only when every requirement is verified. Due dates are stated
// by the checker from the applicable policy, never derived from an upload date (CN-R13). Fund
// records (STL FundRecordRef v1, R38) are linked to the bundle as evidence of receipts only: STL's
// applications are a commercial allocation, never 出口收汇核销 (CN-R11).
package documentbundle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// CodeDocumentRequirementUnverified is the domain's own code (error catalog V0.1, 409).
const CodeDocumentRequirementUnverified = "DOCUMENT_REQUIREMENT_UNVERIFIED"

// Categories are the categories of the domain's own codes.
var Categories = map[string]kernel.Category{CodeDocumentRequirementUnverified: kernel.CategoryConflict}

func reject(code, path, reason string) *kernel.Rejection {
	return kernel.RejectAs(code, Categories[code], path, reason)
}

// Requirement kinds, statuses, bundle states and the label of fund record links.
const (
	KindCommercialInvoice     = "commercial_invoice"
	KindPackingList           = "packing_list"
	KindBillOfLading          = "bill_of_lading"
	KindCustomsDeclaration    = "customs_declaration"
	KindCertificateOfOrigin   = "certificate_of_origin"
	KindInsurance             = "insurance"
	KindInspectionCertificate = "inspection_certificate"
	KindExportLicense         = "export_license"
	KindPurchaseInvoice       = "purchase_invoice"
	KindTaxRefund             = "tax_refund"
	KindFundRecord            = "fund_record"
	KindOther                 = "other"

	Missing  = "missing"
	Present  = "present"
	Verified = "verified"

	Incomplete = "incomplete"
	Complete   = "complete"

	// CommercialAllocation labels STL's fund records and their applications in a bundle (CN-R11):
	// a commercial allocation, never an administrative 出口收汇核销.
	CommercialAllocation = "commercial_allocation"
)

// Kinds are the requirement kinds a bundle lists.
var Kinds = []string{KindCommercialInvoice, KindPackingList, KindBillOfLading, KindCustomsDeclaration, KindCertificateOfOrigin, KindInsurance,
	KindInspectionCertificate, KindExportLicense, KindPurchaseInvoice, KindTaxRefund, KindFundRecord, KindOther}

// Bundle is the DocumentBundle root.
type Bundle struct {
	ID              string
	ExportLotID     string
	OrderID         string
	Status          string
	Checks          int
	Reconciliations int
	RowVersion      int64
	OpenedBy        kernel.ActorRef
	Epoch           int64
	OpenedAt        time.Time
}

// CheckVersion compares the caller's expected row version with b's (0 for a bundle not opened).
func CheckVersion(b *Bundle, expected int64) error {
	if (b == nil && expected != 0) || (b != nil && b.RowVersion != expected) {
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	return nil
}

// Evidence is an EVD source and its content hash.
type Evidence struct {
	SourceID      string `json:"source_id"`
	ContentSHA256 string `json:"content_sha256"`
}

// Owner is who answers for a requirement: an actor, a PTY party or a role.
type Owner struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// DeclarationRef is the declaration a customs_declaration requirement rests on, as CNX holds it.
type DeclarationRef struct {
	DeclarationID     string `json:"declaration_id"`
	Status            string `json:"status"`
	DeclarationNumber string `json:"declaration_number"`
	ExportedAt        string `json:"exported_at"`
	Exported          bool   `json:"customs_released"`
}

// FundRef is an STL fund record linked to a fund_record requirement (FundRecordRef v1, R38): its kind,
// direction, currency and what it received, labelled commercial_allocation.
type FundRef struct {
	FundRecordID  string `json:"fund_record_id"`
	Kind          string `json:"kind"`
	Direction     string `json:"direction"`
	Currency      string `json:"currency"`
	ReceivedMinor string `json:"received_minor"`
	Label         string `json:"label"`
}

// Requirement is one entry of a checklist as a check states it.
type Requirement struct {
	RequirementID string           `json:"requirement_id"`
	Kind          string           `json:"kind"`
	Version       string           `json:"version"`
	Status        string           `json:"status"`
	Owner         Owner            `json:"owner"`
	DueOn         string           `json:"due_on"`
	Evidence      []Evidence       `json:"evidence"`
	Declaration   *DeclarationRef  `json:"declaration"`
	FundRecords   []FundRef        `json:"fund_records"`
	Basis         string           `json:"basis"`
	Note          string           `json:"note"`
	Certificates  []CertificateRef `json:"certificates,omitempty"` // S5 C5: certificate results named (certificate.go)
}

// CheckInput is a check of a bundle: the requirements as stated, whether the checker states the
// bundle complete, whether the checker is a human, and the lot's line IDs (S5 C5: what certificates
// must cover).
type CheckInput struct {
	Requirements   []Requirement
	ExpectComplete bool
	Human          bool
	Lines          []string
}

// Counts are a checklist's requirements by status.
type Counts struct {
	Missing, Present, Verified int
	MissingIDs, PresentIDs     []string
}

// Check decides a checklist (CNX-INV-008, CN-AT14), in this order for each requirement: a verified
// one stated by an AI agent (PERMISSION_DENIED: only a human verifies); a customs declaration stated
// verified whose declaration customs has not released (DOCUMENT_REQUIREMENT_UNVERIFIED); a verified
// certificate requirement without certificate results or not covering every lot line
// (checkCertificates, S5 C5); before them, more than one certificate of origin or origin declaration named
// (checkOneOrigin, S5R-D12: INVALID_KNOWN_VALUE); then, when
// the checker states the bundle complete, the first requirement that is only present or missing
// (DOCUMENT_REQUIREMENT_UNVERIFIED: an attachment is not a verification). It returns the counts.
func Check(in CheckInput) (Counts, error) {
	var n Counts
	if err := checkOneOrigin(in.Requirements); err != nil {
		return n, err
	}
	for i, r := range in.Requirements {
		path := "/args/requirements/" + strconv.Itoa(i) + "/status"
		if r.Status == Verified && !in.Human {
			return n, kernel.Reject(kernel.CodePermissionDenied, path, "Only a human verifies a document requirement")
		}
		if r.Status == Verified && r.Kind == KindCustomsDeclaration && (r.Declaration == nil || !r.Declaration.Exported) {
			return n, reject(CodeDocumentRequirementUnverified, path, "Customs has not released the declaration")
		}
		if err := checkCertificates(r, i, in.Lines); err != nil {
			return n, err
		}
		switch r.Status {
		case Missing:
			n.Missing++
			n.MissingIDs = append(n.MissingIDs, r.RequirementID)
		case Present:
			n.Present++
			n.PresentIDs = append(n.PresentIDs, r.RequirementID)
		default:
			n.Verified++
		}
	}
	if in.ExpectComplete {
		for i, r := range in.Requirements {
			if r.Status != Verified {
				return n, reject(CodeDocumentRequirementUnverified, "/args/requirements/"+strconv.Itoa(i)+"/status",
					"Requirement "+r.RequirementID+" is "+r.Status+", not verified: the bundle is not complete")
			}
		}
	}
	return n, nil
}

// Status is the bundle's state after a check with counts n.
func (n Counts) Status() string {
	if n.Missing == 0 && n.Present == 0 && n.Verified > 0 {
		return Complete
	}
	return Incomplete
}

// CheckContract names the check document's layout.
const CheckContract = "brant.china-export.document-bundle-check/0.1.0"

// CheckDocument is a check as stored: the whole checklist, its counts, by whom and when.
type CheckDocument struct {
	Contract       string        `json:"contract"`
	BundleID       string        `json:"bundle_id"`
	CheckNo        int           `json:"check_no"`
	ExportLotID    string        `json:"export_lot_id"`
	LotVersion     int64         `json:"lot_version"`
	OrderID        string        `json:"order_id"`
	Requirements   []Requirement `json:"requirements"`
	Status         string        `json:"status"`
	Missing        int           `json:"missing"`
	Present        int           `json:"present"`
	Verified       int           `json:"verified"`
	CheckedBy      Owner         `json:"checked_by"`
	AuthorityEpoch int64         `json:"authority_epoch"`
	CheckedAt      string        `json:"checked_at"`
}

// Sealed is a stored check: its document, bytes and SHA-256.
type Sealed struct {
	CheckDocument
	Bytes  []byte
	SHA256 string
}

// Seal stores d as canonical JSON with its SHA-256.
func Seal(d CheckDocument) (*Sealed, error) {
	d.Contract = CheckContract
	for i := range d.Requirements {
		if d.Requirements[i].Evidence == nil {
			d.Requirements[i].Evidence = []Evidence{}
		}
		if d.Requirements[i].FundRecords == nil {
			d.Requirements[i].FundRecords = []FundRef{}
		}
	}
	b, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	return &Sealed{CheckDocument: d, Bytes: b, SHA256: hex.EncodeToString(sum[:])}, nil
}

// Unseal reads a stored check; false when it does not match its SHA-256.
func Unseal(b []byte, hash string) (*Sealed, bool, error) {
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != hash {
		return nil, false, nil
	}
	var d CheckDocument
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, false, err
	}
	return &Sealed{CheckDocument: d, Bytes: b, SHA256: hash}, true, nil
}

// Reconciliation subjects, measures, responsibilities and adjustments.
const (
	SubjectExportLot     = "export_lot"
	SubjectDeclaration   = "declaration"
	SubjectTaxRefundCase = "tax_refund_case"
	SubjectFundRecord    = "fund_record"
	SubjectDocument      = "document"

	MeasureAmount   = "amount"
	MeasureQuantity = "quantity"

	AdjustmentNone     = "none"
	AdjustmentApproved = "approved"
)

// Responsibilities are who a difference is attributed to.
var Responsibilities = []string{"none", "internal", "customer", "supplier", "broker", "carrier", "authority", "bank", "other"}

// Reconciliation is an external reconciliation (ExternalReconciliation/AdjustmentCase): the external
// value and where it comes from, the internal value and where it comes from, their exact difference
// (external − internal), who answers for it, and whether an adjustment was approved.
type Reconciliation struct {
	ID                    string
	BundleID              string
	No                    int
	SubjectKind           string
	SubjectID             string
	Measure               string
	Unit                  string // currency code or unit code
	External              kernel.Decimal
	Internal              kernel.Decimal
	Difference            kernel.Decimal
	ExternalSource        string
	InternalSource        string
	ResponsibilityKind    string
	ResponsibilityPartyID string
	ResponsibilityNote    string
	Adjustment            string
	AdjustmentNote        string
	Evidence              []Evidence
	Reason                string
	By                    kernel.ActorRef
	Epoch                 int64
	At                    time.Time
	// SubjectVersion and SubjectDetail: of a credit_note subject, the credit note's version and what STL stated of
	// it (canonical JSON) when the reconciliation was recorded (S5 C10).
	SubjectVersion int64
	SubjectDetail  string
}

// Reconcile decides a reconciliation: the difference is external − internal, exactly; an approved
// adjustment needs a difference to adjust (INVALID_KNOWN_VALUE otherwise). Brant records the
// approval a person gives; it computes no tax, duty or refund (CN-E01).
func Reconcile(r *Reconciliation) error {
	r.Difference = r.External.Sub(r.Internal)
	if r.Adjustment == AdjustmentApproved && r.Difference.IsZero() {
		return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/adjustment/kind", "There is no difference to adjust")
	}
	return nil
}
