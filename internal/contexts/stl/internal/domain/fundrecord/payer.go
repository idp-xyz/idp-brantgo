package fundrecord

import "slices"

// PayerVerification is one verification that a receipt's third-party payer pays for a customer
// (S5R-D21; SM-STL-FUND fund.verify_payer; STL-INV-006): who verified it, on which evidence, why.
type PayerVerification struct {
	Seq         int
	PayerID     string
	CustomerID  string
	EvidenceIDs []string
	By          Act
}

// VerifiedFor reports whether vs verify that the fund's payer pays for customer.
func VerifiedFor(vs []PayerVerification, customer string) bool {
	return slices.ContainsFunc(vs, func(v PayerVerification) bool { return v.CustomerID == customer })
}

// VerifyPayer verifies, on evidence, that receipt f — paid by a third party — pays for customer, by
// someone other than the recorder (independence row fund.verify_payer independentOf 资金登记人), once
// per customer; prior are f's verifications so far. f's version moves by one, its state stays
// (SM-STL-FUND recorded → recorded, difference_resolved → difference_resolved).
func (f *FundRecord) VerifyPayer(customer string, evidence []string, prior []PayerVerification, by Act) (*PayerVerification, error) {
	switch {
	case f.Kind != KindReceipt:
		return nil, state("/args/fund_record_id", "Only a receipt's payer is verified; "+f.ID+" is a "+f.Kind)
	case by.By == f.Recorded.By:
		return nil, Reject(CodeIndependentReviewRequired, "/args/fund_record_id", "The recorder of the fund record does not verify its payer")
	case customer == f.CounterpartyID:
		return nil, state("/args/customer_id", "The payer "+f.CounterpartyID+" is the customer: there is no third party to verify")
	case VerifiedFor(prior, customer):
		return nil, state("/args/customer_id", "The payer of "+f.ID+" is verified for "+customer+" already")
	}
	v := &PayerVerification{Seq: len(prior) + 1, PayerID: f.CounterpartyID, CustomerID: customer, EvidenceIDs: slices.Clone(evidence), By: by}
	f.Version++
	return v, nil
}
