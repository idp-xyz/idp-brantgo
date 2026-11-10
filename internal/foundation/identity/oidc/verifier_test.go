package oidc_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/foundation/identity/oidc"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// refused requires err to wrap identity.ErrUnauthenticated, to name reason and not to echo token.
func refused(t *testing.T, err error, token, reason string) {
	t.Helper()
	if err == nil {
		t.Fatalf("token accepted; want refusal naming %q", reason)
	}
	if !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("error %q does not wrap identity.ErrUnauthenticated", err)
	}
	if !strings.Contains(err.Error(), reason) {
		t.Fatalf("error %q does not name %q", err, reason)
	}
	if token != "" && strings.Contains(err.Error(), token) {
		t.Fatalf("error echoes the token: %q", err)
	}
}

func wantHits(t *testing.T, is *issuer, discovery, jwks int64) {
	t.Helper()
	if d, j := is.hits(); d != discovery || j != jwks {
		t.Fatalf("issuer hits: discovery %d, jwks %d; want %d, %d", d, j, discovery, jwks)
	}
}

func TestVerifyHumanToken(t *testing.T) {
	is := newIssuer(t, rsa1())
	v := is.verifier(t, &fakeClock{now: start})
	token := mint(t, rsa1(), is.claims(start, nil))

	p, err := v.Verify(t.Context(), token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	want := identity.Principal{
		Actor: kernel.ActorRef{Kind: kernel.ActorHuman, ID: "user-1"},
		Scope: kernel.Scope{Tenant: "tenant-1", Company: "company-1"},
	}
	if p != want {
		t.Fatalf("principal %+v, want %+v", p, want)
	}
	if _, err := identity.WithPrincipal(t.Context(), p); err != nil {
		t.Fatalf("WithPrincipal: %v", err)
	}
	if _, err := v.Verify(t.Context(), token); err != nil {
		t.Fatalf("second Verify: %v", err)
	}
	wantHits(t, is, 1, 1) // the key set is cached
}

func TestHeaderVerifier(t *testing.T) {
	is := newIssuer(t, rsa1())
	h := oidc.HeaderVerifier{Tokens: is.verifier(t, &fakeClock{now: start})}
	token := mint(t, rsa1(), is.claims(start, map[string]any{"brant_actor_kind": "ai"}))
	p, err := h.Verify(t.Context(), "Bearer "+token)
	if err != nil || p.Actor.Kind != kernel.ActorAI {
		t.Fatalf("Verify(Bearer token) = %+v, %v", p, err)
	}
	for _, header := range []string{token, "Basic " + token, "Bearer  " + token, ""} {
		_, err := h.Verify(t.Context(), header)
		refused(t, err, token, "bearer")
	}
	wantHits(t, is, 1, 1)
}

func TestVerifySigningAlgorithms(t *testing.T) {
	is := newIssuer(t, rsa1(), ecSigner(), edSigner())
	v := is.verifier(t, &fakeClock{now: start})
	k := testKeys()
	for _, s := range []signer{
		rsa1(),
		{"rsa-1", jose.PS256, k.rsa1},
		ecSigner(),
		edSigner(),
	} {
		t.Run(string(s.alg), func(t *testing.T) {
			if _, err := v.Verify(t.Context(), mint(t, s, is.claims(start, nil))); err != nil {
				t.Fatalf("Verify: %v", err)
			}
		})
	}
	wantHits(t, is, 1, 1)
}

func TestActorKindsAndHumanOnlyRules(t *testing.T) {
	is := newIssuer(t, rsa1())
	v := is.verifier(t, &fakeClock{now: start})
	policy, err := authorization.NewPolicy(authorization.HumanOnly("inquiry.confirm", "inquiry.confirm"))
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem} {
		t.Run(string(kind), func(t *testing.T) {
			p, err := v.Verify(t.Context(), mint(t, rsa1(), is.claims(start, map[string]any{"brant_actor_kind": string(kind)})))
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if p.Actor.Kind != kind {
				t.Fatalf("actor kind %q, want %q", p.Actor.Kind, kind)
			}
			_, err = policy.Admit(p, "inquiry.confirm")
			if human := kind == kernel.ActorHuman; human != (err == nil) {
				t.Fatalf("human-only rule for %s: %v", kind, err)
			}
			if kind != kernel.ActorHuman && !errors.Is(err, authorization.ErrActorKindDenied) {
				t.Fatalf("human-only rule for %s: %v, want ErrActorKindDenied", kind, err)
			}
		})
	}
}

