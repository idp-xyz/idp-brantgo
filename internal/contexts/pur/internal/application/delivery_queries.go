package application

import (
	"context"
	"slices"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pur/api"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/commitment"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/draft"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/supplierdelivery"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

func deliveryEvidenceView(es []supplierdelivery.Evidence) []api.Evidence {
	out := make([]api.Evidence, len(es))
	for i, e := range es {
		out[i] = api.Evidence{SourceID: e.SourceID, Locator: e.Locator}
	}
	return out
}

func deliveryFilesView(fs []supplierdelivery.File) []api.DeliveryFile {
	out := make([]api.DeliveryFile, len(fs))
	for i, f := range fs {
		out[i] = api.DeliveryFile{FileID: f.FileID, SHA256: f.SHA256}
	}
	return out
}

// verifiedText is a line's verified quantity as the reads show it: empty until its version was
// verified.
func verifiedText(l supplierdelivery.Line) string {
	if !l.Decided {
		return ""
	}
	return text(l.Verified)
}

// deliveryView is a stored delivery as the reads show it; poNumber is its commitment's purchase
// order number.
func deliveryView(d *supplierdelivery.Delivery, poNumber string) api.DeliveryView {
	v := api.DeliveryView{DeliveryID: d.ID, Status: d.Status(), RowVersion: d.RowVersion, CommitmentID: d.CommitmentID, PONumber: poNumber,
		OrderID: d.OrderID, SupplierID: d.SupplierID, LatestVersion: d.LatestVersion, VerifiedVersion: d.VerifiedVersion,
		Versions: []api.DeliveryVersionView{}}
	if p := d.Pending(); p != nil {
		v.PendingVersion = p.Number
	}
	for _, x := range d.Versions {
		vv := api.DeliveryVersionView{Version: x.Number, Status: x.Status, HandoverPlace: x.HandoverPlace, HandedOverAt: x.HandedOverAt,
			SupplierReference: x.SupplierReference, CorrectionReason: x.CorrectionReason, Evidence: deliveryEvidenceView(x.Evidence),
			Files: deliveryFilesView(x.Files), Lines: []api.DeliveryLineView{}, RecordedBy: x.Recorded.By, RecordedAt: x.Recorded.At,
			RecordEpoch: x.Recorded.Epoch, DecisionEvidence: []api.Evidence{}, SupersededByVersion: x.SupersededBy, SupersededAt: x.SupersededAt}
		if dec := x.Decision; dec != nil {
			vv.Decision, vv.DecidedBy, vv.DecidedAt, vv.DecideEpoch, vv.DecisionReason = api.DecisionReject, dec.By, dec.At, dec.Epoch, dec.Reason
			if dec.Verify {
				vv.Decision = api.DecisionVerify
			}
			vv.DecisionEvidence = deliveryEvidenceView(dec.Evidence)
		}
		for _, l := range x.Lines {
			vv.Lines = append(vv.Lines, api.DeliveryLineView{OrderLineID: l.OrderLineID, SkuID: l.SkuID, SpecRevision: l.SpecRevision,
				UnitID: l.UnitID, UnitRevision: l.UnitRevision, ClaimedQuantity: text(l.Claimed), VerifiedQuantity: verifiedText(l)})
		}
		v.Versions = append(v.Versions, vv)
	}
	return v
}

func poNumber(c *commitment.Commitment) string {
	if c == nil || c.Issue == nil {
		return ""
	}
	return c.Issue.Number
}

// orderReadable asks TRD, before PUR's read transaction, whether the caller reads order id.
func (s Service) orderReadable(ctx context.Context, id string) (bool, error) {
	orders, err := s.Upstream.Orders.ResolveOrders(ctx, []string{id})
	if err != nil {
		return false, err
	}
	_, ok := orders[id]
	return ok, nil
}

// GetDelivery returns a delivery the caller sees, audited as supplier_delivery.get (pur.read):
// NOT_FOUND otherwise; REFERENCE_INVALID when the caller no longer reads its order (TRD's lookup,
// before PUR's read transaction).
func (s Service) GetDelivery(ctx context.Context, id string) (api.DeliveryView, error) {
	if !kernel.ValidIdentifier(id) {
		return api.DeliveryView{}, s.refuseRead(ctx, api.CommandGetDelivery, "/id")
	}
	var stored *supplierdelivery.Delivery
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		var err error
		stored, err = postgres.LoadDelivery(ctx, exec, scope(ctx), id, postgres.NoLock)
		return err
	})
	if err != nil {
		return api.DeliveryView{}, err
	}
	readable := false
	if stored != nil {
		if readable, err = s.orderReadable(ctx, stored.OrderID); err != nil {
			return api.DeliveryView{}, err
		}
	}
	var v api.DeliveryView
	err = s.Ledger.Read(ctx, api.CommandGetDelivery, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		sc := scope(ctx)
		dl, err := postgres.LoadDelivery(ctx, exec, sc, id, postgres.NoLock)
		if err != nil {
			return err
		}
		if dl == nil || !d.Sees(dl.OwnerActor, dl.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		if !readable || stored == nil || stored.OrderID != dl.OrderID {
			return kernel.Reject(kernel.CodeReferenceInvalid, "/id", "The caller no longer reads the delivery's order")
		}
		c, err := postgres.LoadCommitment(ctx, exec, sc, dl.CommitmentID, postgres.NoLock)
		if err != nil {
			return err
		}
		v = deliveryView(dl, poNumber(c))
		return nil
	})
	return v, err
}

