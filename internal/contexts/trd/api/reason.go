package api

import (
	"strings"
	"unicode"
)

// reasonText reports whether s is a reason a person gave for ending or revoking something: 1 to
// max characters of valid UTF-8, not only white space and free of control characters (Python T04
// A39 test_revocation_reason_is_nonempty_bounded_and_valid_unicode, PYT-0497; PUR's api.Reason).
// The order commands that revoke an approval or end an order use it.
func reasonText(s string, max int) bool {
	return text(s, max) && strings.TrimSpace(s) != "" && strings.IndexFunc(s, unicode.IsControl) < 0
}
