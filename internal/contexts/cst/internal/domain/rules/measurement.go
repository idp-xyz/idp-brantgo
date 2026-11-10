package rules

import (
	"fmt"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// MeasurementKind says whether a measurement was taken or estimated; an estimate stays labelled as
// one after verification (T02 §13.8).
type MeasurementKind string

const (
	Measured  MeasurementKind = "measured"
	Estimated MeasurementKind = "estimated"
)

// Package is a leaf package line of a measurement: Count identical packages, each with outer
// dimensions in metres and net and gross weight in kilograms, holding goods of LineIDs.
type Package struct {
	Path                     string
	ID                       string
	Count                    int64
	LengthM, WidthM, HeightM kernel.Decimal
	NetKG, GrossKG           kernel.Decimal
	LineIDs                  []string
}

// Volume returns the outer volume of one package in cubic metres, exactly.
func (p Package) Volume() kernel.Decimal { return p.LengthM.Mul(p.WidthM).Mul(p.HeightM) }

// Measurement is a MeasurementSnapshot revision as the rules need it. The volume derived from the
// dimensions is authoritative; a declared volume only confirms it.
type Measurement struct {
	Path              string
	Kind              MeasurementKind
	ObservedAt        time.Time
	Packages          []Package
	DeclaredVolume    kernel.Decimal // m³, compared when HasDeclaredVolume
	HasDeclaredVolume bool
}

// Volume returns the derived total volume in cubic metres, exactly.
func (m Measurement) Volume() kernel.Decimal {
	total := kernel.DecimalFromInt(0)
	for _, p := range m.Packages {
		total = total.Add(p.Volume().Mul(kernel.DecimalFromInt(p.Count)))
	}
	return total
}

// Check applies the rules of measurement.record_verified and of every later use at instant now: a
// measurement observed after now is not a verified fact; package IDs and their lines once; counts
// positive; dimensions and weights positive with at most 6 fractional digits, each axis at most
// 1000 m and the totals at most 10^9 kg and m³; gross weight not below net weight
// (INVALID_KNOWN_VALUE); and a declared volume equal to the derived one at 6 digits HALF_UP
// (MEASUREMENT_CONFLICT otherwise).
func (m Measurement) Check(now time.Time) error {
	switch m.Kind {
	case Measured, Estimated:
	default:
		return fail(kernel.CodeSchemaInvalid, m.Path+"/kind", "")
	}
	if m.ObservedAt.IsZero() || kernel.Instant(m.ObservedAt).After(kernel.Instant(now)) {
		return fail(kernel.CodeInvalidKnownValue, m.Path+"/observed_at", "A measurement observed after now is not a verified fact")
	}
	switch n := len(m.Packages); {
	case n == 0:
		return fail(kernel.CodeInvalidKnownValue, m.Path+"/packages", "A measurement has at least one package")
	case n > MaxPackages:
		return fail(CodeCalculationLimitExceeded, m.Path+"/packages", "A measurement has at most 1000 package lines")
	}
	ids := make([]string, len(m.Packages))
	weight := kernel.DecimalFromInt(0)
	for i, p := range m.Packages {
		ids[i] = p.ID
		if p.Count < 1 {
			return fail(kernel.CodeInvalidKnownValue, p.Path+"/count", "A package count is a positive integer")
		}
		for _, d := range []struct {
			v     kernel.Decimal
			field string
			max   kernel.Decimal
		}{
			{p.LengthM, "/length_m", maxDimension}, {p.WidthM, "/width_m", maxDimension}, {p.HeightM, "/height_m", maxDimension},
			{p.NetKG, "/net_kg", maxMeasureTotal}, {p.GrossKG, "/gross_kg", maxMeasureTotal},
		} {
			switch {
			case !fits(d.v, QuantityScale):
				return fail(kernel.CodeSchemaInvalid, p.Path+d.field, "A measurement has at most 6 fractional digits")
			case d.v.Sign() <= 0:
				return fail(kernel.CodeInvalidKnownValue, p.Path+d.field, "A measurement is positive")
			case d.v.Cmp(d.max) > 0:
				return fail(CodeCalculationLimitExceeded, p.Path+d.field, "The measurement is beyond its bound")
			}
		}
		if p.GrossKG.Cmp(p.NetKG) < 0 {
			return fail(kernel.CodeInvalidKnownValue, p.Path+"/gross_kg", "Gross weight is not below net weight")
		}
		if len(p.LineIDs) == 0 {
			return fail(kernel.CodeInvalidKnownValue, p.Path+"/line_ids", "A package holds goods of at least one line")
		}
		if err := unique(p.LineIDs, p.Path+"/line_ids"); err != nil {
			return err
		}
		weight = weight.Add(p.GrossKG.Mul(kernel.DecimalFromInt(p.Count)))
	}
	if i := repeated(ids); i >= 0 {
		return fail(kernel.CodeDuplicateLineID, m.Packages[i].Path+"/id", "")
	}
	volume := m.Volume()
	if weight.Cmp(maxMeasureTotal) > 0 || volume.Cmp(maxMeasureTotal) > 0 {
		return fail(CodeCalculationLimitExceeded, m.Path+"/packages", "A measurement totals at most 1000000000 kg and m³")
	}
	if m.HasDeclaredVolume {
		if !fits(m.DeclaredVolume, QuantityScale) {
			return fail(kernel.CodeSchemaInvalid, m.Path+"/declared_volume_m3", "")
		}
		if !volume.RoundHalfUp(QuantityScale).Equal(m.DeclaredVolume) {
			return fail(CodeMeasurementConflict, m.Path+"/declared_volume_m3",
				fmt.Sprintf("The declared volume %s m³ contradicts the %s m³ derived from the packages", m.DeclaredVolume, volume.RoundHalfUp(QuantityScale)))
		}
	}
	return nil
}

// Usable admits the measurement for a calculation that may be frozen: an estimate only when the
// pricing policy explicitly allows verified estimates (allow_verified_estimates; a policy without
// the setting allows none).
func (m Measurement) Usable(allowVerifiedEstimates bool) error {
	if m.Kind == Estimated && !allowVerifiedEstimates {
		return fail(CodeCostInputNotReady, m.Path+"/kind", "The pricing policy does not allow verified estimates")
	}
	return nil
}

// perLine sums net weight or volume per line over single-line packages; every package's lines must
// be lines of the scenario.
func (m Measurement) perLine(basis Basis, lines map[string]BaseLine) (map[string]kernel.Decimal, error) {
	out := map[string]kernel.Decimal{}
	for _, p := range m.Packages {
		for i, id := range p.LineIDs {
			if _, ok := lines[id]; !ok {
				return nil, fail(kernel.CodeReferenceInvalid, index(p.Path, "line_ids", i), "The package holds a line the scenario does not purchase")
			}
		}
		if len(p.LineIDs) != 1 {
			return nil, fail(CodeDependencyNotSupported, p.Path+"/line_ids", "A mixed package cannot allocate by weight or volume; use an explicit allocation")
		}
		per := p.NetKG
		if basis == BasisVolume {
			per = p.Volume()
		}
		id := p.LineIDs[0]
		out[id] = out[id].Add(per.Mul(kernel.DecimalFromInt(p.Count)))
	}
	return out, nil
}
