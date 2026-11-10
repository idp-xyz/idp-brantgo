// Package costing is the CostingContext aggregate (tactical model CST-AGG-CostingContext, T02 §4,
// §13.1, §13.2, §15.2): the RFQ a costing covers with its complete line set, the POL
// configurations it costs under, the comparison basis and the costing assumptions, identified by
// (tenant, company, context_id). Its content lives in revisions with monotonic numbers:
//
//	working → active      costing_context.verify: a human who did not contribute to the revision
//	active  → superseded  costing_context.verify of the next revision, in the same transaction
//	active, superseded → withdrawn   costing_context.withdraw
//	working → discarded   costing_context.discard_draft
//
// One working revision is saved whole (costing_context.save_draft) or copied from a sealed one
// (costing_context.revise); sealed revisions never change. Verifying binds the RFQ as INQ said it
// is then (RfqRef v1: ready, at the bound row version, with the hash of its line set). An actual
// context covers the RFQ's lines exactly; a what_if context keeps their identity and changes
// quantities (CST-INV-001 input, T02 §13.1). Assumptions stay unverified: any assumption keeps a
// result provisional, so it is never frozen (T02 §15.2).
//
// The upstream facts (INQ, CAT, POL) are read before CST's transaction and judged here as Facts;
// the transaction re-checks the RFQ and the configurations through the upstream guards. Checks run
// in the Python order: existence, root version, state, completeness, independence, effect of the
// references, then the document.
package costing

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // time zones do not depend on the host's zone database

	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/delivery"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Codes of the CST error catalog rows this package decides; package api declares the same.
const (
	CodeCostInputNotReady       = "COST_INPUT_NOT_READY"
	CodeComparisonBasisMismatch = "COMPARISON_BASIS_MISMATCH"
	CodeDependencyNotSupported  = "DEPENDENCY_NOT_SUPPORTED"
	CodeReferenceStale          = "REFERENCE_STALE"
)

// Categories maps the codes above to their error catalog category.
var Categories = map[string]kernel.Category{
	CodeCostInputNotReady:       kernel.CategoryInvalid,
	CodeComparisonBasisMismatch: kernel.CategoryInvalid,
	CodeDependencyNotSupported:  kernel.CategoryInvalid,
	CodeReferenceStale:          kernel.CategoryConflict,
}

func fail(code, path, reason string) error {
	if c, ok := Categories[code]; ok {
		return kernel.RejectAs(code, c, path, reason)
	}
	return kernel.Reject(code, path, reason)
}

// Status of a revision.
type Status string

const (
	Working    Status = "working"
	Active     Status = "active"
	Superseded Status = "superseded"
	Withdrawn  Status = "withdrawn"
	Discarded  Status = "discarded"
)

// Sealed reports whether a revision with status s was verified.
func (s Status) Sealed() bool { return s == Active || s == Superseded || s == Withdrawn }

// Kinds of context.
const (
	Actual = "actual"
	WhatIf = "what_if"
)

// POL kinds each field takes, and the kinds of the other references.
const (
	KindCurrency       = "currency"
	KindPricingPolicy  = "pricing_policy"
	KindUnit           = "unit"
	KindUnitConversion = "unit_conversion"
	KindFeeCategory    = "fee_category"
	KindTradeTerms     = "trade_terms"
	KindPaymentTerms   = "payment_terms"
	KindCalendar       = "calendar"
	KindLocation       = "location"
	KindDeliveryAnchor = "delivery_anchor"
)

// Ref is a typed reference to an exact revision: a POL configuration, a place or a delivery anchor.
type Ref struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
}

func (r Ref) String() string { return r.Kind + ":" + r.ID + "@" + strconv.FormatInt(r.Revision, 10) }

// RfqRef is the INQ RFQ and the row version a context binds.
type RfqRef struct {
	RfqID      string `json:"id"`
	RowVersion int64  `json:"row_version"`
}

