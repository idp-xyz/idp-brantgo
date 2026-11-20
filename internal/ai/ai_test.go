package ai

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"go.idp.xyz/idp-brant/internal/ai/capability"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The AI layer's commands are the Context Map's platformCommands AI, each human-only; its reads
// need ai.read and its audit ai.audit, the permissions of the role templates.
func TestRulesArePlatformCommandsAI(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "03_STRATEGIC_DDD", "01_IDP_Brant_Strategic_Context_Map_V0.1_Draft.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		PlatformCommands []struct {
			ID       string   `json:"id"`
			Schema   string   `json:"schema"`
			Commands []string `json:"commands"`
		} `json:"platformCommands"`
	}
	if err = json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var commands []string
	for _, p := range doc.PlatformCommands {
		if p.ID == "AI" {
			if p.Schema != "brant_ai" {
				t.Fatalf("schema %s", p.Schema)
			}
			commands = p.Commands
		}
	}
	if len(commands) != 4 {
		t.Fatalf("platformCommands AI = %v", commands)
	}
	policy, err := authorization.NewPolicy(Rules()...)
	if err != nil {
		t.Fatal(err)
	}
	var writes []string
	for _, r := range Rules() {
		if slices.Contains(r.ActorKinds, kernel.ActorHuman) && len(r.ActorKinds) == 1 {
			writes = append(writes, r.Command)
		}
	}
	slices.Sort(writes)
	want := slices.Clone(commands)
	slices.Sort(want)
	if !slices.Equal(writes, want) {
		t.Fatalf("human-only rules %v, platformCommands AI %v", writes, want)
	}
	for _, c := range commands {
		if !capability.IsHumanOnly(c) {
			t.Errorf("%s is not in humanOnlyCommands", c)
		}
		if r, _ := policy.Rule(c); r.Permission != c {
			t.Errorf("%s needs %s", c, r.Permission)
		}
	}
	held := map[string]bool{}
	for _, tpl := range authorization.Templates() {
		for _, p := range tpl.Permissions {
			held[p] = true
		}
	}
	for _, r := range Rules() {
		if !held[r.Permission] {
			t.Errorf("no role template holds %s (%s)", r.Permission, r.Command)
		}
	}
}
