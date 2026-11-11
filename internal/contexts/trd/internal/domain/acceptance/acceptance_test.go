package acceptance

import (
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/response"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// SC-07 and ES-08: the quote is formalized on 2026-10-21 and expires on 2026-10-28T08:00:00Z (day
// 7); the acceptance reaches the channel server on 2026-10-27T10:00:00Z (day 6) and is verified on
// 2026-10-29 (day 8). The revision quotes customer.1 at revision 2, contact.1 at row version 2, and
// lines L1 and L2 (Python examples: red and blue M).
var (
	formalized = time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)
	expires    = time.Date(2026, 10, 28, 8, 0, 0, 0, time.UTC)
	arrived    = time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	verifiedOn = time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	hash       = strings.Repeat("b", 64)
	customer   = quote.PartyRef{PartyID: "customer.1", Revision: 2}
	contact    = quote.ContactRef{ContactID: "contact.1", RowVersion: 2}
	clerk      = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "clerk"}
	verifier   = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "verifier"}
	readable   = map[string]bool{"mail.1": true, "mail.2": true}
)

func activeQuote() *quote.Quote {
	q := quote.New("quote.1", "clerk", "trade")
	q.Revisions = []*quote.Revision{{Number: 1, Status: quote.Active,
		Data:   quote.Data{Customer: &customer, Contact: &contact, Lines: []quote.Line{{LineID: "L1"}, {LineID: "L2"}}},
		Seal:   &quote.Seal{BusinessHash: hash, ExpiresAt: expires},
		Formal: &quote.Formalization{Document: quote.Document{Number: "QT-2026-00001"}, Commit: quote.Commit{At: formalized}}}}
	return q
}

