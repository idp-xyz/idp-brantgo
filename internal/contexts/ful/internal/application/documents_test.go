package application

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/ful/api"
	docs "go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipmentdocumentset"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// humanOnlyCommands (D-17) name shipment_document.issue and shipment_document.record_signed: an AI
// or system actor is refused. booking.record and shipment_document.prepare are human or AI under
// their own permission (role templates merchandiser and documentation); the reads need ful.read.
func TestDocumentRules(t *testing.T) {
	policy, err := authorization.NewPolicy(Rules()...)
	if err != nil {
		t.Fatal(err)
	}
	scope := kernel.Scope{Tenant: "tenant.1", Company: "company.A"}
	admit := func(command string, kind kernel.ActorKind) bool {
		_, err := policy.Admit(identity.Principal{Actor: kernel.ActorRef{Kind: kind, ID: "actor.1"}, Scope: scope}, command)
		return err == nil
	}
	for command, ai := range map[string]bool{api.CommandRecordBooking: true, api.CommandPrepareShipmentDocument: true,
		api.CommandIssueShipmentDocuments: false, api.CommandRecordSignedShipmentDocument: false} {
		if !admit(command, kernel.ActorHuman) || admit(command, kernel.ActorAI) != ai || admit(command, kernel.ActorSystem) {
			t.Errorf("%s", command)
		}
		if r, err := policy.Rule(command); err != nil || r.Permission != command {
			t.Errorf("%s: %+v %v", command, r, err)
		}
	}
	for _, command := range []string{api.CommandGetBookings, api.CommandGetDocumentSet, api.CommandGetShipmentDocument} {
		if r, err := policy.Rule(command); err != nil || r.Permission != api.PermissionRead {
			t.Errorf("%s: %+v %v", command, r, err)
		}
	}
}

// The request hash covers the business arguments only; the document rules' codes are FUL's
// declared codes with the same categories; POL's idempotency key for the number fits an identifier.
func TestDocumentRequests(t *testing.T) {
	a := api.IssueShipmentDocuments{IdempotencyKey: "k1", DocumentSetID: "docs.1", ExpectedVersion: 2}
	b := a
	b.IdempotencyKey, b.ExpectedVersion = "k2", 7
	if requestHash(a) != requestHash(b) {
		t.Fatal("neither the key nor the expected version is part of the request")
	}
	b.Acknowledgements = []api.Acknowledgement{{Path: "/x", Reason: "y"}}
	if requestHash(a) == requestHash(b) {
		t.Fatal("the acknowledgements are")
	}
	for _, cmd := range []api.Command{api.RecordBooking{}, api.PrepareShipmentDocument{}, api.IssueShipmentDocuments{},
		api.RecordSignedShipmentDocument{}} {
		if name, _, _ := request(cmd); name == "" {
			t.Errorf("%T", cmd)
		}
	}
	for code, category := range docs.Codes {
		if c, ok := api.Category(code); !ok || c != category {
			t.Errorf("%s: %v %v", code, c, ok)
		}
	}
	if k := numberKey(api.CommandPrepareShipmentDocument, "k1"); len(k) > 64 || !kernel.ValidIdentifier(k) ||
		k == numberKey(api.CommandPrepareShipmentDocument, "k2") {
		t.Fatal(k)
	}
	if (DocumentUpstream{}).Complete() {
		t.Fatal("an empty document upstream is incomplete")
	}
}

// S5-16: insurance.record is human or AI under its own permission (role template documentation;
// not in humanOnlyCommands); insurance.get needs ful.read; the request hash covers the business
// arguments only; the coverage code is FUL's declared one.
func TestInsuranceRules(t *testing.T) {
	policy, err := authorization.NewPolicy(Rules()...)
	if err != nil {
		t.Fatal(err)
	}
	scope := kernel.Scope{Tenant: "tenant.1", Company: "company.A"}
	for kind, want := range map[kernel.ActorKind]bool{kernel.ActorHuman: true, kernel.ActorAI: true, kernel.ActorSystem: false} {
		_, err := policy.Admit(identity.Principal{Actor: kernel.ActorRef{Kind: kind, ID: "actor.1"}, Scope: scope}, api.CommandRecordInsurance)
		if (err == nil) != want {
			t.Errorf("%s: %v", kind, err)
		}
	}
	if r, err := policy.Rule(api.CommandRecordInsurance); err != nil || r.Permission != api.CommandRecordInsurance {
		t.Fatalf("%+v %v", r, err)
	}
	if r, err := policy.Rule(api.CommandGetInsurance); err != nil || r.Permission != api.PermissionRead {
		t.Fatalf("%+v %v", r, err)
	}
	a := api.RecordInsurance{IdempotencyKey: "k1", DocumentSetID: "docs.1", ExpectedVersion: 2, InsuredAmount: "8602.00"}
	b := a
	b.IdempotencyKey, b.ExpectedVersion = "k2", 5
	if name, _, _ := request(a); name != api.CommandRecordInsurance || requestHash(a) != requestHash(b) {
		t.Fatal("neither the key nor the expected version is part of the request")
	}
	b.InsuredAmount = "8601.99"
	if requestHash(a) == requestHash(b) {
		t.Fatal("the insured amount is")
	}
	if c, ok := api.Category(docs.CodeInsuranceCoverageInsufficient); !ok || c != kernel.CategoryInvalid ||
		docs.Codes[docs.CodeInsuranceCoverageInsufficient] != c {
		t.Fatal("INSURANCE_COVERAGE_INSUFFICIENT is 422 in both")
	}
	if docs.KindInsurancePolicy != api.DocumentInsurancePolicy || docs.ServiceInsurer != api.ServiceInsurer ||
		docs.DefaultCoveragePercent != api.DefaultCoveragePercent || docs.PremiumEqual != api.PremiumEqual ||
		docs.PremiumDifferent != api.PremiumDifferent || docs.PremiumNotComparable != api.PremiumNotComparable ||
		docs.PremiumNoComponent != api.PremiumNoComponent || docs.PremiumUnavailable != api.PremiumUnavailable {
		t.Fatal("the domain and the contract name the same kinds and outcomes")
	}
}

// The views print amounts in their currency's decimals; what is not an amount stays as it is.
func TestInsuranceAmounts(t *testing.T) {
	if amount("USD", 2, "860200") != "8602.00" || amount("JPY", 0, "1106") != "1106" || amount("USD", 2, "-150") != "-1.50" ||
		amount("usd", 2, "1") != "1" || amount("USD", 2, "x") != "x" {
		t.Fatal("amounts")
	}
	v := policyComparisonView(&docs.InsurancePolicy{Currency: docs.CurrencyContent{Code: "USD", MinorUnits: 2}, Costing: docs.PremiumComparison{
		Status: docs.PremiumDifferent, Currency: "USD", MinorUnits: 2, ComponentMinor: "1200", Components: []string{"fee.2"}, PremiumCurrency: "USD",
		PremiumMinor: "1500", DifferenceMinor: "300"}})
	if v.Premium != "15.00" || v.ComponentAmount != "12.00" || v.Difference != "3.00" || v.Status != api.PremiumDifferent {
		t.Fatalf("%+v", v)
	}
}
