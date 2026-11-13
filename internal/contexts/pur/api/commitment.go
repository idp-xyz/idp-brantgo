package api

import (
	"context"
	"strconv"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Purchase commitments (WBS S5-01; tactical model PUR-AGG-PurchaseCommitment, SM-PUR-COMMITMENT,
// PUR-INV-009, PUR-INV-010; S5 decision D-14; A47 #1–#8 of Python V0.50's
// docs/T04_A46_PURCHASE_DRAFT_REPORT_V0.1.md, design only):
//
//	purchase.commitment.request            human or AI: requests the commitment of a prepared draft,
//	                                       restating its draft hash; a draft has one live
//	                                       commitment (requested, approved, issued or confirmed).
//	purchase.commitment.decide             human only, independent of the draft's preparer and the
//	                                       requester (INDEPENDENT_REVIEW_REQUIRED): approve or reject
//	                                       (reason required). An approval prepares the draft's
//	                                       request again on the facts as they are now, inside the
//	                                       decision's transaction after the commitment and the
//	                                       demand allocation roots are locked, and requires it equal
//	                                       to the sealed draft field by field (PURCHASE_DRAFT_STALE
//	                                       otherwise); every upstream reference is re-checked
//	                                       through its owner's guard (CAT, CST, POL, PTY, SUP, TRD);
//	                                       the approval is valid until valid_until, after the
//	                                       decision and not after the draft's supply validity; and
//	                                       every deviation of the draft (source_differs,
//	                                       cost_above_costing, cost_not_comparable, overbuy) is
//	                                       acknowledged with a reason
//	                                       (PURCHASE_DEVIATION_UNACKNOWLEDGED).
//	purchase.commitment.revoke             human only: revokes an approval before it is issued.
//	purchase.commitment.issue              human only: issues an approved commitment once, before
//	                                       its approval expires (APPROVAL_EXPIRED) and while its
//	                                       decider is still qualified (PURCHASE_COMMITMENT_NOT_APPROVED
//	                                       otherwise), with POL's purchase order number from the
//	                                       number_sequence revision sequence_ref (document type
//	                                       purchase_order, taken before the transaction: number.issue
//	                                       as the issuer, business reference the commitment ID), the
//	                                       time it was sent and the EVD evidence of sending; the same
//	                                       re-checks as an approval. The draft is then committed and is
//	                                       never voided (PURCHASE_DRAFT_COMMITTED).
//	purchase.supplier_confirmation.record  human or AI: the supplier's answer with its EVD evidence and
//	                                       received time; as_issued confirms the commitment,
//	                                       with_deviations records the deviation and leaves it issued.
//
// Request members in snake_case: purchase.commitment.request {commitment_id, draft_id, draft_hash};
// purchase.commitment.decide {commitment_id, expected_version, draft_hash, decision (approve or
// reject), valid_until (approve), acknowledgements [{order_line_id, kind, reason}] (approve), reason
// (reject; optional to approve)}; purchase.commitment.revoke {commitment_id, expected_version,
// reason}; purchase.commitment.issue {commitment_id, expected_version, sequence_ref {id, revision},
// sent_at, evidence [{source_id, locator}], file_id (optional)};
// purchase.supplier_confirmation.record {commitment_id, expected_version, result (as_issued or
// with_deviations), received_at, evidence [{source_id, locator}], deviation (with_deviations),
// supplier_reference (optional)}. Timestamps are the contracts' UTC form with a literal Z.

// Commitment commands, reads and events (command catalog, Context Map PUR facts).
const (
	CommandRequestCommitment          = "purchase.commitment.request"
	CommandDecideCommitment           = "purchase.commitment.decide"
	CommandRevokeCommitment           = "purchase.commitment.revoke"
	CommandIssueCommitment            = "purchase.commitment.issue"
	CommandRecordSupplierConfirmation = "purchase.supplier_confirmation.record"

	// Reads: one commitment and an order's commitments (pur.read).
	CommandGetCommitment       = "purchase.commitment.get"
	CommandGetOrderCommitments = "purchase.commitment.list"

	EventPurchaseCommitmentApproved   = "idp-brant.pur.purchase_commitment_approved.v1"
	EventPurchaseCommitmentRevoked    = "idp-brant.pur.purchase_commitment_revoked.v1"
	EventPurchaseCommitmentIssued     = "idp-brant.pur.purchase_commitment_issued.v1"
	EventSupplierConfirmationRecorded = "idp-brant.pur.supplier_confirmation_recorded.v1"

	// States of a commitment (SM-PUR-COMMITMENT); CommitmentStateExpired is derived when read.
	CommitmentRequested    = "requested"
	CommitmentApproved     = "approved"
	CommitmentRejected     = "rejected"
	CommitmentRevoked      = "revoked"
	CommitmentIssued       = "issued"
	CommitmentConfirmed    = "confirmed"
	CommitmentStateExpired = "expired"

	// CommitmentCommitted is the commitment state of a draft whose commitment was issued.
	CommitmentCommitted = "committed"

	DecisionApprove = "approve"
	DecisionReject  = "reject"

	// Deviations an approval acknowledges, line by line.
	DeviationSourceDiffers = "source_differs"
	DeviationCostAbove     = "cost_above_costing"
	DeviationNotComparable = "cost_not_comparable"
	DeviationOverbuy       = "overbuy"

	// Results of a supplier confirmation.
	ConfirmationAsIssued       = "as_issued"
	ConfirmationWithDeviations = "with_deviations"

	MaxEvidence     = 20
	MaxLocator      = 500
	MaxSupplierRef  = 64
	MaxAcknowledged = 4 * MaxLines
)

var deviationKinds = []string{DeviationSourceDiffers, DeviationCostAbove, DeviationNotComparable, DeviationOverbuy}

// Evidence is an EVD source (SourceRef v1, Context Map R76) and where in it the fact is shown.
type Evidence struct {
	SourceID string
	Locator  string
}

// Acknowledgement is an approval's acknowledgement of one deviation of a draft line, with its reason.
type Acknowledgement struct {
	OrderLineID string
	Kind        string
	Reason      string
}

// SequenceRef is a POL number_sequence revision.
type SequenceRef struct {
	ID       string
	Revision int64
}

// RequestCommitment requests commitment CommitmentID of draft DraftID at DraftHash. Human or AI.
type RequestCommitment struct {
	IdempotencyKey string
	CommitmentID   string
	DraftID        string
	DraftHash      string
}

// DecideCommitment approves or rejects commitment CommitmentID at ExpectedVersion, stating the draft
// hash the decider reviewed. Human only.
type DecideCommitment struct {
	IdempotencyKey   string
	CommitmentID     string
	ExpectedVersion  int64
	DraftHash        string
	Decision         string
	ValidUntil       string
	Acknowledgements []Acknowledgement
	Reason           string
}

// RevokeCommitment revokes the approval of commitment CommitmentID at ExpectedVersion. Human only.
type RevokeCommitment struct {
	IdempotencyKey  string
	CommitmentID    string
	ExpectedVersion int64
	Reason          string
}

// IssueCommitment issues commitment CommitmentID at ExpectedVersion. Human only.
type IssueCommitment struct {
	IdempotencyKey  string
	CommitmentID    string
	ExpectedVersion int64
	SequenceRef     SequenceRef
	SentAt          string
	Evidence        []Evidence
	FileID          string
}

// RecordSupplierConfirmation records the supplier's answer to commitment CommitmentID at
// ExpectedVersion. Human or AI.
type RecordSupplierConfirmation struct {
	IdempotencyKey    string
	CommitmentID      string
	ExpectedVersion   int64
	Revision          int64 // optional: the commitment revision the supplier confirms (S5-19); 0 for the one in force
	Result            string
	ReceivedAt        string
	Evidence          []Evidence
	Deviation         string
	SupplierReference string
}

func (RequestCommitment) purCommand()          {}
func (DecideCommitment) purCommand()           {}
func (RevokeCommitment) purCommand()           {}
func (IssueCommitment) purCommand()            {}
func (RecordSupplierConfirmation) purCommand() {}

func version(v int64) bool { return v >= 1 && v <= maxVersion }

func hash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func timestamp(s string) bool {
	_, err := kernel.ParseTimestamp(s)
	return err == nil
}

func index(path string, i int) string { return path + "/" + strconv.Itoa(i) }

func validEvidence(es []Evidence, path string) error {
	if len(es) < 1 || len(es) > MaxEvidence {
		return schema(path)
	}
	seen := map[Evidence]bool{}
	for i, e := range es {
		p := index(path, i)
		switch {
		case !kernel.ValidIdentifier(e.SourceID):
			return schema(p + "/source_id")
		case !text(e.Locator, MaxLocator):
			return schema(p + "/locator")
		case seen[e]:
			return schema(p)
		}
		seen[e] = true
	}
	return nil
}

// text reports whether s is 1 to max characters of valid UTF-8 without control characters and not
// only white space.
func text(s string, max int) bool { return Reason(s) && len([]rune(s)) <= max }

// Validate checks the closed contract.
func (c RequestCommitment) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.CommitmentID):
		return schema("/args/commitment_id")
	case !kernel.ValidIdentifier(c.DraftID):
		return schema("/args/draft_id")
	case !hash(c.DraftHash):
		return schema("/args/draft_hash")
	}
	return nil
}

