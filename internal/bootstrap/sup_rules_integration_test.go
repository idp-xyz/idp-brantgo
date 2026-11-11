//go:build integration

package bootstrap_test

import (
	"context"
	"sync"
	"testing"
	"time"

	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	supapi "go.idp.xyz/idp-brant/internal/contexts/sup/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// refuseSeal saves content of kind as a fresh draft, has the reviewer seal it and expects the
// rejection code at path; the store is unchanged by the refusal (Python test_costing_domains.py
// DomainCostingTests.reject_input).
func (s *supWorld) refuseSeal(kind string, content any, wantCode, wantPath string) {
	s.t.Helper()
	id := s.key()
	_, err := s.save(s.author, kind, id, 0, content)
	s.must(err)
	before := s.supState()
	_, err = s.seal(s.reviewer, kind, id, 1, 1, 0)
	if code(err) != wantCode || (wantPath != "" && path(err) != wantPath) {
		s.t.Fatalf("seal of %s: %s at %s, want %s at %s (%v)", kind, code(err), path(err), wantCode, wantPath, err)
	}
	if s.supState() != before {
		s.t.Fatalf("a refused seal of %s changed the store", kind)
	}
}

// Verification rules of a supplier offer: Python test_costing_domains.py
// test_gap_in_price_tiers_cannot_be_verified, test_unexplained_zero_unit_price_is_rejected,
// test_goods_cannot_reuse_service_tax_application and
// test_invalid_delivery_time_zone_cannot_be_verified, with SUP-INV-002 (ambiguous tiers stay
// pending) and the completeness, validity and reference rules of verification.
func TestSupplierOfferVerificationRules(t *testing.T) {
	s := newSupWorld(t)
	edit := func(change func(o *supapi.SupplierOffer)) supapi.SupplierOffer {
		o := s.offer()
		change(&o)
		return o
	}
	graduated := func(o *supapi.SupplierOffer) {
		o.Lines[0].PricingMode = supapi.PricingGraduated
		o.Lines[0].Tiers = []supapi.Tier{{LowerInclusive: "0", UpperExclusive: "100", UnitPrice: "5"}, {LowerInclusive: "100", UnitPrice: "4"}}
	}
	s.verified(supapi.KindSupplierOffer, "offer.graduated", edit(graduated)) // Python test_graduated_tiers_price_each_band_once

	draft := polapi.ConfigRef{Kind: polapi.KindTaxProfile, ConfigID: "tax.zero", Revision: 2}
	_, err := s.saveConfig(s.author, polapi.KindTaxProfile, "tax.zero", 2, s.win.from, time.Time{}, polapi.ConfigData{TaxProfile: &polapi.TaxProfile{
		Name: "SYNTHETIC ZERO", Rate: "0", PriceBasis: polapi.PriceBasisExclusive, RecoveryTreatment: polapi.RecoveryNonrecoverable,
		PolicyBasis: "draft"}})
	s.must(err)
	line := "/args/data/lines/0"
	for _, c := range []struct {
		name       string
		change     func(o *supapi.SupplierOffer)
		code, path string
	}{
		{"gap in tiers", func(o *supapi.SupplierOffer) {
			graduated(o)
			o.Lines[0].Tiers[1].LowerInclusive = "101"
		}, supapi.CodePriceTierAmbiguous, line + "/tiers/1/lower_inclusive"},
		{"undetermined tiers stay pending", func(o *supapi.SupplierOffer) {
			graduated(o)
			o.Lines[0].PricingMode = ""
		}, supapi.CodePriceTierAmbiguous, line + "/pricing_mode"},
		{"overlapping tiers", func(o *supapi.SupplierOffer) {
			graduated(o)
			o.Lines[0].Tiers[1].LowerInclusive = "90"
		}, supapi.CodePriceTierAmbiguous, line + "/tiers/1/lower_inclusive"},
		{"group tiers", func(o *supapi.SupplierOffer) { o.Lines[0].PriceTierScope = supapi.TierScopeExplicitGroup },
			supapi.CodeUnsupportedPricingMode, line + "/price_tier_scope"},
		{"unknown pricing mode", func(o *supapi.SupplierOffer) { o.Lines[0].PricingMode = "volume_discount" },
			supapi.CodeUnsupportedPricingMode, line + "/pricing_mode"},
		{"unexplained zero price", func(o *supapi.SupplierOffer) { o.Lines[0].Tiers[0].UnitPrice = "0" },
			supapi.CodeCostInputNotReady, line + "/tiers/0/zero_price_reason"},
		{"fractional MOQ of pieces", func(o *supapi.SupplierOffer) { o.Lines[0].MOQ.Quantity = "100.5" },
			kernel.CodeInvalidKnownValue, line + "/moq/quantity"},
		{"goods reusing the service tax application", func(o *supapi.SupplierOffer) { o.Lines[0].TaxApplicationRef = cref(s.service) },
			kernel.CodeReferenceInvalid, line + "/tax_application_ref"},
		{"unknown delivery time zone", func(o *supapi.SupplierOffer) { o.Lines[0].Delivery.TimeZone = "Invalid/Nowhere" },
			kernel.CodeInvalidKnownValue, line + "/delivery/time_zone"},
		{"incomplete line", func(o *supapi.SupplierOffer) { o.Lines[0].Delivery = nil }, kernel.CodeDraftNotReady, line + "/delivery"},
		{"no lines", func(o *supapi.SupplierOffer) { o.Lines = nil }, kernel.CodeDraftNotReady, "/args/data/lines"},
		{"expired validity", func(o *supapi.SupplierOffer) {
			o.ValidFrom, o.ValidUntil = kernel.FormatTimestamp(s.win.from.Add(-48*time.Hour)), kernel.FormatTimestamp(s.win.from.Add(-24*time.Hour))
		}, supapi.CodeSupplyConditionExpired, "/args/data/valid_until"},
		{"tax profile not in force", func(o *supapi.SupplierOffer) { o.Lines[0].TaxProfileRef = cref(draft) },
			polapi.CodeConfigNotActive, line + "/tax_profile_ref"},
		{"line outside the RFQ", func(o *supapi.SupplierOffer) { o.RfqLineIDs = []string{"L9"} },
			kernel.CodeReferenceInvalid, "/args/data/rfq_line_ids/0"},
		{"stale RFQ version", func(o *supapi.SupplierOffer) { o.Rfq.RowVersion = 1 },
			supapi.CodeReferenceStale, "/args/data/rfq"},
		{"stale supplier version", func(o *supapi.SupplierOffer) { o.Supplier.RowVersion = 1 },
			supapi.CodeReferenceStale, "/args/data/supplier"},
		{"SKU of no covered line", func(o *supapi.SupplierOffer) { o.Lines[0].Sku.SpecRevision = 9 },
			kernel.CodeReferenceInvalid, line + "/sku"},
	} {
		t.Run(c.name, func(t *testing.T) {
			o := s.offer() // fresh values: cases never share what they change
			c.change(&o)
			if c.name == "SKU of no covered line" {
				// A specification revision that was never confirmed is refused when it is saved.
				_, err := s.save(s.author, supapi.KindSupplierOffer, s.key(), 0, o)
				if code(err) != kernel.CodeReferenceInvalid {
					t.Fatalf("save: %s", code(err))
				}
				return
			}
			s.refuseSeal(supapi.KindSupplierOffer, o, c.code, c.path)
		})
	}

	// A draft states only what is known, but what it states must exist for the caller.
	o := s.offer()
	o.Supplier = &supapi.PartyRef{PartyID: "customer.1", RowVersion: 2}
	_, err = s.save(s.author, supapi.KindSupplierOffer, "offer.customer", 0, o)
	if code(err) != kernel.CodeReferenceInvalid || path(err) != "/args/data/supplier" {
		t.Fatalf("a customer as supplier: %s at %s", code(err), path(err))
	}
	if _, err = s.save(s.author, supapi.KindSupplierOffer, "offer.empty", 0, supapi.SupplierOffer{}); err != nil {
		t.Fatalf("an empty draft: %v", err)
	}
	o = s.offer()
	o.Lines[0].CurrencyRef = cref(s.tax)
	if _, err = s.save(s.author, supapi.KindSupplierOffer, "offer.kind", 0, o); code(err) != kernel.CodeReferenceInvalid {
		t.Fatalf("a currency reference of another kind: %s", code(err))
	}
}

// SUP-INV-001: verified content is frozen; a correction is a new revision; at verification the
// revision superseded must be the current active one, and verification supersedes it atomically.
func TestSupplierOfferSupersedeChain(t *testing.T) {
	s := newSupWorld(t)
	ref := s.verified(supapi.KindSupplierOffer, "offer.1", s.offer())
	if _, err := s.save(s.author, supapi.KindSupplierOffer, "offer.1", 2, s.offer()); code(err) != kernel.CodeInvalidState {
		t.Fatalf("saving a verified offer without a revision: %s", code(err))
	}
	if _, err := s.revise(s.author, ref.Kind, ref.ID, 2, 9); code(err) != kernel.CodeInvalidState {
		t.Fatalf("revising a revision that does not exist: %s", code(err))
	}
	_, err := s.revise(s.author, ref.Kind, ref.ID, 2, 1)
	s.must(err)
	corrected := s.offer()
	corrected.Lines[0].Tiers[0].UnitPrice = "4.8"
	_, err = s.save(s.author, ref.Kind, ref.ID, 3, corrected)
	s.must(err)
	if _, err = s.seal(s.reviewer, ref.Kind, ref.ID, 4, 2, 0); code(err) != kernel.CodeVersionConflict || path(err) != "/args/supersedes_revision" {
		t.Fatalf("verifying without naming the active revision: %s at %s", code(err), path(err))
	}
	r, err := s.seal(s.reviewer, ref.Kind, ref.ID, 4, 2, 1)
	s.must(err)
	v := s.condition(ref.Kind, ref.ID)
	if r.Status != supapi.StatusActive || v.ActiveRevision != 2 || v.Revisions[0].Status != supapi.StatusSuperseded ||
		v.Revisions[0].SupersededAt.IsZero() || v.Revisions[1].Offer.Lines[0].Tiers[0].UnitPrice != "4.8" ||
		v.Revisions[0].Offer.Lines[0].Tiers[0].UnitPrice != "5" {
		t.Fatalf("after the correction: %+v", v)
	}
	events := s.events("company.A", supapi.Source)
	if len(events) != 2 || events[1].Type != supapi.EventSupplierOfferVerified || events[1].Payload["superseded_revision"] != float64(1) ||
		events[1].Payload["revision"] != float64(2) || events[1].Payload["content_hash"] != v.Revisions[1].ContentHash {
		t.Fatalf("events %+v", events)
	}

	// The database refuses what the domain never does: editing sealed content, changing a
	// verification, deleting a revision, and columns brant_app may not write.
	for sql, state := range map[string]string{
		`UPDATE brant_sup.condition_revision SET data = '{}' WHERE condition_id = 'offer.1' AND revision = 2`:                "23000",
		`UPDATE brant_sup.condition_revision SET verified_by_id = 'someone' WHERE condition_id = 'offer.1' AND revision = 1`: "23000",
		`UPDATE brant_sup.condition_revision SET status = 'active' WHERE condition_id = 'offer.1' AND revision = 1`:          "23000",
		`DELETE FROM brant_sup.condition_revision WHERE condition_id = 'offer.1'`:                                            "42501",
		`UPDATE brant_sup.condition_revision SET base_revision = 1 WHERE condition_id = 'offer.1' AND revision = 2`:          "42501",
		`UPDATE brant_sup.condition_root SET owner_actor_id = 'someone' WHERE condition_id = 'offer.1'`:                      "42501",
		`UPDATE brant_sup.condition_root SET root_version = 1, updated_at = now() WHERE condition_id = 'offer.1'`:            "23000",
		`INSERT INTO brant_sup.condition_revision (tenant_id, company_id, kind, condition_id, revision, status, data, content_hash,
			saved_by_kind, saved_by_id, saved_epoch, saved_at, contributors) SELECT tenant_id, company_id, kind, condition_id, 9, 'active',
			'{}', repeat('0', 64), 'human', 'x', 1, now(), ARRAY['x'] FROM brant_sup.condition_root WHERE condition_id = 'offer.1'`: "23514",
	} {
		if got := sqlCode(s.exec("company.A", sql)); got != state {
			t.Errorf("%s: SQLSTATE %s, want %s", sql, got, state)
		}
	}
}

// Python test_costing_domains.py test_competing_verifications_cannot_seal_the_same_input_twice
// (there a costing context; here the supply conditions SUP verifies and publishes): two concurrent
// seals of one draft with different keys commit once; the other is VERSION_CONFLICT.
func TestCompetingSupplyVerificationsSealOnce(t *testing.T) {
	s := newSupWorld(t)
	s.verified(supapi.KindSupplierOffer, "offer.1", s.offer())
	for kind, content := range map[string]any{supapi.KindSupplierOffer: s.offer(), supapi.KindPurchaseTerms: s.terms("offer.1", supapi.ValidityIndependent),
		supapi.KindLogisticsOffer: s.logistics()} {
		id := "race." + kind
		_, err := s.save(s.author, kind, id, 0, content)
		s.must(err)
		var wg sync.WaitGroup
		results := make([]error, 2)
		keys := []string{s.key(), s.key()} // two requests, two idempotency keys
		for i := range results {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, results[i] = s.c.SUP.Seal(s.reviewer, supapi.Seal{IdempotencyKey: keys[i], Kind: kind, ID: id, ExpectedVersion: 1,
					WorkingRevision: 1, Evidence: evidenceOf("核实依据")})
			}()
		}
		wg.Wait()
		codes := map[string]int{}
		for _, err := range results {
			codes[code(err)]++
		}
		if codes["OK"] != 1 || codes[kernel.CodeVersionConflict] != 1 {
			t.Fatalf("%s: %v", kind, codes)
		}
		v := s.condition(kind, id)
		if v.RootVersion != 2 || v.ActiveRevision != 1 || len(v.Revisions) != 1 {
			t.Fatalf("%s: %+v", kind, v)
		}
	}
}

