package application

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	"go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipment"
	docs "go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipmentdocumentset"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The insurance policy record (WBS S5-16; api/insurance.go; FUL-INV-005, F-03, D-13). Before FUL's
// transaction — after the ledger authorized insurance.record and found no stored result for its key
// — the command
//
//  1. reads the set and the shipment in a FUL transaction of its own (a set or shipment the actor
//     does not see, or a shipment with another set, is refused in the transaction with its path);
//  2. asks EVD which cited sources the actor reads and for the policy's file (R74), PTY what the
//     insurer's service role is now (ServiceRoleRef v1, R44; PTY publishes no guard for service
//     roles, as for bookings), POL for the currency revision (whatever its state: its code and minor
//     units never change);
//  3. asks TRD for the shipment's order (ActiveOrderRef v1: the cost snapshot the quote adopted,
//     R27) and CST for that snapshot's components by category (R78, read only, no guard): the
//     costing's insurance component the premium is compared with.
//
// In the transaction, in FUL's root order as shipment_document.prepare: the shipment FOR SHARE, the
// set FOR UPDATE (a new set: none yet; two first versions meet on its key and on the
// one-set-per-shipment key), then the authority guard row FOR SHARE (the ledger). No upstream guard
// is called: the record binds what was read, the premium comparison never refuses, and the cover is
// decided on FUL's own commercial invoice version, which the set lock holds.

// Costs is FUL's port to CST (Context Map R78, CostSnapshotRef v1 by cost category): the components
// of the order's cost snapshot with their categories, before FUL's transaction.
type Costs interface {
	ResolveSnapshotComponents(ctx context.Context, refs []cstapi.CostSnapshotRef) (map[cstapi.CostSnapshotRef]cstapi.SnapshotComponents, error)
}

// insuranceRules are the rules of insurance.record (human or AI under its own permission, role
// template documentation; not in humanOnlyCommands) and of its read (ful.read).
func insuranceRules() []authorization.Rule {
	return []authorization.Rule{
		{Command: api.CommandRecordInsurance, Permission: api.CommandRecordInsurance, ActorKinds: humanOrAgent},
		{Command: api.CommandGetInsurance, Permission: api.PermissionRead, ActorKinds: anyActor},
	}
}

// insuranceCommand runs insurance.record.
type insuranceCommand struct {
	s   Service
	cmd api.RecordInsurance
	req docs.InsuranceRequest
	up  docs.InsuranceUpstream
	// in the transaction
	set   *docs.Set
	other string // the set the shipment has, when it is not cmd's
	facts docs.Facts
}

