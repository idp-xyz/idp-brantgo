package kernel

import (
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
)

// A list query returns the objects a caller sees, most recently changed first, a page at a time.
// The order is (changed at, ID), both descending, and a page's cursor names the last entry it
// returned: the next page holds the entries strictly after it in that order. An object that
// changes between two pages moves to the front, so a reader that pages through a changing list
// may meet it twice or miss it; a list is a worklist, not a snapshot.
const (
	DefaultListPage = 50  // entries of a page when the query names no limit
	MaxListPage     = 200 // most entries of one page
)

// ErrInvalidCursor reports a list cursor this package did not issue.
var ErrInvalidCursor = errors.New("kernel: invalid list cursor")

// ListCursor is a position in a list: the entry changed at At with identifier ID.
type ListCursor struct {
	At time.Time
	ID string
}

// String is the cursor as a query parameter: opaque to the caller, URL-safe.
func (c ListCursor) String() string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(Instant(c.At).UnixMicro(), 10) + ":" + c.ID))
}

// ParseListCursor reads a cursor String returned. The empty string is not a cursor: a query
// without one starts at the first entry.
func ParseListCursor(s string) (ListCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return ListCursor{}, ErrInvalidCursor
	}
	micros, id, found := strings.Cut(string(raw), ":")
	at, err := strconv.ParseInt(micros, 10, 64)
	if !found || err != nil || !ValidIdentifier(id) {
		return ListCursor{}, ErrInvalidCursor
	}
	c := ListCursor{At: time.UnixMicro(at).UTC(), ID: id}
	if c.String() != s {
		return ListCursor{}, ErrInvalidCursor
	}
	return c, nil
}

// ListSize is the number of entries a page with this limit holds: the limit, or DefaultListPage
// when it is 0. A valid limit is 0 to MaxListPage.
func ListSize(limit int) int {
	if limit == 0 {
		return DefaultListPage
	}
	return limit
}
