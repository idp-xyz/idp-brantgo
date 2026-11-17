package capability

import (
	"context"
	"unicode/utf8"

	catapi "go.idp.xyz/idp-brant/internal/contexts/cat/api"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The reads a Q01 task uses to name the references its drafts and its grant-gated steps bind
// (S4 G1, decision D-7). Each command is the owning context's audited read. The permission is the
// context's read permission, which the role templates already grant. A read changes nothing, so it
// is not an ActionExecution (TSK-INV-003 is intent before a side effect; D-2's executor is the path
// of commands that write a task's contexts).
const (
	ReadRfq       = "rfq.get"
	ReadSnapshots = "cost_snapshot.list"
	ReadContext   = "costing_context.get"
	ReadConfig    = "config.lookup"
	ReadSku       = "sku.get"
	ReadParty     = "party.get"
	ReadQuote     = "quote.get"
	ReadResponse  = "quote.response.get"
	ReadOrder     = "order.get"
	ReadSource    = "source.get"
)

// The api methods a read calls. A context module implements them; a test fake implements only these.
type (
	rfqReads interface {
		GetRfq(ctx context.Context, rfqID string) (inqapi.RfqView, error)
	}
	contextReads interface {
		GetContext(ctx context.Context, contextID string) (cstapi.ContextView, error)
	}
	snapshotReads interface {
		SnapshotsOfRfq(ctx context.Context, rfqID string) (cstapi.RfqSnapshotList, error)
	}
	configReads interface {
		ConfigsInForce(ctx context.Context, kind string) (polapi.ConfigsInForce, error)
	}
	skuReads interface {
		GetSku(ctx context.Context, skuID string) (catapi.SkuView, error)
	}
	partyReads interface {
		GetParty(ctx context.Context, partyID string) (ptyapi.PartyView, error)
	}
	profileReads interface {
		GetTradingProfile(ctx context.Context, partyID string) (ptyapi.TradingProfileView, error)
	}
	tradingReads interface {
		GetQuote(ctx context.Context, quoteID string) (trdapi.QuoteView, error)
		QuoteReady(ctx context.Context, q trdapi.ReadyQuery) (trdapi.QuoteReadiness, error)
		GetCustomerResponse(ctx context.Context, responseID string) (trdapi.CustomerResponseView, error)
		GetOrder(ctx context.Context, orderID string) (trdapi.OrderView, error)
	}
	sourceReads interface {
		GetSource(ctx context.Context, sourceID string) (evdapi.SourceView, error)
	}
	extractReads interface {
		GetExtract(ctx context.Context, sourceID string) (evdapi.ExtractView, error)
	}
)

// Bounds of what a read returns to the model. MaxReadBytes is the JSON of one tool answer. What is
// left out of every read, whether or not the cap is reached: file bytes, commercial and formal
// document bytes, configuration bodies (and bank accounts entirely), cost-snapshot documents and
// their component graphs, internal notes, contributor lists, evidence bodies and audit trails.
// Source text beyond SourceTextRunes and a file extract's locator form beyond LocatorRunes are cut,
// and the content SHA-256 and the full length stay.
const (
	MaxReadBytes    = 16 * 1024
	SourceTextRunes = 3000
	LocatorRunes    = 1500
	maxReadLines    = 40
	maxRevisions    = 6
	maxReasons      = 8
	maxAddresses    = 8
)

// readReceipt is the answer of a read. It carries the receipt fields Define requires; a read has
// no action id and is never a replay. Body is the bounded projection the model sees.
type readReceipt struct {
	ActionID   string `json:"-"`
	ObjectID   string `json:"object_id"`
	RowVersion int64  `json:"row_version,omitempty"`
	Status     string `json:"status,omitempty"`
	Replayed   bool   `json:"-"`
	Truncated  bool   `json:"truncated,omitempty"`
	Omitted    string `json:"omitted,omitempty"`
	Body       any    `json:"result,omitempty"`
}

func readCapabilities(t Q01Targets) ([]Capability, error) {
	var out []Capability
	add := func(c Capability, err error) error {
		if err != nil {
			return err
		}
		out = append(out, c)
		return nil
	}
	steps := []func() (Capability, error){
		func() (Capability, error) { return readRfq(t) },
		func() (Capability, error) { return readSnapshots(t) },
		func() (Capability, error) { return readContext(t) },
		func() (Capability, error) { return readConfig(t) },
		func() (Capability, error) { return readSku(t) },
		func() (Capability, error) { return readParty(t) },
		func() (Capability, error) { return readQuote(t) },
		func() (Capability, error) { return readResponse(t) },
		func() (Capability, error) { return readOrder(t) },
		func() (Capability, error) { return readSource(t) },
	}
	for _, step := range steps {
		c, err := step()
		if err = add(c, err); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func readRfq(t Q01Targets) (Capability, error) {
	type in struct {
		IdempotencyKey string
		RfqID          string
	}
	return defineRead(Spec{Command: ReadRfq, Context: "inq", Contract: inqapi.Contract, Perm: "inq.read",
		Summary: "Read an RFQ and its lines: row version, customer, contact, and each line's sku, quantity and unit. It does not change the RFQ."},
		func(c in) ObjectRef { return ObjectRef{Type: "inq.rfq", ID: c.RfqID} },
		func(ctx context.Context, c in) (readReceipt, error) {
			if t.INQReads == nil {
				return readReceipt{}, errNoTarget
			}
			v, err := t.INQReads.GetRfq(ctx, c.RfqID)
			if err != nil {
				return readReceipt{}, err
			}
			return projectRfq(v), nil
		})
}

func readSnapshots(t Q01Targets) (Capability, error) {
	type in struct {
		IdempotencyKey string
		RfqID          string
	}
	return defineRead(Spec{Command: ReadSnapshots, Context: "cst", Contract: cstapi.Contract, Perm: "cst.read",
		Summary: "Read the cost snapshots of an RFQ: snapshot id, result hash, costing context, currency, total in minor units, and whether the snapshot is adoptable now. The snapshot document and the component graph are not returned."},
		func(c in) ObjectRef { return ObjectRef{Type: "inq.rfq", ID: c.RfqID} },
		func(ctx context.Context, c in) (readReceipt, error) {
			if t.CSTLists == nil {
				return readReceipt{}, errNoTarget
			}
			v, err := t.CSTLists.SnapshotsOfRfq(ctx, c.RfqID)
			if err != nil {
				return readReceipt{}, err
			}
			return readReceipt{ObjectID: c.RfqID, Status: "ok", Truncated: v.Truncated,
				Omitted: "snapshot document, normalized input and component graph",
				Body:    v}, nil
		})
}

func readContext(t Q01Targets) (Capability, error) {
	type in struct {
		IdempotencyKey string
		ContextID      string
	}
	return defineRead(Spec{Command: ReadContext, Context: "cst", Contract: cstapi.Contract, Perm: "cst.read",
		Summary: "Read a costing context: the RFQ it binds and the configuration references its active revision costs under (currency, pricing policy, trade and payment terms, named place, units). Assumption text and evidence bodies are not returned."},
		func(c in) ObjectRef { return ObjectRef{Type: "cst.costing_context", ID: c.ContextID} },
		func(ctx context.Context, c in) (readReceipt, error) {
			if t.CSTReads == nil {
				return readReceipt{}, errNoTarget
			}
			v, err := t.CSTReads.GetContext(ctx, c.ContextID)
			if err != nil {
				return readReceipt{}, err
			}
			return projectContext(v), nil
		})
}

func readConfig(t Q01Targets) (Capability, error) {
	type in struct {
		IdempotencyKey string
		Kind           string
	}
	return defineRead(Spec{Command: ReadConfig, Context: "pol", Contract: polapi.Contract, Perm: "pol.read",
		Summary: "Read the configurations of one kind that are in force now: each id and revision, never the configuration body. Kind bank_account is refused; this read never returns pol.read_sensitive content."},
		func(c in) ObjectRef { return ObjectRef{Type: "pol.config", ID: c.Kind} },
		func(ctx context.Context, c in) (readReceipt, error) {
			if t.POLLists == nil {
				return readReceipt{}, errNoTarget
			}
			v, err := t.POLLists.ConfigsInForce(ctx, c.Kind)
			if err != nil {
				return readReceipt{}, err
			}
			return readReceipt{ObjectID: c.Kind, Status: "ok", Truncated: v.Truncated,
				Omitted: "configuration bodies; bank_account is not this read",
				Body:    v}, nil
		})
}

func readSku(t Q01Targets) (Capability, error) {
	type in struct {
		IdempotencyKey string
		SkuID          string
	}
	return defineRead(Spec{Command: ReadSku, Context: "cat", Contract: catapi.Contract, Perm: "cat.read",
		Summary: "Read a SKU: status, base unit and confirmed specification revisions. The current revision is the highest confirmed one."},
		func(c in) ObjectRef { return ObjectRef{Type: "cat.sku", ID: c.SkuID} },
		func(ctx context.Context, c in) (readReceipt, error) {
			if t.CATReads == nil {
				return readReceipt{}, errNoTarget
			}
			v, err := t.CATReads.GetSku(ctx, c.SkuID)
			if err != nil {
				return readReceipt{}, err
			}
			return projectSku(v), nil
		})
}

func readParty(t Q01Targets) (Capability, error) {
	type in struct {
		IdempotencyKey string
		PartyID        string
	}
	return defineRead(Spec{Command: ReadParty, Context: "pty", Contract: ptyapi.Contract, Perm: "pty.read",
		Summary: "Read a customer and its trading profile: the confirmed party revision, address ids, the profile's active revision, payment and trade terms, and contacts at their row versions. Address text is not returned."},
		func(c in) ObjectRef { return ObjectRef{Type: "pty.party", ID: c.PartyID} },
		func(ctx context.Context, c in) (readReceipt, error) {
			if t.PTYReads == nil || t.PTYProfiles == nil {
				return readReceipt{}, errNoTarget
			}
			party, err := t.PTYReads.GetParty(ctx, c.PartyID)
			if err != nil {
				return readReceipt{}, err
			}
			profile, err := t.PTYProfiles.GetTradingProfile(ctx, c.PartyID)
			if rej, ok := kernel.AsRejection(err); ok && rej.Code == kernel.CodeNotFound {
				profile, err = ptyapi.TradingProfileView{}, nil
			}
			if err != nil {
				return readReceipt{}, err
			}
			return projectParty(party, profile), nil
		})
}

func readQuote(t Q01Targets) (Capability, error) {
	type in struct {
		IdempotencyKey string
		QuoteID        string
	}
	return defineRead(Spec{Command: ReadQuote, Context: "trd", Contract: trdapi.Contract, Perm: "trd.read",
		Summary: "Read a quote: root version, each revision's status and business hash, the references a revision names, its lines, and whether the active revision is ready now. Commercial document bytes and the internal note are not returned."},
		func(c in) ObjectRef { return ObjectRef{Type: trdapi.ObjectQuote, ID: c.QuoteID} },
		func(ctx context.Context, c in) (readReceipt, error) {
			if t.TRDReads == nil {
				return readReceipt{}, errNoTarget
			}
			v, err := t.TRDReads.GetQuote(ctx, c.QuoteID)
			if err != nil {
				return readReceipt{}, err
			}
			ready, err := t.TRDReads.QuoteReady(ctx, trdapi.ReadyQuery{QuoteID: c.QuoteID})
			code := ""
			if rej, ok := kernel.AsRejection(err); ok {
				code = rej.Code
				err = nil
			}
			if err != nil {
				return readReceipt{}, err
			}
			return projectQuote(v, ready, code), nil
		})
}

func readResponse(t Q01Targets) (Capability, error) {
	type in struct {
		IdempotencyKey string
		ResponseID     string
	}
	return defineRead(Spec{Command: ReadResponse, Context: "trd", Contract: trdapi.Contract, Perm: "trd.read",
		Summary: "Read a customer response and the acceptances verified from it: business hash, effectivity and the accepted line ids. Evidence bodies are not returned."},
		func(c in) ObjectRef { return ObjectRef{Type: "trd.response", ID: c.ResponseID} },
		func(ctx context.Context, c in) (readReceipt, error) {
			if t.TRDReads == nil {
				return readReceipt{}, errNoTarget
			}
			v, err := t.TRDReads.GetCustomerResponse(ctx, c.ResponseID)
			if err != nil {
				return readReceipt{}, err
			}
			return projectResponse(v), nil
		})
}

func readOrder(t Q01Targets) (Capability, error) {
	type in struct {
		IdempotencyKey string
		OrderID        string
	}
	return defineRead(Spec{Command: ReadOrder, Context: "trd", Contract: trdapi.Contract, Perm: "trd.read",
		Summary: "Read a sales order: status, row version, order hash, the quote revision and business hash it binds, and the acceptance. Document bytes are not returned."},
		func(c in) ObjectRef { return ObjectRef{Type: trdapi.ObjectOrder, ID: c.OrderID} },
		func(ctx context.Context, c in) (readReceipt, error) {
			if t.TRDReads == nil {
				return readReceipt{}, errNoTarget
			}
			v, err := t.TRDReads.GetOrder(ctx, c.OrderID)
			if err != nil {
				return readReceipt{}, err
			}
			return projectOrder(v), nil
		})
}

func readSource(t Q01Targets) (Capability, error) {
	type in struct {
		IdempotencyKey string
		SourceID       string
	}
	return defineRead(Spec{Command: ReadSource, Context: "evd", Contract: evdapi.Contract, Perm: "evd.read",
		Summary: "Read a source's text (at most 3000 characters) and, for a file extract, its locator form (at most 1500 characters), with the content SHA-256 and the full length. File bytes are not returned."},
		func(c in) ObjectRef { return ObjectRef{Type: "evd.source", ID: c.SourceID} },
		func(ctx context.Context, c in) (readReceipt, error) {
			if t.EVDReads == nil {
				return readReceipt{}, errNoTarget
			}
			v, err := t.EVDReads.GetSource(ctx, c.SourceID)
			if err != nil {
				return readReceipt{}, err
			}
			var form string
			if t.EVDExtracts != nil && v.Extract != nil {
				x, err := t.EVDExtracts.GetExtract(ctx, c.SourceID)
				if rej, ok := kernel.AsRejection(err); ok && rej.Code == kernel.CodeNotFound {
					err = nil
				}
				if err != nil {
					return readReceipt{}, err
				}
				form = x.LocatedForm
			}
			return projectSource(v, form), nil
		})
}

func defineRead[C any](spec Spec, object func(C) ObjectRef, run func(context.Context, C) (readReceipt, error)) (Capability, error) {
	spec.SideEffect = Read
	spec.Risk = RiskLow
	spec.Approval = ApprovalNone
	if spec.Version == 0 {
		spec.Version = 1
	}
	return Define(spec, object, run)
}

type refView struct {
	Kind     string `json:"kind,omitempty"`
	ID       string `json:"id,omitempty"`
	Revision int64  `json:"revision,omitempty"`
}

func clip(s string, n int) (string, bool) {
	if utf8.RuneCountInString(s) <= n {
		return s, false
	}
	return string([]rune(s)[:n]), true
}

func projectRfq(v inqapi.RfqView) readReceipt {
	type line struct {
		LineID        string `json:"line_id"`
		SkuID         string `json:"sku_id,omitempty"`
		Quantity      string `json:"quantity,omitempty"`
		UOM           string `json:"uom,omitempty"`
		RequestedItem string `json:"requested_item,omitempty"`
		SpecRevision  int64  `json:"spec_revision,omitempty"`
	}
	lines := make([]line, 0, len(v.Lines))
	truncated := false
	for _, l := range v.Lines {
		if len(lines) == maxReadLines {
			truncated = true
			break
		}
		lines = append(lines, line{LineID: l.LineID, SkuID: l.SkuID, Quantity: l.Quantity, UOM: l.UOM,
			RequestedItem: l.RequestedItem, SpecRevision: l.SkuSpecRevision})
	}
	body := struct {
		RfqID      string `json:"rfq_id"`
		Status     string `json:"status"`
		RowVersion int64  `json:"row_version"`
		CustomerID string `json:"customer_id,omitempty"`
		ContactID  string `json:"contact_id,omitempty"`
		Lines      []line `json:"lines"`
	}{RfqID: v.RfqID, Status: v.Status, RowVersion: v.RowVersion, CustomerID: v.CustomerID, ContactID: v.ContactID, Lines: lines}
	omitted := "field-locator quotes, corrections and confirmation evidence"
	if truncated {
		omitted = "lines beyond 40; " + omitted
	}
	return readReceipt{ObjectID: v.RfqID, RowVersion: v.RowVersion, Status: v.Status, Truncated: truncated, Omitted: omitted, Body: body}
}

func projectContext(v cstapi.ContextView) readReceipt {
	rev := activeContext(v)
	type line struct {
		LineID       string   `json:"line_id"`
		SkuID        string   `json:"sku_id,omitempty"`
		SpecRevision int64    `json:"spec_revision,omitempty"`
		Quantity     string   `json:"quantity,omitempty"`
		Unit         *refView `json:"unit,omitempty"`
	}
	var lines []line
	var currency, policy, trade, payment, place *refView
	var rfqID string
	var rfqVersion int64
	status := ""
	revision := int64(0)
	truncated := false
	if rev != nil {
		status, revision = rev.Status, rev.Revision
		if rev.Data.RfqRef != nil {
			rfqID, rfqVersion = rev.Data.RfqRef.RfqID, rev.Data.RfqRef.RowVersion
		}
		currency = cstRef(rev.Data.CalculationCurrencyRef)
		policy = cstRef(rev.Data.PricingPolicyRef)
		if rev.Data.ComparisonBasis != nil {
			b := rev.Data.ComparisonBasis
			trade = cstRef(&b.SalesTradeTermsRef)
			payment = cstRef(&b.SalesPaymentTermsRef)
			place = &refView{Kind: b.NamedPlaceRef.Kind, ID: b.NamedPlaceRef.ID, Revision: b.NamedPlaceRef.Revision}
		}
		for _, l := range rev.Data.Lines {
			if len(lines) == maxReadLines {
				truncated = true
				break
			}
			lines = append(lines, line{LineID: l.LineID, SkuID: l.SkuID, SpecRevision: l.SkuSpecRevision, Quantity: l.Quantity,
				Unit: cstRef(&l.UnitRef)})
		}
	}
	body := struct {
		ContextID   string   `json:"context_id"`
		RootVersion int64    `json:"root_version"`
		Revision    int64    `json:"revision,omitempty"`
		Status      string   `json:"status,omitempty"`
		RfqID       string   `json:"rfq_id,omitempty"`
		RfqVersion  int64    `json:"rfq_row_version,omitempty"`
		Currency    *refView `json:"currency,omitempty"`
		Policy      *refView `json:"pricing_policy,omitempty"`
		TradeTerms  *refView `json:"trade_terms,omitempty"`
		Payment     *refView `json:"payment_terms,omitempty"`
		NamedPlace  *refView `json:"named_place,omitempty"`
		Lines       []line   `json:"lines,omitempty"`
	}{ContextID: v.ContextID, RootVersion: v.RootVersion, Revision: revision, Status: status, RfqID: rfqID, RfqVersion: rfqVersion,
		Currency: currency, Policy: policy, TradeTerms: trade, Payment: payment, NamedPlace: place, Lines: lines}
	omitted := "assumptions, evidence and the other revisions' bodies"
	if truncated {
		omitted = "lines beyond 40; " + omitted
	}
	return readReceipt{ObjectID: v.ContextID, RowVersion: v.RootVersion, Status: status, Truncated: truncated, Omitted: omitted, Body: body}
}

func activeContext(v cstapi.ContextView) *cstapi.ContextRevisionView {
	for i := range v.Revisions {
		if v.Revisions[i].Revision == v.ActiveRevision && v.ActiveRevision != 0 {
			return &v.Revisions[i]
		}
	}
	for i := range v.Revisions {
		if v.Revisions[i].Revision == v.WorkingRevision && v.WorkingRevision != 0 {
			return &v.Revisions[i]
		}
	}
	return nil
}

func cstRef(r *cstapi.ConfigRef) *refView {
	if r == nil || r.ID == "" {
		return nil
	}
	return &refView{Kind: r.Kind, ID: r.ID, Revision: r.Revision}
}

func projectSku(v catapi.SkuView) readReceipt {
	type rev struct {
		Revision int64  `json:"revision"`
		Name     string `json:"name,omitempty"`
		BaseUnit string `json:"base_unit,omitempty"`
	}
	var current int64
	revs := make([]rev, 0, len(v.Revisions))
	truncated := false
	for _, r := range v.Revisions {
		if r.Revision > current {
			current = r.Revision
		}
		if len(revs) == maxRevisions {
			truncated = true
			continue
		}
		revs = append(revs, rev{Revision: r.Revision, Name: r.Name, BaseUnit: r.BaseUnit})
	}
	body := struct {
		SkuID     string `json:"sku_id"`
		Status    string `json:"status"`
		Name      string `json:"name,omitempty"`
		BaseUnit  string `json:"base_unit,omitempty"`
		Current   int64  `json:"current_spec_revision,omitempty"`
		Revisions []rev  `json:"revisions,omitempty"`
	}{SkuID: v.SkuID, Status: v.Status, Name: v.Name, BaseUnit: v.BaseUnit, Current: current, Revisions: revs}
	return readReceipt{ObjectID: v.SkuID, RowVersion: v.RowVersion, Status: v.Status, Truncated: truncated,
		Omitted: "evidence of the specification revisions", Body: body}
}

func projectParty(party ptyapi.PartyView, profile ptyapi.TradingProfileView) readReceipt {
	type addr struct {
		AddressID string `json:"address_id"`
		Role      string `json:"role,omitempty"`
		Country   string `json:"country,omitempty"`
	}
	type contact struct {
		ContactID  string `json:"contact_id"`
		RowVersion int64  `json:"row_version"`
	}
	type terms struct {
		Kind     string `json:"kind,omitempty"`
		ID       string `json:"id,omitempty"`
		Revision int64  `json:"revision,omitempty"`
	}
	addresses := make([]addr, 0, len(party.Addresses))
	truncated := false
	for _, a := range party.Addresses {
		if len(addresses) == maxAddresses {
			truncated = true
			break
		}
		addresses = append(addresses, addr{AddressID: a.AddressID, Role: a.Role, Country: a.Country})
	}
	var active int64
	var payment, trade *terms
	var contacts []contact
	if profile.PartyID != "" {
		active = profile.ActiveRevision
		for _, r := range profile.Revisions {
			if r.Revision != profile.ActiveRevision || profile.ActiveRevision == 0 {
				continue
			}
			payment = &terms{Kind: r.PaymentTerms.Kind, ID: r.PaymentTerms.ConfigID, Revision: r.PaymentTerms.Revision}
			trade = &terms{Kind: r.TradeTerms.Kind, ID: r.TradeTerms.ConfigID, Revision: r.TradeTerms.Revision}
			for _, c := range r.ContactRefs {
				if len(contacts) == maxAddresses {
					truncated = true
					break
				}
				contacts = append(contacts, contact{ContactID: c.ContactID, RowVersion: c.RowVersion})
			}
		}
	}
	body := struct {
		PartyID       string    `json:"party_id"`
		Status        string    `json:"status"`
		RowVersion    int64     `json:"row_version"`
		Revision      int64     `json:"revision,omitempty"`
		Name          string    `json:"name,omitempty"`
		Addresses     []addr    `json:"addresses,omitempty"`
		ProfileActive int64     `json:"trading_profile_revision,omitempty"`
		PaymentTerms  *terms    `json:"payment_terms,omitempty"`
		TradeTerms    *terms    `json:"trade_terms,omitempty"`
		Contacts      []contact `json:"contacts,omitempty"`
	}{PartyID: party.PartyID, Status: party.Status, RowVersion: party.RowVersion, Revision: party.Revision, Name: party.Name,
		Addresses: addresses, ProfileActive: active, PaymentTerms: payment, TradeTerms: trade, Contacts: contacts}
	return readReceipt{ObjectID: party.PartyID, RowVersion: party.RowVersion, Status: party.Status, Truncated: truncated,
		Omitted: "address text, screening and the profile's other revisions", Body: body}
}

func projectQuote(v trdapi.QuoteView, ready trdapi.QuoteReadiness, readyCode string) readReceipt {
	type line struct {
		LineID       string   `json:"line_id"`
		RfqLineID    string   `json:"rfq_line_id,omitempty"`
		SkuID        string   `json:"sku_id,omitempty"`
		SpecRevision int64    `json:"spec_revision,omitempty"`
		Quantity     string   `json:"quantity,omitempty"`
		Unit         *refView `json:"unit,omitempty"`
		UnitPrice    string   `json:"unit_price,omitempty"`
		Tax          *refView `json:"tax_application,omitempty"`
	}
	type rev struct {
		Revision     int64    `json:"revision"`
		Status       string   `json:"status"`
		BusinessHash string   `json:"business_hash,omitempty"`
		Customer     string   `json:"customer_id,omitempty"`
		CustomerRev  int64    `json:"customer_revision,omitempty"`
		ContactID    string   `json:"contact_id,omitempty"`
		ContactRow   int64    `json:"contact_row_version,omitempty"`
		ProfileRev   int64    `json:"trading_profile_revision,omitempty"`
		RfqID        string   `json:"rfq_id,omitempty"`
		RfqVersion   int64    `json:"rfq_row_version,omitempty"`
		SnapshotID   string   `json:"snapshot_id,omitempty"`
		ResultHash   string   `json:"result_hash,omitempty"`
		Currency     *refView `json:"currency,omitempty"`
		Policy       *refView `json:"pricing_policy,omitempty"`
		TradeTerms   *refView `json:"trade_terms,omitempty"`
		Payment      *refView `json:"payment_terms,omitempty"`
		NamedPlace   *refView `json:"named_place,omitempty"`
		Template     *refView `json:"document_template,omitempty"`
		Sequence     *refView `json:"number_sequence,omitempty"`
		Issuer       *refView `json:"issuer,omitempty"`
		PayableMinor string   `json:"payable_minor,omitempty"`
		Lines        []line   `json:"lines,omitempty"`
	}
	start := 0
	truncated := len(v.Revisions) > maxRevisions
	if truncated {
		start = len(v.Revisions) - maxRevisions
	}
	revs := make([]rev, 0, len(v.Revisions)-start)
	for _, r := range v.Revisions[start:] {
		one := rev{Revision: r.Revision, Status: r.Status, BusinessHash: r.BusinessHash}
		d := r.Data
		if d.CustomerRef != nil {
			one.Customer, one.CustomerRev = d.CustomerRef.PartyID, d.CustomerRef.Revision
		}
		if d.ContactRef != nil {
			one.ContactID, one.ContactRow = d.ContactRef.ContactID, d.ContactRef.RowVersion
		}
		if d.TradingProfileRef != nil {
			one.ProfileRev = d.TradingProfileRef.Revision
		}
		if d.RfqRef != nil {
			one.RfqID, one.RfqVersion = d.RfqRef.RfqID, d.RfqRef.RowVersion
		}
		if d.CostSnapshotRef != nil {
			one.SnapshotID, one.ResultHash = d.CostSnapshotRef.SnapshotID, d.CostSnapshotRef.ResultHash
		}
		one.Currency = trdRef(d.CurrencyRef)
		one.Policy = trdRef(d.PricingPolicyRef)
		one.TradeTerms = trdRef(d.TradeTermsRef)
		one.Payment = trdRef(d.PaymentTermsRef)
		one.Template = trdRef(d.DocumentTemplateRef)
		one.Sequence = trdRef(d.SequenceRef)
		one.Issuer = trdRef(d.IssuerRef)
		if d.NamedPlaceRef != nil {
			one.NamedPlace = &refView{Kind: d.NamedPlaceRef.Kind, ID: d.NamedPlaceRef.ID, Revision: d.NamedPlaceRef.Revision}
		}
		if r.Calculation != nil {
			one.PayableMinor = r.Calculation.PayableMinor
		}
		for _, l := range d.Lines {
			if len(one.Lines) == maxReadLines {
				truncated = true
				break
			}
			one.Lines = append(one.Lines, line{LineID: l.LineID, RfqLineID: l.RfqLineID, SkuID: l.Sku.SkuID, SpecRevision: l.Sku.SpecRevision,
				Quantity: l.Quantity, Unit: trdRef(&l.UnitRef), UnitPrice: l.UnitPrice, Tax: trdRef(&l.TaxApplicationRef)})
		}
		revs = append(revs, one)
	}
	reasons := make([]string, 0, len(ready.Reasons))
	for _, r := range ready.Reasons {
		if len(reasons) == maxReasons {
			truncated = true
			break
		}
		if r.Code != "" {
			reasons = append(reasons, r.Check+":"+r.Code)
		}
	}
	body := struct {
		QuoteID         string   `json:"quote_id"`
		RootVersion     int64    `json:"root_version"`
		WorkingRevision int64    `json:"working_revision,omitempty"`
		ActiveRevision  int64    `json:"active_revision,omitempty"`
		Revisions       []rev    `json:"revisions"`
		Ready           bool     `json:"ready"`
		ReadyRevision   int64    `json:"ready_revision,omitempty"`
		ReadyHash       string   `json:"ready_business_hash,omitempty"`
		ReadyReasons    []string `json:"ready_reasons,omitempty"`
		ReadinessCode   string   `json:"readiness_code,omitempty"`
	}{QuoteID: v.QuoteID, RootVersion: v.RootVersion, WorkingRevision: v.WorkingRevision, ActiveRevision: v.ActiveRevision,
		Revisions: revs, Ready: ready.Ready, ReadyRevision: ready.BusinessRevision, ReadyHash: ready.BusinessHash,
		ReadyReasons: reasons, ReadinessCode: readyCode}
	return readReceipt{ObjectID: v.QuoteID, RowVersion: v.RootVersion, Status: statusOf(v.ActiveRevision, v.WorkingRevision),
		Truncated: truncated, Omitted: "commercial document bytes, internal note, review reasons and evidence", Body: body}
}

func trdRef(r *trdapi.ConfigRef) *refView {
	if r == nil || r.ID == "" {
		return nil
	}
	return &refView{Kind: r.Kind, ID: r.ID, Revision: r.Revision}
}

func statusOf(active, working int64) string {
	switch {
	case active > 0:
		return "active"
	case working > 0:
		return "working"
	default:
		return "ok"
	}
}

func projectResponse(v trdapi.CustomerResponseView) readReceipt {
	type acc struct {
		AcceptanceID string   `json:"acceptance_id"`
		BusinessHash string   `json:"business_hash,omitempty"`
		Effectivity  string   `json:"effectivity,omitempty"`
		Version      int64    `json:"effectivity_version,omitempty"`
		Formed       bool     `json:"formed"`
		LineIDs      []string `json:"line_ids,omitempty"`
	}
	all := make([]acc, 0, len(v.Acceptances))
	truncated := false
	for _, a := range v.Acceptances {
		if len(all) == maxRevisions {
			truncated = true
			break
		}
		ids := a.LineIDs
		if len(ids) > maxReadLines {
			ids = ids[:maxReadLines]
			truncated = true
		}
		all = append(all, acc{AcceptanceID: a.AcceptanceID, BusinessHash: a.BusinessHash, Effectivity: a.Effectivity,
			Version: a.EffectivityVersion, Formed: a.Formed, LineIDs: ids})
	}
	body := struct {
		ResponseID   string `json:"response_id"`
		QuoteID      string `json:"quote_id"`
		Revision     int64  `json:"business_revision"`
		BusinessHash string `json:"business_hash,omitempty"`
		Late         bool   `json:"late,omitempty"`
		Acceptances  []acc  `json:"acceptances"`
	}{ResponseID: v.ResponseID, QuoteID: v.QuoteID, Revision: v.BusinessRevision, BusinessHash: v.BusinessHash, Late: v.Late, Acceptances: all}
	return readReceipt{ObjectID: v.ResponseID, Status: "recorded", Truncated: truncated,
		Omitted: "the response summary's extra text and evidence bodies", Body: body}
}

func projectOrder(v trdapi.OrderView) readReceipt {
	body := struct {
		OrderID      string `json:"order_id"`
		Status       string `json:"status"`
		RowVersion   int64  `json:"row_version"`
		OrderHash    string `json:"order_hash,omitempty"`
		QuoteID      string `json:"quote_id,omitempty"`
		Revision     int64  `json:"business_revision,omitempty"`
		BusinessHash string `json:"business_hash,omitempty"`
		AcceptanceID string `json:"acceptance_id,omitempty"`
		Number       string `json:"number,omitempty"`
	}{OrderID: v.OrderID, Status: v.Status, RowVersion: v.RowVersion, OrderHash: v.OrderHash, QuoteID: v.QuoteID,
		Revision: v.BusinessRevision, BusinessHash: v.BusinessHash, AcceptanceID: v.AcceptanceID, Number: v.Number}
	return readReceipt{ObjectID: v.OrderID, RowVersion: v.RowVersion, Status: v.Status,
		Omitted: "document bytes, the commercial snapshot, reviews and amendments", Body: body}
}

func projectSource(v evdapi.SourceView, located string) readReceipt {
	text, cutText := clip(v.ContentText, SourceTextRunes)
	form, cutForm := clip(located, LocatorRunes)
	body := struct {
		SourceID      string `json:"source_id"`
		Kind          string `json:"source_kind"`
		ContentSHA256 string `json:"content_sha256,omitempty"`
		Characters    int    `json:"characters"`
		ContentText   string `json:"content_text,omitempty"`
		FileID        string `json:"file_id,omitempty"`
		Extractor     string `json:"extractor,omitempty"`
		LocatedForm   string `json:"located_form,omitempty"`
	}{SourceID: v.SourceID, Kind: v.SourceKind, ContentSHA256: v.ContentSHA256, Characters: utf8.RuneCountInString(v.ContentText),
		ContentText: text, LocatedForm: form}
	if v.Extract != nil {
		body.FileID, body.Extractor = v.Extract.FileID, v.Extract.Extractor
	}
	omitted := "file bytes"
	if cutText {
		omitted = "source text beyond 3000 characters; " + omitted
	}
	if cutForm {
		omitted = "locator form beyond 1500 characters; " + omitted
	}
	return readReceipt{ObjectID: v.SourceID, Status: v.SourceKind, Truncated: cutText || cutForm, Omitted: omitted, Body: body}
}