func TestRefusedClaims(t *testing.T) {
	is := newIssuer(t, rsa1())
	v := is.verifier(t, &fakeClock{now: start})
	long := strings.Repeat("a", 65)
	cases := []struct {
		name   string
		edits  map[string]any
		reason string
	}{
		{"missing actor kind", map[string]any{"brant_actor_kind": remove}, "missing claim brant_actor_kind"},
		{"empty actor kind", map[string]any{"brant_actor_kind": ""}, "brant_actor_kind is not human, ai or system"},
		{"capitalised actor kind", map[string]any{"brant_actor_kind": "Human"}, "brant_actor_kind is not human, ai or system"},
		{"unknown actor kind", map[string]any{"brant_actor_kind": "administrator"}, "brant_actor_kind is not human, ai or system"},
		{"padded actor kind", map[string]any{"brant_actor_kind": "human "}, "brant_actor_kind is not human, ai or system"},
		{"actor kind true", map[string]any{"brant_actor_kind": true}, "claim brant_actor_kind is not a string"},
		{"actor kind list", map[string]any{"brant_actor_kind": []string{"human"}}, "claim brant_actor_kind is not a string"},
		{"actor kind null", map[string]any{"brant_actor_kind": nil}, "claim brant_actor_kind is not a string"},
		{"missing issuer", map[string]any{"iss": remove}, "missing claim iss"},
		{"other issuer", map[string]any{"iss": "https://idp.example.com"}, "issuer mismatch"},
		{"issuer with trailing slash", map[string]any{"iss": is.url + "/"}, "issuer mismatch"},
		{"missing audience", map[string]any{"aud": remove}, "missing claim aud"},
		{"other audience", map[string]any{"aud": "billing-api"}, "audience mismatch"},
		{"audience list without ours", map[string]any{"aud": []string{"billing-api", "crm-api"}}, "audience mismatch"},
		{"empty audience list", map[string]any{"aud": []string{}}, "claim aud is not a string or an array of strings"},
		{"audience number", map[string]any{"aud": 7}, "claim aud is not a string or an array of strings"},
		{"audience null", map[string]any{"aud": nil}, "claim aud is not a string or an array of strings"},
		{"missing exp", map[string]any{"exp": remove}, "missing claim exp"},
		{"expired", map[string]any{"exp": start.Add(-2 * time.Minute).Unix()}, "token expired"},
		{"exp string", map[string]any{"exp": "1999999999"}, "claim exp is not a NumericDate"},
		{"exp exponent", map[string]any{"exp": json.Number("2e9")}, "claim exp is not a NumericDate"},
		{"exp negative", map[string]any{"exp": json.Number("-1")}, "claim exp is not a NumericDate"},
		{"exp beyond year 9999", map[string]any{"exp": json.Number("253402300800")}, "claim exp is not a NumericDate"},
		{"nbf in the future", map[string]any{"nbf": start.Add(2 * time.Minute).Unix()}, "token not yet valid"},
		{"iat in the future", map[string]any{"iat": start.Add(2 * time.Minute).Unix()}, "token issued in the future"},
		{"missing sub", map[string]any{"sub": remove}, "missing claim sub"},
		{"sub with space", map[string]any{"sub": "user one"}, "claim sub is not a valid identifier"},
		{"sub too long", map[string]any{"sub": long}, "claim sub is not a valid identifier"},
		{"missing tenant", map[string]any{"brant_tenant": remove}, "missing claim brant_tenant"},
		{"tenant with slash", map[string]any{"brant_tenant": "tenant/other"}, "claim brant_tenant is not a valid identifier"},
		{"tenant starting with dash", map[string]any{"brant_tenant": "-tenant"}, "claim brant_tenant is not a valid identifier"},
		{"empty tenant", map[string]any{"brant_tenant": ""}, "claim brant_tenant is not a valid identifier"},
		{"tenant number", map[string]any{"brant_tenant": 42}, "claim brant_tenant is not a string"},
		{"missing company", map[string]any{"brant_company": remove}, "missing claim brant_company"},
		{"company too long", map[string]any{"brant_company": long}, "claim brant_company is not a valid identifier"},
		{"company not ascii", map[string]any{"brant_company": "公司"}, "claim brant_company is not a valid identifier"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			token := mint(t, rsa1(), is.claims(start, c.edits))
			_, err := v.Verify(t.Context(), token)
			refused(t, err, token, c.reason)
			for _, value := range c.edits {
				if s, ok := value.(string); ok && len(s) > 3 && !strings.Contains(c.reason, s) && strings.Contains(err.Error(), s) {
					t.Fatalf("error %q echoes claim value %q", err, s)
				}
			}
		})
	}
}

