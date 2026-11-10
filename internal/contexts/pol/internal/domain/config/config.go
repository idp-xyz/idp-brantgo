// Package config is the ConfigurationRoot aggregate: one configuration of a closed kind, such as a
// currency or an exchange rate, identified by (tenant, company, kind, config_id). Its content lives
// in revisions with monotonic numbers (SM-POL-REVISION):
//
//	working  → active      config.publish: a human other than the last editor (POL-INV-001)
//	active   → superseded  config.publish of the next revision, in the same transaction
//	active   → retired     config.retire
//	working  → discarded   config.discard_draft
//
// One working revision is saved whole until it is published or discarded; published revisions
// never change. A currency or unit keeps the meaning of its first publication (POL-INV-002); an
// exchange rate applies only in its direction and for its purpose (POL-INV-003); payment stages are
// positive and sum to 100 (POL-INV-004). Checks run in the Python order: existence, root version,
// state, independence, content, validity, references, definition.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"time"
	_ "time/tzdata" // sequence time zones do not depend on the host's zone database

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Codes of POL's error catalog rows that configuration rules decide; package api declares the
// same codes for callers.
const (
	CodeIndependentReviewRequired = "INDEPENDENT_REVIEW_REQUIRED"
	CodeImmutableDefinition       = "IMMUTABLE_DEFINITION"
	CodeFXScopeMismatch           = "FX_SCOPE_MISMATCH"
	CodeFXStale                   = "FX_STALE"
	CodePaymentTotalMismatch      = "PAYMENT_TOTAL_MISMATCH"
	CodeConfigNotActive           = "CONFIG_NOT_ACTIVE"
	CodeConfigExpired             = "CONFIG_EXPIRED"
)

// Categories are the categories of the codes above (all 422 in the error catalog).
var Categories = map[string]kernel.Category{
	CodeIndependentReviewRequired: kernel.CategoryInvalid,
	CodeImmutableDefinition:       kernel.CategoryInvalid,
	CodeFXScopeMismatch:           kernel.CategoryInvalid,
	CodeFXStale:                   kernel.CategoryInvalid,
	CodePaymentTotalMismatch:      kernel.CategoryInvalid,
	CodeConfigNotActive:           kernel.CategoryInvalid,
	CodeConfigExpired:             kernel.CategoryInvalid,
}

func reject(code, path string) error { return kernel.RejectAs(code, Categories[code], path, "") }

// Repath returns the rejection in err at path instead of its own; other errors pass unchanged.
func Repath(err error, path string) error {
	if rej, ok := kernel.AsRejection(err); ok {
		return kernel.RejectAs(rej.Code, rej.Category, path, "")
	}
	return err
}

// Kind is a configuration kind. The domain knows the kinds it has rules for.
type Kind string

const (
	KindCurrency       Kind = "currency"
	KindUnit           Kind = "unit"
	KindFXRate         Kind = "fx_rate"
	KindPaymentTerms   Kind = "payment_terms"
	KindNumberSequence Kind = "number_sequence"

	KindScreeningRegulation Kind = "screening_regulation"
)

// JurisdictionForeign is the jurisdiction of a screening list a tenant enables only with its legal
// confirmation (ADR-BR-017, SR-19).
const JurisdictionForeign = "foreign"

// Status of a revision.
type Status string

const (
	Working    Status = "working"
	Active     Status = "active"
	Superseded Status = "superseded"
	Retired    Status = "retired"
	Discarded  Status = "discarded"
)

// Currency is an ISO 4217 currency; code, minor units and rounding are its meaning.
type Currency struct {
	Code       string `json:"code"`
	Name       string `json:"name"`
	MinorUnits int    `json:"minor_units"`
	Rounding   string `json:"rounding"`
}

// Unit is a unit of measure; code, dimension and precision are its meaning.
type Unit struct {
	Code      string `json:"code"`
	Name      string `json:"name"`
	Dimension string `json:"dimension"`
	Precision int    `json:"precision"`
}

// FXRate is one unit of FromCurrency in ToCurrency, for one purpose. ObservedAt is a canonical
// timestamp (kernel.FormatTimestamp).
type FXRate struct {
	FromCurrency string `json:"from_currency"`
	ToCurrency   string `json:"to_currency"`
	Rate         string `json:"rate"`
	Purpose      string `json:"purpose"`
	SourceName   string `json:"source_name"`
	ObservedAt   string `json:"observed_at"`
}

