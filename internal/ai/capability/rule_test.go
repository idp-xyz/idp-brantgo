package capability

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The rules an api command type states about its members (struct tags, Rule) are kept true here
// against the context's own validation, for every Q01 capability whose command validates itself
// (S4 G3): a call built from the rules' minimum passes; every member given its example passes; a
// required member left out is refused; each value of an enumeration is accepted and another one
// refused; a value outside a stated format or bound is refused. A rule that drifts from Validate
// fails here, whichever side changed.

// validating is an api command that checks its closed contract.
type validating interface{ Validate() error }

// example is a value the rule's format admits.
func example(r Rule) string {
	switch {
	case len(r.Enum) > 0:
		return r.Enum[0]
	case r.Decimal != "":
		return "1"
	case r.Ratio:
		return "0"
	case r.Date:
		return "2027-01-31"
	case r.Instant:
		return "2027-01-31T00:00:00Z"
	case r.Hash:
		return strings.Repeat("a", 64)
	}
	return "x1" // an identifier, and a text
}

// outside is a value the rule's format refuses, or "" when the rule states no format.
func outside(r Rule) string {
	switch {
	case len(r.Enum) > 0:
		return "zz.not.allowed"
	case r.Decimal != "":
		return "1e3"
	case r.Ratio:
		return "1"
	case r.Date:
		return "31.01.2027"
	case r.Instant:
		return "2027-01-31 00:00"
	case r.Hash:
		return "abc"
	case r.ID:
		return "not an id"
	}
	return ""
}

// build returns a value of t with the members its rules require, the members of the form its
// controlling members select, and every member on the way to focus (field names; a list gets one
// item). leaf, when not nil, is the value of the member focus ends at.
func build(t *testing.T, typ reflect.Type, rule Rule, focus []string, leaf *reflect.Value) reflect.Value {
	t.Helper()
	if len(focus) == 0 && leaf != nil && leaf.Type().AssignableTo(typ) {
		return *leaf
	}
	switch typ.Kind() {
	case reflect.Pointer:
		p := reflect.New(typ.Elem())
		p.Elem().Set(build(t, typ.Elem(), rule, focus, leaf))
		return p
	case reflect.Slice:
		s := reflect.MakeSlice(typ, 1, 1)
		s.Index(0).Set(build(t, typ.Elem(), Rule{}, focus, leaf))
		return s
	case reflect.Struct:
		return buildStruct(t, typ, rule, focus, leaf)
	}
	v := reflect.New(typ).Elem()
	switch {
	case typ.Kind() == reflect.String:
		v.SetString(example(rule))
	case v.CanInt():
		n := int64(1)
		if rule.Min != nil && *rule.Min > 1 {
			n = *rule.Min
		}
		v.SetInt(n)
	case typ.Kind() == reflect.Bool:
		v.SetBool(true)
	default:
		t.Fatalf("no example of kind %s", typ.Kind())
	}
	return v
}

