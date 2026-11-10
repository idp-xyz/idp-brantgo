package delivery

import (
	"testing"
	"time"
	_ "time/tzdata" // the test does not depend on the host's zoneinfo

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The vectors come from Python V0.50 tests/test_costing_domains.py on its fixture: clock
// 2026-10-22T09:00:00Z, the synthetic calendar of tests/fixtures/extension_inputs.json
// (Asia/Shanghai, 2026-01-01 – 2027-12-31, Monday–Friday, holiday 2026-10-26, working Saturday
// 2026-10-24) and lead_time(): 3 working days after the anchor, handover at port.test.

var clock = time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)

func calendar() *Calendar {
	return &Calendar{Path: "/delivery_calendar/domain.calendar", TimeZone: "Asia/Shanghai",
		CoverageFrom: MustDate("2026-01-01"), CoverageUntil: MustDate("2027-12-31"), WorkingWeekdays: []int{1, 2, 3, 4, 5},
		Holidays: []Date{MustDate("2026-10-26")}, WorkingOverrides: []Date{MustDate("2026-10-24")}}
}

func leadTime() Rule {
	return Rule{Path: "/lines/0/delivery", Kind: KindLeadTime, Days: 3, DayBasis: DayBasisWorking,
		Milestone: MilestoneHandover, Place: "location:port.test@1", TimeZone: "Asia/Shanghai"}
}

// verified is the supplier offer's verification at the fixture clock (17:00 in Shanghai).
func verified() *Anchor {
	return &Anchor{Path: "/lines/0/delivery/anchor_ref", Verified: true, OccurredAt: clock}
}

func rejected(t *testing.T, err error, code, path string) {
	t.Helper()
	r, ok := kernel.AsRejection(err)
	if !ok || r.Code != code || (path != "" && r.Fields[0].Path != path) {
		t.Fatalf("got %v, want %s at %q", err, code, path)
	}
}

// Python test_working_days_use_real_verification_event_and_reviewed_overrides: from Thursday
// 2026-10-22, Friday 23 (1), the working Saturday 24 (2), Sunday 25 no, holiday Monday 26 no,
// Tuesday 27 (3). test_zero_lead_days_resolves_to_anchor_local_date: 0 days is 2026-10-22.
func TestWorkingDays(t *testing.T) {
	for days, want := range map[int]string{3: "2026-10-27", 0: "2026-10-22", 1: "2026-10-23", 2: "2026-10-24"} {
		r := leadTime()
		r.Days = days
		got, err := Resolve(r, verified(), calendar(), clock)
		if err != nil || !got.Resolved || got.Date.String() != want {
			t.Errorf("%d days: %+v %v, want %s", days, got, err, want)
		}
	}
	r := leadTime()
	r.DayBasis = DayBasisCalendar
	got, err := Resolve(r, verified(), calendar(), clock)
	if err != nil || got.Date.String() != "2026-10-25" {
		t.Errorf("calendar days: %+v %v", got, err)
	}
	// The local date counts: 2026-10-21T16:30Z is already the 22nd in Shanghai.
	early := verified()
	early.OccurredAt = time.Date(2026, 10, 21, 16, 30, 0, 0, time.UTC)
	got, err = Resolve(leadTime(), early, calendar(), clock)
	if err != nil || got.Date.String() != "2026-10-27" {
		t.Errorf("local date: %+v %v", got, err)
	}
}

// Python test_pending_verification_anchor_does_not_invent_a_date: a pending anchor resolves to no
// date (the result is provisional and cannot be frozen; the freeze refusal is the application's).
func TestPendingAnchor(t *testing.T) {
	for _, a := range []*Anchor{nil, {Path: "/a"}, {Path: "/a", Verified: true}} {
		got, err := Resolve(leadTime(), a, calendar(), clock)
		if err != nil || got.Resolved || !got.Date.IsZero() || got.Reason == "" {
			t.Errorf("%+v: %+v %v", a, got, err)
		}
	}
}