// recorded is a response to revision 1 classified c, recorded on day 6 at noon or an hour after its
// message mail.1 arrived as arrival says (nil: none stated).
func recorded(t *testing.T, q *quote.Quote, id string, c response.Classification, arrival *response.Arrival) *response.Response {
	t.Helper()
	at := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	if arrival != nil && arrival.At.After(at) {
		at = arrival.At.Add(time.Hour)
	}
	r, _, err := response.Record(q, nil, response.Draft{ID: id, QuoteID: "quote.1", Revision: 1, BusinessHash: hash, Classification: c,
		Summary: "接受", Message: quote.Evidence{SourceID: "mail.1", Locator: id}, Arrival: arrival}, readable, clerk, 1, at)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func verification(id string) Verification {
	return Verification{AcceptanceID: id, Revision: 1, BusinessHash: hash, Customer: customer, Contact: contact, LineIDs: []string{"L2", "L1"}}
}

func channel(at time.Time) *response.Arrival {
	return &response.Arrival{At: at, Basis: response.Channel}
}

func code(err error) string {
	if rej, ok := kernel.AsRejection(err); ok {
		return rej.Code + " " + rej.Fields[0].Path
	}
	if err != nil {
		return err.Error()
	}
	return "OK"
}

// SC-07, ES-08, BC-AT13's premise (TRD-INV-012, 017; ADR-BR-013): an acceptance that arrived on
// day 6 and is verified on day 8, after the quote expired, forms the contract at its arrival; the
// acceptance records the arrival, its basis and the expires_at it was judged against, is valid at
// effectivity version 1 with the verification as its first ledger entry, and stands for the
// contract whatever the clock says afterwards. This is the deliberate divergence from Python T04
// A37 (test_expiry_boundary_and_historical_replay, PYT-0473), which refuses a verification at or
// after expires_at with QUOTE_EXPIRED.
func TestTimelyAcceptanceFormsTheContractWhenVerifiedAfterExpiry(t *testing.T) {
	q := activeQuote()
	r := recorded(t, q, "response.1", response.FullUnconditional, channel(arrived))
	a, replay, err := Verify(r, q, verification("acceptance.1"), Known{}, readable, verifier, 4, verifiedOn)
	if err != nil || replay {
		t.Fatalf("%v %v", replay, err)
	}
	if !a.ArrivalAt.Equal(arrived) || a.ArrivalBasis != string(response.Channel) || !a.ExpiresAt.Equal(expires) || !a.Formed() ||
		a.Effectivity != Valid || a.Version != 1 || a.Number != "QT-2026-00001" || a.Verified.By != verifier || !a.Verified.At.Equal(verifiedOn) ||
		a.Message != r.Message || a.ResponseID != "response.1" || strings.Join(a.LineIDs, ",") != "L1,L2" || !a.Fresh {
		t.Fatalf("the acceptance %+v", a)
	}
	if len(a.History) != 1 || a.History[0].Version != 1 || a.History[0].Effectivity != Valid || a.History[0].Act.By != verifier {
		t.Fatalf("its ledger %+v", a.History)
	}
	if err := a.Check("quote.1", 1, hash, "/args/acceptance_id"); err != nil || a.Code() != "" {
		t.Fatalf("a formed contract stands after the quote expired: %v", err)
	}
	// The recorder may verify: the design names no independence for quote.response.verify.
	q2 := activeQuote()
	own := recorded(t, q2, "response.2", response.FullUnconditional, channel(arrived))
	if _, _, err := Verify(own, q2, verification("acceptance.2"), Known{}, readable, clerk, 1, verifiedOn); err != nil {
		t.Fatalf("the recorder verifies: %v", err)
	}
}

// TD-AT03 and SC-07's failure: an acceptance that reached the channel server exactly at expires_at
// is late (ACCEPTANCE_LATE): no acceptance is created; the response stays recorded and is late. A
// microsecond before, it forms the contract.
func TestLateAcceptanceFormsNoContract(t *testing.T) {
	q := activeQuote()
	late := recorded(t, q, "response.late", response.FullUnconditional, channel(expires))
	if _, _, err := Verify(late, q, verification("acceptance.late"), Known{}, readable, verifier, 1, verifiedOn); code(err) !=
		"ACCEPTANCE_LATE /args/response_id" {
		t.Fatalf("an arrival at expires_at: %s", code(err))
	}
	if !late.Late(verifiedOn) {
		t.Fatal("the response reads late")
	}
	just := recorded(t, q, "response.just", response.FullUnconditional, channel(expires.Add(-time.Microsecond)))
	if a, _, err := Verify(just, q, verification("acceptance.just"), Known{}, readable, verifier, 1, verifiedOn); err != nil || !a.Formed() {
		t.Fatalf("a microsecond before expires_at: %v", err)
	}
}

// BC-AT12 and ES-08's counterexample: the sender's claimed time (an email's Date header) is not the
// arrival; without a verifiable arrival the server's verification time judges: verified before
// expires_at the contract forms then (basis verification), after it the acceptance is late. An
// import's record counts like the channel server's.
func TestClaimedTimeIsNotTheArrival(t *testing.T) {
	q := activeQuote()
	claimed := recorded(t, q, "response.claimed", response.FullUnconditional, &response.Arrival{At: arrived, Basis: response.Claimed})
	if _, _, err := Verify(claimed, q, verification("acceptance.claimed"), Known{}, readable, verifier, 1, verifiedOn); code(err) !=
		"ACCEPTANCE_LATE /args/response_id" {
		t.Fatalf("a claimed day-6 time verified on day 8: %s", code(err))
	}
	early := expires.Add(-time.Hour)
	a, _, err := Verify(claimed, q, verification("acceptance.claimed"), Known{}, readable, verifier, 1, early)
	if err != nil || !a.ArrivalAt.Equal(early) || a.ArrivalBasis != ByVerification || !a.Formed() {
		t.Fatalf("verified before expires_at: %+v %v", a, err)
	}
	none := recorded(t, q, "response.none", response.FullUnconditional, nil)
	if _, _, err := Verify(none, q, verification("acceptance.none"), Known{}, readable, verifier, 1, expires); code(err) !=
		"ACCEPTANCE_LATE /args/response_id" {
		t.Fatalf("no arrival, verified at expires_at: %s", code(err))
	}
	imported := recorded(t, q, "response.import", response.FullUnconditional, &response.Arrival{At: arrived, Basis: response.Import})
	if a, _, err := Verify(imported, q, verification("acceptance.import"), Known{}, readable, verifier, 1, verifiedOn); err != nil ||
		a.ArrivalBasis != string(response.Import) {
		t.Fatalf("an import's record: %v", err)
	}
}

// BC-AT10, TD-AT04 and ES-05 (Python test_t04_a37 test_only_full_unconditional_can_be_verified,
// PYT-0469): a conditional reply ("accept but change the colour"; a customer PO with blue M at 50),
// a partial, a rejection or an ambiguous reply is never verified (ACCEPTANCE_NOT_UNCONDITIONAL).
func TestOnlyAFullUnconditionalResponseIsVerified(t *testing.T) {
	q := activeQuote()
	for _, c := range []response.Classification{response.Conditional, response.Partial, response.Reject, response.Ambiguous} {
		r := recorded(t, q, "response."+string(c), c, channel(arrived))
		if a, _, err := Verify(r, q, verification("acceptance."+string(c)), Known{}, readable, verifier, 1, verifiedOn); a != nil ||
			code(err) != "ACCEPTANCE_NOT_UNCONDITIONAL /args/response_id" {
			t.Errorf("%s: %s", c, code(err))
		}
	}
}

// BC-AT11 (Python test_t04_a37 test_quote_scope_is_rechecked, PYT-0470; test_t03_a20
// test_wrong_version_and_unapproved_activation_are_blocked, PYT-0168, the acceptance half): the
// same amounts with another customer, contact, revision or business hash are
// ACCEPTANCE_SCOPE_MISMATCH; a line set that is not the revision's complete one is
// ACCEPTANCE_LINES_MISMATCH; the verifier reads the response's message (REFERENCE_INVALID).
func TestVerificationRestatesTheRevisionsScope(t *testing.T) {
	q := activeQuote()
	r := recorded(t, q, "response.1", response.FullUnconditional, channel(arrived))
	for want, edit := range map[string]func(v *Verification){
		"ACCEPTANCE_SCOPE_MISMATCH /args/customer_ref":      func(v *Verification) { v.Customer.PartyID = "other" },
		"ACCEPTANCE_SCOPE_MISMATCH /args/customer_ref (r1)": func(v *Verification) { v.Customer.Revision = 1 },
		"ACCEPTANCE_SCOPE_MISMATCH /args/contact_ref":       func(v *Verification) { v.Contact.RowVersion = 1 },
		"ACCEPTANCE_SCOPE_MISMATCH /args/business_revision": func(v *Verification) { v.Revision = 2 },
		"ACCEPTANCE_SCOPE_MISMATCH /args/business_hash":     func(v *Verification) { v.BusinessHash = strings.Repeat("0", 64) },
		"ACCEPTANCE_LINES_MISMATCH /args/line_ids":          func(v *Verification) { v.LineIDs = []string{"L1"} },
		"ACCEPTANCE_LINES_MISMATCH /args/line_ids (extra)":  func(v *Verification) { v.LineIDs = []string{"L1", "L2", "L3"} },
	} {
		v := verification("acceptance.1")
		edit(&v)
		_, _, err := Verify(r, q, v, Known{}, readable, verifier, 1, verifiedOn)
		if got, w := code(err), strings.SplitN(want, " (", 2)[0]; got != w {
			t.Errorf("%s: got %s", want, got)
		}
	}
	if _, _, err := Verify(r, q, verification("acceptance.1"), Known{}, map[string]bool{}, verifier, 1, verifiedOn); code(err) !=
		"REFERENCE_INVALID /args/response_id" {
		t.Fatalf("a verifier who does not read the message: %s", code(err))
	}
}

// The revision must still be the quote's active, unheld revision at the verification: a
// replacement, a cancellation or a risk hold registered after the response was recorded refuses it.
func TestVerificationRechecksTheRevision(t *testing.T) {
	for want, edit := range map[string]func(q *quote.Quote){
		"QUOTE_NOT_CURRENT /args/business_revision": func(q *quote.Quote) { q.Revisions[0].Status = quote.Superseded },
		"APPROVAL_REVOKED /args/business_revision": func(q *quote.Quote) {
			q.Holds = []*quote.Hold{{ID: "approval_revoked:a", Revision: 1, Cause: quote.HoldApprovalRevoked, Code: quote.CodeApprovalRevoked}}
		},
	} {
		q := activeQuote()
		r := recorded(t, q, "response.1", response.FullUnconditional, channel(arrived))
		edit(q)
		if _, _, err := Verify(r, q, verification("acceptance.1"), Known{}, readable, verifier, 1, verifiedOn); code(err) != want {
			t.Errorf("%s: got %s", want, code(err))
		}
	}
}

// TRD-INV-011 and the effectivity ledger (Python test_t04_a37
// test_response_binding_and_source_message_are_unique, PYT-0471,
// test_existing_acceptance_cannot_be_attached_and_invalid_unicode_is_rejected, PYT-0475; test_t04_a44
// test_verification_reuses_existing_business_guard_and_does_not_repeat_acceptance, PYT-0528;
// test_t03_a20 test_late_and_duplicate_confirmation_are_rejected_without_residue, PYT-0167, the
// duplicate half): the same verification of a verified response answers with its acceptance, another
// one is RESPONSE_VERIFICATION_IMMUTABLE; a taken acceptance ID and a revision with a valid
// acceptance are ACCEPTANCE_ALREADY_EXISTS; a message backing a live acceptance is
// SOURCE_MESSAGE_DUPLICATE. Once invalidated, the response is verified again into a new acceptance;
// nothing returns to valid.
func TestOneLiveAcceptancePerResponseMessageAndRevision(t *testing.T) {
	q := activeQuote()
	r := recorded(t, q, "response.1", response.FullUnconditional, channel(arrived))
	first, _, err := Verify(r, q, verification("acceptance.1"), Known{}, readable, verifier, 1, verifiedOn)
	if err != nil {
		t.Fatal(err)
	}
	first.Fresh = false
	known := Known{OfQuote: []*Acceptance{first}}
	if again, replay, err := Verify(r, q, verification("acceptance.1"), Known{OfQuote: known.OfQuote, Taken: first}, readable, verifier, 2,
		verifiedOn.Add(time.Hour)); err != nil || !replay || again != first {
		t.Fatalf("the same verification again: %v %v", replay, err)
	}
	changed := verification("acceptance.1")
	changed.Contact.RowVersion = 1
	if _, _, err := Verify(r, q, changed, known, readable, verifier, 1, verifiedOn); code(err) != "RESPONSE_VERIFICATION_IMMUTABLE /args/response_id" {
		t.Fatalf("another verification of the response: %s", code(err))
	}
	if _, _, err := Verify(r, q, verification("acceptance.other"), known, readable, verifier, 1, verifiedOn); code(err) !=
		"RESPONSE_VERIFICATION_IMMUTABLE /args/response_id" {
		t.Fatalf("another acceptance ID for the response: %s", code(err))
	}
	second := recorded(t, q, "response.2", response.FullUnconditional, channel(arrived))
	if _, _, err := Verify(second, q, verification("acceptance.1"), Known{OfQuote: known.OfQuote, Taken: first}, readable, verifier, 1,
		verifiedOn); code(err) != "ACCEPTANCE_ALREADY_EXISTS /args/acceptance_id" {
		t.Fatalf("a taken acceptance ID: %s", code(err))
	}
	if _, _, err := Verify(second, q, verification("acceptance.2"), known, readable, verifier, 1, verifiedOn); code(err) !=
		"ACCEPTANCE_ALREADY_EXISTS /args/business_revision" {
		t.Fatalf("a second acceptance of the revision: %s", code(err))
	}

	// A dispute keeps the response and its message taken, and lets another message accept.
	if err := first.MarkDisputed(1, quote.Act{By: verifier, Epoch: 2, At: verifiedOn.Add(time.Hour), Reason: "客户否认",
		Evidence: []quote.Evidence{{SourceID: "mail.2", Locator: "p"}}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Verify(r, q, verification("acceptance.3"), known, readable, verifier, 1, verifiedOn); code(err) !=
		"RESPONSE_VERIFICATION_IMMUTABLE /args/response_id" {
		t.Fatalf("a disputed response verified again: %s", code(err))
	}
	if _, _, err := Verify(second, q, verification("acceptance.3"), Known{OfQuote: known.OfQuote, Message: first}, readable, verifier, 1,
		verifiedOn); code(err) != "SOURCE_MESSAGE_DUPLICATE /args/response_id" {
		t.Fatalf("a message backing a disputed acceptance: %s", code(err))
	}
	if a, _, err := Verify(second, q, verification("acceptance.3"), known, readable, verifier, 1, verifiedOn); err != nil || !a.Formed() {
		t.Fatalf("another message while the first is disputed: %v", err)
	}
	if err := first.Check("quote.1", 1, hash, "/a"); code(err) != "ACCEPTANCE_NOT_VALID /a" || first.Code() != CodeAcceptanceNotValid {
		t.Fatalf("a disputed acceptance: %s", code(err))
	}
}

// SM-TRD-ACCEPTANCE: valid → invalidated and valid → disputed at the effectivity version
// (VERSION_CONFLICT otherwise), each appended to the ledger with who, when, why and on which
// evidence; neither moves again (INVALID_STATE), and re-verification creates a new acceptance (UL
// TRD-AcceptanceEffectivity: 重新核实建立新 Acceptance).
func TestEffectivityLedger(t *testing.T) {
	q := activeQuote()
	r := recorded(t, q, "response.1", response.FullUnconditional, channel(arrived))
	a, _, err := Verify(r, q, verification("acceptance.1"), Known{}, readable, verifier, 1, verifiedOn)
	if err != nil {
		t.Fatal(err)
	}
	a.Fresh, a.History[0].Fresh = false, false
	act := quote.Act{By: clerk, Epoch: 5, At: verifiedOn.Add(time.Hour), Reason: "核实人误认联系人", Evidence: []quote.Evidence{{SourceID: "mail.2",
		Locator: "更正"}}}
	if err := a.Invalidate(2, act); code(err) != "VERSION_CONFLICT /args/expected_effectivity_version" || a.Changed {
		t.Fatalf("at another version: %s", code(err))
	}
	if err := a.Invalidate(1, act); err != nil {
		t.Fatal(err)
	}
	if a.Effectivity != Invalidated || a.Version != 2 || !a.Changed || len(a.History) != 2 || !a.History[1].Fresh ||
		a.History[1].Effectivity != Invalidated || a.History[1].Act.Reason != act.Reason || a.History[1].Act.By != clerk {
		t.Fatalf("invalidated %+v", a)
	}
	if err := a.MarkDisputed(2, act); code(err) != "INVALID_STATE /args/acceptance_id" {
		t.Fatalf("an invalidated acceptance disputed: %s", code(err))
	}
	if err := a.Check("quote.1", 1, hash, "/a"); code(err) != "ACCEPTANCE_NOT_VALID /a" {
		t.Fatalf("an invalidated acceptance: %s", code(err))
	}
	known := Known{OfQuote: []*Acceptance{a}}
	if _, _, err := Verify(r, q, verification("acceptance.1"), Known{OfQuote: known.OfQuote, Taken: a}, readable, verifier, 1, verifiedOn); code(err) !=
		"ACCEPTANCE_ALREADY_EXISTS /args/acceptance_id" {
		t.Fatalf("the invalidated acceptance's ID: %s", code(err))
	}
	again, _, err := Verify(r, q, verification("acceptance.1b"), known, readable, verifier, 1, verifiedOn)
	if err != nil || again.Effectivity != Valid || again.Version != 1 {
		t.Fatalf("re-verified %+v %v", again, err)
	}
	if err := again.Check("quote.1", 2, hash, "/a"); code(err) != "ACCEPTANCE_SCOPE_MISMATCH /a" {
		t.Fatalf("another revision: %s", code(err))
	}
}
