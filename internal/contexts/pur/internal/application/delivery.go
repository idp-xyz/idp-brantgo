package application

import (
	"context"
	"maps"
	"slices"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pur/api"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/commitment"
	"go.idp.xyz/idp-brant/internal/contexts/pur/internal/domain/supplierdelivery"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Supplier deliveries (WBS S5-15; domain/supplierdelivery; api/delivery.go).
//
// Lock order (ADR-BR-011, tactical lockOrder): the delivery root FOR UPDATE when it exists (Lock);
// then the root of the commitment it is recorded against FOR UPDATE — the root the verified
// quantities of a commitment line are measured under (PUR-INV-008), locked and not written (S5
// decision D-3); a new delivery's ID is then read again, so a record of the same delivery that
// committed meanwhile is seen; then the company's authority guard FOR SHARE (the ledger's
// Authorize); then the delivery's rows, the ledger's result, audit and Outbox. The delivery root
// comes first so that a command waiting behind FUL's SupplierDeliveryGuard v1, which holds that root
// FOR SHARE, holds no other lock of PUR's. Commitment commands lock the commitment root and never a
// delivery, so the two orders cannot cross.
//
// A delivery command calls no upstream guard: what a supplier handed over is a fact of the
// supplier's whatever became of the order since, and it adds nothing to what DownstreamCommitments
// v1 (R71) shows TRD. EVD's sources and files are read before the transaction (R76, no guard lock).

var deliveryCmds = []string{api.CommandRecordSupplierDelivery, api.CommandVerifySupplierDelivery}

// deliveryCommitment refuses a commitment d's actor does not see (NOT_FOUND at path) and a draft
// that is not intact (REFERENCE_INVALID), and returns what the commitment says to a delivery.
func deliveryCommitment(ctx context.Context, d authorization.Decision, l locked, path string) (supplierdelivery.Commitment, error) {
	if l.c == nil || !d.Sees(l.c.OwnerActor, l.c.OwnerTeam) {
		return supplierdelivery.Commitment{}, kernel.Reject(kernel.CodeNotFound, path, "")
	}
	if l.dr == nil {
		return supplierdelivery.Commitment{}, kernel.Reject(kernel.CodeReferenceInvalid, path, "The commitment's draft is missing")
	}
	if err := l.dr.Verify(scope(ctx), path); err != nil {
		return supplierdelivery.Commitment{}, err
	}
	return supplierdelivery.From(l.c, l.dr, path)
}

func deliveryEvidence(es []api.Evidence) []supplierdelivery.Evidence {
	out := make([]supplierdelivery.Evidence, len(es))
	for i, e := range es {
		out[i] = supplierdelivery.Evidence{SourceID: e.SourceID, Locator: e.Locator}
	}
	return out
}

func deliveryOutcome(d *supplierdelivery.Delivery, status string, events ...action.Event) action.Outcome {
	return action.Outcome{ObjectID: d.ID, RowVersion: d.RowVersion, Status: status, Events: events}
}

// held is what a delivery command holds: the delivery, its root locked FOR UPDATE when it existed as
// the command began (locked), and the commitment root it then locked FOR UPDATE, with its draft.
type held struct {
	d      *supplierdelivery.Delivery
	locked bool
	l      locked
}

// lockDelivery locks delivery id FOR UPDATE when it exists and then the root of its commitment FOR
// UPDATE, reading the commitment's draft. For a delivery that does not exist it locks the root of
// commitmentID, the commitment a new delivery names, and then reads the delivery ID again without a
// lock: a record of the same new delivery that committed while this one waited for the commitment
// is seen and answered, never changed — locking it there would take the two roots in the other
// order.
func lockDelivery(ctx context.Context, exec bentopg.Executor, id, commitmentID string) (held, error) {
	sc := scope(ctx)
	d, err := postgres.LoadDelivery(ctx, exec, sc, id, postgres.ForUpdate)
	if err != nil {
		return held{}, err
	}
	if d != nil {
		l, err := lockCommitment(ctx, exec, d.CommitmentID, false)
		return held{d: d, locked: true, l: l}, err
	}
	if commitmentID == "" {
		return held{}, nil
	}
	l, err := lockCommitment(ctx, exec, commitmentID, false)
	if err != nil {
		return held{}, err
	}
	d, err = postgres.LoadDelivery(ctx, exec, sc, id, postgres.NoLock)
	return held{d: d, l: l}, err
}

// recordDelivery runs supplier_delivery.record.
type recordDelivery struct {
	s        Service
	cmd      api.RecordSupplierDelivery
	hash     string
	readable map[string]bool
	files    []supplierdelivery.File
	fileErr  error
	h        held
	replay   bool
}

// Prepare reads, before the transaction, which of the claim's EVD sources the recorder reads and
// the files it cites, with their hashes (R76). A file the recorder does not read is refused by
// Apply, after the delivery's own checks.
func (h *recordDelivery) Prepare(ctx context.Context) error {
	h.readable, h.files, h.fileErr = nil, nil, nil
	var err error
	if h.readable, err = h.s.readable(ctx, h.cmd.Evidence); err != nil {
		return err
	}
	for i, id := range h.cmd.FileIDs {
		f, err := h.s.Upstream.Sources.GetFile(ctx, id)
		if err != nil {
			if _, ok := kernel.AsRejection(err); !ok {
				return err
			}
			if h.fileErr == nil {
				h.fileErr = factErr(err, "/args/file_ids/"+itoa(int64(i)))
			}
			continue
		}
		h.files = append(h.files, supplierdelivery.File{FileID: f.FileID, SHA256: f.ContentSHA256})
	}
	return nil
}

func (h *recordDelivery) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.replay = false
	h.h, err = lockDelivery(ctx, exec, h.cmd.DeliveryID, h.cmd.CommitmentID)
	return err
}

