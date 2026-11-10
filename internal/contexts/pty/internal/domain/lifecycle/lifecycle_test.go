package lifecycle

import (
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func code(err error) string {
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code
	}
	return "OK"
}

// The order of the checks is the Python order: existence and visibility, version, state.
func TestChecksRunInContractOrder(t *testing.T) {
	confirmed := &Record{ID: "c", Status: Confirmed, RowVersion: 2}
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"new at 0", CheckSave(nil, false, 0), "OK"},
		{"new at 1", CheckSave(nil, false, 1), kernel.CodeNotFound},
		{"invisible", CheckSave(confirmed, false, 2), kernel.CodeNotFound},
		{"stale before state", CheckSave(confirmed, true, 1), kernel.CodeVersionConflict},
		{"existing at 0", CheckSave(confirmed, true, 0), kernel.CodeVersionConflict},
		{"confirmed is not a draft", CheckSave(confirmed, true, 2), kernel.CodeInvalidState},
		{"deactivate confirmed", Check(confirmed, true, 2, Confirmed), "OK"},
		{"missing", Check(nil, true, 1, Confirmed), kernel.CodeNotFound},
	} {
		if got := code(c.err); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

func TestMoveCountsVersionsAndConfirmations(t *testing.T) {
	r := New("p", "alice", "")
	r.Move(Draft)
	r.Move(Confirmed)
	r.Move(Draft)
	r.Move(Confirmed)
	if r.RowVersion != 4 || r.Revision != 2 || r.Status != Confirmed {
		t.Fatalf("%+v", r)
	}
}
