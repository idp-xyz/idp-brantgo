// Package acceptance is the Acceptance aggregate of TRD (tactical model TRD-AGG-Acceptance; WBS
// S3-06, S3-13; SM-TRD-ACCEPTANCE; TRD-INV-011, 012, 017; ADR-BR-013; UL TRD-Acceptance,
// TRD-AcceptanceEffectivity, TRD-ContractFormation, TRD-LateAcceptance; Python t03_storage
// register_acceptance and T04 A37 verify_customer_response), identified by (tenant, company,
// acceptance_id): a human-verified, complete and unconditional customer acceptance bound to exactly
// one formal quote revision, its business hash and document number, the customer, the contact and
// the revision's complete line set, and its effectivity ledger:
//
//	valid → invalidated      acceptance.invalidate: a human, at the effectivity version, with a
//	valid → disputed         acceptance.mark_disputed  reason and new evidence
//	disputed → invalidated   acceptance.invalidate
//
// Nothing returns to valid: a new verification creates a new acceptance (UL
// TRD-AcceptanceEffectivity). Every change appends an entry to the ledger; version 1 is the
// verification. A disputed acceptance is invalidated (version 3) by the owner's decision of
// 2026-11-11: a live (valid or disputed) acceptance keeps its revision from being superseded,
// withdrawn or cancelled until a human invalidates it (quote.CheckContract).
//
// Verify creates an acceptance from a full_unconditional customer response in the response's
// transaction (the tactical model's registered creating exception). It judges contract formation
// (ADR-BR-013, TRD-INV-012) by the rule the revision's legal terms imply (WBS S3-17, G0 F-02: 合同
// 成立规则按适用法律选择; quote.Formation, sealed with the revision): under the arrival rule — the
// CISG (Art. 18(2), 23), PRC law (Civil Code Art. 137, 483, 484) or, for a revision without legal
// terms, the design's default of both, and the only rule Brant implements — the acceptance arrived,
// at the response's counting arrival or else at the verification, before the revision's
// expires_at; an acceptance arriving at or after it is late (ACCEPTANCE_LATE) and none is created:
// it is the customer's new offer, answered by a new revision and approval. A revision sealing another
// rule is refused (DEPENDENCY_NOT_SUPPORTED; the freeze never seals one). The verification may come
// after the quote expired. A stored acceptance therefore always records a formed contract and the
// formation it was judged by, and the quote's expiry after formation never ends it (TRD-INV-017):
// Check judges an acceptance by its effectivity and formation, never by a clock.
//
// One valid acceptance per revision; one live (valid or disputed) acceptance per response and per
// customer message (TRD-INV-011 同消息不重复: the message is the response's EVD source and locator).
package acceptance

