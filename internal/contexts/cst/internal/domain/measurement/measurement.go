// Package measurement is the MeasurementSnapshot aggregate (tactical model
// CST-AGG-MeasurementSnapshot, T02 §13.1, §13.8): package measurements a human recorded as verified
// facts, identified by (tenant, company, measurement_id), with immutable revisions and an effect
// head:
//
//	measurement.record_verified  creates the snapshot with revision 1 active
//	measurement.supersede        appends the next revision and supersedes the exact active one
//	measurement.withdraw         withdraws an exact revision; withdrawing the active one leaves none
//
// No older revision comes back into effect. Recording and every later use judge the content with
// the same rule, rules.Measurement.Check: a measurement observed after the instant is not a fact,
// gross weight is not below net weight, and a declared volume equals the volume derived from the
// packages (MEASUREMENT_CONFLICT otherwise).
package measurement

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/rules"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// CodeReferenceStale (409) is the CST error catalog row this package decides besides the rules'.
const CodeReferenceStale = "REFERENCE_STALE"

// Categories maps the code above to its error catalog category.
var Categories = map[string]kernel.Category{CodeReferenceStale: kernel.CategoryConflict}

// Status of a revision.
type Status string

const (
	Active     Status = "active"
	Superseded Status = "superseded"
	Withdrawn  Status = "withdrawn"
)

// Evidence is a source and a locator in it.
type Evidence struct {
	SourceID string `json:"source_id"`
	Locator  string `json:"locator"`
}

// Package is a leaf package line.
type Package struct {
	ID      string   `json:"id"`
	Count   int64    `json:"count"`
	LengthM string   `json:"length_m"`
	WidthM  string   `json:"width_m"`
	HeightM string   `json:"height_m"`
	NetKG   string   `json:"net_kg"`
	GrossKG string   `json:"gross_kg"`
	LineIDs []string `json:"line_ids"`
}

// Data is the content of a revision, stored as this JSON document.
type Data struct {
	Kind             string     `json:"kind"`
	ObservedAt       string     `json:"observed_at"`
	Packages         []Package  `json:"packages"`
	DeclaredVolumeM3 string     `json:"declared_volume_m3,omitempty"`
	EstimateReason   string     `json:"estimate_reason,omitempty"`
	Evidence         []Evidence `json:"evidence"`
}

// Hash returns the content hash: the SHA-256 of the canonical JSON of the data.
func (d Data) Hash() string {
	doc, err := json.Marshal(d)
	if err != nil {
		panic(fmt.Sprintf("measurement: data does not encode: %v", err)) // only strings, integers and lists
	}
	sum := sha256.Sum256(doc)
	return hex.EncodeToString(sum[:])
}

