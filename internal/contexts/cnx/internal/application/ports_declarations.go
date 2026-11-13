package application

import (
	"context"

	dcl "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/declaration"
)

// DeclarationProfiles is the part of CNX's port to CAT a declaration reads (R39,
// DeclarationProfileRef v1, no guard lock): for each SKU specification revision under a code table
// version, the revision verified now with its content — commodity code, declaration name and
// elements, the legal units and their conversion from the base unit with their bases — or the
// profile unverified (only a draft: Verified false, no content). A key no profile is kept for is
// absent. A declaration records the revision and content hash it adopted; confirmation asks again
// and refuses a version whose profile was revised since (REFERENCE_STALE).
type DeclarationProfiles interface {
	ResolveDeclarationProfiles(ctx context.Context, refs []ProfileRef) (map[ProfileRef]dcl.Profile, error)
}

// OrderDocument is a sales contract or proforma invoice of an order as TRD seals it
// (SalesContractDocumentRef v1, R46): its kind, number, the order revision and hash it renders, its
// SHA-256, whether it is the current one of its kind, and how many signed copies it has.
type OrderDocument struct {
	Kind           string
	Number         string
	OrderRevision  int64
	DocumentSHA256 string
	Current        bool
	Signatures     int
}
