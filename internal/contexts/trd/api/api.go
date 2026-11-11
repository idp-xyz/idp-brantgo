// Package api is the contract of the TRD bounded context (quotes, customer acceptance and sales
// orders) for transports, the AI layer and the contexts downstream of TRD. It exposes no TRD domain
// type. S3 starts with QuoteRoot (WBS S3-01, S3-08): working drafts, the freeze with its commercial
// document and business hash, internal metadata and the root's object control.
//
// A quote root (tenant, company, quote_id) holds business revisions with monotonic numbers: one
// working revision is saved whole (quote.save_draft) or copied from a frozen one (quote.revise),
// discarded (quote.discard_draft) or frozen by a human (quote.freeze). Freezing binds the customer
// (PTY PartyRef, TradingProfileRef and AddressRoleRef v1), the RFQ (INQ RfqRef v1), the cost
// snapshot (CST CostSnapshotRef v1, adoptable at the freeze), the SKU specifications (CAT) and the
// POL configurations, fixes issued_at, and seals the commercial document whose business hash is
// Python V0.50 protocol.business_hash; neither ever changes (TRD-INV-002). Labels and internal
// notes live on the root (quote.metadata.update) and change only its version.
//
// Request members in snake_case (for transports): quote.save_draft {quote_id, expected_version,
// data}; quote.revise {quote_id, expected_version, base_revision}; quote.discard_draft {quote_id,
// expected_version, working_revision, reason, evidence}; quote.freeze {quote_id, expected_version,
// working_revision}; quote.metadata.update {quote_id, expected_version, labels, internal_note};
// object_control.bind {quote_id, task_id, expected_control_epoch}; object_control.stop and
// object_control.release {quote_id, expected_control_epoch, reason}. The members of data are the
// JSON names of QuoteData's fields as documented there.
//
// The freeze computes the quote's amounts on the server (WBS S3-02: internal/domain/calc) and seals
// them in the commercial document's calculation. A frozen revision is reviewed (WBS S3-03, S3-14;
// review.go): quote.submit_review, quote.review.decide with its margin exception, and
// quote.approval.revoke. An approved revision is formalized (WBS S3-04, S3-05; formal.go):
// quote.activate and quote.replace bind its formal document, number and commit evidence and make it
// the root's one active revision; quote.withdraw and quote.cancel end a revision. Steps relying on a
// formal revision resolve it (Lookups) and re-check it in their transaction (QuoteRevisionGuard v1,
// guard.go). The customer's reply to an active formal revision is recorded and verified into an
// acceptance, which forms the contract when it arrived before the revision expired, and is
// invalidated or disputed later (WBS S3-06, S3-13; acceptance.go); steps relying on a formed
// contract resolve the acceptance (AcceptanceLookups) and re-check it in their transaction
// (AcceptanceGuard v1, acceptance_guard.go).
package api

import (
	"context"
	"regexp"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"

	"go.idp.xyz/idp-brant/internal/kernel"
)

const (
	// Contract names the request and response shapes of TRD commands (Python brant.quotation/0.1.0).
	Contract = "brant.quotation/0.1.0"
	// Source identifies TRD as the producer of integration events.
	Source = "idp-brant.trd"

	// EventQuoteFrozen is the event catalog's QuoteFrozen.
	EventQuoteFrozen = "idp-brant.trd.quote_frozen.v1"

	CommandSaveDraft      = "quote.save_draft"
	CommandRevise         = "quote.revise"
	CommandDiscardDraft   = "quote.discard_draft"
	CommandFreeze         = "quote.freeze"
	CommandUpdateMetadata = "quote.metadata.update"

	// NormalizationVersion names the normalization of the commercial document (Python
	// protocol.NORMALIZATION).
	NormalizationVersion = "brant.quote-normalization/0.1.0"
)

