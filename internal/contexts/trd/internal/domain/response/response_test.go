package response

import (
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The time line of SC-07 and ES-08: the revision is formalized on day 1 and expires on day 7 at
// 08:00 (2026-10-28T08:00:00Z); the customer's acceptance reaches the channel server on day 6.
var (
	formalized = time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)
	expires    = time.Date(2026, 10, 28, 8, 0, 0, 0, time.UTC)
	arrived    = time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	hash       = strings.Repeat("b", 64)
	clerk      = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "clerk"}
	agent      = kernel.ActorRef{Kind: kernel.ActorAI, ID: "agent"}
)

// activeQuote is quote.1 with revision 1 active and formalized (number QT-2026-00001) and revision 2
// pending approval.
func activeQuote() *quote.Quote {
	q := quote.New("quote.1", "clerk", "trade")
	q.Revisions = []*quote.Revision{
		{Number: 1, Status: quote.Active, Seal: &quote.Seal{BusinessHash: hash, IssuedAt: formalized.Add(-time.Hour), ExpiresAt: expires},
			Formal: &quote.Formalization{Document: quote.Document{Number: "QT-2026-00001"}, Commit: quote.Commit{At: formalized}}},
		{Number: 2, Status: quote.PendingApproval, Seal: &quote.Seal{BusinessHash: strings.Repeat("c", 64), ExpiresAt: expires}},
	}
	return q
}

func draft() Draft {
	return Draft{ID: "response.1", QuoteID: "quote.1", Revision: 1, BusinessHash: hash, Classification: FullUnconditional, Summary: "完整接受",
		Message: quote.Evidence{SourceID: "mail.1", Locator: "paragraph 1"}, Arrival: &Arrival{At: arrived, Basis: Channel}}
}

var readable = map[string]bool{"mail.1": true, "po.1": true}

func code(err error) string {
	if rej, ok := kernel.AsRejection(err); ok {
		return rej.Code + " " + rej.Fields[0].Path
	}
	if err != nil {
		return err.Error()
	}
	return "OK"
}

// Python test_t04_a36_customer_response.py test_conditional_record_is_not_an_acceptance_or_order
// (PYT-0461) and test_classifications_and_immutable_id (PYT-0462): every classification is
// recorded as an observation bound to the active revision, its hash, number and validity end; the
// same content under the same ID answers from the stored response, other content is
// RESPONSE_IMMUTABLE. An AI agent records like a human (command catalog: 人工或 AI).
func TestRecordBindsTheActiveRevisionAndIsAppendOnly(t *testing.T) {
	q := activeQuote()
	now := arrived.Add(time.Hour)
	for _, c := range []Classification{FullUnconditional, Conditional, Partial, Reject, Ambiguous} {
		d := draft()
		d.ID, d.Classification = "response."+string(c), c
		r, replay, err := Record(q, nil, d, readable, agent, 3, now)
		if err != nil || replay || r.Number != "QT-2026-00001" || !r.ExpiresAt.Equal(expires) || r.Classification != c ||
			r.Recorded.By != agent || r.Recorded.Epoch != 3 || !r.Recorded.At.Equal(now) || !r.Fresh {
			t.Fatalf("%s: %+v %v %v", c, r, replay, err)
		}
	}
	stored, _, err := Record(q, nil, draft(), readable, clerk, 1, now)
	if err != nil {
		t.Fatal(err)
	}
	stored.Fresh = false
	again, replay, err := Record(q, stored, draft(), readable, clerk, 2, now.Add(time.Hour))
	if err != nil || !replay || again != stored {
		t.Fatalf("the same response again: %v %v", replay, err)
	}
	changed := draft()
	changed.Summary = "different"
	if _, _, err := Record(q, stored, changed, readable, clerk, 2, now); code(err) != "RESPONSE_IMMUTABLE /args/response_id" {
		t.Fatalf("other content: %s", code(err))
	}
	other := draft()
	other.Arrival = &Arrival{At: arrived, Basis: Import}
	if _, _, err := Record(q, stored, other, readable, clerk, 2, now); code(err) != "RESPONSE_IMMUTABLE /args/response_id" {
		t.Fatalf("another arrival basis: %s", code(err))
	}
}

