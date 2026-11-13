package application

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/internal/domain/task"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The goal verifications read (task.verifications) needs tsk.read and admits every actor kind, as
// task.get does; the codes a verification records are the error catalog's 任务与 AI rows of TSK
// (422), and the published names are the domain's.
func TestVerificationReadAndCodes(t *testing.T) {
	policy, err := authorization.NewPolicy(Rules()...)
	if err != nil {
		t.Fatal(err)
	}
	r, err := policy.Rule(api.CommandVerifications)
	if err != nil || r.Permission != api.PermissionRead || !r.Admits(kernel.ActorHuman) || !r.Admits(kernel.ActorAI) ||
		!r.Admits(kernel.ActorSystem) {
		t.Fatalf("%+v %v", r, err)
	}
	var catalog struct {
		Codes [][]any `json:"codes"`
	}
	load(t, root("docs", "00_START_HERE", "tools", "data", "error_catalog.json"), &catalog)
	rows := map[string][]any{}
	for _, row := range catalog.Codes {
		rows[row[0].(string)] = row
	}
	for _, code := range []string{api.CodeRequiredCheckUnsatisfied, api.CodeRequiredCheckUnknown} {
		row, ok := rows[code]
		if !ok || row[2].(float64) != 422 || row[1].(string) != "任务与 AI" || row[4].(string) != "TSK／TRD" {
			t.Errorf("%s: %v", code, row)
		}
	}
	if api.CodeRequiredCheckUnsatisfied != task.CodeRequiredCheckUnsatisfied || api.CodeRequiredCheckUnknown != task.CodeRequiredCheckUnknown ||
		api.AssessorGoalReadiness != task.AssessorGoalReadiness || api.AssessorTSK != task.AssessorTSK ||
		api.AssertionExecution != task.AssertionExecution || api.AnswerSatisfied != task.Satisfied || api.AnswerUnsatisfied != task.Unsatisfied ||
		api.AnswerUnknown != task.Unknown || api.AnswerError != task.Error {
		t.Fatal("the published names differ from the domain's")
	}
}
