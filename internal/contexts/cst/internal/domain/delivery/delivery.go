// Package delivery resolves delivery commitments of costing contexts and supply conditions into
// dates (T02 §15.3): a fixed date, or a number of calendar or working days after a verified anchor
// event, counted on a published delivery calendar. It never uses the system's today for an unknown
// anchor, never infers holidays and never extrapolates beyond a calendar's coverage. It is pure:
// the caller resolves the anchor, the calendar and the instant of the calculation.
//
// Rejections point at the Path of the rule, anchor or calendar the caller passes (JSON Pointers of
// its request or stored document) followed by the contract field.
package delivery

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Codes of the CST error catalog rows the delivery rules decide; package api declares the same.
const (
	CodeCostInputNotReady        = "COST_INPUT_NOT_READY"
	CodeDependencyNotSupported   = "DEPENDENCY_NOT_SUPPORTED"
	CodeCalculationLimitExceeded = "CALCULATION_LIMIT_EXCEEDED"
)

// Categories maps the codes above to their error catalog category.
var Categories = map[string]kernel.Category{
	CodeCostInputNotReady:        kernel.CategoryInvalid,
	CodeDependencyNotSupported:   kernel.CategoryInvalid,
	CodeCalculationLimitExceeded: kernel.CategoryInvalid,
}

func fail(code, path, reason string) error {
	if c, ok := Categories[code]; ok {
		return kernel.RejectAs(code, c, path, reason)
	}
	return kernel.Reject(code, path, reason)
}

// MaxLeadDays bounds the days of a lead time.
const MaxLeadDays = 3650

// Date is a civil date without time or zone.
type Date struct{ t time.Time } // midnight UTC

// ParseDate parses a real date in the form YYYY-MM-DD.
func ParseDate(s string) (Date, error) {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil || len(s) != len(time.DateOnly) {
		return Date{}, fmt.Errorf("delivery: invalid date %q", s)
	}
	return Date{t: t}, nil
}

// MustDate is ParseDate that panics, for constants and tests.
func MustDate(s string) Date {
	d, err := ParseDate(s)
	if err != nil {
		panic(err)
	}
	return d
}

// LocalDate returns the date of instant t in loc.
func LocalDate(t time.Time, loc *time.Location) Date {
	y, m, d := t.In(loc).Date()
	return Date{t: time.Date(y, m, d, 0, 0, 0, 0, time.UTC)}
}

// IsZero reports whether d is the zero Date.
func (d Date) IsZero() bool { return d.t.IsZero() }

func (d Date) String() string { return d.t.Format(time.DateOnly) }

// Compare returns -1, 0 or +1 as d is before, equal to or after o.
func (d Date) Compare(o Date) int { return d.t.Compare(o.t) }

// AddDays returns d plus n days.
func (d Date) AddDays(n int) Date { return Date{t: d.t.AddDate(0, 0, n)} }

func hasDate(ds []Date, d Date) bool {
	return slices.ContainsFunc(ds, func(x Date) bool { return x.Compare(d) == 0 })
}

// ISOWeekday returns 1 (Monday) to 7 (Sunday).
func (d Date) ISOWeekday() int {
	if w := int(d.t.Weekday()); w != 0 {
		return w
	}
	return 7
}

// Calendar is a published delivery calendar revision: the working weekdays (ISO 1–7), holidays
// and working overrides (make-up working days) of its time zone over [CoverageFrom, CoverageUntil],
// both ends included. The company verifies and publishes it; nothing here knows public holidays.
type Calendar struct {
	Path             string
	TimeZone         string
	CoverageFrom     Date
	CoverageUntil    Date
	WorkingWeekdays  []int
	Holidays         []Date
	WorkingOverrides []Date
}

