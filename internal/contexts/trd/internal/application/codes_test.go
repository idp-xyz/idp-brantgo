package application

import (
	"slices"
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/acceptance"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/amendment"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/calc"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/document"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/order"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/orderdoc"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/orderrev"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/response"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The codes the domain packages decide are the codes package api declares, with the same
// categories, so a refusal reaches transports with the catalog's status.
func TestDomainCodesAreDeclaredByTheAPI(t *testing.T) {
	for name, m := range map[string]map[string]kernel.Category{"quote": quote.Categories, "calc": calc.Categories, "document": document.Categories,
		"response": response.Categories, "acceptance": acceptance.Categories, "order": order.Categories, "orderrev": orderrev.Categories,
		"amendment": amendment.Categories, "orderdoc": orderdoc.Categories} {
		for code, category := range m {
			if got, ok := api.Category(code); !ok || got != category {
				t.Errorf("%s code %s: api says %v (declared %v), domain %v", name, code, got, ok, category)
			}
		}
	}
}

// The tactical model's humanOnlyCommands that QuoteRoot implements are human only; the rest admit
// what the command catalog says, and every command has a rule.
func TestRulesFollowTheCatalog(t *testing.T) {
	policy, err := authorization.NewPolicy(Rules()...)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]kernel.ActorKind{
		api.CommandSaveDraft:      {kernel.ActorHuman, kernel.ActorAI},
		api.CommandRevise:         {kernel.ActorHuman, kernel.ActorAI},
		api.CommandDiscardDraft:   {kernel.ActorHuman, kernel.ActorAI},
		api.CommandFreeze:         {kernel.ActorHuman},
		api.CommandUpdateMetadata: {kernel.ActorHuman, kernel.ActorAI},
		api.CommandBindControl:    {kernel.ActorHuman, kernel.ActorSystem},
		api.CommandStopControl:    {kernel.ActorHuman},
		api.CommandReleaseControl: {kernel.ActorHuman},
		api.CommandSubmitReview:   {kernel.ActorHuman},
		api.CommandDecideReview:   {kernel.ActorHuman},
		api.CommandRevokeApproval: {kernel.ActorHuman},
		api.CommandActivate:       {kernel.ActorHuman},
		api.CommandReplace:        {kernel.ActorHuman},
		api.CommandWithdraw:       {kernel.ActorHuman},
		api.CommandCancel:         {kernel.ActorHuman},
		CommandGetQuote:           {kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem},
		CommandCheckApproval:      {kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem},
		CommandGetFormalDocument:  {kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem},
		CommandGetActiveRevision:  {kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem},
		// Customer responses and acceptance: the command catalog's 人工或 AI for the record, the
		// tactical model's humanOnlyCommands for the rest.
		api.CommandRecordResponse:       {kernel.ActorHuman, kernel.ActorAI},
		api.CommandVerifyResponse:       {kernel.ActorHuman},
		api.CommandInvalidateAcceptance: {kernel.ActorHuman},
		api.CommandDisputeAcceptance:    {kernel.ActorHuman},
		CommandGetResponse:              {kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem},
		CommandGetAcceptance:            {kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem},
		// Sales orders: the command catalog's 人工或 AI（按动作权限） for the preparation and the review
		// request, the tactical model's humanOnlyCommands for the rest.
		api.CommandPrepareOrder:          {kernel.ActorHuman, kernel.ActorAI},
		api.CommandRequestOrderReview:    {kernel.ActorHuman, kernel.ActorAI},
		api.CommandDecideOrderReview:     {kernel.ActorHuman},
		api.CommandActivateOrder:         {kernel.ActorHuman},
		api.CommandRevokeOrderApproval:   {kernel.ActorHuman},
		api.CommandCancelOrder:           {kernel.ActorHuman},
		api.CommandCloseOrderUnfulfilled: {kernel.ActorHuman},
		// Active orders: the command catalog's 人工或 AI（同一守卫） for the amendment's preparation.
		api.CommandPrepareAmendment:    {kernel.ActorHuman, kernel.ActorAI},
		api.CommandSubmitAmendment:     {kernel.ActorHuman},
		api.CommandDecideAmendment:     {kernel.ActorHuman},
		api.CommandApplyAmendment:      {kernel.ActorHuman},
		api.CommandWithdrawAmendment:   {kernel.ActorHuman},
		api.CommandCloseOrderRemainder: {kernel.ActorHuman},
		api.CommandCloseOrder:          {kernel.ActorHuman},
		CommandGetOrder:                {kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem},
		CommandCheckOrder:              {kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem},
		// Order documents: the command catalog's 人工或 AI（按动作权限） and 仅人工.
		api.CommandGenerateOrderDocument:     {kernel.ActorHuman, kernel.ActorAI},
		api.CommandRecordSignedOrderDocument: {kernel.ActorHuman},
		CommandGetOrderDocument:              {kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem},
	}
	for command, kinds := range want {
		r, err := policy.Rule(command)
		if err != nil {
			t.Fatalf("%s: %v", command, err)
		}
		for _, k := range []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem} {
			admitted := false
			for _, w := range kinds {
				admitted = admitted || w == k
			}
			if r.Admits(k) != admitted {
				t.Errorf("%s admits %s: %v", command, k, r.Admits(k))
			}
		}
		reads := []string{CommandGetQuote, CommandCheckApproval, CommandGetFormalDocument, CommandGetActiveRevision, CommandGetResponse,
			CommandGetAcceptance, CommandGetOrder, CommandCheckOrder, CommandGetOrderDocument}
		if !slices.Contains(reads, command) && r.Permission != command {
			t.Errorf("%s needs permission %s", command, r.Permission)
		}
	}
	for _, read := range []string{CommandCheckApproval, CommandGetFormalDocument, CommandGetActiveRevision, CommandGetResponse,
		CommandGetAcceptance, CommandGetOrder, CommandCheckOrder, CommandGetOrderDocument} {
		if r, _ := policy.Rule(read); r.Permission != PermissionRead {
			t.Errorf("%s needs %s", read, r.Permission)
		}
	}
	// The domain's permission names are the api's command and read names.
	if quote.PermissionReview != api.CommandDecideReview || quote.PermissionMarginException != api.PermissionMarginException ||
		quote.PermissionRead != PermissionRead || order.PermissionDecide != api.CommandDecideOrderReview || order.PermissionRead != PermissionRead ||
		amendment.PermissionDecide != api.CommandDecideAmendment || amendment.PermissionRead != PermissionRead {
		t.Error("permission names differ")
	}
	// The domain's command names are the api's.
	if amendment.CommandPrepare != api.CommandPrepareAmendment || amendment.CommandSubmit != api.CommandSubmitAmendment ||
		amendment.CommandDecide != api.CommandDecideAmendment || amendment.CommandApply != api.CommandApplyAmendment ||
		amendment.CommandWithdraw != api.CommandWithdrawAmendment || orderrev.CommandCancel != api.CommandCancelOrder ||
		orderrev.CommandCloseRemainder != api.CommandCloseOrderRemainder || orderrev.CommandClose != api.CommandCloseOrder ||
		orderrev.NotWired != api.NotWired || amendment.Contract == "" {
		t.Error("command names differ")
	}
	if !slices.Equal(amendment.Areas(), api.ImpactAreas()) {
		t.Error("impact areas differ")
	}
	// The order documents' kinds and signed copies are the api's.
	if orderdoc.KindSalesContract != api.DocumentSalesContract || orderdoc.KindProformaInvoice != api.DocumentProformaInvoice ||
		orderdoc.Signed != api.SignatureSigned || orderdoc.Countersigned != api.SignatureCountersigned {
		t.Error("document kinds differ")
	}
}

// The idempotency key of POL's number.issue is a valid identifier, fixed by the formalization
// command and its key, and differs between commands and keys.
func TestNumberKeyIsAnIdentifierPerCommandAndKey(t *testing.T) {
	a, b, c := numberKey(api.CommandActivate, "k1"), numberKey(api.CommandReplace, "k1"), numberKey(api.CommandActivate, "k2")
	if !kernel.ValidIdentifier(a) || a != numberKey(api.CommandActivate, "k1") || a == b || a == c || len(a) > 64 {
		t.Fatalf("%s %s %s", a, b, c)
	}
	if d := numberKey(api.CommandGenerateOrderDocument, "k1"); !kernel.ValidIdentifier(d) || d == a || d == b {
		t.Fatalf("order document: %s", d)
	}
}