// Line is one demand line.
type Line struct {
	LineID          string `json:"line_id"`
	SkuID           string `json:"sku_id"`
	SkuSpecRevision int64  `json:"sku_spec_revision"`
	Quantity        string `json:"quantity"`
	Unit            Ref    `json:"unit_ref"`
	Conversions     []Ref  `json:"conversion_refs,omitempty"`
}

// Delivery is the delivery requirement.
type Delivery struct {
	Kind      string `json:"kind"`
	Date      string `json:"date,omitempty"`
	Anchor    *Ref   `json:"anchor_ref,omitempty"`
	Days      int    `json:"days,omitempty"`
	DayBasis  string `json:"day_basis,omitempty"`
	Calendar  *Ref   `json:"calendar_ref,omitempty"`
	Milestone string `json:"milestone"`
	Place     Ref    `json:"place_ref"`
	TimeZone  string `json:"time_zone"`
}

// Basis is the comparison basis.
type Basis struct {
	SpecificationBasis string   `json:"specification_basis"`
	NamedPlace         Ref      `json:"named_place_ref"`
	RequiredFeeTypes   []Ref    `json:"required_fee_type_refs,omitempty"`
	TaxBasis           string   `json:"tax_basis"`
	Delivery           Delivery `json:"delivery"`
	CostBasis          string   `json:"cost_basis"`
	SalesTradeTerms    Ref      `json:"sales_trade_terms_ref"`
	SalesPaymentTerms  Ref      `json:"sales_payment_terms_ref"`
}

// Evidence is a source and a locator in it.
type Evidence struct {
	SourceID string `json:"source_id"`
	Locator  string `json:"locator"`
}

// Assumption annotates one typed input.
type Assumption struct {
	ID        string     `json:"id"`
	Path      string     `json:"path"`
	ValueText string     `json:"value_text"`
	Reason    string     `json:"reason"`
	Evidence  []Evidence `json:"evidence"`
}

// Data is the content of a revision, stored as this JSON document.
type Data struct {
	Rfq                   *RfqRef      `json:"rfq_ref,omitempty"`
	Kind                  string       `json:"context_kind,omitempty"`
	Lines                 []Line       `json:"lines,omitempty"`
	CalculationCurrency   *Ref         `json:"calculation_currency_ref,omitempty"`
	PricingPolicy         *Ref         `json:"pricing_policy_ref,omitempty"`
	Basis                 *Basis       `json:"comparison_basis,omitempty"`
	RequestedCurrency     *Ref         `json:"requested_currency_ref,omitempty"`
	RequestedTradeTerms   *Ref         `json:"requested_trade_terms_ref,omitempty"`
	RequestedPaymentTerms *Ref         `json:"requested_payment_terms_ref,omitempty"`
	Assumptions           []Assumption `json:"assumptions,omitempty"`
}

// Hash returns the content hash: the SHA-256 of the canonical JSON of the data. Field order is
// fixed by the Go types, so equal content always hashes equally.
func (d Data) Hash() string {
	doc, err := json.Marshal(d)
	if err != nil {
		panic(fmt.Sprintf("costing: data does not encode: %v", err)) // only strings, integers and lists
	}
	sum := sha256.Sum256(doc)
	return hex.EncodeToString(sum[:])
}

// Field is a POL reference of the data with the kind its field takes and its path in the request.
type Field struct {
	Ref  Ref
	Kind string
	Path string
}

