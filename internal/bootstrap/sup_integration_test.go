//go:build integration

package bootstrap_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/bootstrap"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	supapi "go.idp.xyz/idp-brant/internal/contexts/sup/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The SUP suite ports the supply condition parts of Python V0.50 tests/test_costing_lifecycle.py,
// test_costing_failures.py and test_costing_domains.py. Its fixture is Python's
// costing_fixture.TradeFixture as Brant's upstream contexts hold it: the ready customer RFQ rfq.1
// (L1: 120 PCS of sku.1), supplier.1 confirmed as a supplier, the POL configurations an offer line
// cites (PCS, USD, the zero-rate tax profile tax.zero and its purchase application tax.goods,
// payment and trade terms), and for logistics the forwarder provider.1 with its verified service
// role, KG, the fee category freight and its application tax.service. The author edits, the
// reviewer verifies: humans with every permission and company data scope, as in Python.

type supWorld struct {
	*world
	author, reviewer context.Context
	win              window
	pcs, kg, usd     polapi.ConfigRef
	tax, goods       polapi.ConfigRef
	service, freight polapi.ConfigRef
	payment, trade   polapi.ConfigRef
	rfqVersion       int64
	supplierVersion  int64
	spec             int64
	role             supapi.ServiceRoleRef
	db               *bentopg.DB // the application role's pool for direct reads, opened once
}

func newSupWorld(t *testing.T) *supWorld {
	w := newWorld(t)
	s := &supWorld{world: w, win: newWindow()}
	s.author, s.reviewer = w.polActors()
	ctx := s.author
	cmd := w.readyArgs(ctx)
	_, err := w.save(ctx, cmd)
	w.must(err)
	ready, err := w.confirm(ctx, "rfq.1", 1, "src.1")
	w.must(err)
	s.rfqVersion = ready.RowVersion
	sku, err := w.c.CAT.GetSku(ctx, "sku.1")
	w.must(err)
	s.spec = sku.Revisions[len(sku.Revisions)-1].Revision
	s.supplierVersion = w.staffParty(ctx, "supplier.1", []string{ptyapi.RoleSupplier}).RowVersion
	provider := w.staffParty(ctx, "provider.1", []string{ptyapi.RoleSupplier})
	role, err := w.verifyRole(ctx, "provider.1", provider.RowVersion, "role.1", s.win.from, s.win.until, "evidence")
	w.must(err)
	_ = role
	s.role = supapi.ServiceRoleRef{PartyID: "provider.1", ServiceRoleID: "role.1", Revision: 1}

	r, rev := s.reviewer, s.win
	s.pcs = w.configure(ctx, r, polapi.KindUnit, "PCS", 0, rev.from, time.Time{},
		polapi.ConfigData{Unit: &polapi.Unit{Code: "PCS", Name: "件", Dimension: polapi.DimensionCount, Precision: 0}}, true)
	s.kg = w.configure(ctx, r, polapi.KindUnit, "KG", 0, rev.from, time.Time{},
		polapi.ConfigData{Unit: &polapi.Unit{Code: "KG", Name: "千克", Dimension: polapi.DimensionMass, Precision: 3}}, true)
	s.usd = w.currency(ctx, r, "USD", rev)
	s.tax = w.configure(ctx, r, polapi.KindTaxProfile, "tax.zero", 0, rev.from, time.Time{}, polapi.ConfigData{TaxProfile: &polapi.TaxProfile{
		Name: "SYNTHETIC ZERO", Rate: "0", PriceBasis: polapi.PriceBasisExclusive, RecoveryTreatment: polapi.RecoveryNonrecoverable,
		PolicyBasis: "test only, not a legal determination"}}, true)
	s.freight = w.configure(ctx, r, polapi.KindFeeCategory, "freight", 0, rev.from, time.Time{}, polapi.ConfigData{FeeCategory: &polapi.FeeCategory{
		Name: "海运费", Category: "freight", Side: polapi.FeeSidePurchaseCost, Inclusion: polapi.InclusionSeparate,
		AllocationBasis: polapi.AllocationQuantity, PolicyBasis: "synthetic"}}, true)
	s.goods = w.configure(ctx, r, polapi.KindTaxApplication, "tax.goods", 0, rev.from, time.Time{}, polapi.ConfigData{TaxApplication: &polapi.TaxApplication{
		Name: "goods", Side: polapi.TaxSidePurchase, SubjectKind: polapi.SubjectSKU, SubjectID: "sku.1", PartyID: "supplier.1",
		TaxProfile: s.tax, Jurisdiction: "CN", BasisNote: "synthetic"}}, true)
	s.service = w.configure(ctx, r, polapi.KindTaxApplication, "tax.service", 0, rev.from, time.Time{}, polapi.ConfigData{TaxApplication: &polapi.TaxApplication{
		Name: "service", Side: polapi.TaxSidePurchase, SubjectKind: polapi.SubjectFeeCategory, SubjectID: "freight", PartyID: "provider.1",
		TaxProfile: s.tax, Jurisdiction: "CN", BasisNote: "synthetic"}}, true)
	payment, trade := w.settlement(ctx, r)
	s.payment = polapi.ConfigRef{Kind: payment.Kind, ConfigID: payment.ConfigID, Revision: payment.Revision}
	s.trade = polapi.ConfigRef{Kind: trade.Kind, ConfigID: trade.ConfigID, Revision: trade.Revision}
	return s
}