// PaymentTerms are payment stages.
type PaymentTerms struct {
	Name   string         `json:"name"`
	Stages []PaymentStage `json:"stages"`
}

// PaymentStage is one stage of PaymentTerms.
type PaymentStage struct {
	StageID string `json:"stage_id"`
	Percent string `json:"percent"`
	Trigger string `json:"trigger"`
	Days    int    `json:"days"`
}

// NumberSequence is how document numbers of one document type are formed.
type NumberSequence struct {
	DocumentType string `json:"document_type"`
	Prefix       string `json:"prefix"`
	Width        int    `json:"width"`
	Reset        string `json:"reset"`
	TimeZone     string `json:"time_zone"`
}

// ScreeningRegulation is the screening lists a company enables and its re-screening interval.
type ScreeningRegulation struct {
	Lists                []ScreeningList `json:"lists"`
	RescreenIntervalDays int             `json:"rescreen_interval_days"`
}

// ScreeningList is one enabled list; a foreign list carries the EVD source of the tenant's legal
// confirmation.
type ScreeningList struct {
	ListCode                    string `json:"list_code"`
	Jurisdiction                string `json:"jurisdiction"`
	LegalConfirmationEvidenceID string `json:"legal_confirmation_evidence_id,omitempty"`
}

// Content is the whole content of a revision: its kind, its validity interval and the data of its
// kind, of which exactly the member of Kind is set. Times are kernel.Instant values.
type Content struct {
	Kind           Kind
	ValidFrom      time.Time
	ValidUntil     time.Time // zero: open-ended
	Currency       *Currency
	Unit           *Unit
	FXRate         *FXRate
	PaymentTerms   *PaymentTerms
	NumberSequence *NumberSequence

	ScreeningRegulation *ScreeningRegulation
}

// Validity returns the revision's validity interval.
func (c Content) Validity() kernel.ValidityInterval {
	return kernel.ValidityInterval{From: c.ValidFrom, Until: c.ValidUntil}
}

// Data returns the member of c.Kind.
func (c Content) Data() any {
	switch c.Kind {
	case KindCurrency:
		return c.Currency
	case KindUnit:
		return c.Unit
	case KindFXRate:
		return c.FXRate
	case KindPaymentTerms:
		return c.PaymentTerms
	case KindNumberSequence:
		return c.NumberSequence
	case KindScreeningRegulation:
		return c.ScreeningRegulation
	}
	return nil
}

// DataJSON returns the data of c as stored.
func (c Content) DataJSON() ([]byte, error) {
	if c.Data() == nil {
		return nil, fmt.Errorf("pol: kind %q has no data", c.Kind)
	}
	return json.Marshal(c.Data())
}

// Hash returns the content hash: the SHA-256 of the canonical JSON of kind, validity and data.
// Field order is fixed by the Go types, so equal content always hashes equally.
func (c Content) Hash() (string, error) {
	data, err := c.DataJSON()
	if err != nil {
		return "", err
	}
	until := ""
	if !c.ValidUntil.IsZero() {
		until = kernel.FormatTimestamp(c.ValidUntil)
	}
	doc, err := json.Marshal(struct {
		Kind       Kind            `json:"kind"`
		ValidFrom  string          `json:"valid_from"`
		ValidUntil string          `json:"valid_until,omitempty"`
		Data       json.RawMessage `json:"data"`
	}{c.Kind, kernel.FormatTimestamp(c.ValidFrom), until, data})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(doc)
	return hex.EncodeToString(sum[:]), nil
}

// DecodeData sets the member of c.Kind from stored data.
func (c *Content) DecodeData(raw []byte) error {
	var target any
	switch c.Kind {
	case KindCurrency:
		c.Currency = &Currency{}
		target = c.Currency
	case KindUnit:
		c.Unit = &Unit{}
		target = c.Unit
	case KindFXRate:
		c.FXRate = &FXRate{}
		target = c.FXRate
	case KindPaymentTerms:
		c.PaymentTerms = &PaymentTerms{}
		target = c.PaymentTerms
	case KindNumberSequence:
		c.NumberSequence = &NumberSequence{}
		target = c.NumberSequence
	case KindScreeningRegulation:
		c.ScreeningRegulation = &ScreeningRegulation{}
		target = c.ScreeningRegulation
	default:
		return fmt.Errorf("pol: no data of kind %q", c.Kind)
	}
	return json.Unmarshal(raw, target)
}