// GetCommitmentDeliveries returns the deliveries of a commitment the caller sees, by delivery ID,
// with what they add up to for each commitment line, audited as supplier_delivery.list (pur.read):
// NOT_FOUND otherwise; REFERENCE_INVALID when the caller no longer reads the commitment's order or
// its draft is not intact.
func (s Service) GetCommitmentDeliveries(ctx context.Context, commitmentID string) (api.CommitmentDeliveriesView, error) {
	if !kernel.ValidIdentifier(commitmentID) {
		return api.CommitmentDeliveriesView{}, s.refuseRead(ctx, api.CommandGetCommitmentDeliveries, "/commitment_id")
	}
	before, err := s.loadCommitment(ctx, commitmentID)
	if err != nil {
		return api.CommitmentDeliveriesView{}, err
	}
	readable := false
	if before.c != nil {
		if readable, err = s.orderReadable(ctx, before.c.OrderID); err != nil {
			return api.CommitmentDeliveriesView{}, err
		}
	}
	var v api.CommitmentDeliveriesView
	err = s.Ledger.Read(ctx, api.CommandGetCommitmentDeliveries, commitmentID, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		sc := scope(ctx)
		var l locked
		var err error
		if l.c, err = postgres.LoadCommitment(ctx, exec, sc, commitmentID, postgres.NoLock); err != nil {
			return err
		}
		if l.c == nil || !d.Sees(l.c.OwnerActor, l.c.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/commitment_id", "")
		}
		if !readable || before.c == nil || before.c.OrderID != l.c.OrderID {
			return kernel.Reject(kernel.CodeReferenceInvalid, "/commitment_id", "The caller no longer reads the commitment's order")
		}
		if l.dr, err = postgres.LoadDraft(ctx, exec, sc, l.c.DraftID, postgres.NoLock); err != nil {
			return err
		}
		c, err := deliveryCommitment(ctx, d, l, "/commitment_id")
		if err != nil {
			return err
		}
		ds, err := postgres.DeliveriesOfCommitment(ctx, exec, sc, commitmentID)
		if err != nil {
			return err
		}
		sums := supplierdelivery.Sum(ds, "")
		v = api.CommitmentDeliveriesView{CommitmentID: l.c.ID, Status: l.c.Status, PONumber: poNumber(l.c), OrderID: l.c.OrderID,
			SupplierID: l.c.SupplierID, Lines: []api.CommitmentLineDeliveries{}, Deliveries: []api.DeliveryView{}}
		for _, cl := range c.Lines {
			verified := sums[cl.OrderLineID]
			v.Lines = append(v.Lines, api.CommitmentLineDeliveries{OrderLineID: cl.OrderLineID, SkuID: cl.SkuID, SpecRevision: cl.SpecRevision,
				UnitID: cl.UnitID, UnitRevision: cl.UnitRevision, PurchaseQuantity: text(cl.Purchased), VerifiedQuantity: text(verified),
				RemainingQuantity: text(cl.Purchased.Sub(verified))})
		}
		for _, x := range ds {
			v.Deliveries = append(v.Deliveries, deliveryView(x, poNumber(l.c)))
		}
		return nil
	})
	return v, err
}