// Validate checks the closed contract: an approval states its validity and its acknowledgements,
// each of a known kind with a reason and at most once; a rejection states neither, and its reason.
func (c DecideCommitment) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.CommitmentID):
		return schema("/args/commitment_id")
	case !version(c.ExpectedVersion):
		return schema("/args/expected_version")
	case !hash(c.DraftHash):
		return schema("/args/draft_hash")
	case c.Reason != "" && !Reason(c.Reason):
		return schema("/args/reason")
	}
	switch c.Decision {
	case DecisionApprove:
		if !timestamp(c.ValidUntil) {
			return schema("/args/valid_until")
		}
		if len(c.Acknowledgements) > MaxAcknowledged {
			return schema("/args/acknowledgements")
		}
		type key struct{ line, kind string }
		seen := map[key]bool{}
		for i, a := range c.Acknowledgements {
			p := index("/args/acknowledgements", i)
			k := key{a.OrderLineID, a.Kind}
			switch {
			case !kernel.ValidIdentifier(a.OrderLineID):
				return schema(p + "/order_line_id")
			case !contains(deviationKinds, a.Kind):
				return schema(p + "/kind")
			case !Reason(a.Reason):
				return schema(p + "/reason")
			case seen[k]:
				return schema(p)
			}
			seen[k] = true
		}
	case DecisionReject:
		switch {
		case c.ValidUntil != "":
			return schema("/args/valid_until")
		case c.Acknowledgements != nil:
			return schema("/args/acknowledgements")
		case c.Reason == "":
			return schema("/args/reason")
		}
	default:
		return schema("/args/decision")
	}
	return nil
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// Validate checks the closed contract.
func (c RevokeCommitment) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.CommitmentID):
		return schema("/args/commitment_id")
	case !version(c.ExpectedVersion):
		return schema("/args/expected_version")
	case !Reason(c.Reason):
		return schema("/args/reason")
	}
	return nil
}