func (h *recordDelivery) request() supplierdelivery.Request {
	c := h.cmd
	at, _ := kernel.ParseTimestamp(c.HandedOverAt)
	r := supplierdelivery.Request{DeliveryID: c.DeliveryID, CommitmentID: c.CommitmentID, HandoverPlace: c.HandoverPlace, HandedOverAt: at,
		Evidence: deliveryEvidence(c.Evidence), Files: h.files, SupplierReference: c.SupplierReference, Reason: c.Reason, RequestHash: h.hash}
	for _, l := range c.Lines {
		q, _ := kernel.ParseDecimal(l.Quantity, kernel.QuantityFormat)
		r.Lines = append(r.Lines, supplierdelivery.RequestLine{OrderLineID: l.OrderLineID, Quantity: q})
	}
	return r
}

// Apply refuses, in this order. A new delivery (expected version 0): a delivery ID that is taken —
// by a delivery the recorder does not see (NOT_FOUND), or by another request or recorder
// (VERSION_CONFLICT; the same request by the same recorder is answered from the stored delivery) —,
// a commitment the recorder does not see (NOT_FOUND at /args/commitment_id), a draft not intact
// (REFERENCE_INVALID), Record's refusals. A correction: a delivery the recorder does not see
// (NOT_FOUND), one that was only recorded while this command waited (VERSION_CONFLICT), a draft
// not intact, Correct's refusals. Then a file the recorder does not read (REFERENCE_INVALID at it).
func (h *recordDelivery) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc := scope(ctx)
	dl := h.h.d
	if (dl != nil && !d.Sees(dl.OwnerActor, dl.OwnerTeam)) || (dl == nil && h.cmd.ExpectedVersion > 0) {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/delivery_id", "")
	}
	if dl != nil && h.cmd.ExpectedVersion == 0 {
		if err := dl.Answer(h.hash, d.Actor); err != nil {
			return action.Outcome{}, err
		}
		h.replay = true
		return deliveryOutcome(dl, dl.Versions[0].Status), nil
	}
	if dl != nil && !h.h.locked {
		return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "The delivery was recorded while the command was prepared")
	}
	path := "/args/commitment_id"
	if dl != nil {
		path = "/args/delivery_id"
	}
	c, err := deliveryCommitment(ctx, d, h.h.l, path)
	if err != nil {
		return action.Outcome{}, err
	}
	now := h.s.Clock.Now()
	in := supplierdelivery.Inputs{Commitment: c, Readable: h.readable, Recorder: d.Actor, Epoch: d.Epoch, Now: now}
	if dl == nil {
		if dl, err = supplierdelivery.Record(h.request(), in); err != nil {
			return action.Outcome{}, err
		}
		if h.fileErr != nil {
			return action.Outcome{}, h.fileErr
		}
		if err = postgres.InsertDelivery(ctx, exec, sc, dl); err != nil {
			return action.Outcome{}, err
		}
		return deliveryOutcome(dl, supplierdelivery.StatusRecorded), nil
	}
	if err = dl.Correct(h.cmd.ExpectedVersion, h.request(), in); err != nil {
		return action.Outcome{}, err
	}
	if h.fileErr != nil {
		return action.Outcome{}, h.fileErr
	}
	if err = postgres.SaveDelivery(ctx, exec, sc, dl, kernel.Instant(now)); err != nil {
		return action.Outcome{}, err
	}
	return deliveryOutcome(dl, supplierdelivery.StatusRecorded), nil
}

