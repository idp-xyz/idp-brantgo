// Package oidc verifies OAuth 2.0 access tokens in JWT form (RFC 9068) issued by an OpenID Connect
// provider and turns them into an identity.Principal (G1 control C10). The actor, the actor kind,
// the tenant and the company come from signed claims only; a request body never supplies them.
//
// The actor kind claim decides whether a caller may confirm business facts: human-only commands
// refuse ai and system actors. It is required and must be exactly "human", "ai" or "system"; a
// missing or unknown kind is refused, never defaulted. The issuer must set the tenant, company and
// actor kind claims from its own records (client or user attributes users cannot edit).
//
// Keys come from the issuer's JWKS, found through OpenID Connect discovery or configured directly.
// Only asymmetric signatures are accepted (RSA of at least 2048 bits, ECDSA P-256/384/521,
// Ed25519); "none" and HMAC algorithms are refused whatever the configuration says, and keys a
// token header embeds or points to (jwk, jku, x5u, x5c) are ignored.
package oidc

import (
	"context"
	"crypto"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"go.idp.xyz/idp-bento-go/application"

	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Defaults of Config.
const (
	DefaultTenantClaim      = "brant_tenant"
	DefaultCompanyClaim     = "brant_company"
	DefaultActorKindClaim   = "brant_actor_kind"
	DefaultActorIDClaim     = "sub"
	DefaultTokenType        = "at+jwt"
	DefaultLeeway           = 60 * time.Second
	DefaultMaxTokenBytes    = 8 << 10
	DefaultRefreshInterval  = 5 * time.Minute
	DefaultKeySetMaxAge     = time.Hour
	DefaultFetchTimeout     = 10 * time.Second
	DefaultMaxResponseBytes = 1 << 20

	// MaxLeeway bounds Config.Leeway; a larger clock skew is a misconfiguration.
	MaxLeeway = 5 * time.Minute
)

// coldRetryInterval bounds how often a Verifier that has never loaded a key set retries, when
// RefreshInterval is longer: a failed first fetch must not lock every caller out for minutes.
const coldRetryInterval = 10 * time.Second

var (
	// ErrInvalidConfig reports a Config that New refuses.
	ErrInvalidConfig = errors.New("oidc: invalid configuration")
	// ErrKeysUnavailable reports that discovery or the key set could not be fetched or parsed.
	// Verify wraps it together with identity.ErrUnauthenticated.
	ErrKeysUnavailable = errors.New("oidc: signing keys unavailable")
)

// defaultAlgorithms are accepted when Config.Algorithms is empty.
var defaultAlgorithms = []string{"RS256", "PS256", "ES256", "EdDSA"}

// supportedAlgorithms are the asymmetric JWS algorithms Config.Algorithms may name.
var supportedAlgorithms = map[string]jose.SignatureAlgorithm{
	"RS256": jose.RS256, "RS384": jose.RS384, "RS512": jose.RS512,
	"PS256": jose.PS256, "PS384": jose.PS384, "PS512": jose.PS512,
	"ES256": jose.ES256, "ES384": jose.ES384, "ES512": jose.ES512,
	"EdDSA": jose.EdDSA,
}

// Config configures a Verifier. Issuer and Audience are required; a zero value in any other field
// selects its default.
type Config struct {
	// Issuer is the exact "iss" of accepted tokens and the base URL of OpenID Connect discovery
	// ({Issuer}/.well-known/openid-configuration), whose "issuer" must equal it exactly.
	Issuer string
	// Audience must appear in the "aud" claim, a string or an array of strings.
	Audience string
	// JWKSURL, when set, is the key set URL; discovery is then skipped.
	JWKSURL string

	// Algorithms are the accepted "alg" header values: RS256, RS384, RS512, PS256, PS384, PS512,
	// ES256, ES384, ES512 or EdDSA. Default RS256, PS256, ES256, EdDSA. "none" and HMAC algorithms
	// make New fail.
	Algorithms []string
	// TokenTypes are the accepted "typ" header values, compared case-insensitively and without an
	// "application/" prefix. Default "at+jwt" (RFC 9068 §4); an issuer that does not implement RFC
	// 9068 may need "JWT" as well. A token without "typ" is refused.
	TokenTypes []string

	// Claim names. Defaults brant_tenant, brant_company, brant_actor_kind and sub; all distinct.
	TenantClaim    string
	CompanyClaim   string
	ActorKindClaim string
	ActorIDClaim   string

	// Leeway is the clock skew allowed for exp, nbf and iat. Default 60 s, at most MaxLeeway.
	Leeway time.Duration
	// MaxTokenBytes refuses longer tokens before parsing them. Default 8 KiB.
	MaxTokenBytes int

	// RefreshInterval is the least time between two key set fetches that tokens trigger: an
	// unknown "kid" or a key set older than KeySetMaxAge. Until a key set has loaded, failed
	// fetches are retried after at most 10 s. Default 5 min.
	RefreshInterval time.Duration
	// KeySetMaxAge is the age after which the key set is fetched again, so that keys the issuer
	// withdraws stop verifying. If that fetch fails, the previous key set stays in use. Default 1 h.
	KeySetMaxAge time.Duration
	// FetchTimeout bounds one key set load (discovery and JWKS). Default 10 s.
	FetchTimeout time.Duration
	// MaxResponseBytes bounds discovery and JWKS response bodies. Default 1 MiB.
	MaxResponseBytes int64
	// HTTPClient fetches discovery and the key set. Default a client on http.DefaultTransport.
	// The Verifier uses a copy that never follows redirects.
	HTTPClient *http.Client
	// Clock reads the current time. Default bento.SystemClock.
	Clock application.Clock

	// AllowInsecureHTTP permits http:// issuer and key set URLs whose host is a loopback address
	// or "localhost". It exists for tests against local httptest servers only; production
	// configuration never sets it, and it never permits http:// to any other host.
	AllowInsecureHTTP bool
}

// Verifier verifies bearer access tokens. It is safe for concurrent use.
type Verifier struct {
	cfg        Config
	algorithms []jose.SignatureAlgorithm
	types      map[string]bool
	client     *http.Client

	keys atomic.Pointer[keySet]
	// fetching is a one-slot semaphore: the holder alone fetches and reads or writes attempted.
	fetching  chan struct{}
	attempted time.Time
}

// New checks cfg and returns its Verifier. It makes no network request: keys load on first use,
// or earlier through Refresh.
func New(cfg Config) (*Verifier, error) {
	if err := applyDefaults(&cfg); err != nil {
		return nil, err
	}
	v := &Verifier{cfg: cfg, types: map[string]bool{}, fetching: make(chan struct{}, 1)}
	for _, name := range cfg.Algorithms {
		alg, err := algorithm(name)
		if err != nil {
			return nil, err
		}
		v.algorithms = append(v.algorithms, alg)
	}
	for _, t := range cfg.TokenTypes {
		if strings.TrimSpace(t) == "" {
			return nil, fmt.Errorf("%w: empty token type", ErrInvalidConfig)
		}
		v.types[normalizeType(t)] = true
	}
	if err := checkIssuer(cfg.Issuer, cfg.AllowInsecureHTTP); err != nil {
		return nil, err
	}
	if cfg.JWKSURL != "" {
		if err := checkURL("JWKS URL", cfg.JWKSURL, cfg.AllowInsecureHTTP); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
		}
	}
	client := *cfg.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	v.client = &client
	return v, nil
}