// deliveryFacts reads, within one lookup transaction, the commitments and deliveries the caller
// sees, once each.
type deliveryFacts struct {
	exec        bentopg.Executor
	sc          kernel.Scope
	d           authorization.Decision
	commitments map[string]*commitmentFacts
	deliveries  map[string]*supplierdelivery.Delivery
}

// commitmentFacts is a commitment the caller sees with its intact draft's document, what it buys of
// each line at its revision in force with the document of that revision (S5-19: its last change's,
// or the draft's) and, once asked for, its deliveries and what they verified for each line.
type commitmentFacts struct {
	c          *commitment.Commitment
	doc        draft.Document
	current    draft.Document
	lines      []commitment.LineQuantity
	deliveries []*supplierdelivery.Delivery
	sums       map[string]kernel.Decimal
	summed     bool
}

func newDeliveryFacts(ctx context.Context, exec bentopg.Executor, d authorization.Decision) *deliveryFacts {
	return &deliveryFacts{exec: exec, sc: scope(ctx), d: d, commitments: map[string]*commitmentFacts{},
		deliveries: map[string]*supplierdelivery.Delivery{}}
}

// commitment returns the facts of commitment id, or nil when the caller does not see it or its
// draft is not intact.
func (f *deliveryFacts) commitment(ctx context.Context, id string) (*commitmentFacts, error) {
	if c, ok := f.commitments[id]; ok {
		return c, nil
	}
	f.commitments[id] = nil
	c, err := postgres.LoadCommitment(ctx, f.exec, f.sc, id, postgres.NoLock)
	if err != nil || c == nil || !f.d.Sees(c.OwnerActor, c.OwnerTeam) {
		return nil, err
	}
	dr, err := postgres.LoadDraft(ctx, f.exec, f.sc, c.DraftID, postgres.NoLock)
	if err != nil || dr == nil || dr.Verify(f.sc, "/") != nil {
		return nil, err
	}
	doc, err := dr.Decode()
	if err != nil {
		return nil, nil //nolint:nilerr // a document that does not decode is not shown
	}
	current, err := c.Document(dr)
	if err != nil {
		return nil, nil //nolint:nilerr // a revision that does not decode is not shown
	}
	lines, err := c.Lines(dr)
	if err != nil {
		return nil, nil //nolint:nilerr // as above
	}
	f.commitments[id] = &commitmentFacts{c: c, doc: doc, current: current, lines: lines}
	return f.commitments[id], nil
}

// line returns line id of the commitment's draft document and what the commitment buys of it now.
func (c *commitmentFacts) line(id string) (draft.DocLine, commitment.LineQuantity, bool) {
	i := slices.IndexFunc(c.doc.Lines, func(x draft.DocLine) bool { return x.OrderLineID == id })
	q := slices.IndexFunc(c.lines, func(x commitment.LineQuantity) bool { return x.OrderLineID == id })
	if i < 0 || q < 0 {
		return draft.DocLine{}, commitment.LineQuantity{}, false
	}
	return c.doc.Lines[i], c.lines[q], true
}

// amounts returns what line id costs at the revision in force: its amounts in the document of the
// last change, or of the draft ("0" for a line that revision does not buy).
func (c *commitmentFacts) amounts(id string) (net, tax, gross string) {
	i := slices.IndexFunc(c.current.Lines, func(x draft.DocLine) bool { return x.OrderLineID == id })
	if i < 0 {
		return "0", "0", "0"
	}
	l := c.current.Lines[i]
	return l.NetMinor, l.TaxMinor, l.GrossMinor
}

// verified loads the commitment's deliveries and what their verified versions add up to by line.
func (f *deliveryFacts) verified(ctx context.Context, c *commitmentFacts) error {
	if c.summed {
		return nil
	}
	ds, err := postgres.DeliveriesOfCommitment(ctx, f.exec, f.sc, c.c.ID)
	if err != nil {
		return err
	}
	c.deliveries, c.sums, c.summed = ds, supplierdelivery.Sum(ds, ""), true
	for _, d := range ds {
		f.deliveries[d.ID] = d
	}
	return nil
}