// Python test_delivery_calendar_cannot_extrapolate_outside_coverage (coverage until 2026-10-26),
// test_delivery_calendar_time_zone_must_match_rule (calendar in UTC), test_invalid_delivery_time_
// zone_cannot_be_verified (Invalid/Nowhere), test_future_anchor_event_is_not_treated_as_already_
// occurred (anchor verified at 10:00, clock 09:00) and test_conflicting_holiday_and_workday_cannot_
// be_published.
func TestDeliveryRefusals(t *testing.T) {
	short := calendar()
	short.CoverageUntil = MustDate("2026-10-26")
	_, err := Resolve(leadTime(), verified(), short, clock)
	rejected(t, err, CodeCostInputNotReady, "/lines/0/delivery/calendar_ref")

	utc := calendar()
	utc.TimeZone = "UTC"
	_, err = Resolve(leadTime(), verified(), utc, clock)
	rejected(t, err, kernel.CodeInvalidKnownValue, "/lines/0/delivery/calendar_ref")

	bad := leadTime()
	bad.TimeZone = "Invalid/Nowhere"
	_, err = Resolve(bad, verified(), calendar(), clock)
	rejected(t, err, kernel.CodeInvalidKnownValue, "/lines/0/delivery/time_zone")
	fixed := Rule{Path: "/lines/0/delivery", Kind: KindFixed, Date: MustDate("2026-11-15"), Milestone: MilestoneHandover,
		Place: "location:port.test@1", TimeZone: "Invalid/Nowhere"}
	_, err = Resolve(fixed, nil, nil, clock)
	rejected(t, err, kernel.CodeInvalidKnownValue, "/lines/0/delivery/time_zone")

	future := verified()
	future.OccurredAt = clock.Add(time.Hour)
	_, err = Resolve(leadTime(), future, calendar(), clock)
	rejected(t, err, kernel.CodeInvalidKnownValue, "/lines/0/delivery/anchor_ref")

	conflict := calendar()
	conflict.WorkingOverrides = append(conflict.WorkingOverrides, conflict.Holidays[0])
	rejected(t, conflict.Check(), kernel.CodeInvalidKnownValue, "/delivery_calendar/domain.calendar/working_overrides/1")

	_, err = Resolve(leadTime(), verified(), nil, clock)
	rejected(t, err, CodeCostInputNotReady, "/lines/0/delivery/calendar_ref")

	before := calendar()
	before.CoverageFrom = MustDate("2026-10-23")
	_, err = Resolve(leadTime(), verified(), before, clock)
	rejected(t, err, CodeCostInputNotReady, "/lines/0/delivery/calendar_ref")

	for _, c := range []struct {
		edit func(c *Calendar)
		path string
	}{
		{func(c *Calendar) { c.WorkingWeekdays = nil }, "/delivery_calendar/domain.calendar/working_weekdays"},
		{func(c *Calendar) { c.WorkingWeekdays = []int{1, 8} }, "/delivery_calendar/domain.calendar/working_weekdays/1"},
		{func(c *Calendar) { c.WorkingWeekdays = []int{1, 1} }, "/delivery_calendar/domain.calendar/working_weekdays/1"},
		{func(c *Calendar) { c.Holidays = []Date{MustDate("2028-01-01")} }, "/delivery_calendar/domain.calendar/holidays/0"},
		{func(c *Calendar) { c.CoverageUntil = MustDate("2025-12-31") }, "/delivery_calendar/domain.calendar/coverage_until"},
	} {
		cal := calendar()
		c.edit(cal)
		rejected(t, cal.Check(), kernel.CodeInvalidKnownValue, c.path)
	}

	days := leadTime()
	days.Days = MaxLeadDays + 1
	_, err = Resolve(days, verified(), calendar(), clock)
	rejected(t, err, kernel.CodeInvalidKnownValue, "/lines/0/delivery/days")
	basis := leadTime()
	basis.DayBasis = "business"
	_, err = Resolve(basis, verified(), calendar(), clock)
	rejected(t, err, kernel.CodeInvalidKnownValue, "/lines/0/delivery/day_basis")
}

// Python test_late_delivery_can_be_analyzed_but_cannot_be_frozen: a supply date of 2026-11-16 for
// a requirement of 2026-11-15 resolves and stays analyzable but is not comparable. Another place,
// milestone or zone is not comparable either.
func TestCompare(t *testing.T) {
	required := Rule{Path: "/context/comparison_basis/delivery", Kind: KindFixed, Date: MustDate("2026-11-15"),
		Milestone: MilestoneHandover, Place: "location:port.test@1", TimeZone: "Asia/Shanghai"}
	late := required
	late.Path, late.Date = "/lines/0/delivery", MustDate("2026-11-16")
	req, err := Resolve(required, nil, nil, clock)
	if err != nil {
		t.Fatal(err)
	}
	sup, err := Resolve(late, nil, nil, clock)
	if err != nil {
		t.Fatal(err)
	}
	if d := Compare(required, late, req, sup); len(d) != 1 || d[0].Path != "/lines/0/delivery" {
		t.Fatalf("late: %+v", d)
	}
	onTime := late
	onTime.Date = MustDate("2026-11-15")
	sup, _ = Resolve(onTime, nil, nil, clock)
	if d := Compare(required, onTime, req, sup); len(d) != 0 {
		t.Fatalf("on time: %+v", d)
	}
	arrival := onTime
	arrival.Milestone = MilestoneArrival
	if d := Compare(required, arrival, req, sup); len(d) != 1 {
		t.Fatalf("milestone: %+v", d)
	}
}

// Python test_unmapped_order_event_is_explicitly_unsupported: an order_confirmed anchor has no
// mapping yet (DEPENDENCY_NOT_SUPPORTED); a supplier offer verification does.
func TestAnchorMapping(t *testing.T) {
	kind, id, err := ParseAnchor("/anchor_ref/id", "supplier_offer:offer.1")
	if err != nil || kind != "supplier_offer" || id != "offer.1" {
		t.Fatalf("%s %s %v", kind, id, err)
	}
	_, _, err = ParseAnchor("/anchor_ref/id", "order_confirmed:order.1")
	rejected(t, err, CodeDependencyNotSupported, "/anchor_ref/id")
	_, _, err = ParseAnchor("/anchor_ref/id", "supplier_offer")
	rejected(t, err, kernel.CodeSchemaInvalid, "/anchor_ref/id")
	_, _, err = ParseAnchor("/anchor_ref/id", "supplier_offer:"+string(make([]byte, 60)))
	rejected(t, err, kernel.CodeSchemaInvalid, "/anchor_ref/id")
}

func TestParseDate(t *testing.T) {
	for _, bad := range []string{"2026-02-30", "2026-2-03", "20261022", "2026-10-22T00:00:00Z", ""} {
		if _, err := ParseDate(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	if d := MustDate("2026-10-25"); d.ISOWeekday() != 7 || d.AddDays(1).ISOWeekday() != 1 {
		t.Fatal("ISO weekdays")
	}
}
