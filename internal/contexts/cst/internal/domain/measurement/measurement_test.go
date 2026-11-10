package measurement

import (
	"slices"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

var (
	recorder = kernel.ActorRef{Kind: kernel.ActorHuman, ID: "reviewer"}
	clock    = time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
)

func code(err error) string {
	if rej, ok := kernel.AsRejection(err); ok {
		return rej.Code + " " + rej.Fields[0].Path
	}
	if err != nil {
		return "ERROR: " + err.Error()
	}
	return "OK"
}

// fixture is the Python costing fixture's measurement.1 (costing_inputs.json), observed at
// 2026-10-22T08:00:00Z, an hour before the Python clock.
func fixture() Data {
	return Data{Kind: "measured", ObservedAt: "2026-10-22T08:00:00Z", DeclaredVolumeM3: "1",
		Packages: []Package{{ID: "P1", Count: 1, LengthM: "1", WidthM: "1", HeightM: "1", NetKG: "9", GrossKG: "10", LineIDs: []string{"L1"}}},
		Evidence: []Evidence{{SourceID: "source.1", Locator: "line 1"}}}
}

// Python test_costing_domains.py test_measurement_volume_contradiction_is_rejected,
// test_gross_weight_cannot_be_less_than_net_weight and test_future_measurement_is_not_a_verified_fact,
// through the rule recording and evaluation share (rules.Measurement.Check).
func TestRecordJudgesTheMeasurement(t *testing.T) {
	cases := []struct {
		name string
		edit func(d *Data)
		want string
	}{
		{"fixture", func(*Data) {}, "OK"},
		{"declared 2 m³ for 1 m³ of packages", func(d *Data) { d.DeclaredVolumeM3 = "2" }, "MEASUREMENT_CONFLICT /args/data/declared_volume_m3"},
		{"declared volume at 6 digits", func(d *Data) {
			d.Packages[0].LengthM, d.Packages[0].WidthM, d.Packages[0].HeightM = "0.333333", "0.5", "0.5"
			d.DeclaredVolumeM3 = "0.083333"
		}, "OK"},
		{"gross 8 below net 9", func(d *Data) { d.Packages[0].GrossKG = "8" }, "INVALID_KNOWN_VALUE /args/data/packages/0/gross_kg"},
		{"observed after the clock", func(d *Data) { d.ObservedAt = "2026-10-22T10:00:00Z" }, "INVALID_KNOWN_VALUE /args/data/observed_at"},
		{"package twice", func(d *Data) { d.Packages = append(d.Packages, d.Packages[0]) }, "DUPLICATE_LINE_ID /args/data/packages/1/id"},
		{"line twice in a package", func(d *Data) { d.Packages[0].LineIDs = []string{"L1", "L1"} }, "DUPLICATE_LINE_ID /args/data/packages/0/line_ids/1"},
		{"an axis beyond 1000 m", func(d *Data) { d.Packages[0].LengthM = "1001"; d.DeclaredVolumeM3 = "" }, "CALCULATION_LIMIT_EXCEEDED /args/data/packages/0/length_m"},
		{"not a decimal", func(d *Data) { d.Packages[0].NetKG = "9e0" }, "SCHEMA_INVALID /args/data/packages/0/net_kg"},
		{"not a timestamp", func(d *Data) { d.ObservedAt = "yesterday" }, "SCHEMA_INVALID /args/data/observed_at"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := fixture()
			d.Packages = slices.Clone(d.Packages)
			c.edit(&d)
			s, err := Record("m", d, recorder, "trade", 3, clock)
			if got := code(err); got != c.want {
				t.Fatalf("%s, want %s", got, c.want)
			}
			if err == nil && (s.RootVersion != 1 || s.Active().Number != 1 || s.OwnerActor != "reviewer" || s.OwnerTeam != "trade" ||
				s.Active().Hash != d.Hash() || !s.Active().Fresh) {
				t.Fatalf("snapshot %+v", s)
			}
		})
	}
}

// T02 §13.8: supersede needs the exact active revision; withdraw targets an exact revision once and
// clears the active one only when it is the target; nothing comes back into effect.
func TestEffectHead(t *testing.T) {
	if code(CheckRecord(&Snapshot{})) != "VERSION_CONFLICT /args/expected_version" || CheckRecord(nil) != nil {
		t.Fatal("record creates only")
	}
	s, err := Record("m", fixture(), recorder, "", 1, clock)
	if err != nil {
		t.Fatal(err)
	}
	if code(Check(s, false, 1)) != "NOT_FOUND /args/id" || code(Check(s, true, 2)) != "VERSION_CONFLICT /args/expected_version" {
		t.Fatal("check")
	}
	_, err = s.Supersede(2, fixture(), recorder, 1, clock, "重新实测")
	if code(err) != "REFERENCE_STALE /args/active_revision" {
		t.Fatalf("supersede a revision that is not active: %v", err)
	}
	bad := fixture()
	bad.DeclaredVolumeM3 = "3"
	if _, err = s.Supersede(1, bad, recorder, 1, clock, "重新实测"); code(err) != "MEASUREMENT_CONFLICT /args/data/declared_volume_m3" || s.Active().Number != 1 {
		t.Fatalf("a contradicting revision supersedes nothing: %v", err)
	}
	r, err := s.Supersede(1, fixture(), recorder, 1, clock.Add(time.Minute), "重新实测")
	if err != nil || r.Number != 2 || r.Reason != "重新实测" || s.Revision(1).Status != Superseded || s.RootVersion != 2 {
		t.Fatalf("supersede %+v, %v", s.Revisions, err)
	}
	if _, err = s.Withdraw(9, recorder, 1, clock, "x", nil); code(err) != "REFERENCE_INVALID /args/target_revision" {
		t.Fatalf("withdraw an unknown revision: %v", err)
	}
	if _, err = s.Withdraw(1, recorder, 1, clock, "撤销旧实测", nil); err != nil || s.Active().Number != 2 || s.RootVersion != 3 {
		t.Fatalf("withdraw superseded: %v", err)
	}
	if _, err = s.Withdraw(1, recorder, 1, clock, "again", nil); code(err) != "INVALID_STATE /args/target_revision" {
		t.Fatalf("withdraw twice: %v", err)
	}
	if _, err = s.Withdraw(2, recorder, 1, clock, "撤销", nil); err != nil || s.Active() != nil {
		t.Fatalf("withdraw active: %v", err)
	}
	if _, err = s.Supersede(2, fixture(), recorder, 1, clock, "无现行"); code(err) != "REFERENCE_STALE /args/active_revision" {
		t.Fatalf("supersede without an active revision: %v", err)
	}
}

func TestLineIDsAndRules(t *testing.T) {
	d := fixture()
	d.Packages = append(d.Packages, Package{ID: "P2", Count: 2, LengthM: "0.5", WidthM: "0.5", HeightM: "0.5", NetKG: "1", GrossKG: "1.5",
		LineIDs: []string{"L2", "L1"}})
	if got := d.LineIDs(); !slices.Equal(got, []string{"L1", "L2"}) {
		t.Fatalf("lines %v", got)
	}
	m, err := d.Rules("/measurement")
	if err != nil || m.Volume().String() != "1.25" || !m.HasDeclaredVolume || m.Packages[1].Path != "/measurement/packages/1" {
		t.Fatalf("rules %+v, %v", m, err)
	}
	if again := fixture(); d.Hash() == fixture().Hash() || again.Hash() != fixture().Hash() {
		t.Fatal("hash")
	}
}