func TestAudienceList(t *testing.T) {
	is := newIssuer(t, rsa1())
	v := is.verifier(t, &fakeClock{now: start})
	token := mint(t, rsa1(), is.claims(start, map[string]any{"aud": []string{"billing-api", audience}}))
	if _, err := v.Verify(t.Context(), token); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestLeewayBoundaries(t *testing.T) {
	is := newIssuer(t, rsa1())
	sec := func(d time.Duration) int64 { return start.Add(d).Unix() }
	frac := func(d time.Duration, f string) json.Number { return json.Number(fmt.Sprintf("%d.%s", sec(d), f)) }
	cases := []struct {
		name   string
		leeway time.Duration
		edits  map[string]any
		reason string // empty: accepted
	}{
		{"exp one second inside the leeway", 0, map[string]any{"exp": sec(-oidc.DefaultLeeway + time.Second)}, ""},
		{"exp half a second inside the leeway", 0, map[string]any{"exp": frac(-oidc.DefaultLeeway, "5")}, ""},
		{"exp exactly at the leeway", 0, map[string]any{"exp": sec(-oidc.DefaultLeeway)}, "token expired"},
		{"nbf exactly at the leeway", 0, map[string]any{"nbf": sec(oidc.DefaultLeeway)}, ""},
		{"nbf one second beyond the leeway", 0, map[string]any{"nbf": sec(oidc.DefaultLeeway + time.Second)}, "token not yet valid"},
		{"iat exactly at the leeway", 0, map[string]any{"iat": sec(oidc.DefaultLeeway)}, ""},
		{"iat one second beyond the leeway", 0, map[string]any{"iat": sec(oidc.DefaultLeeway + time.Second)}, "token issued in the future"},
		{"configured leeway, exp inside", 10 * time.Second, map[string]any{"exp": sec(-9 * time.Second)}, ""},
		{"configured leeway, exp outside", 10 * time.Second, map[string]any{"exp": sec(-10 * time.Second)}, "token expired"},
		{"configured leeway, nbf outside", 10 * time.Second, map[string]any{"nbf": sec(11 * time.Second)}, "token not yet valid"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := is.verifier(t, &fakeClock{now: start}, func(cfg *oidc.Config) { cfg.Leeway = c.leeway })
			token := mint(t, rsa1(), is.claims(start, c.edits))
			_, err := v.Verify(t.Context(), token)
			if c.reason == "" {
				if err != nil {
					t.Fatalf("Verify: %v", err)
				}
				return
			}
			refused(t, err, token, c.reason)
		})
	}
}