// Statuses of a business revision (SM-TRD-REVISION; discarded ends a working revision). S3-01
// writes working, frozen and discarded.
const (
	StatusWorking         = "working"
	StatusFrozen          = "frozen"
	StatusDiscarded       = "discarded"
	StatusPendingApproval = "pending_approval"
	StatusActive          = "active"
	StatusSuperseded      = "superseded"
	StatusWithdrawn       = "withdrawn"
	StatusCancelled       = "cancelled"

	// StatusUpdated is the receipt status of quote.metadata.update.
	StatusUpdated = "updated"
)

// Charge inclusions.
const (
	InclusionSeparate       = "separate"
	InclusionIncludedInLine = "included_in_line"
)

// Places and deliveries, as CST's costing context compares candidates on them (CST api.Delivery):
// a place is a BusinessRef of kind location, a delivery anchor one of kind delivery_anchor.
const (
	PlaceKind  = "location"
	AnchorKind = "delivery_anchor"

	DeliveryFixed    = "fixed"
	DeliveryLeadTime = "lead_time"

	MilestoneGoodsReady = "goods_ready"
	MilestoneHandover   = "handover"
	MilestoneArrival    = "arrival"

	DayBasisCalendar = "calendar"
	DayBasisWorking  = "working"

	maxLeadDays = 3650
	maxTimeZone = 100
)

// Limits of the contract (Python T03_write-commands.schema.json).
const (
	MaxLines     = 200
	MaxCharges   = 100
	MaxEvidence  = 200
	MaxAddresses = 7
	MaxLabels    = 20

	maxVersion  = 2147483647
	maxText     = 2000
	maxNote     = 5000
	maxLabel    = 64
	maxInternal = 2000
)

var hash64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ConfigRef is a POL ConfigRef v1 {kind, id, revision}. Each field of QuoteData names the kind it
// takes.
type ConfigRef struct {
	Kind     string
	ID       string
	Revision int64
}

// PartyRef is PTY PartyRef v1 as a quote binds it {party_id, revision}: the customer at its
// confirmed revision.
type PartyRef struct {
	PartyID  string
	Revision int64
}

// ContactRef is a contact of the customer {contact_id, row_version}.
type ContactRef struct {
	ContactID  string
	RowVersion int64
}

// TradingProfileRef is PTY TradingProfileRef v1 {party_id, revision}: a published revision of the
// customer's trading profile.
type TradingProfileRef struct {
	PartyID  string
	Revision int64
}

// AddressRoleRef is PTY AddressRoleRef v1 {party_id, party_revision, address_id}.
type AddressRoleRef struct {
	PartyID       string
	PartyRevision int64
	AddressID     string
}

// RfqRef is INQ RfqRef v1 as a draft names it {rfq_id, row_version}.
type RfqRef struct {
	RfqID      string
	RowVersion int64
}

// CostSnapshotRef is CST CostSnapshotRef v1 {snapshot_id, result_hash}.
type CostSnapshotRef struct {
	SnapshotID string
	ResultHash string
}

// SkuRef is a CAT SKU at a specification revision {sku_id, spec_revision}.
type SkuRef struct {
	SkuID        string
	SpecRevision int64
}

// BusinessRef names a revision of a business object no Brant context owns yet {kind, id,
// revision}: a place (kind location) or a delivery anchor (kind delivery_anchor), compared as
// identities, as CST compares them.
type BusinessRef struct {
	Kind     string
	ID       string
	Revision int64
}

// Delivery is the customer's delivery requirement a quote states {kind, date, anchor_ref, days,
// day_basis, calendar_ref, milestone, place_ref, time_zone}, of CST's form: a milestone
// (goods_ready, handover or arrival) at place_ref (kind location) in an IANA time zone, either on a
// fixed date (kind fixed, YYYY-MM-DD) or days (0–3650) calendar or working days (day_basis) after
// the event anchor_ref (kind delivery_anchor) names, counted on the POL calendar calendar_ref (kind
// lead_time). The freeze requires it to be the adopted costing's delivery exactly.
type Delivery struct {
	Kind        string
	Date        string
	AnchorRef   *BusinessRef
	Days        int
	DayBasis    string
	CalendarRef *ConfigRef
	Milestone   string
	PlaceRef    BusinessRef
	TimeZone    string
}

