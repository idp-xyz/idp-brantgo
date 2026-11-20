package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/ai/adapters/model"
)

const secret = "sk-test-not-a-real-key-7f3a"

// The adapter speaks chat completions with tools to an OpenAI-compatible service (decision E-4):
// command names travel as function names with "__" for ".", tool call arguments as JSON strings,
// the bearer key only in the Authorization header; the answer's tool calls come back under the
// command names, with the model ID the service returned, its token usage and the latency.
func TestCompleteSpeaksChatCompletionsWithTools(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("%s %s %s", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
		}
		if r.Header.Get("Authorization") != "Bearer "+secret {
			t.Errorf("authorization %q", r.Header.Get("Authorization"))
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Error(err)
		}
		time.Sleep(5 * time.Millisecond)
		_, _ = io.WriteString(w, `{"id":"x","model":"qwen3.8-flash-next-iq3_xxs-served","choices":[{"index":0,"message":{"role":"assistant",
			"content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"rfq__save_draft","arguments":"{\"RfqID\":\"rfq.1\"}"}},
			{"id":"call_2","type":"function","function":{"name":"source__register","arguments":{"SourceID":"s.1"}}},
			{"id":"call_3","type":"function","function":{"name":"quote__activate","arguments":"not json"}}]},"finish_reason":"tool_calls"}],
			"usage":{"prompt_tokens":120,"completion_tokens":30,"total_tokens":150}}`)
	}))
	defer srv.Close()
	c, err := New(Config{BaseURL: srv.URL + "/", Model: "qwen3.8-flash-next-iq3_xxs", APIKey: secret})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Complete(context.Background(), model.Request{MaxOutputTokens: 512, Messages: []model.Message{
		{Role: model.RoleSystem, Content: "rules"},
		{Role: model.RoleUser, Content: "facts"},
		{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "c0", Name: "rfq.save_draft", Arguments: json.RawMessage(`{"RfqID":"rfq.0"}`)}}},
		{Role: model.RoleTool, ToolCallID: "c0", Content: `{"ok":true}`},
	}, Tools: []model.Tool{
		{Name: "rfq.save_draft", Description: "save", Parameters: json.RawMessage(`{"type":"object","properties":{"RfqID":{"type":"string"}}}`)},
		{Name: "source.register", Description: "register"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Model != "qwen3.8-flash-next-iq3_xxs-served" || resp.FinishReason != "tool_calls" || resp.Usage != (model.Usage{InputTokens: 120, OutputTokens: 30}) ||
		resp.Latency <= 0 || len(resp.Message.ToolCalls) != 3 {
		t.Fatalf("%+v", resp)
	}
	calls := resp.Message.ToolCalls
	if calls[0].Name != "rfq.save_draft" || string(calls[0].Arguments) != `{"RfqID":"rfq.1"}` ||
		calls[1].Name != "source.register" || string(calls[1].Arguments) != `{"SourceID":"s.1"}` ||
		calls[2].Name != "quote__activate" || string(calls[2].Arguments) != `"not json"` {
		t.Fatalf("calls %+v", calls)
	}
	if got["model"] != "qwen3.8-flash-next-iq3_xxs" || got["max_tokens"] != float64(512) || got["stream"] != false {
		t.Fatalf("request %v", got)
	}
	tools := got["tools"].([]any)
	if fn := tools[0].(map[string]any)["function"].(map[string]any); fn["name"] != "rfq__save_draft" || fn["parameters"] == nil {
		t.Fatalf("tool %v", fn)
	}
	if fn := tools[1].(map[string]any)["function"].(map[string]any); fmt.Sprint(fn["parameters"]) != "map[type:object]" {
		t.Fatalf("tool without parameters %v", fn)
	}
	msgs := got["messages"].([]any)
	asst := msgs[2].(map[string]any)
	call := asst["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if asst["content"] != nil || call["name"] != "rfq__save_draft" || call["arguments"] != `{"RfqID":"rfq.0"}` {
		t.Fatalf("assistant %v", asst)
	}
	if tool := msgs[3].(map[string]any); tool["role"] != "tool" || tool["tool_call_id"] != "c0" || tool["content"] != `{"ok":true}` {
		t.Fatalf("tool message %v", tool)
	}
	for _, s := range []string{c.String(), c.GoString(), fmt.Sprintf("%v %+v %#v", c, c, c)} {
		if strings.Contains(s, secret) {
			t.Fatalf("the key leaks: %s", s)
		}
	}
}

// Without a key no Authorization header is sent; a base URL ending in /v1 keeps its path; the
// service's missing model and finish reason are filled in.
func TestCompleteWithoutKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.URL.Path != "/api/v1/chat/completions" {
			t.Errorf("%q %s", r.Header.Get("Authorization"), r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"done"}}]}`)
	}))
	defer srv.Close()
	c, err := New(Config{BaseURL: srv.URL + "/api/v1", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Complete(context.Background(), model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: "x"}}})
	if err != nil || resp.Model != "m" || resp.FinishReason != "stop" || resp.Message.Content != "done" || resp.Usage != (model.Usage{}) {
		t.Fatalf("%+v %v", resp, err)
	}
}

// Failures map to the port's errors (BC-AT26: an unavailable model is retried within the budget,
// then the task pauses): connection refused, timeouts, 408/429/5xx and broken answers are
// model.ErrUnavailable; refused credentials ErrAuthentication (also unavailable); other 4xx
// model.ErrInvalidRequest. No error carries the key or the service's message text.
func TestFailuresMapToThePortsErrors(t *testing.T) {
	status, body := 0, ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status == -1 {
			time.Sleep(300 * time.Millisecond)
			return
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	req := model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: "x"}}}
	cases := []struct {
		status int
		body   string
		want   []error
	}{
		{401, `{"error":{"message":"missing or wrong API key ` + secret + `","type":"authentication_error"}}`, []error{ErrAuthentication, model.ErrUnavailable}},
		{403, `{}`, []error{ErrAuthentication, model.ErrUnavailable}},
		{429, `{"error":{"type":"rate_limit","code":429}}`, []error{model.ErrUnavailable}},
		{503, ``, []error{model.ErrUnavailable}},
		{408, ``, []error{model.ErrUnavailable}},
		{400, `{"error":{"message":"bad","type":"invalid_request_error"}}`, []error{model.ErrInvalidRequest}},
		{404, ``, []error{model.ErrInvalidRequest}},
		{200, `{"choices":[]}`, []error{ErrMalformed, model.ErrUnavailable}},
		{200, `not json`, []error{ErrMalformed, model.ErrUnavailable}},
		{200, `{"choices":[{"message":{"role":"assistant","content":"` + strings.Repeat("x", 2048) + `"}}]}`, []error{ErrMalformed}},
		{-1, ``, []error{model.ErrUnavailable, context.DeadlineExceeded}},
	}
	c, err := New(Config{BaseURL: srv.URL, Model: "m", APIKey: secret, Timeout: 100 * time.Millisecond, MaxResponseBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		status, body = tc.status, tc.body
		_, err := c.Complete(context.Background(), req)
		for _, w := range tc.want {
			if !errors.Is(err, w) {
				t.Errorf("status %d: %v is not %v", tc.status, err, w)
			}
		}
		if err != nil && (strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "missing or wrong")) {
			t.Errorf("status %d: the error carries the service's text: %v", tc.status, err)
		}
	}
	// Nobody listening.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	down, err := New(Config{BaseURL: "http://" + addr, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = down.Complete(context.Background(), req); !errors.Is(err, model.ErrUnavailable) {
		t.Fatalf("connection refused: %v", err)
	}
	// An invalid request is refused before anything is sent.
	if _, err = c.Complete(context.Background(), model.Request{}); !errors.Is(err, model.ErrInvalidRequest) {
		t.Fatalf("empty request: %v", err)
	}
	// A cancelled caller is not an unavailable model's fault but is reported as unavailable with the
	// context's error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = c.Complete(ctx, req); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}

func TestConfigIsChecked(t *testing.T) {
	for _, cfg := range []Config{
		{BaseURL: "", Model: "m"},
		{BaseURL: "ftp://host", Model: "m"},
		{BaseURL: "http://", Model: "m"},
		{BaseURL: "http://host?x=1", Model: "m"},
		{BaseURL: "http://user:pw@host", Model: "m"},
		{BaseURL: "http://host", Model: ""},
		{BaseURL: "http://host", Model: "m", APIKey: "a\nb"},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%+v accepted", cfg)
		}
	}
	if WireName("cost_scenario.save_draft") != "cost_scenario__save_draft" || WireName("quote.response.record") != "quote__response__record" {
		t.Fatal(WireName("quote.response.record"))
	}
}
