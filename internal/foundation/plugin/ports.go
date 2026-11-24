package plugin

import "go.idp.xyz/idp-brant/internal/kernel"

// DocumentProfile is the industry template of one document. Empty means generic.
type DocumentProfile string

const (
	ProfileGeneric DocumentProfile = "generic"
	ProfileApparel DocumentProfile = "apparel"
)

// Uniform returns the one profile in profiles. An empty set is generic. A mix is
// DOCUMENT_PROFILE_MISMATCH at path, with the instruction to split the shipment.
func Uniform(profiles []string, path string) (DocumentProfile, error) {
	if len(profiles) == 0 {
		return ProfileGeneric, nil
	}
	first, err := Canonical(profiles[0], path)
	if err != nil {
		return "", err
	}
	for _, p := range profiles[1:] {
		got, err := Canonical(p, path)
		if err != nil {
			return "", err
		}
		if got != first {
			return "", ProfileMismatch(path)
		}
	}
	return first, nil
}

// Canonical returns generic for an empty or generic profile and apparel for apparel.
// Anything else is SCHEMA_INVALID at path.
func Canonical(profile, path string) (DocumentProfile, error) {
	switch DocumentProfile(profile) {
	case "", ProfileGeneric:
		return ProfileGeneric, nil
	case ProfileApparel:
		return ProfileApparel, nil
	default:
		return "", kernel.Reject(kernel.CodeSchemaInvalid, path, "")
	}
}

// Verdict is the merged result of industry findings. reject dominates, then requires_approval.
type Verdict string

const (
	VerdictPass             Verdict = "pass"
	VerdictRequiresApproval Verdict = "requires_approval"
	VerdictReject           Verdict = "reject"
)

// Finding is one industry rule result. MergeFindings keeps every finding; the verdict is separate.
type Finding struct {
	RuleID        string
	Verdict       Verdict
	Code          string
	Path          string
	Message       string
	Waivable      bool
	ApprovalScope string
	EvidenceIDs   []string
}

// MergeFindings returns the strict aggregate. Order does not matter. The caller keeps findings.
func MergeFindings(findings []Finding) Verdict {
	out := VerdictPass
	for _, f := range findings {
		switch f.Verdict {
		case VerdictReject:
			return VerdictReject
		case VerdictRequiresApproval:
			out = VerdictRequiresApproval
		}
	}
	return out
}

// CostEstimateInput and CostEstimateOutput are the cost.estimate.v1 shapes. S7-03 fills the
// garment inputs. Money stays in minor units.
type CostEstimateInput struct{}

// CostEstimateOutput is the deterministic estimate. It is not a selling price.
type CostEstimateOutput struct {
	Amount kernel.Money
}

// CostProvider is cost.estimate.v1 bound to one document profile. It does not touch a database.
type CostProvider interface {
	PluginID() string
	DocumentProfile() DocumentProfile
	Estimate(in CostEstimateInput) (CostEstimateOutput, error)
}

// QuoteValidationInput is quote.validate.v1. S7-03 fills it.
type QuoteValidationInput struct{}

// QuoteValidator runs after TRD's own checks on a freeze.
type QuoteValidator interface {
	PluginID() string
	DocumentProfile() DocumentProfile
	ValidateQuote(in QuoteValidationInput) ([]Finding, error)
}

// PackingValidationInput is packing.validate.v1. S7-03 fills it.
type PackingValidationInput struct{}

// PackingValidator checks assortment and carton rules.
type PackingValidator interface {
	PluginID() string
	DocumentProfile() DocumentProfile
	ValidatePacking(in PackingValidationInput) ([]Finding, error)
}

// InspectionValidationInput is inspection.validate.v1. S7-03 fills it.
type InspectionValidationInput struct{}

// InspectionValidator checks apparel inspection parameters.
type InspectionValidator interface {
	PluginID() string
	DocumentProfile() DocumentProfile
	ValidateInspection(in InspectionValidationInput) ([]Finding, error)
}
