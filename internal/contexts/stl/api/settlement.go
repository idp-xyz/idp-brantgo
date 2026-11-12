package api

import (
	"context"
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Settlement statements, business receivables and payables (S5 unit B2b, WBS S5-04b; tactical model
// STL-AGG-SettlementStatement, STL-AGG-Application; SM-STL-SETTLEMENT; STL-INV-001, STL-INV-002,
// STL-INV-003; UL-INV-22, UL-INV-23; Context Map R30 ShipmentLineRef v1, R31 PurchaseSettlementSource
// v1; baseline 4.2, 4.3, 4.6 S-A; S5 plan D-3, D-4, D-5).
//
//	settlement.confirm  human only (treasury): confirms a settlement statement. A sales statement
//	                    (kind sales) settles confirmed FUL shipment lines of one order — sources
//	                    [{shipment_id, line_id}] — at the order line's price at the revision the
//	                    shipment bound, and forms a business receivable of their payable amounts, due
//	                    in installments that follow the order account's payment stages (the order's
//	                    settled amount fills the stages in plan order; a stage due on order
//	                    confirmation or before shipment is due when the first of the goods left, one
//	                    due after shipment that many calendar days later, one due after invoice that
//	                    many days after the statement; what exceeds the plan is due with its last
//	                    stage). The order's standing stage applications (prepayments) move to the
//	                    receivable's installments of their stages (application state moved, replaced
//	                    by applications of the same fund to the receivable and, for a part that does
//	                    not fit, on the stage), so the stages still read paid. A purchase statement
//	                    (kind purchase) settles PUR's verified supplier delivery lines of one supplier
//	                    in one currency — sources [{delivery_id, line_id, version}], line_id being the
//	                    order line the commitment line buys — each valued at its share of the
//	                    commitment line's amounts (verified ÷ purchased, HALF_UP, net and tax apart),
//	                    and forms a business payable due at confirmation. A source line settled by a
//	                    standing statement is SETTLEMENT_SOURCE_DUPLICATE (of two statements settling
//	                    it at once one wins); evidence_ids (optional, EVD sources) are the supplier's
//	                    or customer's documents the statement rests on.
//
// application.apply also targets {kind: "receivable", statement_id, stage_id?} — an installment of a
// sales statement's receivable: of stage_id, or the first one still open — and {kind: "payable",
// statement_id}; the fund is a receipt of the statement's customer, or a payment to its supplier, in
// its currency. application.unapply undoes those as any other; a moved stage application is
// INVALID_STATE (unapply its replacement on the receivable).
//
// Request members: settlement.confirm {statement_id, kind, sources, evidence_ids}.
const (
	CommandConfirmSettlement = "settlement.confirm"

	// Reads.
	CommandGetStatement       = "settlement_statement.get"
	CommandGetReceivable      = "receivable.get"
	CommandGetPayable         = "payable.get"
	CommandGetOrderSettlement = "order_settlement.get"

	EventSettlementConfirmed = "idp-brant.stl.settlement_confirmed.v1"

	// Kinds and states of a statement, its source kinds, and the application targets and state it adds.
	StatementSales     = "sales"
	StatementPurchase  = "purchase"
	StatementConfirmed = "confirmed"
	SourceShipmentLine = "shipment_line"
	SourceDeliveryLine = "supplier_delivery_line"
	TargetReceivable   = "receivable"
	TargetPayable      = "payable"
	ApplicationMoved   = "moved"

	// MaxSources bounds the source lines of one statement (ShipmentLineRef v1 and
	// PurchaseSettlementSource v1 answer at most 200 each).
	MaxSources = 200
)

// SettlementSourceRef is one source line of a statement: of a sales statement a FUL shipment line
// (ShipmentID, LineID); of a purchase statement a PUR supplier delivery line (DeliveryID, LineID —
// the order line the commitment line buys — and Version, the delivery's verified version).
type SettlementSourceRef struct {
	ShipmentID string
	DeliveryID string
	LineID     string
	Version    int64
}

// ConfirmSettlement confirms statement StatementID of StatementKind ("kind": sales or purchase) on
// Sources, resting on EvidenceIDs (0 to 20 EVD sources). Human only.
type ConfirmSettlement struct {
	IdempotencyKey string
	StatementID    string
	StatementKind  string `json:"kind"`
	Sources        []SettlementSourceRef
	EvidenceIDs    []string
}

func (ConfirmSettlement) stlCommand() {}

// Validate checks the closed contract: identifiers, the kind, 1 to MaxSources distinct source lines
// of that kind and 0 to 20 distinct evidence IDs.
func (c ConfirmSettlement) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.StatementID):
		return schema("/args/statement_id")
	case c.StatementKind != StatementSales && c.StatementKind != StatementPurchase:
		return schema("/args/kind")
	case len(c.Sources) == 0 || len(c.Sources) > MaxSources:
		return schema("/args/sources")
	}
	seen := map[SettlementSourceRef]bool{}
	for i, s := range c.Sources {
		p := "/args/sources/" + strconv.Itoa(i)
		key := SettlementSourceRef{ShipmentID: s.ShipmentID, DeliveryID: s.DeliveryID, LineID: s.LineID}
		switch {
		case c.StatementKind == StatementSales && (!kernel.ValidIdentifier(s.ShipmentID) || s.DeliveryID != "" || s.Version != 0):
			return schema(p + "/shipment_id")
		case c.StatementKind == StatementPurchase && (!kernel.ValidIdentifier(s.DeliveryID) || s.ShipmentID != ""):
			return schema(p + "/delivery_id")
		case c.StatementKind == StatementPurchase && !versionOK(s.Version):
			return schema(p + "/version")
		case !kernel.ValidIdentifier(s.LineID):
			return schema(p + "/line_id")
		case seen[key]:
			return schema(p)
		}
		seen[key] = true
	}
	return evidence(c.EvidenceIDs, "/args/evidence_ids", 0)
}

