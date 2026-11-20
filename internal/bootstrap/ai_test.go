package bootstrap

import (
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/ai/adapters/model/openaicompat"
)

// brant-worker -agent's model choice: the scripted model from a script file (the default and the
// only one in CI) or the OpenAI-compatible adapter (decision E-4); both are free.
func TestNewModel(t *testing.T) {
	m, p, err := NewModel(ModelConfig{Script: []byte(`[{"content":"done"}]`)})
	if err != nil || p.Provider != "scripted" || p.Model != "scripted-q01-v1" || !p.Free() || m == nil {
		t.Fatalf("%+v %v", p, err)
	}
	if _, _, err = NewModel(ModelConfig{Kind: ModelScripted, Script: []byte(`{}`)}); err == nil {
		t.Fatal("a broken script")
	}
	m, p, err = NewModel(ModelConfig{Kind: ModelOpenAICompat, BaseURL: "http://127.0.0.1:8080", APIKey: "secret-key-value"})
	if err != nil || p.Provider != "openaicompat" || p.Model != "qwen3.8-flash-next-iq3_xxs" || !p.Free() {
		t.Fatalf("%+v %v", p, err)
	}
	if c, ok := m.(*openaicompat.Client); !ok || strings.Contains(c.String(), "secret-key-value") {
		t.Fatalf("%v", m)
	}
	if _, _, err = NewModel(ModelConfig{Kind: "claude"}); err == nil {
		t.Fatal("an unknown kind")
	}
	if _, _, err = NewModel(ModelConfig{Kind: ModelOpenAICompat, BaseURL: "file:///x"}); err == nil {
		t.Fatal("a bad base URL")
	}
}

func TestParseScopes(t *testing.T) {
	s, err := ParseScopes("t.1/c.1, t.1/c.2")
	if err != nil || len(s) != 2 || s[1].Company != "c.2" {
		t.Fatalf("%v %v", s, err)
	}
	for _, bad := range []string{"", "t.1", "t.1/", "/c", "t 1/c"} {
		if _, err := ParseScopes(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
