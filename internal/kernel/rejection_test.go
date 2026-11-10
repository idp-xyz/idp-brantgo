package kernel

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestRejectUsesTheCatalogCategoryAndPythonDefaults(t *testing.T) {
	r := Reject(CodeVersionConflict, "", "")
	if r.Category != CategoryConflict || len(r.Fields) != 1 || r.Fields[0] != (FieldError{Path: "/", Reason: CodeVersionConflict}) {
		t.Fatalf("%+v", r)
	}
	wrapped := fmt.Errorf("outer: %w", errors.Join(errors.New("cause"), Reject(CodeNotFound, "/args/id", "")))
	got, ok := AsRejection(wrapped)
	if !ok || got.Code != CodeNotFound || got.Category != CategoryNotFound {
		t.Fatalf("AsRejection = %+v, %v", got, ok)
	}
	if _, ok := AsRejection(errors.New("plain")); ok {
		t.Fatal("a plain error is not a rejection")
	}
}

func TestRejectPanicsOnAnUnsharedCode(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("want a panic")
		}
	}()
	Reject("SOURCE_IDENTITY_CONFLICT", "/", "")
}

func TestFieldTextIsLimitedTo200Characters(t *testing.T) {
	r := RejectAs("X", CategoryInvalid, strings.Repeat("路", 300), strings.Repeat("a", 250))
	if n := len([]rune(r.Fields[0].Path)); n != 200 {
		t.Fatalf("path has %d characters", n)
	}
	if n := len(r.Fields[0].Reason); n != 200 {
		t.Fatalf("reason has %d characters", n)
	}
}
