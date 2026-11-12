// Package api is CNX's published contract (China export professional collaboration, CN-M01). S5's
// first CNX unit delivers the trade compliance check of a sales order revision (WBS S5-13; tactical
// model CNX-AGG-TradeComplianceCheck, SM-CNX-COMPLIANCE, CNX-INV-001; TRD-INV-023; ADR-BR-014,
// ADR-BR-017; S5 decisions D-8, D-9): country embargoes, sanctions programmes and export control.
// Declarations, purchase invoices, tax refund cases, document bundles, export licences and
// certificates come with later S5 units. Brant records the check and the conclusion an authorized
// compliance officer gives; it gives no legal opinion, and AI or list services only produce
// candidates.
//
//	trade_compliance.evaluate  human, AI or system (compliance template): evaluates order_id at
//	                           order_revision — its current revision in TRD (SalesOrderRef v1, R46),
//	                           a draft or an active order — against the screening_regulation revision
//	                           regulation_id/regulation_revision in force (POL, R50, re-checked by
//	                           ConfigGuard v1 in the transaction) and the country programme lists
//	                           programme_lists names (reference data, ADR-BR-017; S5 A3b: until POL's
//	                           screening_regulation names them, the evaluation does, and a foreign
//	                           list is refused for want of the regulation's legal confirmation; a
//	                           list never imported is a gap, never a clear one): the destination
//	                           country (the consignee address's country unless destination enters
//	                           another with a
//	                           reason and evidence, D-8), the countries of the order's party addresses
//	                           (PTY, R77; address_countries enters one PTY has none for, with evidence),
//	                           every line's export-control classification (CAT DeclarationProfileRef
//	                           v1, R39) and the end use with evidence. It opens the check of the order
//	                           revision (expected_version 0) or re-evaluates a pending or blocked one;
//	                           a clear one is reopened first. A missing fact is a gap: the check stays
//	                           pending and cannot conclude clear.
//	trade_compliance.decide    human only, compliance-exclusive: clear (license_required stated) or
//	                           blocked, with a reason and optional evidence, on the check's current
//	                           evaluation (evaluation_id), binding the regulation revision (ConfigGuard
//	                           v1 in the transaction) and, for clear, a validity of the regulation's
//	                           rescreen interval from the evaluation, never past the regulation's own.
//	trade_compliance.reopen    human (compliance) or system (S5 D-18 consumers): cause
//	                           regulation_revised (check_id, a regulation revision in force other than
//	                           the one the clear conclusion follows), programme_updated (check_id, a
//	                           programme list the conclusion used has a new version, MD-47), expired
//	                           (check_id, a clear conclusion past its validity) or order_amended
//	                           (order_id at its new
//	                           order_revision, expected_version 0: a pending check for the revision,
//	                           PL-14, D-9). A blocked conclusion is never reopened: evaluate it again
//	                           after its facts changed.
//
// A conclusion applies to exactly one order revision at its order hash. TradeComplianceGuard v1
// (guard.go) is how TRD's order.activate (R58) and FUL's release (R56) rely on it.
//
// Request members in snake_case: trade_compliance.evaluate {order_id, order_revision,
// expected_version, regulation_id, regulation_revision, programme_lists, destination {country, reason,
// evidence_ids}, address_countries [{party_id, address_id, country, evidence_ids}], end_use
// {statement, evidence_ids}}; trade_compliance.decide {check_id, expected_version, evaluation_id,
// conclusion, reason, license_required, evidence_ids}; trade_compliance.reopen {cause, check_id,
// order_id, order_revision, expected_version, regulation_id, regulation_revision, reason}.
package api

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Source is CNX's event source.
const Source = "idp-brant.cnx"

// Contract names CNX's command contract, the envelope's contract member on the command routes
// POST /api/brant/v1/cnx/commands/<command>.
const Contract = "brant.china-export/0.1.0"

// Commands, reads, permissions and events (tactical model CNX-AGG-TradeComplianceCheck, role
// templates: compliance holds the three commands, trade_compliance.decide exclusively).
const (
	CommandEvaluate = "trade_compliance.evaluate"
	CommandDecide   = "trade_compliance.decide"
	CommandReopen   = "trade_compliance.reopen"

	// Reads: a check, the checks of an order's revisions, the audit trail.
	CommandGetCheck    = "trade_compliance.get"
	CommandOrderChecks = "trade_compliance.order_checks"
	CommandAudit       = "cnx.audit"
	PermissionRead     = "cnx.read"
	PermissionAudit    = "cnx.audit"

	// Facts (Context Map CNX facts): an evaluation publishes nothing (S3-D09: an internal step
	// without consumers).
	EventTradeComplianceDecided  = "idp-brant.cnx.trade_compliance_decided.v1"
	EventTradeComplianceReopened = "idp-brant.cnx.trade_compliance_reopened.v1"
)