func buildStruct(t *testing.T, typ reflect.Type, rule Rule, focus []string, leaf *reflect.Value) reflect.Value {
	t.Helper()
	v := reflect.New(typ).Elem()
	rules := map[string]Rule{}
	for i := range typ.NumField() {
		f := typ.Field(i)
		r, err := RuleOf(f)
		if err != nil {
			t.Fatal(err)
		}
		rules[f.Name] = r
		for _, sibling := range []string{r.With, condField(r)} {
			if _, ok := typ.FieldByName(sibling); sibling != "" && !ok {
				t.Fatalf("%s.%s names a sibling %s that is not there", typ.Name(), f.Name, sibling)
			}
		}
	}
	forced, form := map[string]bool{}, map[string]string{}
	if len(focus) > 0 {
		forced[focus[0]] = true
		r := rules[focus[0]]
		if r.With != "" {
			forced[r.With] = true
		}
		if r.When != nil {
			form[r.When.Field] = r.When.Values[0] // the form the forced member belongs to
		}
	}
	set := func(i int) {
		f := typ.Field(i)
		var sub []string
		var subLeaf *reflect.Value
		if len(focus) > 0 && focus[0] == f.Name {
			sub, subLeaf = focus[1:], leaf
		}
		r := rules[f.Name]
		if want, ok := form[f.Name]; ok && subLeaf == nil {
			s := reflect.ValueOf(want)
			subLeaf = &s
		}
		v.Field(i).Set(build(t, f.Type, r, sub, subLeaf))
	}
	// The members that stand on their own, then those of the form the first select, then those
	// that go with a given member.
	for i := range typ.NumField() {
		f := typ.Field(i)
		if r := rules[f.Name]; f.IsExported() && f.Name != KeyField && r.When == nil && (r.Required || forced[f.Name]) {
			set(i)
		}
	}
	for i := range typ.NumField() {
		f := typ.Field(i)
		r := rules[f.Name]
		if r.When == nil || !r.When.Holds(v.FieldByName(r.When.Field).String()) || (r.Optional && !forced[f.Name]) {
			continue
		}
		set(i)
	}
	for i := range typ.NumField() {
		f := typ.Field(i)
		if r := rules[f.Name]; r.With != "" && v.Field(i).IsZero() && !v.FieldByName(r.With).IsZero() {
			set(i)
		}
	}
	if rule.Kind != "" && (len(focus) != 1 || focus[0] != "Kind" || leaf == nil) {
		v.FieldByName("Kind").SetString(rule.Kind)
	}
	return v
}

func isInt(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return true
	}
	return false
}

func condField(r Rule) string {
	if r.When != nil {
		return r.When.Field
	}
	return ""
}

// site is a member of a command type: the field names that lead to it, its rule and its type.
type site struct {
	path []string
	rule Rule
	typ  reflect.Type
}

func (s site) String() string { return strings.Join(s.path, ".") }

// sites lists every member reachable from struct type typ.
func sites(t *testing.T, typ reflect.Type, prefix []string, top bool) []site {
	t.Helper()
	var out []site
	for i := range typ.NumField() {
		f := typ.Field(i)
		if !f.IsExported() || (top && f.Name == KeyField) {
			continue
		}
		r, err := RuleOf(f)
		if err != nil {
			t.Fatal(err)
		}
		path := append(slices.Clone(prefix), f.Name)
		out = append(out, site{path: path, rule: r, typ: f.Type})
		if inner := base(f.Type); inner.Kind() == reflect.Struct {
			out = append(out, sites(t, inner, path, false)...)
		}
	}
	return out
}

// at returns the member of v that path leads to (through pointers and the first item of a list).
func at(v reflect.Value, path []string) reflect.Value {
	for _, name := range path {
		for v.Kind() == reflect.Pointer || v.Kind() == reflect.Slice {
			if v.Kind() == reflect.Pointer {
				v = v.Elem()
			} else {
				v = v.Index(0)
			}
		}
		v = v.FieldByName(name)
	}
	return v
}

func validate(c Capability, v reflect.Value) error {
	return c.WithKey(v.Interface(), "k1").(validating).Validate()
}

// described reports whether the command type states rules: at least one top-level contract tag.
func described(c Capability) bool {
	for i := range c.Input().NumField() {
		if c.Input().Field(i).Tag.Get("contract") != "" {
			return true
		}
	}
	return false
}

