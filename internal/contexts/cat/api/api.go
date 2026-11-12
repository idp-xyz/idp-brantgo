// Package api is the contract of the CAT bounded context (catalog: tradeable SKUs and their
// specification revisions, and the mappings of customers' and suppliers' item codes to SKUs) for
// transports, the AI layer and the contexts downstream of CAT. It exposes no CAT domain type.
package api

import (
	"context"
	"slices"
	"unicode/utf8"

	"go.idp.xyz/idp-brant/internal/kernel"
)

const (
	// Contract names the request and response shapes of CAT commands.
	Contract = "brant.cat/0.1.0"
	// Source identifies CAT as the producer of integration events.
	Source = "idp-brant.cat"

	EventSkuConfirmed         = "idp-brant.cat.sku_confirmed.v1"
	EventSpecificationRevised = "idp-brant.cat.specification_revised.v1"
	EventSkuDeactivated       = "idp-brant.cat.sku_deactivated.v1"

	CommandSaveSku       = "sku.save_draft"
	CommandConfirmSku    = "sku.confirm"
	CommandReviseSpec    = "sku.revise_spec"
	CommandDeactivateSku = "sku.deactivate"

	StatusDraft     = "draft"
	StatusConfirmed = "confirmed"
	StatusInactive  = "inactive"

	// UnitPCS is the only base unit of S1 (Python T01 base_uom const PCS). Units become POL
	// configuration references when POL lands.
	UnitPCS = "PCS"
)

// SaveSku creates a draft SKU at ExpectedVersion 0 or replaces the whole draft at its current
// version. Unknown SKUs are never created by other contexts (CAT-INV-001).
type SaveSku struct {
	IdempotencyKey  string
	SkuID           string
	ExpectedVersion int64
	Name            string
	BaseUnit        string // args.base_uom
}

// ConfirmSku verifies a draft SKU against sources the confirming human may read; each
// confirmation starts a new specification revision.
type ConfirmSku struct {
	IdempotencyKey  string
	SkuID           string
	ExpectedVersion int64
	EvidenceIDs     []string
}

// ReviseSpec reopens a confirmed SKU as a draft for a recorded reason; the confirmed specification
// revision stays as it was, and references to it are no longer current.
type ReviseSpec struct {
	IdempotencyKey  string
	SkuID           string
	ExpectedVersion int64
	Reason          string
}

// DeactivateSku stops a confirmed SKU for new business; its history stays readable.
type DeactivateSku struct {
	IdempotencyKey  string
	SkuID           string
	ExpectedVersion int64
	Reason          string
}

func (c SaveSku) Validate() error {
	if err := head(c.IdempotencyKey, c.SkuID, c.ExpectedVersion, 0); err != nil {
		return err
	}
	if c.Name != "" && !text(c.Name, 200) {
		return schema("/args/name")
	}
	if c.BaseUnit != "" && c.BaseUnit != UnitPCS {
		return schema("/args/base_uom")
	}
	return nil
}

func (c ConfirmSku) Validate() error {
	if err := head(c.IdempotencyKey, c.SkuID, c.ExpectedVersion, 1); err != nil {
		return err
	}
	if len(c.EvidenceIDs) < 1 || len(c.EvidenceIDs) > 200 {
		return schema("/args/evidence_ids")
	}
	for i, id := range c.EvidenceIDs {
		if !kernel.ValidIdentifier(id) || slices.Index(c.EvidenceIDs, id) != i {
			return schema("/args/evidence_ids")
		}
	}
	return nil
}

func (c ReviseSpec) Validate() error {
	if err := head(c.IdempotencyKey, c.SkuID, c.ExpectedVersion, 1); err != nil {
		return err
	}
	if !text(c.Reason, 1000) {
		return schema("/args/reason")
	}
	return nil
}

func (c DeactivateSku) Validate() error {
	if err := head(c.IdempotencyKey, c.SkuID, c.ExpectedVersion, 1); err != nil {
		return err
	}
	if !text(c.Reason, 1000) {
		return schema("/args/reason")
	}
	return nil
}

