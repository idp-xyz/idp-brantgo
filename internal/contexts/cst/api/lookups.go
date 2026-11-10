package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// ContextRef names a sealed costing context revision (the context_ref of a cost scenario,
// BusinessRef kind costing_context).
type ContextRef struct {
	ContextID string
	Revision  int64
}

// BoundLine is one RFQ line as the verification saw it through RfqRef v1.
type BoundLine struct {
	LineID          string
	SkuID           string
	SkuSpecRevision int64
	Quantity        string
	UOM             string
}

// BoundAssumption is an assumption of a verified revision with its proposer. Verification never
// upgrades an assumption: VerificationStatus is always "unverified" (T02 §15.2).
type BoundAssumption struct {
	Assumption
	ProposedBy         kernel.ActorRef
	ProposedAt         time.Time
	VerificationStatus string
}

// AssumptionUnverified is the only verification status of an assumption.
const AssumptionUnverified = "unverified"

// VerifiedContext is a sealed costing context revision as the scenario evaluation and the freeze
// read it. Status is its status now: active (Current), or superseded or withdrawn, which a new
// evaluation or freeze does not use (Check). Rfq is the RfqRef v1 binding and Lines what the
// verification judged against INQ (RfqLines, the RFQ's complete line set then); ConfigRefs are all POL configurations the revision names,
// deduplicated and sorted by kind, ID and revision: the foundation references an evaluation adopts
// and a freeze re-checks through ConfigGuard v1, together with RfqGuard v1 for Rfq.
type VerifiedContext struct {
	Ref                      ContextRef
	Status                   string
	Kind                     string
	ContentHash              string
	Rfq                      RfqBinding
	RfqLines                 []BoundLine
	Lines                    []ContextLine
	CalculationCurrencyRef   ConfigRef
	PricingPolicyRef         ConfigRef
	ComparisonBasis          ComparisonBasis
	RequestedCurrencyRef     *ConfigRef
	RequestedTradeTermsRef   *ConfigRef
	RequestedPaymentTermsRef *ConfigRef
	Assumptions              []BoundAssumption
	ConfigRefs               []ConfigRef
	VerifiedBy               kernel.ActorRef
	VerifiedAt               time.Time
	AuthorityEpoch           int64
	Evidence                 []Evidence
}

// Current reports whether the revision is the context's active revision.
func (v VerifiedContext) Current() bool { return v.Status == StatusActive }

// Provisional reports whether a result computed from the revision is provisional: it carries
// unverified assumptions (T02 §15.2). Delivery and other findings of an evaluation may make a
// result provisional too.
func (v VerifiedContext) Provisional() bool { return len(v.Assumptions) > 0 }

// Check returns nil for the active revision and REFERENCE_STALE (409) at path for one that was
// superseded or withdrawn: refresh the reference to the active revision and evaluate again.
func (v VerifiedContext) Check(path string) error {
	if v.Current() {
		return nil
	}
	return Reject(CodeReferenceStale, path)
}

// Freezable returns nil when a cost may be frozen on the revision as a formal costing of its RFQ,
// and COST_INPUT_NOT_READY at path otherwise: a what_if context is never frozen for its RFQ, and a
// context with unverified assumptions only yields provisional results (T02 §13.5, §15.2; WBS S2-03
// "provisional 不可冻结"). It does not check currency (Check) or the freeze's other conditions.
func (v VerifiedContext) Freezable(path string) error {
	switch {
	case v.Kind != ContextActual:
		return kernel.RejectAs(CodeCostInputNotReady, kernel.CategoryInvalid, path+"/context_kind",
			"A what_if costing context is not frozen for its RFQ")
	case v.Provisional():
		return kernel.RejectAs(CodeCostInputNotReady, kernel.CategoryInvalid, path+"/assumptions",
			"Unverified assumptions keep the result provisional")
	}
	return nil
}

// MeasurementRef names a measurement revision (the measurement_ref of a logistics fee or an
// allocation).
type MeasurementRef struct {
	MeasurementID string
	Revision      int64
}

// ResolvedMeasurement is a measurement revision as the scenario evaluation and the freeze read it.
// Status is its status now: active (Current), or superseded or withdrawn, which a new evaluation
// or freeze does not use (Check). LineIDs are the RFQ lines its packages hold; the evaluation checks
// them against the scenario's lines and the pricing policy's allowance for estimates.
type ResolvedMeasurement struct {
	Ref            MeasurementRef
	Status         string
	ContentHash    string
	Data           MeasurementData
	LineIDs        []string
	RecordedBy     kernel.ActorRef
	RecordedAt     time.Time
	AuthorityEpoch int64
}

// Current reports whether the revision is the measurement's active revision.
func (m ResolvedMeasurement) Current() bool { return m.Status == StatusActive }

// Check returns nil for the active revision and REFERENCE_STALE (409) at path otherwise.
func (m ResolvedMeasurement) Check(path string) error {
	if m.Current() {
		return nil
	}
	return Reject(CodeReferenceStale, path)
}

// Lookups answers the references a cost scenario names, for its evaluation and freeze. A
// reference that does not exist, is not sealed or lies outside the caller's data scope is absent.
// They need no permission beyond an active grant and are not audited: the command that asks
// records its own outcome. Inside its own transaction the freeze reads the same rows through CST's
// store, locked FOR SHARE after the scenario root.
type Lookups interface {
	ResolveContexts(ctx context.Context, refs []ContextRef) (map[ContextRef]VerifiedContext, error)
	ResolveMeasurements(ctx context.Context, refs []MeasurementRef) (map[MeasurementRef]ResolvedMeasurement, error)
	// MeasurementsForLines returns the active measurement revisions whose packages hold any of
	// lineIDs, keyed by reference. A measurement names RFQ line IDs, not the RFQ (Python T02
	// measurement data), so line IDs of different RFQs collide: a scenario still names the
	// measurement it uses, and the evaluation checks every package line against its own lines.
	MeasurementsForLines(ctx context.Context, lineIDs []string) (map[MeasurementRef]ResolvedMeasurement, error)
}
