package declaration

import (
	"errors"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func verdict(err error) string {
	if err == nil {
		return "OK"
	}
	if rej, ok := kernel.AsRejection(err); ok {
		return rej.Code + " " + rej.Fields[0].Path
	}
	return err.Error()
}

var usd = kernel.MustCurrency("USD", 2)

func money(minor int64) kernel.Money {
	m, _ := kernel.NewMoney(usd, minor)
	return m
}

func tshirt(revision int64, perKg string) Profile {
	return Profile{ProfileID: "decl.sku.1", Revision: revision, ContentHash: "h1", Verified: true, CommodityCode: "6109100021",
		DeclarationName: "棉制针织T恤衫", Elements: []Element{{Name: "品名", Value: "T恤衫"}},
		LegalUnits: []LegalUnit{{Unit: "件", PerBaseUnit: kernel.MustDecimal("1"), Basis: "一件即一 PCS"},
			{Unit: "千克", PerBaseUnit: kernel.MustDecimal(perKg), Basis: "实测净重"}}}
}

// A lot of two shipments' lines of two sales lines and a second SKU (CN-AT02 "同次出口包含多销售行").
func lot() []LotLine {
	return []LotLine{
		{LineID: "E1", ShipmentID: "ship.1", OrderLineID: "L1", SkuID: "sku.1", SpecRevision: 1, Quantity: kernel.MustDecimal("30"), Unit: BaseUnit, Amount: money(24000)},
		{LineID: "E2", ShipmentID: "ship.1", OrderLineID: "L2", SkuID: "sku.1", SpecRevision: 1, Quantity: kernel.MustDecimal("20"), Unit: BaseUnit, Amount: money(16000)},
		{LineID: "E3", ShipmentID: "ship.1", OrderLineID: "L3", SkuID: "sku.2", SpecRevision: 1, Quantity: kernel.MustDecimal("5"), Unit: "DOZEN", Amount: money(9000)},
	}
}

func profiles() map[string]Profile {
	p2 := tshirt(3, "0.3")
	p2.ProfileID, p2.CommodityCode, p2.DeclarationName = "decl.sku.2", "6110200045", "棉制针织套头衫"
	return map[string]Profile{"E1": tshirt(1, "0.25"), "E2": tshirt(1, "0.25"), "E3": p2}
}

func dozen() *Conversion {
	return &Conversion{ConfigID: "dozen.pcs", Revision: 2, ContentHash: "c", From: "DOZEN", To: BaseUnit, Factor: kernel.MustDecimal("12")}
}

// CN-AT02 and CNX-INV-006: two sales lines of one classification are one item, the other SKU's
// classification another; base and legal quantities follow the verified profiles' conversions
// exactly, the values sum; a POL unit_conversion converts a line in dozens (受控换算).
func TestItemsMapMultiLineLotsCNAT02(t *testing.T) {
	items, mapping, err := Prepare(PrepareInput{Lines: lot(), Profiles: profiles(), Items: []ItemInput{
		{ItemNo: 1, Lines: []LineInput{{LineID: "E1"}, {LineID: "E2"}}},
		{ItemNo: 2, Lines: []LineInput{{LineID: "E3", Conversion: dozen()}}}}})
	if err != nil || mapping != MappingVerified || len(items) != 2 {
		t.Fatalf("%v %s %+v", err, mapping, items)
	}
	one, two := items[0], items[1]
	if one.CommodityCode != "6109100021" || one.BaseQuantity.String() != "50" || one.LegalQuantities[0].Quantity.String() != "50" ||
		one.LegalQuantities[1].Quantity.String() != "12.5" || one.Value.String() != "400.00 USD" || len(one.Lines) != 2 {
		t.Fatalf("item 1 %+v", one)
	}
	if l := two.Lines[0]; two.BaseQuantity.String() != "60" || two.LegalQuantities[1].Quantity.String() != "18" ||
		l.Conversion.Source != ConversionUnitConversion || l.Conversion.ConfigID != "dozen.pcs" || l.ProfileRevision != 3 {
		t.Fatalf("item 2 %+v", two)
	}
	docs := DocItems(items)
	if docs[0].Lines[1].OrderLineID != "L2" || docs[0].LegalQuantities[1].Quantity != "12.5" || docs[1].Lines[0].Conversion.Factor != "12" ||
		docs[1].Lines[0].LegalQuantities[1].PerBaseUnit != "0.3" {
		t.Fatalf("documents %+v", docs)
	}
}

// CNX-INV-006 "不同编码或申报要素不得合并", every lot line declared exactly once, an unverified
// profile refused (DECLARATION_PROFILE_UNVERIFIED), conversions that do not fit refused.
func TestPrepareRefusals(t *testing.T) {
	unverified := profiles()
	unverified["E3"] = Profile{ProfileID: "decl.sku.2", Revision: 1}
	for _, c := range []struct {
		profiles map[string]Profile
		items    []ItemInput
		want     string
	}{
		{profiles(), []ItemInput{{ItemNo: 1, Lines: []LineInput{{LineID: "E1"}, {LineID: "E3", Conversion: dozen()}}}, {ItemNo: 2, Lines: []LineInput{{LineID: "E2"}}}},
			"INVALID_KNOWN_VALUE /args/items/0/lines/1/line_id"},
		{profiles(), []ItemInput{{ItemNo: 1, Lines: []LineInput{{LineID: "E1"}, {LineID: "E2"}}}}, "INVALID_KNOWN_VALUE /args/items"},
		{profiles(), []ItemInput{{ItemNo: 1, Lines: []LineInput{{LineID: "E9"}}}}, "REFERENCE_INVALID /args/items/0/lines/0/line_id"},
		{unverified, []ItemInput{{ItemNo: 1, Lines: []LineInput{{LineID: "E3"}}}}, "DECLARATION_PROFILE_UNVERIFIED /args/items/0/lines/0/line_id"},
		{profiles(), []ItemInput{{ItemNo: 1, Lines: []LineInput{{LineID: "E1", Conversion: dozen()}}}}, "INVALID_KNOWN_VALUE /args/items/0/lines/0/unit_conversion"},
		{profiles(), []ItemInput{{ItemNo: 1, Lines: []LineInput{{LineID: "E3", Conversion: &Conversion{From: "DOZEN", To: "KG", Factor: kernel.MustDecimal("1")}}}}},
			"INVALID_KNOWN_VALUE /args/items/0/lines/0/unit_conversion"},
		{profiles(), []ItemInput{{ItemNo: 1, Lines: []LineInput{{LineID: "E3", Err: kernel.RejectAs("CONFIG_NOT_ACTIVE", kernel.CategoryInvalid, "/conversion", "")}}}},
			"CONFIG_NOT_ACTIVE /args/items/0/lines/0/unit_conversion"},
	} {
		if _, _, err := Prepare(PrepareInput{Lines: lot(), Profiles: c.profiles, Items: c.items}); verdict(err) != c.want {
			t.Errorf("%+v: want %s, got %s", c.items, c.want, verdict(err))
		}
	}
}

// CN-AT03 (票据品名或单位与报关资料不同): a line in dozens without a controlled conversion is mapped
// but unverified — no base or legal quantity is guessed — and its confirmation is refused with
// DECLARATION_MAPPING_UNVERIFIED; with the conversion it is verified and confirmable. A version that
// is not the current one is REFERENCE_STALE; only a draft is confirmed.
func TestUnverifiedMappingBlocksConfirmationCNAT03(t *testing.T) {
	items, mapping, err := Prepare(PrepareInput{Lines: lot()[2:], Profiles: profiles(), Items: []ItemInput{{ItemNo: 1, Lines: []LineInput{{LineID: "E3"}}}}})
	if err != nil || mapping != MappingUnverified || items[0].Mapping != MappingUnverified || items[0].LegalQuantities != nil ||
		items[0].Lines[0].Reason != ReasonUnitNotConverted || items[0].Lines[0].Conversion.Source != ConversionNone {
		t.Fatalf("%v %s %+v", err, mapping, items)
	}
	if d := DocItems(items)[0]; d.BaseQuantity != "" || d.Lines[0].BaseQuantity != "" || len(d.LegalQuantities) != 0 || d.Lines[0].Conversion.Factor != "" {
		t.Fatalf("an unverified line has no quantities: %+v", d)
	}
	c := &Case{Status: Draft, Version: 1}
	if got := verdict(CheckConfirm(c, 1, items)); got != "DECLARATION_MAPPING_UNVERIFIED /args/version" {
		t.Fatal(got)
	}
	converted, _, _ := Prepare(PrepareInput{Lines: lot()[2:], Profiles: profiles(), Items: []ItemInput{{ItemNo: 1, Lines: []LineInput{{LineID: "E3", Conversion: dozen()}}}}})
	c.Version = 2
	for _, x := range []struct {
		c       *Case
		version int
		want    string
	}{{c, 2, "OK"}, {c, 1, "REFERENCE_STALE /args/version"}, {&Case{Status: Confirmed, Version: 2}, 2, "INVALID_STATE /args/declaration_id"}} {
		if got := verdict(CheckConfirm(x.c, x.version, converted)); got != x.want {
			t.Errorf("want %s, got %s", x.want, got)
		}
	}
}

func receipt(outcome string, sequence int64) ReceiptInput {
	now := time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	return ReceiptInput{Outcome: outcome, Sequence: sequence, DeclarationNumber: "SYN-1", ReceivedAt: now, At: now, AmendedItems: map[int]bool{}}
}

// CN-AT06, CN-R05, CNX-INV-003: receipts arrive out of order — the release (sequence 3) before the
// acceptance (2): the case moves forward to released, the late acceptance and a repeated release
// are DECLARATION_RECEIPT_STALE and change nothing; an amendment restates the export date, a
// further amendment follows it, a withdrawal clears the export date and is final; nothing goes back.
func TestReceiptsOutOfOrderCNAT06(t *testing.T) {
	c := &Case{Status: Submitted}
	items := []Item{{ItemNo: 1, Mapping: MappingVerified}}
	release := receipt(Released, 3)
	release.ExportedAt = release.ReceivedAt.Add(-48 * time.Hour)
	if err := CheckReceipt(c, items, release); err != nil {
		t.Fatal(err)
	}
	c.Apply(release)
	if c.Status != Released || c.LastSequence != 3 || !Exported(c.Status) || !c.ExportedAt.Equal(release.ExportedAt) || c.DeclarationNumber != "SYN-1" {
		t.Fatalf("released %+v", c)
	}
	for _, x := range []struct {
		in   ReceiptInput
		want string
	}{
		{receipt(Accepted, 2), "DECLARATION_RECEIPT_STALE /args/external/sequence"},
		{receipt(Released, 3), "DECLARATION_RECEIPT_STALE /args/external/sequence"},
		{receipt(Received, 4), "INVALID_STATE /args/outcome"},
		{receipt(Unknown, 0), "INVALID_STATE /args/outcome"},
		{func() ReceiptInput { r := receipt(Amended, 4); r.DeclarationNumber = "SYN-2"; return r }(), "INVALID_KNOWN_VALUE /args/declaration_number"},
		{func() ReceiptInput { r := receipt(Amended, 4); r.AmendedItems[2] = true; return r }(), "INVALID_KNOWN_VALUE /args/amended_items"},
		{func() ReceiptInput { r := receipt(Amended, 4); r.ExportedAt = r.ReceivedAt.Add(time.Hour); return r }(), "INVALID_KNOWN_VALUE /args/exported_at"},
	} {
		if got := verdict(CheckReceipt(c, items, x.in)); got != x.want {
			t.Errorf("%s %d: want %s, got %s", x.in.Outcome, x.in.Sequence, x.want, got)
		}
	}
	amended := receipt(Amended, 5)
	amended.AmendedItems[1] = true
	amended.ExportedAt = release.ExportedAt.Add(24 * time.Hour)
	if err := CheckReceipt(c, items, amended); err != nil {
		t.Fatal(err)
	}
	c.Apply(amended)
	again := receipt(Amended, 6)
	if c.Status != Amended || !c.ExportedAt.Equal(amended.ExportedAt) || verdict(CheckReceipt(c, items, again)) != "OK" {
		t.Fatalf("amended %+v", c)
	}
	withdrawn := receipt(Withdrawn, 7)
	if err := CheckReceipt(c, items, withdrawn); err != nil {
		t.Fatal(err)
	}
	c.Apply(withdrawn)
	if c.Status != Withdrawn || !c.ExportedAt.IsZero() || Exported(c.Status) || !Terminal(c.Status) {
		t.Fatalf("withdrawn %+v", c)
	}
	if got := verdict(CheckReceipt(c, items, receipt(Released, 8))); got != "INVALID_STATE /args/outcome" {
		t.Fatalf("nothing follows a withdrawal: %s", got)
	}
}

// CN-AT13, CNX-INV-003 "unknown 先查证再决定是否重新提交": after a timeout recorded as unknown, nothing
// is submitted again (EXTERNAL_RESULT_UNKNOWN); the verified result moves the case on; nothing is
// received before a submission; unknown carries no external sequence.
func TestUnknownIsVerifiedBeforeResubmissionCNAT13(t *testing.T) {
	if got := verdict(CheckSubmission(&Case{Status: Draft})); got != "INVALID_STATE /args/declaration_id" {
		t.Fatal(got)
	}
	c := &Case{Status: Confirmed}
	if err := CheckSubmission(c); err != nil {
		t.Fatal(err)
	}
	if got := verdict(CheckReceipt(c, nil, receipt(Received, 1))); got != "INVALID_STATE /args/declaration_id" {
		t.Fatalf("nothing received before a submission: %s", got)
	}
	c.Status = Submitted
	unknown := receipt(Unknown, 0)
	if err := CheckReceipt(c, nil, unknown); err != nil {
		t.Fatal(err)
	}
	c.Apply(unknown)
	if c.Status != Unknown || c.LastSequence != 0 || c.Receipts != 1 {
		t.Fatalf("unknown %+v", c)
	}
	if got := verdict(CheckSubmission(c)); got != "EXTERNAL_RESULT_UNKNOWN /args/declaration_id" {
		t.Fatal(got)
	}
	if got := verdict(CheckReceipt(c, nil, receipt(Unknown, 0))); got != "INVALID_STATE /args/outcome" {
		t.Fatalf("unknown twice: %s", got)
	}
	verified := receipt(Accepted, 2)
	if err := CheckReceipt(c, nil, verified); err != nil {
		t.Fatal(err)
	}
	c.Apply(verified)
	if c.Status != Accepted || c.LastSequence != 2 || verdict(CheckSubmission(c)) != "INVALID_STATE /args/declaration_id" {
		t.Fatalf("verified %+v", c)
	}
}

// A version document is stored with its SHA-256 and read back only when it matches; an amendment
// records the items' base quantities before and after, as customs stated them.
func TestVersionDocument(t *testing.T) {
	items, _, err := Prepare(PrepareInput{Lines: lot()[:2], Profiles: profiles(), Items: []ItemInput{{ItemNo: 1, Lines: []LineInput{{LineID: "E1"}, {LineID: "E2"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	v, err := NewVersion(Document{DeclarationID: "dcl-1", Version: 1, Kind: VersionPrepared, Items: DocItems(items), Mapping: MappingVerified})
	if err != nil || v.Contract != DocumentContract || len(v.SHA256) != 64 {
		t.Fatalf("%v %+v", err, v)
	}
	back, err := ParseVersion(v.Bytes, v.SHA256)
	if err != nil || back.Items[0].BaseQuantity != "50" || back.Items[0].ValueMinor != "40000" || back.Items[0].Currency != "USD" {
		t.Fatalf("%v %+v", err, back)
	}
	if _, err := ParseVersion(append([]byte(" "), v.Bytes...), v.SHA256); !errors.Is(err, ErrDocumentNotIntact) {
		t.Fatalf("a changed document: %v", err)
	}
	amended, changes := Amend(back.Items, map[int]string{1: "45"})
	if amended[0].BaseQuantity != "45" || len(changes) != 1 || changes[0] != (DocAmendment{ItemNo: 1, Before: "50", After: "45"}) {
		t.Fatalf("%+v %+v", amended, changes)
	}
	if got := Items(back.Document); len(got) != 1 || got[0].Mapping != MappingVerified {
		t.Fatalf("%+v", got)
	}
}
