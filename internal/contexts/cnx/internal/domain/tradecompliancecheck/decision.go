package tradecompliancecheck

import (
	"slices"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// RegulationRef is a screening_regulation revision as a decision or a reopening binds it.
type RegulationRef struct {
	ID          string
	Revision    int64
	ContentHash string
}

// Decision is a human compliance conclusion on one evaluation (trade_compliance.decide,
// CNX-INV-001): clear or blocked with the reason, the evidence, whether the goods need an export
// licence (clear only), the regulation revision it follows and, for clear, how long it stays valid.
// Decisions are only appended.
type Decision struct {
	ID              string
	CheckID         string
	EvaluationID    string
	Conclusion      Status
	Reason          string
	LicenseRequired bool
	Regulation      RegulationRef
	ValidUntil      time.Time // zero for blocked: it never lapses
	Evidence        []Evidence
	By              kernel.ActorRef
	Epoch           int64
	At              time.Time
}

// DecideInput is what trade_compliance.decide asks.
type DecideInput struct {
	ID              string
	EvaluationID    string
	Conclusion      Status
	Reason          string
	LicenseRequired bool
	Evidence        []Evidence
	By              kernel.ActorRef
	Epoch           int64
	At              time.Time
}

// Decide decides the pending check c on its current evaluation ev (SM-CNX-COMPLIANCE pending →
// clear | blocked). Only a human decides (PERMISSION_DENIED otherwise, besides the command's rule);
// the evaluation decided must be the current one (REFERENCE_STALE: the evaluator looked at another);
// a check that is not pending is INVALID_STATE. Clear needs complete facts: a line without a
// verified declaration profile is DECLARATION_PROFILE_UNVERIFIED (CAT-INV-002), any other missing
// fact INVALID_STATE; and an evaluation older than the regulation's rescreen interval, or past the
// regulation's validity, no longer supports a conclusion (REFERENCE_STALE). Blocked needs nothing
// but a reason: compliance may block on incomplete facts.
func Decide(c *Check, ev *Evaluation, in DecideInput) (*Decision, error) {
	switch {
	case !in.By.IsHuman():
		return nil, kernel.Reject(kernel.CodePermissionDenied, "/", "Only a human compliance officer concludes")
	case c.Status != Pending:
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/check_id", "The check is "+string(c.Status))
	case c.Evaluation == "" || ev == nil:
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/evaluation_id", "The check has no evaluation to decide")
	case c.Evaluation != in.EvaluationID || ev.EvaluationID != in.EvaluationID:
		return nil, reject(CodeReferenceStale, "/args/evaluation_id", "Another evaluation is current")
	case ev.Order != c.Binding || ev.CheckID != c.ID:
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/args/evaluation_id", "The evaluation binds another order revision")
	}
	d := &Decision{ID: in.ID, CheckID: c.ID, EvaluationID: ev.EvaluationID, Conclusion: in.Conclusion, Reason: in.Reason,
		Regulation: RegulationRef{ID: ev.Regulation.ID, Revision: ev.Regulation.Revision, ContentHash: ev.Regulation.ContentHash},
		Evidence:   sortedEvidence(in.Evidence), By: in.By, Epoch: in.Epoch, At: kernel.Instant(in.At)}
	switch in.Conclusion {
	case Blocked:
		return d, nil
	case Clear:
	default:
		return nil, kernel.Reject(kernel.CodeSchemaInvalid, "/args/conclusion", "")
	}
	for _, g := range ev.Gaps {
		if g.Kind == GapClassificationUnknown || g.Kind == GapClassificationUnverified {
			return nil, reject(CodeDeclarationProfileUnverified, "/args/evaluation_id", "Line "+g.Ref+" has no verified declaration profile")
		}
	}
	if len(ev.Gaps) > 0 {
		g := ev.Gaps[0]
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/evaluation_id", "The evaluation is incomplete: "+g.Kind+" "+g.Ref)
	}
	until, err := kernel.ParseTimestamp(ev.ValidUntil)
	if err != nil {
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/args/evaluation_id", "The evaluation record is not intact")
	}
	if !until.After(d.At) {
		return nil, reject(CodeReferenceStale, "/args/evaluation_id", "The evaluation is older than the regulation's rescreen interval")
	}
	d.LicenseRequired, d.ValidUntil = in.LicenseRequired, until
	return d, nil
}

// Causes of a reopening (trade_compliance.reopen).
const (
	CauseRegulationRevised = "regulation_revised" // a screening_regulation revision in force other than the one the conclusion follows
	CauseProgrammeUpdated  = "programme_updated"  // a programme list the conclusion used has a new current version (MD-47)
	CauseExpired           = "expired"            // the clear conclusion's validity ended
	CauseOrderAmended      = "order_amended"      // a new order revision has no conclusion: open a pending check for it (PL-14)
)

// Reopening records why a check became, or was opened, pending: the cause, the decision it
// reopened ("" when it opened a check for a new order revision), the regulation revision in force
// or the programme lists with a new version that caused it, and who did it.
type Reopening struct {
	ID         string
	CheckID    string
	Cause      string
	DecisionID string
	Regulation *RegulationRef
	Updated    []string
	Reason     string
	By         kernel.ActorRef
	Epoch      int64
	At         time.Time
}

// Reopen returns the clear check c, decided by d, to pending for cause: regulation_revised needs a
// regulation revision other than the one d follows (in force: the caller's POL guard),
// programme_updated a programme list d's evaluation used whose current version is another now
// (updated, as reference data's list guard says), expired a conclusion whose validity has ended by
// at (INVALID_STATE otherwise).
func Reopen(c *Check, d *Decision, cause string, reg *RegulationRef, updated []string, r Reopening) (*Reopening, error) {
	if c.Status != Clear || d == nil || d.ID != c.Decision {
		return nil, kernel.Reject(kernel.CodeInvalidState, "/args/check_id", "Only a clear conclusion is reopened")
	}
	switch cause {
	case CauseRegulationRevised:
		if reg == nil || (reg.ID == d.Regulation.ID && reg.Revision == d.Regulation.Revision) {
			return nil, kernel.Reject(kernel.CodeInvalidState, "/args/regulation_revision", "The conclusion follows this regulation revision")
		}
	case CauseProgrammeUpdated:
		if len(updated) == 0 {
			return nil, kernel.Reject(kernel.CodeInvalidState, "/args/check_id", "Every programme list the conclusion used is current")
		}
		reg = nil
		r.Updated = slices.Sorted(slices.Values(updated))
	case CauseExpired:
		if d.ValidUntil.After(r.At) {
			return nil, kernel.Reject(kernel.CodeInvalidState, "/args/check_id", "The conclusion is still valid")
		}
		reg = nil
	default:
		return nil, kernel.Reject(kernel.CodeSchemaInvalid, "/args/cause", "")
	}
	if err := c.Reopened(); err != nil {
		return nil, err
	}
	r.CheckID, r.Cause, r.DecisionID, r.Regulation, r.At = c.ID, cause, d.ID, reg, kernel.Instant(r.At)
	return &r, nil
}
