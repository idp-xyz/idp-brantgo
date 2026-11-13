// Package settlement holds STL's SettlementStatement rules (tactical model STL-AGG-SettlementStatement,
// SM-STL-SETTLEMENT, STL-INV-001; UL STL-SettlementStatement, STL-Receivable, UL-INV-23; Context Map
// R30 ShipmentLineRef v1, R31 PurchaseSettlementSource v1; baseline 4.2, 4.3, 4.6 S-A; S5 plan D-3,
// D-4; WBS S5-04b).
//
// A settlement statement is the internal controlled document that forms a business receivable or
// payable — not a commercial or tax invoice. A sales statement settles confirmed FUL shipment lines
// of one order, valued at the order line's price at the revision the shipment bound (FUL's amounts);
// a purchase statement settles PUR's verified supplier delivery lines of one supplier, each valued at
// its share of the commitment line's sealed amounts (verified ÷ purchased, HALF_UP to the minor unit,
// net and tax separately). A source line is settled once while its statement stands
// (SETTLEMENT_SOURCE_DUPLICATE; reversal is unit C8's).
//
// A receivable falls due in installments that follow the order's payment stages (the order
// account's revision): the order's settled amount fills the stages in plan order, so what was paid
// before the goods left covers the first goods settled (4.6 S-A: the 200 prepayment covers the first
// statement of 600); a stage due on order confirmation or before shipment is due when the goods left,
// one due after shipment that many days later, one due after invoice that many days after the
// statement; what exceeds the plan is due with the plan's last stage. A payable is one installment,
// due when it is confirmed (PUR's contract states no supplier payment terms).
package settlement

