package oidc

import (
	"context"
	"strings"

	"go.idp.xyz/idp-brant/internal/foundation/identity"
)

// HeaderVerifier verifies an Authorization header value rather than a bare token: BearerToken
// first, then Tokens.Verify. It suits callers that hand over the header, as the HTTP transport's
// Verifier does.
type HeaderVerifier struct {
	Tokens *Verifier
}

// Verify returns the principal of the bearer token in header.
func (h HeaderVerifier) Verify(ctx context.Context, header string) (identity.Principal, error) {
	token, err := BearerToken(header)
	if err != nil {
		return identity.Principal{}, err
	}
	return h.Tokens.Verify(ctx, token)
}

// BearerToken returns the token of an "Authorization: Bearer <token>" header value (RFC 6750
// §2.1): the scheme in any case, exactly one space, and a non-empty token of b64token characters.
// Every failure wraps identity.ErrUnauthenticated and never echoes the header.
func BearerToken(header string) (string, error) {
	const scheme = "bearer "
	if len(header) <= len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return "", unauthenticated("authorization header is not a bearer token")
	}
	token := header[len(scheme):]
	if !b64token(token) {
		return "", unauthenticated("malformed bearer token")
	}
	return token, nil
}

// b64token reports whether s matches 1*( ALPHA / DIGIT / "-" / "." / "_" / "~" / "+" / "/" ) *"=".
func b64token(s string) bool {
	body := strings.TrimRight(s, "=")
	if body == "" {
		return false
	}
	for i := 0; i < len(body); i++ {
		c := body[i]
		ok := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '.' || c == '_' || c == '~' || c == '+' || c == '/'
		if !ok {
			return false
		}
	}
	return true
}
