package application

import (
	"context"
	"slices"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pur/api"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/commitment"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/draft"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

func evidenceView(es []commitment.Evidence) []api.Evidence {
	out := make([]api.Evidence, len(es))
	for i, e := range es {
		out[i] = api.Evidence{SourceID: e.SourceID, Locator: e.Locator}
	}
	return out
}

// commitmentView is a stored commitment as the reads show it at instant now.
func commitmentView(c *commitment.Commitment, now time.Time) api.CommitmentView {
	at := kernel.Instant(now)
	v := api.CommitmentView{CommitmentID: c.ID, Status: c.Status, State: c.State(at), RowVersion: c.RowVersion, DraftID: c.DraftID,
		DraftHash: c.DraftHash, OrderID: c.OrderID, OrderRevision: c.OrderRevision, OrderHash: c.OrderHash, SupplierID: c.SupplierID,
		Currency: c.Currency, SupplyValidUntil: c.SupplyValidUntil, RequestedBy: c.Requested.By, RequestedAt: c.Requested.At,
		RequestEpoch: c.Requested.Epoch, Acknowledgements: []api.AcknowledgementView{}, IssueEvidence: []api.Evidence{},
		Confirmations: []api.ConfirmationView{}, Dispositions: []api.DispositionView{}, CheckedAt: at}
	if d := c.Decision; d != nil {
		v.Decision, v.DecidedBy, v.DecidedAt, v.DecideEpoch, v.DecisionReason = api.DecisionReject, d.By, d.At, d.Epoch, d.Reason
		if d.Approve {
			v.Decision, v.ValidUntil = api.DecisionApprove, d.ValidUntil
		}
		for _, a := range d.Acknowledgements {
			v.Acknowledgements = append(v.Acknowledgements, api.AcknowledgementView{OrderLineID: a.OrderLineID, Kind: a.Kind, Reason: a.Reason})
		}
	}
	if r := c.Revocation; r != nil {
		v.RevokedBy, v.RevokedAt, v.RevokeEpoch, v.RevokeReason = r.By, r.At, r.Epoch, r.Reason
	}
	if is := c.Issue; is != nil {
		v.IssuedBy, v.IssuedAt, v.IssueEpoch, v.PONumber = is.By, is.At, is.Epoch, is.Number
		v.SequenceRef, v.SentAt, v.IssueEvidence = api.SequenceRef{ID: is.Sequence.ID, Revision: is.Sequence.Revision}, is.SentAt, evidenceView(is.Evidence)
		v.IssueFileID, v.IssueFileSHA256 = is.FileID, is.FileSHA256
	}
	for _, f := range c.Confirmations {
		v.Confirmations = append(v.Confirmations, api.ConfirmationView{CommitmentVersion: f.Version, CommitmentRevision: f.Revision,
			Result: f.Result, ReceivedAt: f.ReceivedAt, Deviation: f.Deviation, SupplierReference: f.SupplierReference,
			Evidence: evidenceView(f.Evidence), RecordedBy: f.By, RecordedAt: f.At, AuthorityEpoch: f.Epoch})
	}
	v.Revision, v.Lines, v.Revisions, v.CancelEvidence = c.Revision, []api.CommitmentLineView{}, []api.RevisionView{}, []api.Evidence{}
	for _, r := range c.Revisions {
		v.Revisions = append(v.Revisions, api.RevisionView{Revision: r.Number, Kind: r.Kind, ChangeID: r.ChangeID, OrderRevision: r.OrderRevision,
			OrderHash: r.OrderHash, DocumentHash: r.DocumentHash, Lines: revisionLines(r.Lines), CreatedBy: r.By, CreatedAt: r.At,
			AuthorityEpoch: r.Epoch})
	}
	if x := c.Cancellation; x != nil {
		v.CancelledBy, v.CancelledAt, v.CancelEpoch, v.CancelReason, v.CancelEvidence = x.By, x.At, x.Epoch, x.Reason, evidenceView(x.Evidence)
	}
	return v
}

// revisionLines are lines as the reads show them.
func revisionLines(ls []commitment.LineQuantity) []api.RevisionLineView {
	out := []api.RevisionLineView{}
	for _, l := range ls {
		out = append(out, api.RevisionLineView{OrderLineID: l.OrderLineID, AllocatedQuantity: text(l.Allocated), OverbuyQuantity: text(l.Overbuy),
			PurchaseQuantity: text(l.Purchased()), SupplierDate: l.SupplierDate})
	}
	return out
}

// withLines completes v, the view of commitment c, with its dispositions (S5R-D05), what it buys of each
// line at its revision in force, what the verified deliveries add up to, what the approved dispositions
// dispose of and whether it is fulfilled; a draft that is not intact leaves the lines out.
func withLines(ctx context.Context, exec bentopg.Executor, c *commitment.Commitment, v *api.CommitmentView) error {
	sc := scope(ctx)
	xs, err := postgres.DispositionsOfCommitment(ctx, exec, sc, c.ID)
	if err != nil {
		return err
	}
	v.Dispositions = dispositionViews(xs)
	disposed := commitment.Disposed(xs)
	dr, err := postgres.LoadDraft(ctx, exec, sc, c.DraftID, postgres.NoLock)
	if err != nil || dr == nil || dr.Verify(sc, "/") != nil {
		return err
	}
	lines, err := c.Lines(dr)
	if err != nil {
		return nil //nolint:nilerr // a draft whose document does not decode shows no lines
	}
	verified, err := postgres.VerifiedOfCommitment(ctx, exec, sc, c.ID, "")
	if err != nil {
		return err
	}
	for _, l := range lines {
		v.Lines = append(v.Lines, api.CommitmentLineView{OrderLineID: l.OrderLineID, AllocatedQuantity: text(l.Allocated),
			OverbuyQuantity: text(l.Overbuy), PurchaseQuantity: text(l.Purchased()), VerifiedQuantity: text(verified[l.OrderLineID]),
			SupplierDate: l.SupplierDate, DisposedQuantity: text(disposed[l.OrderLineID])})
	}
	v.Fulfilled = commitment.IsOpen(c.Status) && commitment.Fulfilled(lines, verified)
	return nil
}

func (s Service) refuseRead(ctx context.Context, command, path string) error {
	err := kernel.Reject(kernel.CodeSchemaInvalid, path, "")
	if auditErr := s.Ledger.AuditRejected(ctx, command, "", kernel.CodeSchemaInvalid); auditErr != nil {
		return auditErr
	}
	return err
}

// GetCommitment returns a commitment the caller sees, audited as purchase.commitment.get (pur.read):
// NOT_FOUND otherwise; REFERENCE_INVALID when the caller no longer reads its order (TRD's lookup,
// before PUR's read transaction).
func (s Service) GetCommitment(ctx context.Context, id string) (api.CommitmentView, error) {
	if !kernel.ValidIdentifier(id) {
		return api.CommitmentView{}, s.refuseRead(ctx, api.CommandGetCommitment, "/id")
	}
	l, err := s.loadCommitment(ctx, id)
	if err != nil {
		return api.CommitmentView{}, err
	}
	readable := false
	if l.c != nil {
		orders, err := s.Upstream.Orders.ResolveOrders(ctx, []string{l.c.OrderID})
		if err != nil {
			return api.CommitmentView{}, err
		}
		_, readable = orders[l.c.OrderID]
	}
	var v api.CommitmentView
	err = s.Ledger.Read(ctx, api.CommandGetCommitment, id, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		c, err := postgres.LoadCommitment(ctx, exec, scope(ctx), id, postgres.NoLock)
		if err != nil {
			return err
		}
		if c == nil || !d.Sees(c.OwnerActor, c.OwnerTeam) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		if !readable || l.c == nil || l.c.OrderID != c.OrderID {
			return kernel.Reject(kernel.CodeReferenceInvalid, "/id", "The caller no longer reads the commitment's order")
		}
		v = commitmentView(c, s.Clock.Now())
		return withLines(ctx, exec, c, &v)
	})
	return v, err
}