// LineIDs returns the distinct lines the packages hold, sorted.
func (d Data) LineIDs() []string {
	var out []string
	for _, p := range d.Packages {
		out = append(out, p.LineIDs...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Rules returns the measurement as the calculation rules take it; path is where the data sits
// ("/args/data" in a request). A value that is not of its contract format is SCHEMA_INVALID.
func (d Data) Rules(path string) (rules.Measurement, error) {
	m := rules.Measurement{Path: path, Kind: rules.MeasurementKind(d.Kind)}
	at, err := kernel.ParseTimestamp(d.ObservedAt)
	if err != nil {
		return rules.Measurement{}, kernel.Reject(kernel.CodeSchemaInvalid, path+"/observed_at", "")
	}
	m.ObservedAt = at
	decimal := func(s, p string) (kernel.Decimal, error) {
		v, err := kernel.ParseDecimal(s, kernel.QuantityFormat)
		if err != nil {
			return kernel.Decimal{}, kernel.Reject(kernel.CodeSchemaInvalid, p, "")
		}
		return v, nil
	}
	for i, p := range d.Packages {
		pp := fmt.Sprintf("%s/packages/%d", path, i)
		k := rules.Package{Path: pp, ID: p.ID, Count: p.Count, LineIDs: slices.Clone(p.LineIDs)}
		for _, f := range []struct {
			to    *kernel.Decimal
			from  string
			field string
		}{
			{&k.LengthM, p.LengthM, "/length_m"}, {&k.WidthM, p.WidthM, "/width_m"}, {&k.HeightM, p.HeightM, "/height_m"},
			{&k.NetKG, p.NetKG, "/net_kg"}, {&k.GrossKG, p.GrossKG, "/gross_kg"},
		} {
			if *f.to, err = decimal(f.from, pp+f.field); err != nil {
				return rules.Measurement{}, err
			}
		}
		m.Packages = append(m.Packages, k)
	}
	if d.DeclaredVolumeM3 != "" {
		if m.DeclaredVolume, err = decimal(d.DeclaredVolumeM3, path+"/declared_volume_m3"); err != nil {
			return rules.Measurement{}, err
		}
		m.HasDeclaredVolume = true
	}
	return m, nil
}

// Check judges the data at instant now with rules.Measurement.Check, the rule every later use
// applies too; path is where the data sits.
func (d Data) Check(path string, now time.Time) error {
	m, err := d.Rules(path)
	if err != nil {
		return err
	}
	return m.Check(now)
}

// Withdrawal is the withdrawal of a revision.
type Withdrawal struct {
	By       kernel.ActorRef
	Epoch    int64
	At       time.Time
	Reason   string
	Evidence []Evidence
}

// Revision is one recorded revision.
type Revision struct {
	Number       int64
	Status       Status
	Data         Data
	Hash         string
	RecordedBy   kernel.ActorRef
	Epoch        int64
	RecordedAt   time.Time
	Reason       string // why it superseded the revision before it
	SupersededAt time.Time
	Withdrawal   *Withdrawal

	// Fresh and Changed tell the store what a command did: Fresh revisions are inserted, Changed
	// ones updated.
	Fresh   bool
	Changed bool
}

// Snapshot is the aggregate root.
type Snapshot struct {
	ID           string
	RootVersion  int64
	LastRevision int64
	OwnerActor   string
	OwnerTeam    string
	Revisions    []*Revision // oldest first
}

// Revision returns revision n, or nil.
func (s *Snapshot) Revision(n int64) *Revision {
	for _, r := range s.Revisions {
		if r.Number == n {
			return r
		}
	}
	return nil
}

// Active returns the active revision, or nil.
func (s *Snapshot) Active() *Revision {
	for _, r := range s.Revisions {
		if r.Status == Active {
			return r
		}
	}
	return nil
}

// CheckRecord admits record_verified: only a new snapshot is recorded (Python: an existing one is
// VERSION_CONFLICT).
func CheckRecord(s *Snapshot) error {
	if s != nil {
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	return nil
}

// Check admits a command on an existing, visible snapshot at its current root version.
func Check(s *Snapshot, visible bool, expected int64) error {
	switch {
	case s == nil || !visible:
		return kernel.Reject(kernel.CodeNotFound, "/args/id", "")
	case s.RootVersion != expected:
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	return nil
}

// Record creates a snapshot with revision 1 active, recorded by a human at now, after judging the
// data at now.
func Record(id string, d Data, by kernel.ActorRef, team string, epoch int64, now time.Time) (*Snapshot, error) {
	if err := d.Check("/args/data", now); err != nil {
		return nil, err
	}
	s := &Snapshot{ID: id, OwnerActor: by.ID, OwnerTeam: team}
	s.append(d, by, epoch, now, "")
	return s, nil
}

func (s *Snapshot) append(d Data, by kernel.ActorRef, epoch int64, now time.Time, reason string) *Revision {
	s.LastRevision++
	r := &Revision{Number: s.LastRevision, Status: Active, Data: d, Hash: d.Hash(), RecordedBy: by, Epoch: epoch,
		RecordedAt: kernel.Instant(now), Reason: reason, Fresh: true}
	s.Revisions = append(s.Revisions, r)
	s.RootVersion++
	return r
}

// Supersede appends the next revision and supersedes revision active, which must be the active one
// (REFERENCE_STALE otherwise, Python measurement.supersede).
func (s *Snapshot) Supersede(active int64, d Data, by kernel.ActorRef, epoch int64, now time.Time, reason string) (*Revision, error) {
	a := s.Active()
	if a == nil || a.Number != active {
		return nil, kernel.RejectAs(CodeReferenceStale, kernel.CategoryConflict, "/args/active_revision", "The revision to supersede is not the active one")
	}
	if err := d.Check("/args/data", now); err != nil {
		return nil, err
	}
	a.Status, a.SupersededAt, a.Changed = Superseded, kernel.Instant(now), true
	return s.append(d, by, epoch, now, reason), nil
}

// Withdraw withdraws revision target (REFERENCE_INVALID when there is none, INVALID_STATE when it
// is withdrawn already); withdrawing the active revision leaves none.
func (s *Snapshot) Withdraw(target int64, by kernel.ActorRef, epoch int64, now time.Time, reason string, evidence []Evidence) (*Revision, error) {
	t := s.Revision(target)
	switch {
	case t == nil:
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/args/target_revision", "")
	case t.Status == Withdrawn:
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/target_revision", "The revision is withdrawn already")
	}
	t.Status, t.Changed = Withdrawn, true
	t.Withdrawal = &Withdrawal{By: by, Epoch: epoch, At: kernel.Instant(now), Reason: reason, Evidence: slices.Clone(evidence)}
	s.RootVersion++
	return t, nil
}