// RecordSupplierDelivery records a supplier's claim of a delivery, or a correction of one
// (supplier_delivery.record; PUR-INV-008, ES-12 step 1). Human or AI; it publishes no event: a claim
// is not a fact.
func (s Service) RecordSupplierDelivery(ctx context.Context, cmd api.RecordSupplierDelivery) (api.DeliveryReceipt, error) {
	h := &recordDelivery{s: s, cmd: cmd}
	validation := cmd.Validate()
	if validation == nil {
		_, _, args := request(cmd)
		var err error
		if h.hash, err = action.RequestHash(args); err != nil {
			return api.DeliveryReceipt{}, err
		}
	}
	res, err := s.run(ctx, action.Command{Name: api.CommandRecordSupplierDelivery, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.DeliveryID,
		Reason: cmd.Reason}, cmd, validation, h)
	if err != nil {
		return api.DeliveryReceipt{}, err
	}
	res.Replayed = res.Replayed || h.replay
	return s.deliveryReceipt(ctx, res)
}

// verifyDelivery runs supplier_delivery.verify.
type verifyDelivery struct {
	s        Service
	cmd      api.VerifySupplierDelivery
	readable map[string]bool
	prepared int64 // the delivery's row version the sources were read for
	h        held
}

// Prepare reads, for a verification, which EVD sources the verifier reads among those the pending
// claim cites and those the verifier adds (R76).
func (h *verifyDelivery) Prepare(ctx context.Context) error {
	h.readable, h.prepared = nil, 0
	if h.cmd.Decision != api.DecisionVerify {
		return nil
	}
	es := slices.Clone(h.cmd.Evidence)
	err := h.s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		d, err := postgres.LoadDelivery(ctx, exec, scope(ctx), h.cmd.DeliveryID, postgres.NoLock)
		if err != nil || d == nil {
			return err
		}
		h.prepared = d.RowVersion
		if p := d.Pending(); p != nil {
			for _, e := range p.Evidence {
				es = append(es, api.Evidence{SourceID: e.SourceID, Locator: e.Locator})
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	h.readable, err = h.s.readable(ctx, es)
	return err
}

func (h *verifyDelivery) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.h, err = lockDelivery(ctx, exec, h.cmd.DeliveryID, "")
	return err
}

// Apply refuses, in this order: a delivery the verifier does not see (NOT_FOUND), Decidable's
// refusals (VERSION_CONFLICT, INVALID_STATE, INDEPENDENT_REVIEW_REQUIRED); then a rejection is
// recorded; a verification goes on with a draft not intact (REFERENCE_INVALID), Verify's refusals,
// measured against what the commitment's other deliveries verified, and a line it would leave verified
// below its approved dispositions (INVALID_STATE; PUR-INV-013, S5R-D05: a correction never takes away
// goods already disposed of), both read under the commitment root's lock.
func (h *verifyDelivery) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc := scope(ctx)
	dl := h.h.d
	if dl == nil || !d.Sees(dl.OwnerActor, dl.OwnerTeam) {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/delivery_id", "")
	}
	if err := dl.Decidable(h.cmd.ExpectedVersion, d.Actor); err != nil {
		return action.Outcome{}, err
	}
	now := kernel.Instant(h.s.Clock.Now())
	act := supplierdelivery.Act{By: d.Actor, Epoch: d.Epoch, At: now, Reason: h.cmd.Reason}
	if h.cmd.Decision == api.DecisionReject {
		dl.Reject(act)
		if err := postgres.SaveDelivery(ctx, exec, sc, dl, now); err != nil {
			return action.Outcome{}, err
		}
		return deliveryOutcome(dl, supplierdelivery.StatusRejected), nil
	}
	if h.prepared != dl.Stored {
		return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/delivery_id", "The delivery changed while the command was prepared")
	}
	c, err := deliveryCommitment(ctx, d, h.h.l, "/args/delivery_id")
	if err != nil {
		return action.Outcome{}, err
	}
	elsewhere, err := postgres.VerifiedOfCommitment(ctx, exec, sc, dl.CommitmentID, dl.ID)
	if err != nil {
		return action.Outcome{}, err
	}
	x := supplierdelivery.Verification{Commitment: c, Elsewhere: elsewhere, Evidence: deliveryEvidence(h.cmd.Evidence), Readable: h.readable}
	for _, l := range h.cmd.Lines {
		q, _ := kernel.ParseDecimal(l.VerifiedQuantity, kernel.QuantityFormat)
		x.Lines = append(x.Lines, supplierdelivery.Quantity{OrderLineID: l.OrderLineID, Verified: q})
	}
	if err = dl.Verify(x, act); err != nil {
		return action.Outcome{}, err
	}
	if err = keepsDispositions(ctx, exec, dl, elsewhere, h.cmd.Lines); err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.SaveDelivery(ctx, exec, sc, dl, now); err != nil {
		return action.Outcome{}, err
	}
	v := dl.Current()
	lines := make([]map[string]any, len(v.Lines))
	for i, l := range v.Lines {
		lines[i] = map[string]any{"order_line_id": l.OrderLineID, "verified_quantity": text(l.Verified)}
	}
	var superseded int64
	if s := dl.Superseded(); s != nil {
		superseded = s.Number
	}
	event := action.Event{Type: api.EventSupplierDeliveryVerified, AggregateType: "SupplierDelivery", AggregateID: dl.ID,
		AggregateVersion: dl.RowVersion, Fields: map[string]any{"delivery_id": dl.ID, "delivery_version": v.Number,
			"superseded_version": superseded, "commitment_id": dl.CommitmentID, "order_id": dl.OrderID, "supplier_id": dl.SupplierID,
			"handover_place": v.HandoverPlace, "handed_over_at": kernel.FormatTimestamp(v.HandedOverAt), "verified_by": d.Actor.ID,
			"lines": lines}}
	return deliveryOutcome(dl, supplierdelivery.StatusVerified, event), nil
}

