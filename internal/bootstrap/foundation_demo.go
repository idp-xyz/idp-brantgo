package bootstrap

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// DemoResultFile is the file FoundationDemo writes into its directory (Python
// foundation-demo-result.json).
const DemoResultFile = "foundation-demo-result.json"

// DemoTenant is the synthetic tenant every foundation demo runs in; each run adds a company of its
// own to it.
const DemoTenant = "tenant.synthetic.demo"

// DemoNotice is the content of the evidence every publication of the demo cites, and the label of
// its result.
const DemoNotice = "SYNTHETIC ONLY: all names, addresses, exchange rates and tax parameters are test fixtures, not real policies or market facts."

// demoPermissions are what the demo's editor and reviewer do. Python grants both "*"; Brant has no
// wildcard, so both get exactly these, and independence comes from POL-INV-001, not from grants.
var demoPermissions = []string{
	"source.register", "evd.read", "sku.save_draft", "sku.confirm",
	"config.save_draft", "config.publish", "pol.read", "foundation.snapshot", "number.issue",
}

// DemoConfig configures FoundationDemo.
type DemoConfig struct {
	// Directory receives DemoResultFile; it is created when missing and must not hold one yet.
	Directory string
}

// DemoResult is the content of DemoResultFile (Python foundation_demo.demo). Every value in it is
// synthetic.
type DemoResult struct {
	Synthetic          bool             `json:"synthetic"`
	Notice             string           `json:"notice"`
	BusinessClock      string           `json:"business_clock"`
	Tenant             string           `json:"tenant"`
	Company            string           `json:"company"`
	ConfigurationKinds int              `json:"configuration_kinds"`
	Configurations     int              `json:"configurations"`
	Kinds              []string         `json:"kinds"`
	Configs            []DemoConfigRef  `json:"configs"`
	Commands           []DemoCommand    `json:"commands"`
	FX                 DemoAmount       `json:"fx"`
	Unit               DemoQuantity     `json:"unit"`
	Tax                DemoTaxSplit     `json:"tax"`
	Lookups            []DemoLookup     `json:"lookups"`
	Snapshot           DemoSnapshot     `json:"snapshot"`
	Number             DemoIssuedNumber `json:"number"`
	ModelCalls         int              `json:"model_calls"`
	ExternalActions    int              `json:"external_business_actions"`
	// ResultFile is where the result was written; it is not part of the file.
	ResultFile string `json:"-"`
}

// DemoConfigRef is a configuration revision; ContentHash is set where the demo read it.
type DemoConfigRef struct {
	Kind        string `json:"kind"`
	ID          string `json:"id"`
	Revision    int64  `json:"revision"`
	ContentHash string `json:"content_hash,omitempty"`
}

// DemoCommand is one command the demo ran and the receipt it got.
type DemoCommand struct {
	Command    string `json:"command"`
	Actor      string `json:"actor"`
	ObjectID   string `json:"object_id"`
	RowVersion int64  `json:"row_version"`
	Status     string `json:"status"`
	ActionID   string `json:"action_id"`
	Replayed   bool   `json:"replayed"`
}

// DemoAmount is an amount of money (Python fx.convert values).
type DemoAmount struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// DemoQuantity is a quantity (Python unit.convert values).
type DemoQuantity struct {
	Quantity string `json:"quantity"`
	Unit     string `json:"unit"`
}

// DemoTaxSplit is a tax split (Python tax.preview values).
type DemoTaxSplit struct {
	NetAmount   string `json:"net_amount"`
	TaxAmount   string `json:"tax_amount"`
	GrossAmount string `json:"gross_amount"`
	Currency    string `json:"currency"`
}

// DemoLookup is how one value was computed: the POL lookup, its input, the revision it adopted and
// the factor kernel arithmetic applied.
type DemoLookup struct {
	Query   string        `json:"query"`
	At      string        `json:"at"`
	Input   string        `json:"input"`
	Adopted DemoConfigRef `json:"adopted"`
	Factor  string        `json:"factor"`
	Result  string        `json:"result"`
}

