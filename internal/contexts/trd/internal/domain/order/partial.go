package order

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/acceptance"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/calc"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Consumption is one append-only quote_order_consumption row a partial order writes (TRD-INV-024).
// Quantity is a QuantityFormat decimal, greater than zero. Cancelling the order does not remove it.
type Consumption struct {
	LineID   string
	Quantity string
}

// PartialLine is one line of order.prepare_partial: a line of the sealed quote and the quantity
// this order takes of it, in the line's unit.
type PartialLine struct {
	LineID   string
	Quantity string
}

// PartialCharge is a charge this order adds. AmountMinor is exact minor units of the quote
// currency on the sealed tax profile's price basis: the net when the tax is exclusive or not
// applicable, the gross when it is inclusive. Omitted charges add nothing; they are not a share
// of the quote's charges.
type PartialCharge struct {
	ChargeID    string
	AmountMinor string
}

// PartialStage is one stated payment amount. The stage must be one of the quote's payment plan,
// and the amounts of the plan must sum to this order's payable. An empty plan is recomputed from
// the quote's percentages.
type PartialStage struct {
	StageID     string
	AmountMinor string
}

// PartialPreparation is order.prepare_partial (TRD-INV-024). Consumed is the quantity already on
// the ledger for each line, summed by the caller under the quote-root lock.
type PartialPreparation struct {
	Preparation
	Lines    []PartialLine
	Charges  []PartialCharge
	Payments []PartialStage
	Currency kernel.Currency
	Consumed map[string]string
}

// PreparePartial decides order.prepare_partial (TRD-INV-024, F-01, BC-D06, SIM-03). The quote's
// active revision must be multi_order, inside its validity window, with no full_v1 slot taken.
// Each line's consumed quantity plus the request must fit the line's cap unless the line is
// uncapped. The order does not take quote_conversion_slot; it appends the ledger and moves the
// root version. Money is repriced by calc; nothing is floated.
func PreparePartial(p PartialPreparation, q *quote.Quote, a *acceptance.Acceptance, scope kernel.Scope, by kernel.ActorRef, epoch int64,
	now time.Time) (*Order, error) {
	if err := q.Usable(p.Revision, p.BusinessHash, "/args/business_revision"); err != nil {
		return nil, err
	}
	if a == nil {
		return nil, fail(acceptance.CodeAcceptanceScopeMismatch, "/args/acceptance_id", "No such acceptance of the revision")
	}
	if err := a.Check(p.QuoteID, p.Revision, p.BusinessHash, "/args/acceptance_id"); err != nil {
		return nil, err
	}
	rev := q.Revision(p.Revision)
	sealed, err := quote.ParseCommercial(rev.Seal.Commercial, rev.Seal.BusinessHash)
	if err != nil {
		return nil, err
	}
	if sealed.Data.ConversionMode != quote.ModeMultiOrder {
		return nil, fail(quote.CodeConversionSlotUnsupported, "/args/quote_id", "The quote converts as one full_v1 order")
	}
	if q.Slot != nil {
		return nil, fail(quote.CodeConversionSlotConflict, "/args/quote_id", "The quote's full_v1 conversion slot is taken")
	}
	if !kernel.Instant(now).Before(rev.Seal.ExpiresAt) {
		return nil, fail(quote.CodeQuoteExpired, "/args/quote_id", "The quote is no longer inside its validity window")
	}
	if err := withinCaps(sealed, p.Lines, p.Consumed); err != nil {
		return nil, err
	}
	src := Source{QuoteID: q.ID, Revision: rev.Number, BusinessHash: rev.Seal.BusinessHash, Number: rev.Formal.Document.Number,
		DocumentSHA256: rev.Formal.Document.ContentSHA256, AcceptanceID: a.ID, AcceptanceVersion: a.Version, ArrivalAt: a.ArrivalAt,
		ArrivalBasis: a.ArrivalBasis, ExpiresAt: a.ExpiresAt}
	sealing, err := sealingOf(p.Preparation, rev)
	if err != nil {
		return nil, err
	}
	snapshot, err := BuildPartialSnapshot(scope, p.OrderID, src, rev.Seal.Commercial, a, sealing, p.Currency, p.Lines, p.Charges, p.Payments)
	if err != nil {
		return nil, err
	}
	q.NoteConsumption()
	at := kernel.Instant(now)
	hash := quote.Hash(snapshot)
	consumed := make([]Consumption, len(p.Lines))
	for i, l := range p.Lines {
		qty, _ := kernel.ParseDecimal(l.Quantity, kernel.QuantityFormat)
		consumed[i] = Consumption{LineID: l.LineID, Quantity: exactDec(qty)}
	}
	return &Order{ID: p.OrderID, Source: src, Slot: quote.SlotMultiOrder, OrderRevision: 1, Snapshot: snapshot, SnapshotHash: hash,
		OrderHash: hash, Status: Draft, RowVersion: 1, Prepared: quote.Act{By: by, Epoch: epoch, At: at}, Fresh: true,
		Consumption: consumed}, nil
}