// Auditor records a request an inbound adapter refused before it became a command, such as a body
// that is not valid JSON, in the context's audit trail. The principal in ctx is the requester.
type Auditor interface {
	AuditRejected(ctx context.Context, command, objectID, code string) error
}

// Receipt is the closed success answer of a command.
type Receipt struct {
	ActionID   string
	ObjectID   string
	RowVersion int64
	Status     string
	Replayed   bool
}

// Commands is CAT's command entry point. Errors are *kernel.Rejection for refusals. For item code
// mapping commands the receipt's ObjectID is the mapping ID, RowVersion the root version and Status
// the status of the revision the command wrote (draft or active); GetItemCodeMapping shows the
// revision numbers.
type Commands interface {
	SaveSku(ctx context.Context, cmd SaveSku) (Receipt, error)
	ConfirmSku(ctx context.Context, cmd ConfirmSku) (Receipt, error)
	ReviseSpec(ctx context.Context, cmd ReviseSpec) (Receipt, error)
	DeactivateSku(ctx context.Context, cmd DeactivateSku) (Receipt, error)
	SaveItemCodeMapping(ctx context.Context, cmd SaveItemCodeMapping) (Receipt, error)
	PublishItemCodeMapping(ctx context.Context, cmd PublishItemCodeMapping) (Receipt, error)
}

// SkuRef is SkuRef v1: a SKU, its specification revision (0 before the first confirmation) and
// its state when asked.
type SkuRef struct {
	SkuID        string
	SpecRevision int64
	RowVersion   int64
	Status       string
	BaseUnit     string
}

// SpecRevision is a confirmed specification revision, kept unchanged.
type SpecRevision struct {
	Revision    int64
	Name        string
	BaseUnit    string
	EvidenceIDs []string
	ConfirmedBy kernel.ActorRef
}

// SkuView is a SKU and its confirmed specification revisions.
type SkuView struct {
	SkuID      string
	Status     string
	RowVersion int64
	Name       string
	BaseUnit   string
	Revisions  []SpecRevision
}

// Queries is CAT's read side.
type Queries interface {
	GetSku(ctx context.Context, skuID string) (SkuView, error)
	// ResolveSkus answers reference lookups of downstream contexts; SKUs outside the caller's data
	// scope are absent. Ask before opening your own transaction.
	ResolveSkus(ctx context.Context, ids []string) (map[string]SkuRef, error)
	// ResolveSpecs tells downstream contexts whether each specification revision is the SKU's
	// current confirmed one, for new use; every reference asked is answered. It needs no permission
	// beyond an active grant. Ask before opening your own transaction; inside it, SkuGuard v1
	// decides the same.
	ResolveSpecs(ctx context.Context, refs []SpecRef) (map[SpecRef]SpecStanding, error)
	// GetItemCodeMapping returns a mapping and its revisions, audited as item_code_mapping.get.
	GetItemCodeMapping(ctx context.Context, mappingID string) (ItemCodeMappingView, error)
	// ResolveItemCodes resolves ItemCodeMappingRef v1 by item code: every code asked is answered,
	// mapped to the active revision of its published mapping or unmapped. Mappings belong to the
	// company; it needs no permission beyond an active grant. Ask before opening your own
	// transaction.
	ResolveItemCodes(ctx context.Context, codes []ItemCode) (map[ItemCode]ResolvedItemCode, error)
	Audit(ctx context.Context) ([]kernel.AuditEntry, error)
}

func head(key, id string, expected, minVersion int64) error {
	switch {
	case !kernel.ValidIdentifier(key):
		return schema("/idempotency_key")
	case !kernel.ValidIdentifier(id):
		return schema("/args/id")
	case expected < minVersion || expected > 2147483647:
		return schema("/args/expected_version")
	}
	return nil
}

func schema(path string) error { return kernel.Reject(kernel.CodeSchemaInvalid, path, "") }

func text(s string, max int) bool {
	n := utf8.RuneCountInString(s)
	return n >= 1 && n <= max && utf8.ValidString(s)
}