// DemoSnapshot is the FoundationSnapshot the demo captured.
type DemoSnapshot struct {
	SnapshotID  string          `json:"snapshot_id"`
	Purpose     string          `json:"purpose"`
	CapturedAt  string          `json:"captured_at"`
	CapturedBy  string          `json:"captured_by"`
	ContentHash string          `json:"content_hash"`
	Entries     []DemoConfigRef `json:"entries"`
}

// DemoIssuedNumber is the document number the demo issued.
type DemoIssuedNumber struct {
	DocumentNumber string        `json:"document_number"`
	BusinessRef    string        `json:"business_ref"`
	Sequence       DemoConfigRef `json:"sequence"`
}

// FoundationDemo runs the synthetic foundation scenario of Python V0.50 (brant_core/foundation_demo.py)
// against PostgreSQL and writes DemoResultFile into cfg.Directory. It makes no model call and no
// external business action.
//
// Each run provisions a fresh synthetic company in DemoTenant, so it never touches other data and
// may run any number of times; idempotency keys are demo.1, demo.2, ... within the run. A run that
// fails leaves its partial company behind and writes no result. A human
// editor registers the synthetic evidence in EVD, confirms the SKU in CAT and saves every
// configuration; a second human, the reviewer, publishes each one citing that evidence (POL-INV-001).
// The editor then captures a foundation snapshot, issues a quote number and computes three values
// through POL's published lookups with kernel arithmetic: 120 USD at the synthetic rate 7.123456
// (LookupFX, rounded once, S1-D13), 113 CNY split by the synthetic inclusive 13 % tax profile
// (LookupTax) and 2 DOZEN of the SKU in PCS (LookupUnitConversion).
//
// Python publishes 22 configurations of its 18 kinds. Four of those kinds are not POL's in Brant:
// sku_profile (the conversion names CAT's SKU and specification revision instead, S1-D19),
// party_profile and contact_profile (PTY) and code_mapping (CAT item-code mapping). The 16 Python
// configurations of the other 14 kinds are published here under POL's names, and one configuration
// of each kind Python did not have (tax_application, quote_document_template, credit_policy,
// screening_regulation, and legal_terms: the tenant's default, the CISG with PRC law, which the
// company profile names), so the demo publishes 21 configurations of POL's 19 kinds. Python's clock
// is fixed at 2026-10-22T09:00:00Z; POL publishes at the system clock, so the business clock is the
// start of the run and the bounded configurations are valid from the hour before it for a day.
func (p *Platform) FoundationDemo(ctx context.Context, cfg DemoConfig) (DemoResult, error) {
	if cfg.Directory == "" {
		return DemoResult{}, errors.New("foundation demo: a directory is required")
	}
	if err := os.MkdirAll(cfg.Directory, 0o755); err != nil {
		return DemoResult{}, err
	}
	file := filepath.Join(cfg.Directory, DemoResultFile)
	if _, err := os.Lstat(file); !errors.Is(err, fs.ErrNotExist) {
		if err == nil {
			err = fmt.Errorf("foundation demo: fresh directory required, %s exists", file)
		}
		return DemoResult{}, err
	}
	c, err := p.Contexts()
	if err != nil {
		return DemoResult{}, err
	}
	start := kernel.Instant(time.Now())
	suffix := make([]byte, 4)
	if _, err = rand.Read(suffix); err != nil {
		return DemoResult{}, err
	}
	d := &demo{p: p, c: c, start: start, from: start.Truncate(time.Hour).Add(-time.Hour),
		company: "company.demo." + start.Format("20060102T150405Z") + "." + hex.EncodeToString(suffix)}
	d.until = d.from.Add(24 * time.Hour)
	result, err := d.run(ctx)
	if err != nil {
		return DemoResult{}, fmt.Errorf("foundation demo in %s/%s: %w", DemoTenant, d.company, err)
	}
	out, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return DemoResult{}, err
	}
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err = enc.Encode(result); err != nil {
		_ = out.Close()
		return DemoResult{}, err
	}
	if err = out.Close(); err != nil {
		return DemoResult{}, err
	}
	result.ResultFile = file
	return result, nil
}