// ConfigFields returns every POL configuration reference of d in document order.
func (d Data) ConfigFields() []Field {
	const p = "/args/data"
	var out []Field
	add := func(r *Ref, kind, path string) {
		if r != nil {
			out = append(out, Field{Ref: *r, Kind: kind, Path: path})
		}
	}
	add(d.CalculationCurrency, KindCurrency, p+"/calculation_currency_ref")
	add(d.PricingPolicy, KindPricingPolicy, p+"/pricing_policy_ref")
	for i := range d.Lines {
		l := &d.Lines[i]
		lp := fmt.Sprintf("%s/lines/%d", p, i)
		add(&l.Unit, KindUnit, lp+"/unit_ref")
		for j := range l.Conversions {
			add(&l.Conversions[j], KindUnitConversion, fmt.Sprintf("%s/conversion_refs/%d", lp, j))
		}
	}
	if b := d.Basis; b != nil {
		bp := p + "/comparison_basis"
		for i := range b.RequiredFeeTypes {
			add(&b.RequiredFeeTypes[i], KindFeeCategory, fmt.Sprintf("%s/required_fee_type_refs/%d", bp, i))
		}
		add(b.Delivery.Calendar, KindCalendar, bp+"/delivery/calendar_ref")
		add(&b.SalesTradeTerms, KindTradeTerms, bp+"/sales_trade_terms_ref")
		add(&b.SalesPaymentTerms, KindPaymentTerms, bp+"/sales_payment_terms_ref")
	}
	add(d.RequestedCurrency, KindCurrency, p+"/requested_currency_ref")
	add(d.RequestedTradeTerms, KindTradeTerms, p+"/requested_trade_terms_ref")
	add(d.RequestedPaymentTerms, KindPaymentTerms, p+"/requested_payment_terms_ref")
	return out
}

// ConfigRefs returns the distinct POL references of d sorted by kind, ID and revision: the order
// in which a transaction guards them.
func (d Data) ConfigRefs() []Ref {
	var out []Ref
	for _, f := range d.ConfigFields() {
		out = append(out, f.Ref)
	}
	slices.SortFunc(out, compareRefs)
	return slices.Compact(out)
}

func compareRefs(a, b Ref) int {
	if c := strings.Compare(a.Kind, b.Kind); c != 0 {
		return c
	}
	if c := strings.Compare(a.ID, b.ID); c != 0 {
		return c
	}
	return cmp.Compare(a.Revision, b.Revision)
}

// Specs returns the SKU specification revisions the lines name.
func (d Data) Specs() []Spec {
	var out []Spec
	for _, l := range d.Lines {
		out = append(out, Spec{SkuID: l.SkuID, Revision: l.SkuSpecRevision})
	}
	return out
}

// Spec is a CAT specification revision.
type Spec struct {
	SkuID    string
	Revision int64
}

// Proposal is who proposed an assumption and when.
type Proposal struct {
	By kernel.ActorRef
	At time.Time
}

// Binding is the RfqRef v1 a verification bound.
type Binding struct {
	RfqID      string
	RowVersion int64
	LinesHash  string
}

// Seal is a revision's verification: who verified it, on what evidence, and the RFQ as INQ gave it
// then (RfqRef v1: the binding and the complete line set).
type Seal struct {
	By       kernel.ActorRef
	Epoch    int64
	At       time.Time
	Evidence []Evidence
	Rfq      Binding
	RfqLines []RfqLine
}

// End is a revision's withdrawal or discarding.
type End struct {
	By       kernel.ActorRef
	Epoch    int64
	At       time.Time
	Reason   string
	Evidence []Evidence // withdrawal only
}

// Revision is one revision of a costing context.
type Revision struct {
	Number       int64
	Status       Status
	Base         int64
	Data         Data
	Hash         string
	Contributors []string
	SavedBy      kernel.ActorRef
	SavedEpoch   int64
	SavedAt      time.Time
	Proposals    map[string]Proposal
	Seal         *Seal
	SupersededAt time.Time
	End          *End

	// Fresh and Changed tell the store what a command did to the revision: Fresh revisions are
	// inserted, Changed ones updated.
	Fresh   bool
	Changed bool
}

// Context is the aggregate root.
type Context struct {
	ID           string
	RootVersion  int64
	LastRevision int64
	OwnerActor   string
	OwnerTeam    string
	Revisions    []*Revision // oldest first
}

// New returns an unsaved costing context the actor will own.
func New(id, ownerActor, ownerTeam string) *Context {
	return &Context{ID: id, OwnerActor: ownerActor, OwnerTeam: ownerTeam}
}

// Revision returns revision n, or nil.
func (c *Context) Revision(n int64) *Revision {
	for _, r := range c.Revisions {
		if r.Number == n {
			return r
		}
	}
	return nil
}

