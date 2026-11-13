package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// GoalReadiness v1 (Context Map R18 TRD→TSK, Open Host Service / Published Language; WBS S4-01c;
// product baseline 15.4 完成断言): whether a quote root achieves a Q01 goal now, judged on TRD's
// authoritative records and never on what a model says (R18's risk; AI-INV-004).
//
// A goal is quote_ready (an effective formal quote with its document) or order_active (a contract
// formed on such a quote and the root's one full_v1 order active). The assessment returns, for every
// assertion the goal requires, satisfied, unsatisfied, unknown or error with the checks that decided
// it and the evidence it rests on (IDs, versions, hashes), and the instant it was judged. Only when
// every required assertion is satisfied is the goal achieved (Outcome satisfied); missing data and
// an unavailable check are never a pass (REQUIRED_CHECK_UNKNOWN). The assertions per goal (baseline
// 15.4):
//
//	assertion       quote_ready                                         order_active
//	Q01.scope       the target customer (and RFQ) is the revision's     same; the acceptance and the order are the root's
//	Q01.quote       the revision is the root's active, unheld, unexpired the contract formed on the revision at its hash
//	                one with its commit evidence, and the root's full_v1 (the acceptance arrived in time); its later
//	                slot is free (conversion: CONVERSION_SLOT_CONFLICT expiry does not matter (TRD-INV-017)
//	                names the order holding it)
//	Q01.lines       the upstream facts the revision binds still serve     the order's snapshot is the sealed commercial
//	                (CST, INQ, POL, PTY guards), the cost scope and      document of the revision, copied whole, and its
//	                sales terms bound again equal the frozen ones, the    current revision is a sound successor (no silent
//	                sales exchange rate is not too old                    change, removal or recalculation)
//	Q01.approval    the approval the commit evidence names still         the review whose approval activated the order is
//	                releases the revision (revocation, rules, reviewer    approved and not revoked (ORDER_APPROVAL_REVOKED)
//	                qualification)
//	Q01.document    the formal document's stored bytes are the bound      same, and the order binds that document
//	                ones for the revision's business hash
//	Q01.industry    CAT's SkuGuard on every SKU specification revision    same, on the order's current revision's lines
//	                the revision binds (技术资料; samples are not modelled)
//	Q01.acceptance  — (not a prerequisite; not claimed)                   the acceptance is valid and formed the contract on
//	                                                                       the revision at its hash
//	Q01.order       — (not a prerequisite)                                the root's full_v1 slot holds an order
//	                                                                       (ORDER_MISSING otherwise) that is active
//	                                                                       (ORDER_NOT_ACTIVE: a draft does not count)
//
// Q01.execution (no unsettled side effect) is TSK's own assertion: TRD does not judge it.
//
// The query is an audited read (goal.readiness, permission trd.read, any actor kind: TSK's system
// actor verifies a goal, an AI agent or a human reads it). It authorizes the caller now and refuses
// a quote outside the caller's data scope or company as NOT_FOUND (X-INV-003), both audited; a
// refusal of the read itself means Q01.scope is not satisfied for that caller. It is not fenced by
// the root's object control: a goal stays readable while the root is bound, stopping or under
// manual control. It writes nothing but its audit entry. Like QuoteReady it reads the upstream
// contexts' inputs before its transaction and calls their guard functions inside it (ADR-BR-004
// option B).

// QueryGoalReadiness is the audited read name of GoalReadiness (trd.read).
const QueryGoalReadiness = "goal.readiness"

// Goals GoalReadiness v1 judges (baseline 15.4; TSK's goal codes) and the version of their
// assertion sets.
const (
	GoalQuoteReady  = "quote_ready"
	GoalOrderActive = "order_active"
	GoalVersion     = 1
)

// Assertions (baseline 15.4).
const (
	AssertionScope      = "Q01.scope"
	AssertionQuote      = "Q01.quote"
	AssertionLines      = "Q01.lines"
	AssertionApproval   = "Q01.approval"
	AssertionDocument   = "Q01.document"
	AssertionIndustry   = "Q01.industry"
	AssertionAcceptance = "Q01.acceptance"
	AssertionOrder      = "Q01.order"
)