// delivery returns delivery id, or nil when it does not exist or the caller does not see it.
func (f *deliveryFacts) delivery(ctx context.Context, id string) (*supplierdelivery.Delivery, error) {
	if d, ok := f.deliveries[id]; ok {
		return d, nil
	}
	f.deliveries[id] = nil
	d, err := postgres.LoadDelivery(ctx, f.exec, f.sc, id, postgres.NoLock)
	if err != nil || d == nil || !f.d.Sees(d.OwnerActor, d.OwnerTeam) {
		return nil, err
	}
	f.deliveries[id] = d
	return d, nil
}

// deliveryLine answers ref: the line of that version of the delivery, with its commitment's facts,
// or false when the caller is shown none.
func (f *deliveryFacts) deliveryLine(ctx context.Context, ref api.DeliveryLineRef) (api.DeliveryLine, *commitmentFacts, *supplierdelivery.Version, bool, error) {
	d, err := f.delivery(ctx, ref.DeliveryID)
	if err != nil || d == nil {
		return api.DeliveryLine{}, nil, nil, false, err
	}
	v := d.Version(ref.Version)
	if v == nil {
		return api.DeliveryLine{}, nil, nil, false, nil
	}
	l, ok := v.Line(ref.LineID)
	if !ok {
		return api.DeliveryLine{}, nil, nil, false, nil
	}
	c, err := f.commitment(ctx, d.CommitmentID)
	if err != nil || c == nil {
		return api.DeliveryLine{}, nil, nil, false, err
	}
	dl, q, ok := c.line(ref.LineID)
	if !ok {
		return api.DeliveryLine{}, nil, nil, false, nil
	}
	out := api.DeliveryLine{Ref: ref, Status: v.Status, VerifiedVersion: d.VerifiedVersion, RowVersion: d.RowVersion, Line: commitmentLine(c.c, dl, q),
		ClaimedQuantity: text(l.Claimed), VerifiedQuantity: verifiedText(l), HandoverPlace: v.HandoverPlace, HandedOverAt: v.HandedOverAt,
		Code: supplierdelivery.Code(v.Status)}
	if dec := v.Decision; dec != nil && dec.Verify {
		out.VerifiedBy, out.VerifiedAt = dec.By, dec.At
	}
	return out, c, v, true, nil
}

// ResolveDeliveryLines answers the lookups of FUL before its transaction (R59) and of QLT for
// delivery line subjects (InspectionSubject v1, R24) in a PUR transaction of its own: the lines of
// the deliveries the caller sees (an active grant, PUR's data scope), each with its commitment line.
// Code is "" for a line of the delivery's verified version, as SupplierDeliveryGuard v1 decides it.
func (s Service) ResolveDeliveryLines(ctx context.Context, refs []api.DeliveryLineRef) (map[api.DeliveryLineRef]api.DeliveryLine, error) {
	out := map[api.DeliveryLineRef]api.DeliveryLine{}
	if err := api.ValidateDeliveryLineRefs(refs); err != nil || len(refs) == 0 {
		return out, err
	}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		f := newDeliveryFacts(ctx, exec, d)
		for _, ref := range refs {
			line, _, _, ok, err := f.deliveryLine(ctx, ref)
			if err != nil {
				return err
			}
			if ok {
				out[ref] = line
			}
		}
		return nil
	})
	return out, err
}

// ResolveSettlementSources answers PurchaseSettlementSource v1 (R31) lookups of STL in a PUR
// transaction of its own: the delivery lines the caller sees, each with what its commitment line
// buys in money (the amounts for the whole line in the document of the commitment's revision in
// force: its last change's, or the sealed draft's), what is verified now for the
// commitment line over all its deliveries, and the delivery's evidence. A line is a settlement
// source when its Code is "".
func (s Service) ResolveSettlementSources(ctx context.Context, refs []api.DeliveryLineRef) (map[api.DeliveryLineRef]api.SettlementSource, error) {
	out := map[api.DeliveryLineRef]api.SettlementSource{}
	if err := api.ValidateDeliveryLineRefs(refs); err != nil || len(refs) == 0 {
		return out, err
	}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		f := newDeliveryFacts(ctx, exec, d)
		for _, ref := range refs {
			line, c, v, ok, err := f.deliveryLine(ctx, ref)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			if err = f.verified(ctx, c); err != nil {
				return err
			}
			net, tax, gross := c.amounts(ref.LineID)
			src := api.SettlementSource{DeliveryLine: line, Currency: c.doc.Currency, MinorUnits: c.doc.MinorUnits, LineNetMinor: net,
				LineTaxMinor: tax, LineGrossMinor: gross, LineVerifiedQuantity: text(c.sums[ref.LineID]),
				Evidence: deliveryEvidenceView(v.Evidence), Files: deliveryFilesView(v.Files), VerificationEvidence: []api.Evidence{}}
			if dec := v.Decision; dec != nil && dec.Verify {
				src.VerificationEvidence = deliveryEvidenceView(dec.Evidence)
			}
			out[ref] = src
		}
		return nil
	})
	return out, err
}

