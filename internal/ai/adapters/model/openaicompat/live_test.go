//go:build modellive

package openaicompat

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/ai/adapters/model"
)

// A live smoke run against the owner's local OpenAI-compatible service (decision E-4): built only
// with -tags modellive, never under CI=true, and only when the service's key is in
// IDP_BRANT_MODEL_API_KEY. IDP_BRANT_MODEL_BASE_URL (default http://127.0.0.1:8080) and
// IDP_BRANT_MODEL (default qwen3.8-flash-next-iq3_xxs) choose the service and model. The request is
// synthetic; the test prints the returned model ID, usage and latency, never the key.
//
//	IDP_BRANT_MODEL_API_KEY=... go test -tags modellive -run TestLiveSmoke -v ./internal/ai/adapters/model/openaicompat/
func TestLiveSmoke(t *testing.T) {
	if os.Getenv("CI") == "true" {
		t.Skip("live model runs never run under CI=true")
	}
	key := os.Getenv(KeyEnv)
	if key == "" {
		t.Skip("no key in " + KeyEnv + ": the live smoke run was not executed")
	}
	base := os.Getenv("IDP_BRANT_MODEL_BASE_URL")
	if base == "" {
		base = "http://127.0.0.1:8080"
	}
	name := os.Getenv("IDP_BRANT_MODEL")
	if name == "" {
		name = "qwen3.8-flash-next-iq3_xxs"
	}
	c, err := New(Config{BaseURL: base, Model: name, APIKey: key, Timeout: 3 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Complete(context.Background(), model.Request{MaxOutputTokens: 512, Messages: []model.Message{
		{Role: model.RoleSystem, Content: "You register inquiry sources. Use the tool."},
		{Role: model.RoleUser, Content: `Register the synthetic inquiry text "120 pcs sku.1, please quote" as source src.live.1 of kind manual_text.`},
	}, Tools: []model.Tool{{Name: "source.register", Description: "Register a text source.", Parameters: json.RawMessage(
		`{"type":"object","properties":{"SourceID":{"type":"string"},"SourceKind":{"type":"string"},"ContentText":{"type":"string"}},` +
			`"required":["SourceID","SourceKind","ContentText"],"additionalProperties":false}`)}}})
	if err != nil {
		t.Fatalf("live call: %v", err)
	}
	t.Logf("model %s, finish %s, usage %+v, latency %s, %d tool calls", resp.Model, resp.FinishReason, resp.Usage, resp.Latency,
		len(resp.Message.ToolCalls))
	for _, tc := range resp.Message.ToolCalls {
		t.Logf("tool call %s with %d bytes of arguments (valid JSON: %t)", tc.Name, len(tc.Arguments), json.Valid(tc.Arguments))
	}
}
