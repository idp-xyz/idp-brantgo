package shipmentdocumentset

import (
	"math/big"
	"slices"
	"strconv"
	"strings"

	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipment"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The insurance policy record of a set (tactical model FUL-AGG-ShipmentDocumentSet entity
// InsurancePolicyRecord with value object InsuredAmount; FUL-INV-005; simulated walkthrough SIM-02
// step 8 and finding F-03; S5 decision D-13 "record-only"; WBS S5-16). A policy is an external
// document — the insurer's — recorded like a bill of lading: its data and an EVD file, versions of
// kind insurance_policy that are issued with the set. When the commercial invoice the set stands on
// states CIF or CIP the seller insures (Incoterms 2020 A5): the set needs a policy record (a missing
// one is a required difference of the reconciliation, SHIPMENT_DOCUMENTS_UNRECONCILED) whose insured
// amount is at least the contract percentage (110 % unless the contract asks more) of the invoice
// value, rounded HALF_UP to the currency's minor units (INSURANCE_COVERAGE_INSUFFICIENT otherwise,
// checked when the policy is recorded against the invoice then and when the set is issued against
// the invoice it is issued with). The premium is compared with the insurance component of the
// costing the order's quote adopted (CST, Context Map R78): a difference is shown, never refused.

// The kind of a policy record, the PTY service role kind that insures, the default cover and the
// outcomes of the premium comparison.
const (
	KindInsurancePolicy = "insurance_policy"
	ServiceInsurer      = "insurer"

	// DefaultCoveragePercent is the contract percentage when the contract states none: CIF and CIP
	// cover at least 110 % of the price (Incoterms 2020 A5; F-03 "CIF 通常按 CIF 价 110%").
	DefaultCoveragePercent = "110"

	// The premium against the costing's insurance component (D-13).
	PremiumEqual         = "equal"          // the same amount in the same currency
	PremiumDifferent     = "different"      // a difference, shown
	PremiumNotComparable = "not_comparable" // another currency: FUL converts nothing
	PremiumNoComponent   = "no_component"   // the costing has no insurance component
	PremiumUnavailable   = "unavailable"    // no costing CST shows, or one whose charges CST cannot all classify
)

var percentFormat = kernel.DecimalFormat{IntDigits: 4, Scale: 2}

// InsuranceRequired reports whether a sale on incoterm has the seller insure the goods: CIF and
// CIP (Incoterms 2020).
func InsuranceRequired(incoterm string) bool { return incoterm == "CIF" || incoterm == "CIP" }

// Insurer is the PTY service role revision of kind insurer a policy was recorded with
// (ServiceRoleRef v1, R44).
type Insurer struct {
	PartyID       string `json:"party_id"`
	ServiceRoleID string `json:"service_role_id"`
	Revision      int64  `json:"revision"`
}

// InsurancePolicy is what an external insurance policy states (InsurancePolicyRecord): the
// insurer, the policy number, the clauses (risks) covered, the date of issue, the currency, the
// insured amount and the premium (minor units), and the percentage the cover is judged on — the
// contract's or a higher one the clerk states (S5R-D06) — with what FUL found when it was recorded:
// the contract percentage and where it came from (the POL trade_terms revision the order revision the
// shipment binds names; "" for a record before S5R), the cover against the commercial invoice the
// set stood on then (nil when that invoice was not CIF or CIP, or when there was none), and the
// premium against the costing.
type InsurancePolicy struct {
	Insurer         Insurer           `json:"insurer"`
	PolicyNumber    string            `json:"policy_number"`
	Clauses         []string          `json:"clauses"`
	IssuedOn        string            `json:"issued_on"`
	Currency        CurrencyContent   `json:"currency"`
	InsuredMinor    string            `json:"insured_minor"`
	PremiumMinor    string            `json:"premium_minor"`
	CoveragePercent string            `json:"coverage_percent"`
	ContractPercent string            `json:"contract_percent,omitempty"`
	ContractSource  string            `json:"contract_source,omitempty"`
	Coverage        *Coverage         `json:"coverage,omitempty"`
	Costing         PremiumComparison `json:"costing"`
}

// Coverage is FUL-INV-005 applied to a policy and a commercial invoice version of CIF or CIP: the
// invoice value (its total, minor units of its currency), the percentage, the amount required
// (HALF_UP to the currency's minor units), the amount insured and whether it covers.
type Coverage struct {
	Incoterm        string `json:"incoterm"`
	InvoiceVersion  int64  `json:"invoice_version"`
	InvoiceNumber   string `json:"invoice_number"`
	InvoiceCurrency string `json:"invoice_currency"`
	MinorUnits      int    `json:"minor_units"`
	InvoiceMinor    string `json:"invoice_minor"`
	Percent         string `json:"percent"`
	RequiredMinor   string `json:"required_minor"`
	InsuredCurrency string `json:"insured_currency"`
	InsuredMinor    string `json:"insured_minor"`
	Sufficient      bool   `json:"sufficient"`
}

// PremiumComparison is the premium against the insurance component of the costing (R78; D-13): the
// snapshot, its currency, the component's amount and IDs, the premium, the difference (premium −
// component; only when the currencies match) and the outcome. It never refuses anything. The
// component is the whole costing's, never prorated (S5R-D06: proration would be false precision);
// beside it the shipment's share of the order revision it binds is shown for a human to judge a
// partial shipment (ShipmentPercent: the shipment's net value at the prices it keeps ÷ the revision's
// net total × 100, HALF_UP to 2 places, in the order currency; "" when FUL cannot state it).
type PremiumComparison struct {
	Status           string   `json:"status"`
	SnapshotID       string   `json:"snapshot_id,omitempty"`
	ResultHash       string   `json:"result_hash,omitempty"`
	Currency         string   `json:"currency,omitempty"`
	MinorUnits       int      `json:"minor_units,omitempty"`
	ComponentMinor   string   `json:"component_minor,omitempty"`
	Components       []string `json:"components,omitempty"`
	PremiumCurrency  string   `json:"premium_currency"`
	PremiumMinor     string   `json:"premium_minor"`
	DifferenceMinor  string   `json:"difference_minor,omitempty"`
	Reason           string   `json:"reason,omitempty"`
	ShipmentNetMinor string   `json:"shipment_net_minor,omitempty"`
	OrderNetMinor    string   `json:"order_net_minor,omitempty"`
	ShipmentPercent  string   `json:"shipment_percent,omitempty"`
}

// Costing is what CST said of the costing the order's quote adopted (R78), before the transaction:
// Available with its snapshot, currency and minor units and the sum of its insurance components
// (InsuranceMinor, their IDs) when CST showed the snapshot and classified every charge of it; else
// Reason says why there is nothing to compare with. ShipmentNetMinor and OrderNetMinor are the
// shipment's net value at the prices it keeps and the net total of the order revision it binds, in
// minor units of the order currency (S5R-D06; "" when FUL cannot state them).
type Costing struct {
	Available        bool
	Reason           string
	SnapshotID       string
	ResultHash       string
	Currency         string
	MinorUnits       int
	InsuranceMinor   string
	Components       []string
	ShipmentNetMinor string
	OrderNetMinor    string
}

// Contract is the contract insurance percentage of the order revision a shipment binds (S5R-D06,
// R49): POL's trade_terms revision that revision names, its insurance_percent (default 110, at least
// 110), and Source naming the revision ("trade_terms:<id>@<revision>").
type Contract struct {
	Percent string
	Source  string
}

// InsuranceRequest is an insurance.record as the contract checked it.
type InsuranceRequest struct {
	Insurer         shipment.ServiceRoleRef
	PolicyNumber    string
	Clauses         []string
	IssuedOn        string
	Insured         string // decimal, at most the currency's minor digits
	Premium         string
	CoveragePercent string // "": DefaultCoveragePercent
	FileID          string
	Evidence        []string
	IdempotencyKey  string
}

// InsuranceUpstream are what PTY, POL, EVD, TRD and CST said before the transaction: the insurer's
// service role (nil: none the actor reads), the policy's currency revision (nil: none), the sources
// and the file the actor reads, the contract percentage (nil: TRD or POL showed the actor no trade
// terms of the order revision the shipment binds; S5R-D06) and the costing.
type InsuranceUpstream struct {
	Insurer  *shipment.ServiceRoleFact
	Currency *Currency
	Found    map[string]bool
	File     *File
	Contract *Contract
	Costing  Costing
}

func money(code string, minorUnits int, minor string) (kernel.Money, error) {
	c, err := kernel.NewCurrency(code, minorUnits)
	if err != nil {
		return kernel.Money{}, err
	}
	return kernel.ParseMinor(c, minor)
}

// Cover returns the amount required to insure invoice at percent (FUL-INV-005): invoice × percent /
// 100, rounded HALF_UP once to the invoice currency's minor units (SIM-02 step 8: 9,200.00 → 10,120.00,
// 7,820.00 → 8,602.00).
func Cover(invoice kernel.Money, percent kernel.Decimal) (kernel.Money, error) {
	return kernel.RoundMoney(invoice.Currency(), invoice.Decimal().Mul(percent).Mul(kernel.DecimalFromMinor(big.NewInt(1), 2)))
}

// CoverageOf applies FUL-INV-005 to policy p and commercial invoice version ci: nil when ci is not
// of CIF or CIP. A policy in another currency than the invoice, or an invoice FUL cannot value, does
// not cover.
func CoverageOf(ci *Document, p *InsurancePolicy) *Coverage {
	if ci == nil || ci.Content == nil || ci.Content.Terms == nil || p == nil || !InsuranceRequired(ci.Content.Terms.Incoterm) {
		return nil
	}
	c := ci.Content
	cv := &Coverage{Incoterm: c.Terms.Incoterm, InvoiceVersion: ci.Version, InvoiceNumber: ci.Number, InvoiceMinor: c.Totals.PayableMinor,
		Percent: p.CoveragePercent, InsuredCurrency: p.Currency.Code, InsuredMinor: p.InsuredMinor}
	if c.Currency == nil {
		return cv
	}
	cv.InvoiceCurrency, cv.MinorUnits = c.Currency.Code, c.Currency.MinorUnits
	invoice, err := money(c.Currency.Code, c.Currency.MinorUnits, c.Totals.PayableMinor)
	if err != nil {
		return cv
	}
	percent, err := kernel.ParseDecimal(p.CoveragePercent, percentFormat)
	if err != nil {
		return cv
	}
	required, err := Cover(invoice, percent)
	if err != nil {
		return cv
	}
	cv.RequiredMinor = required.MinorString()
	if p.Currency.Code != c.Currency.Code || p.Currency.MinorUnits != c.Currency.MinorUnits {
		return cv
	}
	insured, err := money(p.Currency.Code, p.Currency.MinorUnits, p.InsuredMinor)
	if err != nil {
		return cv
	}
	n, err := insured.Cmp(required)
	cv.Sufficient = err == nil && n >= 0
	return cv
}

// refuse is the refusal of a cover that does not suffice, at path.
func (cv *Coverage) refuse(path string) error {
	if cv.InsuredCurrency != cv.InvoiceCurrency {
		return Reject(CodeInsuranceCoverageInsufficient, path, "The policy insures in "+cv.InsuredCurrency+"; the "+cv.Incoterm+
			" commercial invoice "+cv.InvoiceNumber+" v"+strconv.FormatInt(cv.InvoiceVersion, 10)+" is in "+cv.InvoiceCurrency)
	}
	major := func(minor string) string {
		if m, err := money(cv.InvoiceCurrency, cv.MinorUnits, minor); err == nil {
			return m.String()
		}
		return minor + " minor units"
	}
	return Reject(CodeInsuranceCoverageInsufficient, path, "Insured "+major(cv.InsuredMinor)+" is below "+cv.Percent+" % of the "+cv.Incoterm+
		" commercial invoice "+cv.InvoiceNumber+" v"+strconv.FormatInt(cv.InvoiceVersion, 10)+" of "+major(cv.InvoiceMinor)+": "+
		major(cv.RequiredMinor))
}

// Compare compares premium with the costing's insurance component (R78; D-13): never a refusal.
func Compare(premium kernel.Money, c Costing) PremiumComparison {
	out := PremiumComparison{Status: PremiumUnavailable, SnapshotID: c.SnapshotID, ResultHash: c.ResultHash, Currency: c.Currency,
		MinorUnits: c.MinorUnits, PremiumCurrency: premium.Currency().Code(), PremiumMinor: premium.MinorString(), Reason: c.Reason}
	if !c.Available {
		return out
	}
	out.Reason = ""
	if len(c.Components) == 0 {
		out.Status, out.Reason = PremiumNoComponent, "The costing has no insurance component"
		return out
	}
	out.ComponentMinor, out.Components = c.InsuranceMinor, slices.Clone(c.Components)
	component, err := money(c.Currency, c.MinorUnits, c.InsuranceMinor)
	if err != nil || c.Currency != premium.Currency().Code() || c.MinorUnits != premium.Currency().MinorUnits() {
		out.Status, out.Reason = PremiumNotComparable, "The costing is in "+c.Currency+", the premium in "+premium.Currency().Code()+
			"; FUL converts no currency"
		return out
	}
	diff, _ := premium.Sub(component) // same currency, bounded amounts
	out.DifferenceMinor = diff.MinorString()
	if diff.IsZero() {
		out.Status = PremiumEqual
	} else {
		out.Status = PremiumDifferent
	}
	return out
}

// shipmentShare states the shipment's share of the order revision beside the comparison (S5R-D06):
// shipment ÷ order × 100, HALF_UP to 2 places; nothing when either is unknown or the order is zero.
func (c *PremiumComparison) shipmentShare(shipment, order string) {
	s, okS := new(big.Int).SetString(shipment, 10)
	o, okO := new(big.Int).SetString(order, 10)
	if !okS || !okO || o.Sign() <= 0 || s.Sign() < 0 {
		return
	}
	p, err := kernel.DecimalFromMinor(s, 0).Mul(kernel.DecimalFromInt(100)).Quo(kernel.DecimalFromMinor(o, 0))
	if err != nil {
		return
	}
	c.ShipmentNetMinor, c.OrderNetMinor, c.ShipmentPercent = shipment, order, p.StringFixed(2)
}

// RecordInsurance decides insurance.record: a new version of the set's insurance policy record on
// facts f and what the upstreams said. It refuses, in this order: no shipment the actor reads
// (REFERENCE_INVALID), a cancelled shipment (INVALID_STATE), a source of evidence the actor does not
// read (REFERENCE_INVALID), an insurer's service role the actor does not read (REFERENCE_INVALID),
// one PTY no longer serves now (REFERENCE_STALE) or of another kind than insurer
// (INVALID_KNOWN_VALUE), a file the actor does not read (REFERENCE_INVALID), a currency revision POL
// does not show (REFERENCE_INVALID), no contract percentage (TRD or POL showed the actor no trade
// terms of the order revision the shipment binds: REFERENCE_INVALID at /args/shipment_id, S5R-D06),
// an amount with more digits than the currency's minor units, or a stated percentage below 110 or
// below the contract's (INVALID_KNOWN_VALUE: the clerk may state more than the contract, never less;
// S5R-D06); and, when the set's current commercial invoice is of CIF or CIP, a policy in another
// currency or insuring less than the percentage of the invoice value (INSURANCE_COVERAGE_INSUFFICIENT).
// The percentage is the contract's when the clerk states none. The premium is compared with the
// costing, the shipment's share of the order shown beside it, and never refused. The version is the
// kind's next; Add appends it.
func RecordInsurance(s *Set, req InsuranceRequest, f Facts, up InsuranceUpstream, act Act) (*Document, error) {
	sh := f.Shipment
	switch {
	case sh == nil:
		return nil, Reject(kernel.CodeReferenceInvalid, "/args/shipment_id", "No shipment the actor reads")
	case sh.Status == shipment.StatusCancelled:
		return nil, Reject(kernel.CodeInvalidState, "/args/shipment_id", "The shipment is cancelled")
	}
	if err := Readable(req.Evidence, up.Found, "/args/evidence_ids"); err != nil {
		return nil, err
	}
	switch r := up.Insurer; {
	case r == nil || r.Ref != req.Insurer:
		return nil, Reject(kernel.CodeReferenceInvalid, "/args/insurer", "No service role the actor reads")
	case r.Code != "":
		return nil, shipment.Upstream(r.Code, "/args/insurer", "The service role does not serve now")
	case r.Role != ServiceInsurer:
		return nil, Reject(kernel.CodeInvalidKnownValue, "/args/insurer", "A policy is issued by an insurer, not a "+r.Role)
	}
	if up.File == nil || up.File.ID != req.FileID {
		return nil, Reject(kernel.CodeReferenceInvalid, "/args/file_id", "No file the actor reads")
	}
	cur := up.Currency
	if cur == nil {
		return nil, Reject(kernel.CodeReferenceInvalid, "/args/currency", "No such POL currency revision")
	}
	currency, err := kernel.NewCurrency(cur.Code, cur.MinorUnits)
	if err != nil {
		return nil, Reject(kernel.CodeReferenceInvalid, "/args/currency", "The currency revision has no ISO code and minor units")
	}
	insured, err := kernel.ParseAmount(currency, req.Insured)
	if err != nil || insured.Sign() <= 0 {
		return nil, Reject(kernel.CodeInvalidKnownValue, "/args/insured_amount", "A positive amount of at most "+
			strconv.Itoa(cur.MinorUnits)+" decimals of "+cur.Code)
	}
	contract, err := contractPercent(up.Contract)
	if err != nil {
		return nil, err
	}
	premium, err := kernel.ParseAmount(currency, req.Premium)
	if err != nil {
		return nil, Reject(kernel.CodeInvalidKnownValue, "/args/premium", "An amount of at most "+strconv.Itoa(cur.MinorUnits)+" decimals of "+
			cur.Code)
	}
	percent := req.CoveragePercent
	if percent == "" {
		percent = text(contract)
	}
	p, err := kernel.ParseDecimal(percent, percentFormat)
	switch {
	case err != nil || p.Cmp(kernel.DecimalFromInt(110)) < 0:
		return nil, Reject(kernel.CodeInvalidKnownValue, "/args/coverage_percent", "CIF and CIP cover at least 110 % of the price")
	case p.Cmp(contract) < 0:
		return nil, Reject(kernel.CodeInvalidKnownValue, "/args/coverage_percent", "The contract insures "+text(contract)+" % ("+up.Contract.Source+
			"); a policy may state more, never less")
	}
	act.At = kernel.Instant(act.At)
	policy := &InsurancePolicy{Insurer: Insurer(req.Insurer), PolicyNumber: req.PolicyNumber, Clauses: slices.Clone(req.Clauses),
		IssuedOn: req.IssuedOn, Currency: CurrencyContent{Code: cur.Code, Revision: cur.Revision, MinorUnits: cur.MinorUnits},
		InsuredMinor: insured.MinorString(), PremiumMinor: premium.MinorString(), CoveragePercent: text(p), ContractPercent: text(contract),
		ContractSource: up.Contract.Source}
	if cv := CoverageOf(s.Current(KindCommercialInvoice), policy); cv != nil {
		if !cv.Sufficient {
			at := "/args/insured_amount"
			if cv.InsuredCurrency != cv.InvoiceCurrency {
				at = "/args/currency"
			}
			return nil, cv.refuse(at)
		}
		policy.Coverage = cv
	}
	policy.Costing = Compare(premium, up.Costing)
	policy.Costing.shipmentShare(up.Costing.ShipmentNetMinor, up.Costing.OrderNetMinor)
	d := &Document{Kind: KindInsurancePolicy, Version: s.NextVersion(KindInsurancePolicy), Number: req.PolicyNumber,
		Evidence: slices.Clone(req.Evidence), IdempotencyKey: req.IdempotencyKey, Prepared: act}
	file := *up.File
	d.File, d.Record = &file, &Record{Insurance: policy}
	d.ContentJSON, d.ContentSHA256 = Canonical(d.Record)
	d.Sources = []Entry{{Name: "shipment", Value: sh.ID + " v" + strconv.FormatInt(sh.RowVersion, 10) + " " + sh.Status},
		{Name: "order_binding", Value: sh.Order.OrderID + " r" + strconv.FormatInt(sh.Order.OrderRevision, 10) + " " + sh.Order.OrderHash},
		{Name: "insurer", Value: req.Insurer.PartyID + "/" + req.Insurer.ServiceRoleID + "@" + strconv.FormatInt(req.Insurer.Revision, 10)},
		{Name: "currency", Value: cur.Code + "@" + strconv.FormatInt(cur.Revision, 10)},
		{Name: "file", Value: file.ID + " " + file.SHA256}}
	if cv := policy.Coverage; cv != nil {
		d.Sources = append(d.Sources, Entry{Name: "coverage", Value: cv.Incoterm + " " + cv.InvoiceNumber + " v" + strconv.FormatInt(cv.InvoiceVersion, 10) +
			" " + cv.InvoiceMinor + " x " + cv.Percent + "% = " + cv.RequiredMinor + " <= " + cv.InsuredMinor})
	}
	d.Sources = append(d.Sources, Entry{Name: "contract", Value: policy.ContractPercent + "% " + policy.ContractSource},
		Entry{Name: "costing", Value: costingSource(policy.Costing)})
	return d, nil
}

// contractPercent is the contract insurance percentage the upstreams stated (S5R-D06): REFERENCE_INVALID
// when TRD or POL showed none, or one FUL cannot read or below 110 (POL never publishes such terms).
func contractPercent(c *Contract) (kernel.Decimal, error) {
	if c == nil {
		return kernel.Decimal{}, Reject(kernel.CodeReferenceInvalid, "/args/shipment_id",
			"TRD or POL shows the actor no trade terms of the order revision the shipment binds: the contract insurance percentage is unknown")
	}
	p, err := kernel.ParseDecimal(c.Percent, percentFormat)
	if err != nil || p.Cmp(kernel.DecimalFromInt(110)) < 0 {
		return kernel.Decimal{}, Reject(kernel.CodeReferenceInvalid, "/args/shipment_id", "The contract insurance percentage of "+c.Source+
			" is not one FUL reads")
	}
	return p, nil
}

func costingSource(c PremiumComparison) string {
	v := c.Status
	if c.SnapshotID != "" {
		v += " " + c.SnapshotID
	}
	if c.ComponentMinor != "" {
		v += " component " + c.ComponentMinor + " " + c.Currency
	}
	if c.DifferenceMinor != "" {
		v += " difference " + c.DifferenceMinor
	}
	return v
}

// Policy returns the policy a version of kind insurance_policy records, or nil.
func (d *Document) Policy() *InsurancePolicy {
	if d == nil || d.Record == nil {
		return nil
	}
	return d.Record.Insurance
}

// CoverageNow applies FUL-INV-005 to the set's current policy and current commercial invoice: nil
// when the invoice is not of CIF or CIP or the set has no policy.
func (s *Set) CoverageNow() *Coverage {
	return CoverageOf(s.Current(KindCommercialInvoice), s.Current(KindInsurancePolicy).Policy())
}

// CheckCoverage refuses an issue whose current policy does not cover the current CIF or CIP
// commercial invoice (INSURANCE_COVERAGE_INSUFFICIENT at /insurance_policy/insured_amount, or at
// /insurance_policy/currency for another currency). A missing policy is the reconciliation's.
func (s *Set) CheckCoverage() error {
	cv := s.CoverageNow()
	if cv == nil || cv.Sufficient {
		return nil
	}
	if cv.InsuredCurrency != cv.InvoiceCurrency {
		return cv.refuse("/" + KindInsurancePolicy + "/currency")
	}
	return cv.refuse("/" + KindInsurancePolicy + "/insured_amount")
}

// insurance adds the policy's items to a reconciliation: a missing policy when the commercial
// invoice is of CIF or CIP (required: never acknowledged; FUL-INV-005, F-03), and a policy issued
// after the bill of lading says the goods were shipped on board (the cover must run from loading).
func insurance(x *diffs, s *Set, ci, bl *Document) {
	incoterm := ""
	if ci != nil && ci.Content != nil && ci.Content.Terms != nil {
		incoterm = ci.Content.Terms.Incoterm
	}
	ins := s.Current(KindInsurancePolicy)
	p := ins.Policy()
	if p == nil {
		if InsuranceRequired(incoterm) {
			x.out = append(x.out, Difference{Path: "/" + KindInsurancePolicy, Document: KindInsurancePolicy, Item: "document",
				Expected: "an insurance policy (" + incoterm + ")", Against: KindCommercialInvoice, Required: true})
		}
		return
	}
	if bl != nil && bl.Record != nil && bl.Record.BillOfLading != nil && p.IssuedOn > bl.Record.BillOfLading.ShippedOnBoard {
		x.add(ins, "issued_on", "/issued_on", p.IssuedOn, bl.Record.BillOfLading.ShippedOnBoard, KindBillOfLading)
	}
	if need, ok := minimumCover[incoterm]; ok { // S5R-D06: the clauses the Incoterm asks of the seller
		if got := ClauseCover(p.Clauses); got < need {
			x.add(ins, "clauses", "/clauses", coverNames[got], coverNames[need]+" or better ("+incoterm+")", KindCommercialInvoice)
		}
	}
}

// The cover a policy's clauses give, from none to the widest (Institute Cargo Clauses (C), (B), (A);
// the China Insurance Clauses' FPA, WPA and All Risks are the equivalents most Chinese insurers issue).
const (
	CoverNone = iota
	CoverC
	CoverB
	CoverA
)

var (
	coverNames = map[int]string{CoverNone: "no Institute Cargo Clauses or equivalent", CoverC: "ICC (C)", CoverB: "ICC (B)", CoverA: "ICC (A)"}
	// minimumCover is what the seller insures by default under Incoterms 2020 A5 (S5R-D06): CIP the
	// widest cover, ICC (A) or equivalent; CIF at least ICC (C). A contract that agrees otherwise is
	// stated when the difference is acknowledged.
	minimumCover = map[string]int{"CIP": CoverA, "CIF": CoverC}
	coverWords   = []struct {
		cover int
		words []string
	}{
		{CoverA, []string{"institutecargoclauses(a)", "icc(a)", "allrisks", "allrisk", "一切险"}},
		{CoverB, []string{"institutecargoclauses(b)", "icc(b)", "withparticularaverage", "w.p.a", "wpa", "水渍险"}},
		{CoverC, []string{"institutecargoclauses(c)", "icc(c)", "freefromparticularaverage", "f.p.a", "fpa", "平安险"}},
	}
)

// ClauseCover returns the widest cover clauses state (CoverNone, CoverC, CoverB or CoverA),
// recognising the Institute Cargo Clauses by letter and the China Insurance Clauses' equivalents in
// English or Chinese, white space and case ignored.
func ClauseCover(clauses []string) int {
	best := CoverNone
	for _, c := range clauses {
		folded := strings.ToLower(strings.Join(strings.Fields(c), ""))
		for _, w := range coverWords {
			for _, word := range w.words {
				if w.cover > best && strings.Contains(folded, word) {
					best = w.cover
				}
			}
		}
	}
	return best
}