// Validate checks the closed contract.
func (c IssueCommitment) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.CommitmentID):
		return schema("/args/commitment_id")
	case !version(c.ExpectedVersion):
		return schema("/args/expected_version")
	case !kernel.ValidIdentifier(c.SequenceRef.ID):
		return schema("/args/sequence_ref/id")
	case !version(c.SequenceRef.Revision):
		return schema("/args/sequence_ref/revision")
	case !timestamp(c.SentAt):
		return schema("/args/sent_at")
	case c.FileID != "" && !kernel.ValidIdentifier(c.FileID):
		return schema("/args/file_id")
	}
	return validEvidence(c.Evidence, "/args/evidence")
}

// Validate checks the closed contract: a confirmation with deviations states them, one as issued
// does not.
func (c RecordSupplierConfirmation) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.CommitmentID):
		return schema("/args/commitment_id")
	case !version(c.ExpectedVersion):
		return schema("/args/expected_version")
	case c.Revision != 0 && !version(c.Revision):
		return schema("/args/revision")
	case c.Result != ConfirmationAsIssued && c.Result != ConfirmationWithDeviations:
		return schema("/args/result")
	case !timestamp(c.ReceivedAt):
		return schema("/args/received_at")
	case (c.Result == ConfirmationWithDeviations) != (c.Deviation != ""), c.Deviation != "" && !Reason(c.Deviation):
		return schema("/args/deviation")
	case c.SupplierReference != "" && !text(c.SupplierReference, MaxSupplierRef):
		return schema("/args/supplier_reference")
	}
	return validEvidence(c.Evidence, "/args/evidence")
}

