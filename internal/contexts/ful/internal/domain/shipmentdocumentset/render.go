package shipmentdocumentset

import (
	"math/big"
	"slices"
	"strconv"
	"strings"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// # shipment-text/0.1.0
//
// UTF-8 without a byte order mark, "\n" line breaks: the template's title, a blank line, the
// template's sections in its order separated by blank lines, and a final "\n" (at most 1 MiB). Every
// language prints the same Content: numbers, codes, quantities, amounts, the currency, the goods'
// SKUs and the terms' codes are the same characters in en and zh-CN; labels, the trigger and day
// basis of a payment stage and the goods' names are the language's (CN-AT17). The en layout:
//
//	seller      Seller: <legal name>
//	            <address>
//	            Registration: <registration no> | Country: <country>
//	buyer       Buyer: <name>
//	            <address>
//	consignee   Consignee: <name>
//	            <address>
//	notify      Notify: <name>
//	            <address>                      (or "Notify: same as consignee")
//	identity    Number: <number>
//	            Version: <version>
//	            Document set: <set id> | Shipment: <shipment id>
//	            Order: <order id> r<revision>
//	            Order SHA-256: <order hash>
//	            Date: <prepared at, UTC>
//	shipment    Booking: <number> | Vessel: <vessel> | Voyage: <voyage> | ETD: <date>   (or "Booking: none")
//	            Port of loading: <UN/LOCODE> | Port of discharge: <UN/LOCODE>          ("-" for none)
//	lines       Lines (amounts in <currency>):                                          (CI)
//	            <line_id> | <sku_id> r<spec> | <name> | <quantity> <unit> | price <unit price> | net <amount> | tax <amount> | total <amount>
//	packages    Packages (per carton):                                                  (PL)
//	            <from>-<to> | <cartons> ctns | <line_id> <sku_id> <name> <quantity per carton> <unit>[; ...] | N.W. <kg> kg | G.W. <kg> kg | <cbm> cbm
//	totals      Currency: <code>                                                        (CI)
//	            Quantity: <quantity> <unit>[; ...]
//	            Net: <amount> | Tax: <amount> | Total: <amount>
//	            (PL) Cartons: <n>
//	            Quantity: <quantity> <unit>[; ...]
//	            N.W.: <kg> kg | G.W.: <kg> kg | Volume: <cbm> cbm
//	terms       Terms: <incoterm> <edition> | Named place: <place>                     (CI)
//	            Payment: <stage> | <percent>% | <trigger> | <days> <day basis> days (zh-CN: 付款：… | <days> 个日历日)
//	marks       Marks:
//	            <marks, or N/M>
//	clauses     Clauses:
//	            1. <clause>
//	signatures  Signatures (this generated copy is unsigned; signed or stamped copies are recorded as evidence):
//	            Seller: <legal name>
//	notes       <footer note>
//
// Lines in the shipment's line order, packages in the order recorded, payment stages in the plan's
// order. Amounts are minor units printed in major units with the currency's minor digits;
// quantities, prices, weights and volumes their exact shortest decimals; the date in UTC as stored.

type labels struct {
	seller, registration, country, buyer, consignee, notify, sameAsConsignee           string
	number, version, documentSet, shipment, order, orderHash, date                     string
	booking, noBooking, vessel, voyage, etd, loading, discharge                        string
	linesHead, price, net, tax, total, packagesHead, ctns, nw, gw, cbm                 string
	currency, quantity, netTotal, taxTotal, totalTotal, cartons, nwTotal, gwTotal, vol string
	terms, namedPlace, payment, days, marks, clauses, signatures, sellerSign           string
	sep                                                                                string
	triggers, dayBases                                                                 map[string]string
}

var layouts = map[string]labels{
	LanguageEn: {seller: "Seller: ", registration: "Registration: ", country: "Country: ", buyer: "Buyer: ", consignee: "Consignee: ",
		notify: "Notify: ", sameAsConsignee: "Notify: same as consignee", number: "Number: ", version: "Version: ", documentSet: "Document set: ",
		shipment: "Shipment: ", order: "Order: ", orderHash: "Order SHA-256: ", date: "Date: ", booking: "Booking: ", noBooking: "Booking: none",
		vessel: "Vessel: ", voyage: "Voyage: ", etd: "ETD: ", loading: "Port of loading: ", discharge: "Port of discharge: ",
		linesHead: "Lines (amounts in %s):", price: "price ", net: "net ", tax: "tax ", total: "total ", packagesHead: "Packages (per carton):",
		ctns: " ctns", nw: "N.W. ", gw: "G.W. ", cbm: " cbm", currency: "Currency: ", quantity: "Quantity: ", netTotal: "Net: ", taxTotal: "Tax: ",
		totalTotal: "Total: ", cartons: "Cartons: ", nwTotal: "N.W.: ", gwTotal: "G.W.: ", vol: "Volume: ", terms: "Terms: ",
		namedPlace: "Named place: ", payment: "Payment: ", days: " days", marks: "Marks:", clauses: "Clauses:",
		signatures: "Signatures (this generated copy is unsigned; signed or stamped copies are recorded as evidence):", sellerSign: "Seller: ",
		sep: " | ", triggers: map[string]string{}, dayBases: map[string]string{}},
	LanguageZhCN: {seller: "卖方：", registration: "注册号：", country: "国家：", buyer: "买方：", consignee: "收货人：", notify: "通知方：",
		sameAsConsignee: "通知方：同收货人", number: "编号：", version: "版本：", documentSet: "单证套：", shipment: "出运：", order: "订单：",
		orderHash: "订单 SHA-256：", date: "日期：", booking: "订舱号：", noBooking: "订舱号：无", vessel: "船名：", voyage: "航次：",
		etd: "预计开航：", loading: "装运港：", discharge: "目的港：", linesHead: "明细（金额币种 %s）：", price: "单价 ", net: "净额 ",
		tax: "税额 ", total: "合计 ", packagesHead: "包装（每箱）：", ctns: " 箱", nw: "净重 ", gw: "毛重 ", cbm: " 立方米",
		currency: "币种：", quantity: "数量：", netTotal: "净额：", taxTotal: "税额：", totalTotal: "合计：", cartons: "箱数：", nwTotal: "净重：",
		gwTotal: "毛重：", vol: "体积：", terms: "贸易术语：", namedPlace: "指定地点：", payment: "付款：", days: "", marks: "唛头：",
		clauses: "条款：", signatures: "签署（本生成件未经签署；签署或盖章件作为证据另行登记）：", sellerSign: "卖方：", sep: " | ",
		triggers: map[string]string{"order_confirmed": "订单确认", "before_shipment": "出运前", "after_shipment": "出运后",
			"after_invoice": "开票后"},
		dayBases: map[string]string{"calendar": "个日历日", "business": "个工作日"}},
}

var (
	requiredSections = map[string][]string{
		KindCommercialInvoice: {"seller", "buyer", "identity", "shipment", "lines", "totals", "terms", "notes"},
		KindPackingList:       {"seller", "buyer", "identity", "shipment", "packages", "totals", "marks", "notes"},
	}
	optionalSections = map[string][]string{
		KindCommercialInvoice: {"consignee", "notify", "marks", "clauses", "signatures"},
		KindPackingList:       {"consignee", "notify", "clauses", "signatures"},
	}
)

// checkSections reports whether sections are kind's required sections once each and its optional
// ones at most once (POL's rule for shipment_document_template); when not, the first offender.
func checkSections(kind string, sections []string) (string, bool) {
	need, may := requiredSections[kind], optionalSections[kind]
	seen := map[string]bool{}
	for _, s := range sections {
		if seen[s] || (!slices.Contains(need, s) && !slices.Contains(may, s)) {
			return s, false
		}
		seen[s] = true
	}
	for _, s := range need {
		if !seen[s] {
			return s, false
		}
	}
	return "", true
}

// Render prints content c with template t in t's language.
func Render(t Template, c *Content) []byte {
	l := layouts[t.Language]
	var b strings.Builder
	b.WriteString(t.Title)
	b.WriteString("\n\n")
	for i, name := range t.Sections {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(section(l, t, c, name))
	}
	b.WriteString("\n")
	return []byte(b.String())
}

func party(label string, p PartyContent) string { return label + p.Name + "\n" + p.Address }

func section(l labels, t Template, c *Content, name string) string {
	switch name {
	case "seller":
		return l.seller + c.Seller.Name + "\n" + c.Seller.Address + "\n" + l.registration + c.Seller.RegistrationNo + l.sep + l.country + c.Seller.Country
	case "buyer":
		return party(l.buyer, c.Buyer)
	case "consignee":
		return party(l.consignee, c.Consignee)
	case "notify":
		if c.Notify == nil {
			return l.sameAsConsignee
		}
		return party(l.notify, *c.Notify)
	case "identity":
		return strings.Join([]string{l.number + c.Number, l.version + strconv.FormatInt(c.Version, 10),
			l.documentSet + c.DocumentSetID + l.sep + l.shipment + c.ShipmentID, l.order + c.Order.OrderID + " r" + strconv.FormatInt(c.Order.OrderRevision, 10),
			l.orderHash + c.Order.OrderHash, l.date + c.Date}, "\n")
	case "shipment":
		head := l.noBooking
		if b := c.Booking; b != nil {
			head = l.booking + b.Number + l.sep + l.vessel + b.Vessel + l.sep + l.voyage + b.Voyage + l.sep + l.etd + b.ETD
		}
		return head + "\n" + l.loading + dash(c.PortOfLoading) + l.sep + l.discharge + dash(c.PortOfDischarge)
	case "lines":
		out := []string{strings.Replace(l.linesHead, "%s", currencyCode(c), 1)}
		for _, x := range c.Lines {
			out = append(out, strings.Join([]string{x.LineID, x.SkuID + " r" + strconv.FormatInt(x.SpecRevision, 10), x.Names[t.Language],
				x.Quantity + " " + x.UnitID, l.price + x.UnitPrice, l.net + amount(c, x.NetMinor), l.tax + amount(c, x.TaxMinor),
				l.total + amount(c, x.PayableMinor)}, l.sep))
		}
		return strings.Join(out, "\n")
	case "packages":
		out := []string{l.packagesHead}
		for _, p := range c.Packages {
			var contents []string
			for _, x := range p.Contents {
				contents = append(contents, x.LineID+" "+x.SkuID+" "+lineName(c, x.LineID, t.Language)+" "+x.QuantityPerCarton+" "+x.UnitID)
			}
			out = append(out, strings.Join([]string{cartonRange(p), strconv.FormatInt(p.Cartons, 10) + l.ctns, strings.Join(contents, "; "),
				l.nw + p.NetWeightKg + " kg", l.gw + p.GrossWeightKg + " kg", p.VolumeCbm + l.cbm}, l.sep))
		}
		return strings.Join(out, "\n")
	case "totals":
		quantity := l.quantity + quantities(c.Totals.Quantities)
		if c.DocumentKind == KindCommercialInvoice {
			return l.currency + currencyCode(c) + "\n" + quantity + "\n" + l.netTotal + amount(c, c.Totals.NetMinor) + l.sep + l.taxTotal +
				amount(c, c.Totals.TaxMinor) + l.sep + l.totalTotal + amount(c, c.Totals.PayableMinor)
		}
		return l.cartons + strconv.FormatInt(c.Totals.Cartons, 10) + "\n" + quantity + "\n" + l.nwTotal + c.Totals.NetWeightKg + " kg" + l.sep +
			l.gwTotal + c.Totals.GrossWeightKg + " kg" + l.sep + l.vol + c.Totals.VolumeCbm + l.cbm
	case "terms":
		out := []string{l.terms + c.Terms.Incoterm + " " + c.Terms.Edition + l.sep + l.namedPlace + dash(c.Terms.NamedPlace)}
		for _, p := range c.Terms.Payments {
			out = append(out, l.payment+strings.Join([]string{p.StageID, p.Percent + "%", translate(l.triggers, p.Trigger),
				strconv.Itoa(p.Days) + " " + translate(l.dayBases, basis(p.DayBasis)) + l.days}, l.sep))
		}
		return strings.Join(out, "\n")
	case "marks":
		marks := c.Marks
		if strings.TrimSpace(marks) == "" {
			marks = "N/M"
		}
		return l.marks + "\n" + marks
	case "clauses":
		out := []string{l.clauses}
		for i, x := range t.Clauses {
			out = append(out, strconv.Itoa(i+1)+". "+x)
		}
		return strings.Join(out, "\n")
	case "signatures":
		return l.signatures + "\n" + l.sellerSign + c.Seller.Name
	case "notes":
		return t.FooterNote
	}
	panic("shipmentdocumentset: unchecked section " + name) // checkSections admits the known sections only
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func basis(s string) string {
	if s == "" {
		return "calendar" // POL: days without a day basis are calendar days
	}
	return s
}

func translate(m map[string]string, code string) string {
	if v, ok := m[code]; ok {
		return v
	}
	return code
}

func cartonRange(p PackageContent) string {
	if p.CartonFrom < 1 {
		return "-"
	}
	return strconv.FormatInt(p.CartonFrom, 10) + "-" + strconv.FormatInt(p.CartonFrom+p.Cartons-1, 10)
}

func lineName(c *Content, lineID, language string) string {
	for _, x := range c.Lines {
		if x.LineID == lineID {
			return x.Names[language]
		}
	}
	return ""
}

func quantities(q []UnitQuantity) string {
	out := make([]string, 0, len(q))
	for _, x := range q {
		out = append(out, x.Quantity+" "+x.UnitID)
	}
	return strings.Join(out, "; ")
}

func currencyCode(c *Content) string {
	if c.Currency == nil {
		return ""
	}
	return c.Currency.Code
}

// amount prints minor units of c's currency in major units ("400.00").
func amount(c *Content, minor string) string {
	v, ok := new(big.Int).SetString(minor, 10)
	if !ok || c.Currency == nil {
		return minor
	}
	cur, err := kernel.NewCurrency(c.Currency.Code, c.Currency.MinorUnits)
	if err != nil {
		return minor
	}
	m, err := kernel.MoneyFromMinor(cur, v)
	if err != nil {
		return minor
	}
	return m.Amount()
}