func TestRefusedAlgorithms(t *testing.T) {
	is := newIssuer(t, rsa1())
	v := is.verifier(t, &fakeClock{now: start})
	claims, err := json.Marshal(is.claims(start, nil))
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.RawURLEncoding.EncodeToString
	valid := mint(t, rsa1(), is.claims(start, nil))
	validSignature := valid[strings.LastIndexByte(valid, '.')+1:]
	der, err := x509.MarshalPKIXPublicKey(&testKeys().rsa1.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	kid := map[jose.HeaderKey]any{"kid": "rsa-1"}

	tokens := map[string]string{
		"none":                                b64([]byte(`{"alg":"none","typ":"at+jwt","kid":"rsa-1"}`)) + "." + b64(claims) + ".",
		"none with a copied signature":        b64([]byte(`{"alg":"none","typ":"at+jwt","kid":"rsa-1"}`)) + "." + b64(claims) + "." + validSignature,
		"NONE":                                b64([]byte(`{"alg":"NONE","typ":"at+jwt","kid":"rsa-1"}`)) + "." + b64(claims) + ".",
		"HS256 keyed with the public key DER": mint(t, signer{"", jose.HS256, der}, is.claims(start, nil), kid),
		"HS256 keyed with the public key PEM": mint(t, signer{"", jose.HS256, pemKey}, is.claims(start, nil), kid),
		"HS512 keyed with the public key DER": mint(t, signer{"", jose.HS512, der}, is.claims(start, nil), kid),
		"RS384, not configured":               mint(t, signer{"rsa-1", jose.RS384, testKeys().rsa1}, is.claims(start, nil)),
	}
	for name, token := range tokens {
		t.Run(name, func(t *testing.T) {
			_, err := v.Verify(t.Context(), token)
			refused(t, err, token, "signature algorithm not accepted")
		})
	}
	wantHits(t, is, 0, 0) // refused before any key lookup

	rs384 := is.verifier(t, &fakeClock{now: start}, func(cfg *oidc.Config) { cfg.Algorithms = []string{"RS384"} })
	if _, err := rs384.Verify(t.Context(), tokens["RS384, not configured"]); err != nil {
		t.Fatalf("RS384 configured: %v", err)
	}
	_, err = rs384.Verify(t.Context(), valid)
	refused(t, err, valid, "signature algorithm not accepted")
}

func TestNewRefusesNoneAndHMAC(t *testing.T) {
	is := newIssuer(t, rsa1())
	for _, alg := range []string{"none", "None", "HS256", "HS384", "HS512", "hs256", "ES256K", "RSA-OAEP", ""} {
		t.Run(alg, func(t *testing.T) {
			cfg := is.config(&fakeClock{now: start})
			cfg.Algorithms = []string{"RS256", alg}
			if _, err := oidc.New(cfg); !errors.Is(err, oidc.ErrInvalidConfig) {
				t.Fatalf("New with algorithm %q: %v, want ErrInvalidConfig", alg, err)
			}
		})
	}
}

func TestSignatureByAnotherKeyWithTheSameKid(t *testing.T) {
	is := newIssuer(t, rsa1())
	v := is.verifier(t, &fakeClock{now: start})
	for _, s := range []signer{
		{"rsa-1", jose.RS256, testKeys().rsaOther},
		{"rsa-1", jose.PS256, testKeys().rsaOther},
	} {
		token := mint(t, s, is.claims(start, nil))
		_, err := v.Verify(t.Context(), token)
		refused(t, err, token, "invalid signature")
	}
	wantHits(t, is, 1, 1) // a known kid triggers no refresh
}

func TestTamperedToken(t *testing.T) {
	is := newIssuer(t, rsa1())
	v := is.verifier(t, &fakeClock{now: start})
	token := mint(t, rsa1(), is.claims(start, nil))
	parts := strings.Split(token, ".")
	forged, err := json.Marshal(is.claims(start, map[string]any{"brant_tenant": "tenant-2"}))
	if err != nil {
		t.Fatal(err)
	}
	tampered := parts[0] + "." + base64.RawURLEncoding.EncodeToString(forged) + "." + parts[2]
	_, err = v.Verify(t.Context(), tampered)
	refused(t, err, tampered, "invalid signature")
}

func TestKeyRotationRefreshesOnce(t *testing.T) {
	is := newIssuer(t, rsa1())
	clock := &fakeClock{now: start}
	v := is.verifier(t, clock)
	if _, err := v.Verify(t.Context(), mint(t, rsa1(), is.claims(start, nil))); err != nil {
		t.Fatalf("Verify rsa-1: %v", err)
	}
	wantHits(t, is, 1, 1)

	is.publish(rsa1(), rsa2())
	clock.Advance(time.Minute)
	rotated := mint(t, rsa2(), is.claims(clock.Now(), nil))
	_, err := v.Verify(t.Context(), rotated)
	refused(t, err, rotated, "unknown key ID") // within the refresh interval of the first fetch
	wantHits(t, is, 1, 1)

	clock.Advance(oidc.DefaultRefreshInterval - time.Minute)
	rotated = mint(t, rsa2(), is.claims(clock.Now(), nil))
	if _, err := v.Verify(t.Context(), rotated); err != nil {
		t.Fatalf("Verify rsa-2 after rotation: %v", err)
	}
	wantHits(t, is, 2, 2)
	for range 3 {
		if _, err := v.Verify(t.Context(), rotated); err != nil {
			t.Fatalf("Verify rsa-2 again: %v", err)
		}
	}
	wantHits(t, is, 2, 2)
}

func TestUnknownKidsRefreshAtMostOncePerInterval(t *testing.T) {
	is := newIssuer(t, rsa1())
	clock := &fakeClock{now: start}
	v := is.verifier(t, clock)
	if _, err := v.Verify(t.Context(), mint(t, rsa1(), is.claims(start, nil))); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	clock.Advance(oidc.DefaultRefreshInterval)

	forge := func(n int) []string {
		tokens := make([]string, n)
		for i := range tokens {
			tokens[i] = mint(t, signer{fmt.Sprintf("forged-%d", i), jose.RS256, testKeys().rsaOther}, is.claims(clock.Now(), nil))
		}
		return tokens
	}
	flood := forge(64)
	errs := make([]error, len(flood))
	var wg sync.WaitGroup
	for i, token := range flood {
		wg.Go(func() { _, errs[i] = v.Verify(t.Context(), token) })
	}
	wg.Wait()
	for i, err := range errs {
		refused(t, err, flood[i], "unknown key ID")
	}
	wantHits(t, is, 2, 2) // 64 concurrent unknown kids, one refresh

	clock.Advance(oidc.DefaultRefreshInterval - time.Second)
	for _, token := range forge(16) {
		_, err := v.Verify(t.Context(), token)
		refused(t, err, token, "unknown key ID")
	}
	wantHits(t, is, 2, 2)

	clock.Advance(time.Second)
	for _, token := range forge(16) {
		_, err := v.Verify(t.Context(), token)
		refused(t, err, token, "unknown key ID")
	}
	wantHits(t, is, 3, 3)
}

func TestConcurrentFirstUseFetchesOnce(t *testing.T) {
	is := newIssuer(t, rsa1())
	v := is.verifier(t, &fakeClock{now: start})
	token := mint(t, rsa1(), is.claims(start, nil))
	errs := make([]error, 32)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Go(func() { _, errs[i] = v.Verify(t.Context(), token) })
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
	}
	wantHits(t, is, 1, 1)
}

func TestUnavailableKeySetRetriesSparingly(t *testing.T) {
	is := newIssuer(t, rsa1())
	is.set(func(is *issuer) { is.jwksStatus = http.StatusInternalServerError })
	clock := &fakeClock{now: start}
	v := is.verifier(t, clock)
	token := mint(t, rsa1(), is.claims(start, map[string]any{"exp": start.Add(time.Hour).Unix()}))

	_, err := v.Verify(t.Context(), token)
	refused(t, err, token, "answered 500")
	if !errors.Is(err, oidc.ErrKeysUnavailable) {
		t.Fatalf("error %q does not wrap ErrKeysUnavailable", err)
	}
	clock.Advance(9 * time.Second)
	_, err = v.Verify(t.Context(), token)
	refused(t, err, token, "no key set loaded yet")
	wantHits(t, is, 1, 1)

	clock.Advance(time.Second)
	_, err = v.Verify(t.Context(), token)
	refused(t, err, token, "answered 500")
	wantHits(t, is, 2, 2)

	is.set(func(is *issuer) { is.jwksStatus = http.StatusOK })
	clock.Advance(10 * time.Second)
	if _, err := v.Verify(t.Context(), token); err != nil {
		t.Fatalf("Verify after recovery: %v", err)
	}
	wantHits(t, is, 3, 3)
}

