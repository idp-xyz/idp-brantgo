package api

import (
	"context"
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Customs declarations (WBS S5-05a; tactical model CNX-AGG-DeclarationCase with DeclarationVersion,
// DeclarationItemMapping, DeclarationSubmission and DeclarationReceipt, CNX-INV-003, CNX-INV-006,
// SM-CNX-DECLARATION; baseline 22.1, 22.2, 23.3, CN-R05, CN-R07; CN-AT02, CN-AT03, CN-AT06,
// CN-AT13; S5 decisions E-1: manual paths only, D-17). Brant organises the declaration of one FUL
// export lot and records what happens outside it; the declaration is filed by a broker or our own
// clerk, and customs is the authority on its result.
//
//	declaration_case.prepare        human or AI (documentation): maps every line of the export lot
//	                                export_lot_id (FUL ExportLotRef v1, R36; registered, not
//	                                cancelled) into items, each item naming its lot lines and, for a
//	                                line not in the SKU's base unit PCS, the POL unit_conversion
//	                                revision that converts it (受控换算); the classification comes from
//	                                each line's CAT declaration profile verified for code_table_version
//	                                (DeclarationProfileRef v1, R39; DECLARATION_PROFILE_UNVERIFIED
//	                                otherwise) and lines of different codes, names, elements or legal
//	                                units are never merged; the order is TRD's at the lot's revision
//	                                (SalesOrderRef v1 with its S/C and PI, R46; REFERENCE_STALE once
//	                                TRD moved to another revision). Without declaration_id it opens the
//	                                lot's case (the next attempt once the last was rejected or
//	                                withdrawn); with it, it revises the draft (a new version).
//	declaration_case.confirm        human only: confirms the draft's current version, whose every item
//	                                has a verified mapping (DECLARATION_MAPPING_UNVERIFIED otherwise,
//	                                CN-AT03) on profiles still verified now (REFERENCE_STALE otherwise).
//	                                The confirmed version is the data handed over for submission.
//	declaration_case.record_submission  human only: the manual submission by a broker (PTY party and the
//	                                verified service role customs_broker it rests on, ServiceRoleRef v1 on
//	                                R77, S5R-D36) or our own clerk, under operation_reference, the business
//	                                operation's identifier (unique). While the last submission's result is
//	                                unknown nothing is submitted again: EXTERNAL_RESULT_UNKNOWN (CN-AT13).
//	declaration_case.record_receipt human only (D-17): an external result — outcome received,
//	                                accepted, released, rejected, amended or withdrawn, with its
//	                                original text, the external status code, the version of the
//	                                external status mapping applied (mapping_version), its external
//	                                sequence (the external version or business order) and the
//	                                declaration number — or unknown (a timeout or an unclear result,
//	                                with a reason, no external facts) — or, after unknown, not_received
//	                                (S5R-D13: the verification found customs never received it, by the
//	                                status customs' own query shows or the broker's written confirmation;
//	                                with a reason and its evidence, no external facts; final, the lot's
//	                                next attempt may then be prepared). A receipt not after the result
//	                                recorded never overrides it: DECLARATION_RECEIPT_STALE (CN-AT06).
//	                                A release states the export date customs gives (exported_at, the
//	                                出口日期 a tax refund case binds its policy at), an amendment may
//	                                restate it and the items it changed (amended_items, base
//	                                quantities: a new version); a withdrawal clears it. A customs
//	                                release is not a departure nor a delivery (CN-R07).
//
// Request members in snake_case: declaration_case.prepare {declaration_id, expected_version,
// export_lot_id, code_table_version, items [{item_no, lines [{line_id, unit_conversion {id,
// revision}}]}]}; declaration_case.confirm {declaration_id, expected_version, version, evidence_ids,
// reason}; declaration_case.record_submission {declaration_id, expected_version, operation_reference,
// submitter {kind, party_id, service_role {id, revision}, name}, submitted_at, evidence_ids, note};
// declaration_case.record_receipt {declaration_id, expected_version, receipt_id, outcome, external
// {sequence, status_code, original_text, received_at}, mapping_version, declaration_number,
// exported_at, amended_items [{item_no, quantity}], evidence_ids, reason}.
const (
	CommandPrepareDeclaration = "declaration_case.prepare"
	CommandConfirmDeclaration = "declaration_case.confirm"
	CommandRecordSubmission   = "declaration_case.record_submission"
	CommandRecordReceipt      = "declaration_case.record_receipt"

	// Reads: a declaration case with its versions and entries, the company's cases.
	CommandGetDeclaration = "declaration_case.get"
	CommandDeclarations   = "declaration_case.list"

	// DeclarationReceiptRecorded (Context Map CNX facts): every recorded external result.
	EventDeclarationReceiptRecorded = "idp-brant.cnx.declaration_receipt_recorded.v1"
)

// States of a declaration case (SM-CNX-DECLARATION).
const (
	DeclarationDraft     = "draft"
	DeclarationConfirmed = "confirmed"
	DeclarationSubmitted = "submitted"
	DeclarationUnknown   = "unknown"
	DeclarationReceived  = "received"
	DeclarationAccepted  = "accepted"
	DeclarationReleased  = "released"
	DeclarationRejected  = "rejected"
	DeclarationAmended   = "amended"
	DeclarationWithdrawn = "withdrawn"
	// DeclarationNotReceived (S5R-D13): after an unknown result, customs never received the declaration. Final.
	DeclarationNotReceived = "not_received"
)

// Mapping states, submitters and the base unit of declarations.
const (
	MappingVerified   = "verified"
	MappingUnverified = "unverified"

	SubmitterBroker   = "broker"
	SubmitterOwnClerk = "own_clerk"

	DeclarationBaseUnit = "PCS"
)

// Limits of the declaration contract.
const (
	MaxDeclarationItems = 50
	MaxDeclarationLines = 200
	MaxExternalStatus   = 64
	MaxOriginalText     = 4000
	MaxSubmitterName    = 200
)

var receiptOutcomes = []string{DeclarationUnknown, DeclarationReceived, DeclarationAccepted, DeclarationReleased, DeclarationRejected,
	DeclarationAmended, DeclarationWithdrawn, DeclarationNotReceived}

// UnitConversionRef is a POL unit_conversion revision.
type UnitConversionRef struct {
	ID       string
	Revision int64
}

// DeclarationLine is a lot line an item declares, with the conversion of its unit into the base
// unit when it is not in it.
type DeclarationLine struct {
	LineID         string
	UnitConversion *UnitConversionRef
}

// DeclarationItem is an item (报关商品项) and the lot lines it declares.
type DeclarationItem struct {
	ItemNo int
	Lines  []DeclarationLine
}

// PrepareDeclaration prepares the declaration of export lot ExportLotID (a new case: DeclarationID
// "" and ExpectedVersion 0) or revises the draft DeclarationID at ExpectedVersion. CustomsOffice is
// the customs office it is declared at (申报地海关, optional): S5R-D04 (一证一关) refuses one other
// than the customs office of the export licence covering the lot (INVALID_KNOWN_VALUE), at
// preparation and at confirmation.
type PrepareDeclaration struct {
	IdempotencyKey   string
	DeclarationID    string
	ExpectedVersion  int64
	ExportLotID      string
	CodeTableVersion string
	Items            []DeclarationItem
	CustomsOffice    string `json:",omitempty"`
}

// Validate checks the closed contract: items numbered 1, 2, …; each lot line in one item only
// (DUPLICATE_LINE_ID).
func (c PrepareDeclaration) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case c.DeclarationID != "" && !kernel.ValidIdentifier(c.DeclarationID):
		return schema("/args/declaration_id")
	case c.ExpectedVersion < 0 || c.ExpectedVersion > maxVersion || (c.DeclarationID == "") != (c.ExpectedVersion == 0):
		return schema("/args/expected_version")
	case !kernel.ValidIdentifier(c.ExportLotID):
		return schema("/args/export_lot_id")
	case !kernel.ValidIdentifier(c.CodeTableVersion):
		return schema("/args/code_table_version")
	case len(c.Items) < 1 || len(c.Items) > MaxDeclarationItems:
		return schema("/args/items")
	case c.CustomsOffice != "" && !CustomsOffice(c.CustomsOffice):
		return schema("/args/customs_office")
	}
	seen, n := map[string]bool{}, 0
	for i, it := range c.Items {
		p := "/args/items/" + strconv.Itoa(i)
		if it.ItemNo != i+1 {
			return schema(p + "/item_no")
		}
		if len(it.Lines) < 1 {
			return schema(p + "/lines")
		}
		for j, l := range it.Lines {
			lp := p + "/lines/" + strconv.Itoa(j)
			n++
			switch {
			case n > MaxDeclarationLines:
				return schema(p + "/lines")
			case !kernel.ValidIdentifier(l.LineID):
				return schema(lp + "/line_id")
			case seen[l.LineID]:
				return kernel.Reject(kernel.CodeDuplicateLineID, lp+"/line_id", "")
			}
			seen[l.LineID] = true
			if u := l.UnitConversion; u != nil {
				if !kernel.ValidIdentifier(u.ID) {
					return schema(lp + "/unit_conversion/id")
				}
				if u.Revision < 1 || u.Revision > maxVersion {
					return schema(lp + "/unit_conversion/revision")
				}
			}
		}
	}
	return nil
}

