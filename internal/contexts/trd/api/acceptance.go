package api

import (
	"context"
	"slices"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Customer responses, acceptance and contract formation (WBS S3-06, the formation half of S3-13;
// tactical model TRD-AGG-CustomerResponse, TRD-AGG-Acceptance, SM-TRD-ACCEPTANCE; TRD-INV-011, 012,
// 017; ADR-BR-013; Python t04_response, t03_storage record_customer_response,
// verify_customer_response and register_acceptance).
//
//	quote.response.record     a human or an AI agent records the customer's reply to an active
//	                          formal revision: bound to the revision, its business hash and its
//	                          formal document's number; classified full_unconditional, conditional
//	                          (a counter-offer, such as a PO that differs from the revision),
//	                          partial, reject or ambiguous; with the customer's message
//	                          (source_ref, an EVD source and the place in it), a summary, optional
//	                          further evidence and customer PO number, and the time the message
//	                          arrived with its basis. A classification is an observation: it creates
//	                          no acceptance and no order. Responses are append only; the same
//	                          response_id with the same content answers from the stored response,
//	                          with other content RESPONSE_IMMUTABLE.
//	quote.response.verify     a human verifies a recorded full_unconditional response into an
//	                          Acceptance, restating the revision, its business hash, the customer,
//	                          the contact and the complete line set it accepts.
//	acceptance.invalidate     valid or disputed → invalidated, human, at expected_effectivity_version,
//	acceptance.mark_disputed  valid → disputed     with a reason and new evidence. Nothing returns to
//	                          valid: a new verification creates a new acceptance. A live (valid or
//	                          disputed) acceptance keeps its revision from being replaced, withdrawn
//	                          or cancelled (ACCEPTANCE_ALREADY_EXISTS; the owner's decision of
//	                          2026-11-11) until a human invalidates it — a dispute too.
//
// The revision a response answers must be the quote's active formal revision at the stated hash,
// not paused by a risk hold, when the response is recorded and when it is verified (REFERENCE_INVALID
// for a revision never formalized, BUSINESS_HASH_MISMATCH, QUOTE_NOT_CURRENT for a superseded,
// cancelled or otherwise inactive one, the hold's code APPROVAL_REVOKED). Every write passes the
// quote root's object control, like a quote write, and locks the quote root FOR UPDATE first: the
// response, its acceptances and their effectivity live under the root (tactical lockOrder: the root,
// then AcceptanceEffectivity). The actor reads the customer's message and every evidence source
// (REFERENCE_INVALID otherwise); the verifier reads the response's.
//
// Arrival (TRD-INV-012, UL TRD-AcceptanceReceivedAt): received_at is when the message reached us, as
// received_at_basis says: channel_server (the receiving channel server's time, located in the
// message) or import_record (the time a trusted import recorded it) can be verified and count;
// source_claimed (the sender's own claim, such as an email's Date header) is kept but does not
// count. A counting arrival lies between the revision's formalization and the recording
// (ACCEPTANCE_TIME_CONFLICT otherwise). The arrival that judges the acceptance is the counting one,
// or else the server's verification time (arrival_basis verification).
//
// Contract formation (ADR-BR-013, TRD-INV-012, UL TRD-ContractFormation): the contract forms when
// the acceptance arrives before the revision's expires_at; quote.response.verify judges it, and the
// verification itself may come after the quote expired. An acceptance that arrived at or after
// expires_at is late (UL TRD-LateAcceptance): verify refuses it with ACCEPTANCE_LATE and creates no
// acceptance; the response stays recorded and reads show it late. The design gives late acceptance
// no confirmation path: it is the customer's new offer, answered by a new quote revision and a new
// approval (TRD-INV-012). Every stored acceptance therefore records a formed contract: its arrival
// time and basis and the expires_at it was judged against. The quote expiring after formation never
// blocks the acceptance's later use (TRD-INV-017): steps relying on it check that it is valid and
// formed (AcceptanceFact.Check, AcceptanceGuard), never that the clock is before expires_at.
//
// Verification refuses, in this order: a reader of the response's sources it is not
// (REFERENCE_INVALID); a classification other than full_unconditional (ACCEPTANCE_NOT_UNCONDITIONAL,
// BC-AT10); a response whose live (valid or disputed) acceptance is another one
// (RESPONSE_VERIFICATION_IMMUTABLE; the same one answers from it); an acceptance ID taken
// (ACCEPTANCE_ALREADY_EXISTS); a restated revision, hash, customer or contact that is not the
// response's and the revision's (ACCEPTANCE_SCOPE_MISMATCH, BC-AT11); a line set that is not the
// revision's complete one (ACCEPTANCE_LINES_MISMATCH); a revision no longer usable; an arrival after
// the verification (ACCEPTANCE_TIME_CONFLICT) or not before expires_at (ACCEPTANCE_LATE, TD-AT03,
// BC-AT12); a message backing a live acceptance (SOURCE_MESSAGE_DUPLICATE, TRD-INV-011: the message
// is source_ref {source_id, locator}); and a revision with a valid acceptance already
// (ACCEPTANCE_ALREADY_EXISTS: one valid acceptance per revision). A verifier need not differ from the
// recorder: the design names no independence for quote.response.verify (role templates
// independence, Python T04 A37); it is human only.
//
// Request members in snake_case: quote.response.record {response_id, quote_id, business_revision,
// business_hash, classification, summary, source_ref {source_id, locator}, received_at,
// received_at_basis, customer_po_number, evidence}; quote.response.verify {response_id,
// acceptance_id, business_revision, business_hash, customer_ref {party_id, revision}, contact_ref
// {contact_id, row_version}, line_ids}; acceptance.invalidate and acceptance.mark_disputed
// {acceptance_id, expected_effectivity_version, reason, evidence}.
const (
	CommandRecordResponse       = "quote.response.record"
	CommandVerifyResponse       = "quote.response.verify"
	CommandInvalidateAcceptance = "acceptance.invalidate"
	CommandDisputeAcceptance    = "acceptance.mark_disputed"

	EventCustomerResponseRecorded = "idp-brant.trd.customer_response_recorded.v1"
	EventAcceptanceVerified       = "idp-brant.trd.acceptance_verified.v1"
	EventAcceptanceInvalidated    = "idp-brant.trd.acceptance_invalidated.v1"
	// EventAcceptanceDisputed is the tactical model's AcceptanceDisputed; the event catalog V0.1 and
	// the Context Map's facts do not list it yet.
	EventAcceptanceDisputed = "idp-brant.trd.acceptance_disputed.v1"

	// Classifications of a customer response (Python T04 A36; ES-05).
	ClassificationFullUnconditional = "full_unconditional"
	ClassificationConditional       = "conditional"
	ClassificationPartial           = "partial"
	ClassificationReject            = "reject"
	ClassificationAmbiguous         = "ambiguous"

	// Bases of an arrival time. ArrivalVerification is an acceptance's basis when its response
	// stated no counting arrival.
	ArrivalChannelServer = "channel_server"
	ArrivalImportRecord  = "import_record"
	ArrivalSourceClaimed = "source_claimed"
	ArrivalVerification  = "verification"

	// StatusRecorded is the receipt status of quote.response.record.
	StatusRecorded = "recorded"

	// Effectivity of an acceptance (SM-TRD-ACCEPTANCE).
	AcceptanceValid       = "valid"
	AcceptanceInvalidated = "invalidated"
	AcceptanceDisputed    = "disputed"

	MaxSummary = 4000
	maxLocator = 500
	maxPO      = 64
)

var (
	classifications = []string{ClassificationFullUnconditional, ClassificationConditional, ClassificationPartial, ClassificationReject,
		ClassificationAmbiguous}
	statedBases = []string{ArrivalChannelServer, ArrivalImportRecord, ArrivalSourceClaimed}
)

// RecordResponse records customer response ResponseID to formal revision BusinessRevision of quote
// QuoteID at BusinessHash (Python t04_record_customer_response). SourceRef is the customer's
// message; ReceivedAt (a timestamp, "" when unknown) is when it arrived, on ReceivedAtBasis
// (channel_server, import_record or source_claimed; "" exactly when ReceivedAt is). Evidence is
// optional further evidence; CustomerPONumber the customer's PO number, when the reply is one (UL
// TRD-CustomerPO). Human or AI.
type RecordResponse struct {
	IdempotencyKey   string
	ResponseID       string
	QuoteID          string
	BusinessRevision int64
	BusinessHash     string
	Classification   string
	Summary          string
	SourceRef        Evidence
	ReceivedAt       string
	ReceivedAtBasis  string
	CustomerPONumber string
	Evidence         []Evidence
}

// VerifyResponse verifies recorded response ResponseID into acceptance AcceptanceID (Python
// t04_verify_customer_response), restating what the verifier checked against the customer's
// message: the revision and its business hash, the customer at its revision, the contact at its row
// version, and the complete set of line IDs accepted. Human only.
type VerifyResponse struct {
	IdempotencyKey   string
	ResponseID       string
	AcceptanceID     string
	BusinessRevision int64
	BusinessHash     string
	CustomerRef      PartyRef
	ContactRef       ContactRef
	LineIDs          []string
}

// InvalidateAcceptance invalidates acceptance AcceptanceID at its effectivity version, with a reason
// and new evidence (SM-TRD-ACCEPTANCE). Human only.
type InvalidateAcceptance struct {
	IdempotencyKey             string
	AcceptanceID               string
	ExpectedEffectivityVersion int64
	Reason                     string
	Evidence                   []Evidence
}

// DisputeAcceptance marks acceptance AcceptanceID disputed at its effectivity version, with a reason
// and new evidence (SM-TRD-ACCEPTANCE). Human only.
type DisputeAcceptance struct {
	IdempotencyKey             string
	AcceptanceID               string
	ExpectedEffectivityVersion int64
	Reason                     string
	Evidence                   []Evidence
}

// Validate checks the closed contract.
func (c RecordResponse) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.ResponseID):
		return schema("/args/response_id")
	case !kernel.ValidIdentifier(c.QuoteID):
		return schema("/args/quote_id")
	}
	if err := positive(c.BusinessRevision, "/args/business_revision"); err != nil {
		return err
	}
	switch {
	case !hash64.MatchString(c.BusinessHash):
		return schema("/args/business_hash")
	case !slices.Contains(classifications, c.Classification):
		return schema("/args/classification")
	case !text(c.Summary, MaxSummary):
		return schema("/args/summary")
	case !kernel.ValidIdentifier(c.SourceRef.SourceID):
		return schema("/args/source_ref/source_id")
	case !text(c.SourceRef.Locator, maxLocator):
		return schema("/args/source_ref/locator")
	}
	if c.ReceivedAt != "" {
		if _, err := kernel.ParseTimestamp(c.ReceivedAt); err != nil {
			return schema("/args/received_at")
		}
	}
	if (c.ReceivedAt == "") != (c.ReceivedAtBasis == "") || (c.ReceivedAtBasis != "" && !slices.Contains(statedBases, c.ReceivedAtBasis)) {
		return schema("/args/received_at_basis")
	}
	if c.CustomerPONumber != "" && !text(c.CustomerPONumber, maxPO) {
		return schema("/args/customer_po_number")
	}
	if len(c.Evidence) > 0 {
		return evidence(c.Evidence, "/args/evidence")
	}
	return nil
}