// CommitmentReceipt is the answer of every commitment command: the receipt (ObjectID the commitment
// ID, RowVersion its row version, Status its status) and what it binds: the draft at its hash, the
// order at its revision and hash, the supplier, the currency, the approval's validity (zero before an
// approval) and the purchase order number (empty before the issue). A replay, and a request answered
// from the stored commitment, are Replayed.
type CommitmentReceipt struct {
	Receipt
	DraftID       string
	DraftHash     string
	OrderID       string
	OrderRevision int64
	OrderHash     string
	SupplierID    string
	Currency      string
	ValidUntil    time.Time
	PONumber      string
	Revision      int64 // the commitment revision in force (S5-19)
}

// AcknowledgementView is an acknowledged deviation.
type AcknowledgementView struct {
	OrderLineID string
	Kind        string
	Reason      string
}

// ConfirmationView is a supplier confirmation: the commitment version it was recorded at, its result,
// when it was received, the deviation, the supplier's reference, the evidence and who recorded it.
type ConfirmationView struct {
	CommitmentVersion  int64
	CommitmentRevision int64
	Result             string
	ReceivedAt         time.Time
	Deviation          string
	SupplierReference  string
	Evidence           []Evidence
	RecordedBy         kernel.ActorRef
	RecordedAt         time.Time
	AuthorityEpoch     int64
}

// CommitmentView is a commitment as the reads show it: what it binds, its status and its state now
// (expired for an approval whose validity ended), the request, the decision with its validity and
// acknowledgements, the revocation, the issue with the purchase order number and the evidence of
// sending, and the supplier's confirmations. Zero times and empty actors mean the step has not
// happened. CheckedAt is when the read decided the state.
type CommitmentView struct {
	CommitmentID     string
	Status           string
	State            string
	RowVersion       int64
	DraftID          string
	DraftHash        string
	OrderID          string
	OrderRevision    int64
	OrderHash        string
	SupplierID       string
	Currency         string
	SupplyValidUntil time.Time
	RequestedBy      kernel.ActorRef
	RequestedAt      time.Time
	RequestEpoch     int64
	Decision         string
	DecidedBy        kernel.ActorRef
	DecidedAt        time.Time
	DecideEpoch      int64
	ValidUntil       time.Time
	DecisionReason   string
	Acknowledgements []AcknowledgementView
	RevokedBy        kernel.ActorRef
	RevokedAt        time.Time
	RevokeEpoch      int64
	RevokeReason     string
	IssuedBy         kernel.ActorRef
	IssuedAt         time.Time
	IssueEpoch       int64
	PONumber         string
	SequenceRef      SequenceRef
	SentAt           time.Time
	IssueEvidence    []Evidence
	IssueFileID      string
	IssueFileSHA256  string
	Confirmations    []ConfirmationView
	// S5-19: the revision in force, what the commitment buys of each line at it with the verified
	// deliveries, whether every line is verified delivered in full (fulfilled, as after a short
	// receipt), the revisions after the first and the cancellation.
	Revision       int64
	Lines          []CommitmentLineView
	Fulfilled      bool
	Revisions      []RevisionView
	CancelledBy    kernel.ActorRef
	CancelledAt    time.Time
	CancelEpoch    int64
	CancelReason   string
	CancelEvidence []Evidence
	CheckedAt      time.Time
}