func (c *Context) with(s Status) *Revision {
	for _, r := range c.Revisions {
		if r.Status == s {
			return r
		}
	}
	return nil
}

// Working returns the working revision, or nil.
func (c *Context) Working() *Revision { return c.with(Working) }

// Active returns the active revision, or nil.
func (c *Context) Active() *Revision { return c.with(Active) }

func number(r *Revision) int64 {
	if r == nil {
		return 0
	}
	return r.Number
}

// CheckSave admits a whole-draft save (Python RevisionModel.save_draft): a missing context only at
// expected version 0; an existing one visible, at its current root version, with a working
// revision or without any sealed revision (a sealed context is revised first).
func CheckSave(c *Context, visible bool, expected int64) error {
	if c == nil {
		if expected != 0 {
			return kernel.Reject(kernel.CodeNotFound, "/args/id", "")
		}
		return nil
	}
	if err := Check(c, visible, expected); err != nil {
		return err
	}
	if c.Working() == nil && slices.ContainsFunc(c.Revisions, func(r *Revision) bool { return r.Status.Sealed() }) {
		return kernel.Reject(kernel.CodeInvalidState, "/args/working_revision", "A verified costing context is revised, not saved")
	}
	return nil
}

// Check admits a command on an existing, visible context at its current root version.
func Check(c *Context, visible bool, expected int64) error {
	switch {
	case c == nil || !visible:
		return kernel.Reject(kernel.CodeNotFound, "/args/id", "")
	case c.RootVersion != expected:
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	return nil
}

// Save replaces the working revision's data whole, starting revision LastRevision+1 when there is
// none, and records by as a contributor. An assumption keeps its proposer while it stays the same;
// a new or changed one is proposed by by at now (Python costing_context.save_draft provenance).
func (c *Context) Save(d Data, by kernel.ActorRef, epoch int64, now time.Time) *Revision {
	w := c.Working()
	if w == nil {
		c.LastRevision++
		w = &Revision{Number: c.LastRevision, Status: Working, Fresh: true, Proposals: map[string]Proposal{}}
		c.Revisions = append(c.Revisions, w)
	}
	previous := map[string]Assumption{}
	for _, a := range w.Data.Assumptions {
		previous[a.ID] = a
	}
	proposals := map[string]Proposal{}
	for _, a := range d.Assumptions {
		if p, ok := w.Proposals[a.ID]; ok && equalAssumption(previous[a.ID], a) {
			proposals[a.ID] = p
		} else {
			proposals[a.ID] = Proposal{By: by, At: kernel.Instant(now)}
		}
	}
	w.Data, w.Hash, w.Proposals = d, d.Hash(), proposals
	if !slices.Contains(w.Contributors, by.ID) {
		w.Contributors = append(w.Contributors, by.ID)
	}
	w.SavedBy, w.SavedEpoch, w.SavedAt, w.Changed = by, epoch, kernel.Instant(now), true
	c.RootVersion++
	return w
}

func equalAssumption(a, b Assumption) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// Revise copies sealed revision base into a new working revision by by; the active revision stays
// (Python RevisionModel.revise). The copy keeps the assumptions' proposers.
func (c *Context) Revise(base int64, by kernel.ActorRef, epoch int64, now time.Time) (*Revision, error) {
	b := c.Revision(base)
	if c.Working() != nil || b == nil || !b.Status.Sealed() {
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/base_revision", "Only a verified revision is revised, while no working revision exists")
	}
	c.LastRevision++
	w := &Revision{Number: c.LastRevision, Status: Working, Base: base, Data: b.Data, Hash: b.Hash, Contributors: []string{by.ID},
		SavedBy: by, SavedEpoch: epoch, SavedAt: kernel.Instant(now), Proposals: map[string]Proposal{}, Fresh: true}
	for id, p := range b.Proposals {
		w.Proposals[id] = p
	}
	c.Revisions = append(c.Revisions, w)
	c.RootVersion++
	return w, nil
}

// Discard ends the working revision, which must be revision working (Python discard_draft).
func (c *Context) Discard(working int64, by kernel.ActorRef, epoch int64, now time.Time, reason string) (*Revision, error) {
	w := c.Working()
	if w == nil || w.Number != working {
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/working_revision", "")
	}
	w.Status, w.End, w.Changed = Discarded, &End{By: by, Epoch: epoch, At: kernel.Instant(now), Reason: reason}, true
	c.RootVersion++
	return w, nil
}

// Withdraw withdraws sealed revision target; it clears the active revision when it is the target
// and revives no older one (Python withdraw). A revision is withdrawn once.
func (c *Context) Withdraw(target int64, by kernel.ActorRef, epoch int64, now time.Time, reason string, evidence []Evidence) (*Revision, error) {
	t := c.Revision(target)
	if t == nil || (t.Status != Active && t.Status != Superseded) {
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/target_revision", "Only a verified revision that is not withdrawn yet is withdrawn")
	}
	t.Status, t.Changed = Withdrawn, true
	t.End = &End{By: by, Epoch: epoch, At: kernel.Instant(now), Reason: reason, Evidence: slices.Clone(evidence)}
	c.RootVersion++
	return t, nil
}

// CheckVerify admits the verification of revision working superseding revision supersedes (0 for
// none) by verifier, before any upstream fact is judged: the working revision, the active one, a
// complete content and an independent verifier (T02 §13.2, §17.2).
func (c *Context) CheckVerify(working, supersedes int64, verifier kernel.ActorRef) error {
	w := c.Working()
	if w == nil || w.Number != working {
		return kernel.Reject(kernel.CodeInvalidState, "/args/working_revision", "")
	}
	if number(c.Active()) != supersedes {
		return fail(CodeReferenceStale, "/args/supersedes_revision", "The revision to supersede is not the active one")
	}
	if err := w.Data.Complete(); err != nil {
		return err
	}
	if slices.Contains(w.Contributors, verifier.ID) {
		// A contributor to the revision does not verify it (T02 §17.2; the costing policy requires
		// independent review). PERMISSION_DENIED carries no field detail.
		return kernel.Reject(kernel.CodePermissionDenied, "/", "")
	}
	return nil
}

// Verify seals the working revision against facts, which INQ, CAT and POL gave before the
// transaction, and supersedes the active revision. The caller re-checks the binding and the
// configurations inside its transaction through RfqGuard and ConfigGuard.
func (c *Context) Verify(working, supersedes int64, verifier kernel.ActorRef, epoch int64, now time.Time, evidence []Evidence, f Facts) (*Revision, error) {
	if err := c.CheckVerify(working, supersedes, verifier); err != nil {
		return nil, err
	}
	w := c.Working()
	binding, err := Ready(w.Data, f)
	if err != nil {
		return nil, err
	}
	at := kernel.Instant(now)
	if a := c.Active(); a != nil {
		a.Status, a.SupersededAt, a.Changed = Superseded, at, true
	}
	w.Status, w.Changed = Active, true
	w.Seal = &Seal{By: verifier, Epoch: epoch, At: at, Evidence: slices.Clone(evidence), Rfq: binding, RfqLines: slices.Clone(f.Rfq.Lines)}
	c.RootVersion++
	return w, nil
}

// Complete refuses a revision that lacks what verification needs (Python _ready:
// COST_INPUT_NOT_READY at the first missing field).
func (d Data) Complete() error {
	const p = "/args/data"
	switch {
	case d.Rfq == nil:
		return fail(CodeCostInputNotReady, p+"/rfq_ref", "")
	case d.Kind == "":
		return fail(CodeCostInputNotReady, p+"/context_kind", "")
	case len(d.Lines) == 0:
		return fail(CodeCostInputNotReady, p+"/lines", "")
	case d.CalculationCurrency == nil:
		return fail(CodeCostInputNotReady, p+"/calculation_currency_ref", "")
	case d.PricingPolicy == nil:
		return fail(CodeCostInputNotReady, p+"/pricing_policy_ref", "")
	case d.Basis == nil:
		return fail(CodeCostInputNotReady, p+"/comparison_basis", "")
	}
	return nil
}

// Facts are what INQ, CAT and POL said about the references of a revision, each in a transaction
// of its own before CST's. A reference that is absent does not exist or lies outside the caller's
// data scope.
type Facts struct {
	Rfq     *Rfq // nil: unknown
	Skus    map[string]bool
	Specs   map[Spec]func(path string) error // CAT's standing of each specification revision: nil when current
	Configs map[Ref]Config
}

// Rfq is RfqRef v1 as INQ answered it.
type Rfq struct {
	RfqID      string
	RowVersion int64
	Ready      bool
	Lines      []RfqLine
	LinesHash  string
}

// RfqLine is one RFQ line.
type RfqLine struct {
	LineID          string
	SkuID           string
	SkuSpecRevision int64
	Quantity        string
	UOM             string
}

// Config is what POL said about one configuration revision at the instant of the command: Check is
// POL's verdict (nil when it is in force) and the other fields the data CST judges.
type Config struct {
	Check          func(path string) error
	Kind           string
	CurrencyCode   string      // currency
	PolicyCurrency string      // pricing_policy: its calculation currency code
	Unit           *Unit       // unit
	Conversion     *Conversion // unit_conversion
	TimeZone       string      // calendar
}

// Unit is a POL unit definition.
type Unit struct {
	Code      string
	Precision int
}

// Conversion is a POL unit conversion of one SKU specification revision.
type Conversion struct {
	SkuID        string
	SpecRevision int64
	From, To     string
}

// CheckDraft judges the content of a save (Python _access_document on a draft): the lines once, each
// reference of the kind its field takes, and every reference the draft states known to its
// upstream; assumptions once each with a supported path; delivery anchors of mapped kinds. A draft
// may reference an RFQ that is not ready or configurations not in force.
func CheckDraft(d Data, f Facts) error {
	const p = "/args/data"
	seen := map[string]bool{}
	for _, l := range d.Lines {
		if seen[l.LineID] {
			return kernel.Reject(kernel.CodeDuplicateLineID, p+"/lines", "")
		}
		seen[l.LineID] = true
	}
	for _, field := range d.ConfigFields() {
		if field.Ref.Kind != field.Kind {
			return kernel.Reject(kernel.CodeReferenceInvalid, field.Path+"/kind", "The reference is not of the kind its field takes")
		}
	}
	if d.Rfq != nil && (f.Rfq == nil || f.Rfq.RfqID != d.Rfq.RfqID) {
		return kernel.Reject(kernel.CodeReferenceInvalid, p+"/rfq_ref", "")
	}
	for i, l := range d.Lines {
		if !f.Skus[l.SkuID] {
			return kernel.Reject(kernel.CodeReferenceInvalid, fmt.Sprintf("%s/lines/%d/sku_id", p, i), "")
		}
	}
	for _, field := range d.ConfigFields() {
		if _, ok := f.Configs[field.Ref]; !ok {
			return kernel.Reject(kernel.CodeReferenceInvalid, field.Path, "")
		}
	}
	if b := d.Basis; b != nil && b.Delivery.Anchor != nil {
		if _, _, err := delivery.ParseAnchor(p+"/comparison_basis/delivery/anchor_ref/id", b.Delivery.Anchor.ID); err != nil {
			return err
		}
	}
	return checkAssumptions(d.Assumptions)
}

// allowedAssumption is the closed set of inputs an assumption may annotate (Python
// assumption_rules.ALLOWED, T02 §15.2): the context's quantities and delivery date or days, the
// selected supply conditions' unit prices and delivery, and logistics fees' amounts and rates.
var allowedAssumption = regexp.MustCompile(`^/(?:context/(?:lines/[0-9]+/quantity|comparison_basis/delivery/(?:date|days))|` +
	`(?:supplier_offer|purchase_terms)/[A-Za-z0-9._:-]+/lines/[0-9]+/(?:tiers/[0-9]+/unit_price|delivery/(?:date|days))|` +
	`logistics_offer/[A-Za-z0-9._:-]+/fees/[0-9]+/billing/(?:amount|rate|minimum_amount))$`)

func checkAssumptions(as []Assumption) error {
	const p = "/args/data/assumptions"
	ids, paths := map[string]bool{}, map[string]bool{}
	for i, a := range as {
		if ids[a.ID] || paths[a.Path] {
			return kernel.Reject(kernel.CodeDuplicateLineID, p, "")
		}
		ids[a.ID], paths[a.Path] = true, true
		if !allowedAssumption.MatchString(a.Path) {
			return fail(CodeDependencyNotSupported, fmt.Sprintf("%s/%d/path", p, i), "Assumptions on this input are not supported")
		}
	}
	return nil
}

// Ready judges a complete revision against the facts for verification (Python _document_ready of
// costing_context) and returns the RFQ binding: the references in effect, the RFQ's complete line
// set covered (exactly for an actual context; same lines, SKUs and units for what_if), quantities
// within their units, the pricing policy's calculation currency, conversions of the line's SKU,
// a known delivery time zone and calendar, and assumptions on the context that state its values.
func Ready(d Data, f Facts) (Binding, error) {
	const p = "/args/data"
	if err := d.Complete(); err != nil {
		return Binding{}, err
	}
	if err := CheckDraft(d, f); err != nil {
		return Binding{}, err
	}
	rfq := f.Rfq
	if !rfq.Ready || rfq.RowVersion != d.Rfq.RowVersion {
		return Binding{}, fail(CodeReferenceStale, p+"/rfq_ref", "The RFQ is not ready at the bound row version")
	}
	for _, field := range d.ConfigFields() {
		if check := f.Configs[field.Ref].Check; check != nil {
			if err := check(field.Path); err != nil {
				return Binding{}, err
			}
		}
	}
	for i, s := range d.Specs() {
		if check := f.Specs[s]; check != nil {
			if err := check(fmt.Sprintf("%s/lines/%d/sku_spec_revision", p, i)); err != nil {
				return Binding{}, err
			}
		} else if _, ok := f.Specs[s]; !ok {
			return Binding{}, kernel.Reject(kernel.CodeReferenceInvalid, fmt.Sprintf("%s/lines/%d/sku_spec_revision", p, i), "")
		}
	}
	if err := cover(d, rfq, f); err != nil {
		return Binding{}, err
	}
	if err := policy(d, f); err != nil {
		return Binding{}, err
	}
	if err := checkDelivery(d.Basis.Delivery, f, p+"/comparison_basis/delivery"); err != nil {
		return Binding{}, err
	}
	if err := contextAssumptions(d); err != nil {
		return Binding{}, err
	}
	return Binding{RfqID: rfq.RfqID, RowVersion: rfq.RowVersion, LinesHash: rfq.LinesHash}, nil
}

// cover checks the lines against the RFQ's complete line set (T02 §13.1: SKU, specification and
// normalized quantity line by line; a what_if context may change quantities only).
func cover(d Data, rfq *Rfq, f Facts) error {
	const p = "/args/data/lines"
	if len(d.Lines) != len(rfq.Lines) {
		return fail(CodeComparisonBasisMismatch, p, "The costing context covers the RFQ's complete line set")
	}
	lines := map[string]RfqLine{}
	for _, l := range rfq.Lines {
		lines[l.LineID] = l
	}
	for i, l := range d.Lines {
		path := fmt.Sprintf("%s/%d", p, i)
		r, ok := lines[l.LineID]
		if !ok {
			return fail(CodeComparisonBasisMismatch, p, "The costing context covers the RFQ's complete line set")
		}
		unit := f.Configs[l.Unit].Unit
		if unit == nil {
			return kernel.Reject(kernel.CodeReferenceInvalid, path+"/unit_ref", "")
		}
		if l.SkuID != r.SkuID || l.SkuSpecRevision != r.SkuSpecRevision || unit.Code != r.UOM {
			return fail(CodeComparisonBasisMismatch, path, "SKU, specification and unit equal the RFQ line's")
		}
		q, err := kernel.ParseDecimal(l.Quantity, kernel.QuantityFormat)
		if err != nil || q.Sign() <= 0 || q.Cmp(kernel.DecimalFromInt(1_000_000)) > 0 || !q.RoundHalfUp(min(6, unit.Precision)).Equal(q) {
			return kernel.Reject(kernel.CodeInvalidKnownValue, path+"/quantity", "The quantity is positive, at most 1000000, within its unit's precision")
		}
		if d.Kind == Actual {
			want, err := kernel.ParseDecimal(r.Quantity, kernel.QuantityFormat)
			if err != nil || !want.Equal(q) {
				return fail(CodeComparisonBasisMismatch, path+"/quantity", "An actual costing context costs the RFQ's quantity; a what_if context another")
			}
		}
		for j, ref := range l.Conversions {
			c := f.Configs[ref].Conversion
			if c == nil || c.SkuID != l.SkuID || c.SpecRevision != l.SkuSpecRevision || (c.From != unit.Code && c.To != unit.Code) {
				return kernel.Reject(kernel.CodeReferenceInvalid, fmt.Sprintf("%s/conversion_refs/%d", path, j),
					"The conversion is not one of this SKU specification and unit")
			}
		}
	}
	return nil
}

func policy(d Data, f Facts) error {
	currency, pol := f.Configs[*d.CalculationCurrency], f.Configs[*d.PricingPolicy]
	if pol.PolicyCurrency == "" || pol.PolicyCurrency != currency.CurrencyCode {
		return kernel.Reject(kernel.CodeReferenceInvalid, "/args/data/pricing_policy_ref", "The pricing policy calculates in another currency")
	}
	return nil
}

// checkDelivery admits the delivery requirement for verification: a known IANA time zone; for a
// lead time, an anchor of a mapped kind and a calendar of the same time zone. The date it resolves
// to is the evaluation's (package delivery).
func checkDelivery(dv Delivery, f Facts, p string) error {
	if dv.TimeZone == "Local" {
		return kernel.Reject(kernel.CodeInvalidKnownValue, p+"/time_zone", "Unknown IANA time zone")
	}
	if _, err := time.LoadLocation(dv.TimeZone); err != nil {
		return kernel.Reject(kernel.CodeInvalidKnownValue, p+"/time_zone", "Unknown IANA time zone")
	}
	if dv.Kind != delivery.KindLeadTime {
		return nil
	}
	if f.Configs[*dv.Calendar].TimeZone != dv.TimeZone {
		return kernel.Reject(kernel.CodeInvalidKnownValue, p+"/calendar_ref", "The calendar's time zone differs from the delivery's")
	}
	return nil
}

// contextAssumptions checks the assumptions on the context's own inputs: the target exists and the
// assumption states its explicit value (Python bind_assumptions). Assumptions on supply conditions
// and fees are bound when a scenario selects them.
func contextAssumptions(d Data) error {
	for i, a := range d.Assumptions {
		rest, ok := strings.CutPrefix(a.Path, "/context/")
		if !ok {
			continue
		}
		p := fmt.Sprintf("/args/data/assumptions/%d", i)
		value, found := "", false
		parts := strings.Split(rest, "/")
		switch {
		case parts[0] == "lines":
			n, err := strconv.Atoi(parts[1])
			if found = err == nil && n >= 0 && n < len(d.Lines); found {
				value = d.Lines[n].Quantity
			}
		case parts[len(parts)-1] == "date":
			value, found = d.Basis.Delivery.Date, d.Basis.Delivery.Kind == delivery.KindFixed
		default: // days
			value, found = strconv.Itoa(d.Basis.Delivery.Days), d.Basis.Delivery.Kind == delivery.KindLeadTime
		}
		if !found {
			return kernel.Reject(kernel.CodeReferenceInvalid, p+"/path", "The assumption names no input of the context")
		}
		if a.ValueText != value {
			return kernel.Reject(kernel.CodeInvalidKnownValue, p+"/value_text", "The assumption states the input's explicit value")
		}
	}
	return nil
}
