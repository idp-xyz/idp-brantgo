//go:build eerlive

package eerlive

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/ai/adapters/eer"
	"go.idp.xyz/idp-brant/internal/ai/runtime"
)

// Defaults of the plan the live runs drive.
const (
	DefaultPlan      = "address-error-actions"
	DefaultPolicy    = "address-exception-policy"
	DefaultEventType = "parcel.address.rejected"
)

// Config returns the live EER client configuration from the environment. Missing settings fail the
// test (a live run that cannot run is a failure under make eer-live, never a skip); CI=true fails
// at once: live runs never run in CI.
func Config(t testing.TB) eer.Config {
	t.Helper()
	if os.Getenv("CI") == "true" {
		t.Fatal("live EER runs never run under CI=true")
	}
	need := func(k string) string {
		v := os.Getenv(k)
		if v == "" {
			t.Fatalf("%s is not set: the live EER run cannot run (see package eerlive)", k)
		}
		return v
	}
	hc := &http.Client{Timeout: 30 * time.Second}
	if ca := os.Getenv("IDP_BRANT_EER_CA_FILE"); ca != "" {
		pem, err := os.ReadFile(ca)
		if err != nil {
			t.Fatalf("IDP_BRANT_EER_CA_FILE: %v", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			t.Fatal("IDP_BRANT_EER_CA_FILE holds no certificate")
		}
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		hc.Transport = tr
	}
	var tokens eer.TokenSource
	if tok := os.Getenv("IDP_BRANT_EER_TOKEN"); tok != "" {
		tokens = eer.StaticToken(tok)
	} else if user := os.Getenv("IDP_BRANT_EER_USERNAME"); user != "" {
		tokens = &Password{TokenURL: need("IDP_BRANT_EER_TOKEN_URL"), ClientID: need("IDP_BRANT_EER_CLIENT_ID"),
			ClientSecret: need("IDP_BRANT_EER_CLIENT_SECRET"), Username: user, Password: need("IDP_BRANT_EER_PASSWORD"), HTTPClient: hc}
	} else {
		tokens = &eer.ClientCredentials{TokenURL: need("IDP_BRANT_EER_TOKEN_URL"), ClientID: need("IDP_BRANT_EER_CLIENT_ID"),
			ClientSecret: need("IDP_BRANT_EER_CLIENT_SECRET"), Audience: os.Getenv("IDP_BRANT_EER_AUDIENCE"), HTTPClient: hc}
	}
	return eer.Config{BaseURL: need("IDP_BRANT_EER_URL"), TenantID: need("IDP_BRANT_EER_TENANT"), ActorID: need("IDP_BRANT_EER_ACTOR"),
		Tokens: tokens, PolicyID: or(os.Getenv("IDP_BRANT_EER_POLICY"), DefaultPolicy), PolicyVersion: 1,
		EventType: or(os.Getenv("IDP_BRANT_EER_EVENT_TYPE"), DefaultEventType), HTTPClient: hc}
}

// Client returns a live client for the environment's configuration, with hc as its HTTP client
// when given (the lost-answer transport of the unknown runs).
func Client(t testing.TB, hc *http.Client) *eer.Client {
	t.Helper()
	cfg := Config(t)
	if hc != nil {
		if base, ok := cfg.HTTPClient.Transport.(*http.Transport); ok && hc.Transport == nil {
			hc.Transport = base
		}
		cfg.HTTPClient = hc
	}
	c, err := eer.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Submission is a synthetic submission of the plan the live runs drive for operation op: an
// approval wait first (T2) when approval, the pilot plan's inputs with a synthetic parcel, recorded
// at at, with a 15-minute deadline so an execution a failed run leaves behind ends by itself.
func Submission(op, taskID string, approval bool, at time.Time) runtime.Submission {
	at = at.UTC().Truncate(time.Microsecond)
	parcel := "BRANT-" + op[len(op)-12:]
	return runtime.Submission{OperationID: op, TaskID: taskID, Objective: "IDP Brant S4 D2 live run (synthetic)",
		Plan: or(os.Getenv("IDP_BRANT_EER_PLAN"), DefaultPlan), PlanVersion: 1, ApprovalRequired: approval, Subject: "parcel:" + parcel,
		Inputs: map[string]any{"context": map[string]any{"parcel": map[string]any{"parcel_id": parcel}},
			"request": map[string]any{"oms_action_cost": 0, "reissue_cost": 0, "fail_mode": "none"}},
		RequestedAt: at, Deadline: at.Add(15 * time.Minute)}
}

// Await reads op until ok holds or timeout passes, and returns the last reading.
func Await(ctx context.Context, t testing.TB, p runtime.Port, op string, timeout time.Duration, ok func(runtime.Operation) bool) runtime.Operation {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		got, err := p.Lookup(ctx, op)
		if err != nil {
			t.Fatalf("lookup %s: %v", op, err)
		}
		if ok(got) {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation %s still %s (%s) after %s", op, got.State, got.RuntimeState, timeout)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// Password is the OAuth 2.0 password grant of a development identity (the TELOS acceptance realm
// maps only people to EER). The password and tokens never appear in an error or a String.
type Password struct {
	TokenURL, ClientID, ClientSecret, Username, Password string
	HTTPClient                                           *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

// Token returns a cached token, or a new one.
func (p *Password) Token(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.token != "" && time.Now().Before(p.expires) {
		return p.token, nil
	}
	if p.Username == "" || p.Password == "" {
		return "", errors.New("eerlive: no development identity")
	}
	tok, ttl, err := eer.FetchToken(ctx, p.HTTPClient, p.TokenURL, url.Values{"grant_type": {"password"}, "client_id": {p.ClientID},
		"client_secret": {p.ClientSecret}, "username": {p.Username}, "password": {p.Password}})
	if err != nil {
		return "", err
	}
	p.token, p.expires = tok, time.Now().Add(ttl)
	return tok, nil
}

// String hides the credentials.
func (p *Password) String() string { return "eerlive.Password{" + p.TokenURL + " " + p.Username + "}" }

// GoString hides the credentials.
func (p *Password) GoString() string { return p.String() }

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