// Validate checks the closed contract: 1–MaxLines distinct line IDs.
func (c VerifyResponse) Validate() error {
	switch {
	case !kernel.ValidIdentifier(c.IdempotencyKey):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(c.ResponseID):
		return schema("/args/response_id")
	case !kernel.ValidIdentifier(c.AcceptanceID):
		return schema("/args/acceptance_id")
	}
	if err := positive(c.BusinessRevision, "/args/business_revision"); err != nil {
		return err
	}
	if !hash64.MatchString(c.BusinessHash) {
		return schema("/args/business_hash")
	}
	if err := idVersion(c.CustomerRef.PartyID, c.CustomerRef.Revision, "/args/customer_ref", "/party_id", "/revision"); err != nil {
		return err
	}
	if err := idVersion(c.ContactRef.ContactID, c.ContactRef.RowVersion, "/args/contact_ref", "/contact_id", "/row_version"); err != nil {
		return err
	}
	if len(c.LineIDs) < 1 || len(c.LineIDs) > MaxLines {
		return schema("/args/line_ids")
	}
	seen := map[string]bool{}
	for i, id := range c.LineIDs {
		if !kernel.ValidIdentifier(id) || seen[id] {
			return schema(index("/args/line_ids", i))
		}
		seen[id] = true
	}
	return nil
}

