// Package api is the contract of the POL bounded context (policy and configuration: the company
// profile, currencies, units and their conversions, exchange rates, calendars, trade and payment
// terms, taxes, fees, pricing and credit policies, bank accounts, document templates and
// numbering) for transports, the AI layer and the contexts downstream of POL. It exposes no POL
// domain type.
//
// A configuration is identified by (tenant, company, kind, config_id). Its content lives in
// revisions with monotonic numbers: one working revision is saved whole, a human other than its
// last editor publishes it, and publishing supersedes the previously active revision atomically
// (SM-POL-REVISION). Downstream contexts adopt an exact revision as a ConfigRef v1 and ask POL
// whether it is in force at an instant: before their transaction through Queries, and inside it
// through ConfigGuard v1 (guard.go), the guard function POL publishes in its schema.
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

const (
	// Contract names the request and response shapes of POL commands.
	Contract = "brant.pol/0.1.0"
	// Source identifies POL as the producer of integration events.
	Source = "idp-brant.pol"

	EventConfigPublished  = "idp-brant.pol.config_published.v1"
	EventConfigSuperseded = "idp-brant.pol.config_superseded.v1"
	EventConfigRetired    = "idp-brant.pol.config_retired.v1"
	EventSnapshotCaptured = "idp-brant.pol.foundation_snapshot_captured.v1"
	EventNumberIssued     = "idp-brant.pol.document_number_issued.v1"

	CommandSaveDraft       = "config.save_draft"
	CommandDiscardDraft    = "config.discard_draft"
	CommandPublish         = "config.publish"
	CommandRetire          = "config.retire"
	CommandCaptureSnapshot = "foundation.snapshot"
	CommandIssueNumber     = "number.issue"
)

// Statuses of a configuration revision (SM-POL-REVISION), of an issued number and of a snapshot.
const (
	StatusWorking    = "working"
	StatusActive     = "active"
	StatusSuperseded = "superseded"
	StatusRetired    = "retired"
	StatusDiscarded  = "discarded"
	StatusIssued     = "issued"
	StatusCaptured   = "captured"
)

// Kinds of configuration: the closed enumeration of the tactical model (POL-AGG-ConfigurationRoot).
// Only the kinds KindImplemented reports are accepted so far; screening_regulation is refused with
// UNSUPPORTED_CONTRACT until its contract lands.
const (
	KindCompanyProfile        = "company_profile"
	KindCurrency              = "currency"
	KindUnit                  = "unit"
	KindUnitConversion        = "unit_conversion"
	KindFXRate                = "fx_rate"
	KindCalendar              = "calendar"
	KindTradeTerms            = "trade_terms"
	KindPaymentTerms          = "payment_terms"
	KindTaxProfile            = "tax_profile"
	KindTaxApplication        = "tax_application"
	KindFeeCategory           = "fee_category"
	KindPricingPolicy         = "pricing_policy"
	KindBankAccount           = "bank_account"
	KindNumberSequence        = "number_sequence"
	KindDocumentTemplate      = "document_template"
	KindQuoteDocumentTemplate = "quote_document_template"
	KindScreeningRegulation   = "screening_regulation"
	KindCreditPolicy          = "credit_policy"
)

// Closed values of configuration data.
const (
	RoundingHalfUp = "HALF_UP"
	ResetAnnual    = "annual"

	PurposeCostEstimate = "cost_estimate"
	PurposeQuotation    = "quotation"

	DimensionCount  = "count"
	DimensionMass   = "mass"
	DimensionLength = "length"
	DimensionVolume = "volume"

	TriggerOrderConfirmed = "order_confirmed"
	TriggerBeforeShipment = "before_shipment"
	TriggerAfterShipment  = "after_shipment"
	TriggerAfterInvoice   = "after_invoice"

	DocumentTypeRfq           = "rfq"
	DocumentTypeQuote         = "quote"
	DocumentTypeOrder         = "order"
	DocumentTypePurchaseOrder = "purchase_order"
	DocumentTypeInvoice       = "invoice"

	// Jurisdictions of a screening list a screening_regulation enables (ADR-BR-017).
	JurisdictionCN      = "CN"
	JurisdictionUN      = "UN"
	JurisdictionForeign = "foreign"
)