// Evidence is an EVD source and a locator in it {source_id, locator}; the actor of the command
// reads the source (REFERENCE_INVALID at its source_id otherwise).
type Evidence struct {
	SourceID string
	Locator  string
}

// Line is one quoted line {line_id, rfq_line_id, sku_ref, quantity, unit_ref (unit),
// conversion_ref (unit_conversion, when the unit is not the costing's), base_quantity (optional),
// unit_price, price_basis_quantity, discount_ratio, tax_application_ref (tax_application, sales
// side), evidence}. Quantities are QuantityFormat strings (positive), prices and the discount
// PriceFormat strings (the discount below 1). BaseQuantity is the quantity in the costing's unit as
// the author states it ("" when not stated): the freeze converts the quantity anyway and refuses a
// stated one that differs (BASE_QUANTITY_MISMATCH, BC-AT04).
type Line struct {
	LineID             string
	RfqLineID          string
	Sku                SkuRef `json:"sku_ref"`
	Quantity           string
	UnitRef            ConfigRef
	ConversionRef      *ConfigRef
	BaseQuantity       string
	UnitPrice          string
	PriceBasisQuantity string
	DiscountRatio      string
	TaxApplicationRef  ConfigRef
	Evidence           []Evidence
}

// Charge is a sales charge {charge_id, fee_category_ref (fee_category, sales_charge side),
// obligation_key, amount, inclusion (separate or included_in_line), included_line_id (with
// included_in_line only), tax_application_ref, evidence}.
type Charge struct {
	ChargeID          string
	FeeCategoryRef    ConfigRef
	ObligationKey     string
	Amount            string
	Inclusion         string
	IncludedLineID    string
	TaxApplicationRef ConfigRef
	Evidence          []Evidence
}

// QuoteData is the content of a working revision {issuer_ref (company_profile of the company),
// customer_ref, contact_ref, trading_profile_ref, customer_address_refs, rfq_ref,
// cost_snapshot_ref, lines, charges, currency_ref (currency), pricing_policy_ref (pricing_policy),
// sales_fx_ref (fx_rate, when the quote currency is not the pricing policy's), trade_terms_ref
// (trade_terms), named_place_ref (a BusinessRef of kind location), payment_terms_ref
// (payment_terms), delivery (Delivery), document_template_ref (quote_document_template),
// sequence_ref (number_sequence), expires_at, customer_notes, evidence}. Every member is optional
// in a draft; quote.freeze needs all but sales_fx_ref, charges and customer_notes
// (DRAFT_NOT_READY, CUSTOMER_REFERENCE_MISSING, CONTACT_REFERENCE_MISSING or COST_SNAPSHOT_MISSING
// at the first missing one). Trade terms, named place, payment terms and delivery are the adopted
// costing's comparison basis (Python T03 A27). CustomerNotes are customer-visible and part of the
// business hash.
type QuoteData struct {
	IssuerRef           *ConfigRef
	CustomerRef         *PartyRef
	ContactRef          *ContactRef
	TradingProfileRef   *TradingProfileRef
	CustomerAddressRefs []AddressRoleRef
	RfqRef              *RfqRef
	CostSnapshotRef     *CostSnapshotRef
	Lines               []Line
	Charges             []Charge
	CurrencyRef         *ConfigRef
	PricingPolicyRef    *ConfigRef
	SalesFXRef          *ConfigRef
	TradeTermsRef       *ConfigRef
	NamedPlaceRef       *BusinessRef
	PaymentTermsRef     *ConfigRef
	Delivery            *Delivery
	DocumentTemplateRef *ConfigRef
	SequenceRef         *ConfigRef
	ExpiresAt           string
	CustomerNotes       *string
	Evidence            []Evidence
}

