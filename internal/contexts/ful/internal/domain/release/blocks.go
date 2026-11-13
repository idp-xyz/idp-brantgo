package release

import (
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipment"
)

// A block that appears after the confirmation (owner decision 2026-11-12 ~23:05, S5 unit C1c; a
// follow-up of C1b and WBS S5-07; FUL-INV-002, CNX-INV-001, SM-CNX-COMPLIANCE, PTY-INV-002, WF-07,
// R44, R56). Releasing the original bill of lading hands over title, and doing so to a sanctioned
// party must not pass on an old verdict. So the documents release of a confirmed shipment, besides
// what confirmed.go judges, asks again in its own transaction
//
//   - CNX's TradeComplianceGuard v1 on the order revision the goods left under and, when the order
//     moved on, on its current revision: TRADE_COMPLIANCE_NOT_CLEAR when compliance decided that
//     revision blocked since — the check reopened (a new regulation revision, a new programme list
//     version, its expiry) and concluded blocked, or the amended revision's check (opened by
//     trade_compliance.reopen, cause order_amended) concluded blocked. A conclusion that merely
//     expired, a programme list with a newer version, or a check reopened or re-evaluated but not
//     concluded is no block: the goods left under the confirmation's clear conclusion, and an earlier
//     revision's check can never be concluded again (CNX evaluates only an active order's current
//     revision), so treating those as blocks would hold a shipped order's documents for ever. The
//     guard answers blocked before it looks at validity or list versions, and a blocked conclusion
//     never lapses, so no block is hidden behind another answer. A blocked conclusion compliance
//     evaluates again on changed facts (SM-CNX-COMPLIANCE blocked → pending) answers pending until
//     it is concluded again: the guard does not tell it from a reopened clear one.
//   - PTY's PartyScreeningGuard v1 on every party the shipment screened at its confirmation:
//     anything but clear is PARTY_SCREENING_NOT_CLEAR, as at the confirmation. PTY's guard answers
//     party_changed, expired and list_updated before a decided true match, so only "clear" shows that
//     no block was decided since; and a confirmed party can always be screened again.
//
// The confirmation's own verdicts stay in the bindings; the answers of this transaction are bound
// beside them (Bindings.Rechecked) and named in the guard evidence.

// ComplianceBlocked is the TradeComplianceGuard v1 status of a revision compliance concluded
// blocked: the only one that stops the documents release of a confirmed shipment.
const ComplianceBlocked = "blocked"

// GuardTradeComplianceCurrent names, among the guard evidence of the documents release of a
// confirmed shipment whose order moved to a later revision, CNX's answer for that current revision;
// shipment.GuardTradeCompliance names its answer for the revision the goods left under.
const GuardTradeComplianceCurrent = shipment.GuardTradeCompliance + "/current"

// ComplianceVerdict is CNX's TradeComplianceGuard v1 on Order, a revision of the shipment's order.
type ComplianceVerdict struct {
	Order              shipment.OrderBinding
	Status             string
	Reason             string
	CheckID            string
	DecisionID         string
	LicenseRequired    bool
	RegulationID       string
	RegulationRevision int64
	ValidUntil         time.Time
}

// The answers the documents release of a confirmed shipment got when it asked again (Rechecked).
type (
	// RecheckedCompliance is CNX's answer for one order revision; Blocked says it stops the release.
	RecheckedCompliance struct {
		OrderRevision      int64     `json:"order_revision"`
		OrderHash          string    `json:"order_hash"`
		Status             string    `json:"status"`
		Reason             string    `json:"reason"`
		CheckID            string    `json:"check_id"`
		DecisionID         string    `json:"decision_id"`
		LicenseRequired    bool      `json:"license_required"`
		RegulationID       string    `json:"regulation_id"`
		RegulationRevision int64     `json:"regulation_revision"`
		ValidUntil         time.Time `json:"valid_until"`
		Blocked            bool      `json:"blocked"`
	}
	// RecheckedScreening is PTY's answer for one party now.
	RecheckedScreening struct {
		PartyID       string `json:"party_id"`
		Status        string `json:"status"`
		Reason        string `json:"reason"`
		ScreeningID   string `json:"screening_id"`
		PartyRevision int64  `json:"party_revision"`
	}
	// RecheckBinding is what CNX answered for each order revision asked (the revision the goods left
	// under first, then the current one when later) and what PTY answered for each party (party ID
	// order).
	RecheckBinding struct {
		TradeCompliance []RecheckedCompliance `json:"trade_compliance"`
		Screenings      []RecheckedScreening  `json:"screenings"`
	}
)

// Rechecked are the guards the documents release of a confirmed shipment asks again (blocks.go):
// CNX for a block, PTY for clear.
var Rechecked = []string{shipment.GuardTradeCompliance, shipment.GuardPartyScreening}

// CheckComplianceBlock judges at path CNX's answer v for a revision the documents release of a
// confirmed shipment asks — the revision the goods left under, or (current) the order's later
// current revision: TRADE_COMPLIANCE_NOT_CLEAR only when compliance concluded that revision blocked;
// any other answer leaves the confirmation's clear conclusion standing. The evidence entry is CNX's
// status/reason/check_id/decision_id/license_required with the revision.
func CheckComplianceBlock(v ComplianceVerdict, current bool, path string) (Entry, RecheckedCompliance, error) {
	rev := strconv.FormatInt(v.Order.OrderRevision, 10)
	name := shipment.GuardTradeCompliance
	if current {
		name = GuardTradeComplianceCurrent
	}
	e := Entry{Name: name, Value: v.Status + "/" + v.Reason + "/" + v.CheckID + "/" + v.DecisionID + "/" + strconv.FormatBool(v.LicenseRequired) +
		"/r" + rev}
	seen := RecheckedCompliance{OrderRevision: v.Order.OrderRevision, OrderHash: v.Order.OrderHash, Status: v.Status, Reason: v.Reason,
		CheckID: v.CheckID, DecisionID: v.DecisionID, LicenseRequired: v.LicenseRequired, RegulationID: v.RegulationID,
		RegulationRevision: v.RegulationRevision, ValidUntil: v.ValidUntil, Blocked: v.Status == ComplianceBlocked}
	if !seen.Blocked {
		return e, seen, nil
	}
	which := "the order revision " + rev + " the goods left under"
	if current {
		which = "the order's current revision " + rev
	}
	return e, seen, Reject(shipment.CodeTradeComplianceNotClear, path, "Trade compliance concluded "+which+" blocked after the shipment's "+
		"confirmation (check "+v.CheckID+", decision "+v.DecisionID+"): its documents are not released")
}