// withinCaps refuses a line the sealed quote does not have, a quantity that is not positive, a
// repeated line, and a quantity past the line's remaining cap (QUOTE_REMAINING_INSUFFICIENT).
// An uncapped line has no remaining check. The cap is max_order_quantity, or the quoted quantity
// when that is empty.
func withinCaps(doc quote.Commercial, lines []PartialLine, consumed map[string]string) error {
	if len(lines) == 0 {
		return kernel.Reject(kernel.CodeSchemaInvalid, "/args/lines", "")
	}
	byID := map[string]quote.Line{}
	for _, l := range doc.Data.Lines {
		byID[l.LineID] = l
	}
	seen := map[string]bool{}
	for i, line := range lines {
		path := fmt.Sprintf("/args/lines/%d/quantity", i)
		quoted, ok := byID[line.LineID]
		if !ok {
			return kernel.Reject(kernel.CodeReferenceInvalid, fmt.Sprintf("/args/lines/%d/line_id", i), "The line is not on the sealed quote")
		}
		if seen[line.LineID] {
			return kernel.Reject(kernel.CodeDuplicateLineID, fmt.Sprintf("/args/lines/%d/line_id", i), "")
		}
		seen[line.LineID] = true
		qty, err := kernel.ParseDecimal(line.Quantity, kernel.QuantityFormat)
		if err != nil || qty.Sign() <= 0 {
			return kernel.Reject(kernel.CodeSchemaInvalid, path, "")
		}
		if quoted.Uncapped {
			continue
		}
		capQty, err := lineCap(quoted)
		if err != nil {
			return err
		}
		already := kernel.Decimal{}
		if raw := consumed[line.LineID]; raw != "" {
			already, err = kernel.ParseDecimal(raw, kernel.QuantityFormat)
			if err != nil {
				return kernel.Reject(kernel.CodeReferenceInvalid, path, "The consumption ledger is not a quantity")
			}
		}
		if already.Add(qty).Cmp(capQty) > 0 {
			return fail(quote.CodeQuoteRemainingInsufficient, path, fmt.Sprintf("cap %s, consumed %s, requested %s, remaining %s",
				exactDec(capQty), exactDec(already), exactDec(qty), exactDec(capQty.Sub(already))))
		}
	}
	return nil
}

