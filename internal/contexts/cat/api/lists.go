package api

import (
	"slices"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// SkuStatuses are the statuses a SkuQuery may name.
var SkuStatuses = []string{"draft", "confirmed", "inactive"}

// SkuQuery asks for a page of the SKUs the caller sees, most recently changed first
// (kernel.ListCursor), audited as sku.list under cat.read (S7-06 read models). A SKU is seen as
// sku.get decides it; the data scope is applied in the query. Status narrows; Search keeps the
// SKUs whose name or ID contains the text, whatever its case. After is the Next of the previous
// page, empty for the first. Limit is 0 (kernel.DefaultListPage) to kernel.MaxListPage.
type SkuQuery struct {
	Status string
	Search string
	After  string
	Limit  int
}

// Validate checks the closed contract.
func (q SkuQuery) Validate() error {
	switch {
	case q.Status != "" && !slices.Contains(SkuStatuses, q.Status):
		return schema("/status")
	case q.Search != "" && !kernel.ValidSearchText(q.Search):
		return schema("/q")
	}
	if _, field := kernel.ListPosition(q.After, q.Limit); field != "" {
		return schema("/" + field)
	}
	return nil
}

// SkuSummary is one SKU of a list.
type SkuSummary struct {
	SkuID        string
	Name         string
	Status       string
	BaseUnit     string
	SpecRevision int64
	RowVersion   int64
	OwnerActorID string
	UpdatedAt    time.Time
}

// SkuPage is a page of a SkuQuery. Next is the After of the following page, empty on the last.
type SkuPage struct {
	Skus []SkuSummary
	Next string
}
