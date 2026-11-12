package application

import (
	"context"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// PurchaseLines is CNX's port to PUR (Context Map R37 PUR→CNX, Customer / Supplier, PurchaseLineRef
// v1: 采购与收货明细（购进发票匹配）; risk 红冲作废后匹配未复核; no guard lock): what PUR says now of each
// commitment line an invoice is matched to — purapi DeliveryLookups.ResolvePurchaseLines, read
// before CNX's transaction. A line PUR does not resolve for the caller is absent.
//
// R37 carries no guard: a match records the quantity bought and delivered it read. CNX serializes
// verifications that match the same commitment line itself (an advisory lock per line) and never
// locks a PUR row.
type PurchaseLines interface {
	ResolvePurchaseLines(ctx context.Context, refs []PurchaseLineRef) (map[PurchaseLineRef]PurchaseLine, error)
}

// PurchaseLineRef names a PUR commitment line: the order line its draft buys.
type PurchaseLineRef struct {
	CommitmentID string
	OrderLineID  string
}

// PurchaseLine is a commitment line as PUR shows it: Code "" for a line of an issued or confirmed
// commitment (its Status otherwise), the supplier, the unit, the purchase order number, the
// quantity bought (allocated plus approved overbuy) and the quantity verified as delivered.
type PurchaseLine struct {
	Code              string
	Status            string
	SupplierID        string
	Unit              string
	PONumber          string
	PurchaseQuantity  kernel.Decimal
	DeliveredQuantity kernel.Decimal
}

// Funds is CNX's port to STL (Context Map R38 STL→CNX, OHS, FundRecordRef v1: 资金流水与核销（收汇资料
// 关联）; risk 商业核销被称为出口收汇核销; no guard lock): the fund records a received refund rests on,
// stlapi FundRecordLookups.ResolveFundRecords before CNX's transaction. A record the caller does
// not see is absent.
type Funds interface {
	ResolveFundRecords(ctx context.Context, ids []string) (map[string]FundRecord, error)
}

// The direction of a fund record that brings money in.
const FundIn = "in"

// FundRecord is a fund record as STL states it: its kind, direction, state, currency and the amount
// the bank credited, in minor units.
type FundRecord struct {
	ID            string
	Kind          string
	Direction     string
	Status        string
	Currency      string
	ReceivedMinor string
}
