package purchaseinvoice

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// redOf is a red invoice reversing quantity of blue line 1 (120 PCS, 600.00 + 78.00 CNY) at 5.00 net and 0.65 tax each.
func redOf(number, quantity, net, tax string) Facts {
	f := blue()
	f.Colour, f.Number, f.RedOf = Red, number, &Ref{Kind: "special_vat", Number: "SYN0001"}
	f.Lines = []Line{{LineNo: 1, Description: "合成针织衫", Quantity: quantity, Unit: "PCS", NetAmount: net, TaxRate: "0.13", TaxAmount: tax}}
	f.NetTotal, f.TaxTotal = net, tax
	return f
}

func verifiedRed(id string, f Facts) *Invoice {
	return &Invoice{ID: id, Status: Verified, Verified: &f, RedOfID: "pin.blue"}
}

// S5R-D20 (SM-CNX-INVOICE verified → verified, CNX-INV-005; 公告 2024 年第 11 号 全额或部分红冲): a red invoice reverses part of a
// blue one line by line — at most what remains of the line's quantity and amounts, in its unit, its whole quantity
// only with its whole amounts — and the one that reverses what remains reverses the blue invoice (full).
func TestS5RPartialRedReversalD20(t *testing.T) {
	b := blue()
	original := &Invoice{ID: "pin.blue", Status: Verified, Verified: &b}
	first := redOf("SYN0002", "20", "100.00", "13.00")
	full, err := CheckRedLines(original, nil, first)
	if err != nil || full {
		t.Fatalf("20 of 120 is a partial reversal: %v %v", full, err)
	}
	reversed, _ := Reversals(b, []*Invoice{verifiedRed("pin.r1", first)}, nil)
	left := Remaining(b.Lines[0], reversed[1], kernel.MustCurrency("CNY", 2))
	if !left.Quantity.Equal(kernel.MustDecimal("100")) || !left.Net.Equal(kernel.MustDecimal("500")) || !left.Tax.Equal(kernel.MustDecimal("65")) {
		t.Fatalf("remaining %+v", left)
	}
	for _, x := range []struct {
		name string
		red  Facts
		want string
	}{
		{"more than remains", redOf("SYN0003", "101", "505.00", "65.65"), "INVALID_KNOWN_VALUE /args/facts/lines/0/quantity"},
		{"more net than remains", redOf("SYN0003", "10", "500.01", "0.00"), "INVALID_KNOWN_VALUE /args/facts/lines/0/net_amount"},
		{"the whole quantity without the whole amount", redOf("SYN0003", "100", "499.00", "65.00"), "INVALID_KNOWN_VALUE /args/facts/lines/0/net_amount"},
		{"the whole amounts without the whole quantity", redOf("SYN0003", "99", "500.00", "65.00"), "INVALID_KNOWN_VALUE /args/facts/lines/0/quantity"},
		{"a line the blue invoice lacks", func() Facts { f := redOf("SYN0003", "1", "5.00", "0.65"); f.Lines[0].LineNo = 2; return f }(),
			"INVALID_KNOWN_VALUE /args/facts/lines/0/line_no"},
		{"another unit", func() Facts { f := redOf("SYN0003", "1", "5.00", "0.65"); f.Lines[0].Unit = "KGM"; return f }(),
			"INVALID_KNOWN_VALUE /args/facts/lines/0/unit"},
	} {
		if _, err := CheckRedLines(original, reversed, x.red); code(err) != x.want {
			t.Errorf("%s: %s, want %s", x.name, code(err), x.want)
		}
	}
	rest := redOf("SYN0003", "100", "500.00", "65.00")
	if full, err := CheckRedLines(original, reversed, rest); err != nil || !full {
		t.Fatalf("what remains reverses the blue invoice: %v %v", full, err)
	}
}

// S5R-D20 and CN-R06: a partial red invoice takes back what it reverses of a blue match (at most what remains of the
// match, together at most the red line's quantity), and after it every touched line matches no more than it evidences.
func TestS5RTakeBacksD20(t *testing.T) {
	b := blue()
	original := &Invoice{ID: "pin.blue", Status: Verified, Verified: &b}
	l1 := LineRef{CommitmentID: "commitment.1", OrderLineID: "L1"}
	matches := []Match{{InvoiceID: "pin.blue", LineNo: 1, Ref: l1, Quantity: kernel.MustDecimal("120"), Unit: "PCS", SupplierID: "supplier.1",
		PONumber: "PO-1", PurchaseQuantity: kernel.MustDecimal("120")}}
	red := redOf("SYN0002", "20", "100.00", "13.00")
	at := time.Now()
	if _, err := CheckTakeBacks(original, nil, matches, nil, red, "pin.r1", nil, false, at); code(err) != "INVALID_KNOWN_VALUE /args/matches" {
		t.Fatalf("120 matched of 100 evidenced: %s", code(err))
	}
	for _, x := range []struct {
		in   []MatchInput
		want string
	}{
		{[]MatchInput{{LineNo: 1, Ref: LineRef{CommitmentID: "commitment.9", OrderLineID: "L1"}, Quantity: kernel.MustDecimal("20")}},
			"REFERENCE_INVALID /args/matches/0/commitment_id"},
		{[]MatchInput{{LineNo: 1, Ref: l1, Quantity: kernel.MustDecimal("21")}}, "INVALID_KNOWN_VALUE /args/matches/0/quantity"},
		{[]MatchInput{{LineNo: 1, Ref: l1, Quantity: kernel.MustDecimal("10")}}, "INVALID_KNOWN_VALUE /args/matches"},
	} {
		if _, err := CheckTakeBacks(original, nil, matches, nil, red, "pin.r1", x.in, false, at); code(err) != x.want {
			t.Errorf("%+v: %s, want %s", x.in, code(err), x.want)
		}
	}
	tb, err := CheckTakeBacks(original, nil, matches, nil, red, "pin.r1", []MatchInput{{LineNo: 1, Ref: l1, Quantity: kernel.MustDecimal("20")}}, false, at)
	if err != nil || len(tb) != 1 || tb[0].InvoiceID != "pin.r1" || tb[0].PONumber != "PO-1" || !tb[0].Quantity.Equal(kernel.MustDecimal("20")) {
		t.Fatalf("%+v %v", tb, err)
	}
	_, taken := Reversals(b, nil, tb)
	if !taken[matches[0].Key()].Equal(kernel.MustDecimal("20")) || matches[0].Status(Verified, Blue, taken[matches[0].Key()]) != Review {
		t.Fatalf("taken back %+v", taken)
	}
	if _, err := CheckTakeBacks(original, nil, matches, taken, redOf("SYN0003", "100", "500.00", "65.00"), "pin.r2",
		[]MatchInput{{LineNo: 1, Ref: l1, Quantity: kernel.MustDecimal("101")}}, true, at); code(err) != "INVALID_KNOWN_VALUE /args/matches/0/quantity" {
		t.Fatalf("100 remain of the match: %s", code(err))
	}
	if out, err := CheckTakeBacks(original, nil, matches, taken, redOf("SYN0003", "100", "500.00", "65.00"), "pin.r2", nil, true, at); err != nil ||
		len(out) != 0 {
		t.Fatalf("a full reversal puts every match under review without naming them: %v", err)
	}
}