func cref(r polapi.ConfigRef) *supapi.ConfigRef {
	return &supapi.ConfigRef{Kind: r.Kind, ConfigID: r.ConfigID, Revision: r.Revision}
}

func evidenceOf(locator string) []supapi.Evidence {
	return []supapi.Evidence{{SourceID: "evidence", Locator: locator}}
}

// line is Python's offer line OL1: sku.1 in PCS, flat 5 USD per 1 PCS, MOQ 100 PCS per SKU.
func (s *supWorld) line() supapi.ConditionLine {
	return supapi.ConditionLine{ID: "OL1", Sku: &supapi.SkuRef{SkuID: "sku.1", SpecRevision: s.spec},
		PurchaseUnitRef: cref(s.pcs), PriceUnitRef: cref(s.pcs), PriceBasisQuantity: "1", TierUnitRef: cref(s.pcs),
		PriceTierScope: supapi.TierScopeSKU, PricingMode: supapi.PricingFlat, Tiers: []supapi.Tier{{LowerInclusive: "0", UnitPrice: "5"}},
		MOQ: &supapi.MOQ{Quantity: "100", UnitRef: cref(s.pcs), Scope: supapi.MOQScopeSKU}, CurrencyRef: cref(s.usd),
		TaxProfileRef: cref(s.tax), TaxApplicationRef: cref(s.goods), TradeTermsRef: cref(s.trade), PaymentTermsRef: cref(s.payment),
		Delivery: &supapi.Delivery{Kind: supapi.DeliveryFixed, Date: "2027-01-15", Milestone: supapi.MilestoneHandover, Place: "CNSHA",
			TimeZone: "Asia/Shanghai"}}
}

// offer is Python's supplier offer offer.1: unsolicited, answering L1 of rfq.1.
func (s *supWorld) offer() supapi.SupplierOffer {
	return supapi.SupplierOffer{Supplier: &supapi.PartyRef{PartyID: "supplier.1", RowVersion: s.supplierVersion},
		Origin: &supapi.Origin{Kind: supapi.OriginUnsolicited}, Rfq: &supapi.RfqRef{RfqID: "rfq.1", RowVersion: s.rfqVersion},
		RfqLineIDs: []string{"L1"}, ValidFrom: kernel.FormatTimestamp(s.win.from), ValidUntil: kernel.FormatTimestamp(s.win.until),
		Lines: []supapi.ConditionLine{s.line()}, Evidence: evidenceOf("line 1")}
}

// terms are purchase terms formed from offer revision 1 of id.
func (s *supWorld) terms(offerID, mode string) supapi.PurchaseTerms {
	o := s.offer()
	return supapi.PurchaseTerms{Supplier: o.Supplier, SourceOfferRef: &supapi.SupplyConditionRef{Kind: supapi.KindSupplierOffer, ID: offerID, Revision: 1},
		ValidityMode: mode, ValidFrom: o.ValidFrom, ValidUntil: o.ValidUntil, Lines: o.Lines, Evidence: o.Evidence}
}

// fee is Python's logistics fee F1: obligation freight.1, borne by the seller, 60 USD fixed,
// allocated by quantity to L1.
func (s *supWorld) fee() supapi.Fee {
	return supapi.Fee{ID: "F1", ObligationID: "freight.1", FeeTypeRef: cref(s.freight), ResponsibleParty: supapi.PartySeller,
		CurrencyRef: cref(s.usd), TaxProfileRef: cref(s.tax), TaxApplicationRef: cref(s.service),
		Billing: &supapi.Billing{Kind: supapi.BillingFixed, Amount: "60"}, Chargeable: &supapi.Chargeable{Kind: supapi.ChargeableFixed},
		Allocation: &supapi.Allocation{Basis: supapi.AllocationQuantity, TargetLineIDs: []string{"L1"}}}
}