// kindsTheCommandChecks are the references whose kind a tool schema states and the api command's
// Validate does not check: the POL references of a quote draft ("<command> <member> <kind>").
var kindsTheCommandChecks = []string{
	"quote.save_draft Data.IssuerRef company_profile",
	"quote.save_draft Data.Lines.UnitRef unit",
	"quote.save_draft Data.Lines.ConversionRef unit_conversion",
	"quote.save_draft Data.Lines.TaxApplicationRef tax_application",
	"quote.save_draft Data.Charges.FeeCategoryRef fee_category",
	"quote.save_draft Data.Charges.TaxApplicationRef tax_application",
	"quote.save_draft Data.CurrencyRef currency",
	"quote.save_draft Data.PricingPolicyRef pricing_policy",
	"quote.save_draft Data.SalesFXRef fx_rate",
	"quote.save_draft Data.TradeTermsRef trade_terms",
	"quote.save_draft Data.PaymentTermsRef payment_terms",
	"quote.save_draft Data.Delivery.CalendarRef calendar",
	"quote.save_draft Data.DocumentTemplateRef quote_document_template",
	"quote.save_draft Data.SequenceRef number_sequence",
	"quote.save_draft Data.LegalTermsRef legal_terms",
}

// TestRulesAreWhatValidateEnforces is the drift guard of S4 G3 (unit item 1).
func TestRulesAreWhatValidateEnforces(t *testing.T) {
	reg, err := Q01Registry(Q01Targets{})
	if err != nil {
		t.Fatal(err)
	}
	var covered, beyond []string
	for _, c := range reg.Capabilities() {
		if _, ok := reflect.Zero(c.Input()).Interface().(validating); !ok || !described(c) {
			continue
		}
		covered = append(covered, c.Command)
		t.Run(c.Command, func(t *testing.T) {
			// A call built from the rules' minimum passes the context's validation.
			if err := validate(c, build(t, c.Input(), Rule{}, nil, nil)); err != nil {
				t.Fatalf("the minimum is refused: %v", err)
			}
			for _, s := range sites(t, c.Input(), nil, true) {
				whole := build(t, c.Input(), Rule{}, s.path, nil)
				if err := validate(c, whole); err != nil {
					t.Errorf("%s given its example is refused: %v", s, err)
					continue
				}
				// A member the rules require, here or in this form, is refused when it is left out.
				if s.rule.Required || (s.rule.When != nil && !s.rule.Optional) || s.rule.With != "" {
					m := at(whole, s.path)
					m.Set(reflect.Zero(m.Type()))
					if _, ok := kernel.AsRejection(validate(c, whole)); !ok {
						t.Errorf("%s is stated required, and a call without it passes", s)
					}
				}
				if base(s.typ).Kind() == reflect.String && s.typ.Kind() != reflect.Slice {
					// Every value of an enumeration passes; a value outside the format is refused.
					for _, value := range s.rule.Enum {
						leaf := reflect.ValueOf(value).Convert(base(s.typ))
						if err := validate(c, build(t, c.Input(), Rule{}, s.path, &leaf)); err != nil {
							t.Errorf("%s = %q is refused: %v", s, value, err)
						}
					}
					if bad := outside(s.rule); bad != "" {
						leaf := reflect.ValueOf(bad).Convert(base(s.typ))
						if _, ok := kernel.AsRejection(validate(c, build(t, c.Input(), Rule{}, s.path, &leaf))); !ok {
							t.Errorf("%s = %q passes, outside what the rule states", s, bad)
						}
					}
					if s.rule.Positive {
						leaf := reflect.ValueOf("0").Convert(base(s.typ))
						if _, ok := kernel.AsRejection(validate(c, build(t, c.Input(), Rule{}, s.path, &leaf))); !ok {
							t.Errorf("%s = 0 passes; the rule states it is above 0", s)
						}
					}
				}
				// The kind a reference names: another kind is refused by Validate, or it is one of the
				// references whose kind the context checks when it runs the command (kindsTheCommandChecks).
				if s.rule.Kind != "" {
					leaf := reflect.ValueOf("zz_other")
					if _, ok := kernel.AsRejection(validate(c, build(t, c.Input(), Rule{}, append(slices.Clone(s.path), "Kind"), &leaf))); !ok {
						beyond = append(beyond, c.Command+" "+s.String()+" "+s.rule.Kind)
					}
				}
				if isInt(base(s.typ)) && s.typ.Kind() != reflect.Slice {
					for _, bound := range []struct {
						limit *int64
						step  int64
					}{{s.rule.Min, -1}, {s.rule.Max, 1}} {
						if bound.limit == nil {
							continue
						}
						leaf := reflect.ValueOf(*bound.limit + bound.step).Convert(base(s.typ))
						if _, ok := kernel.AsRejection(validate(c, build(t, c.Input(), Rule{}, s.path, &leaf))); !ok {
							t.Errorf("%s = %d passes, outside its bounds", s, *bound.limit+bound.step)
						}
					}
				}
				if s.typ.Kind() == reflect.Slice && s.rule.Min != nil && *s.rule.Min > 0 && !s.rule.Required {
					empty := reflect.MakeSlice(s.typ, 0, 0)
					if _, ok := kernel.AsRejection(validate(c, build(t, c.Input(), Rule{}, s.path, &empty))); !ok {
						t.Errorf("%s given empty passes, below its minimum", s)
					}
				}
			}
		})
	}
	// The kinds Validate does not check are the POL references of a quote draft, each a POL kind:
	// TRD checks them when it runs quote.save_draft (REFERENCE_INVALID at the reference's kind),
	// which the composed test TestQuoteDraftReferencesTakeTheKindsTheSchemaStates proves on
	// PostgreSQL for exactly this list.
	if !slices.Equal(beyond, kindsTheCommandChecks) {
		t.Errorf("kinds Validate does not check:\n got %q\nwant %q", beyond, kindsTheCommandChecks)
	}
	for _, entry := range beyond {
		if kind := entry[strings.LastIndexByte(entry, ' ')+1:]; !polapi.KindKnown(kind) {
			t.Errorf("%s is not a POL kind", entry)
		}
	}
	// The write tools the unit names state their rules, and are checked.
	for _, command := range []string{evdapi.CommandRegisterSource, evdapi.CommandRegisterFile, inqapi.CommandSaveRfq, trdapi.CommandSaveDraft,
		trdapi.CommandRevise, trdapi.CommandUpdateMetadata, trdapi.CommandRecordResponse, trdapi.CommandPrepareOrder,
		trdapi.CommandRequestOrderReview} {
		if !slices.Contains(covered, command) {
			t.Errorf("%s states no rules or does not validate itself", command)
		}
	}
}