// Withdrawing ends the exact revision named: the active one leaves the root without a revision in
// force and no older revision comes back; a draft is never withdrawn; a new revision is verified
// with nothing to supersede (T02 §13.2 withdraw／retire).
func TestWithdrawalEndsTheExactRevision(t *testing.T) {
	s := newSupWorld(t)
	ref := s.verified(supapi.KindSupplierOffer, "offer.1", s.offer())
	_, err := s.revise(s.author, ref.Kind, ref.ID, 2, 1)
	s.must(err)
	if _, err = s.end(s.reviewer, ref.Kind, ref.ID, 3, 2); code(err) != kernel.CodeInvalidState {
		t.Fatalf("withdrawing a draft: %s", code(err))
	}
	ai := s.other("agent", kernel.ActorAI, allPermissions...)
	system := s.other("batch", kernel.ActorSystem, allPermissions...)
	for _, ctx := range []context.Context{ai, system} {
		if _, err = s.end(ctx, ref.Kind, ref.ID, 3, 1); code(err) != kernel.CodePermissionDenied {
			t.Fatalf("withdrawal by a non-human: %s", code(err))
		}
		if _, err = s.seal(ctx, ref.Kind, ref.ID, 3, 2, 1); code(err) != kernel.CodePermissionDenied {
			t.Fatalf("verification by a non-human: %s", code(err))
		}
	}
	r, err := s.end(s.reviewer, ref.Kind, ref.ID, 3, 1)
	s.must(err)
	v := s.condition(ref.Kind, ref.ID)
	if r.Status != supapi.StatusWithdrawn || v.ActiveRevision != 0 || v.WorkingRevision != 2 || v.Revisions[0].End == nil ||
		v.Revisions[0].End.Reason != "撤销" {
		t.Fatalf("after the withdrawal %+v %+v", r, v)
	}
	if _, err = s.end(s.reviewer, ref.Kind, ref.ID, 4, 1); code(err) != kernel.CodeInvalidState {
		t.Fatalf("withdrawing twice: %s", code(err))
	}
	resolved, err := s.c.SUP.ResolveSupplyConditions(s.author, []supapi.SupplyConditionRef{ref}, time.Now())
	s.must(err)
	if resolved[ref].Code != supapi.CodeSupplyConditionNotCurrent || resolved[ref].Status != supapi.StatusWithdrawn {
		t.Fatalf("resolved %+v", resolved[ref])
	}
	if _, err = s.seal(s.reviewer, ref.Kind, ref.ID, 4, 2, 0); err != nil {
		t.Fatalf("verifying the revision with nothing active: %v", err)
	}
	events := s.events("company.A", supapi.Source)
	if len(events) != 3 || events[1].Type != supapi.EventSupplierOfferWithdrawn || events[1].Payload["active_revision"] != float64(0) {
		t.Fatalf("events %+v", events)
	}
}

