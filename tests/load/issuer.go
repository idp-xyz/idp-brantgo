package main

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// issuer is a local OpenID Connect issuer on loopback: discovery, a JWKS with one RSA key, and a
// signer for RFC 9068 access tokens carrying Brant's claims (brant_tenant, brant_company,
// brant_actor_kind). No real identity provider is connected before the B0 pilot gate; the tests of
// internal/bootstrap use the same shape.
type issuer struct {
	url    string
	srv    *http.Server
	signer jose.Signer

	mu     sync.Mutex
	tokens map[string]cachedToken
}

type cachedToken struct {
	raw     string
	renewAt time.Time
}

// tokenLifetime is how long an access token is valid; a client renews it at half its lifetime,
// as an OAuth client reuses its token instead of asking for one per request.
const tokenLifetime = 15 * time.Minute

func newIssuer() (*issuer, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("at+jwt").WithHeader("kid", "load-key"))
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	is := &issuer{url: "http://" + ln.Addr().String(), signer: signer, tokens: map[string]cachedToken{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": is.url, "jwks_uri": is.url + "/jwks"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "load-key", Algorithm: "RS256", Use: "sig"}}})
	})
	is.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = is.srv.Serve(ln) }()
	return is, nil
}

func (is *issuer) close() { _ = is.srv.Close() }

// token returns a valid access token for a human actor in tenant and company.
func (is *issuer) token(tenant, company, actor string) (string, error) {
	key := tenant + "\x00" + company + "\x00" + actor
	now := time.Now()
	is.mu.Lock()
	defer is.mu.Unlock()
	if c, ok := is.tokens[key]; ok && now.Before(c.renewAt) {
		return c.raw, nil
	}
	raw, err := jwt.Signed(is.signer).Claims(jwt.Claims{Issuer: is.url, Subject: actor, Audience: jwt.Audience{audience},
		Expiry: jwt.NewNumericDate(now.Add(tokenLifetime)), IssuedAt: jwt.NewNumericDate(now)}).
		Claims(map[string]any{"brant_tenant": tenant, "brant_company": company, "brant_actor_kind": "human"}).Serialize()
	if err != nil {
		return "", fmt.Errorf("sign a token for %s: %w", actor, err)
	}
	is.tokens[key] = cachedToken{raw: raw, renewAt: now.Add(tokenLifetime / 2)}
	return raw, nil
}