// Validate checks the closed contract.
func (c InvalidateAcceptance) Validate() error {
	return effectivityChange(c.IdempotencyKey, c.AcceptanceID, c.ExpectedEffectivityVersion, c.Reason, c.Evidence)
}

// Validate checks the closed contract.
func (c DisputeAcceptance) Validate() error {
	return effectivityChange(c.IdempotencyKey, c.AcceptanceID, c.ExpectedEffectivityVersion, c.Reason, c.Evidence)
}

func effectivityChange(key, id string, expected int64, reason string, proofs []Evidence) error {
	switch {
	case !kernel.ValidIdentifier(key):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(id):
		return schema("/args/acceptance_id")
	}
	if err := positive(expected, "/args/expected_effectivity_version"); err != nil {
		return err
	}
	if !text(reason, maxText) {
		return schema("/args/reason")
	}
	return evidence(proofs, "/args/evidence")
}

// ResponseReceipt is the answer of quote.response.record: the receipt (ObjectID the response ID,
// RowVersion 1, Status recorded; Replayed also when the same response was recorded under another
// key) and what the response binds. Late says that no acceptance can form from it: its counting
// arrival, or else its recording, is not before the revision's expires_at (TD-AT03: a late reply is
// recorded, never verified). A replay rebuilds it from the stored response.
type ResponseReceipt struct {
	Receipt
	QuoteID          string
	BusinessRevision int64
	BusinessHash     string
	Number           string
	Classification   string
	ExpiresAt        time.Time
	RecordedAt       time.Time
	Late             bool
}

