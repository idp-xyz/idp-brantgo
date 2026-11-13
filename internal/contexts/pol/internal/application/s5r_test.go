package application

import (
	"reflect"
	"slices"
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/pol/internal/domain/config"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
)

// S5R-D06, D38: a save that uses none of the new members has the request hash it had before them,
// so a stored command replays after the upgrade instead of conflicting. The hashes were computed by
// the code of 90925f3 (the S5R base, before R2) and are pinned here; SaveDraft hashes the command
// without its idempotency key and expected version.
func TestS5RSaveRequestsKeepTheirHash(t *testing.T) {
	cal := &api.ConfigRef{Kind: "calendar", ConfigID: "cal", Revision: 2}
	trade := api.SaveDraft{IdempotencyKey: "k", Kind: api.KindTradeTerms, ConfigID: "trade.cif", ExpectedVersion: 3, ValidFrom: "2026-11-13T00:00:00Z",
		Data: api.ConfigData{TradeTerms: &api.TradeTerms{Incoterm: "CIF", Edition: "2020", NamedPlace: "DEHAM", ResponsibilityNote: "synthetic CIF",
			Delivery: api.Delivery{Kind: "lead_time", AnchorEvent: "order_confirmed", Days: 30, DayBasis: "working", Calendar: cal}}}}
	screening := api.SaveDraft{IdempotencyKey: "k", Kind: api.KindScreeningRegulation, ConfigID: "screening", ValidFrom: "2026-11-13T00:00:00Z",
		Data: api.ConfigData{ScreeningRegulation: &api.ScreeningRegulation{Lists: []api.ScreeningList{{ListCode: "cn.uel", Jurisdiction: "CN"},
			{ListCode: "us.sdn", Jurisdiction: "foreign", LegalConfirmationEvidenceID: "legal.1"}}, RescreenIntervalDays: 30}}}
	tax := api.SaveDraft{IdempotencyKey: "k", Kind: api.KindExportTaxPolicy, ConfigID: "export.tax", ValidFrom: "2026-11-13T00:00:00Z",
		Data: api.ConfigData{ExportTaxPolicy: &api.ExportTaxPolicy{Name: "synthetic export tax policy", RefundMethod: "exempt_refund",
			PolicyBasis: "synthetic policy basis", ProfessionalReviewEvidenceID: "tax.review"}}}
	hash := func(cmd api.SaveDraft) string {
		args := cmd
		args.IdempotencyKey, args.ExpectedVersion = "", 0
		h, err := action.RequestHash(args)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	for _, c := range []struct {
		cmd  api.SaveDraft
		want string
	}{
		{trade, "a875b4370b401b5d94ec4f743a94426d83fcbd8725c0846e2be6f2f760651322"},
		{screening, "2e55aa7cc48a0890fb144132434f998cfa5e4526d8f547744f93db185dd53a3d"},
		{tax, "182957999ec74e64117f71a1775c34fd6fd6abe41070800c9ee8a772824f4c8d"},
	} {
		if got := hash(c.cmd); got != c.want {
			t.Errorf("%s: %s, want %s", c.cmd.Kind, got, c.want)
		}
	}
	// A stated new member is a different request.
	trade.Data.TradeTerms.InsurancePercent = "110"
	screening.Data.ScreeningRegulation.Programmes = []api.ScreeningList{{ListCode: "cn.country_programmes", Jurisdiction: "CN"}}
	if hash(trade) == "a875b4370b401b5d94ec4f743a94426d83fcbd8725c0846e2be6f2f760651322" ||
		hash(screening) == "2e55aa7cc48a0890fb144132434f998cfa5e4526d8f547744f93db185dd53a3d" {
		t.Fatal("a stated insurance percentage or programme list changes the request")
	}
}

// The S5R members survive the trip into domain content and back, and none is set when the request
// states none (S5R-D06, D38).
func TestS5RMembersConvertBothWays(t *testing.T) {
	for name, d := range map[string]api.ConfigData{
		"trade terms 120 %": {TradeTerms: &api.TradeTerms{Incoterm: "CIP", Edition: "2020", NamedPlace: "n", ResponsibilityNote: "r",
			Delivery: api.Delivery{Kind: "fixed_date", Date: "2026-12-01"}, InsurancePercent: "120"}},
		"programme lists": {ScreeningRegulation: &api.ScreeningRegulation{Lists: []api.ScreeningList{{ListCode: "cn.uel", Jurisdiction: "CN"}},
			RescreenIntervalDays: 30, Programmes: []api.ScreeningList{{ListCode: "cn.country_programmes", Jurisdiction: "CN"},
				{ListCode: "eu.p", Jurisdiction: "foreign", LegalConfirmationEvidenceID: "legal.eu"}}}},
		"taxable": {ExportTaxPolicy: &api.ExportTaxPolicy{Name: "n", RefundMethod: api.RefundTaxable, PolicyBasis: "p",
			ProfessionalReviewEvidenceID: "review"}},
	} {
		kind := api.KindTradeTerms
		switch {
		case d.ScreeningRegulation != nil:
			kind = api.KindScreeningRegulation
		case d.ExportTaxPolicy != nil:
			kind = api.KindExportTaxPolicy
		}
		c := content(api.SaveDraft{Kind: kind, ValidFrom: "2026-11-13T00:00:00Z", Data: d})
		if back := data(c); !reflect.DeepEqual(back, d) {
			t.Errorf("%s: %+v back as %+v", name, d, back)
		}
	}
	none := content(api.SaveDraft{Kind: api.KindScreeningRegulation, ValidFrom: "2026-11-13T00:00:00Z",
		Data: api.ConfigData{ScreeningRegulation: &api.ScreeningRegulation{Lists: []api.ScreeningList{{ListCode: "cn.uel", Jurisdiction: "CN"}},
			RescreenIntervalDays: 30, Programmes: []api.ScreeningList{}}}})
	if none.ScreeningRegulation.Programmes != nil || data(none).ScreeningRegulation.Programmes != nil {
		t.Fatal("no programme lists are none")
	}
}

// S5R-D30: the capabilities POL requires are the role templates' (roles_gen.go): each specialist
// kind's capability is held by its specialist template alone, config.retire (the general kinds) by
// administrator alone, and every template that holds config.publish publishes some kind with it.
// Approval tier levels are the templates that decide credit.
func TestS5RCapabilitiesAreTheRoleTemplates(t *testing.T) {
	holders := func(permission string) []string {
		var out []string
		for _, tpl := range authorization.Templates() {
			if slices.Contains(tpl.Permissions, permission) {
				out = append(out, tpl.Code)
			}
		}
		return out
	}
	for capability, want := range map[string][]string{
		api.PermissionPublishCreditPolicy:        {"credit_manager"},
		api.PermissionPublishExportTaxPolicy:     {"tax_specialist"},
		api.PermissionPublishScreeningRegulation: {"compliance"},
		api.PermissionAdministerConfigs:          {"administrator"},
	} {
		if got := holders(capability); !slices.Equal(got, want) {
			t.Errorf("%s is held by %v, want %v", capability, got, want)
		}
	}
	for _, tpl := range authorization.Templates() {
		if !slices.Contains(tpl.Permissions, api.CommandPublish) {
			continue
		}
		var publishes []string
		for _, kind := range []string{api.KindCurrency, api.KindCreditPolicy, api.KindExportTaxPolicy, api.KindScreeningRegulation} {
			if slices.Contains(tpl.Permissions, api.PublishCapability(kind)) {
				publishes = append(publishes, kind)
			}
		}
		if len(publishes) == 0 || (tpl.Code != "administrator" && len(publishes) != 1) {
			t.Errorf("%s publishes %v", tpl.Code, publishes)
		}
	}
	if got := holders("credit_exception.decide"); !slices.Equal(got, config.CreditApprovalLevels) {
		t.Fatalf("credit decisions are taken by %v, approval levels are %v", got, config.CreditApprovalLevels)
	}
}