// StatementReceipt is the answer of settlement.confirm: the receipt (ObjectID the statement,
// RowVersion 1, Status confirmed), its kind, party, order (sales), currency, amount, what is open on
// it, how many lines and installments it has and the stage applications that moved to it.
type StatementReceipt struct {
	Receipt
	StatementKind     string `json:"kind"`
	CounterpartyID    string
	OrderID           string
	Currency          string
	AmountMinor       string
	OpenMinor         string
	Lines             int
	Installments      int
	MovedApplications []string
}

// StatementLineView is a statement line: the source line settled (kind, ID, line, version), the order
// binding and order line it is of, the commitment and supplier, the SKU specification and unit, the
// quantity settled, the basis it was valued from, its amounts in minor units, when the goods left or
// were handed over, and its state.
type StatementLineView struct {
	Seq           int
	SourceKind    string
	SourceID      string
	SourceLineID  string
	SourceVersion int64
	OrderID       string
	OrderRevision int64
	OrderHash     string
	OrderLineID   string
	CommitmentID  string
	SupplierID    string
	SkuID         string
	SpecRevision  int64
	UnitID        string
	UnitRevision  int64
	Quantity      string
	BasisQuantity string
	UnitPrice     string
	BasisNetMinor string
	BasisTaxMinor string
	NetMinor      string
	TaxMinor      string
	AmountMinor   string
	OccurredAt    time.Time
	Status        string
}

// InstallmentView is an installment of a receivable or payable now: the payment stage it is due for
// (none beyond the plan and for a payable), the amount, what stands applied and is open, the due
// instant, and how many whole days it is overdue (-1: not overdue).
type InstallmentView struct {
	Seq            int
	StageID        string
	Trigger        string
	Days           int
	ReleaseControl string
	AmountMinor    string
	AppliedMinor   string
	OpenMinor      string
	DueAt          time.Time
	OverdueDays    int
}

// BalanceView is a business receivable or payable now (receivable.get, payable.get): its statement,
// kind (receivable or payable), party, order (receivable), currency, amount, what stands applied and
// is open, the installments, the applications the caller sees and the earliest open due instant.
type BalanceView struct {
	StatementID    string
	Kind           string
	Status         string
	CounterpartyID string
	OrderID        string
	OrderRevision  int64
	Currency       string
	AmountMinor    string
	AppliedMinor   string
	OpenMinor      string
	EarliestDueAt  time.Time
	Installments   []InstallmentView
	Applications   []AllocationView
	ConfirmedAt    time.Time
	CheckedAt      time.Time
}

// StatementView is a settlement statement as settlement_statement.get shows it: its kind, state,
// party, order and the account revision it was confirmed against (sales), when its first goods left,
// currency, net, tax and total, what stands applied and is open, its evidence, lines, installments
// and the applications the caller sees.
type StatementView struct {
	StatementID    string
	Kind           string
	Status         string
	Version        int64
	CounterpartyID string
	OrderID        string
	OrderRevision  int64
	OrderHash      string
	ShippedAt      time.Time
	Currency       string
	NetMinor       string
	TaxMinor       string
	AmountMinor    string
	AppliedMinor   string
	OpenMinor      string
	EvidenceIDs    []string
	Lines          []StatementLineView
	Installments   []InstallmentView
	Applications   []AllocationView
	ConfirmedBy    kernel.ActorRef
	ConfirmedAt    time.Time
	CheckedAt      time.Time
}

// StagePositionView is a payment stage of the order account's revision as the order's settlement
// position shows it: its amount, what stands prepaid on the stage (not yet settled), what the order's
// receivables put on it (settled), what is paid on it (prepaid and received on its installments),
// and what is not settled yet.
type StagePositionView struct {
	StageID        string
	Seq            int
	Trigger        string
	ReleaseControl string
	AmountMinor    string
	PrepaidMinor   string
	SettledMinor   string
	PaidMinor      string
	UnsettledMinor string
}

// PurchasePositionView is a purchase statement line of the order: the statement, supplier and
// currency, the line's source, quantity and amount.
type PurchasePositionView struct {
	StatementID  string
	Seq          int
	SupplierID   string
	Currency     string
	CommitmentID string
	SourceID     string
	OrderLineID  string
	Quantity     string
	AmountMinor  string
}

// OrderSettlementView is an order's settlement position (order_settlement.get) in the order account's
// currency: the account's revision and stages, what stands prepaid, what is settled on sales
// statements, applied and open on their receivables and overdue now, each receivable, and the
// purchase statement lines of the order with the payables that hold them.
type OrderSettlementView struct {
	OrderID       string
	OrderRevision int64
	Currency      string
	PrepaidMinor  string
	SettledMinor  string
	AppliedMinor  string
	OpenMinor     string
	OverdueMinor  string
	Stages        []StagePositionView
	Receivables   []BalanceView
	PurchaseLines []PurchasePositionView
	Payables      []BalanceView
	CheckedAt     time.Time
}

// SettlementCommands is STL's command entry point for settlement statements; errors as for Commands.
type SettlementCommands interface {
	ConfirmSettlement(ctx context.Context, cmd ConfirmSettlement) (StatementReceipt, error)
}

// SettlementQueries is the audited read side of statements (stl.read); statements outside the
// caller's data scope are NOT_FOUND.
type SettlementQueries interface {
	GetStatement(ctx context.Context, id string) (StatementView, error)
	GetReceivable(ctx context.Context, id string) (BalanceView, error)
	GetPayable(ctx context.Context, id string) (BalanceView, error)
	GetOrderSettlement(ctx context.Context, orderID string) (OrderSettlementView, error)
}
