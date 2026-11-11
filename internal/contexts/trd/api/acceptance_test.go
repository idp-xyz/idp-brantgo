package api

import (
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

var someHash = strings.Repeat("a", 64)

func recordCmd() RecordResponse {
	return RecordResponse{IdempotencyKey: "k1", ResponseID: "response.1", QuoteID: "quote.1", BusinessRevision: 1, BusinessHash: someHash,
		Classification: ClassificationFullUnconditional, Summary: "完整接受", SourceRef: Evidence{SourceID: "source.1", Locator: "paragraph 1"},
		ReceivedAt: "2026-10-27T10:00:00Z", ReceivedAtBasis: ArrivalChannelServer}
}

// The closed contract of quote.response.record (Python T03 write-commands quote.response.record and
// T04 A36's limits: response_id, quote_id and source_id 64, locator 500, summary 4000 characters;
// the five classifications) with what Brant adds: the revision and its hash, the arrival and its
// basis, the customer PO number and further evidence. Python test_t04_a36 test_classifications_and_immutable_id
// (PYT-0462, the invalid classifications) and test_t04_a44 test_closed_schema_and_payload_conflict
// (PYT-0527, the form half).
func TestRecordResponseContract(t *testing.T) {
	if err := recordCmd().Validate(); err != nil {
		t.Fatal(err)
	}
	for _, c := range classifications {
		r := recordCmd()
		r.Classification = c
		r.ReceivedAt, r.ReceivedAtBasis, r.CustomerPONumber = "", "", "PO 2026/17"
		r.Evidence = []Evidence{{SourceID: "s", Locator: "l"}}
		if err := r.Validate(); err != nil {
			t.Errorf("%s: %v", c, err)
		}
	}
	for want, edit := range map[string]func(c *RecordResponse){
		"SCHEMA_INVALID /idempotency_key":              func(c *RecordResponse) { c.IdempotencyKey = "" },
		"SCHEMA_INVALID /args/response_id":             func(c *RecordResponse) { c.ResponseID = "回复 A45" },
		"SCHEMA_INVALID /args/quote_id":                func(c *RecordResponse) { c.QuoteID = "" },
		"SCHEMA_INVALID /args/business_revision":       func(c *RecordResponse) { c.BusinessRevision = 0 },
		"SCHEMA_INVALID /args/business_hash":           func(c *RecordResponse) { c.BusinessHash = strings.ToUpper(someHash) },
		"SCHEMA_INVALID /args/classification":          func(c *RecordResponse) { c.Classification = "accepted" },
		"SCHEMA_INVALID /args/summary":                 func(c *RecordResponse) { c.Summary = strings.Repeat("字", MaxSummary+1) },
		"SCHEMA_INVALID /args/source_ref/source_id":    func(c *RecordResponse) { c.SourceRef.SourceID = "" },
		"SCHEMA_INVALID /args/source_ref/locator":      func(c *RecordResponse) { c.SourceRef.Locator = strings.Repeat("x", 501) },
		"SCHEMA_INVALID /args/received_at":             func(c *RecordResponse) { c.ReceivedAt = "2026-10-27T18:00:00+08:00" },
		"SCHEMA_INVALID /args/received_at_basis":       func(c *RecordResponse) { c.ReceivedAtBasis = "verification" },
		"SCHEMA_INVALID /args/customer_po_number":      func(c *RecordResponse) { c.CustomerPONumber = strings.Repeat("9", 65) },
		"SCHEMA_INVALID /args/evidence/0/locator":      func(c *RecordResponse) { c.Evidence = []Evidence{{SourceID: "s"}} },
		"SCHEMA_INVALID /args/received_at_basis (gap)": func(c *RecordResponse) { c.ReceivedAt = "" },
	} {
		c := recordCmd()
		edit(&c)
		if got := pathOf(c.Validate()); got != strings.TrimSuffix(want, " (gap)") {
			t.Errorf("got %s, want %s", got, want)
		}
	}
	// A summary of 3600 Chinese characters is kept whole (Python test_t04_a45
	// test_local_long_summary_and_timestamp_are_preserved, PYT-0533, the form half).
	long := recordCmd()
	long.Summary = strings.Repeat("合成回复", 900)
	if err := long.Validate(); err != nil {
		t.Fatal(err)
	}
	// Invalid UTF-8 is never a summary (Python test_t04_a37
	// test_existing_acceptance_cannot_be_attached_and_invalid_unicode_is_rejected, PYT-0475, the
	// form half).
	bad := recordCmd()
	bad.Summary = "\xed\xa0\x80"
	if got := pathOf(bad.Validate()); got != "SCHEMA_INVALID /args/summary" {
		t.Fatalf("invalid UTF-8: %s", got)
	}
}

func verifyCmd() VerifyResponse {
	return VerifyResponse{IdempotencyKey: "k2", ResponseID: "response.1", AcceptanceID: "acceptance.1", BusinessRevision: 1,
		BusinessHash: someHash, CustomerRef: PartyRef{PartyID: "customer.1", Revision: 2}, ContactRef: ContactRef{ContactID: "contact.1", RowVersion: 2},
		LineIDs: []string{"L1", "L2"}}
}

// The closed contract of quote.response.verify, acceptance.invalidate and acceptance.mark_disputed:
// an empty line set is not an acceptance (Python test_t04_a37 test_quote_scope_is_rechecked,
// PYT-0470: SCHEMA_INVALID), nor a line named twice; an effectivity change needs its version, a
// reason and new evidence (SM-TRD-ACCEPTANCE).
func TestVerifyAndEffectivityContracts(t *testing.T) {
	if err := verifyCmd().Validate(); err != nil {
		t.Fatal(err)
	}
	for want, edit := range map[string]func(c *VerifyResponse){
		"SCHEMA_INVALID /args/response_id":            func(c *VerifyResponse) { c.ResponseID = "" },
		"SCHEMA_INVALID /args/acceptance_id":          func(c *VerifyResponse) { c.AcceptanceID = "a b" },
		"SCHEMA_INVALID /args/business_revision":      func(c *VerifyResponse) { c.BusinessRevision = -1 },
		"SCHEMA_INVALID /args/business_hash":          func(c *VerifyResponse) { c.BusinessHash = "0" },
		"SCHEMA_INVALID /args/customer_ref/revision":  func(c *VerifyResponse) { c.CustomerRef.Revision = 0 },
		"SCHEMA_INVALID /args/contact_ref/contact_id": func(c *VerifyResponse) { c.ContactRef.ContactID = "" },
		"SCHEMA_INVALID /args/line_ids":               func(c *VerifyResponse) { c.LineIDs = []string{} },
		"SCHEMA_INVALID /args/line_ids/1":             func(c *VerifyResponse) { c.LineIDs = []string{"L1", "L1"} },
	} {
		c := verifyCmd()
		edit(&c)
		if got := pathOf(c.Validate()); got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	}
	proof := []Evidence{{SourceID: "s", Locator: "l"}}
	for _, c := range []struct {
		err  error
		want string
	}{
		{InvalidateAcceptance{IdempotencyKey: "k", AcceptanceID: "a", ExpectedEffectivityVersion: 1, Reason: "r", Evidence: proof}.Validate(), "OK"},
		{DisputeAcceptance{IdempotencyKey: "k", AcceptanceID: "a", ExpectedEffectivityVersion: 1, Reason: "r", Evidence: proof}.Validate(), "OK"},
		{InvalidateAcceptance{IdempotencyKey: "k", AcceptanceID: "a", Reason: "r", Evidence: proof}.Validate(),
			"SCHEMA_INVALID /args/expected_effectivity_version"},
		{InvalidateAcceptance{IdempotencyKey: "k", AcceptanceID: "a", ExpectedEffectivityVersion: 1, Evidence: proof}.Validate(),
			"SCHEMA_INVALID /args/reason"},
		{DisputeAcceptance{IdempotencyKey: "k", AcceptanceID: "a", ExpectedEffectivityVersion: 1, Reason: "r"}.Validate(),
			"SCHEMA_INVALID /args/evidence"},
		{DisputeAcceptance{IdempotencyKey: "k", ExpectedEffectivityVersion: 1, Reason: "r", Evidence: proof}.Validate(),
			"SCHEMA_INVALID /args/acceptance_id"},
	} {
		if got := pathOf(c.err); got != c.want {
			t.Errorf("got %s, want %s", got, c.want)
		}
	}
	if err := ValidateAcceptanceIDs(make([]string, MaxAcceptanceRefs+1)); pathOf(err) != "SCHEMA_INVALID /acceptance_ids" {
		t.Fatalf("too many IDs: %s", pathOf(err))
	}
	if err := ValidateAcceptanceIDs([]string{"a", ""}); pathOf(err) != "SCHEMA_INVALID /acceptance_ids/1" {
		t.Fatalf("an empty ID: %s", pathOf(err))
	}
}

// AcceptanceGuard v1's verdicts as a caller reads them: a valid, formed acceptance passes whatever
// the clock says of its expires_at (TRD-INV-017); an unknown one is REFERENCE_INVALID, one of
// another revision ACCEPTANCE_SCOPE_MISMATCH, an invalidated or disputed one ACCEPTANCE_NOT_VALID
// (409).
func TestAcceptanceGuardVerdict(t *testing.T) {
	b := AcceptanceBinding{AcceptanceID: "acceptance.1", QuoteID: "quote.1", BusinessRevision: 1, BusinessHash: someHash}
	expired := time.Date(2026, 10, 28, 8, 0, 0, 0, time.UTC)
	row := func(known bool, code *string) func(dest ...any) error {
		return func(dest ...any) error {
			*dest[0].(*bool) = known
			if !known {
				*dest[11].(**string) = code
				return nil
			}
			quote, hash, response, effectivity, basis := "quote.1", someHash, "response.1", AcceptanceValid, ArrivalChannelServer
			revision, version := int64(1), int64(1)
			arrival, formed := expired.Add(-22*time.Hour), true
			*dest[1].(**string), *dest[2].(**int64), *dest[3].(**string), *dest[4].(**string) = &quote, &revision, &hash, &response
			*dest[5].(**string), *dest[6].(**int64), *dest[7].(**time.Time), *dest[8].(**string) = &effectivity, &version, &arrival, &basis
			*dest[9].(**time.Time), *dest[10].(**bool), *dest[11].(**string) = &expired, &formed, code
			return nil
		}
	}
	v, err := ScanAcceptanceGuard(b, row(true, nil))
	if err != nil || !v.Usable() || v.Check("/args/acceptance_id") != nil || !v.Formed || !v.ExpiresAt.Equal(expired) ||
		v.EffectivityVersion != 1 || v.ArrivalBasis != ArrivalChannelServer || v.ResponseID != "response.1" {
		t.Fatalf("a valid acceptance %+v %v", v, err)
	}
	for code, want := range map[string]string{
		CodeAcceptanceScopeMismatch: "ACCEPTANCE_SCOPE_MISMATCH /args/acceptance_id",
		CodeAcceptanceNotValid:      "ACCEPTANCE_NOT_VALID /args/acceptance_id",
		CodeAcceptanceLate:          "ACCEPTANCE_LATE /args/acceptance_id",
	} {
		c := code
		v, err := ScanAcceptanceGuard(b, row(true, &c))
		if err != nil || v.Usable() || pathOf(v.Check("/args/acceptance_id")) != want {
			t.Errorf("%s: %s", code, pathOf(v.Check("/args/acceptance_id")))
		}
	}
	notValid := Reject(CodeAcceptanceNotValid, "/")
	if notValid.Category != kernel.CategoryConflict {
		t.Fatalf("ACCEPTANCE_NOT_VALID is %v", notValid.Category)
	}
	unknown := kernel.CodeReferenceInvalid
	if v, _ := ScanAcceptanceGuard(b, row(false, &unknown)); v.Known || pathOf(v.Check("/x")) != "REFERENCE_INVALID /x" {
		t.Fatalf("an unknown acceptance %+v", v)
	}
	// The lookup's answer binds the same way.
	f := AcceptanceFact{AcceptanceID: "acceptance.1", Ref: QuoteRevisionRef{QuoteID: "quote.1", BusinessRevision: 1}, BusinessHash: someHash,
		Effectivity: AcceptanceValid, EffectivityVersion: 1, Formed: true, ExpiresAt: expired}
	if err := f.CheckFor(QuoteRevisionBinding{QuoteID: "quote.1", BusinessRevision: 1, BusinessHash: someHash}, "/a"); err != nil {
		t.Fatal(err)
	}
	if got := pathOf(f.CheckFor(QuoteRevisionBinding{QuoteID: "quote.1", BusinessRevision: 2, BusinessHash: someHash}, "/a")); got !=
		"ACCEPTANCE_SCOPE_MISMATCH /a" {
		t.Fatalf("another revision: %s", got)
	}
	f.Code, f.Effectivity, f.EffectivityVersion = CodeAcceptanceNotValid, AcceptanceInvalidated, 2
	if got := pathOf(f.Check("/a")); got != "ACCEPTANCE_NOT_VALID /a" {
		t.Fatalf("an invalidated acceptance: %s", got)
	}
}
