package api

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// snake is the transport's derivation of a JSON member name from a Go field name
// (internal/transport/httpapi/reflectjson.go): LegalName → legal_name, EvidenceIDs → evidence_ids.
func snake(name string) string {
	r := []rune(name)
	var b strings.Builder
	for i, c := range r {
		if unicode.IsUpper(c) && i > 0 {
			prevLower := unicode.IsLower(r[i-1]) || unicode.IsDigit(r[i-1])
			nextLower := i+1 < len(r) && unicode.IsLower(r[i+1])
			plural := nextLower && r[i+1] == 's' && (i+2 == len(r) || unicode.IsUpper(r[i+2]))
			if prevLower || (unicode.IsUpper(r[i-1]) && nextLower && !plural) {
				b.WriteByte('_')
			}
		}
		b.WriteRune(unicode.ToLower(c))
	}
	return b.String()
}

// SUP stores its content as JSON with the names transports derive, so a stored document and an
// HTTP request carry the same members: every tag is the snake_case of its field (a ConfigRef's
// ConfigID is "id" there too).
func TestContentJSONNamesAreTheTransportsNames(t *testing.T) {
	seen := map[reflect.Type]bool{}
	var walk func(reflect.Type)
	walk = func(typ reflect.Type) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct || seen[typ] {
			return
		}
		seen[typ] = true
		for f := range typ.Fields() {
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			want := snake(f.Name)
			if f.Name == "ConfigID" {
				want = "id"
			}
			if name != want {
				t.Errorf("%s.%s is %q in JSON, the transport names it %q", typ.Name(), f.Name, name, want)
			}
			walk(f.Type)
		}
	}
	for _, v := range []any{SupplierRfq{}, SupplierOffer{}, PurchaseTerms{}, LogisticsOffer{}} {
		walk(reflect.TypeOf(v))
	}
	if len(seen) < 20 {
		t.Fatalf("walked %d types", len(seen))
	}
}

func schemaAt(t *testing.T, err error, code, path string) {
	t.Helper()
	r, ok := kernel.AsRejection(err)
	if !ok || r.Code != code || r.Fields[0].Path != path {
		t.Fatalf("got %v, want %s at %s", err, code, path)
	}
}

