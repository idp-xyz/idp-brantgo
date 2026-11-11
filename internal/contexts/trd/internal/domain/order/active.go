package order

import (
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/amendment"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/orderrev"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The active order (WBS S3-15; SM-TRD-ORDER after activation, SM-TRD-ORDER-AMENDMENT; TRD-INV-016,
// 018, 019). Its amendments are prepared, submitted, decided and withdrawn under the order's lock
// without moving the order's row version (each has a version of its own); applying one moves the
// order to its next revision and row version, with the revision, the amendment's applied state and
// the commit evidence in one transaction. Cancelling or closing an active order moves its row
// version once more and records how it ended.

// RevisionRecord is an order revision after the first as stored: its number and hash, the revision
// it amends, the amendment that produced it and the order's row version it became current at.
type RevisionRecord struct {
	Revision      int64
	Hash          string
	PreviousHash  string
	AmendmentID   string
	AmendmentHash string
	RowVersion    int64
	At            time.Time
}

// AmendmentCommit is the commit evidence of an applied amendment (TRD-INV-016): the revision it
// produced and the one it amended, the amendment, the command's key, the epochs and row version it
// committed at, who and when, and its entries in a fixed order.
type AmendmentCommit struct {
	IdempotencyKey string
	OrderRevision  int64
	OrderHash      string
	FromHash       string
	AmendmentID    string
	AmendmentHash  string
	AuthorityEpoch int64
	ControlEpoch   int64
	RowVersion     int64
	By             kernel.ActorRef
	At             time.Time
	Entries        []quote.Entry
	Fresh          bool
}

// EntriesJSON returns the entries as stored: canonical JSON, the bytes their SHA-256 covers.
func (c AmendmentCommit) EntriesJSON() ([]byte, error) {
	return quote.Commit{Entries: c.Entries}.EntriesJSON()
}

// End is how an active order ended (orderrev.Ending): at which revision and row version.
type End struct {
	orderrev.Ending
	OrderRevision int64
	OrderHash     string
	RowVersion    int64
	Fresh         bool
}

// SetCurrent gives the loader's current revision to o (a later revision read from its document).
func (o *Order) SetCurrent(r *orderrev.Revision) { o.current = r }

// Revision returns the order's current revision: revision 1 read from the snapshot, or the later
// revision the loader set (REFERENCE_INVALID at /order when it does not match the order's columns).
func (o *Order) Revision() (*orderrev.Revision, error) {
	if o.current == nil && o.OrderRevision == 1 {
		r, err := orderrev.FromSnapshot(o.Snapshot, o.SnapshotHash)
		if err != nil {
			return nil, err
		}
		o.current = r
	}
	if r := o.current; r == nil || r.Number != o.OrderRevision || r.Hash != o.OrderHash || r.OrderID != o.ID {
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/order", "The order's current revision is not readable")
	}
	return o.current, nil
}

// Amendment returns amendment id of the order, or nil.
func (o *Order) Amendment(id string) *amendment.Amendment {
	for _, a := range o.Amendments {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// state is what the amendment rules read of the order.
func (o *Order) state() (amendment.Order, error) {
	if o.Status != Active {
		return amendment.Order{Status: orderrev.Status(o.Status)}, nil
	}
	r, err := o.Revision()
	if err != nil {
		return amendment.Order{}, err
	}
	return amendment.Order{Status: orderrev.Active, Current: r}, nil
}

// amendmentOf returns amendment id, NOT_FOUND at /args/amendment_id for none.
func (o *Order) amendmentOf(id string) (*amendment.Amendment, error) {
	a := o.Amendment(id)
	if a == nil {
		return nil, kernel.Reject(kernel.CodeNotFound, "/args/amendment_id", "")
	}
	return a, nil
}

// PrepareAmendment decides order.amendment.prepare (amendment.Prepare) on the order: a new
// amendment at expected version 0, or the draft of that ID again. It returns the amendment and the
// revision it would yield with what that changes.
func (o *Order) PrepareAmendment(p amendment.Preparation, f orderrev.Fulfilment, facts orderrev.Facts, by kernel.ActorRef, epoch int64,
	now time.Time) (*amendment.Amendment, *orderrev.Revision, *orderrev.Diff, error) {
	if o.Status != Active {
		return nil, nil, nil, kernel.RejectAs(amendment.CodeOrderNotActive, orderrev.Categories[orderrev.CodeOrderNotActive], "/args/order_id",
			"The order is "+string(o.Status))
	}
	st, err := o.state()
	if err != nil {
		return nil, nil, nil, err
	}
	existing := o.Amendment(p.AmendmentID)
	a, preview, d, err := amendment.Prepare(existing, p, st, f, facts, by, epoch, now)
	if err != nil {
		return nil, nil, nil, err
	}
	if existing == nil {
		o.Amendments = append(o.Amendments, a)
	}
	return a, preview, d, nil
}

// SubmitAmendment decides order.amendment.submit of amendment id at its expected version.
func (o *Order) SubmitAmendment(id string, expected int64, by kernel.ActorRef, epoch int64, now time.Time) (*amendment.Amendment, error) {
	a, err := o.amendmentOf(id)
	if err != nil {
		return nil, err
	}
	st, err := o.state()
	if err != nil {
		return nil, err
	}
	return a, a.Submit(expected, st, by, epoch, now)
}

// DecideAmendment decides order.amendment.decide of amendment id at its expected version.
func (o *Order) DecideAmendment(id string, expected int64, d amendment.Decision, by kernel.ActorRef, epoch int64,
	now time.Time) (*amendment.Amendment, error) {
	a, err := o.amendmentOf(id)
	if err != nil {
		return nil, err
	}
	st, err := o.state()
	if err != nil {
		return nil, err
	}
	return a, a.Decide(expected, d, st, by, epoch, now)
}

// WithdrawAmendment decides order.amendment.withdraw of amendment id at its expected version.
func (o *Order) WithdrawAmendment(id string, expected int64, act quote.Act) (*amendment.Amendment, error) {
	a, err := o.amendmentOf(id)
	if err != nil {
		return nil, err
	}
	return a, a.Withdraw(expected, act)
}

// ApplyAmendment decides order.amendment.apply of amendment id (SM-TRD-ORDER active → active): the
// amendment applies (amendment.Apply) against the order's current revision, the shipments and facts
// x brings and the decider's standing now; the order moves to the revision it yields at its next row
// version, and the commit evidence is recorded with it.
func (o *Order) ApplyAmendment(id string, x amendment.Applying, standings quote.Standings, by kernel.ActorRef, epoch int64,
	now time.Time) (*amendment.Amendment, *orderrev.Revision, *orderrev.Diff, error) {
	a, err := o.amendmentOf(id)
	if err != nil {
		return nil, nil, nil, err
	}
	st, err := o.state()
	if err != nil {
		return nil, nil, nil, err
	}
	x.RowVersion = o.RowVersion + 1
	next, d, commit, err := a.Apply(x, st, standings, by, epoch, now)
	if err != nil {
		return nil, nil, nil, err
	}
	from := o.OrderHash
	o.RowVersion++
	o.OrderRevision, o.OrderHash, o.current, o.NewRevision = next.Number, next.Hash, next, next
	at := kernel.Instant(now)
	o.Revisions = append(o.Revisions, RevisionRecord{Revision: next.Number, Hash: next.Hash, PreviousHash: from, AmendmentID: a.ID,
		AmendmentHash: a.Hash, RowVersion: o.RowVersion, At: at})
	o.AmendmentCommits = append(o.AmendmentCommits, &AmendmentCommit{IdempotencyKey: x.IdempotencyKey, OrderRevision: next.Number,
		OrderHash: next.Hash, FromHash: from, AmendmentID: a.ID, AmendmentHash: a.Hash, AuthorityEpoch: epoch, ControlEpoch: x.ControlEpoch,
		RowVersion: o.RowVersion, By: by, At: at, Entries: commit.Entries, Fresh: true})
	o.Changed = true
	return a, next, d, nil
}

// EndActive decides how an active order ends (TRD-INV-019): order.cancel (orderrev.Cancel),
// order.close_remainder (orderrev.CloseRemainder) or order.close (orderrev.Close), at its current
// revision, against the shipments FUL confirmed (f), what is open downstream (d) and the approved
// tolerances, by act. The order's row version moves once; the slot and every revision stay.
func (o *Order) EndActive(command string, f orderrev.Fulfilment, d orderrev.Downstream, tolerances map[string]orderrev.Tolerance,
	act quote.Act) (*End, error) {
	from := orderrev.Status(o.Status)
	var r *orderrev.Revision
	if o.Status == Active {
		var err error
		if r, err = o.Revision(); err != nil {
			return nil, err
		}
	}
	var e *orderrev.Ending
	var err error
	switch command {
	case orderrev.CommandCancel:
		e, err = orderrev.Cancel(from, r, f, d, act)
	case orderrev.CommandCloseRemainder:
		e, err = orderrev.CloseRemainder(from, r, f, d, act)
	case orderrev.CommandClose:
		e, err = orderrev.Close(from, r, f, tolerances, act)
	default:
		_, err = orderrev.Transition(from, command)
	}
	if err != nil {
		return nil, err
	}
	o.RowVersion++
	o.Status, o.Changed = Status(e.Status), true
	o.End = &End{Ending: *e, OrderRevision: o.OrderRevision, OrderHash: o.OrderHash, RowVersion: o.RowVersion, Fresh: true}
	return o.End, nil
}