// Validate judges content the contract admitted (Python foundation.validate): a bounded validity
// that ends after it starts, a currency or unit whose config_id is its code, an exchange rate
// between two currencies with a positive rate observed no later than it becomes valid, payment
// stages with distinct IDs and positive percentages summing to 100, and a sequence time zone that
// exists.
func Validate(id string, c Content) error {
	if c.Data() == nil {
		return kernel.Reject(kernel.CodeSchemaInvalid, "/args/data", "")
	}
	if !c.Validity().Valid() {
		return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/valid_until", "")
	}
	switch c.Kind {
	case KindCurrency:
		if id != c.Currency.Code {
			return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/id", "Currency id must equal its code")
		}
	case KindUnit:
		if id != c.Unit.Code {
			return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/id", "Unit id must equal its code")
		}
	case KindFXRate:
		x := c.FXRate
		if x.FromCurrency == x.ToCurrency {
			return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/data/to_currency", "Same currency is an explicit 1, not a rate")
		}
		if rate, err := kernel.ParseDecimal(x.Rate, kernel.FactorFormat); err != nil || rate.Sign() <= 0 {
			return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/data/rate", "")
		}
		if c.ValidUntil.IsZero() {
			return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/valid_until", "An exchange rate expires")
		}
		observed, err := kernel.ParseTimestamp(x.ObservedAt)
		if err != nil || observed.After(c.ValidFrom) {
			return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/data/observed_at", "")
		}
	case KindPaymentTerms:
		return checkStages(c.PaymentTerms.Stages)
	case KindNumberSequence:
		if tz := c.NumberSequence.TimeZone; tz == "Local" {
			return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/data/time_zone", "")
		} else if _, err := time.LoadLocation(tz); err != nil {
			return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/data/time_zone", "")
		}
	case KindScreeningRegulation:
		for i, l := range c.ScreeningRegulation.Lists {
			if l.Jurisdiction == JurisdictionForeign && l.LegalConfirmationEvidenceID == "" {
				return kernel.Reject(kernel.CodeInvalidKnownValue, fmt.Sprintf("/args/data/lists/%d/legal_confirmation_evidence_id", i),
					"A foreign list needs the tenant's legal confirmation")
			}
		}
	}
	return nil
}

// CheckLegalConfirmations requires a screening regulation's publication to cite, among its
// evidence, the legal confirmation of every list that has one, so the publisher must be able to
// read it like the rest of the evidence (ADR-BR-017, Context Map R63).
func CheckLegalConfirmations(c Content, evidence []string) error {
	if c.Kind != KindScreeningRegulation {
		return nil
	}
	for _, l := range c.ScreeningRegulation.Lists {
		if id := l.LegalConfirmationEvidenceID; id != "" && !slices.Contains(evidence, id) {
			return kernel.Reject(kernel.CodeReferenceInvalid, "/args/evidence_ids",
				"Cite the legal confirmation of screening list "+l.ListCode)
		}
	}
	return nil
}

// checkStages enforces POL-INV-004: distinct stage IDs, every percentage above 0 and exactly 100 in
// total, compared exactly.
func checkStages(stages []PaymentStage) error {
	seen := map[string]bool{}
	for _, s := range stages {
		if seen[s.StageID] {
			return kernel.Reject(kernel.CodeInvalidKnownValue, "/args/data/stages", "Stage IDs must be distinct")
		}
		seen[s.StageID] = true
	}
	total := kernel.DecimalFromInt(0)
	for _, s := range stages {
		// The contract admitted PercentFormat; FactorFormat accepts every such string.
		p, err := kernel.ParseDecimal(s.Percent, kernel.FactorFormat)
		if err != nil || p.Sign() <= 0 {
			return reject(CodePaymentTotalMismatch, "/args/data/stages")
		}
		total = total.Add(p)
	}
	if !total.Equal(kernel.DecimalFromInt(100)) {
		return reject(CodePaymentTotalMismatch, "/args/data/stages")
	}
	return nil
}

// Publication is the independent review that made a revision active.
type Publication struct {
	By          kernel.ActorRef
	Epoch       int64 // the authority epoch the publication saw
	At          time.Time
	EvidenceIDs []string
}

// Revision is one revision of a configuration. SavedBy is its last editor.
type Revision struct {
	Number      int64
	Status      Status
	Content     Content
	Hash        string
	SavedBy     kernel.ActorRef
	SavedEpoch  int64
	SavedAt     time.Time
	Publication *Publication // nil until published
	EndedAt     time.Time    // when it was superseded, retired or discarded
}

