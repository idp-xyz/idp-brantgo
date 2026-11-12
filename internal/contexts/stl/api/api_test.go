package api

import (
	"errors"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func code(err error) string {
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code + " " + r.Fields[0].Path
	}
	if err != nil {
		return err.Error()
	}
	return "OK"
}

const orderHash = "4f2a6c9e0b1d3f5a7c9e1b3d5f7a9c1e3b5d7f9a1c3e5b7d9f1a3c5e7b9d1f3a"

var policy = ConfigRef{Kind: KindCreditPolicy, ConfigID: "credit.usd", Revision: 1}

func limitRequest() RequestCreditLimit {
	return RequestCreditLimit{IdempotencyKey: "k1", RequestID: "limit.1", PartyID: "customer.1",
		Currency:  ConfigRef{Kind: KindCurrency, ConfigID: "USD", Revision: 1},
		Sources:   []LimitSource{{Source: SourceInternalAssessment, Amount: "10000.00", EvidenceIDs: []string{"assessment.1"}}},
		ValidFrom: "2026-11-12T00:00:00Z", ValidUntil: "2027-11-12T00:00:00Z", CreditPolicy: policy, Reason: "合成内部评估额度"}
}

// The closed contract of credit_limit.request (SM-STL-CREDIT-LIMIT, ADR-BR-016): one or both
// accepted sources with amounts and evidence, an interval that ends after it starts, POL references
// of the right kinds and a reason.
func TestRequestCreditLimitContract(t *testing.T) {
	for want, edit := range map[string]func(c *RequestCreditLimit){
		"OK": func(*RequestCreditLimit) {},
		"OK ": func(c *RequestCreditLimit) {
			c.Sources = append(c.Sources, LimitSource{Source: SourceSinosureBuyerLimit, Amount: "8000", EvidenceIDs: []string{"sinosure.1"}})
		},
		"SCHEMA_INVALID /idempotency_key":        func(c *RequestCreditLimit) { c.IdempotencyKey = "" },
		"SCHEMA_INVALID /args/request_id":        func(c *RequestCreditLimit) { c.RequestID = "额度 1" },
		"SCHEMA_INVALID /args/party_id":          func(c *RequestCreditLimit) { c.PartyID = "" },
		"SCHEMA_INVALID /args/currency/kind":     func(c *RequestCreditLimit) { c.Currency.Kind = KindFXRate },
		"SCHEMA_INVALID /args/currency/revision": func(c *RequestCreditLimit) { c.Currency.Revision = 0 },
		"SCHEMA_INVALID /args/sources":           func(c *RequestCreditLimit) { c.Sources = nil },
		"SCHEMA_INVALID /args/sources/0/source":  func(c *RequestCreditLimit) { c.Sources[0].Source = "guess" },
		"SCHEMA_INVALID /args/sources/1/source":  func(c *RequestCreditLimit) { c.Sources = append(c.Sources, c.Sources[0]) },
		"SCHEMA_INVALID /args/sources/0/amount":  func(c *RequestCreditLimit) { c.Sources[0].Amount = "-1" },
		"SCHEMA_INVALID /args/sources/0/amount ": func(c *RequestCreditLimit) { c.Sources[0].Amount = "1e3" },
		"SCHEMA_INVALID /args/sources/0/evidence_ids": func(c *RequestCreditLimit) {
			c.Sources[0].EvidenceIDs = nil
		},
		"SCHEMA_INVALID /args/sources/0/evidence_ids/1": func(c *RequestCreditLimit) {
			c.Sources[0].EvidenceIDs = []string{"e", "e"}
		},
		"SCHEMA_INVALID /args/valid_from":         func(c *RequestCreditLimit) { c.ValidFrom = "2026-11-12" },
		"SCHEMA_INVALID /args/valid_until":        func(c *RequestCreditLimit) { c.ValidUntil = c.ValidFrom },
		"SCHEMA_INVALID /args/credit_policy/kind": func(c *RequestCreditLimit) { c.CreditPolicy.Kind = KindCurrency },
		"SCHEMA_INVALID /args/reason":             func(c *RequestCreditLimit) { c.Reason = " " },
		"SCHEMA_INVALID /args/reason ":            func(c *RequestCreditLimit) { c.Reason = strings.Repeat("x", 1001) },
	} {
		c := limitRequest()
		edit(&c)
		if got := code(c.Validate()); got != strings.TrimSpace(want) {
			t.Errorf("want %s, got %s", want, got)
		}
	}
}