// schemaAt returns the schema of the member path names in a tool's schema (item of a list: "[]").
func schemaAt(t *testing.T, raw json.RawMessage, path ...string) map[string]any {
	t.Helper()
	var s map[string]any
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	for _, name := range path {
		if name == "[]" {
			s, _ = s["items"].(map[string]any)
		} else {
			props, _ := s["properties"].(map[string]any)
			s, _ = props[name].(map[string]any)
		}
		if s == nil {
			t.Fatalf("the schema has no %s", strings.Join(path, "."))
		}
	}
	return s
}

func strs(v any) []string {
	var out []string
	list, _ := v.([]any)
	for _, x := range list {
		out = append(out, x.(string))
	}
	return out
}

func q01Schema(t *testing.T, command string) (Capability, json.RawMessage) {
	t.Helper()
	reg, err := Q01Registry(Q01Targets{})
	if err != nil {
		t.Fatal(err)
	}
	c, err := reg.Lookup("tenant.1", command, "/command")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := c.Schema()
	if err != nil {
		t.Fatal(err)
	}
	return c, raw
}

// An enumeration of a tool schema is exactly the api's constants (unit item 1): the values the
// model is offered are the ones the context's contract names, no more and no fewer.
func TestSchemaEnumerationsAreTheApisConstants(t *testing.T) {
	lookupKinds := slices.DeleteFunc(polapi.Kinds(), func(k string) bool { return k == polapi.KindBankAccount })
	for _, c := range []struct {
		command string
		path    []string
		want    []string
	}{
		{trdapi.CommandSaveDraft, []string{"Data", "Delivery", "Kind"}, []string{trdapi.DeliveryFixed, trdapi.DeliveryLeadTime}},
		{trdapi.CommandSaveDraft, []string{"Data", "Delivery", "Milestone"},
			[]string{trdapi.MilestoneGoodsReady, trdapi.MilestoneHandover, trdapi.MilestoneArrival}},
		{trdapi.CommandSaveDraft, []string{"Data", "Delivery", "DayBasis"}, []string{trdapi.DayBasisCalendar, trdapi.DayBasisWorking}},
		{trdapi.CommandSaveDraft, []string{"Data", "Charges", "[]", "Inclusion"},
			[]string{trdapi.InclusionSeparate, trdapi.InclusionIncludedInLine}},
		{trdapi.CommandRecordResponse, []string{"Classification"}, []string{trdapi.ClassificationFullUnconditional,
			trdapi.ClassificationConditional, trdapi.ClassificationPartial, trdapi.ClassificationReject, trdapi.ClassificationAmbiguous}},
		{trdapi.CommandRecordResponse, []string{"ReceivedAtBasis"},
			[]string{trdapi.ArrivalChannelServer, trdapi.ArrivalImportRecord, trdapi.ArrivalSourceClaimed}},
		{inqapi.CommandSaveRfq, []string{"Lines", "[]", "UOM"}, []string{inqapi.UnitPCS}},
		{inqapi.CommandSaveRfq, []string{"Lines", "[]", "Locators", "[]", "Field"}, inqapi.Fields},
		{inqapi.CommandSaveRfq, []string{"Lines", "[]", "Pending", "[]", "Field"}, inqapi.Fields},
		{inqapi.CommandSaveRfq, []string{"Lines", "[]", "Pending", "[]", "Reason"}, inqapi.PendingReasons},
		{evdapi.CommandRegisterSource, []string{"SourceKind"}, []string{evdapi.KindFileExtract, evdapi.KindManualText, evdapi.KindEmailText}},
		{evdapi.CommandRegisterFile, []string{"MimeType"}, []string{evdapi.MimeText, evdapi.MimePDF, evdapi.MimeXLSX}},
		{ReadConfig, []string{"Kind"}, lookupKinds},
	} {
		_, raw := q01Schema(t, c.command)
		if got := strs(schemaAt(t, raw, c.path...)["enum"]); !slices.Equal(got, c.want) {
			t.Errorf("%s %s: enum %v, the api's constants %v", c.command, strings.Join(c.path, "."), got, c.want)
		}
	}
	// The kinds a place and a delivery anchor take are the api's too.
	_, raw := q01Schema(t, trdapi.CommandSaveDraft)
	for path, want := range map[string]string{"NamedPlaceRef": trdapi.PlaceKind, "Delivery.PlaceRef": trdapi.PlaceKind,
		"Delivery.AnchorRef": trdapi.AnchorKind} {
		if got := schemaAt(t, raw, append([]string{"Data"}, append(strings.Split(path, "."), "Kind")...)...)["const"]; got != want {
			t.Errorf("%s names kind %v, the api's %s", path, got, want)
		}
	}
}

