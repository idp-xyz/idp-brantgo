package main

import (
	"errors"
	"io"
	"testing"
)

// Usage errors are told apart from failures (exit status 2, not 1) before anything touches a
// database.
func TestUsageIsCheckedBeforeTheDatabase(t *testing.T) {
	for _, args := range [][]string{nil, {"demo"}, {"foundation-demo", "extra"}, {"foundation-demo", "-unknown"}} {
		if err := run(args, "postgres://unused", io.Discard); !errors.Is(err, errUsage) {
			t.Errorf("%q: %v, want the usage error", args, err)
		}
	}
	if err := run([]string{"foundation-demo"}, "", io.Discard); err == nil || errors.Is(err, errUsage) {
		t.Fatalf("missing DSN: %v", err)
	}
}