// CommitmentCommands is PUR's command entry point for purchase commitments. Errors are
// *kernel.Rejection for refusals.
type CommitmentCommands interface {
	RequestCommitment(ctx context.Context, cmd RequestCommitment) (CommitmentReceipt, error)
	DecideCommitment(ctx context.Context, cmd DecideCommitment) (CommitmentReceipt, error)
	RevokeCommitment(ctx context.Context, cmd RevokeCommitment) (CommitmentReceipt, error)
	IssueCommitment(ctx context.Context, cmd IssueCommitment) (CommitmentReceipt, error)
	RecordSupplierConfirmation(ctx context.Context, cmd RecordSupplierConfirmation) (CommitmentReceipt, error)
}

// CommitmentQueries is PUR's audited read side of purchase commitments.
type CommitmentQueries interface {
	// GetCommitment returns a commitment the caller sees, audited as purchase.commitment.get
	// (pur.read): NOT_FOUND otherwise; REFERENCE_INVALID when the caller no longer reads its order.
	GetCommitment(ctx context.Context, commitmentID string) (CommitmentView, error)
	// GetOrderCommitments returns the commitments of an order the caller sees (TRD's lookup decides
	// that: NOT_FOUND otherwise) that the caller sees, by commitment ID, audited as
	// purchase.commitment.list (pur.read).
	GetOrderCommitments(ctx context.Context, orderID string) ([]CommitmentView, error)
}

// DownstreamCommitments v1 (Context Map R71 PUR→TRD, "OHS（事务内无锁读取）", guardLock false: 订单的未处置
// 采购草稿、承诺与需求分配; risk 有未处置承诺仍取消订单). TRD reads what is open against an order in PUR
// with its own executor inside its own transaction, after it locked the order row FOR UPDATE, without
// taking a lock in PUR (S5 decision D-1): every PUR command that adds to the answer — a draft's
// preparation, a commitment's request, approval and issue, a change's application — holds that order row FOR SHARE through
// OrderGuard v1 until it commits, so it either committed before TRD's lock and is read, or waits and
// then finds the order changed. Commands that only remove from it (void, reject, revoke, cancel) take no
// order lock: TRD reading them as still open refuses conservatively.
//
// The function is SECURITY DEFINER in PUR's schema with a fixed search_path
// (migrations/pur/000002_pur_purchase_commitments.sql):
//
//	brant_pur.order_commitments_v1(p_order_id text)
//	    RETURNS TABLE (kind text, object_id text, draft_id text, order_line_id text, order_revision bigint, state text,
//	                   quantity text, disposition text)
//
// It returns one row per open object of the order in the caller's scope (app.tenant_id and
// app.company_id), ordered by kind and object, and no row when nothing is open:
//
//	kind purchase_commitment  object_id the commitment, draft_id its draft, state requested or approved
//	                          (its draft not voided), issued or confirmed, or fulfilled — issued or
//	                          confirmed and every line verified delivered in full at the revision in
//	                          force (a short receipt, S5-19); a cancelled commitment is not listed;
//	kind purchase_draft       object_id and draft_id the draft, prepared and without an issued
//	                          commitment (nor a cancelled one), state prepared;
//	kind demand_allocation    object_id and order_line_id the order line, quantity the effective
//	                          allocated quantity in the line's unit (QuantityFormat) — after a change
//	                          the revision's, after a cancellation what was verified delivered —,
//	                          state allocated.
//
// order_revision is the order revision the object binds (a commitment's: that of its revision in
// force, which a change prepared on an amended order moves); disposition is the approved disposition of
// the object, empty until an order-change review records one (S5-09). The function applies no data
// scope. A caller:
//
//	rows, err := exec.Query(ctx, purapi.DownstreamCommitmentsSQL, purapi.DownstreamCommitmentsArgs(orderID)...)
//	for rows.Next() {
//		item, err := purapi.ScanDownstreamCommitment(rows.Scan)
//		...
//	}
const (
	DownstreamCommitmentsFunction = "brant_pur.order_commitments_v1"
	DownstreamCommitmentsSQL      = `SELECT kind, object_id, draft_id, order_line_id, order_revision, state, quantity, disposition
		FROM brant_pur.order_commitments_v1($1)`

	DownstreamKindCommitment = "purchase_commitment"
	DownstreamKindDraft      = "purchase_draft"
	DownstreamKindAllocation = "demand_allocation"
	DownstreamStateAllocated = "allocated"
)