type demo struct {
	p                  *Platform
	c                  *Contexts
	company            string
	start, from, until time.Time
	editor, reviewer   context.Context
	commands           []DemoCommand
	configs            []DemoConfigRef
}

func (d *demo) key() string { return "demo." + strconv.Itoa(len(d.commands)+1) }

func (d *demo) record(ctx context.Context, command, actionID, objectID string, rowVersion int64, status string, replayed bool) {
	p, _ := identity.FromContext(ctx)
	d.commands = append(d.commands, DemoCommand{Command: command, Actor: string(p.Actor.Kind) + ":" + p.Actor.ID, ObjectID: objectID,
		RowVersion: rowVersion, Status: status, ActionID: actionID, Replayed: replayed})
}

// actor provisions a human of the demo's company with demoPermissions and company data scope
// (Python core.provision(who, ["*"], team_id="trade", data_scope="company")) and returns its
// request context.
func (d *demo) actor(ctx context.Context, id, correlation string) (context.Context, error) {
	scope, err := kernel.NewScope(DemoTenant, d.company)
	if err != nil {
		return nil, err
	}
	actor := kernel.ActorRef{Kind: kernel.ActorHuman, ID: id}
	if _, err = d.p.Provision(ctx, Grant{Scope: scope, Actor: actor, Permissions: demoPermissions, DataScope: authorization.DataScopeCompany,
		Team: "trade"}); err != nil {
		return nil, err
	}
	ctx, err = identity.WithPrincipal(ctx, identity.Principal{Actor: actor, Scope: scope})
	if err != nil {
		return nil, err
	}
	return action.WithCorrelation(ctx, correlation), nil
}

// configure saves data as the editor and publishes it as the reviewer, citing the evidence (Python
// foundation_demo config).
func (d *demo) configure(kind, id string, bounded bool, data polapi.ConfigData) (polapi.ConfigRef, error) {
	cmd := polapi.SaveDraft{IdempotencyKey: d.key(), Kind: kind, ConfigID: id, ValidFrom: kernel.FormatTimestamp(d.from), Data: data}
	if bounded {
		cmd.ValidUntil = kernel.FormatTimestamp(d.until)
	}
	saved, err := d.c.POL.SaveDraft(d.editor, cmd)
	if err != nil {
		return polapi.ConfigRef{}, fmt.Errorf("%s %s: %w", polapi.CommandSaveDraft, polapi.ObjectID(kind, id), err)
	}
	d.record(d.editor, polapi.CommandSaveDraft, saved.ActionID, saved.ObjectID, saved.RowVersion, saved.Status, saved.Replayed)
	v, err := d.c.POL.GetConfig(d.editor, kind, id)
	if err != nil {
		return polapi.ConfigRef{}, err
	}
	ref := polapi.ConfigRef{Kind: kind, ConfigID: id, Revision: v.WorkingRevision}
	published, err := d.c.POL.Publish(d.reviewer, polapi.Publish{IdempotencyKey: d.key(), Kind: kind, ConfigID: id, ExpectedVersion: saved.RowVersion,
		Revision: ref.Revision, EvidenceIDs: []string{"evidence"}, Reason: "Independent review of synthetic fixture"})
	if err != nil {
		return polapi.ConfigRef{}, fmt.Errorf("%s %s: %w", polapi.CommandPublish, saved.ObjectID, err)
	}
	d.record(d.reviewer, polapi.CommandPublish, published.ActionID, published.ObjectID, published.RowVersion, published.Status, published.Replayed)
	d.configs = append(d.configs, DemoConfigRef{Kind: kind, ID: id, Revision: ref.Revision})
	return ref, nil
}