// ConfirmDeclaration confirms version Version, the draft's current one, of DeclarationID at
// ExpectedVersion (human only), with optional evidence and reason.
type ConfirmDeclaration struct {
	IdempotencyKey  string
	DeclarationID   string
	ExpectedVersion int64
	Version         int
	EvidenceIDs     []string
	Reason          string
}

// Validate checks the closed contract.
func (c ConfirmDeclaration) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.DeclarationID):
		return schema("/args/declaration_id")
	case c.ExpectedVersion < 1 || c.ExpectedVersion > maxVersion:
		return schema("/args/expected_version")
	case c.Version < 1 || c.Version > maxVersion:
		return schema("/args/version")
	case c.Reason != "" && !Text(c.Reason, MaxReason):
		return schema("/args/reason")
	}
	return evidence(c.EvidenceIDs, "/args/evidence_ids", 0)
}

// Submitter is who filed the declaration: a broker (报关行, its PTY party, the verified PTY service role of kind
// customs_broker it rests on — ServiceRoleRef v1, R77, S5R-D36 — and its name) or our own clerk (name only).
type Submitter struct {
	Kind        string
	PartyID     string
	ServiceRole *ServiceRoleRef `json:",omitempty"`
	Name        string
}

// ServiceRoleRef names a revision of a PTY service role of the submitter's party (PTY ServiceRoleRef v1).
type ServiceRoleRef struct {
	ID       string
	Revision int64
}