// ResolvePurchaseLines answers PurchaseLineRef v1 (R37) lookups of CNX in a PUR transaction of its
// own: the lines of the commitments the caller sees, each with what it buys in money (the sealed
// draft's amounts) and its verified deliveries, by delivery ID.
func (s Service) ResolvePurchaseLines(ctx context.Context, refs []api.CommitmentLineRef) (map[api.CommitmentLineRef]api.PurchaseLine, error) {
	out := map[api.CommitmentLineRef]api.PurchaseLine{}
	if err := api.ValidateCommitmentLineRefs(refs); err != nil || len(refs) == 0 {
		return out, err
	}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		f := newDeliveryFacts(ctx, exec, d)
		disposed := map[string][]*commitment.Disposition{}
		for _, ref := range refs {
			c, err := f.commitment(ctx, ref.CommitmentID)
			if err != nil {
				return err
			}
			if c == nil {
				continue
			}
			dl, q, ok := c.line(ref.OrderLineID)
			if !ok {
				continue
			}
			if err = f.verified(ctx, c); err != nil {
				return err
			}
			net, tax, gross := c.amounts(ref.OrderLineID)
			line := api.PurchaseLine{Line: commitmentLine(c.c, dl, q), Currency: c.doc.Currency, MinorUnits: c.doc.MinorUnits, NetMinor: net,
				TaxMinor: tax, GrossMinor: gross, VerifiedQuantity: text(c.sums[ref.OrderLineID]),
				Deliveries: []api.PurchaseDelivery{}}
			for _, x := range c.deliveries {
				v := x.Current()
				if v == nil {
					continue
				}
				l, ok := v.Line(ref.OrderLineID)
				if !ok {
					continue
				}
				line.Deliveries = append(line.Deliveries, api.PurchaseDelivery{
					Ref:              api.DeliveryLineRef{DeliveryID: x.ID, LineID: ref.OrderLineID, Version: v.Number},
					VerifiedQuantity: text(l.Verified), HandoverPlace: v.HandoverPlace, HandedOverAt: v.HandedOverAt, VerifiedAt: v.Decision.At})
			}
			if line.DisposedQuantity, line.Dispositions, err = approvedDispositions(ctx, exec, disposed, ref); err != nil {
				return err
			}
			out[ref] = line
		}
		return nil
	})
	return out, err
}

// approvedDispositions returns what the approved dispositions of line ref add up to and those
// dispositions (PurchaseLineRef v1, R31, S5R-D42), reading each commitment's dispositions once into
// loaded.
func approvedDispositions(ctx context.Context, exec bentopg.Executor, loaded map[string][]*commitment.Disposition,
	ref api.CommitmentLineRef) (string, []api.PurchaseDisposition, error) {
	xs, ok := loaded[ref.CommitmentID]
	if !ok {
		var err error
		if xs, err = postgres.DispositionsOfCommitment(ctx, exec, scope(ctx), ref.CommitmentID); err != nil {
			return "", nil, err
		}
		loaded[ref.CommitmentID] = xs
	}
	out := []api.PurchaseDisposition{}
	for _, x := range xs {
		if x.Status == commitment.DispositionApproved && x.OrderLineID == ref.OrderLineID && x.Decision != nil {
			out = append(out, api.PurchaseDisposition{DispositionID: x.ID, Disposition: x.Kind, Quantity: text(x.Quantity), DecidedAt: x.Decision.At})
		}
	}
	return text(commitment.Disposed(xs)[ref.OrderLineID]), out, nil
}
