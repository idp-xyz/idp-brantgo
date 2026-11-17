package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// api is a black-box HTTP client of Brant's API (the closed command envelope POST
// /api/brant/v1/{bc}/commands/{command}, the task-step route and the GET reads), acting with access
// tokens of the local issuer.
type api struct {
	base, tenant, company string
	hc                    *http.Client
	is                    *issuer
	keys                  *atomic.Int64
	prefix                string // idempotency key prefix of the run
}

// actor is one identity acting on the API: a human, the AI agent or a system actor.
type actor struct {
	a        *api
	id, kind string
	n        *atomic.Int64 // requests sent (per trial counters read it)
}

func (a *api) as(id, kind string) *actor { return &actor{a: a, id: id, kind: kind, n: &atomic.Int64{}} }

// refusal is an answer other than success.
type refusal struct {
	Status int
	Code   string
	Path   string
	Body   map[string]any
}

func (r *refusal) Error() string {
	b, _ := json.Marshal(r.Body)
	if len(b) > 800 {
		b = append(b[:800], "..."...)
	}
	return fmt.Sprintf("HTTP %d %s: %s", r.Status, r.Code, b)
}

// codeOf returns the code of a refusal, "" for nil and "TRANSPORT" for any other error.
func codeOf(err error) string {
	if err == nil {
		return ""
	}
	var r *refusal
	if errors.As(err, &r) {
		return r.Code
	}
	return "TRANSPORT"
}

func (a *api) key() string { return a.prefix + strconv.FormatInt(a.keys.Add(1), 10) }

// do sends one request; any HTTP answer is returned with its decoded JSON body.
func (c *actor) do(ctx context.Context, method, path string, body any) (int, map[string]any, error) {
	token, err := c.a.is.token(c.a.tenant, c.a.company, c.id, c.kind)
	if err != nil {
		return 0, nil, err
	}
	var payload io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		payload = bytes.NewReader(b)
	}
	rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, method, c.a.base+path, payload)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	c.n.Add(1)
	resp, err := c.a.hc.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	var out map[string]any
	if len(raw) > 0 {
		if err = json.Unmarshal(raw, &out); err != nil {
			return resp.StatusCode, map[string]any{"code": "NOT_JSON", "body": string(raw)}, nil
		}
	}
	return resp.StatusCode, out, nil
}

func answer(status int, out map[string]any, err error) (map[string]any, error) {
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK || out["ok"] != true {
		r := &refusal{Status: status, Body: out}
		r.Code, _ = out["code"].(string)
		if r.Code == "" {
			r.Code, _ = out["transport_error"].(string) // 503 UNAVAILABLE or COMMIT_UNCERTAIN
		}
		r.Path = str(dig(out, "field_errors", 0, "path"))
		return nil, r
	}
	return out, nil
}

// cmd posts command of bc under a fresh idempotency key and requires "ok": true.
func (c *actor) cmd(ctx context.Context, bc, contract, command string, args map[string]any) (map[string]any, error) {
	return answer(c.do(ctx, http.MethodPost, "/api/brant/v1/"+bc+"/commands/"+command,
		map[string]any{"contract": contract, "command": command, "idempotency_key": c.a.key(), "args": args}))
}

// step sends a person's step on task through the task-step route (D-2): no idempotency key, the
// business intent names the action (the same intent to retry).
func (c *actor) step(ctx context.Context, task, bc, contract, command, intent string, args map[string]any) (map[string]any, error) {
	return answer(c.do(ctx, http.MethodPost, "/api/brant/v1/tsk/tasks/"+task+"/steps/"+bc+"/"+command,
		map[string]any{"contract": contract, "command": command, "business_intent_id": intent, "args": args}))
}

// get reads path and requires 200 with "ok": true.
func (c *actor) get(ctx context.Context, path string) (map[string]any, error) {
	return answer(c.do(ctx, http.MethodGet, path, nil))
}

// dig walks v by map keys (string) and array indexes (int); nil when the path is missing.
func dig(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				return nil
			}
			v = m[k]
		case int:
			a, ok := v.([]any)
			if !ok || k < 0 || k >= len(a) {
				return nil
			}
			v = a[k]
		}
	}
	return v
}

func str(v any) string { s, _ := v.(string); return s }

func num(v any) int64 { f, _ := v.(float64); return int64(f) }

func list(v any) []any { a, _ := v.([]any); return a }