// Limits of the contract.
const (
	// MaxConfigIDLength keeps ObjectID(kind, config_id) a 64-character identifier.
	MaxConfigIDLength = 40
	MaxRefs           = 200
	MaxEvidence       = 200
	MaxPaymentStages  = 20
	MaxScreeningLists = 100

	maxVersion            = 2147483647
	maxNameCharacters     = 500
	maxTimeZoneCharacters = 64
	maxPaymentDays        = 3650
	maxCurrencyMinorUnits = 4
	maxUnitPrecision      = 6
	minSequenceWidth      = 4
	maxSequenceWidth      = 12
	maxSequencePrefix     = 16
	maxListCodeLength     = 32
	maxRescreenDays       = 3650
)

var (
	kinds = []string{KindCompanyProfile, KindCurrency, KindUnit, KindUnitConversion, KindFXRate, KindCalendar, KindTradeTerms,
		KindPaymentTerms, KindTaxProfile, KindTaxApplication, KindFeeCategory, KindPricingPolicy, KindBankAccount,
		KindNumberSequence, KindDocumentTemplate, KindQuoteDocumentTemplate, KindScreeningRegulation, KindCreditPolicy}
	implemented   = slices.Clone(kinds)
	purposes      = []string{PurposeCostEstimate, PurposeQuotation}
	dimensions    = []string{DimensionCount, DimensionMass, DimensionLength, DimensionVolume}
	triggers      = []string{TriggerOrderConfirmed, TriggerBeforeShipment, TriggerAfterShipment, TriggerAfterInvoice}
	documentTypes = []string{DocumentTypeRfq, DocumentTypeQuote, DocumentTypeOrder, DocumentTypePurchaseOrder, DocumentTypeInvoice}
	jurisdictions = []string{JurisdictionCN, JurisdictionUN, JurisdictionForeign}
)

// PercentFormat is the lexical form of a payment stage's percentage.
var PercentFormat = kernel.DecimalFormat{IntDigits: 3, Scale: 6}

// KindKnown reports whether kind is one of the eighteen configuration kinds.
func KindKnown(kind string) bool { return slices.Contains(kinds, kind) }

// KindImplemented reports whether POL accepts configurations of kind yet.
func KindImplemented(kind string) bool { return slices.Contains(implemented, kind) }

// Codes POL declares, with the category the error catalog V0.1 gives them (422 invalid, 409
// conflict).
const (
	CodeIndependentReviewRequired = "INDEPENDENT_REVIEW_REQUIRED"
	CodeImmutableDefinition       = "IMMUTABLE_DEFINITION"
	CodeFXScopeMismatch           = "FX_SCOPE_MISMATCH"
	CodeFXStale                   = "FX_STALE"
	CodePaymentTotalMismatch      = "PAYMENT_TOTAL_MISMATCH"
	CodeSequenceExhausted         = "SEQUENCE_EXHAUSTED"
	CodeBusinessKeyConflict       = "BUSINESS_KEY_CONFLICT"
	CodeConfigNotActive           = "CONFIG_NOT_ACTIVE"
	CodeConfigExpired             = "CONFIG_EXPIRED"
	CodeUnitScopeMismatch         = "UNIT_SCOPE_MISMATCH"
	CodeTaxPolicyUnconfirmed      = "TAX_POLICY_UNCONFIRMED"
	CodeTemplateInvalid           = "TEMPLATE_INVALID"
)