// SUP-INV-003 and A46: purchase terms are published by a human who never edited them, formed from
// a verified offer revision of the same supplier; dependent terms follow their source offer,
// independent terms keep their own validity; retiring ends them.
func TestPurchaseTermsIndependentAndDependent(t *testing.T) {
	s := newSupWorld(t)
	offer := s.verified(supapi.KindSupplierOffer, "offer.1", s.offer())
	_, err := s.save(s.author, supapi.KindPurchaseTerms, "terms.ind", 0, s.terms("offer.1", supapi.ValidityIndependent))
	s.must(err)
	if _, err = s.seal(s.author, supapi.KindPurchaseTerms, "terms.ind", 1, 1, 0); code(err) != supapi.CodeIndependentReviewRequired {
		t.Fatalf("publication by the editor: %s", code(err))
	}
	_, err = s.seal(s.reviewer, supapi.KindPurchaseTerms, "terms.ind", 1, 1, 0)
	s.must(err)
	dependent := s.verified(supapi.KindPurchaseTerms, "terms.dep", s.terms("offer.1", supapi.ValidityDependent))
	independent := supapi.SupplyConditionRef{Kind: supapi.KindPurchaseTerms, ID: "terms.ind", Revision: 1}

	s.staffParty(s.author, "supplier.2", []string{ptyapi.RoleSupplier})
	other := s.terms("offer.1", supapi.ValidityIndependent)
	other.Supplier = &supapi.PartyRef{PartyID: "supplier.2", RowVersion: 2}
	s.refuseSeal(supapi.KindPurchaseTerms, other, kernel.CodeReferenceInvalid, "/args/data/supplier")
	_, err = s.save(s.author, supapi.KindSupplierOffer, "offer.draft", 0, s.offer())
	s.must(err)
	s.refuseSeal(supapi.KindPurchaseTerms, s.terms("offer.draft", supapi.ValidityIndependent), kernel.CodeReferenceInvalid, "/args/data/source_offer_ref")
	if _, err = s.save(s.author, supapi.KindPurchaseTerms, "terms.none", 0, s.terms("offer.none", supapi.ValidityIndependent)); code(err) != kernel.CodeReferenceInvalid {
		t.Fatalf("terms from no offer: %s", code(err))
	}

	_, err = s.end(s.reviewer, offer.Kind, offer.ID, 2, 1)
	s.must(err)
	resolved, err := s.c.SUP.ResolveSupplyConditions(s.author, []supapi.SupplyConditionRef{dependent, independent}, time.Now())
	s.must(err)
	if resolved[dependent].Code != supapi.CodeSupplyConditionNotCurrent || resolved[dependent].Status != supapi.StatusActive ||
		resolved[independent].Code != "" || resolved[independent].Terms.ValidityMode != supapi.ValidityIndependent {
		t.Fatalf("after the withdrawal of the source offer: %+v", resolved)
	}
	s.refuseSeal(supapi.KindPurchaseTerms, s.terms("offer.1", supapi.ValidityDependent), supapi.CodeSupplyConditionNotCurrent, "/args/data/source_offer_ref")
	if _, err = s.save(s.author, supapi.KindPurchaseTerms, "terms.later", 0, s.terms("offer.1", supapi.ValidityIndependent)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.seal(s.reviewer, supapi.KindPurchaseTerms, "terms.later", 1, 1, 0); err != nil {
		t.Fatalf("independent terms from a withdrawn but verified offer revision: %v", err)
	}

	r, err := s.end(s.reviewer, independent.Kind, independent.ID, 2, 1)
	s.must(err)
	if r.Status != supapi.StatusRetired {
		t.Fatalf("retire %+v", r)
	}
	var types []string
	for _, e := range s.events("company.A", supapi.Source) {
		types = append(types, e.Type)
	}
	want := []string{supapi.EventSupplierOfferVerified, supapi.EventPurchaseTermsPublished, supapi.EventPurchaseTermsPublished,
		supapi.EventSupplierOfferWithdrawn, supapi.EventPurchaseTermsPublished, supapi.EventPurchaseTermsRetired}
	if len(types) != len(want) {
		t.Fatalf("events %v", types)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("events %v, want %v", types, want)
		}
	}
}