// logistics is Python's logistics offer log.1 by provider.1's service role.
func (s *supWorld) logistics() supapi.LogisticsOffer {
	role := s.role
	return supapi.LogisticsOffer{ServiceRoleRef: &role, ServiceCode: "sea.fcl", Mode: supapi.ModeSea, Origin: "CNSHA", Destination: "DEHAM",
		ValidFrom: kernel.FormatTimestamp(s.win.from), ValidUntil: kernel.FormatTimestamp(s.win.until), Fees: []supapi.Fee{s.fee()},
		Evidence: evidenceOf("line 1")}
}

// draft returns the content of kind for a save of document id.
func (s *supWorld) save(ctx context.Context, kind, id string, expected int64, content any) (supapi.Receipt, error) {
	switch kind {
	case supapi.KindSupplierOffer:
		return s.c.SUP.SaveSupplierOffer(ctx, supapi.SaveSupplierOffer{IdempotencyKey: s.key(), OfferID: id, ExpectedVersion: expected,
			Data: content.(supapi.SupplierOffer)})
	case supapi.KindPurchaseTerms:
		return s.c.SUP.SavePurchaseTerms(ctx, supapi.SavePurchaseTerms{IdempotencyKey: s.key(), TermsID: id, ExpectedVersion: expected,
			Data: content.(supapi.PurchaseTerms)})
	}
	return s.c.SUP.SaveLogisticsOffer(ctx, supapi.SaveLogisticsOffer{IdempotencyKey: s.key(), OfferID: id, ExpectedVersion: expected,
		Data: content.(supapi.LogisticsOffer)})
}

func (s *supWorld) seal(ctx context.Context, kind, id string, expected, working, supersedes int64) (supapi.Receipt, error) {
	return s.c.SUP.Seal(ctx, supapi.Seal{IdempotencyKey: s.key(), Kind: kind, ID: id, ExpectedVersion: expected, WorkingRevision: working,
		SupersedesRevision: supersedes, Evidence: evidenceOf("核实依据")})
}

func (s *supWorld) end(ctx context.Context, kind, id string, expected, target int64) (supapi.Receipt, error) {
	return s.c.SUP.End(ctx, supapi.End{IdempotencyKey: s.key(), Kind: kind, ID: id, ExpectedVersion: expected, TargetRevision: target,
		Reason: "撤销", Evidence: evidenceOf("撤销依据")})
}

func (s *supWorld) revise(ctx context.Context, kind, id string, expected, base int64) (supapi.Receipt, error) {
	return s.c.SUP.Revise(ctx, supapi.Revise{IdempotencyKey: s.key(), Kind: kind, ID: id, ExpectedVersion: expected, BaseRevision: base,
		Reason: "修订"})
}

// verified saves content of kind as id and has the reviewer seal it; it returns revision 1's ref.
func (s *supWorld) verified(kind, id string, content any) supapi.SupplyConditionRef {
	s.t.Helper()
	_, err := s.save(s.author, kind, id, 0, content)
	s.must(err)
	_, err = s.seal(s.reviewer, kind, id, 1, 1, 0)
	s.must(err)
	return supapi.SupplyConditionRef{Kind: kind, ID: id, Revision: 1}
}

func (s *supWorld) condition(kind, id string) supapi.ConditionView {
	s.t.Helper()
	v, err := s.c.SUP.GetCondition(s.author, kind, id)
	s.must(err)
	return v
}

// other provisions another actor of company.A with company data scope.
func (s *supWorld) other(id string, kind kernel.ActorKind, permissions ...string) context.Context {
	return s.provision(bootstrap.Grant{Scope: s.scope("company.A"), Actor: kernel.ActorRef{Kind: kind, ID: id}, Permissions: permissions,
		DataScope: "company", Team: "trade"})
}

// sqlCode returns the SQLSTATE of a database error, or the rejection code.
func sqlCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return code(err)
}

// bootstrapGrant is a grant of a human in company.A with a data scope.
func bootstrapGrant(s *supWorld, id, dataScope string, permissions ...string) bootstrap.Grant {
	return bootstrapGrantIn(s, "company.A", id, dataScope, permissions...)
}

func bootstrapGrantIn(s *supWorld, company, id, dataScope string, permissions ...string) bootstrap.Grant {
	return bootstrap.Grant{Scope: s.scope(company), Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: id}, Permissions: permissions,
		DataScope: authorizationScope(dataScope)}
}