// States of a check (SM-CNX-COMPLIANCE), and the state a read or the guard derives when a clear
// conclusion's validity has ended.
const (
	StatusPending = "pending"
	StatusClear   = "clear"
	StatusBlocked = "blocked"
	StatusExpired = "expired"
)

// Reasons for a check's state at an instant, as the guard and the reads decide them.
const (
	ReasonNotChecked       = "not_checked"       // pending: no check for the order revision
	ReasonOrderRevised     = "order_revised"     // pending: only another revision of the order has a check (CNX-INV-001)
	ReasonOrderChanged     = "order_changed"     // the check binds another order hash
	ReasonNotEvaluated     = "not_evaluated"     // pending: opened or reopened, no current evaluation
	ReasonAwaitingDecision = "awaiting_decision" // pending: evaluated, no conclusion
	ReasonListUpdated      = "list_updated"      // pending: a programme list the clear conclusion used has a new version (MD-47)
	ReasonBlocked          = "blocked"           // blocked by compliance; no business approval overrides it
	ReasonExpired          = "expired"           // expired: the clear conclusion's validity has ended
	ReasonCleared          = "cleared"           // clear and valid
)

// Conclusions of trade_compliance.decide.
const (
	ConclusionClear   = "clear"
	ConclusionBlocked = "blocked"
)

// Causes of trade_compliance.reopen.
const (
	CauseRegulationRevised = "regulation_revised"
	CauseProgrammeUpdated  = "programme_updated"
	CauseExpired           = "expired"
	CauseOrderAmended      = "order_amended"
)

// Outcomes of an evaluation (candidates, not conclusions).
const (
	OutcomeNoMatch    = "no_match"
	OutcomeMatch      = "match"
	OutcomeIncomplete = "incomplete"
)

// Limits of the contract.
const (
	MaxReason          = 1000
	MaxStatement       = 2000
	MaxEvidence        = 20
	MaxAddresses       = 20
	MaxRegulationIDLen = 40
	MaxProgrammeLists  = 100 // reference data's MaxProgrammeLists
	MaxListCodeLength  = 32  // reference data's MaxListCodeLength
	maxVersion         = 2147483647
)

func schema(path string) error { return kernel.Reject(kernel.CodeSchemaInvalid, path, "") }

// Text reports whether s is 1 to max characters of valid UTF-8, not only white space, without
// control characters.
func Text(s string, max int) bool {
	n := utf8.RuneCountInString(s)
	return n >= 1 && n <= max && utf8.ValidString(s) && strings.TrimSpace(s) != "" && strings.IndexFunc(s, unicode.IsControl) < 0
}

// Country reports whether c is an officially assigned ISO 3166-1 alpha-2 code.
func Country(c string) bool { return kernel.ValidCountryCode(c) }

func evidence(ids []string, path string, min int) error {
	if len(ids) < min || len(ids) > MaxEvidence {
		return schema(path)
	}
	seen := map[string]bool{}
	for i, id := range ids {
		if !kernel.ValidIdentifier(id) || seen[id] {
			return schema(path + "/" + strconv.Itoa(i))
		}
		seen[id] = true
	}
	return nil
}

func regulation(id string, revision int64) error {
	switch {
	case len(id) > MaxRegulationIDLen || !kernel.ValidIdentifier(id):
		return schema("/args/regulation_id")
	case revision < 1 || revision > maxVersion:
		return schema("/args/regulation_revision")
	}
	return nil
}

// Destination enters the destination country instead of the consignee address's (S5 D-8): why,
// and the EVD sources it rests on (at least one).
type Destination struct {
	Country     string
	Reason      string
	EvidenceIDs []string
}

// AddressCountry enters the country of an order address PTY records none for, with evidence.
type AddressCountry struct {
	PartyID     string
	AddressID   string
	Country     string
	EvidenceIDs []string
}

// EndUse states the goods' end use with the EVD sources it rests on (at least one).
type EndUse struct {
	Statement   string
	EvidenceIDs []string
}

// Evaluate evaluates order OrderID at OrderRevision under the screening_regulation revision
// (RegulationID, RegulationRevision) against the current versions of the country programme lists
// ProgrammeLists (1 to MaxProgrammeLists codes). ExpectedVersion is the check's row version, 0
// when the order revision has none.
type Evaluate struct {
	IdempotencyKey     string
	OrderID            string
	OrderRevision      int64
	ExpectedVersion    int64
	RegulationID       string
	RegulationRevision int64
	ProgrammeLists     []string
	Destination        *Destination
	AddressCountries   []AddressCountry
	EndUse             *EndUse
}