func (d *demo) run(ctx context.Context) (DemoResult, error) {
	correlation := action.NewID("corr", d.start)
	var err error
	if d.editor, err = d.actor(ctx, "editor", correlation); err != nil {
		return DemoResult{}, err
	}
	if d.reviewer, err = d.actor(ctx, "reviewer", correlation); err != nil {
		return DemoResult{}, err
	}
	source, err := d.c.EVD.RegisterSource(d.editor, evdapi.RegisterSource{IdempotencyKey: d.key(), SourceID: "evidence",
		SourceKind: evdapi.KindManualText, ContentText: DemoNotice})
	if err != nil {
		return DemoResult{}, fmt.Errorf("source.register: %w", err)
	}
	d.record(d.editor, "source.register", source.ActionID, source.ObjectID, source.RowVersion, source.Status, source.Replayed)
	sku, err := d.sku()
	if err != nil {
		return DemoResult{}, err
	}
	refs, err := d.configurations(sku)
	if err != nil {
		return DemoResult{}, err
	}
	snap, err := d.snapshot(refs)
	if err != nil {
		return DemoResult{}, err
	}
	number, err := d.c.POL.IssueNumber(d.editor, polapi.IssueNumber{IdempotencyKey: d.key(), Sequence: refs.sequence, BusinessRef: "draft.quote1"})
	if err != nil {
		return DemoResult{}, fmt.Errorf("%s: %w", polapi.CommandIssueNumber, err)
	}
	d.record(d.editor, polapi.CommandIssueNumber, number.ActionID, number.ObjectID, number.RowVersion, number.Status, number.Replayed)
	r := DemoResult{Synthetic: true, Notice: DemoNotice, BusinessClock: kernel.FormatTimestamp(d.start), Tenant: DemoTenant, Company: d.company,
		Configurations: len(d.configs), Configs: d.configs, Snapshot: snap,
		Number: DemoIssuedNumber{DocumentNumber: number.ObjectID, BusinessRef: "draft.quote1", Sequence: demoRef(refs.sequence, "")}}
	for _, c := range d.configs {
		if !slices.Contains(r.Kinds, c.Kind) {
			r.Kinds = append(r.Kinds, c.Kind)
		}
	}
	slices.Sort(r.Kinds)
	r.ConfigurationKinds = len(r.Kinds)
	if err = d.lookups(&r, refs, sku); err != nil {
		return DemoResult{}, err
	}
	r.Commands = d.commands
	return r, nil
}

// sku saves and confirms the SKU (Python master kind sku, base_uom PCS) and returns its reference
// from CAT, whose specification revision the unit conversion names.
func (d *demo) sku() (catapi.SkuRef, error) {
	saved, err := d.c.CAT.SaveSku(d.editor, catapi.SaveSku{IdempotencyKey: d.key(), SkuID: "sku", Name: "合成商品", BaseUnit: catapi.UnitPCS})
	if err != nil {
		return catapi.SkuRef{}, fmt.Errorf("sku.save_draft: %w", err)
	}
	d.record(d.editor, "sku.save_draft", saved.ActionID, saved.ObjectID, saved.RowVersion, saved.Status, saved.Replayed)
	confirmed, err := d.c.CAT.ConfirmSku(d.editor, catapi.ConfirmSku{IdempotencyKey: d.key(), SkuID: "sku", ExpectedVersion: saved.RowVersion,
		EvidenceIDs: []string{"evidence"}})
	if err != nil {
		return catapi.SkuRef{}, fmt.Errorf("sku.confirm: %w", err)
	}
	d.record(d.editor, "sku.confirm", confirmed.ActionID, confirmed.ObjectID, confirmed.RowVersion, confirmed.Status, confirmed.Replayed)
	skus, err := d.c.CAT.ResolveSkus(d.editor, []string{"sku"})
	if err != nil {
		return catapi.SkuRef{}, err
	}
	ref, ok := skus["sku"]
	if !ok || ref.SpecRevision < 1 {
		return catapi.SkuRef{}, errors.New("CAT does not resolve the confirmed SKU")
	}
	return ref, nil
}

