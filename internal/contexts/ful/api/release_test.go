package api

import "testing"

// The closed contracts of the release commands (S5-07): a known scope, the exception's stage,
// currency, positive minor-unit shortfall, timestamp and reason, and the document release's kind
// with at least one source.
func TestReleaseContracts(t *testing.T) {
	ev := EvaluateRelease{IdempotencyKey: "k", ShipmentID: "shipment.1", Scope: ScopeShipment}
	if ev.Validate() != nil {
		t.Fatal(ev.Validate())
	}
	ev.Scope = "goods"
	if outcome(ev.Validate()) != "SCHEMA_INVALID /args/scope" {
		t.Fatal(outcome(ev.Validate()))
	}
	ok := GrantReleaseException{IdempotencyKey: "k", ShipmentID: "shipment.1", Scope: ScopeDocuments, ExpectedVersion: 1, StageID: "P2", Currency: "USD",
		ShortfallMinor: "200", ValidUntil: "2026-11-20T00:00:00Z", Reason: "客户书面承诺", EvidenceIDs: []string{"ev.1"}}
	if ok.Validate() != nil {
		t.Fatal(ok.Validate())
	}
	for want, edit := range map[string]func(*GrantReleaseException){
		"SCHEMA_INVALID /args/expected_version": func(c *GrantReleaseException) { c.ExpectedVersion = 0 },
		"SCHEMA_INVALID /args/stage_id":         func(c *GrantReleaseException) { c.StageID = "" },
		"SCHEMA_INVALID /args/currency":         func(c *GrantReleaseException) { c.Currency = "usd" },
		"SCHEMA_INVALID /args/shortfall_minor":  func(c *GrantReleaseException) { c.ShortfallMinor = "2.00" },
		"SCHEMA_INVALID /args/valid_until":      func(c *GrantReleaseException) { c.ValidUntil = "tomorrow" },
		"SCHEMA_INVALID /args/reason":           func(c *GrantReleaseException) { c.Reason = " " },
		"SCHEMA_INVALID /args/evidence_ids/1":   func(c *GrantReleaseException) { c.EvidenceIDs = []string{"ev.1", "ev.1"} },
	} {
		c := ok
		edit(&c)
		if outcome(c.Validate()) != want {
			t.Errorf("%s: %s", want, outcome(c.Validate()))
		}
	}
	zero := ok
	zero.ShortfallMinor = "0"
	if outcome(zero.Validate()) != "SCHEMA_INVALID /args/shortfall_minor" {
		t.Fatal("a zero shortfall")
	}
	doc := RecordDocumentRelease{IdempotencyKey: "k", ShipmentID: "shipment.1", ExpectedVersion: 1, ReleaseKind: ReleaseTelexRelease,
		EvidenceIDs: []string{"telex"}}
	if doc.Validate() != nil {
		t.Fatal(doc.Validate())
	}
	doc.EvidenceIDs = nil
	if outcome(doc.Validate()) != "SCHEMA_INVALID /args/evidence_ids" {
		t.Fatal(outcome(doc.Validate()))
	}
	doc.EvidenceIDs, doc.ReleaseKind = []string{"telex"}, "courier"
	if outcome(doc.Validate()) != "SCHEMA_INVALID /args/release_kind" {
		t.Fatal(outcome(doc.Validate()))
	}
}