func TestKeySetMaxAgeWithdrawsKeys(t *testing.T) {
	is := newIssuer(t, rsa1(), rsa2())
	clock := &fakeClock{now: start}
	v := is.verifier(t, clock)
	if _, err := v.Verify(t.Context(), mint(t, rsa1(), is.claims(start, nil))); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	is.publish(rsa2()) // the issuer withdraws rsa-1

	clock.Advance(oidc.DefaultKeySetMaxAge - time.Second)
	if _, err := v.Verify(t.Context(), mint(t, rsa1(), is.claims(clock.Now(), nil))); err != nil {
		t.Fatalf("Verify with the cached key set: %v", err)
	}
	wantHits(t, is, 1, 1)

	clock.Advance(time.Second)
	withdrawn := mint(t, rsa1(), is.claims(clock.Now(), nil))
	_, err := v.Verify(t.Context(), withdrawn)
	refused(t, err, withdrawn, "unknown key ID")
	wantHits(t, is, 2, 2)
	if _, err := v.Verify(t.Context(), mint(t, rsa2(), is.claims(clock.Now(), nil))); err != nil {
		t.Fatalf("Verify rsa-2: %v", err)
	}
	wantHits(t, is, 2, 2)
}

func TestStaleKeySetStaysInUseWhenTheIssuerFails(t *testing.T) {
	is := newIssuer(t, rsa1())
	clock := &fakeClock{now: start}
	v := is.verifier(t, clock)
	if _, err := v.Verify(t.Context(), mint(t, rsa1(), is.claims(start, nil))); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	is.set(func(is *issuer) { is.jwksStatus = http.StatusServiceUnavailable })
	clock.Advance(oidc.DefaultKeySetMaxAge)
	for range 3 {
		if _, err := v.Verify(t.Context(), mint(t, rsa1(), is.claims(clock.Now(), nil))); err != nil {
			t.Fatalf("Verify with a stale key set: %v", err)
		}
	}
	wantHits(t, is, 2, 2) // one failed refresh, then the refresh interval holds
}

func TestOversizedToken(t *testing.T) {
	is := newIssuer(t, rsa1())
	v := is.verifier(t, &fakeClock{now: start})
	huge := strings.Repeat("a", oidc.DefaultMaxTokenBytes+1)
	_, err := v.Verify(t.Context(), huge)
	refused(t, err, huge, "token exceeds 8192 bytes")

	token := mint(t, rsa1(), is.claims(start, nil))
	tight := is.verifier(t, &fakeClock{now: start}, func(cfg *oidc.Config) { cfg.MaxTokenBytes = len(token) - 1 })
	_, err = tight.Verify(t.Context(), token)
	refused(t, err, token, "token exceeds")
	wantHits(t, is, 0, 0)

	exact := is.verifier(t, &fakeClock{now: start}, func(cfg *oidc.Config) { cfg.MaxTokenBytes = len(token) })
	if _, err := exact.Verify(t.Context(), token); err != nil {
		t.Fatalf("Verify at the size limit: %v", err)
	}
}

