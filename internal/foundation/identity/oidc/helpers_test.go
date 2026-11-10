package oidc_test

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"go.idp.xyz/idp-brant/internal/foundation/identity/oidc"
)

const audience = "brant-api"

// start is the fake clock's initial reading.
var start = time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)

// Keys are generated once per test binary; RSA generation is the slow part.
var testKeys = sync.OnceValue(func() (k struct {
	rsa1, rsa2, rsaOther *rsa.PrivateKey
	ec                   *ecdsa.PrivateKey
	ed                   ed25519.PrivateKey
}) {
	must := func(err error) {
		if err != nil {
			panic(err)
		}
	}
	var err error
	k.rsa1, err = rsa.GenerateKey(rand.Reader, 2048)
	must(err)
	k.rsa2, err = rsa.GenerateKey(rand.Reader, 2048)
	must(err)
	k.rsaOther, err = rsa.GenerateKey(rand.Reader, 2048)
	must(err)
	k.ec, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(err)
	_, k.ed, err = ed25519.GenerateKey(rand.Reader)
	must(err)
	return k
})

// signer is a private key with the kid and alg its tokens name.
type signer struct {
	kid string
	alg jose.SignatureAlgorithm
	key any
}

func (s signer) public(use, alg string) jose.JSONWebKey {
	k := jose.JSONWebKey{Key: s.key, KeyID: s.kid, Use: use, Algorithm: alg}
	return k.Public()
}

func rsa1() signer     { return signer{"rsa-1", jose.RS256, testKeys().rsa1} }
func rsa2() signer     { return signer{"rsa-2", jose.RS256, testKeys().rsa2} }
func ecSigner() signer { return signer{"ec-1", jose.ES256, testKeys().ec} }
func edSigner() signer { return signer{"ed-1", jose.EdDSA, testKeys().ed} }

// fakeClock is an application.Clock the test moves by hand.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// issuer is an OpenID Connect provider on an httptest server: discovery and a JWKS it counts.
type issuer struct {
	srv *httptest.Server
	url string

	mu              sync.Mutex
	jwks            []byte
	jwksStatus      int
	discoveryIssuer string
	jwksURI         string
	discovery       []byte // replaces the generated discovery document when set

	discoveryHits, jwksHits atomic.Int64
}

// newIssuer starts an https issuer publishing the public keys of signers.
func newIssuer(t *testing.T, signers ...signer) *issuer {
	return startIssuer(t, httptest.NewTLSServer, signers...)
}

func startIssuer(t *testing.T, newServer func(http.Handler) *httptest.Server, signers ...signer) *issuer {
	t.Helper()
	is := &issuer{jwksStatus: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		is.discoveryHits.Add(1)
		is.mu.Lock()
		body := is.discovery
		if body == nil {
			body, _ = json.Marshal(map[string]string{"issuer": is.discoveryIssuer, "jwks_uri": is.jwksURI})
		}
		is.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		is.jwksHits.Add(1)
		is.mu.Lock()
		status, body := is.jwksStatus, is.jwks
		is.mu.Unlock()
		w.Header().Set("Content-Type", "application/jwk-set+json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	})
	is.srv = newServer(mux)
	t.Cleanup(is.srv.Close)
	is.url = is.srv.URL
	is.discoveryIssuer = is.url
	is.jwksURI = is.url + "/jwks"
	is.publish(signers...)
	return is
}

// publish replaces the key set with the public keys of signers, marked for signatures.
func (is *issuer) publish(signers ...signer) {
	keys := make([]jose.JSONWebKey, 0, len(signers))
	for _, s := range signers {
		keys = append(keys, s.public("sig", ""))
	}
	is.publishKeys(keys...)
}

func (is *issuer) publishKeys(keys ...jose.JSONWebKey) {
	body, err := json.Marshal(jose.JSONWebKeySet{Keys: keys})
	if err != nil {
		panic(err)
	}
	is.publishRaw(body)
}

func (is *issuer) publishRaw(body []byte) {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.jwks = body
}

func (is *issuer) set(f func(is *issuer)) {
	is.mu.Lock()
	defer is.mu.Unlock()
	f(is)
}

func (is *issuer) hits() (discovery, jwks int64) { return is.discoveryHits.Load(), is.jwksHits.Load() }

// config is the Verifier configuration that trusts is and reads clock.
func (is *issuer) config(clock *fakeClock) oidc.Config {
	return oidc.Config{Issuer: is.url, Audience: audience, HTTPClient: is.srv.Client(), Clock: clock}
}

func (is *issuer) verifier(t *testing.T, clock *fakeClock, edit ...func(*oidc.Config)) *oidc.Verifier {
	t.Helper()
	cfg := is.config(clock)
	for _, e := range edit {
		e(&cfg)
	}
	v, err := oidc.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return v
}

// remove marks a claim that claims leaves out.
type removeClaim struct{}

var remove = removeClaim{}

// claims are valid access token claims at now for is, changed by edits.
func (is *issuer) claims(now time.Time, edits map[string]any) map[string]any {
	c := map[string]any{
		"iss":              is.url,
		"aud":              audience,
		"exp":              now.Add(5 * time.Minute).Unix(),
		"iat":              now.Unix(),
		"nbf":              now.Unix(),
		"sub":              "user-1",
		"client_id":        "brant-web",
		"jti":              "token-1",
		"brant_tenant":     "tenant-1",
		"brant_company":    "company-1",
		"brant_actor_kind": "human",
	}
	for k, v := range edits {
		if v == remove {
			delete(c, k)
			continue
		}
		c[k] = v
	}
	return c
}

// mint signs claims with s; headers replace the default "typ": "at+jwt" (nil removes it).
func mint(t *testing.T, s signer, claims map[string]any, headers ...map[jose.HeaderKey]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return mintRaw(t, s, payload, headers...)
}

func mintRaw(t *testing.T, s signer, payload []byte, headers ...map[jose.HeaderKey]any) string {
	t.Helper()
	h := map[jose.HeaderKey]any{jose.HeaderType: "at+jwt"}
	for _, m := range headers {
		for k, v := range m {
			if v == nil {
				delete(h, k)
				continue
			}
			h[k] = v
		}
	}
	opts := &jose.SignerOptions{}
	for k, v := range h {
		opts.WithHeader(k, v)
	}
	var key any = jose.JSONWebKey{Key: s.key, KeyID: s.kid}
	if s.kid == "" {
		key = s.key
	}
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: s.alg, Key: key}, opts)
	if err != nil {
		t.Fatal(err)
	}
	obj, err := sig.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	token, err := obj.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return token
}