// Validate checks the closed contract.
func (c Evaluate) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.OrderID):
		return schema("/args/order_id")
	case c.OrderRevision < 1 || c.OrderRevision > maxVersion:
		return schema("/args/order_revision")
	case c.ExpectedVersion < 0 || c.ExpectedVersion > maxVersion:
		return schema("/args/expected_version")
	}
	if err := regulation(c.RegulationID, c.RegulationRevision); err != nil {
		return err
	}
	if len(c.ProgrammeLists) < 1 || len(c.ProgrammeLists) > MaxProgrammeLists {
		return schema("/args/programme_lists")
	}
	for i, code := range c.ProgrammeLists {
		if len(code) > MaxListCodeLength || !kernel.ValidIdentifier(code) || slices.Index(c.ProgrammeLists, code) != i {
			return schema("/args/programme_lists/" + strconv.Itoa(i))
		}
	}
	if d := c.Destination; d != nil {
		switch {
		case !Country(d.Country):
			return schema("/args/destination/country")
		case !Text(d.Reason, MaxReason):
			return schema("/args/destination/reason")
		}
		if err := evidence(d.EvidenceIDs, "/args/destination/evidence_ids", 1); err != nil {
			return err
		}
	}
	if len(c.AddressCountries) > MaxAddresses {
		return schema("/args/address_countries")
	}
	seen := map[[2]string]bool{}
	for i, a := range c.AddressCountries {
		p := "/args/address_countries/" + strconv.Itoa(i)
		switch {
		case !kernel.ValidIdentifier(a.PartyID):
			return schema(p + "/party_id")
		case !kernel.ValidIdentifier(a.AddressID) || seen[[2]string{a.PartyID, a.AddressID}]:
			return schema(p + "/address_id")
		case !Country(a.Country):
			return schema(p + "/country")
		}
		if err := evidence(a.EvidenceIDs, p+"/evidence_ids", 1); err != nil {
			return err
		}
		seen[[2]string{a.PartyID, a.AddressID}] = true
	}
	if e := c.EndUse; e != nil {
		if !Text(e.Statement, MaxStatement) {
			return schema("/args/end_use/statement")
		}
		return evidence(e.EvidenceIDs, "/args/end_use/evidence_ids", 1)
	}
	return nil
}

// Decide concludes check CheckID at ExpectedVersion on its current evaluation EvaluationID:
// Conclusion clear (LicenseRequired stated) or blocked (LicenseRequired omitted), with Reason and
// optional evidence. Human only.
type Decide struct {
	IdempotencyKey  string
	CheckID         string
	ExpectedVersion int64
	EvaluationID    string
	Conclusion      string
	Reason          string
	LicenseRequired *bool
	EvidenceIDs     []string
}

// Validate checks the closed contract.
func (c Decide) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.CheckID):
		return schema("/args/check_id")
	case c.ExpectedVersion < 1 || c.ExpectedVersion > maxVersion:
		return schema("/args/expected_version")
	case !kernel.ValidIdentifier(c.EvaluationID):
		return schema("/args/evaluation_id")
	case c.Conclusion != ConclusionClear && c.Conclusion != ConclusionBlocked:
		return schema("/args/conclusion")
	case !Text(c.Reason, MaxReason):
		return schema("/args/reason")
	case (c.Conclusion == ConclusionClear) != (c.LicenseRequired != nil):
		return schema("/args/license_required")
	}
	return evidence(c.EvidenceIDs, "/args/evidence_ids", 0)
}

// Reopen returns a clear conclusion to pending, or opens a pending check for a new order revision:
// Cause regulation_revised (CheckID, ExpectedVersion, RegulationID, RegulationRevision),
// programme_updated or expired (CheckID, ExpectedVersion) or order_amended (OrderID, OrderRevision, ExpectedVersion 0), with
// Reason. The members another cause needs are omitted.
type Reopen struct {
	IdempotencyKey     string
	Cause              string
	CheckID            string
	OrderID            string
	OrderRevision      int64
	ExpectedVersion    int64
	RegulationID       string
	RegulationRevision int64
	Reason             string
}

