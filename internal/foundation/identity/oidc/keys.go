package oidc

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// minRSABits is the smallest RSA modulus a key set may use; smaller keys are skipped. go-jose
// refuses moduli above jose.MaxRSAModulusBits (4096).
const minRSABits = 2048

// keySet is one fetched JWKS: the usable signing keys by key ID.
type keySet struct {
	byKID     map[string][]jose.JSONWebKey
	fetchedAt time.Time
}

// lookup returns the keys of kid; a nil set knows no key.
func (s *keySet) lookup(kid string) ([]jose.JSONWebKey, bool) {
	if s == nil {
		return nil, false
	}
	keys, ok := s.byKID[kid]
	return keys, ok
}

// pick returns the one key of keys that fits alg.
func pick(keys []jose.JSONWebKey, alg jose.SignatureAlgorithm) (crypto.PublicKey, error) {
	var match crypto.PublicKey
	n := 0
	for _, k := range keys {
		if fits(k, alg) {
			match, n = k.Key, n+1
		}
	}
	switch n {
	case 0:
		return nil, unauthenticated("key does not fit the signature algorithm")
	case 1:
		return match, nil
	}
	return nil, unauthenticated("key ID names several keys for the signature algorithm")
}

// fits reports whether k may verify alg: the key type and curve match the algorithm and the key's
// own "alg", when present, names it.
func fits(k jose.JSONWebKey, alg jose.SignatureAlgorithm) bool {
	if k.Algorithm != "" && k.Algorithm != string(alg) {
		return false
	}
	switch key := k.Key.(type) {
	case *rsa.PublicKey:
		switch alg {
		case jose.RS256, jose.RS384, jose.RS512, jose.PS256, jose.PS384, jose.PS512:
			return true
		}
	case *ecdsa.PublicKey:
		switch alg {
		case jose.ES256:
			return key.Curve == elliptic.P256()
		case jose.ES384:
			return key.Curve == elliptic.P384()
		case jose.ES512:
			return key.Curve == elliptic.P521()
		}
	case ed25519.PublicKey:
		return alg == jose.EdDSA
	}
	return false
}

// usable reports whether k may sign tokens at all: a public asymmetric key with a key ID, meant
// for signatures, of an accepted size. Symmetric and private keys are never used.
func usable(k jose.JSONWebKey) bool {
	if k.KeyID == "" || (k.Use != "" && k.Use != "sig") {
		return false
	}
	switch key := k.Key.(type) {
	case *rsa.PublicKey:
		return key.N.BitLen() >= minRSABits
	case *ecdsa.PublicKey:
		return key.Curve == elliptic.P256() || key.Curve == elliptic.P384() || key.Curve == elliptic.P521()
	case ed25519.PublicKey:
		return len(key) == ed25519.PublicKeySize
	}
	return false
}

// parseKeySet reads a JWKS document. Keys go-jose cannot parse (another key type, malformed
// members) and keys that are not usable are skipped, so one foreign key does not hide the others.
func parseKeySet(body []byte) (*keySet, error) {
	var doc struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("%w: key set is not a JWKS document", ErrKeysUnavailable)
	}
	set := &keySet{byKID: map[string][]jose.JSONWebKey{}}
	for _, raw := range doc.Keys {
		var k jose.JSONWebKey
		if err := k.UnmarshalJSON(raw); err != nil || !usable(k) {
			continue
		}
		set.byKID[k.KeyID] = append(set.byKID[k.KeyID], k)
	}
	if len(set.byKID) == 0 {
		return nil, fmt.Errorf("%w: key set has no usable signing key", ErrKeysUnavailable)
	}
	return set, nil
}

// load resolves the key set URL, through discovery unless Config.JWKSURL is set, and fetches the
// key set. It runs detached from the cancellation of ctx, bounded by FetchTimeout, so a caller that
// goes away does not spend the refresh interval of every other caller.
func (v *Verifier) load(ctx context.Context) (*keySet, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), v.cfg.FetchTimeout)
	defer cancel()
	jwksURL := v.cfg.JWKSURL
	if jwksURL == "" {
		var err error
		if jwksURL, err = v.discover(ctx); err != nil {
			return nil, err
		}
	}
	body, err := v.get(ctx, jwksURL, "application/jwk-set+json, application/json")
	if err != nil {
		return nil, err
	}
	return parseKeySet(body)
}

// discover reads the issuer's OpenID Connect discovery document and returns its jwks_uri.
func (v *Verifier) discover(ctx context.Context) (string, error) {
	body, err := v.get(ctx, strings.TrimSuffix(v.cfg.Issuer, "/")+"/.well-known/openid-configuration", "application/json")
	if err != nil {
		return "", err
	}
	var doc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", fmt.Errorf("%w: discovery document is not JSON", ErrKeysUnavailable)
	}
	if doc.Issuer != v.cfg.Issuer {
		return "", fmt.Errorf("%w: discovery names another issuer", ErrKeysUnavailable)
	}
	if err := checkURL("jwks_uri", doc.JWKSURI, v.cfg.AllowInsecureHTTP); err != nil {
		return "", fmt.Errorf("%w: %w", ErrKeysUnavailable, err)
	}
	return doc.JWKSURI, nil
}

// get fetches url and returns a 200 response body of at most MaxResponseBytes.
func (v *Verifier) get(ctx context.Context, url, accept string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrKeysUnavailable, err)
	}
	req.Header.Set("Accept", accept)
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrKeysUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		note := ""
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			note = "; redirects are not followed"
		}
		return nil, fmt.Errorf("%w: GET %s answered %d%s", ErrKeysUnavailable, url, resp.StatusCode, note)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, v.cfg.MaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: GET %s: %w", ErrKeysUnavailable, url, err)
	}
	if int64(len(body)) > v.cfg.MaxResponseBytes {
		return nil, fmt.Errorf("%w: GET %s: response exceeds %d bytes", ErrKeysUnavailable, url, v.cfg.MaxResponseBytes)
	}
	return body, nil
}