func applyDefaults(cfg *Config) error {
	if cfg.Audience == "" {
		return fmt.Errorf("%w: audience is required", ErrInvalidConfig)
	}
	if len(cfg.Algorithms) == 0 {
		cfg.Algorithms = defaultAlgorithms
	}
	if len(cfg.TokenTypes) == 0 {
		cfg.TokenTypes = []string{DefaultTokenType}
	}
	setDefault(&cfg.TenantClaim, DefaultTenantClaim)
	setDefault(&cfg.CompanyClaim, DefaultCompanyClaim)
	setDefault(&cfg.ActorKindClaim, DefaultActorKindClaim)
	setDefault(&cfg.ActorIDClaim, DefaultActorIDClaim)
	names := map[string]bool{cfg.TenantClaim: true, cfg.CompanyClaim: true, cfg.ActorKindClaim: true, cfg.ActorIDClaim: true}
	if len(names) != 4 {
		return fmt.Errorf("%w: tenant, company, actor kind and actor ID claims must be distinct", ErrInvalidConfig)
	}
	setDefault(&cfg.Leeway, DefaultLeeway)
	setDefault(&cfg.MaxTokenBytes, DefaultMaxTokenBytes)
	setDefault(&cfg.RefreshInterval, DefaultRefreshInterval)
	setDefault(&cfg.KeySetMaxAge, DefaultKeySetMaxAge)
	setDefault(&cfg.FetchTimeout, DefaultFetchTimeout)
	setDefault(&cfg.MaxResponseBytes, DefaultMaxResponseBytes)
	if cfg.Leeway < 0 || cfg.Leeway > MaxLeeway || cfg.MaxTokenBytes < 0 || cfg.RefreshInterval < 0 ||
		cfg.KeySetMaxAge < 0 || cfg.FetchTimeout < 0 || cfg.MaxResponseBytes < 0 {
		return fmt.Errorf("%w: negative limit or leeway above %s", ErrInvalidConfig, MaxLeeway)
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{}
	}
	if cfg.Clock == nil {
		cfg.Clock = bento.SystemClock{}
	}
	return nil
}

