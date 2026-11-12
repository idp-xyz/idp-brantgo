package api

import (
	"errors"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The closed contracts of quote.activate, quote.replace, quote.withdraw and quote.cancel (Python
// T03_write-commands.schema.json; expected_version for Python's expected_root_version,
// expected_active_revision for expected_old_active_revision).
func TestFormalizationCommandContracts(t *testing.T) {
	proof := []Evidence{{SourceID: "source.1", Locator: "新证据"}}
	activate := Activate{IdempotencyKey: "k1", QuoteID: "quote.1", ExpectedVersion: 4, BusinessRevision: 1, ApprovalID: "approval.1",
		ExpectedApprovalEffectivityVersion: 1}
	replace := Replace{IdempotencyKey: "k1", QuoteID: "quote.1", ExpectedVersion: 9, BusinessRevision: 2, ExpectedActiveRevision: 1,
		ApprovalID: "approval.2", ExpectedApprovalEffectivityVersion: 1}
	withdraw := Withdraw{IdempotencyKey: "k1", QuoteID: "quote.1", ExpectedVersion: 3, BusinessRevision: 1, Reason: "客户撤回", Evidence: proof}
	cancel := Cancel{IdempotencyKey: "k1", QuoteID: "quote.1", ExpectedVersion: 5, BusinessRevision: 1, Reason: "客户取消", Evidence: proof}
	for _, c := range []interface{ Validate() error }{activate, replace, withdraw, cancel} {
		if err := c.Validate(); err != nil {
			t.Fatalf("%T: %v", c, err)
		}
	}
	for _, c := range []struct {
		want string
		cmd  interface{ Validate() error }
	}{
		{"SCHEMA_INVALID /idempotency_key", func() Activate { c := activate; c.IdempotencyKey = ""; return c }()},
		{"SCHEMA_INVALID /args/expected_version", func() Activate { c := activate; c.ExpectedVersion = 0; return c }()},
		{"SCHEMA_INVALID /args/business_revision", func() Activate { c := activate; c.BusinessRevision = 0; return c }()},
		{"SCHEMA_INVALID /args/approval_id", func() Activate { c := activate; c.ApprovalID = "bad id"; return c }()},
		{"SCHEMA_INVALID /args/expected_approval_effectivity_version", func() Activate {
			c := activate
			c.ExpectedApprovalEffectivityVersion = 0
			return c
		}()},
		{"SCHEMA_INVALID /args/expected_active_revision", func() Replace { c := replace; c.ExpectedActiveRevision = 0; return c }()},
		{"SCHEMA_INVALID /args/quote_id", func() Replace { c := replace; c.QuoteID = ""; return c }()},
		{"SCHEMA_INVALID /args/reason", func() Withdraw { c := withdraw; c.Reason = strings.Repeat("r", 2001); return c }()},
		{"SCHEMA_INVALID /args/evidence", func() Withdraw { c := withdraw; c.Evidence = nil; return c }()},
		{"SCHEMA_INVALID /args/evidence/0/locator", func() Cancel { c := cancel; c.Evidence = []Evidence{{SourceID: "s"}}; return c }()},
		{"SCHEMA_INVALID /args/business_revision", func() Cancel { c := cancel; c.BusinessRevision = -1; return c }()},
	} {
		if got := pathOf(c.cmd.Validate()); got != c.want {
			t.Errorf("%T: %s, want %s", c.cmd, got, c.want)
		}
	}
}

// QuoteRevisionGuard v1's scan and verdicts: a usable revision, each refusal with its code and
// category, the conversion slot only for CheckConvertible.
func TestQuoteRevisionGuardVerdict(t *testing.T) {
	ref := QuoteRevisionBinding{QuoteID: "quote.1", BusinessRevision: 2, BusinessHash: strings.Repeat("a", 64)}
	if args := QuoteRevisionGuardArgs(ref); len(args) != 3 || args[0] != "quote.1" || args[1] != int64(2) || args[2] != ref.BusinessHash {
		t.Fatalf("args %v", args)
	}
	str := func(s string) *string { return &s }
	num := func(n int64) *int64 { return &n }
	expires := time.Date(2026, 11, 20, 1, 2, 3, 4567, time.FixedZone("x", 3600))
	row := func(known bool, status *string, active *int64, code *string, hold, slot bool) func(dest ...any) error {
		return func(dest ...any) error {
			if len(dest) != 10 {
				return errors.New("one destination per column")
			}
			*dest[0].(*bool) = known
			*dest[1].(**string) = status
			*dest[2].(**int64) = active
			*dest[3].(**string) = str(ref.BusinessHash)
			*dest[4].(**string) = str("QT-2026-00001")
			*dest[5].(**string) = str(strings.Repeat("b", 64))
			*dest[6].(**time.Time) = &expires
			*dest[7].(*bool) = hold
			*dest[8].(*bool) = slot
			*dest[9].(**string) = code
			return nil
		}
	}
	for _, c := range []struct {
		name               string
		scan               func(dest ...any) error
		check, convertible string
	}{
		{"active", row(true, str(StatusActive), num(2), nil, false, false), "OK", "OK"},
		{"active and converted", row(true, str(StatusActive), num(2), nil, false, true), "OK", "CONVERSION_SLOT_CONFLICT /p"},
		{"superseded", row(true, str(StatusSuperseded), num(3), str(CodeQuoteNotCurrent), false, false), "QUOTE_NOT_CURRENT /p", "QUOTE_NOT_CURRENT /p"},
		{"held", row(true, str(StatusActive), num(2), str(CodeApprovalRevoked), true, false), "APPROVAL_REVOKED /p", "APPROVAL_REVOKED /p"},
		{"another hash", row(true, str(StatusActive), num(2), str(CodeBusinessHashMismatch), false, false), "BUSINESS_HASH_MISMATCH /p",
			"BUSINESS_HASH_MISMATCH /p"},
		{"unknown", row(false, nil, nil, str(kernel.CodeReferenceInvalid), false, false), "REFERENCE_INVALID /p", "REFERENCE_INVALID /p"},
	} {
		v, err := ScanQuoteRevisionGuard(ref, c.scan)
		if err != nil {
			t.Fatal(err)
		}
		if got := pathOf(v.Check("/p")); got != c.check {
			t.Errorf("%s: Check %s, want %s", c.name, got, c.check)
		}
		if got := pathOf(v.CheckConvertible("/p")); got != c.convertible {
			t.Errorf("%s: CheckConvertible %s, want %s", c.name, got, c.convertible)
		}
		if v.Ref != ref || (c.name == "active" && (!v.Usable() || v.Number != "QT-2026-00001" || v.ActiveRevision != 2 ||
			!v.ExpiresAt.Equal(kernel.Instant(expires)) || v.ExpiresAt.Location() != time.UTC)) {
			t.Errorf("%s: %+v", c.name, v)
		}
	}
	if rej, ok := kernel.AsRejection((QuoteRevisionVerdict{Known: true, Code: CodeQuoteNotCurrent}).Check("/p")); !ok ||
		rej.Category != kernel.CategoryConflict {
		t.Fatal("QUOTE_NOT_CURRENT is a conflict")
	}
	if (QuoteRevisionVerdict{}).Check("/x") == nil {
		t.Fatal("a verdict the guard did not give is REFERENCE_INVALID")
	}
	broken := errors.New("connection lost")
	if _, err := ScanQuoteRevisionGuard(ref, func(...any) error { return broken }); !errors.Is(err, broken) {
		t.Fatal(err)
	}
}

// FormalRevision's verdicts and the lookups' reference limits.
func TestFormalRevisionAndLookupContracts(t *testing.T) {
	for _, c := range []struct {
		r                  FormalRevision
		check, convertible string
	}{
		{FormalRevision{Active: true}, "OK", "OK"},
		{FormalRevision{Active: true, ConversionTaken: true}, "OK", "CONVERSION_SLOT_CONFLICT /p"},
		{FormalRevision{OnHold: true, Code: CodeApprovalRevoked}, "APPROVAL_REVOKED /p", "APPROVAL_REVOKED /p"},
		{FormalRevision{Code: CodeFormalDocumentIntegrityFailure}, "FORMAL_DOCUMENT_INTEGRITY_FAILURE /p", "FORMAL_DOCUMENT_INTEGRITY_FAILURE /p"},
		{FormalRevision{Code: kernel.CodeReferenceInvalid}, "REFERENCE_INVALID /p", "REFERENCE_INVALID /p"},
	} {
		if got := pathOf(c.r.Check("/p")); got != c.check {
			t.Errorf("%+v: Check %s", c.r, got)
		}
		if got := pathOf(c.r.CheckConvertible("/p")); got != c.convertible {
			t.Errorf("%+v: CheckConvertible %s", c.r, got)
		}
	}
	refs := make([]QuoteRevisionRef, MaxRevisionRefs+1)
	for i := range refs {
		refs[i] = QuoteRevisionRef{QuoteID: "q", BusinessRevision: 1}
	}
	if got := pathOf(ValidateRevisionRefs(refs)); got != "SCHEMA_INVALID /refs" {
		t.Errorf("too many refs: %s", got)
	}
	if got := pathOf(ValidateRevisionRefs([]QuoteRevisionRef{{QuoteID: "q"}})); got != "SCHEMA_INVALID /refs/0/business_revision" {
		t.Errorf("revision 0: %s", got)
	}
	if got := pathOf(ValidateQuoteIDs([]string{"ok", "bad id"})); got != "SCHEMA_INVALID /quote_ids/1" {
		t.Errorf("a bad quote ID: %s", got)
	}
}