func (h *insuranceCommand) Prepare(ctx context.Context) error {
	h.up = docs.InsuranceUpstream{}
	var ship *shipment.Shipment
	refused := false
	err := h.s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		sc := scope(ctx)
		set, err := postgres.LoadSet(ctx, exec, sc, h.cmd.DocumentSetID, postgres.NoLock)
		if err != nil {
			return err
		}
		if set != nil && (!d.Sees(set.OwnerActor, set.OwnerTeam) || set.ShipmentID != h.cmd.ShipmentID) {
			refused = true // the transaction refuses it with its path
			return nil
		}
		if set == nil {
			other, err := postgres.SetOfShipment(ctx, exec, sc, h.cmd.ShipmentID)
			if err != nil || other != "" {
				refused = other != ""
				return err
			}
		}
		if ship, err = postgres.LoadShipment(ctx, exec, sc, h.cmd.ShipmentID, postgres.NoLock); err != nil || ship == nil {
			return err
		}
		if !d.Sees(ship.OwnerActor, ship.OwnerTeam) || ship.Status == shipment.StatusCancelled {
			ship = nil
		}
		return nil
	})
	if err != nil || refused || ship == nil {
		return err
	}
	if h.up.Found, err = h.s.sources(ctx, h.cmd.EvidenceIDs); err != nil {
		return err
	}
	f, err := h.s.Upstream.Sources.GetFile(ctx, h.cmd.FileID)
	if rej, ok := kernel.AsRejection(err); ok && (rej.Code == kernel.CodeNotFound || rej.Code == kernel.CodePermissionDenied) {
		err = nil // the domain refuses a file the actor does not read
	} else if err == nil {
		h.up.File = &docs.File{ID: f.FileID, SHA256: f.ContentSHA256, MimeType: f.MimeType}
	}
	if err != nil {
		return err
	}
	now := h.s.Clock.Now()
	ref := ptyapi.ServiceRoleRef{PartyID: h.cmd.Insurer.PartyID, ServiceRoleID: h.cmd.Insurer.ServiceRoleID, Revision: h.cmd.Insurer.Revision}
	roles, err := h.s.Upstream.Documents.Parties.ResolveServiceRoles(ctx, []ptyapi.ServiceRoleRef{ref}, now)
	if err != nil {
		return err
	}
	if r, ok := roles[ref]; ok {
		h.up.Insurer = &shipment.ServiceRoleFact{Ref: shipment.ServiceRoleRef(ref), Role: r.Role, Code: r.Code}
	}
	cur := polRef(h.cmd.Currency)
	configs, err := h.s.Upstream.Documents.Configs.ResolveConfigs(ctx, []polapi.ConfigRef{cur}, now)
	if err != nil {
		return err
	}
	if c, ok := configs[cur]; ok && c.Data.Currency != nil {
		h.up.Currency = &docs.Currency{Code: cur.ConfigID, Revision: cur.Revision, MinorUnits: c.Data.Currency.MinorUnits}
	}
	h.up.Costing, err = h.s.costing(ctx, ship.Order.OrderID)
	return err
}

// costing asks TRD for the cost snapshot order's quote adopted and CST for its insurance component
// (R27, R78). What cannot be compared is said, never refused.
func (s Service) costing(ctx context.Context, orderID string) (docs.Costing, error) {
	facts, err := s.Upstream.Orders.ResolveActiveOrders(ctx, []string{orderID})
	if err != nil {
		return docs.Costing{}, err
	}
	f, ok := facts[orderID]
	if !ok {
		return docs.Costing{Reason: "TRD shows no order of the shipment to the actor"}, nil
	}
	if f.CostSnapshotRef.SnapshotID == "" {
		return docs.Costing{Reason: "The order's quote adopted no cost snapshot"}, nil
	}
	ref := cstapi.CostSnapshotRef{SnapshotID: f.CostSnapshotRef.SnapshotID, ResultHash: f.CostSnapshotRef.ResultHash}
	got, err := s.Upstream.Costs.ResolveSnapshotComponents(ctx, []cstapi.CostSnapshotRef{ref})
	if err != nil {
		return docs.Costing{}, err
	}
	c, ok := got[ref]
	if !ok {
		return docs.Costing{SnapshotID: ref.SnapshotID, ResultHash: ref.ResultHash,
			Reason: "CST shows no cost snapshot " + ref.SnapshotID + " to the actor"}, nil
	}
	minor, ids, complete := c.Total(cstapi.CategoryInsurance)
	if !complete {
		return docs.Costing{SnapshotID: ref.SnapshotID, ResultHash: ref.ResultHash, Currency: c.Currency, MinorUnits: c.MinorUnits,
			Reason: "CST cannot tell the cost category of charge component(s) " + strings.Join(c.Unresolved, ", ") + " now"}, nil
	}
	return docs.Costing{Available: true, SnapshotID: ref.SnapshotID, ResultHash: ref.ResultHash, Currency: c.Currency, MinorUnits: c.MinorUnits,
		InsuranceMinor: minor, Components: ids}, nil
}

func (h *insuranceCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.set, h.other, h.facts = nil, "", docs.Facts{}
	sc := scope(ctx)
	shipmentID, err := postgres.ShipmentOfSet(ctx, exec, sc, h.cmd.DocumentSetID)
	if err != nil {
		return err
	}
	if shipmentID == "" {
		shipmentID = h.cmd.ShipmentID
	}
	if err = loadFacts(ctx, exec, sc, shipmentID, &h.facts); err != nil {
		return err
	}
	if h.set, err = postgres.LoadSet(ctx, exec, sc, h.cmd.DocumentSetID, postgres.ForUpdate); err != nil || h.set != nil {
		return err
	}
	h.other, err = postgres.SetOfShipment(ctx, exec, sc, shipmentID)
	return err
}