// GetOrderCommitments returns the commitments the caller sees of an order the caller sees (TRD's
// lookup, before PUR's read transaction: NOT_FOUND otherwise), by commitment ID, audited as
// purchase.commitment.list (pur.read).
func (s Service) GetOrderCommitments(ctx context.Context, orderID string) ([]api.CommitmentView, error) {
	if !kernel.ValidIdentifier(orderID) {
		return nil, s.refuseRead(ctx, api.CommandGetOrderCommitments, "/order_id")
	}
	orders, err := s.Upstream.Orders.ResolveOrders(ctx, []string{orderID})
	if err != nil {
		return nil, err
	}
	_, visible := orders[orderID]
	var out []api.CommitmentView
	err = s.Ledger.Read(ctx, api.CommandGetOrderCommitments, orderID, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		if !visible {
			return kernel.Reject(kernel.CodeNotFound, "/order_id", "")
		}
		owners, err := postgres.CommitmentsOfOrder(ctx, exec, scope(ctx), orderID)
		if err != nil {
			return err
		}
		out = []api.CommitmentView{}
		now := s.Clock.Now()
		for _, o := range owners {
			if !d.Sees(o.Actor, o.Team) {
				continue
			}
			c, err := postgres.LoadCommitment(ctx, exec, scope(ctx), o.ID, postgres.NoLock)
			if err != nil {
				return err
			}
			if c != nil {
				v := commitmentView(c, now)
				if err = withLines(ctx, exec, c, &v); err != nil {
					return err
				}
				out = append(out, v)
			}
		}
		return nil
	})
	return out, err
}

