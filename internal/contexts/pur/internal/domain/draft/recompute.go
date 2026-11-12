package draft

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// DecodeRequest returns the purchase.draft.prepare request the draft answered, from its stored
// canonical request: what a purchase commitment decision prepares again on current facts (A47 #3).
func (d *Draft) DecodeRequest() (Request, error) {
	var doc requestDoc
	dec := json.NewDecoder(bytes.NewReader(d.Request))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return Request{}, fmt.Errorf("draft: stored request of %s: %w", d.ID, err)
	}
	r := Request{DraftID: doc.DraftID, OrderID: doc.OrderID, SelectionReason: doc.SelectionReason}
	for _, l := range doc.Lines {
		q, err := kernel.ParseDecimal(l.Quantity, kernel.QuantityFormat)
		if err != nil {
			return Request{}, fmt.Errorf("draft: stored request of %s: %w", d.ID, err)
		}
		x := RequestLine{OrderLineID: l.OrderLineID, Quantity: q, Source: Source{Ref: Ref{Kind: l.Source.Kind, ID: l.Source.ID,
			Revision: l.Source.Revision}, ConditionLineID: l.Source.ConditionLineID}}
		if o := l.Overbuy; o != nil {
			oq, err := kernel.ParseDecimal(o.Quantity, kernel.QuantityFormat)
			if err != nil {
				return Request{}, fmt.Errorf("draft: stored request of %s: %w", d.ID, err)
			}
			x.Overbuy = &Overbuy{Quantity: oq, Reason: o.Reason}
		}
		r.Lines = append(r.Lines, x)
	}
	return r, nil
}

// Recompute compares p, the draft's request prepared again on the facts as they are now, with the
// draft's sealed document, field by field (A47 #3: 数量、MOQ、倍数、阶梯价、税价和交期须与草稿逐字段一致；
// PUR-INV-009). What is not the calculation is left out: when and by whom the draft was prepared, the
// cost snapshot's adoptability (recorded, never blocking: R21) and an S3 preparer's overbuy approval.
// Any other difference is PURCHASE_DRAFT_STALE at path, naming the first field that differs: the
// draft no longer stands and is prepared anew (fail closed).
func (d *Draft) Recompute(p *Prepared, path string) error {
	stored, err := d.Decode()
	if err != nil {
		return fail(kernel.CodeReferenceInvalid, path, "The stored draft is not intact")
	}
	again := p.Doc
	again.CheckedAt, again.PreparedBy, again.AuthorityEpoch = stored.CheckedAt, stored.PreparedBy, stored.AuthorityEpoch
	c := &again.CostingSnapshot
	c.Adoptability, c.Code, c.Input = stored.CostingSnapshot.Adoptability, stored.CostingSnapshot.Code, stored.CostingSnapshot.Input
	again.Lines = slices.Clone(again.Lines)
	for i := range again.Lines {
		if i >= len(stored.Lines) || again.Lines[i].Overbuy == nil || stored.Lines[i].Overbuy == nil {
			continue
		}
		o := *again.Lines[i].Overbuy
		o.ApprovedBy, o.AuthorityEpoch = stored.Lines[i].Overbuy.ApprovedBy, stored.Lines[i].Overbuy.AuthorityEpoch
		again.Lines[i].Overbuy = &o
	}
	a, err := canonical(stored)
	if err != nil {
		return err
	}
	b, err := canonical(again)
	if err != nil {
		return err
	}
	if bytes.Equal(a, b) {
		return nil
	}
	return fail(CodePurchaseDraftStale, path, "Recomputed now, the draft differs at "+difference(a, b)+"; prepare a new draft")
}

// difference returns the JSON Pointer of the first member where the canonical documents a and b
// differ, members in byte order.
func difference(a, b []byte) string {
	var x, y any
	da, db := json.NewDecoder(bytes.NewReader(a)), json.NewDecoder(bytes.NewReader(b))
	da.UseNumber()
	db.UseNumber()
	if da.Decode(&x) != nil || db.Decode(&y) != nil {
		return "/"
	}
	if p := differ(x, y, ""); p != "" {
		return p
	}
	return "/"
}

func differ(x, y any, at string) string {
	switch a := x.(type) {
	case map[string]any:
		b, ok := y.(map[string]any)
		if !ok {
			return pointer(at)
		}
		keys := make([]string, 0, len(a)+len(b))
		for k := range a {
			keys = append(keys, k)
		}
		for k := range b {
			if _, seen := a[k]; !seen {
				keys = append(keys, k)
			}
		}
		slices.Sort(keys)
		for _, k := range keys {
			va, ina := a[k]
			vb, inb := b[k]
			if ina != inb {
				return at + "/" + escape(k)
			}
			if p := differ(va, vb, at+"/"+escape(k)); p != "" {
				return p
			}
		}
		return ""
	case []any:
		b, ok := y.([]any)
		if !ok {
			return pointer(at)
		}
		for i := range min(len(a), len(b)) {
			if p := differ(a[i], b[i], at+"/"+strconv.Itoa(i)); p != "" {
				return p
			}
		}
		if len(a) != len(b) {
			return at + "/" + strconv.Itoa(min(len(a), len(b)))
		}
		return ""
	}
	if x != y {
		return pointer(at)
	}
	return ""
}

func pointer(at string) string {
	if at == "" {
		return "/"
	}
	return at
}

func escape(k string) string { return strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1") }