// SaveDraft creates a quote root with working revision 1 at ExpectedVersion 0, or replaces the
// working revision whole at the current root version (Python quote.save_draft). A root without a
// working revision is revised first (INVALID_STATE). Human or AI.
type SaveDraft struct {
	IdempotencyKey  string
	QuoteID         string
	ExpectedVersion int64
	Data            QuoteData
}

// Revise copies frozen revision BaseRevision into a new working revision (Python quote.revise).
// Human or AI.
type Revise struct {
	IdempotencyKey  string
	QuoteID         string
	ExpectedVersion int64
	BaseRevision    int64
}

// DiscardDraft ends working revision WorkingRevision with a reason and evidence (Python
// quote.discard_draft); the revision is kept and its number never reused. Human or AI.
type DiscardDraft struct {
	IdempotencyKey  string
	QuoteID         string
	ExpectedVersion int64
	WorkingRevision int64
	Reason          string
	Evidence        []Evidence
}

// Freeze seals working revision WorkingRevision (Python quote.freeze). Human only.
type Freeze struct {
	IdempotencyKey  string
	QuoteID         string
	ExpectedVersion int64
	WorkingRevision int64
}

// UpdateMetadata replaces the root's internal labels and note (BC-AT09). Neither is part of any
// revision or of the business hash. Human or AI.
type UpdateMetadata struct {
	IdempotencyKey  string
	QuoteID         string
	ExpectedVersion int64
	Labels          []string
	InternalNote    string
}

// Validate checks the closed contract.
func (c SaveDraft) Validate() error {
	if err := head(c.IdempotencyKey, c.QuoteID, c.ExpectedVersion, 0); err != nil {
		return err
	}
	return c.Data.Validate("/args/data")
}

// Validate checks the closed contract.
func (c Revise) Validate() error {
	if err := head(c.IdempotencyKey, c.QuoteID, c.ExpectedVersion, 1); err != nil {
		return err
	}
	return positive(c.BaseRevision, "/args/base_revision")
}

// Validate checks the closed contract.
func (c DiscardDraft) Validate() error {
	if err := head(c.IdempotencyKey, c.QuoteID, c.ExpectedVersion, 1); err != nil {
		return err
	}
	if err := positive(c.WorkingRevision, "/args/working_revision"); err != nil {
		return err
	}
	if !text(c.Reason, maxText) {
		return schema("/args/reason")
	}
	return evidence(c.Evidence, "/args/evidence")
}

// Validate checks the closed contract.
func (c Freeze) Validate() error {
	if err := head(c.IdempotencyKey, c.QuoteID, c.ExpectedVersion, 1); err != nil {
		return err
	}
	return positive(c.WorkingRevision, "/args/working_revision")
}

// Validate checks the closed contract: at most MaxLabels distinct labels of 1–64 characters, a note
// of at most 2000.
func (c UpdateMetadata) Validate() error {
	if err := head(c.IdempotencyKey, c.QuoteID, c.ExpectedVersion, 1); err != nil {
		return err
	}
	if len(c.Labels) > MaxLabels {
		return schema("/args/labels")
	}
	seen := map[string]bool{}
	for i, l := range c.Labels {
		if !text(l, maxLabel) || seen[l] {
			return schema(index("/args/labels", i))
		}
		seen[l] = true
	}
	if !utf8.ValidString(c.InternalNote) || utf8.RuneCountInString(c.InternalNote) > maxInternal {
		return schema("/args/internal_note")
	}
	return nil
}

