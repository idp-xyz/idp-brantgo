package capability

import (
	_ "embed"
	"strconv"
	"strings"
)

// A context states the reason of a refusal to its caller (kernel.FieldError.Reason, the
// field_errors of the HTTP answer). What a model is told of a refused tool call, and what the
// ledger keeps of it for the next turn, is narrower (S4 G3): only a reason that is one of the
// static texts of the product's source — byte-equal to a string literal that stands at a reason
// position of a rejection in internal/kernel, internal/foundation, internal/ai or a context. Such
// a text was written by a developer and carries nothing of a run: no other party's data, no
// credential, no value the caller may not read. A reason a context builds at run time (a count, a
// state, a locator, a limit and the actual value) is not passed on; the model still gets the code,
// the path and what the tool's schema says of the member.
//
// reasons.txt is that list, one Go-quoted text per line, in order. It is generated from the source
// (go test ./internal/ai/capability -run TestStatedReasonsAreTheSourcesLiterals with
// BRANT_UPDATE_REASONS=1) and the same test fails when it is not current.

//go:embed reasons.txt
var reasonList string

var statedReasons = func() map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(reasonList, "\n") {
		if text, err := strconv.Unquote(line); err == nil && text != "" {
			out[text] = true
		}
	}
	return out
}()

// StatedReason returns reason when a refusal with code states it and it is one of the static
// texts of the product's source, and "" otherwise: when the context stated none (the reason of a
// rejection without one is its code) or built it at run time.
func StatedReason(code, reason string) string {
	if reason == "" || reason == code || !statedReasons[reason] {
		return ""
	}
	return reason
}