// demoRefs are the configurations the snapshot and the lookups adopt.
type demoRefs struct {
	company, conversion, terms, payment, tax, fee, policy, fx, sequence polapi.ConfigRef
}

// configurations publishes Python's configurations in Python's order under POL's kinds, then one
// configuration of each kind Python did not have; the default legal terms come before the company
// profile that names them.
func (d *demo) configurations(sku catapi.SkuRef) (demoRefs, error) {
	var r demoRefs
	var legal polapi.ConfigRef
	steps := []func() error{
		func() error { return d.publish(polapi.KindCurrency, "CNY", false, currency("CNY"), nil) },
		func() error { return d.publish(polapi.KindCurrency, "USD", false, currency("USD"), nil) },
		// The tenant's default legal terms, which the company profile names (WBS S3-17, G0 F-02: 默认
		// CISG 与中国法): published before the profile, which adopts its exact revision.
		func() error {
			return d.publish(polapi.KindLegalTerms, "legal.default", false, polapi.ConfigData{LegalTerms: &polapi.LegalTerms{
				Name: "synthetic default: CISG with PRC law", GoverningLaw: polapi.GoverningLawCN, CISG: polapi.CISGApplies,
				FormationRule: polapi.FormationArrival, DisputeResolution: polapi.DisputeResolution{Method: polapi.DisputeArbitration,
					Institution: "synthetic arbitration institution", Seat: "Shanghai", Language: polapi.LanguageZhCN}}}, &legal)
		},
		func() error {
			return d.publish(polapi.KindCompanyProfile, d.company, false, polapi.ConfigData{CompanyProfile: &polapi.CompanyProfile{
				LegalName: "合成外贸企业", RegistrationNo: "SYNTHETIC-NOT-REAL", Country: "CN", Address: "测试地址", BaseCurrency: "CNY",
				TimeZone: "Asia/Shanghai", PolicyScope: "synthetic general trade scenario", DefaultLegalTerms: &legal}}, &r.company)
		},
		func() error { return d.publish(polapi.KindUnit, "PCS", false, unit("PCS", "件"), nil) },
		func() error { return d.publish(polapi.KindUnit, "DOZEN", false, unit("DOZEN", "打"), nil) },
		func() error {
			return d.publish(polapi.KindUnitConversion, "dozen.pcs", true, polapi.ConfigData{UnitConversion: &polapi.UnitConversion{
				SkuID: sku.SkuID, SpecRevision: sku.SpecRevision, FromUnit: "DOZEN", ToUnit: "PCS", Factor: "12",
				Basis: "synthetic SKU-specific pack definition"}}, &r.conversion)
		},
	}
	var calendar polapi.ConfigRef
	steps = append(steps,
		func() error {
			return d.publish(polapi.KindCalendar, "calendar", false, polapi.ConfigData{Calendar: &polapi.Calendar{TimeZone: "Asia/Shanghai",
				WorkingWeekdays: []int{1, 2, 3, 4, 5}}}, &calendar)
		},
		func() error {
			return d.publish(polapi.KindTradeTerms, "terms", false, polapi.ConfigData{TradeTerms: &polapi.TradeTerms{Incoterm: "FCA",
				Edition: polapi.IncotermsEdition2020, NamedPlace: "TEST NAMED PLACE", ResponsibilityNote: "synthetic terms, no ownership inference",
				Delivery: polapi.Delivery{Kind: polapi.DeliveryLeadTime, AnchorEvent: polapi.AnchorTechnicalApproved, Days: 30,
					DayBasis: polapi.DayBasisWorking, Calendar: &calendar}}}, &r.terms)
		},
		func() error {
			return d.publish(polapi.KindPaymentTerms, "payment", false, polapi.ConfigData{PaymentTerms: &polapi.PaymentTerms{Name: "30/70 TEST",
				Stages: []polapi.PaymentStage{{StageID: "deposit", Percent: "30", Trigger: polapi.TriggerOrderConfirmed},
					{StageID: "balance", Percent: "70", Trigger: polapi.TriggerBeforeShipment}},
				DayBasis: polapi.DayBasisCalendar, Calendar: &calendar}}, &r.payment)
		},
		func() error {
			return d.publish(polapi.KindTaxProfile, "tax", true, polapi.ConfigData{TaxProfile: &polapi.TaxProfile{Name: "SYNTHETIC 13%", Rate: "0.13",
				PriceBasis: polapi.PriceBasisInclusive, RecoveryTreatment: polapi.RecoveryNonrecoverable,
				PolicyBasis: "synthetic arithmetic only; not tax advice"}}, &r.tax)
		},
		func() error {
			return d.publish(polapi.KindFeeCategory, "packaging", false, polapi.ConfigData{FeeCategory: &polapi.FeeCategory{Name: "包装费",
				Category: "packaging", Side: polapi.FeeSidePurchaseCost, Inclusion: polapi.InclusionIncludedInLine,
				AllocationBasis: polapi.AllocationQuantity, PolicyBasis: "synthetic included fee"}}, &r.fee)
		},
		func() error {
			return d.publish(polapi.KindPricingPolicy, "pricing", false, polapi.ConfigData{PricingPolicy: &polapi.PricingPolicy{
				Name: "synthetic policy", CalculationCurrency: "CNY", MinimumMargin: "0.25", QuoteValidDays: 7, MaxFXAgeSeconds: 86400,
				TaxProfile: r.tax}}, &r.policy)
		},
		func() error {
			return d.publish(polapi.KindFXRate, "usd.cny", true, polapi.ConfigData{FXRate: &polapi.FXRate{FromCurrency: "USD", ToCurrency: "CNY",
				Rate: "7.123456", Purpose: polapi.PurposeCostEstimate, SourceName: "SYNTHETIC RATE",
				ObservedAt: kernel.FormatTimestamp(d.from)}}, &r.fx)
		},
		func() error {
			return d.publish(polapi.KindBankAccount, "bank", false, polapi.ConfigData{BankAccount: &polapi.BankAccount{OwnerKind: polapi.OwnerCompany,
				OwnerID: d.company, BankName: "synthetic bank", AccountNumber: "TEST1234567890", Currency: "CNY", Purpose: polapi.AccountBoth}}, nil)
		},
		func() error {
			return d.publish(polapi.KindNumberSequence, "quote.sequence", false, polapi.ConfigData{NumberSequence: &polapi.NumberSequence{
				DocumentType: polapi.DocumentTypeQuote, Prefix: "QT-", Width: 5, Reset: polapi.ResetAnnual, TimeZone: "Asia/Shanghai"}}, &r.sequence)
		},
		func() error {
			return d.publish(polapi.KindDocumentTemplate, "quote.template", false, polapi.ConfigData{DocumentTemplate: &polapi.DocumentTemplate{
				Name: "synthetic plaintext draft", DocumentType: polapi.DocumentTypeQuote,
				TemplateText: "{document.number}\n{customer.name}\n{quote.total} {quote.currency}",
				FieldNames:   []string{"document.number", "customer.name", "quote.total", "quote.currency"}}}, nil)
		},
		// The kinds Python did not have.
		func() error {
			return d.publish(polapi.KindTaxApplication, "sales.sku", false, polapi.ConfigData{TaxApplication: &polapi.TaxApplication{
				Name: "synthetic sales tax application", Side: polapi.TaxSideSales, SubjectKind: polapi.SubjectSKU, SubjectID: sku.SkuID,
				TaxProfile: r.tax, Jurisdiction: "synthetic CN", BasisNote: "synthetic arithmetic only; not a tax determination"}}, nil)
		},
		func() error {
			return d.publish(polapi.KindQuoteDocumentTemplate, "formal.quote", false, polapi.ConfigData{QuoteDocumentTemplate: &polapi.QuoteDocumentTemplate{
				Name: "synthetic formal quote", Language: polapi.LanguageZhCN, RendererVersion: polapi.QuoteRendererVersion, Title: "报价单（合成样例）",
				Sections: polapi.QuoteSections(), FooterNote: "SYNTHETIC ONLY: not an offer"}}, nil)
		},
		func() error {
			return d.publish(polapi.KindCreditPolicy, "credit", false, polapi.ConfigData{CreditPolicy: &polapi.CreditPolicy{
				Name: "synthetic credit policy", Currency: "CNY", DefaultLimit: "0", LimitSources: []string{polapi.LimitSourceInternalAssessment},
				ApprovalTiers: []polapi.CreditApprovalTier{{Level: "credit_officer", MaxAmount: "10000.00"}}, OverdueToleranceDays: 30}}, nil)
		},
		func() error {
			return d.publish(polapi.KindScreeningRegulation, "screening", false, polapi.ConfigData{ScreeningRegulation: &polapi.ScreeningRegulation{
				Lists: []polapi.ScreeningList{{ListCode: "synthetic.cn", Jurisdiction: polapi.JurisdictionCN}}, RescreenIntervalDays: 90}}, nil)
		},
	)
	for _, step := range steps {
		if err := step(); err != nil {
			return r, err
		}
	}
	return r, nil
}

