package application

import (
	"slices"
	"strconv"
	"time"
	_ "time/tzdata" // delivery time zones do not depend on the host's zone database

	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/contexts/sup/api"
	"go.idp.xyz/idp-brant/internal/contexts/sup/internal/domain/pricing"
	"go.idp.xyz/idp-brant/internal/contexts/sup/internal/domain/revision"
	"go.idp.xyz/idp-brant/internal/contexts/sup/internal/domain/supplierrfq"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Command-time admission of SUP's documents (Python write_admission: _access_document and
// _document_ready). A draft may lack anything; the references it states must exist and be visible
// to the caller, of the right kind and role. Sealing a draft (verify, publish, record_issued)
// requires, in this order: complete content (DRAFT_NOT_READY), a validity containing the instant
// (SUPPLY_CONDITION_EXPIRED), current references — the supplier confirmed at the bound row version
// and the RFQ ready at it (REFERENCE_STALE otherwise), POL configurations in force (POL's
// CONFIG_NOT_ACTIVE, CONFIG_EXPIRED), SKU specifications current (CAT's REFERENCE_STALE), sources
// readable — and then the rules of the content: the lines it answers, the origin, the pricing rules
// SUP shares with CST (domain/pricing), the tax applications and the delivery commitments. What only
// a costing scenario can decide stays with CST.

// data is where a document's content sits in its save_draft request; rejections about stored
// content point there.
const data = "/args/data"

// own are SUP's own facts a command is judged against, read in SUP's transaction.
type own struct {
	supplierRfq        *supplierrfq.SupplierRfq // the supplier RFQ an offer answers; nil when missing or not visible
	supplierRfqContent *api.SupplierRfq
	source             *revision.Revision // the source offer revision of purchase terms; nil when missing or not visible
	sourceContent      *api.SupplierOffer
}

// judge judges a document against facts at instant at; seal asks for a complete, current document.
type judge struct {
	f    facts
	at   time.Time
	seal bool
}

func notReady(path string) error { return kernel.Reject(kernel.CodeDraftNotReady, path, "") }

func invalid(path, reason string) error {
	return kernel.Reject(kernel.CodeReferenceInvalid, path, reason)
}

func stale(path, reason string) error {
	c, _ := api.Category(api.CodeReferenceStale)
	return kernel.RejectAs(api.CodeReferenceStale, c, path, reason)
}

func at(path string, i int, field string) string {
	if field == "" {
		return path + "/" + strconv.Itoa(i)
	}
	return path + "/" + strconv.Itoa(i) + "/" + field
}

// party judges a supplier or service party: it exists for the caller with the supplier role and,
// when sealing, is confirmed at the row version bound.
func (j judge) party(ref *api.PartyRef, path string) error {
	p, ok := j.f.parties[ref.PartyID]
	if !ok || !slices.Contains(p.Roles, ptyapi.RoleSupplier) {
		return invalid(path, "The party is not a supplier the caller sees")
	}
	if j.seal && (p.Status != ptyapi.StatusConfirmed || p.RowVersion != ref.RowVersion) {
		return stale(path, "The supplier is not confirmed at the bound row version")
	}
	return nil
}

// rfq judges a customer RFQ reference: it exists for the caller and, when sealing, is ready at the
// row version bound (RfqLineRef v1).
func (j judge) rfq(ref *api.RfqRef, path string) (inqapi.RfqRef, error) {
	r, ok := j.f.rfqs[ref.RfqID]
	if !ok {
		return r, invalid(path, "The RFQ does not exist for the caller")
	}
	if j.seal && (r.Status != inqapi.StatusReady || r.RowVersion != ref.RowVersion) {
		return r, stale(path, "The RFQ is not ready at the bound row version")
	}
	return r, nil
}

func (j judge) sources(ev []api.Evidence, path string) error {
	for i, e := range ev {
		if _, ok := j.f.sources[e.SourceID]; !ok {
			return invalid(at(path, i, "source_id"), "Source is not accessible")
		}
	}
	return nil
}

// config judges a POL reference: it exists and, when sealing, is in force.
func (j judge) config(ref *api.ConfigRef, path string) (polapi.ResolvedConfig, error) {
	c, ok := j.f.configs[polRef(ref)]
	if !ok {
		return c, invalid(path, "The configuration revision does not exist")
	}
	if j.seal {
		return c, c.Check(path)
	}
	return c, nil
}

// spec judges a SKU specification reference: when sealing it is the SKU's current confirmed
// revision; a draft may cite a revision since revised, never one that was not confirmed.
func (j judge) spec(ref *api.SkuRef, path string) error {
	s := j.f.specs[catapi.SpecRef{SkuID: ref.SkuID, SpecRevision: ref.SpecRevision}]
	if j.seal || (!s.Current && s.Code != catapi.CodeReferenceStale) {
		return s.Check(path)
	}
	return nil
}

func lineConfigs(l api.ConditionLine) []struct {
	ref  *api.ConfigRef
	name string
} {
	out := []struct {
		ref  *api.ConfigRef
		name string
	}{
		{l.PurchaseUnitRef, "purchase_unit_ref"}, {l.PriceUnitRef, "price_unit_ref"}, {l.TierUnitRef, "tier_unit_ref"},
		{l.CurrencyRef, "currency_ref"}, {l.TaxProfileRef, "tax_profile_ref"}, {l.TaxApplicationRef, "tax_application_ref"},
		{l.TradeTermsRef, "trade_terms_ref"}, {l.PaymentTermsRef, "payment_terms_ref"},
	}
	if l.MOQ != nil {
		out = append(out, struct {
			ref  *api.ConfigRef
			name string
		}{l.MOQ.UnitRef, "moq/unit_ref"})
	}
	for i := range l.ConversionRefs {
		out = append(out, struct {
			ref  *api.ConfigRef
			name string
		}{&l.ConversionRefs[i], "conversion_refs/" + strconv.Itoa(i)})
	}
	if l.Delivery != nil && l.Delivery.CalendarRef != nil {
		out = append(out, struct {
			ref  *api.ConfigRef
			name string
		}{l.Delivery.CalendarRef, "delivery/calendar_ref"})
	}
	return out
}

// lineRefs judges the references of condition lines.
func (j judge) lineRefs(ls []api.ConditionLine, path string) error {
	for i, l := range ls {
		p := at(path, i, "")
		if l.Sku != nil {
			if err := j.spec(l.Sku, p+"/sku"); err != nil {
				return err
			}
		}
		for _, c := range lineConfigs(l) {
			if c.ref == nil {
				continue
			}
			if _, err := j.config(c.ref, p+"/"+c.name); err != nil {
				return err
			}
		}
	}
	return nil
}

// validity judges, when sealing, that the content's validity contains the instant.
func (j judge) validity(from, until string) error {
	if !j.seal {
		return nil
	}
	switch {
	case j.at.Before(instant(from)):
		return api.Reject(api.CodeSupplyConditionExpired, data+"/valid_from")
	case !j.at.Before(instant(until)):
		return api.Reject(api.CodeSupplyConditionExpired, data+"/valid_until")
	}
	return nil
}

// complete returns DRAFT_NOT_READY at the first missing field, in order.
func complete(fields ...struct {
	missing bool
	path    string
}) error {
	for _, f := range fields {
		if f.missing {
			return notReady(f.path)
		}
	}
	return nil
}

type field = struct {
	missing bool
	path    string
}

func lineComplete(ls []api.ConditionLine, path string) error {
	for i, l := range ls {
		p := at(path, i, "")
		if err := complete(
			field{l.Sku == nil, p + "/sku"}, field{l.PurchaseUnitRef == nil, p + "/purchase_unit_ref"},
			field{l.PriceUnitRef == nil, p + "/price_unit_ref"}, field{l.PriceBasisQuantity == "", p + "/price_basis_quantity"},
			field{l.TierUnitRef == nil, p + "/tier_unit_ref"}, field{len(l.Tiers) == 0, p + "/tiers"},
			field{l.MOQ == nil, p + "/moq"}, field{l.CurrencyRef == nil, p + "/currency_ref"},
			field{l.TaxProfileRef == nil, p + "/tax_profile_ref"}, field{l.TaxApplicationRef == nil, p + "/tax_application_ref"},
			field{l.TradeTermsRef == nil, p + "/trade_terms_ref"}, field{l.PaymentTermsRef == nil, p + "/payment_terms_ref"},
			field{l.Delivery == nil, p + "/delivery"},
		); err != nil {
			return err
		}
	}
	return nil
}

func decimal(s string, f kernel.DecimalFormat) kernel.Decimal {
	d, _ := kernel.ParseDecimal(s, f) // the contract admitted it; empty is 0
	return d
}

// condition returns line l as the pricing rules need it; its references are in force.
func (j judge) condition(l api.ConditionLine, path string) (pricing.Condition, error) {
	precision := 0
	if u := j.f.configs[polRef(l.PurchaseUnitRef)].Data.Unit; u != nil {
		precision = u.Precision
	}
	c := pricing.Condition{Path: path, LineID: l.ID, PurchaseUnit: kernel.UnitCode(l.PurchaseUnitRef.ConfigID), PurchasePrecision: precision,
		PriceUnit: kernel.UnitCode(l.PriceUnitRef.ConfigID), TierUnit: kernel.UnitCode(l.TierUnitRef.ConfigID),
		Price: schedule(path, l.PricingMode, l.PriceTierScope, l.PriceBasisQuantity, l.Tiers), OrderMultiple: decimal(l.OrderMultiple, kernel.QuantityFormat),
		IncludedObligations: l.IncludedObligationIDs}
	m := l.MOQ
	q, err := kernel.NewQuantity(decimal(m.Quantity, kernel.QuantityFormat), kernel.UnitCode(m.UnitRef.ConfigID))
	if err != nil {
		return c, invalid(path+"/moq/unit_ref", "The unit is not a unit code")
	}
	c.MOQ = pricing.MOQ{Quantity: q, Scope: pricing.MOQScope(m.Scope), GroupID: m.GroupID, Members: m.MemberLineIDs}
	return c, nil
}

func schedule(path, mode, scope, basis string, ts []api.Tier) pricing.Schedule {
	s := pricing.Schedule{Path: path, Mode: pricing.PricingMode(mode), Scope: pricing.TierScope(scope),
		BasisQuantity: decimal(basis, kernel.QuantityFormat), Tiers: make([]pricing.Tier, len(ts))}
	for i, t := range ts {
		s.Tiers[i] = pricing.Tier{Lower: decimal(t.LowerInclusive, kernel.QuantityFormat), UnitPrice: decimal(t.UnitPrice, kernel.PriceFormat),
			ZeroPriceReason: t.ZeroPriceReason}
		if t.UpperExclusive != "" {
			s.Tiers[i].Upper, s.Tiers[i].Bounded = decimal(t.UpperExclusive, kernel.QuantityFormat), true
		}
	}
	return s
}

// conditions applies, when sealing, the rules of condition lines: the pricing rules SUP shares with
// CST over all lines (demand: the request lines an explicit MOQ group may name), then each line's
// item code mapping, tax application, conversions and delivery.
func (j judge) conditions(ls []api.ConditionLine, path string, demand []string, supplier string) error {
	cs := make([]pricing.Condition, len(ls))
	for i, l := range ls {
		var err error
		if cs[i], err = j.condition(l, at(path, i, "")); err != nil {
			return err
		}
	}
	if err := pricing.CheckConditions(cs, demand); err != nil {
		return err
	}
	for i, l := range ls {
		p := at(path, i, "")
		if m := l.ItemCodeMapping; m != nil {
			code := j.f.codes[catapi.ItemCode{PartyID: supplier, PartyRole: catapi.RoleSupplier, ExternalCode: l.SupplierItemCode}]
			if !code.Mapped() || code.Mapping != (catapi.ItemCodeMappingRef{MappingID: m.MappingID, Revision: m.Revision}) ||
				code.Sku != (catapi.SpecRef{SkuID: l.Sku.SkuID, SpecRevision: l.Sku.SpecRevision}) {
				return invalid(p+"/item_code_mapping", "CAT does not map the supplier's item code to the line's SKU with this mapping revision")
			}
		}
		a := j.f.configs[polRef(l.TaxApplicationRef)].Data.TaxApplication
		if a == nil || a.Side != polapi.TaxSidePurchase || a.SubjectKind != polapi.SubjectSKU || a.SubjectID != l.Sku.SkuID ||
			(a.PartyID != "" && a.PartyID != supplier) || a.TaxProfile != polRef(l.TaxProfileRef) {
			return invalid(p+"/tax_application_ref", "The tax application does not apply to purchasing the line's SKU from the supplier with its tax profile")
		}
		for k := range l.ConversionRefs {
			u := j.f.configs[polRef(&l.ConversionRefs[k])].Data.UnitConversion
			if u == nil || u.SkuID != l.Sku.SkuID || u.SpecRevision != l.Sku.SpecRevision {
				return invalid(at(p+"/conversion_refs", k, ""), "The conversion is for another SKU or specification revision")
			}
		}
		if err := j.delivery(l.Delivery, p+"/delivery"); err != nil {
			return err
		}
	}
	return nil
}

// delivery judges a delivery commitment (Python resolve_delivery at verification): a known IANA
// time zone, and for a lead time a calendar in the same time zone (INVALID_KNOWN_VALUE).
func (j judge) delivery(d *api.Delivery, path string) error {
	if _, err := time.LoadLocation(d.TimeZone); err != nil || d.TimeZone == "Local" {
		return kernel.Reject(kernel.CodeInvalidKnownValue, path+"/time_zone", "Unknown IANA time zone")
	}
	if d.Kind == api.DeliveryLeadTime {
		cal := j.f.configs[polRef(d.CalendarRef)].Data.Calendar
		if cal == nil || cal.TimeZone != d.TimeZone {
			return kernel.Reject(kernel.CodeInvalidKnownValue, path+"/calendar_ref", "The calendar's time zone differs from the delivery's")
		}
	}
	return nil
}

// covered judges the RFQ lines a document answers: each once (DUPLICATE_LINE_ID) and a line of the
// RFQ (REFERENCE_INVALID).
func covered(ids []string, rfq inqapi.RfqRef, path string) error {
	if err := pricing.Unique(ids, path); err != nil {
		return err
	}
	for i, id := range ids {
		if !slices.ContainsFunc(rfq.Lines, func(l inqapi.RfqRefLine) bool { return l.LineID == id }) {
			return invalid(at(path, i, ""), "The RFQ has no such line")
		}
	}
	return nil
}

// offer judges a supplier offer (T02 §5, §13.1).
func (j judge) offer(o *api.SupplierOffer, w own) error {
	if j.seal {
		if err := complete(field{o.Supplier == nil, data + "/supplier"}, field{o.Origin == nil, data + "/origin"},
			field{o.Rfq == nil, data + "/rfq"}, field{len(o.RfqLineIDs) == 0, data + "/rfq_line_ids"},
			field{o.ValidFrom == "", data + "/valid_from"}, field{o.ValidUntil == "", data + "/valid_until"},
			field{len(o.Lines) == 0, data + "/lines"}, field{len(o.Evidence) == 0, data + "/evidence"}); err != nil {
			return err
		}
		if err := lineComplete(o.Lines, data+"/lines"); err != nil {
			return err
		}
		if err := j.validity(o.ValidFrom, o.ValidUntil); err != nil {
			return err
		}
	}
	if o.Supplier != nil {
		if err := j.party(o.Supplier, data+"/supplier"); err != nil {
			return err
		}
	}
	if o.Origin != nil && o.Origin.SupplierRfq != nil && w.supplierRfq == nil {
		return invalid(data+"/origin/supplier_rfq", "The supplier RFQ does not exist for the caller")
	}
	var rfq inqapi.RfqRef
	if o.Rfq != nil {
		var err error
		if rfq, err = j.rfq(o.Rfq, data+"/rfq"); err != nil {
			return err
		}
	}
	if err := j.sources(o.Evidence, data+"/evidence"); err != nil {
		return err
	}
	if err := j.lineRefs(o.Lines, data+"/lines"); err != nil {
		return err
	}
	if !j.seal {
		return nil
	}
	if err := covered(o.RfqLineIDs, rfq, data+"/rfq_line_ids"); err != nil {
		return err
	}
	for i, l := range o.Lines {
		if !slices.ContainsFunc(rfq.Lines, func(r inqapi.RfqRefLine) bool {
			return slices.Contains(o.RfqLineIDs, r.LineID) && r.SkuID == l.Sku.SkuID && r.SkuSpecRevision == l.Sku.SpecRevision
		}) {
			return invalid(at(data+"/lines", i, "sku"), "The SKU is not the SKU of an RFQ line the offer answers")
		}
	}
	if o.Origin.Kind == api.OriginInResponseToRfq {
		ref, path := o.Origin.SupplierRfq, data+"/origin/supplier_rfq"
		if !w.supplierRfq.Answerable(ref.RowVersion) {
			return stale(path, "The supplier RFQ was not issued at the bound row version")
		}
		req := w.supplierRfqContent
		if req.Supplier == nil || req.Supplier.PartyID != o.Supplier.PartyID || req.Rfq == nil || *req.Rfq != *o.Rfq {
			return invalid(path, "The supplier RFQ asked another supplier or about another RFQ")
		}
		for _, id := range o.RfqLineIDs {
			if !slices.Contains(req.RfqLineIDs, id) {
				return invalid(path, "The offer answers lines the supplier RFQ did not ask about")
			}
		}
	}
	return j.conditions(o.Lines, data+"/lines", o.RfqLineIDs, o.Supplier.PartyID)
}

// terms judges purchase terms (T02 §5 可复用条件, §13.8; A46): formed from a verified offer revision
// of the same supplier; of dependent validity only while that revision is in force.
func (j judge) terms(t *api.PurchaseTerms, w own) error {
	if j.seal {
		if err := complete(field{t.Supplier == nil, data + "/supplier"}, field{t.SourceOfferRef == nil, data + "/source_offer_ref"},
			field{t.ValidityMode == "", data + "/validity_mode"}, field{t.ValidFrom == "", data + "/valid_from"},
			field{t.ValidUntil == "", data + "/valid_until"}, field{len(t.Lines) == 0, data + "/lines"},
			field{len(t.Evidence) == 0, data + "/evidence"}); err != nil {
			return err
		}
		if err := lineComplete(t.Lines, data+"/lines"); err != nil {
			return err
		}
		if err := j.validity(t.ValidFrom, t.ValidUntil); err != nil {
			return err
		}
	}
	if t.Supplier != nil {
		if err := j.party(t.Supplier, data+"/supplier"); err != nil {
			return err
		}
	}
	if t.SourceOfferRef != nil {
		path := data + "/source_offer_ref"
		if w.source == nil {
			return invalid(path, "The offer revision does not exist for the caller")
		}
		if j.seal {
			if !w.source.Sealed() {
				return invalid(path, "The offer revision is not verified")
			}
			if t.ValidityMode == api.ValidityDependent {
				if code := revision.InForce(w.source, j.at); code != "" {
					return api.Reject(code, path)
				}
			}
			if s := w.sourceContent.Supplier; s == nil || s.PartyID != t.Supplier.PartyID {
				return invalid(data+"/supplier", "The source offer is another supplier's")
			}
		}
	}
	if err := j.sources(t.Evidence, data+"/evidence"); err != nil {
		return err
	}
	if err := j.lineRefs(t.Lines, data+"/lines"); err != nil {
		return err
	}
	if !j.seal {
		return nil
	}
	return j.conditions(t.Lines, data+"/lines", w.sourceContent.RfqLineIDs, t.Supplier.PartyID)
}

// logistics judges a logistics offer (T02 §6, §13.1): exactly the verified service role's service,
// origin and destination, and charges that CST can evaluate.
func (j judge) logistics(l *api.LogisticsOffer) error {
	if j.seal {
		if err := complete(field{l.ServiceRoleRef == nil, data + "/service_role_ref"}, field{l.ServiceCode == "", data + "/service_code"},
			field{l.Mode == "", data + "/mode"}, field{l.Origin == "", data + "/origin"}, field{l.Destination == "", data + "/destination"},
			field{l.ValidFrom == "", data + "/valid_from"}, field{l.ValidUntil == "", data + "/valid_until"},
			field{len(l.Fees) == 0, data + "/fees"}, field{len(l.Evidence) == 0, data + "/evidence"}); err != nil {
			return err
		}
		for i, f := range l.Fees {
			if err := feeComplete(f, at(data+"/fees", i, "")); err != nil {
				return err
			}
		}
		if err := j.validity(l.ValidFrom, l.ValidUntil); err != nil {
			return err
		}
	}
	var role ptyapi.ResolvedServiceRole
	if s := l.ServiceRoleRef; s != nil {
		path := data + "/service_role_ref"
		var ok bool
		if role, ok = j.f.roles[ptyapi.ServiceRoleRef{PartyID: s.PartyID, ServiceRoleID: s.ServiceRoleID, Revision: s.Revision}]; !ok {
			return invalid(path, "The service role revision does not exist for the caller")
		}
		if j.seal {
			if err := role.Check(path); err != nil {
				return err
			}
		}
	}
	if err := j.sources(l.Evidence, data+"/evidence"); err != nil {
		return err
	}
	for i, f := range l.Fees {
		p := at(data+"/fees", i, "")
		for _, c := range []struct {
			ref  *api.ConfigRef
			name string
		}{{f.FeeTypeRef, "fee_type_ref"}, {f.CurrencyRef, "currency_ref"}, {f.TaxProfileRef, "tax_profile_ref"},
			{f.TaxApplicationRef, "tax_application_ref"}} {
			if c.ref == nil {
				continue
			}
			if _, err := j.config(c.ref, p+"/"+c.name); err != nil {
				return err
			}
		}
		if f.Billing != nil && f.Billing.UnitRef != nil {
			if _, err := j.config(f.Billing.UnitRef, p+"/billing/unit_ref"); err != nil {
				return err
			}
		}
	}
	if !j.seal {
		return nil
	}
	if role.ServiceCode != l.ServiceCode || role.Origin != l.Origin || role.Destination != l.Destination {
		return invalid(data+"/service_role_ref", "The offer's service, origin or destination is not the service role's")
	}
	fees := make([]pricing.Fee, len(l.Fees))
	for i, f := range l.Fees {
		fees[i] = fee(f, at(data+"/fees", i, ""))
	}
	if err := pricing.CheckFees(fees); err != nil {
		return err
	}
	for i, f := range l.Fees {
		p := at(data+"/fees", i, "")
		if err := fees[i].Chargeable.CheckMeasured(); err != nil {
			return err
		}
		a := j.f.configs[polRef(f.TaxApplicationRef)].Data.TaxApplication
		if a == nil || a.Side != polapi.TaxSidePurchase || a.SubjectKind != polapi.SubjectFeeCategory || a.SubjectID != f.FeeTypeRef.ConfigID ||
			(a.PartyID != "" && a.PartyID != l.ServiceRoleRef.PartyID) || a.TaxProfile != polRef(f.TaxProfileRef) {
			return invalid(p+"/tax_application_ref", "The tax application does not apply to buying the fee type from the service party with its tax profile")
		}
	}
	return nil
}

// feeComplete returns DRAFT_NOT_READY at the first missing field of a charge, including the fields
// its tariff and chargeable basis need.
func feeComplete(f api.Fee, p string) error {
	if err := complete(field{f.ObligationID == "", p + "/obligation_id"}, field{f.FeeTypeRef == nil, p + "/fee_type_ref"},
		field{f.ResponsibleParty == "", p + "/responsible_party"}, field{f.CurrencyRef == nil, p + "/currency_ref"},
		field{f.TaxProfileRef == nil, p + "/tax_profile_ref"}, field{f.TaxApplicationRef == nil, p + "/tax_application_ref"},
		field{f.Billing == nil, p + "/billing"}, field{f.Chargeable == nil, p + "/chargeable"},
		field{f.Allocation == nil, p + "/allocation"}); err != nil {
		return err
	}
	b, c := f.Billing, f.Chargeable
	switch b.Kind {
	case api.BillingFixed:
		if err := complete(field{b.Amount == "", p + "/billing/amount"}); err != nil {
			return err
		}
	case api.BillingPerBasis, api.BillingTiered:
		if err := complete(field{b.Kind == api.BillingPerBasis && b.Rate == "", p + "/billing/rate"},
			field{b.Kind == api.BillingTiered && len(b.Tiers) == 0, p + "/billing/tiers"},
			field{b.BasisQuantity == "", p + "/billing/basis_quantity"}, field{b.UnitRef == nil, p + "/billing/unit_ref"},
			field{b.MinimumAmount == "", p + "/billing/minimum_amount"}); err != nil {
			return err
		}
	}
	switch c.Kind {
	case api.ChargeableActualWeight, api.ChargeableVolume, api.ChargeableMaxActualVolumetric:
		volumetric := c.Kind == api.ChargeableMaxActualVolumetric
		return complete(field{c.BillingIncrement == "", p + "/chargeable/billing_increment"},
			field{c.RoundingMode == "", p + "/chargeable/rounding_mode"}, field{c.RoundingScope == "", p + "/chargeable/rounding_scope"},
			field{volumetric && c.MaxScope == "", p + "/chargeable/max_scope"},
			field{volumetric && c.DivisorM3PerKG == "", p + "/chargeable/divisor_m3_per_kg"})
	}
	return nil
}

// fee returns a complete charge as the pricing rules need it.
func fee(f api.Fee, p string) pricing.Fee {
	b, c, a := f.Billing, f.Chargeable, f.Allocation
	out := pricing.Fee{Path: p, ID: f.ID, ObligationID: f.ObligationID, Party: pricing.Party(f.ResponsibleParty),
		Chargeable: pricing.ChargeableRule{Path: p + "/chargeable", Kind: pricing.ChargeableKind(c.Kind),
			BillingIncrement: decimal(c.BillingIncrement, kernel.QuantityFormat), Rounding: pricing.Rounding(c.RoundingMode),
			RoundingScope: pricing.RoundingScope(c.RoundingScope), MaxScope: pricing.RoundingScope(c.MaxScope),
			DivisorM3PerKG: decimal(c.DivisorM3PerKG, kernel.FactorFormat), Measured: c.MeasurementRef != nil},
		Billing: pricing.Billing{Path: p + "/billing", Kind: pricing.BillingKind(b.Kind), Amount: decimal(b.Amount, kernel.PriceFormat),
			Rate: decimal(b.Rate, kernel.PriceFormat), BasisQuantity: decimal(b.BasisQuantity, kernel.QuantityFormat),
			Minimum: decimal(b.MinimumAmount, kernel.PriceFormat), ZeroPriceReason: f.ZeroPriceReason,
			Tiers: schedule(p+"/billing", b.PricingMode, "", b.BasisQuantity, b.Tiers)},
		Allocation: pricing.AllocationRule{Path: p + "/allocation", Basis: pricing.Basis(a.Basis), Targets: a.TargetLineIDs,
			Measured: a.MeasurementRef != nil},
	}
	if b.UnitRef != nil {
		out.Billing.Unit = kernel.UnitCode(b.UnitRef.ConfigID)
	}
	for _, w := range a.Weights {
		n, _ := strconv.ParseInt(w.Weight, 10, 64) // at most 18 digits (contract)
		out.Allocation.Weights = append(out.Allocation.Weights, pricing.ExplicitWeight{LineID: w.LineID, Weight: n})
	}
	return out
}

// supplierRfq judges a supplier RFQ (T02 §13.1, §17.1): a supplier and the lines of a ready RFQ.
func (j judge) supplierRfq(d *api.SupplierRfq) error {
	if j.seal {
		if err := complete(field{d.Supplier == nil, data + "/supplier"}, field{d.Rfq == nil, data + "/rfq"},
			field{len(d.RfqLineIDs) == 0, data + "/rfq_line_ids"}); err != nil {
			return err
		}
	}
	if d.Supplier != nil {
		if err := j.party(d.Supplier, data+"/supplier"); err != nil {
			return err
		}
	}
	var rfq inqapi.RfqRef
	if d.Rfq != nil {
		var err error
		if rfq, err = j.rfq(d.Rfq, data+"/rfq"); err != nil {
			return err
		}
	}
	if err := j.sources(d.Evidence, data+"/evidence"); err != nil {
		return err
	}
	if !j.seal {
		return nil
	}
	return covered(d.RfqLineIDs, rfq, data+"/rfq_line_ids")
}