func setDefault[T comparable](field *T, value T) {
	var zero T
	if *field == zero {
		*field = value
	}
}

func algorithm(name string) (jose.SignatureAlgorithm, error) {
	upper := strings.ToUpper(name)
	if upper == "NONE" || strings.HasPrefix(upper, "HS") {
		return "", fmt.Errorf("%w: algorithm %s is never accepted", ErrInvalidConfig, name)
	}
	alg, ok := supportedAlgorithms[name]
	if !ok {
		return "", fmt.Errorf("%w: unsupported algorithm %s", ErrInvalidConfig, name)
	}
	return alg, nil
}

// normalizeType compares "typ" values the way RFC 7515 §4.1.9 asks: media types are
// case-insensitive and "application/" may be omitted.
func normalizeType(t string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(t)), "application/")
}

func checkIssuer(issuer string, allowHTTP bool) error {
	if err := checkURL("issuer", issuer, allowHTTP); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	u, _ := url.Parse(issuer)
	if u.RawQuery != "" || u.ForceQuery {
		return fmt.Errorf("%w: issuer has a query", ErrInvalidConfig)
	}
	return nil
}

// checkURL requires an absolute https URL without credentials or fragment; with allowHTTP, an
// http URL to a loopback host passes too.
func checkURL(what, raw string, allowHTTP bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s is not a URL", what)
	}
	shown := u.Redacted() // never log a password from configuration
	if u.Host == "" || u.Opaque != "" {
		return fmt.Errorf("%s %q is not an absolute URL", what, shown)
	}
	if u.User != nil || u.Fragment != "" {
		return fmt.Errorf("%s %q carries credentials or a fragment", what, shown)
	}
	switch {
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && allowHTTP && loopback(u.Hostname()):
		return nil
	case u.Scheme == "http" && allowHTTP:
		return fmt.Errorf("%s %q: AllowInsecureHTTP permits loopback hosts only", what, shown)
	}
	return fmt.Errorf("%s %q must use https", what, shown)
}