// Check applies the publication rules of a calendar: a known IANA time zone, a coverage that does
// not end before it starts, holidays and overrides inside it and never on the same day, and
// working weekdays 1–7, at least one, each once. Violations are INVALID_KNOWN_VALUE.
func (c Calendar) Check() error {
	if _, err := location(c.TimeZone); err != nil {
		return fail(kernel.CodeInvalidKnownValue, c.Path+"/time_zone", "Unknown IANA time zone")
	}
	if c.CoverageFrom.IsZero() || c.CoverageUntil.IsZero() || c.CoverageUntil.Compare(c.CoverageFrom) < 0 {
		return fail(kernel.CodeInvalidKnownValue, c.Path+"/coverage_until", "The coverage ends on or after its start")
	}
	inside := func(d Date) bool { return d.Compare(c.CoverageFrom) >= 0 && d.Compare(c.CoverageUntil) <= 0 }
	for i, h := range c.Holidays {
		if !inside(h) {
			return fail(kernel.CodeInvalidKnownValue, fmt.Sprintf("%s/holidays/%d", c.Path, i), "A holiday lies outside the coverage")
		}
	}
	for i, o := range c.WorkingOverrides {
		if !inside(o) {
			return fail(kernel.CodeInvalidKnownValue, fmt.Sprintf("%s/working_overrides/%d", c.Path, i), "A working override lies outside the coverage")
		}
		if hasDate(c.Holidays, o) {
			return fail(kernel.CodeInvalidKnownValue, fmt.Sprintf("%s/working_overrides/%d", c.Path, i), "A day is both a holiday and a working day")
		}
	}
	if len(c.WorkingWeekdays) == 0 {
		return fail(kernel.CodeInvalidKnownValue, c.Path+"/working_weekdays", "A calendar has at least one working weekday")
	}
	for i, w := range c.WorkingWeekdays {
		if w < 1 || w > 7 || slices.Index(c.WorkingWeekdays, w) != i {
			return fail(kernel.CodeInvalidKnownValue, fmt.Sprintf("%s/working_weekdays/%d", c.Path, i), "Working weekdays are ISO weekdays 1–7, each once")
		}
	}
	return nil
}

func (c Calendar) working(d Date) bool {
	return hasDate(c.WorkingOverrides, d) || (slices.Contains(c.WorkingWeekdays, d.ISOWeekday()) && !hasDate(c.Holidays, d))
}

// Kinds, milestones and day bases of a delivery rule.
const (
	KindFixed    = "fixed"
	KindLeadTime = "lead_time"

	MilestoneGoodsReady = "goods_ready"
	MilestoneHandover   = "handover"
	MilestoneArrival    = "arrival"

	DayBasisCalendar = "calendar"
	DayBasisWorking  = "working"
)

// Rule is a delivery commitment or requirement: a milestone at an exact place in a time zone, on a
// fixed Date or Days after an anchor counted on DayBasis. Place is the identity of the location
// revision; the rules only compare it.
type Rule struct {
	Path      string
	Kind      string
	Date      Date
	Days      int
	DayBasis  string
	Milestone string
	Place     string
	TimeZone  string
}

// Anchor is the business fact a lead time counts from, as projected from a verified revision:
// Verified with the instant it occurred, or pending (not yet occurred, no instant).
type Anchor struct {
	Path       string
	Verified   bool
	OccurredAt time.Time
}

// Resolution is a rule resolved to a date, or unresolved with the reason no date was assumed. An
// unresolved delivery makes a result provisional and not comparable.
type Resolution struct {
	Resolved bool
	Date     Date
	Reason   string
}