// Verification rules of a logistics offer: Python test_costing_domains.py
// test_service_cannot_reuse_goods_tax_application, test_duplicate_fee_obligation_cannot_be_hidden_by_new_id,
// test_zero_explicit_allocation_weights_are_rejected and test_weight_allocation_requires_measurement,
// with the service role's exact scope (T02 §13.1) and the measured charges CST cannot evaluate.
func TestLogisticsOfferVerificationRules(t *testing.T) {
	s := newSupWorld(t)
	ref := s.verified(supapi.KindLogisticsOffer, "log.1", s.logistics())
	fee := "/args/data/fees/0"
	for _, c := range []struct {
		name       string
		change     func(l *supapi.LogisticsOffer)
		code, path string
	}{
		{"service reusing the goods tax application", func(l *supapi.LogisticsOffer) { l.Fees[0].TaxApplicationRef = cref(s.goods) },
			kernel.CodeReferenceInvalid, fee + "/tax_application_ref"},
		{"same obligation under another ID", func(l *supapi.LogisticsOffer) {
			f := s.fee()
			f.ID = "different.name"
			l.Fees = append(l.Fees, f)
		}, supapi.CodeChargeDoubleCount, "/args/data/fees/1/obligation_id"},
		{"zero explicit weights", func(l *supapi.LogisticsOffer) {
			l.Fees[0].Allocation = &supapi.Allocation{Basis: supapi.AllocationExplicit, Weights: []supapi.ExplicitWeight{{LineID: "L1", Weight: "0"}}}
		}, supapi.CodeAllocationInvalid, fee + "/allocation/weights"},
		{"weight allocation without a measurement", func(l *supapi.LogisticsOffer) {
			l.Fees[0].Allocation = &supapi.Allocation{Basis: supapi.AllocationWeight, TargetLineIDs: []string{"L1"}}
		}, supapi.CodeCostInputNotReady, fee + "/allocation/measurement_ref"},
		{"measured charge without a measurement", func(l *supapi.LogisticsOffer) {
			l.Fees[0].Chargeable = &supapi.Chargeable{Kind: supapi.ChargeableActualWeight, BillingIncrement: "1", RoundingMode: supapi.RoundingCeil,
				RoundingScope: supapi.ScopeShipment}
			l.Fees[0].Billing = &supapi.Billing{Kind: supapi.BillingPerBasis, Rate: "2.5", BasisQuantity: "10", UnitRef: cref(s.kg), MinimumAmount: "30"}
		}, supapi.CodeCostInputNotReady, fee + "/chargeable/measurement_ref"},
		{"measured charge billed fixed", func(l *supapi.LogisticsOffer) {
			l.Fees[0].Chargeable = &supapi.Chargeable{Kind: supapi.ChargeableActualWeight, BillingIncrement: "1", RoundingMode: supapi.RoundingCeil,
				RoundingScope: supapi.ScopeShipment, MeasurementRef: &supapi.MeasurementRef{MeasurementID: "measurement.1", Revision: 1}}
		}, supapi.CodeUnsupportedPricingMode, fee + "/billing/kind"},
		{"another service", func(l *supapi.LogisticsOffer) { l.ServiceCode = "air.express" },
			kernel.CodeReferenceInvalid, "/args/data/service_role_ref"},
		{"another destination", func(l *supapi.LogisticsOffer) { l.Destination = "USLAX" },
			kernel.CodeReferenceInvalid, "/args/data/service_role_ref"},
		{"incomplete charge", func(l *supapi.LogisticsOffer) { l.Fees[0].Allocation = nil }, kernel.CodeDraftNotReady, fee + "/allocation"},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := s.logistics()
			l.Fees = []supapi.Fee{s.fee()}
			c.change(&l)
			s.refuseSeal(supapi.KindLogisticsOffer, l, c.code, c.path)
		})
	}

	// A measured charge with its measurement verifies: CST owns the measurement and checks it.
	measured := s.logistics()
	measured.Fees[0].Chargeable = &supapi.Chargeable{Kind: supapi.ChargeableMaxActualVolumetric, BillingIncrement: "0.5",
		RoundingMode: supapi.RoundingCeil, RoundingScope: supapi.ScopePerPackage, MaxScope: supapi.ScopePerPackage, DivisorM3PerKG: "0.005",
		MeasurementRef: &supapi.MeasurementRef{MeasurementID: "measurement.1", Revision: 1}}
	measured.Fees[0].Billing = &supapi.Billing{Kind: supapi.BillingTiered, PricingMode: supapi.PricingGraduated, BasisQuantity: "1",
		UnitRef: cref(s.kg), MinimumAmount: "0", Tiers: []supapi.Tier{{LowerInclusive: "0", UpperExclusive: "100", UnitPrice: "5"},
			{LowerInclusive: "100", UnitPrice: "4"}}}
	s.verified(supapi.KindLogisticsOffer, "log.measured", measured)

	// A service role verified again makes its earlier revision stale for new verifications.
	_, err := s.verifyRole(s.author, "provider.1", 3, "role.1", s.win.from, s.win.until, "evidence")
	s.must(err)
	s.refuseSeal(supapi.KindLogisticsOffer, s.logistics(), supapi.CodeReferenceStale, "/args/data/service_role_ref")

	r, err := s.end(s.reviewer, ref.Kind, ref.ID, 2, 1)
	s.must(err)
	events := s.events("company.A", supapi.Source)
	if r.Status != supapi.StatusWithdrawn || len(events) != 3 || events[0].Type != supapi.EventLogisticsOfferVerified ||
		events[2].Type != supapi.EventLogisticsOfferWithdrawn {
		t.Fatalf("withdrawal %+v, events %+v", r, events)
	}
}