// What a draft states must be well formed; completeness is judged when it is sealed.
func TestDraftValidation(t *testing.T) {
	unit := &ConfigRef{Kind: ConfigUnit, ConfigID: "PCS", Revision: 1}
	good := SupplierOffer{Origin: &Origin{Kind: OriginUnsolicited}, ValidFrom: "2026-10-22T00:00:00Z", ValidUntil: "2026-10-29T00:00:00Z",
		Lines: []ConditionLine{{ID: "OL1", PurchaseUnitRef: unit, PricingMode: "volume_discount", Tiers: []Tier{{LowerInclusive: "0", UnitPrice: "5"}},
			MOQ: &MOQ{Quantity: "100", UnitRef: unit, Scope: MOQScopeSKU},
			Delivery: &Delivery{Kind: DeliveryLeadTime, AnchorEvent: AnchorOrderConfirmed, Days: 30, DayBasis: DayBasisWorking,
				CalendarRef: &ConfigRef{Kind: ConfigCalendar, ConfigID: "cn", Revision: 1}, Milestone: MilestoneGoodsReady, Place: "CNSHA",
				TimeZone: "Asia/Shanghai"}}}}
	if err := (SaveSupplierOffer{IdempotencyKey: "k1", OfferID: "offer.1", Data: good}).Validate(); err != nil {
		t.Fatalf("an unsupported pricing mode is judged at verification: %v", err)
	}
	if err := (SaveSupplierOffer{IdempotencyKey: "k1", OfferID: "offer.1"}).Validate(); err != nil {
		t.Fatalf("an empty draft: %v", err)
	}
	for _, c := range []struct {
		name       string
		edit       func(o *SupplierOffer)
		code, path string
	}{
		{"origin naming no request", func(o *SupplierOffer) { o.Origin.Kind = OriginInResponseToRfq }, kernel.CodeSchemaInvalid, "/args/data/origin/supplier_rfq"},
		{"unsolicited naming a request", func(o *SupplierOffer) { o.Origin.SupplierRfq = &SupplierRfqRef{SupplierRfqID: "r", RowVersion: 2} },
			kernel.CodeSchemaInvalid, "/args/data/origin/supplier_rfq"},
		{"validity ending before it starts", func(o *SupplierOffer) { o.ValidUntil = "2026-10-21T00:00:00Z" }, kernel.CodeInvalidKnownValue, "/args/data/valid_until"},
		{"local time", func(o *SupplierOffer) { o.ValidFrom = "2026-10-22T08:00:00+08:00" }, kernel.CodeSchemaInvalid, "/args/data/valid_from"},
		{"float price", func(o *SupplierOffer) { o.Lines[0].Tiers[0].UnitPrice = "5e0" }, kernel.CodeSchemaInvalid, "/args/data/lines/0/tiers/0/unit_price"},
		{"nine price digits", func(o *SupplierOffer) { o.Lines[0].Tiers[0].UnitPrice = "0.000000001" }, kernel.CodeSchemaInvalid, "/args/data/lines/0/tiers/0/unit_price"},
		{"negative bound", func(o *SupplierOffer) { o.Lines[0].Tiers[0].LowerInclusive = "-1" }, kernel.CodeSchemaInvalid, "/args/data/lines/0/tiers/0/lower_inclusive"},
		{"tier without price", func(o *SupplierOffer) { o.Lines[0].Tiers[0].UnitPrice = "" }, kernel.CodeSchemaInvalid, "/args/data/lines/0/tiers/0/unit_price"},
		{"unit of another kind", func(o *SupplierOffer) {
			o.Lines[0].PurchaseUnitRef = &ConfigRef{Kind: ConfigCurrency, ConfigID: "USD", Revision: 1}
		},
			kernel.CodeReferenceInvalid, "/args/data/lines/0/purchase_unit_ref/kind"},
		{"MOQ per SKU naming a group", func(o *SupplierOffer) { o.Lines[0].MOQ.GroupID = "G1" }, kernel.CodeSchemaInvalid, "/args/data/lines/0/moq/scope"},
		{"zone instead of a place", func(o *SupplierOffer) { o.Lines[0].Delivery.Place = "EU" }, kernel.CodeSchemaInvalid, "/args/data/lines/0/delivery/place"},
		{"lead time without a calendar", func(o *SupplierOffer) { o.Lines[0].Delivery.CalendarRef = nil }, kernel.CodeSchemaInvalid,
			"/args/data/lines/0/delivery/calendar_ref"},
		{"fixed date with a lead time", func(o *SupplierOffer) {
			o.Lines[0].Delivery.Kind, o.Lines[0].Delivery.Date = DeliveryFixed, "2026-11-31"
		},
			kernel.CodeSchemaInvalid, "/args/data/lines/0/delivery/date"},
		{"mapping without the supplier's code", func(o *SupplierOffer) { o.Lines[0].ItemCodeMapping = &ItemCodeMappingRef{MappingID: "m", Revision: 1} },
			kernel.CodeSchemaInvalid, "/args/data/lines/0/supplier_item_code"},
		{"padded item code", func(o *SupplierOffer) { o.Lines[0].SupplierItemCode = " A-1" }, kernel.CodeSchemaInvalid, "/args/data/lines/0/supplier_item_code"},
	} {
		t.Run(c.name, func(t *testing.T) {
			o := good
			o.Origin = &Origin{Kind: OriginUnsolicited}
			l := good.Lines[0]
			moq, d := *l.MOQ, *l.Delivery
			l.MOQ, l.Delivery, l.Tiers = &moq, &d, []Tier{l.Tiers[0]}
			o.Lines = []ConditionLine{l}
			c.edit(&o)
			schemaAt(t, SaveSupplierOffer{IdempotencyKey: "k1", OfferID: "offer.1", Data: o}.Validate(), c.code, c.path)
		})
	}

	fee := Fee{ID: "F1", Billing: &Billing{Kind: BillingFixed, Amount: "60"}, Chargeable: &Chargeable{Kind: ChargeableFixed},
		Allocation: &Allocation{Basis: AllocationExplicit, Weights: []ExplicitWeight{{LineID: "L1", Weight: "1000000000"}}}}
	l := LogisticsOffer{Origin: "CNSHA", Destination: "DEHAM", Mode: ModeSea, Fees: []Fee{fee}}
	if err := (SaveLogisticsOffer{IdempotencyKey: "k1", OfferID: "log.1", Data: l}).Validate(); err != nil {
		t.Fatal(err)
	}
	l.Fees[0].Allocation = &Allocation{Basis: AllocationExplicit, TargetLineIDs: []string{"L1"}}
	schemaAt(t, SaveLogisticsOffer{IdempotencyKey: "k1", OfferID: "log.1", Data: l}.Validate(), kernel.CodeSchemaInvalid, "/args/data/fees/0/allocation/basis")
	l.Fees[0].Allocation = &Allocation{Basis: AllocationExplicit, Weights: []ExplicitWeight{{LineID: "L1", Weight: "1.5"}}}
	schemaAt(t, SaveLogisticsOffer{IdempotencyKey: "k1", OfferID: "log.1", Data: l}.Validate(), kernel.CodeSchemaInvalid, "/args/data/fees/0/allocation/weights/0/weight")
	l.Fees[0].Allocation, l.Mode = fee.Allocation, "pipeline"
	schemaAt(t, SaveLogisticsOffer{IdempotencyKey: "k1", OfferID: "log.1", Data: l}.Validate(), kernel.CodeSchemaInvalid, "/args/data/mode")

	terms := PurchaseTerms{SourceOfferRef: &SupplyConditionRef{Kind: KindLogisticsOffer, ID: "log.1", Revision: 1}}
	schemaAt(t, SavePurchaseTerms{IdempotencyKey: "k1", TermsID: "t", Data: terms}.Validate(), kernel.CodeReferenceInvalid, "/args/data/source_offer_ref/kind")
}