// ResolveCommitmentLines answers InspectionSubject v1 (R24) lookups of QLT in a PUR transaction of
// its own: the lines of the commitments the caller sees (an active grant, PUR's data scope), from
// their drafts' documents; a line of an issued or confirmed commitment is a subject (Code "").
func (s Service) ResolveCommitmentLines(ctx context.Context, refs []api.CommitmentLineRef) (map[api.CommitmentLineRef]api.CommitmentLine, error) {
	out := map[api.CommitmentLineRef]api.CommitmentLine{}
	if err := api.ValidateCommitmentLineRefs(refs); err != nil || len(refs) == 0 {
		return out, err
	}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		sc := scope(ctx)
		loaded := map[string]*locked{}
		for _, ref := range refs {
			l, ok := loaded[ref.CommitmentID]
			if !ok {
				c, err := postgres.LoadCommitment(ctx, exec, sc, ref.CommitmentID, postgres.NoLock)
				if err != nil {
					return err
				}
				l = &locked{c: c}
				if c != nil && d.Sees(c.OwnerActor, c.OwnerTeam) {
					if l.dr, err = postgres.LoadDraft(ctx, exec, sc, c.DraftID, postgres.NoLock); err != nil {
						return err
					}
					if l.dr != nil && l.dr.Verify(sc, "/") != nil {
						l.dr = nil
					}
				}
				loaded[ref.CommitmentID] = l
			}
			if l.c == nil || l.dr == nil {
				continue
			}
			doc, err := l.dr.Decode()
			if err != nil {
				continue
			}
			lines, err := l.c.Lines(l.dr)
			if err != nil {
				continue
			}
			i := slices.IndexFunc(doc.Lines, func(x draft.DocLine) bool { return x.OrderLineID == ref.OrderLineID })
			q := slices.IndexFunc(lines, func(x commitment.LineQuantity) bool { return x.OrderLineID == ref.OrderLineID })
			if i < 0 || q < 0 {
				continue
			}
			out[ref] = commitmentLine(l.c, doc.Lines[i], lines[q])
		}
		return nil
	})
	return out, err
}

// commitmentLine is line dl of the document of commitment c's draft as InspectionSubject v1 shows
// it, with what c buys of it at its revision in force (q) and the order revision and hash of that
// revision (S5-19): a line of an issued or confirmed commitment has Code "", one of a cancelled
// commitment INVALID_STATE.
func commitmentLine(c *commitment.Commitment, dl draft.DocLine, q commitment.LineQuantity) api.CommitmentLine {
	orderRevision, orderHash := c.OrderBinding()
	line := api.CommitmentLine{Ref: api.CommitmentLineRef{CommitmentID: c.ID, OrderLineID: dl.OrderLineID}, Status: c.Status,
		RowVersion: c.RowVersion, DraftID: c.DraftID, DraftHash: c.DraftHash, OrderID: c.OrderID, OrderRevision: orderRevision,
		OrderHash: orderHash, SupplierID: c.SupplierID, SkuID: dl.Sku.SkuID, SpecRevision: dl.Sku.SpecRevision, UnitID: dl.UnitRef.ID,
		UnitRevision: dl.UnitRef.Revision, AllocatedQuantity: text(q.Allocated), PurchaseQuantity: text(q.Purchased()),
		SupplierDate: q.SupplierDate}
	if c.Issue != nil {
		line.PONumber = c.Issue.Number
	}
	if !commitment.IsOpen(c.Status) {
		line.Code = kernel.CodeInvalidState
	}
	return line
}