func lineCap(l quote.Line) (kernel.Decimal, error) {
	raw := l.MaxOrderQuantity
	if raw == "" {
		raw = l.Quantity
	}
	q, err := kernel.ParseDecimal(raw, kernel.QuantityFormat)
	if err != nil || q.Sign() <= 0 {
		return kernel.Decimal{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/quote_id", "The line cap is not a quantity")
	}
	return q, nil
}

// BuildPartialSnapshot is the commercial snapshot of a partial order: the sealed quote's terms
// and the chosen lines, repriced, with charges and a payment plan that sums to that payable.
// payment lines empty means the quote's percentages (largest remainder). The snapshot names slot
// multi_order and carries the currency code and minor units so a later read can rebuild it.
func BuildPartialSnapshot(scope kernel.Scope, id string, src Source, commercial []byte, a *acceptance.Acceptance, sealing Sealing,
	currency kernel.Currency, lines []PartialLine, charges []PartialCharge, payments []PartialStage) ([]byte, error) {
	if _, err := quote.ParseCommercial(commercial, src.BusinessHash); err != nil {
		return nil, err
	}
	tree, err := quote.Tree(commercial)
	if err != nil {
		return nil, err
	}
	docMap, _ := tree.(map[string]any)
	data, _ := docMap["data"].(map[string]any)
	if docMap == nil || data == nil || docMap["calculation"] == nil || data["lines"] == nil {
		return nil, kernel.Reject(kernel.CodeDraftNotReady, "/args/business_revision", "The revision has no sealed calculation")
	}
	var doc quote.Commercial
	if err := json.Unmarshal(commercial, &doc); err != nil || doc.Calculation == nil {
		return nil, kernel.Reject(kernel.CodeDraftNotReady, "/args/business_revision", "The revision has no sealed calculation")
	}
	if doc.Data.ConversionMode != quote.ModeMultiOrder {
		return nil, fail(quote.CodeConversionSlotUnsupported, "/args/quote_id", "The quote converts as one full_v1 order")
	}
	if currency.IsZero() {
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/args/quote_id", "The quote currency is not known")
	}
	priced, err := reprice(doc, currency, lines, charges, payments)
	if err != nil {
		return nil, err
	}
	terms := map[string]any{"issued_at": docMap["issued_at"]}
	for _, k := range termKeys {
		v, ok := data[k]
		if !ok {
			return nil, kernel.Reject(kernel.CodeDraftNotReady, "/args/business_revision", "The revision has no "+k)
		}
		terms[k] = v
	}
	if n := sealing.NotifyParty; n != nil {
		if terms["notify_party_ref"], err = quote.TreeOf(n); err != nil {
			return nil, err
		}
	}
	lineIDs := make([]any, len(a.LineIDs))
	for i, l := range a.LineIDs {
		lineIDs[i] = l
	}
	lineTree, err := quote.TreeOf(priced.lines)
	if err != nil {
		return nil, err
	}
	chargeTree, err := quote.TreeOf(priced.charges)
	if err != nil {
		return nil, err
	}
	calcTree, err := quote.TreeOf(priced.calc)
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"snapshot_contract":      SnapshotContract,
		"scope":                  map[string]any{"tenant": string(scope.Tenant), "company": string(scope.Company)},
		"order_id":               id,
		"conversion_slot":        quote.SlotMultiOrder,
		"conversion_mode":        quote.ModeMultiOrder,
		"currency_code":          currency.Code(),
		"currency_minor_units":   currency.MinorUnits(),
		"payment_source":         priced.paymentSource,
		"customer_ref":           data["customer_ref"],
		"contact_ref":            data["contact_ref"],
		"currency_ref":           data["currency_ref"],
		"terms":                  terms,
		"lines":                  lineTree,
		"charges":                chargeTree,
		"calculation":            calcTree,
		"source_quote_id":        src.QuoteID,
		"source_quote_revision":  src.Revision,
		"source_quote_hash":      src.BusinessHash,
		"source_quote_number":    src.Number,
		"source_document_sha256": src.DocumentSHA256,
		"acceptance_id":          src.AcceptanceID,
		"acceptance": map[string]any{
			"acceptance_id": src.AcceptanceID, "effectivity_version": src.AcceptanceVersion,
			"arrival_at": kernel.FormatTimestamp(src.ArrivalAt), "arrival_basis": src.ArrivalBasis,
			"expires_at":   kernel.FormatTimestamp(src.ExpiresAt),
			"customer_ref": map[string]any{"party_id": a.Customer.PartyID, "revision": a.Customer.Revision},
			"contact_ref":  map[string]any{"contact_id": a.Contact.ContactID, "row_version": a.Contact.RowVersion},
			"line_ids":     lineIDs,
		},
		"source_quote_snapshot": tree,
	}
	if sealing.Seal != nil {
		if body["seal"], err = quote.TreeOf(sealing.Seal); err != nil {
			return nil, err
		}
	}
	return quote.Canonical(body)
}

