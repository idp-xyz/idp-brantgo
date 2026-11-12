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