// What the model is sent for quote.save_draft and rfq.save_draft (the two the live trials stopped
// at): the required members, the formats and the words.
func TestSchemaStatesWhatADraftNeeds(t *testing.T) {
	_, quote := q01Schema(t, trdapi.CommandSaveDraft)
	if got := strs(schemaAt(t, quote)["required"]); !slices.Equal(got, []string{"QuoteID"}) {
		t.Errorf("quote.save_draft requires %v", got)
	}
	line := schemaAt(t, quote, "Data", "Lines", "[]")
	if got := strs(line["required"]); !slices.Equal(got, []string{"DiscountRatio", "Evidence", "LineID", "PriceBasisQuantity", "Quantity",
		"RfqLineID", "TaxApplicationRef", "UnitPrice", "UnitRef", "sku_ref"}) {
		t.Errorf("a quote line requires %v", got)
	}
	for member, words := range map[string]string{"UnitPrice": "decimal, up to 8 decimals", "PriceBasisQuantity": "decimal above 0, up to 6 decimals",
		"DiscountRatio": "decimal from 0 to below 1"} {
		if d, _ := schemaAt(t, quote, "Data", "Lines", "[]", member)["description"].(string); !strings.Contains(d, words) {
			t.Errorf("%s is described as %q", member, d)
		}
	}
	delivery := schemaAt(t, quote, "Data", "Delivery")
	if got := strs(delivery["required"]); !slices.Equal(got, []string{"Kind", "Milestone", "PlaceRef", "TimeZone"}) {
		t.Errorf("a delivery requires %v", got)
	}
	if schemaAt(t, quote, "Data", "Delivery", "Date")["format"] != "date" || schemaAt(t, quote, "Data", "ExpiresAt")["format"] != "date-time" ||
		schemaAt(t, quote, "Data", "CostSnapshotRef", "ResultHash")["pattern"] != "^[0-9a-f]{64}$" ||
		schemaAt(t, quote, "Data", "CurrencyRef", "Kind")["const"] != "currency" {
		t.Errorf("formats of the quote draft: %s", quote)
	}
	if min, ok := schemaAt(t, quote, "ExpectedVersion")["minimum"].(float64); !ok || min != 0 {
		t.Errorf("ExpectedVersion: %v", schemaAt(t, quote, "ExpectedVersion"))
	}
	_, rfq := q01Schema(t, inqapi.CommandSaveRfq)
	if strings.Contains(string(rfq), "opportunity") || strings.Contains(string(rfq), "Opportunity") {
		t.Fatalf("rfq.save_draft schema names an opportunity, which would change the q01.agent bundle: %s", rfq)
	}
	if strings.Contains(string(rfq), "document_profile") || strings.Contains(string(rfq), "DocumentProfile") ||
		strings.Contains(string(quote), "document_profile") || strings.Contains(string(quote), "DocumentProfile") {
		t.Fatalf("a Q01 tool schema names document_profile, which would change the q01.agent bundle")
	}
	if got := strs(schemaAt(t, rfq)["required"]); !slices.Equal(got, []string{"RfqID"}) {
		t.Errorf("rfq.save_draft requires %v", got)
	}
	if got := strs(schemaAt(t, rfq, "Lines", "[]", "Locators", "[]")["required"]); !slices.Equal(got, []string{"Field", "Locator", "Quote"}) {
		t.Errorf("a field locator requires %v", got)
	}
	// No member repeats that a string is not empty: the gateway refuses every empty string.
	for _, raw := range []json.RawMessage{quote, rfq} {
		if strings.Contains(string(raw), "minLength") {
			t.Errorf("the schema repeats minLength: %s", raw)
		}
	}
}