// AcceptanceReceipt is the answer of quote.response.verify, acceptance.invalidate and
// acceptance.mark_disputed: the receipt (ObjectID the acceptance ID, RowVersion its effectivity
// version, Status its effectivity; Replayed also when quote.response.verify found the response
// verified into the same acceptance under another key) and the acceptance's formation. A replay
// rebuilds it from the stored acceptance.
type AcceptanceReceipt struct {
	Receipt
	ResponseID       string
	QuoteID          string
	BusinessRevision int64
	BusinessHash     string
	Number           string
	ArrivalAt        time.Time
	ArrivalBasis     string
	ExpiresAt        time.Time
	Formed           bool
}

// CustomerResponseView is a recorded response with the acceptances verified from it, oldest first.
// ReceivedAt is zero when no arrival was stated. Late is as ResponseReceipt's, now: true when the
// counting arrival is not before ExpiresAt, or, without one, when no acceptance formed from it and
// the recording or now is not before ExpiresAt.
type CustomerResponseView struct {
	ResponseID       string
	QuoteID          string
	BusinessRevision int64
	BusinessHash     string
	Number           string
	ExpiresAt        time.Time
	Classification   string
	Summary          string
	SourceRef        Evidence
	Evidence         []Evidence
	ReceivedAt       time.Time
	ReceivedAtBasis  string
	CustomerPONumber string
	RecordedBy       kernel.ActorRef
	RecordedAt       time.Time
	Late             bool
	Acceptances      []AcceptanceView
}

// AcceptanceView is an acceptance: what it binds, its formation (ArrivalAt on ArrivalBasis, before
// ExpiresAt), who verified it, and its effectivity with every change, version 1 (valid, at the
// verification) first.
type AcceptanceView struct {
	AcceptanceID       string
	ResponseID         string
	QuoteID            string
	BusinessRevision   int64
	BusinessHash       string
	Number             string
	CustomerRef        PartyRef
	ContactRef         ContactRef
	LineIDs            []string
	SourceRef          Evidence
	ArrivalAt          time.Time
	ArrivalBasis       string
	ExpiresAt          time.Time
	Formed             bool
	VerifiedBy         kernel.ActorRef
	VerifiedAt         time.Time
	Effectivity        string
	EffectivityVersion int64
	History            []EffectivityView
}

// EffectivityView is one entry of an acceptance's effectivity ledger.
type EffectivityView struct {
	EffectivityVersion int64
	Effectivity        string
	ChangedBy          kernel.ActorRef
	ChangedAt          time.Time
	Reason             string     // invalidated and disputed
	Evidence           []Evidence // invalidated and disputed
}

// AcceptanceQueries is the audited read side of responses and acceptances.
type AcceptanceQueries interface {
	// GetCustomerResponse returns a response of a quote the caller sees, with its acceptances,
	// audited as quote.response.get (trd.read): NOT_FOUND otherwise, REFERENCE_INVALID when the
	// caller does not read the customer's message or an evidence source (Python T04 A45).
	GetCustomerResponse(ctx context.Context, responseID string) (CustomerResponseView, error)
	// GetAcceptance returns an acceptance of a quote the caller sees with its effectivity ledger,
	// audited as acceptance.get (trd.read; Python acceptance_history).
	GetAcceptance(ctx context.Context, acceptanceID string) (AcceptanceView, error)
}

