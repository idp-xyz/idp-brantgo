package release

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipment"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// A block that appears after the confirmation (owner decision 2026-11-12 ~23:05, S5 C1c;
// FUL-INV-002, CNX-INV-001, SM-CNX-COMPLIANCE): of CNX's TradeComplianceGuard v1 answers only a
// blocked conclusion stops the documents release of a confirmed shipment, for the revision the goods
// left under as for the order's current revision. Every other answer — clear, expired, a programme
// list with a newer version (MD-47), a check reopened or evaluated again but not concluded, no check
// of the revision yet, the check's own status under another hash — leaves the confirmation's clear
// conclusion standing. The evidence names the revision. Python V0.50 has no trade compliance: the
// vectors are CNX's guard answers (migrations/cnx/000001).
func TestOnlyABlockedConclusionStopsTheDocuments(t *testing.T) {
	until := t0.Add(30 * 24 * time.Hour)
	for _, c := range []struct {
		status, reason string
		blocked        bool
	}{
		{"clear", "cleared", false},
		{"expired", "expired", false},
		{"pending", "list_updated", false},
		{"pending", "not_evaluated", false},
		{"pending", "awaiting_decision", false},
		{"pending", "order_revised", false},
		{"pending", "not_checked", false},
		{"pending", "order_changed", false},
		{"blocked", "blocked", true},
		{"blocked", "order_changed", true},
	} {
		for _, current := range []bool{false, true} {
			rev := order.OrderRevision
			if current {
				rev = 3
			}
			v := ComplianceVerdict{Order: shipment.OrderBinding{OrderID: "order.1", OrderRevision: rev, OrderHash: order.OrderHash},
				Status: c.status, Reason: c.reason, CheckID: "chk.1", DecisionID: "dec.1", RegulationID: "screening", RegulationRevision: 2,
				ValidUntil: until}
			e, seen, err := CheckComplianceBlock(v, current, "/args/shipment_id")
			name := GuardTradeComplianceCurrent
			if !current {
				name = shipment.GuardTradeCompliance
			}
			if e.Name != name || e.Value != c.status+"/"+c.reason+"/chk.1/dec.1/false/r"+strconv.FormatInt(rev, 10) {
				t.Errorf("%s/%s current=%v: entry %+v", c.status, c.reason, current, e)
			}
			if seen.Blocked != c.blocked || seen.OrderRevision != rev || seen.OrderHash != order.OrderHash || seen.CheckID != "chk.1" ||
				seen.RegulationRevision != 2 || !seen.ValidUntil.Equal(until) || seen.Status != c.status || seen.Reason != c.reason {
				t.Errorf("%s/%s current=%v: bound %+v", c.status, c.reason, current, seen)
			}
			rej, _ := kernel.AsRejection(err)
			switch {
			case !c.blocked && err != nil:
				t.Errorf("%s/%s current=%v: no block, refused %v", c.status, c.reason, current, err)
			case c.blocked && (rej == nil || rej.Code != shipment.CodeTradeComplianceNotClear || rej.Fields[0].Path != "/args/shipment_id" ||
				!strings.Contains(rej.Fields[0].Reason, "revision "+strconv.FormatInt(rev, 10)) || !strings.Contains(rej.Fields[0].Reason, "chk.1")):
				t.Errorf("%s/%s current=%v: %v", c.status, c.reason, current, err)
			}
		}
	}
	doc, _ := json.Marshal(Bindings{Rechecked: &RecheckBinding{TradeCompliance: []RecheckedCompliance{{OrderRevision: 2, Status: "blocked",
		Blocked: true}}, Screenings: []RecheckedScreening{{PartyID: "customer.1", Status: "clear", Reason: "screened"}}}})
	if !strings.Contains(string(doc), `"rechecked":{"trade_compliance":[{"order_revision":2,`) ||
		!strings.Contains(string(doc), `"blocked":true}],"screenings":[{"party_id":"customer.1","status":"clear","reason":"screened",`) {
		t.Fatalf("bindings JSON %s", doc)
	}
}
