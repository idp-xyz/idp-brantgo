package declaration

import "testing"

// S5R-D13 (SM-CNX-DECLARATION unknown → not_received, CNX-INV-003): after an unknown result is verified, customs never
// having received the declaration is recorded as not_received — from unknown only, with no external sequence, on the
// verification's evidence (INVALID_KNOWN_VALUE without) — and is final: the lot's next attempt may be prepared.
func TestS5RNotReceivedAfterAnUnknownResultD13(t *testing.T) {
	c := &Case{Status: Submitted}
	if got := verdict(CheckReceipt(c, nil, func() ReceiptInput { r := receipt(NotReceived, 0); r.Evidence = 1; return r }())); got != "INVALID_STATE /args/outcome" {
		t.Fatalf("not_received follows an unknown result only: %s", got)
	}
	c.Apply(receipt(Unknown, 0))
	noEvidence := receipt(NotReceived, 0)
	if got := verdict(CheckReceipt(c, nil, noEvidence)); got != "INVALID_KNOWN_VALUE /args/evidence_ids" {
		t.Fatalf("without the verification's evidence: %s", got)
	}
	verified := receipt(NotReceived, 0)
	verified.Evidence = 1
	if err := CheckReceipt(c, nil, verified); err != nil {
		t.Fatal(err)
	}
	c.Apply(verified)
	if c.Status != NotReceived || c.LastSequence != 0 || c.Receipts != 2 || !Terminal(c.Status) || Exported(c.Status) || External(NotReceived) {
		t.Fatalf("not received %+v", c)
	}
	for _, o := range []string{Received, Released, Unknown, NotReceived} {
		r := receipt(o, 1)
		r.Evidence = 1
		if got := verdict(CheckReceipt(c, nil, r)); got != "INVALID_STATE /args/outcome" {
			t.Errorf("%s after not_received: %s", o, got)
		}
	}
	if got := verdict(CheckSubmission(c)); got != "INVALID_STATE /args/declaration_id" {
		t.Fatalf("the case is final; the next attempt submits: %s", got)
	}
}