func TestMalformedTokens(t *testing.T) {
	is := newIssuer(t, rsa1())
	v := is.verifier(t, &fakeClock{now: start})
	valid := mint(t, rsa1(), is.claims(start, nil))
	for name, token := range map[string]string{
		"empty":           "",
		"one part":        "abc",
		"two parts":       "abc.def",
		"three garbage":   "abc.def.ghi",
		"four parts":      valid + ".abc",
		"inner space":     strings.Replace(valid, ".", ". ", 1),
		"JSON serialized": `{"payload":"e30","protected":"eyJhbGciOiJSUzI1NiJ9","signature":"AA"}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := v.Verify(t.Context(), token)
			refused(t, err, token, "token")
		})
	}
	wantHits(t, is, 0, 0)
}

func TestHeaderChecks(t *testing.T) {
	is := newIssuer(t, rsa1())
	v := is.verifier(t, &fakeClock{now: start})
	claims := is.claims(start, nil)
	cases := []struct {
		name    string
		headers map[jose.HeaderKey]any
		reason  string
	}{
		{"no typ", map[jose.HeaderKey]any{jose.HeaderType: nil}, "token type not accepted"},
		{"typ JWT", map[jose.HeaderKey]any{jose.HeaderType: "JWT"}, "token type not accepted"},
		{"typ id token", map[jose.HeaderKey]any{jose.HeaderType: "id_token+jwt"}, "token type not accepted"},
		{"crit", map[jose.HeaderKey]any{"crit": []string{"exp"}, "exp": 1}, "critical header extensions are not supported"},
		{"b64", map[jose.HeaderKey]any{"b64": true}, "unencoded payloads are not supported"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			token := mint(t, rsa1(), claims, c.headers)
			_, err := v.Verify(t.Context(), token)
			refused(t, err, token, c.reason)
		})
	}
	noKid := mint(t, signer{"", jose.RS256, testKeys().rsa1}, claims)
	_, err := v.Verify(t.Context(), noKid)
	refused(t, err, noKid, "token header has no key ID")
	wantHits(t, is, 0, 0)

	for _, typ := range []string{"at+jwt", "AT+JWT", "application/at+jwt"} {
		if _, err := v.Verify(t.Context(), mint(t, rsa1(), claims, map[jose.HeaderKey]any{jose.HeaderType: typ})); err != nil {
			t.Fatalf("typ %q: %v", typ, err)
		}
	}
	generic := is.verifier(t, &fakeClock{now: start}, func(cfg *oidc.Config) { cfg.TokenTypes = []string{"at+jwt", "JWT"} })
	if _, err := generic.Verify(t.Context(), mint(t, rsa1(), claims, map[jose.HeaderKey]any{jose.HeaderType: "JWT"})); err != nil {
		t.Fatalf("typ JWT configured: %v", err)
	}
}

func TestKeyMustFitTheAlgorithm(t *testing.T) {
	is := newIssuer(t)
	k := testKeys()
	is.publishKeys(
		signer{"rsa-1", jose.RS256, k.rsa1}.public("sig", "RS256"),
		ecSigner().public("", ""),
		edSigner().public("sig", ""),
	)
	v := is.verifier(t, &fakeClock{now: start})
	for name, s := range map[string]signer{
		"ES256 under an RSA kid":            {"rsa-1", jose.ES256, k.ec},
		"EdDSA under an EC kid":             {"ec-1", jose.EdDSA, k.ed},
		"RS256 under an Ed25519 kid":        {"ed-1", jose.RS256, k.rsa1},
		"PS256 with a key limited to RS256": {"rsa-1", jose.PS256, k.rsa1},
	} {
		t.Run(name, func(t *testing.T) {
			token := mint(t, s, is.claims(start, nil))
			_, err := v.Verify(t.Context(), token)
			refused(t, err, token, "key does not fit the signature algorithm")
		})
	}
	if _, err := v.Verify(t.Context(), mint(t, rsa1(), is.claims(start, nil))); err != nil {
		t.Fatalf("RS256 with its key: %v", err)
	}
}

func TestECDSACurveMustFitTheAlgorithm(t *testing.T) {
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	es384 := signer{"ec-384", jose.ES384, p384}
	is := newIssuer(t, es384)
	v := is.verifier(t, &fakeClock{now: start}, func(c *oidc.Config) { c.Algorithms = []string{"ES256", "ES384"} })
	if _, err := v.Verify(t.Context(), mint(t, es384, is.claims(start, nil))); err != nil {
		t.Fatalf("ES384 with a P-384 key: %v", err)
	}
	token := mint(t, signer{"ec-384", jose.ES256, testKeys().ec}, is.claims(start, nil))
	_, err = v.Verify(t.Context(), token)
	refused(t, err, token, "key does not fit the signature algorithm")
}

func TestKeySetSkipsUnusableKeys(t *testing.T) {
	k := testKeys()
	small, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	keyJSON := func(key jose.JSONWebKey) string {
		b, err := json.Marshal(key)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	private := jose.JSONWebKey{Key: k.rsa2, KeyID: "rsa-private", Use: "sig"}
	ecOther := signer{"dup", jose.ES256, k.ec}
	body := `{"keys":[` + strings.Join([]string{
		keyJSON(signer{"rsa-enc", jose.RS256, k.rsa1}.public("enc", "")),
		keyJSON(private),
		keyJSON(signer{"rsa-small", jose.RS256, small}.public("sig", "")),
		`{"kty":"oct","kid":"hmac-1","use":"sig","k":"c2VjcmV0LXNlY3JldC1zZWNyZXQtc2VjcmV0LXNlY3JldA"}`,
		`{"kty":"XYZ","kid":"future-1","use":"sig","x":"AA"}`,
		`{"kty":"RSA","kid":"broken-1","n":"!!","e":"AQAB"}`,
		keyJSON(signer{"", jose.RS256, k.rsa1}.public("sig", "")),
		keyJSON(signer{"dup", jose.RS256, k.rsa1}.public("sig", "")),
		keyJSON(signer{"dup", jose.RS256, k.rsaOther}.public("sig", "")),
		keyJSON(ecOther.public("sig", "")),
		keyJSON(edSigner().public("sig", "")),
	}, ",") + `]}`
	is := newIssuer(t)
	is.publishRaw([]byte(body))
	v := is.verifier(t, &fakeClock{now: start})

	if _, err := v.Verify(t.Context(), mint(t, edSigner(), is.claims(start, nil))); err != nil {
		t.Fatalf("Ed25519 key next to unusable keys: %v", err)
	}
	if _, err := v.Verify(t.Context(), mint(t, ecOther, is.claims(start, nil))); err != nil {
		t.Fatalf("EC key sharing its kid with RSA keys: %v", err)
	}
	for name, c := range map[string]struct {
		s      signer
		reason string
	}{
		"use enc":              {signer{"rsa-enc", jose.RS256, k.rsa1}, "unknown key ID"},
		"private key in JWKS":  {signer{"rsa-private", jose.RS256, k.rsa2}, "unknown key ID"},
		"RSA below 2048 bits":  {signer{"rsa-small", jose.RS256, small}, "unknown key ID"},
		"two RSA keys one kid": {signer{"dup", jose.RS256, k.rsa1}, "key ID names several keys"},
	} {
		t.Run(name, func(t *testing.T) {
			token := mint(t, c.s, is.claims(start, nil))
			_, err := v.Verify(t.Context(), token)
			refused(t, err, token, c.reason)
		})
	}
}

func TestKeySetWithoutUsableKeys(t *testing.T) {
	is := newIssuer(t)
	is.publishRaw([]byte(`{"keys":[{"kty":"oct","kid":"hmac-1","k":"c2VjcmV0"}]}`))
	v := is.verifier(t, &fakeClock{now: start})
	token := mint(t, rsa1(), is.claims(start, nil))
	_, err := v.Verify(t.Context(), token)
	refused(t, err, token, "key set has no usable signing key")
	if !errors.Is(err, oidc.ErrKeysUnavailable) {
		t.Fatalf("error %q does not wrap ErrKeysUnavailable", err)
	}
}

func TestDuplicateClaimsAndNonObjects(t *testing.T) {
	is := newIssuer(t, rsa1())
	v := is.verifier(t, &fakeClock{now: start})
	claims, err := json.Marshal(is.claims(start, map[string]any{"brant_actor_kind": "ai"}))
	if err != nil {
		t.Fatal(err)
	}
	duplicate := append([]byte(`{"brant_actor_kind":"human",`), claims[1:]...)
	token := mintRaw(t, rsa1(), duplicate)
	_, err = v.Verify(t.Context(), token)
	refused(t, err, token, "duplicate claim")

	for _, payload := range []string{`[]`, `"claims"`, `null`, string(claims) + `{}`, `{"sub":"user-1"`} {
		token := mintRaw(t, rsa1(), []byte(payload))
		_, err := v.Verify(t.Context(), token)
		refused(t, err, token, "claims are not a JSON object")
	}

	spaced := strings.ReplaceAll(string(claims), `":`, `" :  `)
	if _, err := v.Verify(t.Context(), mintRaw(t, rsa1(), []byte(spaced))); err != nil {
		t.Fatalf("claims with insignificant whitespace: %v", err)
	}
}

func TestDiscovery(t *testing.T) {
	t.Run("issuer mismatch", func(t *testing.T) {
		is := newIssuer(t, rsa1())
		is.set(func(is *issuer) { is.discoveryIssuer = is.url + "/" })
		v := is.verifier(t, &fakeClock{now: start})
		token := mint(t, rsa1(), is.claims(start, nil))
		_, err := v.Verify(t.Context(), token)
		refused(t, err, token, "discovery names another issuer")
		if !errors.Is(err, oidc.ErrKeysUnavailable) {
			t.Fatalf("error %q does not wrap ErrKeysUnavailable", err)
		}
		if err := v.Refresh(t.Context()); !errors.Is(err, oidc.ErrKeysUnavailable) {
			t.Fatalf("Refresh: %v, want ErrKeysUnavailable", err)
		}
		wantHits(t, is, 2, 0)
	})
	t.Run("http jwks_uri", func(t *testing.T) {
		plain := startIssuer(t, httptest.NewServer, rsa1())
		is := newIssuer(t, rsa1())
		is.set(func(is *issuer) { is.jwksURI = plain.url + "/jwks" })
		v := is.verifier(t, &fakeClock{now: start})
		token := mint(t, rsa1(), is.claims(start, nil))
		_, err := v.Verify(t.Context(), token)
		refused(t, err, token, "must use https")
		wantHits(t, plain, 0, 0)
	})
	t.Run("redirect", func(t *testing.T) {
		is := newIssuer(t, rsa1())
		redirect := httptest.NewTLSServer(http.RedirectHandler(is.url+"/.well-known/openid-configuration", http.StatusFound))
		t.Cleanup(redirect.Close)
		v, err := oidc.New(oidc.Config{Issuer: redirect.URL, Audience: audience, HTTPClient: redirect.Client(), Clock: &fakeClock{now: start}})
		if err != nil {
			t.Fatal(err)
		}
		if err := v.Refresh(t.Context()); err == nil || !strings.Contains(err.Error(), "redirects are not followed") {
			t.Fatalf("Refresh through a redirect: %v", err)
		}
		wantHits(t, is, 0, 0)
	})
	t.Run("oversized response", func(t *testing.T) {
		is := newIssuer(t, rsa1(), rsa2(), ecSigner(), edSigner())
		v := is.verifier(t, &fakeClock{now: start}, func(cfg *oidc.Config) { cfg.MaxResponseBytes = 512 })
		if err := v.Refresh(t.Context()); !errors.Is(err, oidc.ErrKeysUnavailable) || !strings.Contains(err.Error(), "response exceeds 512 bytes") {
			t.Fatalf("Refresh with an oversized key set: %v", err)
		}
	})
	t.Run("configured JWKS URL skips discovery", func(t *testing.T) {
		is := newIssuer(t, rsa1())
		v := is.verifier(t, &fakeClock{now: start}, func(cfg *oidc.Config) { cfg.JWKSURL = is.url + "/jwks" })
		if _, err := v.Verify(t.Context(), mint(t, rsa1(), is.claims(start, nil))); err != nil {
			t.Fatalf("Verify: %v", err)
		}
		wantHits(t, is, 0, 1)
	})
	t.Run("refresh at start-up", func(t *testing.T) {
		is := newIssuer(t, rsa1())
		v := is.verifier(t, &fakeClock{now: start})
		if err := v.Refresh(t.Context()); err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		if _, err := v.Verify(t.Context(), mint(t, rsa1(), is.claims(start, nil))); err != nil {
			t.Fatalf("Verify: %v", err)
		}
		wantHits(t, is, 1, 1)
	})
}

func TestInsecureHTTPIsOptInAndLoopbackOnly(t *testing.T) {
	plain := startIssuer(t, httptest.NewServer, rsa1())
	cfg := plain.config(&fakeClock{now: start})
	if _, err := oidc.New(cfg); !errors.Is(err, oidc.ErrInvalidConfig) {
		t.Fatalf("http issuer without AllowInsecureHTTP: %v, want ErrInvalidConfig", err)
	}
	cfg.AllowInsecureHTTP = true
	v, err := oidc.New(cfg)
	if err != nil {
		t.Fatalf("http loopback issuer with AllowInsecureHTTP: %v", err)
	}
	if _, err := v.Verify(t.Context(), mint(t, rsa1(), plain.claims(start, nil))); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	for _, edit := range []func(*oidc.Config){
		func(c *oidc.Config) { c.Issuer = "http://idp.example.com" },
		func(c *oidc.Config) { c.Issuer = "http://10.0.0.8:8080/realms/brant" },
		func(c *oidc.Config) { c.JWKSURL = "http://idp.example.com/jwks" },
	} {
		c := cfg
		edit(&c)
		if _, err := oidc.New(c); !errors.Is(err, oidc.ErrInvalidConfig) || !strings.Contains(err.Error(), "loopback hosts only") {
			t.Fatalf("AllowInsecureHTTP with a remote host: %v", err)
		}
	}
	tls := newIssuer(t, rsa1())
	c := tls.config(&fakeClock{now: start})
	c.JWKSURL = plain.url + "/jwks"
	if _, err := oidc.New(c); !errors.Is(err, oidc.ErrInvalidConfig) {
		t.Fatalf("http JWKS URL without AllowInsecureHTTP: %v, want ErrInvalidConfig", err)
	}
}

func TestNewRefusesInvalidConfig(t *testing.T) {
	is := newIssuer(t, rsa1())
	for name, edit := range map[string]func(*oidc.Config){
		"no issuer":            func(c *oidc.Config) { c.Issuer = "" },
		"relative issuer":      func(c *oidc.Config) { c.Issuer = "/realms/brant" },
		"issuer with query":    func(c *oidc.Config) { c.Issuer += "?realm=brant" },
		"issuer with fragment": func(c *oidc.Config) { c.Issuer += "#brant" },
		"issuer with userinfo": func(c *oidc.Config) { c.Issuer = "https://user:pw@idp.example.com" },
		"no audience":          func(c *oidc.Config) { c.Audience = "" },
		"same claim twice":     func(c *oidc.Config) { c.CompanyClaim = oidc.DefaultTenantClaim },
		"kind claim is sub":    func(c *oidc.Config) { c.ActorKindClaim = "sub" },
		"negative leeway":      func(c *oidc.Config) { c.Leeway = -time.Second },
		"leeway too large":     func(c *oidc.Config) { c.Leeway = oidc.MaxLeeway + time.Second },
		"negative token size":  func(c *oidc.Config) { c.MaxTokenBytes = -1 },
		"empty token type":     func(c *oidc.Config) { c.TokenTypes = []string{"at+jwt", " "} },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := is.config(&fakeClock{now: start})
			edit(&cfg)
			_, err := oidc.New(cfg)
			if !errors.Is(err, oidc.ErrInvalidConfig) {
				t.Fatalf("New: %v, want ErrInvalidConfig", err)
			}
			if strings.Contains(err.Error(), "pw@") {
				t.Fatalf("error %q shows a configured password", err)
			}
		})
	}
}

func TestCustomClaimNames(t *testing.T) {
	is := newIssuer(t, rsa1())
	v := is.verifier(t, &fakeClock{now: start}, func(c *oidc.Config) {
		c.TenantClaim, c.CompanyClaim, c.ActorKindClaim, c.ActorIDClaim = "tid", "cid", "kind", "uid"
	})
	token := mint(t, rsa1(), is.claims(start, map[string]any{
		"tid": "tenant-9", "cid": "company-9", "kind": "system", "uid": "svc-relay",
		"brant_tenant": remove, "brant_company": remove, "brant_actor_kind": remove, "sub": "ignored subject",
	}))
	p, err := v.Verify(t.Context(), token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	want := identity.Principal{
		Actor: kernel.ActorRef{Kind: kernel.ActorSystem, ID: "svc-relay"},
		Scope: kernel.Scope{Tenant: "tenant-9", Company: "company-9"},
	}
	if p != want {
		t.Fatalf("principal %+v, want %+v", p, want)
	}
	token = mint(t, rsa1(), is.claims(start, nil))
	_, err = v.Verify(t.Context(), token)
	refused(t, err, token, "missing claim tid")
}
