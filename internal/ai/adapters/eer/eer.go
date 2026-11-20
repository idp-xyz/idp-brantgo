// Package eer is the anti-corruption layer to EER, the Enterprise Execution Runtime of the IDP TELOS
// platform (WBS S4-05; BC-D08; decision E-2; Context Map externalBoundaries "EER（AI 执行运行时候选）→
// internal/ai, ACL / Adapter"). It is a client written by hand against EER's documented Runtime API v1
// (intents, executions with transitions, audit and Actor invocations, approval, cancellation, human
// tasks) over net/http: no EER code, contract file or fixture is part of this repository (EER is
// private and PolyForm Noncommercial).
//
// Client implements runtime.Port (internal/ai/runtime). What Brant reads from EER is translated into
// Brant's terms at this boundary and nowhere else:
//
//   - an operation is an EER execution under "execution:<operation ID>", with intent
//     "intent:<operation ID>" and the originating event "event:<operation ID>" that Brant publishes
//     first (EER admits no intent without one); every field of both requests comes from the
//     runtime.Submission, so a resubmission is byte-for-byte the same request and EER answers it as an
//     idempotent replay, and the same ID with other content is runtime.ErrConflict
//     (IDEMPOTENCY_PAYLOAD_CONFLICT);
//   - EER states: Created, Planned, Scheduled → accepted; Executing, Recovering → running; Waiting
//     whose last transition is approval_required → waiting_approval, other Waiting → waiting;
//     Failed → failed (EER may still recover it); Completed → completed, or cancelled when its outcome
//     is cancelled. A state EER adds later is refused (runtime.ErrInvalid), never guessed;
//   - approval needs runtime.Submission.ApprovalRequired: disposition tier T2, else T1; the task's
//     deadline is the intent's constraints.deadline;
//   - a write whose answer is lost after the request may have reached EER is runtime.ErrUnknown and
//     is settled by Lookup (runtime.ReconcileSubmission); a request that never left (dial failure) or
//     a failed read is runtime.ErrUnavailable;
//   - EER's 401/403 are runtime.ErrDenied: EER maps the token's subject to its tenant and actor, and
//     admits approvals and cancellations only from the actor the token maps to (Config.ActorID).
//
// EER never fences Brant's business writes and owns no quote, order or account truth (the Context
// Map's boundary): its approvals are not TRD approvals or AI approval grants, and its cancellation
// stops EER's own steps only. The token, the client secret and the response bodies of token requests
// never appear in an error, a log or a String of this package.
package eer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.idp.xyz/idp-brant/internal/ai/runtime"
)

// Defaults of a Config.
const (
	DefaultTimeout          = 30 * time.Second
	DefaultMaxResponseBytes = 4 << 20
	// DefaultEventSource is the source entity of the events Brant publishes to EER.
	DefaultEventSource = "system:idp-brant"
)

// Config configures a Client.
type Config struct {
	// BaseURL is EER's Runtime API (http(s)://host:port, no path).
	BaseURL string
	// TenantID is the EER tenant the token's principal maps to; EER takes the tenant from the token
	// and refuses a body that names another.
	TenantID string
	// ActorID is the EER actor the token's principal maps to: approvals and cancellations are
	// recorded under it.
	ActorID string
	// Tokens supplies the bearer token of every request.
	Tokens TokenSource
	// PolicyID and PolicyVersion are the intent policy Brant's operations run under at EER.
	PolicyID      string
	PolicyVersion int
	// EventType is the type of the originating event Brant publishes (one the plans support), and
	// EventSource its source entity (default DefaultEventSource).
	EventType   string
	EventSource string
	Timeout     time.Duration
	// MaxResponseBytes bounds a response body (default DefaultMaxResponseBytes).
	MaxResponseBytes int64
	// HTTPClient sends the requests (default: a client with Timeout).
	HTTPClient *http.Client
}

// Client is EER's Runtime API v1 as Brant uses it.
type Client struct {
	cfg  Config
	base *url.URL
	http *http.Client
}

var _ runtime.Port = (*Client)(nil)

// New checks cfg and returns a client.
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/"):
		return nil, fmt.Errorf("%w: EER base URL %q", runtime.ErrInvalid, cfg.BaseURL)
	case !tenantPattern(cfg.TenantID):
		return nil, fmt.Errorf("%w: EER tenant %q", runtime.ErrInvalid, cfg.TenantID)
	case strings.TrimSpace(cfg.ActorID) == "":
		return nil, fmt.Errorf("%w: the EER actor of the credentials", runtime.ErrInvalid)
	case cfg.Tokens == nil:
		return nil, fmt.Errorf("%w: a token source", runtime.ErrInvalid)
	case strings.TrimSpace(cfg.PolicyID) == "" || cfg.PolicyVersion < 1:
		return nil, fmt.Errorf("%w: the intent policy", runtime.ErrInvalid)
	case !eventTypePattern(cfg.EventType):
		return nil, fmt.Errorf("%w: event type %q", runtime.ErrInvalid, cfg.EventType)
	}
	if cfg.EventSource == "" {
		cfg.EventSource = DefaultEventSource
	}
	if !entityPattern(cfg.EventSource) {
		return nil, fmt.Errorf("%w: event source %q", runtime.ErrInvalid, cfg.EventSource)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.MaxResponseBytes <= 0 {
		cfg.MaxResponseBytes = DefaultMaxResponseBytes
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: cfg.Timeout}
	}
	u.Path = ""
	return &Client{cfg: cfg, base: u, http: hc}, nil
}