// Resolve resolves rule at instant now (T02 §15.3). A fixed rule is its date. A lead time needs
// its calendar (COST_INPUT_NOT_READY), in the rule's time zone (INVALID_KNOWN_VALUE otherwise); a
// pending anchor resolves to nothing, and an anchor that occurs after now is not a fact
// (INVALID_KNOWN_VALUE). Counting starts on the anchor's local date in that zone, which does not
// count; 0 days is that date. Calendar days all count; working days are the calendar's working
// weekdays except holidays, plus working overrides. A count that leaves the coverage is
// COST_INPUT_NOT_READY: nothing is extrapolated.
func Resolve(rule Rule, anchor *Anchor, cal *Calendar, now time.Time) (Resolution, error) {
	if _, err := location(rule.TimeZone); err != nil {
		return Resolution{}, fail(kernel.CodeInvalidKnownValue, rule.Path+"/time_zone", "Unknown IANA time zone")
	}
	switch rule.Milestone {
	case MilestoneGoodsReady, MilestoneHandover, MilestoneArrival:
	default:
		return Resolution{}, fail(kernel.CodeSchemaInvalid, rule.Path+"/milestone", "")
	}
	if rule.Place == "" {
		return Resolution{}, fail(kernel.CodeSchemaInvalid, rule.Path+"/place_ref", "A delivery names its exact place")
	}
	switch rule.Kind {
	case KindFixed:
		if rule.Date.IsZero() {
			return Resolution{}, fail(kernel.CodeSchemaInvalid, rule.Path+"/date", "")
		}
		return Resolution{Resolved: true, Date: rule.Date}, nil
	case KindLeadTime:
	default:
		return Resolution{}, fail(kernel.CodeSchemaInvalid, rule.Path+"/kind", "")
	}
	if rule.Days < 0 || rule.Days > MaxLeadDays {
		return Resolution{}, fail(kernel.CodeInvalidKnownValue, rule.Path+"/days", "A lead time is 0–3650 days")
	}
	if rule.DayBasis != DayBasisCalendar && rule.DayBasis != DayBasisWorking {
		return Resolution{}, fail(kernel.CodeInvalidKnownValue, rule.Path+"/day_basis", "")
	}
	if cal == nil {
		return Resolution{}, fail(CodeCostInputNotReady, rule.Path+"/calendar_ref", "A lead time needs a published delivery calendar")
	}
	if err := cal.Check(); err != nil {
		return Resolution{}, err
	}
	if cal.TimeZone != rule.TimeZone {
		return Resolution{}, fail(kernel.CodeInvalidKnownValue, rule.Path+"/calendar_ref", "The calendar's time zone differs from the delivery's")
	}
	if anchor == nil || !anchor.Verified || anchor.OccurredAt.IsZero() {
		return Resolution{Reason: "The anchor has no verified occurrence; no date was assumed"}, nil
	}
	if kernel.Instant(anchor.OccurredAt).After(kernel.Instant(now)) {
		return Resolution{}, fail(kernel.CodeInvalidKnownValue, anchor.Path, "An anchor that has not occurred yet is not a verified fact")
	}
	loc, _ := location(cal.TimeZone)
	current := LocalDate(anchor.OccurredAt, loc)
	if current.Compare(cal.CoverageFrom) < 0 || current.Compare(cal.CoverageUntil) > 0 {
		return Resolution{}, fail(CodeCostInputNotReady, rule.Path+"/calendar_ref", "The anchor's date lies outside the calendar's coverage")
	}
	for remaining := rule.Days; remaining > 0; {
		if current.Compare(cal.CoverageUntil) >= 0 {
			return Resolution{}, fail(CodeCostInputNotReady, rule.Path+"/calendar_ref", "The lead time runs beyond the calendar's coverage; nothing is extrapolated")
		}
		current = current.AddDays(1)
		if rule.DayBasis == DayBasisCalendar || cal.working(current) {
			remaining--
		}
	}
	return Resolution{Resolved: true, Date: current}, nil
}

// Difference is a reason a supply delivery is not comparable with the requirement.
type Difference struct {
	Path   string
	Reason string
}

// Compare compares a supply delivery with the requirement (T02 §15.3): another milestone, place or
// time zone is not comparable, since no transport time or event equivalence is assumed; a resolved
// supply date after the resolved required date is not comparable either, though the cost stays
// analyzable. Unresolved deliveries are reported by their Resolution.
func Compare(required, supplied Rule, req, sup Resolution) []Difference {
	var out []Difference
	if required.Milestone != supplied.Milestone || required.Place != supplied.Place || required.TimeZone != supplied.TimeZone {
		out = append(out, Difference{Path: supplied.Path,
			Reason: "Delivery milestone, place or time zone differs; no transport time or event equivalence was assumed"})
	}
	if req.Resolved && sup.Resolved && sup.Date.Compare(req.Date) > 0 {
		out = append(out, Difference{Path: supplied.Path, Reason: "The supply date is after the required date"})
	}
	return out
}

// AnchorKinds are the verified revisions a delivery anchor may project (T02 domain acceptance
// 0.5.0): the anchor's ID is "<kind>:<object ID>".
var AnchorKinds = []string{"costing_context", "supplier_offer", "purchase_terms", "service_party_role", "logistics_offer"}

// ParseAnchor splits an anchor ID into the kind and ID of the revision it projects. An event kind
// without a mapping (order confirmations, payments, handovers) is DEPENDENCY_NOT_SUPPORTED until
// its slice delivers it; an ID longer than 64 characters is never truncated.
func ParseAnchor(path, id string) (kind, objectID string, err error) {
	kind, objectID, ok := strings.Cut(id, ":")
	switch {
	case len(id) > 64 || !ok || objectID == "":
		return "", "", fail(kernel.CodeSchemaInvalid, path, "An anchor ID is <kind>:<object ID>, at most 64 characters")
	case !slices.Contains(AnchorKinds, kind):
		return "", "", fail(CodeDependencyNotSupported, path, fmt.Sprintf("Anchor events of kind %q are not mapped yet", kind))
	}
	return kind, objectID, nil
}

func location(name string) (*time.Location, error) {
	if name == "" || name == "Local" {
		return nil, fmt.Errorf("delivery: time zone %q", name)
	}
	return time.LoadLocation(name)
}