// DownstreamCommitmentsArgs returns the arguments of DownstreamCommitmentsSQL for order orderID.
func DownstreamCommitmentsArgs(orderID string) []any { return []any{orderID} }

// DownstreamCommitment is one row of DownstreamCommitments v1.
type DownstreamCommitment struct {
	Kind          string
	ObjectID      string
	DraftID       string
	OrderLineID   string
	OrderRevision int64
	State         string
	Quantity      string
	Disposition   string
}

// ScanDownstreamCommitment reads one row of DownstreamCommitmentsSQL through scan, such as a pgx
// row's Scan.
func ScanDownstreamCommitment(scan func(dest ...any) error) (DownstreamCommitment, error) {
	var out DownstreamCommitment
	var draft, line, quantity, disposition *string
	if err := scan(&out.Kind, &out.ObjectID, &draft, &line, &out.OrderRevision, &out.State, &quantity, &disposition); err != nil {
		return DownstreamCommitment{}, err
	}
	for _, f := range []struct {
		from *string
		to   *string
	}{{draft, &out.DraftID}, {line, &out.OrderLineID}, {quantity, &out.Quantity}, {disposition, &out.Disposition}} {
		if f.from != nil {
			*f.to = *f.from
		}
	}
	return out, nil
}

// InspectionSubject v1 (Context Map R24 PUR→QLT, guardLock false: 受检对象（采购批次、供应商交付）; risk
// 供应商自报合格被当作放行): the lines of purchase commitments QLT inspects (S5 decision D-15), resolved
// before QLT's transaction. A line of an issued or confirmed commitment is a subject (Code ""); one
// of a commitment not issued, or issued but since then voided, has Code INVALID_STATE.

// CommitmentLineRef names one line of a commitment: the order line its draft buys.
type CommitmentLineRef struct {
	CommitmentID string
	OrderLineID  string
}

// CommitmentLine is a commitment line as InspectionSubject v1 shows it: the commitment's status,
// version and purchase order number, the draft at its hash, the order at its revision and hash, the
// supplier, the order line's SKU specification revision and unit, the quantity allocated to the
// draft and the quantity bought (allocated plus overbuy), the supplier's delivery date, and Code ""
// when the line is an inspection subject now.
type CommitmentLine struct {
	Ref               CommitmentLineRef
	Status            string
	RowVersion        int64
	PONumber          string
	DraftID           string
	DraftHash         string
	OrderID           string
	OrderRevision     int64
	OrderHash         string
	SupplierID        string
	SkuID             string
	SpecRevision      int64
	UnitID            string
	UnitRevision      int64
	AllocatedQuantity string
	PurchaseQuantity  string
	SupplierDate      string
	Code              string
}

// Check returns nil for a line that is an inspection subject now and INVALID_STATE at path otherwise.
func (l CommitmentLine) Check(path string) error {
	if l.Code == "" {
		return nil
	}
	return kernel.Reject(kernel.CodeInvalidState, path, "The purchase commitment is "+l.Status+", not issued")
}

// MaxCommitmentLineRefs bounds one InspectionSubject v1 lookup.
const MaxCommitmentLineRefs = 200

// InspectionSubjects answer InspectionSubject v1 lookups of QLT (R24). Like the other lookups they
// need no permission beyond an active grant; commitments outside the caller's data scope are absent,
// and so are refs of lines a commitment does not have. Ask before opening your own transaction.
type InspectionSubjects interface {
	// ResolveCommitmentLines answers at most MaxCommitmentLineRefs refs.
	ResolveCommitmentLines(ctx context.Context, refs []CommitmentLineRef) (map[CommitmentLineRef]CommitmentLine, error)
}

// ValidateCommitmentLineRefs checks a lookup's refs.
func ValidateCommitmentLineRefs(refs []CommitmentLineRef) error {
	if len(refs) > MaxCommitmentLineRefs {
		return schema("/refs")
	}
	for i, r := range refs {
		p := index("/refs", i)
		switch {
		case !kernel.ValidIdentifier(r.CommitmentID):
			return schema(p + "/commitment_id")
		case !kernel.ValidIdentifier(r.OrderLineID):
			return schema(p + "/order_line_id")
		}
	}
	return nil
}
