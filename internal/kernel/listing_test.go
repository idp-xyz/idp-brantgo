package kernel

import (
	"errors"
	"testing"
	"time"
)

func TestListCursorRoundTrip(t *testing.T) {
	at := time.Date(2026, 11, 26, 3, 4, 5, 123456789, time.FixedZone("x", 8*3600))
	c := ListCursor{At: at, ID: "rfq.2026-0001"}
	got, err := ParseListCursor(c.String())
	if err != nil {
		t.Fatal(err)
	}
	// Stored instants are UTC microseconds, so the cursor carries exactly that.
	if !got.At.Equal(Instant(at)) || got.ID != c.ID || got.String() != c.String() {
		t.Fatalf("round trip %+v, want %+v", got, c)
	}
}

func TestParseListCursorRefusesWhatItDidNotIssue(t *testing.T) {
	valid := ListCursor{At: time.Unix(1, 0), ID: "a"}.String()
	for _, s := range []string{"", "!", "YQ", valid + "A", valid[:len(valid)-1],
		ListCursor{At: time.Unix(1, 0), ID: "has space"}.String()} {
		if _, err := ParseListCursor(s); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("%q: %v", s, err)
		}
	}
}

func TestListSize(t *testing.T) {
	if ListSize(0) != DefaultListPage || ListSize(7) != 7 {
		t.Fatal("list size")
	}
}