// T02 §13.1, §13.8 and §17.1: a supplier RFQ records the human sending of a request for lines of a
// ready RFQ and seals it; an offer in response names it at its issued version and stays within it.
func TestSupplierRfqIssueAndAnsweringOffer(t *testing.T) {
	s := newSupWorld(t)
	o := s.offer()
	request := supapi.SupplierRfq{Supplier: o.Supplier, Rfq: o.Rfq, RfqLineIDs: []string{"L1"}}
	_, err := s.c.SUP.SaveSupplierRfq(s.author, supapi.SaveSupplierRfq{IdempotencyKey: s.key(), SupplierRfqID: "request.1", Data: supapi.SupplierRfq{Supplier: o.Supplier}})
	s.must(err)
	issue := func(ctx context.Context, id string, expected int64) (supapi.Receipt, error) {
		return s.c.SUP.RecordSupplierRfqIssued(ctx, supapi.RecordSupplierRfqIssued{IdempotencyKey: s.key(), SupplierRfqID: id, ExpectedVersion: expected,
			Evidence: evidenceOf("发送记录"), Reason: "人工发送"})
	}
	if _, err = issue(s.reviewer, "request.1", 1); code(err) != kernel.CodeDraftNotReady || path(err) != "/args/data/rfq" {
		t.Fatalf("issuing an incomplete request: %s at %s", code(err), path(err))
	}
	_, err = s.c.SUP.SaveSupplierRfq(s.author, supapi.SaveSupplierRfq{IdempotencyKey: s.key(), SupplierRfqID: "request.1", ExpectedVersion: 1, Data: request})
	s.must(err)
	ai := s.other("agent", kernel.ActorAI, allPermissions...)
	if _, err = issue(ai, "request.1", 2); code(err) != kernel.CodePermissionDenied {
		t.Fatalf("an AI agent records the sending: %s", code(err))
	}
	issued, err := issue(s.author, "request.1", 2)
	s.must(err)
	if issued.RowVersion != 3 || issued.Status != supapi.StatusIssued {
		t.Fatalf("issued %+v", issued)
	}
	if _, err = s.c.SUP.SaveSupplierRfq(s.author, supapi.SaveSupplierRfq{IdempotencyKey: s.key(), SupplierRfqID: "request.1", ExpectedVersion: 3,
		Data: request}); code(err) != kernel.CodeInvalidState {
		t.Fatalf("editing an issued request: %s", code(err))
	}
	if got := sqlCode(s.exec("company.A", `UPDATE brant_sup.supplier_rfq SET data = '{}', row_version = row_version + 1
		WHERE supplier_rfq_id = 'request.1'`)); got != "23000" {
		t.Fatalf("the database edits an issued request: %s", got)
	}

	answer := func(version int64) supapi.SupplierOffer {
		a := s.offer()
		a.Origin = &supapi.Origin{Kind: supapi.OriginInResponseToRfq, SupplierRfq: &supapi.SupplierRfqRef{SupplierRfqID: "request.1", RowVersion: version}}
		return a
	}
	s.refuseSeal(supapi.KindSupplierOffer, answer(2), supapi.CodeReferenceStale, "/args/data/origin/supplier_rfq")
	s.verified(supapi.KindSupplierOffer, "offer.answer", answer(3))
	if _, err = s.save(s.author, supapi.KindSupplierOffer, "offer.nowhere", 0, func() supapi.SupplierOffer {
		a := answer(3)
		a.Origin.SupplierRfq.SupplierRfqID = "request.none"
		return a
	}()); code(err) != kernel.CodeReferenceInvalid {
		t.Fatalf("an answer to no request: %s", code(err))
	}
	if _, err = s.save(s.author, supapi.KindSupplierOffer, "offer.invented", 0, func() supapi.SupplierOffer {
		a := s.offer()
		a.Origin.SupplierRfq = &supapi.SupplierRfqRef{SupplierRfqID: "request.1", RowVersion: 3}
		return a
	}()); code(err) != kernel.CodeSchemaInvalid {
		t.Fatalf("an unsolicited offer naming a request: %s", code(err))
	}
	closed, err := s.c.SUP.CloseSupplierRfq(s.reviewer, supapi.CloseSupplierRfq{IdempotencyKey: s.key(), SupplierRfqID: "request.1", ExpectedVersion: 3,
		Evidence: evidenceOf("关闭记录"), Reason: "报价已收到"})
	s.must(err)
	// Closing does not undo the sending: an offer may still answer the issued version.
	s.verified(supapi.KindSupplierOffer, "offer.late", answer(3))
	events := s.events("company.A", supapi.Source)
	if closed.Status != supapi.StatusClosed || events[0].Type != supapi.EventSupplierRfqIssued || events[0].Payload["issued_version"] != float64(3) ||
		events[0].Payload["rfq_id"] != "rfq.1" {
		t.Fatalf("closed %+v, events %+v", closed, events)
	}
}

