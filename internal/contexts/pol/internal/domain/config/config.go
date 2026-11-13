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
//
// A content refers to other configurations in two ways (kinds.go, References): a definition whose
// meaning POL-INV-002 fixes (a currency, a unit) by its code, resolved to the revision in force when
// it is used; a configuration whose content may change (a tax profile, a calendar) by its exact
// revision, a ConfigRef v1 the publisher reviewed.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
	_ "time/tzdata" // time zones do not depend on the host's zone database

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
	CodeUnitScopeMismatch         = "UNIT_SCOPE_MISMATCH"
	CodeTaxPolicyUnconfirmed      = "TAX_POLICY_UNCONFIRMED"
	CodeTemplateInvalid           = "TEMPLATE_INVALID"
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
	CodeUnitScopeMismatch:         kernel.CategoryInvalid,
	CodeTaxPolicyUnconfirmed:      kernel.CategoryInvalid,
	CodeTemplateInvalid:           kernel.CategoryInvalid,
}

// Reject returns a rejection with one of the codes above at path.
func Reject(code, path string) error { return kernel.RejectAs(code, Categories[code], path, "") }

func reject(code, path string) error { return Reject(code, path) }

// Repath returns the rejection in err at path instead of its own; other errors pass unchanged.
func Repath(err error, path string) error {
	if rej, ok := kernel.AsRejection(err); ok {
		return kernel.RejectAs(rej.Code, rej.Category, path, "")
	}
	return err
}

// Status of a revision.
type Status string

const (
	Working    Status = "working"
	Active     Status = "active"
	Superseded Status = "superseded"
	Retired    Status = "retired"
	Discarded  Status = "discarded"
)

// Content is the whole content of a revision: its kind, its validity interval and the data of its
// kind, of which exactly the member of Kind is set. Times are kernel.Instant values.
type Content struct {
	Kind                     Kind
	ValidFrom                time.Time
	ValidUntil               time.Time // zero: open-ended
	CompanyProfile           *CompanyProfile
	Currency                 *Currency
	Unit                     *Unit
	UnitConversion           *UnitConversion
	FXRate                   *FXRate
	Calendar                 *Calendar
	TradeTerms               *TradeTerms
	PaymentTerms             *PaymentTerms
	TaxProfile               *TaxProfile
	TaxApplication           *TaxApplication
	FeeCategory              *FeeCategory
	PricingPolicy            *PricingPolicy
	BankAccount              *BankAccount
	NumberSequence           *NumberSequence
	DocumentTemplate         *DocumentTemplate
	QuoteDocumentTemplate    *QuoteDocumentTemplate
	CreditPolicy             *CreditPolicy
	ScreeningRegulation      *ScreeningRegulation
	LegalTerms               *LegalTerms
	OrderDocumentTemplate    *OrderDocumentTemplate
	ShipmentDocumentTemplate *ShipmentDocumentTemplate
	ExportTaxPolicy          *ExportTaxPolicy
}

// Validity returns the revision's validity interval.
func (c Content) Validity() kernel.ValidityInterval {
	return kernel.ValidityInterval{From: c.ValidFrom, Until: c.ValidUntil}
}

// present returns p as an interface, and nil (not a typed nil pointer) when p is nil.
func present[T any](p *T) any {
	if p == nil {
		return nil
	}
	return p
}

// Data returns the member of c.Kind, or nil when it is not set.
func (c Content) Data() any {
	switch c.Kind {
	case KindCompanyProfile:
		return present(c.CompanyProfile)
	case KindCurrency:
		return present(c.Currency)
	case KindUnit:
		return present(c.Unit)
	case KindUnitConversion:
		return present(c.UnitConversion)
	case KindFXRate:
		return present(c.FXRate)
	case KindCalendar:
		return present(c.Calendar)
	case KindTradeTerms:
		return present(c.TradeTerms)
	case KindPaymentTerms:
		return present(c.PaymentTerms)
	case KindTaxProfile:
		return present(c.TaxProfile)
	case KindTaxApplication:
		return present(c.TaxApplication)
	case KindFeeCategory:
		return present(c.FeeCategory)
	case KindPricingPolicy:
		return present(c.PricingPolicy)
	case KindBankAccount:
		return present(c.BankAccount)
	case KindNumberSequence:
		return present(c.NumberSequence)
	case KindDocumentTemplate:
		return present(c.DocumentTemplate)
	case KindQuoteDocumentTemplate:
		return present(c.QuoteDocumentTemplate)
	case KindCreditPolicy:
		return present(c.CreditPolicy)
	case KindScreeningRegulation:
		return present(c.ScreeningRegulation)
	case KindLegalTerms:
		return present(c.LegalTerms)
	case KindOrderDocumentTemplate:
		return present(c.OrderDocumentTemplate)
	case KindShipmentDocumentTemplate:
		return present(c.ShipmentDocumentTemplate)
	case KindExportTaxPolicy:
		return present(c.ExportTaxPolicy)
	}
	return nil
}

