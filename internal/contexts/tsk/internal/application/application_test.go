package application

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/contexts/tsk/api"
	"go.idp.xyz/idp-brant/internal/contexts/tsk/internal/domain/task"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

func root(parts ...string) string {
	return filepath.Join(append([]string{"..", "..", "..", "..", ".."}, parts...)...)
}

func load(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, v); err != nil {
		t.Fatal(err)
	}
}

// The codes the domain gives are TSK's declared codes with the same categories, and every code TSK
// declares is in the error catalog V0.1 with the category its HTTP status means (422 invalid, 409
// conflict), as S3 kept its tables in step.
func TestCodesAreTheCatalogs(t *testing.T) {
	for code, cat := range task.Codes() {
		if got, ok := api.Category(code); !ok || got != cat {
			t.Errorf("domain code %s (%s) is declared as %s, %v", code, cat, got, ok)
		}
	}
	var catalog struct {
		Codes [][]any `json:"codes"`
	}
	load(t, root("docs", "00_START_HERE", "tools", "data", "error_catalog.json"), &catalog)
	status := map[string]float64{}
	for _, row := range catalog.Codes {
		status[row[0].(string)] = row[2].(float64)
	}
	for code, cat := range api.Codes() {
		want := map[kernel.Category]float64{kernel.CategoryInvalid: 422, kernel.CategoryConflict: 409}[cat]
		if s, ok := status[code]; !ok || s != want {
			t.Errorf("%s (%s): catalog status %v, %v", code, cat, s, ok)
		}
	}
}

// The rules: a permission per command, named as the command (role templates V0.1); the human-only
// rules are exactly the tactical model's humanOnlyCommands of TSK (X-INV-002); no command of a task's
// lifecycle admits an AI actor (AI-INV-004), while ActionExecution's two admit a task step's
// executor, human or AI (role templates: ai_q01_agent holds them), and no system actor; every TSK
// command of the Context Map has a rule, and none other.
func TestRulesFollowTheDesign(t *testing.T) {
	var tactical struct {
		HumanOnly []string `json:"humanOnlyCommands"`
	}
	load(t, root("docs", "05_TACTICAL_DDD", "02_IDP_Brant_Tactical_DDD_Candidate_V0.1.json"), &tactical)
	var cmap struct {
		Contexts []struct {
			ID       string   `json:"id"`
			Commands []string `json:"commands"`
		} `json:"contexts"`
	}
	load(t, root("docs", "03_STRATEGIC_DDD", "01_IDP_Brant_Strategic_Context_Map_V0.1_Draft.json"), &cmap)
	var designed []string
	for _, c := range cmap.Contexts {
		if c.ID == "TSK" {
			designed = append(designed, c.Commands...)
		}
	}
	policy, err := authorization.NewPolicy(Rules()...)
	if err != nil {
		t.Fatal(err)
	}
	var commands []string
	for _, r := range Rules() {
		read := r.Command == api.CommandGetTask || r.Command == api.CommandClaim || r.Command == api.CommandAudit ||
			r.Command == api.CommandGetAction || r.Command == api.CommandTaskActions || r.Command == api.CommandVerifications
		if read {
			if r.Permission != api.PermissionRead && r.Command != api.CommandAudit {
				t.Errorf("%s needs %s", r.Command, r.Permission)
			}
			continue
		}
		commands = append(commands, r.Command)
		if r.Permission != r.Command {
			t.Errorf("%s needs %s", r.Command, r.Permission)
		}
		if strings.HasPrefix(r.Command, "action.") {
			if !slices.Equal(r.ActorKinds, []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI}) {
				t.Errorf("%s admits %v", r.Command, r.ActorKinds)
			}
		} else if r.Admits(kernel.ActorAI) {
			t.Errorf("%s admits an AI actor", r.Command)
		}
		humanOnly := slices.Equal(r.ActorKinds, []kernel.ActorKind{kernel.ActorHuman})
		if humanOnly != slices.Contains(tactical.HumanOnly, r.Command) {
			t.Errorf("%s human only %v, humanOnlyCommands %v", r.Command, humanOnly, !humanOnly)
		}
	}
	slices.Sort(commands)
	slices.Sort(designed)
	if !slices.Equal(commands, designed) || len(commands) != 17 {
		t.Fatalf("rules %v, Context Map %v", commands, designed)
	}
	for _, c := range []string{"task.transition", "task.set_state", "task.complete", "action.record_outcome", "action.commit"} {
		if _, err := policy.Rule(c); err == nil {
			t.Errorf("%s has a rule", c)
		}
	}
	claim, _ := policy.Rule(api.CommandClaim)
	if claim.Admits(kernel.ActorSystem) || !claim.Admits(kernel.ActorAI) || claim.Permission != api.PermissionRead {
		t.Fatalf("claim rule %+v", claim)
	}
}

