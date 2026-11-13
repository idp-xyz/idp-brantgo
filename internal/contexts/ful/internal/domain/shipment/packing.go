package shipment

import (
	"strconv"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Kinds of a packing record: the planned packing (PackingPlan) and the actual one (ActualPackage).
const (
	KindPlan   = "plan"
	KindActual = "actual"
)

// Content is what each carton of a package holds of shipment line LineID.
type Content struct {
	LineID    string
	PerCarton kernel.Decimal
}

// Package is a group of Cartons equal cartons, numbered CartonFrom to CartonFrom+Cartons−1 (0: not
// numbered), each holding Contents; Gross, Net (kg) and Volume (cbm) are per carton, nil when a
// plan leaves them out.
type Package struct {
	Cartons    int64
	CartonFrom int64
	Contents   []Content
	Gross      *kernel.Decimal
	Net        *kernel.Decimal
	Volume     *kernel.Decimal
}

// Packing is one packing record of a shipment: number Seq of its Kind, written at the shipment's
// row version ShipmentVersion.
type Packing struct {
	Kind            string
	Seq             int64
	ShipmentVersion int64
	Packages        []Package
	Evidence        []string
	Recorded        Act
}

// Cartons is the number of cartons of the record.
func (p *Packing) Cartons() int64 {
	var n int64
	for _, k := range p.Packages {
		n += k.Cartons
	}
	return n
}

// Totals returns the gross weight, net weight and volume of all cartons; ok is false when a package
// leaves one of them out.
func (p *Packing) Totals() (gross, net, volume kernel.Decimal, ok bool) {
	for _, k := range p.Packages {
		if k.Gross == nil || k.Net == nil || k.Volume == nil {
			return kernel.Decimal{}, kernel.Decimal{}, kernel.Decimal{}, false
		}
		n := kernel.DecimalFromInt(k.Cartons)
		gross, net, volume = gross.Add(n.Mul(*k.Gross)), net.Add(n.Mul(*k.Net)), volume.Add(n.Mul(*k.Volume))
	}
	return gross, net, volume, true
}

// Pack records a packing of kind on a prepared shipment at expected: the plan, or the actual
// packing with the carton numbers, weights and volume of every package (the contract checks them).
// The contents name lines of the shipment (REFERENCE_INVALID) and the packages account for exactly
// the quantity of every line (INVALID_KNOWN_VALUE; BT11 "箱至订单数量一致"); every source of evidence
// is one the actor reads, as found says (REFERENCE_INVALID). The record replaces the one before of
// its kind, which is kept, and moves the shipment's row version.
func (s *Shipment) Pack(kind string, expected int64, packages []Package, evidence []string, found map[string]bool, act Act) (*Packing, error) {
	if err := s.open(expected); err != nil {
		return nil, err
	}
	if err := Readable(evidence, found, "/args/evidence_ids"); err != nil {
		return nil, err
	}
	packed := make([]kernel.Decimal, len(s.Lines))
	for i, p := range packages {
		for j, c := range p.Contents {
			_, at, ok := s.Line(c.LineID)
			if !ok {
				return nil, Reject(kernel.CodeReferenceInvalid, "/args/packages/"+strconv.Itoa(i)+"/contents/"+strconv.Itoa(j)+"/line_id",
					"The shipment has no such line")
			}
			packed[at] = packed[at].Add(kernel.DecimalFromInt(p.Cartons).Mul(c.PerCarton))
		}
	}
	for i, l := range s.Lines {
		if !packed[i].Equal(l.Quantity) {
			return nil, Reject(kernel.CodeInvalidKnownValue, "/args/packages",
				"The packages hold "+packed[i].String()+" of line "+l.ID+", the shipment ships "+l.Quantity.String())
		}
	}
	act.At = kernel.Instant(act.At)
	rec := &Packing{Kind: kind, Seq: 1, Packages: packages, Evidence: evidence, Recorded: act}
	s.RowVersion, s.PackingRecords = s.RowVersion+1, s.PackingRecords+1
	rec.ShipmentVersion = s.RowVersion
	if kind == KindActual {
		if s.Actual != nil {
			rec.Seq = s.Actual.Seq + 1
		}
		s.Actual = rec
	} else {
		if s.Plan != nil {
			rec.Seq = s.Plan.Seq + 1
		}
		s.Plan = rec
	}
	return rec, nil
}
