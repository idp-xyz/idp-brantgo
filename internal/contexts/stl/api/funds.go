package api

import (
	"context"
	"regexp"
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Funds, applications and order accounts (S5 unit B2a, WBS S5-04a and the STL half of S5-07;
// tactical model STL-AGG-FundRecord, STL-AGG-Application, STL-AGG-OrderAccount; SM-STL-FUND,
// SM-STL-APPLICATION; STL-INV-002, STL-INV-003, STL-INV-006; Context Map R38, R45, R75; S5 plan
// D-3, D-4, D-5, D-16; baseline 4.2, 4.3, 4.6 S-A, CN-R10, CN-R11, CN-AT07/08/09/15).
//
//	fund.record              human only (treasury): records a verified movement of money on one of
//	                         the company's POL bank accounts with bank evidence (EVD sources): a
//	                         receipt from a payer, a non-goods receipt (never applied: it waits for
//	                         verification), a payment to a payee, a refund or a returned remittance
//	                         (drawn on an earlier fund record: never more than is unapplied and still
//	                         on the account), or an FX conversion of an earlier receipt into another
//	                         currency (a transfer with the bank's rate and fee, never a receipt). A
//	                         receipt keeps the remitted amount, the bank's credit and the stated bank
//	                         charges apart; the difference stays unexplained. A bank account that is
//	                         not the company's is PARTY_ENTITY_MISMATCH (CN-AT15); a bank reference
//	                         recorded on the account before is BUSINESS_KEY_CONFLICT.
//	fund.resolve_difference  human only (credit manager), independent of the recorder: resolves part
//	                         or all of a receipt's unexplained difference as a bank charge or a
//	                         discount borne by us, with its basis.
//	order_account.open       human or system (payment_plan_review.open): opens the order account of an
//	                         active order at TRD's current revision with its payment stages.
//	application.apply        human only (treasury): allocates part of a receipt's available balance to
//	                         a payment stage of an order account in the same currency, the payer being
//	                         the order's customer (APPLICATION_EXCEEDS_AVAILABLE,
//	                         FUND_DIFFERENCE_UNEXPLAINED, APPLICATION_EXCEEDS_OPEN,
//	                         PARTY_ENTITY_MISMATCH, FX_SCOPE_MISMATCH). Commercial allocation, never
//	                         the administrative 出口收汇核销 (CN-R11).
//	application.unapply      human only (treasury): undoes an application with a reason; a new record,
//	                         the fund fact stays.
//
// Request members: fund.record {fund_record_id, kind, counterparty_id, currency {kind, id, revision},
// bank_account {kind, id, revision}, received_amount, remitted_amount, bank_charges, value_date,
// bank_reference, evidence_ids, source_fund_record_id, fx {sold_amount, bank_rate, fee}};
// fund.resolve_difference {fund_record_id, expected_version, resolution, amount, evidence_ids,
// reason}; order_account.open {order_id, order_revision, order_hash}; application.apply
// {application_id, fund_record_id, target {kind, order_id, stage_id}, amount}; application.unapply
// {application_id, expected_version, reason}.
const (
	CommandRecordFund        = "fund.record"
	CommandResolveDifference = "fund.resolve_difference"
	CommandOpenOrderAccount  = "order_account.open"
	CommandApply             = "application.apply"
	CommandUnapply           = "application.unapply"

	// order_account.open needs the permission that adopts an order revision's payment stages.
	PermissionOpenOrderAccount = "payment_plan_review.open"

	// Reads.
	CommandGetFundRecord   = "fund_record.get"
	CommandGetOrderAccount = "order_account.get"
	CommandGetApplication  = "application.get"

	EventFundRecorded           = "idp-brant.stl.fund_recorded.v1"
	EventFundDifferenceResolved = "idp-brant.stl.fund_difference_resolved.v1"
	EventApplied                = "idp-brant.stl.applied.v1"
	EventUnapplied              = "idp-brant.stl.unapplied.v1"

	// Kinds of a fund record.
	FundReceipt            = "receipt"
	FundNonGoods           = "non_goods"
	FundPayment            = "payment"
	FundRefund             = "refund"
	FundReturnedRemittance = "returned_remittance"
	FundFXConversion       = "fx_conversion"

	// States of a fund record (SM-STL-FUND), an order account and an application (SM-STL-APPLICATION).
	FundRecorded           = "recorded"
	FundDifferenceResolved = "difference_resolved"
	AccountOpen            = "open"
	ApplicationApplied     = "applied"
	ApplicationUnapplied   = "unapplied"

	// Resolutions of a difference, and the target kinds of an application (B2b adds receivables).
	ResolutionBankCharge = "bank_charge"
	ResolutionDiscount   = "discount"
	TargetOrderStage     = "order_stage"

	KindBankAccount = "bank_account"
)

var bankReference = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,63}$`)

// RateFormat is the decimal form of a bank's conversion rate.
var RateFormat = kernel.DecimalFormat{IntDigits: 8, Scale: 10}

// FXSale is a conversion's sale: SoldAmount in the source's currency, the bank's BankRate (target
// units per source unit) and its Fee in the target currency ("" is none).
type FXSale struct {
	SoldAmount string
	BankRate   string
	Fee        string
}

// RecordFund records fund record FundRecordID of FundKind ("kind"). CounterpartyID is the payer of
// a receipt or non-goods receipt or the payee of a payment (a PTY party); a refund, a returned
// remittance and a conversion name SourceFundRecordID instead and take its counterparty (none for a
// conversion). Currency is the POL currency of the money and BankAccount the company's POL
// bank_account it moved on; ReceivedAmount is what the bank credited or debited in that currency.
// A receipt may state RemittedAmount (default the received amount) and the BankCharges the advice
// states. ValueDate is the bank's value date, BankReference the bank's reference of the movement,
// EvidenceIDs the EVD sources of the bank evidence. A conversion states FX.
type RecordFund struct {
	IdempotencyKey     string
	FundRecordID       string
	FundKind           string `json:"kind"`
	CounterpartyID     string
	Currency           ConfigRef
	BankAccount        ConfigRef
	ReceivedAmount     string
	RemittedAmount     string
	BankCharges        string
	ValueDate          string
	BankReference      string
	EvidenceIDs        []string
	SourceFundRecordID string
	FX                 *FXSale
}

// ResolveFundDifference resolves Amount of fund record FundRecordID's unexplained difference at
// ExpectedVersion as Resolution (bank_charge or discount) on the basis of EvidenceIDs, for Reason.
// Human only, not the recorder.
type ResolveFundDifference struct {
	IdempotencyKey  string
	FundRecordID    string
	ExpectedVersion int64
	Resolution      string
	Amount          string
	EvidenceIDs     []string
	Reason          string
}

// OpenOrderAccount opens the order account of order OrderID at OrderRevision and OrderHash, TRD's
// current revision of an active order.
type OpenOrderAccount struct {
	IdempotencyKey string
	OrderID        string
	OrderRevision  int64
	OrderHash      string
}

// ApplicationTarget is what an application allocates to: Kind order_stage, the order's account and
// its payment stage StageID; Kind receivable, sales statement StatementID's installment of StageID
// ("" the first one still open); Kind payable, purchase statement StatementID (settlement.go).
type ApplicationTarget struct {
	Kind        string
	OrderID     string
	StageID     string
	StatementID string
}

// Apply allocates Amount (in the fund's currency) of fund record FundRecordID to Target as
// application ApplicationID. Human only.
type Apply struct {
	IdempotencyKey string
	ApplicationID  string
	FundRecordID   string
	Target         ApplicationTarget
	Amount         string
}

// Unapply undoes application ApplicationID at ExpectedVersion for Reason. Human only.
type Unapply struct {
	IdempotencyKey  string
	ApplicationID   string
	ExpectedVersion int64
	Reason          string
}

func (RecordFund) stlCommand()            {}
func (ResolveFundDifference) stlCommand() {}
func (OpenOrderAccount) stlCommand()      {}
func (Apply) stlCommand()                 {}
func (Unapply) stlCommand()               {}

func evidence(ids []string, path string, min int) error {
	if len(ids) < min || len(ids) > MaxEvidence {
		return schema(path)
	}
	for j, e := range ids {
		if !kernel.ValidIdentifier(e) || indexOf(ids, e) != j {
			return schema(path + "/" + strconv.Itoa(j))
		}
	}
	return nil
}

// Validate checks the closed contract: identifiers, the kind with the members it needs and only
// those, amounts, the value date, the bank reference and 1 to 20 distinct evidence IDs.
func (c RecordFund) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.FundRecordID):
		return schema("/args/fund_record_id")
	}
	drawn := false
	switch c.FundKind {
	case FundReceipt, FundNonGoods, FundPayment:
	case FundRefund, FundReturnedRemittance, FundFXConversion:
		drawn = true
	default:
		return schema("/args/kind")
	}
	switch {
	case drawn && (c.CounterpartyID != "" || !kernel.ValidIdentifier(c.SourceFundRecordID)):
		return schema("/args/source_fund_record_id")
	case !drawn && (!kernel.ValidIdentifier(c.CounterpartyID) || c.SourceFundRecordID != ""):
		return schema("/args/counterparty_id")
	case c.SourceFundRecordID == c.FundRecordID:
		return schema("/args/source_fund_record_id")
	case (c.FX != nil) != (c.FundKind == FundFXConversion):
		return schema("/args/fx")
	case c.FundKind != FundReceipt && (c.RemittedAmount != "" || c.BankCharges != ""):
		return schema("/args/remitted_amount")
	}
	if err := c.Currency.Validate(KindCurrency, "/args/currency"); err != nil {
		return err
	}
	if err := c.BankAccount.Validate(KindBankAccount, "/args/bank_account"); err != nil {
		return err
	}
	for path, a := range map[string]string{"/args/received_amount": c.ReceivedAmount, "/args/remitted_amount": c.RemittedAmount,
		"/args/bank_charges": c.BankCharges} {
		if (a != "" || path == "/args/received_amount") && !amount(a) {
			return schema(path)
		}
	}
	if c.FX != nil {
		switch {
		case !amount(c.FX.SoldAmount):
			return schema("/args/fx/sold_amount")
		case c.FX.Fee != "" && !amount(c.FX.Fee):
			return schema("/args/fx/fee")
		}
		if _, err := kernel.ParseDecimal(c.FX.BankRate, RateFormat); err != nil {
			return schema("/args/fx/bank_rate")
		}
	}
	if _, err := kernel.ParseTimestamp(c.ValueDate); err != nil {
		return schema("/args/value_date")
	}
	if !bankReference.MatchString(c.BankReference) {
		return schema("/args/bank_reference")
	}
	return evidence(c.EvidenceIDs, "/args/evidence_ids", 1)
}

// Validate checks the closed contract.
func (c ResolveFundDifference) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.FundRecordID):
		return schema("/args/fund_record_id")
	case !versionOK(c.ExpectedVersion):
		return schema("/args/expected_version")
	case c.Resolution != ResolutionBankCharge && c.Resolution != ResolutionDiscount:
		return schema("/args/resolution")
	case !amount(c.Amount):
		return schema("/args/amount")
	case !Reason(c.Reason):
		return schema("/args/reason")
	}
	return evidence(c.EvidenceIDs, "/args/evidence_ids", 1)
}

// Validate checks the closed contract.
func (c OpenOrderAccount) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.OrderID):
		return schema("/args/order_id")
	case !versionOK(c.OrderRevision):
		return schema("/args/order_revision")
	case !hash(c.OrderHash):
		return schema("/args/order_hash")
	}
	return nil
}

// Validate checks the closed contract.
func (c Apply) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.ApplicationID):
		return schema("/args/application_id")
	case !kernel.ValidIdentifier(c.FundRecordID):
		return schema("/args/fund_record_id")
	case c.Target.Kind != TargetOrderStage && c.Target.Kind != TargetReceivable && c.Target.Kind != TargetPayable:
		return schema("/args/target/kind")
	case c.Target.Kind == TargetOrderStage && (!kernel.ValidIdentifier(c.Target.OrderID) || c.Target.StatementID != ""):
		return schema("/args/target/order_id")
	case c.Target.Kind == TargetOrderStage && !kernel.ValidIdentifier(c.Target.StageID):
		return schema("/args/target/stage_id")
	case c.Target.Kind != TargetOrderStage && (!kernel.ValidIdentifier(c.Target.StatementID) || c.Target.OrderID != ""):
		return schema("/args/target/statement_id")
	case c.Target.Kind == TargetReceivable && c.Target.StageID != "" && !kernel.ValidIdentifier(c.Target.StageID),
		c.Target.Kind == TargetPayable && c.Target.StageID != "":
		return schema("/args/target/stage_id")
	case !amount(c.Amount):
		return schema("/args/amount")
	}
	return nil
}

// Validate checks the closed contract.
func (c Unapply) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.ApplicationID):
		return schema("/args/application_id")
	case !versionOK(c.ExpectedVersion):
		return schema("/args/expected_version")
	case !Reason(c.Reason):
		return schema("/args/reason")
	}
	return nil
}

// FundRecordReceipt is the answer of fund.record and fund.resolve_difference: the receipt
// (ObjectID the fund record, RowVersion its version, Status its state) and its balances now, in
// minor units of Currency.
type FundRecordReceipt struct {
	Receipt
	FundKind         string `json:"kind"`
	Currency         string
	ReceivedMinor    string
	DifferenceMinor  string
	UnexplainedMinor string
	AvailableMinor   string
}

// OrderAccountReceipt is the answer of order_account.open: the receipt (ObjectID the order, Status
// open) and the revision, customer, currency and number of stages the account holds.
type OrderAccountReceipt struct {
	Receipt
	OrderRevision int64
	OrderHash     string
	PartyID       string
	Currency      string
	Stages        int
}

// ApplicationReceipt is the answer of application.apply and application.unapply: the receipt
// (ObjectID the application, RowVersion its version, Status applied or unapplied), its target and
// amount, and what is available on the fund and open on the stage — or on the receivable or
// payable — now.
type ApplicationReceipt struct {
	Receipt
	FundRecordID       string
	TargetKind         string
	OrderID            string
	OrderRevision      int64
	StageID            string
	StatementID        string
	InstallmentSeq     int
	Currency           string
	AmountMinor        string
	FundAvailableMinor string
	StageOpenMinor     string
	StatementOpenMinor string
}

// ResolutionView is one resolved part of a receipt's difference.
type ResolutionView struct {
	Seq         int
	Resolution  string
	AmountMinor string
	EvidenceIDs []string
	Reason      string
	ResolvedBy  kernel.ActorRef
	ResolvedAt  time.Time
}

// FXView is a conversion's sale.
type FXView struct {
	SoldCurrency string
	SoldMinor    string
	BankRate     string
	FeeMinor     string
}

// AllocationView is one application of a fund record: its commercial allocation to an order stage,
// a receivable's installment (with the stage it is due for) or a payable, and the moved application a
// replacement or remainder comes from.
type AllocationView struct {
	ApplicationID  string
	FundRecordID   string
	TargetKind     string
	OrderID        string
	OrderRevision  int64
	StageID        string
	StatementID    string
	InstallmentSeq int
	MovedFrom      string
	AmountMinor    string
	Status         string
}

// DrawView is a refund, returned remittance or conversion drawn on a fund record, in its currency
// (a conversion's sold amount).
type DrawView struct {
	FundRecordID string
	Kind         string
	AmountMinor  string
}

// FundRecordRef is FundRecordRef v1 (Context Map R38 STL→CNX, OHS: 资金流水与核销（收汇资料关联）): a
// fund record as STL states it — kind, direction, state, counterparty, the bank account, reference,
// value date and evidence, the amounts in minor units of Currency (received, remitted, bank charges
// stated, difference, resolved, unexplained, applied, drawn by refunds and returns, sold by
// conversions, available for applications, still on the account), the conversion's sale, the
// resolutions, the draws and the commercial allocations. CNX links it to its document bundles;
// STL's allocations are commercial, not the administrative 出口收汇核销 (CN-R11).
type FundRecordRef struct {
	FundRecordID       string
	Kind               string
	Direction          string
	Status             string
	Version            int64
	CounterpartyID     string
	Currency           string
	CurrencyRevision   int64
	BankAccount        ConfigRef
	BankReference      string
	ValueDate          time.Time
	EvidenceIDs        []string
	ReceivedMinor      string
	RemittedMinor      string
	BankChargesMinor   string
	DifferenceMinor    string
	ResolvedMinor      string
	UnexplainedMinor   string
	AppliedMinor       string
	DrawnMinor         string
	SoldMinor          string
	AvailableMinor     string
	CashMinor          string
	SourceFundRecordID string
	FX                 *FXView
	Resolutions        []ResolutionView
	Draws              []DrawView
	Allocations        []AllocationView
	RecordedBy         kernel.ActorRef
	RecordedAt         time.Time
}

// FundRecordView is a fund record as fund_record.get shows it, as of CheckedAt.
type FundRecordView struct {
	FundRecordRef
	CheckedAt time.Time
}

// StageView is one payment stage of an order account with what stands applied and is open on it.
type StageView struct {
	StageID        string
	Seq            int
	Trigger        string
	Days           int
	Percent        string
	AmountMinor    string
	ReleaseControl string
	AppliedMinor   string
	OpenMinor      string
}

// ConditionView is an order account's payment condition for a scope now (PaymentConditionGuard v1's
// verdict as the read derives it).
type ConditionView struct {
	Scope          string
	Status         string
	Reason         string
	RequiredMinor  string
	AppliedMinor   string
	StageID        string
	ShortfallMinor string
}

// OrderAccountView is an order account as order_account.get shows it: the revision it holds, its
// stages, the applications on it the caller sees and the payment condition per scope.
type OrderAccountView struct {
	OrderID          string
	OrderRevision    int64
	OrderHash        string
	PartyID          string
	Currency         string
	CurrencyRevision int64
	Status           string
	Version          int64
	Stages           []StageView
	Applications     []AllocationView
	Conditions       []ConditionView
	OpenedBy         kernel.ActorRef
	OpenedAt         time.Time
	CheckedAt        time.Time
}

// ApplicationView is an application as application.get shows it.
type ApplicationView struct {
	ApplicationID  string
	FundRecordID   string
	TargetKind     string
	OrderID        string
	OrderRevision  int64
	StageID        string
	StatementID    string
	InstallmentSeq int
	MovedFrom      string
	Currency       string
	AmountMinor    string
	Status         string
	Version        int64
	AppliedBy      kernel.ActorRef
	AppliedAt      time.Time
	UnappliedBy    kernel.ActorRef
	UnappliedAt    time.Time
	UnapplyReason  string
	CheckedAt      time.Time
}

// FundCommands is STL's command entry point for funds, applications and order accounts; errors as
// for Commands.
type FundCommands interface {
	RecordFund(ctx context.Context, cmd RecordFund) (FundRecordReceipt, error)
	ResolveFundDifference(ctx context.Context, cmd ResolveFundDifference) (FundRecordReceipt, error)
	OpenOrderAccount(ctx context.Context, cmd OpenOrderAccount) (OrderAccountReceipt, error)
	Apply(ctx context.Context, cmd Apply) (ApplicationReceipt, error)
	Unapply(ctx context.Context, cmd Unapply) (ApplicationReceipt, error)
}

// FundQueries is the audited read side of funds (stl.read); objects outside the caller's data scope
// are NOT_FOUND.
type FundQueries interface {
	GetFundRecord(ctx context.Context, id string) (FundRecordView, error)
	GetOrderAccount(ctx context.Context, orderID string) (OrderAccountView, error)
	GetApplication(ctx context.Context, id string) (ApplicationView, error)
}

// FundRecordLookups publishes FundRecordRef v1 to CNX (R38): ResolveFundRecords returns, among ids,
// the fund records that exist and the caller sees (an active grant and its data scope), never
// locking anything (R38 has no guard lock). It is not audited: the caller's command records its own
// outcome.
type FundRecordLookups interface {
	ResolveFundRecords(ctx context.Context, ids []string) (map[string]FundRecordRef, error)
}