// Evidence a verification or a document cites must be readable by the actor (Python _source_ref:
// SourceRef v1 under R53); an actor without the EVD read permission cannot verify on evidence.
func TestSupplyConditionEvidenceMustBeReadable(t *testing.T) {
	s := newSupWorld(t)
	_, err := s.save(s.author, supapi.KindSupplierOffer, "offer.1", 0, s.offer())
	s.must(err)
	if _, err = s.c.SUP.Seal(s.reviewer, supapi.Seal{IdempotencyKey: s.key(), Kind: supapi.KindSupplierOffer, ID: "offer.1", ExpectedVersion: 1,
		WorkingRevision: 1, Evidence: []supapi.Evidence{{SourceID: "missing.source", Locator: "p.1"}}}); code(err) != kernel.CodeReferenceInvalid ||
		path(err) != "/args/evidence/0/source_id" {
		t.Fatalf("verification on unreadable evidence: %s at %s", code(err), path(err))
	}
	unread := s.offer()
	unread.Evidence = []supapi.Evidence{{SourceID: "missing.source", Locator: "p.1"}}
	if _, err = s.save(s.author, supapi.KindSupplierOffer, "offer.2", 0, unread); code(err) != kernel.CodeReferenceInvalid ||
		path(err) != "/args/data/evidence/0/source_id" {
		t.Fatalf("a document citing an unreadable source: %s at %s", code(err), path(err))
	}
	var withoutEVD []string
	for _, p := range allPermissions {
		if p != "evd.read" {
			withoutEVD = append(withoutEVD, p)
		}
	}
	blind := s.other("blind", kernel.ActorHuman, withoutEVD...)
	if _, err = s.seal(blind, supapi.KindSupplierOffer, "offer.1", 1, 1, 0); code(err) != kernel.CodePermissionDenied {
		t.Fatalf("verification without the EVD read permission: %s", code(err))
	}
}