func (h *insuranceCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	set, sh := h.set, h.facts.Shipment
	switch {
	case set != nil && !d.Sees(set.OwnerActor, set.OwnerTeam):
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/document_set_id", "")
	case set != nil:
		if err := set.Open(h.cmd.ExpectedVersion, h.cmd.ShipmentID); err != nil {
			return action.Outcome{}, err
		}
	case h.cmd.ExpectedVersion != 0:
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/document_set_id", "")
	case h.other != "":
		return action.Outcome{}, docs.Reject(docs.CodeBusinessKeyConflict, "/args/shipment_id", "The shipment's document set is "+h.other)
	}
	if sh == nil || !d.Sees(sh.OwnerActor, sh.OwnerTeam) {
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/shipment_id", "No shipment the actor reads")
	}
	now := kernel.Instant(h.s.Clock.Now())
	act := docs.Act{By: d.Actor, Epoch: d.Epoch, At: now}
	fresh := set == nil
	if fresh {
		set = docs.NewSet(h.cmd.DocumentSetID, sh, d.Team, act)
	}
	doc, err := docs.RecordInsurance(set, h.req, h.facts, h.up, act)
	if err != nil {
		return action.Outcome{}, err
	}
	from := set.RowVersion
	superseded := set.Add(doc)
	doc.Entries = insuranceEntries(set, doc, d, now)
	doc.EntriesJSON, doc.EntriesSHA256 = shipment.Seal(doc.Entries)
	sc := scope(ctx)
	if fresh {
		err = postgres.InsertSet(ctx, exec, sc, set)
	} else if superseded != nil {
		err = postgres.MoveDocument(ctx, exec, sc, set.ID, superseded)
	}
	if err == nil {
		err = postgres.InsertDocument(ctx, exec, sc, set.ID, doc)
	}
	if err == nil && !fresh {
		err = conflict(postgres.MoveSet(ctx, exec, sc, set, from))
	}
	if err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: set.ID, RowVersion: set.RowVersion, Status: docs.Draft}, nil
}

// insuranceEntries are a policy version's commit evidence: what it is, what it was checked on, who
// recorded it under which epoch, when.
func insuranceEntries(set *docs.Set, d *docs.Document, dec authorization.Decision, now time.Time) []docs.Entry {
	p := d.Policy()
	out := []docs.Entry{{Name: "command", Value: api.CommandRecordInsurance}, {Name: "document_set_id", Value: set.ID},
		{Name: "shipment_id", Value: set.ShipmentID}, {Name: "document_kind", Value: d.Kind}, {Name: "version", Value: strconv.FormatInt(d.Version, 10)},
		{Name: "set_version", Value: strconv.FormatInt(d.SetVersion, 10)}, {Name: "policy_number", Value: d.Number},
		{Name: "content_sha256", Value: d.ContentSHA256}, {Name: "insured", Value: p.InsuredMinor + " " + p.Currency.Code},
		{Name: "premium", Value: p.PremiumMinor + " " + p.Currency.Code}, {Name: "coverage_percent", Value: p.CoveragePercent}}
	if f := d.File; f != nil {
		out = append(out, docs.Entry{Name: "file", Value: f.ID + " " + f.SHA256 + " " + f.MimeType})
	}
	out = append(out, d.Sources...)
	return append(out, docs.Entry{Name: "upstream_checked_at", Value: kernel.FormatTimestamp(now)},
		docs.Entry{Name: "recorded_by", Value: dec.Actor.String()}, docs.Entry{Name: "authority_epoch", Value: strconv.FormatInt(dec.Epoch, 10)},
		docs.Entry{Name: "recorded_at", Value: kernel.FormatTimestamp(now)}, docs.Entry{Name: "idempotency_key", Value: d.IdempotencyKey})
}