import (
	"slices"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/quote"
	"go.idp.xyz/idp-brant/internal/contexts/trd/internal/domain/response"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Codes of the error catalog rows this package decides; package api declares the same with their
// categories.
const (
	CodeAcceptanceNotUnconditional    = "ACCEPTANCE_NOT_UNCONDITIONAL"
	CodeAcceptanceScopeMismatch       = "ACCEPTANCE_SCOPE_MISMATCH"
	CodeAcceptanceLinesMismatch       = "ACCEPTANCE_LINES_MISMATCH"
	CodeAcceptanceTimeConflict        = "ACCEPTANCE_TIME_CONFLICT"
	CodeAcceptanceLate                = "ACCEPTANCE_LATE"
	CodeAcceptanceNotValid            = "ACCEPTANCE_NOT_VALID"
	CodeAcceptanceAlreadyExists       = "ACCEPTANCE_ALREADY_EXISTS"
	CodeSourceMessageDuplicate        = "SOURCE_MESSAGE_DUPLICATE"
	CodeResponseVerificationImmutable = "RESPONSE_VERIFICATION_IMMUTABLE"
)

// Categories maps the codes above to their error catalog category.
var Categories = map[string]kernel.Category{
	CodeAcceptanceNotUnconditional:    kernel.CategoryInvalid,
	CodeAcceptanceScopeMismatch:       kernel.CategoryInvalid,
	CodeAcceptanceLinesMismatch:       kernel.CategoryInvalid,
	CodeAcceptanceTimeConflict:        kernel.CategoryInvalid,
	CodeAcceptanceLate:                kernel.CategoryInvalid,
	CodeAcceptanceNotValid:            kernel.CategoryConflict,
	CodeAcceptanceAlreadyExists:       kernel.CategoryConflict,
	CodeSourceMessageDuplicate:        kernel.CategoryConflict,
	CodeResponseVerificationImmutable: kernel.CategoryInvalid,
}

func fail(code, path, reason string) error {
	if c, ok := Categories[code]; ok {
		return kernel.RejectAs(code, c, path, reason)
	}
	if c, ok := quote.Categories[code]; ok {
		return kernel.RejectAs(code, c, path, reason)
	}
	return kernel.Reject(code, path, reason)
}

// Effectivity is the state of an acceptance (SM-TRD-ACCEPTANCE).
type Effectivity string

const (
	Valid       Effectivity = "valid"
	Invalidated Effectivity = "invalidated"
	Disputed    Effectivity = "disputed"
)

// ByVerification is the basis of an arrival the response stated none for: the server's
// verification time (TRD-INV-012: 无法核实到达时间时以服务端核实时间判定).
const ByVerification = "verification"

// Change is one entry of the effectivity ledger: the effectivity from its version on, who set it,
// under which authority epoch, when, why and on which evidence (version 1: the verification, without
// reason or evidence).
type Change struct {
	Version     int64
	Effectivity Effectivity
	Act         quote.Act
	Fresh       bool
}

// Acceptance is the Acceptance aggregate.
type Acceptance struct {
	ID           string
	ResponseID   string
	QuoteID      string
	Revision     int64
	BusinessHash string
	Number       string
	Customer     quote.PartyRef
	Contact      quote.ContactRef
	LineIDs      []string       // sorted
	Message      quote.Evidence // the response's message
	ArrivalAt    time.Time      // when the acceptance arrived: the formation instant
	ArrivalBasis string         // channel_server, import_record or verification
	ExpiresAt    time.Time      // the revision's validity end it was judged against
	Verified     quote.Act      // who verified it, under which authority epoch, when
	Effectivity  Effectivity
	Version      int64 // effectivity version
	History      []*Change
	Fresh        bool // not stored yet
	Changed      bool // stored, its effectivity changed by this command

	// Formation is the formation rule and its basis the revision seals, which judged the acceptance
	// (WBS S3-17; quote.Formation); the zero value is quote.DefaultFormation (FormedBy).
	Formation quote.Formation
}

// FormedBy is the formation rule and basis a was judged by: its Formation, or the default for an
// acceptance that records none (a revision without legal terms).
func (a *Acceptance) FormedBy() quote.Formation {
	if a.Formation == (quote.Formation{}) {
		return quote.DefaultFormation
	}
	return a.Formation
}

// Live reports whether a stands for its response and message: valid or disputed.
func (a *Acceptance) Live() bool { return a.Effectivity != Invalidated }

// Formed reports whether the contract formed under the acceptance's formation rule: under arrival,
// the only rule Brant implements, the acceptance arrived before the revision expired.
func (a *Acceptance) Formed() bool {
	return a.FormedBy().Rule == quote.FormationArrival && a.ArrivalAt.Before(a.ExpiresAt)
}

// Check decides whether a stands for a formed contract on revision revision of quote quoteID at
// businessHash now (TRD-INV-017): ACCEPTANCE_SCOPE_MISMATCH for another quote, revision or hash
// (businessHash "" skips the hash), ACCEPTANCE_NOT_VALID while invalidated or disputed,
// ACCEPTANCE_LATE when it did not form a contract. The quote's expiry is not judged.
func (a *Acceptance) Check(quoteID string, revision int64, businessHash, path string) error {
	switch {
	case a.QuoteID != quoteID || a.Revision != revision || (businessHash != "" && a.BusinessHash != businessHash):
		return fail(CodeAcceptanceScopeMismatch, path, "The acceptance accepts another revision")
	case a.Effectivity != Valid:
		return fail(CodeAcceptanceNotValid, path, "The acceptance is "+string(a.Effectivity))
	case !a.Formed():
		return fail(CodeAcceptanceLate, path, "The acceptance arrived after the quote expired")
	}
	return nil
}

// Code is Check's code for the revision a accepts, "" when it stands.
func (a *Acceptance) Code() string {
	if rej, ok := kernel.AsRejection(a.Check(a.QuoteID, a.Revision, a.BusinessHash, "/")); ok {
		return rej.Code
	}
	return ""
}

// Verification is what quote.response.verify restates of the response it verifies: the acceptance
// ID to create, the revision and its business hash, the customer, the contact and the accepted line
// set.
type Verification struct {
	AcceptanceID string
	Revision     int64
	BusinessHash string
	Customer     quote.PartyRef
	Contact      quote.ContactRef
	LineIDs      []string
}

// Known is what verification knows besides the response and its quote, read under the quote root's
// lock: the quote's acceptances, the acceptance with the requested ID wherever it is, and a live
// acceptance of the response's message wherever it is (nil: none).
type Known struct {
	OfQuote []*Acceptance
	Taken   *Acceptance
	Message *Acceptance
}

// Verify decides quote.response.verify of response r, a response of quote q (locked by the caller),
// as v restates it, at now (Python verify_customer_response and register_acceptance; TRD-INV-011,
// 012): the verifier reads the response's sources (readable; REFERENCE_INVALID); the response is
// full_unconditional (ACCEPTANCE_NOT_UNCONDITIONAL, BC-AT10); a response verified into a live
// acceptance answers with it when v restates exactly it (true) and is
// RESPONSE_VERIFICATION_IMMUTABLE otherwise; the acceptance ID is not taken
// (ACCEPTANCE_ALREADY_EXISTS); v restates the response's revision and hash and the revision's
// customer and contact (ACCEPTANCE_SCOPE_MISMATCH, BC-AT11) and its complete line set
// (ACCEPTANCE_LINES_MISMATCH); the revision is still q's active, unheld revision (quote.Usable);
// its sealed formation is one Brant implements (DEPENDENCY_NOT_SUPPORTED); the acceptance arrived,
// at the response's counting arrival or else now, no later than now (ACCEPTANCE_TIME_CONFLICT) and,
// by the arrival rule, before the revision's expires_at (ACCEPTANCE_LATE, TD-AT03, BC-AT12); the
// message backs no live acceptance (SOURCE_MESSAGE_DUPLICATE) and the revision has no valid one
// (ACCEPTANCE_ALREADY_EXISTS). The caller checked the quote's visibility and object
// control first. The acceptance is valid at effectivity version 1, its ledger's first entry the
// verification.
func Verify(r *response.Response, q *quote.Quote, v Verification, k Known, readable map[string]bool, by kernel.ActorRef, epoch int64,
	now time.Time) (*Acceptance, bool, error) {
	for _, id := range r.Sources() {
		if !readable[id] {
			return nil, false, kernel.Reject(kernel.CodeReferenceInvalid, "/args/response_id", "The verifier does not read the response's sources")
		}
	}
	if r.Classification != response.FullUnconditional {
		return nil, false, fail(CodeAcceptanceNotUnconditional, "/args/response_id", "Only a full_unconditional response is verified")
	}
	lines := slices.Sorted(slices.Values(v.LineIDs))
	for _, a := range k.OfQuote {
		if a.ResponseID == r.ID && a.Live() {
			if a.ID == v.AcceptanceID && a.Revision == v.Revision && a.BusinessHash == v.BusinessHash && a.Customer == v.Customer &&
				a.Contact == v.Contact && slices.Equal(a.LineIDs, lines) {
				return a, true, nil
			}
			return nil, false, fail(CodeResponseVerificationImmutable, "/args/response_id", "The response is verified into acceptance "+a.ID)
		}
	}
	if k.Taken != nil {
		return nil, false, fail(CodeAcceptanceAlreadyExists, "/args/acceptance_id", "The acceptance ID is taken")
	}
	rev := q.Revision(r.Revision)
	switch {
	case v.Revision != r.Revision:
		return nil, false, fail(CodeAcceptanceScopeMismatch, "/args/business_revision", "The response answers another revision")
	case v.BusinessHash != r.BusinessHash:
		return nil, false, fail(CodeAcceptanceScopeMismatch, "/args/business_hash", "The response answers another business hash")
	case rev == nil || rev.Seal == nil || rev.Formal == nil:
		return nil, false, kernel.Reject(kernel.CodeReferenceInvalid, "/args/business_revision", "The revision has no formal document")
	case rev.Data.Customer == nil || *rev.Data.Customer != v.Customer:
		return nil, false, fail(CodeAcceptanceScopeMismatch, "/args/customer_ref", "The revision quotes another customer")
	case rev.Data.Contact == nil || *rev.Data.Contact != v.Contact:
		return nil, false, fail(CodeAcceptanceScopeMismatch, "/args/contact_ref", "The revision quotes another contact")
	}
	quoted := make([]string, 0, len(rev.Data.Lines))
	for _, l := range rev.Data.Lines {
		quoted = append(quoted, l.LineID)
	}
	if !slices.Equal(lines, slices.Sorted(slices.Values(quoted))) {
		return nil, false, fail(CodeAcceptanceLinesMismatch, "/args/line_ids", "An acceptance accepts the revision's complete line set")
	}
	if err := q.Usable(r.Revision, r.BusinessHash, "/args/business_revision"); err != nil {
		return nil, false, err
	}
	formation, err := rev.Seal.Formation()
	if err != nil {
		return nil, false, err
	}
	if err = formation.Check("/args/business_revision"); err != nil {
		return nil, false, err
	}
	// The arrival rule: the contract formed when the acceptance reached us, if that was before the
	// revision expired.
	at := kernel.Instant(now)
	arrival, basis := at, ByVerification
	if c := r.CountingArrival(); c != nil {
		arrival, basis = c.At, string(c.Basis)
	}
	expires := rev.Seal.ExpiresAt
	switch {
	case arrival.After(at):
		return nil, false, fail(CodeAcceptanceTimeConflict, "/args/response_id", "The message arrived after its verification")
	case !arrival.Before(expires):
		return nil, false, fail(CodeAcceptanceLate, "/args/response_id",
			"The acceptance arrived when the quote had expired: a late acceptance is the customer's new offer")
	}
	if k.Message != nil {
		return nil, false, fail(CodeSourceMessageDuplicate, "/args/response_id", "The customer's message backs acceptance "+k.Message.ID)
	}
	for _, a := range k.OfQuote {
		if a.Revision == r.Revision && a.Effectivity == Valid {
			return nil, false, fail(CodeAcceptanceAlreadyExists, "/args/business_revision", "The revision has valid acceptance "+a.ID)
		}
	}
	act := quote.Act{By: by, Epoch: epoch, At: at}
	a := &Acceptance{ID: v.AcceptanceID, ResponseID: r.ID, QuoteID: q.ID, Revision: r.Revision, BusinessHash: r.BusinessHash, Number: r.Number,
		Customer: v.Customer, Contact: v.Contact, LineIDs: lines, Message: r.Message, ArrivalAt: arrival, ArrivalBasis: basis,
		ExpiresAt: expires, Formation: formation, Verified: act, Effectivity: Valid, Version: 1, Fresh: true}
	a.History = []*Change{{Version: 1, Effectivity: Valid, Act: act, Fresh: true}}
	return a, false, nil
}

// Invalidate invalidates a at its effectivity version expected (acceptance.invalidate), by act
// (who, epoch, when, reason, evidence): a valid acceptance, or a disputed one (the owner's decision
// of 2026-11-11: a live acceptance keeps its revision from being superseded, withdrawn or cancelled
// until a human invalidates it, so a dispute ends in invalidation too).
func (a *Acceptance) Invalidate(expected int64, act quote.Act) error {
	return a.change(Invalidated, expected, act, Valid, Disputed)
}

// MarkDisputed marks a valid acceptance disputed at its effectivity version expected
// (acceptance.mark_disputed).
func (a *Acceptance) MarkDisputed(expected int64, act quote.Act) error {
	return a.change(Disputed, expected, act, Valid)
}

// change moves an acceptance in one of the effectivities from to effectivity to: VERSION_CONFLICT at
// another effectivity version, INVALID_STATE otherwise (nothing returns to valid, an invalidated
// acceptance never changes again, a disputed one is only invalidated).
func (a *Acceptance) change(to Effectivity, expected int64, act quote.Act, from ...Effectivity) error {
	if a.Version != expected {
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_effectivity_version", "")
	}
	if !slices.Contains(from, a.Effectivity) {
		return kernel.Reject(kernel.CodeInvalidState, "/args/acceptance_id", "The acceptance is "+string(a.Effectivity))
	}
	act.At = kernel.Instant(act.At)
	act.Evidence = slices.Clone(act.Evidence)
	a.Version++
	a.Effectivity, a.Changed = to, true
	a.History = append(a.History, &Change{Version: a.Version, Effectivity: to, Act: act, Fresh: true})
	return nil
}
