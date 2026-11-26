//go:build integration

package bootstrap_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/bootstrap"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// config.list (S7-06 read models): a page of the configurations of one kind, most recently
// changed first, each with its latest revision's status and its active revision, never a body.
// bank_account is refused; a refused query is audited.
func TestConfigListReturnsOneKindWithoutBodies(t *testing.T) {
	w := newWorld(t)
	editor, reviewer := w.polActors()
	win := newWindow()
	w.currency(editor, reviewer, "USD", win)
	w.currency(editor, reviewer, "EUR", win)
	w.configure(editor, reviewer, polapi.KindCurrency, "CNY", 0, win.from, time.Time{}, currencyData("CNY", 2, "CNY"), false) // a draft, never published
	w.tax(editor, reviewer, polapi.RecoveryNonrecoverable, "tax.a", win)
	inB := func(id string) context.Context {
		return w.provision(bootstrap.Grant{Scope: w.scope("company.B"), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: id},
			Permissions: allPermissions, DataScope: "company"})
	}
	outside := inB("pol.outside")
	w.source(outside, "evidence", "另一家公司的依据")
	w.currency(outside, inB("pol.outside.reviewer"), "JPY", win)

	list := func(ctx context.Context, q polapi.ConfigQuery) []string {
		t.Helper()
		p, err := w.c.POL.ListConfigs(ctx, q)
		w.must(err)
		ids := []string{}
		for _, c := range p.Configs {
			ids = append(ids, c.ConfigID)
		}
		return ids
	}
	if got := list(editor, polapi.ConfigQuery{Kind: polapi.KindCurrency}); !slices.Equal(got, []string{"CNY", "EUR", "USD"}) {
		t.Fatalf("currencies %v", got)
	}
	if got := list(editor, polapi.ConfigQuery{Kind: polapi.KindCurrency, Active: true}); !slices.Equal(got, []string{"EUR", "USD"}) {
		t.Fatalf("active currencies %v", got)
	}
	if got := list(editor, polapi.ConfigQuery{Kind: polapi.KindCurrency, Search: "us"}); !slices.Equal(got, []string{"USD"}) {
		t.Fatalf("search %v", got)
	}
	if got := list(editor, polapi.ConfigQuery{Kind: polapi.KindTaxProfile, Search: "%"}); len(got) != 0 {
		t.Fatalf("a percent sign matched %v", got)
	}
	if got := list(editor, polapi.ConfigQuery{Kind: polapi.KindUnit}); len(got) != 0 {
		t.Fatalf("units %v", got)
	}
	if got := list(outside, polapi.ConfigQuery{Kind: polapi.KindCurrency}); !slices.Equal(got, []string{"JPY"}) {
		t.Fatalf("another company lists %v", got)
	}

	page, err := w.c.POL.ListConfigs(editor, polapi.ConfigQuery{Kind: polapi.KindCurrency, Limit: 2})
	w.must(err)
	draft, eur := page.Configs[0], page.Configs[1]
	if page.Kind != polapi.KindCurrency || page.Next == "" || draft.Status != polapi.StatusWorking || draft.ActiveRevision != 0 || !draft.ValidFrom.IsZero() ||
		eur.Status != polapi.StatusActive || eur.ActiveRevision != 1 || eur.LastRevision != 1 || eur.RootVersion != 2 || !eur.ValidFrom.Equal(win.from) || !eur.ValidUntil.IsZero() {
		t.Fatalf("page %+v", page)
	}
	if got := list(editor, polapi.ConfigQuery{Kind: polapi.KindCurrency, Limit: 2, After: page.Next}); !slices.Equal(got, []string{"USD"}) {
		t.Fatalf("second page %v", got)
	}
	tax, err := w.c.POL.ListConfigs(editor, polapi.ConfigQuery{Kind: polapi.KindTaxProfile})
	w.must(err)
	if c := tax.Configs[0]; len(tax.Configs) != 1 || !c.ValidUntil.Equal(win.until) {
		t.Fatalf("tax profiles %+v", tax)
	}

	// Refusals.
	_, err = w.c.POL.ListConfigs(editor, polapi.ConfigQuery{Kind: polapi.KindBankAccount})
	w.want(err, kernel.CodePermissionDenied)
	for _, bad := range []polapi.ConfigQuery{{Kind: "colour"}, {}, {Kind: polapi.KindCurrency, Search: " "}, {Kind: polapi.KindCurrency, After: "x"},
		{Kind: polapi.KindCurrency, Limit: kernel.MaxListPage + 1}} {
		_, err = w.c.POL.ListConfigs(editor, bad)
		w.want(err, kernel.CodeSchemaInvalid)
	}
	_, err = w.c.POL.ListConfigs(w.actor("pol.none"), polapi.ConfigQuery{Kind: polapi.KindCurrency})
	w.want(err, kernel.CodePermissionDenied)
	trail, err := w.c.POL.Audit(editor)
	w.must(err)
	refused := 0
	for _, e := range trail {
		if e.Command == "config.list" && e.Outcome != "" && (e.Code == kernel.CodeSchemaInvalid || e.Code == kernel.CodePermissionDenied) {
			refused++
		}
	}
	if refused < 6 {
		t.Fatalf("%d refused config.list entries in the audit trail", refused)
	}
}