// RecordInsurance records a new draft version of a set's insurance policy record (insurance.record;
// FUL-INV-005, F-03, D-13, R44, R74, R78).
func (s Service) RecordInsurance(ctx context.Context, cmd api.RecordInsurance) (api.InsuranceReceipt, error) {
	h := &insuranceCommand{s: s, cmd: cmd}
	err := cmd.Validate()
	if err == nil {
		h.req = docs.InsuranceRequest{Insurer: shipment.ServiceRoleRef(cmd.Insurer), PolicyNumber: cmd.PolicyNumber, Clauses: slices.Clone(cmd.Clauses),
			IssuedOn: cmd.IssuedOn, Insured: cmd.InsuredAmount, Premium: cmd.Premium, CoveragePercent: cmd.CoveragePercent, FileID: cmd.FileID,
			Evidence: slices.Clone(cmd.EvidenceIDs), IdempotencyKey: cmd.IdempotencyKey}
	}
	res, err := s.run(ctx, action.Command{Name: api.CommandRecordInsurance, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.DocumentSetID},
		cmd, err, h)
	if err != nil {
		return api.InsuranceReceipt{}, err
	}
	out := api.InsuranceReceipt{Receipt: res}
	err = s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		set, err := postgres.LoadSet(ctx, exec, scope(ctx), res.ObjectID, postgres.NoLock)
		if err != nil || set == nil {
			return err
		}
		for _, d := range set.Versions(docs.KindInsurancePolicy) {
			if p := d.Policy(); p != nil && (d.SetVersion == res.RowVersion || d.IdempotencyKey == cmd.IdempotencyKey) {
				out.Version, out.Coverage, out.Costing = d.Version, policyCoverageView(p.Coverage, p), policyComparisonView(p)
			}
		}
		return nil
	})
	return out, err
}

// amount is minor units of a currency as a decimal of it, such as "8602.00"; minor as it is when it
// is not one.
func amount(code string, minorUnits int, minor string) string {
	c, err := kernel.NewCurrency(code, minorUnits)
	if err != nil {
		return minor
	}
	m, err := kernel.ParseMinor(c, minor)
	if err != nil {
		return minor
	}
	return m.Amount()
}

func coverageView(c *docs.Coverage) *api.InsuranceCoverage {
	if c == nil {
		return nil
	}
	return &api.InsuranceCoverage{Incoterm: c.Incoterm, InvoiceVersion: c.InvoiceVersion, InvoiceNumber: c.InvoiceNumber,
		InvoiceCurrency: c.InvoiceCurrency, InvoiceAmount: amount(c.InvoiceCurrency, c.MinorUnits, c.InvoiceMinor), Percent: c.Percent,
		RequiredAmount: amount(c.InvoiceCurrency, c.MinorUnits, c.RequiredMinor), InsuredCurrency: c.InsuredCurrency,
		InsuredAmount: amount(c.InsuredCurrency, c.MinorUnits, c.InsuredMinor), Sufficient: c.Sufficient}
}

// policyCoverageView is c with the insured amount in the policy's own minor units.
func policyCoverageView(c *docs.Coverage, p *docs.InsurancePolicy) *api.InsuranceCoverage {
	v := coverageView(c)
	if v != nil {
		v.InsuredAmount = amount(p.Currency.Code, p.Currency.MinorUnits, p.InsuredMinor)
	}
	return v
}

func comparisonView(c docs.PremiumComparison) api.PremiumComparison {
	v := api.PremiumComparison{Status: c.Status, SnapshotID: c.SnapshotID, ResultHash: c.ResultHash, Currency: c.Currency,
		Components: slices.Clone(c.Components), PremiumCurrency: c.PremiumCurrency, Reason: c.Reason}
	if v.Components == nil {
		v.Components = []string{}
	}
	if c.ComponentMinor != "" {
		v.ComponentAmount = amount(c.Currency, c.MinorUnits, c.ComponentMinor)
	}
	if c.DifferenceMinor != "" {
		v.Difference = amount(c.Currency, c.MinorUnits, c.DifferenceMinor)
	}
	return v
}