var codes = map[string]kernel.Category{
	CodeIndependentReviewRequired: kernel.CategoryInvalid,
	CodeImmutableDefinition:       kernel.CategoryInvalid,
	CodeFXScopeMismatch:           kernel.CategoryInvalid,
	CodeFXStale:                   kernel.CategoryInvalid,
	CodePaymentTotalMismatch:      kernel.CategoryInvalid,
	CodeSequenceExhausted:         kernel.CategoryInvalid,
	CodeBusinessKeyConflict:       kernel.CategoryConflict,
	CodeConfigNotActive:           kernel.CategoryInvalid,
	CodeConfigExpired:             kernel.CategoryInvalid,
	CodeUnitScopeMismatch:         kernel.CategoryInvalid,
	CodeTaxPolicyUnconfirmed:      kernel.CategoryInvalid,
	CodeTemplateInvalid:           kernel.CategoryInvalid,
}

// Category returns the category of a code POL declares.
func Category(code string) (kernel.Category, bool) {
	c, ok := codes[code]
	return c, ok
}

// Reject returns a rejection with a code POL declares, for downstream contexts that pass a POL
// verdict on at a path of their own request. It panics on a code POL does not declare.
func Reject(code, path string) *kernel.Rejection {
	c, ok := codes[code]
	if !ok {
		panic("pol: undeclared rejection code " + code)
	}
	return kernel.RejectAs(code, c, path, "")
}

// ConfigRef is ConfigRef v1: a configuration and the exact revision a downstream fact adopted.
type ConfigRef struct {
	Kind     string
	ConfigID string
	Revision int64
}

// Validate checks the form of the reference; path is where the reference sits in the request.
func (r ConfigRef) Validate(path string) error {
	switch {
	case !KindKnown(r.Kind):
		return schema(path + "/kind")
	case !ValidConfigID(r.ConfigID):
		return schema(path + "/id")
	case r.Revision < 1 || r.Revision > maxVersion:
		return schema(path + "/revision")
	}
	return nil
}

// ValidConfigID reports whether id is an identifier of at most MaxConfigIDLength characters.
func ValidConfigID(id string) bool { return len(id) <= MaxConfigIDLength && kernel.ValidIdentifier(id) }

// ObjectID is how the audit trail, receipts and events name the configuration kind:id.
func ObjectID(kind, configID string) string { return kind + ":" + configID }

// SaveDraft creates a configuration at ExpectedVersion 0 or replaces its working revision whole at
// the current root version; without a working revision it starts one with the next revision
// number. Times are UTC timestamps ("2026-10-22T00:00:00Z"); ValidUntil empty is open-ended, which
// an exchange rate may not be. Data carries exactly the member of Kind.
type SaveDraft struct {
	IdempotencyKey  string
	Kind            string
	ConfigID        string
	ExpectedVersion int64
	ValidFrom       string
	ValidUntil      string
	Data            ConfigData
}

// DiscardDraft ends the working revision for a recorded reason; its number is not reused.
type DiscardDraft struct {
	IdempotencyKey  string
	Kind            string
	ConfigID        string
	ExpectedVersion int64
	Reason          string
}

// Publish makes the working revision Revision the active one. Only a human other than the
// revision's last editor publishes (POL-INV-001), citing the evidence verified and a reason; the
// revision's validity must contain the instant of publication, and the previously active revision
// is superseded in the same transaction.
type Publish struct {
	IdempotencyKey  string
	Kind            string
	ConfigID        string
	ExpectedVersion int64
	Revision        int64
	EvidenceIDs     []string
	Reason          string
}

// Retire ends the active revision for a recorded reason; every revision stays readable.
type Retire struct {
	IdempotencyKey  string
	Kind            string
	ConfigID        string
	ExpectedVersion int64
	Reason          string
}

// IssueNumber issues the next document number of the sequence's document type and year
// (POL-INV-005). The same business reference gets its original number back.
type IssueNumber struct {
	IdempotencyKey string
	Sequence       ConfigRef // a number_sequence revision in force
	BusinessRef    string    // the downstream object the number is for, such as a quote ID
}

// CaptureSnapshot records the exact configuration revisions a calculation adopts, with their
// content hashes, at the instant of capture (FoundationSnapshot, append-only).
type CaptureSnapshot struct {
	IdempotencyKey string
	SnapshotID     string
	Purpose        string // the use of every exchange rate among Refs
	Refs           []ConfigRef
}

