package application

import (
	"context"
	"slices"
	"testing"
	"time"

	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The read side's codes are the api's, with the same categories.
func TestReadCodesAreDeclaredByTheAPI(t *testing.T) {
	for code, category := range quote.ReadCategories {
		if got, ok := api.Category(code); !ok || got != category {
			t.Errorf("%s: api says %v (declared %v), domain %v", code, got, ok, category)
		}
	}
}

// X-INV-003: every query of the read side is an audited read under trd.read that any actor kind
// may run (TSK's system actor and AI agents verify goals as human readers do); none needs a write
// grant (Python READ_PERMISSIONS: historical results need read grants only).
func TestReadQueriesNeedTrdRead(t *testing.T) {
	policy, err := authorization.NewPolicy(Rules()...)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{api.QueryQuoteReady, api.QueryQuoteHistory, api.QueryApprovalHistory, api.QueryDocumentHistory,
		api.QueryCommitHistory, api.QueryOutboxHistory, api.QueryDispatchPreflight, api.QueryDispatchTarget} {
		r, err := policy.Rule(q)
		if err != nil || r.Permission != PermissionRead {
			t.Errorf("%s: %+v %v", q, r, err)
			continue
		}
		for _, k := range []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem} {
			if !r.Admits(k) {
				t.Errorf("%s refuses %s", q, k)
			}
		}
	}
}

// The sources each command cites, as sent: a replay or reconciliation of it needs the caller to read
// them now (contract 06 §07); a response's are those it was recorded with.
func TestCommandsCiteTheirSources(t *testing.T) {
	ev := func(ids ...string) []api.Evidence {
		var out []api.Evidence
		for _, id := range ids {
			out = append(out, api.Evidence{SourceID: id, Locator: "p"})
		}
		return out
	}
	for name, c := range map[string]struct {
		cmd        api.Command
		ids        []string
		ofResponse bool
	}{
		"save_draft":       {api.SaveDraft{Data: api.QuoteData{Evidence: ev("a"), Lines: []api.Line{{Evidence: ev("b")}}}}, []string{"a", "b"}, false},
		"discard_draft":    {api.DiscardDraft{Evidence: ev("a")}, []string{"a"}, false},
		"decide":           {api.DecideReview{Evidence: ev("a"), MarginException: &api.MarginException{Evidence: ev("b")}}, []string{"a", "b"}, false},
		"decide, no proof": {api.DecideReview{}, nil, false},
		"revoke":           {api.RevokeApproval{Evidence: ev("a")}, []string{"a"}, false},
		"withdraw":         {api.Withdraw{Evidence: ev("a")}, []string{"a"}, false},
		"cancel":           {api.Cancel{Evidence: ev("a")}, []string{"a"}, false},
		"invalidate":       {api.InvalidateAcceptance{Evidence: ev("a")}, []string{"a"}, false},
		"dispute":          {api.DisputeAcceptance{Evidence: ev("a")}, []string{"a"}, false},
		"record":           {api.RecordResponse{SourceRef: api.Evidence{SourceID: "mail"}}, nil, true},
		"verify":           {api.VerifyResponse{}, nil, true},
		"freeze":           {api.Freeze{}, nil, false},
		"activate":         {api.Activate{}, nil, false},
		"record_signed":    {api.RecordSignedOrderDocument{Evidence: ev("a")}, []string{"a"}, false},
		"generate":         {api.GenerateOrderDocument{}, nil, false},
	} {
		ids, of := cited(c.cmd)
		slices.Sort(ids)
		if !slices.Equal(ids, c.ids) || of != c.ofResponse {
			t.Errorf("%s: %v %v", name, ids, of)
		}
	}
}