// publish is configure that stores the reference in *ref when ref is not nil.
func (d *demo) publish(kind, id string, bounded bool, data polapi.ConfigData, ref *polapi.ConfigRef) error {
	r, err := d.configure(kind, id, bounded, data)
	if err == nil && ref != nil {
		*ref = r
	}
	return err
}

func currency(code string) polapi.ConfigData {
	return polapi.ConfigData{Currency: &polapi.Currency{Code: code, Name: code, MinorUnits: 2, Rounding: polapi.RoundingHalfUp}}
}

func unit(code, name string) polapi.ConfigData {
	return polapi.ConfigData{Unit: &polapi.Unit{Code: code, Name: name, Dimension: polapi.DimensionCount, Precision: 0}}
}

func demoRef(r polapi.ConfigRef, hash string) DemoConfigRef {
	return DemoConfigRef{Kind: r.Kind, ID: r.ConfigID, Revision: r.Revision, ContentHash: hash}
}

// snapshot captures the cost estimate foundation of Python's snapshot refs; its sku_profile is not
// a configuration in Brant, and snapshots carry no master data (S1-D21).
func (d *demo) snapshot(r demoRefs) (DemoSnapshot, error) {
	receipt, err := d.c.POL.CaptureSnapshot(d.editor, polapi.CaptureSnapshot{IdempotencyKey: d.key(), SnapshotID: "snapshot",
		Purpose: polapi.PurposeCostEstimate, Refs: []polapi.ConfigRef{r.company, r.fx, r.policy, r.conversion, r.terms, r.payment, r.fee}})
	if err != nil {
		return DemoSnapshot{}, fmt.Errorf("%s: %w", polapi.CommandCaptureSnapshot, err)
	}
	d.record(d.editor, polapi.CommandCaptureSnapshot, receipt.ActionID, receipt.ObjectID, receipt.RowVersion, receipt.Status, receipt.Replayed)
	v, err := d.c.POL.GetSnapshot(d.editor, "snapshot")
	if err != nil {
		return DemoSnapshot{}, err
	}
	s := DemoSnapshot{SnapshotID: v.SnapshotID, Purpose: v.Purpose, CapturedAt: kernel.FormatTimestamp(v.CapturedAt),
		CapturedBy: string(v.CapturedBy.Kind) + ":" + v.CapturedBy.ID, ContentHash: v.ContentHash}
	for _, e := range v.Entries {
		s.Entries = append(s.Entries, demoRef(e.Ref, e.ContentHash))
	}
	return s, nil
}