// String names the client without its credentials.
func (c *Client) String() string {
	return "eer.Client{" + c.base.String() + " tenant " + c.cfg.TenantID + " actor " + c.cfg.ActorID + "}"
}

// GoString is String.
func (c *Client) GoString() string { return c.String() }

// Error is EER's error answer: the HTTP status and EER's error code and message.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("eer: %d %s: %s", e.Status, e.Code, e.Message)
}

// call sends one request and decodes a 2xx answer into out (nil: discard). write marks a request
// that changes something at EER: an answer lost after it may have arrived is runtime.ErrUnknown.
func (c *Client) call(ctx context.Context, method, path string, body, out any, write bool) (int, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("%w: %w", runtime.ErrInvalid, err)
		}
		reader = bytes.NewReader(raw)
	}
	token, err := c.cfg.Tokens.Token(ctx)
	if err != nil {
		return 0, fmt.Errorf("%w: token: %w", runtime.ErrDenied, err)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base.String()+path, reader)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", runtime.ErrInvalid, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, transportError(method, path, err, write)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, c.cfg.MaxResponseBytes+1))
	if err != nil {
		return resp.StatusCode, transportError(method, path, err, write)
	}
	if int64(len(raw)) > c.cfg.MaxResponseBytes {
		return resp.StatusCode, fmt.Errorf("%w: eer %s %s: the answer exceeds %d bytes", runtime.ErrInvalid, method, path, c.cfg.MaxResponseBytes)
	}
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, statusError(method, path, resp.StatusCode, raw, write)
	}
	if out != nil {
		if err = json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("%w: eer %s %s: malformed answer: %w", runtime.ErrInvalid, method, path, err)
		}
	}
	return resp.StatusCode, nil
}

// transportError classifies a request that got no complete answer: a dial failure never left
// (unavailable); anything else may have reached EER, so a write's outcome is unknown.
func transportError(method, path string, err error, write bool) error {
	var op *net.OpError
	if !write || (errors.As(err, &op) && op.Op == "dial") {
		return fmt.Errorf("%w: eer %s %s: %w", runtime.ErrUnavailable, method, path, err)
	}
	return fmt.Errorf("%w: eer %s %s: %w", runtime.ErrUnknown, method, path, err)
}

// statusError maps EER's error answer.
func statusError(method, path string, status int, raw []byte, write bool) error {
	var doc struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &doc)
	e := &Error{Status: status, Code: doc.Error.Code, Message: doc.Error.Message}
	if e.Code == "" {
		e.Code = http.StatusText(status)
	}
	var sentinel error
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		sentinel = runtime.ErrDenied
	case status == http.StatusConflict && (e.Code == "execution_identity_conflict" || e.Code == "event_identity_conflict"):
		sentinel = runtime.ErrConflict
	case status == http.StatusConflict && e.Code == "approval_state_conflict":
		sentinel = runtime.ErrNotWaiting
	case status == http.StatusNotFound:
		sentinel = errNotFound
	case status >= 500 && write:
		// EER may have recorded part of the request before failing (baseline 17.2: 不把超时当未发生).
		sentinel = runtime.ErrUnknown
	case status >= 500 || status == http.StatusTooManyRequests:
		sentinel = runtime.ErrUnavailable
	case status == http.StatusConflict:
		sentinel = errStateConflict
	default:
		sentinel = runtime.ErrInvalid
	}
	return fmt.Errorf("%w: %s %s: %w", sentinel, method, path, e)
}

var (
	// errNotFound is EER's 404 (an execution, human task or plan it does not have).
	errNotFound = errors.New("eer: not found")
	// errStateConflict is another 409 (a cancellation of a Completed execution).
	errStateConflict = errors.New("eer: state conflict")
)

// IsNotFound reports whether err is EER's 404.
func IsNotFound(err error) bool { return errors.Is(err, errNotFound) }

func tenantPattern(s string) bool {
	if len(s) == 0 || len(s) > 128 || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if c := s[i]; (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// eventTypePattern is EER's event type: two to five dot-separated lower-case segments.
func eventTypePattern(s string) bool {
	parts := strings.Split(s, ".")
	if len(s) > 128 || len(parts) < 2 || len(parts) > 5 {
		return false
	}
	for _, p := range parts {
		if len(p) == 0 || p[0] < 'a' || p[0] > 'z' {
			return false
		}
		for i := 1; i < len(p); i++ {
			if c := p[i]; (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
				return false
			}
		}
	}
	return true
}

// entityPattern is EER's entity reference "<type>:<id>".
func entityPattern(s string) bool {
	typ, id, ok := strings.Cut(s, ":")
	if !ok || id == "" || len(typ) == 0 || typ[0] < 'a' || typ[0] > 'z' {
		return false
	}
	for i := 1; i < len(typ); i++ {
		if c := typ[i]; (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

// TokenSource supplies bearer tokens for EER.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// StaticToken is a fixed bearer token.
type StaticToken string

// Token returns the token.
func (t StaticToken) Token(context.Context) (string, error) {
	if t == "" {
		return "", errors.New("eer: empty token")
	}
	return string(t), nil
}

// String hides the token.
func (StaticToken) String() string { return "eer.StaticToken{***}" }

// GoString hides the token.
func (t StaticToken) GoString() string { return t.String() }