// RecordSubmission records the manual submission of DeclarationID's confirmed version at
// ExpectedVersion (human only): the business operation's identifier OperationReference, the
// Submitter, when it was submitted and the evidence of the submission.
type RecordSubmission struct {
	IdempotencyKey     string
	DeclarationID      string
	ExpectedVersion    int64
	OperationReference string
	Submitter          Submitter
	SubmittedAt        string
	EvidenceIDs        []string
	Note               string
}

// Validate checks the closed contract.
func (c RecordSubmission) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.DeclarationID):
		return schema("/args/declaration_id")
	case c.ExpectedVersion < 1 || c.ExpectedVersion > maxVersion:
		return schema("/args/expected_version")
	case !kernel.ValidIdentifier(c.OperationReference):
		return schema("/args/operation_reference")
	case c.Submitter.Kind != SubmitterBroker && c.Submitter.Kind != SubmitterOwnClerk:
		return schema("/args/submitter/kind")
	case (c.Submitter.Kind == SubmitterBroker) != (c.Submitter.PartyID != ""),
		c.Submitter.PartyID != "" && !kernel.ValidIdentifier(c.Submitter.PartyID):
		return schema("/args/submitter/party_id")
	case (c.Submitter.Kind == SubmitterBroker) != (c.Submitter.ServiceRole != nil):
		return schema("/args/submitter/service_role")
	case c.Submitter.ServiceRole != nil && !kernel.ValidIdentifier(c.Submitter.ServiceRole.ID):
		return schema("/args/submitter/service_role/id")
	case c.Submitter.ServiceRole != nil && (c.Submitter.ServiceRole.Revision < 1 || c.Submitter.ServiceRole.Revision > maxVersion):
		return schema("/args/submitter/service_role/revision")
	case !Text(c.Submitter.Name, MaxSubmitterName):
		return schema("/args/submitter/name")
	case !timestamp(c.SubmittedAt):
		return schema("/args/submitted_at")
	case c.Note != "" && !Text(c.Note, MaxReason):
		return schema("/args/note")
	}
	return evidence(c.EvidenceIDs, "/args/evidence_ids", 0)
}

