package api

import (
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Kinds of measurement (T02 §13.8): an estimate stays labelled as one after a human verified it,
// and a costing that may be frozen uses it only when its pricing policy allows verified estimates.
const (
	MeasurementMeasured  = "measured"
	MeasurementEstimated = "estimated"
)

// Package is a leaf package line: Count identical packages, each with outer dimensions in metres
// and net and gross weight in kilograms, holding goods of the RFQ lines LineIDs. Decimals are
// positive with at most 6 fractional digits.
type Package struct {
	PackageID string
	Count     int64
	LengthM   string
	WidthM    string
	HeightM   string
	NetKG     string
	GrossKG   string
	LineIDs   []string
}

// MeasurementData is the content of a measurement revision. ObservedAt is a UTC timestamp
// ("2026-10-22T08:00:00Z"); DeclaredVolumeM3, when given, must equal the volume derived from the
// packages at 6 digits HALF_UP. EstimateReason is required for an estimate and refused otherwise.
type MeasurementData struct {
	Kind             string
	ObservedAt       string
	Packages         []Package
	DeclaredVolumeM3 string
	EstimateReason   string
	Evidence         []Evidence
}

// RecordMeasurement creates a measurement snapshot with revision 1 active; only a human records
// it, and ExpectedVersion is 0 (Python measurement.record_verified).
type RecordMeasurement struct {
	IdempotencyKey  string
	MeasurementID   string
	ExpectedVersion int64
	Data            MeasurementData
}

// SupersedeMeasurement records the next revision and supersedes ActiveRevision, which must be the
// active revision, in one step.
type SupersedeMeasurement struct {
	IdempotencyKey  string
	MeasurementID   string
	ExpectedVersion int64
	ActiveRevision  int64
	Data            MeasurementData
	Reason          string
}

// WithdrawMeasurement withdraws TargetRevision for a recorded reason with evidence; it clears the
// active revision when it is the target and revives no older one.
type WithdrawMeasurement struct {
	IdempotencyKey  string
	MeasurementID   string
	ExpectedVersion int64
	TargetRevision  int64
	Reason          string
	Evidence        []Evidence
}

func (c RecordMeasurement) Validate() error {
	if err := head(c.IdempotencyKey, c.MeasurementID, c.ExpectedVersion, 0); err != nil {
		return err
	}
	if c.ExpectedVersion != 0 {
		return schema("/args/expected_version")
	}
	return c.Data.validate("/args/data")
}

func (c SupersedeMeasurement) Validate() error {
	if err := head(c.IdempotencyKey, c.MeasurementID, c.ExpectedVersion, 1); err != nil {
		return err
	}
	if err := revision(c.ActiveRevision, "/args/active_revision"); err != nil {
		return err
	}
	if err := c.Data.validate("/args/data"); err != nil {
		return err
	}
	return reason(c.Reason)
}

func (c WithdrawMeasurement) Validate() error {
	if err := head(c.IdempotencyKey, c.MeasurementID, c.ExpectedVersion, 1); err != nil {
		return err
	}
	if err := revision(c.TargetRevision, "/args/target_revision"); err != nil {
		return err
	}
	if err := reason(c.Reason); err != nil {
		return err
	}
	return evidence(c.Evidence, 1, MaxEvidence, "/args/evidence")
}

func (d MeasurementData) validate(p string) error {
	switch d.Kind {
	case MeasurementMeasured:
		if d.EstimateReason != "" {
			return schema(p + "/estimate_reason")
		}
	case MeasurementEstimated:
		if !text(d.EstimateReason, maxText) {
			return schema(p + "/estimate_reason")
		}
	default:
		return schema(p + "/kind")
	}
	if _, err := kernel.ParseTimestamp(d.ObservedAt); err != nil {
		return schema(p + "/observed_at")
	}
	if len(d.Packages) < 1 || len(d.Packages) > MaxPackages {
		return schema(p + "/packages")
	}
	for i, k := range d.Packages {
		q := index(p+"/packages", i)
		if !kernel.ValidIdentifier(k.PackageID) {
			return schema(q + "/id")
		}
		if k.Count < 1 || k.Count > MaxPackageCount {
			return schema(q + "/count")
		}
		for _, f := range []struct{ value, field string }{
			{k.LengthM, "/length_m"}, {k.WidthM, "/width_m"}, {k.HeightM, "/height_m"}, {k.NetKG, "/net_kg"}, {k.GrossKG, "/gross_kg"},
		} {
			if !positive(f.value) {
				return schema(q + f.field)
			}
		}
		if err := identifiers(k.LineIDs, 1, MaxPackageLines, q+"/line_ids"); err != nil {
			return err
		}
	}
	if d.DeclaredVolumeM3 != "" && !positive(d.DeclaredVolumeM3) {
		return schema(p + "/declared_volume_m3")
	}
	return evidence(d.Evidence, 1, MaxEvidence, p+"/evidence")
}

// MeasurementRevisionView is one revision of a measurement snapshot; its content never changes.
type MeasurementRevisionView struct {
	Revision       int64
	Status         string
	ContentHash    string
	Data           MeasurementData
	LineIDs        []string // the distinct RFQ lines its packages hold, sorted
	Reason         string   // why it superseded the revision before it; empty for revision 1
	RecordedBy     kernel.ActorRef
	RecordedAt     time.Time
	AuthorityEpoch int64

	SupersededAt     time.Time
	WithdrawnBy      kernel.ActorRef
	WithdrawnAt      time.Time
	WithdrawReason   string
	WithdrawEvidence []Evidence
}

// MeasurementView is a measurement snapshot and all its revisions, oldest first.
type MeasurementView struct {
	MeasurementID  string
	RootVersion    int64
	ActiveRevision int64 // 0: none
	LastRevision   int64
	Revisions      []MeasurementRevisionView
}