// A replay or reconciliation of a command that cites sources is refused without EVD's answer for it
// (fail closed), with an answer for another command, or when the caller does not read them all;
// commands citing no source need none.
func TestSourcesReadFailsClosed(t *testing.T) {
	ctx := context.Background()
	with := func(v sourcesVerdict) context.Context { return context.WithValue(ctx, sourcesKey{}, v) }
	for name, c := range map[string]struct {
		ctx     context.Context
		command string
		want    string
	}{
		"no answer":           {ctx, api.CommandRecordResponse, kernel.CodeReferenceInvalid},
		"another command":     {with(sourcesVerdict{command: api.CommandSaveDraft, readable: true}), api.CommandRecordResponse, kernel.CodeReferenceInvalid},
		"a source unread":     {with(sourcesVerdict{command: api.CommandWithdraw}), api.CommandWithdraw, kernel.CodeReferenceInvalid},
		"all read":            {with(sourcesVerdict{command: api.CommandWithdraw, readable: true}), api.CommandWithdraw, ""},
		"no source cited":     {ctx, api.CommandFreeze, ""},
		"control, no sources": {ctx, api.CommandBindControl, ""},
	} {
		err := sourcesRead(c.ctx, c.command)
		got := ""
		if rej, ok := kernel.AsRejection(err); ok {
			got = rej.Code
		} else if err != nil {
			got = err.Error()
		}
		if got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

// Pages of a history: entries after the position, at most the page size, and the next position only
// when more follow.
func TestHistoryPages(t *testing.T) {
	all := []int64{1, 2, 3, 4, 5}
	id := func(n int64) int64 { return n }
	for name, c := range map[string]struct {
		p    api.Page
		want []int64
		next int64
	}{
		"from the start":   {api.Page{Limit: 2}, []int64{1, 2}, 2},
		"the next page":    {api.Page{After: 2, Limit: 2}, []int64{3, 4}, 4},
		"the last page":    {api.Page{After: 4, Limit: 2}, []int64{5}, 0},
		"exactly the rest": {api.Page{After: 3, Limit: 2}, []int64{4, 5}, 0},
		"past the end":     {api.Page{After: 9}, nil, 0},
		"the default page": {api.Page{}, all, 0},
	} {
		got, next := page(all, c.p, id)
		if !slices.Equal(got, c.want) || next != c.next {
			t.Errorf("%s: %v next %d", name, got, next)
		}
	}
}

// The recipient as bound against PTY now (T03 A14/A15): the customer confirmed at the bound revision
// with the customer role, the contact confirmed at the bound row version and the customer's, each
// address role in force.
func TestRecipientChecks(t *testing.T) {
	addr := quote.AddressRef{PartyID: "customer.1", PartyRevision: 2, AddressID: "hq"}
	d := quote.Data{Customer: &quote.PartyRef{PartyID: "customer.1", Revision: 2}, Contact: &quote.ContactRef{ContactID: "contact.1", RowVersion: 2},
		Addresses: []quote.AddressRef{addr}}
	ok := func() *quote.Facts {
		return &quote.Facts{Party: &quote.Party{Confirmed: true, Revision: 2, Customer: true},
			Contact: &quote.Contact{PartyID: "customer.1", RowVersion: 2, Confirmed: true}, Addresses: map[quote.AddressRef]quote.Address{addr: {}}}
	}
	codes := func(f *quote.Facts) []string {
		var out []string
		for _, c := range recipientChecks(d, f) {
			rej, _ := kernel.AsRejection(c.err)
			out = append(out, c.check+":"+rej.Code+":"+rej.Fields[0].Path)
		}
		return out
	}
	if got := codes(ok()); len(got) != 0 {
		t.Fatalf("as bound: %v", got)
	}
	revised := ok()
	revised.Party.Revision = 3
	moved := ok()
	moved.Contact.RowVersion = 3
	gone := ok()
	gone.Party, gone.Contact, gone.Addresses = nil, nil, map[quote.AddressRef]quote.Address{}
	retired := ok()
	retired.Addresses[addr] = quote.Address{Check: func(path string) error { return api.Reject(api.CodeReferenceStale, path) }}
	for name, c := range map[string]struct {
		f    *quote.Facts
		want []string
	}{
		"the customer revised":   {revised, []string{"customer:REFERENCE_STALE:/data/customer_ref"}},
		"the contact moved on":   {moved, []string{"contact:REFERENCE_STALE:/data/contact_ref"}},
		"an address role lapsed": {retired, []string{"addresses:REFERENCE_STALE:/data/customer_address_refs/0"}},
		"nothing visible": {gone, []string{"customer:REFERENCE_INVALID:/data/customer_ref", "contact:REFERENCE_INVALID:/data/contact_ref",
			"addresses:REFERENCE_INVALID:/data/customer_address_refs/0"}},
	} {
		if got := codes(c.f); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v", name, got)
		}
	}
}

// Python test_t03_a32_sales_fx.py test_age_boundary_and_stale_formalization_leave_no_business_writes
// (PYT-0383, the re-check at quote_ready): readiness judges the sales exchange rate's age against
// the pricing policy's maximum at the instant it checks, the boundary inclusive, as the
// formalization did; a quote in the comparison currency has no rate to judge.
func TestReadinessJudgesTheSalesFXAge(t *testing.T) {
	observed := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	rate := quote.Ref{Kind: "fx_rate", ID: "fx.1", Revision: 1}
	pricing := quote.Ref{Kind: "pricing_policy", ID: "policy.1", Revision: 1}
	f := facts{configs: map[polapi.ConfigRef]polapi.ResolvedConfig{
		{Kind: rate.Kind, ConfigID: rate.ID, Revision: 1}: {Data: polapi.ConfigData{FXRate: &polapi.FXRate{ObservedAt: kernel.FormatTimestamp(observed)}}},
		{Kind: pricing.Kind, ConfigID: pricing.ID, Revision: 1}: {Data: polapi.ConfigData{
			PricingPolicy: &polapi.PricingPolicy{MaxFXAgeSeconds: 3600}}},
	}}
	d := quote.Data{SalesFX: &rate, PricingPolicy: &pricing}
	codeOf := func(err error) string {
		if rej, ok := kernel.AsRejection(err); ok {
			return rej.Code + " at " + rej.Fields[0].Path
		}
		return "OK"
	}
	for name, c := range map[string]struct {
		at   time.Time
		want string
	}{
		"within the age":       {observed.Add(30 * time.Minute), "OK"},
		"at the boundary":      {observed.Add(time.Hour), "OK"},
		"a microsecond beyond": {observed.Add(time.Hour + time.Microsecond), "FX_STALE at /data/sales_fx_ref"},
		"before it was seen":   {observed.Add(-time.Second), "FX_STALE at /data/sales_fx_ref"},
	} {
		if got := codeOf(salesFXAge(d, f, c.at)); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
	if got := codeOf(salesFXAge(quote.Data{PricingPolicy: &pricing}, f, observed.Add(48*time.Hour))); got != "OK" {
		t.Fatalf("no sales rate: %s", got)
	}
	if got := codeOf(salesFXAge(d, facts{}, observed)); got != "REFERENCE_INVALID at /data/sales_fx_ref" {
		t.Fatalf("a rate POL did not answer: %s", got)
	}
}