// ExternalReceipt is an external result as customs or the broker gave it: the external version or
// business order (Sequence, positive), the external status code and the original text as received,
// and when it was received.
type ExternalReceipt struct {
	Sequence     int64
	StatusCode   string
	OriginalText string
	ReceivedAt   string
}

// AmendedItem is an item an amendment changed and its base quantity (PCS) after it.
type AmendedItem struct {
	ItemNo   int
	Quantity string
}

// RecordReceipt records, as ReceiptID, an external result of DeclarationID at ExpectedVersion (human
// only). Outcome unknown names no External, MappingVersion or DeclarationNumber and needs a Reason; so does
// not_received, whose evidence the domain requires (INVALID_KNOWN_VALUE without: CNX-INV-003, S5R-D13); every
// other outcome names them and at least one evidence source. ExportedAt (a UTC timestamp) is
// required for released, optional for amended and absent otherwise; AmendedItems only for amended.
type RecordReceipt struct {
	IdempotencyKey    string
	DeclarationID     string
	ExpectedVersion   int64
	ReceiptID         string
	Outcome           string
	External          *ExternalReceipt
	MappingVersion    string
	DeclarationNumber string
	ExportedAt        string
	AmendedItems      []AmendedItem
	EvidenceIDs       []string
	Reason            string
}

// Validate checks the closed contract.
func (c RecordReceipt) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.DeclarationID):
		return schema("/args/declaration_id")
	case c.ExpectedVersion < 1 || c.ExpectedVersion > maxVersion:
		return schema("/args/expected_version")
	case !kernel.ValidIdentifier(c.ReceiptID):
		return schema("/args/receipt_id")
	case !contains(receiptOutcomes, c.Outcome):
		return schema("/args/outcome")
	case c.Reason != "" && !Text(c.Reason, MaxReason):
		return schema("/args/reason")
	}
	unknown := c.Outcome == DeclarationUnknown || c.Outcome == DeclarationNotReceived
	if unknown {
		switch {
		case c.External != nil:
			return schema("/args/external")
		case c.MappingVersion != "":
			return schema("/args/mapping_version")
		case c.DeclarationNumber != "":
			return schema("/args/declaration_number")
		case c.Reason == "":
			return schema("/args/reason")
		}
	} else {
		x := c.External
		switch {
		case x == nil:
			return schema("/args/external")
		case x.Sequence < 1:
			return schema("/args/external/sequence")
		case !Text(x.StatusCode, MaxExternalStatus):
			return schema("/args/external/status_code")
		case !Text(x.OriginalText, MaxOriginalText):
			return schema("/args/external/original_text")
		case !timestamp(x.ReceivedAt):
			return schema("/args/external/received_at")
		case !kernel.ValidIdentifier(c.MappingVersion):
			return schema("/args/mapping_version")
		case !Text(c.DeclarationNumber, MaxDeclarationRef):
			return schema("/args/declaration_number")
		}
	}
	switch {
	case c.Outcome == DeclarationReleased && c.ExportedAt == "",
		c.ExportedAt != "" && (c.Outcome != DeclarationReleased && c.Outcome != DeclarationAmended || !timestamp(c.ExportedAt)):
		return schema("/args/exported_at")
	case len(c.AmendedItems) > 0 && c.Outcome != DeclarationAmended, len(c.AmendedItems) > MaxDeclarationItems:
		return schema("/args/amended_items")
	}
	items := map[int]bool{}
	for i, a := range c.AmendedItems {
		p := "/args/amended_items/" + strconv.Itoa(i)
		if a.ItemNo < 1 || a.ItemNo > MaxDeclarationItems || items[a.ItemNo] {
			return schema(p + "/item_no")
		}
		items[a.ItemNo] = true
		if !positive(a.Quantity, kernel.QuantityFormat, true) {
			return schema(p + "/quantity")
		}
	}
	min := 1
	if unknown {
		min = 0
	}
	return evidence(c.EvidenceIDs, "/args/evidence_ids", min)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func (PrepareDeclaration) cnxCommand() {}
