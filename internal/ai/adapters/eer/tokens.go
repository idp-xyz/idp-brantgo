package eer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ClientCredentials obtains EER tokens from an OAuth 2.0 token endpoint with the client credentials
// grant (a service account of Brant that EER maps to its tenant and actor), and caches each token
// until shortly before it expires. The secret and the tokens never appear in an error or a String.
type ClientCredentials struct {
	TokenURL     string
	ClientID     string
	ClientSecret string
	// Audience, when set, is requested (Keycloak: the EER audience "eer-runtime").
	Audience   string
	HTTPClient *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

// Token returns a cached token, or a new one.
func (c *ClientCredentials) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.expires) {
		return c.token, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {c.ClientID}, "client_secret": {c.ClientSecret}}
	if c.Audience != "" {
		form.Set("audience", c.Audience)
	}
	tok, ttl, err := FetchToken(ctx, c.HTTPClient, c.TokenURL, form)
	if err != nil {
		return "", err
	}
	c.token, c.expires = tok, time.Now().Add(ttl)
	return tok, nil
}

// String hides the secret.
func (c *ClientCredentials) String() string {
	return "eer.ClientCredentials{" + c.TokenURL + " client " + c.ClientID + "}"
}

// GoString hides the secret.
func (c *ClientCredentials) GoString() string { return c.String() }

// FetchToken posts form to an OAuth 2.0 token endpoint and returns the access token and how long to
// use it (its lifetime less a margin). Errors name the endpoint and the status, never the form, the
// answer's body or the token.
func FetchToken(ctx context.Context, hc *http.Client, tokenURL string, form url.Values) (string, time.Duration, error) {
	if hc == nil {
		hc = &http.Client{Timeout: DefaultTimeout}
	}
	if u, err := url.Parse(tokenURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", 0, fmt.Errorf("eer: token endpoint %q", tokenURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("eer: token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return "", 0, fmt.Errorf("eer: token endpoint %s: %w", tokenURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", 0, fmt.Errorf("eer: token endpoint %s: %w", tokenURL, err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("eer: token endpoint %s answered %d", tokenURL, resp.StatusCode)
	}
	var doc struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err = json.Unmarshal(raw, &doc); err != nil || doc.AccessToken == "" || !strings.EqualFold(doc.TokenType, "bearer") {
		return "", 0, fmt.Errorf("eer: token endpoint %s gave no bearer token", tokenURL)
	}
	ttl := time.Duration(doc.ExpiresIn)*time.Second - 30*time.Second
	if ttl < 5*time.Second {
		ttl = 5 * time.Second
	}
	return doc.AccessToken, ttl, nil
}
