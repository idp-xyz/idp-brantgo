// Package numbering is the DocumentNumberSequence aggregate: the yearly counter of one document type
// in a company, identified by (tenant, company, document_type, year), and the numbers it issued
// (POL-INV-005). A number is taken under the counter's row lock, so concurrent issues never
// duplicate; a business reference keeps the number it got first, so a retry never takes another;
// a counter that would need more digits than its sequence allows is exhausted. Numbers are issued
// before the downstream transaction that uses them, so a downstream rollback leaves a gap.
package numbering

import (
	"fmt"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Codes of POL's error catalog rows that numbering decides.
const (
	CodeSequenceExhausted   = "SEQUENCE_EXHAUSTED"
	CodeBusinessKeyConflict = "BUSINESS_KEY_CONFLICT"
)

// Categories are the categories of the codes above (422 and 409 in the error catalog).
var Categories = map[string]kernel.Category{
	CodeSequenceExhausted:   kernel.CategoryInvalid,
	CodeBusinessKeyConflict: kernel.CategoryConflict,
}

// Year returns the calendar year of at in time zone tz: the year a sequence's counter belongs to.
func Year(at time.Time, tz string) (int, error) {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return 0, err
	}
	return at.In(loc).Year(), nil
}

// Counter is the root: the last value issued for a document type in a year. Stored is false until
// the year's first number is issued.
type Counter struct {
	DocumentType string
	Year         int
	Last         int64
	Stored       bool
}

// Issued is a document number and what it was issued for.
type Issued struct {
	Number           string
	DocumentType     string
	Year             int
	Value            int64
	BusinessRef      string
	SequenceID       string
	SequenceRevision int64
}

// Next takes the counter's next value and forms its number: prefix, year, '-' and the value
// zero-padded to width digits (Python QT-2026-00001). A value of more than width digits is
// SEQUENCE_EXHAUSTED and takes nothing.
func (c *Counter) Next(prefix string, width int) (Issued, error) {
	limit := int64(1)
	for range width {
		limit *= 10
	}
	next := c.Last + 1
	if width < 1 || width > 18 || next >= limit {
		return Issued{}, kernel.RejectAs(CodeSequenceExhausted, Categories[CodeSequenceExhausted], "/args/sequence", "")
	}
	c.Last = next
	return Issued{Number: fmt.Sprintf("%s%d-%0*d", prefix, c.Year, width, next), DocumentType: c.DocumentType, Year: c.Year, Value: next}, nil
}

// Reuse decides a request for a business reference that already has a number: the same sequence
// gets the original number back, another sequence is BUSINESS_KEY_CONFLICT.
func Reuse(existing Issued, sequenceID string) error {
	if existing.SequenceID != sequenceID {
		return kernel.RejectAs(CodeBusinessKeyConflict, Categories[CodeBusinessKeyConflict], "/args/business_ref", "")
	}
	return nil
}