// Q01 V0.1 tests/test_execution.py::test_generic_transition_cannot_set_completed, AI-INV-004 and the
// rule that scope, actor and task control come from the Principal and TSK: no command of the
// contract carries a target state, a lifecycle, a completion, a control epoch or mode outside
// task.create's own mode, a tenant, a company or an actor.
func TestNoCommandCarriesStateScopeOrControl(t *testing.T) {
	forbidden := []string{"State", "Status", "Lifecycle", "Completed", "CompletionActor", "ControlEpoch", "Epoch", "Tenant", "TenantID",
		"Company", "CompanyID", "Actor", "ActorID", "ActorKind", "Initiator", "To", "Target State"}
	cmds := []api.Command{api.CreateTask{}, api.ScopeTask{}, api.WaitTask{}, api.VerifyTask{}, api.VerifyGoal{}, api.PauseTask{},
		api.ResumeTask{}, api.TakeControl{}, api.Handoff{}, api.CancelTask{}, api.FinishStop{}, api.CloseUnfulfilled{}, api.FailTask{},
		api.BindObject{}, api.ReleaseObject{}}
	var walk func(reflect.Type, string)
	walk = func(rt reflect.Type, path string) {
		for rt.Kind() == reflect.Pointer || rt.Kind() == reflect.Slice {
			rt = rt.Elem()
		}
		if rt.Kind() != reflect.Struct {
			return
		}
		for i := range rt.NumField() {
			f := rt.Field(i)
			if slices.Contains(forbidden, f.Name) || (f.Name == "ControlMode" && rt != reflect.TypeFor[api.CreateTask]()) {
				t.Errorf("%s.%s", path, f.Name)
			}
			walk(f.Type, path+"."+f.Name)
		}
	}
	for _, c := range cmds {
		rt := reflect.TypeOf(c)
		walk(rt, rt.Name())
		if name, _, _ := request(c); name == "" {
			t.Errorf("%s has no command name", rt.Name())
		}
	}
	if len(cmds) != len(task.Commands()) {
		t.Fatalf("%d command structs for %d commands", len(cmds), len(task.Commands()))
	}
}

// The request hash covers the business arguments only: the idempotency key and the expected task
// version do not change it, a business field does.
func TestRequestHashIgnoresKeyAndExpectedVersion(t *testing.T) {
	a := api.TakeControl{IdempotencyKey: "k1", TaskID: "task.1", ExpectedVersion: 3, Reason: "model down"}
	b := a
	b.IdempotencyKey, b.ExpectedVersion = "k2", 9
	if requestHash(a) != requestHash(b) {
		t.Fatal("key or version changed the hash")
	}
	b.Reason = "other"
	if requestHash(a) == requestHash(b) {
		t.Fatal("the reason did not change the hash")
	}
}

// The TRD key of a task's control command at a recorded epoch is a valid identifier, the same for
// every attempt, and another for another task, command or epoch.
func TestDeriveKey(t *testing.T) {
	k := DeriveKey("task.1", trdStop, 3)
	if !kernel.ValidIdentifier(k) || k != DeriveKey("task.1", trdStop, 3) {
		t.Fatalf("%q", k)
	}
	for _, other := range []string{DeriveKey("task.2", trdStop, 3), DeriveKey("task.1", trdRelease, 3), DeriveKey("task.1", trdStop, 4)} {
		if other == k {
			t.Fatal("keys collide")
		}
	}
	if r := ControlReason("task.1", 3); r != "TSK task task.1 at control epoch 3" {
		t.Fatal(r)
	}
}

func TestReadinessNotWired(t *testing.T) {
	_, err := ReadinessNotWired{}.Assess(context.Background(), Goal{})
	if rej, ok := kernel.AsRejection(err); !ok || rej.Code != api.CodeDependencyNotSupported {
		t.Fatalf("%v", err)
	}
}

// The database's own guard (brant_tsk.guard_task, migration 000001) allows exactly the moves of
// SM-TSK-TASK: its list of state pairs is the set of (from, to) pairs of the transition table.
func TestDatabaseGuardMovesAreSMTSKTASK(t *testing.T) {
	raw, err := os.ReadFile(root("migrations", "tsk", "000001_tsk_tasks.sql"))
	if err != nil {
		t.Fatal(err)
	}
	sql := string(raw)
	start := strings.Index(sql, "v_moves text[] := ARRAY[")
	end := strings.Index(sql[start:], "];")
	if start < 0 || end < 0 {
		t.Fatal("v_moves not found")
	}
	var got []string
	for _, part := range strings.Split(sql[start+len("v_moves text[] := ARRAY["):start+end], ",") {
		got = append(got, strings.Trim(strings.TrimSpace(part), "'"))
	}
	var want []string
	for _, tr := range task.Transitions() {
		if m := string(tr.From) + ">" + string(tr.To); !slices.Contains(want, m) {
			want = append(want, m)
		}
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("guard moves %v\nSM-TSK-TASK %v", got, want)
	}
}