// Validate checks the closed contract.
func (c Reopen) Validate() error {
	if !kernel.ValidIdentifier(c.IdempotencyKey) {
		return schema("/idempotency_key")
	}
	switch c.Cause {
	case CauseRegulationRevised, CauseProgrammeUpdated, CauseExpired:
		switch {
		case !kernel.ValidIdentifier(c.CheckID):
			return schema("/args/check_id")
		case c.ExpectedVersion < 1 || c.ExpectedVersion > maxVersion:
			return schema("/args/expected_version")
		case c.OrderID != "":
			return schema("/args/order_id")
		case c.OrderRevision != 0:
			return schema("/args/order_revision")
		}
		if c.Cause == CauseRegulationRevised {
			if err := regulation(c.RegulationID, c.RegulationRevision); err != nil {
				return err
			}
		} else if c.RegulationID != "" || c.RegulationRevision != 0 {
			return schema("/args/regulation_id")
		}
	case CauseOrderAmended:
		switch {
		case c.CheckID != "":
			return schema("/args/check_id")
		case !kernel.ValidIdentifier(c.OrderID):
			return schema("/args/order_id")
		case c.OrderRevision < 2 || c.OrderRevision > maxVersion:
			return schema("/args/order_revision")
		case c.ExpectedVersion != 0:
			return schema("/args/expected_version")
		case c.RegulationID != "" || c.RegulationRevision != 0:
			return schema("/args/regulation_id")
		}
	default:
		return schema("/args/cause")
	}
	if !Text(c.Reason, MaxReason) {
		return schema("/args/reason")
	}
	return nil
}

// Command is one of CNX's commands.
type Command interface{ cnxCommand() }

func (Evaluate) cnxCommand() {}
func (Decide) cnxCommand()   {}
func (Reopen) cnxCommand()   {}

// Receipt is the closed success answer of a command: ObjectID is the check ID, RowVersion the
// check's row version and Status its state after the command.
type Receipt struct {
	ActionID   string
	ObjectID   string
	RowVersion int64
	Status     string
	Replayed   bool
}

// CheckReceipt is the answer of every command: the receipt and what the check binds — the order
// revision at its order hash — with its current evaluation and decision as stored now (a replay is
// rebuilt from the stored check).
type CheckReceipt struct {
	Receipt
	CheckID         string
	OrderID         string
	OrderRevision   int64
	OrderHash       string
	EvaluationID    string
	Outcome         string
	DecisionID      string
	LicenseRequired bool
	ValidUntil      time.Time
}

// Commands is CNX's command entry point. Errors are *kernel.Rejection for refusals. An error that
// is application.ErrCommitUncertain from IDP-Bento-Go means the outcome is unknown: the caller sends
// the same command with the same idempotency key again, or reconciles it.
type Commands interface {
	Evaluate(ctx context.Context, cmd Evaluate) (CheckReceipt, error)
	Decide(ctx context.Context, cmd Decide) (CheckReceipt, error)
	Reopen(ctx context.Context, cmd Reopen) (CheckReceipt, error)
}

// EvidenceView is an EVD source an entry rests on (SourceRef v1).
type EvidenceView struct {
	SourceID      string
	ContentSHA256 string
}

// RegulationView is a screening_regulation revision as a check binds it.
type RegulationView struct {
	ID                   string
	Revision             int64
	ContentHash          string
	RescreenIntervalDays int
	ValidUntil           time.Time
}

// DestinationView is the destination country an evaluation examined and where it came from:
// consignee_address (PartyID, AddressID) or entered (Reason, Evidence); Country "" when unknown.
type DestinationView struct {
	Country   string
	Source    string
	PartyID   string
	AddressID string
	Reason    string
	Evidence  []EvidenceView
}

// PartyView is one address of the order's parties and its country: from pty, entered (with
// Evidence) or "" when unknown.
type PartyView struct {
	Role          string
	PartyID       string
	PartyRevision int64
	AddressID     string
	Country       string
	Source        string
	Evidence      []EvidenceView
}

// LineView is an order line and its export-control classification: not_controlled or controlled
// (ListReference) from a verified declaration profile, unverified or unknown.
type LineView struct {
	LineID           string
	SkuID            string
	SpecRevision     int64
	Classification   string
	ProfileID        string
	ProfileRevision  int64
	CodeTableVersion string
	CommodityCode    string
	ListReference    string
	ProfileHash      string
}

// EndUseView is the end use the evaluator stated.
type EndUseView struct {
	Statement string
	Evidence  []EvidenceView
}

// ProgrammeView is a country programme version an evaluation matched against.
type ProgrammeView struct {
	Code         string
	Jurisdiction string
	Version      string
	ContentHash  string
}

