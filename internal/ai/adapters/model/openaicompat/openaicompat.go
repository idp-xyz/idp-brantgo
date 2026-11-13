// Package openaicompat is the model.Port of an OpenAI-compatible chat completions service over plain
// net/http (owner decisions E-3/E-4 of 2026-11-12: the local Strata service at
// http://127.0.0.1:8080, model qwen3.8-flash-next-iq3_xxs, cost 0; no new dependency; the scripted
// model stays the default and the only one in CI). It sends the conversation and the offered tools
// (POST <base>/v1/chat/completions, "tools" of type function) and returns the assistant's text and
// tool calls, the model ID the service returned, the token usage it reported and the latency.
//
// The service may require a bearer key: Config.APIKey, which the caller reads from the
// environment. The key is never written to a log, an error or a String form; it lives only in the
// Authorization header of the requests.
//
// Function names of the chat completions contract allow [a-zA-Z0-9_-]; a command name's dots are
// sent as "__" and mapped back through the request's own tool list, so a name the model invents is
// returned as the model wrote it and refused by the gateway.
package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.idp.xyz/idp-brant/internal/ai/adapters/model"
)

// Defaults of a Config.
const (
	DefaultTimeout          = 120 * time.Second
	DefaultMaxResponseBytes = 8 << 20
	// KeyEnv is the environment variable brant-worker reads the optional bearer key from.
	KeyEnv = "IDP_BRANT_MODEL_API_KEY"
)

// Config configures a Client.
type Config struct {
	// BaseURL is the service, such as http://127.0.0.1:8080 or https://host/v1; the path
	// /v1/chat/completions is appended (/chat/completions when it already ends in /v1).
	BaseURL string
	// Model is the model the requests ask for; the answer names the model that served it.
	Model string
	// APIKey, when set, is sent as a bearer token. Never logged.
	APIKey string
	// Timeout bounds one call, connection and answer included (DefaultTimeout when zero).
	Timeout time.Duration
	// MaxResponseBytes bounds the answer read (DefaultMaxResponseBytes when zero).
	MaxResponseBytes int64
	// HTTPClient, when set, sends the requests (its own Timeout applies too).
	HTTPClient *http.Client
}

// Client is the port of one service and model.
type Client struct {
	endpoint string
	model    string
	key      string
	timeout  time.Duration
	limit    int64
	http     *http.Client
}

var _ model.Port = (*Client)(nil)

// New checks cfg and returns its client. The base URL must be http or https with a host.
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, errors.New("openaicompat: the base URL is an http or https URL with a host and nothing after its path")
	}
	if strings.TrimSpace(cfg.Model) == "" || len(cfg.Model) > 200 {
		return nil, errors.New("openaicompat: a model is required")
	}
	if strings.ContainsAny(cfg.APIKey, "\r\n") {
		return nil, errors.New("openaicompat: the API key is not a header value")
	}
	path := "/v1/chat/completions"
	if strings.HasSuffix(u.Path, "/v1") {
		path = "/chat/completions"
	}
	c := &Client{endpoint: u.String() + path, model: cfg.Model, key: cfg.APIKey, timeout: cfg.Timeout, limit: cfg.MaxResponseBytes,
		http: cfg.HTTPClient}
	if c.timeout <= 0 {
		c.timeout = DefaultTimeout
	}
	if c.limit <= 0 {
		c.limit = DefaultMaxResponseBytes
	}
	if c.http == nil {
		c.http = &http.Client{}
	}
	return c, nil
}

// String names the endpoint and model, never the key.
func (c *Client) String() string {
	return fmt.Sprintf("openaicompat(%s, model %s, key set: %t)", c.endpoint, c.model, c.key != "")
}

// GoString is String, so %#v does not print the key either.
func (c *Client) GoString() string { return c.String() }

// Errors of a call besides model.ErrUnavailable and model.ErrInvalidRequest, each wrapping one of
// them.
var (
	// ErrAuthentication: the service refused the credentials (401, 403). Retrying does not help;
	// it wraps model.ErrUnavailable, so the task pauses once its retries are used.
	ErrAuthentication = fmt.Errorf("%w: the service refused the credentials", model.ErrUnavailable)
	// ErrMalformed: the answer is not a chat completion.
	ErrMalformed = fmt.Errorf("%w: the answer is not a chat completion", model.ErrUnavailable)
)

type wireFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type wireTool struct {
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireCallFunction struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type wireCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function wireCallFunction `json:"function"`
}

type wireMessage struct {
	Role       string     `json:"role"`
	Content    *string    `json:"content"`
	ToolCalls  []wireCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type wireRequest struct {
	Model     string        `json:"model"`
	Messages  []wireMessage `json:"messages"`
	Tools     []wireTool    `json:"tools,omitempty"`
	MaxTokens int           `json:"max_tokens,omitempty"`
	Stream    bool          `json:"stream"`
}

type wireResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message      wireMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage"`
}

type wireError struct {
	Error struct {
		Type string `json:"type"`
		Code any    `json:"code"`
	} `json:"error"`
}

// WireName is the function name a tool name is sent as.
func WireName(name string) string { return strings.ReplaceAll(name, ".", "__") }