// The revision a response answers is q's active, unheld formal revision at the stated hash; the
// actor reads the message and every evidence source (Python test_t04_a36
// test_permissions_scope_and_source, PYT-0463, the source half).
func TestRecordRefusesWhatItCannotAnswer(t *testing.T) {
	now := arrived.Add(time.Hour)
	for want, edit := range map[string]func(q *quote.Quote, d *Draft){
		"REFERENCE_INVALID /args/source_ref/source_id": func(_ *quote.Quote, d *Draft) { d.Message.SourceID = "missing" },
		"REFERENCE_INVALID /args/evidence/1/source_id": func(_ *quote.Quote, d *Draft) {
			d.Evidence = []quote.Evidence{{SourceID: "po.1", Locator: "p"}, {SourceID: "hidden", Locator: "p"}}
		},
		"REFERENCE_INVALID /args/business_revision":      func(_ *quote.Quote, d *Draft) { d.Revision = 2 },
		"REFERENCE_INVALID /args/business_revision (r9)": func(_ *quote.Quote, d *Draft) { d.Revision = 9 },
		"BUSINESS_HASH_MISMATCH /args/business_hash":     func(_ *quote.Quote, d *Draft) { d.BusinessHash = strings.Repeat("0", 64) },
		"QUOTE_NOT_CURRENT /args/business_revision": func(q *quote.Quote, _ *Draft) {
			q.Revisions[0].Status = quote.Superseded
		},
		"QUOTE_NOT_CURRENT /args/business_revision (cancelled)": func(q *quote.Quote, _ *Draft) {
			q.Revisions[0].Status = quote.Cancelled
		},
		"APPROVAL_REVOKED /args/business_revision": func(q *quote.Quote, _ *Draft) {
			q.Holds = []*quote.Hold{{ID: "approval_revoked:a", Revision: 1, Cause: quote.HoldApprovalRevoked, Code: quote.CodeApprovalRevoked}}
		},
		"ACCEPTANCE_TIME_CONFLICT /args/received_at": func(_ *quote.Quote, d *Draft) { d.Arrival.At = now.Add(time.Second) },
		"ACCEPTANCE_TIME_CONFLICT /args/received_at (before the offer)": func(_ *quote.Quote, d *Draft) {
			d.Arrival = &Arrival{At: formalized.Add(-time.Microsecond), Basis: Import}
		},
	} {
		q, d := activeQuote(), draft()
		edit(q, &d)
		_, _, err := Record(q, nil, d, readable, clerk, 1, now)
		if got, w := code(err), strings.SplitN(want, " (", 2)[0]; got != w {
			t.Errorf("%s: got %s", want, got)
		}
	}
	// The sender's claim is kept as stated: it never judges an acceptance, so it is not bounded.
	d := draft()
	d.Arrival = &Arrival{At: formalized.Add(-48 * time.Hour), Basis: Claimed}
	if r, _, err := Record(activeQuote(), nil, d, readable, clerk, 1, now); err != nil || r.CountingArrival() != nil {
		t.Fatalf("a claimed time: %v", err)
	}
}

// TD-AT03 (登记为迟到回复): a response is late when its verifiable arrival is not before expires_at,
// or, without one, when it is recorded or read at or after expires_at; a timely one is not.
func TestLateResponses(t *testing.T) {
	q := activeQuote()
	at := func(d Draft, now time.Time) *Response {
		r, _, err := Record(q, nil, d, readable, clerk, 1, now)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	timely := at(draft(), arrived.Add(time.Hour))
	if timely.Late(expires.Add(48 * time.Hour)) {
		t.Fatal("an acceptance that arrived on day 6 stays timely after the quote expired")
	}
	exact := draft()
	exact.Arrival.At = expires
	if r := at(exact, expires.Add(time.Hour)); !r.Late(r.Recorded.At) {
		t.Fatal("an arrival at expires_at is late")
	}
	unstated := draft()
	unstated.Arrival = nil
	if r := at(unstated, expires.Add(-time.Hour)); r.Late(r.Recorded.At) || !r.Late(expires) {
		t.Fatal("without an arrival, the verification's time judges")
	}
	claimed := draft()
	claimed.Arrival = &Arrival{At: arrived, Basis: Claimed}
	if r := at(claimed, expires.Add(time.Minute)); !r.Late(r.Recorded.At) {
		t.Fatal("a claimed arrival does not count (BC-AT12)")
	}
}