func TestCommandValidation(t *testing.T) {
	ev := []Evidence{{SourceID: "s", Locator: "p.1"}}
	for _, c := range []struct {
		err  error
		path string
	}{
		{Seal{IdempotencyKey: "k", Kind: "offer", ID: "o", ExpectedVersion: 1, WorkingRevision: 1, Evidence: ev}.Validate(), "/args/kind"},
		{Seal{IdempotencyKey: "k", Kind: KindSupplierOffer, ID: "o", ExpectedVersion: 1, WorkingRevision: 1}.Validate(), "/args/evidence"},
		{Seal{IdempotencyKey: "k", Kind: KindSupplierOffer, ID: "o", ExpectedVersion: 1, WorkingRevision: 1, Evidence: append(ev, ev[0])}.Validate(),
			"/args/evidence/1"},
		{Seal{IdempotencyKey: "k", Kind: KindSupplierOffer, ID: "o", ExpectedVersion: 0, WorkingRevision: 1, Evidence: ev}.Validate(), "/args/expected_version"},
		{End{IdempotencyKey: "k", Kind: KindPurchaseTerms, ID: "t", ExpectedVersion: 1, TargetRevision: 1, Evidence: ev}.Validate(), "/args/reason"},
		{Revise{IdempotencyKey: "k", Kind: KindLogisticsOffer, ID: "l", ExpectedVersion: 1, Reason: "r"}.Validate(), "/args/base_revision"},
		{DiscardDraft{IdempotencyKey: "k", Kind: KindLogisticsOffer, ID: "l", ExpectedVersion: 1, WorkingRevision: 1}.Validate(), "/args/reason"},
		{RecordSupplierRfqIssued{IdempotencyKey: "k", SupplierRfqID: "r", ExpectedVersion: 1, Reason: "sent"}.Validate(), "/args/evidence"},
		{CloseSupplierRfq{IdempotencyKey: "k", SupplierRfqID: "r", ExpectedVersion: 1, Evidence: ev}.Validate(), "/args/reason"},
	} {
		schemaAt(t, c.err, kernel.CodeSchemaInvalid, c.path)
	}
	if SealAction(KindPurchaseTerms) != ActionPublish || EndAction(KindPurchaseTerms) != ActionRetire || SealAction(KindSupplierOffer) != ActionVerify ||
		Command(KindLogisticsOffer, EndAction(KindLogisticsOffer)) != CommandWithdrawLogisticsOffer {
		t.Fatal("command names")
	}
	schemaAt(t, ValidateRefs([]SupplyConditionRef{{Kind: KindSupplierOffer, ID: "o", Revision: 1}, {Kind: KindSupplierOffer, ID: "o", Revision: 1}}),
		kernel.CodeSchemaInvalid, "/refs")
}

func TestGuardVerdict(t *testing.T) {
	ref := SupplyConditionRef{Kind: KindSupplierOffer, ID: "offer.1", Revision: 1}
	if args := SupplyConditionGuardArgs(ref, time.Date(2026, 10, 22, 9, 0, 0, 123456789, time.FixedZone("CST", 8*3600))); args[3] !=
		time.Date(2026, 10, 22, 1, 0, 0, 123456000, time.UTC) || args[0] != KindSupplierOffer {
		t.Fatalf("args %v", args)
	}
	scan := func(known bool, status, code, hash *string) func(dest ...any) error {
		return func(dest ...any) error {
			*dest[0].(*bool) = known
			*dest[1].(**string), *dest[2].(**string), *dest[3].(**string) = status, code, hash
			return nil
		}
	}
	str := func(s string) *string { return &s }
	v, err := ScanSupplyConditionGuard(ref, scan(true, str(StatusActive), nil, str("h")))
	if err != nil || !v.InForce() || v.Check("/x") != nil || v.ContentHash != "h" {
		t.Fatalf("%+v %v", v, err)
	}
	v, _ = ScanSupplyConditionGuard(ref, scan(true, str(StatusWithdrawn), str(CodeSupplyConditionNotCurrent), str("h")))
	if r, ok := kernel.AsRejection(v.Check("/x")); v.InForce() || !ok || r.Code != CodeSupplyConditionNotCurrent || r.Category != kernel.CategoryConflict {
		t.Fatalf("%+v", v)
	}
	v, _ = ScanSupplyConditionGuard(ref, scan(false, nil, str(kernel.CodeReferenceInvalid), nil))
	if r, ok := kernel.AsRejection(v.Check("/x")); !ok || r.Code != kernel.CodeReferenceInvalid {
		t.Fatalf("%+v", v)
	}
	if _, err = ScanSupplyConditionGuard(ref, func(...any) error { return errors.New("no row") }); err == nil {
		t.Fatal("a scan error is returned")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("an undeclared code panics")
		}
	}()
	_ = Reject("CONFIG_NOT_ACTIVE", "/x")
}
