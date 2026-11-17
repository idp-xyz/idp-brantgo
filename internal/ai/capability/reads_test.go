package capability

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Every Q01 read calls an api query, never a human-only command and never a grant. The result the
// model sees is bounded: a long extract is cut, and file bytes, document bytes and configuration
// bodies are not in it. An object the query does not return is NOT_FOUND, which is also what a
// read of another tenant's object is: the context's own scope hides it.
func TestQ01ReadsAreBoundedApiReads(t *testing.T) {
	f := &readFakes{text: strings.Repeat("询", 20000), form: strings.Repeat("L", 8000)}
	reg, err := Q01Registry(Q01Targets{INQReads: f, CSTReads: f, CSTLists: f, POLLists: f, CATReads: f,
		PTYReads: f, PTYProfiles: f, TRDReads: f, EVDReads: f, EVDExtracts: f})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, c := range reg.Capabilities() {
		if c.SideEffect != Read {
			continue
		}
		seen[c.Command] = true
		if c.Approval != ApprovalNone {
			t.Errorf("%s carries a grant", c.Command)
		}
		if IsHumanOnly(c.Command) || IsHumanOnly(c.Permission()) || !strings.HasSuffix(c.Permission(), ".read") {
			t.Errorf("%s permission %s", c.Command, c.Permission())
		}
		raw, err := c.Schema()
		if err != nil || strings.Contains(string(raw), "Grant") || strings.Contains(string(raw), KeyField) {
			t.Errorf("%s schema %s %v", c.Command, raw, err)
		}
		res, err := c.Run(context.Background(), c.WithKey(reflectZero(c), "k1"))
		if err != nil {
			t.Fatalf("%s: %v", c.Command, err)
		}
		if res.ActionID != "" || res.Replayed {
			t.Errorf("%s looks like an action %+v", c.Command, res.Receipt)
		}
		out, err := json.Marshal(res.Detail)
		if err != nil {
			t.Fatal(err)
		}
		if len(out) > MaxReadBytes {
			t.Errorf("%s result is %d bytes", c.Command, len(out))
		}
		if c.Command == ReadSource {
			f.sourceOut = string(out)
		}
		if strings.Contains(string(out), "account_number") {
			t.Errorf("%s returned sensitive content", c.Command)
		}
	}
	for _, name := range []string{ReadRfq, ReadSnapshots, ReadContext, ReadConfig, ReadSku, ReadParty, ReadQuote, ReadResponse, ReadOrder, ReadSource} {
		if !seen[name] {
			t.Errorf("missing read %s", name)
		}
	}
	if !f.rfq || !f.snapshots || !f.context || !f.configs || !f.sku || !f.party || !f.profile || !f.quote || !f.ready || !f.response || !f.order || !f.source || !f.extract {
		t.Fatalf("a read did not call its api: %+v", *f)
	}
	if !strings.Contains(f.sourceOut, `"truncated":true`) || strings.Contains(f.sourceOut, strings.Repeat("询", SourceTextRunes+1)) {
		t.Fatalf("source text was not bounded: %s", f.sourceOut)
	}
	if strings.Contains(f.sourceOut, strings.Repeat("L", LocatorRunes+1)) {
		t.Fatal("the locator form was not bounded")
	}
	c, err := reg.Lookup(kernel.TenantID("tenant.a"), ReadRfq, "/command")
	if err != nil {
		t.Fatal(err)
	}
	in := reflect.New(c.Input()).Elem()
	in.FieldByName("IdempotencyKey").SetString("k1")
	in.FieldByName("RfqID").SetString("rfq.other-tenant")
	_, err = c.Run(context.Background(), in.Interface())
	if rej, ok := kernel.AsRejection(err); !ok || rej.Code != kernel.CodeNotFound {
		t.Fatalf("a read of an object outside the query's answer: %v", err)
	}
}

// A read cannot be registered with an approval grant (D-7).
func TestAReadCannotCarryAGrant(t *testing.T) {
	type in struct {
		IdempotencyKey string
		RfqID          string
	}
	_, err := Define(Spec{Command: "rfq.peek", Context: "inq", Contract: inqapi.Contract, Version: 1,
		SideEffect: Read, Risk: RiskLow, Approval: ApprovalGrant, Perm: "inq.read", Summary: "no"},
		func(in) ObjectRef { return ObjectRef{} },
		func(context.Context, in) (readReceipt, error) { return readReceipt{}, nil })
	if err == nil {
		t.Fatal("a read with a grant was registered")
	}
}

type readFakes struct {
	text, form, sourceOut                                                                                 string
	rfq, snapshots, context, configs, sku, party, profile, quote, ready, response, order, source, extract bool
}

func (f *readFakes) GetRfq(_ context.Context, id string) (inqapi.RfqView, error) {
	f.rfq = true
	if id == "rfq.other-tenant" {
		return inqapi.RfqView{}, kernel.Reject(kernel.CodeNotFound, "/id", "")
	}
	return inqapi.RfqView{RfqID: "rfq.1", Status: "ready", RowVersion: 2, CustomerID: "customer.1",
		Lines: []inqapi.RfqLineView{{RfqLine: inqapi.RfqLine{LineID: "L1", SkuID: "sku.1", Quantity: "24", UOM: "PCS"}}}}, nil
}