// lookups computes the three values through POL's published lookups with kernel arithmetic (Python
// foundation.fx.convert, foundation.unit.convert and foundation.tax.preview, which Brant does not
// offer as commands, S1-D21). Each lookup must adopt the revision the snapshot holds.
func (d *demo) lookups(r *DemoResult, refs demoRefs, sku catapi.SkuRef) error {
	at := kernel.Instant(time.Now())
	stamp := kernel.FormatTimestamp(at)
	held := func(a DemoConfigRef) error {
		if !slices.Contains(r.Snapshot.Entries, a) {
			return fmt.Errorf("the lookup adopted %s revision %d, which the snapshot does not hold", polapi.ObjectID(a.Kind, a.ID), a.Revision)
		}
		return nil
	}

	rate, err := d.c.POL.LookupFX(d.editor, polapi.FXQuery{FromCurrency: "USD", ToCurrency: "CNY", Purpose: polapi.PurposeCostEstimate, At: at,
		Rate: &refs.fx, Policy: &refs.policy})
	if err != nil {
		return fmt.Errorf("LookupFX: %w", err)
	}
	amount, err := kernel.ParseAmount(rate.Value.From(), "120")
	if err != nil {
		return err
	}
	converted, err := rate.Value.Convert(amount)
	if err != nil {
		return err
	}
	r.FX = DemoAmount{Amount: converted.Amount(), Currency: converted.Currency().Code()}
	fxLookup := DemoLookup{Query: "LookupFX", At: stamp, Input: amount.String(), Adopted: demoRef(rate.Rate, rate.ContentHash),
		Factor: rate.Value.Rate().String(), Result: converted.String()}

	conversion, err := d.c.POL.LookupUnitConversion(d.editor, polapi.UnitQuery{Conversion: refs.conversion, SkuID: sku.SkuID,
		SpecRevision: sku.SpecRevision, At: at})
	if err != nil {
		return fmt.Errorf("LookupUnitConversion: %w", err)
	}
	two, err := kernel.ParseQuantity("2", conversion.From)
	if err != nil {
		return err
	}
	pcs, err := conversion.Convert(two)
	if err != nil {
		return err
	}
	r.Unit = DemoQuantity{Quantity: pcs.Value().String(), Unit: string(pcs.Unit())}
	unitLookup := DemoLookup{Query: "LookupUnitConversion", At: stamp, Input: two.String(), Adopted: demoRef(conversion.Conversion, conversion.ContentHash),
		Factor: conversion.Factor.String(), Result: pcs.String()}

	tax, err := d.c.POL.LookupTax(d.editor, polapi.TaxQuery{Profile: refs.tax, Currency: "CNY", At: at})
	if err != nil {
		return fmt.Errorf("LookupTax: %w", err)
	}
	gross, err := kernel.ParseDecimal("113", kernel.PriceFormat)
	if err != nil {
		return err
	}
	split, err := tax.Split(gross)
	if err != nil {
		return err
	}
	r.Tax = DemoTaxSplit{NetAmount: split.Net.Amount(), TaxAmount: split.Tax.Amount(), GrossAmount: split.Gross.Amount(),
		Currency: split.Gross.Currency().Code()}
	taxLookup := DemoLookup{Query: "LookupTax", At: stamp, Input: gross.String() + " " + tax.Currency.Code() + " " + tax.PriceBasis,
		Adopted: demoRef(tax.Profile, tax.ContentHash), Factor: tax.Rate.String(),
		Result: "net " + split.Net.String() + ", tax " + split.Tax.String() + ", gross " + split.Gross.String()}

	r.Lookups = []DemoLookup{fxLookup, unitLookup, taxLookup}
	for _, l := range r.Lookups {
		if err = held(l.Adopted); err != nil {
			return err
		}
	}
	return nil
}
