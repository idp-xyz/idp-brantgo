package api

import (
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func decideOrderCmd() DecideOrderReview {
	return DecideOrderReview{IdempotencyKey: "k1", OrderID: "order.1", ExpectedVersion: 2, ReviewID: "review.1", OrderHash: someHash,
		Decision: DecisionApprove, ExpiresAt: "2026-11-30T09:00:00Z",
		RiskAcknowledgements: []RiskAcknowledgement{{Path: "/data/cost_snapshot_ref", Code: "COST_SNAPSHOT_INELIGIBLE", Detail: "/sources/src.1"}}}
}

// The closed contracts of the order commands (Python T03 write-commands order.prepare and
// order.activate, T04 A39–A41: order_review_id, review_id and reason; PYT-0497, 0507, 0509, 0515,
// 0518 for their form): identifiers, versions and hashes; approve with a UTC expires_at (an offset
// or a timestamp without a zone is SCHEMA_INVALID) and distinct risk acknowledgements; reject with a
// reason and neither; a reason of 1–2000 characters for revoke, cancel and close; evidence for
// cancel and close.
func TestOrderCommandContracts(t *testing.T) {
	good := []interface{ Validate() error }{
		PrepareOrder{IdempotencyKey: "k1", OrderID: "order.1", QuoteID: "quote.1", BusinessRevision: 1, BusinessHash: someHash, AcceptanceID: "acceptance.1"},
		RequestOrderReview{IdempotencyKey: "k1", OrderID: "order.1", ExpectedVersion: 1, ReviewID: "review.1"},
		decideOrderCmd(),
		DecideOrderReview{IdempotencyKey: "k1", OrderID: "order.1", ExpectedVersion: 2, ReviewID: "review.1", OrderHash: someHash,
			Decision: DecisionReject, Reason: "条款待定", Evidence: []Evidence{{SourceID: "s", Locator: "l"}}},
		ActivateOrder{IdempotencyKey: "k1", OrderID: "order.1", ExpectedVersion: 3, ReviewID: "review.1"},
		RevokeOrderApproval{IdempotencyKey: "k1", OrderID: "order.1", ReviewID: "review.1", Reason: "合成测试：客户条件变化"},
		CancelOrder{IdempotencyKey: "k1", OrderID: "order.1", ExpectedVersion: 1, Reason: "r", Evidence: []Evidence{{SourceID: "s", Locator: "l"}}},
		CloseOrderUnfulfilled{IdempotencyKey: "k1", OrderID: "order.1", ExpectedVersion: 1, Reason: "r", Evidence: []Evidence{{SourceID: "s", Locator: "l"}}},
	}
	for _, c := range good {
		if err := c.Validate(); err != nil {
			t.Errorf("%T: %v", c, err)
		}
	}
	for want, edit := range map[string]func(c *DecideOrderReview){
		"SCHEMA_INVALID /args/order_id":            func(c *DecideOrderReview) { c.OrderID = "" },
		"SCHEMA_INVALID /args/expected_version":    func(c *DecideOrderReview) { c.ExpectedVersion = 0 },
		"SCHEMA_INVALID /args/review_id":           func(c *DecideOrderReview) { c.ReviewID = strings.Repeat("x", 65) },
		"SCHEMA_INVALID /args/order_hash":          func(c *DecideOrderReview) { c.OrderHash = "invalid" },
		"SCHEMA_INVALID /args/decision":            func(c *DecideOrderReview) { c.Decision = "approved" },
		"SCHEMA_INVALID /args/expires_at":          func(c *DecideOrderReview) { c.ExpiresAt = "2026-10-27T09:00:00" },
		"SCHEMA_INVALID /args/expires_at (offset)": func(c *DecideOrderReview) { c.ExpiresAt = "2026-10-27T17:00:00+08:00" },
		"SCHEMA_INVALID /args/risk_acknowledgements/1": func(c *DecideOrderReview) {
			c.RiskAcknowledgements = append(c.RiskAcknowledgements, c.RiskAcknowledgements[0])
		},
		"SCHEMA_INVALID /args/risk_acknowledgements/0/path": func(c *DecideOrderReview) { c.RiskAcknowledgements[0].Path = "data" },
		"SCHEMA_INVALID /args/risk_acknowledgements/0/code": func(c *DecideOrderReview) { c.RiskAcknowledgements[0].Code = "" },
		"SCHEMA_INVALID /args/reason":                       func(c *DecideOrderReview) { c.Decision, c.ExpiresAt, c.RiskAcknowledgements = DecisionReject, "", nil },
		"SCHEMA_INVALID /args/expires_at (reject)":          func(c *DecideOrderReview) { c.Decision, c.Reason = DecisionReject, "r" },
		"SCHEMA_INVALID /args/evidence/0/locator":           func(c *DecideOrderReview) { c.Evidence = []Evidence{{SourceID: "s"}} },
	} {
		c := decideOrderCmd()
		edit(&c)
		if got := pathOf(c.Validate()); got != strings.Split(want, " (")[0] {
			t.Errorf("got %s, want %s", got, want)
		}
	}
	for want, c := range map[string]interface{ Validate() error }{
		"SCHEMA_INVALID /args/acceptance_id": PrepareOrder{IdempotencyKey: "k1", OrderID: "order.1", QuoteID: "quote.1", BusinessRevision: 1,
			BusinessHash: someHash},
		"SCHEMA_INVALID /args/business_hash": PrepareOrder{IdempotencyKey: "k1", OrderID: "order.1", QuoteID: "quote.1", BusinessRevision: 1,
			AcceptanceID: "a"},
		"SCHEMA_INVALID /args/review_id": ActivateOrder{IdempotencyKey: "k1", OrderID: "order.1", ExpectedVersion: 3},
		"SCHEMA_INVALID /args/reason": RevokeOrderApproval{IdempotencyKey: "k1", OrderID: "order.1", ReviewID: "review.1",
			Reason: strings.Repeat("x", 2001)},
		"SCHEMA_INVALID /args/reason (empty)": RevokeOrderApproval{IdempotencyKey: "k1", OrderID: "order.1", ReviewID: "review.1"},
		"SCHEMA_INVALID /args/evidence":       CancelOrder{IdempotencyKey: "k1", OrderID: "order.1", ExpectedVersion: 1, Reason: "r"},
		"SCHEMA_INVALID /idempotency_key":     CloseOrderUnfulfilled{OrderID: "order.1", ExpectedVersion: 1, Reason: "r"},
	} {
		if got := pathOf(c.Validate()); got != strings.Split(want, " (")[0] {
			t.Errorf("%T: got %s, want %s", c, got, want)
		}
	}
	if err := ValidateOrderIDs(make([]string, MaxOrderRefs+1)); pathOf(err) != "SCHEMA_INVALID /order_ids" {
		t.Errorf("too many order IDs: %s", pathOf(err))
	}
}

// Python test_t04_a39_order_gate.py test_revocation_reason_is_nonempty_bounded_and_valid_unicode
// (PYT-0497): a reason that revokes an order approval or ends an order is something a person wrote —
// empty, only white space, a control character, invalid UTF-8 or more than 2000 characters is
// SCHEMA_INVALID at /args/reason, for order.approval.revoke, order.cancel, order.close_unfulfilled,
// order.close_remainder and (when given) order.close. Python's local limit is 1000 characters, its
// public one 2000; Go has one path and keeps 2000.
func TestOrderReasonsAreWrittenByAPerson(t *testing.T) {
	ev := []Evidence{{SourceID: "s", Locator: "l"}}
	commands := map[string]func(reason string) interface{ Validate() error }{
		"order.approval.revoke": func(r string) interface{ Validate() error } {
			return RevokeOrderApproval{IdempotencyKey: "k1", OrderID: "order.1", ReviewID: "review.1", Reason: r}
		},
		"order.cancel": func(r string) interface{ Validate() error } {
			return CancelOrder{IdempotencyKey: "k1", OrderID: "order.1", ExpectedVersion: 1, Reason: r, Evidence: ev}
		},
		"order.close_unfulfilled": func(r string) interface{ Validate() error } {
			return CloseOrderUnfulfilled{IdempotencyKey: "k1", OrderID: "order.1", ExpectedVersion: 1, Reason: r, Evidence: ev}
		},
		"order.close_remainder": func(r string) interface{ Validate() error } {
			return CloseOrderRemainder{IdempotencyKey: "k1", OrderID: "order.1", ExpectedVersion: 1, Reason: r}
		},
		"order.close": func(r string) interface{ Validate() error } {
			return CloseOrder{IdempotencyKey: "k1", OrderID: "order.1", ExpectedVersion: 1, Reason: r}
		},
	}
	bad := map[string]string{
		"only spaces":          "  ",
		"tab and newline only": "\t\n",
		"NUL":                  "bad\x00text",
		"another control":      "bad\x1btext",
		"invalid UTF-8":        "bad\xed\xa0\x80",
		"over 2000 characters": strings.Repeat("x", 2001),
	}
	for name, build := range commands {
		for _, good := range []string{"合成测试：客户条件变化", strings.Repeat("理", 2000), "x"} {
			if err := build(good).Validate(); err != nil {
				t.Errorf("%s with %.12q: %v", name, good, err)
			}
		}
		for what, r := range bad {
			if got := pathOf(build(r).Validate()); got != "SCHEMA_INVALID /args/reason" {
				t.Errorf("%s, %s: %s", name, what, got)
			}
		}
		empty := pathOf(build("").Validate())
		if name == "order.close" {
			if empty != "OK" {
				t.Errorf("order.close without a reason: %s", empty)
			}
		} else if empty != "SCHEMA_INVALID /args/reason" {
			t.Errorf("%s without a reason: %s", name, empty)
		}
	}
}

// The order codes are the error catalog's rows with their categories (ORDER_REVIEW_STALE and
// ORDER_RISK_UNACKNOWLEDGED 409, the rest 422); TRD declares them once.
func TestOrderCodesFollowTheErrorCatalog(t *testing.T) {
	for code, want := range map[string]kernel.Category{CodeOrderNotDraft: kernel.CategoryInvalid, CodeOrderApprovalRequired: kernel.CategoryInvalid,
		CodeOrderApprovalRevoked: kernel.CategoryInvalid, CodeOrderApprovalExpired: kernel.CategoryInvalid,
		CodeOrderReviewExpiryInvalid: kernel.CategoryInvalid, CodeOrderReviewMismatch: kernel.CategoryInvalid,
		CodeOrderReviewStale: kernel.CategoryConflict, CodeOrderReviewImmutable: kernel.CategoryInvalid,
		CodeReviewerNotIndependent: kernel.CategoryInvalid, CodeOrderRiskUnacknowledged: kernel.CategoryConflict,
		CodeOrderNotActive: kernel.CategoryInvalid} {
		if got, ok := Category(code); !ok || got != want {
			t.Errorf("%s: %v %v", code, got, ok)
		}
		if _, clash := codes[code]; clash {
			t.Errorf("%s is declared twice", code)
		}
	}
}

// OrderGuard v1's verdicts (Context Map R19): usable for the active order at the bound revision and
// hash; REFERENCE_INVALID unknown, REFERENCE_STALE (409) at another revision or hash,
// ORDER_NOT_ACTIVE, ORDER_APPROVAL_REVOKED; one scan target per column.
func TestOrderGuardVerdict(t *testing.T) {
	b := OrderBinding{OrderID: "order.1", OrderRevision: 1, OrderHash: someHash}
	row := func(known bool, code *string, revoked bool) func(dest ...any) error {
		return func(dest ...any) error {
			if len(dest) != 10 {
				t.Fatalf("%d targets", len(dest))
			}
			*dest[0].(*bool) = known
			*dest[8].(*bool) = revoked
			*dest[9].(**string) = code
			if !known {
				return nil
			}
			status, hash, quote, bhash, review := OrderActive, someHash, "quote.1", someHash, "review.1"
			revision, brev := int64(1), int64(1)
			*dest[1].(**string), *dest[2].(**int64), *dest[3].(**string), *dest[4].(**string) = &status, &revision, &hash, &quote
			*dest[5].(**int64), *dest[6].(**string), *dest[7].(**string) = &brev, &bhash, &review
			return nil
		}
	}
	v, err := ScanOrderGuard(b, row(true, nil, false))
	if err != nil || !v.Usable() || v.Check("/args/order_ref") != nil || v.Status != OrderActive || v.ReviewID != "review.1" || v.QuoteID != "quote.1" {
		t.Fatalf("an active order %+v %v", v, err)
	}
	for code, want := range map[string]string{
		CodeReferenceStale:          "REFERENCE_STALE /args/order_ref",
		CodeOrderNotActive:          "ORDER_NOT_ACTIVE /args/order_ref",
		CodeOrderApprovalRevoked:    "ORDER_APPROVAL_REVOKED /args/order_ref",
		kernel.CodeReferenceInvalid: "REFERENCE_INVALID /args/order_ref",
	} {
		c := code
		v, err := ScanOrderGuard(b, row(code != kernel.CodeReferenceInvalid, &c, code == CodeOrderApprovalRevoked))
		if err != nil || v.Usable() || pathOf(v.Check("/args/order_ref")) != want {
			t.Errorf("%s: %s", code, pathOf(v.Check("/args/order_ref")))
		}
	}
	if (ActiveOrderFact{Code: CodeOrderApprovalRevoked}).Check("/x") == nil || (ActiveOrderFact{}).Check("/x") != nil {
		t.Error("ActiveOrderFact.Check")
	}
}