// SupplyConditionRef v1 lookups (R09, R20): sealed revisions the caller sees, with their content and
// the guard's verdict at the instant; drafts, other companies and records outside the data scope
// are absent; reads are audited and confined to the company (FORCE RLS).
func TestResolveSupplyConditionsAndScope(t *testing.T) {
	s := newSupWorld(t)
	ref := s.verified(supapi.KindSupplierOffer, "offer.1", s.offer())
	_, err := s.save(s.author, supapi.KindSupplierOffer, "offer.draft", 0, s.offer())
	s.must(err)
	draft := supapi.SupplyConditionRef{Kind: supapi.KindSupplierOffer, ID: "offer.draft", Revision: 1}
	resolved, err := s.c.SUP.ResolveSupplyConditions(s.author, []supapi.SupplyConditionRef{ref, draft}, time.Now())
	s.must(err)
	got, ok := resolved[ref]
	if _, present := resolved[draft]; present || !ok || got.Code != "" || got.Offer == nil || got.Offer.Lines[0].Tiers[0].UnitPrice != "5" ||
		got.Verification.By.ID != "reviewer" || !got.ValidUntil.Equal(s.win.until) || got.Check("/x") != nil {
		t.Fatalf("resolved %+v", resolved)
	}
	at, err := s.c.SUP.ResolveSupplyConditions(s.author, []supapi.SupplyConditionRef{ref}, s.win.until)
	s.must(err)
	if at[ref].Code != supapi.CodeSupplyConditionExpired || code(at[ref].Check("/x")) != supapi.CodeSupplyConditionExpired {
		t.Fatalf("at the end of the validity %+v", at[ref])
	}
	own := s.provision(bootstrapGrant(s, "own.reader", "own", "sup.read"))
	if mine, err := s.c.SUP.ResolveSupplyConditions(own, []supapi.SupplyConditionRef{ref}, time.Now()); err != nil || len(mine) != 0 {
		t.Fatalf("a reader of its own records: %+v %v", mine, err)
	}
	if _, err = s.c.SUP.GetCondition(own, ref.Kind, ref.ID); code(err) != kernel.CodeNotFound {
		t.Fatalf("a reader of its own records reads another's offer: %s", code(err))
	}
	outsider := s.provision(bootstrapGrantIn(s, "company.B", "outsider", "company", allPermissions...))
	if theirs, err := s.c.SUP.ResolveSupplyConditions(outsider, []supapi.SupplyConditionRef{ref}, time.Now()); err != nil || len(theirs) != 0 {
		t.Fatalf("another company: %+v %v", theirs, err)
	}
	if _, err = s.c.SUP.GetCondition(outsider, ref.Kind, ref.ID); code(err) != kernel.CodeNotFound {
		t.Fatalf("another company reads the offer: %s", code(err))
	}
	if n := s.count("company.B", "brant_sup.condition_revision"); n != 0 {
		t.Fatalf("company.B sees %d revisions", n)
	}
	if _, err = s.c.SUP.ResolveSupplyConditions(s.author, []supapi.SupplyConditionRef{{Kind: "offer", ID: "offer.1", Revision: 1}}, time.Now()); code(err) != kernel.CodeSchemaInvalid {
		t.Fatalf("a reference of no kind: %s", code(err))
	}
	s.condition(ref.Kind, ref.ID)
	audit, err := s.c.SUP.Audit(s.author)
	s.must(err)
	reads := 0
	for _, e := range audit {
		if e.Command == "supplier_offer.get" && e.Outcome == "read" {
			reads++
		}
	}
	if reads == 0 {
		t.Fatalf("no audited reads in %+v", audit)
	}
}