func (f *readFakes) SnapshotsOfRfq(context.Context, string) (cstapi.RfqSnapshotList, error) {
	f.snapshots = true
	return cstapi.RfqSnapshotList{RfqID: "rfq.1", Snapshots: []cstapi.RfqSnapshot{{
		SnapshotID: "snap.1", ResultHash: strings.Repeat("ab", 32), Adoptability: "adoptable", ContextID: "context.1"}}}, nil
}

func (f *readFakes) GetContext(context.Context, string) (cstapi.ContextView, error) {
	f.context = true
	return cstapi.ContextView{ContextID: "context.1", RootVersion: 2, ActiveRevision: 1, Revisions: []cstapi.ContextRevisionView{{
		Revision: 1, Status: "active", Data: cstapi.ContextData{
			RfqRef:                 &cstapi.RfqRef{RfqID: "rfq.1", RowVersion: 2},
			CalculationCurrencyRef: &cstapi.ConfigRef{Kind: "currency", ID: "USD", Revision: 1}}}}}, nil
}

func (f *readFakes) ConfigsInForce(_ context.Context, kind string) (polapi.ConfigsInForce, error) {
	f.configs = true
	if kind == polapi.KindBankAccount {
		return polapi.ConfigsInForce{}, kernel.Reject(kernel.CodePermissionDenied, "/args/Kind", "")
	}
	return polapi.ConfigsInForce{Kind: kind, Configs: []polapi.ConfigInForce{{ID: "USD", Revision: 1}}}, nil
}

func (f *readFakes) GetSku(context.Context, string) (catapi.SkuView, error) {
	f.sku = true
	return catapi.SkuView{SkuID: "sku.1", Status: "active", RowVersion: 1, Name: "shirt",
		Revisions: []catapi.SpecRevision{{Revision: 1, Name: "s"}}}, nil
}

func (f *readFakes) GetParty(context.Context, string) (ptyapi.PartyView, error) {
	f.party = true
	return ptyapi.PartyView{PartyID: "customer.1", Status: "active", RowVersion: 2, Revision: 1, Name: "客户",
		Addresses: []ptyapi.Address{{AddressID: "hq", Role: "headquarters", Country: "DE", Address: "secret street"}}}, nil
}

func (f *readFakes) GetTradingProfile(context.Context, string) (ptyapi.TradingProfileView, error) {
	f.profile = true
	return ptyapi.TradingProfileView{PartyID: "customer.1", ActiveRevision: 1, Revisions: []ptyapi.TradingProfileRevisionView{{
		Revision: 1, Status: "active",
		PaymentTerms: ptyapi.TermsRef{Kind: "payment_terms", ConfigID: "NET30", Revision: 1},
		TradeTerms:   ptyapi.TermsRef{Kind: "trade_terms", ConfigID: "FCA", Revision: 1}}}}, nil
}

func (f *readFakes) GetQuote(context.Context, string) (trdapi.QuoteView, error) {
	f.quote = true
	return trdapi.QuoteView{QuoteID: "quote.1", RootVersion: 3, ActiveRevision: 1, Revisions: []trdapi.RevisionView{{
		Revision: 1, Status: "active", BusinessHash: strings.Repeat("ab", 32), Commercial: "NOT FOR THE MODEL",
		Data: trdapi.QuoteData{CurrencyRef: &trdapi.ConfigRef{Kind: "currency", ID: "USD", Revision: 1}}}}}, nil
}

func (f *readFakes) QuoteReady(context.Context, trdapi.ReadyQuery) (trdapi.QuoteReadiness, error) {
	f.ready = true
	return trdapi.QuoteReadiness{QuoteID: "quote.1", BusinessRevision: 1, BusinessHash: strings.Repeat("ab", 32), Ready: true}, nil
}

func (f *readFakes) GetCustomerResponse(context.Context, string) (trdapi.CustomerResponseView, error) {
	f.response = true
	return trdapi.CustomerResponseView{ResponseID: "response.1", QuoteID: "quote.1", BusinessRevision: 1,
		BusinessHash: strings.Repeat("ab", 32), Acceptances: []trdapi.AcceptanceView{{AcceptanceID: "acceptance.1", Formed: true}}}, nil
}

func (f *readFakes) GetOrder(context.Context, string) (trdapi.OrderView, error) {
	f.order = true
	return trdapi.OrderView{OrderID: "order.1", Status: "draft", RowVersion: 1, QuoteID: "quote.1", BusinessRevision: 1,
		BusinessHash: strings.Repeat("ab", 32), Snapshot: "the commercial snapshot is not returned"}, nil
}

func (f *readFakes) GetSource(context.Context, string) (evdapi.SourceView, error) {
	f.source = true
	return evdapi.SourceView{SourceID: "src.1", SourceKind: "file_extract", ContentSHA256: strings.Repeat("cd", 32), ContentText: f.text,
		Extract: &evdapi.ExtractRef{FileID: "file.1", Extractor: "brant.ingest.text/1"}}, nil
}

func (f *readFakes) GetExtract(context.Context, string) (evdapi.ExtractView, error) {
	f.extract = true
	return evdapi.ExtractView{SourceID: "src.1", LocatedForm: f.form}, nil
}