func (ConfirmDeclaration) cnxCommand() {}
func (RecordSubmission) cnxCommand()   {}
func (RecordReceipt) cnxCommand()      {}

// DeclarationReceipt is the answer of the declaration commands: the receipt (ObjectID the
// declaration ID, Status its state) and the case as stored now — its lot and attempt, its current
// version and mapping, the declaration number and export date once recorded — with the receipt ID
// of record_receipt.
type DeclarationReceipt struct {
	Receipt
	DeclarationID     string
	ExportLotID       string
	Attempt           int
	Version           int
	Mapping           string
	DeclarationNumber string
	ExportedAt        time.Time
	ReceiptID         string
}

// DeclarationCommands is CNX's command entry point for declarations.
type DeclarationCommands interface {
	PrepareDeclaration(ctx context.Context, cmd PrepareDeclaration) (DeclarationReceipt, error)
	ConfirmDeclaration(ctx context.Context, cmd ConfirmDeclaration) (DeclarationReceipt, error)
	RecordSubmission(ctx context.Context, cmd RecordSubmission) (DeclarationReceipt, error)
	RecordReceipt(ctx context.Context, cmd RecordReceipt) (DeclarationReceipt, error)
}

// ElementView is a declaration element.
type ElementView struct {
	Name  string
	Value string
}

// LegalQuantityView is a quantity in a legal unit; on a line with the conversion factor from the
// base unit and its basis as the verified profile states them.
type LegalQuantityView struct {
	Unit        string
	Quantity    string
	PerBaseUnit string
	Basis       string
}

// ConversionView is how a line's quantity reached the base unit: base_unit, unit_conversion (the
// POL revision, its content hash and factor) or none (the mapping is unverified).
type ConversionView struct {
	Source      string
	ConfigID    string
	Revision    int64
	ContentHash string
	Factor      string
}

// DeclarationLineView is a lot line of an item: the shipment and order line it comes from, its SKU
// specification, quantity, unit and amount, the profile revision it rests on, the conversion, its
// base and legal quantities and its mapping (with the reason when unverified).
type DeclarationLineView struct {
	LineID          string
	ShipmentID      string
	OrderLineID     string
	SkuID           string
	SpecRevision    int64
	Quantity        string
	Unit            string
	AmountMinor     string
	ProfileID       string
	ProfileRevision int64
	ProfileHash     string
	Conversion      ConversionView
	BaseQuantity    string
	LegalQuantities []LegalQuantityView
	Mapping         string
	Reason          string
}

// DeclarationItemView is a declaration item.
type DeclarationItemView struct {
	ItemNo          int
	CommodityCode   string
	DeclarationName string
	Elements        []ElementView
	BaseUnit        string
	BaseQuantity    string
	LegalQuantities []LegalQuantityView
	CurrencyCode    string
	ValueMinor      string
	Mapping         string
	Lines           []DeclarationLineView
}

// SalesDocumentView is a sales contract or proforma invoice of the order as TRD showed it.
type SalesDocumentView struct {
	Kind           string
	Number         string
	OrderRevision  int64
	DocumentSHA256 string
	Current        bool
	Signatures     int
}

// AmendmentView is an item an amendment changed: its base quantity before and after.
type AmendmentView struct {
	ItemNo int
	Before string
	After  string
}