type pricedOrder struct {
	lines         []quote.Line
	charges       []quote.Charge
	calc          quote.Calculation
	paymentSource string
}

// reprice prices the chosen lines and charges in currency and builds the calculation the snapshot
// stores. Comparison and margin figures stay the sealed quote's; net, tax, payable and the payment
// plan are this order's.
func reprice(doc quote.Commercial, currency kernel.Currency, lines []PartialLine, charges []PartialCharge, payments []PartialStage) (pricedOrder, error) {
	byLine := map[string]quote.Line{}
	for _, l := range doc.Data.Lines {
		byLine[l.LineID] = l
	}
	taxes := map[string]quote.TaxBinding{}
	for _, t := range doc.Calculation.SalesTerms.Taxes {
		taxes[t.Role] = t
	}
	bindings := map[string]quote.LineBinding{}
	for _, b := range doc.Calculation.CostScope.Lines {
		bindings[b.LineID] = b
	}
	orderLines := make([]quote.Line, 0, len(lines))
	amounts := make([]calc.Amounts, 0, len(lines))
	amountLines := make([]quote.AmountLine, 0, len(lines))
	scopeLines := make([]quote.LineBinding, 0, len(lines))
	taxLines := make([]quote.TaxBinding, 0, len(lines))
	seen := map[string]bool{}
	for i, want := range lines {
		path := fmt.Sprintf("/args/lines/%d", i)
		src, ok := byLine[want.LineID]
		if !ok {
			return pricedOrder{}, kernel.Reject(kernel.CodeReferenceInvalid, path+"/line_id", "The line is not on the sealed quote")
		}
		if seen[want.LineID] {
			return pricedOrder{}, kernel.Reject(kernel.CodeDuplicateLineID, path+"/line_id", "")
		}
		seen[want.LineID] = true
		qty, err := kernel.ParseDecimal(want.Quantity, kernel.QuantityFormat)
		if err != nil || qty.Sign() <= 0 {
			return pricedOrder{}, kernel.Reject(kernel.CodeSchemaInvalid, path+"/quantity", "")
		}
		binding, taxed := taxes["line:"+src.LineID]
		if !taxed {
			binding = quote.TaxBinding{Role: "line:" + src.LineID, Application: src.TaxApplication, Profile: src.TaxApplication,
				PriceBasis: string(calc.TaxNotApplicable), Rate: "0"}
		}
		profile, err := taxProfile(binding)
		if err != nil {
			return pricedOrder{}, kernel.Reject(kernel.CodeReferenceInvalid, path+"/line_id", "The line tax is not readable")
		}
		price, err := kernel.ParseDecimal(src.UnitPrice, kernel.PriceFormat)
		if err != nil {
			return pricedOrder{}, err
		}
		basis, err := kernel.ParseDecimal(src.PriceBasisQuantity, kernel.QuantityFormat)
		if err != nil {
			return pricedOrder{}, err
		}
		discount := kernel.Decimal{}
		if src.DiscountRatio != "" {
			discount, err = kernel.ParseDecimal(src.DiscountRatio, kernel.PriceFormat)
			if err != nil {
				return pricedOrder{}, err
			}
		}
		got, err := calc.PriceLine(currency, calc.Line{Path: path, LineID: src.LineID, Quantity: qty, UnitPrice: price,
			PriceBasisQuantity: basis, Discount: discount, Tax: &calc.TaxApplication{Profile: profile}})
		if err != nil {
			return pricedOrder{}, err
		}
		line := src
		line.Quantity = exactDec(qty)
		line.MaxOrderQuantity = ""
		line.Uncapped = false
		line.BaseQuantity = ""
		orderLines = append(orderLines, line)
		amounts = append(amounts, got)
		amountLines = append(amountLines, quote.AmountLine{ID: got.ID, NetMinor: got.Net.MinorString(), TaxMinor: got.Tax.MinorString(),
			PayableMinor: got.Payable.MinorString()})
		scopeLines = append(scopeLines, scaledBinding(src, qty, bindings[src.LineID]))
		taxLines = append(taxLines, binding)
	}
	orderCharges := make([]quote.Charge, 0, len(charges))
	chargeAmounts := make([]calc.ChargeAmounts, 0, len(charges))
	chargeLines := make([]quote.AmountLine, 0, len(charges))
	chargeBindings := make([]quote.ChargeBinding, 0, len(charges))
	byCharge := map[string]quote.Charge{}
	for _, c := range doc.Data.Charges {
		byCharge[c.ChargeID] = c
	}
	seenCharge := map[string]bool{}
	for i, want := range charges {
		path := fmt.Sprintf("/args/charges/%d", i)
		src, ok := byCharge[want.ChargeID]
		if !ok {
			return pricedOrder{}, kernel.Reject(kernel.CodeReferenceInvalid, path+"/charge_id", "The charge is not on the sealed quote")
		}
		if src.Inclusion != quote.InclusionSeparate {
			return pricedOrder{}, kernel.Reject(kernel.CodeInvalidKnownValue, path+"/charge_id", "An included charge is part of its line")
		}
		if seenCharge[want.ChargeID] {
			return pricedOrder{}, kernel.Reject(kernel.CodeDuplicateLineID, path+"/charge_id", "")
		}
		seenCharge[want.ChargeID] = true
		money, err := kernel.ParseMinor(currency, want.AmountMinor)
		if err != nil || money.Sign() <= 0 {
			return pricedOrder{}, kernel.Reject(kernel.CodeSchemaInvalid, path+"/amount_minor", "")
		}
		binding, taxed := taxes["charge:"+src.ObligationKey]
		if !taxed {
			return pricedOrder{}, kernel.Reject(kernel.CodeReferenceInvalid, path+"/charge_id", "The charge has no tax binding")
		}
		profile, err := taxProfile(binding)
		if err != nil {
			return pricedOrder{}, err
		}
		got, err := calc.PriceCharge(currency, calc.Charge{Path: path, ChargeID: src.ChargeID, ObligationKey: src.ObligationKey,
			Amount: money.Decimal(), Inclusion: calc.InclusionSeparate, Tax: &calc.TaxApplication{Profile: profile}})
		if err != nil {
			return pricedOrder{}, err
		}
		ch := src
		ch.Amount = exactDec(money.Decimal())
		orderCharges = append(orderCharges, ch)
		chargeAmounts = append(chargeAmounts, got)
		chargeLines = append(chargeLines, quote.AmountLine{ID: got.ID, ObligationKey: got.ObligationKey, NetMinor: got.Net.MinorString(),
			TaxMinor: got.Tax.MinorString(), PayableMinor: got.Payable.MinorString()})
		chargeBindings = append(chargeBindings, quote.ChargeBinding{ChargeID: src.ChargeID, ObligationKey: src.ObligationKey,
			FeeCategory: src.FeeCategory, Amount: ch.Amount, Inclusion: src.Inclusion, IncludedLineID: src.IncludedLineID})
		taxLines = append(taxLines, binding)
	}
	if orderCharges == nil {
		orderCharges = []quote.Charge{}
	}
	totals, err := calc.Total(currency, "/args", amounts, chargeAmounts)
	if err != nil {
		return pricedOrder{}, err
	}
	plan, source, err := paymentPlan(doc, totals.Payable, payments)
	if err != nil {
		return pricedOrder{}, err
	}
	slices.SortFunc(orderLines, func(a, b quote.Line) int { return strings.Compare(a.LineID, b.LineID) })
	slices.SortFunc(amountLines, func(a, b quote.AmountLine) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(scopeLines, func(a, b quote.LineBinding) int { return strings.Compare(a.LineID, b.LineID) })
	slices.SortFunc(taxLines, func(a, b quote.TaxBinding) int { return strings.Compare(a.Role, b.Role) })
	out := *doc.Calculation
	out.Lines = amountLines
	out.Charges = chargeLines
	out.NetMinor = totals.Net.MinorString()
	out.TaxMinor = totals.Tax.MinorString()
	out.PayableMinor = totals.Payable.MinorString()
	out.Payments = plan
	if out.ExcludedCostCategories == nil {
		out.ExcludedCostCategories = []string{}
	}
	out.CostScope.Lines = scopeLines
	out.SalesTerms.Taxes = taxLines
	out.SalesTerms.Charges = chargeBindings
	return pricedOrder{lines: orderLines, charges: orderCharges, calc: out, paymentSource: source}, nil
}

func taxProfile(b quote.TaxBinding) (calc.TaxProfile, error) {
	rate, err := kernel.ParseDecimal(b.Rate, kernel.DecimalFormat{IntDigits: 12, Scale: 12})
	if err != nil {
		return calc.TaxProfile{}, err
	}
	return calc.TaxProfile{Rate: rate, PriceBasis: calc.PriceBasis(b.PriceBasis), Recovery: calc.Recoverable}, nil
}

// scaledBinding is the quote line's cost binding at the ordered quantity. The normalized quantity
// is quoted_normalized × ordered / quoted, HALF_UP to 6 fractional digits. A line the calculation
// did not bind is normalized as its ordered quantity in its own unit.
func scaledBinding(src quote.Line, ordered kernel.Decimal, b quote.LineBinding) quote.LineBinding {
	quoted, err := kernel.ParseDecimal(src.Quantity, kernel.QuantityFormat)
	if b.LineID == "" || err != nil || quoted.Sign() == 0 {
		return quote.LineBinding{LineID: src.LineID, RfqLineID: src.RfqLineID, SkuID: src.Sku.SkuID, SpecRevision: src.Sku.SpecRevision,
			Quantity: exactDec(ordered), Unit: src.Unit.ID, Conversion: src.Conversion, NormalizedQuantity: exactDec(ordered), CostUnit: src.Unit.ID}
	}
	normRaw := b.NormalizedQuantity
	norm, nerr := kernel.ParseDecimal(normRaw, kernel.DecimalFormat{IntDigits: 24, Scale: 12})
	if nerr != nil {
		norm = ordered
	}
	ratio, rerr := ordered.Quo(quoted)
	scaled := ordered
	if rerr == nil {
		scaled = norm.Mul(ratio).RoundHalfUp(6)
	}
	b.Quantity = exactDec(ordered)
	b.NormalizedQuantity = exactDec(scaled)
	return b
}

// paymentPlan returns this order's schedule. An empty stated plan allocates the quote's
// percentages across payable (largest remainder, HALF_UP). A stated plan names each quote stage
// once and its amounts sum to payable.
func paymentPlan(doc quote.Commercial, payable kernel.Money, stated []PartialStage) ([]quote.Payment, string, error) {
	quotePay := doc.Calculation.Payments
	if len(stated) == 0 {
		stages := make([]calc.Stage, len(quotePay))
		for i, p := range quotePay {
			pct, err := kernel.ParseDecimal(p.Percent, kernel.DecimalFormat{IntDigits: 3, Scale: 6})
			if err != nil {
				return nil, "", kernel.Reject(kernel.CodeReferenceInvalid, "/args/payment_plan", "The quote payment percent is not readable")
			}
			stages[i] = calc.Stage{StageID: p.StageID, Percent: pct}
		}
		allocated, err := calc.AllocatePayments("/args", payable, stages)
		if err != nil {
			return nil, "", err
		}
		out := slices.Clone(quotePay)
		for i := range out {
			out[i].AmountMinor = allocated[i].Amount.MinorString()
		}
		return out, "quoted", nil
	}
	if len(stated) != len(quotePay) {
		return nil, "", kernel.Reject(kernel.CodeInvalidKnownValue, "/args/payment_plan", "The payment plan names the quote's stages")
	}
	byID := map[string]kernel.Money{}
	for i, s := range stated {
		money, err := kernel.ParseMinor(payable.Currency(), s.AmountMinor)
		if err != nil || money.Sign() <= 0 {
			return nil, "", kernel.Reject(kernel.CodeSchemaInvalid, fmt.Sprintf("/args/payment_plan/%d/amount_minor", i), "")
		}
		if _, dup := byID[s.StageID]; dup {
			return nil, "", kernel.Reject(kernel.CodeDuplicateLineID, fmt.Sprintf("/args/payment_plan/%d/stage_id", i), "")
		}
		byID[s.StageID] = money
	}
	out := slices.Clone(quotePay)
	parts := make([]kernel.Money, len(out))
	for i, p := range out {
		money, ok := byID[p.StageID]
		if !ok {
			return nil, "", kernel.Reject(kernel.CodeReferenceInvalid, "/args/payment_plan", "The payment plan names the quote's stages")
		}
		out[i].AmountMinor = money.MinorString()
		parts[i] = money
	}
	sum, err := kernel.Sum(payable.Currency(), parts...)
	if err != nil || !sum.Equal(payable) {
		return nil, "", kernel.RejectAs(calc.CodePaymentTotalMismatch, kernel.CategoryInvalid, "/args/payment_plan",
			"The payment plan does not sum to the order's payable")
	}
	return out, "stated", nil
}

func exactDec(d kernel.Decimal) string {
	if s, ok := d.Exact(); ok {
		return s
	}
	return d.String()
}

// rebuildPartial rebuilds a partial snapshot from the choices stored in snapshot, to confirm the
// bytes are what BuildPartialSnapshot writes for those choices.
func rebuildPartial(scope kernel.Scope, id string, src Source, commercial []byte, a *acceptance.Acceptance, sealing Sealing, snapshot []byte) ([]byte, error) {
	tree, err := quote.Tree(snapshot)
	if err != nil {
		return nil, err
	}
	body, _ := tree.(map[string]any)
	if body == nil {
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/snapshot", "The order snapshot is not readable")
	}
	code, _ := body["currency_code"].(string)
	units, err := jsonInt(body["currency_minor_units"])
	if err != nil {
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/snapshot/currency_minor_units", "The order snapshot names no currency")
	}
	currency, err := kernel.NewCurrency(code, units)
	if err != nil {
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/snapshot/currency_code", "The order snapshot names no currency")
	}
	var lines []PartialLine
	for _, item := range asMaps(body["lines"]) {
		id, _ := item["line_id"].(string)
		qty, _ := item["quantity"].(string)
		lines = append(lines, PartialLine{LineID: id, Quantity: qty})
	}
	var charges []PartialCharge
	for _, item := range asMaps(body["charges"]) {
		cid, _ := item["charge_id"].(string)
		amount, _ := item["amount"].(string)
		dec, err := kernel.ParseDecimal(amount, kernel.PriceFormat)
		if err != nil {
			return nil, err
		}
		charges = append(charges, PartialCharge{ChargeID: cid, AmountMinor: dec.ScaledHalfUp(currency.MinorUnits()).String()})
	}
	var payments []PartialStage
	if body["payment_source"] == "stated" {
		calcTree, _ := body["calculation"].(map[string]any)
		for _, item := range asMaps(calcTree["payments"]) {
			sid, _ := item["stage_id"].(string)
			amt, _ := item["amount_minor"].(string)
			payments = append(payments, PartialStage{StageID: sid, AmountMinor: amt})
		}
	}
	return BuildPartialSnapshot(scope, id, src, commercial, a, sealing, currency, lines, charges, payments)
}

func asMaps(v any) []map[string]any {
	items, _ := v.([]any)
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func jsonInt(v any) (int, error) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		return int(i), err
	case int:
		return n, nil
	case int64:
		return int(n), nil
	default:
		return 0, fmt.Errorf("not an integer")
	}
}