// A rejection's path names a member of the tool's arguments: the contexts' snake_case paths and
// the gateway's own Go-name paths resolve to the same member, with what the api type states of it.
func TestMemberOfARejectionsPath(t *testing.T) {
	quote, _ := q01Schema(t, trdapi.CommandSaveDraft)
	for path, want := range map[string]string{
		"/args/data/lines/0/unit_price":         "Data.Lines[0].UnitPrice",
		"/args/data/lines/3/sku_ref/sku_id":     "Data.Lines[3].sku_ref.SkuID",
		"/args/data/delivery/milestone":         "Data.Delivery.Milestone",
		"/args/data/delivery":                   "Data.Delivery",
		"/args/data/sales_fx_ref/revision":      "Data.SalesFXRef.Revision",
		"/args/Data/Lines/0/UnitPrice":          "Data.Lines[0].UnitPrice",
		"/args/expected_version":                "ExpectedVersion",
		"/args/data/customer_address_refs/1":    "Data.CustomerAddressRefs[1]",
		"/args/data/lines/0/evidence/0/locator": "Data.Lines[0].Evidence[0].Locator",
	} {
		m, ok := quote.Member(path)
		if !ok || m.Path != want {
			t.Errorf("%s resolves to %q (%v), want %s", path, m.Path, ok, want)
		}
	}
	for _, path := range []string{"/idempotency_key", "/command", "/grant", "/args", "/args/", "/args/nope", "/args/data/lines/x/unit_price",
		"/args/IdempotencyKey", "/budget/max_model_calls", "/"} {
		if m, ok := quote.Member(path); ok {
			t.Errorf("%s resolves to %q; it names no argument", path, m.Path)
		}
	}
	m, _ := quote.Member("/args/data/lines/0/unit_price")
	if !m.Rule.Required || !strings.Contains(m.Rule.Needs(), "required; decimal, up to 8 decimals") {
		t.Errorf("unit price needs %q", m.Rule.Needs())
	}
	if !m.Missing(json.RawMessage(`{"QuoteID":"q","Data":{"Lines":[{"LineID":"L1"}]}}`)) ||
		m.Missing(json.RawMessage(`{"Data":{"Lines":[{"UnitPrice":"8.00"}]}}`)) || !m.Missing(json.RawMessage(`{"Data":{}}`)) ||
		!m.Missing(json.RawMessage(`[1]`)) {
		t.Error("Missing misreads the arguments")
	}
	m, _ = quote.Member("/args/data/delivery/milestone")
	if !slices.Equal(m.Rule.Allowed(), []string{"goods_ready", "handover", "arrival"}) {
		t.Errorf("milestone allows %v", m.Rule.Allowed())
	}
	m, _ = quote.Member("/args/data/currency_ref/kind")
	if !slices.Equal(m.Rule.Allowed(), []string{"currency"}) {
		t.Errorf("the currency reference's kind allows %v", m.Rule.Allowed())
	}
	rfq, _ := q01Schema(t, inqapi.CommandSaveRfq)
	for path, want := range map[string]string{"/args/id": "RfqID", "/args/lines/1/locators/0/quote": "Lines[1].Locators[0].Quote",
		"/args/Lines/0/SourceID": "Lines[0].SourceID", "/args/source_ids/0": "SourceIDs[0]", "/args/lines/2/uom": "Lines[2].UOM"} {
		if m, ok := rfq.Member(path); !ok || m.Path != want {
			t.Errorf("%s resolves to %q (%v), want %s", path, m.Path, ok, want)
		}
	}
	source, _ := q01Schema(t, evdapi.CommandRegisterSource)
	if m, ok := source.Member("/args/id"); !ok || m.Path != "SourceID" {
		t.Errorf("source.register /args/id resolves to %q", m.Path)
	}
	lookup, _ := q01Schema(t, ReadConfig)
	if m, ok := lookup.Member("/args/Kind"); !ok || len(m.Rule.Allowed()) != len(polapi.Kinds())-1 {
		t.Errorf("config.lookup Kind allows %v", m.Rule.Allowed())
	}
}

// A tag that is not a rule, or a rule its member cannot carry, is refused when the capability is
// defined: a typing mistake never becomes a silent "no rule".
func TestRuleTagsAreChecked(t *testing.T) {
	type typo struct {
		IdempotencyKey string
		ID             string `contract:"requird"`
	}
	type misplaced struct {
		IdempotencyKey string
		Count          int64 `contract:"enum=a|b"`
	}
	type alone struct {
		IdempotencyKey string
		ID             string `contract:"optional"`
	}
	for name, err := range map[string]error{"typo": define[typo](draftSpec("probe.save_draft")),
		"misplaced": define[misplaced](draftSpec("probe.save_draft")), "alone": define[alone](draftSpec("probe.save_draft"))} {
		if err == nil {
			t.Errorf("%s: defined", name)
		}
	}
}