func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Verify checks the signature, header and claims of a bearer access token and returns the
// principal it names. Every failure wraps identity.ErrUnauthenticated; when the key set cannot be
// fetched, it wraps ErrKeysUnavailable as well. Messages name the failed check, never the token or
// claim values.
func (v *Verifier) Verify(ctx context.Context, token string) (identity.Principal, error) {
	if token == "" {
		return identity.Principal{}, unauthenticated("empty token")
	}
	if len(token) > v.cfg.MaxTokenBytes {
		return identity.Principal{}, unauthenticated(fmt.Sprintf("token exceeds %d bytes", v.cfg.MaxTokenBytes))
	}
	jws, err := jose.ParseSignedCompact(token, v.algorithms)
	if err != nil {
		var unexpected *jose.ErrUnexpectedSignatureAlgorithm
		if errors.As(err, &unexpected) {
			return identity.Principal{}, unauthenticated("signature algorithm not accepted")
		}
		return identity.Principal{}, unauthenticated("malformed token")
	}
	if len(jws.Signatures) != 1 {
		return identity.Principal{}, unauthenticated("malformed token")
	}
	header := jws.Signatures[0].Protected
	if err := v.checkHeader(header); err != nil {
		return identity.Principal{}, err
	}
	key, err := v.key(ctx, header.KeyID, jose.SignatureAlgorithm(header.Algorithm))
	if err != nil {
		return identity.Principal{}, err
	}
	payload, err := jws.Verify(key)
	if err != nil {
		return identity.Principal{}, unauthenticated("invalid signature")
	}
	return v.principal(payload)
}

func (v *Verifier) checkHeader(h jose.Header) error {
	if _, ok := h.ExtraHeaders["crit"]; ok {
		return unauthenticated("critical header extensions are not supported")
	}
	if _, ok := h.ExtraHeaders["b64"]; ok {
		return unauthenticated("unencoded payloads are not supported")
	}
	typ, _ := h.ExtraHeaders[jose.HeaderType].(string)
	if !v.types[normalizeType(typ)] {
		return unauthenticated("token type not accepted")
	}
	if h.KeyID == "" {
		return unauthenticated("token header has no key ID")
	}
	return nil
}

// key returns the public key of kid for alg, fetching the key set when kid is unknown or the key
// set has outlived KeySetMaxAge.
func (v *Verifier) key(ctx context.Context, kid string, alg jose.SignatureAlgorithm) (crypto.PublicKey, error) {
	set := v.keys.Load()
	keys, known := set.lookup(kid)
	var err error
	switch {
	case !known:
		set, err = v.refresh(ctx, set, true)
		keys, known = set.lookup(kid)
	case v.now().Sub(set.fetchedAt) >= v.cfg.KeySetMaxAge:
		// A stale key set stays in use when the fetch fails or another caller is fetching.
		if fresh, ferr := v.refresh(ctx, set, false); ferr == nil {
			keys, known = fresh.lookup(kid)
		}
	}
	if !known {
		if err == nil && set == nil {
			err = fmt.Errorf("%w: no key set loaded yet", ErrKeysUnavailable)
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %w", identity.ErrUnauthenticated, err)
		}
		return nil, unauthenticated("unknown key ID")
	}
	return pick(keys, alg)
}

// Refresh loads discovery and the key set now, whatever RefreshInterval says; call it at start-up
// to fail fast on a wrong issuer. ctx bounds the wait for a fetch already in progress; the fetch
// itself is bounded by FetchTimeout. Errors wrap ErrKeysUnavailable.
func (v *Verifier) Refresh(ctx context.Context) error {
	select {
	case v.fetching <- struct{}{}:
	case <-ctx.Done():
		return fmt.Errorf("%w: %w", ErrKeysUnavailable, ctx.Err())
	}
	defer func() { <-v.fetching }()
	_, err := v.fetchLocked(ctx)
	return err
}

