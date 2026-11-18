package api

import "go.idp.xyz/idp-brant/internal/kernel"

// MissingForFreeze says what quote.freeze would refuse a draft of content d for before it judges
// anything else: the first member the draft lacks, in the order the freeze looks, as the freeze
// answers it — nil when d has every member a freeze needs. All but SalesFXRef, Charges,
// CustomerNotes and LegalTermsRef are needed; the customer and contact references and the cost
// snapshot have codes of their own (CUSTOMER_REFERENCE_MISSING, CONTACT_REFERENCE_MISSING,
// COST_SNAPSHOT_MISSING), the rest are DRAFT_NOT_READY, each at "/args/data/<member>" in the
// transport's names.
//
// It decides completeness only. Whether a reference exists, is in force or fits the others, the
// validity window and the prices are judged where they always were: by the freeze, which calls the
// domain's own rule (quote.Data.Complete), not this function. Neither package can call the other
// (the domain imports only the kernel and this package only the standard library and the kernel),
// so a test inside TRD takes a complete draft, removes every member in turn and every combination of
// them, and compares the two answers (internal/application, TestMissingForFreezeIsWhatTheFreezeAsks).
// A caller that wants a draft a person can freeze, such as the AI layer's gateway for an agent's
// quote.save_draft (S4 G6), asks this before it sends the draft: the draft is not saved when it
// could not be frozen as it stands, and the author is told in the same call.
//
// The function is pure: it reads d and nothing else. A draft saved by a person is not subject to it:
// quote.save_draft accepts every member as optional, as the contract of QuoteData says.
func MissingForFreeze(d QuoteData) error {
	const p = "/args/data"
	notReady := func(member string) error { return kernel.Reject(kernel.CodeDraftNotReady, p+member, "") }
	switch {
	case d.IssuerRef == nil:
		return notReady("/issuer_ref")
	case d.CustomerRef == nil:
		return Reject(CodeCustomerReferenceMiss, p+"/customer_ref")
	case d.ContactRef == nil:
		return Reject(CodeContactReferenceMiss, p+"/contact_ref")
	case d.TradingProfileRef == nil:
		return notReady("/trading_profile_ref")
	case len(d.CustomerAddressRefs) == 0:
		return notReady("/customer_address_refs")
	case d.RfqRef == nil:
		return notReady("/rfq_ref")
	case d.CostSnapshotRef == nil:
		return Reject(CodeCostSnapshotMissing, p+"/cost_snapshot_ref")
	case len(d.Lines) == 0:
		return notReady("/lines")
	case d.CurrencyRef == nil:
		return notReady("/currency_ref")
	case d.PricingPolicyRef == nil:
		return notReady("/pricing_policy_ref")
	case d.TradeTermsRef == nil:
		return notReady("/trade_terms_ref")
	case d.NamedPlaceRef == nil:
		return notReady("/named_place_ref")
	case d.PaymentTermsRef == nil:
		return notReady("/payment_terms_ref")
	case d.Delivery == nil:
		return notReady("/delivery")
	case d.DocumentTemplateRef == nil:
		return notReady("/document_template_ref")
	case d.SequenceRef == nil:
		return notReady("/sequence_ref")
	case d.ExpiresAt == "":
		return notReady("/expires_at")
	case len(d.Evidence) == 0:
		return notReady("/evidence")
	}
	return nil
}