// keepsDispositions refuses a verification of dl that would leave a line of its commitment verified
// below the line's approved dispositions (INVALID_STATE at the line's verified quantity): what the
// commitment's other deliveries verified (elsewhere) and this version's verified quantities against the
// commitment's dispositions, read under the commitment root's lock.
func keepsDispositions(ctx context.Context, exec bentopg.Executor, dl *supplierdelivery.Delivery, elsewhere map[string]kernel.Decimal,
	lines []api.DeliveryVerifiedLine) error {
	xs, err := postgres.DispositionsOfCommitment(ctx, exec, scope(ctx), dl.CommitmentID)
	if err != nil || len(xs) == 0 {
		return err
	}
	after := maps.Clone(elsewhere)
	var touched []string
	for _, l := range dl.Current().Lines {
		after[l.OrderLineID] = after[l.OrderLineID].Add(l.Verified)
		touched = append(touched, l.OrderLineID)
	}
	line := commitment.Undercut(xs, after, touched)
	if line == "" {
		return nil
	}
	path := "/args/lines"
	if i := slices.IndexFunc(lines, func(l api.DeliveryVerifiedLine) bool { return l.OrderLineID == line }); i >= 0 {
		path = "/args/lines/" + itoa(int64(i)) + "/verified_quantity"
	}
	return kernel.Reject(kernel.CodeInvalidState, path, "Line "+line+" is disposed of "+commitment.Disposed(xs)[line].String()+
		" by approved dispositions; a verification leaves it verified no less (PUR-INV-013)")
}

// VerifySupplierDelivery verifies or rejects the claim of a delivery that awaits verification
// (supplier_delivery.verify; PUR-INV-008, SM-PUR-SUPPLIER-DELIVERY, ES-12 step 2). Human only,
// independent of the claim's recorder. A verification publishes SupplierDeliveryVerified; a
// rejection publishes nothing.
func (s Service) VerifySupplierDelivery(ctx context.Context, cmd api.VerifySupplierDelivery) (api.DeliveryReceipt, error) {
	h := &verifyDelivery{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandVerifySupplierDelivery, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.DeliveryID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
	if err != nil {
		return api.DeliveryReceipt{}, err
	}
	return s.deliveryReceipt(ctx, res)
}

// deliveryReceipt completes res with what the stored delivery binds and counts now.
func (s Service) deliveryReceipt(ctx context.Context, res api.Receipt) (api.DeliveryReceipt, error) {
	out := api.DeliveryReceipt{Receipt: res}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		d, err := postgres.LoadDelivery(ctx, exec, scope(ctx), res.ObjectID, postgres.NoLock)
		if err != nil || d == nil {
			return err
		}
		out.CommitmentID, out.OrderID, out.SupplierID, out.LatestVersion, out.VerifiedVersion = d.CommitmentID, d.OrderID, d.SupplierID,
			d.LatestVersion, d.VerifiedVersion
		return nil
	})
	return out, err
}