func insurancePolicyView(d *docs.Document) api.InsurancePolicyView {
	p := d.Policy()
	v := api.InsurancePolicyView{Version: d.Version, Status: d.Status, SetVersion: d.SetVersion, Insurer: api.ServiceRoleRef{PartyID: p.Insurer.PartyID,
		ServiceRoleID: p.Insurer.ServiceRoleID, Revision: p.Insurer.Revision}, PolicyNumber: p.PolicyNumber, Clauses: slices.Clone(p.Clauses),
		IssuedOn: p.IssuedOn, Currency: api.ConfigRef{Kind: api.KindCurrency, ID: p.Currency.Code, Revision: p.Currency.Revision},
		InsuredAmount: amount(p.Currency.Code, p.Currency.MinorUnits, p.InsuredMinor), Premium: amount(p.Currency.Code, p.Currency.MinorUnits,
			p.PremiumMinor), CoveragePercent: p.CoveragePercent, Coverage: policyCoverageView(p.Coverage, p),
		Costing: policyComparisonView(p), ContentSHA256: d.ContentSHA256, EvidenceIDs: slices.Clone(d.Evidence), Sources: entries(d.Sources),
		RecordedBy: d.Prepared.By, RecordedAt: d.Prepared.At, AuthorityEpoch: d.Prepared.Epoch, Entries: entries(d.Entries),
		EntriesSHA256: d.EntriesSHA256, IssueSeq: d.IssueSeq, IssuedAt: d.IssuedAt, SupersededAt: d.SupersededAt}
	if f := d.File; f != nil {
		v.FileID, v.FileSHA256, v.FileMimeType = f.ID, f.SHA256, f.MimeType
	}
	if v.Clauses == nil {
		v.Clauses = []string{}
	}
	if v.EvidenceIDs == nil {
		v.EvidenceIDs = []string{}
	}
	return v
}

// policyComparisonView is the premium comparison of p with its amounts as decimals: the premium in
// the policy's currency, the component and the difference in the costing's.
func policyComparisonView(p *docs.InsurancePolicy) api.PremiumComparison {
	v := comparisonView(p.Costing)
	v.Premium = amount(p.Currency.Code, p.Currency.MinorUnits, p.Costing.PremiumMinor)
	return v
}

// insuranceView is the insurance of set now.
func insuranceView(set *docs.Set, now time.Time) api.InsuranceView {
	v := api.InsuranceView{DocumentSetID: set.ID, ShipmentID: set.ShipmentID, RowVersion: set.RowVersion, Versions: []api.InsurancePolicyView{},
		CheckedAt: now}
	if ci := set.Current(docs.KindCommercialInvoice); ci != nil && ci.Content != nil && ci.Content.Terms != nil {
		v.Incoterm = ci.Content.Terms.Incoterm
	}
	v.Required = docs.InsuranceRequired(v.Incoterm)
	for _, d := range set.Versions(docs.KindInsurancePolicy) {
		if d.Policy() != nil {
			v.Versions = append(v.Versions, insurancePolicyView(d))
		}
	}
	if c := set.Current(docs.KindInsurancePolicy); c.Policy() != nil {
		cv := insurancePolicyView(c)
		v.Current = &cv
		v.Coverage = policyCoverageView(set.CoverageNow(), c.Policy())
	}
	return v
}

// GetInsurance returns the insurance of a set the caller sees, audited as insurance.get (ful.read):
// NOT_FOUND otherwise.
func (s Service) GetInsurance(ctx context.Context, id string) (api.InsuranceView, error) {
	if !kernel.ValidIdentifier(id) {
		return api.InsuranceView{}, invalidID(s, ctx, api.CommandGetInsurance)
	}
	var v api.InsuranceView
	err := s.Ledger.Read(ctx, api.CommandGetInsurance, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		set, err := postgres.LoadSet(ctx, exec, scope(ctx), id, postgres.NoLock)
		if err != nil {
			return err
		}
		if set == nil || !d.Sees(set.OwnerActor, set.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v = insuranceView(set, kernel.Instant(s.Clock.Now()))
		return nil
	})
	return v, err
}