// DataJSON returns the data of c as stored.
func (c Content) DataJSON() ([]byte, error) {
	data := c.Data()
	if data == nil {
		return nil, fmt.Errorf("pol: kind %q has no data", c.Kind)
	}
	return json.Marshal(data)
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

func decode[T any](raw []byte, member **T) error {
	v := new(T)
	if err := json.Unmarshal(raw, v); err != nil {
		return err
	}
	*member = v
	return nil
}

// DecodeData sets the member of c.Kind from stored data.
func (c *Content) DecodeData(raw []byte) error {
	switch c.Kind {
	case KindCompanyProfile:
		return decode(raw, &c.CompanyProfile)
	case KindCurrency:
		return decode(raw, &c.Currency)
	case KindUnit:
		return decode(raw, &c.Unit)
	case KindUnitConversion:
		return decode(raw, &c.UnitConversion)
	case KindFXRate:
		return decode(raw, &c.FXRate)
	case KindCalendar:
		return decode(raw, &c.Calendar)
	case KindTradeTerms:
		return decode(raw, &c.TradeTerms)
	case KindPaymentTerms:
		return decode(raw, &c.PaymentTerms)
	case KindTaxProfile:
		return decode(raw, &c.TaxProfile)
	case KindTaxApplication:
		return decode(raw, &c.TaxApplication)
	case KindFeeCategory:
		return decode(raw, &c.FeeCategory)
	case KindPricingPolicy:
		return decode(raw, &c.PricingPolicy)
	case KindBankAccount:
		return decode(raw, &c.BankAccount)
	case KindNumberSequence:
		return decode(raw, &c.NumberSequence)
	case KindDocumentTemplate:
		return decode(raw, &c.DocumentTemplate)
	case KindQuoteDocumentTemplate:
		return decode(raw, &c.QuoteDocumentTemplate)
	case KindCreditPolicy:
		return decode(raw, &c.CreditPolicy)
	case KindScreeningRegulation:
		return decode(raw, &c.ScreeningRegulation)
	case KindLegalTerms:
		return decode(raw, &c.LegalTerms)
	case KindOrderDocumentTemplate:
		return decode(raw, &c.OrderDocumentTemplate)
	case KindShipmentDocumentTemplate:
		return decode(raw, &c.ShipmentDocumentTemplate)
	case KindExportTaxPolicy:
		return decode(raw, &c.ExportTaxPolicy)
	}
	return fmt.Errorf("pol: no data of kind %q", c.Kind)
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

// Publish makes working revision number active for the human by, superseding the active revision.
// Its last editor may not publish it (POL-INV-001); its content must still be valid, its validity
// must contain at (CONFIG_EXPIRED: an expired or future revision never replaces the active one), its
// references must be in force, the sources it rests on (a screening regulation's legal
// confirmations, an export tax policy's professional review) must be among the evidence, and a
// currency or unit keeps the definition of its latest publication, retired or not (POL-INV-002). It
// returns the superseded revision, if any.
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
	if err := CheckReferences(r.ID, w.Content, f); err != nil {
		return nil, err
	}
	if err := CheckCitedEvidence(w.Content, evidence); err != nil {
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

// At returns the revision of one configuration that applies at instant at by its validity (ConfigAt
// v1, S5R-D23, POL-INV-007), or nil: among revisions, every revision of the configuration, the one
// published last of those ever published whose validity contains at. A retirement ends the
// configuration: once a revision is retired, at or before at, no revision published before that
// retirement applies at at, so an older superseded revision does not come back; one published after
// it does. Publication time is otherwise no condition: a revision published after at applies to it
// when its validity covers it (财税〔2012〕39号: the export date decides). brant_pol.config_at_v1
// (migrations/pol/000006) decides the same in SQL.
func At(revisions []*Revision, at time.Time) *Revision {
	var retiredAt time.Time // the latest retirement at or before at
	for _, r := range revisions {
		if r.Status == Retired && !r.EndedAt.After(at) && r.EndedAt.After(retiredAt) {
			retiredAt = r.EndedAt
		}
	}
	var applies *Revision
	for _, r := range revisions {
		p := r.Publication
		switch {
		case p == nil || (r.Status != Active && r.Status != Superseded && r.Status != Retired):
			continue
		case !retiredAt.IsZero() && !p.At.After(retiredAt):
			continue
		case !r.Content.Validity().Contains(at):
			continue
		}
		if applies == nil || p.At.After(applies.Publication.At) || (p.At.Equal(applies.Publication.At) && r.Number > applies.Number) {
			applies = r
		}
	}
	return applies
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
