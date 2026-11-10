package oidc_test

import (
	"errors"
	"testing"

	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/foundation/identity/oidc"
)

func TestBearerToken(t *testing.T) {
	const token = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ1In0.c2ln-_~+/="
	for _, header := range []string{
		"Bearer " + token,
		"bearer " + token,
		"BEARER " + token,
		"bEaReR " + token,
	} {
		got, err := oidc.BearerToken(header)
		if err != nil || got != token {
			t.Fatalf("BearerToken(%q) = %q, %v; want the token", header, got, err)
		}
	}
	for _, header := range []string{
		"",
		"Bearer",
		"Bearer ",
		"Bearer  " + token,
		"Bearer\t" + token,
		" Bearer " + token,
		"Bearer " + token + " ",
		"Bearer " + token + " extra",
		"Bearer =",
		"Bearer ab=c",
		"Bearer tok\x00en",
		"Basic dXNlcjpwYXNz",
		"Bearertoken",
		"Token " + token,
		"Bearer \"" + token + "\"",
	} {
		got, err := oidc.BearerToken(header)
		if !errors.Is(err, identity.ErrUnauthenticated) || got != "" {
			t.Fatalf("BearerToken(%q) = %q, %v; want ErrUnauthenticated", header, got, err)
		}
	}
}