// Root is the aggregate root. Version is the root version every command compares and increments;
// LastRevision the highest revision number ever assigned. Working and Active are the revisions in
// those states; Definition is the latest revision ever published, whatever its state now.
type Root struct {
	Kind         Kind
	ID           string
	Version      int64
	LastRevision int64
	Working      *Revision
	Active       *Revision
	Definition   *Revision
}

// New returns a configuration that is not stored yet.
func New(kind Kind, id string) *Root { return &Root{Kind: kind, ID: id} }

// CheckSave admits a whole-revision save: a new configuration only at expected version 0, an
// existing one only at its current root version.
func CheckSave(r *Root, expected int64) error {
	if r == nil {
		if expected != 0 {
			return kernel.Reject(kernel.CodeNotFound, "/args/id", "")
		}
		return nil
	}
	return Check(r, expected)
}

// Check admits a command on an existing configuration at its current root version.
func Check(r *Root, expected int64) error {
	switch {
	case r == nil:
		return kernel.Reject(kernel.CodeNotFound, "/args/id", "")
	case r.Version != expected:
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	return nil
}

// Save replaces the working revision whole, or starts one with the next revision number, and makes
// by its last editor. It reports whether the revision is new.
func (r *Root) Save(c Content, by kernel.ActorRef, epoch int64, at time.Time) (*Revision, bool, error) {
	hash, err := c.Hash()
	if err != nil {
		return nil, false, err
	}
	created := r.Working == nil
	if created {
		r.LastRevision++
		r.Working = &Revision{Number: r.LastRevision, Status: Working}
	}
	w := r.Working
	w.Content, w.Hash, w.SavedBy, w.SavedEpoch, w.SavedAt = c, hash, by, epoch, at
	r.Version++
	return w, created, nil
}

// Facts are the states of the configurations a content refers to, at the instant of a save or a
// publication: for each currency code an exchange rate names, what InForce said of that currency
// (nil when in force). A code without an entry is not a configured currency.
type Facts struct {
	Currencies map[string]error
}

// CheckReferences requires the currencies of an exchange rate to be configured and in force
// (Python: every child reference is active at save and publication).
func CheckReferences(c Content, f Facts) error {
	if c.Kind != KindFXRate {
		return nil
	}
	for _, ref := range []struct{ code, path string }{
		{c.FXRate.FromCurrency, "/args/data/from_currency"}, {c.FXRate.ToCurrency, "/args/data/to_currency"},
	} {
		state, known := f.Currencies[ref.code]
		if !known {
			return kernel.Reject(kernel.CodeReferenceInvalid, ref.path, "")
		}
		if state != nil {
			return Repath(state, ref.path)
		}
	}
	return nil
}

// Publish makes working revision number active for the human by, superseding the active revision.
// Its last editor may not publish it (POL-INV-001); its content must still be valid, its validity
// must contain at (CONFIG_EXPIRED: an expired or future revision never replaces the active one), its
// references must be in force, a screening regulation's legal confirmations must be among the
// evidence, and a currency or unit keeps the definition of its latest publication, retired or not
// (POL-INV-002). It returns the superseded revision, if any.
func (r *Root) Publish(number int64, f Facts, by kernel.ActorRef, epoch int64, evidence []string, at time.Time) (*Revision, error) {
	w := r.Working
	switch {
	case number > r.LastRevision:
		return nil, kernel.Reject(kernel.CodeNotFound, "/args/revision", "")
	case w == nil || w.Number != number:
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/revision", "")
	case w.SavedBy.ID == by.ID:
		return nil, reject(CodeIndependentReviewRequired, "/args/revision")
	}
	if err := Validate(r.ID, w.Content); err != nil {
		return nil, err
	}
	if !w.Content.Validity().Contains(at) {
		return nil, reject(CodeConfigExpired, "/args/revision")
	}
	if err := CheckReferences(w.Content, f); err != nil {
		return nil, err
	}
	if err := CheckLegalConfirmations(w.Content, evidence); err != nil {
		return nil, err
	}
	if d := r.Definition; d != nil && !sameDefinition(d.Content, w.Content) {
		return nil, reject(CodeImmutableDefinition, "/args/revision")
	}
	superseded := r.Active
	if superseded != nil {
		superseded.Status, superseded.EndedAt = Superseded, at
	}
	w.Status = Active
	w.Publication = &Publication{By: by, Epoch: epoch, At: at, EvidenceIDs: evidence}
	r.Active, r.Working, r.Definition = w, nil, w
	r.Version++
	return superseded, nil
}

// sameDefinition reports whether b keeps the meaning of a (POL-INV-002); other kinds have no
// immutable definition.
func sameDefinition(a, b Content) bool {
	switch a.Kind {
	case KindCurrency:
		x, y := a.Currency, b.Currency
		return x.Code == y.Code && x.MinorUnits == y.MinorUnits && x.Rounding == y.Rounding
	case KindUnit:
		x, y := a.Unit, b.Unit
		return x.Code == y.Code && x.Dimension == y.Dimension && x.Precision == y.Precision
	}
	return true
}

// Discard ends the working revision; its number is not reused.
func (r *Root) Discard(at time.Time) (*Revision, error) {
	w := r.Working
	if w == nil {
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/id", "")
	}
	w.Status, w.EndedAt = Discarded, at
	r.Working = nil
	r.Version++
	return w, nil
}

// Retire ends the active revision; it stays readable as retired.
func (r *Root) Retire(at time.Time) (*Revision, error) {
	a := r.Active
	if a == nil {
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/id", "")
	}
	a.Status, a.EndedAt = Retired, at
	r.Active = nil
	r.Version++
	return a, nil
}

// published reports whether rev was the published revision of its configuration at instant at:
// published at or before at and not yet superseded or retired at at.
func published(rev *Revision, at time.Time) bool {
	return rev != nil && rev.Publication != nil && !at.Before(rev.Publication.At) && (rev.EndedAt.IsZero() || at.Before(rev.EndedAt))
}

// InForce reports whether rev is in force at instant at: CONFIG_NOT_ACTIVE unless it was the
// published revision at at (a nil revision never was), CONFIG_EXPIRED unless its validity contains
// at. The rejection's path is "/"; callers Repath it.
func InForce(rev *Revision, at time.Time) error {
	if !published(rev, at) {
		return reject(CodeConfigNotActive, "/")
	}
	if !rev.Content.Validity().Contains(at) {
		return reject(CodeConfigExpired, "/")
	}
	return nil
}

// FXUse is one use of an exchange rate: converting From into To for Purpose at instant At, with
// observations no older than MaxAge when MaxAge is positive.
type FXUse struct {
	From, To, Purpose string
	At                time.Time
	MaxAge            time.Duration
}

// DecideFX applies POL-INV-003 to use u of rate, the fx_rate revision the caller named (nil when
// it named none), and returns the rate. The same currency is exactly 1 and takes no rate; different
// currencies need a rate in force for exactly that direction and purpose: a missing rate is never 1
// and a rate never answers the opposite direction. A rate whose validity has ended, or whose
// observation is older than MaxAge, is FX_STALE. Paths are relative to the query (/rate, /purpose).
func DecideFX(u FXUse, rate *Revision) (kernel.Decimal, error) {
	if u.From == u.To {
		if rate != nil {
			return kernel.Decimal{}, reject(CodeFXScopeMismatch, "/rate")
		}
		return kernel.DecimalFromInt(1), nil
	}
	switch {
	case rate == nil:
		return kernel.Decimal{}, reject(CodeFXScopeMismatch, "/rate")
	case rate.Content.Kind != KindFXRate:
		return kernel.Decimal{}, kernel.Reject(kernel.CodeReferenceInvalid, "/rate", "")
	case !published(rate, u.At):
		return kernel.Decimal{}, reject(CodeConfigNotActive, "/rate")
	case u.At.Before(rate.Content.ValidFrom):
		return kernel.Decimal{}, reject(CodeConfigExpired, "/rate")
	case !rate.Content.Validity().Contains(u.At):
		return kernel.Decimal{}, reject(CodeFXStale, "/rate")
	}
	x := rate.Content.FXRate
	if x.Purpose != u.Purpose {
		return kernel.Decimal{}, reject(CodeFXScopeMismatch, "/purpose")
	}
	if x.FromCurrency != u.From || x.ToCurrency != u.To {
		return kernel.Decimal{}, reject(CodeFXScopeMismatch, "/rate")
	}
	observed, err := kernel.ParseTimestamp(x.ObservedAt)
	if err != nil {
		return kernel.Decimal{}, err
	}
	if u.MaxAge > 0 && u.At.Sub(observed) > u.MaxAge {
		return kernel.Decimal{}, reject(CodeFXStale, "/rate")
	}
	return kernel.ParseDecimal(x.Rate, kernel.FactorFormat)
}
