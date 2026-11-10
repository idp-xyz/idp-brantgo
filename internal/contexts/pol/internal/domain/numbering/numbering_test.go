package numbering

import (
	"testing"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func code(err error) string {
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code + " " + r.Fields[0].Path
	}
	if err != nil {
		return "ERROR " + err.Error()
	}
	return "OK"
}

// Python test_foundation.py test_number_and_plaintext_template_bind_snapshot (number half): the
// first quote number of 2026 is QT-2026-00001.
func TestNumbersArePrefixYearAndZeroPaddedValue(t *testing.T) {
	c := &Counter{DocumentType: "quote", Year: 2026}
	n, err := c.Next("QT-", 5)
	if err != nil || n.Number != "QT-2026-00001" || n.Value != 1 || c.Last != 1 {
		t.Fatalf("%+v %v", n, err)
	}
	if n, _ = c.Next("QT-", 5); n.Number != "QT-2026-00002" {
		t.Fatal(n.Number)
	}
}

// POL-INV-005: a counter that would need more digits than the width allows is exhausted and takes
// nothing.
func TestExhaustedSequenceTakesNothing(t *testing.T) {
	c := &Counter{DocumentType: "invoice", Year: 2026, Last: 9998, Stored: true}
	if n, err := c.Next("INV-", 4); err != nil || n.Number != "INV-2026-9999" {
		t.Fatalf("%+v %v", n, err)
	}
	if _, err := c.Next("INV-", 4); code(err) != "SEQUENCE_EXHAUSTED /args/sequence" || c.Last != 9999 {
		t.Fatalf("%s, last %d", code(err), c.Last)
	}
}

func TestYearFollowsTheSequenceTimeZone(t *testing.T) {
	newYearInShanghai := time.Date(2026, 12, 31, 16, 30, 0, 0, time.UTC)
	if y, err := Year(newYearInShanghai, "Asia/Shanghai"); err != nil || y != 2027 {
		t.Fatal(y, err)
	}
	if y, _ := Year(newYearInShanghai, "UTC"); y != 2026 {
		t.Fatal(y)
	}
	if _, err := Year(newYearInShanghai, "Nowhere/City"); err == nil {
		t.Fatal("an unknown time zone has no year")
	}
}

// Python test_foundation.py test_number_and_plaintext_template_bind_snapshot (BUSINESS_KEY_CONFLICT)
// and POL-INV-005: the same business reference keeps its number; another sequence for it conflicts.
func TestBusinessReferenceKeepsItsNumber(t *testing.T) {
	existing := Issued{Number: "QT-2026-00001", BusinessRef: "quote1", SequenceID: "quote.seq"}
	if err := Reuse(existing, "quote.seq"); err != nil {
		t.Fatal(err)
	}
	if got := code(Reuse(existing, "other.seq")); got != "BUSINESS_KEY_CONFLICT /args/business_ref" {
		t.Fatal(got)
	}
}