// Validate checks the closed contract.
func (c SaveDraft) Validate() error {
	if err := target(c.IdempotencyKey, c.Kind, c.ConfigID, c.ExpectedVersion, 0); err != nil {
		return err
	}
	if _, err := kernel.ParseTimestamp(c.ValidFrom); err != nil {
		return schema("/args/valid_from")
	}
	if c.ValidUntil != "" {
		if _, err := kernel.ParseTimestamp(c.ValidUntil); err != nil {
			return schema("/args/valid_until")
		}
	} else if c.Kind == KindFXRate {
		return schema("/args/valid_until")
	}
	return c.Data.validate(c.Kind)
}

func (c DiscardDraft) Validate() error {
	if err := target(c.IdempotencyKey, c.Kind, c.ConfigID, c.ExpectedVersion, 1); err != nil {
		return err
	}
	return reason(c.Reason)
}

func (c Publish) Validate() error {
	if err := target(c.IdempotencyKey, c.Kind, c.ConfigID, c.ExpectedVersion, 1); err != nil {
		return err
	}
	if c.Revision < 1 || c.Revision > maxVersion {
		return schema("/args/revision")
	}
	if err := uniqueIDs(c.EvidenceIDs, 1, MaxEvidence, "/args/evidence_ids"); err != nil {
		return err
	}
	return reason(c.Reason)
}

func (c Retire) Validate() error {
	if err := target(c.IdempotencyKey, c.Kind, c.ConfigID, c.ExpectedVersion, 1); err != nil {
		return err
	}
	return reason(c.Reason)
}

func (c IssueNumber) Validate() error {
	if !kernel.ValidIdentifier(c.IdempotencyKey) {
		return schema("/idempotency_key")
	}
	if err := c.Sequence.Validate("/args/sequence"); err != nil {
		return err
	}
	if c.Sequence.Kind != KindNumberSequence {
		return schema("/args/sequence/kind")
	}
	if !kernel.ValidIdentifier(c.BusinessRef) {
		return schema("/args/business_ref")
	}
	return nil
}

func (c CaptureSnapshot) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.SnapshotID):
		return schema("/args/id")
	case !slices.Contains(purposes, c.Purpose):
		return schema("/args/purpose")
	}
	return refs(c.Refs, 1, "/args/refs")
}

// Auditor records a request an inbound adapter refused before it became a command, such as a body
// that is not valid JSON, in the context's audit trail. The principal in ctx is the requester.
type Auditor interface {
	AuditRejected(ctx context.Context, command, objectID, code string) error
}

// Receipt is the closed success answer of a command. For configuration commands ObjectID is
// ObjectID(kind, config_id), RowVersion the root version and Status the status of the revision the
// command changed; GetConfig shows the revision numbers. For number.issue ObjectID is the issued
// document number.
type Receipt struct {
	ActionID   string
	ObjectID   string
	RowVersion int64
	Status     string
	Replayed   bool
}

// Commands is POL's command entry point. Errors are *kernel.Rejection for refusals.
type Commands interface {
	SaveDraft(ctx context.Context, cmd SaveDraft) (Receipt, error)
	DiscardDraft(ctx context.Context, cmd DiscardDraft) (Receipt, error)
	Publish(ctx context.Context, cmd Publish) (Receipt, error)
	Retire(ctx context.Context, cmd Retire) (Receipt, error)
	IssueNumber(ctx context.Context, cmd IssueNumber) (Receipt, error)
	CaptureSnapshot(ctx context.Context, cmd CaptureSnapshot) (Receipt, error)
}

// RevisionView is one revision of a configuration. Published revisions never change.
type RevisionView struct {
	Revision    int64
	Status      string
	ContentHash string // SHA-256 of the revision's canonical content
	ValidFrom   time.Time
	ValidUntil  time.Time // zero: open-ended
	Data        ConfigData
	SavedBy     kernel.ActorRef // the last editor
	SavedAt     time.Time
	PublishedBy kernel.ActorRef // zero before publication
	PublishedAt time.Time
	EvidenceIDs []string  // the evidence the publisher verified
	EndedAt     time.Time // when it was superseded, retired or discarded
}