// AcceptanceCommands is the command entry point of customer responses and acceptance. Errors are as
// Commands describes them.
type AcceptanceCommands interface {
	RecordResponse(ctx context.Context, cmd RecordResponse) (ResponseReceipt, error)
	VerifyResponse(ctx context.Context, cmd VerifyResponse) (AcceptanceReceipt, error)
	InvalidateAcceptance(ctx context.Context, cmd InvalidateAcceptance) (AcceptanceReceipt, error)
	DisputeAcceptance(ctx context.Context, cmd DisputeAcceptance) (AcceptanceReceipt, error)
}

func (RecordResponse) trdCommand()       {}
func (VerifyResponse) trdCommand()       {}
func (InvalidateAcceptance) trdCommand() {}
func (DisputeAcceptance) trdCommand()    {}

// AcceptanceFact is what TRD says of an acceptance now, for the steps that rely on a formed contract
// (TRD's order step; contexts the Context Map makes TRD upstream of): what it binds (the revision,
// its business hash and document number, the customer, the contact and the accepted lines), its
// effectivity at its version, its formation (ArrivalAt on ArrivalBasis before ExpiresAt), who
// verified it, and Code, the first reason it does not stand for a formed contract now (""
// when it does; ACCEPTANCE_NOT_VALID while invalidated or disputed). An order binds AcceptanceID at
// EffectivityVersion, ArrivalAt and the formation; it never compares its own clock with ExpiresAt.
type AcceptanceFact struct {
	AcceptanceID       string
	ResponseID         string
	Ref                QuoteRevisionRef
	BusinessHash       string
	Number             string
	CustomerRef        PartyRef
	ContactRef         ContactRef
	LineIDs            []string
	Effectivity        string
	EffectivityVersion int64
	ArrivalAt          time.Time
	ArrivalBasis       string
	ExpiresAt          time.Time
	Formed             bool
	VerifiedBy         kernel.ActorRef
	VerifiedAt         time.Time
	Code               string
}

// Check returns nil when the acceptance stands for a formed contract now, and its refusal at path
// otherwise (ACCEPTANCE_NOT_VALID, ACCEPTANCE_LATE).
func (f AcceptanceFact) Check(path string) error {
	if f.Code == "" {
		return nil
	}
	return refuse(f.Code, path)
}

// CheckFor is Check for a step bound to revision b: ACCEPTANCE_SCOPE_MISMATCH when the acceptance
// accepts another quote, revision or business hash.
func (f AcceptanceFact) CheckFor(b QuoteRevisionBinding, path string) error {
	if f.Ref.QuoteID != b.QuoteID || f.Ref.BusinessRevision != b.BusinessRevision || f.BusinessHash != b.BusinessHash {
		return Reject(CodeAcceptanceScopeMismatch, path)
	}
	return f.Check(path)
}

// MaxAcceptanceRefs bounds one acceptance lookup.
const MaxAcceptanceRefs = 200

// AcceptanceLookups answer acceptance lookups of the steps relying on a formed contract. Like
// Lookups, they need no permission beyond an active grant; acceptances of quotes outside the
// caller's data scope are absent. Ask before opening your own transaction; inside it, use
// AcceptanceGuard (acceptance_guard.go).
type AcceptanceLookups interface {
	// ResolveAcceptances answers at most MaxAcceptanceRefs acceptance IDs.
	ResolveAcceptances(ctx context.Context, ids []string) (map[string]AcceptanceFact, error)
	// ResolveRevisionAcceptances returns each revision's current acceptance: its valid one, or
	// else the one verified last (invalidated or disputed); revisions never accepted are absent.
	ResolveRevisionAcceptances(ctx context.Context, refs []QuoteRevisionRef) (map[QuoteRevisionRef]AcceptanceFact, error)
}

// ValidateAcceptanceIDs checks a lookup's acceptance IDs.
func ValidateAcceptanceIDs(ids []string) error {
	if len(ids) > MaxAcceptanceRefs {
		return schema("/acceptance_ids")
	}
	for i, id := range ids {
		if !kernel.ValidIdentifier(id) {
			return schema(index("/acceptance_ids", i))
		}
	}
	return nil
}
