package api

import (
	"context"
	"slices"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Active sales orders: amendments, cancellation and closes (WBS S3-15; tactical model
// TRD-AGG-SalesOrder, SM-TRD-ORDER after activation, SM-TRD-ORDER-AMENDMENT; TRD-INV-016, 018, 019;
// UL-INV-21, UL-INV-27; event storming ES-09, SIM-02 step 7, TD-AT01, TD-AT02). Python has none of
// this.
//
//	order.amendment.prepare   human or AI: amendment amendment_id of active order order_id, new
//	                          (expected_version 0) or a draft again (its version), against the order's
//	                          current revision (expected_order_revision and expected_order_hash;
//	                          ORDER_AMENDMENT_STALE otherwise), from a source (the customer's
//	                          response, a revision of the customer's PO or our proposal the customer
//	                          confirmed), proposing the complete content of the next revision (lines,
//	                          charges, terms; QUANTITY_BELOW_SHIPPED for less than the shipments), with
//	                          the impact assessment so far. Every amount of the next revision is
//	                          recalculated by the server; the receipt names its preview hash.
//	order.amendment.submit    human: draft → pending_approval; the impact assessment covers purchasing,
//	                          materials, operations, quality, packing, booking and the payment plan
//	                          (DRAFT_NOT_READY at /impact/<area>).
//	order.amendment.decide    human, independent of every preparer and the submitter
//	                          (REVIEWER_NOT_INDEPENDENT), holding trd.read now: approve or reject (with a
//	                          reason), restating amendment_hash (ORDER_REVIEW_MISMATCH).
//	order.amendment.apply     human: approved → applied while the order is still at the revision the
//	                          amendment binds (ORDER_AMENDMENT_STALE: of two, the first applied wins) and
//	                          the decider is still qualified (ORDER_APPROVAL_REQUIRED). The order moves
//	                          to its next revision and row version; OrderGuard v1 then answers
//	                          REFERENCE_STALE to every binding of the old revision.
//	order.amendment.withdraw  human: a draft or pending amendment, with a reason.
//	order.cancel              human: also cancels an active order with nothing shipped
//	                          (ORDER_HAS_SHIPMENTS) and nothing open downstream without an approved
//	                          disposition (DOWNSTREAM_COMMITMENT_OPEN).
//	order.close_remainder     human: closes an active order short, something shipped, with a reason.
//	order.close               human: closes an active order every line of which shipped its quantity
//	                          (ORDER_NOT_FULFILLED).
//
// Each locks the quote root FOR UPDATE, passes its object control, then locks the order FOR UPDATE.
// The amendment commands take the amendment's own expected_version and leave the order's row version
// alone, except apply, which moves it; the close commands take the order's. Since S5 (unit C4a,
// D-1) the end of an active order and the amendment's prepare and apply read, inside their
// transaction after the order lock and without locks, what FUL confirmed (ShipmentFacts v1:
// ORDER_HAS_SHIPMENTS, ORDER_NOT_FULFILLED with zero tolerance (D-19), close_remainder's "something
// shipped", QUANTITY_BELOW_SHIPPED) and, for cancel and close_remainder, what PUR lists against the
// order (DownstreamCommitments v1: DOWNSTREAM_COMMITMENT_OPEN for a prepared draft, a commitment
// requested, approved, issued or confirmed, or goods allocated beyond what the line shipped, each
// without an approved disposition; a fulfilled commitment is not open). STK is not_enabled; STL's
// credit reservations are released by STL when the order ends (PL-13). Prepare also pre-fills the
// purchasing, quality, packing, booking and payment_plan areas a preparer left unassessed as open
// items with the facts of PUR, FUL and STL. Since S5 (unit B4, D-9) apply asks STL's
// CreditGuard v1 for the revision it produces when that revision asks more credit than the
// current one (CREDIT_RESERVATION_REQUIRED at /args/amendment_id; STL reserves it beforehand from
// OrderSnapshotFact.Proposed), and records trade compliance as GuardNotCheckedAtApply.
//
// Request members in snake_case: order.amendment.prepare {order_id, amendment_id, expected_version,
// expected_order_revision, expected_order_hash, source {kind, response_id, evidence}, lines
// [{line_id, sku_ref, quantity, unit_ref, conversion_ref, unit_price, price_basis_quantity,
// discount_ratio, tax_application_ref}], charges [{charge_id, amount}], terms {trade_terms_ref,
// named_place_ref, payment_terms_ref, delivery}, impact [{area, status, note, evidence}]};
// order.amendment.submit and order.amendment.apply {order_id, amendment_id, expected_version};
// order.amendment.decide {order_id, amendment_id, expected_version, amendment_hash, decision, reason,
// evidence}; order.amendment.withdraw {order_id, amendment_id, expected_version, reason, evidence};
// order.close_remainder and order.close {order_id, expected_version, reason, evidence} (close: reason
// optional).
const (
	CommandPrepareAmendment    = "order.amendment.prepare"
	CommandSubmitAmendment     = "order.amendment.submit"
	CommandDecideAmendment     = "order.amendment.decide"
	CommandApplyAmendment      = "order.amendment.apply"
	CommandWithdrawAmendment   = "order.amendment.withdraw"
	CommandCloseOrderRemainder = "order.close_remainder"
	CommandCloseOrder          = "order.close"

	// EventOrderAmendmentRequested and EventOrderAmendmentDecided are the tactical model's
	// OrderAmendmentRequested (at every preparation, ES-09 step 2) and OrderAmendmentDecided; the event
	// catalog V0.1 does not list them yet. EventOrderAmendmentWithdrawn is beyond both.
	EventOrderAmendmentRequested = "idp-brant.trd.order_amendment_requested.v1"
	EventOrderAmendmentDecided   = "idp-brant.trd.order_amendment_decided.v1"
	EventOrderAmendmentWithdrawn = "idp-brant.trd.order_amendment_withdrawn.v1"
	EventOrderAmended            = "idp-brant.trd.order_amended.v1"
	EventOrderRemainderClosed    = "idp-brant.trd.order_remainder_closed.v1"
	EventOrderClosed             = "idp-brant.trd.order_closed.v1"

	// Amendment states (SM-TRD-ORDER-AMENDMENT).
	AmendmentDraft           = "draft"
	AmendmentPendingApproval = "pending_approval"
	AmendmentApproved        = "approved"
	AmendmentApplied         = "applied"
	AmendmentRejected        = "rejected"
	AmendmentWithdrawn       = "withdrawn"

	// Kinds of an amendment's source.
	SourceCustomerResponse   = "customer_response"
	SourceCustomerPORevision = "customer_po_revision"
	SourceConfirmedProposal  = "confirmed_proposal"

	// What an impact assessment says of an area.
	ImpactUnaffected = "unaffected"
	ImpactAffected   = "affected"
	ImpactOpen       = "open"

	// GuardFULShipments and GuardDownstream name, by their contracts, where the active order's
	// commands read FUL's confirmed shipments (ShipmentFacts v1, Context Map R70) and PUR's open
	// drafts, commitments and demand allocations (DownstreamCommitments v1, R71): the commit
	// evidence's fulfilment_source, the end's fulfilment_source and downstream_source.
	// GuardSTKReservations names STK's stock reservations, which TRD records as NotEnabled (S5 D-10:
	// no stock route before STK). NotWired is the answer of a TRD assembled without a read (unit
	// tests only; trd.New refuses it).
	GuardFULShipments    = "ful.shipment_facts.v1"
	GuardDownstream      = "pur.downstream_commitments.v1"
	GuardSTKReservations = "stk.stock_reservations.v1"
	NotWired             = "not_wired"
	NotEnabled           = "not_enabled"

	maxImpactNote = 2000
)

// ImpactAreas are the seven areas an amendment's impact assessment covers, in its order.
func ImpactAreas() []string { return slices.Clone(impactAreas) }

var impactAreas = []string{"purchasing", "materials", "operations", "quality", "packing", "booking", "payment_plan"}

// Codes of active sales orders and their documents (error catalog V0.1, rows whose contexts include
// TRD), with their categories.
const (
	CodeOrderAmendmentStale      = "ORDER_AMENDMENT_STALE"      // 409: the order is no longer at the revision the amendment binds
	CodeQuantityBelowShipped     = "QUANTITY_BELOW_SHIPPED"     // a line would hold less than its confirmed shipments
	CodeOrderHasShipments        = "ORDER_HAS_SHIPMENTS"        // 409: the order has confirmed shipments; close the remainder instead
	CodeDownstreamCommitmentOpen = "DOWNSTREAM_COMMITMENT_OPEN" // 409: a downstream commitment is open without an approved disposition
	CodeOrderNotFulfilled        = "ORDER_NOT_FULFILLED"        // 409: the shipments do not reach the order quantity
	CodeOrderDocumentStale       = "ORDER_DOCUMENT_STALE"       // 409: an S/C or PI binds another order revision than the current one
	CodeDocumentIntegrityFailure = "DOCUMENT_INTEGRITY_FAILURE" // an order document's stored bytes are not the bound ones
)

var amendmentCodes = map[string]kernel.Category{
	CodeOrderAmendmentStale:      kernel.CategoryConflict,
	CodeQuantityBelowShipped:     kernel.CategoryInvalid,
	CodeOrderHasShipments:        kernel.CategoryConflict,
	CodeDownstreamCommitmentOpen: kernel.CategoryConflict,
	CodeOrderNotFulfilled:        kernel.CategoryConflict,
	CodeOrderDocumentStale:       kernel.CategoryConflict,
	CodeDocumentIntegrityFailure: kernel.CategoryInvalid,
}

// AmendmentSource is what a change comes from {kind, response_id, evidence}: a customer response TRD
// recorded (response_id, evidence optional), or a revision of the customer's PO or our proposal the
// customer confirmed (evidence, 1–200 sources the actor reads).
type AmendmentSource struct {
	Kind       string
	ResponseID string
	Evidence   []Evidence
}

// AmendmentLine is one line the next revision holds {line_id, sku_ref, quantity, unit_ref,
// conversion_ref, unit_price, price_basis_quantity, discount_ratio, tax_application_ref}: a line of
// the current revision by its ID, or a new one. Quantities are QuantityFormat strings, prices and the
// discount PriceFormat strings.
type AmendmentLine struct {
	LineID             string
	Sku                SkuRef `json:"sku_ref"`
	Quantity           string
	UnitRef            ConfigRef
	ConversionRef      *ConfigRef
	UnitPrice          string
	PriceBasisQuantity string
	DiscountRatio      string
	TaxApplicationRef  ConfigRef
}

// AmendmentCharge is a charge of the current revision the next one keeps {charge_id, amount}.
type AmendmentCharge struct {
	ChargeID string
	Amount   string
}

// AmendmentTerms are the next revision's terms {trade_terms_ref, named_place_ref, payment_terms_ref,
// delivery}.
type AmendmentTerms struct {
	TradeTermsRef   ConfigRef
	NamedPlaceRef   BusinessRef
	PaymentTermsRef ConfigRef
	Delivery        Delivery
}

// ImpactAssessment assesses one area {area, status (unaffected, affected or open), note (required
// unless unaffected), evidence}.
type ImpactAssessment struct {
	Area     string
	Status   string
	Note     string
	Evidence []Evidence
}

// PrepareAmendment prepares amendment AmendmentID of active order OrderID. Human or AI.
type PrepareAmendment struct {
	IdempotencyKey        string
	OrderID               string
	AmendmentID           string
	ExpectedVersion       int64 // 0 for a new amendment
	ExpectedOrderRevision int64
	ExpectedOrderHash     string
	Source                AmendmentSource
	Lines                 []AmendmentLine
	Charges               []AmendmentCharge
	Terms                 AmendmentTerms
	Impact                []ImpactAssessment
}

// SubmitAmendment submits draft amendment AmendmentID for approval. Human only.
type SubmitAmendment struct {
	IdempotencyKey  string
	OrderID         string
	AmendmentID     string
	ExpectedVersion int64
}

// DecideAmendment approves or rejects pending amendment AmendmentID at AmendmentHash; a rejection
// gives a reason. Human only.
type DecideAmendment struct {
	IdempotencyKey  string
	OrderID         string
	AmendmentID     string
	ExpectedVersion int64
	AmendmentHash   string
	Decision        string // approve or reject
	Reason          string
	Evidence        []Evidence
}

// ApplyAmendment applies approved amendment AmendmentID. Human only.
type ApplyAmendment struct {
	IdempotencyKey  string
	OrderID         string
	AmendmentID     string
	ExpectedVersion int64
}

// WithdrawAmendment withdraws draft or pending amendment AmendmentID with a reason. Human only.
type WithdrawAmendment struct {
	IdempotencyKey  string
	OrderID         string
	AmendmentID     string
	ExpectedVersion int64
	Reason          string
	Evidence        []Evidence
}

// CloseOrderRemainder closes active order OrderID short, with a reason. Human only.
type CloseOrderRemainder struct {
	IdempotencyKey  string
	OrderID         string
	ExpectedVersion int64
	Reason          string
	Evidence        []Evidence
}

// CloseOrder closes fulfilled active order OrderID; the reason is optional. Human only.
type CloseOrder struct {
	IdempotencyKey  string
	OrderID         string
	ExpectedVersion int64
	Reason          string
	Evidence        []Evidence
}

func amendmentHead(key, order, amendment string) error {
	switch {
	case !kernel.ValidIdentifier(key):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(order):
		return schema("/args/order_id")
	case !kernel.ValidIdentifier(amendment):
		return schema("/args/amendment_id")
	}
	return nil
}

// Validate checks the closed contract.
func (c PrepareAmendment) Validate() error {
	if err := amendmentHead(c.IdempotencyKey, c.OrderID, c.AmendmentID); err != nil {
		return err
	}
	switch {
	case c.ExpectedVersion < 0:
		return schema("/args/expected_version")
	case c.ExpectedOrderRevision < 1:
		return schema("/args/expected_order_revision")
	case !hash64.MatchString(c.ExpectedOrderHash):
		return schema("/args/expected_order_hash")
	}
	s := c.Source
	switch s.Kind {
	case SourceCustomerResponse:
		if !kernel.ValidIdentifier(s.ResponseID) {
			return schema("/args/source/response_id")
		}
	case SourceCustomerPORevision, SourceConfirmedProposal:
		if s.ResponseID != "" || len(s.Evidence) == 0 {
			return schema("/args/source")
		}
	default:
		return schema("/args/source/kind")
	}
	if len(s.Evidence) > 0 {
		if err := evidence(s.Evidence, "/args/source/evidence"); err != nil {
			return err
		}
	}
	if len(c.Lines) < 1 || len(c.Lines) > MaxLines {
		return schema("/args/lines")
	}
	for i, l := range c.Lines {
		p := index("/args/lines", i)
		if !kernel.ValidIdentifier(l.LineID) {
			return schema(p + "/line_id")
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
		switch {
		case !decimal(l.UnitPrice, kernel.PriceFormat, true):
			return schema(p + "/unit_price")
		case !decimal(l.PriceBasisQuantity, kernel.PriceFormat, true):
			return schema(p + "/price_basis_quantity")
		case !belowOne(l.DiscountRatio):
			return schema(p + "/discount_ratio")
		}
		if err := ref(l.TaxApplicationRef, p+"/tax_application_ref"); err != nil {
			return err
		}
	}
	if len(c.Charges) > MaxCharges {
		return schema("/args/charges")
	}
	for i, ch := range c.Charges {
		p := index("/args/charges", i)
		switch {
		case !kernel.ValidIdentifier(ch.ChargeID):
			return schema(p + "/charge_id")
		case !decimal(ch.Amount, kernel.PriceFormat, true):
			return schema(p + "/amount")
		}
	}
	t := c.Terms
	if err := ref(t.TradeTermsRef, "/args/terms/trade_terms_ref"); err != nil {
		return err
	}
	if err := business(t.NamedPlaceRef, PlaceKind, "/args/terms/named_place_ref"); err != nil {
		return err
	}
	if err := ref(t.PaymentTermsRef, "/args/terms/payment_terms_ref"); err != nil {
		return err
	}
	if err := t.Delivery.validate("/args/terms/delivery"); err != nil {
		return err
	}
	if len(c.Impact) > len(impactAreas) {
		return schema("/args/impact")
	}
	for i, a := range c.Impact {
		p := index("/args/impact", i)
		switch {
		case !slices.Contains(impactAreas, a.Area):
			return schema(p + "/area")
		case a.Status != ImpactUnaffected && a.Status != ImpactAffected && a.Status != ImpactOpen:
			return schema(p + "/status")
		case a.Note != "" && !text(a.Note, maxImpactNote):
			return schema(p + "/note")
		}
		if len(a.Evidence) > 0 {
			if err := evidence(a.Evidence, p+"/evidence"); err != nil {
				return err
			}
		}
	}
	return nil
}

// Validate checks the closed contract.
func (c SubmitAmendment) Validate() error {
	if err := amendmentHead(c.IdempotencyKey, c.OrderID, c.AmendmentID); err != nil {
		return err
	}
	return positive(c.ExpectedVersion, "/args/expected_version")
}

// Validate checks the closed contract: approve, or reject with a reason; 0–200 evidence entries.
func (c DecideAmendment) Validate() error {
	if err := amendmentHead(c.IdempotencyKey, c.OrderID, c.AmendmentID); err != nil {
		return err
	}
	if err := positive(c.ExpectedVersion, "/args/expected_version"); err != nil {
		return err
	}
	switch {
	case !hash64.MatchString(c.AmendmentHash):
		return schema("/args/amendment_hash")
	case c.Decision != DecisionApprove && c.Decision != DecisionReject:
		return schema("/args/decision")
	case c.Decision == DecisionReject && !text(c.Reason, maxText):
		return schema("/args/reason")
	case c.Reason != "" && !text(c.Reason, maxText):
		return schema("/args/reason")
	}
	if len(c.Evidence) > 0 {
		return evidence(c.Evidence, "/args/evidence")
	}
	return nil
}

// Validate checks the closed contract.
func (c ApplyAmendment) Validate() error {
	if err := amendmentHead(c.IdempotencyKey, c.OrderID, c.AmendmentID); err != nil {
		return err
	}
	return positive(c.ExpectedVersion, "/args/expected_version")
}

// Validate checks the closed contract: a reason and 0–200 evidence entries.
func (c WithdrawAmendment) Validate() error {
	if err := amendmentHead(c.IdempotencyKey, c.OrderID, c.AmendmentID); err != nil {
		return err
	}
	if err := positive(c.ExpectedVersion, "/args/expected_version"); err != nil {
		return err
	}
	if !text(c.Reason, maxText) {
		return schema("/args/reason")
	}
	if len(c.Evidence) > 0 {
		return evidence(c.Evidence, "/args/evidence")
	}
	return nil
}

// Validate checks the closed contract: a reason (reasonText) and 0–200 evidence entries.
func (c CloseOrderRemainder) Validate() error {
	if err := orderHead(c.IdempotencyKey, c.OrderID, c.ExpectedVersion); err != nil {
		return err
	}
	if !reasonText(c.Reason, maxText) {
		return schema("/args/reason")
	}
	if len(c.Evidence) > 0 {
		return evidence(c.Evidence, "/args/evidence")
	}
	return nil
}

// Validate checks the closed contract: the reason is optional; a given one is a reasonText.
func (c CloseOrder) Validate() error {
	if err := orderHead(c.IdempotencyKey, c.OrderID, c.ExpectedVersion); err != nil {
		return err
	}
	if c.Reason != "" && !reasonText(c.Reason, maxText) {
		return schema("/args/reason")
	}
	if len(c.Evidence) > 0 {
		return evidence(c.Evidence, "/args/evidence")
	}
	return nil
}

func (PrepareAmendment) trdCommand()    {}
func (SubmitAmendment) trdCommand()     {}
func (DecideAmendment) trdCommand()     {}
func (ApplyAmendment) trdCommand()      {}
func (WithdrawAmendment) trdCommand()   {}
func (CloseOrderRemainder) trdCommand() {}
func (CloseOrder) trdCommand()          {}

// AmendmentReceipt is the answer of the amendment commands: the receipt (ObjectID the order ID,
// RowVersion the amendment's version, Status its state) and what it binds — its ID and hash, the
// order revision it amends, the order's current revision, hash, state and row version — and, for a
// preparation, the revision it would yield (PreviewRevision, PreviewHash). A replay rebuilds it from
// the stored order.
type AmendmentReceipt struct {
	Receipt
	AmendmentID     string
	AmendmentHash   string
	BaseRevision    int64
	BaseHash        string
	OrderRevision   int64
	OrderHash       string
	OrderStatus     string
	OrderRowVersion int64
	PreviewRevision int64
	PreviewHash     string
}

// OrderRevisionView is an order revision after the first: its number and hash, the revision it
// amends, the amendment that produced it, the order's row version it became current at and when.
type OrderRevisionView struct {
	OrderRevision int64
	OrderHash     string
	PreviousHash  string
	AmendmentID   string
	AmendmentHash string
	RowVersion    int64
	At            time.Time
}

// OrderAmendmentView is an order amendment: its state and version, the revision it binds, its hash
// and content (canonical JSON, the bytes the hash covers), who prepared, submitted, decided, applied
// or withdrew it, the open items of its impact assessment, and whether the order has moved past the
// revision it binds (Stale).
type OrderAmendmentView struct {
	AmendmentID     string
	Version         int64
	Status          string
	BaseRevision    int64
	BaseHash        string
	AmendmentHash   string
	Content         string
	Preparers       []kernel.ActorRef
	SubmittedBy     kernel.ActorRef
	SubmittedAt     time.Time
	DecidedBy       kernel.ActorRef
	DecidedAt       time.Time
	Reason          string
	AppliedBy       kernel.ActorRef
	AppliedAt       time.Time
	AppliedRevision int64
	WithdrawnBy     kernel.ActorRef
	WithdrawnAt     time.Time
	WithdrawReason  string
	OpenItems       []string
	Stale           bool
}

// OrderEndView is how an active order ended: the command and state, the revision it ended at, each
// line's ordered, shipped and given-up quantity, where the shipments and downstream commitments
// came from (GuardFULShipments, GuardDownstream; GuardNotRequired for order.close's downstream;
// not_wired for an end recorded before S5 C4a), who, when, why.
type OrderEndView struct {
	Command          string
	Status           string
	OrderRevision    int64
	OrderHash        string
	Lines            []OrderEndLine
	FulfilmentSource string
	DownstreamSource string
	EndedBy          kernel.ActorRef
	EndedAt          time.Time
	Reason           string
	Evidence         []Evidence
	RowVersion       int64
}

// OrderEndLine is one line at an order's end.
type OrderEndLine struct {
	LineID    string
	Unit      string
	Ordered   string
	Shipped   string
	Remainder string
}

// ActiveOrderCommands is the command entry point of active sales orders. Errors are as Commands
// describes them. It is not part of Commands until the transport routes it.
type ActiveOrderCommands interface {
	PrepareAmendment(ctx context.Context, cmd PrepareAmendment) (AmendmentReceipt, error)
	SubmitAmendment(ctx context.Context, cmd SubmitAmendment) (AmendmentReceipt, error)
	DecideAmendment(ctx context.Context, cmd DecideAmendment) (AmendmentReceipt, error)
	ApplyAmendment(ctx context.Context, cmd ApplyAmendment) (AmendmentReceipt, error)
	WithdrawAmendment(ctx context.Context, cmd WithdrawAmendment) (AmendmentReceipt, error)
	CloseOrderRemainder(ctx context.Context, cmd CloseOrderRemainder) (OrderReceipt, error)
	CloseOrder(ctx context.Context, cmd CloseOrder) (OrderReceipt, error)
}