// Validate checks the form of a draft at path.
func (d QuoteData) Validate(path string) error {
	checks := []func() error{
		func() error { return optionalRef(d.IssuerRef, path+"/issuer_ref") },
		func() error {
			if r := d.CustomerRef; r != nil {
				return idVersion(r.PartyID, r.Revision, path+"/customer_ref", "/party_id", "/revision")
			}
			return nil
		},
		func() error {
			if r := d.ContactRef; r != nil {
				return idVersion(r.ContactID, r.RowVersion, path+"/contact_ref", "/contact_id", "/row_version")
			}
			return nil
		},
		func() error {
			if r := d.TradingProfileRef; r != nil {
				return idVersion(r.PartyID, r.Revision, path+"/trading_profile_ref", "/party_id", "/revision")
			}
			return nil
		},
		func() error { return addresses(d.CustomerAddressRefs, path+"/customer_address_refs") },
		func() error {
			if r := d.RfqRef; r != nil {
				return idVersion(r.RfqID, r.RowVersion, path+"/rfq_ref", "/rfq_id", "/row_version")
			}
			return nil
		},
		func() error {
			if r := d.CostSnapshotRef; r != nil {
				switch {
				case !kernel.ValidIdentifier(r.SnapshotID):
					return schema(path + "/cost_snapshot_ref/snapshot_id")
				case !hash64.MatchString(r.ResultHash):
					return schema(path + "/cost_snapshot_ref/result_hash")
				}
			}
			return nil
		},
		func() error { return lines(d.Lines, path+"/lines") },
		func() error { return charges(d.Charges, path+"/charges") },
		func() error { return optionalRef(d.CurrencyRef, path+"/currency_ref") },
		func() error { return optionalRef(d.PricingPolicyRef, path+"/pricing_policy_ref") },
		func() error { return optionalRef(d.SalesFXRef, path+"/sales_fx_ref") },
		func() error { return optionalRef(d.TradeTermsRef, path+"/trade_terms_ref") },
		func() error {
			if r := d.NamedPlaceRef; r != nil {
				return business(*r, PlaceKind, path+"/named_place_ref")
			}
			return nil
		},
		func() error { return optionalRef(d.PaymentTermsRef, path+"/payment_terms_ref") },
		func() error {
			if d.Delivery != nil {
				return d.Delivery.validate(path + "/delivery")
			}
			return nil
		},
		func() error { return optionalRef(d.DocumentTemplateRef, path+"/document_template_ref") },
		func() error { return optionalRef(d.SequenceRef, path+"/sequence_ref") },
		func() error {
			if d.ExpiresAt != "" {
				if _, err := kernel.ParseTimestamp(d.ExpiresAt); err != nil {
					return schema(path + "/expires_at")
				}
			}
			return nil
		},
		func() error {
			if n := d.CustomerNotes; n != nil && (!utf8.ValidString(*n) || utf8.RuneCountInString(*n) > maxNote) {
				return schema(path + "/customer_notes")
			}
			return nil
		},
		func() error {
			if d.Evidence != nil {
				return evidence(d.Evidence, path+"/evidence")
			}
			return nil
		},
	}
	for _, check := range checks {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
}

func lines(ls []Line, path string) error {
	if ls == nil {
		return nil
	}
	if len(ls) < 1 || len(ls) > MaxLines {
		return schema(path)
	}
	for i, l := range ls {
		p := index(path, i)
		switch {
		case !kernel.ValidIdentifier(l.LineID):
			return schema(p + "/line_id")
		case !kernel.ValidIdentifier(l.RfqLineID):
			return schema(p + "/rfq_line_id")
		}
		if err := idVersion(l.Sku.SkuID, l.Sku.SpecRevision, p+"/sku_ref", "/sku_id", "/spec_revision"); err != nil {
			return err
		}
		if !decimal(l.Quantity, kernel.QuantityFormat, true) {
			return schema(p + "/quantity")
		}
		if err := ref(l.UnitRef, p+"/unit_ref"); err != nil {
			return err
		}
		if err := optionalRef(l.ConversionRef, p+"/conversion_ref"); err != nil {
			return err
		}
		if l.BaseQuantity != "" && !decimal(l.BaseQuantity, kernel.QuantityFormat, true) {
			return schema(p + "/base_quantity")
		}
		switch {
		case !decimal(l.UnitPrice, kernel.PriceFormat, false):
			return schema(p + "/unit_price")
		case !decimal(l.PriceBasisQuantity, kernel.QuantityFormat, true):
			return schema(p + "/price_basis_quantity")
		case !belowOne(l.DiscountRatio):
			return schema(p + "/discount_ratio")
		}
		if err := ref(l.TaxApplicationRef, p+"/tax_application_ref"); err != nil {
			return err
		}
		if err := evidence(l.Evidence, p+"/evidence"); err != nil {
			return err
		}
	}
	return nil
}

func charges(cs []Charge, path string) error {
	if len(cs) > MaxCharges {
		return schema(path)
	}
	for i, c := range cs {
		p := index(path, i)
		if !kernel.ValidIdentifier(c.ChargeID) {
			return schema(p + "/charge_id")
		}
		if err := ref(c.FeeCategoryRef, p+"/fee_category_ref"); err != nil {
			return err
		}
		switch {
		case !kernel.ValidIdentifier(c.ObligationKey):
			return schema(p + "/obligation_key")
		case !decimal(c.Amount, kernel.PriceFormat, true):
			return schema(p + "/amount")
		case c.Inclusion != InclusionSeparate && c.Inclusion != InclusionIncludedInLine:
			return schema(p + "/inclusion")
		case (c.Inclusion == InclusionIncludedInLine) != (c.IncludedLineID != ""),
			c.IncludedLineID != "" && !kernel.ValidIdentifier(c.IncludedLineID):
			return schema(p + "/included_line_id")
		}
		if err := ref(c.TaxApplicationRef, p+"/tax_application_ref"); err != nil {
			return err
		}
		if err := evidence(c.Evidence, p+"/evidence"); err != nil {
			return err
		}
	}
	return nil
}

func addresses(as []AddressRoleRef, path string) error {
	if as == nil {
		return nil
	}
	if len(as) < 1 || len(as) > MaxAddresses {
		return schema(path)
	}
	for i, a := range as {
		p := index(path, i)
		if err := idVersion(a.PartyID, a.PartyRevision, p, "/party_id", "/party_revision"); err != nil {
			return err
		}
		if !kernel.ValidIdentifier(a.AddressID) {
			return schema(p + "/address_id")
		}
	}
	return nil
}

// Receipt is the closed success answer of a command. ObjectID is the quote ID. For the quote
// commands RowVersion is the root version and Status the status of the revision the command wrote
// (StatusUpdated for quote.metadata.update); for the object control commands RowVersion is the
// control epoch and Status the control state after the command. GetQuote shows the revision
// numbers.
type Receipt struct {
	ActionID   string
	ObjectID   string
	RowVersion int64
	Status     string
	Replayed   bool
}

// FreezeReceipt is the answer of quote.freeze: the receipt and what the freeze sealed. A replay
// rebuilds it from the stored revision.
type FreezeReceipt struct {
	Receipt
	BusinessRevision int64
	BusinessHash     string
	IssuedAt         string // kernel.FormatTimestamp
	ExpiresAt        string
}

// Auditor records a request an inbound adapter refused before it became a command, such as a body
// that is not valid JSON, in the context's audit trail. The principal in ctx is the requester.
type Auditor interface {
	AuditRejected(ctx context.Context, command, objectID, code string) error
}

// Commands is TRD's command entry point for QuoteRoot. Errors are *kernel.Rejection for refusals.
// An error that is application.ErrCommitUncertain from IDP-Bento-Go means the outcome is unknown:
// the caller sends the same command with the same idempotency key again (or reconciles), which
// returns the stored receipt if the command committed. A write on a root bound to a task carries the
// task's control claim in its context (WithControl).
type Commands interface {
	SaveDraft(ctx context.Context, cmd SaveDraft) (Receipt, error)
	Revise(ctx context.Context, cmd Revise) (Receipt, error)
	DiscardDraft(ctx context.Context, cmd DiscardDraft) (Receipt, error)
	Freeze(ctx context.Context, cmd Freeze) (FreezeReceipt, error)
	UpdateMetadata(ctx context.Context, cmd UpdateMetadata) (Receipt, error)
	BindControl(ctx context.Context, cmd BindControl) (Receipt, error)
	StopControl(ctx context.Context, cmd StopControl) (Receipt, error)
	ReleaseControl(ctx context.Context, cmd ReleaseControl) (Receipt, error)
	SubmitReview(ctx context.Context, cmd SubmitReview) (ReviewReceipt, error)
	DecideReview(ctx context.Context, cmd DecideReview) (ReviewReceipt, error)
	RevokeApproval(ctx context.Context, cmd RevokeApproval) (ReviewReceipt, error)
	Activate(ctx context.Context, cmd Activate) (FormalizationReceipt, error)
	Replace(ctx context.Context, cmd Replace) (FormalizationReceipt, error)
	Withdraw(ctx context.Context, cmd Withdraw) (RevisionReceipt, error)
	Cancel(ctx context.Context, cmd Cancel) (RevisionReceipt, error)
	AcceptanceCommands
}

// Queries is TRD's audited read side.
type Queries interface {
	// GetQuote returns a quote root the caller sees with all its revisions, reviews and approvals,
	// audited as quote.get (trd.read). A frozen revision's commercial document is checked against
	// its business hash (BUSINESS_HASH_MISMATCH for a corrupt one).
	GetQuote(ctx context.Context, quoteID string) (QuoteView, error)
	// CheckApproval says whether an approval releases its revision now, audited as
	// quote.approval.check (trd.read): the check quote.activate and quote.replace make in their
	// transaction (review.go), run in a read transaction. It changes nothing.
	CheckApproval(ctx context.Context, quoteID, approvalID string) (ApprovalCheck, error)
	// GetFormalDocument returns the formal document of a revision with its bytes, audited as
	// quote.document.get (trd.read). The bytes are verified against the binding on every read.
	GetFormalDocument(ctx context.Context, quoteID string, businessRevision int64) (FormalDocument, error)
	// GetActiveRevision returns the root's active revision as Lookups describe it, audited as
	// quote.active.get (trd.read); NO_ACTIVE_REVISION when there is none.
	GetActiveRevision(ctx context.Context, quoteID string) (FormalRevision, error)
	Audit(ctx context.Context) ([]kernel.AuditEntry, error)
	AcceptanceQueries
	ReadQueries
}

// Command is one of TRD's commands.
type Command interface{ trdCommand() }

func (SaveDraft) trdCommand()      {}
func (Revise) trdCommand()         {}
func (DiscardDraft) trdCommand()   {}
func (Freeze) trdCommand()         {}
func (UpdateMetadata) trdCommand() {}
func (BindControl) trdCommand()    {}
func (StopControl) trdCommand()    {}
func (ReleaseControl) trdCommand() {}
func (SubmitReview) trdCommand()   {}
func (DecideReview) trdCommand()   {}
func (RevokeApproval) trdCommand() {}
func (Activate) trdCommand()       {}
func (Replace) trdCommand()        {}
func (Withdraw) trdCommand()       {}
func (Cancel) trdCommand()         {}

// Reconciliation is what became of a command whose answer the caller lost.
type Reconciliation struct {
	State    kernel.ReconcileState
	ActionID string   // committed and payload_conflict
	ObjectID string   // committed and payload_conflict
	Receipt  *Receipt // the stored receipt; committed only
}

// Reconciler recovers from an unknown outcome without executing anything (contract 06, 动作对账;
// Python t03_reconciliation). The caller passes the command exactly as it sent it, idempotency key
// included. Reconciling needs trd.read, not the command's own permission, and re-checks it and the
// caller's data scope now; it stays available while the root is bound.
type Reconciler interface {
	Reconcile(ctx context.Context, cmd Command) (Reconciliation, error)
}

func head(key, id string, expected, minVersion int64) error {
	switch {
	case !kernel.ValidIdentifier(key):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(id):
		return schema("/args/quote_id")
	case expected < minVersion || expected > maxVersion:
		return schema("/args/expected_version")
	}
	return nil
}

func positive(n int64, path string) error {
	if n < 1 || n > maxVersion {
		return schema(path)
	}
	return nil
}

func idVersion(id string, version int64, path, idField, versionField string) error {
	if !kernel.ValidIdentifier(id) {
		return schema(path + idField)
	}
	return positive(version, path+versionField)
}

func ref(r ConfigRef, path string) error {
	switch {
	case !kernel.ValidIdentifier(r.Kind):
		return schema(path + "/kind")
	case !kernel.ValidIdentifier(r.ID):
		return schema(path + "/id")
	}
	return positive(r.Revision, path+"/revision")
}

func optionalRef(r *ConfigRef, path string) error {
	if r == nil {
		return nil
	}
	return ref(*r, path)
}

// business checks a BusinessRef of kind at path.
func business(r BusinessRef, kind, path string) error {
	switch {
	case r.Kind != kind:
		return schema(path + "/kind")
	case !kernel.ValidIdentifier(r.ID):
		return schema(path + "/id")
	}
	return positive(r.Revision, path+"/revision")
}

var milestones = []string{MilestoneGoodsReady, MilestoneHandover, MilestoneArrival}

// validate checks the closed form of a delivery at p (CST api.Delivery's rules).
func (d Delivery) validate(p string) error {
	if !slices.Contains(milestones, d.Milestone) {
		return schema(p + "/milestone")
	}
	if !text(d.TimeZone, maxTimeZone) {
		return schema(p + "/time_zone")
	}
	if err := business(d.PlaceRef, PlaceKind, p+"/place_ref"); err != nil {
		return err
	}
	switch d.Kind {
	case DeliveryFixed:
		if _, err := time.Parse(time.DateOnly, d.Date); err != nil {
			return schema(p + "/date")
		}
		if d.AnchorRef != nil || d.Days != 0 || d.DayBasis != "" || d.CalendarRef != nil {
			return schema(p)
		}
		return nil
	case DeliveryLeadTime:
	default:
		return schema(p + "/kind")
	}
	switch {
	case d.Date != "":
		return schema(p + "/date")
	case d.AnchorRef == nil:
		return schema(p + "/anchor_ref")
	case d.Days < 0 || d.Days > maxLeadDays:
		return schema(p + "/days")
	case d.DayBasis != DayBasisCalendar && d.DayBasis != DayBasisWorking:
		return schema(p + "/day_basis")
	case d.CalendarRef == nil:
		return schema(p + "/calendar_ref")
	}
	if err := business(*d.AnchorRef, AnchorKind, p+"/anchor_ref"); err != nil {
		return err
	}
	return ref(*d.CalendarRef, p+"/calendar_ref")
}

func evidence(es []Evidence, path string) error {
	if len(es) < 1 || len(es) > MaxEvidence {
		return schema(path)
	}
	for i, e := range es {
		p := index(path, i)
		switch {
		case !kernel.ValidIdentifier(e.SourceID):
			return schema(p + "/source_id")
		case !text(e.Locator, maxText):
			return schema(p + "/locator")
		}
	}
	return nil
}

// decimal reports whether s is a decimal of format f, positive when positive is set.
func decimal(s string, f kernel.DecimalFormat, positive bool) bool {
	d, err := kernel.ParseDecimal(s, f)
	return err == nil && (!positive || d.Sign() > 0)
}

// belowOne reports whether s is a PriceFormat ratio in [0, 1).
func belowOne(s string) bool {
	d, err := kernel.ParseDecimal(s, kernel.PriceFormat)
	return err == nil && d.Cmp(kernel.DecimalFromInt(1)) < 0
}

func schema(path string) error { return kernel.Reject(kernel.CodeSchemaInvalid, path, "") }

func text(s string, max int) bool {
	n := utf8.RuneCountInString(s)
	return n >= 1 && n <= max && utf8.ValidString(s)
}

func index(path string, i int) string { return path + "/" + strconv.Itoa(i) }