// Answers of a check, an assertion and a goal (baseline 15.4).
const (
	AnswerSatisfied   = "satisfied"
	AnswerUnsatisfied = "unsatisfied"
	AnswerUnknown     = "unknown"
	AnswerError       = "error"
)

// Checks of GoalReadiness besides those of QuoteReadiness (Check* in queries.go).
const (
	CheckRfq           = "rfq"            // the target RFQ is the one the revision answers
	CheckConversion    = "conversion"     // the root's full_v1 conversion slot (quote_ready: free; order_active: held by the order)
	CheckIndustry      = "industry"       // CAT's SkuGuard on the bound SKU specification revisions
	CheckAcceptance    = "acceptance"     // the acceptance stands for a formed contract on the revision
	CheckOrder         = "order"          // the order's existence and state
	CheckOrderSnapshot = "order_snapshot" // the order's snapshot and current revision are intact and the revision's
	CheckOrderApproval = "order_approval" // the approval that activated the order
)

// Kinds of evidence an assertion rests on.
const (
	EvidenceQuoteRoot      = "quote_root"      // ID quote, Version root version
	EvidenceQuoteRevision  = "quote_revision"  // ID quote, Version business revision, Hash business hash
	EvidenceCustomer       = "customer"        // ID party, Version party revision
	EvidenceRfq            = "rfq"             // ID RFQ, Version row version
	EvidenceCommit         = "commit_evidence" // ID the formalization's idempotency key, Version business revision, Hash entries SHA-256
	EvidenceConversionSlot = "conversion_slot" // ID the order holding it, Version business revision it was taken for, Hash business hash
	EvidenceCostSnapshot   = "cost_snapshot"   // ID snapshot, Hash result hash
	EvidenceApproval       = "approval"        // ID approval, Version effectivity version
	EvidenceDocument       = "formal_document" // ID formal number, Version business revision, Hash document SHA-256
	EvidenceSkuSpec        = "sku_spec"        // ID SKU, Version specification revision
	EvidenceAcceptance     = "acceptance"      // ID acceptance, Version effectivity version, Hash the business hash it accepts
	EvidenceOrder          = "order"           // ID order, Version order revision, Hash order hash
	EvidenceOrderReview    = "order_review"    // ID review, Version order revision it approved, Hash order hash it approved
)

// GoalQuery asks whether quote root QuoteID achieves goal Goal at version GoalVersion now
// {goal, goal_version, quote_id, business_revision, customer_id, rfq_id}. BusinessRevision, when not
// 0, is the revision the goal was set for (quote_ready: it must be the active one; order_active: the
// order must convert it); CustomerID and RfqID, when given, are the task's target customer and RFQ,
// which the judged revision must bind (Q01.scope).
type GoalQuery struct {
	Goal             string
	GoalVersion      int64
	QuoteID          string
	BusinessRevision int64
	CustomerID       string
	RfqID            string
}

// Validate checks the closed contract: a goal of GoalReadiness v1 at version 1, identifiers where
// identifiers are named.
func (q GoalQuery) Validate() error {
	switch {
	case q.Goal != GoalQuoteReady && q.Goal != GoalOrderActive:
		return schema("/goal")
	case q.GoalVersion != GoalVersion:
		return schema("/goal_version")
	case !kernel.ValidIdentifier(q.QuoteID):
		return schema("/quote_id")
	case q.BusinessRevision < 0 || q.BusinessRevision > maxVersion:
		return schema("/business_revision")
	case q.CustomerID != "" && !kernel.ValidIdentifier(q.CustomerID):
		return schema("/customer_id")
	case q.RfqID != "" && !kernel.ValidIdentifier(q.RfqID):
		return schema("/rfq_id")
	}
	return nil
}

// EvidenceRef is a record an assertion rests on: its kind (Evidence* constants), ID, version and
// hash ("" and 0 where the kind has none).
type EvidenceRef struct {
	Kind    string
	ID      string
	Version int64
	Hash    string
}

// GoalCheck is one check of an assertion: the check (Check* constants), its answer, and when it is
// not satisfied the code (an error catalog row) and the path it points at (in the revision's
// commercial document "/data/...", in the order "/order/...", or the query's own members).
type GoalCheck struct {
	Check  string
	Answer string
	Code   string
	Path   string
}