// refresh fetches the key set again unless another caller replaced seen in the meantime or the
// last attempt is more recent than the refresh interval. With wait false it gives up at once
// while another caller is fetching. It returns the current key set, possibly nil, and the error
// of a fetch it attempted.
func (v *Verifier) refresh(ctx context.Context, seen *keySet, wait bool) (*keySet, error) {
	if wait {
		select {
		case v.fetching <- struct{}{}:
		case <-ctx.Done():
			return v.keys.Load(), fmt.Errorf("%w: %w", ErrKeysUnavailable, ctx.Err())
		}
	} else {
		select {
		case v.fetching <- struct{}{}:
		default:
			return seen, nil
		}
	}
	defer func() { <-v.fetching }()
	if current := v.keys.Load(); current != seen {
		return current, nil
	}
	interval := v.cfg.RefreshInterval
	if seen == nil {
		interval = min(interval, coldRetryInterval)
	}
	if !v.attempted.IsZero() && v.now().Sub(v.attempted) < interval {
		return seen, nil
	}
	return v.fetchLocked(ctx)
}

// fetchLocked loads the key set; the caller holds the fetching semaphore.
func (v *Verifier) fetchLocked(ctx context.Context) (*keySet, error) {
	now := v.now()
	v.attempted = now
	set, err := v.load(ctx)
	if err != nil {
		return v.keys.Load(), err
	}
	set.fetchedAt = now
	v.keys.Store(set)
	return set, nil
}

func (v *Verifier) now() time.Time { return kernel.Instant(v.cfg.Clock.Now()) }

func (v *Verifier) principal(payload []byte) (identity.Principal, error) {
	claims, err := decodeClaims(payload)
	if err != nil {
		return identity.Principal{}, err
	}
	if err := v.checkRegistered(claims); err != nil {
		return identity.Principal{}, err
	}
	tenant, err := claims.identifier(v.cfg.TenantClaim)
	if err != nil {
		return identity.Principal{}, err
	}
	company, err := claims.identifier(v.cfg.CompanyClaim)
	if err != nil {
		return identity.Principal{}, err
	}
	actorID, err := claims.identifier(v.cfg.ActorIDClaim)
	if err != nil {
		return identity.Principal{}, err
	}
	kind, err := claims.str(v.cfg.ActorKindClaim)
	if err != nil {
		return identity.Principal{}, err
	}
	if !kernel.ActorKind(kind).Valid() {
		return identity.Principal{}, unauthenticated("claim " + v.cfg.ActorKindClaim + " is not human, ai or system")
	}
	p := identity.Principal{
		Actor: kernel.ActorRef{Kind: kernel.ActorKind(kind), ID: actorID},
		Scope: kernel.Scope{Tenant: kernel.TenantID(tenant), Company: kernel.CompanyID(company)},
	}
	if p.Validate() != nil {
		return identity.Principal{}, fmt.Errorf("%w: %w", identity.ErrUnauthenticated, identity.ErrInvalidPrincipal)
	}
	return p, nil
}

// checkRegistered checks iss, aud, exp, nbf and iat.
func (v *Verifier) checkRegistered(claims claimSet) error {
	iss, err := claims.str("iss")
	if err != nil {
		return err
	}
	if iss != v.cfg.Issuer {
		return unauthenticated("issuer mismatch")
	}
	aud, err := claims.audience()
	if err != nil {
		return err
	}
	if !slices.Contains(aud, v.cfg.Audience) {
		return unauthenticated("audience mismatch")
	}
	now := v.now()
	exp, present, err := claims.date("exp")
	if err != nil {
		return err
	}
	if !present {
		return unauthenticated("missing claim exp")
	}
	if !now.Before(exp.Add(v.cfg.Leeway)) {
		return unauthenticated("token expired")
	}
	nbf, present, err := claims.date("nbf")
	if err != nil {
		return err
	}
	if present && now.Before(nbf.Add(-v.cfg.Leeway)) {
		return unauthenticated("token not yet valid")
	}
	iat, present, err := claims.date("iat")
	if err != nil {
		return err
	}
	if present && now.Before(iat.Add(-v.cfg.Leeway)) {
		return unauthenticated("token issued in the future")
	}
	return nil
}

func unauthenticated(reason string) error {
	return fmt.Errorf("%w: oidc: %s", identity.ErrUnauthenticated, reason)
}