// ConfigView is a configuration and all its revisions, oldest first.
type ConfigView struct {
	Kind            string
	ConfigID        string
	RootVersion     int64
	WorkingRevision int64 // 0: none
	ActiveRevision  int64 // 0: none
	Revisions       []RevisionView
}

// ResolvedConfig is what POL says about one ConfigRef at an instant. Code is empty when the
// revision is in force then: published at or before the instant, not superseded or retired by it
// (otherwise CONFIG_NOT_ACTIVE) and valid at it (otherwise CONFIG_EXPIRED).
type ResolvedConfig struct {
	Ref         ConfigRef
	Status      string
	Code        string
	ContentHash string
	ValidFrom   time.Time
	ValidUntil  time.Time
	Data        ConfigData
}

// Check returns nil when the revision is in force and the rejection POL's verdict is otherwise,
// at path of the caller's request.
func (r ResolvedConfig) Check(path string) error {
	if r.Code == "" {
		return nil
	}
	return Reject(r.Code, path)
}

// FXQuery asks for the exchange rate of one use (POL-INV-003): converting FromCurrency into
// ToCurrency for Purpose at instant At. Rate names the fx_rate revision the caller adopted; the same
// currency needs none and gets exactly 1, different currencies need one: a missing rate is never 1
// and never the reciprocal of the opposite direction. A positive MaxObservationAge also refuses a
// rate observed longer ago than that before At. Policy names the pricing_policy revision of the
// calculation (Python fx.convert): it must be in force with its tax profile, ToCurrency must be its
// calculation currency, and its max_fx_age_seconds bounds the observation age as well.
type FXQuery struct {
	FromCurrency      string
	ToCurrency        string
	Purpose           string
	At                time.Time
	Rate              *ConfigRef
	MaxObservationAge time.Duration
	Policy            *ConfigRef
}

// Validate checks the form of the query.
func (q FXQuery) Validate() error {
	switch {
	case !currencyCode(q.FromCurrency):
		return schema("/from_currency")
	case !currencyCode(q.ToCurrency):
		return schema("/to_currency")
	case !slices.Contains(purposes, q.Purpose):
		return schema("/purpose")
	case q.At.IsZero():
		return schema("/at")
	case q.MaxObservationAge < 0:
		return schema("/max_observation_age")
	}
	if q.Rate != nil {
		if err := q.Rate.Validate("/rate"); err != nil {
			return err
		}
		if q.Rate.Kind != KindFXRate {
			return schema("/rate/kind")
		}
	}
	return optionalRef(q.Policy, KindPricingPolicy, "/policy")
}

// ExchangeRate is the rate a use may apply. Rate is the fx_rate revision used, zero for the same
// currency; Value carries both currencies with the minor units of their published definitions.
type ExchangeRate struct {
	Rate        ConfigRef
	ContentHash string
	Value       kernel.ExchangeRate
	ObservedAt  time.Time
}

// SnapshotEntry is one revision a snapshot adopted, with its content hash.
type SnapshotEntry struct {
	Ref         ConfigRef
	ContentHash string
}

// SnapshotView is a FoundationSnapshot: the exact revisions adopted at CapturedAt and the hash of
// the whole snapshot.
type SnapshotView struct {
	SnapshotID  string
	Purpose     string
	CapturedAt  time.Time
	CapturedBy  kernel.ActorRef
	ContentHash string
	Entries     []SnapshotEntry
}

