package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Third-party payers (S5R-D21; tactical model SM-STL-FUND fund.verify_payer, STL-INV-006; Context Map
// R43 PTY→STL TradingProfileRef v1; baseline 24.3, CN-AT08, CN-AT15). A receipt whose payer is not the
// customer is PARTY_ENTITY_MISMATCH for the customer's order stages and receivables, pending, unless
//
//   - the payer is one of the permitted payers of the customer's PTY trading profile (the active
//     revision, the payer confirmed in PTY): its receipts count as the customer's, or
//   - a credit manager verified this third-party payment for the customer (fund.verify_payer),
//     independent of the person who recorded the fund, on the evidence of a payment instruction or a
//     tripartite agreement.
//
// Every application records the basis its payer had (PayerCustomer, PayerPermitted with the profile
// revision, PayerVerified); a payable is always paid to the supplier itself.
//
//	fund.verify_payer  human only (credit manager), not the fund's recorder: verifies that receipt
//	                   FundRecordID, paid by a third party, pays for customer CustomerID (a confirmed PTY
//	                   party with role customer), on EvidenceIDs (EVD sources) for Reason. The fund
//	                   record's version moves by one, its state stays (recorded → recorded,
//	                   difference_resolved → difference_resolved); one verification per customer.
//
// Request members: fund.verify_payer {fund_record_id, expected_version, customer_id, evidence_ids,
// reason}. Receipt: FundRecordReceipt. Event idp-brant.stl.fund_payer_verified.v1 {fund_record_id,
// fund_record_version, payer_party_id, customer_party_id, evidence_ids}.
const (
	CommandVerifyPayer     = "fund.verify_payer"
	EventFundPayerVerified = "idp-brant.stl.fund_payer_verified.v1"

	// The payer basis of an application.
	PayerCustomer  = "customer"
	PayerPermitted = "permitted_payer"
	PayerVerified  = "verified_payer"
)

// VerifyPayer verifies at ExpectedVersion that fund record FundRecordID, a receipt from a third party,
// pays for customer CustomerID, on the evidence EvidenceIDs, for Reason. Human only, not the recorder.
type VerifyPayer struct {
	IdempotencyKey  string
	FundRecordID    string
	ExpectedVersion int64
	CustomerID      string
	EvidenceIDs     []string
	Reason          string
}

func (VerifyPayer) stlCommand() {}

// Validate checks the closed contract.
func (c VerifyPayer) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.FundRecordID):
		return schema("/args/fund_record_id")
	case !versionOK(c.ExpectedVersion):
		return schema("/args/expected_version")
	case !kernel.ValidIdentifier(c.CustomerID):
		return schema("/args/customer_id")
	case !Reason(c.Reason):
		return schema("/args/reason")
	}
	return evidence(c.EvidenceIDs, "/args/evidence_ids", 1)
}

// PayerVerificationView is one verification of a receipt's third-party payer.
type PayerVerificationView struct {
	Seq         int
	PayerID     string
	CustomerID  string
	EvidenceIDs []string
	Reason      string
	VerifiedBy  kernel.ActorRef
	VerifiedAt  time.Time
}

// PayerCommands is STL's command entry point for third-party payers; errors as for Commands.
type PayerCommands interface {
	VerifyPayer(ctx context.Context, cmd VerifyPayer) (FundRecordReceipt, error)
}
