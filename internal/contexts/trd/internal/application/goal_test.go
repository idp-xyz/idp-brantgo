package application

import (
	"errors"
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// An assertion takes its checks' answers combined (api.Outcome) and the code of the first check that
// gave that answer; a refusal is recorded with the answer the caller gives it, except
// DEPENDENCY_NOT_SUPPORTED, which is always unknown (an upstream path not delivered cannot pass);
// an error that is not a refusal is returned, not recorded.
func TestVerdictCombinesItsChecks(t *testing.T) {
	v := newVerdict(api.AssertionLines)
	_ = v.check(api.CheckUpstream, nil, "")
	if a := v.done(); a.Answer != api.AnswerSatisfied || a.Code != "" || len(a.Checks) != 1 {
		t.Fatalf("satisfied %+v", a)
	}
	_ = v.check(api.CheckCostScope, refusal(api.CodeDependencyNotSupported, "/data/lines/0/unit_ref"), api.AnswerUnsatisfied)
	if a := v.done(); a.Answer != api.AnswerUnknown || a.Code != api.CodeDependencyNotSupported || a.Checks[1].Path != "/data/lines/0/unit_ref" {
		t.Fatalf("unknown %+v", a)
	}
	_ = v.check(api.CheckOrderSnapshot, kernel.Reject(kernel.CodeReferenceInvalid, "/order/snapshot", ""), api.AnswerError)
	if a := v.done(); a.Answer != api.AnswerError || a.Code != kernel.CodeReferenceInvalid {
		t.Fatalf("error %+v", a)
	}
	_ = v.check(api.CheckSalesFX, refusal("FX_STALE", "/data/sales_fx_ref"), api.AnswerUnsatisfied)
	_ = v.check(api.CheckUpstream, refusal(api.CodeReferenceStale, "/data/rfq_ref"), api.AnswerUnsatisfied)
	if a := v.done(); a.Answer != api.AnswerUnsatisfied || a.Code != "FX_STALE" || len(a.Checks) != 5 {
		t.Fatalf("unsatisfied %+v", a)
	}
	broken := errors.New("connection lost")
	if err := v.check(api.CheckUpstream, broken, api.AnswerUnsatisfied); !errors.Is(err, broken) || len(v.a.Checks) != 5 {
		t.Fatalf("an infrastructure error: %v, %d checks", err, len(v.a.Checks))
	}
}
