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
	"sync"
	"sync/atomic"
	"time"
)

// Request classes: the WBS S5-11 candidates distinguish ordinary reads (P95 500 ms) from business
// commands (P95 800 ms); setup requests are recorded apart and never enter a stage.
const (
	classRead    = "read"
	classCommand = "command"
	classSetup   = "setup"
)

// Outcomes of one request. Latency percentiles are taken over "ok" answers; errors and timeouts
// are counted apart (baseline 11.1: 错误率与超时分开).
const (
	outcomeOK        = "ok"
	outcomeTimeout   = "timeout"
	outcomeTransport = "transport"
)

// client is a black-box HTTP client of Brant's API: the closed command envelope
// (POST /api/brant/v1/{bc}/commands/{command}) and the GET read routes, each with the acting
// human's access token from the local issuer.
type client struct {
	base, tenant, company string
	hc                    *http.Client
	is                    *issuer
	rec                   *recorder
	timeout               time.Duration
	keys                  atomic.Int64

	ackMu sync.Mutex
	ack   io.Writer // when set, every acknowledged command is appended as a JSON line (drill writes)
}

// refusal is a command or read the API answered with something other than success.
type refusal struct {
	Status int
	Code   string
	Body   map[string]any
}

func (r *refusal) Error() string {
	b, _ := json.Marshal(r.Body)
	if len(b) > 600 {
		b = append(b[:600], "..."...)
	}
	return fmt.Sprintf("HTTP %d %s: %s", r.Status, r.Code, b)
}

// key returns a fresh idempotency key; keys are unique within the run's tenant.
func (c *client) key() string { return "k." + strconv.FormatInt(c.keys.Add(1), 10) }

// do sends one request as actor and records it under class and name. A transport error or a
// timeout is returned as an error; any HTTP answer is returned with its decoded JSON body.
func (c *client) do(ctx context.Context, class, name, actor, method, path string, body any) (int, map[string]any, error) {
	token, err := c.is.token(c.tenant, c.company, actor)
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
	rctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, method, c.base+path, payload)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	status, out, err := c.exchange(req)
	end := time.Now()
	s := sample{end: end, dur: end.Sub(start), class: class, name: name, outcome: outcomeOK}
	switch {
	case err != nil && ctx.Err() != nil:
		return 0, nil, ctx.Err() // the stage ended: neither a result nor an error of the system
	case err != nil && errors.Is(err, context.DeadlineExceeded):
		s.outcome = outcomeTimeout
	case err != nil:
		s.outcome = outcomeTransport
	case status < 200 || status > 299:
		s.outcome = "http_" + strconv.Itoa(status)
		s.code, _ = out["code"].(string)
	}
	c.rec.add(s)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	return status, out, nil
}

func (c *client) exchange(req *http.Request) (int, map[string]any, error) {
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, err
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

// command posts command of bc as actor under a fresh idempotency key and requires "ok": true.
func (c *client) command(ctx context.Context, class, actor, bc, contract, command string, args map[string]any) (map[string]any, error) {
	key := c.key()
	status, out, err := c.do(ctx, class, bc+" "+command, actor, http.MethodPost, "/api/brant/v1/"+bc+"/commands/"+command,
		map[string]any{"contract": contract, "command": command, "idempotency_key": key, "args": args})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK || out["ok"] != true {
		code, _ := out["code"].(string)
		return nil, &refusal{Status: status, Code: code, Body: out}
	}
	if c.ack != nil {
		c.acknowledge(bc, command, key, out)
	}
	return out, nil
}

// acknowledge appends an acknowledged command to the drill's write log: the wall-clock time the
// answer arrived, the context, the command, its idempotency key and the object it wrote.
func (c *client) acknowledge(bc, command, key string, out map[string]any) {
	line, _ := json.Marshal(map[string]any{"at": time.Now().UTC().Format(time.RFC3339Nano), "bc": bc, "command": command,
		"idempotency_key": key, "object_id": out["object_id"]})
	c.ackMu.Lock()
	defer c.ackMu.Unlock()
	_, _ = c.ack.Write(append(line, '\n'))
}

// get reads path as actor and requires 200 with "ok": true.
func (c *client) get(ctx context.Context, class, name, actor, path string) (map[string]any, error) {
	status, out, err := c.do(ctx, class, name, actor, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK || out["ok"] != true {
		code, _ := out["code"].(string)
		return nil, &refusal{Status: status, Code: code, Body: out}
	}
	return out, nil
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
			if !ok || k >= len(a) {
				return nil
			}
			v = a[k]
		}
	}
	return v
}

func str(v any) string { s, _ := v.(string); return s }

func num(v any) int64 { f, _ := v.(float64); return int64(f) }