// DeclarationVersionView is a version of the declared data: prepared (by a preparation) or amended
// (by an amendment receipt), its SHA-256, the lot and order as read, the items and the mapping.
type DeclarationVersionView struct {
	Version        int
	Kind           string
	DocumentSHA256 string
	ExportLotID    string
	LotVersion     int64
	ShipmentID     string
	OrderID        string
	OrderRevision  int64
	OrderHash      string
	OrderStatus    string
	SalesDocuments []SalesDocumentView
	LotCurrency    string
	LotValueMinor  string
	Mapping        string
	Items          []DeclarationItemView
	Amendments     []AmendmentView
	ReceiptID      string
	PreparedBy     kernel.ActorRef
	AuthorityEpoch int64
	PreparedAt     time.Time
	CustomsOffice  string `json:",omitempty"` // S5R-D04: the customs office declared at, "" when not stated
}

// ConfirmationEntryView is the confirmation of a version.
type ConfirmationEntryView struct {
	Version        int
	Evidence       []EvidenceView
	Reason         string
	ConfirmedBy    kernel.ActorRef
	AuthorityEpoch int64
	ConfirmedAt    time.Time
}

// SubmissionView is a manual submission; ServiceRoleKind is the kind of the broker's PTY service role (S5R-D36).
type SubmissionView struct {
	SubmissionNo       int
	Version            int
	OperationReference string
	Submitter          Submitter
	ServiceRoleKind    string
	SubmittedAt        time.Time
	Evidence           []EvidenceView
	Note               string
	RecordedBy         kernel.ActorRef
	AuthorityEpoch     int64
	RecordedAt         time.Time
}

// ReceiptView is a recorded external result as it was given: outcome and the state it followed,
// the external sequence, status code and original text, the mapping version, when it was received,
// the declaration number, the export date, the amendment version and the evidence.
type ReceiptView struct {
	ReceiptID         string
	ReceiptNo         int
	Outcome           string
	From              string
	Sequence          int64
	StatusCode        string
	OriginalText      string
	MappingVersion    string
	ReceivedAt        time.Time
	DeclarationNumber string
	ExportedAt        time.Time
	Version           int
	Evidence          []EvidenceView
	Reason            string
	RecordedBy        kernel.ActorRef
	AuthorityEpoch    int64
	RecordedAt        time.Time
}

// DeclarationView is a declaration case as the reads show it. CustomsReleased is whether customs
// released the goods (a released or amended case): it is not a departure nor a delivery, which are
// FUL's milestones (CN-R07). ExportedAt is the export date customs states (zero: none).
type DeclarationView struct {
	DeclarationID     string
	ExportLotID       string
	Attempt           int
	ShipmentID        string
	OrderID           string
	OrderRevision     int64
	OrderHash         string
	CodeTableVersion  string
	Status            string
	RowVersion        int64
	CurrentVersion    int
	Mapping           string
	DeclarationNumber string
	CustomsReleased   bool
	ExportedAt        time.Time
	LastSequence      int64
	Versions          []DeclarationVersionView
	Confirmations     []ConfirmationEntryView
	Submissions       []SubmissionView
	Receipts          []ReceiptView
	OpenedBy          kernel.ActorRef
	AuthorityEpoch    int64
	OpenedAt          time.Time
}

// DeclarationSummary is one case of the company's.
type DeclarationSummary struct {
	DeclarationID     string
	ExportLotID       string
	Attempt           int
	Status            string
	RowVersion        int64
	CurrentVersion    int
	Mapping           string
	DeclarationNumber string
	ExportedAt        time.Time
	OpenedAt          time.Time
}

// DeclarationQueries is the audited read of declarations (cnx.read).
type DeclarationQueries interface {
	// GetDeclaration returns a case, audited as declaration_case.get: NOT_FOUND otherwise; a version
	// document that does not match its hash is REFERENCE_INVALID.
	GetDeclaration(ctx context.Context, declarationID string) (DeclarationView, error)
	// Declarations returns the company's cases, newest first, audited as declaration_case.list.
	Declarations(ctx context.Context) ([]DeclarationSummary, error)
}
