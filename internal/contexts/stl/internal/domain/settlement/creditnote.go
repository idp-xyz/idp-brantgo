package settlement

import (
	"slices"

	"go.idp.xyz/idp-brant/internal/contexts/stl/internal/domain/fundrecord"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Credit notes and settlement reversal (S5 unit C8, WBS S5-18; tactical model STL-AGG-SettlementStatement
// entities CreditNote and SettlementReversal; SM-STL-CREDIT-NOTE, SM-STL-SETTLEMENT; STL-INV-008,
// STL-INV-009; UL STL-CreditNote; baseline 4.2, 4.5, 4.7 S-B).
//
// A credit note adjusts the business receivable of one confirmed sales statement for a price allowance, a
// return or a claim: a sales clerk requests it with its basis and evidence, a credit manager who did not
// request it confirms or rejects it. Confirmed, it is a source for applications to that receivable
// (domain/application) — never more than it amounts to, never more than is open. It changes no shipped
// quantity, refunds nothing and touches no tax-refund material (STL-INV-008): the refund is a fund record,
// the tax impact CNX's case. A statement is reversed when nothing stands applied on it, by someone other
// than its confirmer: its lines are reversed and their sources free for a new statement; both records stay
// (STL-INV-009).

// Kinds and states of a credit note, the reversed state of a statement and its lines.
const (
	CreditPriceAllowance = "price_allowance"
	CreditReturn         = "return"
	CreditClaim          = "claim"
	CreditRequested      = "requested"
	CreditConfirmed      = "confirmed"
	CreditRejected       = "rejected"
	DecisionConfirm      = "confirm"
	DecisionReject       = "reject"
	StatusReversed       = "reversed"
	LineReversed         = "reversed"
)

// CodeSettlementHasApplications: something stands applied on the statement (STL-INV-009; unapply first).
const CodeSettlementHasApplications = "SETTLEMENT_HAS_APPLICATIONS"

// CreditNote is a credit note of a sales statement (entity of STL-AGG-SettlementStatement).
type CreditNote struct {
	ID          string
	StatementID string
	OrderID     string
	Kind        string
	Status      string
	Version     int64
	Amount      kernel.Money
	Basis       string
	EvidenceIDs []string
	OwnerActor  string
	OwnerTeam   string
	Requested   fundrecord.Act
	Decided     *fundrecord.Act // with the decision's reason
}

// Rest is what of a confirmed credit note is not applied yet, given applied, what stands applied from it.
func (n *CreditNote) Rest(applied kernel.Money) kernel.Money { return must(n.Amount.Sub(applied)) }

// Effective reports whether the credit note counts against the statement's sales (baseline 4.5: confirmed
// net sales are the confirmed statements less their effective credit notes): confirmed, on a statement that
// stands.
func (n *CreditNote) Effective(st *Statement) bool {
	return n.Status == CreditConfirmed && st.Status == StatusConfirmed
}

// Reversal is a statement's reversal record.
type Reversal struct {
	Reason      string
	EvidenceIDs []string
	By          fundrecord.Act
}

// CreditRoom is how much of statement st a credit note may still credit: what is open on its receivable
// (applied: what stands on each installment, credit note applications included) less the unapplied rest of
// its other confirmed credit notes (noteApplied: what stands applied from each).
func CreditRoom(st *Statement, applied map[int]kernel.Money, notes []CreditNote, noteApplied map[string]kernel.Money, except string) kernel.Money {
	room := st.Open(applied)
	for i := range notes {
		n := &notes[i]
		if n.ID == except || n.Status != CreditConfirmed {
			continue
		}
		a, ok := noteApplied[n.ID]
		if !ok {
			a = must(kernel.NewMoney(st.Currency(), 0))
		}
		room = must(room.Sub(n.Rest(a)))
	}
	return room
}

// CreditInput is a credit_note.request as the rules see it, with the statement read under its lock, what
// stands on its installments, its credit notes and what stands applied from each.
type CreditInput struct {
	ID          string
	Statement   *Statement
	OrderID     string
	Kind        string
	Amount      kernel.Money
	Basis       string
	EvidenceIDs []string
	Applied     map[int]kernel.Money
	Notes       []CreditNote
	NoteApplied map[string]kernel.Money
	By          fundrecord.Act
	Team        string
}

func exceedsRoom(amount, room kernel.Money) error {
	if c, _ := amount.Cmp(room); c > 0 {
		r := room
		if r.Sign() < 0 {
			r = must(kernel.NewMoney(room.Currency(), 0))
		}
		return fundrecord.Reject(fundrecord.CodeApplicationExceedsOpen, "/args/amount",
			"Only "+r.String()+" of the receivable is open beyond the confirmed credit notes not yet applied")
	}
	return nil
}

// RequestCredit is credit_note.request's rule (STL-INV-008): a credit note of a confirmed sales statement of
// the order named, in its currency, above zero and within the credit room.
func RequestCredit(in CreditInput) (*CreditNote, error) {
	st := in.Statement
	switch {
	case st.Kind != KindSales:
		return nil, state("/args/statement_id", "A credit note adjusts a business receivable: "+st.ID+" is a purchase statement")
	case st.Status != StatusConfirmed:
		return nil, state("/args/statement_id", "The statement is "+st.Status)
	case st.OrderID != in.OrderID:
		return nil, invalid("/args/order_id", "Statement "+st.ID+" settles order "+st.OrderID)
	case !slices.Contains([]string{CreditPriceAllowance, CreditReturn, CreditClaim}, in.Kind):
		return nil, invalid("/args/kind", "A credit note is a price_allowance, a return or a claim")
	case in.Amount.Currency() != st.Currency() || in.Amount.Sign() <= 0:
		return nil, invalid("/args/amount", "The amount is above zero")
	}
	if err := exceedsRoom(in.Amount, CreditRoom(st, in.Applied, in.Notes, in.NoteApplied, "")); err != nil {
		return nil, err
	}
	return &CreditNote{ID: in.ID, StatementID: st.ID, OrderID: st.OrderID, Kind: in.Kind, Status: CreditRequested, Version: 1, Amount: in.Amount,
		Basis: in.Basis, EvidenceIDs: in.EvidenceIDs, OwnerActor: in.By.By.ID, OwnerTeam: in.Team, Requested: in.By}, nil
}

// Decide is credit.confirm's rule (SM-STL-CREDIT-NOTE): a requested credit note is confirmed or rejected
// once, with a reason, by a person other than its requester (INDEPENDENT_REVIEW_REQUIRED); confirming needs
// its statement to stand and the amount within room, the credit room without this note.
func (n *CreditNote) Decide(decision string, st *Statement, room kernel.Money, by fundrecord.Act) error {
	switch {
	case n.Status != CreditRequested:
		return state("/args/credit_note_id", "The credit note is already "+n.Status)
	case by.By.ID == n.Requested.By.ID:
		return fundrecord.Reject(fundrecord.CodeIndependentReviewRequired, "/args/credit_note_id",
			"The requester of the credit note does not decide it")
	case decision != DecisionConfirm && decision != DecisionReject:
		return invalid("/args/decision", "The decision is confirm or reject")
	}
	if decision == DecisionConfirm {
		if st.Status != StatusConfirmed {
			return state("/args/credit_note_id", "Statement "+st.ID+" is "+st.Status)
		}
		if err := exceedsRoom(n.Amount, room); err != nil {
			return err
		}
		n.Status = CreditConfirmed
	} else {
		n.Status = CreditRejected
	}
	n.Version, n.Decided = n.Version+1, &by
	return nil
}

// Reverse is settlement.reverse's rule (SM-STL-SETTLEMENT, STL-INV-009): a confirmed statement, by a person
// other than its confirmer, with nothing standing applied on it (standing: the IDs of its standing
// applications — unapply them first) and no credit note waiting for a decision (pending), becomes reversed
// with its lines; the reversal is recorded.
func (s *Statement) Reverse(standing, pending []string, r Reversal) error {
	switch {
	case s.Status != StatusConfirmed:
		return state("/args/statement_id", "The statement is "+s.Status)
	case r.By.By.ID == s.Confirmed.By.ID:
		return fundrecord.Reject(fundrecord.CodeIndependentReviewRequired, "/args/statement_id", "The confirmer of the statement does not reverse it")
	case len(standing) > 0:
		return Reject(CodeSettlementHasApplications, "/args/statement_id", "Unapply first: "+join(standing))
	case len(pending) > 0:
		return state("/args/statement_id", "Decide the credit notes waiting for a decision first: "+join(pending))
	}
	s.Status, s.Version = StatusReversed, s.Version+1
	for i := range s.Lines {
		s.Lines[i].Status = LineReversed
	}
	return nil
}

func join(ids []string) string {
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += ", "
		}
		out += id
	}
	return out
}