// Assertion is the answer for one required assertion: its checks' answers combined as Outcome
// combines answers (satisfied when every check is; else unsatisfied, error, unknown in that order),
// with the code of its first check that gave that answer. Checks lists every check made, in the
// order made; Evidence the records judged.
type Assertion struct {
	Assertion string
	Answer    string
	Code      string
	Checks    []GoalCheck
	Evidence  []EvidenceRef
}

// GoalAssessment is GoalReadiness's answer: the goal and the records judged (the root at its version,
// the revision with its business hash, the order with its revision and hash; zero where there is
// none), every assertion the goal requires in the table's order, the outcome — satisfied when every
// assertion is; otherwise unsatisfied when one is, else error when one is, else unknown — with its
// code (REQUIRED_CHECK_UNSATISFIED for unsatisfied, REQUIRED_CHECK_UNKNOWN for unknown and error,
// "" when satisfied), and the instant judged.
type GoalAssessment struct {
	Goal             string
	GoalVersion      int64
	QuoteID          string
	RootVersion      int64
	BusinessRevision int64
	BusinessHash     string
	OrderID          string
	OrderRevision    int64
	OrderHash        string
	Outcome          string
	Code             string
	Assertions       []Assertion
	CheckedAt        time.Time
}

// Unmet returns the assertions of a that are not satisfied, in order: the items a task waits on.
func (a GoalAssessment) Unmet() []string {
	var out []string
	for _, x := range a.Assertions {
		if x.Answer != AnswerSatisfied {
			out = append(out, x.Assertion)
		}
	}
	return out
}

// Assertion returns the assertion named code, or false.
func (a GoalAssessment) Assertion(code string) (Assertion, bool) {
	for _, x := range a.Assertions {
		if x.Assertion == code {
			return x, true
		}
	}
	return Assertion{}, false
}

// Codes of GoalReadiness (error catalog: 任务与 AI and 订单 rows whose contexts include TRD).
const (
	CodeOrderMissing             = "ORDER_MISSING"              // order_active: no order holds the root's full_v1 slot (Q01 V0.1 code)
	CodeRequiredCheckUnsatisfied = "REQUIRED_CHECK_UNSATISFIED" // a required assertion is not satisfied
	CodeRequiredCheckUnknown     = "REQUIRED_CHECK_UNKNOWN"     // a required assertion lacks data or its check is unavailable; never a pass
)

var goalCodes = map[string]kernel.Category{
	CodeOrderMissing:             kernel.CategoryInvalid,
	CodeRequiredCheckUnsatisfied: kernel.CategoryInvalid,
	CodeRequiredCheckUnknown:     kernel.CategoryInvalid,
}

// GoalCategory returns the category of a code GoalReadiness declares.
func GoalCategory(code string) (kernel.Category, bool) {
	c, ok := goalCodes[code]
	return c, ok
}

// Outcome combines answers (of an assertion's checks, of a goal's assertions): satisfied when every
// one is (and there is at least one); else unsatisfied when one is — someone has to act, waiting
// does not help —, else error when one is, else unknown; with the goal's code for it
// (REQUIRED_CHECK_UNSATISFIED, REQUIRED_CHECK_UNKNOWN for error and unknown, "" for satisfied).
func Outcome(answers ...string) (outcome, code string) {
	seen := map[string]bool{}
	for _, a := range answers {
		seen[a] = true
	}
	switch {
	case len(answers) == 0:
		return AnswerUnknown, CodeRequiredCheckUnknown
	case seen[AnswerUnsatisfied]:
		return AnswerUnsatisfied, CodeRequiredCheckUnsatisfied
	case seen[AnswerError]:
		return AnswerError, CodeRequiredCheckUnknown
	case seen[AnswerUnknown]:
		return AnswerUnknown, CodeRequiredCheckUnknown
	}
	return AnswerSatisfied, ""
}

// GoalQueries is GoalReadiness v1 (Context Map R18), an audited read under trd.read.
type GoalQueries interface {
	// GoalReadiness judges goal q.Goal on quote root q.QuoteID now, audited as goal.readiness. A
	// check that does not hold is an answer, not an error; SCHEMA_INVALID, PERMISSION_DENIED and
	// NOT_FOUND (also for a business revision the root does not have) are refusals.
	GoalReadiness(ctx context.Context, q GoalQuery) (GoalAssessment, error)
}
