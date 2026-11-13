package tradecompliancecheck

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// RegulationRef is a screening_regulation revision as a decision or a reopening binds it.
type RegulationRef struct {
	ID          string
	Revision    int64
	ContentHash string
}

// Kinds of an authorization (S5R-D17: 许可证、豁免或主管机关批复).
const (
	AuthorizationLicense           = "license"
	AuthorizationExemption         = "exemption"
	AuthorizationAuthorityApproval = "authority_approval"
)

// Authorization is an EVD source a clear decision names as the authorization an embargo or
// sanctions finding is cleared on, of Kind, valid until ValidUntil (S5R-D17).
type Authorization struct {
	Evidence
	Kind       string    `json:"kind"`
	ValidUntil time.Time `json:"valid_until"`
}

// Decision is a human compliance conclusion on one evaluation (trade_compliance.decide,
// CNX-INV-001): clear or blocked with the reason, the evidence, whether the goods need an export
// licence (clear only), the regulation revision it follows and, for clear, how long it stays valid
// and the authorizations it rests on (S5R-D17). Decisions are only appended.
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
	Authorizations  []Authorization
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
	Authorizations  []Authorization
	By              kernel.ActorRef
	Epoch           int64
	At              time.Time
}

// Decide decides the pending check c on its current evaluation ev (SM-CNX-COMPLIANCE pending →
// clear | blocked). Only a human decides (PERMISSION_DENIED otherwise, besides the command's rule);
// the evaluation decided must be the current one (REFERENCE_STALE: the evaluator looked at another);
// a check that is not pending is INVALID_STATE. Clear needs complete facts: a line without a
// verified declaration profile is DECLARATION_PROFILE_UNVERIFIED (CAT-INV-002), any other missing
// fact TRADE_COMPLIANCE_EVALUATION_INCOMPLETE (S5R; it was INVALID_STATE); and an evaluation older
// than the regulation's rescreen interval, or past the regulation's validity, no longer supports a
// conclusion (REFERENCE_STALE). S5R-D17: an applying embargo or sanctions finding is cleared only on
// at least one authorization valid at the decision (INVALID_STATE /args/evidence_ids; one already
// lapsed is INVALID_STATE at its valid_until), an applying arms_embargo, export_control or
// controlled_goods finding only with evidence that the goods are not controlled (INVALID_STATE
// /args/evidence_ids), and the clear conclusion's validity never passes the earliest authorization.
// A region_review finding is the officer's judgement, recorded with the reason every decision gives
// (S5R-D15). Blocked needs nothing but a reason: compliance may block on incomplete facts.
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
		return nil, reject(CodeEvaluationIncomplete, "/args/evaluation_id", "The evaluation is incomplete: "+g.Kind+" "+g.Ref)
	}
	until, err := kernel.ParseTimestamp(ev.ValidUntil)
	if err != nil {
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, "/args/evaluation_id", "The evaluation record is not intact")
	}
	if !until.After(d.At) {
		return nil, reject(CodeReferenceStale, "/args/evaluation_id", "The evaluation is older than the regulation's rescreen interval")
	}
	if until, err = authorized(ev, in, d.At, until); err != nil {
		return nil, err
	}
	d.LicenseRequired, d.ValidUntil, d.Authorizations = in.LicenseRequired, until, sortedAuthorizations(in.Authorizations)
	return d, nil
}

// authorized applies S5R-D17 to a clear decision at at on ev: the applying findings it clears need
// an authorization (embargo, sanctions) or evidence (arms_embargo, export_control, controlled
// goods), and the conclusion's validity until is capped by every authorization's.
func authorized(ev *Evaluation, in DecideInput, at, until time.Time) (time.Time, error) {
	for i, a := range in.Authorizations {
		if !a.ValidUntil.After(at) {
			return until, kernel.Reject(kernel.CodeInvalidState, "/args/authorizations/"+strconv.Itoa(i)+"/valid_until", "The authorization has lapsed")
		}
		if a.ValidUntil.Before(until) {
			until = kernel.Instant(a.ValidUntil)
		}
	}
	for _, f := range ev.Findings {
		if !f.Applies {
			continue
		}
		switch {
		case f.Kind == FindingProgramme && (f.Measure == MeasureEmbargo || f.Measure == MeasureSanctions):
			if len(in.Authorizations) == 0 {
				return until, kernel.Reject(kernel.CodeInvalidState, "/args/evidence_ids", "Programme "+f.ProgrammeID+" ("+f.Measure+
					") applies: clear it on an authorization (a licence, an exemption or the authority's approval)")
			}
		case len(in.Evidence) == 0:
			return until, kernel.Reject(kernel.CodeInvalidState, "/args/evidence_ids", "A "+f.Kind+" finding applies: clear it with evidence"+
				" that the goods are not controlled")
		}
	}
	return until, nil
}

func sortedAuthorizations(as []Authorization) []Authorization {
	out := slices.Clone(as)
	slices.SortFunc(out, func(a, b Authorization) int { return strings.Compare(a.SourceID, b.SourceID) })
	for i := range out {
		out[i].ValidUntil = kernel.Instant(out[i].ValidUntil)
	}
	return out
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
