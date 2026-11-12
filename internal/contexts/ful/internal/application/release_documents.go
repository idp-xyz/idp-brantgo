package application

import (
	"context"
	"strconv"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/release"
	docs "go.idp.xyz/idp-brant/internal/contexts/ful/internal/domain/shipmentdocumentset"
)

// The documents release on the issued bill of lading record (S5-07 with S5-08): document_release.record
// releases the original B/L or a telex release of what the shipment's document set issued as its
// bill of lading record. After its release root it locks the shipment's document set FOR SHARE —
// shipment_document.prepare and issue lock the shipment FOR SHARE before the set FOR UPDATE, so the
// two meet on the shipment first — and needs the set's issued B/L version: none is
// SHIPMENT_DOCUMENTS_UNRECONCILED (a required document is missing). The consumption records the set,
// the version, its number and content SHA-256; a telex release cites the telex release message among
// its evidence, which the command's contract requires of every documents release.

// lockDocumentSetOf locks shipment id's document set FOR SHARE and returns it, or nil.
func lockDocumentSetOf(ctx context.Context, exec bentopg.Executor, id string) (*docs.Set, error) {
	setID, err := postgres.SetOfShipment(ctx, exec, scope(ctx), id)
	if err != nil || setID == "" {
		return nil, err
	}
	return postgres.LoadSet(ctx, exec, scope(ctx), setID, postgres.ForShare)
}

// billOfLadingOf is the issued B/L record of set the documents release binds, with the evidence entry
// of the consuming transaction.
func billOfLadingOf(set *docs.Set) (*release.BillOfLadingRef, docs.Entry, error) {
	var bl *docs.Document
	if set != nil {
		bl = set.IssuedBillOfLading()
	}
	if bl == nil {
		return nil, docs.Entry{}, api.Reject(api.CodeShipmentDocumentsUnreconciled, "/args/shipment_id",
			"The shipment's document set has no issued bill of lading record: documents are released on it")
	}
	ref := &release.BillOfLadingRef{DocumentSetID: set.ID, Version: bl.Version, Number: bl.Number, ContentSHA256: bl.ContentSHA256}
	return ref, docs.Entry{Name: "ful.bill_of_lading", Value: set.ID + "/v" + strconv.FormatInt(bl.Version, 10) + "/" + bl.Number + "/" +
		bl.ContentSHA256}, nil
}
