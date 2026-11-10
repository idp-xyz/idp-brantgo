package kernel

import (
	"errors"
	"fmt"
	"strings"
)

// Category groups rejection codes by how an inbound transport answers them. Transports map the
// category, never the code or the cause, to a status (engineering contract C5).
type Category string

const (
	CategoryUnsupported     Category = "unsupported"     // contract or command not registered
	CategoryUnauthenticated Category = "unauthenticated" // no verified token
	CategoryForbidden       Category = "forbidden"       // no permission; never reveals existence
	CategoryNotFound        Category = "not_found"       // nothing visible under that ID in the scope
	CategoryConflict        Category = "conflict"        // CAS, idempotency or identity conflict
	CategoryTooLarge        Category = "too_large"       // request beyond an ingress limit
	CategoryInvalid         Category = "invalid"         // structure, value, state or reference
)

// Codes every context shares (error catalog V0.1). A context declares its own codes in its api
// package with RejectAs.
const (
	CodeUnsupportedContract        = "UNSUPPORTED_CONTRACT"
	CodeSchemaInvalid              = "SCHEMA_INVALID"
	CodeRequestTooLarge            = "REQUEST_TOO_LARGE"
	CodeUnauthenticated            = "UNAUTHENTICATED"
	CodePermissionDenied           = "PERMISSION_DENIED"
	CodeNotFound                   = "NOT_FOUND"
	CodeVersionConflict            = "VERSION_CONFLICT"
	CodeIdempotencyPayloadConflict = "IDEMPOTENCY_PAYLOAD_CONFLICT"
	CodeInvalidKnownValue          = "INVALID_KNOWN_VALUE"
	CodeInvalidState               = "INVALID_STATE"
	CodeDraftNotReady              = "DRAFT_NOT_READY"
	CodeReferenceInvalid           = "REFERENCE_INVALID"
	CodeDuplicateLineID            = "DUPLICATE_LINE_ID"
)

var sharedCodes = map[string]Category{
	CodeUnsupportedContract:        CategoryUnsupported,
	CodeSchemaInvalid:              CategoryInvalid,
	CodeRequestTooLarge:            CategoryTooLarge,
	CodeUnauthenticated:            CategoryUnauthenticated,
	CodePermissionDenied:           CategoryForbidden,
	CodeNotFound:                   CategoryNotFound,
	CodeVersionConflict:            CategoryConflict,
	CodeIdempotencyPayloadConflict: CategoryConflict,
	CodeInvalidKnownValue:          CategoryInvalid,
	CodeInvalidState:               CategoryInvalid,
	CodeDraftNotReady:              CategoryInvalid,
	CodeReferenceInvalid:           CategoryInvalid,
	CodeDuplicateLineID:            CategoryInvalid,
}

// FieldError points at the request field a rejection is about, as a JSON Pointer into the command
// request ("/args/lines/0/quantity").
type FieldError struct {
	Path   string
	Reason string
}

// Rejection is a deterministic refusal of a command or query: a registered code, its category and
// the fields it concerns. It is never retried unchanged.
type Rejection struct {
	Code     string
	Category Category
	Fields   []FieldError
}

func (r *Rejection) Error() string {
	paths := make([]string, len(r.Fields))
	for i, f := range r.Fields {
		paths[i] = f.Path
	}
	return fmt.Sprintf("rejected %s at %s", r.Code, strings.Join(paths, ", "))
}

// Reject returns a rejection with one of the shared codes. path defaults to "/" and reason to the
// code, as in the Python contracts. It panics on a code that is not shared: that is a programming
// error, and a context's own codes go through RejectAs.
func Reject(code, path, reason string) *Rejection {
	category, ok := sharedCodes[code]
	if !ok {
		panic("kernel: unregistered rejection code " + code)
	}
	return RejectAs(code, category, path, reason)
}

// RejectAs returns a rejection with a context's own code and its category.
func RejectAs(code string, category Category, path, reason string) *Rejection {
	if path == "" {
		path = "/"
	}
	if reason == "" {
		reason = code
	}
	return &Rejection{Code: code, Category: category, Fields: []FieldError{{Path: truncate(path), Reason: truncate(reason)}}}
}

// AsRejection returns the rejection in err's chain.
func AsRejection(err error) (*Rejection, bool) {
	var r *Rejection
	ok := errors.As(err, &r)
	return r, ok
}

// truncate keeps a path or reason within the 200 characters the response contract allows.
func truncate(s string) string {
	if n := 0; len(s) > 200 {
		for i := range s {
			if n == 200 {
				return s[:i]
			}
			n++
		}
	}
	return s
}