func TestCommandContracts(t *testing.T) {
	fx := ConfigRef{Kind: KindFXRate, ConfigID: "cny.usd", Revision: 2}
	cases := []struct {
		cmd  interface{ Validate() error }
		want string
	}{
		{PublishCreditLimit{IdempotencyKey: "k", RequestID: "l", ExpectedVersion: 1, Decision: DecisionPublish, CreditPolicy: policy, Reason: "r"}, "OK"},
		{PublishCreditLimit{IdempotencyKey: "k", RequestID: "l", ExpectedVersion: 1, Decision: DecisionRelease, CreditPolicy: policy, Reason: "r"},
			"SCHEMA_INVALID /args/decision"},
		{PublishCreditLimit{IdempotencyKey: "k", RequestID: "l", Decision: DecisionReject, CreditPolicy: policy, Reason: "r"},
			"SCHEMA_INVALID /args/expected_version"},
		{ReserveCredit{IdempotencyKey: "k", ReservationID: "r", OrderID: "o", OrderRevision: 1, OrderHash: orderHash, CreditPolicy: policy}, "OK"},
		{ReserveCredit{IdempotencyKey: "k", ReservationID: "r", OrderID: "o", OrderRevision: 1, OrderHash: orderHash, CreditPolicy: policy, FXRate: &fx},
			"OK"},
		{ReserveCredit{IdempotencyKey: "k", ReservationID: "r", OrderID: "o", OrderRevision: 1, OrderHash: orderHash, CreditPolicy: policy,
			FXRate: &ConfigRef{Kind: KindCurrency, ConfigID: "USD", Revision: 1}}, "SCHEMA_INVALID /args/fx_rate/kind"},
		{ReserveCredit{IdempotencyKey: "k", ReservationID: "r", OrderID: "o", OrderRevision: 1, OrderHash: "ABC", CreditPolicy: policy},
			"SCHEMA_INVALID /args/order_hash"},
		{ReserveCredit{IdempotencyKey: "k", ReservationID: "r", OrderID: "o", OrderHash: orderHash, CreditPolicy: policy},
			"SCHEMA_INVALID /args/order_revision"},
		{DecideCreditException{IdempotencyKey: "k", DecisionID: "d", ExpectedVersion: 1, Decision: DecisionRelease, CreditPolicy: policy, Reason: "r"}, "OK"},
		{DecideCreditException{IdempotencyKey: "k", DecisionID: "d", ExpectedVersion: 1, Decision: DecisionPublish, CreditPolicy: policy, Reason: "r"},
			"SCHEMA_INVALID /args/decision"},
		{DecideCreditException{IdempotencyKey: "k", DecisionID: "d", ExpectedVersion: 1, Decision: DecisionReject, CreditPolicy: policy},
			"SCHEMA_INVALID /args/reason"},
		{ReleaseCredit{IdempotencyKey: "k", ReservationID: "r", ExpectedVersion: 1, Reason: "订单未生效"}, "OK"},
		{ReleaseCredit{IdempotencyKey: "k", ReservationID: "r", ExpectedVersion: 1}, "SCHEMA_INVALID /args/reason"},
		{ConsumeCredit{IdempotencyKey: "k", ReservationID: "r", OrderID: "o", OrderRevision: 1, OrderHash: orderHash, ActivatedAt: "2026-11-12T08:00:00Z"}, "OK"},
		{ConsumeCredit{IdempotencyKey: "k", ReservationID: "r", OrderID: "o", OrderRevision: 1, OrderHash: orderHash, ActivatedAt: "now"},
			"SCHEMA_INVALID /args/activated_at"},
	}
	for _, c := range cases {
		if got := code(c.cmd.Validate()); got != c.want {
			t.Errorf("%+v: want %s, got %s", c.cmd, c.want, got)
		}
	}
}

// CreditGuard v1's verdicts as TRD receives them (R57): covered and documents_secured are satisfied
// for the bound order hash; everything else is CREDIT_RESERVATION_REQUIRED (409).
func TestCreditGuardVerdicts(t *testing.T) {
	q := CreditQuery{OrderID: "o", OrderRevision: 1, OrderHash: orderHash, PortionMinor: "67200", Currency: "USD", At: time.Now()}
	row := func(status string, hash string, code *string) func(dest ...any) error {
		return func(dest ...any) error {
			*dest[0].(*string) = status
			id, cur, amount, basis := "r.1", "USD", "67200", BasisWithinLimit
			*dest[1].(**string), *dest[3].(**string), *dest[5].(**string), *dest[6].(**string), *dest[8].(**string) = &id, &hash, &cur, &amount, &basis
			*dest[11].(**string) = code
			return nil
		}
	}
	required := CodeCreditReservationRequired
	for _, c := range []struct {
		status, hash string
		code         *string
		want         string
	}{
		{CreditCovered, orderHash, nil, "OK"},
		{CreditDocumentsSecured, orderHash, nil, "OK"},
		{CreditCovered, strings.Repeat("0", 64), nil, "CREDIT_RESERVATION_REQUIRED /args/order_id"},
		{CreditMissing, "", &required, "CREDIT_RESERVATION_REQUIRED /args/order_id"},
		{CreditExpired, orderHash, &required, "CREDIT_RESERVATION_REQUIRED /args/order_id"},
		{CreditInsufficient, orderHash, &required, "CREDIT_RESERVATION_REQUIRED /args/order_id"},
	} {
		v, err := ScanCreditGuard(q, row(c.status, c.hash, c.code))
		if err != nil {
			t.Fatal(err)
		}
		if got := code(v.Check("/args/order_id")); got != c.want || v.ReservationID != "r.1" || v.ReservedMinor != "67200" {
			t.Errorf("%s: %s %+v", c.status, got, v)
		}
		if r, _ := kernel.AsRejection(v.Check("/x")); r != nil && r.Category != kernel.CategoryConflict {
			t.Errorf("%s: category %s", c.status, r.Category)
		}
	}
	if _, err := ScanCreditGuard(q, func(...any) error { return errors.New("no row") }); err == nil {
		t.Fatal("a scan error")
	}
	if args := CreditGuardArgs(q); len(args) != 5 || args[2] != "67200" {
		t.Fatalf("args %v", args)
	}
}

func TestRejectPanicsOnUndeclaredCodes(t *testing.T) {
	if c, ok := Category(CodeCreditDecisionNotAuthorized); !ok || c != kernel.CategoryForbidden {
		t.Fatal(c)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("no panic")
		}
	}()
	_ = Reject("SURPRISE", "/x", "")
}