// Queries is POL's read side.
type Queries interface {
	// GetConfig returns a configuration and its revisions, audited as config.get. Bank accounts
	// follow their read rules (BankAccount): a party's account is NOT_FOUND to a reader whose data
	// scope is narrower than the company, and account numbers are masked without pol.read_sensitive.
	GetConfig(ctx context.Context, kind, configID string) (ConfigView, error)
	// ResolveConfigs answers ConfigRef v1 lookups of downstream contexts at instant at. It needs no
	// permission beyond an active grant; references that do not exist in the caller's company, or
	// that the bank account read rules hide, are absent. Code covers the revision itself, not the
	// revisions it depends on. Ask before opening your own transaction; inside it, use ConfigGuard.
	ResolveConfigs(ctx context.Context, refs []ConfigRef, at time.Time) (map[ConfigRef]ResolvedConfig, error)
	// LookupFX applies POL-INV-003 to one use of an exchange rate. Ask before opening your own
	// transaction.
	LookupFX(ctx context.Context, q FXQuery) (ExchangeRate, error)
	// LookupTax returns the tax profile revision a calculation adopts, in force, with a confirmed
	// recovery treatment, and the currency it splits amounts in (Python foundation.tax.preview).
	LookupTax(ctx context.Context, q TaxQuery) (TaxRate, error)
	// LookupUnitConversion returns the unit_conversion revision a calculation adopts for one SKU at
	// one specification revision, in force with both its units (Python foundation.unit.convert).
	LookupUnitConversion(ctx context.Context, q UnitQuery) (QuantityConversion, error)
	// GetSnapshot returns a FoundationSnapshot, audited as snapshot.get.
	GetSnapshot(ctx context.Context, snapshotID string) (SnapshotView, error)
	Audit(ctx context.Context) ([]kernel.AuditEntry, error)
}

// ValidateRefs checks a list of references of a query.
func ValidateRefs(rs []ConfigRef) error { return refs(rs, 0, "/refs") }

func refs(rs []ConfigRef, min int, path string) error {
	if len(rs) < min || len(rs) > MaxRefs {
		return schema(path)
	}
	for i, r := range rs {
		if err := r.Validate(path + "/" + strconv.Itoa(i)); err != nil {
			return err
		}
		if slices.Index(rs, r) != i {
			return schema(path)
		}
	}
	return nil
}

func target(key, kind, id string, expected, minVersion int64) error {
	switch {
	case !kernel.ValidIdentifier(key):
		return schema("/idempotency_key")
	case !KindKnown(kind):
		return schema("/args/kind")
	case !KindImplemented(kind):
		return kernel.Reject(kernel.CodeUnsupportedContract, "/args/kind", "Configuration kind is not implemented yet")
	case !ValidConfigID(id):
		return schema("/args/id")
	case expected < minVersion || expected > maxVersion:
		return schema("/args/expected_version")
	}
	return nil
}

func reason(s string) error {
	if !name(s) {
		return schema("/args/reason")
	}
	return nil
}

func uniqueIDs(ids []string, min, max int, path string) error {
	if len(ids) < min || len(ids) > max {
		return schema(path)
	}
	for i, id := range ids {
		if !kernel.ValidIdentifier(id) || slices.Index(ids, id) != i {
			return schema(path)
		}
	}
	return nil
}

func schema(path string) error { return kernel.Reject(kernel.CodeSchemaInvalid, path, "") }

func currencyCode(s string) bool {
	if len(s) != 3 {
		return false
	}
	for i := 0; i < 3; i++ {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	return true
}

func sequencePrefix(s string) bool {
	if len(s) < 1 || len(s) > maxSequencePrefix || s[0] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if (s[i] < 'A' || s[i] > 'Z') && (s[i] < '0' || s[i] > '9') && s[i] != '-' {
			return false
		}
	}
	return true
}

func decimal(s string, f kernel.DecimalFormat) bool {
	_, err := kernel.ParseDecimal(s, f)
	return err == nil
}

// name is text of 1–500 characters with at least one that is not white space (Python \S).
func name(s string) bool { return nonBlank(s, maxNameCharacters) }

// nonBlank is text of 1–max characters with at least one that is not white space.
func nonBlank(s string, max int) bool {
	return text(s, max) && strings.IndexFunc(s, func(r rune) bool { return !unicode.IsSpace(r) }) >= 0
}

func text(s string, max int) bool {
	n := utf8.RuneCountInString(s)
	return n >= 1 && n <= max && utf8.ValidString(s)
}