// FindingView is a candidate: kind programme_match (programme ProgrammeID of list version
// ListCode/Version names Country, which the destination or the parties in Roles have; Goods is what
// its measure covers — all for embargo and sanctions, controlled for arms_embargo and
// export_control — and Applies whether it applies to these goods) or controlled_goods (LineID's
// goods are on a control list).
type FindingView struct {
	Kind          string
	ListCode      string
	Jurisdiction  string
	Version       string
	ProgrammeID   string
	ProgrammeName string
	ProgrammeHash string
	Country       string
	Measure       string
	Goods         string
	Scope         string
	Roles         []string
	LineID        string
	ListReference string
	Applies       bool
}

// GapView is a missing fact: its kind and the address (party/address) or line it concerns.
type GapView struct {
	Kind string
	Ref  string
}

// EvaluationView is one evaluation of a check, as its document records it.
type EvaluationView struct {
	EvaluationID   string
	Seq            int
	OrderRevision  int64
	OrderHash      string
	Regulation     RegulationView
	Destination    DestinationView
	Parties        []PartyView
	Lines          []LineView
	EndUse         *EndUseView
	ProgrammeLists []string
	Programmes     []ProgrammeView
	Findings       []FindingView
	Gaps           []GapView
	Outcome        string
	FactsHash      string
	EvaluatedBy    kernel.ActorRef
	EvaluatedAt    time.Time
	ValidUntil     time.Time // the latest a conclusion on it may stay valid
	AuthorityEpoch int64
}

// DecisionView is a conclusion.
type DecisionView struct {
	DecisionID      string
	EvaluationID    string
	Conclusion      string
	Reason          string
	LicenseRequired bool
	Regulation      RegulationView
	ValidUntil      time.Time // zero for blocked
	Evidence        []EvidenceView
	DecidedBy       kernel.ActorRef
	DecidedAt       time.Time
	AuthorityEpoch  int64
}

// ReopeningView is a reopening: its cause, the decision it reopened ("" for order_amended) and the
// regulation revision or the programme lists with a new version that caused it.
type ReopeningView struct {
	ReopeningID        string
	Cause              string
	DecisionID         string
	RegulationID       string
	RegulationRevision int64
	UpdatedLists       []string
	Reason             string
	ReopenedBy         kernel.ActorRef
	ReopenedAt         time.Time
	AuthorityEpoch     int64
}

// CheckView is a check as the reads show it: what it binds, its stored state and, as of CheckedAt,
// its state and reason as TradeComplianceGuard v1 decides them for its binding, and every
// evaluation, decision and reopening, oldest first.
type CheckView struct {
	CheckID             string
	OrderID             string
	OrderRevision       int64
	OrderHash           string
	Status              string
	State               string
	Reason              string
	RowVersion          int64
	CurrentEvaluationID string
	CurrentDecisionID   string
	PreviousCheckID     string
	OpenedBy            kernel.ActorRef
	OpenedAt            time.Time
	Evaluations         []EvaluationView
	Decisions           []DecisionView
	Reopenings          []ReopeningView
	CheckedAt           time.Time
}

// CheckSummary is one check of an order's revisions, as of CheckedAt.
type CheckSummary struct {
	CheckID         string
	OrderRevision   int64
	OrderHash       string
	Status          string
	State           string
	Reason          string
	RowVersion      int64
	DecisionID      string
	LicenseRequired bool
	ValidUntil      time.Time
	CheckedAt       time.Time
}

// Queries is CNX's audited read side.
type Queries interface {
	// GetCheck returns a check the caller sees, audited as trade_compliance.get (cnx.read):
	// NOT_FOUND otherwise; an evaluation that is not intact is REFERENCE_INVALID.
	GetCheck(ctx context.Context, checkID string) (CheckView, error)
	// OrderChecks returns the checks of an order's revisions the caller sees, newest revision first,
	// audited as trade_compliance.order_checks (cnx.read); an order without any is an empty list.
	OrderChecks(ctx context.Context, orderID string) ([]CheckSummary, error)
	// Audit returns CNX's audit trail (cnx.audit).
	Audit(ctx context.Context) ([]kernel.AuditEntry, error)
}

// Auditor records a request an inbound adapter refused before it became a command.
type Auditor interface {
	AuditRejected(ctx context.Context, command, objectID, code string) error
}

// Reconciliation is what became of a command whose answer the caller lost.
type Reconciliation struct {
	State    kernel.ReconcileState
	ActionID string
	ObjectID string
	Receipt  *Receipt
}

// Reconciler recovers from an unknown outcome without executing anything (contract 06, 动作对账).
// Reconciling needs cnx.read, not the command's own permission.
type Reconciler interface {
	Reconcile(ctx context.Context, cmd Command) (Reconciliation, error)
}
