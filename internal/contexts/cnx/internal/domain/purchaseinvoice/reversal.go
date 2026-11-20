package purchaseinvoice

import (
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Partial red reversal (S5R-D20, SM-CNX-INVOICE, CNX-INV-005; 国家税务总局公告 2024 年第 11 号: 全额或部分开具红字数电发票).
//
// A red invoice reverses one verified blue invoice, wholly or in part. Its lines name the blue lines they reverse by
// their line numbers (a red line numbered 2 reverses blue line 2) and state the quantity and the net and tax amounts
// they reverse, in the blue line's unit. What every verified red invoice reversed of a blue line is no longer evidence:
// the line evidences its quantity less what was reversed, and the remainder may still be allocated to tax refund cases.
// Several red invoices may reverse one blue invoice; the one that reverses what remained of every line reverses it
// (its state becomes reversed), every other one leaves it verified. A line's whole quantity is reversed together with
// its whole amounts, and its whole amounts only with its whole quantity.
//
// A red invoice's matches take back what a match of the blue invoice matched to a purchase commitment line: the same
// line and commitment line, at most what the match matched less what earlier red invoices took back, together at most
// the red line's quantity. After a partial reversal every touched line still matches no more than it evidences: a red
// invoice that would leave more matched names the matches it takes back. Taken back quantities no longer count toward
// the commitment line, so a corrected invoice can match them again.

// LineReversal is what verified red invoices reversed of a blue invoice line: quantity, net and tax amounts.
type LineReversal struct {
	Quantity kernel.Decimal
	Net, Tax kernel.Decimal
}

// MatchKey names a match of an invoice: its line and the commitment line it matches.
type MatchKey struct {
	LineNo int
	Ref    LineRef
}

// Key returns the match's key.
func (m Match) Key() MatchKey { return MatchKey{LineNo: m.LineNo, Ref: m.Ref} }

func amountOf(c kernel.Currency, s string) kernel.Decimal {
	m, err := kernel.ParseAmount(c, s)
	if err != nil {
		return kernel.Decimal{}
	}
	return m.Decimal()
}

// Reversals sums what verified red invoices reds reversed of each line of blue facts f, by line number, and what they
// took back of each of its matches.
func Reversals(f Facts, reds []*Invoice, takeBacks []Match) (map[int]LineReversal, map[MatchKey]kernel.Decimal) {
	lines, matches := map[int]LineReversal{}, map[MatchKey]kernel.Decimal{}
	c, _ := f.Currency.Kernel()
	for _, r := range reds {
		if r.Status != Verified || r.Verified == nil {
			continue
		}
		for _, l := range r.Verified.Lines {
			q, _ := kernel.ParseDecimal(l.Quantity, kernel.QuantityFormat)
			x := lines[l.LineNo]
			x.Quantity, x.Net, x.Tax = x.Quantity.Add(q), x.Net.Add(amountOf(c, l.NetAmount)), x.Tax.Add(amountOf(c, l.TaxAmount))
			lines[l.LineNo] = x
		}
	}
	for _, m := range takeBacks {
		matches[m.Key()] = matches[m.Key()].Add(m.Quantity)
	}
	return lines, matches
}

// Remaining is what blue line l evidences now, reversed what red invoices reversed of it.
func Remaining(l Line, reversed LineReversal, c kernel.Currency) LineReversal {
	q, _ := kernel.ParseDecimal(l.Quantity, kernel.QuantityFormat)
	return LineReversal{Quantity: q.Sub(reversed.Quantity), Net: amountOf(c, l.NetAmount).Sub(reversed.Net), Tax: amountOf(c, l.TaxAmount).Sub(reversed.Tax)}
}

// CheckRedLines judges the lines of red facts against blue invoice original, of which earlier verified red invoices
// reversed reversed: each red line names a line of the blue invoice (INVALID_KNOWN_VALUE line_no), in its unit
// (INVALID_KNOWN_VALUE unit), reverses at most what remains of its quantity, net and tax amounts (INVALID_KNOWN_VALUE),
// and its whole quantity exactly with its whole amounts (INVALID_KNOWN_VALUE). Full reports whether nothing of the blue
// invoice remains after it: the red invoice reverses it.
func CheckRedLines(original *Invoice, reversed map[int]LineReversal, red Facts) (full bool, err error) {
	blue := original.Facts()
	c, err := blue.Currency.Kernel()
	if err != nil {
		return false, kernel.Reject(kernel.CodeReferenceInvalid, "/args/facts/red_of", "")
	}
	after := map[int]LineReversal{}
	for k, v := range reversed {
		after[k] = v
	}
	for i, rl := range red.Lines {
		p := "/args/facts/lines/" + strconv.Itoa(i)
		bl, ok := blue.Line(rl.LineNo)
		if !ok {
			return false, kernel.Reject(kernel.CodeInvalidKnownValue, p+"/line_no", "The blue invoice has no line "+strconv.Itoa(rl.LineNo)+
				": a red line names the blue line it reverses by its number")
		}
		if bl.Unit != rl.Unit {
			return false, kernel.Reject(kernel.CodeInvalidKnownValue, p+"/unit", "Blue line "+strconv.Itoa(rl.LineNo)+" is in "+bl.Unit)
		}
		left := Remaining(bl, reversed[rl.LineNo], c)
		q, _ := kernel.ParseDecimal(rl.Quantity, kernel.QuantityFormat)
		net, tax := amountOf(c, rl.NetAmount), amountOf(c, rl.TaxAmount)
		switch {
		case q.Cmp(left.Quantity) > 0:
			return false, kernel.Reject(kernel.CodeInvalidKnownValue, p+"/quantity", "Blue line "+strconv.Itoa(rl.LineNo)+" has "+left.Quantity.String()+" left")
		case net.Cmp(left.Net) > 0:
			return false, kernel.Reject(kernel.CodeInvalidKnownValue, p+"/net_amount", "Blue line "+strconv.Itoa(rl.LineNo)+" has "+left.Net.String()+" net left")
		case tax.Cmp(left.Tax) > 0:
			return false, kernel.Reject(kernel.CodeInvalidKnownValue, p+"/tax_amount", "Blue line "+strconv.Itoa(rl.LineNo)+" has "+left.Tax.String()+" tax left")
		}
		wholeQ, wholeAmounts := q.Equal(left.Quantity), net.Equal(left.Net) && tax.Equal(left.Tax)
		if wholeQ && !wholeAmounts {
			return false, kernel.Reject(kernel.CodeInvalidKnownValue, p+"/net_amount", "The red line reverses what remains of blue line "+
				strconv.Itoa(rl.LineNo)+": it reverses what remains of its amounts too")
		}
		if wholeAmounts && !wholeQ {
			return false, kernel.Reject(kernel.CodeInvalidKnownValue, p+"/quantity", "The red line reverses what remains of the amounts of blue line "+
				strconv.Itoa(rl.LineNo)+": it reverses what remains of its quantity too")
		}
		x := after[rl.LineNo]
		x.Quantity, x.Net, x.Tax = x.Quantity.Add(q), x.Net.Add(net), x.Tax.Add(tax)
		after[rl.LineNo] = x
	}
	for _, bl := range blue.Lines {
		if Remaining(bl, after[bl.LineNo], c).Quantity.Sign() > 0 {
			return false, nil
		}
	}
	return true, nil
}

// CheckTakeBacks judges the matches in a red invoice red states (the matches of blue invoice original, whose matches are
// matches and of which earlier red invoices took back taken) and returns them as the red invoice's matches, at instant
// at: each names a match of the blue invoice (REFERENCE_INVALID), on a line the red invoice reverses
// (INVALID_KNOWN_VALUE line_no), at most what remains of that match (INVALID_KNOWN_VALUE quantity), together at most
// the red line's quantity (INVALID_KNOWN_VALUE quantity). Unless the reversal is full, every line the red invoice reverses
// then matches no more than it evidences (INVALID_KNOWN_VALUE /args/matches: name the matches taken back).
func CheckTakeBacks(original *Invoice, reversed map[int]LineReversal, matches []Match, taken map[MatchKey]kernel.Decimal, red Facts, redID string,
	in []MatchInput, full bool, at time.Time) ([]Match, error) {
	blue := original.Facts()
	c, _ := blue.Currency.Kernel()
	byKey := map[MatchKey]Match{}
	for _, m := range matches {
		byKey[m.Key()] = m
	}
	now := map[MatchKey]kernel.Decimal{}
	perLine := map[int]kernel.Decimal{}
	out := []Match{}
	for i, t := range in {
		p := "/args/matches/" + strconv.Itoa(i)
		k := MatchKey{LineNo: t.LineNo, Ref: t.Ref}
		m, ok := byKey[k]
		if !ok {
			return nil, kernel.Reject(kernel.CodeReferenceInvalid, p+"/commitment_id", "Line "+strconv.Itoa(t.LineNo)+
				" of the blue invoice has no match to that commitment line")
		}
		rl, ok := red.Line(t.LineNo)
		if !ok {
			return nil, kernel.Reject(kernel.CodeInvalidKnownValue, p+"/line_no", "The red invoice reverses nothing of line "+strconv.Itoa(t.LineNo))
		}
		left := m.Quantity.Sub(taken[k]).Sub(now[k])
		if t.Quantity.Cmp(left) > 0 {
			return nil, kernel.Reject(kernel.CodeInvalidKnownValue, p+"/quantity", "The match has "+left.String()+" left to take back")
		}
		rq, _ := kernel.ParseDecimal(rl.Quantity, kernel.QuantityFormat)
		if perLine[t.LineNo].Add(t.Quantity).Cmp(rq) > 0 {
			return nil, kernel.Reject(kernel.CodeInvalidKnownValue, p+"/quantity", "More is taken back than red line "+strconv.Itoa(t.LineNo)+" reverses")
		}
		now[k], perLine[t.LineNo] = now[k].Add(t.Quantity), perLine[t.LineNo].Add(t.Quantity)
		out = append(out, Match{InvoiceID: redID, LineNo: t.LineNo, Ref: t.Ref, Quantity: t.Quantity, Unit: m.Unit, SupplierID: m.SupplierID,
			PONumber: m.PONumber, PurchaseQuantity: m.PurchaseQuantity, DeliveredQuantity: m.DeliveredQuantity, MatchedAt: at})
	}
	if full {
		return out, nil
	}
	for _, rl := range red.Lines {
		bl, _ := blue.Line(rl.LineNo)
		q, _ := kernel.ParseDecimal(rl.Quantity, kernel.QuantityFormat)
		left := Remaining(bl, reversed[rl.LineNo], c).Quantity.Sub(q)
		var matched kernel.Decimal
		for _, m := range matches {
			if m.LineNo == rl.LineNo {
				matched = matched.Add(m.Quantity.Sub(taken[m.Key()]).Sub(now[m.Key()]))
			}
		}
		if matched.Cmp(left) > 0 {
			return nil, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/matches", "Blue line "+strconv.Itoa(rl.LineNo)+" would keep "+matched.String()+
				" matched of the "+left.String()+" it evidences: name the matches the red invoice takes back")
		}
	}
	return out, nil
}