// Complete sends one turn.
func (c *Client) Complete(ctx context.Context, req model.Request) (model.Response, error) {
	if err := req.Validate(); err != nil {
		return model.Response{}, err
	}
	names := map[string]string{}
	body := wireRequest{Model: c.model, MaxTokens: req.MaxOutputTokens}
	for _, t := range req.Tools {
		wire := WireName(t.Name)
		names[wire] = t.Name
		params := t.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object"}`)
		}
		body.Tools = append(body.Tools, wireTool{Type: "function", Function: wireFunction{Name: wire, Description: t.Description, Parameters: params}})
	}
	for _, m := range req.Messages {
		body.Messages = append(body.Messages, toWire(m))
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return model.Response{}, fmt.Errorf("%w: %w", model.ErrInvalidRequest, err)
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(raw))
	if err != nil {
		return model.Response{}, fmt.Errorf("%w: %w", model.ErrInvalidRequest, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if c.key != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.key)
	}
	start := time.Now()
	resp, err := c.http.Do(httpReq)
	if err != nil {
		// Connection refused, reset, DNS, TLS or the timeout: the model cannot answer now.
		return model.Response{}, fmt.Errorf("%w: %w", model.ErrUnavailable, sanitize(err, ctx))
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, c.limit+1))
	latency := time.Since(start)
	if err != nil {
		return model.Response{}, fmt.Errorf("%w: reading the answer: %w", model.ErrUnavailable, sanitize(err, ctx))
	}
	if int64(len(data)) > c.limit {
		return model.Response{}, fmt.Errorf("%w: the answer exceeds %d bytes", ErrMalformed, c.limit)
	}
	if resp.StatusCode != http.StatusOK {
		return model.Response{}, statusError(resp.StatusCode, data)
	}
	var out wireResponse
	if err = json.Unmarshal(data, &out); err != nil || len(out.Choices) == 0 {
		return model.Response{}, ErrMalformed
	}
	choice := out.Choices[0]
	msg, err := fromWire(choice.Message, names)
	if err != nil {
		return model.Response{}, err
	}
	r := model.Response{Model: out.Model, Message: msg, FinishReason: choice.FinishReason, Latency: latency}
	if r.Model == "" {
		r.Model = c.model // the service did not say; the request's model is recorded
	}
	if r.FinishReason == "" {
		r.FinishReason = "stop"
		if len(msg.ToolCalls) > 0 {
			r.FinishReason = "tool_calls"
		}
	}
	if out.Usage != nil {
		r.Usage = model.Usage{InputTokens: max(out.Usage.PromptTokens, 0), OutputTokens: max(out.Usage.CompletionTokens, 0)}
	}
	return r, nil
}

func toWire(m model.Message) wireMessage {
	w := wireMessage{Role: string(m.Role), ToolCallID: m.ToolCallID}
	if m.Content != "" || len(m.ToolCalls) == 0 {
		content := m.Content
		w.Content = &content
	}
	for _, tc := range m.ToolCalls {
		args, _ := json.Marshal(string(tc.Arguments)) // the contract's arguments are a JSON string
		w.ToolCalls = append(w.ToolCalls, wireCall{ID: tc.ID, Type: "function", Function: wireCallFunction{Name: WireName(tc.Name), Arguments: args}})
	}
	return w
}

// fromWire reads the assistant's message. Arguments arrive as a JSON string holding the JSON
// object (the contract) or, from some services, as the object itself; anything else is passed on as
// a JSON string, which the gateway refuses (SCHEMA_INVALID).
func fromWire(w wireMessage, names map[string]string) (model.Message, error) {
	m := model.Message{Role: model.RoleAssistant}
	if w.Content != nil {
		m.Content = *w.Content
	}
	for i, tc := range w.ToolCalls {
		name, ok := names[tc.Function.Name]
		if !ok {
			name = tc.Function.Name
		}
		id := tc.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", i)
		}
		m.ToolCalls = append(m.ToolCalls, model.ToolCall{ID: id, Name: name, Arguments: arguments(tc.Function.Arguments)})
	}
	return m, nil
}

func arguments(raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	var s string
	if len(trimmed) > 0 && trimmed[0] == '"' && json.Unmarshal(trimmed, &s) == nil {
		if json.Valid([]byte(s)) {
			return json.RawMessage(s)
		}
		quoted, _ := json.Marshal(s)
		return quoted
	}
	if len(trimmed) > 0 && json.Valid(trimmed) {
		return json.RawMessage(bytes.Clone(trimmed))
	}
	quoted, _ := json.Marshal(string(raw))
	return quoted
}

// statusError maps a non-200 answer: 401/403 ErrAuthentication; 408, 429 and 5xx
// model.ErrUnavailable; other 4xx model.ErrInvalidRequest. Only the status and the error type and
// code the service gave are kept (an error message may echo the request).
func statusError(status int, body []byte) error {
	var we wireError
	detail := ""
	if json.Unmarshal(body, &we) == nil && we.Error.Type != "" {
		detail = " " + bounded(we.Error.Type, 60)
		if we.Error.Code != nil {
			detail += fmt.Sprintf(" (%v)", we.Error.Code)
		}
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Errorf("%w: status %d%s", ErrAuthentication, status, detail)
	case status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500:
		return fmt.Errorf("%w: status %d%s", model.ErrUnavailable, status, detail)
	}
	return fmt.Errorf("%w: status %d%s", model.ErrInvalidRequest, status, detail)
}

// sanitize keeps the cause of a transport error without the request URL's query or anything else
// the error string may carry; context errors stay matchable with errors.Is.
func sanitize(err error, ctx context.Context) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return context.DeadlineExceeded
		}
		return ue.Err
	}
	return err
}

func bounded(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