import (
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/fundrecord"
	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/orderaccount"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Kinds, states and source kinds of a statement and its lines.
const (
	KindSales          = "sales"
	KindPurchase       = "purchase"
	StatusConfirmed    = "confirmed"
	LineSettled        = "settled"
	SourceShipmentLine = "shipment_line"
	SourceDeliveryLine = "supplier_delivery_line"
)

// CodeSettlementSourceDuplicate: the source line is settled by a standing statement (STL-INV-001).
const CodeSettlementSourceDuplicate = "SETTLEMENT_SOURCE_DUPLICATE"

// Codes are the codes the statement rules give besides the kernel's, with their categories.
var Codes = map[string]kernel.Category{CodeSettlementSourceDuplicate: kernel.CategoryConflict,
	CodeSettlementHasApplications: kernel.CategoryConflict, CodeOpeningBalanceDuplicate: kernel.CategoryConflict}

// SourceKey names a source line across its versions: (shipment, line) or (delivery, order line).
type SourceKey struct {
	Kind   string
	ID     string
	LineID string
}

// Line is one settled source line: what it is (source, version, order binding, order line, SKU
// specification, unit), the quantity settled, the basis it was valued from (the order line's or
// commitment line's quantity and amounts) and its amounts.
type Line struct {
	Seq           int
	Source        SourceKey
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
	Quantity      kernel.Decimal
	BasisQuantity kernel.Decimal
	UnitPrice     string
	BasisNet      kernel.Money
	BasisTax      kernel.Money
	Net           kernel.Money
	Tax           kernel.Money
	Amount        kernel.Money
	OccurredAt    time.Time
	Status        string
}

// Installment is the part of a statement's amount due at DueAt: for a receivable, the part of one
// payment stage of the order account's revision (Stage set) or the part beyond the plan; for a
// payable, the whole amount.
type Installment struct {
	Seq            int
	OrderID        string
	OrderRevision  int64 // the account revision of the stage; 0 without a stage
	StageID        string
	Trigger        string
	Days           int
	ReleaseControl string
	Amount         kernel.Money
	DueAt          time.Time
}

// Statement is the root of STL-AGG-SettlementStatement.
type Statement struct {
	ID             string
	Kind           string
	Status         string
	Version        int64
	CounterpartyID string
	OrderID        string // sales: the order and the account revision it was confirmed against
	OrderRevision  int64
	OrderHash      string
	ShippedAt      time.Time // sales: when the first of its goods left
	Net            kernel.Money
	Tax            kernel.Money
	Amount         kernel.Money
	EvidenceIDs    []string
	Lines          []Line
	Installments   []Installment
	OwnerActor     string
	OwnerTeam      string
	Confirmed      fundrecord.Act
	Opening        bool // an opening statement (C10, opening.go): no lines, no order
}

// Currency is the statement's currency.
func (s *Statement) Currency() kernel.Currency { return s.Amount.Currency() }

// Installment returns installment seq, or nil.
func (s *Statement) Installment(seq int) *Installment {
	for i := range s.Installments {
		if s.Installments[i].Seq == seq {
			return &s.Installments[i]
		}
	}
	return nil
}

// InstallmentOf returns the installment of stage, or nil.
func (s *Statement) InstallmentOf(stage string) *Installment {
	for i := range s.Installments {
		if s.Installments[i].StageID == stage && stage != "" {
			return &s.Installments[i]
		}
	}
	return nil
}

func must(m kernel.Money, err error) kernel.Money {
	if err != nil {
		panic("stl: statement amounts in one currency: " + err.Error())
	}
	return m
}

// OpenOf returns what is open of installment i given applied, what stands applied on it.
func OpenOf(i Installment, applied kernel.Money) kernel.Money { return must(i.Amount.Sub(applied)) }

// Open returns what is open of the statement given applied, what stands on each installment by seq.
func (s *Statement) Open(applied map[int]kernel.Money) kernel.Money {
	sum := must(kernel.NewMoney(s.Currency(), 0))
	for _, i := range s.Installments {
		a, ok := applied[i.Seq]
		if !ok {
			a = must(kernel.NewMoney(s.Currency(), 0))
		}
		sum = must(sum.Add(OpenOf(i, a)))
	}
	return sum
}

func path(i int) string { return "/args/sources/" + strconv.Itoa(i) }

func invalid(p, reason string) error   { return kernel.Reject(kernel.CodeInvalidKnownValue, p, reason) }
func reference(p, reason string) error { return kernel.Reject(kernel.CodeReferenceInvalid, p, reason) }
func state(p, reason string) error     { return kernel.Reject(kernel.CodeInvalidState, p, reason) }

// Reject returns a rejection with one of Codes at path.
func Reject(code, p, reason string) *kernel.Rejection {
	return kernel.RejectAs(code, Codes[code], p, reason)
}

// duplicate refuses a source line a standing statement settles.
func duplicate(settled map[SourceKey]string, k SourceKey, i int) error {
	if by, ok := settled[k]; ok {
		return Reject(CodeSettlementSourceDuplicate, path(i), "The line is settled by statement "+by)
	}
	return nil
}

func minor(c kernel.Currency, s, p, what string) (kernel.Money, error) {
	m, err := kernel.ParseMinor(c, s)
	if err != nil || m.Sign() < 0 {
		return kernel.Money{}, reference(p, "The source states no "+what)
	}
	return m, nil
}

func quantity(s, p string) (kernel.Decimal, error) {
	q, err := kernel.ParseDecimal(s, kernel.QuantityFormat)
	if err != nil {
		return kernel.Decimal{}, reference(p, "The source states no quantity")
	}
	return q, nil
}

// ShipmentLine is a FUL shipment line a sales statement settles (ShipmentLineRef v1), FUL's verdict
// already passed: confirmed (ShippedAt is its confirmation) and priced in Currency at the bound order
// revision, with the order line's amounts (Basis) and those of the quantity shipped, in minor units.
type ShipmentLine struct {
	ShipmentID        string
	LineID            string
	ShipmentVersion   int64
	ShippedAt         time.Time
	OrderID           string
	OrderRevision     int64
	OrderHash         string
	CustomerID        string
	OrderLineID       string
	SkuID             string
	SpecRevision      int64
	UnitID            string
	UnitRevision      int64
	OrderLineQuantity string
	Quantity          string
	Currency          string
	UnitPrice         string
	BasisNet          string
	BasisTax          string
	Net               string
	Tax               string
	Payable           string
	CommitmentID      string
	SupplierID        string
}

// SalesInput is a sales settlement.confirm as the rules see it: the order account (locked), the lines
// FUL showed, the source lines a standing statement settles, and Prior, what the order's standing
// statements put on each stage ID so far.
type SalesInput struct {
	ID          string
	Account     *orderaccount.Account
	Lines       []ShipmentLine
	Settled     map[SourceKey]string
	Prior       map[string]kernel.Money
	EvidenceIDs []string
	By          fundrecord.Act
	Team        string
}

// Sales is settlement.confirm's rule for a sales statement (STL-INV-001, R30, D-4): every line is of
// the account's order and customer, in its currency, and settled by no standing statement; the
// receivable is the lines' payable amounts, due in installments that fill the account's stages.
func Sales(in SalesInput) (*Statement, error) {
	a := in.Account
	if a == nil {
		return nil, state(path(0), "The order has no order account: open it first")
	}
	c := a.Currency()
	st := &Statement{ID: in.ID, Kind: KindSales, Status: StatusConfirmed, Version: 1, CounterpartyID: a.PartyID, OrderID: a.OrderID,
		OrderRevision: a.OrderRevision, OrderHash: a.OrderHash, EvidenceIDs: in.EvidenceIDs, OwnerActor: in.By.By.ID, OwnerTeam: in.Team,
		Confirmed: in.By, Net: must(kernel.NewMoney(c, 0))}
	st.Tax, st.Amount = st.Net, st.Net
	for i, l := range in.Lines {
		p := path(i)
		k := SourceKey{Kind: SourceShipmentLine, ID: l.ShipmentID, LineID: l.LineID}
		if err := duplicate(in.Settled, k, i); err != nil {
			return nil, err
		}
		switch {
		case l.OrderID != a.OrderID:
			return nil, invalid(p, "A sales statement settles the lines of one order: this line ships order "+l.OrderID+", not "+a.OrderID)
		case l.CustomerID != a.PartyID:
			return nil, reference(p, "The line's customer "+l.CustomerID+" is not the order account's "+a.PartyID)
		case l.Currency != c.Code():
			return nil, reference(p, "The line is priced in "+l.Currency+", the order account in "+c.Code())
		}
		line := Line{Seq: i + 1, Source: k, SourceVersion: l.ShipmentVersion, OrderID: l.OrderID, OrderRevision: l.OrderRevision,
			OrderHash: l.OrderHash, OrderLineID: l.OrderLineID, CommitmentID: l.CommitmentID, SupplierID: l.SupplierID, SkuID: l.SkuID,
			SpecRevision: l.SpecRevision, UnitID: l.UnitID, UnitRevision: l.UnitRevision, UnitPrice: l.UnitPrice, OccurredAt: kernel.Instant(l.ShippedAt),
			Status: LineSettled}
		var err error
		if line.Quantity, err = quantity(l.Quantity, p); err != nil {
			return nil, err
		}
		if line.BasisQuantity, err = quantity(l.OrderLineQuantity, p); err != nil {
			return nil, err
		}
		if line.Quantity.Sign() <= 0 || line.Quantity.Cmp(line.BasisQuantity) > 0 {
			return nil, reference(p, "The shipped quantity is not within the order line's")
		}
		for _, x := range []struct {
			dst  *kernel.Money
			s, w string
		}{{&line.BasisNet, l.BasisNet, "line net amount"}, {&line.BasisTax, l.BasisTax, "line tax amount"}, {&line.Net, l.Net, "net amount"},
			{&line.Tax, l.Tax, "tax amount"}, {&line.Amount, l.Payable, "payable amount"}} {
			if *x.dst, err = minor(c, x.s, p, x.w); err != nil {
				return nil, err
			}
		}
		if !must(line.Net.Add(line.Tax)).Equal(line.Amount) {
			return nil, reference(p, "The line's payable amount is not its net and tax")
		}
		if st.ShippedAt.IsZero() || line.OccurredAt.Before(st.ShippedAt) {
			st.ShippedAt = line.OccurredAt
		}
		st.Net, st.Tax, st.Amount = must(st.Net.Add(line.Net)), must(st.Tax.Add(line.Tax)), must(st.Amount.Add(line.Amount))
		st.Lines = append(st.Lines, line)
	}
	if st.Amount.Sign() <= 0 {
		return nil, invalid("/args/sources", "The lines settle no amount")
	}
	st.Installments = Schedule(a, in.Prior, st.Amount, st.ShippedAt, in.By.At)
	return st, nil
}

// due is when stage s falls due for goods that left at shipped, on a statement confirmed at
// confirmed (calendar days; a working-day basis is not modelled).
func due(s orderaccount.Stage, shipped, confirmed time.Time) time.Time {
	switch s.Trigger {
	case orderaccount.TriggerAfterShipment:
		return shipped.AddDate(0, 0, s.Days)
	case orderaccount.TriggerAfterInvoice:
		return confirmed.AddDate(0, 0, s.Days)
	}
	return shipped
}

// Schedule splits amount over a's stages in plan order (D-4, 4.6 S-A): each stage takes what is left
// of it after prior, what the order's standing statements put on it; what exceeds the plan forms an
// installment without a stage, due with the plan's last stage.
func Schedule(a *orderaccount.Account, prior map[string]kernel.Money, amount kernel.Money, shipped, confirmed time.Time) []Installment {
	var out []Installment
	left := amount
	for _, s := range a.Stages {
		if left.Sign() <= 0 {
			break
		}
		room := s.Amount
		if p, ok := prior[s.StageID]; ok {
			room = must(room.Sub(p))
		}
		if room.Sign() <= 0 {
			continue
		}
		take := room
		if c, _ := left.Cmp(room); c < 0 {
			take = left
		}
		left = must(left.Sub(take))
		out = append(out, Installment{Seq: len(out) + 1, OrderID: a.OrderID, OrderRevision: a.OrderRevision, StageID: s.StageID, Trigger: s.Trigger,
			Days: s.Days, ReleaseControl: s.ReleaseControl, Amount: take, DueAt: kernel.Instant(due(s, shipped, confirmed))})
	}
	if left.Sign() > 0 {
		at := confirmed
		if n := len(a.Stages); n > 0 {
			at = due(a.Stages[n-1], shipped, confirmed)
		}
		out = append(out, Installment{Seq: len(out) + 1, OrderID: a.OrderID, Amount: left, DueAt: kernel.Instant(at)})
	}
	return out
}

// DeliveryLine is a PUR supplier delivery line a purchase statement settles (PurchaseSettlementSource
// v1), PUR's verdict already passed: the verified quantity of the delivery's verified version, the
// commitment line it is delivered against (order binding, supplier, SKU specification, unit, the
// quantity purchased) and the commitment line's net and tax for that quantity, in minor units of
// Currency.
type DeliveryLine struct {
	DeliveryID       string
	LineID           string
	Version          int64
	HandedOverAt     time.Time
	CommitmentID     string
	SupplierID       string
	OrderID          string
	OrderRevision    int64
	OrderHash        string
	SkuID            string
	SpecRevision     int64
	UnitID           string
	UnitRevision     int64
	PurchaseQuantity string
	VerifiedQuantity string
	Currency         string
	MinorUnits       int
	LineNet          string
	LineTax          string
}

// PurchaseInput is a purchase settlement.confirm as the rules see it.
type PurchaseInput struct {
	ID          string
	Lines       []DeliveryLine
	Settled     map[SourceKey]string
	EvidenceIDs []string
	By          fundrecord.Act
	Team        string
}

// share is m × part ÷ whole, HALF_UP to m's minor units.
func share(m kernel.Money, part, whole kernel.Decimal) kernel.Money {
	f, err := part.Quo(whole)
	if err != nil {
		panic("stl: share of a zero quantity")
	}
	return must(m.MulHalfUp(f))
}

// Purchase is settlement.confirm's rule for a purchase statement (STL-INV-001, R31, MD-45): every
// line is a verified delivery of one supplier in one currency, settled by no standing statement, with
// something verified; it is valued at its share of the commitment line's amounts; the payable is
// their sum, one installment due at confirmation.
func Purchase(in PurchaseInput) (*Statement, error) {
	if len(in.Lines) == 0 {
		return nil, invalid("/args/sources", "Nothing to settle")
	}
	first := in.Lines[0]
	c, err := kernel.NewCurrency(first.Currency, first.MinorUnits)
	if err != nil {
		return nil, reference(path(0), "PUR states no currency for the line")
	}
	st := &Statement{ID: in.ID, Kind: KindPurchase, Status: StatusConfirmed, Version: 1, CounterpartyID: first.SupplierID,
		EvidenceIDs: in.EvidenceIDs, OwnerActor: in.By.By.ID, OwnerTeam: in.Team, Confirmed: in.By, Net: must(kernel.NewMoney(c, 0))}
	st.Tax, st.Amount = st.Net, st.Net
	for i, l := range in.Lines {
		p := path(i)
		k := SourceKey{Kind: SourceDeliveryLine, ID: l.DeliveryID, LineID: l.LineID}
		if err := duplicate(in.Settled, k, i); err != nil {
			return nil, err
		}
		switch {
		case l.SupplierID != st.CounterpartyID:
			return nil, invalid(p, "A purchase statement settles the deliveries of one supplier: this line is "+l.SupplierID+"'s, not "+
				st.CounterpartyID+"'s")
		case l.Currency != c.Code() || l.MinorUnits != c.MinorUnits():
			return nil, invalid(p, "A purchase statement is in one currency: this line is in "+l.Currency+", not "+c.Code())
		}
		line := Line{Seq: i + 1, Source: k, SourceVersion: l.Version, OrderID: l.OrderID, OrderRevision: l.OrderRevision, OrderHash: l.OrderHash,
			OrderLineID: l.LineID, CommitmentID: l.CommitmentID, SupplierID: l.SupplierID, SkuID: l.SkuID, SpecRevision: l.SpecRevision,
			UnitID: l.UnitID, UnitRevision: l.UnitRevision, OccurredAt: kernel.Instant(l.HandedOverAt), Status: LineSettled}
		if line.Quantity, err = quantity(l.VerifiedQuantity, p); err != nil {
			return nil, err
		}
		if line.BasisQuantity, err = quantity(l.PurchaseQuantity, p); err != nil {
			return nil, err
		}
		switch {
		case line.Quantity.Sign() <= 0:
			return nil, state(p, "Nothing of the delivery line is verified")
		case line.BasisQuantity.Sign() <= 0 || line.Quantity.Cmp(line.BasisQuantity) > 0:
			return nil, reference(p, "The verified quantity is not within the commitment line's")
		}
		if line.BasisNet, err = minor(c, l.LineNet, p, "line net amount"); err != nil {
			return nil, err
		}
		if line.BasisTax, err = minor(c, l.LineTax, p, "line tax amount"); err != nil {
			return nil, err
		}
		line.Net, line.Tax = share(line.BasisNet, line.Quantity, line.BasisQuantity), share(line.BasisTax, line.Quantity, line.BasisQuantity)
		line.Amount = must(line.Net.Add(line.Tax))
		st.Net, st.Tax, st.Amount = must(st.Net.Add(line.Net)), must(st.Tax.Add(line.Tax)), must(st.Amount.Add(line.Amount))
		st.Lines = append(st.Lines, line)
	}
	if st.Amount.Sign() <= 0 {
		return nil, invalid("/args/sources", "The lines settle no amount")
	}
	st.Installments = []Installment{{Seq: 1, Amount: st.Amount, DueAt: kernel.Instant(in.By.At)}}
	return st, nil
}
